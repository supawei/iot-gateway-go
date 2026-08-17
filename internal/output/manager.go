package output

import (
	"log/slog"
	"sync"

	"iot-gateway-go/internal/model"
)

// queueSize 是每个输出各自的缓冲队列长度:队列满时丢弃新数据点并告警,
// 避免慢输出(如 MQTT 阻塞)反压回采集侧拖垮全局(与旧 pipeline 语义一致)。
const queueSize = 1024

// BuildFunc 按当前配置构建一组输出(由 main 注入:读 store 输出表 + 调 registry.Build)。
// 约定:返回 (nil, err) 时须自行关闭已构建的部分输出;返回的输出集由 Manager 接管生命周期。
type BuildFunc func() ([]Output, error)

// Manager 维护当前活跃的输出集合,支持热重载(原子替换 + 旧输出优雅关闭),
// 并作为数据点的扇出点:每个输出独立队列 + 发布 goroutine,实现输出间解耦与背压隔离。
// 并发安全:Reload/Publish/Notifiers/Close 可被多个 goroutine 同时调用。
type Manager struct {
	build BuildFunc

	mu    sync.Mutex
	slots []*slot
}

// slot 是一个活跃输出及其发布队列。
type slot struct {
	out  Output
	ch   chan model.DataPoint
	done chan struct{}
	wg   sync.WaitGroup
}

// NewManager 构造输出管理器;build 用于按配置构建输出集,不得为 nil。
func NewManager(build BuildFunc) *Manager {
	return &Manager{build: build}
}

// Reload 重建输出集合:构建成功则原子替换并关闭旧输出;失败则保留旧输出(返回错误)。
func (m *Manager) Reload() error {
	outs, err := m.build()
	if err != nil {
		return err
	}
	newSlots := make([]*slot, 0, len(outs))
	for _, o := range outs {
		newSlots = append(newSlots, startSlot(o))
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

// Publish 把数据点扇出给所有活跃输出;队列满则丢弃(与旧 pipeline 语义一致)。
func (m *Manager) Publish(dp model.DataPoint) {
	m.mu.Lock()
	slots := m.slots
	m.mu.Unlock()

	for _, s := range slots {
		select {
		case s.ch <- dp:
		default:
			slog.Warn("output queue full, drop datapoint", "device", dp.DeviceID, "point", dp.Point)
		}
	}
}

// Notifiers 返回当前输出中实现 DeviceNotifier 的子集(供 scheduler 上报设备上下线)。
func (m *Manager) Notifiers() []DeviceNotifier {
	m.mu.Lock()
	defer m.mu.Unlock()

	notifiers := make([]DeviceNotifier, 0, len(m.slots))
	for _, s := range m.slots {
		if n, ok := s.out.(DeviceNotifier); ok {
			notifiers = append(notifiers, n)
		}
	}
	return notifiers
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
func startSlot(o Output) *slot {
	s := &slot{
		out:  o,
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
				if err := o.Publish(dp); err != nil {
					slog.Error("publish datapoint failed", "err", err)
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
	if err := s.out.Close(); err != nil {
		slog.Error("close output failed", "err", err)
	}
}
