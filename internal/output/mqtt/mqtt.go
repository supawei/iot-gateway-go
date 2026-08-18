package mqtt

import (
	"encoding/json"
	"fmt"
	"log/slog"
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

	// defaultBatchMax 是批量模式单条消息的最大点数,超出拆分,防超大帧。
	defaultBatchMax = 64
	// maxPendingPoints 是批量模式待发缓冲上限:超过上限丢弃新点并告警,
	// 避免慢 broker 期间内存无界增长(与其他输出同策略)。
	maxPendingPoints = 8192
)

// Config 是 MQTT 输出的配置(存 SQLite,经 Web UI 配置)。
type Config struct {
	Broker   string `json:"broker"`
	ClientID string `json:"clientId"`
	Username string `json:"username"`
	Password string `json:"password"`
	QoS      byte   `json:"qos"`
	// FlushInterval 是批量聚合间隔:空=即时单条发布(默认,向后兼容);
	// 设置如 "200ms" 启用批量模式——同一设备在窗口内到达的点聚合为一条消息。
	FlushInterval string `json:"flushInterval"`
	// BatchMax 是批量模式单条消息最大点数(默认 64)。
	BatchMax int `json:"batchMax"`
}

// init 注册 MQTT 输出类型:声明配置 schema 并绑定构造器。
func init() {
	output.Register(output.Descriptor{
		Type:  "mqtt",
		Label: "MQTT",
		Schema: []output.Field{
			{Name: "broker", Label: "Broker 地址", Type: output.FieldString, Required: true, Placeholder: "tcp://127.0.0.1:1883"},
			{Name: "clientId", Label: "Client ID", Type: output.FieldString, Placeholder: "iot-gateway"},
			{Name: "username", Label: "用户名", Type: output.FieldString},
			{Name: "password", Label: "密码", Type: output.FieldPassword},
			{Name: "qos", Label: "QoS", Type: output.FieldInt, Default: 1},
			{Name: "flushInterval", Label: "批量间隔", Type: output.FieldString, Placeholder: "200ms",
				Hint: "留空=即时单条发布(默认);设置如 200ms 启用批量,同设备多点聚合为一条消息"},
			{Name: "batchMax", Label: "单条最大点数", Type: output.FieldInt, Default: 64,
				Hint: "批量模式单条消息最大点数,超出拆分"},
		},
	}, func(bc output.BuildContext, raw json.RawMessage) (output.Output, error) {
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("mqtt config: %w", err)
		}
		return New(cfg, bc.GatewayID, bc.OutputID, bc.Backfill)
	})
}

type mqttOutput struct {
	client    pahomqtt.Client
	gatewayID string
	qos       byte

	// 断网补传(见 docs/offline-backfill-design.md):outputID 为补传队列隔离键,
	// backfill 为持久化队列(未接线时 nil,退化为"失败丢弃 + 告警")。
	outputID string
	backfill output.BackfillSink

	flushInterval time.Duration
	batchMax      int

	// 批量模式缓冲(flushInterval>0 时启用)。
	mu           sync.Mutex
	pending      map[string][]model.DataPoint // deviceID -> 待发点
	pendingCount int

	done chan struct{}
	wg   sync.WaitGroup

	// 实际上送统计(见 docs/output-status-design.md)
	output.SendStats
}

func New(cfg Config, gatewayID string, outputID string, backfill output.BackfillSink) (output.Output, error) {
	if cfg.QoS == 0 {
		cfg.QoS = defaultQoS
	}
	batchMax := cfg.BatchMax
	if batchMax <= 0 {
		batchMax = defaultBatchMax
	}
	flushInterval := time.Duration(0)
	if cfg.FlushInterval != "" {
		d, err := time.ParseDuration(cfg.FlushInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid mqtt flushInterval %q: %w", cfg.FlushInterval, err)
		}
		flushInterval = d
	}

	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.Broker)
	opts.SetClientID(cfg.ClientID)
	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}
	mqttutil.ApplyResilience(opts)
	client := pahomqtt.NewClient(opts)
	// 非阻塞连接:broker 不可达时不再阻塞构建,由 ConnectRetry 后台自动重连兜底。
	mqttutil.ConnectNonBlocking(client, "mqtt")

	m := &mqttOutput{
		client:        client,
		gatewayID:     gatewayID,
		qos:           cfg.QoS,
		outputID:      outputID,
		backfill:      backfill,
		flushInterval: flushInterval,
		batchMax:      batchMax,
	}
	if flushInterval > 0 {
		m.pending = make(map[string][]model.DataPoint)
		m.done = make(chan struct{})
		m.wg.Add(1)
		go m.runFlusher()
	}
	return m, nil
}

// Publish 即时模式同步单条发布;批量模式入缓冲由 flusher 聚合发布。
func (m *mqttOutput) Publish(dp model.DataPoint) error {
	if m.flushInterval <= 0 {
		return m.publishNow(dp)
	}
	// 批量模式:入缓冲,由 flusher 每窗口聚合为每设备一条消息。
	// 缓冲满不再丢弃:启用补传时落库待续传。
	var toSave []model.DataPoint
	m.mu.Lock()
	if m.pendingCount >= maxPendingPoints {
		toSave = append(toSave, dp)
	} else {
		m.pending[dp.DeviceID] = append(m.pending[dp.DeviceID], dp)
		m.pendingCount++
	}
	m.mu.Unlock()
	if len(toSave) > 0 {
		m.saveBackfill(toSave)
	}
	return nil
}

