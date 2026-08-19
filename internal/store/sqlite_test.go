package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"iot-gateway-go/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestDSNPragmas 验证并发相关的 PRAGMA 注入逻辑。
func TestDSNPragmas(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"gateway.db", "file:gateway.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"},
		{"/data/gateway.db", "file:/data/gateway.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"},
		{":memory:", ":memory:"},
		{"file:existing.db?_pragma=journal_mode(MEMORY)", "file:existing.db?_pragma=journal_mode(MEMORY)"},
	}
	for _, tc := range cases {
		if got := dsnWithPragmas(tc.in); got != tc.want {
			t.Errorf("dsnWithPragmas(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func sampleConnection() model.Connection {
	return model.Connection{
		ID:     "conn-1",
		Name:   "conn-1",
		Driver: "modbus",
		Config: []byte(`{"mode":"tcp","address":"127.0.0.1:502"}`),
	}
}

func saveSampleConnection(t *testing.T, st *Store) {
	t.Helper()
	if err := st.SaveConnection(sampleConnection()); err != nil {
		t.Fatalf("save connection: %v", err)
	}
}

func sampleDevice(id string) model.Device {
	return model.Device{
		ID:           id,
		Name:         id,
		ConnectionID: "conn-1",
		Params:       []byte(`{"slaveId":1}`),
		IntervalMs:   1000,
		Enabled:      true,
		Points: []model.Point{
			{Name: "temperature", Address: "holding:0", DataType: model.DataTypeInt16, Scale: 0.1},
			{Name: "humidity", Address: "holding:1", DataType: model.DataTypeInt16, Scale: 0.1},
		},
	}
}

func TestSaveAndGetConnection(t *testing.T) {
	st := newTestStore(t)
	if err := st.SaveConnection(sampleConnection()); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetConnection("conn-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Driver != "modbus" || string(got.Config) == "" {
		t.Fatalf("unexpected connection: %+v", got)
	}
}

func TestListConnections(t *testing.T) {
	st := newTestStore(t)
	st.SaveConnection(sampleConnection())
	conns, err := st.ListConnections()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("want 1 connection got %d", len(conns))
	}
}

func TestDeleteConnectionBlockedByDevice(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)
	if err := st.SaveDevice(sampleDevice("d1")); err != nil {
		t.Fatalf("save device: %v", err)
	}
	if err := st.DeleteConnection("conn-1"); !errors.Is(err, ErrConnectionInUse) {
		t.Fatalf("expected ErrConnectionInUse got %v", err)
	}
}

func TestSaveAndGetDevice(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)
	if err := st.SaveDevice(sampleDevice("sensor-01")); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetDevice("sensor-01")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Points) != 2 || !got.Enabled || got.ConnectionID != "conn-1" {
		t.Fatalf("unexpected device: %+v", got)
	}
}

func TestListDevices(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)
	st.SaveDevice(sampleDevice("d1"))
	st.SaveDevice(sampleDevice("d2"))
	devices, err := st.ListDevices()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("want 2 devices got %d", len(devices))
	}
}

func TestSaveDeviceReplacesPoints(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)
	st.SaveDevice(sampleDevice("d1"))
	replaced := sampleDevice("d1")
	replaced.Points = []model.Point{{Name: "pressure", Address: "holding:2", DataType: model.DataTypeFloat}}
	st.SaveDevice(replaced)
	got, _ := st.GetDevice("d1")
	if len(got.Points) != 1 || got.Points[0].Name != "pressure" {
		t.Fatalf("points not replaced: %+v", got.Points)
	}
}

func TestDeleteDevice(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)
	st.SaveDevice(sampleDevice("d1"))
	if err := st.DeleteDevice("d1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetDevice("d1"); err == nil {
		t.Fatal("expected not found after delete")
	}
}

