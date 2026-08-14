package core

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/store"
)

const defaultInterval = 5 * time.Second

// Scheduler 用 cron 统一调度采集任务,任务投递到 worker pool 执行。
// 常驻 goroutine = cron 调度器 + poolSize 个 worker,与设备数解耦;
// 配置变更时全量重载(停旧调度与 pool、按新配置重建)。
type Scheduler struct {
	store         *store.Store
	output        chan<- model.DataPoint
	status        *status.Registry
	poolSize      int
	baseCtx       context.Context
	mu            sync.Mutex
	cron          *cron.Cron
	conns         []driver.Conn
	taskCh        chan collectTask
	workersDone   <-chan struct{}
	collectCancel context.CancelFunc
}

func NewScheduler(st *store.Store, dataPoints chan<- model.DataPoint, poolSize int, statusReg *status.Registry) *Scheduler {
	if poolSize <= 0 {
		poolSize = 16
	}
	return &Scheduler{store: st, output: dataPoints, poolSize: poolSize, status: statusReg}
}

func (s *Scheduler) Run(ctx context.Context) error {
	s.baseCtx = ctx
	if err := s.reload(); err != nil {
		// 首次 reload 失败不退出调度器:等下一次 OnChange 重试(API 仍可修复配置)。
		slog.Error("initial reload failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			s.stopCollectors()
			return ctx.Err()
		case <-s.store.OnChange():
			if err := s.reload(); err != nil {
				slog.Error("scheduler reload failed", "err", err)
			}
		}
	}
}

func (s *Scheduler) reload() error {
	s.stopCollectors()

	collectCtx, cancel := context.WithCancel(s.baseCtx)
	taskCh := make(chan collectTask, s.poolSize)
	s.mu.Lock()
	s.collectCancel = cancel
	s.taskCh = taskCh
	s.workersDone = s.startWorkers(taskCh)
	s.mu.Unlock()

	devices, err := s.store.ListDevices()
	if err != nil {
		slog.Error("list devices failed", "err", err)
		s.stopCollectors()
		return err
	}

	// 并行打开设备连接(受 poolSize 限流),避免串行连接超时拖慢启动/热加载。
	type openedDevice struct {
		device model.Device
		conn   driver.Conn
	}
	var (
		openMu sync.Mutex
		opened []openedDevice
		wg     sync.WaitGroup
		sem    = make(chan struct{}, s.poolSize)
	)
	for _, device := range devices {
		if !device.Enabled || len(device.Points) == 0 {
			continue
		}
		wg.Add(1)
		go func(d model.Device) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			conn, ok := s.openDevice(collectCtx, d)
			if !ok {
				return
			}
			openMu.Lock()
			opened = append(opened, openedDevice{d, conn})
			openMu.Unlock()
		}(device)
	}
	wg.Wait()

	// 顺序注册采集方式(订阅/监听/cron);注册逻辑轻量,cron AddJob 须在 Start 前完成。
	c := cron.New()
	for _, od := range opened {
		device, conn := od.device, od.conn
		// 订阅模式:网关主动订阅,数据变化即推送。
		if sub, isSub := conn.(driver.Subscriber); isSub {
			if err := sub.Subscribe(collectCtx, device.Points, func(dp model.DataPoint) {
				s.pushData(collectCtx, device.ID, dp)
			}); err != nil {
				slog.Error("subscribe failed", "device", device.ID, "err", err)
				s.status.SetOffline(device.ID, err.Error())
				continue
			}
			s.status.SetOnline(device.ID, time.Now())
			continue
		}
		// 监听模式:网关被动 listen,设备连入上报数据即推送。
		if lis, isListen := conn.(driver.Listener); isListen {
			if err := lis.Listen(collectCtx, device.Points, func(dp model.DataPoint) {
				s.pushData(collectCtx, device.ID, dp)
			}); err != nil {
				slog.Error("listen failed", "device", device.ID, "err", err)
				s.status.SetOffline(device.ID, err.Error())
				continue
			}
			s.status.SetOnline(device.ID, time.Now())
			continue
		}
		job := &deviceJob{
			taskCh:   taskCh,
			conn:     conn,
			points:   device.Points,
			deviceID: device.ID,
			ctx:      collectCtx,
		}
		if _, err := c.AddJob(intervalSpec(device.IntervalMs), job); err != nil {
			slog.Error("add cron job failed", "device", device.ID, "err", err)
			s.status.SetOffline(device.ID, err.Error())
		}
	}

	s.mu.Lock()
	s.cron = c
	s.mu.Unlock()
	c.Start()
	return nil
}

