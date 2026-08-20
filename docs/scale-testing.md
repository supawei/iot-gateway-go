# 规模化压测方案 (Scale Testing)

> **状态**:方案 + 可运行 harness(2026-08-20)
> **关联**:[hack/scalebench](../hack/scalebench/)、[ops-monitoring-design.md](ops-monitoring-design.md)(/metrics 指标口径)
> **目标**:验证网关在设备/点位规模扩大、采集频率升高时的**采集吞吐、输出吞吐、资源占用**与**瓶颈位置**,重点覆盖 32 位 ARM 网关盒的低内存场景。

## 1. 目标与范围

### 1.1 压测什么

网关数据链路(端到端,真实进程):

```
scheduler 周期采集(Modbus TCP 模拟从站)
   ──DataPoint──▶ 处理层(过滤/聚合) ──▶ 告警引擎 ──▶ 输出(MQTT 假 broker)
                                          │
                     SQLite(WAL:配置 + 断网补传 + 告警记录)
```

- **南向**:Modbus TCP 模拟从站(`modbus_sim`),支持 FC1-4 读 + FC6/16 写,逐连接独立端口(端点防冲突),统计请求/寄存器数。
- **北向**:记录型 MQTT 假 broker(复用 `internal/output/mqtttest`),应答 CONNACK/PUBACK/SUBACK,统计发布条数/topic。
- **被测对象**:真实 `gateway` 二进制(经 REST 批量配置、/metrics 采样),不是单元级 mock。

### 1.2 不测什么

- 协议设备真实性(模拟从站仅确定性应答,不含真实设备延迟/抖动);
- 公网 MQTT broker 吞吐(假 broker 零延迟);
- 长时间稳定性(建议 >72h 另跑,见 §8)。

## 2. 环境要求

| 项 | 要求 |
|---|---|
| Go | ≥ 1.25(与 go.mod 一致) |
| 前端 | `web/dist` 需存在(`go:embed` 内嵌);`make web` 或 `make build` 生成 |
| 端口 | harness 自动选空闲端口,不冲突 |
| 运行平台 | x86_64 开发机 / 目标 ARMv7 网关(交叉编译 harness 与 gateway 后在同机运行) |

## 3. 快速开始

```bash
# 默认规模:500 设备 × 4 点位 × 5 连接,1s 轮询,pool=16,30s 采样
go run ./hack/scalebench

# 完整选项
go run ./hack/scalebench \
  -devices 1000 -points 4 -conns 5 -interval-ms 1000 -pool 32 \
  -mqtt -alerts 50 \
  -warmup 30s -duration 60s -step 2s \
  -out /tmp/bench.csv
```

`go run ./hack/scalebench -h` 查看全部参数。harness 会自动 `go build` 网关二进制到临时目录。

**建议预热时长**:让**全部设备先上线**再进采样窗口,否则采样区间会混入"连接建立/首轮采集"的爬坡段(见 §6 发现)。经验值:`-warmup ≥ 设备数 × 轮询周期 + 5s`(200 设备 @1s ≈ 35s)。

**CI 冒烟**:`make smoke` 跑 200 设备 @1s 快速回归,`-min-rate` / `-require-online` 低于阈值即失败退出——已接入 CI(见 [.github/workflows/ci.yml](../.github/workflows/ci.yml)),防调度/采集类功能回归(如 robfig/cron 子秒轮询 bug,见 §6.1)。

## 4. 参数说明

| 参数 | 默认 | 说明 |
|---|---|---|
| `-devices` | 500 | 设备总数 |
| `-points` | 4 | 每设备点位数量(保持寄存器,float32) |
| `-conns` | 5 | Modbus 连接数(每条连接独立模拟从站端口;设备轮询分布其上) |
| `-interval-ms` | 1000 | 轮询周期(毫秒) |
| `-pool` | 16 | scheduler worker pool 大小 |
| `-mqtt` | off | 启用 MQTT 输出到假 broker(测北向吞吐) |
| `-alerts` | 0 | 附加告警规则数(引用点位、阈值 200000 不触发,仅测求值开销) |
| `-warmup` | 10s | 预热(等待全部设备上线或到期) |
| `-duration` | 30s | 采样窗口 |
| `-step` | 2s | 采样间隔 |
| `-gateway` | 自动 | 网关二进制路径(缺省自动构建) |
| `-out` | 空 | CSV 汇总报告路径 |
| `-min-rate` | 0 | 采集速率下限(次/秒),低于则失败退出(CI 冒烟断言);0=不检查 |
| `-require-online` | off | 采样结束要求全部设备在线,否则失败退出(CI 冒烟断言) |

## 5. 场景矩阵