// TestManagedByRoundTrip managed_by 标记随 Save/Get 持久化;手工配置的实体默认空串。
func TestManagedByRoundTrip(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)

	conn := sampleConnection()
	conn.ManagedBy = "smardaten:out-1"
	if err := st.SaveConnection(conn); err != nil {
		t.Fatalf("save connection: %v", err)
	}
	got, err := st.GetConnection("conn-1")
	if err != nil {
		t.Fatalf("get connection: %v", err)
	}
	if got.ManagedBy != "smardaten:out-1" {
		t.Errorf("connection ManagedBy = %q, want smardaten:out-1", got.ManagedBy)
	}

	dev := sampleDevice("d-managed")
	dev.ManagedBy = "smardaten:out-1"
	if err := st.SaveDevice(dev); err != nil {
		t.Fatalf("save device: %v", err)
	}
	gotDev, err := st.GetDevice("d-managed")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if gotDev.ManagedBy != "smardaten:out-1" {
		t.Errorf("device ManagedBy = %q, want smardaten:out-1", gotDev.ManagedBy)
	}

	// 手工配置(未设 ManagedBy)持久化后为空串
	manual := sampleDevice("d-manual")
	if err := st.SaveDevice(manual); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.GetDevice("d-manual"); d.ManagedBy != "" {
		t.Errorf("manual device ManagedBy = %q, want empty", d.ManagedBy)
	}
}

// TestListManagedIDs ListManaged* 只返回指定 manager 创建管理的实体 ID,
// 不同 manager(不同 smardaten 输出实例)互不干扰,手工配置不在结果中。
func TestListManagedIDs(t *testing.T) {
	st := newTestStore(t)

	c1 := sampleConnection() // ID=conn-1
	c1.ManagedBy = "smardaten:out-1"
	c2 := sampleConnection()
	c2.ID = "c2"
	c2.ManagedBy = "smardaten:out-2"
	manual := sampleConnection()
	manual.ID = "manual-conn"
	for _, c := range []model.Connection{c1, c2, manual} {
		if err := st.SaveConnection(c); err != nil {
			t.Fatal(err)
		}
	}

	// 设备均引用 conn-1,满足外键
	d1 := sampleDevice("d1")
	d1.ManagedBy = "smardaten:out-1"
	d2 := sampleDevice("d2")
	d2.ManagedBy = "smardaten:out-2"
	dm := sampleDevice("dm") // 手工设备,ManagedBy 空
	for _, d := range []model.Device{d1, d2, dm} {
		if err := st.SaveDevice(d); err != nil {
			t.Fatal(err)
		}
	}

	connIDs, err := st.ListManagedConnectionIDs("smardaten:out-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(connIDs) != 1 || connIDs[0] != "conn-1" {
		t.Errorf("ListManagedConnectionIDs(out-1) = %v, want [conn-1]", connIDs)
	}
	devIDs, err := st.ListManagedDeviceIDs("smardaten:out-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(devIDs) != 1 || devIDs[0] != "d2" {
		t.Errorf("ListManagedDeviceIDs(out-2) = %v, want [d2]", devIDs)
	}
	if ids, _ := st.ListManagedConnectionIDs("smardaten:none"); len(ids) != 0 {
		t.Errorf("unknown manager returned %v, want empty", ids)
	}
	if ids, _ := st.ListManagedDeviceIDs("smardaten:none"); len(ids) != 0 {
		t.Errorf("unknown manager returned %v, want empty", ids)
	}
}

func TestAddAndDeletePoint(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)
	device := sampleDevice("d1")
	device.Points = nil
	st.SaveDevice(device)
	if err := st.AddPoint("d1", model.Point{Name: "p1", Address: "holding:0", DataType: model.DataTypeInt16}); err != nil {
		t.Fatalf("add point: %v", err)
	}
	got, _ := st.GetDevice("d1")
	if len(got.Points) != 1 {
		t.Fatalf("point not added: %+v", got.Points)
	}
	if err := st.DeletePoint("d1", "p1"); err != nil {
		t.Fatalf("delete point: %v", err)
	}
	got, _ = st.GetDevice("d1")
	if len(got.Points) != 0 {
		t.Fatalf("point not deleted: %+v", got.Points)
	}
}

