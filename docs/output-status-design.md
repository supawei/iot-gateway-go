# 输出状态监控设计文档

> **状态**:已实现(M1–M4 完成 + 全量回归 + 端到端验证)
> **关联**: [mqtt-resilience-design.md](mqtt-resilience-design.md)、[northbound-output-config.md](northbound-output-config.md)
> **更新**: 2026-08-18
> **范围**: `internal/output`(接口/管理器)、四个输出插件(mqtt / thingsboard / smardaten / tdengine)、`internal/api`、`web/src/views/Outputs.vue`

---

## 1. 背景与目标

### 1.1 背景

当前北向输出只有**配置**(SQLite `output` 表 + CRUD API + `Outputs.vue` 列表),**没有运行态观测**:

- broker 是否连上、断连后是否在重连、数据是否真的在往上游送——完全不可见;
- 输出内部缓冲积压(断连期 pending 增长)、Manager 扇出队列打满丢点——无感知;
- 与设备侧形成反差:设备已有 `status.Registry` + `GET /api/v1/status`(在线/离线/最近采集/最近错误),输出侧空白。

> 关联背景:上一轮韧性改造后,输出在 broker 不可达时会「后台自动重连 + 缓存待发」,这使「连接状态」成为更关键的监控维度——用户需要知道输出是在正常上送、重连中、还是积压丢点。

### 1.2 目标

为每个输出提供运行态快照,供 API 与 Web UI 观测:

1. **身份与存活**:输出 ID/名称/类型、配置启用、是否当前已构建运行(Active)。
2. **连接状态**(MQTT 类输出):逻辑连接(可收发/重连中)与物理连接是否真的建立。
3. **上送活动**:成功发送次数、最近发送时间、最近错误、发送错误计数。
4. **积压与丢弃**:输出内部待发缓冲(Pending)、Manager 扇出队列占用(QueueUsed/Cap)、队列满丢弃数(Dropped)。

### 1.3 非目标

- 不改变 `Output` / `DeviceNotifier` 接口签名(仅新增可选接口 `StatusProvider`)。
- 不做持久化/历史指标(与设备 status 一致,纯内存态可观测性,不落库)。
- 不做告警推送/阈值策略(本设计只提供观测数据)。
- 不新增配置字段。

---

## 2. 现状分析

| 现状 | 位置 | 与本设计的关系 |
|---|---|---|
| 设备状态注册表 `status.Registry`(在线/离线/最近采集/最近错误) | `internal/status` | 输出状态不复用该注册表(维度不同),但沿用「内存态、不持久化、按 ID 可查」的约定 |
| 设备状态 API `GET /api/v1/status`、`GET /devices/{id}/status`(scope `status:read`) | `internal/api/api.go:85-86` | 新增输出状态端点沿用 `status:read` scope |
| 输出管理器 `output.Manager`:每输出独立队列 + 发布 goroutine + 热重载 | `internal/output/manager.go` | 是输出运行态指标的天然归属点(接入/丢弃/队列) |
| 输出配置存储 `store.ListOutputs()` | `internal/store` | API 合并层用它补齐「未运行输出」的身份 |
| 输出插件:Mqtt 类持有 paho client,可查 `IsConnected()`/`IsConnectionOpen()`;smardaten/thingsboard 持有内部 `pendingCount` | 各输出包 | 类型相关的运行态由各输出自己上报 |
| Web 输出页 `Outputs.vue` | `web/src/views/Outputs.vue` | 增加状态列 + 详情行展示 |

关键缺口:**Manager 的 `slot` 不持有输出配置身份(ID/名称/类型)**,无法把运行态与配置关联;各输出也没有暴露运行态的接口。

---

## 3. 总体方案

在 `internal/output` 引入两层状态产生 + 一层聚合:

```
┌────────────────────────── 输出运行态快照 ──────────────────────────┐
│ OutputStatus(由 Manager 聚合)                                     │
│  ├─ 身份/存活: OutputID Name Type Enabled Active                  │  ← Manager slot
│  ├─ 接入/丢弃: QueueUsed QueueCap Received Dropped                │  ← Manager slot(发布循环/扇出)
│  ├─ 补传:      Backfill                                           │  ← Manager(backfill.Store)
│  └─ 类型相关:  Connected ConnectionOpen Pending Sent LastSentAt   │  ← 各输出 StatusProvider
│                LastError LastErrorAt                              │
└──────────────────────────────────────────────────────────────────┘
```

