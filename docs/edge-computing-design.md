# 边缘计算(Processing 处理层)设计文档

> **状态**:已实现(2026-08-18,单测 + 竞态检测 + 端到端冒烟验证通过)
> **关联**: [offline-backfill-design.md](offline-backfill-design.md)、[incremental-hot-reload-design.md](incremental-hot-reload-design.md)
> **更新**: 2026-08-18
> **范围**: `internal/model`、`internal/store`、新增 `internal/processing`、`internal/core/pipeline.go`、`internal/api`、`cmd/gateway/main.go`、`web/src/views/Devices.vue`

---

## 1. 背景与目标

P3 路线图中「边缘计算」指在 **Processing 处理层**内对采集数据进行**过滤与聚合**，
减少北向上送数据量、在边缘就地清洗数据。README 架构图自 P1 起就预留了
「Processing 处理层（预留，目前直通）」；`core.RunPipeline` 注释也明确
「处理层（过滤/规则）未来在此处插入，目前直通」。

### 1.1 目标

1. **过滤**：死区（deadband）、阈值（threshold）、质量（quality）三类规则，
   按点位配置，命中即丢弃，不北向上送。
2. **聚合**：时间窗口聚合（avg / min / max / sum / count / last），窗口关闭时
   产出**派生点位**（如 `temperature.avg`）继续北向上送，原始点不再逐条上送。
3. **可配置 + 热加载**：配置随设备点位一起存 SQLite，经 Web UI 编辑，写入后
   **增量热重载**（未变规则零打扰），与调度器、输出一致。
4. **可观测**：处理层运行统计（入站 / 放行 / 过滤 / 聚合计数、活跃聚合器数）经
   REST 暴露，供 UI 与后续运维监控使用。

### 1.2 非目标（与「暂缓清单」保持克制）

- **不做通用规则引擎**：无跨点位表达式、无复杂条件逻辑、无动作联动；
  只做单点位上的过滤与聚合，按真实需求驱动演进。
- **不改数据采集路径**：调度器 / 驱动 / values 注册表原样保留；
  原始点仍记录实时值（`values.Registry`），处理只影响**北向输出流**。
- **不做聚合的持久化与重启续算**：窗口内的中间数据仅存内存，进程重启即丢
  （聚合语义使然；如需持久化，列为后续评估）。

---

## 2. 数据模型

处理配置挂在**点位**上（与「设备-点位」模型天然吻合，无需全局规则表）。

```go
type Point struct {
    Name       string          `json:"name"`
    Address    string          `json:"address"`
    DataType   DataType        `json:"dataType"`
    Scale      float64         `json:"scale"`
    Processing *PointProcessing `json:"processing,omitempty"` // 边缘处理(新增)
}

// PointProcessing 描述单点位的边缘处理。Filters 按序应用,全部通过才放行;
// Aggregate 非空时,过滤通过的数值点进入时间窗口聚合(不再逐条上送)。
type PointProcessing struct {
    Filters   []Filter   `json:"filters,omitempty"`
    Aggregate *Aggregate `json:"aggregate,omitempty"`
}

// Filter 是单条过滤规则。
type Filter struct {
    Type    string  `json:"type"`               // deadband | threshold | quality
    Delta   float64 `json:"delta,omitempty"`    // deadband: 死区阈值(>=0,0 表示值变化即放行)
    Op      string  `json:"op,omitempty"`       // threshold: gt|ge|lt|le|eq|ne
    Value   float64 `json:"value,omitempty"`    // threshold: 单阈值
    Min     float64 `json:"min,omitempty"`      // threshold: between/outside 下界
    Max     float64 `json:"max,omitempty"`      // threshold: between/outside 上界
    DropBad bool    `json:"dropBad,omitempty"`  // quality: 丢弃 bad/uncertain
}

// Aggregate 是时间窗口聚合;窗口关闭时产出派生点位。
type Aggregate struct {
    Type     string `json:"type"`               // avg|min|max|sum|count|last
    Window   string `json:"window"`             // 如 "10s"、"1m"、"30s"(time.ParseDuration)
    EmitName string `json:"emitName,omitempty"` // 派生点位名,默认 <point>.<type>
}
```

### 2.1 语义约定