// TestListEmptyReturnsEmptySlice 验证空库时列表返回非 nil 空切片(JSON 序列化为 [] 而非 null),
// 避免前端对 `.length` 访问抛错导致页面空白。
func TestListEmptyReturnsEmptySlice(t *testing.T) {
	st := newTestStore(t)

	conns, err := st.ListConnections()
	if err != nil {
		t.Fatalf("list connections: %v", err)
	}
	if conns == nil || len(conns) != 0 {
		t.Fatalf("empty connections should be non-nil empty slice, got %#v", conns)
	}

	devices, err := st.ListDevices()
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if devices == nil || len(devices) != 0 {
		t.Fatalf("empty devices should be non-nil empty slice, got %#v", devices)
	}
}

func TestSaveAndListOutputs(t *testing.T) {
	st := newTestStore(t)
	o := model.Output{
		ID:      "mqtt",
		Name:    "MQTT",
		Type:    "mqtt",
		Config:  []byte(`{"broker":"tcp://127.0.0.1:1883","qos":1}`),
		Enabled: true,
	}
	if err := st.SaveOutput(o); err != nil {
		t.Fatalf("save output: %v", err)
	}
	got, err := st.GetOutput("mqtt")
	if err != nil {
		t.Fatalf("get output: %v", err)
	}
	if got.Type != "mqtt" || !got.Enabled || string(got.Config) == "" {
		t.Fatalf("unexpected output: %+v", got)
	}

	// 更新并停用
	got.Enabled = false
	if err := st.SaveOutput(got); err != nil {
		t.Fatalf("update output: %v", err)
	}
	updated, _ := st.GetOutput("mqtt")
	if updated.Enabled {
		t.Fatalf("output should be disabled: %+v", updated)
	}

	outputs, err := st.ListOutputs()
	if err != nil {
		t.Fatalf("list outputs: %v", err)
	}
	if len(outputs) != 1 {
		t.Fatalf("want 1 output got %d", len(outputs))
	}
}

func TestDeleteOutput(t *testing.T) {
	st := newTestStore(t)
	st.SaveOutput(model.Output{ID: "td", Name: "TD", Type: "tdengine", Config: []byte(`{}`)})
	if err := st.DeleteOutput("td"); err != nil {
		t.Fatalf("delete output: %v", err)
	}
	if _, err := st.GetOutput("td"); err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestGatewaySettings(t *testing.T) {
	st := newTestStore(t)

	// Open 时预置默认网关 ID
	id, err := st.GetGatewayID()
	if err != nil {
		t.Fatalf("get default gateway id: %v", err)
	}
	if id != DefaultGatewayID {
		t.Fatalf("default gateway id = %q, want %q", id, DefaultGatewayID)
	}

	// 不存在 key 返回 ok=false
	if _, ok, err := st.GetSetting("no.such.key"); err != nil || ok {
		t.Fatalf("get missing setting: ok=%v err=%v", ok, err)
	}

	// 修改并读回
	if err := st.SetSetting(SettingGatewayID, "gw-02"); err != nil {
		t.Fatalf("set gateway id: %v", err)
	}
	got, err := st.GetGatewayID()
	if err != nil {
		t.Fatalf("get gateway id: %v", err)
	}
	if got != "gw-02" {
		t.Fatalf("gateway id = %q, want gw-02", got)
	}
}

func TestOnChange(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)
	ch, cancel := st.OnChange()
	defer cancel()
	select {
	case <-ch:
	default:
	}
	st.SaveDevice(sampleDevice("d1"))
	select {
	case <-ch:
	default:
		t.Fatal("expected change signal after save")
	}
}

