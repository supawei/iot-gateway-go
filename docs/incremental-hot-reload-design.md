# 增量热加载设计文档

## 1. 背景与问题

当前 `Scheduler.reload()` 是**全量停-重建**:任何一次配置变更(`store.OnChange()` 覆盖连接/设备/点位/输出/网关 ID 的增删改)都会 `stopCollectors()`——取消全部 Read、停掉整个 cron、关闭全部设备连接,再按新配置并行重开所有连接。

带来的实际问题:

| 问题 | 表现 |
|---|---|
| **全局采集空窗** | 改一个点位地址,所有设备(含无关设备)暂停采集一段时间 |
| **连接 churn** | 串口重开、OPC UA 会话重建、订阅重挂,未变更的连接也重连,易抖动/离线误报 |
| **无关变更误伤** | 只改输出配置或网关 ID 也会触发调度器全量重载(store 统一 notify) |

ROADMAP P3 项"增量热加载:scheduler 对设备/点位做 diff,增删改而非全量重启"。

## 2. 设计目标与非目标

### 2.1 目标

- **未变的连接与设备零打扰**:编辑任一设备/点位/输出/网关 ID 时,其余设备的连接与采集完全不受影响。
- **轮询设备(poll)原地更新**:点位/间隔变化不重连、不重建 cron 调度相位;参数(从机地址等)变化只换设备级 conn,物理连接经驱动连接池保持。
- **连接级粒度**:连接配置/驱动变化只重开该连接下的设备;连接删除/新增同理。
- **订阅/监听连接**:纯新增设备可增量;涉及删除/点位变化的设备时,以"整组重开"兜底(共享订阅无单设备卸载能力),但**不影响其他连接**。
- **首次加载 = 现状**:首次(无运行时状态)按新配置全量打开,行为与当前一致。

### 2.2 非目标

- 共享订阅/监听的**单设备增量卸载**:OPC UA `sharedSession.release` 仅在 refCount 归零时撤销订阅,`targets` 无单设备清理接口;modbus_listen 同理。v1 不做驱动级 `Reconfigure` 能力,以"整组重开"保证正确性(未来按需加)。
- 采集语义变更:点位数据的读/写语义不变,只改"何时/如何加载"。
- 跨设备数据去重或迁移。

## 3. 方案总览

调度器把"每设备运行态"显式建模为 `deviceRuntime`,配置变更时对新旧配置做 **diff + reconcile**:

```
store.OnChange → reload()
  ├─ 读新配置,构建 desired(deviceID → {connKey, sig})
  ├─ 对比当前 runtimes(deviceID → runtime)
  ├─ 生成操作集:keep / add(开+注册) / remove(停)
  │                 / poll-update(原地改点/换job) / group-restart(整组停+重开)
  └─ 执行:仅对受影响设备操作;任务池与 cron 实例跨 reload 保持
```

**diff 键**:
- `connKey` = `connectionID + driver + connConfig(JSON)` —— 物理/逻辑连接身份;变了即连接级变更。
- `sig` = `deviceParams + points + intervalMs + enabled` —— 采集规格;变了即设备级变更。

**reconcile 规则**(按连接组):
- 组只在旧侧 → 连接删除,停该组全部设备。
- 组只在新侧 → 连接新增,开该组全部设备。
- 组两侧都在(连接未变),按设备:
  - `sig` 未变 → **keep**(零操作)。
  - 轮询设备 `sig` 变 → **原地更新**(见 §4.3)。
  - 订阅/监听组内出现删除或 `sig` 变 → **整组重开**(见 §4.4);仅纯新增 → 增量 add。
- 组 `connKey` 变(连接配置/驱动/ConnectionID 变)→ 旧组停 + 新组开。

## 4. 详细设计

### 4.1 运行态建模

