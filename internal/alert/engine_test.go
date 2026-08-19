package alert

import (
	"sync"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/store"
)

// fakeOutput 记录收到的数据点与告警消息,用于断言扇出与定向投递。
type fakeOutput struct {
	mu     sync.Mutex
	points []model.DataPoint
	alerts []model.AlertMessage
}

func (f *fakeOutput) Publish(dp model.DataPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.points = append(f.points, dp)
	return nil
}

func (f *fakeOutput) PublishAlert(a model.AlertMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, a)
	return nil
}

func (f *fakeOutput) Close() error { return nil }

func (f *fakeOutput) Alerts() []model.AlertMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.AlertMessage, len(f.alerts))
	copy(out, f.alerts)
	return out
}

func dpf(deviceID, point string, value any) model.DataPoint {
	return model.DataPoint{DeviceID: deviceID, Point: point, Value: value, Timestamp: time.Now(), Quality: model.QualityGood}
}

func newTestEngine(t *testing.T, rules []model.AlertRule) (*Engine, *store.Store, *output.Manager, *fakeOutput) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, r := range rules {
		if r.CreatedAt == "" {
			r.CreatedAt, r.UpdatedAt = "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"
		}
		if err := st.SaveAlertRule(r); err != nil {
			t.Fatalf("save rule %q: %v", r.ID, err)
		}
	}
	fo := &fakeOutput{}
	mgr := output.NewManager(func() ([]output.Instance, error) {
		return []output.Instance{{Out: fo, ID: "out1", Name: "out1", Type: "mqtt", Enabled: true}}, nil
	})
	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload mgr: %v", err)
	}
	eng := NewEngine(st, mgr, "gw-test")
	if err := eng.reload(); err != nil {
		t.Fatalf("reload eng: %v", err)
	}
	return eng, st, mgr, fo
}

func assertAlertCount(t *testing.T, st *store.Store, want int) {
	t.Helper()
	alerts, err := st.ListAlerts("")
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	if len(alerts) != want {
		t.Fatalf("alert count = %d, want %d", len(alerts), want)
	}
}

func assertAlertStatus(t *testing.T, st *store.Store, want string) {
	t.Helper()
	alerts, err := st.ListAlerts("")
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	if len(alerts) == 0 {
		t.Fatalf("no alerts to check status")
	}
	if alerts[0].Status != want {
		t.Fatalf("status = %q, want %q", alerts[0].Status, want)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within timeout")
}

// TestSinglePointAlert 验证 N=1:触发 -> 去重 -> 解除。
func TestSinglePointAlert(t *testing.T) {
	rule := model.AlertRule{
		ID: "r1", Name: "高温", Enabled: true, Severity: "warning",
		Expr:             `point("d1","temp")>30`,
		ReferencedPoints: []model.RefPoint{{DeviceID: "d1", Point: "temp"}},
		OutputIDs:        []string{"out1"},
		FreshnessSeconds: 300,
	}
	eng, st, mgr, fo := newTestEngine(t, []model.AlertRule{rule})
	defer mgr.Close()

	eng.Process(dpf("d1", "temp", 31.0))
	assertAlertCount(t, st, 1)

	eng.Process(dpf("d1", "temp", 31.0)) // 仍 active,不重复
	assertAlertCount(t, st, 1)

	eng.Process(dpf("d1", "temp", 25.0)) // 条件不成立,解除
	assertAlertStatus(t, st, string(model.AlertResolved))

	waitFor(t, func() bool { return len(fo.Alerts()) == 1 })
}

// TestCrossDeviceAlert 验证 N>1:跨消息状态--先到的点位不触发,凑齐才触发。
func TestCrossDeviceAlert(t *testing.T) {
	rule := model.AlertRule{
		ID: "r2", Name: "高温且空调关", Enabled: true, Severity: "warning",
		Expr: `point("d1","temp")>30 && point("d2","sw")=="off"`,
		ReferencedPoints: []model.RefPoint{
			{DeviceID: "d1", Point: "temp"},
			{DeviceID: "d2", Point: "sw"},
		},
		OutputIDs:        []string{"out1"},
		FreshnessSeconds: 300,
	}
	eng, st, mgr, _ := newTestEngine(t, []model.AlertRule{rule})
	defer mgr.Close()

	eng.Process(dpf("d2", "sw", "off")) // d2 有值,d1 无,不触发
	assertAlertCount(t, st, 0)

	eng.Process(dpf("d1", "temp", 31.0)) // 两者齐全且满足,触发
	assertAlertCount(t, st, 1)
}

// TestFreshnessExpiry 验证引用点过期后不参与判断。
func TestFreshnessExpiry(t *testing.T) {
	rule := model.AlertRule{
		ID: "r3", Name: "高温且空调关", Enabled: true, Severity: "warning",
		Expr: `point("d1","temp")>30 && point("d2","sw")=="off"`,
		ReferencedPoints: []model.RefPoint{
			{DeviceID: "d1", Point: "temp"},
			{DeviceID: "d2", Point: "sw"},
		},
		OutputIDs:        []string{"out1"},
		FreshnessSeconds: 1, // 1 秒新鲜度
	}
	eng, st, mgr, _ := newTestEngine(t, []model.AlertRule{rule})
	defer mgr.Close()

	eng.Process(dpf("d1", "temp", 31.0))
	time.Sleep(1100 * time.Millisecond) // 等 d1 过期
	eng.Process(dpf("d2", "sw", "off")) // d1 已过期,不触发
	assertAlertCount(t, st, 0)
}

// TestCooldown 验证解除后 cooldown 内重触发被抑制。
func TestCooldown(t *testing.T) {
	rule := model.AlertRule{
		ID: "r4", Name: "高温", Enabled: true, Severity: "warning",
		Expr:             `point("d1","temp")>30`,
		ReferencedPoints: []model.RefPoint{{DeviceID: "d1", Point: "temp"}},
		OutputIDs:        []string{"out1"},
		FreshnessSeconds: 300,
		CooldownSeconds:   2,
	}
	eng, st, mgr, _ := newTestEngine(t, []model.AlertRule{rule})
	defer mgr.Close()

	eng.Process(dpf("d1", "temp", 31.0)) // 触发
	assertAlertCount(t, st, 1)
	eng.Process(dpf("d1", "temp", 25.0)) // 解除
	eng.Process(dpf("d1", "temp", 31.0)) // cooldown 内重触发,抑制
	assertAlertCount(t, st, 1)

	time.Sleep(2100 * time.Millisecond) // 等 cooldown 过
	eng.Process(dpf("d1", "temp", 31.0)) // 重触发
	assertAlertCount(t, st, 2)
}

// TestNoRulePassthrough 验证无规则引用的点位直接扇出、零告警。
func TestNoRulePassthrough(t *testing.T) {
	eng, st, mgr, fo := newTestEngine(t, nil)
	defer mgr.Close()

	eng.Process(dpf("d9", "x", 1))
	assertAlertCount(t, st, 0)
	waitFor(t, func() bool { return len(fo.Alerts()) == 0 && alertReceived(fo) })
}

func alertReceived(fo *fakeOutput) bool {
	fo.mu.Lock()
	defer fo.mu.Unlock()
	return len(fo.points) > 0
}
