package output

import "iot-gateway-go/internal/model"

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
