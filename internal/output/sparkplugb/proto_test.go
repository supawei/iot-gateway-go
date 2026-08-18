package sparkplugb

import (
	"bytes"
	"testing"
	"time"
)

// TestProtoFixedTime 用固定时间戳构造 payload,断言精确字节(与 SparkplugB.proto 一致)。
func TestProtoFixedTime(t *testing.T) {
	now := time.UnixMilli(1_000_000_000) // 2001-09-09T01:46:40Z
	ts := uint64(now.UnixMilli())
	// 单 metric:name="temperature", alias=1, datatype=Double(10), value=25.5
	got := encodePayload(1, []metric{
		{name: "temperature", alias: 1, datatype: DataTypeDouble, timestamp: ts, value: 25.5},
	}, now)

	// 手工构造期望字节(25.5 的 float64 bits = 0x4039800000000000,LE 为 00..80 39 40):
	// Payload:
	//   field1 timestamp uint64: 0x08 + varint(1_000_000_000)
	//   field2 seq uint32:       0x10 + varint(1)
	//   field5 Metric(长度 32):
	//     field1 name "temperature": 0x0A 0x0B + "temperature"
	//     field2 alias 1:            0x10 0x01
	//     field3 timestamp:          0x18 + varint(ts)
	//     field4 datatype 10:        0x20 0x0A
	//     field8 double_value:       0x41 + 8 字节 LE(25.5)
	want := []byte{
		0x08, 0x80, 0x94, 0xeb, 0xdc, 0x03, // timestamp varint(1_000_000_000)
		0x10, 0x01, // seq = 1
		0x2a, 0x20, // field5, len 32
		0x0a, 0x0b, 't', 'e', 'm', 'p', 'e', 'r', 'a', 't', 'u', 'r', 'e',
		0x10, 0x01, // alias 1
		0x18, 0x80, 0x94, 0xeb, 0xdc, 0x03, // timestamp
		0x20, 0x0a, // datatype 10
		0x41, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x39, 0x40, // double 25.5 LE
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload bytes mismatch:\n got %x\nwant %x", got, want)
	}
}

// TestProtoEncodeDataTypes 各类值的编码能按 datatype 落到正确的 oneof 字段。
func TestProtoEncodeDataTypes(t *testing.T) {
	cases := []struct {
		datatype uint32
		value    interface{}
		want     []byte
	}{
		{DataTypeInt16, int16(-2), []byte{0x20, 0x02, 0x50, 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}}, // field10 int_value,负数补码
		{DataTypeInt32, int32(300), []byte{0x20, 0x03, 0x50, 0xac, 0x02}},
		{DataTypeUInt32, uint32(300), []byte{0x20, 0x07, 0x50, 0xac, 0x02}},
		{DataTypeInt64, int64(-1), []byte{0x20, 0x04, 0x50, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}},
		{DataTypeUInt64, uint64(300), []byte{0x20, 0x08, 0x58, 0xac, 0x02}},             // field11 long_value
		{DataTypeFloat, float32(1.5), []byte{0x20, 0x09, 0x4d, 0x00, 0x00, 0xc0, 0x3f}}, // field9 fixed32 LE
		{DataTypeBoolean, true, []byte{0x20, 0x0b, 0x60, 0x01}},                         // field12 boolean_value
		{DataTypeString, "hi", []byte{0x20, 0x0c, 0x6a, 0x02, 0x68, 0x69}},              // field13 string_value
	}
	for _, tc := range cases {
		got := encodeMetric(metric{datatype: tc.datatype, value: tc.value})
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("datatype %d: bytes mismatch:\n got %x\nwant %x", tc.datatype, got, tc.want)
		}
	}
}

// TestProtoEncodeIsNull is_null 置位时不携带 value 字段。
func TestProtoEncodeIsNull(t *testing.T) {
	got := encodeMetric(metric{name: "p", datatype: DataTypeDouble, isNull: true, value: nil})
	// 期望: name field(0x0a 0x01 'p'), datatype(0x20 0x0a), is_null(0x38 0x01), 无 value 字段。
	want := []byte{0x0a, 0x01, 'p', 0x20, 0x0a, 0x38, 0x01}
	if !bytes.Equal(got, want) {
		t.Fatalf("is_null metric mismatch:\n got %x\nwant %x", got, want)
	}
}

// TestProtoEncodeEmptyMetrics 空 metric 列表:payload 只含 timestamp+seq。
func TestProtoEncodeEmptyMetrics(t *testing.T) {
	now := time.UnixMilli(42)
	got := encodePayload(7, nil, now)
	want := []byte{0x08, 0x2a, 0x10, 0x07} // timestamp=42, seq=7
	if !bytes.Equal(got, want) {
		t.Fatalf("empty metrics mismatch:\n got %x\nwant %x", got, want)
	}
}

// TestProtoVarint 边界的 varint 编码(多字节)。
func TestProtoVarint(t *testing.T) {
	cases := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{16383, []byte{0xff, 0x7f}},
		{300, []byte{0xac, 0x02}},
		{1_000_000_000, []byte{0x80, 0x94, 0xeb, 0xdc, 0x03}},
	}
	for _, tc := range cases {
		if got := appendVarint(nil, tc.v); !bytes.Equal(got, tc.want) {
			t.Fatalf("varint(%d) = %x, want %x", tc.v, got, tc.want)
		}
	}
}

// TestProtoFixedValues fixed64/fixed32 小端编码。
func TestProtoFixedValues(t *testing.T) {
	if got := appendFixed64(nil, 0x0102030405060708); !bytes.Equal(got, []byte{0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01}) {
		t.Fatalf("fixed64 LE mismatch: %x", got)
	}
	if got := appendFixed32(nil, 0x01020304); !bytes.Equal(got, []byte{0x04, 0x03, 0x02, 0x01}) {
		t.Fatalf("fixed32 LE mismatch: %x", got)
	}
}
