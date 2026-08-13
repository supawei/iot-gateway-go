package model

import (
	"encoding/json"
	"time"
)

// Device 表示一个物理或逻辑设备,对应一个协议连接实例。
// IntervalMs 为设备级采集周期,连读时按此周期批量读取所有点位。
type Device struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Driver     string          `json:"driver"`
	Connection json.RawMessage `json:"connection"`
	Points     []Point         `json:"points"`
	IntervalMs int             `json:"intervalMs"`
	Enabled    bool            `json:"enabled"`
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
