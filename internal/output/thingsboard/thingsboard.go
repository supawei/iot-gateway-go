// Package thingsboard 实现 ThingsBoard 平台对接(北向输出插件)。
// 采用 ThingsBoard MQTT Gateway 模式:网关作为一个"网关设备",用一个 MQTT 连接
// 代表 N 个子设备。每个 DataPoint 映射为子设备的一条遥测;Quality 映射为客户端属性。
// 同时实现 output.DeviceNotifier(设备上线/离线 → connect/disconnect)与下行
// (共享属性 → 设备写),详见 docs/thingsboard.md。
package thingsboard

import (
	"context"
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
	defaultQoS        = 1
	disconnectQuiesce = 250 * time.Millisecond

	defaultFlushInterval = 200 * time.Millisecond
	writeTimeout         = 5 * time.Second
	writeQueueSize       = 64

	// maxPendingPoints 待上报遥测缓冲的全局上限:断连期间 flush 仍持续灌入时,
	// 超过上限丢弃新点并告警,避免内存无界增长(与 smardaten 输出同策略)。
	maxPendingPoints = 8192
)

// ThingsBoard MQTT Gateway 的 topic。
const (
	topicConnect    = "v1/gateway/connect"
	topicDisconnect = "v1/gateway/disconnect"
	topicTelemetry  = "v1/gateway/telemetry"
	topicAttributes = "v1/gateway/attributes"
	topicRPC        = "v1/gateway/rpc"
)

// Config 是 ThingsBoard 输出的配置(存 SQLite,经 Web UI 配置)。
type Config struct {
	Broker           string `json:"broker"`           // MQTT broker,如 tcp://tb.example.com:1883
	AccessToken      string `json:"accessToken"`      // 网关设备 Access Token(作为 MQTT 用户名)
	ClientID         string `json:"clientId"`         // MQTT client id
	Username         string `json:"username"`         // 可选,覆盖默认的 AccessToken 用户名
	Password         string `json:"password"`         // 可选
	QoS              byte   `json:"qos"`              // 默认 1
	DeviceNamePrefix string `json:"deviceNamePrefix"` // 子设备名前缀,默认空
	ReportQuality    *bool  `json:"reportQuality"`    // 是否上报 quality 属性,默认 true
	FlushInterval    string `json:"flushInterval"`    // 微批 flush 间隔,如 "200ms",默认 200ms
}

// init 注册 ThingsBoard 输出类型:声明配置 schema 并绑定构造器。
func init() {
	output.Register(output.Descriptor{
		Type:  "thingsboard",
		Label: "ThingsBoard",
		Schema: []output.Field{
			{Name: "broker", Label: "Broker 地址", Type: output.FieldString, Required: true, Placeholder: "tcp://tb.example.com:1883"},
			{Name: "accessToken", Label: "Access Token", Type: output.FieldPassword, Required: true, Hint: "网关设备 Access Token(作为 MQTT 用户名)"},
			{Name: "clientId", Label: "Client ID", Type: output.FieldString, Placeholder: "iot-gateway-tb"},
			{Name: "username", Label: "用户名", Type: output.FieldString, Hint: "可选,覆盖默认的 AccessToken 用户名"},
			{Name: "password", Label: "密码", Type: output.FieldPassword},
			{Name: "qos", Label: "QoS", Type: output.FieldInt, Default: 1},
			{Name: "deviceNamePrefix", Label: "子设备名前缀", Type: output.FieldString, Hint: "默认空"},
			{Name: "reportQuality", Label: "上报质量属性", Type: output.FieldBool, Default: true},
			{Name: "flushInterval", Label: "Flush 间隔", Type: output.FieldString, Default: "200ms", Hint: "微批聚合上报间隔,如 200ms"},
		},
	}, func(bc output.BuildContext, raw json.RawMessage) (output.Output, error) {
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("thingsboard config: %w", err)
		}
		return New(cfg, WriteFunc(bc.Write), bc.OutputID, bc.Backfill)
	})
}

