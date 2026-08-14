package model

import (
	"encoding/json"
	"time"
)

// Connection 是传输层连接,可被多个设备共享(如同一串口或 DTU 下的多个 Modbus 从站)。
// Config 只含传输参数(怎么到达总线);设备级协议参数(从机地址等)归 Device.Params。
type Connection struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Driver string          `json:"driver"`
	Config json.RawMessage `json:"config"`
}

// Device 表示一个物理或逻辑设备,引用一个 Connection 并携带设备级协议参数。
// IntervalMs 为设备级采集周期,连读时按此周期批量读取所有点位。
type Device struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	ConnectionID string          `json:"connectionId"`
	Params       json.RawMessage `json:"params"`
	Points       []Point         `json:"points"`
	IntervalMs   int             `json:"intervalMs"`
	Enabled      bool            `json:"enabled"`
}

// Point 描述单个采集点:采什么、怎么采。
type Point struct {
	Name     string   `json:"name"`
	Address  string   `json:"address"`
	DataType DataType `json:"dataType"`
	Scale    float64  `json:"scale"`
}

// DataPoint 是采集的标准化结果,贯穿 Core 与北向输出,协议无关。
type DataPoint struct {
	DeviceID  string      `json:"deviceId"`
	Point     string      `json:"point"`
	Value     interface{} `json:"value"`
	Timestamp time.Time   `json:"timestamp"`
	Quality   Quality     `json:"quality"`
}

type DataType string

const (
	DataTypeBool   DataType = "bool"
	DataTypeInt16  DataType = "int16"
	DataTypeUInt16 DataType = "uint16"
	DataTypeInt32  DataType = "int32"
	DataTypeUInt32 DataType = "uint32"
	DataTypeFloat  DataType = "float32"
)

type Quality string

const (
	QualityGood      Quality = "good"
	QualityBad       Quality = "bad"
	QualityUncertain Quality = "uncertain"
)