**字段归属**:

| 字段 | 生产方 | 含义 |
|---|---|---|
| `OutputID` `Name` `Type` `Enabled` `Active` | Manager slot(经 Instance 携带配置身份) | 身份与是否当前运行 |
| `QueueUsed` `QueueCap` | Manager slot | 扇出队列占用/容量(默认 1024) |
| `Received` | Manager slot 发布循环 | 成功投递给该输出发布循环的数据点数 |
| `Dropped` | Manager 扇出 | 队列满被丢弃的数据点数(未启用断网补传的输出) |
| `Backfill` | Manager(backfill.Store) | 持久化补传队列深度(断网待补送条数,见 docs/offline-backfill-design.md) |
| `Connected` `ConnectionOpen` | MQTT 类输出(paho client) | 逻辑连接 / 物理连接 |
| `Pending` | 各输出 | 输出内部待发缓冲积压 |
| `Sent` `LastSentAt` `LastError` `LastErrorAt` | 各输出(共享 SendStats) | 实际上送统计 |

> 语义约定:Manager 层负责「接入侧」(数据是否进入该输出、是否排队),输出层负责「上送侧」(是否真的发到上游)。二者互不重复:接入侧不计发送成功与否,上送侧只在真正 Publish/写库后更新。

---

## 4. 详细设计

### 4.1 `internal/output` 新增可选接口与类型

```go
// output.go(新增)
// RuntimeStatus 是输出自行上报的运行态(类型相关)。
type RuntimeStatus struct {
    Connected      bool      `json:"connected"`      // 逻辑连接:可收发或正在重连(可期待恢复)
    ConnectionOpen bool      `json:"connectionOpen"` // 物理连接是否真的建立
    Pending        int       `json:"pending"`        // 输出内部待发缓冲积压
    Sent           int64     `json:"sent"`           // 成功上送次数
    LastSentAt     time.Time `json:"lastSentAt"`     // 最近一次成功上送时间
    LastError      string    `json:"lastError"`      // 最近一次上送错误(空=无)
    LastErrorAt    time.Time `json:"lastErrorAt"`
}

// StatusProvider 可选接口:输出实现后,Manager 将其并入整体状态(与 DeviceNotifier 同模式)。
type StatusProvider interface {
    RuntimeStatus() RuntimeStatus
}
```

### 4.2 共享发送统计 `SendStats`(放在 `internal/output` 包,各输出复用)

```go
// sendstats.go(新增)
// SendStats 记录实际上送的成功/失败,供各输出内嵌,线程安全。
type SendStats struct {
    mu          sync.Mutex
    sent        int64
    lastSentAt  time.Time
    lastError   string
    lastErrorAt time.Time
}

func (s *SendStats) Success(t time.Time)
func (s *SendStats) Failure(err error)
func (s *SendStats) Snapshot() (sent int64, lastSentAt time.Time, lastError string, lastErrorAt time.Time)
```

各输出在**真正发送**的路径上调用:
- `mqtt.Publish`(同步发送):成功 → `Success`,失败 → `Failure`;
- `smardaten.flush` 属性上报、`thingsboard.publish`(f lusher 内):同上;
- `tdengine.flush` 每组合并写库:同上。

> 注意:smardaten/thingsboard 的 `Publish` 只入缓冲,不在这里记 `Sent`;`Sent/LastSentAt` 只在 flusher 真正 Publish 到 broker 后更新,保证「最近上送」语义准确。

### 4.3 Manager:Instance 重构 + slot 指标 + `Status()`

**4.3.1 配置身份入队(`Instance`)**

现状 `BuildFunc`/`BuildSet` 只返回 `[]Output`,slot 不知道输出是谁。改为携带配置身份:

```go
// manager.go(重构)
type Instance struct {
    Out     Output
    ID      string
    Name    string
    Type    string
    Enabled bool
}
type BuildFunc func() ([]Instance, error)
```

- `BuildSet(bc, configs) ([]Instance, error)`:构建时把 `model.Output` 的 ID/Name/Type/Enabled 带入 Instance;
- `main.buildOutputs` 返回 `BuildFunc` 改签名;
- `slot` 持有 `inst Instance`。

