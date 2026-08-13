# 物模型映射层设计 (TSL Mapping)

> **状态**:草案
> **关联阶段**:P3(云平台对接) → P4(物模型映射)
> **更新**:2026-08-13

## 1. 背景与目标

### 1.1 背景

网关内部采用"设备-点位"模型(`model.Device` / `model.Point` / `model.DataPoint`),它是**采集导向、协议无关、语义贫乏**的抽象:

```
DataPoint{device:"sensor-01", point:"temperature", value:25.6, quality:"good"}
```

它只知道"有个值",不知道这是温度、单位是 ℃、范围 -40~125、只读还是可写。这对内部采集和 MQTT 自定义 JSON 直发够用。

但云平台(阿里云 IoT / 华为云 IoT / AWS IoT)不认内部点位名,它们以**物模型(TSL,Thing Specification Language)**作为设备语义的强制规范。要让网关数据在云上正确展示、联动、告警,必须把内部点位翻译成云的物模型格式。

### 1.2 目标

- 在**不改动设备-点位内核**的前提下,加一层翻译,把 `DataPoint` 转换为云平台物模型消息。
- 让一个云平台输出插件能产出符合 TSL 语义的上报消息。

### 1.3 非目标

- **不替换**内部设备-点位模型--它是采集内核,保持轻量。
- **不做万能物模型引擎**--起步只覆盖属性映射(80% 价值),服务和事件按需扩展。
- **不预先抽象多云**--先做透一个云,第二家云接入时再提炼共性。

## 2. 问题分析:两套模型的鸿沟

| 维度 | 设备-点位(内部) | 物模型 TSL(云) |
|---|---|---|
| 导向 | 采集 | 业务语义 |
| 结构 | 扁平 point 列表 | 属性 / 服务 / 事件 三要素 |
| 元数据 | name / address / type / scale | 单位、范围、读写权限、枚举 |
| 典型消费者 | 网关自身、自定义下游 | 云平台控制台、规则引擎 |

内部点位不带语义(单位/范围/读写),这是 MVP 刻意为之的反过度工程选择--语义对采集是负担。但对接云平台时,语义是云的强制要求,翻译层负责**补全并重组**这些语义。

## 3. TSL 物模型规范

物模型用一个"数字孪生描述"定义一类设备的三类语义元素:

- **属性 (properties)** -- 设备状态量,如温度、湿度、开关。带数据类型、单位、取值范围、读写权限。**对应内部点位的数值**,是映射层的主战场。
- **服务 (services)** -- 设备可被云端调用的功能,如重启、校准。带输入/输出参数。**对应下行指令**,网关需把云调用反向翻译为对设备的写操作。
- **事件 (events)** -- 设备主动上报的告警/通知,如过温告警。带事件类型(info/alert/error)和输出参数。**由规则触发**,非直接采集值。

## 4. 设计

### 4.1 架构位置

映射层坐落在 pipeline 的**处理层**位置(目前该层直通,P3/P4 起填充):

```
scheduler 采集 ──DataPoint(设备-点位)──▶
                                        │
                                [物模型映射层]   ← 补语义、转格式
                                        │
                                物模型消息(属性上报 / 事件)
                                        │
                                云平台输出插件(阿里云 / 华为云格式封装)
```

映射层职责:
1. **补语义** -- 内部点位不带单位/范围/读写,按映射配置补全。
2. **转格式** -- 把扁平 `DataPoint` 重组成云平台要求的消息结构(方法名、params 嵌套、时间戳格式)。

### 4.2 映射配置

挂在 device 上的扩展段(YAML 示例):

```yaml
device: sensor-01
cloudModel: TempHumidSensor          # 云平台物模型标识
propertyMapping:
  temperature:                        # 内部点位名
    propertyId: CurrentTemperature    # 云物模型属性标识
    unit: "℃"
    accessMode: "r"                   # 只读
  humidity:
    propertyId: CurrentHumidity
    unit: "%RH"
    accessMode: "r"
```

Go 数据结构(草案,实现时据实调整):

```go
// ModelMapping 设备到云物模型的映射配置。
type ModelMapping struct {
    DeviceID   string            `json:"deviceId"`
    CloudModel string            `json:"cloudModel"`
    Properties []PropertyMapping `json:"properties"`
}

// PropertyMapping 单个点位到云属性的映射。
type PropertyMapping struct {
    Point      string `json:"point"`      // 内部点位名
    PropertyID string `json:"propertyId"` // 云物模型属性标识
    Unit       string `json:"unit"`
    AccessMode string `json:"accessMode"` // "r" | "rw"
}
```

