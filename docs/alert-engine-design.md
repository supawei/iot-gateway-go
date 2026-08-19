# 告警引擎(Alert)设计文档

> **状态**:已实现(2026-08-19,单测覆盖 N=1/N>1/新鲜度/cooldown/直通 + 全工程编译验证通过)
> **关联**: [edge-computing-design.md](edge-computing-design.md)、[incremental-hot-reload-design.md](incremental-hot-reload-design.md)、[offline-backfill-design.md](offline-backfill-design.md)
> **更新**: 2026-08-19
> **范围**: `internal/model`、`internal/store`、`internal/output`、`internal/output/mqtt`、新增 `internal/alert`、`internal/api`、`cmd/gateway/main.go`、`web/src/views/AlertRules.vue`、`web/src/views/Alerts.vue`

---

## 1. 背景与目标

processing 处理层([edge-computing-design.md](edge-computing-design.md))刻意限定在**单点位**过滤与聚合,
明确「不做通用规则引擎:无跨点位表达式、无复杂条件逻辑、无动作联动」。但实际运行中出现了
processing 覆盖不了的真实需求:**跨设备/跨点位告警**(如「温度传感器温度 >30 且 空调开关处于
关闭状态」触发告警)。两个点位的值不同时刻到达,必须在消息之间保持状态才能判定「且」。

ROADMAP 曾把「规则引擎」列入「暂缓清单(刻意不做)」,理由是当时无真实需求。如今跨设备告警是真实
需求,但只做**告警**这一件事--不顺势扩张成通用规则引擎/工作流。引入一个完整规则引擎库
(如 RuleGo,含 goja JS 引擎、DAG、DSL)对当前一两条规则的规模属杀鸡用牛刀,故**自研轻量告警引擎**,
表达式求值用 `expr-lang/expr`(纯 Go、无 CGO)。

### 1.1 目标

1. **跨设备/跨点位告警**:表达式求值为 `true` 即触发告警,表达式用 `point("设备ID","点位名")`
   引用任意点位最新值,天然支持单点位(N=1)与跨点位(N>1)。
2. **边沿触发 + 去重**:条件从「不成立」翻转为「成立」时告警一次,告警态期间不重复;条件解除自动恢复。
3. **状态新鲜度**:引用点位的值超过有效期视为失效,防陈旧开关状态误告警。
4. **告警独立消息 + 定向投递**:告警不复用 `DataPoint` 格式,走独立 `AlertMessage` + `AlertPublisher`
   接口,定向投递到规则指定的输出(如某 MQTT 输出的告警 topic)。
5. **告警表持久化**:触发即落 SQLite `gw_alert` 表,`pending`/`resolved` 状态机,供 Web UI 查看。
6. **配置热重载**:规则存 SQLite,经 Web UI/API 编辑后 `store.OnChange` 自动热重载,范式与 processing 一致。

### 1.2 非目标(保持克制)

- **不做通用规则引擎**:无规则链 DAG、无节点编排、无子规则链、无可视化拖拽;只做「表达式求值 -> 触发」。
- **不做联动下行控制**:告警只产出通知(发消息 + 写表),不反向写设备(安全责任不同,见 Q4 决策)。
- **不做 webhook/邮件**:二期按需新加 output 类型。
- **告警不补传**:告警是实时事件,迟到的告警价值骤降,发送失败丢弃 + 日志(与采集数据的时序完整性是两种诉求)。
- **不引入 RuleGo/goja**:当前规模不匹配;未来规则爆炸、需非工程师配置或复杂编排时再评估替换。

---

## 2. 数据模型

告警规则是网关级配置(跨设备,不属于单个点位),故独立 `gw_alert_rule` 表,不挂在 `Point` 上。

