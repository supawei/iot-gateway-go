# 运维监控设计文档

> **状态**:已实现(指标/健康/日志三件套 + 测试 + 端到端编译)
> **关联**:[output-status-design.md](output-status-design.md)(可观测边界先例)、[alert-engine-design.md](alert-engine-design.md)(业务告警,非运维告警)
> **范围**:`internal/observability`、`internal/core/scheduler.go`(采集计数)、`cmd/gateway/main.go`(装配)

## 1. 目标与边界

P4 运维监控三件套:**指标采集** / **健康检查端点** / **结构化日志增强**。

沿用 `output-status-design` 立的可观测边界:

- 纯内存态快照,抓取时即时读,**不落历史指标、不持久化**;
- **不做运维阈值告警/告警推送**--网关只做"数据源",告警判定与投递交外部 Alertmanager;
- 不引入 `prometheus/client_golang` / OpenTelemetry,**纯标准库**手写,保持零监控依赖与静态二进制美学。

> 与 P3 规则告警的区别:规则告警是**业务数据告警**(点位越限,写 SQLite + 定向投递);运维告警(进程/磁盘/队列满)不在本层,留给外部。

## 2. 指标采集(`/metrics`)

### 2.1 端点

- `/metrics`,**根路径、匿名、始终开**(与 `/livez` `/readyz` 一致,便于 Prometheus 抓取);
- `Content-Type: text/plain; version=0.0.4`;
- 渲染器:`observability.Collector`,抓取时即时读各数据源内存态,无缓存。

### 2.2 指标清单

采集/调度(全局,无标签;`core.Scheduler.Stats()`):

| 指标 | 类型 | 来源 |
|---|---|---|
| `iot_gateway_collect_total` | counter | 轮询采集执行次数(scheduler.collectOnce 入口原子累加) |
| `iot_gateway_collect_errors_total` | counter | 采集失败次数(Read 失败或全部点位质量坏) |
| `iot_gateway_task_queue_length` | gauge | `len(taskCh)` |
| `iot_gateway_task_queue_capacity` | gauge | `cap(taskCh)`(2×poolSize) |

设备(全局;`status.Registry.List()`):

| 指标 | 类型 | 来源 |
|---|---|---|
| `iot_gateway_devices_online` | gauge | 在线设备数 |
| `iot_gateway_devices_total` | gauge | scheduler 已登记状态的设备数 |

处理(全局;`processing.Engine.Stats()`):

| 指标 | 类型 | 来源 |
|---|---|---|
| `iot_gateway_processing_points_in_total` / `_passed_total` / `_filtered_total` / `_aggregated_total` | counter | PointsIn/Pass/Filtered/Aggregated |

输出(按 `output_id` 标签;`output.Manager.Status()`):

| 指标 | 类型 | 来源 |
|---|---|---|
| `iot_gateway_output_connected` | gauge | RuntimeStatus.Connected |
| `iot_gateway_output_sent_total` | counter | RuntimeStatus.Sent |
| `iot_gateway_output_pending` | gauge | RuntimeStatus.Pending |
| `iot_gateway_output_dropped_total` | counter | OutputStatus.Dropped |
| `iot_gateway_output_queue_used` / `_queue_capacity` | gauge | QueueUsed / QueueCap |
| `iot_gateway_output_backlog` | gauge | 补传队列深度(Backfill) |

运行时(全局,Linux 专属、非 Linux 缺省):

| 指标 | 类型 | 来源 |
|---|---|---|
| `iot_gateway_uptime_seconds` | gauge | 进程运行时长 |
| `iot_gateway_info{version,commit}` | gauge | =1,构建信息 |
| `iot_gateway_go_goroutines` | gauge | `runtime.NumGoroutine` |
| `iot_gateway_process_rss_bytes` | gauge | `/proc/self/status` VmRSS |
| `iot_gateway_mem_total_bytes` | gauge | `/proc/meminfo` MemTotal(kB→字节) |
| `iot_gateway_mem_available_bytes` | gauge | `/proc/meminfo` MemAvailable(kB→字节,含可回收缓存) |
| `iot_gateway_mem_used_percent` | gauge | (MemTotal−MemAvailable)/MemTotal×100 |
| `iot_gateway_disk_total_bytes{path}` | gauge | `syscall.Statfs` Blocks×Bsize |
| `iot_gateway_disk_free_bytes{path}` | gauge | `syscall.Statfs` Bavail×Bsize(非 root 可用) |
| `iot_gateway_disk_used_percent{path}` | gauge | (Blocks−Bavail)/Blocks×100 |