### 4.3 翻译规则

`DataPoint` -> 云属性上报消息的转换:

| DataPoint 字段 | 云消息字段 | 说明 |
|---|---|---|
| `point` | params 的 key | 经映射配置转为云属性标识 |
| `value` | params.key.value | 直接搬用 |
| `timestamp` | params.key.time | 格式转为云要求(如毫秒时间戳) |
| `quality` | (可选过滤) | bad 质量是否上报,按云策略 |
| (无) | method / version / id | 映射层按云规范补齐 |

### 4.4 配置存储

**起步方案**:在 `device` 表加一个 `mapping` TEXT 字段(JSON),随设备配置一起存取,无需独立表。

```sql
ALTER TABLE device ADD COLUMN mapping TEXT;
```

若后续映射关系复杂化(含服务/事件/多版本),再拆为独立 `model_mapping` 表。避免一开始过度拆分。

### 4.5 核心接口草案

```go
// Mapper 把内部 DataPoint 翻译成云平台物模型消息。
// 起步仅实现属性映射;单云实现,不预先抽象多云。
type Mapper interface {
    MapProperty(dp model.DataPoint) (CloudMessage, error)
}

// CloudMessage 是云平台上报消息的载体,具体结构由各云实现填充。
type CloudMessage struct {
    Topic   string
    Payload []byte
}
```

多云抽象等第二家云接入时再提炼,起步直接产出目标云格式。

## 5. 端到端示例:温度点位 -> 阿里云属性上报

**内部 DataPoint(映射前)**:
```json
{"deviceId":"sensor-01","point":"temperature","value":25.6,"quality":"good"}
```

**阿里云物模型属性上报(映射后)**:
```json
{
  "id": "1734567890123",
  "version": "1.0",
  "method": "thing.event.property.post",
  "params": {
    "CurrentTemperature": {"value": 25.6, "time": 1691923200000}
  }
}
```

`point:"temperature"` 经映射变为 `CurrentTemperature`,`value` 包进 `params`,并补齐 `method` 与毫秒时间戳--这就是翻译。

## 6. 与边缘计算层的边界

两者常被混谈,但职责不同:

- **物模型映射层** -- 做**格式/语义翻译**(点位 -> 属性),确定性转换。
- **边缘计算层** -- 做**规则判断**(温度>80 且持续 10s),负责生成事件。

完整链路:`采集 -> (规则引擎判定生成事件) -> 映射层翻译事件消息 -> 云输出`。事件上报的"触发逻辑"归边缘计算层,映射层只负责把已触发的事件翻译成云的事件格式。P3 边缘计算与 P4 物模型映射是协作关系。

## 7. 实现范围(分阶段)

### P3:属性映射 + 单云(起步)
- [ ] 映射配置结构 + 存储(device 表 mapping 字段)
- [ ] `Mapper` 接口 + 单云(如阿里云)属性映射实现
- [ ] 接入 pipeline 处理层,云平台输出插件消费映射后消息
- [ ] REST API 扩展:配置/查询设备映射

### P4:服务/事件 + 多云
- [ ] 服务映射:云下行指令反向翻译为设备写操作
- [ ] 事件映射:与边缘计算层协作,翻译事件消息
- [ ] 第二家云接入时,提炼多云共性抽象
- [ ] (可选)从云平台 TSL JSON 导入,自动生成映射配置

## 8. 设计取舍

| 取舍 | 选择 | 理由 |
|---|---|---|
| 起步范围 | 仅属性映射 | 覆盖 80% 价值,服务/事件复杂度高且非首需 |
| 多云抽象 | 不预先做 | 第二家云出现前抽象是猜测,违背反过度工程 |
| 配置存储 | device 表加字段 | 简单;复杂化后再拆独立表 |
| 内部模型 | 不动设备-点位 | 采集内核保持轻量,语义外挂在映射层 |
| 事件触发 | 归边缘计算层 | 映射层只翻译,不判定,职责单一 |

## 9. 开放问题

- **服务下行**:云调用服务的反向链路(云 -> 网关 -> 设备写)如何接入现有 `Driver` 接口?当前 `Driver.Conn` 只有 `Read`,需评估是否加 `Write`。
- **质量位策略**:`bad`/`uncertain` 质量的数据点是否上报云平台?不同云策略不一,需按云配置。
- **TSL 导入**:能否从云平台导出的 TSL JSON 自动生成映射配置,减少手工配点?
- **映射热加载**:映射配置变更是否复用现有 `store.OnChange` 热加载机制?
