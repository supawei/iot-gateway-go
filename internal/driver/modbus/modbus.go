package modbus

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grid-x/modbus"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
)

const (
	defaultTimeout  = 1 * time.Second
	defaultBaudRate = 9600
	defaultDataBits = 8
	defaultParity   = "N"
	defaultStopBits = 1

	// Modbus 协议单次读取上限(规范):寄存器 125,线圈/离散输入 2000
	maxRegistersPerRead = 125
	maxCoilsPerRead     = 2000

	// TCP 类连接的链路/协议恢复窗口:单次请求内连接断开或协议错乱时,
	// 在此窗口内重连重试,避免等到下个采集周期才恢复。
	modbusLinkRecoveryTimeout     = 10 * time.Second
	modbusProtocolRecoveryTimeout = 10 * time.Second
)

var errShortResponse = errors.New("modbus response too short")

func init() {
	driver.Register("modbus", &modbusDriver{pool: make(map[string]*sharedConn)})
}

// modbusDriver 维护按 ConnectionID 复用的连接池:同连接(同串口/DTU)的多个从机
// 设备共享底层 handler,经 sharedConn.mu 串行化请求以契合 RTU/DTU 半双工总线。
type modbusDriver struct {
	mu   sync.Mutex
	pool map[string]*sharedConn
}

func (d *modbusDriver) Open(ctx context.Context, req driver.OpenRequest) (driver.Conn, error) {
	cfg, err := parseConnConfig(req.ConnConfig)
	if err != nil {
		return nil, err
	}
	params, err := parseDeviceParams(req.DeviceParams)
	if err != nil {
		return nil, err
	}
	shared, err := d.acquire(ctx, req.ConnectionID, cfg)
	if err != nil {
		return nil, err
	}
	return &modbusConn{
		deviceID:   req.DeviceID,
		slaveID:    params.SlaveID,
		shared:     shared,
		driver:     d,
		pollBlocks: indexPollBlocks(params.PollBlocks),
	}, nil
}

// acquire 取或建共享连接:同 ConnectionID 复用并增引用计数,否则建新连接入池。
func (d *modbusDriver) acquire(ctx context.Context, connectionID string, cfg connConfig) (*sharedConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if shared, ok := d.pool[connectionID]; ok {
		shared.refCount++
		return shared, nil
	}
	client, handler, err := buildClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	shared := &sharedConn{
		connectionID: connectionID,
		handler:      handler,
		client:       client,
		refCount:     1,
	}
	d.pool[connectionID] = shared
	return shared, nil
}

// release 减引用计数,归零则关底层连接并移出池。
func (d *modbusDriver) release(shared *sharedConn) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	shared.refCount--
	if shared.refCount > 0 {
		return nil
	}
	delete(d.pool, shared.connectionID)
	return shared.handler.Close()
}

// sharedConn 是被多设备共享的底层连接:mu 串行化请求(半双工),refCount 管生命周期。
type sharedConn struct {
	connectionID string
	handler      modbus.ClientHandler
	client       modbus.Client
	mu           sync.Mutex
	refCount     int
}

type modbusConn struct {
	deviceID   string
	slaveID    byte
	shared     *sharedConn
	driver     *modbusDriver
	pollBlocks map[string][]pollBlock
	warnOnce   sync.Once
}

// Read 批量读取点位:同 function 的连续/相邻地址合并成少数 Modbus 请求(连读),
// 减少多点设备的请求往返。返回 DataPoint 与输入 points 顺序一一对应。
// 单点通信失败/解码异常用 Quality 表达,不阻断同批其他点;error 仅用于配置级错误。
func (c *modbusConn) Read(ctx context.Context, points []model.Point) ([]model.DataPoint, error) {
	timestamp := time.Now()
	results := make([]model.DataPoint, len(points))
	for index, point := range points {
		results[index] = model.DataPoint{
			DeviceID:  c.deviceID,
			Point:     point.Name,
			Timestamp: timestamp,
			Quality:   model.QualityBad,
		}
	}
	itemsByFunction := map[string][]pointItem{}
	for index, point := range points {
		function, register, err := parseAddress(point.Address)
		if err != nil {
			continue // 解析失败:该点保持 bad,跳过不阻断
		}
		itemsByFunction[function] = append(itemsByFunction[function], pointItem{
			index:    index,
			point:    point,
			function: function,
			register: register,
			quantity: quantityOf(point.DataType),
		})
	}
	for function, items := range itemsByFunction {
		if blocks, ok := c.pollBlocks[function]; ok {
			c.readPollBlocks(ctx, function, blocks, items, results, timestamp)
		} else {
			c.readGroup(ctx, function, items, results, timestamp)
		}
	}
	return results, nil
}