```go
type collectMode int
const (collectPoll collectMode = iota; collectSubscribe; collectListen)

type deviceRuntime struct {
    deviceID string
    conn     driver.Conn
    mode     collectMode
    // poll: cron 条目 + 任务
    cronID   cron.EntryID
    job      *deviceJob
    // subscribe/listen: 设备级 ctx 取消 + 退出信号
    cancel   context.CancelFunc
    subDone  <-chan struct{}
    // diff 键
    connKey  string
    sig      string
}
```

`Scheduler` 字段调整:以 `runtimes map[string]*deviceRuntime` 取代 `conns []driver.Conn`;`cron`/`taskCh`/`workers`/`collectCtx` 变为**跨 reload 持久**(首次创建,shutdown 才销毁)。

### 4.2 执行顺序与并发

- reload 仅在 `Run` 循环单 goroutine 触发,reconcile 天然串行;worker 只消费 taskCh,不读 runtimes。
- 需要**重开**的设备:先开新 conn(并行、受 poolSize 限流,与现状一致),注册后再关旧 conn——同 `connKey` 组内驱动池复用物理连接,无空窗;连接配置变化的组则必须先停旧(释放池项)再开新。
- `deviceJob` 的 `points` 改为锁内可替换,原地更新不影响已投递任务(迟到的旧点位采集一次,可接受)。

### 4.3 轮询设备原地更新

| 变化 | 操作 | 连接影响 |
|---|---|---|
| 仅点位(`points`)变 | `job.setPoints(new)` | 无 |
| `intervalMs` 变 | 移除旧 cron 条目 + 新增新间隔 job(新点位) | 无 |
| 设备参数(`params`,如 slaveId)变 | 新开设备 conn(驱动池复用物理连接)+ 重建 job,再关旧 conn | 物理连接不动 |
| `enabled` false | 视为 remove | — |

### 4.4 订阅/监听连接

- **纯新增设备** → `Subscribe`/`Listen` 在该连接共享会话上登记(现状已支持多设备共享,干净增量)。
- **删除设备 / 点位变化 / 参数变化** → 整组重开:停该组全部设备(最后一个 `Close` 触发 refCount 归零,会话/订阅/监听正确撤销),再按新配置开该组全部设备。其他连接不受影响。

### 4.5 持久化资源与关闭

- `cron` 实例、`taskCh`、worker pool、`collectCtx` 在首个 reload 创建,后续 reload 复用;
- 设备停止:轮询 → `cron.Remove(entryID)` + `conn.Close()`;订阅/监听 → `cancel()` + 等 `subDone` + `conn.Close()`(组内最后一个触发共享资源释放);
- `Run` ctx 取消(进程关闭):停 cron、关 taskCh、等 workers、按组释放全部连接(等价旧 `stopCollectors`)。

### 4.6 状态与通知

- `markOnline/markOffline` 语义不变;原地更新点位不触发状态翻转。
- 连接打开失败、驱动未注册等仍按设备级失败隔离(标记离线、继续其余)。

## 5. 常量与参数

| 项 | 说明 |
|---|---|
| `connKey` | `connectionID \x00 driver \x00 connConfigJSON` 的字符串 |
| `sig` | `paramsJSON \x00 intervalMs \x00 pointsJSON` 的字符串(含 enabled) |
| 打开并发 | 复用 `poolSize` 信号量(与现状一致) |

## 6. 行为变化与兼容性

- 未变更的设备连接不再随任意配置保存而关闭重开(主要收益)。
- 连接配置/驱动变更、删除 → 仅该连接组重开,其余不变。
- 订阅/监听组内删除/点位变更 → 该组重开(行为正确性优先),其余不变。
- 首次启动、整体启动路径:全量打开,行为与当前完全一致。
- API/Web 无接口变化(配置模型不变),仅调度行为更精细。

## 7. 风险与已知限制

- **共享订阅无单设备卸载**(OPC UA / modbus_listen):删除或改点走整组重开,该组短暂断采;驱动级 `Reconfigure` 留待未来按需实现。
- 关闭设备 conn 时,该设备可能仍有在途采集任务 → 对已关连接执行 Read 返回错误并记离线;设备已从配置移除,无害(现状全量重载同样存在边界)。
- 轮询点位原地更新:在途旧点位任务最多再采集一次,可接受。

