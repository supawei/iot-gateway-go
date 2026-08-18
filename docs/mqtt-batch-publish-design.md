# MQTT 批量发布设计文档

## 1. 背景与问题

`internal/output/mqtt` 目前是**单点即发**:每个采集 DataPoint 一次 `Publish` → 一条 MQTT 消息(`gateway/{gw}/device/{dev}/data`,payload 为单个 DataPoint JSON),每次 `WaitToken` 有界等待。

高频场景(设备多点位、短周期)下发布次数过高:如 1 台设备 10 个点位 × 100ms 周期 = 100 msg/s。ROADMAP P3 项"MQTT 批量发布:减少高频场景的发布次数"。

## 2. 设计目标与非目标

### 2.1 目标

- **批量**:把同一设备在一个时间窗内到达的多个点位聚合成一条 MQTT 消息,显著降低发布次数。
- **向后兼容**:默认仍为即时单条发布(`flushInterval` 为空),现有行为与测试不变;批量经配置开启。
- **复用既有韧性**:沿用 `mqttutil` 的非阻塞连接、有界等待、指数退避,不引入新发送模型。
- **消息有界**:单条消息点数上限 `batchMax`,避免超大帧。

### 2.2 非目标

- 跨设备合并(不同设备 topic 不同,天然无法合并)。
- 服务端聚合语义(如 ThingsBoard 的按 ts 分组)——本输出保持"DataPoint 数组"这一自描述格式。
- mqtt 输出的断网补传:不在本次范围(已有 backfill 基建,留待后续按需接入)。

## 3. 方案总览

```
flushInterval 为空(默认) → 即时模式:Publish 同步单条发布(现状)
flushInterval 非空        → 批量模式:
    Publish(dp) ──▶ pending[deviceID] 追加(带上限,满则丢弃告警)
                    runFlusher(每 flushInterval)──▶ 按设备各发一条消息
                    payload = DataPoint 数组,单条 > batchMax 时拆条
```

- **即时模式**:`Publish` 直接 `publishNow(dp)`(现逻辑不变)。
- **批量模式**:`Publish` 入缓冲返回 nil;flusher goroutine 每 `flushInterval` 取走缓冲,按设备分组,每设备一条消息(topic 不变,payload 为 `[]DataPoint`),逐条有界等待发布。
- **关闭**:停 flusher → flush 剩余缓冲 → 断开连接。

## 4. 详细设计

### 4.1 配置与 schema

```go
type Config struct {
    Broker, ClientID, Username, Password string
    QoS        byte
    FlushInterval string `json:"flushInterval"` // 批量窗,如 "200ms";空=即时
    BatchMax     int    `json:"batchMax"`       // 单条消息最大点数,默认 64
}
```

Schema 增加 `flushInterval`(hint: 留空=即时单条;设置如 200ms 启用批量)与 `batchMax`(hint: 单条消息最大点数,超出拆分)。

### 4.2 批量模式内部

```go
type mqttOutput struct {
    client, gatewayID, qos
    flushInterval time.Duration
    batchMax int
    // 批量模式缓冲
    mu sync.Mutex
    pending map[string][]model.DataPoint // deviceID -> 待发点
    pendingCount int                     // 上限 maxPendingPoints=8192
    done chan struct{}
    wg   sync.WaitGroup
    output.SendStats
}
```

- `Publish`(批量):锁内追加 `pending[dp.DeviceID]`;超 `maxPendingPoints` 丢弃新点并告警(与 thingsboard/tdengine 同策略)。
- `runFlusher`:每 `flushInterval` 调 `flushOnce`;`Close` 关闭 `done` 后等 flusher 退出并 flush 剩余。
- `flushOnce`:取走 `pending`,按设备逐个发布:payload 为 `json.Marshal([]model.DataPoint)`,长度超 `batchMax` 拆成多条;每条 `WaitToken(PublishTimeout)` 有界等待,成功 `SendStats.Success`、失败 `SendStats.Failure` 并继续(不阻断其他设备)。
- `RuntimeStatus.Pending` = `pendingCount`(即时模式恒 0)。

### 4.3 即时模式

`flushInterval==0` 时不起 flusher、不建缓冲,`Publish` 走原同步单条路径(含 `ErrPublishTimeout` 语义),`Close` 仅断开。

