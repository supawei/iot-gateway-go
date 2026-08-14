// Package status 维护设备采集状态(在线/离线、最近采集、最近错误),供 scheduler
// 运行时上报、API 健康端点查询。它是内存态的可观测性信息,不持久化。
package status

import (
	"sort"
	"sync"
	"time"
)

// DeviceStatus 是单台设备的运行时健康快照。
type DeviceStatus struct {
	DeviceID    string    `json:"deviceId"`
	Online      bool      `json:"online"`
	LastCollect time.Time `json:"lastCollect"` // 最近一次成功采集时间(零值=从未)
	LastError   string    `json:"lastError"`   // 最近一次错误(空=无)
	LastErrorAt time.Time `json:"lastErrorAt"`
}

// Registry 是设备状态的并发安全注册表。
type Registry struct {
	mu      sync.RWMutex
	devices map[string]*DeviceStatus
}

func NewRegistry() *Registry {
	return &Registry{devices: make(map[string]*DeviceStatus)}
}

// SetOnline 标记设备在线并刷新最近采集时间,清空错误。
func (r *Registry) SetOnline(deviceID string, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.entry(deviceID)
	d.Online = true
	d.LastCollect = t
	d.LastError = ""
	d.LastErrorAt = time.Time{}
}

// SetOffline 标记设备离线并记录错误;LastCollect 保留最后一次成功采集时间。
func (r *Registry) SetOffline(deviceID string, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.entry(deviceID)
	d.Online = false
	d.LastError = errMsg
	d.LastErrorAt = time.Now()
}

// Get 返回设备状态;设备从未被上报过时 ok=false。
func (r *Registry) Get(deviceID string) (DeviceStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.devices[deviceID]
	if !ok {
		return DeviceStatus{}, false
	}
	return *d, true
}

// List 返回全部设备状态,按 DeviceID 排序。
func (r *Registry) List() []DeviceStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DeviceStatus, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}

func (r *Registry) entry(deviceID string) *DeviceStatus {
	d, ok := r.devices[deviceID]
	if !ok {
		d = &DeviceStatus{DeviceID: deviceID}
		r.devices[deviceID] = d
	}
	return d
}
