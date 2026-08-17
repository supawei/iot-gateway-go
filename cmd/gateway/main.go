package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"iot-gateway-go/internal/api"
	"iot-gateway-go/internal/auth"
	"iot-gateway-go/internal/config"
	"iot-gateway-go/internal/core"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/store"
	"iot-gateway-go/internal/values"
	"iot-gateway-go/web"

	_ "iot-gateway-go/internal/driver/modbus"        // 注册 modbus 驱动
	_ "iot-gateway-go/internal/driver/modbus_listen" // 注册 modbus 监听驱动
	_ "iot-gateway-go/internal/driver/opcua"         // 注册 opcua 驱动
	_ "iot-gateway-go/internal/output/mqtt"          // 注册 mqtt 输出
	_ "iot-gateway-go/internal/output/tdengine"      // 注册 tdengine 输出
	_ "iot-gateway-go/internal/output/thingsboard"   // 注册 thingsboard 输出
)

const (
	datapointBufferSize = 1024
	shutdownTimeout     = 5 * time.Second
)

func main() {
	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fatal("load config failed", "err", err)
	}
	slog.SetDefault(initLogger(cfg))

	st, err := store.Open(cfg.Storage.SqlitePath)
	if err != nil {
		fatal("open store failed", "err", err)
	}
	defer st.Close()

	// 下行写回调:共享属性/RPC → core.WritePoint → 驱动 Writer。
	write := func(ctx context.Context, deviceID, point string, value interface{}) error {
		_, err := core.WritePoint(ctx, st, deviceID, point, value)
		return err
	}
	// 输出管理器:从 SQLite 读配置动态构建,Web UI 变更后热重载(原子替换 + 关闭旧输出)。
	// 网关 ID 亦存 SQLite(settings 表),每次重载时读取,故改 ID 后热重载即生效。
	outputs := output.NewManager(buildOutputs(st, write))
	if err := outputs.Reload(); err != nil {
		// 首次构建失败不退出:API 仍可修复配置并触发热重载。
		slog.Warn("initial output reload failed", "err", err)
	}

	statusReg := status.NewRegistry()
	valuesReg := values.NewRegistry()
	dataPoints := make(chan model.DataPoint, datapointBufferSize)

	// 鉴权:预置默认管理员(首次登录强制改密),内存 session TTL 来自配置。
	sessionTTL := 24 * time.Hour
	if cfg.Auth.SessionTTL != "" {
		if d, err := time.ParseDuration(cfg.Auth.SessionTTL); err == nil && d > 0 {
			sessionTTL = d
		} else {
			fatal("invalid auth.sessionTtl", "value", cfg.Auth.SessionTTL)
		}
	}
	authz := auth.NewManager(st, sessionTTL)
	if created, err := authz.BootstrapAdmin(); err != nil {
		fatal("bootstrap admin failed", "err", err)
	} else if created {
		slog.Warn("bootstrap admin created, login and change the default password", "user", auth.DefaultAdminUser)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// pipelineDone := make(chan struct{})
	go func() {
		core.RunPipeline(ctx, dataPoints, outputs)
		// close(pipelineDone)
	}()

	// schedulerDone := make(chan struct{})
	go func() {
		if err := core.NewScheduler(st, dataPoints, cfg.Scheduler.PoolSize, statusReg, valuesReg, outputs).Run(ctx); err != nil {
			slog.Error("scheduler exited", "err", err)
		}
		// close(schedulerDone)
	}()

	// 根路由挂内嵌前端(SPA),/api/ 挂 REST 接口,单端口同时提供界面与 API。
	mux := http.NewServeMux()
	mux.Handle("/", web.Handler())
	authEnabled := true
	if cfg.Auth.Enabled != nil {
		authEnabled = *cfg.Auth.Enabled
	}
	mux.Handle("/api/", api.New(st, statusReg, valuesReg, authz, authEnabled, outputs).Routes())

	server := &http.Server{Addr: cfg.HTTP.Addr, Handler: mux}
	go func() {
		slog.Info("HTTP server listening", "addr", cfg.HTTP.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("http server failed", "err", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	server.Shutdown(shutdownCtx)

	// <-schedulerDone
	// <-pipelineDone
	outputs.Close()
}

// initLogger 按 config 构造 slog logger:level 控制级别,format 选 text/json,
// file.path 非空时同时写 stdout 与轮转文件(防爆盘)。
func initLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLogLevel(cfg.Log.Level)}

	writer := io.Writer(os.Stdout)
	if cfg.Log.File.Path != "" {
		writer = io.MultiWriter(os.Stdout, &lumberjack.Logger{
			Filename:   cfg.Log.File.Path,
			MaxSize:    cfg.Log.File.MaxSize,
			MaxBackups: cfg.Log.File.MaxBackups,
			MaxAge:     cfg.Log.File.MaxAge,
			LocalTime:  true,
			Compress:   cfg.Log.File.Compress,
		})
	}

	if strings.EqualFold(cfg.Log.Format, "json") {
		return slog.New(slog.NewJSONHandler(writer, opts))
	}
	return slog.New(slog.NewTextHandler(writer, opts))
}

func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

// buildOutputs 返回输出管理器的构建函数:每次重载都从 SQLite 读最新网关 ID 与输出配置,
// 逐个经 registry 构造为 Output 实例。任一输出构建失败即整体失败并关闭已构建部分,
// 由 Manager 保留旧输出(原子替换语义)。
func buildOutputs(st *store.Store, write output.WriteFunc) output.BuildFunc {
	return func() ([]output.Output, error) {
		gatewayID, err := st.GetGatewayID()
		if err != nil {
			return nil, err
		}
		bc := output.BuildContext{GatewayID: gatewayID, Write: write}
		configs, err := st.ListOutputs()
		if err != nil {
			return nil, err
		}
		result := make([]output.Output, 0, len(configs))
		for _, o := range configs {
			if !o.Enabled {
				continue
			}
			out, err := output.Build(bc, o.Type, o.Config)
			if err != nil {
				for _, built := range result {
					built.Close()
				}
				return nil, fmt.Errorf("build output %q: %w", o.ID, err)
			}
			result = append(result, out)
		}
		return result, nil
	}
}
