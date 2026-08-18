package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/store"
	"iot-gateway-go/internal/values"
)

// ---- 可追踪调用次数的假驱动/连接 ----

type trackingPollDriver struct {
	mu     sync.Mutex
	opens  int
	closes int
}

func (d *trackingPollDriver) Open(_ context.Context, req driver.OpenRequest) (driver.Conn, error) {
	d.mu.Lock()
	d.opens++
	d.mu.Unlock()
	return &trackingPollConn{deviceID: req.DeviceID, drv: d}, nil
}

func (d *trackingPollDriver) counters() (opens, closes int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.opens, d.closes
}

type trackingPollConn struct {
	deviceID string
	drv      *trackingPollDriver
}

func (c *trackingPollConn) Read(_ context.Context, points []model.Point) ([]model.DataPoint, error) {
	out := make([]model.DataPoint, len(points))
	now := time.Now()
	for i, p := range points {
		out[i] = model.DataPoint{DeviceID: c.deviceID, Point: p.Name, Value: 1, Timestamp: now, Quality: model.QualityGood}
	}
	return out, nil
}

func (c *trackingPollConn) Close() error {
	c.drv.mu.Lock()
	c.drv.closes++
	c.drv.mu.Unlock()
	return nil
}

type trackingSubDriver struct {
	mu     sync.Mutex
	opens  int
	closes int
}

func (d *trackingSubDriver) Open(_ context.Context, req driver.OpenRequest) (driver.Conn, error) {
	d.mu.Lock()
	d.opens++
	d.mu.Unlock()
	return &trackingSubConn{deviceID: req.DeviceID, drv: d}, nil
}

func (d *trackingSubDriver) counters() (opens, closes int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.opens, d.closes
}

type trackingSubConn struct {
	deviceID string
	drv      *trackingSubDriver
}

func (c *trackingSubConn) Read(_ context.Context, _ []model.Point) ([]model.DataPoint, error) {
	return nil, nil
}

func (c *trackingSubConn) Close() error {
	c.drv.mu.Lock()
	c.drv.closes++
	c.drv.mu.Unlock()
	return nil
}

func (c *trackingSubConn) Subscribe(_ context.Context, points []model.Point, onData func(model.DataPoint)) error {
	for _, p := range points {
		onData(model.DataPoint{DeviceID: c.deviceID, Point: p.Name, Value: 1, Timestamp: time.Now(), Quality: model.QualityGood})
	}
	return nil
}

// ---- 测试基础设施 ----

// incScheduler 构造直接驱动 reload 的调度器(不启动 Run 循环,baseCtx 手动设置)。
func incScheduler(t *testing.T, st *store.Store) *Scheduler {
	t.Helper()
	dataPoints := make(chan model.DataPoint, 64)
	s := NewScheduler(st, dataPoints, 4, status.NewRegistry(), values.NewRegistry(), nil)
	s.baseCtx = context.Background()
	t.Cleanup(s.stopCollectors)
	return s
}

func saveConn(t *testing.T, st *store.Store, id, drv string, cfg string) {
	t.Helper()
	if err := st.SaveConnection(model.Connection{ID: id, Name: id, Driver: drv, Config: []byte(cfg)}); err != nil {
		t.Fatalf("save connection %s: %v", id, err)
	}
}

func saveDev(t *testing.T, st *store.Store, d model.Device) {
	t.Helper()
	if err := st.SaveDevice(d); err != nil {
		t.Fatalf("save device %s: %v", d.ID, err)
	}
}

func pollDevice(id, connID string, params string, interval int, points ...string) model.Device {
	pts := make([]model.Point, 0, len(points))
	for _, p := range points {
		pts = append(pts, model.Point{Name: p, Address: p, DataType: model.DataTypeInt16})
	}
	return model.Device{ID: id, ConnectionID: connID, Params: []byte(params), IntervalMs: interval, Enabled: true, Points: pts}
}

// ---- 测试 ----

// TestIncrementalNoChangeKeepsEverything 无变化 reload 零操作:连接对象不变、无 Open/Close。
func TestIncrementalNoChangeKeepsEverything(t *testing.T) {
	drv := &trackingPollDriver{}
	driver.Register("inc-poll", drv)

	st, _ := store.Open(":memory:")
	defer st.Close()
	saveConn(t, st, "c1", "inc-poll", `{"addr":"x"}`)
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 3600_000, "p1"))

	s := incScheduler(t, st)
	if err := s.reload(); err != nil {
		t.Fatalf("reload1: %v", err)
	}
	rt1 := s.runtimes["d1"]
	if rt1 == nil {
		t.Fatal("d1 not scheduled")
	}
	o1, c1 := drv.counters()
	if o1 != 1 || c1 != 0 {
		t.Fatalf("after first reload: opens=%d closes=%d, want 1/0", o1, c1)
	}

	// 相同配置再次 reload(模拟输出/网关 ID 等无关变更触发的 OnChange)。
	if err := s.reload(); err != nil {
		t.Fatalf("reload2: %v", err)
	}
	if s.runtimes["d1"] != rt1 {
		t.Fatal("unchanged device runtime should be kept (same pointer)")
	}
	o2, c2 := drv.counters()
	if o2 != 1 || c2 != 0 {
		t.Fatalf("no-change reload caused churn: opens=%d closes=%d, want 1/0", o2, c2)
	}
}

