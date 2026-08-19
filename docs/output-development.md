# 北向输出插件开发指导

> **定位**:面向新增云平台/数据出口的完整实现指引——从接口契约、能力矩阵到分步示例、测试与提交清单。
> **配套**:接口定义 `internal/output/output.go`、`registry.go`、`backfill.go`、`sendstats.go`;参考实现 `internal/output/{mqtt,thingsboard,tdengine,smardaten,sparkplugb}`;相关设计见 [northbound-output-config.md](northbound-output-config.md)、[mqtt-resilience-design.md](mqtt-resilience-design.md)、[offline-backfill-design.md](offline-backfill-design.md)、[output-status-design.md](output-status-design.md)。
> **更新**:2026-08-19

---

## 1. 总览:输出插件在架构中的位置

北向输出(Output)是网关的数据出口:消费采集管道产出的**协议无关 `DataPoint`**,转换为目标平台的格式后上送。
与南向驱动对称——驱动解决"怎么采",输出解决"送哪去、怎么送"。

```
scheduler ──DataPoint──▶ pipeline(过滤/聚合) ──▶ output.Manager ──(扇出到每个 slot)──▶ Out.Publish
                                                      │  ▲
                                               DeviceNotifier │（设备上下线）
                                                      ▼  │
                                                 Output 插件(mqtt / thingsboard / tdengine / smardaten / sparkplugb ...)
```

**插件模型**:每个输出插件是一个**独立子包**,在 `init()` 里 `output.Register(Descriptor, Constructor)` 声明**类型、配置 schema、构造器**;
`cmd/gateway/main.go` 以**空导入**加载:

```go
_ "iot-gateway-go/internal/output/mqtt"        // 注册 mqtt 输出
_ "iot-gateway-go/internal/output/smardaten"   // 注册 smardaten-iot 输出
_ "iot-gateway-go/internal/output/tdengine"    // 注册 tdengine 输出
_ "iot-gateway-go/internal/output/thingsboard" // 注册 thingsboard 输出
_ "iot-gateway-go/internal/output/sparkplugb"  // 注册 sparkplugb 输出
// 新增输出只需在此加一行空导入,其余全部零改动
```

### 1.1 生命周期与配置

- 配置存 SQLite `gw_output` 表(`id/name/type/config/enabled`),经 Web UI + REST 增删改,写入即触发 `Manager.Reload()` 热重载(无需重启)。
- 一个输出类型可有**多个实例**(每条 `output` 记录一个实例),实例以 `OutputID` 相互隔离。
- 新增输出插件 = 新增子包 + main.go 一行空导入,Web UI 表单、API、状态面板全部自动适配(见 §6.7)。

### 1.2 能力矩阵

| 能力接口 | 含义 | 检测方式 | 参考实现 |
|---|---|---|---|
| `output.Output`(必选) | `Publish(dp)` + `Close()` | 直接持有 | 全部 |
| `output.DeviceNotifier` | 设备上线/离线通知 | `out.(DeviceNotifier)` | thingsboard(connect/disconnect)、sparkplugb(DBIRTH/DDEATH)、smardaten |
| `output.StatusProvider` | 上报类型相关运行态 | `out.(StatusProvider)` | mqtt / thingsboard / tdengine / smardaten |
| `output.BackfillHealthy` | 声明支持断网补传 + 健康判定 | `out.(BackfillHealthy)` | mqtt / thingsboard / tdengine / smardaten / sparkplugb |
| `output.BackfillSink` | 持久化补传队列(**注入**,非实现) | 经 `BuildContext.Backfill` 传入 | main 注入 `*backfill.Store` |

> 前三个是输出**实现**的能力(通过类型断言被 Manager 发现);`BackfillSink` 是网关**注入给输出**的接口,输出在"无法即时送出"的路径上调用它落库,而非丢弃。

---

## 2. 核心接口契约

全部定义在 `internal/output/output.go` 与 `backfill.go`,实现前逐条读透注释。

### 2.1 Output:Publish / Close

```go
type Output interface {
    Publish(dp model.DataPoint) error
    Close() error
}
```

