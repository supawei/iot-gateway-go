package mqtttest

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
)

// RecordedMessage 是一条被 broker 记录到的 PUBLISH 消息。
type RecordedMessage struct {
	Topic   string
	Payload []byte
	QoS     byte
}

// RecordingBroker 是应答 CONNACK/PUBACK/SUBACK/PINGRESP 并记录 PUBLISH 消息的假 broker。
// 用于验证发布的条数/topic/payload(如 MQTT 批量发布的聚合与拆条)。
type RecordingBroker struct {
	Addr string
	ln   net.Listener
	done chan struct{}

	mu   sync.Mutex
	msgs []RecordedMessage
}

// StartRecording 启动一个记录型假 broker(测试结束时自动清理)。
func StartRecording(tb testing.TB) *RecordingBroker {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("mqtttest listen: %v", err)
	}
	b := &RecordingBroker{Addr: ln.Addr().String(), ln: ln, done: make(chan struct{})}
	go b.acceptLoop()
	tb.Cleanup(b.Close)
	return b
}

// Close 关闭监听与所有存活连接。
func (b *RecordingBroker) Close() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	b.ln.Close()
}

// Messages 返回已记录的 PUBLISH 消息副本(按到达顺序)。
func (b *RecordingBroker) Messages() []RecordedMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]RecordedMessage, len(b.msgs))
	copy(out, b.msgs)
	return out
}

func (b *RecordingBroker) acceptLoop() {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.serve(c)
	}
}

func (b *RecordingBroker) serve(c net.Conn) {
	defer c.Close()
	for {
		pkt, ok := readPacket(c)
		if !ok {
			return
		}
		switch pkt[0] >> 4 {
		case 1: // CONNECT → CONNACK 成功
			if _, err := c.Write([]byte{0x20, 0x02, 0x00, 0x00}); err != nil {
				return
			}
		case 3: // PUBLISH
			if !b.handlePublish(c, pkt[0], pkt[1:]) {
				return
			}
		case 8: // SUBSCRIBE → SUBACK
			if !handleSubscribe(c, pkt[1:]) {
				return
			}
		case 12: // PINGREQ → PINGRESP
			if _, err := c.Write([]byte{0xd0, 0x00}); err != nil {
				return
			}
		case 14: // DISCONNECT
			return
		}
	}
}

// readPacket 读一个 MQTT 包,返回 [固定头字节, body...](不含剩余长度字节)。
func readPacket(c net.Conn) ([]byte, bool) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return nil, false
	}
	rl, mult := 0, 1
	lengthByte := hdr[1]
	if lengthByte&0x80 == 0 {
		rl = int(lengthByte)
	} else {
		rl = int(lengthByte & 0x7f)
		for i := 0; i < 3; i++ { // 剩余长度最多 4 字节
			buf := make([]byte, 1)
			if _, err := io.ReadFull(c, buf); err != nil {
				return nil, false
			}
			mult *= 128
			rl += int(buf[0]&0x7f) * mult
			if buf[0]&0x80 == 0 {
				break
			}
		}
	}
	body := make([]byte, rl)
	if _, err := io.ReadFull(c, body); err != nil {
		return nil, false
	}
	out := make([]byte, 1, 1+len(body))
	out[0] = hdr[0]
	return append(out, body...), true
}

// handlePublish 记录消息;QoS>0 时回 PUBACK。
func (b *RecordingBroker) handlePublish(c net.Conn, header byte, body []byte) bool {
	qos := (header >> 1) & 0x03
	if len(body) < 2 {
		return false
	}
	topicLen := int(binary.BigEndian.Uint16(body[0:2]))
	if len(body) < 2+topicLen {
		return false
	}
	topic := string(body[2 : 2+topicLen])
	idx := 2 + topicLen

	if qos > 0 {
		if len(body) < idx+2 {
			return false
		}
		pktID := body[idx : idx+2]
		msg := RecordedMessage{Topic: topic, Payload: append([]byte(nil), body[idx+2:]...), QoS: qos}
		b.mu.Lock()
		b.msgs = append(b.msgs, msg)
		b.mu.Unlock()
		// PUBACK:0x40 0x02 packetID
		if _, err := c.Write([]byte{0x40, 0x02, pktID[0], pktID[1]}); err != nil {
			return false
		}
		return true
	}
	msg := RecordedMessage{Topic: topic, Payload: append([]byte(nil), body[idx:]...), QoS: qos}
	b.mu.Lock()
	b.msgs = append(b.msgs, msg)
	b.mu.Unlock()
	return true
}

// handleSubscribe 回 SUBACK(granted QoS 0),返回码数与 topic filter 数一致。
func handleSubscribe(c net.Conn, body []byte) bool {
	if len(body) < 2 {
		return false
	}
	pktID := body[0:2]
	count := 0
	idx := 2
	for idx+2 <= len(body) {
		tl := int(binary.BigEndian.Uint16(body[idx : idx+2]))
		if idx+2+tl+1 > len(body) {
			break
		}
		idx += 2 + tl + 1 // topic + QoS byte
		count++
	}
	if count == 0 {
		count = 1
	}
	resp := make([]byte, 4+count)
	resp[0] = 0x90
	resp[1] = byte(2 + count)
	resp[2], resp[3] = pktID[0], pktID[1]
	for i := 0; i < count; i++ {
		resp[4+i] = 0x00 // granted QoS 0
	}
	if _, err := c.Write(resp); err != nil {
		return false
	}
	return true
}
