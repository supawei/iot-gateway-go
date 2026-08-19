package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"iot-gateway-go/internal/alert"
	"iot-gateway-go/internal/api"
	"iot-gateway-go/internal/auth"
	"iot-gateway-go/internal/backfill"
	"iot-gateway-go/internal/config"
	"iot-gateway-go/internal/core"
	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/observability"
	"iot-gateway-go/internal/output"
	"iot-gateway-go/internal/processing"
	"iot-gateway-go/internal/status"
	"iot-gateway-go/internal/store"
	"iot-gateway-go/internal/values"
	"iot-gateway-go/internal/version"
	"iot-gateway-go/web"

	_ "iot-gateway-go/internal/driver/modbus"        // 注册 modbus 驱动
	_ "iot-gateway-go/internal/driver/modbus_listen" // 注册 modbus 监听驱动
	_ "iot-gateway-go/internal/driver/opcua"         // 注册 opcua 驱动
	_ "iot-gateway-go/internal/output/mqtt"          // 注册 mqtt 输出
	_ "iot-gateway-go/internal/output/smardaten"     // 注册 smardaten-iot 输出
	_ "iot-gateway-go/internal/output/sparkplugb"    // 注册 sparkplugb 输出
	_ "iot-gateway-go/internal/output/tdengine"      // 注册 tdengine 输出
	_ "iot-gateway-go/internal/output/thingsboard"   // 注册 thingsboard 输出
)

const (
	datapointBufferSize = 1024
	shutdownTimeout     = 5 * time.Second
)

// usageText 是 -h/--help 与参数解析错误的帮助文本,头部会拼接当前版本号。
const usageText = `用法:
  gateway [options] [config-path]

参数:
  config-path            配置文件路径(位置参数,默认 config.yaml)

选项:
  -h, -help, --help      显示程序当前版本与用法并退出
  -v, -version, --version
                         仅显示程序当前版本并退出
  -config <path>         指定配置文件路径(默认 config.yaml)

示例:
  gateway                使用默认 config.yaml 启动
  gateway /etc/gateway.yaml
                         使用指定配置文件启动
  gateway -h             查看当前版本与用法
`

// printUsage 输出版本信息与用法,供 -h/--help 及参数错误时调用。
func printUsage() {
	fmt.Fprintf(os.Stdout, "%s\n\n%s", version.String(), usageText)
}

// parseArgs 解析命令行参数,返回配置文件路径。帮助/版本请求会直接退出进程。
// 位置参数(兼容旧用法 ./gateway config.yaml)优先于 -config,再回退到默认值。
func parseArgs(args []string) (configPath string) {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // 错误文本由本函数自行处理,不打印 flag 包英文信息
	showVersion := fs.Bool("v", false, "print version and exit")
	fs.BoolVar(showVersion, "version", false, "print version and exit")
	configFlag := fs.String("config", "", "path to config file")
	fs.Usage = printUsage

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// flag 包遇到 -h/-help 时已调用 fs.Usage 输出版本与用法,直接退出即可。
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "gateway:", err)
		fmt.Fprintln(os.Stderr, "run 'gateway -h' for usage")
		os.Exit(2)
	}
	if *showVersion {
		fmt.Fprintln(os.Stdout, version.String())
		os.Exit(0)
	}

	configPath = "config.yaml"
	if *configFlag != "" {
		configPath = *configFlag
	}
	if fs.NArg() > 0 {
		configPath = fs.Arg(0)
	}
	return configPath
}

