package mqtt

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/output/mqtttest"
	"iot-gateway-go/internal/output/mqttutil"
)

// TestNewNonBlockingUnreachable broker 不可达(黑洞地址)时,New 必须在 ConnectProbe 附近
// 返回且不报错(非阻塞构造),这是「启动不卡」的回归测试。
func TestNewNonBlockingUnreachable(t *testing.T) {
	start := time.Now()
	out, err := New(Config{Broker: "tcp://192.0.2.1:1883", ClientID: "test", QoS: 1}, "gw-01")
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

// TestPublishTimeout 半死 broker 下,Publish 应在 PublishTimeout 内返回 ErrPublishTimeout,
// 绝不永久阻塞(「发送不卡」的回归测试)。
func TestPublishTimeout(t *testing.T) {
	b := mqtttest.StartSilent(t)
	out, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "test", QoS: 1}, "gw-01")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer out.Close()

	start := time.Now()
	err = out.Publish(model.DataPoint{DeviceID: "d1", Point: "temperature", Value: 25.5, Timestamp: time.Now()})
	elapsed := time.Since(start)
	if !errors.Is(err, mqttutil.ErrPublishTimeout) {
		t.Fatalf("Publish err = %v, want ErrPublishTimeout", err)
	}
	if elapsed > mqttutil.PublishTimeout+time.Second {
		t.Fatalf("Publish blocked for %v, want ≤ %v+1s", elapsed, mqttutil.PublishTimeout)
	}
}

// TestRuntimeStatusConnected 连接建立后 RuntimeStatus 应上报 connected=true。
func TestRuntimeStatusConnected(t *testing.T) {
	b := mqtttest.StartSilent(t)
	out, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "st", QoS: 1}, "gw-01")
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer out.Close()
	mo := out.(*mqttOutput)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := mo.RuntimeStatus()
		if st.ConnectionOpen {
			if !st.Connected {
				t.Fatal("connected should be true when connection is open")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("connection never opened")
}

// waitMQTTConnected 轮询等待 MQTT 连接建立(RecordingBroker 会 CONNACK)。
func waitMQTTConnected(t *testing.T, out output.Output) {
	t.Helper()
	mo := out.(*mqttOutput)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mo.client.IsConnected() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("mqtt connection never established")
}

// waitMessages 轮询直到 broker 记录到至少 n 条消息。
func waitMessages(t *testing.T, b *mqtttest.RecordingBroker, n int) []mqtttest.RecordedMessage {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if msgs := b.Messages(); len(msgs) >= n {
			return msgs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("got %d messages, want >= %d", len(b.Messages()), n)
	return nil
}

// TestBatchPublishesGroupedByDevice 批量模式:同设备多点聚合为一条消息(数组 payload),
// 不同设备分条;默认即时模式仍是单点单条。
func TestBatchPublishesGroupedByDevice(t *testing.T) {
	b := mqtttest.StartRecording(t)
	out, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "batch", QoS: 1, FlushInterval: "50ms"}, "gw-01")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer out.Close()
	waitMQTTConnected(t, out)

	now := time.Now()
	for i := 0; i < 3; i++ {
		out.Publish(model.DataPoint{DeviceID: "d1", Point: "p", Value: i, Timestamp: now})
	}
	for i := 0; i < 2; i++ {
		out.Publish(model.DataPoint{DeviceID: "d2", Point: "p", Value: i, Timestamp: now})
	}

	msgs := waitMessages(t, b, 2)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (one per device)", len(msgs))
	}
	topicD1 := "gateway/gw-01/device/d1/data"
	topicD2 := "gateway/gw-01/device/d2/data"
	if (msgs[0].Topic == topicD1 && msgs[1].Topic == topicD2) ||
		(msgs[0].Topic == topicD2 && msgs[1].Topic == topicD1) {
		// ok
	} else {
		t.Fatalf("unexpected topics: %q, %q", msgs[0].Topic, msgs[1].Topic)
	}
	for _, m := range msgs {
		var pts []model.DataPoint
		if err := json.Unmarshal(m.Payload, &pts); err != nil {
			t.Fatalf("batch payload not an array of DataPoint: %v (payload=%s)", err, m.Payload)
		}
		want := 3
		if m.Topic == topicD2 {
			want = 2
		}
		if len(pts) != want {
			t.Fatalf("topic %s has %d points, want %d", m.Topic, len(pts), want)
		}
	}
}

