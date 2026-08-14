package core

import (
	"context"
	"testing"
	"time"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/status"
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

// mockSubConn 实现 driver.Subscriber,验证 scheduler 对订阅能力走推送而非轮询。
type mockSubConn struct {
	deviceID string
}

func (c *mockSubConn) Read(_ context.Context, _ []model.Point) ([]model.DataPoint, error) {
	return nil, nil
}

func (c *mockSubConn) Close() error { return nil }

func (c *mockSubConn) Subscribe(_ context.Context, points []model.Point, onData func(model.DataPoint)) error {
	for _, p := range points {
		onData(model.DataPoint{
			DeviceID: c.deviceID, Point: p.Name, Value: 1,
			Timestamp: time.Now(), Quality: model.QualityGood,
		})
	}
	return nil
}

type mockSubDriver struct{}

func (mockSubDriver) Open(_ context.Context, req driver.OpenRequest) (driver.Conn, error) {
	return &mockSubConn{deviceID: req.DeviceID}, nil
}

func TestSchedulerSubscribes(t *testing.T) {
	driver.Register("subdriver", mockSubDriver{})

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.SaveConnection(model.Connection{
		ID:     "c1",
		Name:   "c1",
		Driver: "subdriver",
		Config: []byte(`{}`),
	})
	// IntervalMs 设很大:若误走轮询则测试必超时,证明走的是订阅推送
	st.SaveDevice(model.Device{
		ID:           "d1",
		ConnectionID: "c1",
		Params:       []byte(`{}`),
		IntervalMs:   3600_000,
		Enabled:      true,
		Points: []model.Point{
			{Name: "p1", Address: "ns=2;s=T", DataType: model.DataTypeInt32},
		},
	})

	dataPoints := make(chan model.DataPoint, 10)
	scheduler := NewScheduler(st, dataPoints, 4, status.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	go scheduler.Run(ctx)

	select {
	case dp := <-dataPoints:
		if dp.DeviceID != "d1" || dp.Point != "p1" {
			t.Fatalf("unexpected datapoint: %+v", dp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for subscribed datapoint")
	}
	cancel()
}

// mockListenConn 实现 driver.Listener,验证 scheduler 对监听能力走推送而非轮询。
type mockListenConn struct {
	deviceID string
}

func (c *mockListenConn) Read(_ context.Context, _ []model.Point) ([]model.DataPoint, error) {
	return nil, nil
}

func (c *mockListenConn) Close() error { return nil }

func (c *mockListenConn) Listen(_ context.Context, points []model.Point, onData func(model.DataPoint)) error {
	for _, p := range points {
		onData(model.DataPoint{
			DeviceID: c.deviceID, Point: p.Name, Value: 1,
			Timestamp: time.Now(), Quality: model.QualityGood,
		})
	}
	return nil
}

type mockListenDriver struct{}

func (mockListenDriver) Open(_ context.Context, req driver.OpenRequest) (driver.Conn, error) {
	return &mockListenConn{deviceID: req.DeviceID}, nil
}

func TestSchedulerListens(t *testing.T) {
	driver.Register("listendriver", mockListenDriver{})

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.SaveConnection(model.Connection{
		ID:     "c1",
		Name:   "c1",
		Driver: "listendriver",
		Config: []byte(`{}`),
	})
	// IntervalMs 设很大:若误走轮询则测试必超时,证明走的是监听推送
	st.SaveDevice(model.Device{
		ID:           "d1",
		ConnectionID: "c1",
		Params:       []byte(`{}`),
		IntervalMs:   3600_000,
		Enabled:      true,
		Points: []model.Point{
			{Name: "p1", Address: "0", DataType: model.DataTypeInt16},
		},
	})

	dataPoints := make(chan model.DataPoint, 10)
	scheduler := NewScheduler(st, dataPoints, 4, status.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	go scheduler.Run(ctx)

	select {
	case dp := <-dataPoints:
		if dp.DeviceID != "d1" || dp.Point != "p1" {
			t.Fatalf("unexpected datapoint: %+v", dp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for listened datapoint")
	}
	cancel()
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
	scheduler := NewScheduler(st, dataPoints, 4, status.NewRegistry())

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

// TestSchedulerReportsOnline 验证采集成功后设备状态被标记在线。
func TestSchedulerReportsOnline(t *testing.T) {
	driver.Register("onlinedriver", mockDriver{})

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.SaveConnection(model.Connection{ID: "c1", Name: "c1", Driver: "onlinedriver", Config: []byte(`{}`)})
	st.SaveDevice(model.Device{
		ID: "d1", ConnectionID: "c1", Params: []byte(`{}`),
		IntervalMs: 100, Enabled: true,
		Points: []model.Point{{Name: "p1", Address: "holding:0", DataType: model.DataTypeInt16}},
	})

	reg := status.NewRegistry()
	scheduler := NewScheduler(st, make(chan model.DataPoint, 10), 4, reg)
	ctx, cancel := context.WithCancel(context.Background())
	go scheduler.Run(ctx)
	defer cancel()

	waitForOnline(t, reg, "d1")
}

// TestSchedulerReportsOffline 验证打开失败(驱动未注册)时设备被标记离线。
func TestSchedulerReportsOffline(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	st.SaveConnection(model.Connection{ID: "c1", Name: "c1", Driver: "nosuchdriver", Config: []byte(`{}`)})
	st.SaveDevice(model.Device{
		ID: "d1", ConnectionID: "c1", Params: []byte(`{}`),
		IntervalMs: 100, Enabled: true,
		Points: []model.Point{{Name: "p1", Address: "holding:0", DataType: model.DataTypeInt16}},
	})

	reg := status.NewRegistry()
	scheduler := NewScheduler(st, make(chan model.DataPoint, 10), 4, reg)
	ctx, cancel := context.WithCancel(context.Background())
	go scheduler.Run(ctx)
	defer cancel()

	waitForOffline(t, reg, "d1")
}

// waitForOnline 轮询等待设备状态变在线(采集成功在 emit 之后才上报状态)。
func waitForOnline(t *testing.T, reg *status.Registry, deviceID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if st, ok := reg.Get(deviceID); ok && st.Online {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for online status")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func waitForOffline(t *testing.T, reg *status.Registry, deviceID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if st, ok := reg.Get(deviceID); ok && !st.Online {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for offline status")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
