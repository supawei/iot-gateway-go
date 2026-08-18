package modbus

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"iot-gateway-go/internal/driver/byteorder"
	"iot-gateway-go/internal/model"
)

func TestParseAddress(t *testing.T) {
	tests := []struct {
		addr     string
		function string
		register uint16
		wantErr  bool
	}{
		{"holding:0", "holding", 0, false},
		{"coil:2", "coil", 2, false},
		{"input:65535", "input", 65535, false},
		{"holding", "", 0, true},
		{"holding:-1", "", 0, true},
		{"holding:65536", "", 0, true},
	}
	for _, tc := range tests {
		function, register, err := parseAddress(tc.addr)
		if (err != nil) != tc.wantErr {
			t.Fatalf("parseAddress(%q) err=%v wantErr=%v", tc.addr, err, tc.wantErr)
		}
		if !tc.wantErr && (function != tc.function || register != tc.register) {
			t.Fatalf("parseAddress(%q)=%q,%d want %q,%d", tc.addr, function, register, tc.function, tc.register)
		}
	}
}

func TestQuantityOf(t *testing.T) {
	tests := map[model.DataType]uint16{
		model.DataTypeBool:   1,
		model.DataTypeInt16:  1,
		model.DataTypeUInt16: 1,
		model.DataTypeInt32:  2,
		model.DataTypeUInt32: 2,
		model.DataTypeFloat:  2,
	}
	for dataType, want := range tests {
		if got := quantityOf(dataType); got != want {
			t.Fatalf("quantityOf(%s)=%d want %d", dataType, got, want)
		}
	}
}

func TestDecodeValue(t *testing.T) {
	if v, err := decodeValue(model.DataTypeBool, []byte{1}, byteorder.ABCD); err != nil || v != true {
		t.Fatalf("bool decode: %v err=%v", v, err)
	}
	if v, err := decodeValue(model.DataTypeInt16, []byte{0x01, 0x02}, byteorder.ABCD); err != nil || v != int16(258) {
		t.Fatalf("int16 decode: %v err=%v", v, err)
	}
	if v, err := decodeValue(model.DataTypeUInt16, []byte{0x01, 0x02}, byteorder.ABCD); err != nil || v != uint16(258) {
		t.Fatalf("uint16 decode: %v err=%v", v, err)
	}
	if v, err := decodeValue(model.DataTypeInt32, []byte{0x00, 0x00, 0x01, 0x02}, byteorder.ABCD); err != nil || v != int32(258) {
		t.Fatalf("int32 decode: %v err=%v", v, err)
	}
	if v, err := decodeValue(model.DataTypeFloat, []byte{0x43, 0x7A, 0x00, 0x00}, byteorder.ABCD); err != nil || v != float32(250.0) {
		t.Fatalf("float decode: %v err=%v", v, err)
	}
	if _, err := decodeValue(model.DataTypeInt16, []byte{0x01}, byteorder.ABCD); err == nil {
		t.Fatal("expected error for short response")
	}
}

func TestDecodeValueByteOrder(t *testing.T) {
	// 原值 0x01020304 跨两寄存器(大端 wire):[0x01 0x02][0x03 0x04]
	raw := []byte{0x01, 0x02, 0x03, 0x04}
	tests := []struct {
		order byteorder.Order
		want  uint32
	}{
		{byteorder.ABCD, 0x01020304},
		{byteorder.CDAB, 0x03040102}, // 字交换:低字在前
		{byteorder.BADC, 0x02010403}, // 字节交换
		{byteorder.DCBA, 0x04030201}, // 字交换 + 字节交换
	}
	for _, tc := range tests {
		got, err := decodeValue(model.DataTypeUInt32, raw, tc.order)
		if err != nil || got != tc.want {
			t.Fatalf("decodeValue(uint32,%s)=%v err=%v want %#x", tc.order, got, err, tc.want)
		}
	}
	// 同一原值按不同字节序解释为不同 float
	if v, err := decodeValue(model.DataTypeFloat, []byte{0x43, 0x7A, 0x00, 0x00}, byteorder.CDAB); err != nil {
		t.Fatalf("float CDAB decode: %v err=%v", v, err)
	}
	// 16 位不受字节序影响(单寄存器恒为大端)
	if v, err := decodeValue(model.DataTypeInt16, []byte{0x01, 0x02}, byteorder.DCBA); err != nil || v != int16(258) {
		t.Fatalf("int16 should ignore byte order: %v err=%v", v, err)
	}
}

