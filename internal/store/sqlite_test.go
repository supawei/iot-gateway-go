package store

import (
	"errors"
	"testing"

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

func TestMeta(t *testing.T) {
	st := newTestStore(t)
	if _, ok, err := st.GetMeta("k"); err != nil || ok {
		t.Fatalf("get missing meta: ok=%v err=%v", ok, err)
	}
	if err := st.SetMeta("k", "v"); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	v, ok, err := st.GetMeta("k")
	if err != nil || !ok || v != "v" {
		t.Fatalf("get meta: v=%q ok=%v err=%v", v, ok, err)
	}
	// 覆盖写
	if err := st.SetMeta("k", "v2"); err != nil {
		t.Fatalf("overwrite meta: %v", err)
	}
	v, _, _ = st.GetMeta("k")
	if v != "v2" {
		t.Fatalf("meta not overwritten: %q", v)
	}
}

func TestOnChange(t *testing.T) {
	st := newTestStore(t)
	saveSampleConnection(t, st)
	select {
	case <-st.OnChange():
	default:
	}
	st.SaveDevice(sampleDevice("d1"))
	select {
	case <-st.OnChange():
	default:
		t.Fatal("expected change signal after save")
	}
}