// WriteFunc 是下行写回调:把设备点位的值写回设备(由 main 注入,最终落到驱动 Writer)。
type WriteFunc func(ctx context.Context, deviceID string, point string, value interface{}) error

type thingsboardOutput struct {
	client        pahomqtt.Client
	prefix        string
	qos           byte
	reportQuality bool
	flushInterval time.Duration

	write WriteFunc // 下行写回调

	// 断网补传(见 docs/offline-backfill-design.md):outputID 为补传队列隔离键,
	// backfill 为持久化队列(未接线时 nil,退化为"丢弃 + 告警")。
	outputID string
	backfill output.BackfillSink

	// 以下由 Publish / DeviceOnline / DeviceOffline / 下行 handler 与 flush 并发访问。
	mu           sync.Mutex
	pending      map[string][]model.DataPoint // 设备名 -> 待上报遥测点
	pendingCount int                          // 待上报遥测点总数(上限 maxPendingPoints)
	qualities    map[string]model.Quality     // 设备名 -> 最近质量
	connects     map[string]bool              // 待发送 connect 的设备名
	disconnects  map[string]bool              // 待发送 disconnect 的设备名

	// connected 仅在 flush 中访问(flusher goroutine 串行,Close 等待其退出后再 flush)。
	connected map[string]bool

	writeCh chan writeRequest // 下行写请求队列(非阻塞投递,不关闭)
	done    chan struct{}
	wg      sync.WaitGroup

	// 实际上送统计(见 docs/output-status-design.md)
	output.SendStats
}

type writeRequest struct {
	deviceID string
	point    string
	value    interface{}
	rpcID    int64 // 非 0 时写完后发 RPC 应答(共享属性下行为 0)
}

func New(cfg Config, write WriteFunc, outputID string, backfill output.BackfillSink) (output.Output, error) {
	if cfg.QoS == 0 {
		cfg.QoS = defaultQoS
	}
	reportQuality := true
	if cfg.ReportQuality != nil {
		reportQuality = *cfg.ReportQuality
	}
	flushInterval := defaultFlushInterval
	if cfg.FlushInterval != "" {
		d, err := time.ParseDuration(cfg.FlushInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid thingsboard flushInterval %q: %w", cfg.FlushInterval, err)
		}
		flushInterval = d
	}

	o := &thingsboardOutput{
		prefix:        cfg.DeviceNamePrefix,
		qos:           cfg.QoS,
		reportQuality: reportQuality,
		flushInterval: flushInterval,
		write:         write,
		outputID:      outputID,
		backfill:      backfill,
		pending:       make(map[string][]model.DataPoint),
		qualities:     make(map[string]model.Quality),
		connects:      make(map[string]bool),
		disconnects:   make(map[string]bool),
		connected:     make(map[string]bool),
		writeCh:       make(chan writeRequest, writeQueueSize),
		done:          make(chan struct{}),
	}

	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.Broker)
	opts.SetClientID(cfg.ClientID)
	username := cfg.Username
	if username == "" {
		username = cfg.AccessToken // ThingsBoard 以 Access Token 作为 MQTT 用户名
	}
	opts.SetUsername(username)
	opts.SetPassword(cfg.Password)
	// 下行订阅迁入 OnConnect:连接(含重连)建立后重新订阅,非阻塞构造下 New 里订阅必失败。
	opts.SetOnConnectHandler(func(pahomqtt.Client) {
		go func() {
			if err := o.onConnectSubscribe(); err != nil {
				slog.Error("thingsboard resubscribe after connect failed", "err", err)
			}
		}()
	})
	mqttutil.ApplyResilience(opts)

	// 先赋值 client 再连接:paho 可能在 Connect 内同步触发 OnConnect,避免 nil 解引用。
	o.client = pahomqtt.NewClient(opts)
	mqttutil.ConnectNonBlocking(o.client, "thingsboard")

	o.wg.Add(2)
	go o.runFlusher()
	go o.runWriter()
	return o, nil
}