| 规则 | 语义 |
|---|---|
| `deadband` | 数值型。维护「上次放行值」基线：首值无条件放行并记录；此后 `\|v-baseline\| >= delta` 才放行并更新基线，否则丢弃。`delta=0` 表示「值变化即放行」。**非数值点直通**（bool/string 不受死区约束）。 |
| `threshold` | 数值型。`op + value`（gt/ge/lt/le/eq/ne）或 `min/max`（between / outside，含端点）命中才放行。**非数值点直通**。 |
| `quality` | 全类型。`dropBad=true` 时丢弃 `bad` / `uncertain` 质量的点。 |
| `Aggregate` | 数值型点进入窗口。窗口按**到达时刻（墙钟）**划分；关闭时产出派生点：`avg=sum/count`、`min/max`、`sum`、`count`、`last`。派生点 `Timestamp=窗口关闭时刻`、`Quality=good`。**非数值点不进窗口**。 |

> **过滤与聚合的次序**：先过滤（不通过即丢弃），通过后再进聚合窗口。
> **聚合替换原始流**：配置聚合后，该点位原始点不再逐条上送，只在窗口关闭时上送派生点。

### 2.2 派生点与下游

- 派生点以 `EmitName`（默认 `<point>.<type>`）为点名，`DeviceID` 沿用原设备，
  经 `output.Manager.Publish` 走与原始点完全相同的下游链路（MQTT 批量 /
  断网补传 / 各云输出均天然兼容）。
- 派生点**不进** `values.Registry`（实时值仍以原始采集值为准），故
  smardaten 的「读取设备当前属性值」仍返回原始值。

---

## 3. 存储

`point` 表新增 `processing TEXT` 列（JSON，可为 NULL）：

```sql
CREATE TABLE IF NOT EXISTS point (
    ... 原有列 ...,
    processing TEXT
);
```

- **开发期演进**：项目未发布、不做版本化迁移（见 development-conventions.md）。
  为避免历史 dev 库缺列，`Open` 里加一行**幂等 ALTER**（已存在时报错忽略）：
  `ALTER TABLE point ADD COLUMN processing TEXT`。
- `SaveDevice` 整表重建点位时一并写入 processing；`GetDevice`/`ListDevices`
  读取并反序列化；`cloneDevice` 随 `Points` 整体拷贝（含处理配置）。
- `Point.Processing == nil` 时存 NULL，读出为 nil。

### 3.1 OnChange 多订阅者

处理引擎与调度器都需要监听配置变更，而 `store.OnChange()` 目前返回**单个**共享
channel（cap 1），两个消费者会竞争信号（随机丢一个）。本次把 `OnChange` 重构为
**多订阅**：

```go
// 每次调用注册一个新的变更信号 channel;返回的 cancel 用于退订。
func (s *Store) OnChange() (<-chan struct{}, func())
```

- `notify()` 向所有已注册订阅者**非阻塞**发送（缓冲满即丢，语义同前）。
- 调用方（scheduler、processing.Run）在自身生命周期内订阅并在退出时退订。
- 现有 `sqlite_test.go` 的 `TestOnChange` 同步适配。

---

## 4. 处理引擎 `internal/processing`

### 4.1 结构与数据流

```go
type Engine struct {
    store *store.Store
    out   func(model.DataPoint) // 放行/派生点的出口(接入 output.Manager.Publish)

    mu    sync.Mutex
    rules map[key]model.PointProcessing  // 只读规则快照(热重载整体替换)
    last  map[key]float64                // deadband 基线
    aggs  map[key]*aggregator            // 聚合器(含窗口状态)
    stats Stats
}
```

- **规则快照**：`reload()` 读 `store.ListDevices()` 提取各设备启用的点位
  `Processing`，校验合法后整体替换 `rules`；并清理已删除规则的 `last` 基线、
  删除/重置聚合器（聚合配置变化时重置窗口，丢弃窗口内未完成数据）。
- **Process**（由 pipeline 单 goroutine 调用）：查规则 → 无规则直通；
  有规则先跑过滤（不过即丢）→ 有聚合进窗口（不立即上送）→ 否则放行。
- **flushLoop**（Run 内后台 goroutine）：`500ms` 节拍检查全部聚合器，
  窗口到期即冲刷产出派生点（保证「窗口无新点也能按时产出」）。
- 引擎加锁粒度：Process / reload / flush 共用一把 `sync.Mutex`；
  `Manager.Publish` 本身并发安全（内部加锁 + 非阻塞扇出），
  Process 与 flush 两处调用 out 不冲突。

### 4.2 窗口与冲刷

- `start` 取窗口内首个点到达时刻；`now-start >= window` 即到期。
- Process 中先查「是否已到期」：到期则先冲刷旧窗口、立即开新窗口再加入新点。
- flushLoop 兜底到期但无新点到达的窗口。
- 冲刷产出派生点后重置计数，`start` 在下个点到达时重新设定。

