package modbus

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
)

var errShortResponse = errors.New("modbus response too short")

func init() {
	driver.Register("modbus", modbusDriver{})
}

type modbusDriver struct{}

func (modbusDriver) Open(ctx context.Context, deviceID string, connection json.RawMessage) (driver.Conn, error) {
	cfg, err := parseConnConfig(connection)
	if err != nil {
		return nil, err
	}
	client, closer, err := buildClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &modbusConn{
		deviceID: deviceID,
		client:   client,
		closer:   closer,
	}, nil
}

type modbusConn struct {
	deviceID string
	client   modbus.Client
	closer   io.Closer
}

func (c *modbusConn) Read(ctx context.Context, point model.Point) (model.DataPoint, error) {
	function, register, err := parseAddress(point.Address)
	if err != nil {
		return model.DataPoint{}, err
	}
	raw, err := c.readRaw(ctx, function, register, quantityOf(point.DataType))
	timestamp := time.Now()
	if err != nil {
		// 通信失败:产出 bad 数据点,让北向感知设备异常
		return model.DataPoint{DeviceID: c.deviceID, Point: point.Name, Timestamp: timestamp, Quality: model.QualityBad}, nil
	}
	value, err := decodeValue(point.DataType, raw)
	if err != nil {
		return model.DataPoint{DeviceID: c.deviceID, Point: point.Name, Timestamp: timestamp, Quality: model.QualityUncertain}, nil
	}
	return model.DataPoint{
		DeviceID:  c.deviceID,
		Point:     point.Name,
		Value:     applyScale(value, point.Scale, point.DataType),
		Timestamp: timestamp,
		Quality:   model.QualityGood,
	}, nil
}

func (c *modbusConn) Close() error {
	return c.closer.Close()
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

type connConfig struct {
	Mode       string `json:"mode"`
	SlaveID    byte   `json:"slaveId"`
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

// buildClient 按配置创建具体 handler 并建立连接,返回 client 与可关闭句柄。
// Connect/Close 在具体 handler 类型上,ClientHandler 接口不暴露,故用 io.Closer 持有。
func buildClient(ctx context.Context, cfg connConfig) (modbus.Client, io.Closer, error) {
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
		handler.SlaveID = cfg.SlaveID
		handler.Timeout = timeout
		if err := handler.Connect(ctx); err != nil {
			return nil, nil, fmt.Errorf("modbus rtu connect: %w", err)
		}
		return modbus.NewClient(handler), handler, nil
	case "rtu-over-tcp":
		// RTU 帧(带 CRC)走 TCP 传输,常见于 RS-485 串口服务器透传
		handler := modbus.NewRTUOverTCPClientHandler(cfg.Address)
		handler.SlaveID = cfg.SlaveID
		handler.Timeout = timeout
		if err := handler.Connect(ctx); err != nil {
			return nil, nil, fmt.Errorf("modbus rtu-over-tcp connect: %w", err)
		}
		return modbus.NewClient(handler), handler, nil
	case "tcp":
		handler := modbus.NewTCPClientHandler(cfg.Address)
		handler.SlaveID = cfg.SlaveID
		handler.Timeout = timeout
		if err := handler.Connect(ctx); err != nil {
			return nil, nil, fmt.Errorf("modbus tcp connect: %w", err)
		}
		return modbus.NewClient(handler), handler, nil
	default:
		return nil, nil, fmt.Errorf("unsupported modbus mode %q", cfg.Mode)
	}
}
