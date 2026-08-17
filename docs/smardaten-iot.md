# smardaten-iot 私有 IoT 平台对接设计文档

> **状态**: 已实现（阶段 1 完成 + 自动同步 + 稳定性修复）
> **关联**: 平台交互契约文档 `iot_platform_interaction.md`
> **更新**: 2026-08-18

## 1. 背景与目标

### 1.1 背景

现有 Go 网关已支持三种北向输出（MQTT 自定义 / ThingsBoard / TDengine），均为单向数据上报。smardaten-iot 是一个私有 IoT 平台，其交互模式为**全双工**：平台不仅接收网关上报的数据，还向网关下发配置、服务调用、诊断请求。网关需要同时扮演"数据上报者"和"指令执行者"两个角色。

### 1.2 目标

在现有北向输出插件体系内，新增 `smardaten-iot` 输出类型，实现网关与 smardaten-iot 平台的完整对接，保持与 `iot_platform_interaction.md` 所描述的契约完全一致。

### 1.3 非目标

- 不改变现有 `Output` / `DeviceNotifier` 接口签名
- 不修改其他输出插件（mqtt / thingsboard / tdengine）
- 不改变南向采集（driver / scheduler / pipeline）逻辑
- Inner MQTT 总线用 Go channel 内部替代，不对外暴露

---

## 2. 架构设计

### 2.1 在现有架构中的位置

```
                          ┌──────────────────────────────────────┐
                          │         smardaten-iot 平台（云）        │
                          │    MQTT Broker + HTTP 文件服务          │
                          └──────▲───────────────────▲────────────┘
                                 │ MQTT               │ HTTP GET
                                 │                    │ (appId 鉴权)
              ┌──────────────────┼────────────────────┼──────────┐
              │   Go 网关 (单进程)                       │          │
              │                                          │          │
              │  ┌───────────────────────────────────────┴──────┐  │
              │  │          smardaten-iot 输出插件               │  │
              │  │  ┌──────────┐ ┌──────────┐ ┌──────────────┐ │  │
              │  │  │ 属性上报  │ │ 服务调用  │ │ 配置/HTTP下載 │ │  │
              │  │  │ (通道3/4) │ │ (通道5/6) │ │ (通道1/2/8/9) │ │  │
              │  │  └────┬─────┘ └────┬─────┘ └──────┬───────┘ │  │
              │  │       │            │               │         │  │
              │  │       └────────────┼───────────────┘         │  │
              │  │                    │                         │  │
              │  │         单一 MQTT 连接 (outer)                │  │
              │  └────────────────────┼─────────────────────────┘  │
              │                      │                             │
              │  ┌───────────────────┴──────────────────────────┐  │
              │  │  Output.Manager (扇出)                        │  │
              │  │  ┌──────────┐  ┌──────────┐  ┌────────────┐  │  │
              │  │  │  smardaten│  │  mqtt    │  │  tdengine   │  │  │
              │  │  └──────────┘  └──────────┘  └────────────┘  │  │
              │  └───────────────────▲──────────────────────────┘  │
              │                      │                             │
              │  ┌───────────────────┴──────────────────────────┐  │
              │  │  core.Pipeline                                │  │
              │  └───────────────────▲──────────────────────────┘  │
              │                      │                             │
              │  ┌───────────────────┴──────────────────────────┐  │
              │  │  core.Scheduler + driver (采集)               │  │
              │  └──────────────────────────────────────────────┘  │
              └────────────────────────────────────────────────────┘
```

**关键决策**：smardaten-iot 作为一个标准 `Output` 插件实现，与 mqtt/thingsboard/tdengine 平级。它从 `Manager.Publish()` 接收 `DataPoint`，内部完成平台格式转换后发布到平台 MQTT。同时，它独立维护到平台的 MQTT 连接，订阅下行 topic（配置、服务调用、诊断），下行指令通过 `BuildContext.Write` 回调写入设备。

### 2.2 与 C 原版进程映射

| C 原版进程 | 职责 | Go 实现 |
|---|---|---|
| `gw_sys_manage` | 配置下发、协议驱动管理 | smardaten-iot 插件（配置订阅 + HTTP 下载） |
| `gw_dev_manage` | 属性上报、设备状态、服务调用、诊断 | smardaten-iot 插件（属性/状态/服务/诊断） |
| `gw_tcp_server` | DTU 状态上报 | **暂不实现**（Go 网关无 DTU 透传需求） |
| Inner MQTT 总线 | 进程间通信 | Go channel（内部实现，不暴露） |

