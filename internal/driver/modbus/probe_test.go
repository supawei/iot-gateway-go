package modbus

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/grid-x/modbus"

	"iot-gateway-go/internal/model"
)

// ---------- 桩:modbus.Client / ClientHandler ----------

// probeClient 实现 modbus.Client,记录读调用并可按配置返回成功/传输错误/异常响应。
type probeClient struct {
	mu     sync.Mutex
	err    error // 传输层错误(非 nil 时读调用返回它)
	exc    byte  // 非 0 时读调用返回带 ExceptionCode 的异常响应错误
	calls  []string
	blocks map[string][]byte // 方法名 -> 成功返回的字节
}

func (c *probeClient) record(name string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name)
	if c.exc != 0 {
		return nil, &modbus.Error{FunctionCode: 0x83, ExceptionCode: c.exc}
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.blocks[name], nil
}

func (c *probeClient) lastCalls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.calls))
	copy(out, c.calls)
	return out
}

// modbus.Client 接口全部方法(未用到的直接转发到 record 以满足接口)。
func (c *probeClient) ReadCoils(ctx context.Context, address, quantity uint16) ([]byte, error) {
	return c.record("ReadCoils")
}
func (c *probeClient) ReadDiscreteInputs(ctx context.Context, address, quantity uint16) ([]byte, error) {
	return c.record("ReadDiscreteInputs")
}
func (c *probeClient) WriteSingleCoil(ctx context.Context, address, value uint16) ([]byte, error) {
	return c.record("WriteSingleCoil")
}
func (c *probeClient) WriteMultipleCoils(ctx context.Context, address, quantity uint16, value []byte) ([]byte, error) {
	return c.record("WriteMultipleCoils")
}
func (c *probeClient) ReadInputRegisters(ctx context.Context, address, quantity uint16) ([]byte, error) {
	return c.record("ReadInputRegisters")
}
func (c *probeClient) ReadHoldingRegisters(ctx context.Context, address, quantity uint16) ([]byte, error) {
	return c.record("ReadHoldingRegisters")
}
func (c *probeClient) WriteSingleRegister(ctx context.Context, address, value uint16) ([]byte, error) {
	return c.record("WriteSingleRegister")
}
func (c *probeClient) WriteMultipleRegisters(ctx context.Context, address, quantity uint16, value []byte) ([]byte, error) {
	return c.record("WriteMultipleRegisters")
}
func (c *probeClient) ReadWriteMultipleRegisters(ctx context.Context, readAddress, readQuantity, writeAddress, writeQuantity uint16, value []byte) ([]byte, error) {
	return c.record("ReadWriteMultipleRegisters")
}
func (c *probeClient) MaskWriteRegister(ctx context.Context, address, andMask, orMask uint16) ([]byte, error) {
	return c.record("MaskWriteRegister")
}
func (c *probeClient) ReadFIFOQueue(ctx context.Context, address uint16) ([]byte, error) {
	return c.record("ReadFIFOQueue")
}
func (c *probeClient) ReadDeviceIdentification(ctx context.Context, code modbus.ReadDeviceIDCode) (map[byte][]byte, error) {
	return nil, c.err
}
func (c *probeClient) ReadDeviceIdentificationSpecificObject(ctx context.Context, objectID byte) (map[byte][]byte, error) {
	return nil, c.err
}

// probeHandler 实现 modbus.ClientHandler,SetSlave/Close 无副作用。
type probeHandler struct{}

func (probeHandler) SetSlave(slaveID byte)                             {}
func (probeHandler) Encode(pdu *modbus.ProtocolDataUnit) ([]byte, error) { return nil, nil }
func (probeHandler) Decode(adu []byte) (*modbus.ProtocolDataUnit, error) { return nil, nil }
func (probeHandler) Verify(aduRequest, aduResponse []byte) error         { return nil }
func (probeHandler) Send(ctx context.Context, aduRequest []byte) ([]byte, error) {
	return nil, nil
}
func (probeHandler) Connect(ctx context.Context) error { return nil }
func (probeHandler) Close() error                      { return nil }

// newProbeConn 构造带桩 client/handler 的 modbusConn(不经过真实连接池)。
func newProbeConn(client *probeClient, blocks []pollBlock) *modbusConn {
	return &modbusConn{
		deviceID:   "dev1",
		slaveID:    1,
		shared:     &sharedConn{handler: probeHandler{}, client: client},
		driver:     &modbusDriver{pool: make(map[string]*sharedConn)},
		pollBlocks: indexPollBlocks(blocks),
	}
}

// ---------- 用例 ----------

func TestProbeUsesPollBlocks(t *testing.T) {
	client := &probeClient{blocks: map[string][]byte{"ReadHoldingRegisters": {0, 1}}}
	c := newProbeConn(client, []pollBlock{{Function: "holding", Start: 0, Count: 2}})
	if err := c.Probe(context.Background(), []model.Point{{Name: "p", Address: "holding:5", DataType: model.DataTypeInt16}}); err != nil {
		t.Fatalf("Probe with pollBlock should succeed, got %v", err)
	}
	calls := client.lastCalls()
	if len(calls) != 1 || calls[0] != "ReadHoldingRegisters" {
		t.Fatalf("expected single ReadHoldingRegisters, got %v", calls)
	}
}

func TestProbeUsesFirstPointWhenNoPollBlocks(t *testing.T) {
	client := &probeClient{blocks: map[string][]byte{"ReadHoldingRegisters": {0, 1}}}
	c := newProbeConn(client, nil)
	// 首个点位地址非法会被跳过,落到第二个有效点位
	points := []model.Point{
		{Name: "bad", Address: "nonsense", DataType: model.DataTypeInt16},
		{Name: "p", Address: "holding:0", DataType: model.DataTypeInt16},
	}
	if err := c.Probe(context.Background(), points); err != nil {
		t.Fatalf("Probe should succeed, got %v", err)
	}
	if calls := client.lastCalls(); len(calls) != 1 || calls[0] != "ReadHoldingRegisters" {
		t.Fatalf("expected ReadHoldingRegisters via point, got %v", calls)
	}
}

func TestProbeFallsBackToHoldingZero(t *testing.T) {
	client := &probeClient{blocks: map[string][]byte{"ReadHoldingRegisters": {0, 1}}}
	c := newProbeConn(client, nil)
	if err := c.Probe(context.Background(), nil); err != nil {
		t.Fatalf("Probe fallback should succeed, got %v", err)
	}
	if calls := client.lastCalls(); len(calls) != 1 || calls[0] != "ReadHoldingRegisters" {
		t.Fatalf("expected fallback ReadHoldingRegisters, got %v", calls)
	}
}

func TestProbeTransportErrorUnreachable(t *testing.T) {
	client := &probeClient{err: errors.New("read timeout")}
	c := newProbeConn(client, nil)
	if err := c.Probe(context.Background(), []model.Point{{Name: "p", Address: "holding:0", DataType: model.DataTypeInt16}}); err == nil {
		t.Fatal("transport error should make Probe return error")
	}
}

func TestProbeModbusExceptionMeansReachable(t *testing.T) {
	// 设备返回 modbus 异常码(如 illegal data address):证明设备在总线上,视为可达。
	client := &probeClient{exc: 0x02}
	c := newProbeConn(client, nil)
	if err := c.Probe(context.Background(), []model.Point{{Name: "p", Address: "holding:0", DataType: model.DataTypeInt16}}); err != nil {
		t.Fatalf("modbus exception response should be treated as reachable, got %v", err)
	}
}
