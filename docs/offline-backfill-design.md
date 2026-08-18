# 断网本地补传(离线缓存)设计文档

## 1. 背景与问题

P1/P2 验收后,北向输出的数据在以下路径上**会被丢弃**,不满足工业网关"采集数据不丢"的核心承诺:

| 丢点路径 | 位置 | 现状 |
|---|---|---|
| 扇出队列满 | `output.Manager.Publish`(`slot.ch` 满) | 直接丢弃并告警(`queueSize=1024`) |
| 输出内存缓冲满 | `thingsboard`/`smardaten` 的 `maxPendingPoints=8192` | 丢弃新点并告警 |
| 上送失败 | 各输出 flusher 中 `publish`/`exec` 失败 | 仅记日志,批次点丢失 |

P2 已知简化中已明确:"ThingsBoard 断网本地补传…归 P3"、ROADMAP P3 项"断网本地补传:网络中断时缓存,恢复后补送,保证采集数据不丢(ThingsBoard/TDengine 均需)"。

## 2. 设计目标与非目标

### 2.1 目标

- **数据不丢**:任一输出在"无法即时送出"(断连 / 上送失败 / 缓冲满)时,把数据点持久化到 SQLite,恢复后按序补送。
- **持久化**:补传队列随网关重启存活(存于既有 `gateway.db`),重启后自动续传。
- **顺序补送**:按入队先后(近似采集先后)补送;数据自带时间戳,平台侧可据此还原。
- **内存有界**:断连期间不再依赖内存缓冲兜底,持久化队列有全局上限(磁盘有界)。
- **与现有背压/热重载架构兼容**:不阻塞采集侧、不拖慢其他输出;输出热重载(重建实例)不丢队列。

### 2.2 非目标

- **严格恰好一次(exactly-once)**:采用 at-least-once。ThingsBoard 按 `ts` 覆盖、TDengine 主键含 `ts`、smardaten 按属性最新值上报,重复投递无害。
- **跨输出去重 / 事务性多输出**:每输出独立队列。
- **补传粒度策略**(如按设备/点位):先按"逐条 DataPoint + 输出自身聚合"实现,后续按需增强。
- 输出构建失败瞬间的配置校验类错误:不属于"断网",不入队。

## 3. 方案总览

```
scheduler → dataPoints → Manager.Publish ──(每 slot)──▶ slot.ch ──▶ slot goroutine ──▶ Out.Publish
                                │ 队列满                               │ 内存缓冲满 / 上送失败
                                ▼                                      ▼
                         backfill.Store ◀── BackfillSink.Save(outputID, dps)  (各输出注入)
                                │
        Manager.runReplay ──────┘  (每 1s:健康时 Peek 一批 → 经 slot.ch 重放 → 成功 Ack)
```

- **持久化队列**:`internal/backfill` 包,SQLite 表 `backfill_queue`,同一 `gateway.db`(WAL)。
- **失败入队**:Manager 扇出队列满、输出内存缓冲满、输出上送失败,三处统一经 `BackfillSink.Save` 落库。
- **重放**:Manager 驱动(集中式),以 `BackfillHealthy`(输出可选能力)判断"当前可上送";重放点经 `slot.ch` 投递,复用 slot 单写 goroutine,保证并发安全与顺序。
- **确认删除**:点经 `slot.ch` 成功投递(即已被输出接收并缓冲/再处理)后 `Ack` 删除;若输出后续仍失败,失败路径会再次入队,数据不丢(at-least-once)。

## 4. 详细设计

### 4.1 `internal/backfill` 持久化队列

表结构(建表随既有 `schema` 外的独立 `CREATE TABLE IF NOT EXISTS`,由 `store.NewBackfillStore` 触发):

```sql
CREATE TABLE IF NOT EXISTS backfill_queue (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    output_id  TEXT    NOT NULL,
    payload    TEXT    NOT NULL,   -- model.DataPoint 的 JSON
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_backfill_output ON backfill_queue (output_id, id);
```

API:

