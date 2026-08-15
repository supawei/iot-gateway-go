package core

import (
	"context"
	"errors"
	"testing"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/store"
)

// writeConn 实现 Conn + Writer,Write 恒成功。
type writeConn struct{ written []model.WriteItem }

func (c *writeConn) Read(context.Context, []model.Point) ([]model.DataPoint, error) {
	return nil, nil
}
func (c *writeConn) Close() error { return nil }
func (c *writeConn) Write(_ context.Context, items []model.WriteItem) ([]driver.WriteResult, error) {
	c.written = append(c.written, items...)
	results := make([]driver.WriteResult, len(items))
	for i, item := range items {
		results[i] = driver.WriteResult{Point: item.Point.Name, Ok: true}
	}
	return results, nil
}

// writeDriver 返回预设的 writeConn。
type writeDriver struct{ conn *writeConn }

func (d *writeDriver) Open(context.Context, driver.OpenRequest) (driver.Conn, error) {
	return d.conn, nil
}

func TestWritePoint(t *testing.T) {
	driver.Register("writedriver", &writeDriver{conn: &writeConn{}})

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.SaveConnection(model.Connection{ID: "c1", Name: "c1", Driver: "writedriver", Config: []byte(`{}`)})
	st.SaveDevice(model.Device{
		ID: "d1", ConnectionID: "c1", Params: []byte(`{}`),
		Points: []model.Point{{Name: "setpoint", Address: "holding:0", DataType: model.DataTypeInt16}},
	})

	results, err := WritePoint(context.Background(), st, "d1", "setpoint", 42)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(results) != 1 || !results[0].Ok || results[0].Point != "setpoint" {
		t.Fatalf("results: %+v", results)
	}

	if _, err := WritePoint(context.Background(), st, "d1", "missing", 42); !errors.Is(err, ErrPointNotFound) {
		t.Fatalf("want ErrPointNotFound, got %v", err)
	}
	if _, err := WritePoint(context.Background(), st, "nope", "setpoint", 42); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("want ErrDeviceNotFound, got %v", err)
	}
}
