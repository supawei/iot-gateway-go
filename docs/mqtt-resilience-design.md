# MQTT 连接与发送韧性改造设计文档

> **状态**:已实现(M1–M4 完成 + 全量回归 + 端到端场景验证)
> **关联**: [smardaten-iot.md](smardaten-iot.md)、[northbound-output-config.md](northbound-output-config.md)
> **更新**: 2026-08-18
> **范围**: `internal/output/mqtt`、`internal/output/thingsboard`、`internal/output/smardaten`、`internal/output`(注册表/管理器)、`cmd/gateway/main.go`

---

## 1. 背景与问题

分析确认当前存在两类真实缺陷(均有 paho v1.5.1 源码核对 + 本地实测复现):

### 1.1 问题 1:网关启动卡在 MQTT 连接

构建/重载链路为**主协程同步**执行:`main()` → `outputs.Reload()` → `buildOutputs()` → `output.Build()` → 各输出 `New()`(`cmd/gateway/main.go:67-71,199`)。任一 `New()` 阻塞,HTTP 服务、调度器都起不来。

| 输出 | 现状 | 实测 |
|---|---|---|
| `smardaten-iot` | `SetConnectRetry(true)` + `token.Wait()`(`smardaten.go:265-297`)。paho 的 Connect token 在 `ConnectRetry=true` 时**直到连上或被取消才完成**(client.go:263-285 死循环重试);`SetConnectTimeout(5s)` 只限制**单次**拨号,不限制整个重试循环 | 黑洞地址下 8s 仍未返回,**永久卡死** |
| `mqtt` | 未设 `SetConnectTimeout`(默认 30s)、未设 `ConnectRetry`(`mqtt.go:59-71`) | 阻塞 30s 后失败 |
| `thingsboard` | 同上(`thingsboard.go:130-144`) | 阻塞 30s 后失败 |

> 注:`smardaten-iot.md` §11.3 曾记录「新增输出后网关不响应 → 加 `SetConnectTimeout(5s)`」,该修复**不完整**:它只限制了单次拨号,`ConnectRetry(true)` 使 `token.Wait()` 仍无限阻塞。

附带缺陷:`buildOutputs` 中**任一**输出构建失败会关闭已建好的全部输出并整体返回错误(`main.go:199-207`),健康输出被一并丢弃,直到下次手动重载。

### 1.2 问题 2:发送消息时卡在 MQTT

| 输出 | 现状 | 实测 |
|---|---|---|
| `mqtt` | `Publish()` 内 `token.Wait()` 无超时(`mqtt.go:80-82`),在专属发布 goroutine 中同步执行 | 对「接受连接但停止应答」的半死 broker,QoS1 publish 12s 仍未返回(**永久阻塞等 PUBACK**,paho 无 ack 超时、无 socket 写 deadline) |
| `smardaten-iot` | `flush()` 内 `token.Wait()`(`smardaten.go:599-602`);且 `pending` 缓冲**无上限**,断连期内存无界增长 | 同上 |
| `thingsboard` | `publish()` 内 `token.Wait()`(`thingsboard.go:396-404`) | 同上 |

阻塞只发生在该输出专属 goroutine(manager.go:112-124 每输出独立队列 + 非阻塞扇出),**全局不会冻结**,但该输出的数据流停摆、1024 队列打满后持续丢点,且 `Reload()`/`Close()` 的 `s.wg.Wait()` 被卡死 goroutine 拖住,热重载也会卡。

**「什么时候阻塞」语义**(paho client.go:772-834):
- 完全断开、未在重连 → `Publish` 立即返回 `ErrNotConnected`,不阻塞;
- 正在重连(AutoReconnect/ConnectRetry)→ 消息落内存 store、立即返回,不阻塞;
- **半死状态**(TCP 未断/keepalive 未触发、broker 不响应)→ paho 视为 connected → 走 obound 发送 → **永久阻塞**。这是最危险窗口(keepalive 30~60s 内的故障都处于该状态)。

---

## 2. 设计目标与非目标

### 2.1 目标

