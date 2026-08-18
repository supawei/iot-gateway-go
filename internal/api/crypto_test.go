package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"

	"iot-gateway-go/internal/auth"
	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
)

// registerPwdOutput 注册一个带 password 字段的测试输出类型。
func registerPwdOutput(t *testing.T) {
	t.Helper()
	output.Register(output.Descriptor{
		Type:  "pwd-out",
		Label: "密码输出",
		Schema: []output.Field{
			{Name: "broker", Label: "地址", Type: output.FieldString},
			{Name: "password", Label: "密码", Type: output.FieldPassword},
		},
	}, func(output.BuildContext, json.RawMessage) (output.Output, error) { return nil, nil })
}

// pwdDriverMock 带 password 字段的驱动(与真实 opcua 一样声明为 string 类型),
// 验证连接配置同样按命名约定识别并打码。
type pwdDriverMock struct{}

func (*pwdDriverMock) Open(context.Context, driver.OpenRequest) (driver.Conn, error) { return nil, nil }
func (*pwdDriverMock) ConfigSchema() []driver.Field {
	return []driver.Field{
		{Name: "host", Label: "地址", Type: driver.FieldString},
		{Name: "password", Label: "密码", Type: driver.FieldString},
	}
}

func TestPublicKeyEndpoint(t *testing.T) {
	apiInstance := newTestAPI(t)
	rec := doRequest(t, apiInstance.Routes(), "GET", "/api/v1/crypto/publicKey", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public key: got %d", rec.Code)
	}
	var body struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(body.PublicKey, "BEGIN PUBLIC KEY") {
		t.Fatalf("public key not PEM: %q", body.PublicKey)
	}
}

// TestLoginRejectsPlaintextPassword 验证明文密码登录被拒绝(强制加密传输)。
func TestLoginRejectsPlaintextPassword(t *testing.T) {
	apiInstance, _ := newAuthAPI(t)
	rec := doRequest(t, apiInstance.Routes(), "POST", "/api/v1/auth/login",
		map[string]string{"username": auth.DefaultAdminUser, "password": auth.DefaultAdminPassword})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("plaintext login: got %d want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestOutputPasswordEncryptedAndMasked 验证输出密码:密文入库解密为明文(驱动需要),
// 但所有 API 响应打码;PUT 密码留空时继承旧值。
func TestOutputPasswordEncryptedAndMasked(t *testing.T) {
	registerPwdOutput(t)
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	cfg := json.RawMessage(`{"broker":"tcp://x:1883","password":"s3cret"}`)
	o := model.Output{
		ID: "o1", Name: "x", Type: "pwd-out", Enabled: true,
		Config: encryptConfigForTest(t, handler, cfg, map[string]bool{"password": true}),
	}
	rec := doRequest(t, handler, "POST", "/api/v1/outputs", o)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var created model.Output
	json.Unmarshal(rec.Body.Bytes(), &created)
	var cfgResp map[string]any
	json.Unmarshal(created.Config, &cfgResp)
	if cfgResp["password"] != "" {
		t.Fatalf("create response password not masked: %v", cfgResp)
	}

	// 入库的是明文(输出插件运行时需要明文)
	stored, err := apiInstance.store.GetOutput("o1")
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	var storedCfg map[string]any
	json.Unmarshal(stored.Config, &storedCfg)
	if storedCfg["password"] != "s3cret" {
		t.Fatalf("stored password = %v want s3cret", storedCfg["password"])
	}

	// GET 打码
	rec = doRequest(t, handler, "GET", "/api/v1/outputs/o1", nil)
	var got model.Output
	json.Unmarshal(rec.Body.Bytes(), &got)
	json.Unmarshal(got.Config, &cfgResp)
	if cfgResp["password"] != "" {
		t.Fatalf("get password not masked: %v", cfgResp)
	}

	// PUT 密码留空 → 继承旧值
	upd := model.Output{
		ID: "o1", Name: "x2", Type: "pwd-out", Enabled: true,
		Config: json.RawMessage(`{"broker":"tcp://x:1883"}`),
	}
	rec = doRequest(t, handler, "PUT", "/api/v1/outputs/o1", upd)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: got %d body=%s", rec.Code, rec.Body.String())
	}
	stored, _ = apiInstance.store.GetOutput("o1")
	json.Unmarshal(stored.Config, &storedCfg)
	if storedCfg["password"] != "s3cret" {
		t.Fatalf("password not inherited on blank: %v", storedCfg["password"])
	}

	// PUT 新密码(密文) → 更新
	upd2 := model.Output{
		ID: "o1", Name: "x3", Type: "pwd-out", Enabled: true,
		Config: encryptConfigForTest(t, handler, json.RawMessage(`{"broker":"tcp://x:1883","password":"newpass"}`), map[string]bool{"password": true}),
	}
	rec = doRequest(t, handler, "PUT", "/api/v1/outputs/o1", upd2)
	if rec.Code != http.StatusOK {
		t.Fatalf("put new password: got %d body=%s", rec.Code, rec.Body.String())
	}
	stored, _ = apiInstance.store.GetOutput("o1")
	json.Unmarshal(stored.Config, &storedCfg)
	if storedCfg["password"] != "newpass" {
		t.Fatalf("password not updated: %v", storedCfg["password"])
	}
}

// TestOutputPasswordPlaintextRejected 验证明文密码(非空)被拒绝。
func TestOutputPasswordPlaintextRejected(t *testing.T) {
	registerPwdOutput(t)
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	o := model.Output{ID: "o1", Name: "x", Type: "pwd-out", Enabled: true,
		Config: json.RawMessage(`{"password":"plain"}`)}
	rec := doRequest(t, handler, "POST", "/api/v1/outputs", o)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("plaintext password accepted: got %d, body=%s", rec.Code, rec.Body.String())
	}
}

