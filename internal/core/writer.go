package core

import (
	"context"
	"errors"
	"fmt"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/store"
)

// 写点位的可识别错误,调用方可用 errors.Is 映射到具体语义(如 HTTP 状态码)。
var (
	ErrDeviceNotFound = errors.New("device not found")
	ErrPointNotFound  = errors.New("point not found")
	ErrNotWritable    = errors.New("driver does not support write")
)

// WritePoint 向设备的某个点位下发一个值:查设备/连接 → 打开驱动连接 → 调 Writer.Write。
// 供 REST 写接口与北向下行(如 ThingsBoard 属性下发)复用。
func WritePoint(ctx context.Context, st *store.Store, deviceID, pointName string, value interface{}) ([]driver.WriteResult, error) {
	device, err := st.GetDevice(deviceID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDeviceNotFound, err)
	}
	point, ok := findPointByName(device.Points, pointName)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPointNotFound, pointName)
	}
	connection, err := st.GetConnection(device.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("get connection: %w", err)
	}
	drv, err := driver.Get(connection.Driver)
	if err != nil {
		return nil, err
	}
	conn, err := drv.Open(ctx, driver.OpenRequest{
		DeviceID:     device.ID,
		ConnectionID: device.ConnectionID,
		ConnConfig:   connection.Config,
		DeviceParams: device.Params,
	})
	if err != nil {
		return nil, fmt.Errorf("open device: %w", err)
	}
	defer conn.Close()
	writer, ok := conn.(driver.Writer)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotWritable, connection.Driver)
	}
	return writer.Write(ctx, []model.WriteItem{{Point: point, Value: value}})
}

func findPointByName(points []model.Point, name string) (model.Point, bool) {
	for _, p := range points {
		if p.Name == name {
			return p, true
		}
	}
	return model.Point{}, false
}

// ProbeDevice 探测设备是否可达(设备诊断 DC1003):查设备/连接 → 打开驱动连接 →
// 调 Prober.Probe 做一次真实协议往返。驱动不支持探测时返回 output.ErrNotProbeable。
// 供 smardaten-iot 通道 6 诊断注入。
func ProbeDevice(ctx context.Context, st *store.Store, deviceID string) error {
	device, err := st.GetDevice(deviceID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDeviceNotFound, err)
	}
	connection, err := st.GetConnection(device.ConnectionID)
	if err != nil {
		return fmt.Errorf("get connection: %w", err)
	}
	drv, err := driver.Get(connection.Driver)
	if err != nil {
		return err
	}
	conn, err := drv.Open(ctx, driver.OpenRequest{
		DeviceID:     device.ID,
		ConnectionID: device.ConnectionID,
		ConnConfig:   connection.Config,
		DeviceParams: device.Params,
	})
	if err != nil {
		return fmt.Errorf("open device: %w", err)
	}
	defer conn.Close()
	prober, ok := conn.(driver.Prober)
	if !ok {
		// 用 output 包的哨兵错误:插件可 errors.Is 识别"驱动不支持探测"并回退软诊断
		// (output 不 import core,core 已 import output,单向依赖无环)。
		return fmt.Errorf("%w: %s", output.ErrNotProbeable, connection.Driver)
	}
	return prober.Probe(ctx, device.Points)
}
