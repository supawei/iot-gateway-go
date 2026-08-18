package smardaten

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output/mqtttest"
	"iot-gateway-go/internal/output/mqttutil"
)

// TestConfigUnmarshalMixedTypes 验证 Config 能同时接受数字和字符串形式的配置值。
// Web UI 的 FieldEnum 控件发送字符串（如 "0"），FieldInt 发送数字（如 60），
// 甚至 FieldEnum 的默认值可能以数字形式（如 0）发送。Config 必须全部兼容。
func TestConfigUnmarshalMixedTypes(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantPub    int
		wantMaxPub int
	}{
		{
			name:       "all strings (user-selected enum values)",
			raw:        `{"broker":"tcp://x:1883","pubMode":"0","maxPubTime":"60"}`,
			wantPub:    0,
			wantMaxPub: 60,
		},
		{
			name:       "all numbers (enum defaults sent as numbers)",
			raw:        `{"broker":"tcp://x:1883","pubMode":0,"maxPubTime":60}`,
			wantPub:    0,
			wantMaxPub: 60,
		},
		{
			name:       "mixed types",
			raw:        `{"broker":"tcp://x:1883","pubMode":1,"maxPubTime":"30"}`,
			wantPub:    1,
			wantMaxPub: 30,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(tc.raw), &cfg); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if cfg.Broker != "tcp://x:1883" {
				t.Errorf("broker = %q, want %q", cfg.Broker, "tcp://x:1883")
			}
			if int(cfg.PubMode) != tc.wantPub {
				t.Errorf("pubMode = %d, want %d", cfg.PubMode, tc.wantPub)
			}
			if int(cfg.MaxPubTime) != tc.wantMaxPub {
				t.Errorf("maxPubTime = %d, want %d", cfg.MaxPubTime, tc.wantMaxPub)
			}
		})
	}
}

