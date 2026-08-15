// Package values 维护设备各点位的最新采集值(内存态快照),供 Web 界面查看
// 设备当前属性值。与 status(健康状态)分离:这里只关心"每个点位最近采到了什么值"。
package values

import (
	"sort"
	"sync"
	"time"

	"iot-gateway-go/internal/model"
)

// PointValue 是单点位最近一次采集到的值快照。
type PointValue struct {
	Point     string        `json:"point"`
	Value     interface{}   `json:"value"`
	Quality   model.Quality `json:"quality"`
	Timestamp time.Time     `json:"timestamp"`
}

// DeviceValues 是单台设备全部点位的最新值快照。
type DeviceValues struct {
	DeviceID string       `json:"deviceId"`
	Points   []PointValue `json:"points"`
}

// Registry 是设备点位最新值的并发安全注册表(内存态,不持久化)。
type Registry struct {
	mu      sync.RWMutex
	devices map[string]map[string]PointValue // deviceID -> pointName -> 最新值
}

func NewRegistry() *Registry {
	return &Registry{devices: make(map[string]map[string]PointValue)}
}

// Update 记录一个数据点的最新值。bad/uncertain 的点也记录(值为 nil),让界面
// 能看到该点位当前的质量状态而非直接消失。
func (r *Registry) Update(dp model.DataPoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	points := r.devices[dp.DeviceID]
	if points == nil {
		points = make(map[string]PointValue)
		r.devices[dp.DeviceID] = points
	}
	points[dp.Point] = PointValue{
		Point:     dp.Point,
		Value:     dp.Value,
		Quality:   dp.Quality,
		Timestamp: dp.Timestamp,
	}
}

// Get 返回设备全部点位的最新值,按点位名排序;设备从未上报过时返回空列表。
func (r *Registry) Get(deviceID string) DeviceValues {
	r.mu.RLock()
	defer r.mu.RUnlock()
	points := r.devices[deviceID]
	out := make([]PointValue, 0, len(points))
	for _, p := range points {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Point < out[j].Point })
	return DeviceValues{DeviceID: deviceID, Points: out}
}
