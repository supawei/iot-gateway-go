package observability

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"iot-gateway-go/internal/core"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/values"
)

func TestPackageParsing(t *testing.T) {
	cases := []struct {
		fn   string
		want string
	}{
		{"iot-gateway-go/internal/processing.(*Engine).reload", "processing"},
		{"iot-gateway-go/internal/observability.AccessLog.func1", "observability"},
		{"main.fatal", "main"},
		{"log/slog.Info", "slog"},
	}
	for _, c := range cases {
		if got := lastSegment(packageOf(c.fn)); got != c.want {
			t.Fatalf("packageOf(%q)=%q want %q", c.fn, got, c.want)
		}
	}
}

// TestLoggerHandlerInjectsPublicFields 走真实 slog.Info 路径(slog 用校准好的 skip 取
// 调用方 PC),验证 gateway_id 与 component 经 handler 注入。这是生产用的机制。
func TestLoggerHandlerInjectsPublicFields(t *testing.T) {
	var buf bytes.Buffer
	h := NewLoggerHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h.SetGatewayID("gw-1")

	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(h))

	slog.Info("hello", "key", "val")

	line := buf.String()
	if !strings.Contains(line, "msg=hello") {
		t.Fatalf("missing msg: %s", line)
	}
	if !strings.Contains(line, "gateway_id=gw-1") {
		t.Fatalf("missing gateway_id: %s", line)
	}
	// slog.Info 的调用点在本测试(包 observability)
	if !strings.Contains(line, "component=observability") {
		t.Fatalf("missing component=observability: %s", line)
	}
}

func TestAccessLogReusesClientRequestID(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil)
	req.Header.Set("X-Request-ID", "req-abc")

	AccessLog(inner).ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-ID"); got != "req-abc" {
		t.Fatalf("echoed request id=%q want req-abc", got)
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d want %d", rec.Code, http.StatusAccepted)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "request_id=req-abc") {
		t.Fatalf("log missing request_id: %s", logged)
	}
	if !strings.Contains(logged, "status=202") {
		t.Fatalf("log missing status=202: %s", logged)
	}
	if !strings.Contains(logged, "path=/api/v1/devices") {
		t.Fatalf("log missing path: %s", logged)
	}
}

func TestAccessLogGeneratesRequestIDWhenMissing(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	rec := httptest.NewRecorder()
	AccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))

	rid := rec.Header().Get("X-Request-ID")
	if rid == "" {
		t.Fatal("missing generated request id in response header")
	}
	if !strings.Contains(logBuf.String(), "request_id="+rid) {
		t.Fatalf("log missing generated request_id=%s: %s", rid, logBuf.String())
	}
}

// fakeOutput 实现输出接口 + StatusProvider,供 metrics 测试构造 output.Manager。
type fakeOutput struct {
	rs output.RuntimeStatus
}

func (f *fakeOutput) Publish(model.DataPoint) error       { return nil }
func (f *fakeOutput) Close() error                        { return nil }
func (f *fakeOutput) RuntimeStatus() output.RuntimeStatus { return f.rs }

func TestMetricsHandler(t *testing.T) {
	mgr := output.NewManager(func() ([]output.Instance, error) {
		return []output.Instance{{
			Out: &fakeOutput{rs: output.RuntimeStatus{Connected: true, Sent: 7, Pending: 2}},
			ID:  "out-1", Name: "n", Type: "mqtt", Enabled: true,
		}}, nil
	})
	if err := mgr.Reload(); err != nil {
		t.Fatalf("reload outputs: %v", err)
	}
	defer mgr.Close()

	reg := status.NewRegistry()
	reg.SetOnline("d1", time.Now())
	reg.SetOffline("d2", "boom")

	c := NewCollector(reg, mgr, nil, nil, "", "")
	rec := httptest.NewRecorder()
	c.MetricsHandler(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type=%q", ct)
	}
	// 运行时
	mustContain(t, body, "iot_gateway_uptime_seconds")
	mustContain(t, body, "iot_gateway_go_goroutines")
	mustContain(t, body, `iot_gateway_info{version=`)
	mustContain(t, body, "# TYPE iot_gateway_info gauge")
	// 设备(1 在线 / 2 总数)
	mustContain(t, body, "iot_gateway_devices_online 1")
	mustContain(t, body, "iot_gateway_devices_total 2")
	mustContain(t, body, "# HELP iot_gateway_devices_online")
	// 输出(按 output_id 标签)
	mustContain(t, body, `iot_gateway_output_connected{output_id="out-1"} 1`)
	mustContain(t, body, `iot_gateway_output_sent_total{output_id="out-1"} 7`)
	mustContain(t, body, `iot_gateway_output_pending{output_id="out-1"} 2`)
	mustContain(t, body, `iot_gateway_output_queue_capacity{output_id="out-1"} 1024`)
	// proc/sched 为 nil -> 不输出对应指标族
	if strings.Contains(body, "iot_gateway_collect_total") {
		t.Fatalf("nil scheduler should suppress collect metrics:\n%s", body)
	}
	if strings.Contains(body, "iot_gateway_processing_points_in_total") {
		t.Fatalf("nil proc should suppress processing metrics:\n%s", body)
	}
}

func TestLivezHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	LivezHandler(ctx).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("livez before shutdown=%d want 200", rec.Code)
	}
	cancel()
	rec2 := httptest.NewRecorder()
	LivezHandler(ctx).ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("livez after shutdown=%d want 503", rec2.Code)
	}
}

func TestReadyzHandler(t *testing.T) {
	// nil scheduler -> not ready
	rec := httptest.NewRecorder()
	ReadyzHandler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz nil scheduler=%d want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not ready") {
		t.Fatalf("readyz body missing 'not ready': %s", rec.Body.String())
	}
	// fresh scheduler(cron 未启动)-> not ready
	sched := core.NewScheduler(nil, nil, 4, status.NewRegistry(), values.NewRegistry(), nil)
	rec2 := httptest.NewRecorder()
	ReadyzHandler(sched).ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz fresh scheduler=%d want 503", rec2.Code)
	}
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("metrics body missing %q\n--- body ---\n%s", want, body)
	}
}
