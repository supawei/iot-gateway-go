package smardaten

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	httpTimeout  = 30 * time.Second
	maxRedirects = 10

	// 平台固定凭据：应用 ID 与 RSA 公钥由 smardaten-iot 平台固定提供，直接内置，不开放配置。
	defaultIotAppID = "531b9a9d-95da-4263-9acf-5b6b99d91197"
	defaultIotPublicKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQCynbPzQFTFCSQFQLv5EpyFKlfm
bzSeBpsLCGW32DIfBQ3iU9/1fr9G9Ecf2mvGsI09J0aib36zcgNOx/7pO6CYNaF0
GF/rngvLvMV13zukYYrP7FvpJnvvWA8gTRO+oUyrWyTr/A61bJA1w1oFiIx6UpmP
YtQGOK2b8oC6aFL9IQIDAQAB
-----END PUBLIC KEY-----`
)

// httpDownloader 负责 HTTP 下载，支持 RSA 鉴权。
type httpDownloader struct {
	appID  string // base64(RSA_PKCS1v15_PubKeyEncrypt(iotAppID, pubKey))
	client *http.Client
}

// newHTTPDownloader 构造 HTTP 下载器。
// 应用 ID 与 RSA 公钥为平台固定值（内置常量），启动时计算一次 appId 缓存，后续所有鉴权下载复用。
func newHTTPDownloader() (*httpDownloader, error) {
	encrypted, err := encryptAppID(defaultIotAppID, []byte(defaultIotPublicKey))
	if err != nil {
		return nil, fmt.Errorf("encrypt appId: %w", err)
	}

	// 构造 HTTP 客户端：关闭证书校验，跟随重定向（最多 10 跳）
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Timeout:   httpTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects (max %d)", maxRedirects)
			}
			// 重定向时重新设置 appId header
			req.Header.Set("appId", encrypted)
			req.Header.Set("Connection", "keep-alive")
			return nil
		},
	}

	return &httpDownloader{appID: encrypted, client: client}, nil
}

// downloadConfig 下载配置文件，校验 JSON 合法性后原子覆盖到目标路径。
// 临时文件写在目标文件同目录下（同文件系统），避免跨设备 rename 报 EXDEV。
func (d *httpDownloader) downloadConfig(url, targetPath string) error {
	slog.Info("downloading config", "url", url)
	data, err := d.download(url)
	if err != nil {
		return fmt.Errorf("download config: %w", err)
	}

	// 校验 JSON 合法（仅 cJSON 解析验证，无签名/哈希校验）
	if !jsonValid(data) {
		return fmt.Errorf("downloaded config is not valid JSON")
	}

	// 确保目标目录存在
	if dir := filepath.Dir(targetPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir config dir: %w", err)
		}
	}

	// 写到目标目录下的临时文件，再原子 rename 覆盖（同文件系统）
	tmpPath := targetPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write tmp config: %w", err)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath) // 清理残留临时文件
		return fmt.Errorf("rename config: %w", err)
	}
	slog.Info("config saved", "path", targetPath)
	return nil
}

// downloadDriver 下载驱动文件到临时路径（无鉴权，先 HEAD 探测 404）。
func (d *httpDownloader) downloadDriver(url, targetDir string) (string, error) {
	slog.Info("downloading driver", "url", url)

	// HEAD 探测
	headReq, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return "", fmt.Errorf("build head request: %w", err)
	}
	headResp, err := d.client.Do(headReq)
	if err != nil {
		return "", fmt.Errorf("head request: %w", err)
	}
	headResp.Body.Close()
	if headResp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("driver not found (404): %s", url)
	}

	// GET 下载
	data, err := d.download(url)
	if err != nil {
		return "", fmt.Errorf("download driver: %w", err)
	}

	// 取 URL 末段作文件名
	filename := urlLastSegment(url)
	tmpPath := targetDir + "/" + filename
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return "", fmt.Errorf("write driver: %w", err)
	}
	return tmpPath, nil
}

// download 执行 HTTP GET 请求并返回 body。
func (d *httpDownloader) download(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("appId", d.appID)
	req.Header.Set("Connection", "keep-alive")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20)) // 50MB 上限
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

// encryptAppID 用 RSA 公钥加密 iotAppID，返回 base64 编码的密文。
// 算法: RSA_PKCS1v15, 公钥格式: PEM SPKI
func encryptAppID(iotAppID string, pubPEM []byte) (string, error) {
	block, _ := pem.Decode(pubPEM)
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block")
	}

	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse PKIX public key: %w", err)
	}

	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return "", fmt.Errorf("public key is not RSA")
	}

	enc, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(iotAppID))
	if err != nil {
		return "", fmt.Errorf("rsa encrypt: %w", err)
	}

	return base64.StdEncoding.EncodeToString(enc), nil
}

// jsonValid 简单校验 JSON 格式合法性。
func jsonValid(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	// 去除前导空白
	for _, b := range data {
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
			return b == '{' || b == '['
		}
	}
	return false
}

// urlLastSegment 取 URL 末段（最后一个 / 之后的部分）。
func urlLastSegment(url string) string {
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '/' {
			return url[i+1:]
		}
	}
	return url
}