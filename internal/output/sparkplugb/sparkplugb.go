// Package sparkplugb 实现 Sparkplug B(工业 MQTT 事实标准)北向输出。
// 网关作为边缘节点(edge node),按 spBv1.0 topic 命名空间发布:
//
//	STATE(在线/离线,retained)、NBIRTH/DBIRTH(节点/设备出生,retained,声明点位与别名)、
//	DDATA(设备数据,别名压缩)、DDEATH/NDEATH(设备/节点死亡,retained,空 payload)。
//
// 复用 internal/output/mqttutil 的连接韧性(非阻塞建连 + 指数退避 + 有界等待),
// 并实现 output.DeviceNotifier(设备上线/离线 → DBIRTH/DDEATH)与断网补传。
// 设计见 docs/sparkplugb.md。
package sparkplugb

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/output/mqttutil"
)

const (
	defaultQoS        byte = 1
	disconnectQuiesce      = 250 * time.Millisecond
	defaultGroupID         = "iot-gateway"
)

// Config 是 Sparkplug B 输出的配置(存 SQLite,经 Web UI 配置)。
type Config struct {
	Broker   string `json:"broker"`
	ClientID string `json:"clientId"`
	Username string `json:"username"`
	Password string `json:"password"`
	QoS      byte   `json:"qos"`
	// GroupID / EdgeNodeID 构成 topic 前两段;EdgeNodeID 留空默认用网关 ID。
	GroupID    string `json:"groupId"`
	EdgeNodeID string `json:"edgeNodeId"`
	// DeviceNamePrefix 可给设备 topic 段加前缀(默认不加)。
	DeviceNamePrefix string `json:"deviceNamePrefix"`
}

// init 注册 Sparkplug B 输出类型。
func init() {
	output.Register(output.Descriptor{
		Type:  "sparkplugb",
		Label: "Sparkplug B",
		Schema: []output.Field{
			{Name: "broker", Label: "Broker 地址", Type: output.FieldString, Required: true, Placeholder: "tcp://127.0.0.1:1883"},
			{Name: "clientId", Label: "Client ID", Type: output.FieldString, Placeholder: "iot-gateway-spb"},
			{Name: "username", Label: "用户名", Type: output.FieldString},
			{Name: "password", Label: "密码", Type: output.FieldPassword},
			{Name: "qos", Label: "QoS", Type: output.FieldInt, Default: 1},
			{Name: "groupId", Label: "Group ID", Type: output.FieldString, Default: "iot-gateway", Placeholder: "iot-gateway"},
			{Name: "edgeNodeId", Label: "Edge Node ID", Type: output.FieldString, Placeholder: "默认=网关 ID",
				Hint: "留空默认使用网关 ID,作为 spBv1.0/{group}/{type}/{edgeNode} 的节点段"},
			{Name: "deviceNamePrefix", Label: "设备名前缀", Type: output.FieldString, Placeholder: "可选"},
		},
	}, func(bc output.BuildContext, raw json.RawMessage) (output.Output, error) {
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("sparkplugb config: %w", err)
		}
		return New(cfg, bc)
	})
}

// metricMeta 是点位在 edge node 命名空间内的出生声明:别名 + Sparkplug datatype。
type metricMeta struct {
	alias    uint64
	datatype uint32
}

type sparkplugOutput struct {
	client pahomqtt.Client
	qos    byte
	group  string
	edge   string
	prefix string

	store    output.StoreAccessor
	latest   output.LatestValuesFunc
	outputID string
	backfill output.BackfillSink

	// 生命周期状态(birth/data/death 并发访问)。
	mu        sync.Mutex
	seq       uint32
	bornAll   bool
	born      map[string]bool       // deviceID -> 已发 DBIRTH
	meta      map[string]metricMeta // "deviceID/point" -> 出生声明
	nextAlias uint64
	birthMu   sync.Mutex // 串行化 birth 序列(重连并发时防止重复出生)

	output.SendStats
}

