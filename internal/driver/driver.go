package driver

import (
	"encoding/json"
	"fmt"
	"sync"

	"iot-gateway-go/internal/model"
)

// Driver 是南向协议驱动的无状态工厂;每个设备 Open 出独立 Conn,
// 这样同一驱动类型可服务多个设备实例(如多个 Modbus 从站)。
type Driver interface {
	Open(deviceID string, connection json.RawMessage) (Conn, error)
}

// Conn 绑定单个设备连接,按点位读取并返回协议无关的 DataPoint。
// 协议的差异性(地址格式、读写细节)封死在实现内部。
// Read 约定:error 仅用于配置级错误(地址无法解析等),此时数据点无效;
// 数据质量问题(通信失败、解码异常)用 DataPoint.Quality 表达,error 为 nil。
type Conn interface {
	Read(point model.Point) (model.DataPoint, error)
	Close() error
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
