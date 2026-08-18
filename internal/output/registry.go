package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"iot-gateway-go/internal/model"
)

// BuildContext 是构造输出所需的网关上下文:与输出自身配置无关的部分。
// 由 main 注入(gatewayID 用于 topic/标识,Write 用于下行写回设备,Store 用于插件自动同步配置,
// LatestValues 用于查询设备点位最新采集值,服务调用 get 等需要"设备当前属性值"的场景使用;
// Probe 用于设备连通性诊断(DC1003),经 core.ProbeDevice 做真实协议往返)。
type BuildContext struct {
	GatewayID string
	Write     WriteFunc
	Store     StoreAccessor

	// LatestValues 查询设备全部点位的最新采集值快照(pointID -> value)。
	// 由 main 基于 values.Registry 注入;实现可返回空 map(设备从未采集到有效值)。
	LatestValues LatestValuesFunc

	// Probe 探测设备连通性(设备诊断 DC1003 用)。由 main 注入 core.ProbeDevice;
	// 未注入时插件回退到在线状态等软信号诊断。
	Probe ProbeFunc
}

// LatestValuesFunc 返回设备全部点位的最新采集值快照,key 为 pointID,value 为采集值。
// 只包含有效值点位(值为 nil 的点位不返回,与 Publish 的 STRING/空值跳过语义一致)。
type LatestValuesFunc func(deviceID string) map[string]interface{}

// ProbeFunc 探测设备是否可达(连通性诊断)。返回 nil 表示可达;错误含具体原因
// (设备不存在/驱动不支持/协议往返失败)。ctx 带超时,实现须可取消。
type ProbeFunc func(ctx context.Context, deviceID string) error

// ErrNotProbeable 是 Probe 返回的可识别错误:驱动的 Conn 未实现 Prober(无法做真实探测)。
// 插件应 errors.Is 识别并回退到在线状态等软诊断,而不是把设备判为不可达。
var ErrNotProbeable = errors.New("driver does not support probe")

// WriteFunc 是下行写回调(如 ThingsBoard 共享属性 / RPC → 设备写)。
// 由 main 注入,最终落到 core.WritePoint。
type WriteFunc func(ctx context.Context, deviceID, point string, value interface{}) error

// StoreAccessor 是插件访问网关配置存储的接口。
// 实现者为 *store.Store;只暴露插件所需的写入方法,避免插件直接依赖 store 包。
type StoreAccessor interface {
	SaveConnection(conn model.Connection) error
	SaveDevice(device model.Device) error
}

// Descriptor 描述一种输出插件类型及其配置 schema,供 Web UI 渲染表单。
type Descriptor struct {
	Type   string  `json:"type"`   // 输出类型,如 "mqtt"
	Label  string  `json:"label"`  // 展示名,如 "MQTT"
	Schema []Field `json:"schema"` // 配置字段结构
}

// Constructor 用一条输出配置(raw JSON)构造一个 Output 实例。
// 返回错误时须保证不泄漏任何已建立的连接。
type Constructor func(bc BuildContext, config json.RawMessage) (Output, error)

type registered struct {
	desc Descriptor
	ctor Constructor
}

var (
	registry   = map[string]registered{}
	registryMu sync.RWMutex
)

// Register 注册一种输出类型,通常在输出插件包 init() 中调用。
func Register(desc Descriptor, ctor Constructor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[desc.Type] = registered{desc: desc, ctor: ctor}
}

// ListTypes 返回全部已注册输出类型的信息,按类型名排序。
func ListTypes() []Descriptor {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)

	infos := make([]Descriptor, 0, len(names))
	for _, name := range names {
		infos = append(infos, registry[name].desc)
	}
	return infos
}

// Build 按类型名与配置构造一个输出实例。
func Build(bc BuildContext, typ string, config json.RawMessage) (Output, error) {
	registryMu.RLock()
	r, ok := registry[typ]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("output type %q not registered", typ)
	}
	return r.ctor(bc, config)
}