```go
type Store struct { db *sql.DB; max int }
type Item struct { ID int64; DP model.DataPoint }

func New(db *sql.DB, max int) (*Store, error)      // max<=0 时用默认 100_000
func (s *Store) Save(outputID string, dps []model.DataPoint) (saved int, err error)
    // 插入后若总数超 max,按 id 顺序删除最旧超量行(保最新数据),并告警。
func (s *Store) CountByOutput(outputID string) (int, error)
func (s *Store) TotalCount() (int, error)
func (s *Store) Peek(outputID string, limit int) ([]Item, error)  // 按 id 升序(最旧优先)
func (s *Store) Ack(outputID string, ids []int64) error
func (s *Store) DropOutput(outputID string) error                  // 输出被删除时清队
```

`payload` 为 `model.DataPoint` 的 JSON(`encoding/json`),`Time`/`Quality`/`Value(interface{})` 均天然可往返。

**上限与淘汰**:默认 `max=100_000` 条(约 50MB 量级),可经 `storage.backfillMax` 配置。`Save` 在事务内先插后删,保"最新 N 条",避免断连超长期间磁盘无限增长。

### 4.2 `output` 包:接口与 BuildContext

```go
// output/backfill.go
type BackfillSink interface {
    Save(outputID string, dps []model.DataPoint) error
}
// BackfillHealthy 是输出的可选能力:报告当前是否处于"可正常上送"状态。
// Manager 仅在 healthy 时对该输出的补传队列执行重放。
type BackfillHealthy interface {
    BackfillHealthy() bool
}
```

`BuildContext` 增加两字段:

```go
type BuildContext struct {
    ...
    OutputID string        // 当前输出实例的配置 ID(BuildSet 逐个构建时注入)
    Backfill BackfillSink  // 断网补传持久化(由 main 注入 backfill.Store)
}
```

`BuildSet` 在循环内为每条配置临时拷贝 `bc` 并写入 `bc.OutputID = o.ID` 再 `Build`,输出构造器从 `bc.OutputID`/`bc.Backfill` 取用。

### 4.3 Manager:扇出满入队 + 集中重放

- 构造:main 先建 `backfill.Store`,`NewManager(build)` 后 `m.SetBackfill(bs)`。`backfill == nil` 时行为与旧版完全一致(测试/未接线场景兼容)。
- **扇出队列满**:`Manager.Publish` 中 `slot.ch` 满时,若该输出实现 `BackfillHealthy`(即支持补传)→ `backfill.Save(slot.ID, [dp])`;否则保持原丢弃逻辑。
- **重放循环**:`NewManager` 启动 `runReplay` goroutine,每 `replayInterval=1s`:

```
for each slot:
    out 未实现 BackfillHealthy            → 跳过
    !out.BackfillHealthy()                → 跳过(连接不可用/最近失败退避)
    backfill.CountByOutput(slot.ID)==0    → 跳过
    items = backfill.Peek(slot.ID, replayBatch=256)
    for each item: select { case slot.ch <- dp: acked=append(id); default: break }
    backfill.Ack(slot.ID, acked)          // 成功投递(输出已接收)即确认
```

  重放点经 `slot.ch` 走 slot 的**单写 goroutine**,不并发调用 `Out.Publish`;channel 满即停止本轮(背压),下轮续传。输出接收后若仍失败,失败路径会再次入队,保证不丢。
- **热重载**:`Reload` 构建成功后,对"旧输出集中不在新输出集的 outputID"调用 `backfill.DropOutput`(用户删了该输出,队列一并清掉);输出编辑/重建(同 ID)自动续传。
- **优雅关闭**:`Close` **不**清队列——补传队列持久化在 SQLite,下次启动自动续传。
- **运行态**:`OutputStatus` 增加 `Backfill int`(该输出补传队列深度),API `/outputs/status` 自动带出。

### 4.4 各输出:失败入队 + BackfillHealthy

**ThingsBoard** (`internal/output/thingsboard`):

- 构造新增 `outputID`/`backfill` 字段(来自 `bc.OutputID`/`bc.Backfill`,测试可传空)。
- `Publish`:内存缓冲满 → 解锁后 `saveBackfill([dp])`(不再丢弃)。
- `flush`:设备 connect 失败 或 telemetry publish 失败 → `saveBackfill(points)`(该批点在取出 pending 后失败,必须落库)。
- `BackfillHealthy() bool` = `o.client.IsConnected()`(paho 重连期间 false → 不重放)。

**TDengine** (`internal/output/tdengine`):

- `flush`:建子表失败 或 INSERT 失败 → `saveBackfill(groups[k])`。
- `BackfillHealthy() bool` = 最近一次上送无错误 或 距上次错误已超 `backfillBackoff=30s`(HTTP 无长连接,按错误时效判定,天然退避)。

