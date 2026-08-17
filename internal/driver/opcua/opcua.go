package opcua

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
)

func init() {
	driver.Register("opcua", &opcuaDriver{pool: make(map[string]*sharedSession)})
}

type opcuaDriver struct {
	mu   sync.Mutex
	pool map[string]*sharedSession
}

// EndpointKey 归一化物理端点(server URL):两个连接指向同一 server 会各自建
// session,虽不似总线那般冲突,但重复配置几乎必属误配,保存时同样拒绝。
func (*opcuaDriver) EndpointKey(connection json.RawMessage) string {
	cfg, err := parseConnConfig(connection)
	if err != nil {
		return ""
	}
	return "opcua|" + strings.ToLower(strings.TrimSpace(cfg.Endpoint))
}

// ConfigSchema 声明 Connection.config 结构。
func (*opcuaDriver) ConfigSchema() []driver.Field {
	return []driver.Field{
		{Name: "endpoint", Label: "端点", Type: driver.FieldString, Required: true, Placeholder: "opc.tcp://192.168.1.5:4840"},
		{Name: "mode", Label: "采集模式", Type: driver.FieldEnum, Default: "poll", Options: []string{"poll", "subscribe"}, Hint: "poll=轮询 / subscribe=订阅推送"},
		{Name: "securityMode", Label: "安全模式", Type: driver.FieldEnum, Default: "none", Options: []string{"none"}},
		{Name: "username", Label: "用户名", Type: driver.FieldString, Hint: "留空为匿名"},
		{Name: "password", Label: "密码", Type: driver.FieldString},
		{Name: "timeout", Label: "请求超时", Type: driver.FieldString, Default: "5s"},
		{Name: "publishInterval", Label: "发布间隔", Type: driver.FieldString, Default: "1s",
			ShowWhen: &driver.ShowWhen{Field: "mode", In: []string{"subscribe"}}},
		{Name: "samplingInterval", Label: "采样间隔(ms)", Type: driver.FieldNumber, Default: 0,
			Hint: "0=沿用发布间隔", ShowWhen: &driver.ShowWhen{Field: "mode", In: []string{"subscribe"}}},
		{Name: "queueSize", Label: "队列长度", Type: driver.FieldInt, Default: 10,
			ShowWhen: &driver.ShowWhen{Field: "mode", In: []string{"subscribe"}}},
	}
}

// ParamSchema 声明 Device.params 结构;OPC UA 无设备级参数。
func (*opcuaDriver) ParamSchema() []driver.Field {
	return nil
}

// sharedSession 是按 ConnectionID 共享的 OPC UA client/session,引用计数管理生命周期。
// OPC UA 走 TCP 全双工且 gopcua Client 支持并发请求,故同连接多设备可并发 Read,无需串行化。
// 订阅模式下,同一 endpoint 的所有设备共享一个 gopcua 订阅(sub),按 ClientHandle 分派到
// 各设备的回调;subCtx/subCancel 管订阅分发 goroutine 的生命周期,随 session 释放一并撤销。
type sharedSession struct {
	connectionID string
	client       *opcua.Client
	refCount     int

	subMu      sync.Mutex
	sub        *opcua.Subscription
	subCtx     context.Context
	subCancel  context.CancelFunc
	targets    map[uint32]subTarget
	nextHandle uint32
}

// subTarget 是订阅分派目标:ClientHandle 反查到的设备点位及其回调。
type subTarget struct {
	deviceID string
	point    model.Point
	onData   func(model.DataPoint)
}

type opcuaConn struct {
	deviceID string
	shared   *sharedSession
	driver   *opcuaDriver
}

// opcuaSubConn 是订阅模式的设备连接:内嵌 opcuaConn 复用 Read/Write/Close,
// 额外实现 driver.Subscriber。Open 仅在连接配置 mode=subscribe 时返回此类型,
// scheduler 据此类型断言切换到推送采集。
type opcuaSubConn struct {
	*opcuaConn
	cfg connConfig
}

func (d *opcuaDriver) Open(ctx context.Context, req driver.OpenRequest) (driver.Conn, error) {
	cfg, err := parseConnConfig(req.ConnConfig)
	if err != nil {
		return nil, err
	}
	shared, err := d.acquire(ctx, req.ConnectionID, cfg)
	if err != nil {
		return nil, err
	}
	base := &opcuaConn{deviceID: req.DeviceID, shared: shared, driver: d}
	if cfg.Mode == modeSubscribe {
		return &opcuaSubConn{opcuaConn: base, cfg: cfg}, nil
	}
	return base, nil
}