// TestIncrementalAddDeviceOnly 新增设备只打开新设备,既有连接不动。
func TestIncrementalAddDeviceOnly(t *testing.T) {
	drv := &trackingPollDriver{}
	driver.Register("inc-poll-add", drv)

	st, _ := store.Open(":memory:")
	defer st.Close()
	saveConn(t, st, "c1", "inc-poll-add", `{}`)
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 3600_000, "p1"))
	s := incScheduler(t, st)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	rt1 := s.runtimes["d1"]

	saveDev(t, st, pollDevice("d2", "c1", `{}`, 3600_000, "p1"))
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if s.runtimes["d1"] != rt1 {
		t.Fatal("existing device conn must be kept on add")
	}
	if s.runtimes["d2"] == nil {
		t.Fatal("new device not scheduled")
	}
	o, c := drv.counters()
	if o != 2 || c != 0 {
		t.Fatalf("opens=%d closes=%d, want 2/0 (only new opened)", o, c)
	}
}

// TestIncrementalRemoveDevice 删除设备只关该设备连接。
func TestIncrementalRemoveDevice(t *testing.T) {
	drv := &trackingPollDriver{}
	driver.Register("inc-poll-rm", drv)

	st, _ := store.Open(":memory:")
	defer st.Close()
	saveConn(t, st, "c1", "inc-poll-rm", `{}`)
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 3600_000, "p1"))
	saveDev(t, st, pollDevice("d2", "c1", `{}`, 3600_000, "p1"))
	s := incScheduler(t, st)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	rt1 := s.runtimes["d1"]

	if err := st.DeleteDevice("d2"); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.runtimes["d2"]; ok {
		t.Fatal("removed device still scheduled")
	}
	if s.runtimes["d1"] != rt1 {
		t.Fatal("surviving device conn must be kept")
	}
	o, c := drv.counters()
	if o != 2 || c != 1 {
		t.Fatalf("opens=%d closes=%d, want 2/1 (only removed closed)", o, c)
	}
}

// TestIncrementalPollPointChange 轮询点位变化原地更新,不重连。
func TestIncrementalPollPointChange(t *testing.T) {
	drv := &trackingPollDriver{}
	driver.Register("inc-poll-point", drv)

	st, _ := store.Open(":memory:")
	defer st.Close()
	saveConn(t, st, "c1", "inc-poll-point", `{}`)
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 3600_000, "p1"))
	s := incScheduler(t, st)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	rt1 := s.runtimes["d1"]

	// 追加点位 p2。
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 3600_000, "p1", "p2"))
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if s.runtimes["d1"] != rt1 {
		t.Fatal("point change must not reopen conn")
	}
	pts := rt1.job.getPoints()
	if len(pts) != 2 || pts[1].Name != "p2" {
		t.Fatalf("job points not updated: %+v", pts)
	}
	o, c := drv.counters()
	if o != 1 || c != 0 {
		t.Fatalf("point change caused churn: opens=%d closes=%d, want 1/0", o, c)
	}
}

// TestIncrementalPollIntervalChange 间隔变化替换 cron 条目,不重连。
func TestIncrementalPollIntervalChange(t *testing.T) {
	drv := &trackingPollDriver{}
	driver.Register("inc-poll-int", drv)

	st, _ := store.Open(":memory:")
	defer st.Close()
	saveConn(t, st, "c1", "inc-poll-int", `{}`)
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 1000, "p1"))
	s := incScheduler(t, st)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	rt1 := s.runtimes["d1"]

	saveDev(t, st, pollDevice("d1", "c1", `{}`, 2000, "p1"))
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if s.runtimes["d1"] != rt1 {
		t.Fatal("interval change must not reopen conn")
	}
	if rt1.intervalMs != 2000 {
		t.Fatalf("intervalMs = %d, want 2000", rt1.intervalMs)
	}
	o, c := drv.counters()
	if o != 1 || c != 0 {
		t.Fatalf("interval change caused churn: opens=%d closes=%d, want 1/0", o, c)
	}
}