> 内存/磁盘四项字节级指标(总/剩余)与 used_percent 同源同口径,便于 Grafana 直接
> 画"总量-剩余"面积图,无需 percent→byte 反推。

### 2.3 设计取舍

- **不做 per-device 标签**:设备数多会爆 Prometheus 基数;采集侧计数为全局。输出数量少,带 `output_id`。
- **不做采集延迟直方图**:手写直方图代价高,且 Modbus 轮询周期已知,延迟分布价值有限。需要时再引 `client_golang`。
- **磁盘路径**:只统计数据库与日志所在分区(关心点:会不会涨满),非全部挂载点;同分区则多条同值,基数极低可接受。

## 3. 健康检查端点(`/livez` `/readyz`)

- 均匿名、始终开,给负载均衡/k8s 探针用。
- `/livez`:`baseCtx` 未取消返 200,优雅退出期返 503(让探针停发流量)。
- `/readyz`:**store 已开 + 配置加载完成 + scheduler 调度器已启动** 返 200,否则 503。
  - store/config 在装配到挂路由阶段已必然为真,就绪判定主要看 `Scheduler.IsReady()`(pollScheduler 非空)。
  - **不含输出连接健康**:上游全断不应让网关被判 not-ready 而被负载均衡误杀。
- 响应体:小 JSON `{status, checks:{scheduler}}` 便于排障。

## 4. 结构化日志增强

基础:`log/slog` + lumberjack(stdout + 可选轮转文件,text/json)。

### 4.1 公共字段注入(`observability.LoggerHandler`)

包装底层 slog handler,给每条日志注入两个公共字段:

- `gateway_id`:启动从 store settings 读一次,经 `SetGatewayID` 注入;改 ID 后日志字段需重启生效(输出侧已热重载)。
- `component`:**由调用方 PC 推导所在包名**(`runtime.FuncForPC` + 名称解析),零调用点改动即让全仓日志带子系统标识。

> 选 PC 推导而非逐包 `slog.With("component",...)`,避免大面积改现有日志调用点(零侵入)。

### 4.2 访问日志中间件(`observability.AccessLog`)

- 仅挂在 `/api/` 下,覆盖全部 REST;前端 `/`、`/livez` `/readyz` `/metrics` 不入 access log(探针/抓取会刷屏)。
- `request_id`:**优先复用客户端 `X-Request-ID`**,缺省则生成 16 位十六进制;响应头回写。
- 字段:method / path / status / duration_ms / request_id / remote。
- `statusRecorder` 捕获最终状态码(含显式 WriteHeader 与隐式 200)。

### 4.3 不做

不引入 OpenTelemetry trace--对单二进制边缘网关 over-engineering,且无 trace 后端。

## 5. 采集侧侵入(`internal/core/scheduler.go`)

- `Scheduler` 新增 `collectCount` / `collectErrors`(`atomic.Int64`,worker 并发自增)。
- `collectOnce` 入口累加 `collectCount`,Read 失败与"全部点位质量坏"两条路径累加 `collectErrors`。
- 新增 `SchedulerStats` + `Stats()`(原子读计数 + 锁内读 taskCh len/cap)+ `IsReady()`(pollScheduler 非空)。
- 仅轮询路径在 collectOnce 计数;订阅/监听为持续推送无"执行次数",其数据计入 `processing_points_in_total`(emit 入口)。

## 6. 装配(`cmd/gateway/main.go`)

- `initLogger` 返回 `(*slog.Logger, *observability.LoggerHandler)`,handler 包装注入公共字段。
- store 打开后读 `gatewayID` 注入 `gidHandler.SetGatewayID`(并复用给告警引擎,删去原重复读取)。
- scheduler 保留 handle(`sched := NewScheduler(...); go sched.Run(ctx)`),供 metrics/readyz 查询。
- 路由:
  - `/api/` → `observability.AccessLog(apiSrv.Routes())`
  - `/livez` / `/readyz` / `/metrics` → 匿名根路径
- `Collector` 数据目录取 `filepath.Dir(sqlitePath)`、日志目录取 `filepath.Dir(logPath)`(空则只统计数据分区)。

## 7. 测试

- `observability` 包:`LoggerHandler` 公共字段端到端(真实 slog.Info 路径)、`AccessLog` request_id 复用/生成 + 状态捕获、`MetricsHandler` 输出格式与各指标族、`Livez`/`Readyz` 状态码、系统指标解析(`/proc` 解析 + 临时目录 Statfs),另含字节级系统指标渲染端到端(`mem_total/available_bytes`、`disk_total/free_bytes` 名称与 path 标签、非零值)。
- `core` 包:`Stats`/`IsReady` 与采集错误计数(成功路径 + Read 失败路径)。