func (c *modbusConn) Close() error {
	return c.driver.release(c.shared)
}

// pointItem 关联点位与其解析后的地址信息,供连读分组与结果回填。
type pointItem struct {
	index    int
	point    model.Point
	function string
	register uint16
	quantity uint16
}

// readGroup 按 planBlocks 划分的合并块逐块请求,读回后按偏移解码回填 results。
func (c *modbusConn) readGroup(ctx context.Context, function string, items []pointItem, results []model.DataPoint, timestamp time.Time) {
	for _, blk := range planBlocks(items, maxQuantity(function)) {
		raw, err := c.readRaw(ctx, function, blk.startRegister, blk.quantity)
		if err != nil {
			continue // 块失败:块内点保持默认 bad
		}
		for _, item := range blk.items {
			c.fillResult(item, raw, blk.startRegister, results, timestamp)
		}
	}
}

// readPollBlocks 按显式声明的固定块读取(绕过自动连读),适配必须整块读的设备。
// 落在块内的点按偏移解码;不在任何块内的点保持 bad 并首次告警。
func (c *modbusConn) readPollBlocks(ctx context.Context, function string, blocks []pollBlock, items []pointItem, results []model.DataPoint, timestamp time.Time) {
	for _, blk := range blocks {
		raw, err := c.readRaw(ctx, function, blk.Start, blk.Count)
		if err != nil {
			continue
		}
		for _, item := range items {
			if pointInBlock(item, blk) {
				c.fillResult(item, raw, blk.Start, results, timestamp)
			}
		}
	}
	c.warnUncovered(items, blocks)
}

func (c *modbusConn) warnUncovered(items []pointItem, blocks []pollBlock) {
	var uncovered []string
	for _, item := range items {
		covered := false
		for _, blk := range blocks {
			if pointInBlock(item, blk) {
				covered = true
				break
			}
		}
		if !covered {
			uncovered = append(uncovered, item.point.Name)
		}
	}
	if len(uncovered) > 0 {
		c.warnOnce.Do(func() {
			slog.Warn("points not covered by pollBlock, marked bad", "device", c.deviceID, "points", uncovered)
		})
	}
}

func pointInBlock(item pointItem, blk pollBlock) bool {
	return item.register >= blk.Start && item.register+item.quantity <= blk.Start+blk.Count
}

// indexPollBlocks 将 pollBlocks 按 function 索引,便于 Read 时按 function 查块。
func indexPollBlocks(blocks []pollBlock) map[string][]pollBlock {
	if len(blocks) == 0 {
		return nil
	}
	indexed := make(map[string][]pollBlock)
	for _, blk := range blocks {
		indexed[blk.Function] = append(indexed[blk.Function], blk)
	}
	return indexed
}

// block 是一次 Modbus 请求覆盖的点位合并块。
type block struct {
	startRegister uint16
	quantity      uint16
	items         []pointItem
}

// planBlocks 将同 function 的点位按地址排序后贪心合并:连续或相邻(允许间隙)
// 的地址并入同一块,直到总跨度超过单次读取上限。返回的块数即请求数。
func planBlocks(items []pointItem, maxQty int) []block {
	sort.Slice(items, func(i, j int) bool { return items[i].register < items[j].register })
	var blocks []block
	for start := 0; start < len(items); {
		blockStart := items[start].register
		blockEnd := items[start].register + items[start].quantity
		end := start + 1
		for end < len(items) {
			next := items[end]
			if int(next.register-blockStart)+int(next.quantity) > maxQty {
				break
			}
			if next.register+next.quantity > blockEnd {
				blockEnd = next.register + next.quantity
			}
			end++
		}
		blocks = append(blocks, block{
			startRegister: blockStart,
			quantity:      blockEnd - blockStart,
			items:         items[start:end],
		})
		start = end
	}
	return blocks
}