**4.3.2 slot 指标**

```go
type slot struct {
    inst Instance
    ch   chan model.DataPoint
    done chan struct{}
    wg   sync.WaitGroup

    mu       sync.Mutex
    received int64 // 发布循环取到的数据点数
    dropped  int64 // 扇出队列满丢弃数
}
```

- 发布循环取到 dp:`received++`;
- `Manager.Publish` 扇出 `default` 分支:`dropped++`;
- 两个计数器均在各自 goroutine 内自增,`mu` 仅保护跨 goroutine 读取(`Status()`),开销可忽略。

**4.3.3 `Manager.Status()`**

```go
func (m *Manager) Status() []OutputStatus {
    // 快照 slots 后,逐个合并:
    //   inst 身份 + len(s.ch)/cap(s.ch) + received/dropped
    //   + (s.inst.Out 实现 StatusProvider ? 其 RuntimeStatus() : 零值)
}
```

锁序:`m.mu` 快照 → 逐 slot 读 `s.mu`;无嵌套反向获取,无死锁。

### 4.4 各输出实现 `StatusProvider`

| 输出 | `Connected` / `ConnectionOpen` | `Pending` | 上送统计 |
|---|---|---|---|
| `mqtt` | `client.IsConnected()` / `IsConnectionOpen()` | 0(直发) | `Publish` 内更新 SendStats |
| `smardaten` | 同上 | `o.pendingCount`(锁内读) | `flush` 属性上报更新 |
| `thingsboard` | 同上 | `o.pendingCount`(锁内读) | `publish` 更新 |
| `tdengine` | false(HTTP 无连接态,恒 false) | `o.pending` 长度(锁内读) | `flush` 每组合并写库更新 |

结构:各输出内嵌 `output.SendStats`,`RuntimeStatus()` 组装(连接态 + 锁内读 pending + `stats.Snapshot()`)。

### 4.5 API:`GET /api/v1/outputs/status`

```go
// api.go(新增路由)
mux.HandleFunc("GET /api/v1/outputs/status",
    a.require(auth.ScopeStatusRead, a.listOutputStatus))
```

处理器做**配置 × 运行时合并**:

1. `a.outputs.Status()` 得到当前运行输出状态,以 `outputId` 建 map(`a.outputs == nil` 时按空处理,兼容测试);
2. `a.store.ListOutputs()` 逐条合并:
   - 运行中 → 用 Manager 状态(名称/类型/启用以 store 为准覆盖),`Active=true`;
   - 配置存在但未运行(禁用或构建失败)→ 返回 `Active=false` 的基础状态(构建失败原因已在日志,本版本不在 API 透传)。

返回 `[]output.OutputStatus`(JSON 扁平)。

> scope 选择:`status:read`(与设备状态一致,属运行时可观测信息);备选 `outputs:read` 见 §10 决策记录。

### 4.6 Web UI:`Outputs.vue`

- `api/index.js` 增加 `listOutputStatus: () => http.get('/outputs/status')`;
- `Outputs.vue` 的 `load()` 用 `Promise.all` 一并拉取,按 `outputId` 合并;
- 表格新增**「状态」列**:连接态 tag(已连接=Connected&&Open / 重连中=Connected&&!Open / 未连接=!Connected / 未启用=!Active),颜色映射 success/warning/danger/info;
- 新增**可展开详情行**(`el-table type="expand"`):队列占用 `QueueUsed/Cap`、待发缓冲 `Pending`、已接收 `Received`、已丢弃 `Dropped`、成功发送 `Sent`、最近发送 `LastSentAt`、最近错误 `LastError`(红色),配手动刷新按钮。

> 前端构建产物经 `//go:embed all:dist` 内嵌,需执行 `web/` 下 `npm run build`(vite)后重新 `go build` 才生效;开发期 `pnpm run dev:web` 可热更。

---

## 5. JSON 示例

