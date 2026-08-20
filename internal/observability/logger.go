package observability

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync/atomic"
)

// LoggerHandler 包装底层 slog handler,给每条日志注入两个公共字段:
//   - gateway_id:由 SetGatewayID 注入(启动读到网关 ID 后设一次),此前为空;
//   - component:在 Handle 时动态走调用栈,跳过 log/slog 与 stdlib 帧,取首个本模块帧的包名。
//
// 动态走栈而非读 slog 预抓的 r.PC:后者用固定 runtime.Callers(3),对 fatal->slog.Error
// 这类被内联的包装器会错位。这里跳过**所有**非本模块帧(stdlib / slog 包装器 / 内联残留),
// 取首个 iot-gateway-go/* 或 main 包帧,对内联稳健。零调用点改动即让全仓日志带子系统标识。
type LoggerHandler struct {
	delegate  slog.Handler
	gatewayID *atomic.Pointer[string]
}

// modulePrefix 是本模块的导入路径前缀,用于区分本仓代码与 stdlib 帧。取自 go.mod 的
// module 名,稳定且可查,不为它单跑 runtime.Callers。
const modulePrefix = "iot-gateway-go/"

// NewLoggerHandler 包装底层 handler。gateway_id 初始为空,待 SetGatewayID 注入。
func NewLoggerHandler(delegate slog.Handler) *LoggerHandler {
	return &LoggerHandler{delegate: delegate, gatewayID: &atomic.Pointer[string]{}}
}

// SetGatewayID 设置日志公共字段 gateway_id。启动从 store 读到网关 ID 后调用一次。
// 所有经该 handler(含 WithAttrs/WithGroup 派生)的日志都会带上。
func (h *LoggerHandler) SetGatewayID(id string) {
	h.gatewayID.Store(&id)
}

func (h *LoggerHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.delegate.Enabled(ctx, level)
}

func (h *LoggerHandler) Handle(ctx context.Context, r slog.Record) error {
	var attrs []slog.Attr
	if p := h.gatewayID.Load(); p != nil {
		attrs = append(attrs, slog.String("gateway_id", *p))
	}
	if comp := deriveComponent(); comp != "" {
		attrs = append(attrs, slog.String("component", comp))
	}
	if len(attrs) > 0 {
		r.AddAttrs(attrs...)
	}
	return h.delegate.Handle(ctx, r)
}

// WithAttrs / WithGroup 委托底层并复用同一 gatewayID 指针,保证派生 logger 也注入公共字段。
func (h *LoggerHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LoggerHandler{delegate: h.delegate.WithAttrs(attrs), gatewayID: h.gatewayID}
}

func (h *LoggerHandler) WithGroup(name string) slog.Handler {
	return &LoggerHandler{delegate: h.delegate.WithGroup(name), gatewayID: h.gatewayID}
}

// deriveComponent 走当前 goroutine 调用栈,跳过 runtime.Callers / 本方法 / Handle 与所有
// stdlib 帧(含 log/slog 包装器、内联残留),取首个本模块(iot-gateway-go/*)或 main 包帧。
// 用 runtime.CallersFrames 逐帧展开(含内联):被内联进用户帧的 stdlib 函数(如
// bytes.Buffer.String)会先占一帧、随后跟一帧其外层用户函数;逐帧展开必能命中用户帧,
// 对常规内联与 -race(禁内联)都稳健。
func deriveComponent() string {
	var pcs [64]uintptr
	n := runtime.Callers(3, pcs[:]) // 跳过 [runtime.Callers, deriveComponent, Handle]
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if f.Function == "" {
			break
		}
		pkg := packageOf(f.Function)
		if isUserPackage(pkg) {
			return lastSegment(pkg)
		}
		if !more {
			break
		}
	}
	return ""
}

// isUserPackage 判断包是否属于本模块或 main 包(其余视为 stdlib/slog 噪声跳过)。
func isUserPackage(pkg string) bool {
	return pkg == "main" || (modulePrefix != "" && strings.HasPrefix(pkg, modulePrefix))
}

// packageOf 从函数全名取导入路径(第一个点号前)。
// iot-gateway-go/internal/processing.(*Engine).reload -> iot-gateway-go/internal/processing
// main.fatal -> main;log/slog.Info -> log/slog
func packageOf(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// lastSegment 取导入路径最后一段作 component。
// iot-gateway-go/internal/processing -> processing;main -> main
func lastSegment(pkg string) string {
	if i := strings.LastIndexByte(pkg, '/'); i >= 0 {
		return pkg[i+1:]
	}
	return pkg
}
