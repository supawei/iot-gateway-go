package smardaten

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

const (
	defaultQoS        byte = 1
	defaultQoS2       byte = 2
	keepAlive              = 60 // 秒
	disconnectQuiesce      = 250 * time.Millisecond

	defaultFlushInterval = 200 * time.Millisecond
	defaultMaxPubTime    = 60
	writeTimeout         = 5 * time.Second

	// pubMode
	pubModeTimely = 0 // 全属性刷新后上报
	pubModeChange = 1 // 变化死区上报
)

// Config 是 smardaten-iot 平台输出的配置（存 SQLite，经 Web UI 配置）。
// 所有数值字段使用 flexInt 类型，兼容 Web UI 发送的数字或字符串。
type Config struct {
	Broker   string `json:"broker"`   // MQTT broker 完整地址，如 tcp://10.0.0.1:1883
	Username string `json:"username"` // MQTT 用户名
	Password string `json:"password"` // MQTT 密码
	ClientID string `json:"clientId"` // MQTT Client ID

	IotConfigPath string `json:"iotConfigPath"` // application.json 落盘路径

	PubMode       flexInt `json:"pubMode"`       // 0=及时上报, 1=变化上报
	MaxPubTime    flexInt `json:"maxPubTime"`    // 变化上报模式最大周期间隔（秒）
	FlushInterval string  `json:"flushInterval"` // 数据聚合 flush 间隔
}

// flexInt 兼容 JSON 数字和字符串，用于 Web UI 配置字段。
// null 视为 0（与 Go 对 int 的默认行为一致）。
type flexInt int

func (f *flexInt) UnmarshalJSON(data []byte) error {
	// null 视为 0
	if string(data) == "null" {
		*f = 0
		return nil
	}
	// 尝试数字
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*f = flexInt(n)
		return nil
	}
	// 尝试字符串
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("flexInt: invalid number %q", s)
		}
		*f = flexInt(n)
		return nil
	}
	return fmt.Errorf("flexInt: expected number or string, got %s", string(data))
}

// init 注册 smardaten-iot 输出类型。
func init() {
	output.Register(output.Descriptor{
		Type:  "smardaten-iot",
		Label: "smardaten-iot",
		Schema: []output.Field{
			{Name: "broker", Label: "Broker 地址", Type: output.FieldString, Required: true, Placeholder: "tcp://平台IP:1883", Hint: "完整地址，含协议与端口"},
			{Name: "username", Label: "用户名", Type: output.FieldString},
			{Name: "password", Label: "密码", Type: output.FieldPassword},
			{Name: "clientId", Label: "Client ID", Type: output.FieldString, Placeholder: "gw-dev-manage"},
			{Name: "iotConfigPath", Label: "配置文件路径", Type: output.FieldString, Default: "config/application.json", Hint: "平台下发的 application.json 落盘路径"},
			{Name: "pubMode", Label: "上报模式", Type: output.FieldEnum, Options: []string{"0", "1"}, Default: 0, Hint: "0=及时上报, 1=变化上报"},
			{Name: "maxPubTime", Label: "最大上报间隔(秒)", Type: output.FieldInt, Default: 60, Hint: "变化上报模式用"},
			{Name: "flushInterval", Label: "Flush 间隔", Type: output.FieldString, Default: "200ms", Hint: "数据聚合上报间隔"},
		},
	}, func(bc output.BuildContext, raw json.RawMessage) (output.Output, error) {
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("smardaten-iot config: %w", err)
		}
		return New(cfg, bc.GatewayID, bc.Write, bc.Store)
	})
}

