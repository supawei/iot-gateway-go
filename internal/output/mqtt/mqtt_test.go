package mqtt

import (
	"errors"
	"testing"
	"time"

	"iot-gateway-go/internal/model"
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
