package core

import (
	"context"
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

func (mockDriver) Open(_ context.Context, req driver.OpenRequest) (driver.Conn, error) {
	return &mockConn{deviceID: req.DeviceID}, nil
}

func TestSchedulerCollects(t *testing.T) {
	driver.Register("testdriver", mockDriver{})

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.SaveConnection(model.Connection{
		ID:     "c1",
		Name:   "c1",
		Driver: "testdriver",
		Config: []byte(`{}`),
	})
	st.SaveDevice(model.Device{
		ID:           "d1",
		ConnectionID: "c1",
		Params:       []byte(`{}`),
		IntervalMs:   100,
		Enabled:      true,
		Points: []model.Point{
			{Name: "p1", Address: "holding:0", DataType: model.DataTypeInt16},
		},
	})

	dataPoints := make(chan model.DataPoint, 10)
	scheduler := NewScheduler(st, dataPoints, 4)

	ctx, cancel := context.WithCancel(context.Background())
	go scheduler.Run(ctx)

	select {
	case dp := <-dataPoints:
		if dp.DeviceID != "d1" || dp.Point != "p1" {
			t.Fatalf("unexpected datapoint: %+v", dp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for datapoint")
	}
	cancel()
}