// platformOutput 实现 output.Output + output.DeviceNotifier。
type platformOutput struct {
	gatewayID string
	write     output.WriteFunc     // 下行写回调
	store     output.StoreAccessor // 配置同步（application.json → 网关配置）

	client pahomqtt.Client
	qos    byte

	// 配置
	cfg        Config
	topics     *topicMapping
	downloader *httpDownloader

	// 解析后的配置值
	pubMode    int // 0=及时, 1=变化
	maxPubTime int // 秒

	// 数据缓冲
	mu          sync.Mutex
	pending     map[string][]model.DataPoint  // 待上报数据点
	lastValues  map[string]map[string]float64 // deviceID -> pointID -> lastValue (变化上报用)
	lastPubTime map[string]time.Time          // deviceID -> 上次上报时间
	connects    map[string]bool               // 待发送 connect 的设备
	disconnects map[string]bool               // 待发送 disconnect 的设备

	// 已连接设备
	connected map[string]bool

	// 生命周期
	flushInterval time.Duration
	done          chan struct{}
	wg            sync.WaitGroup
}

// New 构造 smardaten-iot 平台输出。
func New(cfg Config, gatewayID string, write output.WriteFunc, store output.StoreAccessor) (output.Output, error) {
	if cfg.Broker == "" {
		return nil, fmt.Errorf("smardaten-iot broker is required")
	}
	if cfg.IotConfigPath == "" {
		cfg.IotConfigPath = "config/application.json"
	}

	// 解析配置值
	pubMode := int(cfg.PubMode)
	maxPubTime := int(cfg.MaxPubTime)
	if maxPubTime <= 0 {
		maxPubTime = defaultMaxPubTime
	}

	flushInterval := defaultFlushInterval
	if cfg.FlushInterval != "" {
		d, err := time.ParseDuration(cfg.FlushInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid flushInterval %q: %w", cfg.FlushInterval, err)
		}
		flushInterval = d
	}

	// 构建 HTTP 下载器（应用 ID 与 RSA 公钥为平台固定值，内置常量）
	downloader, err := newHTTPDownloader()
	if err != nil {
		return nil, fmt.Errorf("init http downloader: %w", err)
	}

	// 构建 MQTT 连接
	client, err := connectMQTT(cfg.Broker, cfg.ClientID, cfg.Username, cfg.Password, gatewayID)
	if err != nil {
		return nil, fmt.Errorf("mqtt connect: %w", err)
	}

	o := &platformOutput{
		gatewayID:     gatewayID,
		write:         write,
		store:         store,
		client:        client,
		qos:           defaultQoS,
		cfg:           cfg,
		topics:        newTopicMapping(),
		downloader:    downloader,
		pubMode:       pubMode,
		maxPubTime:    maxPubTime,
		pending:       make(map[string][]model.DataPoint),
		lastValues:    make(map[string]map[string]float64),
		lastPubTime:   make(map[string]time.Time),
		connects:      make(map[string]bool),
		disconnects:   make(map[string]bool),
		connected:     make(map[string]bool),
		flushInterval: flushInterval,
		done:          make(chan struct{}),
	}

	// 尝试加载本地 application.json
	o.loadConfig()

	// 订阅平台 topic
	if err := o.subscribeAll(); err != nil {
		client.Disconnect(uint(disconnectQuiesce / time.Millisecond))
		return nil, fmt.Errorf("subscribe platform topics: %w", err)
	}

	o.wg.Add(1)
	go o.runFlusher()

	return o, nil
}

// Publish 缓冲 DataPoint，由 flusher 定时聚合上报。
func (o *platformOutput) Publish(dp model.DataPoint) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	deviceID := dp.DeviceID
	if !o.topics.hasDevice(deviceID) {
		// 设备不在 application.json 中，跳过
		return nil
	}

	// STRING 类型不上报
	if dp.Value == nil {
		return nil
	}

	o.pending[deviceID] = append(o.pending[deviceID], dp)
	return nil
}

// DeviceOnline 记录设备上线意图。
func (o *platformOutput) DeviceOnline(deviceID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.topics.hasDevice(deviceID) {
		return
	}
	o.connects[deviceID] = true
	delete(o.disconnects, deviceID)
}

