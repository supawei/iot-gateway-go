package byteorder

import (
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want Order
		err  bool
	}{
		{"", ABCD, false},
		{"ABCD", ABCD, false},
		{"abcd", ABCD, false},
		{" AbCd ", ABCD, false},
		{"BADC", BADC, false},
		{"CDAB", CDAB, false},
		{"DCBA", DCBA, false},
		{"XYZ", "", true},
		{"ABC", "", true},
	}
	for _, tc := range tests {
		got, err := Parse(tc.in)
		if (err != nil) != tc.err {
			t.Fatalf("Parse(%q) err=%v want err=%v", tc.in, err, tc.err)
		}
		if !tc.err && got != tc.want {
			t.Fatalf("Parse(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestUint32Permutations(t *testing.T) {
	// wire 4 字节 [0x01 0x02][0x03 0x04](两寄存器大端)
	raw := []byte{0x01, 0x02, 0x03, 0x04}
	tests := []struct {
		order Order
		want  uint32
	}{
		{ABCD, 0x01020304},
		{CDAB, 0x03040102},
		{BADC, 0x02010403},
		{DCBA, 0x04030201},
	}
	for _, tc := range tests {
		if got := Uint32(tc.order, raw); got != tc.want {
			t.Fatalf("Uint32(%s)=%#x want %#x", tc.order, got, tc.want)
		}
	}
}

func TestPutUint32RoundTrip(t *testing.T) {
	const v = uint32(0x12345678)
	for _, order := range []Order{ABCD, BADC, CDAB, DCBA} {
		b := make([]byte, 4)
		PutUint32(order, b, v)
		if got := Uint32(order, b); got != v {
			t.Fatalf("roundtrip %s: %#x != %#x (bytes %x)", order, got, v, b)
		}
	}
}

func TestRegistersUint32(t *testing.T) {
	// 相邻寄存器 [0x0102][0x0304],与字节级置换一致
	tests := []struct {
		order Order
		want  uint32
	}{
		{ABCD, 0x01020304},
		{CDAB, 0x03040102},
		{BADC, 0x02010403},
		{DCBA, 0x04030201},
	}
	for _, tc := range tests {
		if got := RegistersUint32(tc.order, 0x0102, 0x0304); got != tc.want {
			t.Fatalf("RegistersUint32(%s)=%#x want %#x", tc.order, got, tc.want)
		}
	}
}

func TestRegistersUint32ConsistentWithBytes(t *testing.T) {
	// 同一 32 位值:寄存器级组合应等于字节级置换后的大端解释
	for _, order := range []Order{ABCD, BADC, CDAB, DCBA} {
		hi, lo := uint16(0xDEAD), uint16(0xBEEF)
		fromRegs := RegistersUint32(order, hi, lo)
		raw := []byte{byte(hi >> 8), byte(hi), byte(lo >> 8), byte(lo)}
		fromBytes := Uint32(order, raw)
		if fromRegs != fromBytes {
			t.Fatalf("%s: registers=%#x bytes=%#x", order, fromRegs, fromBytes)
		}
	}
}
