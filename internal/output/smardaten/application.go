// Package smardaten 实现 smardaten-iot 私有 IoT 平台对接（北向输出插件）。
//
// 本插件作为标准 output.Output 实现，与 mqtt/thingsboard/tdengine 平级。
// 从 Manager.Publish() 接收 DataPoint，按平台契约格式转换后发布到平台 MQTT。
// 同时维护到平台的 MQTT 连接，订阅下行 topic（配置/服务调用/诊断），
// 下行指令通过 BuildContext.Write 回调写入设备。
//
// 平台契约参考: iot_platform_interaction.md
package smardaten

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// ---------- application.json 结构 ----------

// ApplicationConfig 是平台下发的 application.json 的完整结构。
// 它是平台契约的核心载体：多个平台 topic 直接取自该文件字段。
type ApplicationConfig struct {
	TopicInfoID string            `json:"topicInfoId"`
	Devices     []PlatformDevice  `json:"devices"`
	Controllers []PlatformController `json:"controllers"`
}

// PlatformDevice 表示平台定义的一个设备。
type PlatformDevice struct {
	DeviceID     string             `json:"deviceId"`
	ControllerID string             `json:"controllerId"`
	ProductID    string             `json:"productId"`
	Properties   []PlatformProperty `json:"properties"`
	Events       []PlatformEvent    `json:"events"`
	Services     []PlatformService  `json:"services"`
}

// PlatformProperty 表示设备的一个属性。
type PlatformProperty struct {
	Identifier string `json:"identifier"` // 属性标识符（上报 params 的 key）
	PointID    string `json:"pointId"`    // 点位ID（与采集数据的 id 对应）
	DataType   int    `json:"dataType"`   // 0..6（见枚举）
	AccessMode string `json:"accessMode"` // 访问模式
	UnitSymbol string `json:"unitSymbol"` // 单位
}

// PlatformEvent 表示设备的一个事件。
// identifier=="post" 的事件为属性上报事件，其 method 字段即为属性上报发布 topic。
type PlatformEvent struct {
	Identifier string `json:"identifier"` // 事件标识（"post" = 属性上报事件）
	Method     string `json:"method"`     // ★属性上报发布 topic（动态）
}

// PlatformService 表示设备的一个服务。
// method 为服务调用订阅 topic，responseMethod 为服务响应发布 topic。
type PlatformService struct {
	Identifier     string `json:"identifier"`     // 服务标识符
	Method         string `json:"method"`         // ★服务调用订阅 topic（动态）
	ResponseMethod string `json:"responseMethod"` // ★服务响应发布 topic（动态）
}

// PlatformController 表示平台定义的一个控制器。
type PlatformController struct {
	ControllerID string                 `json:"controllerId"`
	Type         string                 `json:"type"` // 控制器类型（字符串，匹配驱动类型）
	Specs        PlatformControllerSpecs `json:"specs"`
	SensorList   []PlatformSensor       `json:"sensorList"`
}

// PlatformControllerSpecs 控制器配置。
type PlatformControllerSpecs struct {
	CID           string          `json:"cid"`
	Enable        int             `json:"enable"`
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	Period        int             `json:"period"`        // 采集周期（秒）
	TimerType     string          `json:"timerType"`     // "0"=周期, "1"=时刻, "2"=监听
	Configuration json.RawMessage `json:"configuration"` // ★连接参数（类型相关）
}

// PlatformSensor 传感器点位。
type PlatformSensor struct {
	PointID  string          `json:"pointId"`
	ItemName string          `json:"itemName"`
	DataType int             `json:"dataType"`
	ExDesc   json.RawMessage `json:"exDesc"`
}

// ---------- 平台消息结构 ----------

// ConfigSetMessage 是平台下发的配置更新消息（通道 1 下行）。
type ConfigSetMessage struct {
	Identifier string `json:"identifier"` // "configUpdate" 触发下载
	URL        string `json:"url"`
}

// ConfigResponseMessage 是配置更新响应（通道 1 上行）。
type ConfigResponseMessage struct {
	Cmd    string `json:"cmd"`
	Status string `json:"status"`
}