```go
// AlertRule 是跨设备/跨点位告警规则:表达式求值为 true 即触发告警。
type AlertRule struct {
    ID               string     `json:"id"`
    Name             string     `json:"name"`
    Enabled          bool       `json:"enabled"`
    Severity         string     `json:"severity"`         // warning | critical
    Expr             string     `json:"expr"`             // 如 point("d1","temp")>30 && point("d2","sw")=="off"
    ReferencedPoints []RefPoint `json:"referencedPoints"` // 显式声明表达式引用的点位(引擎据此建反向索引,免 AST 分析)
    OutputIDs        []string   `json:"outputIds"`        // 定向投递的 output ID
    FreshnessSeconds int        `json:"freshnessSeconds"` // 状态新鲜度秒数,默认 300
    CooldownSeconds  int        `json:"cooldownSeconds"`  // 解除后防抖秒数,默认 0
    CreatedAt        string     `json:"createdAt"`
    UpdatedAt        string     `json:"updatedAt"`
}

type RefPoint struct {
    DeviceID string `json:"deviceId"`
    Point    string `json:"point"`
}
```

### 2.1 表达式语法

经 `expr-lang/expr` 求值,引擎注入 `point(deviceID, pointName)` 函数,返回该点位最新采集值
(未收到过返回 `nil`)。支持算术、比较、逻辑运算符:

```
point("temp-sensor","temp") > 30 && point("ac","sw") == "off"     // 跨设备且
point("d1","hum") > 90 || point("d2","hum") > 90                    // 或
point("d1","temp") - point("d2","temp") > 5                        // 跨设备差值
```

**为何显式声明 `referencedPoints`**:引擎需知道每条 `DataPoint` 到来时要更新哪些规则的状态。
显式声明让引擎在 reload 时建 `map[(deviceID,point)] -> []ruleID` 反向索引,`Process` 里 O(1) 查找,
免做表达式 AST 静态分析(复杂且易错)。前端从已配置设备点位里选择,保证声明与表达式一致。

### 2.2 告警消息(独立于 DataPoint)

```go
// AlertMessage 是告警事件的消息载荷,经 AlertPublisher 定向投递。与 Alert 表记录对齐,
// 但不带 Status/ResolvedAt(那是本地表的状态机概念)。
type AlertMessage struct {
    AlertID     string         `json:"alertId"`
    RuleID      string         `json:"ruleId"`
    RuleName    string         `json:"ruleName"`
    Severity    string         `json:"severity"`
    Message     string         `json:"message"`
    TriggeredAt time.Time      `json:"triggeredAt"`
    GatewayID   string         `json:"gatewayId"`
    Context     []AlertContext `json:"context"` // 触发瞬间各引用点值快照
}

type AlertContext struct {
    DeviceID  string      `json:"deviceId"`
    Point     string      `json:"point"`
    Value     interface{} `json:"value"`
    Timestamp time.Time   `json:"timestamp"`
}

// Alert 是一条已触发的告警记录(存 alerts 表),比 AlertMessage 多本地状态机字段。
type Alert struct {
    // ...同 AlertMessage...
    Status     string     `json:"status"` // pending | resolved
    ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}
```

**为何不复用 `DataPoint`**:告警是多设备多点位关联结果,塞进单点位值结构语义错乱,订阅端
schema 也不统一(普通点 value 是标量,告警 value 是对象)。告警走独立类型 + 独立 topic。

---

## 3. 存储

新增两表(开发期演进,`schema` 常量内 `CREATE TABLE IF NOT EXISTS`,见 development-conventions.md):

```sql
CREATE TABLE IF NOT EXISTS gw_alert_rule (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL,
    enabled            INTEGER NOT NULL DEFAULT 1,
    severity           TEXT NOT NULL DEFAULT 'warning',
    expr               TEXT NOT NULL,
    referenced_points  TEXT NOT NULL DEFAULT '[]',
    output_ids         TEXT NOT NULL DEFAULT '[]',
    freshness_seconds  INTEGER NOT NULL DEFAULT 300,
    cooldown_seconds   INTEGER NOT NULL DEFAULT 0,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS gw_alert (
    id           TEXT PRIMARY KEY,
    rule_id      TEXT NOT NULL,
    rule_name    TEXT NOT NULL,
    severity     TEXT NOT NULL,
    message      TEXT NOT NULL,
    triggered_at TEXT NOT NULL,
    gateway_id   TEXT NOT NULL,
    context      TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',
    resolved_at  TEXT
);
```