// DeviceOffline 记录设备离线意图。
func (o *platformOutput) DeviceOffline(deviceID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.topics.hasDevice(deviceID) {
		return
	}
	o.disconnects[deviceID] = true
	delete(o.connects, deviceID)
}

// Close 停止 flusher、写尽缓冲、断开 MQTT。
func (o *platformOutput) Close() error {
	close(o.done)
	o.wg.Wait()
	o.flush()
	o.client.Disconnect(uint(disconnectQuiesce / time.Millisecond))
	return nil
}

// ---------- MQTT 连接 ----------

// connectMQTT 建立到平台的 MQTT 连接，broker 为完整地址（含协议与端口）。
// 当前仅支持 MQTT 3.1.1 协议。
func connectMQTT(broker, clientID, username, password, gatewayID string) (pahomqtt.Client, error) {
	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(broker)

	if clientID == "" {
		// 用 gatewayID 生成唯一 clientID，避免网关 ID 变更时新旧连接冲突
		clientID = "gw-dev-manage-" + gatewayID
	}
	opts.SetClientID(clientID)

	opts.SetKeepAlive(keepAlive * time.Second)
	opts.SetConnectTimeout(5 * time.Second) // 初始连接超时，避免阻塞 New() 导致 HTTP 挂起
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(2 * time.Second)
	opts.SetMaxReconnectInterval(30 * time.Second)

	if username != "" {
		opts.SetUsername(username)
		opts.SetPassword(password)
	}

	// 协议版本：固定 MQTT 3.1.1（paho 用 4 表示）
	opts.SetProtocolVersion(4)

	client := pahomqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return nil, fmt.Errorf("mqtt connect: %w", token.Error())
	}

	slog.Info("smardaten-iot mqtt connected", "broker", broker, "clientId", clientID)
	return client, nil
}

// ---------- 订阅管理 ----------

// subscribeAll 订阅所有平台 topic。
func (o *platformOutput) subscribeAll() error {
	// 通道 1: 配置下发
	topic := fmt.Sprintf("/sys/%s/thing/config/set", o.gatewayID)
	if token := o.client.Subscribe(topic, defaultQoS2, o.handleConfigSet); token.Wait() && token.Error() != nil {
		return fmt.Errorf("subscribe config/set: %w", token.Error())
	}
	slog.Info("subscribed", "topic", topic)

	// 通道 6: 设备诊断
	topic = fmt.Sprintf("/sys/%s/thing/event/diagnose/set", o.gatewayID)
	if token := o.client.Subscribe(topic, defaultQoS, o.handleDiagnose); token.Wait() && token.Error() != nil {
		return fmt.Errorf("subscribe diagnose/set: %w", token.Error())
	}
	slog.Info("subscribed", "topic", topic)

	// 动态 topic: 服务调用（从 application.json 加载）
	o.resubscribeServices()

	return nil
}

// resubscribeServices 重新订阅所有服务调用 topic。
func (o *platformOutput) resubscribeServices() {
	for _, method := range o.topics.serviceMethods() {
		if token := o.client.Subscribe(method, defaultQoS2, o.handleServiceCall); token.Wait() && token.Error() != nil {
			slog.Error("subscribe service method failed", "topic", method, "err", token.Error())
		} else {
			slog.Info("subscribed service", "topic", method)
		}
	}
}

// ---------- 下行消息处理 ----------