---

## 3. 配置设计

### 3.1 输出插件配置（SQLite `output` 表）

smardaten-iot 与其他输出插件一样，配置存储在 SQLite 的 `output` 表中，类型为 `"smardaten-iot"`。配置 JSON 结构如下：

```json
{
  "broker":      "tcp://平台MQTT地址:1883",
  "port":        1883,
  "protoVer":    311,
  "username":    "admin",
  "password":    "admin",
  "clientId":    "gw-dev-manage",
  "iotAppId":    "531b9a9d-95da-4263-9acf-5b6b99d91197",
  "iotRsaKeyPath": "config/test_pub.key",
  "iotConfigPath": "config/application.json",
  "pubMode":      0,
  "maxPubTime":   60,
  "flushInterval": "200ms"
}
```

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `broker` | string | 是 | — | 平台 MQTT broker 地址，如 `tcp://10.0.0.1:1883` |
| `port` | int | 否 | 1883 | MQTT 端口（broker 已含端口时可选） |
| `protoVer` | int | 否 | 311 | MQTT 协议版本：311=3.1.1, 5=5.0 |
| `username` | string | 否 | — | MQTT 用户名 |
| `password` | string | 否 | — | MQTT 密码 |
| `clientId` | string | 否 | — | MQTT Client ID |
| `iotAppId` | string | 是 | — | 应用 ID，RSA 加密后作 HTTP `appId` header |
| `iotRsaKeyPath` | string | 是 | — | RSA 公钥文件路径（PEM SPKI 格式） |
| `iotConfigPath` | string | 否 | `config/application.json` | 平台下发的 application.json 落盘路径 |
| `pubMode` | int | 否 | 0 | 上报模式：0=全属性刷新后上报, 1=变化死区上报 |
| `maxPubTime` | int | 否 | 60 | 变化上报模式的最大周期间隔（秒） |
| `flushInterval` | string | 否 | `"200ms"` | 数据聚合 flush 间隔 |

### 3.2 gatewayID 来源

`gatewayID` 不从插件配置读取，而是从 `BuildContext.GatewayID`（即 SQLite `settings` 表的 `gateway.id`）注入。这与现有 mqtt 输出一致。

### 3.3 application.json（平台下发）

此文件由平台通过 MQTT 触发 HTTP 下载后落盘，结构见 `iot_platform_interaction.md` 第 3 节。插件启动时尝试加载本地已有文件，后续收到平台配置更新时重新下载并热加载。

---

## 4. 数据流设计

### 4.1 上行数据流（属性上报）

```
Scheduler 采集
    │
    ▼
DataPoint{deviceID, point, value, timestamp, quality}
    │
    ▼
Manager.Publish(dp)  ──扇出──▶  smardaten-iot.Publish(dp)
                                    │
                                    ▼
                              缓冲到 pending map[deviceID][]DataPoint
                                    │
                                    ▼ (flushInterval 定时)
                              查 application.json 映射:
                                deviceID → eventTopic (events[].method)
                                pointID  → propertyIdentifier
                                    │
                                    ▼
                              按平台格式组包:
                                {version, params:{identifier:value, deviceId, reportTime}}
                                    │
                                    ▼
                              MQTT Publish(eventTopic, Qos1)
                                    │
                                    ▼ (紧随属性上报)
                              设备状态上报:
                                {deviceId, status:1, reportTime}
                              MQTT Publish(/sys/thing/event/deviceStatus/post, Qos1)
```

**行为契约**：
- 值精度：`round(value * 100) / 100`（保留 2 位小数）
- 时间戳：毫秒级（13 位）
- `pubMode=0`（及时上报）：设备所有属性都被刷新过一次后才发一次
- `pubMode=1`（变化上报）：`|Δ| > 0.01` 立即上报，或距上次上报超过 `maxPubTime` 秒周期性上报
- STRING 类型（`dataType=6`）不上报
- `version` 恒为 `"1.0"`

### 4.2 下行数据流（服务调用）

