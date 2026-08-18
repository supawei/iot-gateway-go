package mqttutil

import (
	"errors"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"iot-gateway-go/internal/output/mqtttest"
)

// TestWaitTokenTimeout 半死 broker(接受连接但不应答 PUBACK)下,QoS1 publish 的 token
// 永远等不到 ack;WaitToken 应在 PublishTimeout 内返回 ErrPublishTimeout,不永久阻塞。
func TestWaitTokenTimeout(t *testing.T) {
	b := mqtttest.StartSilent(t)
	opts := paho.NewClientOptions()
	opts.AddBroker("tcp://" + b.Addr)
	opts.SetClientID("wait-token-timeout")
	ApplyResilience(opts)
	c := paho.NewClient(opts)
	if tok := c.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("connect failed: %v", tok.Error())
	}
	defer c.Disconnect(100)

	start := time.Now()
	err := WaitToken(c.Publish("t", 1, false, []byte("hi")), PublishTimeout)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrPublishTimeout) {
		t.Fatalf("err = %v, want ErrPublishTimeout", err)
	}
	if elapsed > PublishTimeout+time.Second {
		t.Fatalf("WaitToken blocked for %v, want ≤ %v+1s", elapsed, PublishTimeout)
	}
}

// TestWaitTokenOK QoS0 publish 无需 ack,应立即完成。
func TestWaitTokenOK(t *testing.T) {
	b := mqtttest.StartSilent(t)
	opts := paho.NewClientOptions()
	opts.AddBroker("tcp://" + b.Addr)
	opts.SetClientID("wait-token-ok")
	ApplyResilience(opts)
	c := paho.NewClient(opts)
	if tok := c.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("connect failed: %v", tok.Error())
	}
	defer c.Disconnect(100)

	if err := WaitToken(c.Publish("t", 0, false, []byte("hi")), PublishTimeout); err != nil {
		t.Fatalf("QoS0 publish should complete, got %v", err)
	}
}

// TestConnectNonBlockingUnreachable broker 不可达(黑洞地址)时,ConnectNonBlocking
// 应在 ConnectProbe 附近返回(仅日志探测),绝不无限阻塞。
func TestConnectNonBlockingUnreachable(t *testing.T) {
	opts := paho.NewClientOptions()
	opts.AddBroker("tcp://192.0.2.1:1883") // TEST-NET-1,拨号必超时
	opts.SetClientID("connect-nonblocking")
	ApplyResilience(opts)
	c := paho.NewClient(opts)

	start := time.Now()
	ConnectNonBlocking(c, "test")
	elapsed := time.Since(start)
	if elapsed > ConnectProbe+time.Second {
		t.Fatalf("ConnectNonBlocking blocked for %v, want ≤ %v+1s", elapsed, ConnectProbe)
	}
	c.Disconnect(100)
}

// TestConnectNonBlockingFast broker 可达(静默 broker 会正常回 CONNACK)时快速完成。
func TestConnectNonBlockingFast(t *testing.T) {
	b := mqtttest.StartSilent(t)
	opts := paho.NewClientOptions()
	opts.AddBroker("tcp://" + b.Addr)
	opts.SetClientID("connect-nonblocking-fast")
	ApplyResilience(opts)
	c := paho.NewClient(opts)

	start := time.Now()
	ConnectNonBlocking(c, "test")
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("ConnectNonBlocking to reachable broker took %v, want fast", elapsed)
	}
	c.Disconnect(100)
}
