package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/store"
	"iot-gateway-go/internal/values"
)

const defaultInterval = 5 * time.Second

// Scheduler 用 pollScheduler(替代 robfig/cron)统一调度采集任务,任务投递到 worker pool 执行。
// 常驻 goroutine = 调度器(恒 1) + poolSize 个 worker,与设备数解耦;
// 配置变更时**增量 reconcile**:只增删改受影响的设备/连接,保留未变设备的
// 连接与采集,消除全量重载造成的采集空窗与连接重连。见 docs/incremental-hot-reload-design.md。
type Scheduler struct {
	store    *store.Store
	output   chan<- model.DataPoint
	status   *status.Registry
	values   *values.Registry
	outputs  *output.Manager // 设备上下线通知的动态来源(热重载后自动跟随最新输出)
	poolSize int

	baseCtx context.Context

	// 以下采集基础设施跨 reload 持久(首次创建,进程关闭才销毁)。
	mu            sync.Mutex
	runtimes      map[string]*deviceRuntime // deviceID -> 运行态(仅 reload 单 goroutine 访问)
	sched         *pollScheduler
	taskCh        chan collectTask
	workersDone   <-chan struct{}
	collectCtx    context.Context
	collectCancel context.CancelFunc

	// 采集计数(原子,worker 并发自增;供 /metrics 暴露)。仅轮询路径在 collectOnce 累加。
	collectCount  atomic.Int64
	collectErrors atomic.Int64
}

// collectMode 是设备的采集方式。
type collectMode int

const (
	collectPoll      collectMode = iota // 按 intervalMs 周期 Read
	collectSubscribe                    // 驱动推送(Subscriber)
	collectListen                       // 驱动被动监听(Listener)
)

// deviceRuntime 跟踪单个设备的运行态,供增量 diff/reconcile。
type deviceRuntime struct {
	deviceID string
	conn     driver.Conn
	mode     collectMode
	// poll:
	job *deviceJob
	// diff 键与内容
	connKey    string
	params     json.RawMessage
	intervalMs int
	sig        string // 采集规格签名(变化即需处理)
}

// desiredDevice 是新配置中一个待调度设备(含其连接配置)。
type desiredDevice struct {
	device  model.Device
	conn    model.Connection
	connKey string
	sig     string
}

func NewScheduler(st *store.Store, dataPoints chan<- model.DataPoint, poolSize int, statusReg *status.Registry, valuesReg *values.Registry, outputs *output.Manager) *Scheduler {
	if poolSize <= 0 {
		poolSize = 16
	}
	return &Scheduler{store: st, output: dataPoints, poolSize: poolSize, status: statusReg, values: valuesReg, outputs: outputs}
}

func (s *Scheduler) Run(ctx context.Context) error {
	s.baseCtx = ctx
	changeCh, cancel := s.store.OnChange()
	defer cancel()
	if err := s.reload(); err != nil {
		// 首次 reload 失败不退出调度器:等下一次 OnChange 重试(API 仍可修复配置)。
		slog.Error("initial reload failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			s.stopCollectors()
			return ctx.Err()
		case <-changeCh:
			if err := s.reload(); err != nil {
				slog.Error("scheduler reload failed", "err", err)
			}
		}
	}
}

// reload 读取最新配置并增量 reconcile:未变设备零操作,变化设备只处理受影响部分。
func (s *Scheduler) reload() error {
	// 1. 确保采集基础设施(首次创建)。
	s.ensureInfra()

	// 2. 读最新配置,构建 desired 计划。
	devices, err := s.store.ListDevices()
	if err != nil {
		slog.Error("list devices failed", "err", err)
		return err
	}
	desired, err := s.buildDesired(devices)
	if err != nil {
		return err
	}

	// 3. 对旧运行态与 desired 做 diff + reconcile(单 goroutine,worker 不读 runtimes)。
	old := s.runtimes
	if old == nil {
		old = map[string]*deviceRuntime{}
	}
	newRt := s.reconcile(old, desired)

	s.mu.Lock()
	s.runtimes = newRt
	s.mu.Unlock()
	return nil
}

