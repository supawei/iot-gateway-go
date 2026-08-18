// Package mqttutil 提供 MQTT 类输出(mqtt / thingsboard / smardaten)共享的连接韧性与
// 有界等待工具,避免各输出重复实现并保证超时语义一致。设计见 docs/mqtt-resilience-design.md。
package mqttutil

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

const (
	// ConnectTimeout 单次 TCP 拨号/握手超时。
	ConnectTimeout = 5 * time.Second
	// ConnectProbe 首次连接仅用于日志的探测等待;超时不代表失败,后台仍会重试。
	ConnectProbe = 5 * time.Second
	// ConnectRetryInterval 首次连接重试间隔。
	ConnectRetryInterval = 2 * time.Second
	// MaxReconnectInterval 重连指数退避上限。
	MaxReconnectInterval = 30 * time.Second
	// PublishTimeout 发布 token 的有界等待上限。
	PublishTimeout = 5 * time.Second
	// WriteTimeout paho obound 投递超时,防止半死 broker 时 Publish 长期占用。
	WriteTimeout = 5 * time.Second
)

// ErrPublishTimeout 发布等待超时的哨兵错误,可用 errors.Is 判定。
var ErrPublishTimeout = errors.New("mqtt publish timeout")

// WaitToken 有界等待 paho token 完成;超时返回 ErrPublishTimeout。
// 基于 paho 原生 Token.WaitTimeout:超时时 token 不置错,可再次等待。
func WaitToken(t paho.Token, d time.Duration) error {
	if t.WaitTimeout(d) {
		return t.Error()
	}
	return fmt.Errorf("%w (after %v)", ErrPublishTimeout, d)
}

// ApplyResilience 把连接韧性参数统一施加到客户端选项上。
// 各输出仍自行设置 broker / clientID / 凭据 / 协议版本等差异化项。
//
// 关键:SetConnectRetry(true) 覆盖「从未连上」场景(首次连接失败后 AutoReconnect 不会触发),
// 配合非阻塞构造实现「先启动、后台自动重连、连接前 publish 落内存 store 待补发」。
func ApplyResilience(opts *paho.ClientOptions) {
	opts.SetConnectTimeout(ConnectTimeout)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(ConnectRetryInterval)
	opts.SetMaxReconnectInterval(MaxReconnectInterval)
	opts.SetAutoReconnect(true)
	opts.SetWriteTimeout(WriteTimeout)
}

// ConnectNonBlocking 发起连接但不阻塞调用方:等待 ConnectProbe 仅用于日志,
// 连接失败/未就绪由 ConnectRetry 在后台无限重试兜底,调用方不应因连接失败而中止构建。
func ConnectNonBlocking(client paho.Client, label string) {
	tok := client.Connect()
	if tok.WaitTimeout(ConnectProbe) {
		if err := tok.Error(); err != nil {
			slog.Warn("mqtt initial connect failed, retrying in background", "output", label, "err", err)
		} else {
			slog.Info("mqtt connected", "output", label)
		}
		return
	}
	slog.Warn("mqtt initial connect not established in time, retrying in background", "output", label)
}