// stopCollectors 按"cancel Read -> 停 cron -> 关 taskCh -> 等 workers 退出 -> Close conns"顺序清理,
// 保证关连接时无并发 Read:cron.Stop 后无 job 投递,workers 退出后无 Read 在执行。
func (s *Scheduler) stopCollectors() {
	s.mu.Lock()
	cancel := s.collectCancel
	c := s.cron
	conns := s.conns
	taskCh := s.taskCh
	workersDone := s.workersDone
	s.collectCancel = nil
	s.cron = nil
	s.conns = nil
	s.taskCh = nil
	s.workersDone = nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	if c != nil {
		<-c.Stop().Done()
	}
	if taskCh != nil {
		close(taskCh)
	}
	if workersDone != nil {
		<-workersDone
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			slog.Error("close device connection failed", "err", err)
		}
	}
}

func (s *Scheduler) openDevice(ctx context.Context, device model.Device) (driver.Conn, bool) {
	connection, err := s.store.GetConnection(device.ConnectionID)
	if err != nil {
		slog.Error("get connection failed", "device", device.ID, "connection", device.ConnectionID, "err", err)
		s.status.SetOffline(device.ID, "get connection failed: "+err.Error())
		return nil, false
	}
	drv, err := driver.Get(connection.Driver)
	if err != nil {
		slog.Error("driver not registered", "device", device.ID, "driver", connection.Driver, "err", err)
		s.status.SetOffline(device.ID, err.Error())
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
		s.status.SetOffline(device.ID, "open failed: "+err.Error())
		return nil, false
	}
	s.mu.Lock()
	s.conns = append(s.conns, conn)
	s.mu.Unlock()
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

// pushData 把推送式(订阅/监听)采集到的 DataPoint 投递到输出,并刷新设备在线状态。
func (s *Scheduler) pushData(ctx context.Context, deviceID string, dp model.DataPoint) {
	s.status.SetOnline(deviceID, time.Now())
	s.emit(ctx, dp)
}

// emit 把单个 DataPoint 投递到输出 channel;ctx 取消时返回 false。
// 轮询、订阅、监听三条采集路径共用此投递入口。
func (s *Scheduler) emit(ctx context.Context, dp model.DataPoint) bool {
	select {
	case s.output <- dp:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Scheduler) collectOnce(ctx context.Context, conn driver.Conn, points []model.Point, deviceID string) {
	dataPoints, err := conn.Read(ctx, points)
	if err != nil {
		// 配置级错误(整批无效):记录离线,不发送
		s.status.SetOffline(deviceID, err.Error())
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
		s.status.SetOnline(deviceID, time.Now())
	} else {
		s.status.SetOffline(deviceID, "all points bad")
	}
}

type collectTask struct {
	ctx      context.Context
	conn     driver.Conn
	points   []model.Point
	deviceID string
}

type deviceJob struct {
	taskCh   chan collectTask
	conn     driver.Conn
	points   []model.Point
	deviceID string
	ctx      context.Context
}

// Run 到达采集周期时触发:把采集任务非阻塞投递到 pool,满则跳过本次(防堆积)。
func (j *deviceJob) Run() {
	select {
	case j.taskCh <- collectTask{ctx: j.ctx, conn: j.conn, points: j.points, deviceID: j.deviceID}:
	default:
		slog.Warn("pool busy, skip collect", "device", j.deviceID)
	}
}

func intervalSpec(intervalMs int) string {
	interval := time.Duration(intervalMs) * time.Millisecond
	if interval <= 0 {
		interval = defaultInterval
	}
	return "@every " + interval.String()
}