## 8. 测试计划

### 8.1 单测(核心 reconcile)

- 无变化 reload → 所有设备 keep(连接对象引用不变、cron 条目数不变)。
- 新增设备 → 仅新增打开;其余设备 conn 对象不变。
- 删除设备 → 仅关闭该设备 conn。
- 轮询点位变化 → 原地更新(conn 不变、job 点位更新);间隔变化 → cron 条目替换。
- 轮询参数变化 → conn 重开(驱动池复用),物理连接不关。
- 连接配置变化 → 该连接组全部重开,其他连接不动。
- 订阅/监听组:纯新增增量;删除/改点整组重开;其他连接不动。
- 输出/网关 ID 变更(设备配置不变)→ 调度器零操作。

### 8.2 集成

- 用 mock 驱动记录 Open/Close/Read/Subscribe 调用次数,验证"编辑 A 设备,不重开 B 设备连接"。
- `go test ./...` 全绿;网关启动冒烟。

## 9. 实施里程碑

1. `deviceRuntime` + diff 计算 + 操作集生成。
2. poll 设备增量(原地更新/换 job)。
3. 连接组管理(新增/删除/配置变更)。
4. 订阅/监听组重开兜底。
5. 持久化 cron/taskCh/workers 改造 + 关闭路径。
6. 测试 + 文档(ROADMAP / 本设计文档实施记录)。

## 10. 决策记录

| 决策点 | 选择 | 理由 |
|---|---|---|
| diff 粒度 | 设备级 `sig` + 连接级 `connKey` | 一次配置保存可能改多设备多连接;键驱动 diff 天然覆盖批量变更 |
| 轮询设备 | 原地更新点位/换 job,参数变化仅换设备 conn | 轮询是主流(Modbus),物理连接复用即最大收益 |
| 订阅/监听组 | 纯新增增量;删除/改点整组重开 | 驱动共享订阅无单设备卸载接口;整组重开保证正确性,其余连接不受影响 |
| 持久化资源 | cron/taskCh/workers/collectCtx 跨 reload 复用 | 避免"每次保存都重建调度骨架";配合增量操作 |
| 连接配置变更 | 该组先停后开 | 驱动池按 ConnectionID 复用,不先释放则复用旧配置连接 |
| 操作执行顺序 | 重开组先开新后关旧(同 connKey 内) | 驱动池复用物理连接,无空窗 |

## 11. 实施记录

### 11.1 代码变更(2026-08-18)

- **`internal/core/scheduler.go`**(重构):
  - 新增 `deviceRuntime`(connKey/sig/mode/job/cronID)与 `collectMode`(poll/subscribe/listen)。
  - `reload()` 改为 diff + reconcile:按连接组增量执行 keep/add/remove/poll-update/group-restart;未变设备零操作。
  - `cron`/`taskCh`/workers/`collectCtx` 跨 reload 持久(首次创建,进程关闭才销毁);`stopCollectors` 仅用于进程关闭。
  - 轮询设备:点位变化 `deviceJob.setPoints` 原地更新;间隔变化替换 cron 条目;参数变化先开新 conn(驱动池复用物理连接)再关旧。
  - 订阅/监听组:纯新增增量;删除/点位/参数变化整组重开(共享订阅无单设备卸载能力)。
- **`internal/core/scheduler_incremental_test.go`**(新增):10 个增量场景测试(无变化零操作 / 增删设备 / 点位·间隔·参数变化 / 连接配置变化 / 订阅组纯新增与整组重开 / 输出变更不碰调度器)。

### 11.2 测试

- 增量测试全过,`go test -race ./internal/core/` 无数据竞争,`go test ./...` 全绿。
- 网关启动冒烟正常。

### 11.3 已知限制(见 §7)

- 订阅/监听组内删除或点位变化走整组重开(该组短暂断采,其他连接不受影响);驱动级 `Reconfigure`(单设备卸载)留待未来按需实现。