// handleConfigSet 处理平台下发的配置更新（通道 1）。
func (o *platformOutput) handleConfigSet(_ pahomqtt.Client, msg pahomqtt.Message) {
	slog.Info("config set received", "topic", msg.Topic())
	var req ConfigSetMessage
	if err := json.Unmarshal(msg.Payload(), &req); err != nil {
		slog.Error("parse config set", "err", err)
		o.publishConfigResponse("fail")
		return
	}

	if req.Identifier != "configUpdate" {
		slog.Info("config set ignored", "identifier", req.Identifier)
		return
	}

	if req.URL == "" {
		slog.Error("config set missing url")
		o.publishConfigResponse("fail")
		return
	}

	// 下载配置
	if err := o.downloader.downloadConfig(req.URL, o.cfg.IotConfigPath); err != nil {
		slog.Error("download config failed", "url", req.URL, "err", err)
		o.publishConfigResponse("fail")
		return
	}

	// 重新加载配置
	if err := o.loadConfig(); err != nil {
		slog.Error("reload config failed", "err", err)
		o.publishConfigResponse("fail")
		return
	}

	o.publishConfigResponse("ok")
}

// publishConfigResponse 发布配置响应（通道 1 上行）。
func (o *platformOutput) publishConfigResponse(status string) {
	topic := fmt.Sprintf("/sys/%s/thing/config/response", o.gatewayID)
	resp := ConfigResponseMessage{Cmd: "config", Status: status}
	data, _ := json.Marshal(resp)
	o.client.Publish(topic, defaultQoS, false, data)
}

// handleServiceCall 处理平台下发的服务调用（通道 5）。
func (o *platformOutput) handleServiceCall(_ pahomqtt.Client, msg pahomqtt.Message) {
	slog.Info("service call received", "topic", msg.Topic())
	var req ServiceCallMessage
	if err := json.Unmarshal(msg.Payload(), &req); err != nil {
		slog.Error("parse service call", "err", err)
		return
	}

	respTopic := o.topics.responseTopic(msg.Topic())
	now := time.Now().UnixMilli()

	switch req.ServiceType {
	case "get":
		o.handleServiceGet(req, respTopic, now)
	case "set":
		o.handleServiceSet(req, respTopic, now)
	default:
		slog.Error("unknown service type", "type", req.ServiceType)
		o.publishServiceError(respTopic, req.CmdID, now)
	}
}

// handleServiceGet 处理 get 服务调用。
func (o *platformOutput) handleServiceGet(req ServiceCallMessage, respTopic string, now int64) {
	// 构造响应：从缓冲中取最新值
	o.mu.Lock()
	points := o.pending[req.DeviceID]
	o.mu.Unlock()

	params := map[string]interface{}{
		"deviceId":   req.DeviceID,
		"reportTime": now,
	}

	// 取每个属性的最新值
	seen := make(map[string]bool)
	for i := len(points) - 1; i >= 0; i-- {
		dp := points[i]
		identifier := o.topics.propIdentifier(req.DeviceID, dp.Point)
		if identifier == "" || seen[identifier] {
			continue
		}
		seen[identifier] = true
		params[identifier] = roundValue(dp.Value)
	}

	resp := ServiceGetResponseMessage{
		CmdID:      req.CmdID,
		StatusCode: 0,
		Version:    "1.0",
		ReportTime: now,
		Params:     params,
	}
	data, _ := json.Marshal(resp)
	o.client.Publish(respTopic, defaultQoS, false, data)
}

// handleServiceSet 处理 set 服务调用。
func (o *platformOutput) handleServiceSet(req ServiceCallMessage, respTopic string, now int64) {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	err := o.write(ctx, req.DeviceID, req.PointID, req.Value)

	statusCode := 0
	if err != nil {
		slog.Error("service set write failed", "device", req.DeviceID, "point", req.PointID, "err", err)
		statusCode = -6 // CTRL_WRITE_ERR
	}

	resp := ServiceSetResponseMessage{
		Identifier:   req.Identifier,
		ServiceType:  "set",
		DeviceID:     req.DeviceID,
		ControllerID: req.ControllerID,
		CmdID:        req.CmdID,
		StatusCode:   statusCode,
		ReportTime:   now,
	}
	data, _ := json.Marshal(resp)
	o.client.Publish(respTopic, defaultQoS, false, data)
}

