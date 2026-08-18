package output

import (
	"time"

	"iot-gateway-go/internal/model"
)

// Output 是北向数据出口,消费协议无关的 DataPoint。
// 网关级单例:一个实例处理所有设备的数据,故无需 registry,由 main 直接构造。
type Output interface {
	Publish(dp model.DataPoint) error
	Close() error
}

// DeviceNotifier 是北向输出的可选能力:接收设备上线/离线事件。
// scheduler 在设备状态发生"上线/离线"转变时,对实现了该接口的输出调用对应方法
// (如 ThingsBoard 据此发 v1/gateway/connect / disconnect)。
type DeviceNotifier interface {
	DeviceOnline(deviceID string)
	DeviceOffline(deviceID string)
}

// RuntimeStatus 是输出自行上报的运行态(类型相关),由 Manager 并入整体状态。
// 见 docs/output-status-design.md。
type RuntimeStatus struct {
	Connected      bool      `json:"connected"`      // 逻辑连接:可收发或正在重连(可期待恢复)
	ConnectionOpen bool      `json:"connectionOpen"` // 物理连接是否真的建立
	Pending        int       `json:"pending"`        // 输出内部待发缓冲积压
	Sent           int64     `json:"sent"`           // 成功上送次数
	LastSentAt     time.Time `json:"lastSentAt"`     // 最近一次成功上送时间
	LastError      string    `json:"lastError"`      // 最近一次上送错误(空=无)
	LastErrorAt    time.Time `json:"lastErrorAt"`
}

// StatusProvider 是北向输出的可选能力:上报类型相关的运行态。
// Manager 在 Status() 中将其并入整体状态(与 DeviceNotifier 同模式)。
type StatusProvider interface {
	RuntimeStatus() RuntimeStatus
}