// ProtocolUpdateMessage 是平台下发的协议驱动管理消息（通道 2 下行）。
type ProtocolUpdateMessage struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"` // add | delete | update | query
	Name       string `json:"name"`
	Type       string `json:"type"`
	Library    string `json:"library"`
	Version    string `json:"version"`
	URL        string `json:"url"`
	Index      []int  `json:"index"`
}

// ProtocolResponseMessage 是协议驱动管理响应（通道 2 上行）。
type ProtocolResponseMessage struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	StatusCode int    `json:"statusCode"`
	List       []struct {
		Index   int    `json:"index"`
		Name    string `json:"name"`
		Type    string `json:"type"`
		Library string `json:"library"`
		Version string `json:"version"`
	} `json:"list,omitempty"`
}

// PropertyReportMessage 是属性上报消息（通道 3 上行）。
type PropertyReportMessage struct {
	Version string                 `json:"version"`
	Params  map[string]interface{} `json:"params"`
}

// DeviceStatusMessage 是设备状态上报消息（通道 4 上行）。
type DeviceStatusMessage struct {
	DeviceID   string `json:"deviceId"`
	Status     int    `json:"status"` // 恒 1（在线）
	ReportTime int64  `json:"reportTime"`
}

// ServiceCallMessage 是平台下发的服务调用消息（通道 5 下行）。
type ServiceCallMessage struct {
	Identifier   string  `json:"identifier"`
	ServiceType  string  `json:"serviceType"` // "get" | "set"
	DeviceID     string  `json:"deviceId"`
	ControllerID string  `json:"controllerId"`
	CmdID        string  `json:"cmdId"`
	PointID      string  `json:"pointId,omitempty"` // set 才有
	Value        float64 `json:"value,omitempty"`   // set 才有
}

// ServiceGetResponseMessage 是服务调用 get 响应（通道 5 上行）。
type ServiceGetResponseMessage struct {
	CmdID      string                 `json:"cmdId"`
	StatusCode int                    `json:"statusCode"`
	Version    string                 `json:"version"`
	ReportTime int64                  `json:"reportTime"`
	Params     map[string]interface{} `json:"params,omitempty"`
}

// ServiceSetResponseMessage 是服务调用 set 响应（通道 5 上行）。
type ServiceSetResponseMessage struct {
	Identifier   string `json:"identifier"`
	ServiceType  string `json:"serviceType"`
	DeviceID     string `json:"deviceId"`
	ControllerID string `json:"controllerId"`
	CmdID        string `json:"cmdId"`
	StatusCode   int    `json:"statusCode"`
	ReportTime   int64  `json:"reportTime"`
}

// DiagnoseRequestMessage 是平台下发的诊断请求消息（通道 6 下行）。
type DiagnoseRequestMessage struct {
	DeviceID          string `json:"deviceId"`
	ControllerID      string `json:"controllerId"`
	DiagnoseReportID  string `json:"diagnose_report_id"`
	AssetID           string `json:"asset_id"`
	ExecuteTime       int64  `json:"executeTime"`
}

// DiagnoseResponseMessage 是诊断响应消息（通道 6 上行）。
type DiagnoseResponseMessage struct {
	IssuanceTime int64               `json:"issuance_time"`
	AssetID      string              `json:"asset_id"`
	Data         []DiagnoseItem      `json:"data"`
}

// DiagnoseItem 诊断项。
type DiagnoseItem struct {
	DiagnoseReportID       string `json:"diagnose_report_id"`
	DiagnoseItemID         string `json:"diagnose_item_id"`
	DiagnoseItemResultDesc string `json:"diagnose_item_result_desc"`
	Status                 int    `json:"status"`
	ExecuteTime            int64  `json:"execute_time"`
}

// ---------- topic 映射表 ----------

// topicMapping 由 application.json 解析后构建，提供快速查找。
type topicMapping struct {
	mu sync.RWMutex

	// deviceID -> event topic（属性上报发布 topic，来自 events[].method, identifier=="post"）
	deviceEventTopic map[string]string

	// deviceID -> pointID -> property identifier
	devicePropMap map[string]map[string]string

	// deviceID -> controllerID
	deviceController map[string]string

	// method topic -> PlatformService（服务调用订阅）
	serviceByMethod map[string]PlatformService

	// method topic -> responseMethod topic
	serviceRespTopic map[string]string

	// deviceID -> 用于判断是否含 DTU
	deviceHasDTU map[string]bool
}

