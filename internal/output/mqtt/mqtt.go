package mqtt

import (
	"encoding/json"
	"fmt"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

const (
	defaultQoS        byte = 1
	disconnectQuiesce      = 250 * time.Millisecond
)

// Config 是 MQTT 输出的网关级配置,来自 config.yaml 的 mqtt 段。
type Config struct {
	Broker   string `yaml:"broker"`
	ClientID string `yaml:"clientId"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	QoS      byte   `yaml:"qos"`
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
	opts.SetAutoReconnect(true)
	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}
	client := pahomqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return nil, fmt.Errorf("mqtt connect: %w", token.Error())
	}
	return &mqttOutput{client: client, gatewayID: gatewayID, qos: cfg.QoS}, nil
}

func (m *mqttOutput) Publish(dp model.DataPoint) error {
	topic := fmt.Sprintf("gateway/%s/device/%s/data", m.gatewayID, dp.DeviceID)
	payload, err := json.Marshal(dp)
	if err != nil {
		return fmt.Errorf("marshal datapoint: %w", err)
	}
	token := m.client.Publish(topic, m.qos, false, payload)
	token.Wait()
	return token.Error()
}

func (m *mqttOutput) Close() error {
	m.client.Disconnect(uint(disconnectQuiesce / time.Millisecond))
	return nil
}