- **规则 CRUD**:`SaveAlertRule`(upsert + `notify`)/`ListAlertRules`/`GetAlertRule`/`DeleteAlertRule`,
  范式与 `Output` 一致。`referenced_points`、`output_ids` 以 JSON 数组存。
- **告警记录**:`SaveAlert`(触发时写,**不 notify**--结果记录不影响运行态)/`ListAlerts(status)`
  /`UpdateAlertStatus`(解除时置 `resolved` + 记时间)。时间字段用 `RFC3339Nano` 字符串。

规则变更经 `notify` 触发告警引擎 `OnChange` 监听 -> `reload`;告警记录写入不触发任何重载。

---

## 4. 告警引擎 `internal/alert`

### 4.1 结构与数据流

```go
type Engine struct {
    store     *store.Store
    mgr       *output.Manager   // 数据继续扇出(Publish) + 告警定向投递(PublishAlertTo)
    gatewayID string

    mu    sync.Mutex
    rules map[string]*compiledRule      // ruleID -> 已编译规则 + 运行态
    index map[pointKey][]string         // (deviceID,point) -> ruleIDs 反向索引
}

type compiledRule struct {
    rule          model.AlertRule
    program       *vm.Program           // expr 编译产物(reload 时编译一次,Process 只 Run)
    env           map[string]any        // 含 point 函数,闭包 state
    state         map[pointKey]pointValue // 各引用点最新值 + ts
    refs          []pointKey
    active        bool                   // 当前是否告警态(边沿触发)
    activeAlertID string                 // 当前告警态对应的 alertID(解除时据此更新表)
    lastResolved  time.Time              // 上次解除时刻(cooldown 用)
    freshness     time.Duration
    cooldown      time.Duration
}
```

- **Process**(由 pipeline 单 goroutine 调用):反向索引查命中规则 -> 更新 `state` -> 新鲜度检查
  -> `expr.Run` 求值 -> 边沿翻转产出动作(锁内收集,锁外执行) -> 末尾 `mgr.Publish(dp)` 让数据继续扇出。
- **Run**:启动 `reload` 一次,随后监听 `store.OnChange` 热重载。
- **无规则引用的点位**直接 `mgr.Publish` 扇出,零开销(反向索引未命中即跳过)。
- 加锁粒度:Process / reload 共用一把 `sync.Mutex`;写表与投递在锁外执行(避免持锁 IO)。

### 4.2 边沿触发状态机

```
              fired && !active                 !fired && active
  ┌──────────┐ ───────────────▶ ┌──────────┐ ───────────────▶ ┌──────────┐
  │ inactive │                  │  active  │                  │ inactive │
  │ active=F │ ◀─────────────── │ active=T │ ◀─────────────── │ active=F │
  └──────────┘   条件再次成立   └──────────┘   条件解除       └──────────┘
                     (不重复告警)                                │
                                                                │ cooldown 内再成立
                                                                │ -> 抑制
                                                                ▼
```

- `fired && !active` -> 触发告警一次,`active=true`,记 `activeAlertID`。
- `!fired && active` -> 解除,`active=false`,更新告警表 `resolved`。
- 告警态期间持续 `fired` 不重复告警(去重);持续 `!fired` 不重复解除。

### 4.3 新鲜度与防抖

- **新鲜度**(`freshness`,默认 300s):求值前检查规则所有引用点的 `state` 是否都已收到且
  `now - ts <= freshness`;任一未收到或过期则跳过本轮判断(不触发也不解除)。防「空调开关点位
  10 分钟没更新,其『关闭』状态还参与判断」的陈旧误报。
- **防抖**(`cooldown`,默认 0s):`fired && !active` 时若 `now - lastResolved < cooldown` 则抑制
  重触发,防「条件抖动导致解除即又触发」的告警风暴。

### 4.4 热重载