// ensureInfra 创建跨 reload 持久的基础设施(pollScheduler / taskCh / workers / collectCtx)。
func (s *Scheduler) ensureInfra() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.taskCh != nil {
		return
	}
	collectCtx, cancel := context.WithCancel(s.baseCtx)
	s.collectCtx = collectCtx
	s.collectCancel = cancel
	// 任务队列容量取 2×poolSize:非阻塞投递、满则跳过(防堆积),缓冲只用于吸收
	// 同一瞬间多个调度刻度同时触发(启动/热重载/间隔对齐)的突发,不做积压。
	s.taskCh = make(chan collectTask, 2*s.poolSize)
	s.workersDone = s.startWorkers(s.taskCh)
	ps := newPollScheduler()
	ps.start()
	s.sched = ps
}

// buildDesired 把设备列表组装为 reconcile 的期望计划(跳过禁用/无点位设备)。
func (s *Scheduler) buildDesired(devices []model.Device) (map[string]desiredDevice, error) {
	connCache := make(map[string]model.Connection)
	getConn := func(id string) (model.Connection, error) {
		if c, ok := connCache[id]; ok {
			return c, nil
		}
		c, err := s.store.GetConnection(id)
		if err != nil {
			return model.Connection{}, err
		}
		connCache[id] = c
		return c, nil
	}

	desired := make(map[string]desiredDevice, len(devices))
	for _, d := range devices {
		if !d.Enabled || len(d.Points) == 0 {
			continue
		}
		conn, err := getConn(d.ConnectionID)
		if err != nil {
			slog.Error("get connection failed", "device", d.ID, "connection", d.ConnectionID, "err", err)
			s.markOffline(d.ID, "get connection failed: "+err.Error())
			continue
		}
		desired[d.ID] = desiredDevice{
			device:  d,
			conn:    conn,
			connKey: connKeyOf(d.ConnectionID, conn.Driver, conn.Config),
			sig:     sigOf(d.Params, d.Points, d.IntervalMs),
		}
	}
	return desired, nil
}

// connKeyOf 是连接级身份:连接配置/驱动/ID 变化即整体重开。
func connKeyOf(connectionID, driverName string, cfg json.RawMessage) string {
	return connectionID + "\x00" + driverName + "\x00" + string(cfg)
}

// sigOf 是设备采集规格签名:参数/点位/间隔变化即需处理。
func sigOf(params json.RawMessage, points []model.Point, intervalMs int) string {
	pts, _ := json.Marshal(points)
	return string(params) + "\x00" + strconv.Itoa(intervalMs) + "\x00" + string(pts)
}

// reconcile 对新旧配置做 diff 并按连接组增量执行。
// 规则见 docs/incremental-hot-reload-design.md §3/§4。
func (s *Scheduler) reconcile(old map[string]*deviceRuntime, desired map[string]desiredDevice) map[string]*deviceRuntime {
	newRt := make(map[string]*deviceRuntime, len(desired))

	oldGroups := make(map[string][]string)
	for id, rt := range old {
		oldGroups[rt.connKey] = append(oldGroups[rt.connKey], id)
	}
	newGroups := make(map[string][]string)
	for id, dd := range desired {
		newGroups[dd.connKey] = append(newGroups[dd.connKey], id)
	}

	connKeys := make([]string, 0, len(oldGroups)+len(newGroups))
	seen := make(map[string]bool)
	for k := range oldGroups {
		if !seen[k] {
			seen[k] = true
			connKeys = append(connKeys, k)
		}
	}
	for k := range newGroups {
		if !seen[k] {
			seen[k] = true
			connKeys = append(connKeys, k)
		}
	}

	for _, ck := range connKeys {
		oldIDs := oldGroups[ck]
		newIDs := newGroups[ck]
		switch {
		case len(oldIDs) == 0:
			// 连接新增:整组打开。
			s.openAndRegisterBatch(newIDs, desired, newRt)
		case len(newIDs) == 0:
			// 连接删除:整组停止。
			for _, id := range oldIDs {
				s.stopDevice(old[id])
			}
		default:
			s.reconcileGroup(ck, oldIDs, newIDs, old, desired, newRt)
		}
	}
	return newRt
}