```
平台 MQTT
    │
    ▼ Publish on {services[].method}
smardaten-iot MQTT handler
    │
    ▼ 解析: {identifier, serviceType, deviceId, controllerId, cmdId, pointId?, value?}
    │
    ├── serviceType="get"
    │       │
    │       ▼ 查 application.json 映射
    │       读取最新采集值
    │       │
    │       ▼ 组响应包: {cmdId, statusCode:0, version, reportTime, params:{...}}
    │       MQTT Publish({services[].responseMethod}, Qos1)
    │
    └── serviceType="set"
            │
            ▼ 调用 BuildContext.Write(deviceId, pointId, value)
            │  (经 core.WritePoint → driver.Write)
            │
            ▼ 组响应包: {identifier, serviceType, deviceId, controllerId, cmdId, statusCode, reportTime}
            MQTT Publish({services[].responseMethod}, Qos1)
```

### 4.3 配置下发流

```
平台 MQTT
    │
    ▼ Publish on /sys/{gatewayID}/thing/config/set
smardaten-iot MQTT handler
    │
    ▼ 解析: {identifier:"configUpdate", url:"https://..."}
    │
    ▼ HTTP GET url (带 RSA 加密的 appId header)
    │
    ▼ 校验 JSON 合法性 → 落盘 iotConfigPath
    │
    ▼ 重新解析 application.json → 更新 topic 映射
    │
    ▼ 响应: {cmd:"config", status:"ok"}
    MQTT Publish(/sys/{gatewayID}/thing/config/response, Qos1)
```

### 4.4 设备诊断流

```
平台 MQTT
    │
    ▼ Publish on /sys/{gatewayID}/thing/event/diagnose/set
smardaten-iot MQTT handler
    │
    ▼ 解析: {deviceId, controllerId, diagnose_report_id, asset_id, executeTime}
    │
    ▼ 执行诊断:
    │   DC1001: 网关服务 → status=1
    │   DC1002: DTU   → status=0 (Go 网关无 DTU, 暂不实现)
    │   DC1003: 设备   → 尝试连接设备, status=1/0
    │
    ▼ 组响应包 → MQTT Publish(/sys/{gatewayID}/thing/event/diagnose/set_reply, Qos1)
```

---

## 5. 接口设计

### 5.1 实现接口

```go
// 实现 output.Output
func (o *platformOutput) Publish(dp model.DataPoint) error
func (o *platformOutput) Close() error

// 实现 output.DeviceNotifier (可选)
func (o *platformOutput) DeviceOnline(deviceID string)
func (o *platformOutput) DeviceOffline(deviceID string)
```

### 5.2 内部组件

```
platformOutput
├── MQTT 连接管理
│   ├── 连接/重连 (keepalive=60s, 指数退避 2s→30s)
│   ├── 订阅 topic 管理 (含动态 topic 重订阅)
│   └── 发布 (QoS 按契约)
├── 数据缓冲与聚合
│   ├── pending map[deviceID][]DataPoint
│   ├── flush goroutine (按 flushInterval)
│   └── 属性上报格式转换
├── application.json 管理
│   ├── 本地加载 + 热重载
│   ├── topic 映射表 (device→eventTopic, point→identifier)
│   └── 服务 topic 注册/注销
├── HTTP 下载
│   ├── RSA PKCS1v15 加密 iotAppId
│   ├── HTTPS 关闭证书校验
│   └── 重定向跟随 (≤10 跳)
└── 下行处理
    ├── 服务调用 (get/set)
    ├── 配置下发
    └── 设备诊断
```

---

## 6. Topic 契约总表

### 6.1 订阅（平台 → 网关）

| Topic | QoS | 用途 |
|---|---|---|
| `/sys/{gatewayID}/thing/config/set` | 2 | 配置下发 |
| `/sys/{gatewayID}/thing/protocol/update` | 2 | 协议驱动管理（暂不实现） |
| `/sys/{gatewayID}/thing/event/diagnose/set` | 1 | 设备诊断 |
| `{services[].method}`（动态） | 2 | 服务调用 |

### 6.2 发布（网关 → 平台）