func (d *opcuaDriver) acquire(ctx context.Context, connectionID string, cfg connConfig) (*sharedSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if shared, ok := d.pool[connectionID]; ok {
		shared.refCount++
		return shared, nil
	}
	client, err := buildClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	subCtx, subCancel := context.WithCancel(context.Background())
	shared := &sharedSession{
		connectionID: connectionID,
		client:       client,
		refCount:     1,
		subCancel:    subCancel,
		subCtx:       subCtx,
	}
	d.pool[connectionID] = shared
	return shared, nil
}

func (d *opcuaDriver) release(shared *sharedSession) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	shared.refCount--
	if shared.refCount > 0 {
		return nil
	}
	delete(d.pool, shared.connectionID)
	// 先撤销订阅分发 goroutine(subCancel 触发其 defer 里 sub.Cancel),再关 client;
	// 顺序保证撤销订阅时 client 尚可用。
	if shared.subCancel != nil {
		shared.subCancel()
	}
	return shared.client.Close(context.Background())
}

func (c *opcuaConn) Read(ctx context.Context, points []model.Point) ([]model.DataPoint, error) {
	results := make([]model.DataPoint, len(points))
	now := time.Now()
	for index, point := range points {
		results[index] = model.DataPoint{
			DeviceID: c.deviceID, Point: point.Name,
			Timestamp: now, Quality: model.QualityBad,
		}
	}
	nodes, indices := planReads(points)
	if len(nodes) == 0 {
		return results, nil
	}
	resp, err := c.shared.client.Read(ctx, &ua.ReadRequest{NodesToRead: nodes})
	if err != nil {
		// 通信失败:全部点已初始化为 bad,照常返回让北向感知设备异常;
		// 不把通信错误升级为整批配置级 error(语义对齐 Read 约定)。
		slog.Error("opcua read failed", "device", c.deviceID, "err", err)
		return results, nil
	}
	applyReadResults(results, indices, resp.Results, points)
	return results, nil
}

func (c *opcuaConn) Close() error {
	return c.driver.release(c.shared)
}

// Write 下发点位值:解析 NodeID 后构造批量 WriteRequest,按 server 返回 status 标记各点。
// 单点 NodeID 解析失败或类型不匹配标记 Ok=false,不阻断同批。
func (c *opcuaConn) Write(ctx context.Context, items []model.WriteItem) ([]driver.WriteResult, error) {
	results := make([]driver.WriteResult, len(items))
	var nodes []*ua.WriteValue
	var indices []int
	for index, item := range items {
		results[index] = driver.WriteResult{Point: item.Point.Name}
		nodeID, err := ua.ParseNodeID(item.Point.Address)
		if err != nil {
			continue
		}
		val, ok := encodeValue(item.Value, item.Point.DataType)
		if !ok {
			continue
		}
		variant, err := ua.NewVariant(val)
		if err != nil {
			continue
		}
		nodes = append(nodes, &ua.WriteValue{
			NodeID:      nodeID,
			AttributeID: ua.AttributeIDValue,
			Value:       &ua.DataValue{Value: variant},
		})
		indices = append(indices, index)
	}
	if len(nodes) == 0 {
		return results, nil
	}
	resp, err := c.shared.client.Write(ctx, &ua.WriteRequest{NodesToWrite: nodes})
	if err != nil {
		return results, err
	}
	for row, status := range resp.Results {
		if row >= len(indices) {
			break
		}
		if status == ua.StatusOK {
			results[indices[row]].Ok = true
		}
	}
	return results, nil
}

// planReads 解析每个点位的 NodeID 地址;解析失败的点跳过(保持 bad),成功的收集为批量读请求。
func planReads(points []model.Point) ([]*ua.ReadValueID, []int) {
	var nodes []*ua.ReadValueID
	var indices []int
	for index, point := range points {
		nodeID, err := ua.ParseNodeID(point.Address)
		if err != nil {
			continue
		}
		nodes = append(nodes, &ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDValue})
		indices = append(indices, index)
	}
	return nodes, indices
}

func applyReadResults(results []model.DataPoint, indices []int, readResults []*ua.DataValue, points []model.Point) {
	for row, value := range readResults {
		if row >= len(indices) {
			break
		}
		index := indices[row]
		if value.Status != ua.StatusOK {
			continue
		}
		decoded, ok := decodeValue(value.Value.Value(), points[index].DataType, points[index].Scale)
		if !ok {
			results[index].Quality = model.QualityUncertain
			continue
		}
		results[index].Value = decoded
		results[index].Quality = model.QualityGood
	}
}