**smardaten-iot** (`internal/output/smardaten`):

- `Publish`:内存缓冲满 → `saveBackfill([dp])`。
- `flush`:属性上报 publish 失败 → `saveBackfill(points)`。
- `BackfillHealthy() bool` = `o.client.IsConnected()`。

三个输出统一 `saveBackfill` 辅助方法:`backfill == nil` 时仅告警(兼容直接构造的旧测试);入队失败时告警(极端磁盘故障,保进程存活)。

### 4.5 main / store / config 接线

- `store.Store` 新增 `NewBackfillStore(max int) (*backfill.Store, error)`(复用同一 `*sql.DB`,WAL/busy_timeout 生效)。
- `main`:启动后 `bs, err := st.NewBackfillStore(cfg.Storage.BackfillMax)`;`outputs.SetBackfill(bs)`;`buildOutputs` 的 `bc.Backfill = bs`。
- `config`:新增 `storage.backfillMax`(默认 100000,≤0 用默认)。

## 5. 常量与参数汇总

| 常量 | 默认值 | 说明 |
|---|---|---|
| `backfill.DefaultMax` | 100000 | 每输出补传队列上限(超限丢最旧) |
| `Manager.replayInterval` | 1s | 重放轮询间隔 |
| `Manager.replayBatch` | 256 | 每轮每输出最多重放条数 |
| `tdengine.backfillBackoff` | 30s | 最近错误后暂停重放的时长 |

## 6. 行为变化与兼容性

- **数据不再静默丢失**:三个失败路径全部改为"入队 + 重放",旧的"drop datapoint"告警只保留在 `backfill==nil`(未接线)或入队失败(磁盘故障)场景。
- `OutputStatus` 新增 `backfill` 字段,`/outputs/status` 响应向后兼容(新增字段)。
- 输出热重载:同 ID 重建自动续传;删除输出时清其队列。
- 优雅关闭不丢队列:重启后续传。
- 未接线 backfill(`NewManager` 后不 `SetBackfill`):行为与旧版一致,全部测试兼容。

## 7. 风险与已知限制

- **at-least-once**:极端下(如重放点已在 channel 中、输出又失败且再入队)可能重复投递;按各平台语义可接受(见 §2.2)。
- **crash 窗口**:点被 `Ack`(进入内存缓冲)后进程崩溃 → 该点丢失。这与常规内存缓冲一致;未 `Ack` 的点(仍在库)不会丢。
- **断连期间磁盘写入**:采集高频时 `Save` 为每批事务;SQLite WAL + busy_timeout 已适配,但仍受磁盘性能约束(每 200ms~1s 一批,量级可接受)。
- **smardaten 变化上报模式**:重放值经 `buildPropertyReport` 走既有变化语义,未变化值可能被平台去重跳过(平台已持有最新值,语义合理)。
- **输出被删除即清队**:若需保留历史补传数据,应先暂停采集再删配置(极端场景,当前不提供)。

## 8. 测试计划

### 8.1 单元测试(`internal/backfill`)

- Save/Count/Peek 顺序(先入先出)、Ack 后不再返回、Ack 幂等(不存在 id 静默)。
- 上限淘汰:超 `max` 丢最旧保最新。
- JSON 往返:Value 数值/字符串/时间戳/Quality 还原一致。
- DropOutput 只清指定 output。

### 8.2 输出集成测试

- thingsboard:缓冲满 → 入队(不丢);flush 失败(断连) → 入队;`BackfillHealthy` 随 `IsConnected` 变化。
- tdengine:INSERT 失败 → 入队;`BackfillHealthy` 在错误后 30s 内为 false。
- Manager:扇出满 → 入队;重放把队中点经 channel 投递并 Ack;重放受 `BackfillHealthy` 门控;热重载同 ID 续传、删除清队。

### 8.3 回归

`go test ./...` 全绿。

## 9. 实施里程碑

1. `internal/backfill` 包 + 单测。
2. `output` 包:接口/BuildContext/Manager(SetBackfill + 扇出入队 + 重放 + status)+ 单测。
3. 三个输出接入失败入队 + `BackfillHealthy`。
4. `store.NewBackfillStore` + `main` 接线 + `config` 配置项。
5. 全量测试 + 文档(ROADMAP / api.md / 本设计文档实施记录)。

