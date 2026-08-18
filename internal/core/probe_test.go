package core

import (
	"context"
	"errors"
	"testing"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/store"
)

// probeConn 实现 Conn + Prober,Probe 行为可配置。
type probeConn struct {
	probeErr error
}

func (c *probeConn) Read(context.Context, []model.Point) ([]model.DataPoint, error) { return nil, nil }
func (c *probeConn) Close() error                                                    { return nil }
func (c *probeConn) Probe(context.Context, []model.Point) error                      { return c.probeErr }

// probeDriver 返回预设的 probeConn。
type probeDriver struct{ conn *probeConn }

func (d *probeDriver) Open(context.Context, driver.OpenRequest) (driver.Conn, error) { return d.conn, nil }

// plainConn 只实现 Conn(不支持 Prober),验证 ErrNotProbeable。
type plainConn struct{}

func (plainConn) Read(context.Context, []model.Point) ([]model.DataPoint, error) { return nil, nil }
func (plainConn) Close() error                                                    { return nil }

type plainDriver struct{}

func (plainDriver) Open(context.Context, driver.OpenRequest) (driver.Conn, error) { return plainConn{}, nil }

func TestProbeDevice(t *testing.T) {
	driver.Register("probedriver", &probeDriver{conn: &probeConn{}})
	driver.Register("plainprobe", &plainDriver{})

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.SaveConnection(model.Connection{ID: "c1", Name: "c1", Driver: "probedriver", Config: []byte(`{}`)})
	st.SaveConnection(model.Connection{ID: "c2", Name: "c2", Driver: "plainprobe", Config: []byte(`{}`)})
	st.SaveDevice(model.Device{ID: "d1", ConnectionID: "c1", Params: []byte(`{}`), Points: []model.Point{{Name: "p", Address: "holding:0", DataType: model.DataTypeInt16}}})
	st.SaveDevice(model.Device{ID: "d2", ConnectionID: "c2", Params: []byte(`{}`), Points: []model.Point{{Name: "p", Address: "holding:0", DataType: model.DataTypeInt16}}})

	// 设备可达
	if err := ProbeDevice(context.Background(), st, "d1"); err != nil {
		t.Fatalf("probe reachable device: %v", err)
	}

	// 设备不存在
	if err := ProbeDevice(context.Background(), st, "nope"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("want ErrDeviceNotFound, got %v", err)
	}

	// 驱动不支持探测 → ErrNotProbeable
	if err := ProbeDevice(context.Background(), st, "d2"); !errors.Is(err, output.ErrNotProbeable) {
		t.Fatalf("want ErrNotProbeable, got %v", err)
	}
}

func TestProbeDeviceUnreachable(t *testing.T) {
	driver.Register("probedriver-unreachable", &probeDriver{conn: &probeConn{probeErr: errors.New("read timeout")}})
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.SaveConnection(model.Connection{ID: "c1", Name: "c1", Driver: "probedriver-unreachable", Config: []byte(`{}`)})
	st.SaveDevice(model.Device{ID: "d1", ConnectionID: "c1", Params: []byte(`{}`), Points: []model.Point{{Name: "p", Address: "holding:0", DataType: model.DataTypeInt16}}})

	if err := ProbeDevice(context.Background(), st, "d1"); err == nil {
		t.Fatal("unreachable device should return error")
	}
}