// onConnectSubscribe 订阅下行共享属性与 RPC 命令 topic(连接/重连后调用)。
func (o *thingsboardOutput) onConnectSubscribe() error {
	if err := mqttutil.WaitToken(o.client.Subscribe(topicAttributes, o.qos, o.handleAttributes), mqttutil.PublishTimeout); err != nil {
		return fmt.Errorf("subscribe attributes: %w", err)
	}
	if err := mqttutil.WaitToken(o.client.Subscribe(topicRPC, o.qos, o.handleRPC), mqttutil.PublishTimeout); err != nil {
		return fmt.Errorf("subscribe rpc: %w", err)
	}
	return nil
}

func (o *thingsboardOutput) deviceName(deviceID string) string {
	return o.prefix + deviceID
}

// deviceID 反向映射:设备名去掉前缀还原为网关 DeviceID。
func (o *thingsboardOutput) deviceID(name string) string {
	return strings.TrimPrefix(name, o.prefix)
}

// Publish 把 DataPoint 缓冲进对应设备的待发队列,由 flusher 定时聚合上报。
// 有值才入遥测队列;Quality 作为状态属性记录(同一设备取最近值)。
// 缓冲达到上限时不再丢弃:启用补传时经持久化队列保存(断网续传),否则告警丢弃。
func (o *thingsboardOutput) Publish(dp model.DataPoint) error {
	var toSave []model.DataPoint
	o.mu.Lock()
	name := o.deviceName(dp.DeviceID)
	if dp.Value != nil {
		if o.pendingCount >= maxPendingPoints {
			toSave = append(toSave, dp)
		} else {
			o.pending[name] = append(o.pending[name], dp)
			o.pendingCount++
		}
	}
	if o.reportQuality {
		o.qualities[name] = dp.Quality
	}
	o.mu.Unlock()
	if len(toSave) > 0 {
		o.saveBackfill(toSave)
	}
	return nil
}

// DeviceOnline 记录设备上线意图,由 flusher 在下一轮 flush 发 v1/gateway/connect。
func (o *thingsboardOutput) DeviceOnline(deviceID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	name := o.deviceName(deviceID)
	o.connects[name] = true
	delete(o.disconnects, name)
}

// DeviceOffline 记录设备离线意图,由 flusher 在下一轮 flush 发 v1/gateway/disconnect。
func (o *thingsboardOutput) DeviceOffline(deviceID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	name := o.deviceName(deviceID)
	o.disconnects[name] = true
	delete(o.connects, name)
}

// handleAttributes 是 MQTT 下行 handler:共享属性更新(server→gateway)带 device/data 包装。
func (o *thingsboardOutput) handleAttributes(_ pahomqtt.Client, msg pahomqtt.Message) {
	o.handleDownlink(msg.Payload())
}

// handleRPC 是 MQTT RPC handler:RPC 请求带 data.method,应答不带(据此区分,避免回环)。
func (o *thingsboardOutput) handleRPC(_ pahomqtt.Client, msg pahomqtt.Message) {
	o.handleRPCDownlink(msg.Payload())
}