- **Publish 只消费一个 DataPoint**,由 Manager 的 slot 发布 goroutine 单线调用(见 §5.1)。
- `Publish` 通常**不立即发送**而是入内部缓冲(微批),真正的上送在 flusher 中批量完成——高频场景下逐条同步发送既慢又打爆对端。
- **返回错误不阻塞**:Manager 侧记录日志,但不会重试;数据的"不丢"由补传机制保证(见 §7.3),而非依赖 Publish 的错误返回。

### 2.2 DeviceNotifier(可选)

```go
type DeviceNotifier interface {
    DeviceOnline(deviceID string)
    DeviceOffline(deviceID string)
}
```

scheduler 在设备状态发生**上线/离线转变**时,对实现了该接口的输出调用对应方法(如 ThingsBoard 发 `connect`/`disconnect`,Sparkplug B 发 `DBIRTH`/`DDEATH`)。

- 实现应把"意图"缓冲起来,由 flusher 统一发出(勿在回调里直接发 IO)。
- 回调可能被并发调用,内部缓冲要加锁。

### 2.3 StatusProvider(可选)

```go
type StatusProvider interface {
    RuntimeStatus() RuntimeStatus
}
```

```go
type RuntimeStatus struct {
    Connected      bool      `json:"connected"`      // 逻辑连接:可收发或正在重连
    ConnectionOpen bool      `json:"connectionOpen"` // 物理连接是否真的建立
    Pending        int       `json:"pending"`        // 输出内部待发缓冲积压
    Sent           int64     `json:"sent"`           // 成功上送次数
    LastSentAt     time.Time `json:"lastSentAt"`     // 最近一次成功上送时间
    LastError      string    `json:"lastError"`      // 最近一次上送错误(空=无)
    LastErrorAt    time.Time `json:"lastErrorAt"`
}
```

Manager 在 `Status()` 中把 `RuntimeStatus` 并入整体快照,API/Web 状态面板据此展示(见 docs/output-status-design.md)。实现要点:

- 统计直接**内嵌 `output.SendStats`**(线程安全),在"真正上送到上游"的路径上调用 `Success`/`Failure`(见 §7.2)。
- `Pending` 为输出内部缓冲长度(非 Manager 队列);`Connected` 语义:M QTT 类用 `client.IsConnected()`,HTTP 类恒 false(健康由 LastError/LastSentAt 体现,参考 tdengine)。

### 2.4 Backfill(可选但强烈建议)

```go
// 输出在失败/缓冲满路径上调用(由 main 注入,实现为 *backfill.Store):
type BackfillSink interface {
    Save(outputID string, dps []model.DataPoint) error
}

// 输出声明支持断网补传;Manager 只在 healthy 时重放其持久化队列:
type BackfillHealthy interface {
    BackfillHealthy() bool
}
```

- **实现 `BackfillHealthy` 即声明"支持补传"**——Manager 据此决定该输出的队列满时落库、健康时重放(见 §7.3)。
- 未实现该接口的输出,队列满仍走"丢弃 + 告警"(旧语义)。

---

## 3. BuildContext:网关注入的上下文

输出构造器签名:`func(bc BuildContext, config json.RawMessage) (Output, error)`。
`BuildContext` 是**与输出自身配置无关**的网关上下文,由 `main` 注入(`cmd/gateway/main.go:buildOutputs`):

```go
type BuildContext struct {
    GatewayID   string            // 网关 ID(用于 topic/标识,来自 settings)
    Write       WriteFunc         // 下行写回调(ctx, deviceID, point, value)→ core.WritePoint
    Store       StoreAccessor     // 配置存储访问(枚举设备/读配置/写配置等)
    OutputID    string            // 当前实例的配置 ID(BuildSet 逐个注入,补传队列隔离键)
    Backfill    BackfillSink      // 断网补传持久化队列(未接线时 nil,退化为丢弃+告警)
    LatestValues LatestValuesFunc // 设备全部点位最新采集值快照(pointID→value)
    Probe       ProbeFunc         // 设备连通性探测回调(经 core.ProbeDevice)
}
```

- **每个插件只需取自己用到的字段**,其余忽略;构造器只拿 `bc` 与 `raw config`,不直接依赖 store/core 包。
- `StoreAccessor` 暴露的访问方法集合见 `registry.go`(Save/Get/List/Delete 连接与设备 + GetSetting/SetSetting),`sparkplugb` 用它枚举设备发 birth 消息,`smardaten` 用它自动同步配置。

