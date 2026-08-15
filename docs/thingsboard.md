# ThingsBoard 平台对接设计文档

## 1. 目标

让 iot-gateway-go 作为**边缘网关**,把采集到的设备数据上送到 ThingsBoard 物联平台,并最终支持平台侧对设备的反向控制(下行)。本文档给出对接方案、架构与分阶段实施计划,先实现"数据上送",下行留作后续。

## 2. 对接方式选型

ThingsBoard 提供三种主流接入方式:

| 方式 | 说明 | 适用 | 结论 |
|---|---|---|---|
| HTTP REST | 逐条 POST 遥测/属性 | 低并发、无长连接场景 | ❌ 开销大、无实时下行 |
| MQTT 标准设备 | 每个设备一个 MQTT 连接 + 独立 token | 设备各自直连平台 | ❌ 网关场景设备多、连接多 |
| **MQTT Gateway** | 网关作为一个"网关设备",用一个连接代表 N 个子设备 | 边缘网关集中接入 | ✅ **采用** |

**选型结论:MQTT Gateway 模式**。理由:

- 单连接复用:一个 MQTT 连接承载所有子设备,契合网关"集中接入多设备"的定位。
- 官方支持:ThingsBoard 有完整的 Gateway MQTT API(connect/disconnect/telemetry/attributes/rpc)。
- 与现有架构契合:我们的 `Output` 就是"网关级单例,一个实例消费所有 DataPoint",与 Gateway 模式的"单连接多设备"天然对应。

## 3. 总体架构

ThingsBoard 对接作为一个**北向输出插件** `internal/output/thingsboard`,与现有 `mqtt` 输出并列,实现 `output.Output` 接口,插入 pipeline 的 outputs 列表:

```
南向驱动(modbus/opcua/...)
   │  DataPoint
   ▼
scheduler → pipeline ──┬──▶ output/mqtt          (现有:自定义 JSON)
                       └──▶ output/thingsboard   (新增:ThingsBoard MQTT Gateway)
                                 │
                                 ▼
                          ThingsBoard 平台
```

- 复用现有 `Output` 接口与 pipeline 的异步分片发布(上一轮已做背压隔离)。
- 协议差异封死在 `thingsboard` 包内,Core/其他输出零改动。
- 多输出并存:可同时上送 MQTT 与 ThingsBoard(在 `buildOutputs` 中构造两个 output 即可)。

## 4. 模型映射

| iot-gateway-go | ThingsBoard | 说明 |
|---|---|---|
| 网关实例 | Gateway 设备 | 一个 gateway token |
| `Device.ID` | 子设备名称(device name) | 网关用 DeviceID 作为 TB 设备名 |
| `Point.Name` | telemetry key | 遥测字段名 |
| `DataPoint.Value` | telemetry value | 遥测值 |
| `DataPoint.Timestamp` | `ts`(毫秒) | 采样时间 |
| `DataPoint.Quality` | 设备属性 `status`(或独立 telemetry) | 见 §7 |

**设备命名**:MVP 用 `Device.ID` 直接作为 ThingsBoard 设备名,可选加统一前缀(`deviceNamePrefix`)。若后续需要"ID 显示名"分离,再扩展映射配置(见 §13)。

**子设备存在性**:ThingsBoard 侧需预创建子设备,或开启 Gateway 的 auto-provisioning(自动建档);`v1/gateway/connect` 在运行时"登记"设备。

## 5. MQTT 协议(ThingsBoard Gateway API)

### 5.1 连接

- MQTT broker:`thingsboard.broker`。
- 认证:以**网关设备的 Access Token 作为 MQTT 用户名**,密码为空(或按平台配置 basic auth/TLS)。
- QoS:遥测建议 QoS 1。

### 5.2 上送消息(网关 → 平台)

| 用途 | Topic | Payload |
|---|---|---|
| 登记子设备 | `v1/gateway/connect` | `{"device":"<deviceName>"}` |
| 注销子设备 | `v1/gateway/disconnect` | `{"device":"<deviceName>"}` |
| 遥测 | `v1/gateway/telemetry` | `{"<deviceName>":[{"ts":<ms>,"values":{"<key>":<val>}}]}` |
| 客户端属性 | `v1/gateway/attributes` | `{"<deviceName>":{"<attrKey>":<val>}}` |

### 5.3 上报格式与批量(遥测 vs 属性)

ThingsBoard 把"数据"分两类,格式不同:

| 类型 | 有无 `ts` | 格式 | 我们的来源 |
|---|---|---|---|
| **遥测 telemetry** | 有(毫秒) | `设备名 → [ {ts, values} ]` 数组 | `DataPoint.Value` |
| **属性 attribute** | 无 | `设备名 → {key: value}` 对象 | `DataPoint.Quality` 等状态 |