// handleRPCDownlink 解析 RPC 请求:约定 method="write"、params={"point":..., "value":...},
// 映射为设备写并登记 RPC id 以便写完后应答。未知方法/缺 point 直接应答错误。
func (o *thingsboardOutput) handleRPCDownlink(payload []byte) {
	var msg struct {
		Device string `json:"device"`
		Data   struct {
			ID     int64                  `json:"id"`
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		slog.Error("thingsboard rpc parse failed", "err", err)
		return
	}
	if msg.Device == "" || msg.Data.Method == "" {
		return // 非 RPC 请求(可能是应答回环)
	}
	if msg.Data.Method != "write" {
		o.replyRPC(msg.Device, msg.Data.ID, fmt.Errorf("unknown rpc method %q", msg.Data.Method))
		return
	}
	point, _ := msg.Data.Params["point"].(string)
	if point == "" {
		o.replyRPC(msg.Device, msg.Data.ID, fmt.Errorf("rpc write missing point"))
		return
	}
	o.enqueueWrite(writeRequest{
		deviceID: o.deviceID(msg.Device),
		point:    point,
		value:    msg.Data.Params["value"],
		rpcID:    msg.Data.ID,
	})
}

// handleDownlink 解析下行消息;对共享属性更新,把每个 key 作为点位、value 作为值,
// 投递到写队列。上行客户端属性(无 device/data 包装)会被忽略,避免回环。
func (o *thingsboardOutput) handleDownlink(payload []byte) {
	var msg struct {
		Device string                 `json:"device"`
		Data   map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		slog.Error("thingsboard downlink parse failed", "err", err)
		return
	}
	if msg.Device == "" || len(msg.Data) == 0 {
		return
	}
	deviceID := o.deviceID(msg.Device)
	for point, value := range msg.Data {
		o.enqueueWrite(writeRequest{deviceID: deviceID, point: point, value: value})
	}
}

// enqueueWrite 非阻塞投递写请求;队列满则丢弃并告警(避免阻塞 MQTT 处理)。
func (o *thingsboardOutput) enqueueWrite(req writeRequest) {
	select {
	case o.writeCh <- req:
	default:
		slog.Warn("thingsboard downlink queue full, drop write", "device", req.deviceID, "point", req.point)
	}
}

// runWriter 消费写队列,调用注入的 WriteFunc(带超时)把值写回设备;RPC 请求写完后应答。
func (o *thingsboardOutput) runWriter() {
	defer o.wg.Done()
	for {
		select {
		case <-o.done:
			return
		case req := <-o.writeCh:
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := o.write(ctx, req.deviceID, req.point, req.value)
			cancel()
			if err != nil {
				slog.Error("thingsboard downlink write failed", "device", req.deviceID, "point", req.point, "err", err)
			}
			if req.rpcID != 0 {
				o.replyRPC(o.deviceName(req.deviceID), req.rpcID, err)
			}
		}
	}
}

// replyRPC 发送 RPC 应答:{"device":name,"id":id,"data":{"ok":...,"error":...}}。
func (o *thingsboardOutput) replyRPC(name string, id int64, err error) {
	data := map[string]interface{}{"ok": err == nil}
	if err != nil {
		data["error"] = err.Error()
	}
	payload := map[string]interface{}{"device": name, "id": id, "data": data}
	if perr := o.publish(topicRPC, payload); perr != nil {
		slog.Error("thingsboard rpc reply failed", "device", name, "err", perr)
	}
}

// runFlusher 按 flushInterval 周期性 flush,直到 Close 关闭 done。
func (o *thingsboardOutput) runFlusher() {
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

// flush 依次处理:disconnect → connect → 遥测(按时间戳聚合)→ quality 属性。
func (o *thingsboardOutput) flush() {
	o.mu.Lock()
	pending := o.pending
	o.pending = make(map[string][]model.DataPoint)
	o.pendingCount = 0
	qualities := o.qualities
	o.qualities = make(map[string]model.Quality)
	connects := o.connects
	o.connects = make(map[string]bool)
	disconnects := o.disconnects
	o.disconnects = make(map[string]bool)
	o.mu.Unlock()

	for name := range disconnects {
		if err := o.publish(topicDisconnect, map[string]interface{}{"device": name}); err != nil {
			slog.Error("thingsboard disconnect failed", "device", name, "err", err)
		}
		delete(o.connected, name)
	}
	for name := range connects {
		if err := o.ensureConnected(name); err != nil {
			slog.Error("thingsboard connect failed", "device", name, "err", err)
		}
	}
	for name, points := range pending {
		if len(points) == 0 {
			continue
		}
		if err := o.ensureConnected(name); err != nil {
			slog.Error("thingsboard connect failed", "device", name, "err", err)
			o.saveBackfill(points) // 设备未上线,该批遥测落库待续传
			continue
		}
		if err := o.publish(topicTelemetry, telemetryBatchPayload(name, points)); err != nil {
			slog.Error("thingsboard publish telemetry failed", "device", name, "err", err)
			o.saveBackfill(points) // 上送失败,该批遥测落库待续传
		}
	}
	for name, q := range qualities {
		attrs := map[string]interface{}{"quality": string(q)}
		if err := o.publish(topicAttributes, attributesPayload(name, attrs)); err != nil {
			slog.Error("thingsboard publish attributes failed", "device", name, "err", err)
		}
	}
}

func (o *thingsboardOutput) ensureConnected(name string) error {
	if o.connected[name] {
		return nil
	}
	if err := o.publish(topicConnect, map[string]interface{}{"device": name}); err != nil {
		return err
	}
	o.connected[name] = true
	return nil
}

func (o *thingsboardOutput) publish(topic string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("thingsboard marshal: %w", err)
	}
	// 有界等待:半死 broker 时最多阻塞 PublishTimeout 后报错,不卡死 flusher。
	err = mqttutil.WaitToken(o.client.Publish(topic, o.qos, false, data), mqttutil.PublishTimeout)
	if err != nil {
		o.SendStats.Failure(err)
		return err
	}
	o.SendStats.Success(time.Now())
	return nil
}

// saveBackfill 把无法即时送出的数据点持久化到补传队列(断网续传)。
// 未接线补传(backfill==nil,如直接构造的旧测试)或落库失败时仅告警,不阻塞调用方。
func (o *thingsboardOutput) saveBackfill(dps []model.DataPoint) {
	if len(dps) == 0 || o.backfill == nil {
		return
	}
	if err := o.backfill.Save(o.outputID, dps); err != nil {
		slog.Error("thingsboard backfill save failed", "err", err)
	}
}

// BackfillHealthy 实现 output.BackfillHealthy:仅在 MQTT 已连接时允许重放补传队列。
func (o *thingsboardOutput) BackfillHealthy() bool {
	return o.client != nil && o.client.IsConnected()
}

// RuntimeStatus 实现 output.StatusProvider:报告 MQTT 连接态、内部待发缓冲与上送统计。
func (o *thingsboardOutput) RuntimeStatus() output.RuntimeStatus {
	o.mu.Lock()
	pending := o.pendingCount
	o.mu.Unlock()
	sent, lastSentAt, lastErr, lastErrAt := o.SendStats.Snapshot()
	var connected, open bool
	if o.client != nil {
		connected = o.client.IsConnected()
		open = o.client.IsConnectionOpen()
	}
	return output.RuntimeStatus{
		Connected:      connected,
		ConnectionOpen: open,
		Pending:        pending,
		Sent:           sent,
		LastSentAt:     lastSentAt,
		LastError:      lastErr,
		LastErrorAt:    lastErrAt,
	}
}

func (o *thingsboardOutput) Close() error {
	close(o.done)
	o.wg.Wait() // 等 flusher 与 writer 退出
	o.flush()   // 上报剩余缓冲
	o.client.Disconnect(uint(disconnectQuiesce / time.Millisecond))
	return nil
}

// telemetryBatchPayload 把一批点位按时间戳分组,构造成网关遥测帧:
// 设备名 → [ {ts, values:{point:value,...}}, ... ]。
func telemetryBatchPayload(name string, points []model.DataPoint) map[string]interface{} {
	grouped := make(map[int64]map[string]interface{})
	var order []int64 // 保持时间戳出现顺序稳定
	for _, dp := range points {
		ts := dp.Timestamp.UnixMilli()
		if _, ok := grouped[ts]; !ok {
			grouped[ts] = make(map[string]interface{})
			order = append(order, ts)
		}
		grouped[ts][dp.Point] = dp.Value
	}
	arr := make([]map[string]interface{}, 0, len(order))
	for _, ts := range order {
		arr = append(arr, map[string]interface{}{"ts": ts, "values": grouped[ts]})
	}
	return map[string]interface{}{name: arr}
}

// attributesPayload 构造网关属性帧:设备名 → {key: value}。
func attributesPayload(name string, attrs map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{name: attrs}
}
