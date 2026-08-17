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

// User 是登录管理员账号。密码只存 bcrypt 哈希,不落明文。
type User struct {
	ID                 string `json:"id"`
	PasswordHash       string `json:"-"` // bcrypt 哈希,永不序列化返回
	MustChangePassword bool   `json:"mustChangePassword"`
	Enabled            bool   `json:"enabled"`
}

// Client 是三方系统接入主体,凭 API Key(只存哈希)+ 绑定 scope 授权。
type Client struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	APIKeyHash string   `json:"-"` // SHA-256 哈希,永不序列化返回
	Scopes     []string `json:"scopes"`
	Enabled    bool     `json:"enabled"`
	CreatedAt  string   `json:"createdAt"`
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

// WriteItem 是下发写操作的输入:复用 Point 的地址/类型/缩放信息,携带工程值。
type WriteItem struct {
	Point Point
	Value interface{}
}

type DataType string

const (
	DataTypeBool   DataType = "bool"
	DataTypeInt16  DataType = "int16"
	DataTypeUInt16 DataType = "uint16"
	DataTypeInt32  DataType = "int32"
	DataTypeUInt32 DataType = "uint32"
	DataTypeFloat  DataType = "float32"
	DataTypeDouble DataType = "float64"
	DataTypeInt64  DataType = "int64"
	DataTypeString DataType = "string"
)

type Quality string

const (
	QualityGood      Quality = "good"
	QualityBad       Quality = "bad"
	QualityUncertain Quality = "uncertain"
)