---

## 4. 注册机制

```go
// init() 中注册:声明类型、展示名、配置 schema 与构造器。
func init() {
    output.Register(output.Descriptor{
        Type:  "mycloud",           // 类型名(API/DB 用)
        Label: "MyCloud",           // 展示名(Web UI 用)
        Schema: []output.Field{     // 配置表单(Web UI 据此渲染)
            {Name: "endpoint", Label: "接入地址", Type: output.FieldString, Required: true, Placeholder: "https://..."},
            {Name: "token", Label: "令牌", Type: output.FieldPassword},
            {Name: "flushInterval", Label: "Flush 间隔", Type: output.FieldString, Default: "1s"},
        },
    }, func(bc output.BuildContext, raw json.RawMessage) (output.Output, error) {
        var cfg Config
        if err := json.Unmarshal(raw, &cfg); err != nil {
            return nil, fmt.Errorf("mycloud config: %w", err)
        }
        return New(cfg, bc.OutputID, bc.Backfill)
    })
}
```

- `output.Field` 与驱动 `driver.Field` 同构,Web 前端 SchemaForm 可复用;字段控件类型:`string / password / int / number / bool / enum(配 Options) / json`,支持 `ShowWhen` 动态显隐。
- 构造器返回错误时,**须保证不泄漏任何已建立的连接**(在返回前关闭);构建失败只跳过该实例,不拖垮其余(失败隔离,见 §5.2)。

---

## 5. Manager:扇出、热重载与背压

### 5.1 扇出与背压隔离

`Manager.Publish` 把每个 DataPoint 扇出到所有活跃输出;每个输出一个**独立队列(`queueSize=1024`)+ 发布 goroutine**:

```
Manager.Publish(dp)
   └─▶ slot.ch(每个输出独立缓冲队列)─▶ slot goroutine ─▶ Out.Publish(dp)
```

- **背压隔离**:慢输出(如半死 broker)只阻塞自己的队列,不拖垮采集侧与其他输出。
- **队列满**时:该输出若实现 `BackfillHealthy` → 落库补传;否则 → 丢弃并计数 `Dropped`。

### 5.2 热重载与失败隔离

- 写/删输出配置触发 `Manager.Reload()`:构建新输出集 → **原子替换** → 关闭旧输出。
- 单实例构建失败仅跳过并告警(`BuildSet` 失败隔离);**全部**启用实例构建失败才返回错误,Manager **保留旧输出**继续运行(配置已持久化,接口返回 502 让用户感知)。
- 从配置中删除的输出,其残留补传队列一并清空(用户删除=放弃缓冲数据);同 ID 重建则自动续传。

### 5.3 输出侧恒等与状态聚合

- 每个输出实例由 `OutputID` 恒等(配置主键),补传队列按它隔离。
- `Manager.Status()` 聚合:身份/启用/活跃、队列占用、Received/Dropped、补传队列深度 + 各输出 `RuntimeStatus`。

---

## 6. 分步实现:一个最小输出插件

以"新增平台 MyCloud"为例。

### 6.1 包布局与注册

```
internal/output/mycloud/
├── mycloud.go        # 输出实现
├── mycloud_test.go   # 单元测试
└── mycloud_e2e_test.go  # 端到端(可选)
```

```go
package mycloud

func init() {
    output.Register(output.Descriptor{...}, func(bc output.BuildContext, raw json.RawMessage) (output.Output, error) {
        return New(...)
    })
}
```

### 6.2 配置(Schema + Config)

配置存 SQLite,经 Web UI 表单编辑。Schema 声明字段(密码用 `FieldPassword` 自动加密/掩码),
`Config` 结构体 `json.Unmarshal` 解析,`New` 里给默认值并校验必填:

```go
type Config struct {
    Endpoint      string `json:"endpoint"`
    Token         string `json:"token"`
    FlushInterval string `json:"flushInterval"`
    BatchMax      int    `json:"batchMax"`
}

func New(cfg Config, outputID string, backfill output.BackfillSink) (output.Output, error) {
    if cfg.Endpoint == "" {
        return nil, errors.New("mycloud endpoint is required")
    }
    batchMax := cfg.BatchMax
    if batchMax <= 0 {
        batchMax = 64
    }
    flush := time.Second
    if cfg.FlushInterval != "" {
        d, err := time.ParseDuration(cfg.FlushInterval)
        if err != nil {
            return nil, fmt.Errorf("invalid flushInterval %q: %w", cfg.FlushInterval, err)
        }
        flush = d
    }
    // ...构造实例,启动 flusher
}
```