// New 构造 Sparkplug B 输出。broker 不可达时非阻塞,由 ConnectRetry 后台自动重连,
// 重连成功经 OnConnect 触发完整出生序列(STATE + NBIRTH + 各设备 DBIRTH)。
func New(cfg Config, bc output.BuildContext) (output.Output, error) {
	if cfg.QoS == 0 {
		cfg.QoS = defaultQoS
	}
	group := cfg.GroupID
	if group == "" {
		group = defaultGroupID
	}
	edge := cfg.EdgeNodeID
	if edge == "" {
		edge = bc.GatewayID
	}
	if edge == "" {
		edge = defaultGroupID
	}

	o := &sparkplugOutput{
		qos:      cfg.QoS,
		group:    group,
		edge:     topicSafe(edge),
		prefix:   cfg.DeviceNamePrefix,
		store:    bc.Store,
		latest:   bc.LatestValues,
		outputID: bc.OutputID,
		backfill: bc.Backfill,
		born:     make(map[string]bool),
		meta:     make(map[string]metricMeta),
	}

	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.Broker)
	opts.SetClientID(cfg.ClientID)
	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}
	// OnConnect 在 paho 回调线程执行,不得阻塞:起 goroutine 做完整出生序列。
	opts.SetOnConnectHandler(func(_ pahomqtt.Client) {
		go o.birthSequence()
	})
	mqttutil.ApplyResilience(opts)
	client := pahomqtt.NewClient(opts)
	// 先赋值 o.client 再连接:避免 Connect() 同步触发 OnConnect 时 nil 解引用。
	o.client = client
	mqttutil.ConnectNonBlocking(client, "sparkplugb")
	return o, nil
}

// ---- 生命周期 ----

// birthSequence 完整出生序列:STATE online + NBIRTH + 各启用设备 DBIRTH。
// 由 OnConnect 触发(每次连上/重连都重新出生,retained 覆盖旧声明)。
func (o *sparkplugOutput) birthSequence() {
	o.birthMu.Lock()
	defer o.birthMu.Unlock()

	o.send(o.stateTopic(), []byte("ONLINE"), true)

	devices, err := o.store.ListDevices()
	if err != nil {
		slog.Error("sparkplugb list devices for birth failed", "err", err)
		devices = nil
	}
	seq := o.nextSeq()
	payload := encodePayload(seq, []metric{
		{name: "deviceCount", datatype: DataTypeInt32, timestamp: msNow(), value: int32(len(devices))},
	}, time.Now())
	if err := o.send(o.nodeTopic("NBIRTH"), payload, true); err != nil {
		slog.Error("sparkplugb NBIRTH failed", "err", err)
	}

	o.mu.Lock()
	o.bornAll = true
	o.mu.Unlock()

	for _, d := range devices {
		if d.Enabled && len(d.Points) > 0 {
			o.sendDeviceBirth(d)
		}
	}
}

// sendDeviceBirth 发布某设备的 DBIRTH(retained),声明各点位 name/alias/datatype,
// 并尽量携带当前采集值(无值则 is_null)。随后该设备数据可用别名压缩发送。
func (o *sparkplugOutput) sendDeviceBirth(d model.Device) {
	var values map[string]interface{}
	if o.latest != nil {
		values = o.latest(d.ID)
	}

	o.mu.Lock()
	if o.born[d.ID] {
		o.mu.Unlock()
		return
	}
	pts := make([]struct {
		name  string
		meta  metricMeta
		value interface{}
		null  bool
	}, 0, len(d.Points))
	for _, p := range d.Points {
		m, ok := o.meta[metaKey(d.ID, p.Name)]
		if !ok {
			o.nextAlias++
			m = metricMeta{alias: o.nextAlias, datatype: sparkplugType(p.DataType)}
			o.meta[metaKey(d.ID, p.Name)] = m
		}
		v, has := values[p.Name]
		pts = append(pts, struct {
			name  string
			meta  metricMeta
			value interface{}
			null  bool
		}{name: p.Name, meta: m, value: v, null: !has})
	}
	o.mu.Unlock()

	ts := msNow()
	metrics := make([]metric, 0, len(pts))
	for _, pm := range pts {
		metrics = append(metrics, metric{
			name: pm.name, alias: pm.meta.alias, datatype: pm.meta.datatype,
			timestamp: ts, value: pm.value, isNull: pm.null,
		})
	}
	seq := o.nextSeq()
	payload := encodePayload(seq, metrics, time.Now())
	if err := o.send(o.deviceTopic("DBIRTH", d.ID), payload, true); err != nil {
		slog.Error("sparkplugb DBIRTH failed", "device", d.ID, "err", err)
		return
	}
	o.mu.Lock()
	o.born[d.ID] = true
	o.mu.Unlock()
}