| Topic | QoS | 用途 |
|---|---|---|
| `/sys/{gatewayID}/thing/config/response` | 1 | 配置响应 |
| `/sys/{gatewayID}/thing/protocol/response` | 1 | 协议驱动响应（暂不实现） |
| `{events[].method}`（动态） | 1 | 属性上报 |
| `/sys/thing/event/deviceStatus/post` | 1 | 设备状态上报 |
| `{services[].responseMethod}`（动态） | 1 | 服务调用响应 |
| `/sys/{gatewayID}/thing/event/diagnose/set_reply` | 1 | 诊断响应 |

---

## 7. 文件结构

```
internal/output/smardaten/
├── smardaten.go        # 主文件：Output 实现、MQTT 连接、数据缓冲、下行处理
├── application.go      # application.json 解析与 topic 映射
├── http.go             # RSA 鉴权 HTTP 下载
├── sync.go             # application.json → 网关配置自动同步（类型感知转换）
└── smardaten_test.go   # 配置解析回归测试

cmd/gateway/main.go     # 新增 import: _ "iot-gateway-go/internal/output/smardaten"
internal/output/registry.go  # BuildContext 增加 StoreAccessor（供插件自动同步）
```

---

## 8. 实现阶段

### 阶段 1：核心对接（✅ 已完成）

| 序号 | 内容 | 通道 | 状态 |
|---|---|---|---|
| 1 | 插件骨架：Config、init 注册、New 构造 | — | ✅ |
| 2 | MQTT 连接管理：连接、重连、订阅/发布 | — | ✅ |
| 3 | application.json 解析与 topic 映射 | — | ✅ |
| 4 | 属性上报（通道 3）+ 设备状态上报（通道 4） | 3, 4 | ✅ |
| 5 | 配置下发（通道 1）+ HTTP 下载（通道 8） | 1, 8 | ✅ |
| 6 | 服务调用（通道 5） | 5 | ✅ |
| 7 | 设备诊断（通道 6） | 6 | ✅ |
| 8 | 变化上报模式（pubMode=1） | 3 | ✅ |
| 9 | **自动同步**：application.json → 网关 Connection/Device/Point | — | ✅ |

### 阶段 2：扩展（待后续）

| 序号 | 内容 | 通道 |
|---|---|---|
| 10 | 协议驱动管理（通道 2）+ 驱动下载（通道 9） | 2, 9 |
| 11 | DTU 状态上报（通道 7） | 7 |

---

## 8.5 自动同步设计

### 背景

平台是模型驱动架构：`application.json` 已包含控制器连接参数、设备、点位等全部配置信息。若网关侧再手工配置一遍 Connection/Device/Point，属于重复劳动。自动同步让**平台成为唯一配置源**，网关收到配置后自动创建/更新采集配置。

### 流程

```
application.json（本地启动加载 / 平台 configUpdate 下发）
    │
    ├─ controllers[].specs.configuration
    │       ▼ 按 type 走类型感知转换器
    │       ├─ type=1 (modbus-rtu): com→serialPort, parity 0→"N", timeOut→"10s"
    │       ├─ type=2/23 (modbus-tcp/rtu-over-tcp): ip+port→address
    │       ├─ type=3 (opcua): ip+port→"opc.tcp://..."
    │       └─ type=21/24 (dlt645): ip+port→address
    │       ▼ Connection{ID, Name, Driver, Config} → store.SaveConnection()
    │
    └─ devices[].properties + controllers[].sensorList
            ▼ 按 deviceId 匹配控制器 → 构建 Device
            ▼ slaveId 从 controller config 移到 Device.Params
            ▼ functionCode → pollBlocks（合并连续地址）
            ▼ Device{ID, Name, ConnectionID, Params, Points} → store.SaveDevice()
```

### 关键转换

