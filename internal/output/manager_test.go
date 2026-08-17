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
	mgr := NewManager(func() ([]Output, error) { return []Output{out}, nil })

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
	mgr := NewManager(func() ([]Output, error) { return []Output{current}, nil })

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
	mgr := NewManager(func() ([]Output, error) {
		if buildErr {
			return nil, errors.New("build failed")
		}
		return []Output{outA}, nil
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
	mgr := NewManager(func() ([]Output, error) { return []Output{plain, notifier}, nil })
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
