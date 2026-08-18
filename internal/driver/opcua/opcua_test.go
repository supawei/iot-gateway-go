package opcua

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"

	"iot-gateway-go/internal/model"
)

func TestParseConnConfig(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantErr      bool
		wantEndpoint string
	}{
		{"valid", `{"endpoint":"opc.tcp://x:4840"}`, false, "opc.tcp://x:4840"},
		{"missing endpoint", `{}`, true, ""},
		{"unsupported security", `{"endpoint":"opc.tcp://x:4840","securityMode":"sign"}`, true, ""},
		{"invalid timeout", `{"endpoint":"opc.tcp://x:4840","timeout":"abc"}`, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseConnConfig(json.RawMessage(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if !tt.wantErr && cfg.Endpoint != tt.wantEndpoint {
				t.Fatalf("endpoint=%q want %q", cfg.Endpoint, tt.wantEndpoint)
			}
		})
	}
}

func TestParseConnConfigDefaults(t *testing.T) {
	cfg, err := parseConnConfig(json.RawMessage(`{"endpoint":"opc.tcp://x:4840"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cfg.SecurityMode != "none" || cfg.Timeout != "5s" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

// TestParseNodeIDAddress 验证点位地址的 NodeID 字符串约定可被 gopcua 正确解析。
// TestParseNodeIDAddress 验证点位地址的 NodeID 字符串约定可被 gopcua 正确解析。
// 注:ParseNodeID 较宽松(裸字符串视为 ns=0 的 string node),非法地址在运行时
// 由 server 返回 bad status 体现,而非解析期报错。
func TestParseNodeIDAddress(t *testing.T) {
	for _, addr := range []string{"ns=2;s=Temperature", "ns=0;i=2258", "i=1234", "s=Foo"} {
		t.Run(addr, func(t *testing.T) {
			nodeID, err := ua.ParseNodeID(addr)
			if err != nil || nodeID == nil {
				t.Fatalf("parse %q: err=%v nodeID=%v", addr, err, nodeID)
			}
		})
	}
}

func TestDecodeValue(t *testing.T) {
	if _, ok := decodeValue("x", model.DataTypeBool, 0); ok {
		t.Fatalf("string as bool should fail")
	}
	if _, ok := decodeValue("x", model.DataTypeDouble, 0); ok {
		t.Fatalf("string as double should fail")
	}

	got, ok := decodeValue(true, model.DataTypeBool, 0)
	if !ok || got != true {
		t.Fatalf("bool: got %v ok %v", got, ok)
	}

	got, ok = decodeValue("temp", model.DataTypeString, 0)
	if !ok || got != "temp" {
		t.Fatalf("string: got %v ok %v", got, ok)
	}

	got, ok = decodeValue(int32(42), model.DataTypeInt32, 0)
	if !ok || got != int64(42) {
		t.Fatalf("int32: got %v ok %v", got, ok)
	}

	got, ok = decodeValue(float64(3.14), model.DataTypeDouble, 0)
	if !ok {
		t.Fatalf("double ok=false")
	}
	if floatValue, _ := got.(float64); floatValue != 3.14 {
		t.Fatalf("double value: %v", got)
	}

	got, ok = decodeValue(int32(42), model.DataTypeInt32, 0.1)
	if !ok {
		t.Fatalf("int32 scaled ok=false")
	}
	if floatValue, _ := got.(float64); math.Abs(floatValue-4.2) > 1e-9 {
		t.Fatalf("int32 scaled value: %v want 4.2", got)
	}

	// 未识别的 dataType 必须 ok=false(uncertain),不得放行 "good + nil 值"
	if _, ok := decodeValue("anything", model.DataType("unknown"), 0); ok {
		t.Fatalf("unknown dataType should fail")
	}
	if _, ok := decodeValue(nil, model.DataType("unknown"), 0); ok {
		t.Fatalf("unknown dataType with nil should fail")
	}
}

func TestParseConnConfigSubscribe(t *testing.T) {
	cfg, err := parseConnConfig(json.RawMessage(`{"endpoint":"opc.tcp://x:4840","mode":"subscribe"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cfg.Mode != modeSubscribe {
		t.Fatalf("mode=%q", cfg.Mode)
	}
	if cfg.PublishInterval != defaultPublishInterval.String() {
		t.Fatalf("publishInterval=%q want %q", cfg.PublishInterval, defaultPublishInterval.String())
	}
	if cfg.QueueSize != defaultQueueSize {
		t.Fatalf("queueSize=%d want %d", cfg.QueueSize, defaultQueueSize)
	}
	if cfg.SamplingInterval != 0 {
		t.Fatalf("samplingInterval=%v want 0", cfg.SamplingInterval)
	}

	cfg, err = parseConnConfig(json.RawMessage(`{"endpoint":"opc.tcp://x:4840","mode":"subscribe","publishInterval":"500ms","samplingInterval":250,"queueSize":20}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cfg.PublishInterval != "500ms" || cfg.SamplingInterval != 250 || cfg.QueueSize != 20 {
		t.Fatalf("subscribe fields not applied: %+v", cfg)
	}
}

func TestParseConnConfigSubscribeErrors(t *testing.T) {
	for _, raw := range []string{
		`{"endpoint":"opc.tcp://x:4840","mode":"bogus"}`,
		`{"endpoint":"opc.tcp://x:4840","mode":"subscribe","publishInterval":"abc"}`,
	} {
		if _, err := parseConnConfig(json.RawMessage(raw)); err == nil {
			t.Fatalf("want error for %s", raw)
		}
	}
}

func TestBuildMonitoredItems(t *testing.T) {
	points := []model.Point{
		{Name: "p1", Address: "ns=2;s=T"},
		{Name: "p2", Address: "i=1234"},
	}
	items, indices, handles := buildMonitoredItems(points, 250, 20, 5)
	if len(items) != 2 || len(indices) != 2 || len(handles) != 2 {
		t.Fatalf("items=%d indices=%d handles=%d", len(items), len(indices), len(handles))
	}
	if indices[0] != 0 || indices[1] != 1 {
		t.Fatalf("indices=%v", indices)
	}
	// ClientHandle 从 nextHandle 起递增分配(跨设备全局唯一)
	if handles[0] != 5 || handles[1] != 6 {
		t.Fatalf("handles=%v", handles)
	}
	if items[0].RequestedParameters.ClientHandle != 5 || items[1].RequestedParameters.ClientHandle != 6 {
		t.Fatalf("client handles wrong: %d %d", items[0].RequestedParameters.ClientHandle, items[1].RequestedParameters.ClientHandle)
	}
	if items[0].RequestedParameters.SamplingInterval != 250 || items[0].RequestedParameters.QueueSize != 20 {
		t.Fatalf("sampling/queue override not applied: %+v", items[0].RequestedParameters)
	}

	items, indices, handles = buildMonitoredItems(nil, 0, 0, 0)
	if len(items) != 0 || len(indices) != 0 || len(handles) != 0 {
		t.Fatalf("empty points should yield no items")
	}
}

func TestNotificationToDataPoint(t *testing.T) {
	point := model.Point{Name: "t", Address: "ns=2;s=T", DataType: model.DataTypeInt32}

	v, err := ua.NewVariant(int32(42))
	if err != nil {
		t.Fatalf("variant: %v", err)
	}
	ts := time.Now().Add(-time.Second)
	dp := notificationToDataPoint("d1", point, &ua.DataValue{Value: v, Status: ua.StatusOK, SourceTimestamp: ts})
	if dp.DeviceID != "d1" || dp.Point != "t" || dp.Quality != model.QualityGood {
		t.Fatalf("dp: %+v", dp)
	}
	if dp.Value != int64(42) {
		t.Fatalf("value: %v", dp.Value)
	}
	if !dp.Timestamp.Equal(ts) {
		t.Fatalf("timestamp: %v want %v", dp.Timestamp, ts)
	}

	// 坏状态 -> quality bad
	dp = notificationToDataPoint("d1", point, &ua.DataValue{Status: ua.StatusBad, SourceTimestamp: ts})
	if dp.Quality != model.QualityBad {
		t.Fatalf("bad status quality: %v", dp.Quality)
	}

	// nil 值 -> quality bad
	dp = notificationToDataPoint("d1", point, nil)
	if dp.Quality != model.QualityBad {
		t.Fatalf("nil quality: %v", dp.Quality)
	}

	// 类型不匹配 -> quality uncertain
	v2, _ := ua.NewVariant("not an int")
	dp = notificationToDataPoint("d1", point, &ua.DataValue{Value: v2, Status: ua.StatusOK})
	if dp.Quality != model.QualityUncertain {
		t.Fatalf("type mismatch quality: %v", dp.Quality)
	}
}

func TestConnectionPoolAcquireRelease(t *testing.T) {
	d := &opcuaDriver{pool: make(map[string]*sharedSession)}
	shared := &sharedSession{connectionID: "c1", refCount: 1}
	d.pool["c1"] = shared

	got, err := d.acquire(context.Background(), "c1", connConfig{})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got != shared || shared.refCount != 2 {
		t.Fatalf("acquire did not reuse: refCount=%d", shared.refCount)
	}

	if err := d.release(shared); err != nil {
		t.Fatalf("release: %v", err)
	}
	if shared.refCount != 1 {
		t.Fatalf("refCount=%d want 1", shared.refCount)
	}
}

func TestEncodeValue(t *testing.T) {
	if v, ok := encodeValue(true, model.DataTypeBool); !ok || v != true {
		t.Fatalf("bool: %v ok=%v", v, ok)
	}
	if v, ok := encodeValue("temp", model.DataTypeString); !ok || v != "temp" {
		t.Fatalf("string: %v ok=%v", v, ok)
	}
	if v, ok := encodeValue(float64(258), model.DataTypeInt16); !ok || v != int16(258) {
		t.Fatalf("int16: %v ok=%v", v, ok)
	}
	if v, ok := encodeValue(float64(258), model.DataTypeInt32); !ok || v != int32(258) {
		t.Fatalf("int32: %v ok=%v", v, ok)
	}
	if v, ok := encodeValue(float64(3.14), model.DataTypeFloat); !ok {
		t.Fatalf("float32 ok=%v", ok)
	} else if f, _ := v.(float32); math.Abs(float64(f)-3.14) > 1e-6 {
		t.Fatalf("float32 value: %v", v)
	}
	if v, ok := encodeValue(float64(3.14), model.DataTypeDouble); !ok || v != float64(3.14) {
		t.Fatalf("double: %v ok=%v", v, ok)
	}
	if _, ok := encodeValue("str", model.DataTypeInt16); ok {
		t.Fatal("string as int16 should fail")
	}
	if _, ok := encodeValue(float64(1), model.DataTypeBool); ok {
		t.Fatal("float as bool should fail")
	}
}

// TestBuildWriteValue 回归:写值必须携带值内容。
// 根因:DataValue 未设置 EncodingMask=DataValueValue 时,gopcua 编码不序列化 Value,
// 发出的 WriteRequest 不含任何值,服务端不写入(前端"写值"表面成功、实际无效)。
func TestBuildWriteValue(t *testing.T) {
	point := model.Point{Name: "sp", Address: "ns=2;s=Setpoint", DataType: model.DataTypeInt32}

	wv, ok := buildWriteValue(point, float64(42))
	if !ok {
		t.Fatalf("build failed")
	}
	if wv.AttributeID != ua.AttributeIDValue {
		t.Fatalf("attribute: %v", wv.AttributeID)
	}
	if wv.Value == nil || wv.Value.EncodingMask != ua.DataValueValue {
		t.Fatalf("DataValue.EncodingMask=%v want DataValueValue(0x1)", wv.Value.EncodingMask)
	}

	// 关键:编码后的 DataValue 必须包含值内容(掩码字节 1 + variant 至少 2 字节)
	encoded, err := wv.Value.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) <= 1 {
		t.Fatalf("encoded DataValue too short (%d bytes), value not serialized", len(encoded))
	}
	// 解码回读,确认值完好
	var decoded ua.DataValue
	if _, err := decoded.Decode(encoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Value == nil || decoded.Value.Value() != int32(42) {
		t.Fatalf("decoded value: %v", decoded.Value)
	}

	// 非法地址 / 类型不匹配应失败
	if _, ok := buildWriteValue(model.Point{Name: "bad", Address: "ns=abc;i=1", DataType: model.DataTypeInt32}, float64(1)); ok {
		t.Fatal("invalid address should fail")
	}
	if _, ok := buildWriteValue(point, "not-an-int"); ok {
		t.Fatal("type mismatch should fail")
	}
}

func TestEndpointKey(t *testing.T) {
	drv := &opcuaDriver{}
	if got := drv.EndpointKey(json.RawMessage(`{"endpoint":"opc.tcp://192.168.1.5:4840"}`)); got != "tcp|192.168.1.5:4840" {
		t.Errorf("EndpointKey=%q", got)
	}
	if got, want := drv.EndpointKey(json.RawMessage(`{"endpoint":" OPC.TCP://Server "}`)), "tcp|server:4840"; got != want {
		t.Errorf("default port EndpointKey=%q want %q", got, want)
	}
	if got := drv.EndpointKey(json.RawMessage(`{"endpoint":""}`)); got != "" {
		t.Errorf("missing endpoint should yield empty key, got %q", got)
	}
	if got := drv.EndpointKey(json.RawMessage(`{"endpoint":"::::"}`)); got != "" {
		t.Errorf("invalid url should yield empty key, got %q", got)
	}
}