| 平台字段 | 网关字段 | 转换逻辑 |
|---|---|---|
| `controllers[].type` | `Connection.Driver` | 查表：1/2/23→"modbus", 3→"opcua", 21/24→"modbus" |
| `specs.configuration.com` | `Connection.Config.serialPort` | 编号→设备路径（1→/dev/ttyS2） |
| `specs.configuration.parity` | `Connection.Config.parity` | 0→"N", 1→"O", 2→"E" |
| `specs.configuration.timeOut` | `Connection.Config.timeout` | 毫秒→duration 字符串 |
| `specs.configuration.ip+port` | `Connection.Config.address/endpoint` | 拼接 |
| `specs.configuration.slaveId` | `Device.Params.slaveId` | 跨层移动（一控制器多从站） |
| `specs.configuration.functionCode` | `Device.Params.pollBlocks[].function` | 1→coil, 2→discrete, 3→holding, 4→input |
| `sensorList[].pointId` | `Point.Name` | 直接（与平台上报 key 一致） |
| `sensorList[].itemName` | `Point.Address` | 直接 |
| `sensorList[].dataType` | `Point.DataType` | 0→bool, 1→int16, 2→int32, 3→int64, 4→float32, 5→float64, 6→string |
| `specs.period` | `Device.IntervalMs` | 秒→毫秒 |

### 语义

- 以 `controllerId`/`deviceId` 为 key 做 upsert，不覆盖网关本地独有的配置
- 平台下发的配置更新（通道 1）触发重新同步，scheduler 经 `store.OnChange` 自动热加载
- 用户在 Web UI 手动修改后，下次平台下发会覆盖（平台为权威源）

---

## 9. 设计取舍

| 取舍 | 选择 | 理由 |
|---|---|---|
| 插件形式 | 单一 `Output` 插件 | 复用现有 Manager 扇出/热重载机制，无需新增架构概念 |
| 进程模型 | 合并到单进程 goroutine | C 原版 5 进程为历史原因，Go 用 goroutine 隔离即可 |
| Inner MQTT | 不实现 | C 原版用于进程间通信，Go 单进程用 channel/go 调用替代 |
| application.json 源 | 本地文件 + 平台下发 | 启动时加载本地，运行时平台下发覆盖 |
| 协议驱动管理 | 暂不实现 | 属于平台运维功能，非核心数据流，按需补充 |
| DTU 状态上报 | 暂不实现 | Go 网关暂无 DTU 透传场景 |
| 下行写 | 复用 BuildContext.Write | 与 thingsboard 下行共享同一 write 回调，经 core.WritePoint → driver |
| pubMode 实现 | 两种模式均已实现 | 及时上报 + 变化死区上报（|Δ|>0.01，maxPubTime 周期兜底） |

---

## 10. 开放问题

1. ~~**application.json 的 deviceId 与 Go 网关 Device.ID 的映射关系**~~ ✅ **已解决**：自动同步功能直接以 `devices[].deviceId` 作为 `Device.ID`，无需用户手动对齐。

2. ~~**pointId 与 Point.Name 的映射**~~ ✅ **已解决**：自动同步以 `sensorList[].pointId` 作为 `Point.Name`，与平台上报 key 天然一致。

3. **服务调用的 get 语义**：平台 `serviceType=get` 要求返回设备当前属性值，但 Go 网关的采集是定时拉取，无实时缓存。需要 `values.Registry` 提供最新值查询能力。

4. **设备诊断 DC1003（终端设备连通性）**：需要 driver 层提供"探测设备是否可达"的能力，当前 `Driver` 接口无此方法，需评估是否扩展。

5. **多平台连接**：若同一网关需同时对接多个 smardaten-iot 平台实例，当前每个插件实例维护一个 MQTT 连接的设计可以支持（创建多个 output 配置即可），但 application.json 的隔离需要确认。

6. **自动同步的覆盖语义**：平台下发配置时会 upsert 覆盖同名 Connection/Device。若网关本地设备与平台设备 ID 冲突，会被平台配置覆盖（预期行为，平台为权威源）。若需保留本地设备，需引入"本地优先"标志或命名空间隔离。

---

## 11. 实施进度

### 11.1 通道覆盖情况

对照 `iot_platform_interaction.md` 的 9 条通道，当前实现状态：