func main() {
	configPath := parseArgs(os.Args[1:])

	cfg, err := config.Load(configPath)
	if err != nil {
		fatal("load config failed", "err", err)
	}
	logger, gidHandler := initLogger(cfg)
	slog.SetDefault(logger)

	st, err := store.Open(cfg.Storage.SqlitePath)
	if err != nil {
		fatal("open store failed", "err", err)
	}
	defer st.Close()

	// 断网补传持久化队列(复用同一 SQLite,WAL):数据无法即时送出时落库,恢复后重放。
	// 见 docs/offline-backfill-design.md。
	backfillStore, err := st.NewBackfillStore(cfg.Storage.BackfillMax)
	if err != nil {
		fatal("init backfill store failed", "err", err)
	}

	// 网关 ID 存 SQLite(settings 表,Web UI 可改):启动读一次注入日志公共字段并供
	// 告警引擎使用。改 ID 后日志字段需重启生效(输出侧已热重载)。
	gatewayID, err := st.GetGatewayID()
	if err != nil {
		fatal("get gateway id", "err", err)
	}
	gidHandler.SetGatewayID(gatewayID)

	// 下行写回调:共享属性/RPC → core.WritePoint → 驱动 Writer。
	write := func(ctx context.Context, deviceID, point string, value interface{}) error {
		_, err := core.WritePoint(ctx, st, deviceID, point, value)
		return err
	}
	statusReg := status.NewRegistry()
	valuesReg := values.NewRegistry()

	// 输出管理器:从 SQLite 读配置动态构建,Web UI 变更后热重载(原子替换 + 关闭旧输出)。
	// 网关 ID 亦存 SQLite(settings 表),每次重载时读取,故改 ID 后热重载即生效。
	outputs := output.NewManager(buildOutputs(st, write, valuesReg, backfillStore))
	outputs.SetBackfill(backfillStore)
	if err := outputs.Reload(); err != nil {
		// 首次构建失败不退出:API 仍可修复配置并触发热重载。
		slog.Warn("initial output reload failed", "err", err)
	}

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

	// 告警引擎:跨设备/跨点位告警,接在边缘处理层下游;规则存 SQLite(alert_rules 表),
	// 随 store.OnChange 热重载。放行点先经告警判断(可能触发告警动作),再正常扇出。
	alertEng := alert.NewEngine(st, outputs, gatewayID)
	go alertEng.Run(ctx)

	// 边缘处理层:过滤/聚合采集数据,配置挂在设备点位上,随 store.OnChange 热重载;
	// 放行/派生点经告警引擎(触发告警 + 继续扇出)。见 docs/edge-computing-design.md。
	proc := processing.NewEngine(st, alertEng.Process)
	go proc.Run(ctx)
	go func() {
		core.RunPipeline(ctx, dataPoints, proc, outputs)
	}()

	// scheduler:用 cron 统一调度采集,任务投递到 worker pool。保留 handle 供
	// /metrics 与 /readyz 查询采集统计与就绪状态。
	sched := core.NewScheduler(st, dataPoints, cfg.Scheduler.PoolSize, statusReg, valuesReg, outputs)
	go func() {
		if err := sched.Run(ctx); err != nil {
			slog.Error("scheduler exited", "err", err)
		}
	}()

	// 根路由挂内嵌前端(SPA),/api/ 挂 REST 接口(经 access log 中间件);
	// /livez /readyz /metrics 为匿名运维端点(不进 access log、不鉴权)。
	mux := http.NewServeMux()
	mux.Handle("/", web.Handler())
	authEnabled := true
	if cfg.Auth.Enabled != nil {
		authEnabled = *cfg.Auth.Enabled
	}
	apiSrv := api.New(st, statusReg, valuesReg, authz, authEnabled, outputs)
	apiSrv.SetProcessing(proc)
	mux.Handle("/api/", observability.AccessLog(apiSrv.Routes()))

	// 运维监控:磁盘占用按数据库与日志所在分区统计。
	logPath := ""
	if cfg.Log.File.Path != "" {
		logPath = filepath.Dir(cfg.Log.File.Path)
	}
	collector := observability.NewCollector(statusReg, outputs, proc, sched, filepath.Dir(cfg.Storage.SqlitePath), logPath)
	mux.HandleFunc("/livez", observability.LivezHandler(ctx))
	mux.HandleFunc("/readyz", observability.ReadyzHandler(sched))
	mux.HandleFunc("/metrics", collector.MetricsHandler)

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
// file.path 非空时同时写 stdout 与轮转文件(防爆盘)。返回的 LoggerHandler 给每条
// 日志注入 gateway_id + component 公共字段;gateway_id 由 main 启动后经 SetGatewayID 注入。
func initLogger(cfg config.Config) (*slog.Logger, *observability.LoggerHandler) {
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

	var base slog.Handler
	if strings.EqualFold(cfg.Log.Format, "json") {
		base = slog.NewJSONHandler(writer, opts)
	} else {
		base = slog.NewTextHandler(writer, opts)
	}
	handler := observability.NewLoggerHandler(base)
	return slog.New(handler), handler
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
// 经 output.BuildSet 逐个构建。单个输出失败仅跳过(失败隔离),全失败才返回错误,
// 由 Manager 保留旧输出(原子替换语义)。
func buildOutputs(st *store.Store, write output.WriteFunc, valuesReg *values.Registry, backfillStore *backfill.Store) output.BuildFunc {
	return func() ([]output.Instance, error) {
		gatewayID, err := st.GetGatewayID()
		if err != nil {
			return nil, err
		}
		bc := output.BuildContext{
			GatewayID: gatewayID,
			Write:     write,
			Store:     st,
			// 断网补传:输出在失败路径上经它持久化待补送数据。
			Backfill: backfillStore,
			// 服务调用 get 等需要"设备当前属性值"的场景:从实时值注册表读取最新采集值。
			LatestValues: func(deviceID string) map[string]interface{} {
				dv := valuesReg.Get(deviceID)
				m := make(map[string]interface{}, len(dv.Points))
				for _, p := range dv.Points {
					if p.Value != nil {
						m[p.Point] = p.Value
					}
				}
				return m
			},
			// 设备诊断 DC1003:经 core.ProbeDevice 做真实协议往返探测设备可达性。
			Probe: func(ctx context.Context, deviceID string) error {
				return core.ProbeDevice(ctx, st, deviceID)
			},
		}
		configs, err := st.ListOutputs()
		if err != nil {
			return nil, err
		}
		return output.BuildSet(bc, configs)
	}
}