// publishServiceError 发布服务错误响应。
func (o *platformOutput) publishServiceError(respTopic, cmdID string, now int64) {
	resp := ServiceGetResponseMessage{
		CmdID:      cmdID,
		StatusCode: -1, // NO_SUCH_SERVICE
		Version:    "1.0",
		ReportTime: now,
	}
	data, _ := json.Marshal(resp)
	o.client.Publish(respTopic, defaultQoS, false, data)
}

// handleDiagnose 处理设备诊断请求（通道 6）。
func (o *platformOutput) handleDiagnose(_ pahomqtt.Client, msg pahomqtt.Message) {
	slog.Info("diagnose request received", "topic", msg.Topic())
	var req DiagnoseRequestMessage
	if err := json.Unmarshal(msg.Payload(), &req); err != nil {
		slog.Error("parse diagnose request", "err", err)
		return
	}

	now := time.Now().UnixMilli()
	items := []DiagnoseItem{
		{
			DiagnoseReportID:       req.DiagnoseReportID,
			DiagnoseItemID:         "DC1001",
			DiagnoseItemResultDesc: "网关服务正常",
			Status:                 1,
			ExecuteTime:            now,
		},
		{
			DiagnoseReportID:       req.DiagnoseReportID,
			DiagnoseItemID:         "DC1003",
			DiagnoseItemResultDesc: "设备在线",
			Status:                 1,
			ExecuteTime:            now,
		},
	}

	resp := DiagnoseResponseMessage{
		IssuanceTime: req.ExecuteTime,
		AssetID:      req.AssetID,
		Data:         items,
	}

	topic := fmt.Sprintf("/sys/%s/thing/event/diagnose/set_reply", o.gatewayID)
	data, _ := json.Marshal(resp)
	o.client.Publish(topic, defaultQoS, false, data)
}

// ---------- 配置加载 ----------

// loadConfig 从本地加载 application.json 并更新 topic 映射。
func (o *platformOutput) loadConfig() error {
	cfg, err := loadApplicationConfig(o.cfg.IotConfigPath)
	if err != nil {
		slog.Warn("load application.json failed, using empty mapping", "path", o.cfg.IotConfigPath, "err", err)
		return err
	}

	o.topics.buildFrom(cfg)
	slog.Info("application.json loaded", "devices", len(cfg.Devices), "controllers", len(cfg.Controllers))

	// 同步到网关配置（自动创建/更新 Connection 和 Device）
	o.syncToGateway(cfg)

	// 重新订阅动态 topic（服务调用）
	o.resubscribeServices()

	return nil
}

// ---------- 数据上报 ----------

// runFlusher 按 flushInterval 周期性 flush。
func (o *platformOutput) runFlusher() {
	defer o.wg.Done()
	ticker := time.NewTicker(o.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-o.done:
			return
		case <-ticker.C:
			o.flush()
		}
	}
}

// flush 处理：设备状态 → 属性上报。
func (o *platformOutput) flush() {
	o.mu.Lock()
	pending := o.pending
	o.pending = make(map[string][]model.DataPoint)
	connects := o.connects
	o.connects = make(map[string]bool)
	disconnects := o.disconnects
	o.disconnects = make(map[string]bool)
	o.mu.Unlock()

	now := time.Now().UnixMilli()

	// 处理 disconnect
	for deviceID := range disconnects {
		delete(o.connected, deviceID)
		// 平台不支持 status=0，不发送离线
		_ = deviceID
	}

	// 处理 connect
	for deviceID := range connects {
		o.connected[deviceID] = true
	}

	// 属性上报
	for deviceID, points := range pending {
		eventTopic := o.topics.eventTopic(deviceID)
		if eventTopic == "" {
			continue
		}

		// 构建上报消息
		msg := o.buildPropertyReport(deviceID, points, now)
		if msg == nil {
			continue
		}

		data, err := json.Marshal(msg)
		if err != nil {
			slog.Error("marshal property report", "device", deviceID, "err", err)
			continue
		}

		if token := o.client.Publish(eventTopic, defaultQoS, false, data); token.Wait() && token.Error() != nil {
			slog.Error("publish property report", "device", deviceID, "topic", eventTopic, "err", token.Error())
			continue
		}

		// 紧随属性上报后发送设备状态上报（通道 4）
		o.publishDeviceStatus(deviceID, now)
	}
}