| 场景 | 目标 | 建议参数 |
|---|---|---|
| A 规模扫描 | 设备数 ↑ 时吞吐/内存曲线 | `-devices 100/500/1000/2000`, `-interval-ms 1000`, `-mqtt`, `-warmup` 按 §3 放量 |
| B 频率扫描 | 采集频率 ↑ 时的上限 | `-interval-ms 1000/500/200`, 固定设备数 |
| C pool 扫描 | worker 池大小对吞吐/队列的影响 | `-pool 8/16/32/64`, 固定规模 |
| D 输出压力 | 北向发布速率与背压 | `-mqtt` + 大 `-points`/高频率;看 broker 收到条数、`output_pending/dropped` |
| E 边缘+告警开销 | 处理层与告警求值的 CPU 影响 | `-alerts 100` + 高频率;看 `/metrics` 处理层计数 |
| F 断连韧性 | 南向/北向故障下的行为 | 运行中手动 kill 模拟从站 / 假 broker,观察补传队列(进阶:另写脚本) |
| G ARMv7 内存 | **低内存盒子的 RSS 上限**(重点) | 见 §7 |

**对比基线**:固定一套参数(如 A 场景 500 设备),跨版本/跨改动记录 `-out` CSV,保证可比性。

## 6. 指标解读

输出示例(200 设备 @1s,pool 64,预热 35s):

```
collect 速率   = 199.7 次/秒(≈ 设备数 × 1/周期,达预期)
在线设备       = 200/200
采集错误       = 0
预估数据点吞吐 ≈ collect速率 × points 点/秒
Modbus 读请求  = 199.7 请求/秒(模拟从站实测)
假 broker 收到 = N 条/秒(仅 -mqtt)
RSS            = X MB(进程常驻,重点看 ARM)
任务队列 len   = 0(非 0 或逼近 cap 说明 pool 不够)
```

- `collect_total` / `iot_gateway_collect_errors_total`:调度器每次设备轮询 +1,读失败 +1。**速率应 ≈ 设备数 / 周期(秒)**;明显偏低排查:预热不足(爬坡)、pool 过小(队列满)、南向慢。
- `iot_gateway_task_queue_length`:逼近 `2×pool` 说明 worker 不够,应放大 `-pool`。
- `iot_gateway_output_pending/dropped_total`:北向背压;非 0 说明输出跟不上或 broker 慢。
- 处理层计数:`processing_points_in/passed/filtered/aggregated_total` 验证边缘计算在压测下不丢路径。
- 内存:`process_rss_bytes` 是产品化关键指标(尤其 32 位 ARM)。

### 6.1 已发现的现象(专项调查结论)

- **子秒轮询未生效 → 已修复(2026-08-20)**。
  - **根因**:**robfig/cron v3.0.1 限制**,非网关用法问题。`@every <d>` 解析后走 `ConstantDelaySchedule.Every()`:
    - `<1s` 的间隔**钳到 1s**(`constantdelay.go:15-17`);
    - 任意间隔的**亚秒余量被静默截掉**(`Delay = d - d%1s`,故 `@every 1500ms` 实际 = 1s);
    - `Next()` 对齐秒边界(截断纳秒),同间隔设备在同一秒边界**集体触发**。
  - 独立探针实测:同一 `cron.New()` 下 `@every 500ms / 1000ms / 1500ms` 在 3.2s 内**均只触发 3 次**(=1 次/秒),证实上述行为。
  - **修复**:`internal/core/pollsched.go` 新增 `pollScheduler` 取代 cron——单 goroutine 定时器 + 小顶堆,**精确支持亚秒间隔、不截断余量**,并按 deviceID 哈希**相位错峰**(消除秒边界惊群),触发仍非阻塞投递 worker pool。`Scheduler` 轮询路径已切换。
  - **修复后复测**:30 设备 `-interval-ms 500` → **59.9 次/s**(原 30/s);200 设备 `@1s` → 200.3 次/s(原 199.7/s,错峰后更平顺),0 错误、全部在线、任务队列 0/cap。
- **配置爬坡**:大批量 REST 建设备时,scheduler 增量 reconcile 逐设备接入,全部上线耗时 = 设备数 × 每设备连接/首采时间。压测必须等全部上线再采样(见 §3 预热指引),否则速率被低估(200 设备预热不足时测得 62/s,充分预热后 200/s)。

### 6.2 压测基线结果(2026-08-20)

> 环境:x86_64 开发机(内存 256GB,实测占用 ~94%)、loopback 网络;网关为当前 develop 二进制(含 pollScheduler);南向为本地 Modbus 模拟从站,北向为记录型假 broker(零延迟)。CSV 见 `/tmp/bench-*.csv`(第 1/2 组未传 `-out`,无 CSV)。

