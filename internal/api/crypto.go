package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

// cryptoBox 保存会话级 RSA 密钥对,用于 Web UI 密码字段加密传输。
// 私钥仅驻内存、不落盘;进程重启后公钥更换,前端每次加载时重新拉取。
type cryptoBox struct {
	priv   *rsa.PrivateKey
	pubPEM string
}

// newCryptoBox 生成 2048 位 RSA 密钥对,公钥导出为 SPKI PEM。
func newCryptoBox() (*cryptoBox, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate rsa key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return &cryptoBox{priv: priv, pubPEM: string(pubPEM)}, nil
}

// publicKeyPEM 返回公钥(SPKI PEM),供前端加密密码。
func (c *cryptoBox) publicKeyPEM() string { return c.pubPEM }

// decrypt 解密 base64(RSA-OAEP-SHA256)密文;空串原样返回空串。
func (c *cryptoBox) decrypt(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	ct, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("invalid base64 ciphertext")
	}
	pt, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, c.priv, ct, nil)
	if err != nil {
		return "", fmt.Errorf("rsa oaep decrypt failed")
	}
	return string(pt), nil
}

// isSensitiveField 判断配置字段是否为敏感凭据(密码/令牌)。
// 规则:字段类型为 password,或字段名命中常见凭据名(不区分大小写)。
func isSensitiveField(name, fieldType string) bool {
	if fieldType == "password" {
		return true
	}
	switch strings.ToLower(name) {
	case "password", "passwd", "accesstoken", "token", "secret", "apikey":
		return true
	}
	return false
}

// outputSensitiveFields 返回某输出类型的敏感字段名集合(按已注册 schema)。
func outputSensitiveFields(typ string) map[string]bool {
	names := map[string]bool{}
	for _, d := range output.ListTypes() {
		if d.Type != typ {
			continue
		}
		for _, f := range d.Schema {
			if isSensitiveField(f.Name, string(f.Type)) {
				names[f.Name] = true
			}
		}
	}
	return names
}

// driverSensitiveFields 返回某驱动的连接配置敏感字段名集合。
func driverSensitiveFields(name string) map[string]bool {
	names := map[string]bool{}
	for _, d := range driver.List() {
		if d.Name != name {
			continue
		}
		for _, f := range d.Config {
			if isSensitiveField(f.Name, string(f.Type)) {
				names[f.Name] = true
			}
		}
	}
	return names
}

// sensitiveNames 合并 schema 声明的敏感字段与按命名约定(配置中实际出现的键)的敏感字段。
// 兜底覆盖:未注册 schema 或 schema 漏标的 password/token 类字段也能被识别。
func sensitiveNames(raw json.RawMessage, declared map[string]bool) map[string]bool {
	names := make(map[string]bool, len(declared))
	for n := range declared {
		names[n] = true
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return names
	}
	for k := range m {
		if isSensitiveField(k, "") {
			names[k] = true
		}
	}
	return names
}

// decryptConfig 解密配置中所有非空敏感字段,返回处理后的 JSON。
// 敏感字段非空但无法解密时返回错误(拒绝明文/非法密文)。
func (a *API) decryptConfig(raw json.RawMessage, declared map[string]bool) (json.RawMessage, error) {
	cb, err := a.cryptoBox()
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, nil // 非对象配置(异常)原样放行,交给插件校验
	}
	for name := range sensitiveNames(raw, declared) {
		v, ok := m[name]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		pt, err := cb.decrypt(s)
		if err != nil {
			return nil, fmt.Errorf("decrypt config field %q: %w", name, err)
		}
		m[name] = pt
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return out, nil
}

// inheritSensitive 敏感字段新值为空/缺失时,从旧配置继承原值;新值非空则保留(已解密)。
// 用于 PUT 更新:密码留空表示"不修改",保持已存值。
func inheritSensitive(newRaw, oldRaw json.RawMessage, declared map[string]bool) (json.RawMessage, error) {
	var nm map[string]any
	if err := json.Unmarshal(newRaw, &nm); err != nil {
		return newRaw, nil
	}
	var om map[string]any
	if err := json.Unmarshal(oldRaw, &om); err != nil {
		return newRaw, nil // 旧配置不可解析则无法继承
	}
	for name := range sensitiveNames(newRaw, declared) {
		if nv, ok := nm[name]; ok {
			if s, isStr := nv.(string); isStr && s != "" {
				continue // 新值非空,使用新值
			}
			delete(nm, name) // 空值/非字符串 → 走继承
		}
		if ov, ok := om[name]; ok {
			nm[name] = ov
		}
	}
	out, err := json.Marshal(nm)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return out, nil
}

// maskConfig 将配置中敏感字段置空(不回显明文),用于所有返回给前端的响应。
func maskConfig(raw json.RawMessage, declared map[string]bool) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	changed := false
	for name := range sensitiveNames(raw, declared) {
		if _, ok := m[name]; ok {
			m[name] = ""
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// maskOutput 返回输出对象的脱敏副本(敏感字段置空)。
func maskOutput(o model.Output) model.Output {
	o.Config = maskConfig(o.Config, outputSensitiveFields(o.Type))
	return o
}

// maskConnection 返回连接对象的脱敏副本(敏感字段置空)。
func maskConnection(c model.Connection) model.Connection {
	c.Config = maskConfig(c.Config, driverSensitiveFields(c.Driver))
	return c
}

// maskDevice 返回设备对象的脱敏副本(参数中的敏感字段置空)。
// 设备参数未在设备上声明 schema(需经连接反查驱动),这里用命名约定兜底识别 password/token 类键。
func maskDevice(d model.Device) model.Device {
	d.Params = maskConfig(d.Params, nil)
	return d
}