// TestIncrementalPollParamChange 参数变化重开设备 conn(驱动池复用物理连接)。
func TestIncrementalPollParamChange(t *testing.T) {
	drv := &trackingPollDriver{}
	driver.Register("inc-poll-param", drv)

	st, _ := store.Open(":memory:")
	defer st.Close()
	saveConn(t, st, "c1", "inc-poll-param", `{}`)
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 3600_000, "p1"))
	s := incScheduler(t, st)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}

	saveDev(t, st, pollDevice("d1", "c1", `{"slaveId":2}`, 3600_000, "p1"))
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	o, c := drv.counters()
	if o != 2 || c != 1 {
		t.Fatalf("param change: opens=%d closes=%d, want 2/1 (reopen)", o, c)
	}
}

// TestIncrementalConnectionConfigChange 连接配置变化只重开该连接组,其他连接不动。
func TestIncrementalConnectionConfigChange(t *testing.T) {
	drv := &trackingPollDriver{}
	driver.Register("inc-poll-conn", drv)

	st, _ := store.Open(":memory:")
	defer st.Close()
	saveConn(t, st, "c1", "inc-poll-conn", `{"addr":"a"}`)
	saveConn(t, st, "c2", "inc-poll-conn", `{"addr":"b"}`)
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 3600_000, "p1"))
	saveDev(t, st, pollDevice("d2", "c2", `{}`, 3600_000, "p1"))
	s := incScheduler(t, st)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	rt2 := s.runtimes["d2"]

	// c1 连接配置变化 → connKey 变,只有 d1 重开。
	saveConn(t, st, "c1", "inc-poll-conn", `{"addr":"a2"}`)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if s.runtimes["d2"] != rt2 {
		t.Fatal("other connection device must be kept")
	}
	o, c := drv.counters()
	if o != 3 || c != 1 {
		t.Fatalf("connection change: opens=%d closes=%d, want 3/1 (only c1 group restarted)", o, c)
	}
}

// TestIncrementalSubscribeAddOnly 订阅组纯新增是增量的,既有订阅连接不重开。
func TestIncrementalSubscribeAddOnly(t *testing.T) {
	drv := &trackingSubDriver{}
	driver.Register("inc-sub-add", drv)

	st, _ := store.Open(":memory:")
	defer st.Close()
	saveConn(t, st, "c1", "inc-sub-add", `{}`)
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 3600_000, "p1"))
	s := incScheduler(t, st)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	rt1 := s.runtimes["d1"]
	if rt1 == nil || rt1.mode != collectSubscribe {
		t.Fatalf("d1 mode = %v, want subscribe", rt1.mode)
	}

	saveDev(t, st, pollDevice("d2", "c1", `{}`, 3600_000, "p1"))
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if s.runtimes["d1"] != rt1 {
		t.Fatal("subscribe add must not reopen existing device")
	}
	if s.runtimes["d2"] == nil || s.runtimes["d2"].mode != collectSubscribe {
		t.Fatal("new subscribe device not registered")
	}
	o, c := drv.counters()
	if o != 2 || c != 0 {
		t.Fatalf("subscribe add: opens=%d closes=%d, want 2/0", o, c)
	}
}

// TestIncrementalSubscribeGroupRestart 订阅组内删除设备 → 整组重开,保证正确清理。
func TestIncrementalSubscribeGroupRestart(t *testing.T) {
	drv := &trackingSubDriver{}
	driver.Register("inc-sub-rm", drv)

	st, _ := store.Open(":memory:")
	defer st.Close()
	saveConn(t, st, "c1", "inc-sub-rm", `{}`)
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 3600_000, "p1"))
	saveDev(t, st, pollDevice("d2", "c1", `{}`, 3600_000, "p1"))
	s := incScheduler(t, st)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteDevice("d2"); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.runtimes["d2"]; ok {
		t.Fatal("removed subscribe device still scheduled")
	}
	// 整组重开:d1 也重开(d1 conn 是新的),d2 关闭。
	o, c := drv.counters()
	if o != 3 || c != 2 {
		t.Fatalf("subscribe removal: opens=%d closes=%d, want 3/2 (group restart)", o, c)
	}
}

// TestIncrementalOutputChangeNoDeviceOp 只改输出配置(设备不变)→ 调度器零操作。
func TestIncrementalOutputChangeNoDeviceOp(t *testing.T) {
	drv := &trackingPollDriver{}
	driver.Register("inc-poll-out", drv)

	st, _ := store.Open(":memory:")
	defer st.Close()
	saveConn(t, st, "c1", "inc-poll-out", `{}`)
	saveDev(t, st, pollDevice("d1", "c1", `{}`, 3600_000, "p1"))
	s := incScheduler(t, st)
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	rt1 := s.runtimes["d1"]

	if err := st.SaveOutput(model.Output{ID: "o1", Name: "out", Type: "mqtt", Config: []byte(`{}`), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	if s.runtimes["d1"] != rt1 {
		t.Fatal("output change must not touch scheduler devices")
	}
	o, c := drv.counters()
	if o != 1 || c != 0 {
		t.Fatalf("output change caused churn: opens=%d closes=%d, want 1/0", o, c)
	}
}