### 6.3 微批 flusher 模式

高频采集下逐条同步发送不可行——标准做法是**缓冲 + flusher 定时聚合批量上送**(参考 mqtt/thingsboard/tdengine):

```go
type mycloudOutput struct {
    outputID string
    backfill output.BackfillSink
    client   *http.Client

    mu      sync.Mutex
    pending []model.DataPoint

    done chan struct{}
    wg   sync.WaitGroup

    output.SendStats // 内嵌上送统计
}

func (o *mycloudOutput) Publish(dp model.DataPoint) error {
    o.mu.Lock()
    o.pending = append(o.pending, dp)
    o.mu.Unlock()
    return nil
}

func (o *mycloudOutput) runFlusher() {
    defer o.wg.Done()
    ticker := time.NewTicker(o.flushInterval)
    defer ticker.Stop()
    for {
        select {
        case <-o.done:
            return
        case <-ticker.C:
            o.flush()
        }
    }
}

func (o *mycloudOutput) flush() {
    o.mu.Lock()
    pending := o.pending
    o.pending = nil
    o.mu.Unlock()
    if len(pending) == 0 {
        return
    }
    // 编码为平台格式并批量上送;失败落库补传(见 §7.3)。
}
```

**要点**:
- `Publish` 只入缓冲(快、不阻塞);上送在 flusher 单 goroutine 串行执行。
- 缓冲设上限(如 `maxPendingPoints`),满时**落库补传**而非丢弃(参考各输出的 `saveBackfill`)。
- flusher 由 `Close` 关闭 `done` 后退出,`Close` 里 `wg.Wait()` 后**flush 剩余缓冲**(不丢尾数)。

### 6.4 Close

```go
func (o *mycloudOutput) Close() error {
    close(o.done)
    o.wg.Wait()   // 等 flusher 退出,避免与 flush 并发
    o.flush()     // 发送剩余缓冲
    return nil
}
```

### 6.5 注册进 main.go

```go
_ "iot-gateway-go/internal/output/mycloud" // 注册 mycloud 输出
```

### 6.6 输出配置的 CRUD 与热重载(零改动)

- `GET/POST/PUT/DELETE /api/v1/outputs` + `GET /api/v1/outputs/types`(受 `outputs:*` scope 保护)自动可用。
- 写/删后自动 `Manager.Reload()`;激活失败保留旧输出并返回 502。

### 6.7 Web UI(零改动)

- 新建输出下拉自动出现 `MyCloud`,表单按 Schema 渲染(`FieldPassword` 用密码框、不回显)。
- 状态面板自动显示 `RuntimeStatus`(连接态/积压/发送统计)与补传队列深度。

---

## 7. 能力接口实现

### 7.1 DeviceNotifier(上线/离线)

缓冲"意图"由 flusher 统一发出(参考 thingsboard 的 `connects`/`disconnects` map):

```go
func (o *mycloudOutput) DeviceOnline(deviceID string) {
    o.mu.Lock(); defer o.mu.Unlock()
    o.connects[deviceID] = true
}
func (o *mycloudOutput) DeviceOffline(deviceID string) {
    o.mu.Lock(); defer o.mu.Unlock()
    o.disconnects[deviceID] = true
}
// flusher 中:先 disconnect 后 connect,再发数据(保证平台侧状态时序)
```

### 7.2 StatusProvider(运行态)

内嵌 `output.SendStats`,在**真正上送完成/失败**处更新:

```go
func (o *mycloudOutput) publish(...) error {
    if err := ...; err != nil {
        o.SendStats.Failure(err)   // 真实上送失败
        return err
    }
    o.SendStats.Success(time.Now()) // 真实上送成功
    return nil
}

func (o *mycloudOutput) RuntimeStatus() output.RuntimeStatus {
    sent, lastSentAt, lastErr, lastErrAt := o.SendStats.Snapshot()
    return output.RuntimeStatus{
        Pending:     o.pendingCount, // 内部待发缓冲
        Sent:        sent,
        LastSentAt:  lastSentAt,
        LastError:   lastErr,
        LastErrorAt: lastErrAt,
    }
}
```