1. **启动不阻塞**:broker 不可达时,网关照常启动(HTTP/调度/采集全在线),MQTT 后台自动重连;连接恢复后自动订阅并补发。
2. **发送不阻塞**:单条发布等待有上界(超时报错返回),绝不永久卡死;MQTT 故障只影响该输出,不拖累全局与热重载。
3. **失败隔离**:单个输出构建失败只跳过该输出,其余正常生效;避免「一个坏配置拖垮全部输出」。
4. **内存有界**:输出内部缓冲设上限,断连期内存不无限增长。

### 2.2 非目标

- 不改变 `Output` / `DeviceNotifier` 接口签名、不改变配置结构(无新配置字段)。
- 不改变南向采集(driver / scheduler / pipeline)逻辑。
- 不做持久化消息队列:断连期间的 QoS1 消息由 paho 内存 store 暂存,**进程重启即丢**(保持现有 QoS 尽力而为语义)。
- 不升级 paho v2(其 timeout/ack 机制差异较大,留作后续评估)。

---

## 3. 方案总览

新增 1 个共享工具包 + 4 项改造:

| 编号 | 改造 | 涉及文件 |
|---|---|---|
| A | 共享 `internal/output/mqttutil`(超时工具 + 连接选项模板) | 新增 |
| 1 | 连接建立非阻塞(启动不卡) | mqtt / thingsboard / smardaten 的 `New` |
| 2 | 发送有界等待(发送不卡) | mqtt.Publish / smardaten.flush / thingsboard.publish |
| 3 | 输出构建失败隔离(单点失败不拖累全体) | `output.BuildSet` 新增 + `main.buildOutputs` |
| 4 | smardaten 缓冲上限(内存有界) | smardaten.Publish / flush |

---

## 4. 详细设计

### 4.0 改造 A:共享工具包 `internal/output/mqttutil`

三个 MQTT 类输出共享同一套超时语义,抽公共包避免三份重复与不一致。包只依赖 paho,不依赖各输出包(无循环依赖)。

```go
package mqttutil

const (
    ConnectTimeout        = 5 * time.Second   // 单次拨号/握手超时
    ConnectProbe          = 5 * time.Second   // 首次连接仅用于日志的探测等待
    ConnectRetryInterval  = 2 * time.Second   // 首次连接重试间隔
    MaxReconnectInterval  = 30 * time.Second  // 重连指数退避上限
    PublishTimeout        = 5 * time.Second   // Publish token 有界等待
    WriteTimeout          = 5 * time.Second   // paho obound 投递超时
)

// ErrPublishTimeout 发布等待超时哨兵错误,可 errors.Is 判断。
var ErrPublishTimeout = errors.New("mqtt publish timeout")

// WaitToken 有界等待 paho token 完成;超时返回 ErrPublishTimeout。
// 基于 paho 原生 Token.WaitTimeout,超时时 token 不置错,可再次等待。
func WaitToken(t paho.Token, d time.Duration) error {
    if t.WaitTimeout(d) {
        return t.Error()
    }
    return fmt.Errorf("%w (after %v)", ErrPublishTimeout, d)
}

// ApplyResilience 把连接韧性参数统一施加到客户端选项上。
// 各输出仍自行设置 broker/clientID/凭据/协议版本等差异化项。
func ApplyResilience(opts *paho.ClientOptions) {
    opts.SetConnectTimeout(ConnectTimeout)
    opts.SetConnectRetry(true)          // 关键:覆盖「从未连上」场景,后台无限重试
    opts.SetConnectRetryInterval(ConnectRetryInterval)
    opts.SetMaxReconnectInterval(MaxReconnectInterval)
    opts.SetAutoReconnect(true)
    opts.SetWriteTimeout(WriteTimeout)  // 限制 obound 投递,配合 WaitToken 兜底
}
```

**为什么保留 `SetConnectRetry(true)`**:paho 的 `AutoReconnect` 只在「已连接后断开」时生效,**首次连接失败后不会触发**——这正是原 bug 的场景(broker 在网关启动后才恢复时,网关必须能自己连上)。`ConnectRetry=true` 且我们**从不阻塞等待**其 token 时,恰好实现「先启动、后台重连、连接前 publish 落内存 store 待补发」。