func TestApplyScale(t *testing.T) {
	if v := applyScale(int16(5), 2.0, model.DataTypeInt16); v != 10.0 {
		t.Fatalf("scale int16: %v want 10.0", v)
	}
	if v := applyScale(uint16(3), 2.0, model.DataTypeUInt16); v != 6.0 {
		t.Fatalf("scale uint16: %v want 6.0", v)
	}
	if v := applyScale(true, 0.1, model.DataTypeBool); v != true {
		t.Fatalf("scale bool should be unchanged: %v", v)
	}
	if v := applyScale(int16(5), 0, model.DataTypeInt16); v != int16(5) {
		t.Fatalf("scale 0 should be unchanged: %v", v)
	}
}

func TestParseConnConfig(t *testing.T) {
	cfg, err := parseConnConfig(json.RawMessage(`{"mode":"tcp","address":"192.168.1.5:502"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cfg.Mode != "tcp" || cfg.Address != "192.168.1.5:502" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if cfg.BaudRate != defaultBaudRate {
		t.Fatalf("default baud rate: %d want %d", cfg.BaudRate, defaultBaudRate)
	}
	rtuOverTCP, err := parseConnConfig(json.RawMessage(`{"mode":"rtu-over-tcp","address":"192.168.1.5:502"}`))
	if err != nil {
		t.Fatalf("rtu-over-tcp err: %v", err)
	}
	if rtuOverTCP.Mode != "rtu-over-tcp" || rtuOverTCP.Address != "192.168.1.5:502" {
		t.Fatalf("unexpected rtu-over-tcp cfg: %+v", rtuOverTCP)
	}
	if _, err := parseConnConfig(json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for missing mode")
	}
}

func TestParseDeviceParams(t *testing.T) {
	params, err := parseDeviceParams(json.RawMessage(`{"slaveId":3}`))
	if err != nil || params.SlaveID != 3 {
		t.Fatalf("parse device params: %+v err=%v", params, err)
	}
	// 未配置字节序默认 ABCD
	if params.ByteOrder != string(byteorder.ABCD) {
		t.Fatalf("default byte order: %q want ABCD", params.ByteOrder)
	}
	// 显式配置小写/空白字节序被规范化
	params, err = parseDeviceParams(json.RawMessage(`{"slaveId":3,"byteOrder":" cdab "}`))
	if err != nil || params.ByteOrder != string(byteorder.CDAB) {
		t.Fatalf("normalized byte order: %+v err=%v", params, err)
	}
	// 非法字节序报错
	if _, err := parseDeviceParams(json.RawMessage(`{"byteOrder":"XYZ"}`)); err == nil {
		t.Fatal("invalid byte order should error")
	}
	empty, err := parseDeviceParams(nil)
	if err != nil || empty.SlaveID != 0 || empty.ByteOrder != string(byteorder.ABCD) {
		t.Fatalf("empty params: %+v err=%v", empty, err)
	}
}

func TestMaxQuantity(t *testing.T) {
	if maxQuantity("holding") != maxRegistersPerRead {
		t.Fatalf("holding max = %d want %d", maxQuantity("holding"), maxRegistersPerRead)
	}
	if maxQuantity("coil") != maxCoilsPerRead {
		t.Fatalf("coil max = %d want %d", maxQuantity("coil"), maxCoilsPerRead)
	}
}

func TestPlanBlocks(t *testing.T) {
	items := func(regs ...uint16) []pointItem {
		out := make([]pointItem, len(regs))
		for i, r := range regs {
			out[i] = pointItem{register: r, quantity: 1}
		}
		return out
	}
	// 连续地址合并成一块
	blocks := planBlocks(items(0, 1, 2), maxRegistersPerRead)
	if len(blocks) != 1 || blocks[0].startRegister != 0 || blocks[0].quantity != 3 {
		t.Fatalf("continuous merge: %+v", blocks)
	}
	// 间隙允许合并(跨度不超上限)
	blocks = planBlocks(items(0, 5), maxRegistersPerRead)
	if len(blocks) != 1 || blocks[0].quantity != 6 {
		t.Fatalf("gap merge: %+v", blocks)
	}
	// 跨度超上限拆分(0 与 125:跨度 126 > 125)
	blocks = planBlocks(items(0, 125), maxRegistersPerRead)
	if len(blocks) != 2 {
		t.Fatalf("split over max: %+v", blocks)
	}
	// 乱序输入应排序后合并
	blocks = planBlocks(items(2, 0, 1), maxRegistersPerRead)
	if len(blocks) != 1 || len(blocks[0].items) != 3 {
		t.Fatalf("unsorted input: %+v", blocks)
	}
}

func TestDecodePointHolding(t *testing.T) {
	// 块从寄存器 0 读回 4 字节:0x0001 0x0002,寄存器 1 = 2
	raw := []byte{0x00, 0x01, 0x00, 0x02}
	item := pointItem{register: 1, quantity: 1, function: "holding", byteOrder: byteorder.ABCD, point: model.Point{DataType: model.DataTypeInt16}}
	v, err := decodePoint(item, raw, 0)
	if err != nil || v != int16(2) {
		t.Fatalf("holding offset decode: %v err=%v", v, err)
	}
}

func TestDecodePointByteOrder(t *testing.T) {
	// 块从寄存器 0 读回 6 字节 = 寄存器 [0x0000][0x0001][0x0002],从寄存器 1 起读 uint32。
	// 该点取到的 wire 字节为 [0x00 0x01][0x00 0x02]。
	raw := []byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x02}
	// ABCD 大端:0x00010002
	v, err := decodePoint(pointItem{register: 1, quantity: 2, function: "holding", byteOrder: byteorder.ABCD, point: model.Point{DataType: model.DataTypeUInt32}}, raw, 0)
	if err != nil || v != uint32(0x00010002) {
		t.Fatalf("ABCD decode: %v err=%v", v, err)
	}
	// CDAB 字交换:0x00020001
	v, err = decodePoint(pointItem{register: 1, quantity: 2, function: "holding", byteOrder: byteorder.CDAB, point: model.Point{DataType: model.DataTypeUInt32}}, raw, 0)
	if err != nil || v != uint32(0x00020001) {
		t.Fatalf("CDAB decode: %v err=%v", v, err)
	}
}

func TestDecodePointCoil(t *testing.T) {
	// 块从线圈 0 读回 1 字节:0b00000100,线圈 2 为 1
	raw := []byte{0x04}
	item := pointItem{register: 2, quantity: 1, function: "coil", point: model.Point{DataType: model.DataTypeBool}}
	v, err := decodePoint(item, raw, 0)
	if err != nil || v != true {
		t.Fatalf("coil bit decode: %v err=%v", v, err)
	}
}

func TestConnectionPoolAcquireRelease(t *testing.T) {
	d := &modbusDriver{pool: make(map[string]*sharedConn)}
	shared := &sharedConn{connectionID: "c1", refCount: 1}
	d.pool["c1"] = shared

	// 同 ConnectionID acquire 复用,refCount 递增,不建新连接
	got, err := d.acquire(context.Background(), "c1", connConfig{})
	if err != nil || got != shared || shared.refCount != 2 {
		t.Fatalf("acquire reuse: got=%v refCount=%d err=%v", got, shared.refCount, err)
	}

	// release 减计数,未归零则池保留连接
	if err := d.release(shared); err != nil {
		t.Fatalf("release: %v", err)
	}
	if shared.refCount != 1 {
		t.Fatalf("refCount after release: %d want 1", shared.refCount)
	}
	if _, ok := d.pool["c1"]; !ok {
		t.Fatal("pool should keep connection while refCount > 0")
	}
}

func TestPointInBlock(t *testing.T) {
	blk := pollBlock{Function: "holding", Start: 0, Count: 12}
	if !pointInBlock(pointItem{register: 0, quantity: 1}, blk) {
		t.Fatal("register 0 should be in block 0..12")
	}
	if !pointInBlock(pointItem{register: 11, quantity: 1}, blk) {
		t.Fatal("register 11 should be in block 0..12")
	}
	if pointInBlock(pointItem{register: 12, quantity: 1}, blk) {
		t.Fatal("register 12 should be outside block 0..12")
	}
	// int32 占 2 寄存器,11+2 越界
	if pointInBlock(pointItem{register: 11, quantity: 2}, blk) {
		t.Fatal("int32 at register 11 should overflow block 0..12")
	}
}

func TestIndexPollBlocks(t *testing.T) {
	indexed := indexPollBlocks([]pollBlock{
		{Function: "holding", Start: 0, Count: 12},
		{Function: "holding", Start: 100, Count: 8},
		{Function: "coil", Start: 0, Count: 16},
	})
	if len(indexed["holding"]) != 2 || len(indexed["coil"]) != 1 {
		t.Fatalf("unexpected index: %+v", indexed)
	}
	if indexPollBlocks(nil) != nil {
		t.Fatal("nil blocks should return nil")
	}
}

func TestEncodeWriteValue(t *testing.T) {
	// int16: 258 -> 0x0102
	b, ok := encodeWriteValue(float64(258), model.DataTypeInt16, 0, byteorder.ABCD)
	if !ok || !bytes.Equal(b, []byte{0x01, 0x02}) {
		t.Fatalf("int16 encode: %x ok=%v", b, ok)
	}
	// uint16
	b, ok = encodeWriteValue(float64(258), model.DataTypeUInt16, 0, byteorder.ABCD)
	if !ok || !bytes.Equal(b, []byte{0x01, 0x02}) {
		t.Fatalf("uint16 encode: %x ok=%v", b, ok)
	}
	// int32: 258 -> 0x00000102
	b, ok = encodeWriteValue(float64(258), model.DataTypeInt32, 0, byteorder.ABCD)
	if !ok || !bytes.Equal(b, []byte{0x00, 0x00, 0x01, 0x02}) {
		t.Fatalf("int32 encode: %x ok=%v", b, ok)
	}
	// float32: 250.0 -> 0x437A0000
	b, ok = encodeWriteValue(float64(250), model.DataTypeFloat, 0, byteorder.ABCD)
	if !ok || !bytes.Equal(b, []byte{0x43, 0x7A, 0x00, 0x00}) {
		t.Fatalf("float32 encode: %x ok=%v", b, ok)
	}
	// scale 反向:工程值 25.0,scale 0.1 -> 原始 250 -> float32 0x437A0000
	b, ok = encodeWriteValue(float64(25), model.DataTypeFloat, 0.1, byteorder.ABCD)
	if !ok || !bytes.Equal(b, []byte{0x43, 0x7A, 0x00, 0x00}) {
		t.Fatalf("scale reverse encode: %x ok=%v", b, ok)
	}
	// 不支持的类型
	if _, ok := encodeWriteValue(float64(1), model.DataTypeDouble, 0, byteorder.ABCD); ok {
		t.Fatal("double should not be encodable")
	}
	if _, ok := encodeWriteValue("str", model.DataTypeInt16, 0, byteorder.ABCD); ok {
		t.Fatal("string value for int16 should fail")
	}
}

func TestEncodeWriteValueByteOrder(t *testing.T) {
	// 原值 0x01020304 编码为不同 wire 字节序(与解码互为逆运算)
	tests := []struct {
		order byteorder.Order
		want  []byte
	}{
		{byteorder.ABCD, []byte{0x01, 0x02, 0x03, 0x04}},
		{byteorder.CDAB, []byte{0x03, 0x04, 0x01, 0x02}},
		{byteorder.BADC, []byte{0x02, 0x01, 0x04, 0x03}},
		{byteorder.DCBA, []byte{0x04, 0x03, 0x02, 0x01}},
	}
	for _, tc := range tests {
		b, ok := encodeWriteValue(float64(0x01020304), model.DataTypeUInt32, 0, tc.order)
		if !ok || !bytes.Equal(b, tc.want) {
			t.Fatalf("encode uint32 %s: %x ok=%v want %x", tc.order, b, ok, tc.want)
		}
		// 逆运算:按同字节序解回原值
		v, err := decodeValue(model.DataTypeUInt32, b, tc.order)
		if err != nil || v != uint32(0x01020304) {
			t.Fatalf("roundtrip uint32 %s: %v err=%v", tc.order, v, err)
		}
	}
	// 16 位编码不受字节序影响
	b, ok := encodeWriteValue(float64(258), model.DataTypeInt16, 0, byteorder.DCBA)
	if !ok || !bytes.Equal(b, []byte{0x01, 0x02}) {
		t.Fatalf("int16 should ignore byte order: %x ok=%v", b, ok)
	}
}

func TestEndpointKey(t *testing.T) {
	drv := &modbusDriver{}
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"tcp address", `{"mode":"tcp","address":"192.168.1.5:502"}`, "tcp|192.168.1.5:502"},
		{"tcp address normalized", `{"mode":"tcp","address":"  Host.example.COM:502 "}`, "tcp|host.example.com:502"},
		{"rtu serial port", `{"mode":"rtu","serialPort":"/dev/ttyS0"}`, "serial|/dev/ttys0"},
		{"rtu-over-tcp same endpoint as tcp", `{"mode":"rtu-over-tcp","address":"192.168.1.5:502"}`, "tcp|192.168.1.5:502"},
		{"invalid config", `{"mode":"tcp"`, ""},
		{"unknown mode", `{"mode":"udp","address":"x"}`, ""},
	}
	for _, tc := range tests {
		if got := drv.EndpointKey(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("%s: EndpointKey=%q want %q", tc.name, got, tc.want)
		}
	}
}