// buildPropertyReport 构建属性上报消息。
func (o *platformOutput) buildPropertyReport(deviceID string, points []model.DataPoint, now int64) *PropertyReportMessage {
	params := map[string]interface{}{
		"deviceId":   deviceID,
		"reportTime": now,
	}

	// 按 pointID 聚合最新值
	latest := make(map[string]model.DataPoint)
	for _, dp := range points {
		if existing, ok := latest[dp.Point]; !ok || dp.Timestamp.After(existing.Timestamp) {
			latest[dp.Point] = dp
		}
	}

	// 根据 pubMode 决定上报策略
	if o.pubMode == pubModeTimely {
		// 及时上报：设备所有属性至少被刷新过一次后才发
		// 简化：每次 flush 时上报所有最新值
		for pointID, dp := range latest {
			identifier := o.topics.propIdentifier(deviceID, pointID)
			if identifier == "" {
				continue
			}
			params[identifier] = roundValue(dp.Value)
		}
	} else {
		// 变化上报：|Δ| > 0.01 或超过 maxPubTime
		o.mu.Lock()
		lastVals := o.lastValues[deviceID]
		if lastVals == nil {
			lastVals = make(map[string]float64)
			o.lastValues[deviceID] = lastVals
		}
		lastPub := o.lastPubTime[deviceID]
		o.mu.Unlock()

		hasChange := false
		for pointID, dp := range latest {
			identifier := o.topics.propIdentifier(deviceID, pointID)
			if identifier == "" {
				continue
			}
			val, ok := toFloat64(dp.Value)
			if !ok {
				continue
			}
			lastVal, exists := lastVals[pointID]
			if !exists || math.Abs(val-lastVal) > 0.01 {
				hasChange = true
				lastVals[pointID] = val
			}
			params[identifier] = roundValue(dp.Value)
		}

		// 无变化且未超 maxPubTime 则跳过
		if !hasChange && time.Since(lastPub) < time.Duration(o.maxPubTime)*time.Second {
			return nil
		}

		o.mu.Lock()
		o.lastPubTime[deviceID] = time.Now()
		o.mu.Unlock()
	}

	if len(params) <= 2 { // 只有 deviceId + reportTime，无实际属性
		return nil
	}

	return &PropertyReportMessage{
		Version: "1.0",
		Params:  params,
	}
}

// publishDeviceStatus 发布设备状态上报（通道 4）。
func (o *platformOutput) publishDeviceStatus(deviceID string, now int64) {
	topic := "/sys/thing/event/deviceStatus/post"
	msg := DeviceStatusMessage{
		DeviceID:   deviceID,
		Status:     1, // 恒为 1（在线）
		ReportTime: now,
	}
	data, _ := json.Marshal(msg)
	o.client.Publish(topic, defaultQoS, false, data)
}

// ---------- 工具函数 ----------

// roundValue 保留 2 位小数（与平台契约一致）。
func roundValue(v interface{}) interface{} {
	switch val := v.(type) {
	case float32:
		return math.Round(float64(val)*100) / 100
	case float64:
		return math.Round(val*100) / 100
	default:
		return v
	}
}

// toFloat64 把值转为 float64（用于变化死区判断）。
func toFloat64(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case float32:
		return float64(val), true
	case float64:
		return val, true
	case int:
		return float64(val), true
	case int16:
		return float64(val), true
	case int32:
		return float64(val), true
	case int64:
		return float64(val), true
	case uint16:
		return float64(val), true
	case uint32:
		return float64(val), true
	default:
		return 0, false
	}
}