```json
[
  {
    "outputId": "out-6d79a45f",
    "name": "sdata",
    "type": "smardaten-iot",
    "enabled": true,
    "active": true,
    "queueUsed": 3,
    "queueCap": 1024,
    "received": 15230,
    "dropped": 0,
    "connected": true,
    "connectionOpen": true,
    "pending": 12,
    "sent": 7611,
    "lastSentAt": "2026-08-18T12:00:00+08:00",
    "lastError": "",
    "lastErrorAt": "0001-01-01T00:00:00Z"
  },
  {
    "outputId": "out-abc123",
    "name": "备机",
    "type": "mqtt",
    "enabled": false,
    "active": false,
    "queueUsed": 0,
    "queueCap": 0,
    "received": 0,
    "dropped": 0,
    "connected": false,
    "connectionOpen": false,
    "pending": 0,
    "sent": 0,
    "lastSentAt": "0001-01-01T00:00:00Z",
    "lastError": "",
    "lastErrorAt": "0001-01-01T00:00:00Z"
  }
]
```

---

## 6. 行为与兼容性

| 项 | 说明 |
|---|---|
| 接口兼容 | `Output` / `DeviceNotifier` 不变;`StatusProvider` 为可选新增 |
| 内部重构 | `BuildFunc`/`BuildSet` 返回类型从 `[]Output` → `[]Instance`(仅 main 与 Manager 使用,编译期可查) |
| 性能 | 计数器自增 + 锁内读 len,均 O(1);`Status()` 为快照,不阻塞发布路径 |
| 热重载 | 重载原子替换 slots,`Status()` 自然反映最新集合(旧输出不再出现) |
| 语义 | 计数器自进程/输出实例创建起累计,重载后归零重建(与输出生命周期一致) |

---

## 7. 风险与已知限制

1. **构建失败不可见**:配置存在但构建失败的输出,在状态里只表现为 `Active=false`,与「禁用」无法区分(失败原因仅在日志)。后续可让 `BuildSet` 返回失败清单,经 Manager 暂存透传 API——本期不做。
2. **`len(ch)` 读取**:对 channel 的 `len()` 并发安全(运行时原子),无数据竞争。
3. **tdengine `Connected` 恒 false**:HTTP 无长连接,不做主动健康探测(需引入 ping SQL,成本高、收益低),其健康由 `LastError`/`LastSentAt` 体现。
4. **tdengine 内部 `pending` 无上限**(既有问题,不在本期):状态会如实上报其 `Pending` 长度,便于发现,但修复属另案。
5. **Web 产物内嵌**:UI 改动需重新 `npm run build` + `go build` 才在网关内生效(既有约定)。

---

## 8. 测试计划

### 8.1 单元测试

| 测试 | 方法 |
|---|---|
| `SendStats` 成功/失败/快照 | 直接构造调用,断言 sent/时间/错误语义 |
| `Manager.Status` 聚合 | 用 mock 输出(一个实现 `StatusProvider`,一个不实现)验证:身份、queue、received/dropped、RuntimeStatus 并入、未实现者为零值 |
| 发布循环计数 | `mgr.Publish` 若干点,断言 `received` 累计、`Dropped` 在队列打满后累计 |
| `BuildSet` 返回 Instance | 断言 ID/Name/Type/Enabled 透传 |
| API `listOutputStatus` | 假 store + 假 manager(实现 `Status`),断言配置×运行时合并、未运行输出 `Active=false`、`outputs==nil` 不 panic |

### 8.2 手测/集成

| 场景 | 预期 |
|---|---|
| broker 不可达(黑洞)启动 | 状态 `connected=false, connectionOpen=false`,`lastError` 有重连期错误,`active=true` |
| broker 恢复 | 状态变 `connected=true, connectionOpen=true`,`lastSentAt` 持续刷新 |
| 半死 broker | `connectionOpen=true` 但 `lastError` 出现 publish timeout,`sent` 停止增长 |
| 断连期间灌数据 | `pending`/`queueUsed` 增长,打满后 `dropped` 增长,`Received` 持续 |
| Web 输出页 | 状态列 tag 正确、详情行数据正确,禁用输出显示「未启用」 |

### 8.3 回归

- `go test ./...` 全绿、`go test -race ./internal/output/...`;
- 既有 API 测试(输出 CRUD、鉴权)不受影响。

---

## 9. 实施里程碑(每步 `go test ./...` 全绿)