| 通道 | 内容 | 订阅 topic | 发布 topic | 状态 |
|---|---|---|---|---|
| 通道 1 | 配置下发 | `/sys/{gw}/thing/config/set` (QoS2) | `/sys/{gw}/thing/config/response` (QoS1) | ✅ 已实现 |
| 通道 2 | 协议驱动管理 | `/sys/{gw}/thing/protocol/update` (QoS2) | `/sys/{gw}/thing/protocol/response` (QoS1) | ❌ 未实现 |
| 通道 3 | 属性上报 | — | `{events[].method}` 动态 (QoS1) | ✅ 已实现 |
| 通道 4 | 设备状态上报 | — | `/sys/thing/event/deviceStatus/post` (QoS1) | ✅ 已实现 |
| 通道 5 | 服务调用 | `{services[].method}` 动态 (QoS2) | `{services[].responseMethod}` 动态 (QoS1) | ✅ 已实现 |
| 通道 6 | 设备诊断 | `/sys/{gw}/thing/event/diagnose/set` (QoS1) | `/sys/{gw}/thing/event/diagnose/set_reply` (QoS1) | ✅ 已实现 |
| 通道 7 | DTU 状态上报 | — | `/sys/thing/event/dtuStatus/post` (QoS1) | ❌ 未实现 |
| 通道 8 | HTTP 下载配置 (RSA 鉴权) | — | — | ✅ 已实现 |
| 通道 9 | HTTP 下载驱动 (无鉴权) | — | — | ❌ 未实现 |

### 11.2 契约对齐情况

| 契约项 | 对齐情况 |
|---|---|
| 7 个 outer MQTT topic 方向/QoS/模板/动态 topic | ✅ 6/7 已实现（缺 DTU 状态上报 topic） |
| 上行消息 JSON 字段名与结构 | ✅ 完全对齐 |
| 行为契约：精度 2 位小数、毫秒时间戳、pubMode 0/1、变化死区 0.01 | ✅ 已实现 |
| HTTP 鉴权：RSA_PKCS1v15 + PEM SPKI + base64 + 关闭证书校验 + 重定向 ≤10 | ✅ 已实现 |
| application.json 解析：完整结构 | ✅ 已实现 |
| 三个诊断 item_id（DC1001/DC1002/DC1003） | ✅ DC1001 + DC1003（DC1002 DTU 不适用） |
| 枚举值：dataType / statusCode / 控制器类型 | ✅ 已实现 |

### 11.3 稳定性修复记录

| 问题 | 原因 | 修复 |
|---|---|---|
| `cannot unmarshal string into Config.pubMode of type int` | Web UI FieldEnum 发字符串，Config 字段是 int | 引入 `flexInt` 类型（数字/字符串/null 均可解析） |
| `cannot unmarshal number into Config.port of type string` | FieldInt 发数字，但被误改为 string | 所有数值字段统一用 `flexInt`，彻底消除类型不匹配 |
| 新增输出后网关不响应（HTTP 挂起） | MQTT `Connect()` 无超时，broker 不可达时阻塞 | `SetConnectTimeout(5s)` |
| 更新网关 ID 报 `connection lost before Subscribe completed` | 重载时新旧连接 clientID 相同，broker 踢旧连接 | clientID 改用 gatewayID 生成，重载时新旧唯一 |

### 11.4 简化实现说明

- **pubMode=0（及时上报）**："所有属性至少被刷新过一次才发"的门槛当前简化为每次 flush 上报所有最新值，未做 iRefresh 轮次计数；如需严格对齐，需在 scheduler 侧增加采集轮次计数器
- **服务调用 get**：从缓冲区取最新值，而非实时读取设备；如需实时值，需依赖 `values.Registry` 的最新值查询能力

### 11.5 未实现项（deferred）

- 通道 2（协议驱动管理）：平台运维功能，非核心数据流
- 通道 7（DTU 状态上报）：Go 网关暂无 DTU 透传场景
- 通道 9（驱动下载）：依赖通道 2
- 诊断 DC1002（DTU 在线检测）：Go 网关无 DTU 场景

### 11.6 代码统计

| 文件 | 行数 | 职责 |
|---|---|---|
| `internal/output/smardaten/smardaten.go` | ~780 | 插件注册、MQTT 连接、数据缓冲、上下行处理、flush |
| `internal/output/smardaten/application.go` | ~372 | application.json 解析、平台消息类型、topic 映射 |
| `internal/output/smardaten/sync.go` | ~330 | application.json → 网关配置自动同步 |
| `internal/output/smardaten/http.go` | ~199 | RSA 加密、HTTP 下载、TLS 跳过校验 |
| `internal/output/smardaten/smardaten_test.go` | ~110 | 配置解析回归测试 |
| `internal/output/registry.go` | +12 | BuildContext.Store 注入 |
| **合计** | **~1800** | |