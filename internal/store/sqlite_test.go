package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

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

// TestLegacyDBSchemaEvolve 验证旧版(无 processing/seq 列)point 表经 Open 自动补列,
// 旧数据可正常读取、新列可写(开发期结构演进,见 docs/development-conventions.md)。
func TestLegacyDBSchemaEvolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	legacy := `
CREATE TABLE connection (id TEXT PRIMARY KEY, name TEXT NOT NULL, driver TEXT NOT NULL, config TEXT NOT NULL);
CREATE TABLE device (id TEXT PRIMARY KEY, name TEXT NOT NULL, connection_id TEXT NOT NULL, params TEXT NOT NULL DEFAULT '{}', interval_ms INTEGER NOT NULL, enabled INTEGER NOT NULL DEFAULT 1);
CREATE TABLE point (device_id TEXT NOT NULL, name TEXT NOT NULL, address TEXT NOT NULL, data_type TEXT NOT NULL, scale REAL NOT NULL DEFAULT 0, PRIMARY KEY (device_id, name));
INSERT INTO connection (id,name,driver,config) VALUES ('c','c','modbus','{}');
INSERT INTO device (id,name,connection_id,params,interval_ms,enabled) VALUES ('d','d','c','{}',1000,1);
INSERT INTO point (device_id,name,address,data_type,scale) VALUES ('d','p','a','float32',0.1);
`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	dev, err := st.GetDevice("d")
	if err != nil {
		t.Fatalf("get device after evolve: %v", err)
	}
	if len(dev.Points) != 1 || dev.Points[0].Name != "p" {
		t.Fatalf("points after evolve: %+v", dev.Points)
	}
	dev.Points[0].Processing = &model.PointProcessing{Aggregate: &model.Aggregate{Type: "avg", Window: "10s"}}
	if err := st.SaveDevice(dev); err != nil {
		t.Fatalf("save with processing after evolve: %v", err)
	}
	got, err := st.GetDevice("d")
	if err != nil {
		t.Fatalf("get after save processing: %v", err)
	}
	if got.Points[0].Processing == nil {
		t.Fatalf("processing not persisted after evolve")
	}
}