| 里程碑 | 内容 | 验证 |
|---|---|---|
| M1 | `output` 包:`RuntimeStatus`/`StatusProvider`/`SendStats` + `Instance` 重构(`BuildSet`/Manager/`main`)+ `Manager.Status()` + 单测 | 单测 + 全量回归 |
| M2 | 四个输出实现 `StatusProvider`(连接态 + pending + SendStats 埋点) | 单测 + 手测 S1-S4 |
| M3 | API `GET /api/v1/outputs/status` + 合并逻辑 + 单测 | 单测 + curl |
| M4 | Web `Outputs.vue` 状态列 + 详情行 + `api/index.js`;`npm run build` + `go build` 内嵌 | 页面手测 |

---

## 10. 决策记录

| 决策点 | 选择 | 理由 |
|---|---|---|
| 状态存放位置 | `output.Manager.Status()`(不建独立 registry) | 输出集合本身由 Manager 动态管理,状态随热重载自然更新;设备用独立 registry 是因调度器跨包上报 |
| 接入侧/上送侧分离 | Manager 管接入(Received/Dropped/Queue),输出管上送(Connected/Sent/LastError) | 语义准确:缓冲型输出的「最近上送」不能被 Publish 入缓冲时间污染 |
| 身份如何入状态 | `BuildFunc`/`BuildSet` 返回 `[]Instance` | slot 直接持有 ID/Name/Type,状态与配置天然关联;改动面小(仅 main/Manager/测试) |
| `StatusProvider` 可选接口 | 是 | 与 `DeviceNotifier` 同模式,tdengine 等非 MQTT 输出可选择性实现 |
| API scope | `status:read` | 与设备状态一致(运行时可观测性);`outputs:read` 属配置读,语义不符 |
| 构建失败是否透传 | 本期不透传(仅 Active=false) | 需 BuildSet 返回失败清单并让 Manager 暂存,成本高、收益低,列为后续 |
| Web 展示 | 状态列 + 展开详情行 | 避免表格列爆炸;详情信息全、列表保持紧凑 |

---

## 11. 实施记录

### 11.1 代码变更

| 文件 | 变更 |
|---|---|
| `internal/output/output.go` | 新增 `RuntimeStatus` / `StatusProvider` 可选接口 |
| `internal/output/sendstats.go`(新增) | 共享 `SendStats` 上送统计(成功/失败/快照) |
| `internal/output/manager.go` | `Instance`(配置身份)+ `BuildFunc` 返回 `[]Instance`;slot 接入侧指标(received/dropped);`Manager.Status()` 聚合 |
| `internal/output/buildset.go` | `BuildSet` 返回 `[]Instance`(透传 ID/Name/Type/Enabled) |
| `cmd/gateway/main.go` | `buildOutputs` 适配新 `BuildFunc` 签名 |
| 四个输出包 | 实现 `StatusProvider`(连接态 + pending + SendStats 埋点于真正上送路径) |
| `internal/api/api.go` | 新增 `GET /api/v1/outputs/status`(scope `status:read`)+ store 配置 × Manager 运行态合并 |
| `web/src/views/Outputs.vue` + `api/index.js` | 状态列(已连接/重连中/未连接/未启用)+ 展开详情行 + 刷新按钮 |

### 11.2 验证结果

- `go test ./...` 全绿;`go test -race ./internal/output/... ./internal/api/...` 无竞态(期间修复了 `TestManagerStatusDropped` 在 race 下的时序抖动:改用固定延迟的 `slowMock`,使扇出丢弃在测试 goroutine 内同步发生)。
- 端到端(真实二进制 + 改动过的 SQLite 输出配置,鉴权关闭):
  - broker 不可达(黑洞):`active=true, connected=true, connectionOpen=false`——paho `ConnectRetry` 下「逻辑连接、后台重连中」,UI 显示「重连中」;
  - 半死 broker:`connected=true, connectionOpen=true`(「已连接」),网关保持存活;
  - `GET /api/v1/outputs/status` 返回合并后的完整状态;Web 根路径返回新构建的前端产物(index-*.js 哈希变化)。

### 11.3 状态语义说明

- `connected=true, connectionOpen=false`:MQTT 逻辑连接成立(paho 视为可期待恢复,消息落内存 store 待补发),但物理连接尚未建立/正在重连——UI 以「重连中」呈现。
- `LastSentAt`/`Sent` 只在真正上送路径更新(如 smardaten 的 flusher 属性上报),不会被「Publish 入缓冲」污染。
- `received` 计 Manager 成功投递给发布循环的点数;`dropped` 计扇出队列满丢弃;二者反映接入侧健康。