func newTopicMapping() *topicMapping {
	return &topicMapping{
		deviceEventTopic:  make(map[string]string),
		devicePropMap:     make(map[string]map[string]string),
		deviceController:  make(map[string]string),
		serviceByMethod:   make(map[string]PlatformService),
		serviceRespTopic:  make(map[string]string),
		deviceHasDTU:      make(map[string]bool),
	}
}

// buildFrom 从 ApplicationConfig 构建 topic 映射。
func (m *topicMapping) buildFrom(cfg *ApplicationConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 清空旧映射
	m.deviceEventTopic = make(map[string]string)
	m.devicePropMap = make(map[string]map[string]string)
	m.deviceController = make(map[string]string)
	m.serviceByMethod = make(map[string]PlatformService)
	m.serviceRespTopic = make(map[string]string)
	m.deviceHasDTU = make(map[string]bool)

	for _, dev := range cfg.Devices {
		// 属性映射: pointId -> identifier
		propMap := make(map[string]string)
		for _, prop := range dev.Properties {
			propMap[prop.PointID] = prop.Identifier
		}
		m.devicePropMap[dev.DeviceID] = propMap

		// 事件 topic: identifier=="post" 的 method
		for _, evt := range dev.Events {
			if evt.Identifier == "post" {
				m.deviceEventTopic[dev.DeviceID] = evt.Method
			}
		}

		// 服务 topic 映射
		for _, svc := range dev.Services {
			m.serviceByMethod[svc.Method] = svc
			m.serviceRespTopic[svc.Method] = svc.ResponseMethod
		}

		// controller 映射
		m.deviceController[dev.DeviceID] = dev.ControllerID

		// 判断是否含 DTU（后续诊断用）
		// 简单策略：controller type 为 DTU 相关类型时标记
		// 暂不实现 DTU 检测，统一设为 false
		m.deviceHasDTU[dev.DeviceID] = false
	}
}

// eventTopic 返回设备的属性上报 topic。
func (m *topicMapping) eventTopic(deviceID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.deviceEventTopic[deviceID]
}

// propIdentifier 返回 pointID 对应的属性标识符。
func (m *topicMapping) propIdentifier(deviceID, pointID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if pm, ok := m.devicePropMap[deviceID]; ok {
		return pm[pointID]
	}
	return ""
}

// controllerID 返回设备所属控制器 ID。
func (m *topicMapping) controllerID(deviceID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.deviceController[deviceID]
}

// serviceMethods 返回所有服务调用订阅 topic。
func (m *topicMapping) serviceMethods() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	methods := make([]string, 0, len(m.serviceByMethod))
	for m := range m.serviceByMethod {
		methods = append(methods, m)
	}
	return methods
}

// service 返回 method topic 对应的 PlatformService。
func (m *topicMapping) service(method string) (PlatformService, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	svc, ok := m.serviceByMethod[method]
	return svc, ok
}

// responseTopic 返回服务 method 对应的响应 topic。
func (m *topicMapping) responseTopic(method string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.serviceRespTopic[method]
}

// allDeviceIDs 返回所有设备 ID。
func (m *topicMapping) allDeviceIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.devicePropMap))
	for id := range m.devicePropMap {
		ids = append(ids, id)
	}
	return ids
}

// hasDevice 检查设备是否在映射中。
func (m *topicMapping) hasDevice(deviceID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.devicePropMap[deviceID]
	return ok
}

// ---------- application.json 加载 ----------

// loadApplicationConfig 从文件加载 application.json。
func loadApplicationConfig(path string) (*ApplicationConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read application.json: %w", err)
	}
	var cfg ApplicationConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse application.json: %w", err)
	}
	return &cfg, nil
}

// saveApplicationConfig 保存 application.json 到文件。
func saveApplicationConfig(path string, cfg *ApplicationConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal application.json: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write application.json: %w", err)
	}
	return nil
}