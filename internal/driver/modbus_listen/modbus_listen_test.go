package modbus_listen

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"testing"
	"time"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
)

// buildFrame 构造一个 Modbus TCP 上报帧(从机/主站主动推给网关):MBAP + 功能码 03
// + 字节数 + 大端寄存器数据。
func buildFrame(unit byte, regs []uint16) []byte {
	frame := make([]byte, 9+len(regs)*2)
	frame[4] = 0
	frame[5] = byte(1 + 1 + 1 + len(regs)*2) // unit + fn + byteCount + data
	frame[6] = unit
	frame[7] = 3 // read holding registers
	frame[8] = byte(len(regs) * 2)
	for i, r := range regs {
		binary.BigEndian.PutUint16(frame[9+i*2:], r)
	}
	return frame
}

func TestParseConnConfig(t *testing.T) {
	cfg, err := parseConnConfig(json.RawMessage(`{"listen":":502"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cfg.Listen != ":502" {
		t.Fatalf("listen=%q", cfg.Listen)
	}
	if cfg.Timeout != defaultTimeout.String() {
		t.Fatalf("timeout=%q want %q", cfg.Timeout, defaultTimeout.String())
	}
	if _, err := parseConnConfig(json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing listen should error")
	}
	if _, err := parseConnConfig(json.RawMessage(`{"listen":":502","timeout":"bad"}`)); err == nil {
		t.Fatal("invalid timeout should error")
	}
}

func TestParseDeviceParams(t *testing.T) {
	params, err := parseDeviceParams(json.RawMessage(`{"slaveId":1}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if params.SlaveID != 1 {
		t.Fatalf("slaveId=%d", params.SlaveID)
	}
}

func TestDecodePoint(t *testing.T) {
	regs := []uint16{0x1234, 0xABCD, 0x3F80, 0x0000}
	// int16 大端
	if v, ok := decodePoint(model.Point{Address: "0", DataType: model.DataTypeInt16}, regs); !ok || v != int16(0x1234) {
		t.Fatalf("int16: %v ok=%v", v, ok)
	}
	// uint16
	if v, ok := decodePoint(model.Point{Address: "1", DataType: model.DataTypeUInt16}, regs); !ok || v != uint16(0xABCD) {
		t.Fatalf("uint16: %v ok=%v", v, ok)
	}
	// int32 占 2 寄存器:0x3F80 << 16 | 0x0000 = 0x3F800000 = 1.0 float,但这里按 int32
	if v, ok := decodePoint(model.Point{Address: "2", DataType: model.DataTypeInt32}, regs); !ok || v != int32(0x3F800000) {
		t.Fatalf("int32: %v ok=%v", v, ok)
	}
	// float32 占 2 寄存器:0x3F800000 = 1.0
	if v, ok := decodePoint(model.Point{Address: "2", DataType: model.DataTypeFloat}, regs); !ok || v != float32(1.0) {
		t.Fatalf("float32: %v ok=%v", v, ok)
	}
	// 越界
	if _, ok := decodePoint(model.Point{Address: "3", DataType: model.DataTypeInt32}, regs); ok {
		t.Fatal("out of range should fail")
	}
	// 非法地址
	if _, ok := decodePoint(model.Point{Address: "abc", DataType: model.DataTypeInt16}, regs); ok {
		t.Fatal("bad address should fail")
	}
}

func TestApplyScale(t *testing.T) {
	if v := applyScale(int16(10), 0.1, model.DataTypeInt16); v != float64(1.0) {
		t.Fatalf("scale: %v", v)
	}
	if v := applyScale(true, 0.1, model.DataTypeBool); v != true {
		t.Fatalf("bool should not scale: %v", v)
	}
}

func TestReadFrame(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	want := buildFrame(1, []uint16{0x1234, 0x5678})
	go func() {
		client.Write(want)
		client.Close()
	}()
	frame, err := readFrame(server)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if len(frame) != len(want) {
		t.Fatalf("len=%d want %d", len(frame), len(want))
	}
	regs := registers(frame[9 : 9+int(frame[8])])
	if len(regs) != 2 || regs[0] != 0x1234 || regs[1] != 0x5678 {
		t.Fatalf("regs=%v", regs)
	}
}

// TestListenerEndToEnd 走完整链路:Open -> Listen 注册 -> 设备连入推帧 -> 按 UnitID
// 路由解码 -> onData 收到 DataPoint。
func TestListenerEndToEnd(t *testing.T) {
	// 预留一个空闲端口再复用
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	addr := reserve.Addr().String()
	reserve.Close()

	d := &modbusListenDriver{pool: make(map[string]*sharedListener)}
	conn, err := d.Open(context.Background(), driver.OpenRequest{
		DeviceID:     "d1",
		ConnectionID: "c1",
		ConnConfig:   json.RawMessage(`{"listen":"` + addr + `"}`),
		DeviceParams: json.RawMessage(`{"slaveId":1}`),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	lc := conn.(*listenConn)
	if _, isListener := conn.(driver.Listener); !isListener {
		t.Fatal("listenConn should implement driver.Listener")
	}

	got := make(chan model.DataPoint, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := lc.Listen(ctx, []model.Point{
		{Name: "temp", Address: "0", DataType: model.DataTypeInt16, Scale: 0.1},
		{Name: "level", Address: "1", DataType: model.DataTypeFloat},
	}, func(dp model.DataPoint) { got <- dp }); err != nil {
		t.Fatalf("listen: %v", err)
	}

	// serve goroutine 异步 bind,轮询等待 listener 就绪后再拨号
	var client net.Conn
	for i := 0; i < 50; i++ {
		client, err = net.Dial("tcp", addr)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if client == nil {
		t.Fatalf("dial after retries: %v", err)
	}
	defer client.Close()

	// 设备连入并推送一帧:寄存器 [0x00C8=200, 0x3F80 0x0000 = 1.0f]
	client.Write(buildFrame(1, []uint16{0x00C8, 0x3F80, 0x0000}))

	select {
	case dp := <-got:
		if dp.DeviceID != "d1" || dp.Point != "temp" || dp.Quality != model.QualityGood {
			t.Fatalf("dp1: %+v", dp)
		}
		if dp.Value != float64(20.0) { // 200 * 0.1
			t.Fatalf("temp value: %v", dp.Value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for temp")
	}
	select {
	case dp := <-got:
		if dp.Point != "level" || dp.Quality != model.QualityGood {
			t.Fatalf("dp2: %+v", dp)
		}
		if dp.Value != float32(1.0) {
			t.Fatalf("level value: %v", dp.Value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for level")
	}
}

// TestDispatchRouting 验证按 UnitID 路由:未配置的从机地址被静默丢弃,命中的被投递。
func TestDispatchRouting(t *testing.T) {
	s := &sharedListener{devices: map[byte]*listenDevice{}}
	var count int
	s.devices[1] = &listenDevice{
		deviceID: "d1", slaveID: 1,
		points: []model.Point{{Name: "p", Address: "0", DataType: model.DataTypeInt16}},
		onData: func(model.DataPoint) { count++ },
	}

	s.dispatch(buildFrame(2, []uint16{42})) // 未知从机 2,应丢弃
	if count != 0 {
		t.Fatalf("unknown unit should not deliver, count=%d", count)
	}
	s.dispatch(buildFrame(1, []uint16{42})) // 命中从机 1,应投递
	if count != 1 {
		t.Fatalf("known unit should deliver once, count=%d", count)
	}
}
