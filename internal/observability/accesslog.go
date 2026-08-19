package observability

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const requestIDHeader = "X-Request-ID"

// statusRecorder 包装 ResponseWriter 捕获最终状态码,供 access log 记录。
// 隐式 WriteHeader(200)(handler 只写 body 未显式设状态)时保持默认 200,无需特殊处理。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// AccessLog 是 REST 访问日志中间件:复用客户端 X-Request-ID,缺省则生成;
// 回写响应头;记 method/path/status/duration/request_id/remote。
// 仅挂在 /api/ 下,故健康与指标端点(/livez /readyz /metrics)不入 access log。
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get(requestIDHeader)
		if rid == "" {
			rid = newRequestID()
		}
		w.Header().Set(requestIDHeader, rid)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		slog.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", rid,
			"remote", r.RemoteAddr,
		)
	})
}

// newRequestID 生成 16 位十六进制 id;crypto/rand 失败时退用纳秒时间戳兜底。
func newRequestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf[:])
}