`reload` 读 `ListAlertRules` -> 对每条启用规则 `expr.Compile` 编译(失败跳过并日志)-> 建反向索引。
**保留同 ID 规则的运行态**(`state`/`active`/`activeAlertID`/`lastResolved`),仅重编译表达式;
被删除的规则若仍 `active`,在锁外将其告警记录置 `resolved`(防孤儿告警)。

---

## 5. 告警投递

### 5.1 AlertPublisher 可选接口

```go
// output.go:与 DeviceNotifier/StatusProvider 同范式的可选能力接口。
type AlertPublisher interface {
    PublishAlert(alert model.AlertMessage) error
}
```

未实现该接口的输出被 `PublishAlertTo` 跳过(告警只发到支持告警的输出)。

### 5.2 Manager.PublishAlertTo 定向投递

```go
func (m *Manager) PublishAlertTo(outputIDs []string, alert model.AlertMessage)
```

遍历活跃输出,命中 `outputIDs` 且实现 `AlertPublisher` 的,投递到该 slot 的 `alertCh`(独立于
数据点队列的 `chan AlertMessage`,缓冲 64)。**队列满丢弃 + 日志**(告警不补传)。
slot 的发布 goroutine 增加一个 `select` 分支消费 `alertCh` 调 `PublishAlert`,复用该输出的
发布 goroutine(顺序保证、不并发调 output)。

### 5.3 MQTT 实现

`mqtt.Config` 增 `AlertTopic`(留空默认 `gateway/{网关ID}/alert`);`PublishAlert` 把
`AlertMessage` JSON 序列化发到 `alertTopic`,复用现有 `m.publish`(有界等待)。发送失败仅返回
错误由 slot 日志,**不落补传队列**(告警不补传)。

```json
topic: gateway/gw-01/alert
{
  "alertId": "550e8400...", "ruleId": "rule_temp_ac", "ruleName": "高温且空调关闭",
  "severity": "warning", "message": "高温且空调关闭 触发",
  "triggeredAt": "2026-08-19T10:00:00Z", "gatewayId": "gw-01",
  "context": [{"deviceId":"temp-sensor","point":"temp","value":30.5,"timestamp":"..."},
              {"deviceId":"ac","point":"sw","value":"off","timestamp":"..."}]
}
```

---

## 6. 接入点

### 6.1 pipeline 串接

告警引擎接在 processing 下游--processing 的 `out` 回调从 `outputs.Publish` 改为
`alertEng.Process`,后者内部末尾再调 `mgr.Publish`:

```
pipeline ─▶ processing.Engine.Process ─▶ (out) alert.Engine.Process
                                          ├─ 告警判断(触发/解除)
                                          └─ mgr.Publish(dp)  ← 数据继续扇出
```

### 6.2 main 装配

```go
alertEng := alert.NewEngine(st, outputs, gatewayID)
go alertEng.Run(ctx)                              // 监听变更热重载
proc := processing.NewEngine(st, alertEng.Process) // out 改为告警引擎
go proc.Run(ctx)
go core.RunPipeline(ctx, dataPoints, proc, outputs)
```

### 6.3 REST

| 方法 | 路径 | scope | 说明 |
|---|---|---|---|
| GET | `/api/v1/alert-rules` | `outputs:read` | 列出全部规则 |
| POST | `/api/v1/alert-rules` | `outputs:write` | 新建(自动生成 ID + 时间戳,SaveAlertRule 内 notify 触发热重载) |
| GET | `/api/v1/alert-rules/{ruleId}` | `outputs:read` | 单条 |
| PUT | `/api/v1/alert-rules/{ruleId}` | `outputs:write` | 更新 |
| DELETE | `/api/v1/alert-rules/{ruleId}` | `outputs:write` | 删除 |
| GET | `/api/v1/alerts?status=pending` | `outputs:read` | 告警记录(可按状态过滤) |

复用 `outputs:read/write` scope(告警规则引用 output,语义接近,不新加 scope)。

### 6.4 Web UI