// DeviceOnline 设备上线 → 若节点已出生且该设备未出生,发 DBIRTH。
func (o *sparkplugOutput) DeviceOnline(deviceID string) {
	o.mu.Lock()
	if !o.bornAll || o.born[deviceID] {
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()
	d, err := o.store.GetDevice(deviceID)
	if err != nil {
		slog.Error("sparkplugb get device for birth failed", "device", deviceID, "err", err)
		return
	}
	o.sendDeviceBirth(d)
}

// DeviceOffline 设备离线 → 发 DDEATH(retained,空 payload)。
func (o *sparkplugOutput) DeviceOffline(deviceID string) {
	o.mu.Lock()
	delete(o.born, deviceID)
	o.mu.Unlock()
	if err := o.send(o.deviceTopic("DDEATH", deviceID), nil, true); err != nil {
		slog.Error("sparkplugb DDEATH failed", "device", deviceID, "err", err)
	}
}

// ---- 数据 ----

// Publish 上送设备点位数据:已出生设备用别名压缩(DDATA),未知点位退化 name 直发。
// 未连接/未出生时落库补传,连上并完成出生后由 Manager 重放。
func (o *sparkplugOutput) Publish(dp model.DataPoint) error {
	o.mu.Lock()
	if !o.bornAll || !o.born[dp.DeviceID] {
		o.mu.Unlock()
		return o.saveBackfill(dp)
	}
	m, known := o.meta[metaKey(dp.DeviceID, dp.Point)]
	o.mu.Unlock()

	if !known {
		// 配置变更新增点位尚未 re-birth:退化 name 直发,不丢数据。
		mtr := metric{name: dp.Point, datatype: datatypeFromValue(dp.Value), timestamp: tsOf(dp.Timestamp), value: dp.Value, isNull: dp.Value == nil}
		return o.send(o.deviceTopic("DDATA", dp.DeviceID), encodePayload(o.nextSeq(), []metric{mtr}, time.Now()), false)
	}
	mtr := metric{alias: m.alias, datatype: m.datatype, timestamp: tsOf(dp.Timestamp), value: dp.Value, isNull: dp.Value == nil}
	return o.send(o.deviceTopic("DDATA", dp.DeviceID), encodePayload(o.nextSeq(), []metric{mtr}, time.Now()), false)
}

// Close 优雅关闭:发 NDEATH + STATE OFFLINE(retained),再断开连接。
func (o *sparkplugOutput) Close() error {
	if o.client != nil && o.client.IsConnected() {
		o.send(o.nodeTopic("NDEATH"), nil, true)
		o.send(o.stateTopic(), []byte("OFFLINE"), true)
		o.client.Disconnect(uint(disconnectQuiesce / time.Millisecond))
	}
	return nil
}

// BackfillHealthy 实现 output.BackfillHealthy:仅在已连接且完成出生后允许重放补传队列。
func (o *sparkplugOutput) BackfillHealthy() bool {
	if o.client == nil || !o.client.IsConnected() {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.bornAll
}

// RuntimeStatus 实现 output.StatusProvider。
func (o *sparkplugOutput) RuntimeStatus() output.RuntimeStatus {
	sent, lastSentAt, lastErr, lastErrAt := o.SendStats.Snapshot()
	return output.RuntimeStatus{
		Connected:      o.client != nil && o.client.IsConnected(),
		ConnectionOpen: o.client != nil && o.client.IsConnectionOpen(),
		Sent:           sent,
		LastSentAt:     lastSentAt,
		LastError:      lastErr,
		LastErrorAt:    lastErrAt,
	}
}

// ---- 工具 ----

// send 有界等待发布一条消息,并更新上送统计。
func (o *sparkplugOutput) send(topic string, payload []byte, retained bool) error {
	if o.client == nil {
		return fmt.Errorf("client not initialized")
	}
	err := mqttutil.WaitToken(o.client.Publish(topic, o.qos, retained, payload), mqttutil.PublishTimeout)
	if err != nil {
		o.SendStats.Failure(err)
		return err
	}
	o.SendStats.Success(time.Now())
	return nil
}

func (o *sparkplugOutput) nextSeq() uint32 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	return o.seq
}

func (o *sparkplugOutput) stateTopic() string {
	return fmt.Sprintf("spBv1.0/%s/STATE/%s", o.group, o.edge)
}

func (o *sparkplugOutput) nodeTopic(msgType string) string {
	return fmt.Sprintf("spBv1.0/%s/%s/%s", o.group, msgType, o.edge)
}

func (o *sparkplugOutput) deviceTopic(msgType, deviceID string) string {
	return fmt.Sprintf("spBv1.0/%s/%s/%s/%s", o.group, msgType, o.edge, topicSafe(o.prefix+deviceID))
}

// saveBackfill 把无法即时送出的数据点持久化到补传队列(断网续传)。
func (o *sparkplugOutput) saveBackfill(dp model.DataPoint) error {
	if o.backfill == nil {
		return nil
	}
	if err := o.backfill.Save(o.outputID, []model.DataPoint{dp}); err != nil {
		slog.Error("sparkplugb backfill save failed", "err", err)
		return err
	}
	return nil
}

func metaKey(deviceID, point string) string {
	return deviceID + "/" + point
}

func msNow() uint64 {
	return uint64(time.Now().UnixMilli())
}

func tsOf(t time.Time) uint64 {
	if t.IsZero() {
		return msNow()
	}
	return uint64(t.UnixMilli())
}

// topicSafe 把标识符中的 MQTT topic 保留字符替换为下划线,防止 topic 段被破坏。
func topicSafe(s string) string {
	r := strings.NewReplacer("/", "_", "+", "_", "#", "_", " ", "_")
	return r.Replace(s)
}

// sparkplugType 把内部 model.DataType 映射为 Sparkplug B datatype。
func sparkplugType(dt model.DataType) uint32 {
	switch dt {
	case model.DataTypeBool:
		return DataTypeBoolean
	case model.DataTypeInt16:
		return DataTypeInt16
	case model.DataTypeUInt16:
		return DataTypeUInt16
	case model.DataTypeInt32:
		return DataTypeInt32
	case model.DataTypeUInt32:
		return DataTypeUInt32
	case model.DataTypeInt64:
		return DataTypeInt64
	case model.DataTypeFloat:
		return DataTypeFloat
	case model.DataTypeDouble:
		return DataTypeDouble
	case model.DataTypeString:
		return DataTypeString
	}
	return DataTypeUnknown
}

// datatypeFromValue 从 Go 值类型推断 Sparkplug datatype(未知点位退化直发用)。
func datatypeFromValue(v interface{}) uint32 {
	switch v.(type) {
	case bool:
		return DataTypeBoolean
	case int8:
		return DataTypeInt8
	case int16:
		return DataTypeInt16
	case int32:
		return DataTypeInt32
	case int64:
		return DataTypeInt64
	case int:
		return DataTypeInt64
	case uint8:
		return DataTypeUInt8
	case uint16:
		return DataTypeUInt16
	case uint32:
		return DataTypeUInt32
	case uint64:
		return DataTypeUInt64
	case uint:
		return DataTypeUInt64
	case float32:
		return DataTypeFloat
	case float64:
		return DataTypeDouble
	case string:
		return DataTypeString
	}
	return DataTypeUnknown
}
