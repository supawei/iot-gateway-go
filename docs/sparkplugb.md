# Sparkplug B 输出设计文档

> **状态**:已实现(2026-08-18,单测 + 竞态检测 + 真实 mosquitto broker 端到端验证)
> **关联**: [mqtt-resilience-design.md](mqtt-resilience-design.md)、[offline-backfill-design.md](offline-backfill-design.md)
> **更新**: 2026-08-18
> **范围**: 新增 `internal/output/sparkplugb`、`internal/output/registry.go`(StoreAccessor 加 ListDevices)、`cmd/gateway/main.go`

---

## 1. 背景与目标

Sparkplug B 是工业物联网领域 MQTT 的事实标准(Eclipse Tahu),用 MQTT topic + protobuf
payload 定义边缘节点(edge node)的**出生/数据/死亡**生命周期,让 SCADA / 平台方能在
不预知设备的前提下,经订阅自动发现设备与点位(数据建模自描述)。

本网关作为**边缘节点**接入 Sparkplug B 生态,把内部"设备-点位"模型映射为 Sparkplug
的设备 + 点位 metric。P2 起 README 就声明"topic 命名空间已预留 Sparkplug B",本次落地。

### 1.1 目标

1. **spBv1.0 topic 命名空间**与消息类型:STATE / NBIRTH / DBIRTH / DDATA / DDEATH / NDEATH。
2. **SparkplugB.proto(Payload/Metric)protobuf 编码**,字节级与规范一致。
3. **生命周期**:连接/重连自动出生(NBIRTH + 各设备 DBIRTH),设备上线/离线 → DBIRTH/DDEATH,
   优雅关闭 → NDEATH + STATE OFFLINE。
4. **别名(alias)压缩**:出生声明点位 name↔alias,数据消息用别名,降低带宽。
5. 复用 `mqttutil` 连接韧性(非阻塞建连 + 指数退避 + 有界等待)、断网补传、设备上下线通知。

### 1.2 非目标(起步克制)

- **只发布不消费**:不实现 NCMD/DCMD 下行命令、不实现 host 侧 STATE 解析(写链路 P3 已有,
  待有下行需求再扩展)。
- **不用模板(Template)/PropertySet**:点位映射用最简 flat metric,不引入模板复杂度。
- **不做 dataset/数组**:点位数据类型仅标量。

---

## 2. Topic 与消息类型

```
spBv1.0/{group_id}/{message_type}/{edge_node_id}[/{device_id}]
```

| 消息类型 | topic | retained | payload | 触发 |
|---|---|---|---|---|
| `STATE` | `.../STATE/{edge}` | ✅ | `"ONLINE"` / `"OFFLINE"`(JSON 字符串) | 连接时 ONLINE;关闭时 OFFLINE |
| `NBIRTH` | `.../NBIRTH/{edge}` | ✅ | Payload,节点级 metric(deviceCount) | 每次连接/重连 |
| `DBIRTH` | `.../DBIRTH/{edge}/{device}` | ✅ | Payload,设备各点位 name/alias/datatype(+当前值) | 连接出生 / 设备上线 |
| `DDATA` | `.../DDATA/{edge}/{device}` | ❌ | Payload,单点位 alias + datatype + value | 采集到数据 |
| `DDEATH` | `.../DDEATH/{edge}/{device}` | ✅ | **空 payload** | 设备离线 |
| `NDEATH` | `.../NDEATH/{edge}` | ✅ | **空 payload** | 优雅关闭 |

- `group_id` / `edge_node_id` 可配置;`edge_node_id` 默认取网关 ID。
- 设备名经 `topicSafe` 清洗 MQTT 保留字符(`/ + # 空格` → `_`),可选 `deviceNamePrefix` 前缀。
- 每个 payload 携带**单调递增 seq**(edge node 级,STATE 除外),供 host 检测乱序/丢包。

---

## 3. Protobuf 编码(手写最小编码器)

`SparkplugB.proto` 的 `Payload` / `Metric` 结构相对固定,为避免引入 protobuf 运行时
依赖,采用**手写 wire-format 编码器**(只编码、不解析),字节级测试锁定正确性:

```go
type metric struct {
    name      string      // field 1,仅 birth 声明携带
    alias     uint64      // field 2,data 消息用别名
    timestamp uint64      // field 3,Unix 毫秒
    datatype  uint32      // field 4
    isNull    bool        // field 7
    value     interface{} // oneof value(field 8..13),按 datatype 落位
}
```