| # | 场景 | 连接 | 点/设备 | pool | collect 速率 | 数据点/s | MQTT 条/s | 错误 | 在线 | RSS | goroutines | 队列 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 30 设备 @500ms | 3 | 1 | 32 | 59.9 | 60 | — | 0 | 30/30 | 23.9MB | 49 | 0/64 |
| 2 | 200 设备 @1s | 5 | 4 | 64 | 200.3 | 801 | — | 0 | 200/200 | 24.0MB | 80 | 0/128 |
| 3 | 2000 设备 @1s | 5 | 1 | 64 | 2000.5 | 2000 | — | 0 | 2000/2000 | 36.0MB | 80 | 0/128 |
| 4 | 2000 设备 @500ms | 5 | 1 | 64 | 3999.4 | 3999 | — | 0 | 2000/2000 | 35.9MB | 80 | 0/128 |
| 5 | 2000 设备 @1s | 20 | 4 | 64 | 2002.1 | 8008 | — | 0 | 2000/2000 | 38.9MB | 80 | 0/128 |
| 6 | 2000 设备 @1s + MQTT | 20 | 4 | 64 | 1997.6 | 7990 | 7990.7 | 0 | 2000/2000 | 38.0MB | 91 | 0/128 |

**结论**:
- **吞吐精确线性扩展**:collect 速率 = 设备数 × 1/周期,200→2000 设备(×10)、@1s→@500ms(×2)均命中理论值,零掉速;
- **规模 × 亚秒成立**:2000 设备 @500ms = 4000 次/s,队列保持 0/cap(相位错峰消除秒边界惊群);
- **常驻 goroutine 与设备/连接数解耦**:恒 80(加 MQTT 客户端 +11 = 91)——单 goroutine pollScheduler + 连接池化设计在 2000 设备量级成立;
- **内存增量 ~6KB/设备**:200 设备 24MB → 2000 设备 36MB;4 点位也仅 +3MB,远低于 ARM 盒子 256–512MB 预算;
- **配置爬坡快**:2000 设备 REST 并发创建 + 全上线 ~2s;
- **MQTT 即时模式(flushInterval 留空)下 8000 点/s = 8000 条 QoS1 消息/s**,同步等 PUBACK、零 dropped/pending——发布管线非瓶颈。

**MQTT 模式说明**:第 6 组为**即时模式**(每点一条、同步确认)。生产可开**批量模式**(`flushInterval: 200ms` → 同设备窗口内多点聚合为一条消息,消息数降约 200 倍,broker 压力与网络开销显著降低,吞吐天花板更高)。

**边界(未覆盖)**:公网真实 broker 的网络 RTT 影响、断连韧性(场景 F)、告警/边缘计算开销(场景 E)、ARMv7 内存上限(§7)、长稳(§8)。

## 7. ARMv7 专项(重点)

32 位 ARM 网关盒**内存受限**(256–512MB 常见),modernc.org/sqlite 转译库内存占用偏大(见 [armv7-compatibility-review.md](armv7-compatibility-review.md))。压测重点:

1. 交叉编译 harness 与 gateway:`GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o scalebench-arm ./hack/scalebench`,连同 `gateway-armv7` 一起拷到目标板。
2. 同机运行(harness 与 gateway 同一进程树上),采样 `-step 1s`。
3. **重点指标**:`RSS`、`systemMem 占用`、`goroutines`;对比 x86 基线,确认规模上限(如 500 vs 1000 设备时 RSS 是否突破内存预算)。
4. **SQLite WAL 专项**:压测期间观察 `gateway.db-wal` 大小(补传/告警写入),确认不超过闪存/内存预算;`-alerts` 与断网补传是主要写源。
5. 若 RSS 超预算:减 `-points`、加 `-interval-ms`、或评估 `-pool` 对 goroutine 数的影响(worker 池 = goroutine 上限之一)。

## 8. 进阶:长稳测试

harness 适合分钟级吞吐基准;长稳(热泄漏/内存慢涨/SQLite 无限增长)建议:

- 连续运行 72h+ 同参数,`-duration 3600s -step 60s`,定期记录 RSS/goroutines/WAL;
- 观察 `iot_gateway_go_goroutines` 是否单调上涨(goroutine 泄漏)、RSS 是否慢涨(内存泄漏)、WAL/补传表是否无界增长。

## 9. 结果记录模板

每次压测建议记录:

| 字段 | 值 |
|---|---|
| 版本/commit | `git log -1 --oneline` |
| 平台 | x86_64 / armv7l,内存 |
| 参数 | devices/points/conns/interval/pool/mqtt/alerts |
| collect 速率 & 错误 | |
| 在线设备 | |
| 数据点吞吐 / MQTT 条率 | |
| RSS / goroutines / 任务队列 | |
| WAL 大小(长稳) | |

`-out bench.csv` 自动产出机器可读首行,便于脚本批量对比。

## 10. 调优指引(依据指标)

| 现象 | 手段 |
|---|---|
| 队列逼近 cap / collect 掉速 | 增大 `-pool`(scheduler.poolSize) |
| 子秒轮询只有 1/s | 已修复:pollScheduler 取代 cron(§6.1);若复现,先确认配置 intervalMs 确实 <1000 |
| 输出 pending/dropped 非 0 | 增大输出批量(`flushInterval`/`batchMax`)或排查 broker |
| RSS 逼近预算(ARM) | 减点位/降频;检查 SQLite 页缓存与补传队列上限(`backfillMax`) |
| goroutines 慢涨 | 检查驱动连接/订阅/补传 goroutine 是否泄漏(长稳) |
