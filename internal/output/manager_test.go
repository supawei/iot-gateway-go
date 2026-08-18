package output

import (
	"errors"
	"sync"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
)

// mockOut 用 channel 收集发布的数据点,并记录是否被关闭。
type mockOut struct {
	mu     sync.Mutex
	ch     chan model.DataPoint
	closed bool
}

func newMockOut() *mockOut {
	return &mockOut{ch: make(chan model.DataPoint, 16)}
}

func (m *mockOut) Publish(dp model.DataPoint) error {
	m.ch <- dp
	return nil
}

func (m *mockOut) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}

func (m *mockOut) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// notifierOut 实现 Output + DeviceNotifier。
type notifierOut struct {
	mockOut
	online  []string
	offline []string
}

func (n *notifierOut) DeviceOnline(id string)  { n.online = append(n.online, id) }
func (n *notifierOut) DeviceOffline(id string) { n.offline = append(n.offline, id) }

func dp(id string) model.DataPoint {
	return model.DataPoint{DeviceID: id, Point: "p", Value: 1, Quality: model.QualityGood}
}

func waitData(t *testing.T, ch <-chan model.DataPoint, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got.DeviceID != want {
			t.Fatalf("got device %q want %q", got.DeviceID, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for datapoint")
	}
}

func TestManagerReloadAndPublish(t *testing.T) {
	out := newMockOut()
	mgr := NewManager(func() ([]Instance, error) { return []Instance{{Out: out}}, nil })

	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	mgr.Publish(dp("d1"))
	waitData(t, out.ch, "d1")

	mgr.Close()
	if !out.isClosed() {
		t.Fatal("output should be closed after manager Close")
	}
}

func TestManagerReloadSwapsOutputs(t *testing.T) {
	outA := newMockOut()
	outB := newMockOut()
	current := outA
	mgr := NewManager(func() ([]Instance, error) { return []Instance{{Out: current}}, nil })

	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload A: %v", err)
	}
	mgr.Publish(dp("d1"))
	waitData(t, outA.ch, "d1")

	// 切换到 B:A 被关闭,数据流向 B
	current = outB
	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload B: %v", err)
	}
	if !outA.isClosed() {
		t.Fatal("old output A should be closed after swap")
	}
	mgr.Publish(dp("d2"))
	waitData(t, outB.ch, "d2")

	mgr.Close()
}

func TestManagerReloadErrorKeepsOld(t *testing.T) {
	outA := newMockOut()
	buildErr := false
	mgr := NewManager(func() ([]Instance, error) {
		if buildErr {
			return nil, errors.New("build failed")
		}
		return []Instance{{Out: outA}}, nil
	})

	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	buildErr = true
	if err := mgr.Reload(); err == nil {
		t.Fatal("expected reload error")
	}
	if outA.isClosed() {
		t.Fatal("old output should remain open after failed reload")
	}
	// 旧输出仍可用
	mgr.Publish(dp("d1"))
	waitData(t, outA.ch, "d1")

	mgr.Close()
}

func TestManagerNotifiers(t *testing.T) {
	plain := newMockOut()
	notifier := &notifierOut{mockOut: *newMockOut()}
	mgr := NewManager(func() ([]Instance, error) { return []Instance{{Out: plain}, {Out: notifier}}, nil })
	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer mgr.Close()

	notifiers := mgr.Notifiers()
	if len(notifiers) != 1 {
		t.Fatalf("want 1 notifier got %d", len(notifiers))
	}
	notifiers[0].DeviceOnline("d1")
	if len(notifier.online) != 1 || notifier.online[0] != "d1" {
		t.Fatalf("online notification: %v", notifier.online)
	}
}

// statusOut 实现 Output + StatusProvider,用于验证 Manager.Status 并入类型相关状态。
type statusOut struct {
	mockOut
	rt RuntimeStatus
}

func (s *statusOut) RuntimeStatus() RuntimeStatus { return s.rt }

// slowMock 的 Publish 固定阻塞 delay,使发布 goroutine 消费速率受限,
// 压测队列溢出时 drops 确定性发生(扇出计数在测试 goroutine 内同步完成)。
type slowMock struct {
	mockOut
	delay time.Duration
}

func (m *slowMock) Publish(dp model.DataPoint) error {
	time.Sleep(m.delay)
	return nil
}

// waitReceived 轮询 mgr.Status,直到指定输出 received >= want 或超时。
func waitReceived(t *testing.T, mgr *Manager, id string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, st := range mgr.Status() {
			if st.OutputID == id && st.Received >= want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("output %q received never reached %d", id, want)
}

// TestManagerStatus 验证 Status() 聚合:身份/队列指标 + 输出上报的类型相关状态。
func TestManagerStatus(t *testing.T) {
	plain := newMockOut()
	withStatus := &statusOut{
		mockOut: *newMockOut(),
		rt:      RuntimeStatus{Connected: true, ConnectionOpen: true, Pending: 3, Sent: 7, LastSentAt: time.Now()},
	}
	mgr := NewManager(func() ([]Instance, error) {
		return []Instance{
			{Out: plain, ID: "out-plain", Name: "plain", Type: "mock"},
			{Out: withStatus, ID: "out-st", Name: "st", Type: "mock", Enabled: true},
		}, nil
	})
	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer mgr.Close()

	mgr.Publish(dp("d1"))
	mgr.Publish(dp("d2"))
	waitReceived(t, mgr, "out-st", 2)
	waitReceived(t, mgr, "out-plain", 2)

	st := mgr.Status()
	if len(st) != 2 {
		t.Fatalf("status len = %d, want 2", len(st))
	}
	byID := make(map[string]OutputStatus, len(st))
	for _, s := range st {
		byID[s.OutputID] = s
	}

	p := byID["out-plain"]
	if !p.Active || p.QueueCap != queueSize || p.Enabled {
		t.Fatalf("plain status: %+v", p)
	}
	if p.Connected || p.ConnectionOpen || p.Sent != 0 {
		t.Fatalf("plain should have zero runtime status: %+v", p)
	}

	s := byID["out-st"]
	if !s.Active || !s.Enabled {
		t.Fatalf("st status identity: %+v", s)
	}
	if !s.Connected || !s.ConnectionOpen || s.Pending != 3 || s.Sent != 7 {
		t.Fatalf("st runtime status not merged: %+v", s)
	}
	if s.Received != 2 || s.Dropped != 0 {
		t.Fatalf("st counters: received=%d dropped=%d, want 2/0", s.Received, s.Dropped)
	}
}

// TestManagerStatusDropped 验证队列打满后 dropped 累计。
func TestManagerStatusDropped(t *testing.T) {
	// slowMock 每点阻塞 1ms,发布 goroutine 消费受限;灌入远超队列容量的数据点时,
	// 扇出同步走 default 分支计数 dropped(确定性),Close 时队列余量 ~1s 内排空。
	out := &slowMock{mockOut: *newMockOut(), delay: time.Millisecond}
	mgr := NewManager(func() ([]Instance, error) { return []Instance{{Out: out, ID: "out-1"}}, nil })
	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer mgr.Close()

	const burst = 2 * queueSize
	for i := 0; i < burst; i++ {
		mgr.Publish(dp("d1"))
	}

	var dropped int64
	for _, st := range mgr.Status() {
		if st.OutputID == "out-1" {
			dropped = st.Dropped
		}
	}
	if dropped == 0 {
		t.Fatal("expected dropped > 0 after queue overflow")
	}
	if dropped > burst-queueSize {
		t.Fatalf("dropped=%d unexpectedly high (burst=%d, queue=%d)", dropped, burst, queueSize)
	}
}