// decodeValue 把 OPC UA variant 返回的 Go 原生值按声明类型校验,并应用缩放。
// 类型不匹配返回 ok=false(标记 uncertain),而非整批失败。
func decodeValue(raw interface{}, dataType model.DataType, scale float64) (interface{}, bool) {
	switch dataType {
	case model.DataTypeBool:
		value, ok := raw.(bool)
		return value, ok
	case model.DataTypeString:
		value, ok := raw.(string)
		return value, ok
	case model.DataTypeInt16, model.DataTypeUInt16, model.DataTypeInt32, model.DataTypeUInt32, model.DataTypeInt64:
		value, ok := toInt64(raw)
		if !ok {
			return nil, false
		}
		if scale != 0 {
			return float64(value) * scale, true
		}
		return value, true
	case model.DataTypeFloat, model.DataTypeDouble:
		value, ok := toFloat64(raw)
		if !ok {
			return nil, false
		}
		if scale != 0 {
			return value * scale, true
		}
		return value, true
	default:
		return raw, true
	}
}

// encodeValue 把 JSON 解码的值按 dataType 转为 Go 原生类型,供 ua.NewVariant 构造写请求。
func encodeValue(value interface{}, dataType model.DataType) (interface{}, bool) {
	switch dataType {
	case model.DataTypeBool:
		b, ok := value.(bool)
		return b, ok
	case model.DataTypeString:
		s, ok := value.(string)
		return s, ok
	case model.DataTypeInt16:
		v, ok := toFloat64(value)
		return int16(v), ok
	case model.DataTypeUInt16:
		v, ok := toFloat64(value)
		return uint16(v), ok
	case model.DataTypeInt32:
		v, ok := toFloat64(value)
		return int32(v), ok
	case model.DataTypeUInt32:
		v, ok := toFloat64(value)
		return uint32(v), ok
	case model.DataTypeInt64:
		v, ok := toFloat64(value)
		return int64(v), ok
	case model.DataTypeFloat:
		v, ok := toFloat64(value)
		return float32(v), ok
	case model.DataTypeDouble:
		v, ok := toFloat64(value)
		return float64(v), ok
	default:
		return nil, false
	}
}

func toFloat64(raw interface{}) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int8:
		return float64(value), true
	case int16:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint8:
		return float64(value), true
	case uint16:
		return float64(value), true
	case uint32:
		return float64(value), true
	case uint64:
		return float64(value), true
	default:
		return 0, false
	}
}

func toInt64(raw interface{}) (int64, bool) {
	switch value := raw.(type) {
	case int:
		return int64(value), true
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		return int64(value), true
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		return int64(value), true
	case float32:
		return int64(value), true
	case float64:
		return int64(value), true
	default:
		return 0, false
	}
}

// notifyBufSize 是订阅通知 channel 的缓冲长度:吸收发布突发,避免阻塞 gopcua 的
// publish 循环;缓冲满时 gopcua 的 notify 会阻塞,故保持足够的余量。
const notifyBufSize = 128

// Subscribe 把设备点位注册进共享订阅:首次调用创建 gopcua 订阅并启动分发 goroutine,
// 后续调用复用同一订阅,只为新点位分配 ClientHandle 并登记。协议差异封死在此,
// Core 只经 onData 收到统一的 DataPoint。
func (c *opcuaSubConn) Subscribe(ctx context.Context, points []model.Point, onData func(model.DataPoint)) error {
	return c.shared.subscribe(ctx, c.deviceID, c.cfg, points, onData)
}

// subscribe 在共享 session 上登记设备点位:同一 endpoint 的多个设备共用一个 gopcua
// 订阅(sub),首次创建、后续复用;ClientHandle 从 nextHandle 递增分配(跨设备全局唯一),
// targets 记录 handle -> 设备点位回调,供分发 goroutine 反查。
func (s *sharedSession) subscribe(ctx context.Context, deviceID string, cfg connConfig, points []model.Point, onData func(model.DataPoint)) error {
	s.subMu.Lock()
	defer s.subMu.Unlock()

	items, indices, handles := buildMonitoredItems(points, cfg.SamplingInterval, cfg.QueueSize, s.nextHandle)
	if len(items) == 0 {
		return errors.New("opcua subscribe: no valid point addresses")
	}

	if s.sub == nil {
		publishInterval, _ := time.ParseDuration(cfg.PublishInterval)
		params := &opcua.SubscriptionParameters{Interval: publishInterval}
		notifyCh := make(chan *opcua.PublishNotificationData, notifyBufSize)
		sub, err := s.client.Subscribe(ctx, params, notifyCh)
		if err != nil {
			return fmt.Errorf("opcua subscribe: %w", err)
		}
		s.sub = sub
		s.targets = make(map[uint32]subTarget)
		go s.dispatch(sub, notifyCh)
	}

	resp, err := s.sub.Monitor(ctx, ua.TimestampsToReturnBoth, items...)
	if err != nil {
		return fmt.Errorf("opcua monitor: %w", err)
	}
	for row := range items {
		handle := handles[row]
		pointIndex := indices[row]
		// Monitor 不因单点失败而报错:结果中的 bad status 记日志且不登记 target,
		// 该点后续不会被推送,语义对齐 Read(单点失败不阻断同批)。
		if row < len(resp.Results) && resp.Results[row].StatusCode != ua.StatusOK {
			slog.Warn("opcua monitored item rejected", "device", deviceID, "point", points[pointIndex].Name, "status", resp.Results[row].StatusCode)
			continue
		}
		s.targets[handle] = subTarget{deviceID: deviceID, point: points[pointIndex], onData: onData}
	}
	s.nextHandle += uint32(len(items))
	slog.Info("opcua subscription points registered", "device", deviceID, "connection", s.connectionID, "points", len(items))
	return nil
}