> **统计语义**:`Success`/`Failure` 只在"真正发送到上游"的路径上更新(MQTT Publish 完成、HTTP 写库成功),
> 不在 `Publish` 入缓冲时更新——否则 LastSentAt/LastError 不反映真实上送状态。

### 7.3 Backfill(断网补传)

三条丢点路径都要落库(`backfill.Save(outputID, dps)`),恢复后由 Manager 重放:

1. **扇出队列满**(Manager 自动处理,前提是输出实现 `BackfillHealthy`)。
2. **输出内部缓冲满**(`maxPendingPoints` 超限)。
3. **上送失败**(flusher 中 `publish` 失败——失败批次落库)。

```go
func (o *mycloudOutput) saveBackfill(dps []model.DataPoint) {
    if len(dps) == 0 || o.backfill == nil {
        return
    }
    if err := o.backfill.Save(o.outputID, dps); err != nil {
        slog.Error("mycloud backfill save failed", "err", err)
    }
}

// 健康判定:HTTP 类无长连接,以"最近上送无持续错误"为准(参考 tdengine):
func (o *mycloudOutput) BackfillHealthy() bool {
    _, lastSentAt, lastErr, lastErrAt := o.SendStats.Snapshot()
    if lastErr == "" {
        return true
    }
    if lastSentAt.After(lastErrAt) {
        return true // 失败后已有成功上送:已恢复
    }
    return time.Since(lastErrAt) > backfillBackoff // 失败后自然退避
}
```

- MQTT 类输出直接返回 `client.IsConnected()`(参考 mqtt/thingsboard)。
- 实现 `BackfillHealthy` 即声明支持补传(见 §2.4)。

### 7.4 下行(平台 → 设备)

需要"平台命令写回设备"的输出(thingsboard RPC/共享属性、smardaten 服务调用):

- 用 `bc.Write` 回调(最终落 `core.WritePoint` → 驱动 `Writer.Write`),不要直接碰 store/驱动。
- 订阅在 `OnConnect` handler 里做(连接/重连后重新订阅,参考 thingsboard)。
- 下行消息经**独立写队列 + goroutine**(非阻塞投递,队列满丢弃并告警),避免阻塞 MQTT 处理(参考 thingsboard `runWriter`)。

---

## 8. 设计模式与约定

1. **Publish 快、上送异步**:`Publish` 只入缓冲,批量上送在 flusher 单 goroutine。
2. **缓冲有界**:内部缓冲设上限,满则落库补传而非无界增长。
3. **MQTT 类输出统一用 `internal/output/mqttutil`**:`ApplyResilience`(非阻塞建连 + ConnectRetry 指数退避 + 有界等待)+ `ConnectNonBlocking`,避免重实现且超时语义一致(见 docs/mqtt-resilience-design.md)。
4. **SendStats 只在真实上送路径更新**(§7.2)。
5. **并发安全**:`Publish`/`DeviceNotifier`/下行 handler 与 flusher 并发访问的内部缓冲必须加锁;flusher 单 goroutine 专用状态(如 connected 集合)可不加锁。
6. **Close 顺序**:关闭 `done` → `wg.Wait()` → flush 剩余缓冲 → 断开连接(先停生产者再排空,不丢尾数)。
7. **密码/令牌字段用 `FieldPassword`**:自动加密落库、掩码返回、修改留空继承旧值。
8. **构造器失败不泄漏连接**:返回错误前关闭已建立的连接/资源。
9. **日志用 `log/slog`**,上下文含 `output` 实例 ID。
10. **不依赖北向其他插件**:输出只 import `internal/output`、`internal/model`(及第三方库/`mqttutil`),不 import 其他输出包或 core 具体实现。

---

## 9. 测试

### 9.1 单元测试(MQTT 类用假 broker)

- `internal/output/mqtttest` 提供:
  - `StartRecording(t)`:`RecordingBroker` 应答 CONNACK/PUBACK/SUBACK/PINGRESP 并记录 PUBLISH,用于断言发布条数/topic/payload。
  - `StartSilent(t)`:`SilentBroker` 只应答 CONNACK 不回 PUBACK,用于验证**有界等待不永久阻塞**。
