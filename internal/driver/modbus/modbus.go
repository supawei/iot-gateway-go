package modbus

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
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
)

var errShortResponse = errors.New("modbus response too short")

func init() {
	driver.Register("modbus", modbusDriver{})
}

type modbusDriver struct{}

func (modbusDriver) Open(ctx context.Context, req driver.OpenRequest) (driver.Conn, error) {
	cfg, err := parseConnConfig(req.ConnConfig)
	if err != nil {
		return nil, err
	}
	params, err := parseDeviceParams(req.DeviceParams)
	if err != nil {
		return nil, err
	}
	client, closer, err := buildClient(ctx, cfg, params.SlaveID)
	if err != nil {
		return nil, err
	}
	return &modbusConn{
		deviceID: req.DeviceID,
		client:   client,
		closer:   closer,
	}, nil
}

type modbusConn struct {
	deviceID string
	client   modbus.Client
	closer   io.Closer
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
		c.readGroup(ctx, function, items, results, timestamp)
	}
	return results, nil
}

func (c *modbusConn) Close() error {
	return c.closer.Close()
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

func (c *modbusConn) readRaw(ctx context.Context, function string, register, quantity uint16) ([]byte, error) {
	switch function {
	case "holding":
		return c.client.ReadHoldingRegisters(ctx, register, quantity)
	case "input":
		return c.client.ReadInputRegisters(ctx, register, quantity)
	case "coil":
		return c.client.ReadCoils(ctx, register, quantity)
	case "discrete":
		return c.client.ReadDiscreteInputs(ctx, register, quantity)
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
	SlaveID byte `json:"slaveId"`
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

// buildClient 按配置创建具体 handler 并建立连接,返回 client 与可关闭句柄。
// Connect/Close 在具体 handler 类型上,ClientHandler 接口不暴露,故用 io.Closer 持有。
func buildClient(ctx context.Context, cfg connConfig, slaveID byte) (modbus.Client, io.Closer, error) {
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
		handler.SlaveID = slaveID
		handler.Timeout = timeout
		if err := handler.Connect(ctx); err != nil {
			return nil, nil, fmt.Errorf("modbus rtu connect: %w", err)
		}
		return modbus.NewClient(handler), handler, nil
	case "rtu-over-tcp":
		// RTU 帧(带 CRC)走 TCP 传输,常见于 RS-485 串口服务器透传
		handler := modbus.NewRTUOverTCPClientHandler(cfg.Address)
		handler.SlaveID = slaveID
		handler.Timeout = timeout
		if err := handler.Connect(ctx); err != nil {
			return nil, nil, fmt.Errorf("modbus rtu-over-tcp connect: %w", err)
		}
		return modbus.NewClient(handler), handler, nil
	case "tcp":
		handler := modbus.NewTCPClientHandler(cfg.Address)
		handler.SlaveID = slaveID
		handler.Timeout = timeout
		if err := handler.Connect(ctx); err != nil {
			return nil, nil, fmt.Errorf("modbus tcp connect: %w", err)
		}
		return modbus.NewClient(handler), handler, nil
	default:
		return nil, nil, fmt.Errorf("unsupported modbus mode %q", cfg.Mode)
	}
}