// TestConnectionPasswordMasked 验证连接配置中的 password 字段(即使 schema 标为 string)
// 也按命名约定打码,入库为解密明文。
func TestConnectionPasswordMasked(t *testing.T) {
	driver.Register("pwd-driver", &pwdDriverMock{})
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	cfg := json.RawMessage(`{"host":"10.0.0.1","password":"secret"}`)
	conn := model.Connection{
		ID: "c1", Name: "x", Driver: "pwd-driver",
		Config: encryptConfigForTest(t, handler, cfg, map[string]bool{"password": true}),
	}
	rec := doRequest(t, handler, "POST", "/api/v1/connections", conn)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, handler, "GET", "/api/v1/connections/c1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d", rec.Code)
	}
	var got model.Connection
	json.Unmarshal(rec.Body.Bytes(), &got)
	var cfgResp map[string]any
	json.Unmarshal(got.Config, &cfgResp)
	if cfgResp["password"] != "" || cfgResp["host"] != "10.0.0.1" {
		t.Fatalf("connection mask: %v", cfgResp)
	}

	stored, _ := apiInstance.store.GetConnection("c1")
	var storedCfg map[string]any
	json.Unmarshal(stored.Config, &storedCfg)
	if storedCfg["password"] != "secret" {
		t.Fatalf("stored password = %v want secret", storedCfg["password"])
	}
}

// TestDeviceParamEncryptedAndMasked 验证设备参数中的敏感字段(命名约定识别):
// 密文解密后入库,响应打码,PU 留空继承旧值。
func TestDeviceParamEncryptedAndMasked(t *testing.T) {
	apiInstance := newTestAPI(t)
	handler := apiInstance.Routes()

	params := json.RawMessage(`{"host":"10.0.0.1","password":"d-pass"}`)
	d := model.Device{
		ID: "d1", Name: "dev", ConnectionID: "c1", Enabled: true,
		Params: encryptConfigForTest(t, handler, params, map[string]bool{"password": true}),
	}
	rec := doRequest(t, handler, "POST", "/api/v1/devices", d)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var created model.Device
	json.Unmarshal(rec.Body.Bytes(), &created)
	var pResp map[string]any
	json.Unmarshal(created.Params, &pResp)
	if pResp["password"] != "" || pResp["host"] != "10.0.0.1" {
		t.Fatalf("create response params not masked: %v", pResp)
	}

	// 入库为解密明文
	stored, err := apiInstance.store.GetDevice("d1")
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	var storedP map[string]any
	json.Unmarshal(stored.Params, &storedP)
	if storedP["password"] != "d-pass" {
		t.Fatalf("stored password = %v want d-pass", storedP["password"])
	}

	// list 打码
	rec = doRequest(t, handler, "GET", "/api/v1/devices", nil)
	var list []model.Device
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("list len = %d", len(list))
	}
	json.Unmarshal(list[0].Params, &pResp)
	if pResp["password"] != "" {
		t.Fatalf("list password not masked: %v", pResp)
	}

	// PUT 密码留空 → 继承
	upd := model.Device{ID: "d1", Name: "dev2", ConnectionID: "c1", Enabled: true,
		Params: json.RawMessage(`{"host":"10.0.0.1"}`)}
	rec = doRequest(t, handler, "PUT", "/api/v1/devices/d1", upd)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: got %d body=%s", rec.Code, rec.Body.String())
	}
	stored, _ = apiInstance.store.GetDevice("d1")
	json.Unmarshal(stored.Params, &storedP)
	if storedP["password"] != "d-pass" {
		t.Fatalf("device password not inherited: %v", storedP["password"])
	}
}

// TestCryptoRoundTrip 验证 cryptoBox 公钥加密/私钥解密往返一致。
func TestCryptoRoundTrip(t *testing.T) {
	cb, err := newCryptoBox()
	if err != nil {
		t.Fatalf("new crypto box: %v", err)
	}
	block, _ := pem.Decode([]byte(cb.publicKeyPEM()))
	if block == nil {
		t.Fatal("decode pem failed")
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		t.Fatal("not rsa")
	}
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte("hunter2"), nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := cb.decrypt(base64.StdEncoding.EncodeToString(ct))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("round trip = %q", got)
	}
}