// TestConfigUnmarshalMissingOptional 验证缺省字段能正常解析（空值）。
func TestConfigUnmarshalMissingOptional(t *testing.T) {
	raw := `{"broker":"tcp://x:1883"}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if cfg.Broker != "tcp://x:1883" {
		t.Errorf("broker = %q, want %q", cfg.Broker, "tcp://x:1883")
	}
	if cfg.PubMode != 0 {
		t.Errorf("pubMode should default to 0, got %d", cfg.PubMode)
	}
}

// TestFlexIntInvalid 验证非法值返回错误。
// null 合法（视为 0，与 Go 对 int 的默认行为一致），其余类型均报错。
func TestFlexIntInvalid(t *testing.T) {
	cases := []string{
		`"abc"`,
		`true`,
		`[1]`,
		`{"a":1}`,
	}
	for _, raw := range cases {
		var f flexInt
		if err := json.Unmarshal([]byte(raw), &f); err == nil {
			t.Errorf("expected error for %s, got none", raw)
		}
	}
	// null 视为 0，不报错
	var f flexInt
	if err := json.Unmarshal([]byte(`null`), &f); err != nil {
		t.Errorf("null should be accepted, got error: %v", err)
	}
	if f != 0 {
		t.Errorf("null should set value to 0, got %d", f)
	}
}

// TestDefaultCredentials 验证内置的平台固定凭据（应用 ID + RSA 公钥）合法可用。
func TestDefaultCredentials(t *testing.T) {
	if defaultIotAppID == "" {
		t.Fatal("defaultIotAppID must not be empty")
	}
	enc, err := encryptAppID(defaultIotAppID, []byte(defaultIotPublicKey))
	if err != nil {
		t.Fatalf("encryptAppID with default credentials failed: %v", err)
	}
	if enc == "" {
		t.Fatal("encrypted appId must not be empty")
	}
}

// TestPendingBufferCap 待上报缓冲达到全局上限后丢弃新点,内存有界(断连场景兜底)。
func TestPendingBufferCap(t *testing.T) {
	o := &platformOutput{
		topics:  newTopicMapping(),
		pending: make(map[string][]model.DataPoint),
	}
	o.topics.buildFrom(&ApplicationConfig{Devices: []PlatformDevice{{DeviceID: "d1"}}})

	dp := model.DataPoint{DeviceID: "d1", Point: "p", Value: 1.0, Timestamp: time.Now()}
	for i := 0; i < maxPendingPoints+10; i++ {
		o.Publish(dp)
	}
	if o.pendingCount != maxPendingPoints {
		t.Fatalf("pendingCount = %d, want %d", o.pendingCount, maxPendingPoints)
	}
	if len(o.pending["d1"]) != maxPendingPoints {
		t.Fatalf("pending[d1] = %d, want %d", len(o.pending["d1"]), maxPendingPoints)
	}
}

// TestFlushResetsPendingCount flush 换出缓冲后计数归零,后续 Publish 可继续入队。
func TestFlushResetsPendingCount(t *testing.T) {
	o := &platformOutput{
		topics:      newTopicMapping(),
		pending:     make(map[string][]model.DataPoint),
		connects:    make(map[string]bool),
		disconnects: make(map[string]bool),
		connected:   make(map[string]bool),
	}
	// d1 无 post 事件→eventTopic 为空→flush 跳过发布,不会触碰 nil client。
	o.topics.buildFrom(&ApplicationConfig{Devices: []PlatformDevice{{DeviceID: "d1"}}})

	dp := model.DataPoint{DeviceID: "d1", Point: "p", Value: 1.0, Timestamp: time.Now()}
	o.Publish(dp)
	o.flush()
	if o.pendingCount != 0 {
		t.Fatalf("pendingCount after flush = %d, want 0", o.pendingCount)
	}
	o.Publish(dp)
	if o.pendingCount != 1 {
		t.Fatalf("pendingCount after re-publish = %d, want 1", o.pendingCount)
	}
}

// TestNewNonBlockingUnreachable broker 不可达(黑洞地址)时,New 必须在 ConnectProbe 附近
// 返回且不报错(非阻塞构造),这是「启动不卡」在 smardaten 的回归测试。
func TestNewNonBlockingUnreachable(t *testing.T) {
	start := time.Now()
	out, err := New(Config{Broker: "tcp://192.0.2.1:1883", ClientID: "t", IotConfigPath: "/nonexistent/application.json"}, "gw-01", nil, nil, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("New should not fail on unreachable broker, got %v", err)
	}
	if out == nil {
		t.Fatal("New returned nil output")
	}
	if elapsed > mqttutil.ConnectProbe+time.Second {
		t.Fatalf("New blocked for %v, want ≤ %v+1s", elapsed, mqttutil.ConnectProbe)
	}
	out.Close()
}

// TestNewWithReachableBroker broker 可达(静默 broker 正常回 CONNACK)时,OnConnect 会在
// Connect 内同步触发;若 client 尚未赋值会 nil 解引用 panic。此测试守护该时序回归。
func TestNewWithReachableBroker(t *testing.T) {
	b := mqtttest.StartSilent(t)
	start := time.Now()
	out, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "t", IotConfigPath: "/nonexistent/application.json"}, "gw-01", nil, nil, nil)
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
	o := &platformOutput{
		topics:  newTopicMapping(),
		pending: make(map[string][]model.DataPoint),
	}
	o.topics.buildFrom(&ApplicationConfig{Devices: []PlatformDevice{{DeviceID: "d1"}}})

	dp := model.DataPoint{DeviceID: "d1", Point: "p", Value: 1.0, Timestamp: time.Now()}
	for i := 0; i < 5; i++ {
		o.Publish(dp)
	}
	st := o.RuntimeStatus()
	if st.Pending != 5 {
		t.Fatalf("pending = %d, want 5", st.Pending)
	}
	if st.Connected || st.ConnectionOpen {
		t.Fatal("nil client should report not connected")
	}
}

// captureClient 是 paho.Client 的测试替身:记录最后一次 Publish 的 topic 与 payload。
type captureClient struct {
	mu      sync.Mutex
	topic   string
	payload []byte
}

func (c *captureClient) IsConnected() bool       { return true }
func (c *captureClient) IsConnectionOpen() bool  { return true }
func (c *captureClient) Connect() pahomqtt.Token { return &pahomqtt.DummyToken{} }
func (c *captureClient) Disconnect(uint)         {}
func (c *captureClient) Publish(topic string, _ byte, _ bool, payload interface{}) pahomqtt.Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.topic = topic
	if b, ok := payload.([]byte); ok {
		c.payload = b
	} else {
		c.payload = nil
	}
	return &pahomqtt.DummyToken{}
}
func (c *captureClient) Subscribe(string, byte, pahomqtt.MessageHandler) pahomqtt.Token {
	return &pahomqtt.DummyToken{}
}
func (c *captureClient) SubscribeMultiple(map[string]byte, pahomqtt.MessageHandler) pahomqtt.Token {
	return &pahomqtt.DummyToken{}
}
func (c *captureClient) Unsubscribe(...string) pahomqtt.Token     { return &pahomqtt.DummyToken{} }
func (c *captureClient) AddRoute(string, pahomqtt.MessageHandler) {}
func (c *captureClient) OptionsReader() pahomqtt.ClientOptionsReader {
	return pahomqtt.ClientOptionsReader{}
}

func (c *captureClient) last() (string, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.topic, c.payload
}

// TestServiceGetReturnsLatestValues 回归:服务调用 get 应返回设备当前属性值,
// 即使待发缓冲已被 flusher 清空,params 也不能只剩 deviceId + reportTime。
// (修复前 handleServiceGet 只读 pending 缓冲,缓冲为空时返回
// {"deviceId":..., "reportTime":...},与设计不符。)
func TestServiceGetReturnsLatestValues(t *testing.T) {
	cfg := &ApplicationConfig{
		Devices: []PlatformDevice{{
			DeviceID: "dev1",
			Properties: []PlatformProperty{
				{Identifier: "temperature", PointID: "p1"},
				{Identifier: "switch", PointID: "p2"},
			},
			Services: []PlatformService{{
				Method:         "/svc/get",
				ResponseMethod: "/svc/get_reply",
			}},
		}},
	}

	o := &platformOutput{
		topics:  newTopicMapping(),
		pending: make(map[string][]model.DataPoint),
		// 模拟 main 注入的 values.Registry 查询:设备已有最新采集值,但 pending 为空。
		latestValues: func(deviceID string) map[string]interface{} {
			return map[string]interface{}{
				"p1": 23.456,
				"p2": 1.0,
			}
		},
	}
	o.topics.buildFrom(cfg)

	client := &captureClient{}
	o.client = client

	o.handleServiceGet(ServiceCallMessage{
		ServiceType: "get",
		DeviceID:    "dev1",
		CmdID:       "cmd-1",
	}, "/svc/get_reply", 1234567890123)

	topic, payload := client.last()
	if topic != "/svc/get_reply" {
		t.Fatalf("resp topic = %q, want /svc/get_reply", topic)
	}

	var resp ServiceGetResponseMessage
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.CmdID != "cmd-1" || resp.StatusCode != 0 || resp.Version != "1.0" || resp.ReportTime != 1234567890123 {
		t.Fatalf("unexpected response envelope: %+v", resp)
	}
	if got := resp.Params["temperature"]; got != 23.46 {
		t.Errorf("temperature = %v, want 23.46 (rounded)", got)
	}
	if got := resp.Params["switch"]; got != 1.0 {
		t.Errorf("switch = %v, want 1.0", got)
	}
	if resp.Params["deviceId"] != "dev1" {
		t.Errorf("deviceId = %v, want dev1", resp.Params["deviceId"])
	}
	// 关键断言:params 必须包含实际属性,不能只有 deviceId + reportTime。
	if len(resp.Params) < 4 {
		t.Errorf("params only has %d entries, want ≥4 (deviceId+reportTime+temperature+switch), got %v", len(resp.Params), resp.Params)
	}
}

// TestServiceGetFallbackPending latestValues 未注入(直接构造)时,get 回退到待发缓冲取最新值。
func TestServiceGetFallbackPending(t *testing.T) {
	cfg := &ApplicationConfig{
		Devices: []PlatformDevice{{
			DeviceID: "dev1",
			Properties: []PlatformProperty{
				{Identifier: "temperature", PointID: "p1"},
			},
			Services: []PlatformService{{
				Method:         "/svc/get",
				ResponseMethod: "/svc/get_reply",
			}},
		}},
	}

	o := &platformOutput{
		topics:  newTopicMapping(),
		pending: make(map[string][]model.DataPoint),
	}
	o.topics.buildFrom(cfg)

	now := time.Now()
	o.pending["dev1"] = []model.DataPoint{
		{DeviceID: "dev1", Point: "p1", Value: 36.6, Timestamp: now},
	}

	client := &captureClient{}
	o.client = client

	o.handleServiceGet(ServiceCallMessage{
		ServiceType: "get",
		DeviceID:    "dev1",
		CmdID:       "cmd-2",
	}, "/svc/get_reply", 1234567890123)

	_, payload := client.last()
	var resp ServiceGetResponseMessage
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got := resp.Params["temperature"]; got != 36.6 {
		t.Errorf("temperature = %v, want 36.6 (from pending)", got)
	}
}