// dispatch 消费共享订阅的通知,把 DataChangeNotification 里的监控项按 ClientHandle
// 反查 target 并回调。subCtx 取消(session 释放)或通知通道关闭时退出并撤销订阅。
func (s *sharedSession) dispatch(sub *opcua.Subscription, notifyCh <-chan *opcua.PublishNotificationData) {
	defer sub.Cancel(context.Background())
	for {
		select {
		case <-s.subCtx.Done():
			return
		case notif, ok := <-notifyCh:
			if !ok {
				return
			}
			if notif.Error != nil {
				slog.Error("opcua subscription notification error", "connection", s.connectionID, "err", notif.Error)
				continue
			}
			dc, ok := notif.Value.(*ua.DataChangeNotification)
			if !ok {
				continue // 状态变更/事件通知,本实现只关心数据变化
			}
			for _, item := range dc.MonitoredItems {
				s.subMu.Lock()
				target, ok := s.targets[item.ClientHandle]
				s.subMu.Unlock()
				if !ok {
					continue
				}
				target.onData(notificationToDataPoint(target.deviceID, target.point, item.Value))
			}
		}
	}
}

// buildMonitoredItems 为每个点位构造监控项:解析 NodeID 地址,失败的点跳过。
// ClientHandle 从 nextHandle 起递增分配(跨设备全局唯一),handles 与 items 一一对应;
// indices 记录 item 行对应的原点位下标,供 Monitor 结果状态回写日志。
func buildMonitoredItems(points []model.Point, samplingInterval float64, queueSize uint32, nextHandle uint32) ([]*ua.MonitoredItemCreateRequest, []int, []uint32) {
	var items []*ua.MonitoredItemCreateRequest
	var indices []int
	var handles []uint32
	handle := nextHandle
	for index, point := range points {
		nodeID, err := ua.ParseNodeID(point.Address)
		if err != nil {
			continue
		}
		item := opcua.NewMonitoredItemCreateRequestWithDefaults(nodeID, ua.AttributeIDValue, handle)
		if samplingInterval > 0 {
			item.RequestedParameters.SamplingInterval = samplingInterval
		}
		if queueSize > 0 {
			item.RequestedParameters.QueueSize = queueSize
		}
		items = append(items, item)
		indices = append(indices, index)
		handles = append(handles, handle)
		handle++
	}
	return items, indices, handles
}

// notificationToDataPoint 把单条数据变化通知转成 DataPoint:取数据源时间戳,校验
// status 与类型,失败用 Quality 表达(bad/uncertain),语义对齐 Read 的结果。
func notificationToDataPoint(deviceID string, point model.Point, dv *ua.DataValue) model.DataPoint {
	dp := model.DataPoint{DeviceID: deviceID, Point: point.Name, Quality: model.QualityBad}
	if dv == nil {
		return dp
	}
	switch {
	case !dv.SourceTimestamp.IsZero():
		dp.Timestamp = dv.SourceTimestamp
	case !dv.ServerTimestamp.IsZero():
		dp.Timestamp = dv.ServerTimestamp
	default:
		dp.Timestamp = time.Now()
	}
	if dv.Status != ua.StatusOK || dv.Value == nil {
		return dp
	}
	decoded, ok := decodeValue(dv.Value.Value(), point.DataType, point.Scale)
	if !ok {
		dp.Quality = model.QualityUncertain
		return dp
	}
	dp.Value = decoded
	dp.Quality = model.QualityGood
	return dp
}