**遥测(批量,一个设备同一时刻的多点位合成一帧)**:

```jsonc
// topic: v1/gateway/telemetry
{
  "设备A": [
    { "ts": 1700000000000, "values": { "温度": 25.5, "湿度": 60, "液位": 1.2 } }
  ]
}
```

- 同一 `ts` 下的所有点位合并进一个 `values`,一次上报。
- 数组支持"同设备多时刻、多设备同帧",为批量/补传留有余地。

**属性(天然批量,一个设备的所有属性合成一条)**:

```jsonc
// topic: v1/gateway/attributes
{
  "设备A": { "quality": "good", "online": true }
}
```

### 5.4 扁平 vs 聚合的取舍

采集周期是**设备级**的,不存在跨设备的"同一采集周期"。真正的天然批是:**某设备一次 `conn.Read`(轮询)返回的全部点位,时间戳相同**(订阅=一次通知的多个监控项,监听=一帧的多个点位)。

但当前实现把天然批在源头摊平了——`collectOnce`(轮询)/`dispatch`(订阅)/`deliver`(监听)都逐条 `emit`/`onData`,批边界在进入 output 前已丢失。因此两种上报策略:

1. **扁平(朴素)**:一条 DataPoint → 一帧 telemetry(数组里 1 个元素)。最简单,先跑通链路。
2. **聚合**:
   - **A. 源头保批(精确)**:让 `collectOnce` 等把一次 Read 的整批 DataPoint 作为一个单元下发(改 `chan DataPoint` 为批次)。精确还原"同设备同时刻一批",但牵动 pipeline 与所有输出。
   - **B. output 层近似聚合(务实)**:接受摊平,在 thingsboard 插件按 `(deviceID, timestamp)` 聚合。轮询场景下时间戳即批标识、几乎等价于精确批;push 场景下是近似。

**结论**:
- 遥测:**按"设备 + 时间戳"聚合**。P1 先扁平跑通;P1.5 先做 B(插件内按设备+时间戳聚合,零牵动 Core),如对精确性有要求再评估 A。
- 属性:**天然批量**,一次把该设备所有属性合成一个对象上报。
- B 的聚合逻辑**封在 thingsboard 插件内部**,不改 pipeline 的 `chan DataPoint`,Core 与其他输出不受影响。

### 5.5 下行消息(平台 → 网关)

| 用途 | Topic | 说明 | 状态 |
|---|---|---|---|
| 共享属性更新 | 订阅 `v1/gateway/attributes` | `{"device":"<name>","data":{"<key>":<val>}}` → 每个 key 作为点位写回设备 | ✅ 已实现 |
| RPC 命令 | 订阅 `v1/gateway/rpc` | `{"device":"<name>","data":{"id":1,"method":"write","params":{"point":"<p>","value":<v>}}}` → 写点位并应答 | ✅ 已实现 |

下行经 `WriteFunc` 回调(由 main 注入 `core.WritePoint`)→ 驱动 `Writer` 写回设备。与上行客户端属性同 topic 但 payload 不同(下行带 `device`/`data` 包装),据此区分;RPC 请求带 `data.method`、应答不带,据此区分。

## 6. 输出插件设计

```go
package thingsboard

type Config struct {
    Broker           string `yaml:"broker"`            // 如 tcp://tb.example.com:1883
    AccessToken      string `yaml:"accessToken"`       // 网关设备 Access Token
    ClientID         string `yaml:"clientId"`          // MQTT client id
    Username         string `yaml:"username,omitempty"` // 默认取 AccessToken
    Password         string `yaml:"password,omitempty"`
    QoS              byte   `yaml:"qos"`               // 默认 1
    DeviceNamePrefix string `yaml:"deviceNamePrefix"`  // 设备名前缀,默认空
}

type thingsboardOutput struct {
    client   pahomqtt.Client
    prefix   string
    qos      byte
    mu       sync.Mutex
    devices  map[string]bool // 已发送 connect 的子设备名集合
}

func New(cfg Config) (output.Output, error)
func (o *thingsboardOutput) Publish(dp model.DataPoint) error
func (o *thingsboardOutput) Close() error
```

**Publish 流程**(单 DataPoint → 一帧):

1. `deviceName := prefix + dp.DeviceID`。
2. 若 `deviceName` 不在 `devices` 集合 → 先发 `v1/gateway/connect` 并记入集合(惰性登记)。
3. 构造 `{"<deviceName>":{"ts":ms,"values":{dp.Point: dp.Value}}}` 发到 `v1/gateway/telemetry`。
4. 按 §7 处理 Quality。

**要点**:
- 依赖 paho 的 `SetAutoReconnect(true)` 自动重连,与现有 mqtt 输出一致。
- 惰性 connect:首次出现某设备时登记,避免在构造时需知道设备列表(输出层只有 DataPoint,无 store 访问)。
- 可选:提供 `NotifyConnected(deviceID)` / `NotifyDisconnected(deviceID)` 作为显式生命周期通知的扩展点(见 §9)。