---

### 4.1 改造 1:连接建立非阻塞(启动不卡)

三个输出的 `New` 统一改为「非阻塞构造」:

```go
client := pahomqtt.NewClient(opts)
tok := client.Connect()
// 仅用于日志探测;绝不阻塞构建。
if tok.WaitTimeout(mqttutil.ConnectProbe) {
    if err := tok.Error(); err != nil {
        slog.Warn("mqtt initial connect failed, retrying in background", "err", err)
    } else {
        slog.Info("mqtt connected")
    }
} else {
    slog.Warn("mqtt initial connect not established in time, retrying in background")
}
// 连接失败不再是构建失败:返回输出实例,由后台重连兜底。
```

- `New()` 不再因连接失败返回 error → 输出始终被构建 → 网关启动不阻塞、重载不阻塞。
- 重连期间 paho 自动处理(QoS1 落内存 store,连上后补发)。

#### 4.1.1 各输出的差异化调整

| 输出 | 调整 |
|---|---|
| `mqtt` | `New` 套用 `ApplyResilience`;无订阅,无需 OnConnect |
| `smardaten-iot` | 删除 `connectMQTT` 中的 `token.Wait()` 阻塞语义(保留 `SetProtocolVersion(4)`、keepalive 60s 等差异化项);**订阅迁入 OnConnect**(见 4.1.2) |
| `thingsboard` | 同 mqtt;**attributes/rpc 两个订阅迁入 OnConnect**(见 4.1.2) |

#### 4.1.2 订阅迁入 OnConnect

非阻塞构造后,连接未必就绪,`New()` 里直接 `Subscribe` 会立刻返回 `ErrNotConnected`,故静态订阅须迁到连接建立回调:

```go
opts.SetOnConnectHandler(func(paho.Client) {
    // paho 约定:OrderMatters 下回调不得阻塞或调用会阻塞的包内函数,故起 goroutine。
    go func() {
        if err := o.subscribeAll(); err != nil {
            slog.Error("smardaten resubscribe after connect failed", "err", err)
        }
    }()
})
```

- **smardaten**:`subscribeAll()`(静态 `/sys/{gw}/thing/config/set`、`diagnose/set` + 动态服务 topic)整体迁入 OnConnect。`loadConfig()` 里的 `resubscribeServices()` 保留:配置热更新后立即订阅动态 topic,不依赖重连。
- **thingsboard**:`topicAttributes` / `topicRPC` 两个 `Subscribe` 迁入一个 `onConnectSubscribe()`。
- 并发安全:`topicMapping` 自带 `RWMutex`(`application.go:201-203`);paho 的 `Subscribe` 并发安全,MQTT SUBSCRIBE 幂等(同 topic 重复订阅仅覆盖 handler),`OnConnect` 与 `loadConfig` 触发的重订阅无副作用。
- 订阅失败仅记日志,不阻塞、不返错。

---

### 4.2 改造 2:发送有界等待(发送不卡)

所有**数据上行路径**的 `token.Wait()` 改为 `mqttutil.WaitToken(token, mqttutil.PublishTimeout)`:

| 位置 | 现状 | 改为 |
|---|---|---|
| `mqtt.go:80-82` `Publish` | `token.Wait(); return token.Error()` | `return mqttutil.WaitToken(token, mqttutil.PublishTimeout)` |
| `smardaten.go:599-602` `flush` 属性上报 | `if token := Publish(...); token.Wait() && token.Error() != nil { ... }` | `if err := mqttutil.WaitToken(token, mqttutil.PublishTimeout); err != nil { slog.Error(...); continue }` |
| `thingsboard.go:396-404` `publish` | `token.Wait(); return token.Error()` | `return mqttutil.WaitToken(token, mqttutil.PublishTimeout)` |

- **下行响应类发布**(smardaten `publishConfigResponse` / `handleService*` / `handleDiagnose` / `publishDeviceStatus`,均为 fire-and-forget、不 `Wait`):不改(调用方不阻塞,断连时由 paho 存储)。**可选优化**:响应类也加 `WaitToken` 以便失败记日志,列入里程碑 M2 时评估。
- `ApplyResilience` 中的 `SetWriteTimeout(5s)` 限制 obound 投递,防止半死 broker 时 `Publish` 长期占用 messageID;`WaitToken` 兜底我们自己的等待。