// reconcileGroup 处理"连接未变"的组内增量。
func (s *Scheduler) reconcileGroup(_ string, oldIDs, newIDs []string, old map[string]*deviceRuntime, desired map[string]desiredDevice, newRt map[string]*deviceRuntime) {
	oldSet := make(map[string]bool, len(oldIDs))
	for _, id := range oldIDs {
		oldSet[id] = true
	}
	newSet := make(map[string]bool, len(newIDs))
	for _, id := range newIDs {
		newSet[id] = true
	}

	// 组模式从任一旧运行态推断;订阅/监听组一旦出现删除或签名变化 → 整组重开
	// (共享订阅无单设备卸载能力,整组重开保证正确清理,见设计文档 §4.4)。
	groupMode := collectPoll
	if len(oldIDs) > 0 {
		groupMode = old[oldIDs[0]].mode
	}
	groupRestart := false
	if groupMode != collectPoll {
		for _, id := range oldIDs {
			if dd, ok := desired[id]; !ok {
				groupRestart = true
				break
			} else if old[id].sig != dd.sig {
				groupRestart = true
				break
			}
		}
	}

	if groupRestart {
		// 整组重开:停全部旧,再开全部新。
		for _, id := range oldIDs {
			s.stopDevice(old[id])
		}
		s.openAndRegisterBatch(newIDs, desired, newRt)
		return
	}

	// 逐设备:keep / poll 原地更新 / 参数变化重开 / 新增 / 删除。
	for _, id := range newIDs {
		rt, ok := old[id]
		if !ok {
			s.openAndRegisterBatch([]string{id}, desired, newRt)
			continue
		}
		dd := desired[id]
		if rt.sig == dd.sig {
			newRt[id] = rt // 未变,零操作
			continue
		}
		if s.updatePollDevice(rt, dd) {
			rt.sig = dd.sig
			newRt[id] = rt // 原地更新成功
			continue
		}
		// 轮询设备参数变化:先开新(驱动池复用物理连接)→ 注册 → 关旧,避免设备级空窗。
		s.openAndRegisterBatch([]string{id}, desired, newRt)
		s.stopDevice(rt)
	}
	// 删除的设备(旧有、新无)。
	for _, id := range oldIDs {
		if !newSet[id] {
			s.stopDevice(old[id])
		}
	}
}

// updatePollDevice 原地更新轮询设备的点位/间隔;参数变化返回 false(需重开连接)。
func (s *Scheduler) updatePollDevice(rt *deviceRuntime, dd desiredDevice) bool {
	if string(rt.params) != string(dd.device.Params) {
		return false // 设备参数变化(如从机地址)须重开设备级 conn
	}
	if rt.intervalMs != dd.device.IntervalMs {
		// 间隔变化:重排调度(复用同 job 指针,点位同时原地更新,不重连不重建)。
		rt.job.setPoints(dd.device.Points)
		s.sched.schedule(rt.deviceID, intervalOf(dd.device.IntervalMs), rt.job)
		rt.intervalMs = dd.device.IntervalMs
		return true
	}
	// 仅点位变化:原地更新 job 点位,不重连不重排。
	rt.job.setPoints(dd.device.Points)
	return true
}

// openAndRegisterBatch 并行打开一批设备并注册采集方式;单设备也走此路径(串行退化为一次)。
func (s *Scheduler) openAndRegisterBatch(ids []string, desired map[string]desiredDevice, newRt map[string]*deviceRuntime) {
	if len(ids) == 0 {
		return
	}
	type res struct {
		id string
		rt *deviceRuntime
	}
	results := make([]res, len(ids))
	var wg sync.WaitGroup
	sem := make(chan struct{}, s.poolSize)
	for i, id := range ids {
		dd := desired[id]
		wg.Add(1)
		go func(i int, id string, dd desiredDevice) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			conn, ok := s.openDevice(s.collectCtx, dd.device, dd.conn)
			if !ok {
				return
			}
			results[i] = res{id: id, rt: &deviceRuntime{
				deviceID:   id,
				conn:       conn,
				connKey:    dd.connKey,
				params:     dd.device.Params,
				intervalMs: dd.device.IntervalMs,
				sig:        dd.sig,
			}}
		}(i, id, dd)
	}
	wg.Wait()

	for _, r := range results {
		if r.rt == nil {
			continue
		}
		if s.registerDevice(r.rt, desired[r.id]) {
			newRt[r.id] = r.rt
		} else {
			r.rt.conn.Close() // 注册失败:释放连接,该设备跳过
		}
	}
}