## 10. 决策记录

| 决策点 | 选择 | 理由 |
|---|---|---|
| 队列存储 | SQLite(既有 `gateway.db`) | 无新依赖;WAL 已启用;随网关重启存活;SQLite 已承担配置存储,运维心智一致 |
| 队列键 | `output_id`(每输出独立队列) | 输出可配多个同类实例;删除/重载按输出粒度管理 |
| 入队点 | Manager 扇出满 / 输出缓冲满 / 输出上送失败 三处 | 覆盖全部丢点路径;各输出只在自己最清楚语义的地方(flusher)补钩子 |
| 重放驱动 | Manager 集中重放(经 `slot.ch`) | 复用单写 goroutine,无并发 `Publish` 顾虑;健康门控靠 `BackfillHealthy` 能力接口,输出各自最懂自身连接语义(TB/smardaten 用 `IsConnected`,TDengine 用错误时效) |
| 确认删除 | 经 `slot.ch` 成功投递即 Ack | 输出失败会再入队,at-least-once;避免"先发后查"的复杂握手 |
| 淘汰策略 | 超上限丢最旧 | 保最新数据(近因更有价值);与"队列有界"目标一致 |
| 优雅关闭 | 不清队列 | 持久化队列跨重启续传,正是本特性价值 |
| 删除输出 | 清其队列 | 配置删除代表用户意图,避免僵尸数据 |

## 11. 实施记录

### 11.1 代码变更(2026-08-18)

- **`internal/backfill`**(新增):`Store` 持久化队列 + `Item`;`Save`(事务 + 超限淘汰最旧)/`CountByOutput`/`TotalCount`/`Peek`(FIFO)/`Ack`(幂等)/`DropOutput`;表 `backfill_queue` 随 `gateway.db`(WAL)。
- **`internal/output/backfill.go`**(新增):`BackfillSink`(Save)与 `BackfillHealthy` 两个能力接口。
- **`internal/output/registry.go`**:`BuildContext` 增加 `OutputID` 与 `Backfill`。
- **`internal/output/buildset.go`**:构建时逐条注入 `bc.OutputID`。
- **`internal/output/manager.go`**:`SetBackfill`;扇出队列满 → 补传入队(仅支持补传的输出);`runReplay` 重放循环(健康门控 + 经 `slot.ch` 投递 + 成功后 Ack);热重载对删除输出清队;`OutputStatus.Backfill` 深度;优雅关闭不清队。
- **`internal/output/thingsboard.go`**:缓冲满/connect 失败/publish 失败 → `saveBackfill`;`BackfillHealthy` = `IsConnected`。
- **`internal/output/tdengine.go`**:建子表失败/INSERT 失败 → `saveBackfill`;`BackfillHealthy` 按"从未失败 / 失败后已成功 / 距上次错误超 30s"判定。
- **`internal/output/smardaten.go`**:缓冲满/属性上报失败 → `saveBackfill`;`BackfillHealthy` = `IsConnected`。
- **`internal/store/sqlite.go`**:`NewBackfillStore(max)` 复用同一 DB 连接。
- **`cmd/gateway/main.go` / `internal/config/config.go` / `config.yaml`**:创建补传 store、注入 BuildContext 与 Manager;`storage.backfillMax` 配置项。
- **`web/src/views/Outputs.vue`**:输出展开面板增加"补传队列"显示。

### 11.2 测试

- `internal/backfill`:FIFO/JSON 往返/Ack 幂等/超限淘汰保最新/`DropOutput` 隔离。
- `internal/output/manager_backfill_test.go`:重放投递 + 顺序 + Ack;健康门控(不健康不重放);非补传输出不参与;扇出队列满入队;热重载删输出清队、同 ID 续传。
- `internal/output/tdengine_test.go`:INSERT 失败入队;`BackfillHealthy` 失败后退避、恢复后立即可重放。
- `internal/output/thingsboard_test.go`:broker 半死(QoS1 无 PUBACK)上送失败 → 入队。
- 全量 `go test ./...` 通过;网关启动冒烟验证 `backfill_queue` 建表正常。

### 11.3 已知限制(见 §7)

- at-least-once(重复投递各平台可容忍);`Ack` 后 crash 窗口与常规内存缓冲一致;输出被删除即清其补传队列。
