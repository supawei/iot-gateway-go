package store

import (
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

func sampleDevice(id string) model.Device {
	return model.Device{
		ID:         id,
		Name:       id,
		Driver:     "modbus",
		Connection: []byte(`{"mode":"tcp","address":"127.0.0.1:502"}`),
		IntervalMs: 1000,
		Enabled:    true,
		Points: []model.Point{
			{Name: "temperature", Address: "holding:0", DataType: model.DataTypeInt16, Scale: 0.1},
			{Name: "humidity", Address: "holding:1", DataType: model.DataTypeInt16, Scale: 0.1},
		},
	}
}

func TestSaveAndGetDevice(t *testing.T) {
	st := newTestStore(t)
	if err := st.SaveDevice(sampleDevice("sensor-01")); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetDevice("sensor-01")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Points) != 2 || !got.Enabled {
		t.Fatalf("unexpected device: %+v", got)
	}
}

func TestListDevices(t *testing.T) {
	st := newTestStore(t)
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

func TestOnChange(t *testing.T) {
	st := newTestStore(t)
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
