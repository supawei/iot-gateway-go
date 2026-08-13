package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/store"
)

type mockConn struct {
	deviceID string
	reads    int
}

func (c *mockConn) Read(_ context.Context, points []model.Point) ([]model.DataPoint, error) {
	c.reads++
	results := make([]model.DataPoint, len(points))
	for i, p := range points {
		results[i] = model.DataPoint{
			DeviceID:  c.deviceID,
			Point:     p.Name,
			Value:     c.reads,
			Timestamp: time.Now(),
			Quality:   model.QualityGood,
		}
	}
	return results, nil
}

func (c *mockConn) Close() error { return nil }

type mockDriver struct{}

func (mockDriver) Open(_ context.Context, deviceID string, _ json.RawMessage) (driver.Conn, error) {
	return &mockConn{deviceID: deviceID}, nil
}

func TestSchedulerCollects(t *testing.T) {
	driver.Register("testdriver", mockDriver{})

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.SaveDevice(model.Device{
		ID:         "d1",
		Driver:     "testdriver",
		Connection: []byte(`{}`),
		IntervalMs: 50,
		Enabled:    true,
		Points: []model.Point{
			{Name: "p1", Address: "holding:0", DataType: model.DataTypeInt16},
		},
	})

	dataPoints := make(chan model.DataPoint, 10)
	scheduler := NewScheduler(st, dataPoints)

	ctx, cancel := context.WithCancel(context.Background())
	go scheduler.Run(ctx)

	select {
	case dp := <-dataPoints:
		if dp.DeviceID != "d1" || dp.Point != "p1" {
			t.Fatalf("unexpected datapoint: %+v", dp)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for datapoint")
	}
	cancel()
}
