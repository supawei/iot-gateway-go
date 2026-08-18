package mqtt

import (
	"encoding/json"
	"fmt"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/output/mqttutil"
)

const (
	defaultQoS        byte = 1
	disconnectQuiesce      = 250 * time.Millisecond
)

// Config 是 MQTT 输出的配置(存 SQLite,经 Web UI 配置)。
type Config struct {
	Broker   string `json:"broker"`
	ClientID string `json:"clientId"`
	Username string `json:"username"`
	Password string `json:"password"`
	QoS      byte   `json:"qos"`
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
		},
	}, func(bc output.BuildContext, raw json.RawMessage) (output.Output, error) {
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("mqtt config: %w", err)
		}
		return New(cfg, bc.GatewayID)
	})
}

type mqttOutput struct {
	client    pahomqtt.Client
	gatewayID string
	qos       byte
}

func New(cfg Config, gatewayID string) (output.Output, error) {
	if cfg.QoS == 0 {
		cfg.QoS = defaultQoS
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
	return &mqttOutput{client: client, gatewayID: gatewayID, qos: cfg.QoS}, nil
}

func (m *mqttOutput) Publish(dp model.DataPoint) error {
	topic := fmt.Sprintf("gateway/%s/device/%s/data", m.gatewayID, dp.DeviceID)
	payload, err := json.Marshal(dp)
	if err != nil {
		return fmt.Errorf("marshal datapoint: %w", err)
	}
	// 有界等待:半死 broker 时最多阻塞 PublishTimeout 后返回错误,绝不永久卡死发布 goroutine。
	return mqttutil.WaitToken(m.client.Publish(topic, m.qos, false, payload), mqttutil.PublishTimeout)
}

func (m *mqttOutput) Close() error {
	m.client.Disconnect(uint(disconnectQuiesce / time.Millisecond))
	return nil
}