// registerDevice 按驱动能力注册采集方式(订阅/监听/轮询),返回是否成功。
func (s *Scheduler) registerDevice(rt *deviceRuntime, dd desiredDevice) bool {
	device := dd.device
	if sub, ok := rt.conn.(driver.Subscriber); ok {
		rt.mode = collectSubscribe
		if err := sub.Subscribe(s.collectCtx, device.Points, func(dp model.DataPoint) {
			s.pushData(s.collectCtx, device.ID, dp)
		}); err != nil {
			slog.Error("subscribe failed", "device", device.ID, "err", err)
			s.markOffline(device.ID, err.Error())
			return false
		}
		s.markOnline(device.ID, time.Now())
		return true
	}
	if lis, ok := rt.conn.(driver.Listener); ok {
		rt.mode = collectListen
		if err := lis.Listen(s.collectCtx, device.Points, func(dp model.DataPoint) {
			s.pushData(s.collectCtx, device.ID, dp)
		}); err != nil {
			slog.Error("listen failed", "device", device.ID, "err", err)
			s.markOffline(device.ID, err.Error())
			return false
		}
		s.markOnline(device.ID, time.Now())
		return true
	}
	// 轮询:注册到 pollScheduler(支持亚秒级精确间隔,替代 cron)。
	rt.mode = collectPoll
	job := &deviceJob{taskCh: s.taskCh, conn: rt.conn, deviceID: device.ID, ctx: s.collectCtx}
	job.setPoints(device.Points)
	s.sched.schedule(device.ID, intervalOf(device.IntervalMs), job)
	rt.job = job
	rt.intervalMs = device.IntervalMs
	return true
}

// stopDevice 停止单个设备:轮询移除调度条目,关闭设备连接(驱动池引用计数释放)。
func (s *Scheduler) stopDevice(rt *deviceRuntime) {
	if rt.mode == collectPoll && rt.job != nil {
		if s.sched != nil {
			s.sched.remove(rt.deviceID)
		}
	}
	if rt.conn != nil {
		if err := rt.conn.Close(); err != nil {
			slog.Error("close device connection failed", "err", err)
		}
	}
}

// stopCollectors 进程关闭时的整体清理:取消采集、停调度器、关 taskCh、等 workers、
// 关闭全部设备连接。补传队列等持久化数据不动(跨重启续传)。
func (s *Scheduler) stopCollectors() {
	s.mu.Lock()
	collectCancel := s.collectCancel
	sched := s.sched
	taskCh := s.taskCh
	workersDone := s.workersDone
	runtimes := s.runtimes
	s.collectCancel = nil
	s.sched = nil
	s.taskCh = nil
	s.workersDone = nil
	s.runtimes = nil
	s.mu.Unlock()

	if collectCancel == nil {
		return
	}
	collectCancel()
	if sched != nil {
		sched.stop()
	}
	if taskCh != nil {
		close(taskCh)
	}
	if workersDone != nil {
		<-workersDone
	}
	for _, rt := range runtimes {
		if rt.conn != nil {
			if err := rt.conn.Close(); err != nil {
				slog.Error("close device connection failed", "err", err)
			}
		}
	}
}

func (s *Scheduler) openDevice(ctx context.Context, device model.Device, connection model.Connection) (driver.Conn, bool) {
	drv, err := driver.Get(connection.Driver)
	if err != nil {
		slog.Error("driver not registered", "device", device.ID, "driver", connection.Driver, "err", err)
		s.markOffline(device.ID, err.Error())
		return nil, false
	}
	conn, err := drv.Open(ctx, driver.OpenRequest{
		DeviceID:     device.ID,
		ConnectionID: device.ConnectionID,
		ConnConfig:   connection.Config,
		DeviceParams: device.Params,
	})
	if err != nil {
		slog.Error("open device failed", "device", device.ID, "err", err)
		s.markOffline(device.ID, "open failed: "+err.Error())
		return nil, false
	}
	return conn, true
}