## 5. 常量与参数

| 常量 | 默认 | 说明 |
|---|---|---|
| `defaultBatchMax` | 64 | 单条消息最大点数 |
| `maxPendingPoints` | 8192 | 批量模式缓冲上限(满则丢弃新点告警) |
| `flushInterval`(配置) | 空 | 空=即时单条;非空=批量窗 |

## 6. 行为变化与兼容性

- 默认(`flushInterval` 为空):零变化,现有部署/测试兼容。
- 启用批量后:同一设备 topic 的 payload 从"单个 DataPoint 对象"变为"DataPoint 数组";该 topic 为网关自定义格式,消费方需按数组解析(文档标注)。
- `RuntimeStatus` 增加 `Pending`(批量模式缓冲深度),`/outputs/status` 向后兼容(新增字段)。

## 7. 风险与已知限制

- 批量模式引入约一个 `flushInterval` 的发送延迟(批量窗本身就是延迟换吞吐)。
- 缓冲上限满丢弃新点(与既有输出一致);断网补传留待后续接入 mqtt 输出。
- 即时模式不聚合,发布次数不降(用户需显式配置批量)。

## 8. 测试计划

- 即时模式回归:`TestPublishTimeout`、`TestNewNonBlockingUnreachable`、`TestRuntimeStatusConnected` 不变通过。
- 批量分组:同一设备多点聚合为一条,payload 为数组;不同设备分条。
- `batchMax` 拆分:超出上限拆为多条。
- 缓冲上限:超 `maxPendingPoints` 丢弃并告警。
- 关闭 flush:Close 后剩余缓冲发出。
- 需要真实 broker 收包验证批量条数(用支持收包断言的假 broker,如 mqtttest 扩展或 mosquitto)。

## 9. 实施里程碑

1. `Config`/schema 增字段。
2. `New` 按 `flushInterval` 分支即时/批量。
3. 批量缓冲 + flusher + 拆条发布 + Close flush。
4. 测试 + 文档(ROADMAP / 本设计文档实施记录 / mqtt 格式说明)。

## 10. 决策记录

| 决策点 | 选择 | 理由 |
|---|---|---|
| 默认行为 | 即时单条(`flushInterval` 空) | 向后兼容;`Publish` 同步语义与既有测试保持 |
| 批量粒度 | 按设备分组,每设备一条 | topic 按设备分层,天然边界;合并不同设备 topic 无意义 |
| 批量 payload | `[]DataPoint`(数组) | 元素与现单条格式一致,消费方解析数组即可,自描述 |
| 拆条 | `batchMax` 上限拆条 | 防超大帧;默认 64 点/条 |
| 发送模型 | 复用 `mqttutil.WaitToken` 有界等待 | 半死 broker 不卡死 flusher,与既有韧性一致 |

## 11. 实施记录

### 11.1 代码变更(2026-08-18)

- **`internal/output/mqtt/mqtt.go`**:`Config` 增加 `flushInterval`/`batchMax`,schema 同步;`New` 按 `flushInterval` 分支即时/批量;批量模式引入 pending 缓冲 + `runFlusher`(每窗口按设备聚合发布,payload 为 `[]DataPoint`,`batchMax` 拆条,有界等待),`Close` flush 剩余;`RuntimeStatus.Pending` 上报缓冲深度。
- **`internal/output/mqtttest/recording.go`**(新增):`RecordingBroker`(CONNACK/PUBACK/SUBACK/PINGRESP + 记录 PUBLISH),供批量条数/载荷断言。
- **`internal/output/mqtt/mqtt_test.go`**:4 个新测试(按设备分组聚合 / `batchMax` 拆分 / Close flush 剩余 / 即时模式单对象 payload)。
- **`README.md`**:MQTT 输出批量模式说明。

### 11.2 测试

- 新测试全过;既有 `TestPublishTimeout`/`TestNewNonBlockingUnreachable`/`TestRuntimeStatusConnected` 不变通过(默认即时模式兼容)。
- `go test -race ./internal/output/mqtt/` 无数据竞争,`go test ./...` 全绿。

### 11.3 已知限制(见 §7)

- 批量模式引入一个 `flushInterval` 的发送延迟;缓冲满丢弃新点(与其他输出一致);mqtt 输出断网补传留待后续。
