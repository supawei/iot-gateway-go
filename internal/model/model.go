package model

import (
	"encoding/json"
	"time"
)

// Connection 是传输层连接,可被多个设备共享(如同一串口或 DTU 下的多个 Modbus 从站)。
// Config 只含传输参数(怎么到达总线);设备级协议参数(从机地址等)归 Device.Params。
// ManagedBy 标记该连接的自动管理来源(如 "smardaten:<outputID>");空串表示
// Web UI 手工配置,不会被平台同步的孤儿清理删除。见 internal/output/smardaten/sync.go。
type Connection struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Driver    string          `json:"driver"`
	Config    json.RawMessage `json:"config"`
	ManagedBy string          `json:"managedBy,omitempty"`
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
// ManagedBy 同 Connection.ManagedBy,标记设备的自动管理来源(空串=手工配置)。
type Device struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	ConnectionID string          `json:"connectionId"`
	Params       json.RawMessage `json:"params"`
	Points       []Point         `json:"points"`
	IntervalMs   int             `json:"intervalMs"`
	Enabled      bool            `json:"enabled"`
	ManagedBy    string          `json:"managedBy,omitempty"`
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

// AlertSeverity 是告警级别。
type AlertSeverity string

const (
	SeverityWarning  AlertSeverity = "warning"
	SeverityCritical AlertSeverity = "critical"
)

// AlertStatus 是告警记录的状态机状态(本地告警表的概念,不放入发出消息)。
type AlertStatus string

const (
	AlertPending  AlertStatus = "pending"
	AlertResolved AlertStatus = "resolved"
)

// RefPoint 是告警规则引用的一个设备点位。
type RefPoint struct {
	DeviceID string `json:"deviceId"`
	Point    string `json:"point"`
}

// AlertRule 是跨设备/跨点位告警规则:表达式求值为 true 即触发告警。
// 规则配置存 SQLite(alert_rules 表),随 store.OnChange 热重载,范式与 Output 一致。
// 表达式经 expr-lang/expr 求值,引擎注入 point(deviceID, pointName) 函数返回引用点位最新值;
// ReferencedPoints 显式声明引用点位,引擎据此建反向索引,免 AST 分析。
type AlertRule struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Enabled          bool       `json:"enabled"`
	Severity         string     `json:"severity"`         // warning | critical
	Expr             string     `json:"expr"`             // 如 point("d1","temp")>30 && point("d2","sw")=="off"
	ReferencedPoints []RefPoint `json:"referencedPoints"` // 显式声明表达式引用的点位
	OutputIDs        []string   `json:"outputIds"`        // 定向投递的 output ID
	FreshnessSeconds int        `json:"freshnessSeconds"` // 状态新鲜度秒数,默认 300
	CooldownSeconds  int        `json:"cooldownSeconds"`  // 解除后防抖秒数,默认 0
	CreatedAt        string     `json:"createdAt"`
	UpdatedAt        string     `json:"updatedAt"`
}

// AlertContext 是触发瞬间一个引用点位的值快照(告警消息与告警表记录的公共字段)。
type AlertContext struct {
	DeviceID  string      `json:"deviceId"`
	Point     string      `json:"point"`
	Value     interface{} `json:"value"`
	Timestamp time.Time   `json:"timestamp"`
}

// AlertMessage 是告警事件的消息载荷(独立于 DataPoint),经 AlertPublisher 定向投递到指定输出。
// 与 Alert 表记录对齐,但不带 Status/ResolvedAt(那是本地表的状态机概念,见 docs)。
type AlertMessage struct {
	AlertID     string         `json:"alertId"`
	RuleID      string         `json:"ruleId"`
	RuleName    string         `json:"ruleName"`
	Severity    string         `json:"severity"`
	Message     string         `json:"message"`
	TriggeredAt time.Time      `json:"triggeredAt"`
	GatewayID   string         `json:"gatewayId"`
	Context     []AlertContext `json:"context"`
}

// Alert 是一条已触发的告警记录(存 alerts 表),比 AlertMessage 多本地状态机字段。
type Alert struct {
	AlertID     string         `json:"alertId"`
	RuleID      string         `json:"ruleId"`
	RuleName    string         `json:"ruleName"`
	Severity    string         `json:"severity"`
	Message     string         `json:"message"`
	TriggeredAt time.Time      `json:"triggeredAt"`
	GatewayID   string         `json:"gatewayId"`
	Context     []AlertContext `json:"context"`
	Status      string         `json:"status"` // pending | resolved
	ResolvedAt  *time.Time     `json:"resolvedAt,omitempty"`
}
