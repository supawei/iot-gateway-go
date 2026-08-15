package main

import (
	"context"
	"errors"
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
	"iot-gateway-go/internal/config"
	"iot-gateway-go/internal/core"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/output/mqtt"
	"iot-gateway-go/internal/output/tdengine"
	"iot-gateway-go/internal/output/thingsboard"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/store"
	"iot-gateway-go/internal/values"
	"iot-gateway-go/web"

	_ "iot-gateway-go/internal/driver/modbus"        // 注册 modbus 驱动
	_ "iot-gateway-go/internal/driver/modbus_listen" // 注册 modbus 监听驱动
	_ "iot-gateway-go/internal/driver/opcua"         // 注册 opcua 驱动
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

	outputs, err := buildOutputs(cfg, st)
	if err != nil {
		fatal("build outputs failed", "err", err)
	}

	statusReg := status.NewRegistry()
	valuesReg := values.NewRegistry()
	dataPoints := make(chan model.DataPoint, datapointBufferSize)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pipelineDone := make(chan struct{})
	go func() {
		core.RunPipeline(ctx, dataPoints, outputs)
		close(pipelineDone)
	}()

	schedulerDone := make(chan struct{})
	go func() {
		if err := core.NewScheduler(st, dataPoints, cfg.Scheduler.PoolSize, statusReg, valuesReg, outputs).Run(ctx); err != nil {
			slog.Error("scheduler exited", "err", err)
		}
		close(schedulerDone)
	}()

	// 根路由挂内嵌前端(SPA),/api/ 挂 REST 接口,单端口同时提供界面与 API。
	mux := http.NewServeMux()
	mux.Handle("/", web.Handler())
	mux.Handle("/api/", api.New(st, statusReg, valuesReg).Routes())

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

	<-schedulerDone
	<-pipelineDone
	for _, out := range outputs {
		out.Close()
	}
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

func buildOutputs(cfg config.Config, st *store.Store) ([]output.Output, error) {
	outputs := make([]output.Output, 0, 2)

	if cfg.MQTT.Broker != "" {
		mqttOutput, err := mqtt.New(cfg.MQTT, cfg.Gateway.ID)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, mqttOutput)
	}

	if cfg.ThingsBoard.Broker != "" && cfg.ThingsBoard.AccessToken != "" {
		// 下行写回调:共享属性更新 → core.WritePoint → 驱动 Writer。
		write := func(ctx context.Context, deviceID, point string, value interface{}) error {
			_, err := core.WritePoint(ctx, st, deviceID, point, value)
			return err
		}
		tbOutput, err := thingsboard.New(cfg.ThingsBoard, write)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, tbOutput)
	}

	if cfg.TDengine.URL != "" {
		tdOutput, err := tdengine.New(cfg.TDengine)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, tdOutput)
	}

	if len(outputs) == 0 {
		return nil, errors.New("no output configured: set mqtt.broker, thingsboard.broker+accessToken, or tdengine.url")
	}
	return outputs, nil
}