> **已知限制**:paho v1 不为底层 socket 设置写 deadline,半死 broker 时 paho 内部 writer 可能卡 OS 级(分钟级,由 TCP 超时最终恢复)。`WaitToken` 保证**我方 goroutine 有界等待**,不会永久卡死调用方——这是本设计对「发送不卡」的承诺边界(见 §7)。

---

### 4.3 改造 3:输出构建失败隔离

在 `internal/output` 新增纯函数 `BuildSet`,把「逐输出构建 + 隔离」逻辑从 `main` 提出,便于测试:

```go
// BuildSet 逐个构建输出;单个失败跳过并告警,不拖垮其余。
// 返回 (outputs, nil) 表示至少一个成功(失败项已跳过);
// 返回 (nil, err) 表示有启用项但全部失败(调用方应保留旧输出)。
func BuildSet(bc BuildContext, configs []model.Output) ([]Output, error) {
    result := make([]Output, 0, len(configs))
    enabled := 0
    for _, o := range configs {
        if !o.Enabled {
            continue
        }
        enabled++
        out, err := Build(bc, o.Type, o.Config)
        if err != nil {
            slog.Error("build output failed, skipped", "id", o.ID, "type", o.Type, "err", err)
            continue
        }
        result = append(result, out)
    }
    if enabled > 0 && len(result) == 0 {
        return nil, fmt.Errorf("all %d enabled outputs failed to build", enabled)
    }
    return result, nil
}
```

`main.buildOutputs` 变薄:读 `gatewayID` + `ListOutputs()` → 调 `output.BuildSet`。

**语义**:
- ≥1 个成功 → 安装新集合(部分降级),失败项跳过;
- 全失败 → 返回 error,`Manager.Reload` 保留旧输出(防误伤,避免把健康运行的旧输出清空);
- 无启用项 → 返回空集(正常清空)。

删除现有「任一失败即关闭已构建输出并整体回滚」逻辑(`main.go:199-207`)。

> 注:改造 1 后,broker 不可达不再导致构建失败,改造 3 主要兜底「配置错误 / 类型未注册」等确定性失败。

---

### 4.4 改造 4:smardaten 缓冲上限(内存有界)

`platformOutput` 增加全局缓冲计数,与 `pending` 一起在 `Publish`/`flush` 维护:

```go
const maxPendingPoints = 8192 // 全局待上报缓冲上限

// Publish 中:
o.mu.Lock()
if o.pendingCount >= maxPendingPoints {
    o.mu.Unlock()
    slog.Warn("smardaten pending buffer full, drop datapoint", "device", dp.DeviceID)
    return nil
}
o.pending[deviceID] = append(o.pending[deviceID], dp)
o.pendingCount++
o.mu.Unlock()
```

- `flush()` swap 出 `pending` 后重置 `o.pendingCount = 0`(已随旧 map 一起清空)。
- 兜底对象是「长时间断连时 200ms flush 仍不断灌入」的场景;正常情况 flush 每 200ms 清空,缓冲保持很小。
- **可选(建议一并做)**:thingsboard 的 `pending map[string][]model.DataPoint` 同样无上限(`thingsboard.go:188-199`),采用同一模式加全局上限(成本极低,杜绝同类问题)。tdengine 无缓冲队列,不需要。

---

## 5. 常量与参数汇总

| 参数 | 值 | 用途 |
|---|---|---|
| `ConnectTimeout` | 5s | 单次 TCP 拨号/握手超时 |
| `ConnectProbe` | 5s | 首次连接探测等待(仅日志) |
| `ConnectRetryInterval` | 2s | 首次连接重试间隔 |
| `MaxReconnectInterval` | 30s | 重连指数退避上限 |
| `PublishTimeout` | 5s | 发布 token 有界等待 |
| `WriteTimeout` | 5s | paho obound 投递超时 |
| `maxPendingPoints` | 8192 | smardaten 待上报缓冲全局上限 |
| keepalive | 60s(smardaten 现有) | MQTT 保活,维持现状 |

