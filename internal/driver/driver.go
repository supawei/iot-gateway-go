package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"iot-gateway-go/internal/model"
)

// OpenRequest 汇聚打开设备连接所需的全部上下文:传输参数(ConnConfig,来自 Connection)
// 与设备级协议参数(DeviceParams,来自 Device)。ConnectionID 用作驱动内部连接复用的池 key。
type OpenRequest struct {
	DeviceID     string
	ConnectionID string
	ConnConfig   json.RawMessage
	DeviceParams json.RawMessage
}

// Driver 是南向协议驱动的无状态工厂;Open 出绑定单个设备的 Conn。
type Driver interface {
	Open(ctx context.Context, req OpenRequest) (Conn, error)
}

// Conn 绑定单个设备连接,批量读取点位并返回协议无关的 DataPoint。
// 协议的差异性(地址格式、读写细节)封死在实现内部。
// Read 约定:error 仅用于整批错误(此时结果无效)——配置级错误或整批传输/会话失败
// (如连接断开、请求超时);单点通信失败/解码异常用对应 DataPoint.Quality 表达
// (bad/uncertain),不阻断同批其他点位,error 为 nil。
type Conn interface {
	Read(ctx context.Context, points []model.Point) ([]model.DataPoint, error)
	Close() error
}

// Writer 是可选能力:支持写入的驱动实现此接口。调用方通过类型断言(conn.(Writer))检测。
type Writer interface {
	Write(ctx context.Context, items []model.WriteItem) ([]WriteResult, error)
}

// Subscriber 是南向驱动的可选推送能力:支持订阅式采集的 Conn 实现此接口。
// scheduler 检测到该能力后,不再按 intervalMs 定时调用 Read,而是注册一次
// Subscribe,数据变化时经 onData 回调推送协议无关的 DataPoint。
// ctx 取消(配置变更/进程关闭)后,实现须停止推送并释放订阅资源。
// 协议差异(订阅参数、回调解码)封死在实现内部,Core 只面对统一的 DataPoint。
type Subscriber interface {
	Subscribe(ctx context.Context, points []model.Point, onData func(model.DataPoint)) error
}

// Prober 是南向驱动的可选探测能力:实现它的 Conn 支持"设备连通性诊断"(如
// smardaten-iot 通道 6 的 DC1003)。points 为该设备配置的全部点位,实现可取
// 代表性点位做一次轻量协议级读来确认设备可达(与采集同寻址语义)。
// 返回 nil 表示设备可达;非 nil 表示不可达(含原因)。未实现该接口的驱动,
// 诊断方回退到在线状态等软信号。
type Prober interface {
	Probe(ctx context.Context, points []model.Point) error
}

// Listener 是南向驱动的可选监听能力:实现它的 Conn 表示网关被动 listen,
// 设备主动连入并上报数据(与 Subscriber 的"网关主动订阅"相对)。
// scheduler 检测到该能力后,不再按 intervalMs 定时调用 Read,而是注册一次
// Listen,数据到达时经 onData 回调推送协议无关的 DataPoint。
// 设备路由信息(如从机地址)在 Open 时经 DeviceParams 传入并封存在 Conn 内,
// Listen 只注册该设备的点位;同一 Connection 的多个设备共享底层监听 socket。
// ctx 取消(配置变更/进程关闭)后,实现须停止监听并释放资源。
type Listener interface {
	Listen(ctx context.Context, points []model.Point, onData func(model.DataPoint)) error
}

// NodeInfo 描述服务器节点树中的一个节点,供 Web UI 节点选择器渲染。
type NodeInfo struct {
	NodeID      string `json:"nodeId"`             // 完整地址(如 OPC UA NodeID "ns=2;i=1001"),可直接写入 Point.Address
	BrowseName  string `json:"browseName"`         // 浏览名
	DisplayName string `json:"displayName"`        // 展示名(优先于 BrowseName 显示)
	NodeClass   string `json:"nodeClass"`          // 节点类型(Variable/Object/Method/...)
	HasChildren bool   `json:"hasChildren"`        // 是否可能有子节点(供前端懒加载提示)
	DataType    string `json:"dataType,omitempty"` // 仅 Variable 节点:数据类型短名(如 "float64"),可直接回填 Point.dataType
}