- **告警规则页**(`/alert-rules`):列表 + 对话框。表单含名称、级别、引用点位(从已配置设备点位多选)、
  触发条件、投递输出(从已配置输出多选)、新鲜度、防抖、启用。保存即热重载。
  - **分段条件编辑**:选中引用点位后自动生成 `point("设备ID","点位名")`,用户只需为每个点位填写条件
    (如 `> 30`、`== "off"`),行间选 AND/OR 连接;底部实时预览拼装后的完整表达式。
  - **高级模式**:勾选后切换为完整表达式 textarea,支持括号嵌套等任意 expr-lang 语法;编辑已有规则时
    若表达式可按顶层 `&&`/`||` 分段反解则回填分段,否则自动回退高级模式。
- **告警记录页**(`/alerts`):列表 + 状态过滤(全部/未解除/已解除)。
- 侧边栏新增「告警规则」「告警记录」入口。

---

## 7. 行为变化与兼容性

| 场景 | 现状 | 改造后 |
|---|---|---|
| 无告警规则的点位 | 直通扇出 | 直通扇出(反向索引未命中,零开销) |
| 配置 N=1 单点位规则 | - | 同一条 DataPoint 内条件齐全即判断,触发/去重/解除 |
| 配置 N>1 跨点位规则 | - | 跨消息保持各方最新值,凑齐且满足才触发 |
| 引用点过期 | - | 该规则跳过判断,不触发不解除 |
| 规则配置变更 | - | 热重载;同 ID 保留运行态,删除规则 resolve 孤儿告警 |
| 告警发送失败 | - | 丢弃 + 日志(不补传) |

兼容性:`DataPoint` 与现有 output 链路完全不变;`Output` 接口未改(新增的是**可选** `AlertPublisher`,
未实现的输出不受影响)。processing 的过滤/聚合语义不变,只是放行点的下一站从 `outputs.Publish`
变成 `alertEng.Process`(对 processing 而言 `out` 回调仍是 `func(DataPoint)`,透明)。

---

## 8. 风险与已知限制

1. **状态内存态**:`ruleState` 仅内存,进程重启即丢(重启后所有规则 `active=false`;已触发未解除的
   告警在表里保持 `pending`,需人工或后续清理。如需重启续算,列为后续评估)。
2. **表达式类型**:`point()` 返回 `interface{}`,与数值比较在运行时动态判定;非法类型比较
   (如 `point("d","s")>30` 而 `s` 是字符串)在 `expr.Run` 报错,引擎日志跳过该规则本轮,不崩。
3. **告警不补传**:断网期间触发的告警发送失败即丢;告警表仍记录(pending),但订阅端收不到。
   若需审计完整性,二期扩展 `backfill` 支持 `AlertMessage`。
4. **不联动下行**:告警只通知,不写设备。需要联动控制(如温度高->关阀门)属另一安全域,单独设计。
5. **单 goroutine**:`Process` 由 pipeline 单 goroutine 调用;告警写表(`store.SaveAlert`)在该
   goroutine 同步执行,SQLite 写若有延迟会反压采集。当前告警是低频边沿事件,可忽略;若高频再异步化。

---

## 9. 测试计划

### 9.1 单元测试(`internal/alert`)

| 用例 | 断言 |
|---|---|
| 无规则引用的点位直通 | 数据点扇出,无告警 |
| N=1 单点触发->去重->解除 | 触发 1 条;重复值不增;解除后 status=resolved |
| N>1 跨设备:先到的点位不触发 | 单点不触发,凑齐才触发 |
| 新鲜度过期不触发 | 引用点过期后跳过判断 |
| cooldown 抑制重触发 | 解除后 cooldown 内再成立不报;过期后报 |
| 投递定向 | 告警消息发到指定 fakeOutput,未配置的输出收不到 |

### 9.2 集成/回归

- `go build ./...`、`go vet ./...`、`go test ./internal/alert/ ./internal/output/ ./internal/store/` 全绿。
- 手测:起网关 -> Web UI 配规则(温度>30 单点) -> 触发 -> 观察
  `GET /api/v1/alerts`、MQTT `alert` topic、告警记录页。
- 跨设备规则(温度>30 且 空调关闭)验证跨消息状态与解除恢复。