## 7. 数据质量(Quality)处理

ThingsBoard 无原生"数据质量"概念。建议:

- **遥测**:始终上报 `values[dp.Point] = dp.Value`(即使 bad 也带上最后值)。
- **状态属性**:当 `Quality != good` 时,额外发一条客户端属性 `{"<deviceName>":{"quality":"bad|uncertain"}}`;`good` 时发 `quality:"good"`。

这样平台侧既能看到数据,又能按 `quality` 属性做告警/过滤。是否上报 quality 属性做成开关 `reportQuality`(默认开),避免高频属性写。

## 8. 配置

`config.yaml` 增加:

```yaml
thingsboard:
  broker: "tcp://tb.example.com:1883"
  accessToken: "gateway-access-token"
  clientId: "iot-gateway"
  qos: 1
  deviceNamePrefix: "factory1/"   # 可选
  reportQuality: true             # 可选,默认 true
```

`main.go` 的 `buildOutputs` 中:若配置了 `thingsboard.accessToken` 非空,则追加构造 `thingsboard.New(...)` 到 outputs 列表(与 mqtt 并列);未配置则跳过,保证向后兼容。

## 9. 设备生命周期与下行通道

1. **生命周期事件(已实现)**:`Output` 增加了可选能力接口 `DeviceNotifier`(与驱动侧的 `Writer/Subscriber/Listener` 同套路):
   ```go
   type DeviceNotifier interface {
       DeviceOnline(deviceID string)
       DeviceOffline(deviceID string)
   }
   ```
   scheduler 在设备状态发生"上线/离线"转变时,对实现了该接口的输出调用对应方法;ThingsBoard 据此发 `v1/gateway/connect` / `v1/gateway/disconnect`(在 flusher 中与实际遥测一起 flush,顺序为 disconnect→connect→telemetry)。

2. **下行反向通道(已实现)**:平台下发 → 网关 → 驱动 `Write`。采用**回调注入**:thingsboard 输出订阅 `v1/gateway/attributes`(共享属性)与 `v1/gateway/rpc`(RPC 命令),解析后经 `WriteFunc` 回调(由 main 注入 `core.WritePoint`)写回设备;RPC 写完后应答 `{"device","id","data":{"ok","error"}}`。`core.WritePoint` 同时被 REST 写接口复用,消除重复。

## 10. 分阶段实施

| 阶段 | 内容 | 状态 |
|---|---|---|
| **P1(上送)** | thingsboard 输出插件:连接 + 惰性 connect + 遥测(扁平,一条一帧)+ quality 属性 | ✅ 已实现 |
| P1.5 | 遥测微批聚合(按设备 + 定时 flush)+ 显式生命周期(DeviceNotifier) | ✅ 已实现 |
| **P2(下行)** | 共享属性 → 驱动 Write + RPC 命令 → 驱动 Write | ✅ 已实现 |
| P3(优化) | 断网本地补传、deviceName 映射 | 待实现 |

## 11. 验收标准

- [ ] 配置 accessToken 后,网关启动即连上 ThingsBoard broker。
- [ ] 设备采集到数据后,ThingsBoard 对应子设备能收到遥测(值与 ts 正确)。
- [ ] 设备名与 DeviceID 一致(含前缀)。
- [ ] Quality 属性按配置上报。
- [ ] 断开 broker 后自动重连,重连后数据恢复上送。
- [ ] 未配置 thingsboard 时,网关行为与之前完全一致(向后兼容)。

## 12. 风险与开放问题

1. **设备命名**:DeviceID 作为 TB 设备名的可读性一般;如需友好名,后续可加映射(Config 级 map 或扩展 DataPoint 携带 Name)。
2. **auto-provisioning**:子设备是否自动建档取决于 TB 侧配置;需在验收时确认。
3. **下行反向通道**:P2 需要新增 commandBus 与 scheduler/driver 的联动,是最大的一块新增架构,本阶段不实现。
4. **时间戳**:TB 期望毫秒整型;`DataPoint.Timestamp` 为 `time.Time`,转换时注意时区/精度。
5. **遥测批量**:高频场景逐条上报会有网络开销,P3 再聚合。

## 13. 命名映射备选(留待需要)

若需"DeviceID ≠ TB 设备名",候选方案:
- (a) `thingsboard.deviceNames`:`map[deviceID]tbName` 静态映射(简单,配置维护成本高);
- (b) 扩展 `DataPoint` 增加 `DeviceName` 字段(驱动在产出时填入,改动面大但最通用)。

MVP 先用 `Device.ID` + 前缀,不引入上述复杂度。