### 4.3 统计

```go
type Stats struct {
    PointsIn          int64 `json:"pointsIn"`
    PointsPass        int64 `json:"pointsPass"`
    PointsFiltered    int64 `json:"pointsFiltered"`
    PointsAggregated  int64 `json:"pointsAggregated"` // 产出的派生点数
    ActiveRules       int   `json:"activeRules"`
    ActiveAggregators int   `json:"activeAggregators"`
}
```

经 `GET /api/v1/processing/status` 暴露（复用 `status:read` scope），
供 UI 与运维确认处理层在生效。

---

## 5. 接入点

### 5.1 pipeline

`core.RunPipeline(ctx, dataPoints, proc, mgr)`：

```go
case dp, ok := <-dataPoints:
    if !ok { return }
    proc.Process(dp)   // 处理层:过滤/聚合后经 out 上送
```

### 5.2 main

```go
proc := processing.NewEngine(st, outputs.Publish)
go proc.Run(ctx)                       // 监听变更 + 冲刷
go core.RunPipeline(ctx, dataPoints, proc, outputs)
```

### 5.3 REST

无新配置端点（处理配置随 `PUT /api/v1/devices/{id}` 的点位一起提交，
沿用设备热加载链路）；仅新增只读统计端点 `GET /api/v1/processing/status`。

### 5.4 Web UI

`web/src/views/Devices.vue` 点位行内增加「处理」按钮 → 对话框编辑：
- **过滤**：动态列表，每项选择类型（死区/阈值/质量）并按其类型显示字段
  （死区 delta；阈值 op+value 或 min/max；质量 dropBad 开关）。
- **聚合**：开关 + 类型选择（avg/min/max/sum/count/last）+ 窗口 + 派生点名。

保存时随设备 payload 提交 `processing`，复用现有热加载，无需新页面。

---

## 6. 行为变化与兼容性

| 场景 | 现状 | 改造后 |
|---|---|---|
| 点位无处理配置 | 直通 | 直通（零变化） |
| 配置死区/阈值/质量 | - | 不满足条件的点被丢弃,不北向上送 |
| 配置聚合 | - | 原始点进窗口,窗口关闭上送派生点 |
| 处理配置变更 | - | 增量热重载;聚合配置变化重置该窗口 |
| 派生点下游 | - | 走完整输出链路(批量/补传/云平台) |

兼容性：`Point` 新增字段为 `omitempty`，旧客户端不传 `processing` 时行为不变；
smardaten 平台配置同步**不感知**处理配置（其 `pointsEqual` 仍只比较
Name/Address/DataType/Scale），平台侧触发设备重写时会清掉该设备点位的处理配置——
列为已知限制（网关本地处理属平台语义之外）。

---

## 7. 风险与已知限制

1. **聚合内存态**：窗口中间数据仅内存，进程重启即丢（设计非目标）。
2. **冲刷延迟 ≤ 500ms**：窗口到期后最多 500ms 内产出（节拍上限），
   对 1s 以上窗口可忽略。
3. **数值判定**：bool/string 点不参与死区/阈值/聚合（直通或忽略），
   quality 过滤全类型生效。
4. **smardaten 重写清处理配置**：见 §6 已知限制。
5. **派生点不进 values**：实时值面板仍显示原始采集值（设计如此）。

---

## 8. 测试计划

### 8.1 单元测试（`internal/processing`）

| 用例 | 断言 |
|---|---|
| 无规则直通 | 入站=放行,计数正确 |
| deadband 首值放行、`<delta` 丢弃、越界放行 | lastSent 基线更新正确 |
| deadband delta=0 变化才放行 | 相同值丢弃 |
| threshold gt/lt/between/outside | 各 op 命中判定 |
| quality dropBad | bad/uncertain 丢弃,good 放行 |
| 聚合 avg/min/max/sum/count/last | 窗口关闭产出正确派生点 |
| 窗口无新点仍产出(flushLoop) | 到期即冲刷 |
| 窗口到期新点到达触发切换 | 先冲旧窗再开新窗 |
| 聚合配置热重载重置 | 窗口状态清空、规则更新 |
| 非数值点不参与聚合 | 忽略 |

### 8.2 集成/回归

- `pipeline_test.go`：接入引擎后直通路径仍全量到达。
- `go test ./...`、`go test -race ./internal/processing/...`、`go vet ./...` 全绿。
- 手测：起网关 → Web UI 给某点位配死区/聚合 → 观察
  `GET /api/v1/processing/status` 计数与 MQTT 上送内容（派生点名）。
