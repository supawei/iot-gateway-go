package driver

import (
	"context"
	"encoding/json"
	"fmt"
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
// Read 约定:error 仅用于整批配置级错误(此时结果无效);单点通信失败/解码异常
// 用对应 DataPoint.Quality 表达(bad/uncertain),不阻断同批其他点位,error 为 nil。
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

// WriteResult 是单点写结果:Ok=false 表示该点写失败(地址错误/类型不匹配/协议拒绝),
// 不阻断同批其他点,语义对齐 Read 的 Quality。
type WriteResult struct {
	Point string
	Ok    bool
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