// publishNow 即时模式:单点一条消息(payload 为单个 DataPoint 对象)。
// 发送失败时落库补传,恢复后由 Manager 重放。
func (m *mqttOutput) publishNow(dp model.DataPoint) error {
	payload, err := json.Marshal(dp)
	if err != nil {
		return fmt.Errorf("marshal datapoint: %w", err)
	}
	if err := m.publish(m.deviceTopic(dp.DeviceID), payload); err != nil {
		m.saveBackfill([]model.DataPoint{dp})
		return err
	}
	return nil
}

// deviceTopic 返回设备数据 topic:gateway/{gw}/device/{dev}/data。
func (m *mqttOutput) deviceTopic(deviceID string) string {
	return fmt.Sprintf("gateway/%s/device/%s/data", m.gatewayID, deviceID)
}

// runFlusher 每 flushInterval 把缓冲聚合发布一次,直到 Close 关闭 done。
func (m *mqttOutput) runFlusher() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.flushOnce()
		}
	}
}

// flushOnce 取走全部缓冲,按设备各发一条消息;broker 完全断连时跳过本轮。
func (m *mqttOutput) flushOnce() {
	if !m.client.IsConnected() {
		return
	}
	m.mu.Lock()
	pending := m.pending
	m.pending = make(map[string][]model.DataPoint)
	m.pendingCount = 0
	m.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	for deviceID, points := range pending {
		m.publishBatch(deviceID, points)
	}
}

// publishBatch 把同一设备的点按 batchMax 拆条发布;payload 为 DataPoint 数组。
// 首条失败即停止该设备剩余拆分(有界等待后已确认不可达,避免叠加超时),
// 并把未发出的点落库补传,恢复后重放。
func (m *mqttOutput) publishBatch(deviceID string, points []model.DataPoint) {
	topic := m.deviceTopic(deviceID)
	for start := 0; start < len(points); start += m.batchMax {
		end := start + m.batchMax
		if end > len(points) {
			end = len(points)
		}
		payload, err := json.Marshal(points[start:end])
		if err != nil {
			slog.Error("mqtt marshal batch failed", "device", deviceID, "err", err)
			continue
		}
		if err := m.publish(topic, payload); err != nil {
			slog.Error("mqtt publish batch failed", "device", deviceID, "points", end-start, "err", err)
			m.saveBackfill(points[start:]) // 失败条 + 未发送的剩余条落库待续传
			return
		}
	}
}

// saveBackfill 把无法即时送出的数据点持久化到补传队列(断网续传)。
// 未接线补传(backfill==nil)或落库失败时仅告警,不阻塞调用方。
func (m *mqttOutput) saveBackfill(dps []model.DataPoint) {
	if len(dps) == 0 || m.backfill == nil {
		return
	}
	if err := m.backfill.Save(m.outputID, dps); err != nil {
		slog.Error("mqtt backfill save failed", "err", err)
	}
}

// BackfillHealthy 实现 output.BackfillHealthy:仅在 MQTT 已连接时允许重放补传队列。
func (m *mqttOutput) BackfillHealthy() bool {
	return m.client != nil && m.client.IsConnected()
}

// publish 有界等待发布单条消息;成功/失败更新上送统计。
func (m *mqttOutput) publish(topic string, payload []byte) error {
	// 有界等待:半死 broker 时最多阻塞 PublishTimeout 后返回错误,绝不永久卡死。
	err := mqttutil.WaitToken(m.client.Publish(topic, m.qos, false, payload), mqttutil.PublishTimeout)
	if err != nil {
		m.SendStats.Failure(err)
		return err
	}
	m.SendStats.Success(time.Now())
	return nil
}

// RuntimeStatus 实现 output.StatusProvider:报告 MQTT 连接态、批量缓冲与上送统计。
func (m *mqttOutput) RuntimeStatus() output.RuntimeStatus {
	sent, lastSentAt, lastErr, lastErrAt := m.SendStats.Snapshot()
	pending := 0
	if m.flushInterval > 0 {
		m.mu.Lock()
		pending = m.pendingCount
		m.mu.Unlock()
	}
	return output.RuntimeStatus{
		Connected:      m.client.IsConnected(),
		ConnectionOpen: m.client.IsConnectionOpen(),
		Pending:        pending,
		Sent:           sent,
		LastSentAt:     lastSentAt,
		LastError:      lastErr,
		LastErrorAt:    lastErrAt,
	}
}

func (m *mqttOutput) Close() error {
	if m.done != nil {
		close(m.done)
		m.wg.Wait()   // 等 flusher 退出,避免与 flush 并发
		m.flushOnce() // 发送剩余缓冲
	}
	m.client.Disconnect(uint(disconnectQuiesce / time.Millisecond))
	return nil
}