func (c *modbusConn) fillResult(item pointItem, raw []byte, blockStart uint16, results []model.DataPoint, timestamp time.Time) {
	value, err := decodePoint(item, raw, blockStart)
	if err != nil {
		results[item.index] = model.DataPoint{
			DeviceID: c.deviceID, Point: item.point.Name, Timestamp: timestamp, Quality: model.QualityUncertain,
		}
		return
	}
	results[item.index] = model.DataPoint{
		DeviceID:  c.deviceID,
		Point:     item.point.Name,
		Value:     applyScale(value, item.point.Scale, item.point.DataType),
		Timestamp: timestamp,
		Quality:   model.QualityGood,
	}
}

// readRaw 在共享连接上串行执行单次请求:SetSlave 切到本设备从机地址后读取,
// mu 保证同连接请求不并发(RTU/DTU 半双工总线不允许帧交错)。
func (c *modbusConn) readRaw(ctx context.Context, function string, register, quantity uint16) ([]byte, error) {
	c.shared.mu.Lock()
	defer c.shared.mu.Unlock()
	c.shared.handler.SetSlave(c.slaveID)
	switch function {
	case "holding":
		return c.shared.client.ReadHoldingRegisters(ctx, register, quantity)
	case "input":
		return c.shared.client.ReadInputRegisters(ctx, register, quantity)
	case "coil":
		return c.shared.client.ReadCoils(ctx, register, quantity)
	case "discrete":
		return c.shared.client.ReadDiscreteInputs(ctx, register, quantity)
	default:
		return nil, fmt.Errorf("unsupported modbus function %q", function)
	}
}

// decodePoint 从连读块 raw 中按点位偏移解码单个值。
// holding/input 按寄存器(2 字节)切片;coil/discrete 按位取。
func decodePoint(item pointItem, raw []byte, blockStart uint16) (interface{}, error) {
	if item.function == "coil" || item.function == "discrete" {
		bitIndex := int(item.register - blockStart)
		byteIndex := bitIndex / 8
		if byteIndex >= len(raw) {
			return nil, errShortResponse
		}
		return raw[byteIndex]>>(bitIndex%8)&1 != 0, nil
	}
	offset := int(item.register-blockStart) * 2
	endByte := offset + int(item.quantity)*2
	if endByte > len(raw) {
		return nil, errShortResponse
	}
	return decodeValue(item.point.DataType, raw[offset:endByte])
}

func maxQuantity(function string) int {
	switch function {
	case "coil", "discrete":
		return maxCoilsPerRead
	default:
		return maxRegistersPerRead
	}
}

// parseAddress 解析 "function:register" 形式地址,如 "holding:0"、"coil:2"。
func parseAddress(addr string) (function string, register uint16, err error) {
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("invalid modbus address %q, want \"function:register\"", addr)
	}
	reg, err := strconv.Atoi(parts[1])
	if err != nil || reg < 0 || reg > 65535 {
		return "", 0, fmt.Errorf("invalid register %q in address %q", parts[1], addr)
	}
	return parts[0], uint16(reg), nil
}

func quantityOf(dt model.DataType) uint16 {
	switch dt {
	case model.DataTypeInt32, model.DataTypeUInt32, model.DataTypeFloat:
		return 2
	default:
		return 1
	}
}

func decodeValue(dt model.DataType, raw []byte) (interface{}, error) {
	switch dt {
	case model.DataTypeBool:
		if len(raw) < 1 {
			return nil, errShortResponse
		}
		return raw[0]&1 != 0, nil
	case model.DataTypeInt16:
		if len(raw) < 2 {
			return nil, errShortResponse
		}
		return int16(binary.BigEndian.Uint16(raw)), nil
	case model.DataTypeUInt16:
		if len(raw) < 2 {
			return nil, errShortResponse
		}
		return binary.BigEndian.Uint16(raw), nil
	case model.DataTypeInt32:
		if len(raw) < 4 {
			return nil, errShortResponse
		}
		return int32(binary.BigEndian.Uint32(raw)), nil
	case model.DataTypeUInt32:
		if len(raw) < 4 {
			return nil, errShortResponse
		}
		return binary.BigEndian.Uint32(raw), nil
	case model.DataTypeFloat:
		if len(raw) < 4 {
			return nil, errShortResponse
		}
		return math.Float32frombits(binary.BigEndian.Uint32(raw)), nil
	default:
		return nil, fmt.Errorf("unsupported dataType %s", dt)
	}
}