均为代码常量,不新增配置字段(保持配置结构不变)。

---

## 6. 行为变化与兼容性

| 场景 | 现状 | 改造后 |
|---|---|---|
| broker 不可达启动 | 网关卡死/延迟 30s | 网关照常启动,后台重连,日志可见 |
| 热重载,某输出构建失败 | 全部输出被关闭并回滚 | 跳过该输出,其余生效;全失败保留旧输出 |
| broker 半死时发送 | 发布 goroutine 永久卡死 | 单条 ≤5s 超时报错,不卡全局 |
| 断连期间上报 | mqtt 输出立即报错丢点;smardaten 内存无限增长 | QoS1 消息落内存 store 待重连补发;smardaten 缓冲有界 |
| 连接恢复 | 依赖 AutoReconnect(仅「先连后断」) | 首连失败也自动重连 + OnConnect 重订阅 |

兼容性:接口签名、配置结构、QoS 语义、平台契约均不变。

---

## 7. 风险与已知限制

1. **paho socket 写无 deadline(已知限制)**:半死 broker 时 paho 内部 writer 可能卡 OS 级。缓解:我方 `WaitToken` 有界;keepalive 最终触发断开进入重连。彻底解决需升级 paho v2 或引入 PingReq 健康探针(均不在本期,列为后续评估)。
2. **内存 store 重启丢失**:断连期间暂存的消息在进程重启后丢失(现有 QoS1 尽力而为语义,不改变)。
3. **OnConnect 订阅与 loadConfig 并发**:SUBSCRIBE 幂等 + paho 并发安全,无副作用;订阅失败仅日志。
4. **响应类发布无超时日志(可选优化)**:下行响应 fire-and-forget 不阻塞,但失败无感知;如需要,在 M2 一并加 `WaitToken` + 日志。

---

## 8. 测试计划

### 8.1 单元测试

| 测试 | 方法 |
|---|---|
| `mqttutil.WaitToken` 超时 | 本地起「accept + 回 CONNACK 后静默」的假 broker(与实测复现同法),断言 `Publish` 在 ≤`PublishTimeout` 内返回 `ErrPublishTimeout`,不永久卡 |
| mqtt `New` 非阻塞 | 对黑洞地址(192.0.2.1:1883)构造,断言 `New` 在 ≤`ConnectProbe+ε` 内返回且无 error |
| smardaten pending 上限 | 包内直接构造 `platformOutput`,`Publish` `maxPendingPoints+1` 次,断言计数封顶、不 panic |
| `output.BuildSet` 隔离 | 注册一个返回 error 的临时输出类型 + 一个正常类型,断言失败项被跳过、成功项保留;全失败返回 error |
| thingsboard OnConnect 订阅 | 断言 `onConnectSubscribe` 对静默 broker 不阻塞、不返错 |

### 8.2 手测/集成场景

| 场景 | 预期 |
|---|---|
| S1 broker 不可达启动 | 网关 ≤5s 内 HTTP 就绪,日志显示「retrying in background」 |
| S2 启动不可达 → 中途恢复 | 自动连接 + OnConnect 重订阅成功,缓存消息补发 |
| S3 broker 半死(accept 不 ack) | 发布 5s 超时报错,不卡死;broker 恢复后自愈 |
| S4 多输出其一不可达 | 该输出跳过,其余正常上报,无整体回滚 |
| S5 长时间断连 | smardaten 内存有界(pending 不增长)、日志限频告警 |

### 8.3 回归

- `go test ./...` 全绿;
- 现有 `smardaten_test.go` / `thingsboard_test.go` / `manager_test.go` 等不受影响。

---

## 9. 实施里程碑(每步 `go test ./...` 全绿)

