package thingsboard

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output/mqtttest"
)

func TestDeviceName(t *testing.T) {
	withPrefix := &thingsboardOutput{prefix: "factory1/"}
	if got := withPrefix.deviceName("sensor-01"); got != "factory1/sensor-01" {
		t.Fatalf("deviceName with prefix = %q", got)
	}
	noPrefix := &thingsboardOutput{}
	if got := noPrefix.deviceName("sensor-01"); got != "sensor-01" {
		t.Fatalf("deviceName without prefix = %q", got)
	}
}

func TestTelemetryBatchPayloadSinglePoint(t *testing.T) {
	dp := model.DataPoint{
		DeviceID:  "sensor-01",
		Point:     "temperature",
		Value:     25.5,
		Timestamp: time.UnixMilli(1700000000000),
		Quality:   model.QualityGood,
	}
	got, err := json.Marshal(telemetryBatchPayload("sensor-01", []model.DataPoint{dp}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"sensor-01":[{"ts":1700000000000,"values":{"temperature":25.5}}]}`
	if string(got) != want {
		t.Fatalf("payload:\n got %s\nwant %s", got, want)
	}
}

// TestTelemetryBatchPayloadGroupsByTimestamp 验证同一设备多时刻的点位按时间戳分组,
// 同一时刻的多点位合并进一个 values。
func TestTelemetryBatchPayloadGroupsByTimestamp(t *testing.T) {
	ts := time.UnixMilli(1700000000000)
	points := []model.DataPoint{
		{Point: "temperature", Value: 25.5, Timestamp: ts},
		{Point: "humidity", Value: 60, Timestamp: ts},
		{Point: "temperature", Value: 26.0, Timestamp: ts.Add(time.Second)},
	}
	got, err := json.Marshal(telemetryBatchPayload("sensor-01", points))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"sensor-01":[{"ts":1700000000000,"values":{"humidity":60,"temperature":25.5}},{"ts":1700000001000,"values":{"temperature":26}}]}`
	if string(got) != want {
		t.Fatalf("payload:\n got %s\nwant %s", got, want)
	}
}

func TestAttributesPayload(t *testing.T) {
	got, err := json.Marshal(attributesPayload("sensor-01", map[string]interface{}{"quality": "good"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"sensor-01":{"quality":"good"}}`
	if string(got) != want {
		t.Fatalf("attributes payload:\n got %s\nwant %s", got, want)
	}
}

// TestDeviceNotifierIntents 验证 DeviceOnline/DeviceOffline 记录 connect/disconnect 意图,后者覆盖前者。
func TestDeviceNotifierIntents(t *testing.T) {
	o := &thingsboardOutput{
		connects:    make(map[string]bool),
		disconnects: make(map[string]bool),
	}
	o.DeviceOnline("sensor-01")
	if !o.connects["sensor-01"] {
		t.Fatal("online should record connect intent")
	}
	o.DeviceOffline("sensor-01")
	if !o.disconnects["sensor-01"] || o.connects["sensor-01"] {
		t.Fatal("offline should override online intent")
	}
}

// TestHandleDownlink 验证共享属性下行被解析并投递到写队列(设备名去前缀还原 DeviceID)。
func TestHandleDownlink(t *testing.T) {
	o := &thingsboardOutput{prefix: "factory1/", writeCh: make(chan writeRequest, 4)}
	o.handleDownlink([]byte(`{"device":"factory1/sensor-01","data":{"setpoint":42}}`))

	select {
	case req := <-o.writeCh:
		if req.deviceID != "sensor-01" || req.point != "setpoint" || req.value != float64(42) {
			t.Fatalf("req: %+v", req)
		}
	default:
		t.Fatal("no write request enqueued")
	}
}

// TestHandleDownlinkIgnoresUplink 验证上行客户端属性(无 device/data 包装)被忽略,避免回环。
func TestHandleDownlinkIgnoresUplink(t *testing.T) {
	o := &thingsboardOutput{writeCh: make(chan writeRequest, 4)}
	o.handleDownlink([]byte(`{"sensor-01":{"quality":"good"}}`))

	select {
	case req := <-o.writeCh:
		t.Fatalf("uplink should be ignored, got %+v", req)
	default:
	}
}

// TestHandleRPCDownlink 验证 RPC write 命令被解析并投递写队列(带 RPC id 用于应答)。
func TestHandleRPCDownlink(t *testing.T) {
	o := &thingsboardOutput{prefix: "factory1/", writeCh: make(chan writeRequest, 4)}
	o.handleRPCDownlink([]byte(`{"device":"factory1/sensor-01","data":{"id":7,"method":"write","params":{"point":"setpoint","value":42}}}`))

	select {
	case req := <-o.writeCh:
		if req.deviceID != "sensor-01" || req.point != "setpoint" || req.rpcID != 7 || req.value != float64(42) {
			t.Fatalf("req: %+v", req)
		}
	default:
		t.Fatal("no write request enqueued")
	}
}

// TestHandleRPCDownlinkIgnoresReply 验证 RPC 应答(无 data.method)被忽略,避免回环。
func TestHandleRPCDownlinkIgnoresReply(t *testing.T) {
	o := &thingsboardOutput{writeCh: make(chan writeRequest, 4)}
	o.handleRPCDownlink([]byte(`{"device":"sensor-01","id":7,"data":{"ok":true}}`))

	select {
	case req := <-o.writeCh:
		t.Fatalf("rpc reply should be ignored, got %+v", req)
	default:
	}
}

// TestPendingBufferCap 待上报缓冲达到全局上限后丢弃新点,内存有界(断连场景兜底)。
func TestPendingBufferCap(t *testing.T) {
	o := &thingsboardOutput{
		pending: make(map[string][]model.DataPoint),
	}
	dp := model.DataPoint{DeviceID: "sensor-01", Point: "temperature", Value: 25.5, Timestamp: time.Now()}
	for i := 0; i < maxPendingPoints+10; i++ {
		o.Publish(dp)
	}
	if o.pendingCount != maxPendingPoints {
		t.Fatalf("pendingCount = %d, want %d", o.pendingCount, maxPendingPoints)
	}
	if len(o.pending["sensor-01"]) != maxPendingPoints {
		t.Fatalf("pending[sensor-01] = %d, want %d", len(o.pending["sensor-01"]), maxPendingPoints)
	}
}

// TestNewWithReachableBroker broker 可达(静默 broker 正常回 CONNACK)时,OnConnect 会在
// Connect 内同步触发并调用 onConnectSubscribe;若 client 尚未赋值会 nil 解引用 panic。
// 此测试守护该时序回归:New 应快速返回且不 panic。
func TestNewWithReachableBroker(t *testing.T) {
	b := mqtttest.StartSilent(t)
	start := time.Now()
	out, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "tb-test", AccessToken: "tok", QoS: 1}, nil, "", nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if out == nil {
		t.Fatal("New returned nil output")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("New took %v, want fast (subscribe runs in background)", elapsed)
	}
	out.Close()
}

// TestRuntimeStatusPending RuntimeStatus 应如实上报内部待发缓冲,且无 client 时不 panic。
func TestRuntimeStatusPending(t *testing.T) {
	o := &thingsboardOutput{
		pending: make(map[string][]model.DataPoint),
	}
	dp := model.DataPoint{DeviceID: "sensor-01", Point: "temperature", Value: 25.5, Timestamp: time.Now()}
	for i := 0; i < 3; i++ {
		o.Publish(dp)
	}
	st := o.RuntimeStatus()
	if st.Pending != 3 {
		t.Fatalf("pending = %d, want 3", st.Pending)
	}
	if st.Connected || st.ConnectionOpen {
		t.Fatal("nil client should report not connected")
	}
}

// fakeBackfillSink 记录被保存到补传队列的点(断网补传测试用)。
type fakeBackfillSink struct {
	mu  sync.Mutex
	dps []model.DataPoint
}

func (f *fakeBackfillSink) Save(_ string, dps []model.DataPoint) error {
	f.mu.Lock()
	f.dps = append(f.dps, dps...)
	f.mu.Unlock()
	return nil
}

func (f *fakeBackfillSink) saved() []model.DataPoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.DataPoint(nil), f.dps...)
}

// TestFlushFailSavesToBackfill 上送失败(broker 半死,QoS1 无 PUBACK)时,
// 该批遥测点应落库补传而非丢弃。
func TestFlushFailSavesToBackfill(t *testing.T) {
	b := mqtttest.StartSilent(t)
	sink := &fakeBackfillSink{}
	reportQuality := false
	out, err := New(Config{
		Broker:        "tcp://" + b.Addr,
		ClientID:      "tb-bf",
		AccessToken:   "tok",
		QoS:           1,
		ReportQuality: &reportQuality,
		FlushInterval: "100ms",
	}, nil, "out-1", sink)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer out.Close()

	dp := model.DataPoint{DeviceID: "sensor-01", Point: "temperature", Value: 25.5, Timestamp: time.Now(), Quality: model.QualityGood}
	if err := out.Publish(dp); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// publish 有界等待 PublishTimeout(5s)后失败 → saveBackfill。给足余量。
	waitForSaved(t, sink, 8*time.Second)
	got := sink.saved()
	if len(got) != 1 || got[0].DeviceID != "sensor-01" || got[0].Point != "temperature" {
		t.Fatalf("saved points = %+v, want [sensor-01/temperature]", got)
	}
}

func waitForSaved(t *testing.T, sink *fakeBackfillSink, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(sink.saved()) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("points not saved to backfill within %v", timeout)
}
