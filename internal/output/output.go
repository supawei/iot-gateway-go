package output

import "iot-gateway-go/internal/model"

// Output 是北向数据出口,消费协议无关的 DataPoint。
// 网关级单例:一个实例处理所有设备的数据,故无需 registry,由 main 直接构造。
type Output interface {
	Publish(dp model.DataPoint) error
	Close() error
}