func (s *Scheduler) startWorkers(taskCh <-chan collectTask) <-chan struct{} {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(s.poolSize)
	for workerIndex := 0; workerIndex < s.poolSize; workerIndex++ {
		go func() {
			defer wg.Done()
			for task := range taskCh {
				s.collectOnce(task.ctx, task.conn, task.points, task.deviceID)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// markOnline 记录设备在线;仅在状态由"离线/未知 → 在线"转变时通知输出。
func (s *Scheduler) markOnline(deviceID string, t time.Time) {
	prev, _ := s.status.Get(deviceID)
	s.status.SetOnline(deviceID, t)
	if !prev.Online {
		s.notifyOnline(deviceID)
	}
}

// markOffline 记录设备离线;仅在状态由"在线 → 离线"转变时通知输出(从未在线不通知)。
func (s *Scheduler) markOffline(deviceID, errMsg string) {
	prev, _ := s.status.Get(deviceID)
	s.status.SetOffline(deviceID, errMsg)
	if prev.Online {
		s.notifyOffline(deviceID)
	}
}

func (s *Scheduler) notifyOnline(deviceID string) {
	if s.outputs == nil {
		return
	}
	for _, n := range s.outputs.Notifiers() {
		n.DeviceOnline(deviceID)
	}
}

func (s *Scheduler) notifyOffline(deviceID string) {
	if s.outputs == nil {
		return
	}
	for _, n := range s.outputs.Notifiers() {
		n.DeviceOffline(deviceID)
	}
}

// pushData 把推送式(订阅/监听)采集到的 DataPoint 投递到输出,并刷新设备在线状态。
func (s *Scheduler) pushData(ctx context.Context, deviceID string, dp model.DataPoint) {
	s.markOnline(deviceID, time.Now())
	s.emit(ctx, dp)
}

// emit 把单个 DataPoint 投递到输出 channel;ctx 取消时返回 false。
// 轮询、订阅、监听三条采集路径共用此投递入口,也是记录实时值快照的唯一入口。
func (s *Scheduler) emit(ctx context.Context, dp model.DataPoint) bool {
	if s.values != nil {
		s.values.Update(dp)
	}
	select {
	case s.output <- dp:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Scheduler) collectOnce(ctx context.Context, conn driver.Conn, points []model.Point, deviceID string) {
	s.collectCount.Add(1)
	dataPoints, err := conn.Read(ctx, points)
	if err != nil {
		// 配置级错误(整批无效):记录离线,不发送
		s.collectErrors.Add(1)
		s.markOffline(deviceID, err.Error())
		slog.Error("read points failed", "points", len(points), "err", err)
		return
	}
	anyGood := false
	for _, dp := range dataPoints {
		if dp.Quality == model.QualityGood {
			anyGood = true
		}
		if !s.emit(ctx, dp) {
			return
		}
	}
	if anyGood {
		s.markOnline(deviceID, time.Now())
	} else {
		// 全部点位质量坏:无可采数据,计入采集错误
		s.collectErrors.Add(1)
		s.markOffline(deviceID, "all points bad")
	}
}

// SchedulerStats 是调度器采集侧运行统计快照,供 /metrics 暴露。
type SchedulerStats struct {
	CollectTotal  int64 // 轮询采集执行次数
	CollectErrors int64 // 轮询采集失败次数(读失败或全部点位质量坏)
	TaskQueueLen  int   // taskCh 当前长度
	TaskQueueCap  int   // taskCh 容量(2×poolSize)
}

// Stats 返回采集计数与任务队列长度快照。
func (s *Scheduler) Stats() SchedulerStats {
	s.mu.Lock()
	var qlen, qcap int
	if s.taskCh != nil {
		qlen = len(s.taskCh)
		qcap = cap(s.taskCh)
	}
	s.mu.Unlock()
	return SchedulerStats{
		CollectTotal:  s.collectCount.Load(),
		CollectErrors: s.collectErrors.Load(),
		TaskQueueLen:  qlen,
		TaskQueueCap:  qcap,
	}
}

// IsReady 指示采集基础设施(调度器)已启动,供 /readyz 就绪探针。
// store 已开 + 配置加载完成在装配期必然为真,故就绪判定主要看调度器是否就绪。
func (s *Scheduler) IsReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sched != nil
}

type collectTask struct {
	ctx      context.Context
	conn     driver.Conn
	points   []model.Point
	deviceID string
}

// deviceJob 是轮询采集的 cron job;points 可原地更新(增量热加载点位变化不重连)。
type deviceJob struct {
	taskCh   chan collectTask
	conn     driver.Conn
	deviceID string
	ctx      context.Context
	mu       sync.Mutex
	points   []model.Point
}

func (j *deviceJob) setPoints(points []model.Point) {
	j.mu.Lock()
	j.points = points
	j.mu.Unlock()
}

func (j *deviceJob) getPoints() []model.Point {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.points
}

// fire 到达采集周期时由 pollScheduler 触发:把采集任务非阻塞投递到 pool,满则跳过本次(防堆积)。
func (j *deviceJob) fire() {
	select {
	case j.taskCh <- collectTask{ctx: j.ctx, conn: j.conn, points: j.getPoints(), deviceID: j.deviceID}:
	default:
		slog.Warn("pool busy, skip collect", "device", j.deviceID)
	}
}

// intervalOf 把设备配置的毫秒间隔转成 Duration,非法值回落默认间隔。
func intervalOf(intervalMs int) time.Duration {
	interval := time.Duration(intervalMs) * time.Millisecond
	if interval <= 0 {
		interval = defaultInterval
	}
	return interval
}
