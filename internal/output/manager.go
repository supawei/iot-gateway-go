package output

import (
	"log/slog"
	"sync"

	"iot-gateway-go/internal/model"
)

// queueSize 是每个输出各自的缓冲队列长度:队列满时丢弃新数据点并告警,
// 避免慢输出(如 MQTT 阻塞)反压回采集侧拖垮全局(与旧 pipeline 语义一致)。
const queueSize = 1024

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

	mu    sync.Mutex
	slots []*slot
}

// slot 是一个活跃输出及其发布队列。
type slot struct {
	inst Instance
	ch   chan model.DataPoint
	done chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	received int64 // 发布循环取到的数据点数
	dropped  int64 // 扇出队列满丢弃数
}

// NewManager 构造输出管理器;build 用于按配置构建输出集,不得为 nil。
func NewManager(build BuildFunc) *Manager {
	return &Manager{build: build}
}

// Reload 重建输出集合:构建成功则原子替换并关闭旧输出;失败则保留旧输出(返回错误)。
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
	return nil
}

// Publish 把数据点扇出给所有活跃输出;队列满则丢弃并计数(与旧 pipeline 语义一致)。
func (m *Manager) Publish(dp model.DataPoint) {
	m.mu.Lock()
	slots := m.slots
	m.mu.Unlock()

	for _, s := range slots {
		select {
		case s.ch <- dp:
		default:
			s.mu.Lock()
			s.dropped++
			s.mu.Unlock()
			slog.Warn("output queue full, drop datapoint", "output", s.inst.ID)
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

// Status 返回当前活跃输出的运行态快照(接入侧指标 + 各输出上报的类型相关状态)。
func (m *Manager) Status() []OutputStatus {
	m.mu.Lock()
	slots := m.slots
	m.mu.Unlock()

	out := make([]OutputStatus, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.status())
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
	Dropped   int64  `json:"dropped"`  // 队列满丢弃的数据点数
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
func (m *Manager) Close() {
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