// TestOnChangeFanout 验证多订阅者各自都能收到同一变更信号(调度器/处理引擎并发消费)。
func TestOnChangeFanout(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)
	ch1, cancel1 := st.OnChange()
	defer cancel1()
	ch2, cancel2 := st.OnChange()
	defer cancel2()
	st.SaveDevice(sampleDevice("d1"))
	for _, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		default:
			t.Fatal("expected change signal on all subscribers")
		}
	}
	// 退订后不再收到。
	cancel1()
	st.DeleteDevice("d1")
	select {
	case <-ch1:
		t.Fatal("canceled subscriber should not receive signal")
	default:
	}
	select {
	case <-ch2:
	default:
		t.Fatal("remaining subscriber should receive signal")
	}
}

// TestPointProcessingRoundtrip 验证点位处理配置的持久化与读取往返。
func TestPointProcessingRoundtrip(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)

	proc := &model.PointProcessing{
		Filters: []model.Filter{
			{Type: "deadband", Delta: 0.5},
			{Type: "quality", DropBad: true},
		},
		Aggregate: &model.Aggregate{Type: "avg", Window: "10s", EmitName: "temp.avg"},
	}
	dev := sampleDevice("d1")
	dev.Points[0].Processing = proc
	if err := st.SaveDevice(dev); err != nil {
		t.Fatalf("save device: %v", err)
	}

	got, err := st.GetDevice("d1")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if got.Points[0].Processing == nil {
		t.Fatal("expected processing to roundtrip")
	}
	if len(got.Points[0].Processing.Filters) != 2 || got.Points[0].Processing.Filters[0].Delta != 0.5 {
		t.Fatalf("filters roundtrip mismatch: %+v", got.Points[0].Processing)
	}
	if got.Points[0].Processing.Aggregate == nil || got.Points[0].Processing.Aggregate.EmitName != "temp.avg" {
		t.Fatalf("aggregate roundtrip mismatch: %+v", got.Points[0].Processing)
	}

	// 无处理配置点位读出为 nil(兼容直通)。
	dev2 := sampleDevice("d1")
	dev2.Points[0].Processing = nil
	if err := st.SaveDevice(dev2); err != nil {
		t.Fatalf("save device without processing: %v", err)
	}
	got2, err := st.GetDevice("d1")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if got2.Points[0].Processing != nil {
		t.Fatalf("expected nil processing, got %+v", got2.Points[0].Processing)
	}
}

// TestTimestampsManagedByStore 连接/设备/输出的 created_at/updated_at 由 store 维护:
// 首次写入双时间戳,再次更新保留 created_at、刷新 updated_at。
func TestTimestampsManagedByStore(t *testing.T) {
	st := newTestStore(t)

	conn := sampleConnection()
	if err := st.SaveConnection(conn); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetConnection("conn-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("connection timestamps not set: created=%q updated=%q", got.CreatedAt, got.UpdatedAt)
	}
	firstCreated, firstUpdated := got.CreatedAt, got.UpdatedAt

	// 更新后 created_at 保留、updated_at 刷新
	conn = sampleConnection()
	conn.Name = "renamed"
	time.Sleep(5 * time.Millisecond)
	if err := st.SaveConnection(conn); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetConnection("conn-1")
	if got.CreatedAt != firstCreated {
		t.Errorf("created_at changed on update: %q -> %q", firstCreated, got.CreatedAt)
	}
	if got.UpdatedAt <= firstUpdated {
		t.Errorf("updated_at not refreshed on update: %q -> %q", firstUpdated, got.UpdatedAt)
	}

	// 设备与输出同样由 store 维护
	if err := st.SaveDevice(sampleDevice("d1")); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetDevice("d1")
	if err != nil {
		t.Fatal(err)
	}
	if d.CreatedAt == "" || d.UpdatedAt == "" {
		t.Fatalf("device timestamps not set")
	}
	if err := st.SaveOutput(model.Output{ID: "o1", Name: "o1", Type: "mqtt", Config: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	o, err := st.GetOutput("o1")
	if err != nil {
		t.Fatal(err)
	}
	if o.CreatedAt == "" || o.UpdatedAt == "" {
		t.Fatalf("output timestamps not set")
	}
}