- 覆盖:Publish 入缓冲/批量拆分、payload 编码、失败落库补传、BackfillHealthy 判定、RuntimeStatus 字段。

参考 `internal/output/mqtt/mqtt_test.go`、`internal/output/thingsboard/thingsboard_test.go`。

### 9.2 契约测试

- `Publish` 返回错误不 panic;`Close` 幂等、尾数不丢。
- 缓冲满(打满 `maxPendingPoints`)→ 落库补传而非丢弃。
- 未接线补传(`backfill==nil`)→ 退化为"丢弃 + 告警",不 panic。
- 构造失败(必填缺失/非法 flushInterval)→ 返回错误且不泄漏连接。

### 9.3 端到端

- 用 `mqtttest` 假 broker 或本地 HTTP server 打通 `Manager.Reload → Publish → 上送` 全链路。
- 补传链路:`Reload` 建输出 → 失败落库 → 恢复(假 broker 恢复响应)→ `runReplay` 重放 → Ack。
  参考 `internal/output/manager_backfill_test.go`。

### 9.4 验收用例清单

| 场景 | 期望 |
|---|---|
| 新建输出并启用 | `Reload` 后实例活跃,Web UI 表单/状态正常 |
| 单实例配置非法 | 该实例跳过,其余输出照常;全失败时旧输出保留 |
| 修改 broker/凭据 | 热重载,无需重启 |
| broker 不可达 | 非阻塞建连 + 后台重连;数据落补传队列不丢 |
| 半死 broker | Publish 有界等待后返回错误,不卡死 flusher |
| 缓冲打满 | 落库补传(支持补传的输出)或丢弃计数(不支持) |
| 高吞吐 | 微批聚合,单条消息有上限(batchMax 拆条) |
| 设备上线/离线 | DeviceNotifier 触发平台 connect/disconnect |
| 平台下行命令 | Write 回调写回设备,应答正确 |

---

## 10. 提交前检查清单

- [ ] `init()` 注册 + `main.go` 空导入,`GET /api/v1/outputs/types` 能看到类型
- [ ] Schema 字段齐全(Label/Type/Required/Default/Hint 合理;凭据用 `FieldPassword`)
- [ ] Publish 快(入缓冲)、上送异步(flusher)、缓冲有界(满则补传)
- [ ] Close 顺序正确:停 flusher → 排空剩余缓冲 → 断开,幂等
- [ ] 构造器失败不泄漏连接;必填/非法配置返回可读错误
- [ ] 实现 `StatusProvider` + `SendStats`(真实上送路径更新)
- [ ] 实现 `BackfillHealthy` 并在三条丢点路径落库补传(或明确选择不支持)
- [ ] 需要设备上下线 → 实现 `DeviceNotifier`;需要下行 → 用 `bc.Write`
- [ ] MQTT 类输出复用 `mqttutil` 韧性(不重实现)
- [ ] 单元/契约测试覆盖;`go build ./...`、`go test ./...`、`go vet ./...` 通过
- [ ] README 与本文档的输出列表同步

---

## 11. 参考

- 接口:`internal/output/output.go`、`registry.go`、`backfill.go`、`sendstats.go`
- 管理器(扇出/热重载/状态/补传重放):`internal/output/manager.go`、`buildset.go`
- 注入与接线:`cmd/gateway/main.go:buildOutputs`
- MQTT 韧性工具:`internal/output/mqttutil/mqttutil.go`
- 测试假 broker:`internal/output/mqtttest`
- 参考实现:
  - `internal/output/mqtt`(即时 + 批量、补传、统计)
  - `internal/output/thingsboard`(DeviceNotifier + 下行 + 微批)
  - `internal/output/tdengine`(HTTP 类、BackfillHealthy 退避判定)
  - `internal/output/sparkplugb`(StoreAccessor 枚举设备、出生序列)
  - `internal/output/smardaten`(配置同步 + 诊断)
- 设计文档:`northbound-output-config.md`、`mqtt-resilience-design.md`、`offline-backfill-design.md`、`output-status-design.md`、`mqtt-batch-publish-design.md`