// Browser 是南向驱动的可选浏览能力:按连接浏览服务器节点树,供点位编辑时
// "浏览选择"节点,避免手填地址。协议差异(浏览方向、引用类型、NodeID 归一化)
// 封死在实现内部。实现它的驱动(如 opcua)使 Web UI 能动态取节点。
type Browser interface {
	// Browse 返回 parentNodeID(空串表示根/Objects 文件夹)的直接子节点。
	// connConfig 为该连接的配置(存储为明文,可直接用于建连),connectionID 用于连接池复用。
	Browse(ctx context.Context, connectionID string, connConfig json.RawMessage, parentNodeID string) ([]NodeInfo, error)
}

// WriteResult 是单点写结果:Ok=false 表示该点写失败(地址错误/类型不匹配/协议拒绝),
// 不阻断同批其他点,语义对齐 Read 的 Quality。
type WriteResult struct {
	Point string
	Ok    bool
}

// FieldType 描述配置字段对应的输入控件类型。
type FieldType string

const (
	FieldString FieldType = "string" // 文本
	FieldInt    FieldType = "int"    // 整数
	FieldNumber FieldType = "number" // 数值(可小数)
	FieldBool   FieldType = "bool"   // 开关
	FieldEnum   FieldType = "enum"   // 下拉选择(配合 Options)
	FieldJSON   FieldType = "json"   // 复杂嵌套结构,用 JSON 编辑
)

// ShowWhen 描述字段的显示条件:当依赖字段(Field)的值属于 In 集合时显示该字段。
// 用于按"连接模式/采集模式"等字段动态显示或隐藏相关配置项。
type ShowWhen struct {
	Field string   `json:"field"`
	In    []string `json:"in"`
}

// Field 描述一个配置字段。驱动借此向 Web UI 暴露自己的配置结构,
// 前端据此动态渲染表单,实现"新协议零前端改动"。
type Field struct {
	Name        string    `json:"name"`                  // JSON key
	Label       string    `json:"label"`                 // 展示名
	Type        FieldType `json:"type"`                  // 控件类型
	Required    bool      `json:"required,omitempty"`    // 是否必填
	Default     any       `json:"default,omitempty"`     // 默认值
	Options     []string  `json:"options,omitempty"`     // enum 的可选项
	Hint        string    `json:"hint,omitempty"`        // 补充说明
	Placeholder string    `json:"placeholder,omitempty"` // 占位提示
	ShowWhen    *ShowWhen `json:"showWhen,omitempty"`    // 显示条件
}

// EndpointResolver 是驱动的可选能力:从 Connection.config 计算物理端点标识。
// key 属于跨驱动的共享命名空间,不含协议/驱动信息,按物理资源取值:
//   - serial|<串口路径>
//   - tcp|<host:port>(设备或 DTU 端点)
//   - listen|<本地绑定地址>(监听型)
//
// 网关据此在所有连接间(不限驱动)阻止两个 Connection 指向同一物理总线/端点:
// 连接池按 ConnectionID 复用,同一串口/DTU 出现两条并发通道会同时写同一条
// 485 总线,导致帧碰撞。返回空串表示无法识别,跳过检查。
type EndpointResolver interface {
	EndpointKey(config json.RawMessage) string
}

// SchemaProvider 是驱动的可选能力:声明 Connection.config 与 Device.params 的结构。
// 未实现该能力的驱动,前端退化为原始 JSON 编辑。
type SchemaProvider interface {
	ConfigSchema() []Field
	ParamSchema() []Field
}

// DriverInfo 是驱动的对外描述(名称 + 配置 schema)。
type DriverInfo struct {
	Name   string  `json:"name"`
	Config []Field `json:"config"`
	Params []Field `json:"params"`
}

// List 返回所有已注册驱动的信息,按名称排序。
func List() []DriverInfo {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)

	infos := make([]DriverInfo, 0, len(names))
	for _, name := range names {
		info := DriverInfo{Name: name}
		if sp, ok := registry[name].(SchemaProvider); ok {
			info.Config = sp.ConfigSchema()
			info.Params = sp.ParamSchema()
		}
		infos = append(infos, info)
	}
	return infos
}

var (
	registry   = map[string]Driver{}
	registryMu sync.RWMutex
)

// Register 注册一个驱动,通常在驱动包 init() 中调用。
func Register(name string, drv Driver) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = drv
}

// Get 按名称获取已注册驱动。
func Get(name string) (Driver, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	drv, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("driver %q not registered", name)
	}
	return drv, nil
}