type connConfig struct {
	Endpoint     string `json:"endpoint"`
	SecurityMode string `json:"securityMode"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Timeout      string `json:"timeout"`
	// Mode 采集模式:"poll"(默认,按周期轮询) 或 "subscribe"(订阅,数据变化即推送)。
	Mode string `json:"mode"`
	// PublishInterval 订阅发布间隔(如 "1s"、"500ms");仅 Mode=subscribe 时生效。
	PublishInterval string `json:"publishInterval"`
	// SamplingInterval 监控项采样间隔(毫秒),0 表示沿用发布间隔;仅订阅模式生效。
	SamplingInterval float64 `json:"samplingInterval"`
	// QueueSize 每个监控项在服务端的队列长度;仅订阅模式生效。
	QueueSize uint32 `json:"queueSize"`
}

const (
	modePoll      = "poll"
	modeSubscribe = "subscribe"

	defaultPublishInterval = 1 * time.Second
	defaultQueueSize       = 10
)

func parseConnConfig(raw json.RawMessage) (connConfig, error) {
	cfg := connConfig{SecurityMode: "none", Timeout: "5s", Mode: modePoll}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return connConfig{}, fmt.Errorf("parse opcua conn config: %w", err)
		}
	}
	if cfg.Endpoint == "" {
		return connConfig{}, errors.New("opcua endpoint is required")
	}
	if cfg.SecurityMode == "" {
		cfg.SecurityMode = "none"
	}
	if cfg.SecurityMode != "none" {
		return connConfig{}, fmt.Errorf("opcua securityMode %q not supported (only \"none\" for now)", cfg.SecurityMode)
	}
	if cfg.Timeout == "" {
		cfg.Timeout = "5s"
	}
	if _, err := time.ParseDuration(cfg.Timeout); err != nil {
		return connConfig{}, fmt.Errorf("invalid timeout %q: %w", cfg.Timeout, err)
	}
	switch cfg.Mode {
	case modePoll, modeSubscribe:
	default:
		return connConfig{}, fmt.Errorf("opcua mode %q not supported (only %q or %q)", cfg.Mode, modePoll, modeSubscribe)
	}
	if cfg.Mode == modeSubscribe {
		if cfg.PublishInterval == "" {
			cfg.PublishInterval = defaultPublishInterval.String()
		}
		if _, err := time.ParseDuration(cfg.PublishInterval); err != nil {
			return connConfig{}, fmt.Errorf("invalid publishInterval %q: %w", cfg.PublishInterval, err)
		}
		if cfg.QueueSize == 0 {
			cfg.QueueSize = defaultQueueSize
		}
	}
	return cfg, nil
}

const opcuaReconnectInterval = 5 * time.Second

// buildClient 建立启用了自动重连的 OPC UA client:连接断开后库自动重连,
// 状态变更经 stateCh 输出供 monitorConnState 记录离线/恢复。
func buildClient(ctx context.Context, cfg connConfig) (*opcua.Client, error) {
	timeout, _ := time.ParseDuration(cfg.Timeout)
	stateCh := make(chan opcua.ConnState, 8)
	opts := []opcua.Option{
		opcua.SecurityMode(ua.MessageSecurityModeNone),
		opcua.RequestTimeout(timeout),
		opcua.AutoReconnect(true),
		opcua.ReconnectInterval(opcuaReconnectInterval),
		opcua.StateChangedCh(stateCh),
	}
	if cfg.Username != "" {
		opts = append(opts, opcua.AuthUsername(cfg.Username, cfg.Password))
	} else {
		opts = append(opts, opcua.AuthAnonymous())
	}
	client, err := opcua.NewClient(cfg.Endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("create opcua client: %w", err)
	}
	if err := client.Connect(ctx); err != nil {
		return nil, fmt.Errorf("connect opcua %q: %w", cfg.Endpoint, err)
	}
	go monitorConnState(ctx, cfg.Endpoint, stateCh)
	return client, nil
}

// monitorConnState 记录连接状态变更(离线/重连/恢复),ctx 取消或连接 Closed 时退出。
func monitorConnState(ctx context.Context, endpoint string, stateCh <-chan opcua.ConnState) {
	for {
		select {
		case <-ctx.Done():
			return
		case state, ok := <-stateCh:
			if !ok {
				return
			}
			switch state {
			case opcua.Connected:
				slog.Info("opcua connected", "endpoint", endpoint)
			case opcua.Disconnected:
				slog.Warn("opcua disconnected, auto-reconnecting", "endpoint", endpoint)
			case opcua.Reconnecting:
				slog.Info("opcua reconnecting", "endpoint", endpoint)
			case opcua.Closed:
				return
			}
		}
	}
}
