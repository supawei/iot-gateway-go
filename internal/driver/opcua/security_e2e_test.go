package opcua

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopcua/opcua/server"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uapolicy"

	"iot-gateway-go/internal/driver"
	"iot-gateway-go/internal/model"
)

// ============================================================================
// 安全模式端到端测试:进程内 gopcua server 启用 Basic256Sha256(Sign/SignAndEncrypt)。
// 与共享 none 服务器分离,避免影响既有测试;单例摊薄 ~13s 标准 nodeset 导入成本。
// ============================================================================

type secureE2EEnv struct {
	endpoint         string
	serverThumbprint string // 服务器证书 SHA-1 指纹(hex)
	srvCertDER       []byte
	intNode          *server.Node // ns=1;i=101
}

var (
	secureOnce sync.Once
	secureInst *secureE2EEnv
	secureErr  error
)

func getSecureE2EEnv(t *testing.T) *secureE2EEnv {
	t.Helper()
	secureOnce.Do(func() {
		const port = 48502
		srvKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			secureErr = err
			return
		}
		srvCertDER := genTestCertDER("urn:iot-gateway:secure-server", srvKey)
		srv := server.New(
			server.EndPoint("127.0.0.1", port),
			server.PrivateKey(srvKey),
			server.Certificate(srvCertDER),
			server.EnableSecurity("Basic256Sha256", ua.MessageSecurityModeSign),
			server.EnableSecurity("Basic256Sha256", ua.MessageSecurityModeSignAndEncrypt),
		)
		ns := server.NewNodeNameSpace(srv, "urn:iot-gateway:secure")
		intNode := ns.AddNewVariableNode("SecIntVar", int32(0))
		ns.Objects().AddRef(intNode, server.RefTypeIDOrganizes, true)
		if err := srv.Start(context.Background()); err != nil {
			secureErr = err
			return
		}
		secureInst = &secureE2EEnv{
			endpoint:         "opc.tcp://127.0.0.1:" + strconv.Itoa(port),
			serverThumbprint: hex.EncodeToString(uapolicy.Thumbprint(srvCertDER)),
			srvCertDER:       srvCertDER,
			intNode:          intNode,
		}
		time.Sleep(150 * time.Millisecond) // 等监听就绪
	})
	if secureErr != nil {
		t.Fatalf("start secure e2e server: %v", secureErr)
	}
	return secureInst
}