| 里程碑 | 内容 | 验证 |
|---|---|---|
| M1 | 新增 `internal/output/mqttutil` + 单测;`mqtt` 输出接入(改造 1+2) | 单测 + S1/S3(mqtt) |
| M2 | `smardaten` 改造:非阻塞连接 + OnConnect 订阅 + flush `WaitToken` + pending 上限(改造 1/2/4);评估响应类发布是否加超时日志 | 单测 + S1/S2/S3/S5 |
| M3 | `thingsboard` 改造:非阻塞连接 + OnConnect 订阅 + `publish` `WaitToken`(改造 1/2) | 单测 + S1/S3(tb) |
| M4 | `output.BuildSet` 隔离 + `main.buildOutputs` 改造(改造 3)+ 单测;全量回归 | 单测 + S4 + 全量手测 |

---

## 10. 决策记录

| 决策点 | 选择 | 理由 |
|---|---|---|
| 连接失败是否仍导致构建失败 | **否**,非阻塞构造 | 符合「网关先启动」目标;broker 恢复后自动兜底 |
| 是否保留 `ConnectRetry(true)` | **保留**(配合不阻塞) | `AutoReconnect` 不覆盖「从未连上」场景,正是原 bug 场景 |
| 用 `WaitTimeout` 还是 select+time.After | `WaitTimeout` | paho 原生 API,超时不置错可再等,语义最简 |
| 订阅迁 OnConnect 还是继续 New 里等 | 迁 OnConnect | 非阻塞构造下 New 里订阅必失败;OnConnect 是 paho 推荐模式,且覆盖重连重订阅 |
| 全失败保留旧输出还是清空 | 保留旧输出 | 防误伤健康运行中的旧输出 |
| pending 全局上限 vs 每设备上限 | 全局上限 | 简单、内存可控;每设备公平性列为可选演进 |
| 是否建共享工具包 | 是 | 三输出复用,避免超时语义漂移;无循环依赖 |
| 下行响应类发布是否加超时 | **保持 fire-and-forget** | 不阻塞 paho 消息泵;`SetWriteTimeout` 已限制 obound 投递,失败有界 |

---

## 11. 实施记录

### 11.1 代码变更

| 文件 | 变更 |
|---|---|
| `internal/output/mqttutil/mqttutil.go`(新增) | `WaitToken` / `ApplyResilience` / `ConnectNonBlocking` + 超时常量 |
| `internal/output/mqtt/mqtt.go` | `New` 非阻塞构造;`Publish` 有界等待 |
| `internal/output/smardaten/smardaten.go` | 非阻塞连接 + OnConnect 订阅 + flush 有界等待 + pending 上限 |
| `internal/output/thingsboard/thingsboard.go` | 非阻塞连接 + OnConnect 订阅 + publish 有界等待 + pending 上限 |
| `internal/output/buildset.go`(新增) | `BuildSet` 失败隔离 |
| `cmd/gateway/main.go` | `buildOutputs` 改调 `BuildSet`,删除整体回滚逻辑 |
| `internal/output/mqtttest/mqtttest.go`(新增) | 测试用静默假 broker |

### 11.2 验证中发现并修复的时序 bug

端到端场景 S3(半死 broker)复现了一个实现初期引入的崩溃:`New` 里先 `ConnectNonBlocking` 后赋值 `o.client`,但 paho 在 **`Connect()` 内可能同步触发 `OnConnect`**,此时 `o.client` 尚为 nil → `subscribeAll`/`onConnectSubscribe` 中 `o.client.Subscribe` nil 解引用 panic。

修复:先赋值 `o.client` 再发起连接(smardaten 的 `connectMQTT` 与 thingsboard 的 `New` 均调整),并新增 `TestNewWithReachableBroker` 回归测试守护该时序。

### 11.3 验证结果

- 单测 + 竞态检测(`go test -race ./internal/output/...`)全绿。
- 端到端(真实二进制 + 改动过的 SQLite 输出配置):
  - S1 broker 不可达启动 → HTTP 约 5s 内就绪,日志 `retrying in background`(原先永久卡死);
  - S3 半死 broker → 连接成功、订阅 5s 超时记日志、网关保持存活,HTTP 200;
  - S4 单输出构建失败 → 仅跳过该输出,其余(mqtt,broker 黑洞)照常构建运行,无整体回滚。
