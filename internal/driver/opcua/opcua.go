package opcua

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uapolicy"

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

// EndpointKey 从 endpoint URL 提取 host:port 进跨驱动共享命名空间:
// 同一 server 端点只允许一个连接,重复配置几乎必属误配,保存时拒绝。
func (*opcuaDriver) EndpointKey(connection json.RawMessage) string {
	cfg, err := parseConnConfig(connection)
	if err != nil {
		return ""
	}
	parsed, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	port := parsed.Port()
	if port == "" {
		port = defaultEndpointPort
	}
	return "tcp|" + strings.ToLower(parsed.Hostname()) + ":" + port
}

// ConfigSchema 声明 Connection.config 结构。
func (*opcuaDriver) ConfigSchema() []driver.Field {
	secShow := &driver.ShowWhen{Field: "securityMode", In: []string{"sign", "signAndEncrypt"}}
	return []driver.Field{
		{Name: "endpoint", Label: "端点", Type: driver.FieldString, Required: true, Placeholder: "opc.tcp://192.168.1.5:4840"},
		{Name: "mode", Label: "采集模式", Type: driver.FieldEnum, Default: "poll", Options: []string{"poll", "subscribe"}, Hint: "poll=轮询 / subscribe=订阅推送"},
		{Name: "securityMode", Label: "安全模式", Type: driver.FieldEnum, Default: "none", Options: []string{"none", "sign", "signAndEncrypt"},
			Hint: "sign=签名防篡改 / signAndEncrypt=签名+加密"},
		{Name: "securityPolicy", Label: "安全策略", Type: driver.FieldEnum, Default: "auto",
			Options: []string{"auto", "Basic128Rsa15", "Basic256", "Basic256Sha256", "Aes128Sha256RsaOaep", "Aes256Sha256RsaPss"},
			Hint:    "auto=按服务器端点协商选最强;Basic256Sha256 为工业界事实标准", ShowWhen: secShow},
		{Name: "clientCertFile", Label: "客户端证书", Type: driver.FieldString, Placeholder: "opcua-client-cert.pem",
			Hint: "留空自动生成自签证书(工作目录)", ShowWhen: secShow},
		{Name: "clientKeyFile", Label: "客户端私钥", Type: driver.FieldString, Placeholder: "opcua-client-key.pem",
			Hint: "留空自动生成;证书/私钥须成对配置", ShowWhen: secShow},
		{Name: "serverThumbprint", Label: "服务器证书指纹", Type: driver.FieldString,
			Hint: "40 位 SHA-1 指纹(hex);设置后建连前校验,防中间人/防证书被换", ShowWhen: secShow},
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

// Browse 实现 driver.Browser:复用连接池共享 session,浏览 parentNodeID 的直接
// 子节点(层次引用 Organizes/HasComponent/HasProperty 等,含子类型)。parentNodeID
// 为空串时从 Objects 文件夹(i=85)开始,返回可写入 Point.Address 的 NodeID 字符串。
func (d *opcuaDriver) Browse(ctx context.Context, connectionID string, connConfig json.RawMessage, parentNodeID string) ([]driver.NodeInfo, error) {
	cfg, err := parseConnConfig(connConfig)
	if err != nil {
		return nil, err
	}
	shared, err := d.acquire(ctx, connectionID, cfg)
	if err != nil {
		return nil, err
	}
	defer d.release(shared)

	parent := ua.NewNumericNodeID(0, id.ObjectsFolder)
	if s := strings.TrimSpace(parentNodeID); s != "" {
		parent, err = ua.ParseNodeID(s)
		if err != nil {
			return nil, fmt.Errorf("opcua browse: invalid parent node id %q: %w", s, err)
		}
	}
	refs, err := shared.client.Node(parent).References(ctx, id.HierarchicalReferences, ua.BrowseDirectionForward, ua.NodeClassAll, true)
	if err != nil {
		return nil, fmt.Errorf("opcua browse %q: %w", parent, err)
	}
	infos := make([]driver.NodeInfo, 0, len(refs))
	var vars []*ua.NodeID // 需要读 DataType 属性的 Variable 节点(批量一次 Read)
	var varIdx []int
	for _, r := range refs {
		if !r.IsForward {
			continue
		}
		nid := ua.NewNodeIDFromExpandedNodeID(r.NodeID)
		if nid == nil {
			continue
		}
		infos = append(infos, driver.NodeInfo{
			NodeID:      nid.String(),
			BrowseName:  qualifiedName(r.BrowseName),
			DisplayName: localizedText(r.DisplayName),
			NodeClass:   normalizeNodeClass(r.NodeClass),
			HasChildren: r.NodeClass == ua.NodeClassObject || r.NodeClass == ua.NodeClassVariable || r.NodeClass == ua.NodeClassMethod,
		})
		if r.NodeClass == ua.NodeClassVariable {
			vars = append(vars, nid)
			varIdx = append(varIdx, len(infos)-1)
		}
	}
	if len(vars) > 0 {
		fillDataTypes(ctx, shared.client, vars, varIdx, infos)
	}
	return infos, nil
}

// fillDataTypes 批量读取 Variable 节点的 DataType 属性(单次 Read 请求),映射为
// 友好短名回填到 NodeInfo.DataType。读取失败仅记日志,不阻断浏览。
func fillDataTypes(ctx context.Context, client *opcua.Client, nodes []*ua.NodeID, indices []int, infos []driver.NodeInfo) {
	nodesToRead := make([]*ua.ReadValueID, len(nodes))
	for i, n := range nodes {
		nodesToRead[i] = &ua.ReadValueID{NodeID: n, AttributeID: ua.AttributeIDDataType}
	}
	resp, err := client.Read(ctx, &ua.ReadRequest{NodesToRead: nodesToRead})
	if err != nil {
		slog.Debug("opcua browse: read data types failed", "err", err)
		return
	}
	for row, dv := range resp.Results {
		if row >= len(indices) || dv == nil || dv.Status != ua.StatusOK || dv.Value == nil {
			continue
		}
		infos[indices[row]].DataType = dataTypeName(dv.Value.NodeID())
	}
}

// dataTypeName 把 DataType 属性返回的 NodeID 映射为网关支持的 dataType 短名;
// 未识别的类型返回原始 NodeID 字符串作为提示(前端不作为可选 dataType 回填)。
func dataTypeName(nid *ua.NodeID) string {
	if nid == nil {
		return ""
	}
	if nid.Namespace() != 0 {
		return nid.String()
	}
	switch nid.IntID() {
	case id.Boolean:
		return "bool"
	case id.Int16:
		return "int16"
	case id.UInt16:
		return "uint16"
	case id.Int32:
		return "int32"
	case id.UInt32:
		return "uint32"
	case id.Int64:
		return "int64"
	case id.Float:
		return "float32"
	case id.Double:
		return "float64"
	case id.String:
		return "string"
	default:
		return nid.String()
	}
}

// normalizeNodeClass 把 ua.NodeClass 枚举转成友好短名(Variable/Object/Method...)——
// NodeClass.String() 返回带 "NodeClass" 前缀的字符串,前端用短名做类型判断(如
// 只有 Variable 节点承载可读写 Value,允许选中回填)。
func normalizeNodeClass(n ua.NodeClass) string {
	return strings.TrimPrefix(n.String(), "NodeClass")
}

func qualifiedName(n *ua.QualifiedName) string {
	if n == nil {
		return ""
	}
	return n.Name
}

func localizedText(n *ua.LocalizedText) string {
	if n == nil {
		return ""
	}
	return n.Text
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

	// dispatchDone 在订阅分发 goroutine 退出(含 defer 里的 sub.Cancel 完成)后关闭;
	// release 据此等待删除订阅请求落地再关 client,避免 DeleteSubscriptions 晚于
	// CloseSession 到达服务端(gopcua server 对已关闭会话的删除请求会 panic)。
	dispatchDone chan struct{}
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
		dispatchDone: make(chan struct{}),
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
	// 先撤销订阅分发 goroutine(subCancel 触发其 defer 里 sub.Cancel),再关 client。
	// 若创建过订阅,须等分发 goroutine 的 DeleteSubscriptions 落地(dispatchDone 关闭)
	// 再 CloseSession,否则删除订阅请求可能晚于会话关闭到达服务端而 panic。
	if shared.subCancel != nil {
		shared.subCancel()
	}
	shared.subMu.Lock()
	hasSub := shared.sub != nil
	shared.subMu.Unlock()
	if hasSub {
		<-shared.dispatchDone
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
		// 整批传输/会话失败:结果无效,返回带原因的 error 让上层以真实错误标记离线
		// (区别于"单点失败用 Quality 表达、error 为 nil"的约定;服务端可达时单点
		// 错误仍走 resp 内逐点 status,不落此分支)。
		slog.Error("opcua read failed", "device", c.deviceID, "err", err)
		return results, fmt.Errorf("opcua read: %w", err)
	}
	applyReadResults(results, indices, resp.Results, points)
	return results, nil
}

func (c *opcuaConn) Close() error {
	return c.driver.release(c.shared)
}

// Probe 探测设备可达性(设备诊断 DC1003):对可解析点位做一次真实读往返。
// OPC UA 单节点状态码(如 BadNodeIdUnknown)在 Read 响应内返回,能收到响应即证明
// server 可达;仅 client.Read 的传输/会话错误判为不可达。
func (c *opcuaConn) Probe(ctx context.Context, points []model.Point) error {
	nodes, _ := planReads(points)
	if len(nodes) == 0 {
		return errors.New("opcua probe: no parseable points")
	}
	_, err := c.shared.client.Read(ctx, &ua.ReadRequest{NodesToRead: nodes})
	return err
}

// Write 下发点位值:解析 NodeID 后构造批量 WriteRequest,按 server 返回 status 标记各点。
// 单点 NodeID 解析失败或类型不匹配标记 Ok=false,不阻断同批。
func (c *opcuaConn) Write(ctx context.Context, items []model.WriteItem) ([]driver.WriteResult, error) {
	results := make([]driver.WriteResult, len(items))
	var nodes []*ua.WriteValue
	var indices []int
	for index, item := range items {
		results[index] = driver.WriteResult{Point: item.Point.Name}
		wv, ok := buildWriteValue(item.Point, item.Value)
		if !ok {
			continue
		}
		nodes = append(nodes, wv)
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

// buildWriteValue 为单个点位构造 WriteValue:NodeID 解析失败或类型不匹配返回 ok=false。
// 关键:DataValue 必须显式设置 EncodingMask=DataValueValue——gopcua 编码时仅按该位
// 序列化 Value,缺省(0)会发出"不含值内容"的空写请求,服务端收到后不写入任何值。
func buildWriteValue(point model.Point, value interface{}) (*ua.WriteValue, bool) {
	nodeID, err := ua.ParseNodeID(point.Address)
	if err != nil {
		return nil, false
	}
	val, ok := encodeValue(value, point.DataType)
	if !ok {
		return nil, false
	}
	variant, err := ua.NewVariant(val)
	if err != nil {
		return nil, false
	}
	return &ua.WriteValue{
		NodeID:      nodeID,
		AttributeID: ua.AttributeIDValue,
		Value: &ua.DataValue{
			EncodingMask: ua.DataValueValue,
			Value:        variant,
		},
	}, true
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
// 未识别的 dataType 一律 ok=false——避免"good + nil 值"等异常数据组合污染北向输出。
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
		return nil, false
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
		go func() {
			defer close(s.dispatchDone)
			s.dispatch(sub, notifyCh)
		}()
	}

	resp, err := s.sub.Monitor(ctx, ua.TimestampsToReturnBoth, items...)
	if err != nil {
		return fmt.Errorf("opcua monitor: %w", err)
	}
	for row := range items {
		handle := handles[row]
		pointIndex := indices[row]
		// Monitor 不因单点失败而报错:结果缺失或 bad status 记日志且不登记 target,
		// 该点后续不会被推送,语义对齐 Read(单点失败不阻断同批)。
		if row >= len(resp.Results) {
			slog.Warn("opcua monitored item missing result", "device", deviceID, "point", points[pointIndex].Name, "requested", len(items), "results", len(resp.Results))
			continue
		}
		if resp.Results[row].StatusCode != ua.StatusOK {
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
			s.handleNotification(notif)
		}
	}
}

// handleNotification 处理一条订阅通知:数据变化分派到 target 回调,状态变更透出日志,
// 其余通知忽略。独立成方法便于单测。
func (s *sharedSession) handleNotification(notif *opcua.PublishNotificationData) {
	if notif.Error != nil {
		slog.Error("opcua subscription notification error", "connection", s.connectionID, "err", notif.Error)
		return
	}
	switch n := notif.Value.(type) {
	case *ua.DataChangeNotification:
		for _, item := range n.MonitoredItems {
			s.subMu.Lock()
			target, ok := s.targets[item.ClientHandle]
			s.subMu.Unlock()
			if !ok {
				continue
			}
			target.onData(notificationToDataPoint(target.deviceID, target.point, item.Value))
		}
	case *ua.StatusChangeNotification:
		// 订阅状态变更(断线转移/重建失败、被服务端删除等)透出日志,供运维感知订阅失效。
		slog.Warn("opcua subscription status changed", "connection", s.connectionID, "status", n.Status, "diag", n.DiagnosticInfo)
	default:
		// 事件/其他通知,本实现只关心数据变化
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
	// SecurityPolicy 安全策略(仅 securityMode != none 生效):"auto"(默认,按端点协商)
	// 或 Basic128Rsa15/Basic256/Basic256Sha256/Aes128Sha256RsaOaep/Aes256Sha256RsaPss。
	SecurityPolicy string `json:"securityPolicy"`
	// ClientCertFile/ClientKeyFile 客户端证书与私钥文件;留空自动生成(工作目录)。
	ClientCertFile string `json:"clientCertFile"`
	ClientKeyFile  string `json:"clientKeyFile"`
	// ServerThumbprint 服务器证书 SHA-1 指纹(40 位 hex);设置后建连前校验(信任锚点)。
	ServerThumbprint string `json:"serverThumbprint"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	Timeout          string `json:"timeout"`
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

	modeSecurityNone           = "none"
	modeSecuritySign           = "sign"
	modeSecuritySignAndEncrypt = "signAndEncrypt"

	defaultPublishInterval = 1 * time.Second
	defaultQueueSize       = 10

	// opc.tcp URL 未写端口时的缺省值,用于端点归一化
	defaultEndpointPort = "4840"

	// 客户端自动生成证书/私钥的默认文件名(网关工作目录)
	defaultClientCertFile = "opcua-client-cert.pem"
	defaultClientKeyFile  = "opcua-client-key.pem"
	clientCertValidFor    = 10 * 365 * 24 * time.Hour
)

// supportedSecurityPolicies 是允许配置的安全策略(短名;auto 表示按端点协商)。
var supportedSecurityPolicies = []string{"Basic128Rsa15", "Basic256", "Basic256Sha256", "Aes128Sha256RsaOaep", "Aes256Sha256RsaPss"}

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
	switch cfg.SecurityMode {
	case modeSecurityNone, modeSecuritySign, modeSecuritySignAndEncrypt:
	default:
		return connConfig{}, fmt.Errorf("opcua securityMode %q not supported (only %q, %q or %q)", cfg.SecurityMode, modeSecurityNone, modeSecuritySign, modeSecuritySignAndEncrypt)
	}
	if cfg.SecurityMode != modeSecurityNone {
		// 安全策略校验(空/auto 允许)
		if p := strings.TrimSpace(cfg.SecurityPolicy); p != "" && p != "auto" {
			ok := false
			for _, supported := range supportedSecurityPolicies {
				if strings.EqualFold(p, supported) {
					cfg.SecurityPolicy = supported
					ok = true
					break
				}
			}
			if !ok {
				return connConfig{}, fmt.Errorf("opcua securityPolicy %q not supported (auto/%v)", cfg.SecurityPolicy, supportedSecurityPolicies)
			}
		}
		// 客户端证书/私钥须成对
		if (cfg.ClientCertFile == "") != (cfg.ClientKeyFile == "") {
			return connConfig{}, errors.New("opcua clientCertFile and clientKeyFile must be set together")
		}
		// 服务器指纹须为 40 位 hex(SHA-1)
		if s := strings.TrimSpace(cfg.ServerThumbprint); s != "" {
			if len(s) != 40 {
				return connConfig{}, fmt.Errorf("opcua serverThumbprint must be 40 hex chars, got %d", len(s))
			}
			if _, err := hex.DecodeString(s); err != nil {
				return connConfig{}, fmt.Errorf("opcua serverThumbprint must be hex: %w", err)
			}
			cfg.ServerThumbprint = strings.ToLower(s)
		}
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
// securityMode=none 走原路径;sign/signAndEncrypt 走安全建连(见 selectEndpoint)。
func buildClient(ctx context.Context, cfg connConfig) (*opcua.Client, error) {
	timeout, _ := time.ParseDuration(cfg.Timeout)
	stateCh := make(chan opcua.ConnState, 8)
	opts := []opcua.Option{
		opcua.RequestTimeout(timeout),
		opcua.AutoReconnect(true),
		opcua.ReconnectInterval(opcuaReconnectInterval),
		opcua.StateChangedCh(stateCh),
	}
	if cfg.SecurityMode == modeSecurityNone {
		opts = append(opts, opcua.SecurityMode(ua.MessageSecurityModeNone))
	} else {
		// 安全建连:客户端证书 → 端点发现匹配 → 服务器指纹校验 → SecurityFromEndpoint
		cert, key, err := ensureClientCert(cfg.ClientCertFile, cfg.ClientKeyFile)
		if err != nil {
			return nil, err
		}
		ep, err := selectEndpoint(ctx, cfg, cert, key)
		if err != nil {
			return nil, err
		}
		authType := ua.UserTokenTypeAnonymous
		if cfg.Username != "" {
			authType = ua.UserTokenTypeUserName
		}
		opts = append(opts, opcua.Certificate(cert), opcua.PrivateKey(key), opcua.SecurityFromEndpoint(ep, authType))
		slog.Info("opcua secure connection", "endpoint", cfg.Endpoint, "mode", cfg.SecurityMode, "policy", ep.SecurityPolicyURI, "securityLevel", ep.SecurityLevel)
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

// selectEndpoint 经 GetEndpoints 发现服务器端点,按 securityMode/securityPolicy 匹配
// (auto 选同 mode 下 SecurityLevel 最高),并对 ep 内嵌的服务器证书做指纹校验。
// 注:gopcua 不做证书链/信任校验,指纹校验是网关补的信任锚点(防中间人/防证书被换)。
func selectEndpoint(ctx context.Context, cfg connConfig, cert []byte, key *rsa.PrivateKey) (*ua.EndpointDescription, error) {
	endpoints, err := opcua.GetEndpoints(ctx, cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("opcua security: get endpoints %q: %w", cfg.Endpoint, err)
	}
	mode := securityModeEnum(cfg.SecurityMode)
	wantPolicy := strings.TrimSpace(cfg.SecurityPolicy)
	var best *ua.EndpointDescription
	for _, ep := range endpoints {
		if ep.SecurityMode != mode {
			continue
		}
		if wantPolicy != "" && wantPolicy != "auto" && ep.SecurityPolicyURI != ua.FormatSecurityPolicyURI(wantPolicy) {
			continue
		}
		if best == nil || ep.SecurityLevel >= best.SecurityLevel {
			best = ep
		}
	}
	if best == nil {
		return nil, fmt.Errorf("opcua security: no endpoint for mode %s policy %q; server offers: %s", mode, wantPolicy, endpointSummary(endpoints))
	}
	// 服务器证书指纹校验(信任锚点)
	if s := cfg.ServerThumbprint; s != "" {
		got := hex.EncodeToString(uapolicy.Thumbprint(best.ServerCertificate))
		if !strings.EqualFold(got, s) {
			return nil, fmt.Errorf("opcua security: server certificate thumbprint mismatch (server %s, configured %s) — 可能被中间人篡改或配置错误", got, s)
		}
		slog.Info("opcua server certificate thumbprint verified", "endpoint", cfg.Endpoint)
	}
	return best, nil
}

func securityModeEnum(s string) ua.MessageSecurityMode {
	switch {
	case strings.EqualFold(s, modeSecuritySign):
		return ua.MessageSecurityModeSign
	case strings.EqualFold(s, modeSecuritySignAndEncrypt):
		return ua.MessageSecurityModeSignAndEncrypt
	default:
		return ua.MessageSecurityModeNone
	}
}

func endpointSummary(endpoints []*ua.EndpointDescription) string {
	parts := make([]string, 0, len(endpoints))
	for _, ep := range endpoints {
		parts = append(parts, fmt.Sprintf("%s/%s(level=%d)", ep.SecurityPolicyURI, ep.SecurityMode, ep.SecurityLevel))
	}
	return strings.Join(parts, ", ")
}

// ensureClientCert 返回客户端证书(DER)与 RSA 私钥,供安全建连使用。
// 配置了 clientCertFile/clientKeyFile 则加载;留空则自动生成自签证书并持久化到
// 网关工作目录(opcua-client-cert.pem / opcua-client-key.pem),仅生成一次,后续复用。
func ensureClientCert(certFile, keyFile string) ([]byte, *rsa.PrivateKey, error) {
	useDefault := certFile == "" && keyFile == ""
	if useDefault {
		certFile, keyFile = defaultClientCertFile, defaultClientKeyFile
	}
	if certFile == "" || keyFile == "" {
		return nil, nil, errors.New("opcua security: clientCertFile and clientKeyFile must be set together")
	}
	certPEM, certErr := os.ReadFile(certFile)
	keyPEM, keyErr := os.ReadFile(keyFile)
	if certErr == nil && keyErr == nil {
		return parseClientCert(certPEM, keyPEM)
	}
	if useDefault {
		// 自动生成(默认路径缺失即生成;用户显式路径缺失则报错,避免覆盖)
		certPEM, keyPEM, err := generateClientCert()
		if err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
			return nil, nil, fmt.Errorf("opcua security: write client cert %s: %w", certFile, err)
		}
		if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
			return nil, nil, fmt.Errorf("opcua security: write client key %s: %w", keyFile, err)
		}
		slog.Info("opcua client certificate generated", "cert", certFile, "key", keyFile)
		return parseClientCert(certPEM, keyPEM)
	}
	return nil, nil, fmt.Errorf("opcua security: load client cert/key: cert=%v key=%v", certErr, keyErr)
}

// parseClientCert 从 PEM 证书与私钥解析出 DER 证书与 RSA 私钥(gopcua 需要 DER)。
func parseClientCert(certPEM, keyPEM []byte) ([]byte, *rsa.PrivateKey, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("opcua security: invalid client certificate PEM")
	}
	key, err := parsePrivateKeyPEM(keyPEM)
	if err != nil {
		return nil, nil, err
	}
	return block.Bytes, key, nil
}

// parsePrivateKeyPEM 解析 PKCS#1(RSA PRIVATE KEY)与 PKCS#8(PRIVATE KEY)私钥 PEM。
func parsePrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("opcua security: invalid private key PEM")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("opcua security: private key is not RSA")
		}
		return key, nil
	default:
		return nil, fmt.Errorf("opcua security: unsupported private key PEM type %q", block.Type)
	}
}

// generateClientCert 生成 OPC UA 客户端自签名证书(含 ApplicationURI、ClientAuth 用法)。
func generateClientCert() (certPEM, keyPEM []byte, err error) {
	hostname, _ := os.Hostname()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("opcua security: generate key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("opcua security: generate serial: %w", err)
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"iot-gateway"}, CommonName: "iot-gateway opcua client"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(clientCertValidFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment | x509.KeyUsageContentCommitment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{hostname},
		URIs:                  []*url.URL{{Scheme: "urn", Opaque: "iot-gateway:" + hostname}},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(hostname); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		tmpl.DNSNames = nil
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("opcua security: create certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustMarshalPKCS8(priv)})
	return certPEM, keyPEM, nil
}

func mustMarshalPKCS8(key *rsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		// 生成刚创建的有效 RSA 私钥,不可能失败
		panic(err)
	}
	return der
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