// genTestCertDER 生成自签名证书 DER(服务器/客户端测试用)。
func genTestCertDER(appURI string, key *rsa.PrivateKey) []byte {
	uri, err := url.Parse(appURI)
	if err != nil {
		panic(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "iot-gateway e2e"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageDataEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		URIs:                  []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return der
}

// writeClientCertFiles 用驱动的 generateClientCert 生成客户端证书/私钥 PEM 并落盘。
func writeClientCertFiles(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	certPEM, keyPEM, err := generateClientCert()
	if err != nil {
		t.Fatalf("generate client cert: %v", err)
	}
	certFile = filepath.Join(dir, "client-cert.pem")
	keyFile = filepath.Join(dir, "client-key.pem")
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

// openSecureConn 打开指向安全服务器的 opcua 驱动连接;extra 为覆盖/追加的 config 字段。
func openSecureConn(t *testing.T, env *secureE2EEnv, connectionID string, mode string, extra map[string]interface{}) driver.Conn {
	t.Helper()
	cfg := map[string]interface{}{
		"endpoint":       env.endpoint,
		"timeout":        "5s",
		"securityMode":   mode,
		"securityPolicy": "Basic256Sha256",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	drv, err := driver.Get("opcua")
	if err != nil {
		t.Fatalf("get driver: %v", err)
	}
	conn, err := drv.Open(context.Background(), driver.OpenRequest{
		DeviceID:     "sec-" + connectionID,
		ConnectionID: connectionID,
		ConnConfig:   raw,
		DeviceParams: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("open secure conn (%s): %v", mode, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func secureWriteRead(t *testing.T, conn driver.Conn, node *server.Node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	point := model.Point{Name: "SecIntVar", Address: node.ID().String(), DataType: model.DataTypeInt32}
	if _, err := conn.(driver.Writer).Write(ctx, []model.WriteItem{{Point: point, Value: 88.0}}); err != nil {
		t.Fatalf("secure write: %v", err)
	}
	dps, err := conn.Read(ctx, []model.Point{point})
	if err != nil {
		t.Fatalf("secure read: %v", err)
	}
	if len(dps) != 1 || dps[0].Quality != model.QualityGood || dps[0].Value != int64(88) {
		t.Fatalf("secure read result: %+v", dps)
	}
}

// TestSecuritySignE2E 安全模式 Sign + Basic256Sha256 下写读真实往返。
func TestSecuritySignE2E(t *testing.T) {
	env := getSecureE2EEnv(t)
	dir := t.TempDir()
	certFile, keyFile := writeClientCertFiles(t, dir)
	conn := openSecureConn(t, env, "sec-sign", "sign", map[string]interface{}{
		"clientCertFile": certFile, "clientKeyFile": keyFile,
		"serverThumbprint": env.serverThumbprint,
	})
	secureWriteRead(t, conn, env.intNode)
}

// TestSecuritySignAndEncryptE2E 安全模式 SignAndEncrypt + Basic256Sha256 下写读真实往返。
func TestSecuritySignAndEncryptE2E(t *testing.T) {
	env := getSecureE2EEnv(t)
	dir := t.TempDir()
	certFile, keyFile := writeClientCertFiles(t, dir)
	conn := openSecureConn(t, env, "sec-se", "signAndEncrypt", map[string]interface{}{
		"clientCertFile": certFile, "clientKeyFile": keyFile,
		"serverThumbprint": env.serverThumbprint,
	})
	secureWriteRead(t, conn, env.intNode)
}

// TestSecurityThumbprintE2E 服务器证书指纹校验:正确通过;错误拒绝建连。
func TestSecurityThumbprintE2E(t *testing.T) {
	env := getSecureE2EEnv(t)
	dir := t.TempDir()
	certFile, keyFile := writeClientCertFiles(t, dir)

	// 正确指纹 -> 成功
	openSecureConn(t, env, "sec-tp-ok", "sign", map[string]interface{}{
		"clientCertFile": certFile, "clientKeyFile": keyFile,
		"serverThumbprint": env.serverThumbprint,
	})

	// 错误指纹 -> Open 报错(指纹不匹配)
	drv, _ := driver.Get("opcua")
	raw, _ := json.Marshal(map[string]interface{}{
		"endpoint": env.endpoint, "timeout": "5s", "securityMode": "sign",
		"securityPolicy": "Basic256Sha256",
		"clientCertFile": certFile, "clientKeyFile": keyFile,
		"serverThumbprint": strings.Repeat("00", 20),
	})
	if _, err := drv.Open(context.Background(), driver.OpenRequest{
		DeviceID: "sec-tp-bad", ConnectionID: "sec-tp-bad", ConnConfig: raw, DeviceParams: []byte(`{}`),
	}); err == nil {
		t.Fatal("wrong server thumbprint should fail open")
	} else if !strings.Contains(err.Error(), "thumbprint") {
		t.Fatalf("want thumbprint mismatch error, got: %v", err)
	}
}

// TestSecurityNoEndpointE2E 请求服务器未提供的策略 -> 明确报错。
func TestSecurityNoEndpointE2E(t *testing.T) {
	env := getSecureE2EEnv(t)
	dir := t.TempDir()
	certFile, keyFile := writeClientCertFiles(t, dir)
	drv, _ := driver.Get("opcua")
	raw, _ := json.Marshal(map[string]interface{}{
		"endpoint": env.endpoint, "timeout": "5s", "securityMode": "sign",
		"securityPolicy": "Basic128Rsa15", // 服务器只提供 Basic256Sha256
		"clientCertFile": certFile, "clientKeyFile": keyFile,
	})
	if _, err := drv.Open(context.Background(), driver.OpenRequest{
		DeviceID: "sec-noep", ConnectionID: "sec-noep", ConnConfig: raw, DeviceParams: []byte(`{}`),
	}); err == nil {
		t.Fatal("unoffered policy should fail open")
	} else if !strings.Contains(err.Error(), "no endpoint") {
		t.Fatalf("want no-endpoint error, got: %v", err)
	}
}

// TestSecurityAutoCertE2E 未配置客户端证书时自动生成(工作目录),建连成功。
func TestSecurityAutoCertE2E(t *testing.T) {
	env := getSecureE2EEnv(t)
	dir := t.TempDir()
	t.Chdir(dir) // 自动生成证书落在临时工作目录

	conn := openSecureConn(t, env, "sec-autocert", "sign", map[string]interface{}{
		"serverThumbprint": env.serverThumbprint,
	})
	if _, err := os.Stat(defaultClientCertFile); err != nil {
		t.Fatalf("auto-generated cert not found: %v", err)
	}
	if _, err := os.Stat(defaultClientKeyFile); err != nil {
		t.Fatalf("auto-generated key not found: %v", err)
	}
	secureWriteRead(t, conn, env.intNode)
}
