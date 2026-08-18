package output

import (
	"log/slog"
	"sync"
	"time"

	"iot-gateway-go/internal/backfill"
	"iot-gateway-go/internal/model"
)

// queueSize 是每个输出各自的缓冲队列长度:队列满时丢弃新数据点并告警(未启用补传时),
// 或经补传队列持久化(启用补传时),避免慢输出(如 MQTT 阻塞)反压回采集侧拖垮全局。
const queueSize = 1024

// 断网本地补传(见 docs/offline-backfill-design.md)的轮询参数。
// replayInterval 用 var 便于测试覆盖为极大值以禁用后台重放(测试自行驱动 replayOnce)。
var (
	replayInterval = time.Second
	replayBatch    = 256
)

// Instance 是一个已构建的输出及其配置身份。Manager 据此维护状态与配置的关联,
// 使运行态能按 outputId 对回 Web UI 里的输出记录。见 docs/output-status-design.md。
type Instance struct {
	Out     Output
	ID      string
	Name    string
	Type    string
	Enabled bool
}

// BuildFunc 按当前配置构建一组输出(由 main 注入:读 store 输出表 + 调 registry.Build)。
// 约定:返回 (nil, err) 时须自行关闭已构建的部分输出;返回的输出集由 Manager 接管生命周期。
type BuildFunc func() ([]Instance, error)

// Manager 维护当前活跃的输出集合,支持热重载(原子替换 + 旧输出优雅关闭),
// 并作为数据点的扇出点:每个输出独立队列 + 发布 goroutine,实现输出间解耦与背压隔离。
// 并发安全:Reload/Publish/Notifiers/Status/Close 可被多个 goroutine 同时调用。
type Manager struct {
	build BuildFunc

	// backfill 是断网补传持久化队列(经 SetBackfill 注入;nil 表示未接线,退化为旧丢弃行为)。
	backfill *backfill.Store

	mu    sync.Mutex
	slots []*slot

	replayDone chan struct{}
	replayWg   sync.WaitGroup
}

// slot 是一个活跃输出及其发布队列。
type slot struct {
	inst Instance
	ch   chan model.DataPoint
	done chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	received int64 // 发布循环取到的数据点数
	dropped  int64 // 扇出队列满丢弃数(仅未启用补传的输出计数)
}

// NewManager 构造输出管理器;build 用于按配置构建输出集,不得为 nil。
// 补传队列经 SetBackfill 注入;重放 goroutine 随构造启动(backfill 为空时自跳过)。
func NewManager(build BuildFunc) *Manager {
	m := &Manager{
		build:      build,
		replayDone: make(chan struct{}),
	}
	m.replayWg.Add(1)
	go m.runReplay()
	return m
}

// SetBackfill 注入断网补传持久化队列(启动时由 main 调用一次)。
func (m *Manager) SetBackfill(bs *backfill.Store) {
	m.mu.Lock()
	m.backfill = bs
	m.mu.Unlock()
}

// Reload 重建输出集合:构建成功则原子替换并关闭旧输出;失败则保留旧输出(返回错误)。
// 从配置中被删除的输出版本,其残留补传队列一并清掉;同 ID 重建则自动续传。
func (m *Manager) Reload() error {
	insts, err := m.build()
	if err != nil {
		return err
	}
	newSlots := make([]*slot, 0, len(insts))
	for _, in := range insts {
		newSlots = append(newSlots, startSlot(in))
	}

	m.mu.Lock()
	old := m.slots
	m.slots = newSlots
	m.mu.Unlock()

	for _, s := range old {
		s.stop()
	}
	m.dropRemoved(old, newSlots)
	return nil
}

// dropRemoved 清掉已从配置中删除的输出的残留补传队列(用户删除配置=放弃其缓冲数据)。
func (m *Manager) dropRemoved(old, current []*slot) {
	if m.backfill == nil {
		return
	}
	active := make(map[string]bool, len(current))
	for _, s := range current {
		active[s.inst.ID] = true
	}
	for _, s := range old {
		if active[s.inst.ID] {
			continue
		}
		n, _ := m.backfill.CountByOutput(s.inst.ID)
		if err := m.backfill.DropOutput(s.inst.ID); err != nil {
			slog.Error("backfill drop output failed", "output", s.inst.ID, "err", err)
		} else if n > 0 {
			slog.Info("backfill queue dropped for removed output", "output", s.inst.ID, "queued", n)
		}
	}
}

// Publish 把数据点扇出给所有活跃输出;队列满时,启用补传的输出经持久化队列保存,
// 未启用的保持旧语义(丢弃并计数)。
func (m *Manager) Publish(dp model.DataPoint) {
	m.mu.Lock()
	slots := m.slots
	backfill := m.backfill
	m.mu.Unlock()

	for _, s := range slots {
		select {
		case s.ch <- dp:
		default:
			if backfill != nil && s.supportsBackfill() {
				if err := backfill.Save(s.inst.ID, []model.DataPoint{dp}); err != nil {
					slog.Error("backfill save failed", "output", s.inst.ID, "err", err)
				}
			} else {
				s.mu.Lock()
				s.dropped++
				s.mu.Unlock()
				slog.Warn("output queue full, drop datapoint", "output", s.inst.ID)
			}
		}
	}
}

// supportsBackfill 判断输出是否声明支持断网补传(实现 BackfillHealthy 即声明)。
func (s *slot) supportsBackfill() bool {
	_, ok := s.inst.Out.(BackfillHealthy)
	return ok
}

