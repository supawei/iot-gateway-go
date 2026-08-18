// Package byteorder 定义 Modbus 多寄存器(32 位)值的字节序。Modbus 协议中单个
// 寄存器(16 位)在总线上恒为大端传输,但跨两个寄存器组合 32 位值(int32/uint32/
// float32)时,不同设备存在四种常见字节序(ABCD/BADC/CDAB/DCBA)。本包提供统一的
// 字节序解析、校验与 32 位编解码,供 modbus 轮询驱动与 modbus_listen 监听驱动共用。
package byteorder

import (
	"fmt"
	"strings"
)

// Order 是 Modbus 32 位值的字节序。
type Order string

const (
	// ABCD 大端(默认):高字在前、高字节在前,即 wire 原样。
	ABCD Order = "ABCD"
	// BADC 高字在前、字内字节交换(高字在前、低字节在前)。
	BADC Order = "BADC"
	// CDAB 字交换(小端):低字在前、高字节在前。
	CDAB Order = "CDAB"
	// DCBA 字交换 + 字内字节交换。
	DCBA Order = "DCBA"
)

// Parse 解析并规范化字节序字符串:去空白、转大写。空串按默认 ABCD 处理,
// 非法值返回错误。
func Parse(s string) (Order, error) {
	o := Order(strings.ToUpper(strings.TrimSpace(s)))
	switch o {
	case "":
		return ABCD, nil
	case ABCD, BADC, CDAB, DCBA:
		return o, nil
	default:
		return "", fmt.Errorf("invalid modbus byte order %q (want ABCD/BADC/CDAB/DCBA)", s)
	}
}

// Perm 返回将 wire 4 字节 [b0 b1 b2 b3](寄存器 N=[b0 b1],N+1=[b2 b3])按 order
// 重排为逻辑大端序列的置换。该置换自逆(应用两次复原),故解码与编码共用。
func Perm(order Order) [4]int {
	switch order {
	case CDAB:
		return [4]int{2, 3, 0, 1}
	case BADC:
		return [4]int{1, 0, 3, 2}
	case DCBA:
		return [4]int{3, 2, 1, 0}
	default: // ABCD 及未知值均按原样(大端)
		return [4]int{0, 1, 2, 3}
	}
}

// Uint32 按 order 将 b(长度 ≥ 4,大端寄存器字节序)解释为 uint32。
func Uint32(order Order, b []byte) uint32 {
	p := Perm(order)
	return uint32(b[p[0]])<<24 | uint32(b[p[1]])<<16 | uint32(b[p[2]])<<8 | uint32(b[p[3]])
}

// PutUint32 按 order 将 v 写入 b(长度 ≥ 4,大端寄存器字节序)。
func PutUint32(order Order, b []byte, v uint32) {
	p := Perm(order)
	b[p[0]] = byte(v >> 24)
	b[p[1]] = byte(v >> 16)
	b[p[2]] = byte(v >> 8)
	b[p[3]] = byte(v)
}

// swap16 交换 16 位寄存器内的高/低字节(字内字节交换)。
func swap16(v uint16) uint16 { return v>>8 | v<<8 }

// RegistersUint32 按 order 从相邻两个寄存器(总线大端读出)组合 32 位值。
// hi/lo 为相邻寄存器的高地址/低地址寄存器值(如 regs[offset]、regs[offset+1])。
func RegistersUint32(order Order, hi, lo uint16) uint32 {
	switch order {
	case CDAB:
		hi, lo = lo, hi
	case BADC:
		hi, lo = swap16(hi), swap16(lo)
	case DCBA:
		hi, lo = swap16(lo), swap16(hi)
	}
	return uint32(hi)<<16 | uint32(lo)
}
