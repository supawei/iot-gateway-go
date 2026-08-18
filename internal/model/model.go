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

// Output 是北向输出配置:一个输出插件实例(如 MQTT / ThingsBoard / TDengine)。
// 与连接/设备一致,输出配置存 SQLite,经 Web UI 增删改并热重载(见 docs/northbound-output-config.md)。
type Output struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Type    string          `json:"type"` // mqtt / thingsboard / tdengine
	Config  json.RawMessage `json:"config"`
	Enabled bool            `json:"enabled"`
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

// Point 描述单个采集点:采什么、怎么采,以及可选的边缘处理(过滤/聚合)。
type Point struct {
	Name     string   `json:"name"`
	Address  string   `json:"address"`
	DataType DataType `json:"dataType"`
	Scale    float64  `json:"scale"`
	// Processing 是边缘处理配置(死区/阈值/质量过滤 + 时间窗口聚合)。
	// 为 nil 表示直通,见 docs/edge-computing-design.md。
	Processing *PointProcessing `json:"processing,omitempty"`
}

// PointProcessing 描述单点位的边缘处理。Filters 按序应用,全部通过才放行;
// Aggregate 非空时,过滤通过的数值点进入时间窗口聚合(不再逐条上送,窗口关闭产出派生点)。
type PointProcessing struct {
	Filters   []Filter   `json:"filters,omitempty"`
	Aggregate *Aggregate `json:"aggregate,omitempty"`
}

// Filter 是单条过滤规则,命中即丢弃该点。
type Filter struct {
	Type    string  `json:"type"`              // deadband | threshold | quality
	Delta   float64 `json:"delta,omitempty"`   // deadband:死区阈值,0 表示值变化即放行
	Op      string  `json:"op,omitempty"`      // threshold:gt|ge|lt|le|eq|ne
	Value   float64 `json:"value,omitempty"`   // threshold:单阈值
	Min     float64 `json:"min,omitempty"`     // threshold:between/outside 下界
	Max     float64 `json:"max,omitempty"`     // threshold:between/outside 上界
	DropBad bool    `json:"dropBad,omitempty"` // quality:丢弃 bad/uncertain
}

// Aggregate 是时间窗口聚合;窗口关闭时产出派生点位(默认名 <point>.<type>)。
type Aggregate struct {
	Type     string `json:"type"`               // avg|min|max|sum|count|last
	Window   string `json:"window"`             // 如 "10s"、"1m"、"30s"(time.ParseDuration)
	EmitName string `json:"emitName,omitempty"` // 派生点位名,默认 <point>.<type>
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