// runReplay 周期驱动断网补传重放:对每个"支持补传且健康"的输出,把其持久化队列
// 中最旧的一批经 slot.ch 投递(复用 slot 单写 goroutine,保证并发安全与顺序),
// 成功投递即 Ack 删除。见 docs/offline-backfill-design.md §4.3。
func (m *Manager) runReplay() {
	defer m.replayWg.Done()
	ticker := time.NewTicker(replayInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.replayDone:
			return
		case <-ticker.C:
			m.replayOnce()
		}
	}
}

func (m *Manager) replayOnce() {
	m.mu.Lock()
	if m.backfill == nil {
		m.mu.Unlock()
		return
	}
	slots := m.slots
	backfill := m.backfill
	m.mu.Unlock()

	for _, s := range slots {
		if !s.supportsBackfill() {
			continue
		}
		if !s.inst.Out.(BackfillHealthy).BackfillHealthy() {
			continue
		}
		n, err := backfill.CountByOutput(s.inst.ID)
		if err != nil {
			slog.Error("backfill count failed", "output", s.inst.ID, "err", err)
			continue
		}
		if n == 0 {
			continue
		}
		items, err := backfill.Peek(s.inst.ID, replayBatch)
		if err != nil {
			slog.Error("backfill peek failed", "output", s.inst.ID, "err", err)
			continue
		}
		if len(items) == 0 {
			continue
		}

		acked := make([]int64, 0, len(items))
	replay:
		for _, it := range items {
			select {
			case s.ch <- it.DP:
				acked = append(acked, it.ID)
			default:
				// 队列满:背压,本轮停止,剩余留待下轮。
				break replay
			}
		}
		if len(acked) > 0 {
			if err := backfill.Ack(s.inst.ID, acked); err != nil {
				slog.Error("backfill ack failed", "output", s.inst.ID, "err", err)
			}
		}
	}
}

// Notifiers 返回当前输出中实现 DeviceNotifier 的子集(供 scheduler 上报设备上下线)。
func (m *Manager) Notifiers() []DeviceNotifier {
	m.mu.Lock()
	defer m.mu.Unlock()

	notifiers := make([]DeviceNotifier, 0, len(m.slots))
	for _, s := range m.slots {
		if n, ok := s.inst.Out.(DeviceNotifier); ok {
			notifiers = append(notifiers, n)
		}
	}
	return notifiers
}

// Status 返回当前活跃输出的运行态快照(接入侧指标 + 各输出上报的类型相关状态
// + 补传队列深度)。
func (m *Manager) Status() []OutputStatus {
	m.mu.Lock()
	slots := m.slots
	backfill := m.backfill
	m.mu.Unlock()

	out := make([]OutputStatus, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.status())
	}
	if backfill != nil {
		for i := range out {
			n, err := backfill.CountByOutput(out[i].OutputID)
			if err == nil {
				out[i].Backfill = n
			}
		}
	}
	return out
}

// OutputStatus 是单个输出的运行态快照(由 Manager 聚合,供 API/Web 观测)。
type OutputStatus struct {
	OutputID  string `json:"outputId"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Enabled   bool   `json:"enabled"`
	Active    bool   `json:"active"` // 当前是否已构建并运行
	QueueUsed int    `json:"queueUsed"`
	QueueCap  int    `json:"queueCap"`
	Received  int64  `json:"received"` // 成功投递给该输出发布循环的数据点数
	Dropped   int64  `json:"dropped"`  // 队列满丢弃的数据点数(未启用补传)
	Backfill  int    `json:"backfill"` // 持久化补传队列深度(见 docs/offline-backfill-design.md)
	RuntimeStatus
}

// status 汇总单个 slot 的接入侧指标与输出的类型相关状态。
func (s *slot) status() OutputStatus {
	st := OutputStatus{
		OutputID:  s.inst.ID,
		Name:      s.inst.Name,
		Type:      s.inst.Type,
		Enabled:   s.inst.Enabled,
		Active:    true,
		QueueUsed: len(s.ch),
		QueueCap:  cap(s.ch),
	}
	s.mu.Lock()
	st.Received = s.received
	st.Dropped = s.dropped
	s.mu.Unlock()
	if p, ok := s.inst.Out.(StatusProvider); ok {
		st.RuntimeStatus = p.RuntimeStatus()
	}
	return st
}

// Close 停止全部输出(进程退出时调用);幂等。
// 补传队列持久化在 SQLite,**不**清空——下次启动自动续传。
func (m *Manager) Close() {
	close(m.replayDone)
	m.replayWg.Wait()

	m.mu.Lock()
	slots := m.slots
	m.slots = nil
	m.mu.Unlock()

	for _, s := range slots {
		s.stop()
	}
}

// startSlot 启动一个输出的发布 goroutine:从队列取数据点并发布,直到 stop 关闭 done。
func startSlot(inst Instance) *slot {
	s := &slot{
		inst: inst,
		ch:   make(chan model.DataPoint, queueSize),
		done: make(chan struct{}),
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-s.done:
				return
			case dp := <-s.ch:
				s.mu.Lock()
				s.received++
				s.mu.Unlock()
				if err := inst.Out.Publish(dp); err != nil {
					slog.Error("publish datapoint failed", "output", inst.ID, "err", err)
				}
			}
		}
	}()
	return s
}

// stop 停止发布 goroutine 并关闭输出。
func (s *slot) stop() {
	close(s.done)
	s.wg.Wait() // 等发布 goroutine 退出后再关闭输出,避免并发 Publish
	if err := s.inst.Out.Close(); err != nil {
		slog.Error("close output failed", "output", s.inst.ID, "err", err)
	}
}
