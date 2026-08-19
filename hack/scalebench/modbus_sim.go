// Package main 规模化压测 harness(见 docs/scale-testing.md)。
//
// 本文件:Modbus TCP 模拟从站。监听 TCP,按 MBAP 解析并应答 Modbus 请求:
// 支持 FC1/FC2 读线圈/离散量、FC3/FC4 读保持/输入寄存器、FC6/FC16 写单/多寄存器。
// 寄存器数据为确定性生成(unitID<<8|addr),便于校验;统计请求/读取/写入/错误计数,
// 供 harness 直接读取以评估南向请求速率。
package main

import (
	"encoding/binary"
	"io"
	"net"
	"sync/atomic"
)

// modbusSim 是 Modbus TCP 模拟从站。
type modbusSim struct {
	ln net.Listener

	reqs    atomic.Int64 // 收到的 MBAP 请求总数
	reads   atomic.Int64 // 读类请求(FC1-4)数
	writes  atomic.Int64 // 写类请求(FC6/16)数
	errs    atomic.Int64 // 解析/异常响应数
	readReg atomic.Int64 // 累计读出的寄存器数(数据量参考)
}

// startModbusSim 启动模拟从站,返回 Addr 供网关连接。
func startModbusSim() (*modbusSim, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &modbusSim{ln: ln}
	go s.acceptLoop()
	return s, nil
}

func (s *modbusSim) Addr() string { return s.ln.Addr().String() }

// Close 关闭监听与所有存活连接。
func (s *modbusSim) Close() { s.ln.Close() }

func (s *modbusSim) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err == nil {
		}
		if err != nil {
			return
		}
		go s.serve(c)
	}
}

func (s *modbusSim) serve(c net.Conn) {
	defer c.Close()
	for {
		resp, err := s.handle(c)
		if err != nil {
			return
		}
		if len(resp) > 0 {
			if _, err := c.Write(resp); err != nil {
				return
			}
		}
	}
}

// handle 读取一个 Modbus TCP 请求帧并返回响应帧;连接异常返回 error。
// MBAP 帧:事务ID(2) + 协议ID(2) + 长度(2) + 长度字段之后的 N 字节(unit + PDU)。
// 注意:长度字段 = unit(1) + PDU 字节数,故读 6 字节头后再读 length 字节即可。
func (s *modbusSim) handle(c net.Conn) ([]byte, error) {
	hdr := make([]byte, 6) // txid(2) + proto(2) + length(2),不含 unit
	if _, err := io.ReadFull(c, hdr); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(hdr[4:6]))
	if length < 2 || length > 254 {
		s.errs.Add(1)
		return nil, nil
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c, body); err != nil {
		return nil, err
	}
	unit := body[0]
	fc := body[1]
	payload := body[2:]

	s.reqs.Add(1)
	respBody, ok := s.process(unit, fc, payload)
	if !ok {
		s.errs.Add(1)
		// 异常响应:fc | 0x80 + 异常码
		respBody = []byte{fc | 0x80, 0x01} // 非法功能
	}
	// 组装 MBAP 响应:txid(2) + proto(2) + 长度(2) + unit(1) + 响应体(fc+数据)。
	// 长度字段(2 字节)= unit(1) + 响应体字节数;grid-x 期望总帧长 = 长度 + 6。
	out := make([]byte, 0, 9+len(respBody))
	out = append(out, hdr[0], hdr[1]) // txid
	out = append(out, 0, 0)           // proto
	l := 1 + len(respBody)
	out = append(out, byte(l>>8), byte(l))
	out = append(out, unit)
	out = append(out, respBody...)
	return out, nil
}

// process 处理一个 Modbus 请求(unit + fc + payload),返回响应体(unit 之后的部分,
// 含功能码 fc),并指示是否成功。异常时返回 nil,false 由调用方构造异常帧。
func (s *modbusSim) process(unit byte, fc byte, payload []byte) ([]byte, bool) {
	switch fc {
	case 0x01, 0x02: // 读线圈 / 读离散输入
		if len(payload) < 4 {
			return nil, false
		}
		start := int(binary.BigEndian.Uint16(payload[0:2]))
		qty := int(binary.BigEndian.Uint16(payload[2:4]))
		if qty == 0 || qty > 2000 {
			return nil, false
		}
		s.reads.Add(1)
		byteCount := (qty + 7) / 8
		data := make([]byte, byteCount)
		for i := 0; i < qty; i++ {
			if (start+i+int(unit))%7 == 0 { // 确定性:部分线圈置位
				data[i/8] |= 1 << (i % 8)
			}
		}
		s.readReg.Add(int64(qty))
		resp := make([]byte, 2+len(data))
		resp[0] = fc
		resp[1] = byte(byteCount)
		copy(resp[2:], data)
		return resp, true

	case 0x03, 0x04: // 读保持 / 读输入寄存器
		if len(payload) < 4 {
			return nil, false
		}
		start := int(binary.BigEndian.Uint16(payload[0:2]))
		qty := int(binary.BigEndian.Uint16(payload[2:4]))
		if qty == 0 || qty > 125 {
			return nil, false
		}
		s.reads.Add(1)
		data := make([]byte, qty*2)
		for i := 0; i < qty; i++ {
			reg := uint16((int(unit) << 8) | ((start + i) & 0xff))
			binary.BigEndian.PutUint16(data[i*2:], reg)
		}
		s.readReg.Add(int64(qty))
		resp := make([]byte, 2+len(data))
		resp[0] = fc
		resp[1] = byte(qty * 2)
		copy(resp[2:], data)
		return resp, true

	case 0x06: // 写单寄存器
		if len(payload) < 4 {
			return nil, false
		}
		s.writes.Add(1)
		resp := make([]byte, 5)
		resp[0] = fc
		copy(resp[1:], payload[0:4]) // 回显 addr+value
		return resp, true

	case 0x10: // 写多寄存器
		if len(payload) < 5 {
			return nil, false
		}
		s.writes.Add(1)
		resp := make([]byte, 5)
		resp[0] = fc
		copy(resp[1:], payload[0:4]) // 回显 addr+quantity
		return resp, true

	default:
		return nil, false
	}
}