// TestBatchMaxSplits 单条消息点数超过 batchMax 时拆分。
func TestBatchMaxSplits(t *testing.T) {
	b := mqtttest.StartRecording(t)
	out, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "split", QoS: 1, FlushInterval: "50ms", BatchMax: 2}, "gw-01")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer out.Close()
	waitMQTTConnected(t, out)

	now := time.Now()
	for i := 0; i < 5; i++ {
		out.Publish(model.DataPoint{DeviceID: "d1", Point: "p", Value: i, Timestamp: now})
	}

	topic := "gateway/gw-01/device/d1/data"
	msgs := waitMessages(t, b, 3)
	for _, m := range msgs {
		if m.Topic != topic {
			t.Fatalf("unexpected topic %q, want %q", m.Topic, topic)
		}
		var pts []model.DataPoint
		if err := json.Unmarshal(m.Payload, &pts); err != nil {
			t.Fatal(err)
		}
		if len(pts) > 2 {
			t.Fatalf("batch chunk has %d points, want <= 2", len(pts))
		}
	}
	total := 0
	for _, m := range msgs {
		var pts []model.DataPoint
		json.Unmarshal(m.Payload, &pts)
		total += len(pts)
	}
	if total != 5 {
		t.Fatalf("total points across chunks = %d, want 5", total)
	}
}

// TestBatchCloseFlushes Close 会发送剩余缓冲。
func TestBatchCloseFlushes(t *testing.T) {
	b := mqtttest.StartRecording(t)
	out, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "close", QoS: 1, FlushInterval: "1h"}, "gw-01")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	waitMQTTConnected(t, out)

	now := time.Now()
	out.Publish(model.DataPoint{DeviceID: "d1", Point: "p", Value: 1, Timestamp: now})
	out.Publish(model.DataPoint{DeviceID: "d1", Point: "q", Value: 2, Timestamp: now})

	out.Close() // 1h 的窗口不会触发 ticker,必须由 Close 的 flush 发出

	msgs := b.Messages()
	if len(msgs) != 1 {
		t.Fatalf("messages after Close = %d, want 1", len(msgs))
	}
	var pts []model.DataPoint
	if err := json.Unmarshal(msgs[0].Payload, &pts); err != nil {
		t.Fatalf("payload not array: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("close flush sent %d points, want 2", len(pts))
	}
}

// TestImmediateSingleObject 默认即时模式:单点一条消息,payload 为单个对象而非数组。
func TestImmediateSingleObject(t *testing.T) {
	b := mqtttest.StartRecording(t)
	out, err := New(Config{Broker: "tcp://" + b.Addr, ClientID: "imm", QoS: 1}, "gw-01")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer out.Close()
	waitMQTTConnected(t, out)

	dp := model.DataPoint{DeviceID: "d1", Point: "temperature", Value: 25.5, Timestamp: time.Now()}
	if err := out.Publish(dp); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	msgs := waitMessages(t, b, 1)
	if msgs[0].Topic != "gateway/gw-01/device/d1/data" {
		t.Fatalf("topic = %q", msgs[0].Topic)
	}
	var got model.DataPoint
	if err := json.Unmarshal(msgs[0].Payload, &got); err != nil {
		t.Fatalf("immediate payload should be a single DataPoint object, got array-ish: %v (payload=%s)", err, msgs[0].Payload)
	}
	if got.Point != "temperature" {
		t.Fatalf("point = %q", got.Point)
	}
}