- Payload: `timestamp(1)` `seq(2)` `repeated Metric metrics(5)`。
- Metric value 按 datatype 写入对应 oneof 字段:double→8、float→9、int 系→10(int_value,
  负数补码)、uint64→11(long_value)、bool→12、string/text→13。
- **值规整**:`toFloat64/toInt64/toBool/toString` 按 datatype 统一转换(如 int 点位的
  缩放值可能是 float64,须转回声明类型,避免类型断言 panic)。

### 3.1 类型映射

| 内部 DataType | Sparkplug datatype |
|---|---|
| bool | Boolean(11) |
| int16 / int32 / int64 | Int16(2) / Int32(3) / Int64(4) |
| uint16 / uint32 / (uint64) | UInt16(6) / UInt32(7) / UInt64(8) |
| float32 / float64 | Float(9) / Double(10) |
| string | String(12) |

---

## 4. 生命周期与数据流

### 4.1 出生(birth)

MQTT 连接建立(`OnConnect`,paho 回调线程 → 起 goroutine 防阻塞)触发完整出生序列
(`birthMu` 串行化,防重连并发重复出生):

1. `STATE` `"ONLINE"`(retained);
2. `ListDevices()` → `NBIRTH`(retained,metric `deviceCount`=设备数);
3. 每台启用且有点位的设备 → `DBIRTH`(retained):逐点位分配**全局别名**(edge node 内
   唯一),声明 `name/alias/datatype`,并尽力携带当前采集值(`LatestValues`,无值则
   `is_null=true`)。

### 4.2 数据(DDATA)

`Publish(dp)` → 若节点与设备均已出生:按 `deviceID/point` 查别名,发 `DDATA`(单点位
metric,仅 alias+datatype+value,不含 name)。**未知点位**(配置变更新增但尚未 re-birth)
退化 name 直发,不丢数据。

### 4.3 上下线(DeviceNotifier)

- `DeviceOnline` → 若未出生则 `DBIRTH`;
- `DeviceOffline` → `DDEATH`(retained,空 payload)。

### 4.4 关闭

`Close()` → `NDEATH`(retained,空)+ `STATE OFFLINE`(retained)→ 断开。

### 4.5 断网补传

- 未连接/未出生时 `Publish` → 落库补传(`backfill.Save(outputID, ...)`);
- `BackfillHealthy()` = 已连接 **且** 出生完成 → Manager 重放补传队列(避免把数据灌进
  未出生的连接)。

---

## 5. 配置(SQLite + Web UI)

| 字段 | 说明 | 默认 |
|---|---|---|
| `broker` | MQTT broker 地址 | 必填 |
| `clientId` / `username` / `password` | 连接凭据 | - |
| `qos` | QoS | 1 |
| `groupId` | topic group 段 | `iot-gateway` |
| `edgeNodeId` | topic edge node 段 | 网关 ID |
| `deviceNamePrefix` | 设备 topic 段前缀 | 空 |

经 `output.Register` 注册类型 `sparkplugb`(Web UI 输出表单自动出现),配置经
`outputSensitiveFields` 自动识别 `password` 走 RSA 加密。

---

## 6. 已知限制与后续

1. **只发布不消费**:NCMD/DCMD 下行未实现(可复用 core.WritePoint 扩展)。
2. **别名/点位变更**:设备点位增删改后需设备重启(离线→在线)或输出重建才 re-birth;
   新增点位在 re-birth 前退化为 name 直发(不丢数据)。
3. **不用模板**:多设备同构点位不合并模板,带宽略高于模板方案(保留扩展点)。
4. **seq 单调递增**:跨重启从 1 重新计数(host 以 rebirth 为准,符合规范)。
5. **属性/PropertySet**:暂不携带设备属性(如固件版本),有需求再加。

---

## 7. 测试

- `proto_test.go`:字节级断言(timestamp/seq/alias/datatype/各 oneof 字段/is_null/varint
  边界),与手工构造的规范字节逐字节比对。
- `sparkplugb_test.go`:用 `mqtttest.RecordingBroker` 验证——连接出生(STATE/NBIRTH/DBIRTH
  topic+payload)、DDATA 用别名且类型正确、DeviceOnline/Offline → DBIRTH/DDEATH、
  Close → NDEATH+STATE OFFLINE、未出生时 Publish 落库补传、topic 清洗与类型映射。
- 端到端:真实 mosquitto broker + 极简 Modbus TCP 模拟器,订阅 `spBv1.0/#` 验证
  NBIRTH/DBIRTH/DDATA(topic、seq 递增、别名、datatype、float 编码、retained)全部正确。