// applyScale 缩放原始值;bool 不缩放,数值类型缩放后统一为 float64。
func applyScale(value interface{}, scale float64, dt model.DataType) interface{} {
	if scale == 0 || dt == model.DataTypeBool {
		return value
	}
	switch v := value.(type) {
	case int16:
		return float64(v) * scale
	case uint16:
		return float64(v) * scale
	case int32:
		return float64(v) * scale
	case uint32:
		return float64(v) * scale
	case float32:
		return float64(v) * scale
	default:
		return value
	}
}

// connConfig 是传输层参数(怎么到达总线),不含从机地址。
type connConfig struct {
	Mode       string `json:"mode"`
	Timeout    string `json:"timeout"`
	SerialPort string `json:"serialPort"`
	BaudRate   int    `json:"baudRate"`
	DataBits   int    `json:"dataBits"`
	Parity     string `json:"parity"`
	StopBits   int    `json:"stopBits"`
	Address    string `json:"address"`
}

func parseConnConfig(connection json.RawMessage) (connConfig, error) {
	cfg := connConfig{
		BaudRate: defaultBaudRate,
		DataBits: defaultDataBits,
		Parity:   defaultParity,
		StopBits: defaultStopBits,
	}
	if err := json.Unmarshal(connection, &cfg); err != nil {
		return connConfig{}, fmt.Errorf("parse modbus connection: %w", err)
	}
	if cfg.Mode == "" {
		return connConfig{}, errors.New("modbus connection mode is required")
	}
	return cfg, nil
}

// deviceParams 是设备级协议参数(总线上怎么寻址该设备)。
type deviceParams struct {
	SlaveID    byte        `json:"slaveId"`
	PollBlocks []pollBlock `json:"pollBlocks"`
}

// pollBlock 声明一个固定读取块,适配必须按固定边界/数量读取的设备(绕过自动连读)。
type pollBlock struct {
	Function string `json:"function"`
	Start    uint16 `json:"start"`
	Count    uint16 `json:"count"`
}

func parseDeviceParams(raw json.RawMessage) (deviceParams, error) {
	params := deviceParams{}
	if len(raw) == 0 {
		return params, nil
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return deviceParams{}, fmt.Errorf("parse modbus device params: %w", err)
	}
	return params, nil
}

// buildClient 按配置创建具体 handler 并建立连接,返回 client 与 handler。
// handler 暴露 ClientHandler 接口(SetSlave 运行时切从机,Close 关连接),供连接复用。
func buildClient(ctx context.Context, cfg connConfig) (modbus.Client, modbus.ClientHandler, error) {
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil || timeout == 0 {
		timeout = defaultTimeout
	}
	switch cfg.Mode {
	case "rtu":
		handler := modbus.NewRTUClientHandler(cfg.SerialPort)
		handler.BaudRate = cfg.BaudRate
		handler.DataBits = cfg.DataBits
		handler.Parity = cfg.Parity
		handler.StopBits = cfg.StopBits
		handler.Timeout = timeout
		handler.IdleTimeout = -1 // 持久占用串口,不因空闲关闭
		if err := handler.Connect(ctx); err != nil {
			return nil, nil, fmt.Errorf("modbus rtu connect: %w", err)
		}
		return modbus.NewClient(handler), handler, nil
	case "rtu-over-tcp":
		// RTU 帧(带 CRC)走 TCP 传输,常见于 RS-485 串口服务器透传
		handler := modbus.NewRTUOverTCPClientHandler(cfg.Address)
		handler.Timeout = timeout
		handler.IdleTimeout = -1
		handler.LinkRecoveryTimeout = modbusLinkRecoveryTimeout
		handler.ProtocolRecoveryTimeout = modbusProtocolRecoveryTimeout
		if err := handler.Connect(ctx); err != nil {
			return nil, nil, fmt.Errorf("modbus rtu-over-tcp connect: %w", err)
		}
		return modbus.NewClient(handler), handler, nil
	case "tcp":
		handler := modbus.NewTCPClientHandler(cfg.Address)
		handler.Timeout = timeout
		handler.IdleTimeout = -1
		handler.LinkRecoveryTimeout = modbusLinkRecoveryTimeout
		handler.ProtocolRecoveryTimeout = modbusProtocolRecoveryTimeout
		if err := handler.Connect(ctx); err != nil {
			return nil, nil, fmt.Errorf("modbus tcp connect: %w", err)
		}
		return modbus.NewClient(handler), handler, nil
	default:
		return nil, nil, fmt.Errorf("unsupported modbus mode %q", cfg.Mode)
	}
}
