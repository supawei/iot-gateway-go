// Package thingsboard 实现 ThingsBoard 平台对接(北向输出插件)。
// 采用 ThingsBoard MQTT Gateway 模式:网关作为一个"网关设备",用一个 MQTT 连接
// 代表 N 个子设备。每个 DataPoint 映射为子设备的一条遥测;Quality 映射为客户端属性。
// 详见 docs/thingsboard.md。
package thingsboard

import (
	"encoding/json"
	"fmt"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

const (
	defaultQoS        = 1
	disconnectQuiesce = 250 * time.Millisecond
)

// ThingsBoard MQTT Gateway 的 topic。
const (
	topicConnect    = "v1/gateway/connect"
	topicDisconnect = "v1/gateway/disconnect"
	topicTelemetry  = "v1/gateway/telemetry"
	topicAttributes = "v1/gateway/attributes"
)

// Config 是 ThingsBoard 输出的网关级配置,来自 config.yaml 的 thingsboard 段。
type Config struct {
	Broker           string `yaml:"broker"`           // MQTT broker,如 tcp://tb.example.com:1883
	AccessToken      string `yaml:"accessToken"`      // 网关设备 Access Token(作为 MQTT 用户名)
	ClientID         string `yaml:"clientId"`         // MQTT client id
	Username         string `yaml:"username"`         // 可选,覆盖默认的 AccessToken 用户名
	Password         string `yaml:"password"`         // 可选
	QoS              byte   `yaml:"qos"`              // 默认 1
	DeviceNamePrefix string `yaml:"deviceNamePrefix"` // 子设备名前缀,默认空
	ReportQuality    *bool  `yaml:"reportQuality"`    // 是否上报 quality 属性,默认 true
}

type thingsboardOutput struct {
	client        pahomqtt.Client
	prefix        string
	qos           byte
	reportQuality bool
	connected     map[string]bool // 已发送 connect 的子设备名集合(单 publishLoop goroutine 访问,无需锁)
}

func New(cfg Config) (output.Output, error) {
	if cfg.QoS == 0 {
		cfg.QoS = defaultQoS
	}
	reportQuality := true
	if cfg.ReportQuality != nil {
		reportQuality = *cfg.ReportQuality
	}

	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.Broker)
	opts.SetClientID(cfg.ClientID)
	opts.SetAutoReconnect(true)
	username := cfg.Username
	if username == "" {
		username = cfg.AccessToken // ThingsBoard 以 Access Token 作为 MQTT 用户名
	}
	opts.SetUsername(username)
	opts.SetPassword(cfg.Password)

	client := pahomqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return nil, fmt.Errorf("thingsboard mqtt connect: %w", token.Error())
	}

	return &thingsboardOutput{
		client:        client,
		prefix:        cfg.DeviceNamePrefix,
		qos:           cfg.QoS,
		reportQuality: reportQuality,
		connected:     make(map[string]bool),
	}, nil
}

func (o *thingsboardOutput) deviceName(deviceID string) string {
	return o.prefix + deviceID
}

// Publish 把单个 DataPoint 上报为子设备遥测(数组单元素),并可选上报 quality 属性。
// 惰性 connect:首次出现某子设备时先发 v1/gateway/connect 登记。
func (o *thingsboardOutput) Publish(dp model.DataPoint) error {
	name := o.deviceName(dp.DeviceID)

	if err := o.ensureConnected(name); err != nil {
		return err
	}

	// 遥测:仅当有值时上报(bad/uncertain 无值,不上报遥测)
	if dp.Value != nil {
		if err := o.publish(topicTelemetry, telemetryPayload(name, dp)); err != nil {
			return err
		}
	}

	// 质量属性(可选)
	if o.reportQuality {
		attrs := map[string]interface{}{"quality": string(dp.Quality)}
		if err := o.publish(topicAttributes, attributesPayload(name, attrs)); err != nil {
			return err
		}
	}
	return nil
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
	token := o.client.Publish(topic, o.qos, false, data)
	token.Wait()
	return token.Error()
}

func (o *thingsboardOutput) Close() error {
	o.client.Disconnect(uint(disconnectQuiesce / time.Millisecond))
	return nil
}

// telemetryPayload 构造网关遥测帧:设备名 → [ {ts, values} ]。
func telemetryPayload(name string, dp model.DataPoint) map[string]interface{} {
	return map[string]interface{}{
		name: []map[string]interface{}{
			{
				"ts":     dp.Timestamp.UnixMilli(),
				"values": map[string]interface{}{dp.Point: dp.Value},
			},
		},
	}
}

// attributesPayload 构造网关属性帧:设备名 → {key: value}。
func attributesPayload(name string, attrs map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{name: attrs}
}
