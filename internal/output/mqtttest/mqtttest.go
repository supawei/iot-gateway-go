// Package mqtttest 提供测试用假 MQTT broker,仅供各输出包 *_test.go 使用。
// 不参与生产二进制链接(仅被测试代码 import)。
package mqtttest

import (
	"io"
	"net"
	"testing"
)

// SilentBroker 接受 TCP 连接、应答 CONNACK 后保持静默,模拟「broker 在线但不响应」的
// 半死场景:QoS1 publish 永远得不到 PUBACK,用于验证有界等待不永久阻塞。
type SilentBroker struct {
	Addr string
	ln   net.Listener
	done chan struct{}
}

// StartSilent 启动一个监听在临时端口上的静默假 broker。
func StartSilent(tb testing.TB) *SilentBroker {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("mqtttest listen: %v", err)
	}
	b := &SilentBroker{Addr: ln.Addr().String(), ln: ln, done: make(chan struct{})}
	go b.acceptLoop()
	tb.Cleanup(b.Close)
	return b
}

// Close 关闭监听与所有存活连接。
func (b *SilentBroker) Close() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	b.ln.Close()
}

func (b *SilentBroker) acceptLoop() {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		go handleConn(c, b.done)
	}
}

func handleConn(c net.Conn, done chan struct{}) {
	defer c.Close()
	// 读 CONNECT 包:首字节高 4 位 = 1,剩余长度用变长编码读取后丢弃 body。
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return
	}
	if hdr[0]>>4 != 1 {
		return
	}
	rl := int(hdr[1])
	mult := 1
	i := 2
	for hdr[1]&0x80 != 0 && i < 4 {
		buf := make([]byte, 1)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		rl += int(buf[0]&0x7f) * mult
		mult *= 128
		i++
	}
	body := make([]byte, rl)
	if _, err := io.ReadFull(c, body); err != nil {
		return
	}
	// CONNACK 成功:0x20 0x02 0x00 0x00
	if _, err := c.Write([]byte{0x20, 0x02, 0x00, 0x00}); err != nil {
		return
	}
	// 之后保持静默,直到 broker 关闭。
	<-done
}
