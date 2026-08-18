# REST API 文档

网关配置接口。通过 REST 增删改查连接、设备与点位,配置写入后**自动热加载**,scheduler 立即按新配置重启采集,无需重启进程。

## 概述

| 项 | 值 |
|---|---|
| Base URL | `http://localhost:8080/api/v1` |
| Content-Type | `application/json` |
| 响应格式 | JSON |
| 端口 | 由 `config.yaml` 的 `http.addr` 决定,默认 `:8080` |

## 数据模型

连接(Connection)与设备(Device)分离:一个连接描述怎么到达总线(传输参数),可被多个设备共享(如同一串口或 DTU 下的多个 Modbus 从站);设备引用连接,并携带总线上寻址该设备的参数(从机地址等)。

### Connection

```json
{
  "id": "conn-1",
  "name": "车间 Modbus TCP",
  "driver": "modbus",
  "config": { "mode": "tcp", "address": "192.168.1.5:502" }
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `id` | string | 是(创建时) | 连接唯一标识 |
| `name` | string | 否 | 可读名称 |
| `driver` | string | 是 | 驱动名,目前支持 `modbus` |
| `config` | object | 是 | 传输参数,结构见[连接配置](#连接配置modbus),不含从机地址 |

### Device

```json
{
  "id": "sensor-01",
  "name": "温湿度传感器",
  "connectionId": "conn-1",
  "params": { "slaveId": 1 },
  "intervalMs": 1000,
  "enabled": true,
  "points": [ { "name": "temperature", "address": "holding:0", "dataType": "int16", "scale": 0.1 } ]
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `id` | string | 是(创建时) | 设备唯一标识 |
| `name` | string | 否 | 可读名称 |
| `connectionId` | string | 是 | 引用的连接 ID |
| `params` | object | 否 | 设备级协议参数(Modbus:`slaveId`),默认 `{}` |
| `intervalMs` | int | 否 | 设备级采集周期(毫秒),默认 `5000` |
| `enabled` | bool | 否 | 是否启用采集,默认 `false` |
| `points` | Point[] | 否 | 采集点位列表 |

### Point

```json
{ "name": "temperature", "address": "holding:0", "dataType": "int16", "scale": 0.1 }
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | string | 是 | 点位名,设备内唯一 |
| `address` | string | 是 | 协议地址,格式见[点位地址](#点位地址) |
| `dataType` | string | 是 | 数据类型,见[枚举](#枚举值) |
| `scale` | float | 否 | 缩放系数,`0` 表示不缩放 |

### 枚举值

**dataType**:`bool` `int16` `uint16` `int32` `uint32` `int64` `float32` `float64` `string`

**quality**(采集结果,非配置项):`good` `bad` `uncertain`

## 连接配置(Modbus)

`Connection.config` 按 `mode` 区分(只含传输参数;从机地址 `slaveId` 在 `Device.params`):

**TCP**:
```json
{ "mode": "tcp", "address": "192.168.1.5:502", "timeout": "1s" }
```

**RTU**:
```json
{ "mode": "rtu", "serialPort": "/dev/ttyS0", "baudRate": 9600, "dataBits": 8, "parity": "N", "stopBits": 1, "timeout": "1s" }
```

**RTU over TCP**(RTU 帧[带 CRC]走 TCP 传输,常见于 RS-485 串口服务器透传):
```json
{ "mode": "rtu-over-tcp", "address": "192.168.1.5:502", "timeout": "1s" }
```

| 字段 | TCP | RTU | RTU over TCP | 默认 |
|---|---|---|---|---|
| `mode` | 必填 `tcp` | 必填 `rtu` | 必填 `rtu-over-tcp` | - |
| `address` | 必填 `host:port` | - | 必填 `host:port` | - |
| `serialPort` | - | 必填 | - | - |
| `baudRate` | - | 可选 | - | `9600` |
| `dataBits` | - | 可选 | - | `8` |
| `parity` | - | 可选 | - | `N` |
| `stopBits` | - | 可选 | - | `1` |
| `timeout` | 可选 | 可选 | 可选 | `1s` |

**设备参数** (`Device.params`,Modbus):

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `slaveId` | int | `0` | Modbus 从机地址 |
| `byteOrder` | string | `ABCD` | 32 位值(`int32`/`uint32`/`float32`)的多寄存器字节序:`ABCD` 大端(默认)/ `CDAB` 字交换(小端)/ `BADC` 字节交换 / `DCBA` 字节+字交换;16 位与线圈恒为大端,不受影响 |
| `pollBlocks` | PollBlock[] | `[]` | 固定读取块;配了块的 function 按块读,未配的自动连读 |

PollBlock:

```json
{ "function": "holding", "start": 0, "count": 12 }
```

| 字段 | 说明 |
|---|---|
| `function` | `holding`/`input`/`coil`/`discrete` |
| `start` | 起始寄存器/线圈地址 |
| `count` | 读取数量(寄存器数或线圈数) |

> 某些设备必须按固定边界/数量读取,自动连读合出的块会触发异常码或数据错位;为该 function 声明 `pollBlocks` 后按固定块读,块外点位标记 bad。

## 连接配置(OPC UA)

`Connection.config`(`driver` 设为 `opcua`):

```json
{ "endpoint": "opc.tcp://192.168.1.5:4840", "securityMode": "none", "timeout": "5s" }
```

| 字段 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `endpoint` | string | 是 | - | OPC UA 端点 |
| `securityMode` | string | 否 | `none` | 安全模式,目前仅支持 `none`(签名/加密需证书,留未来) |
| `username` | string | 否 | - | 非空则用户名密码认证,否则匿名 |
| `password` | string | 否 | - | 配合 `username` |
| `timeout` | string | 否 | `5s` | 请求超时 |
| `mode` | string | 否 | `poll` | 采集模式:`poll`(按周期轮询)或 `subscribe`(订阅,数据变化即推送) |
| `publishInterval` | string | 否 | `1s` | 订阅发布间隔,如 `1s`、`500ms`(仅 `subscribe` 生效) |
| `samplingInterval` | float | 否 | `0` | 监控项采样间隔(毫秒),`0` 表示沿用发布间隔(仅 `subscribe` 生效) |
| `queueSize` | int | 否 | `10` | 每个点位在服务端的队列长度(仅 `subscribe` 生效) |

### 轮询 vs 订阅

- **`poll`(默认)**:与 Modbus 同模型,scheduler 按 `intervalMs` 定时批量读取。
- **`subscribe`**:driver 实现 `driver.Subscriber` 推送能力,scheduler 检测到后注册一次 OPC UA 订阅,数据变化即上送,不再按 `intervalMs` 轮询(该字段在订阅模式下被忽略)。**同一 `endpoint` 下的多个设备共享一个订阅**,按 ClientHandle 分派回各自设备;单点被服务端拒绝(bad status)只记日志不阻断同批;断线由 gopcua 自动重连并重建订阅。

订阅连接配置示例:

```json
{ "endpoint": "opc.tcp://192.168.1.5:4840", "mode": "subscribe",
  "publishInterval": "1s", "samplingInterval": 250, "queueSize": 10 }
```

> OPC UA 设备级 `params` 默认空 `{}`(无从机地址概念);一个 endpoint 可挂多个 Device,共享 session。

## 连接容错与重连

驱动内置自动重连,无需应用层干预,连接断开后自动恢复采集:

- **OPC UA**:gopcua `AutoReconnect`,断开后每 `5s` 重试并自动恢复 session;状态变更(connected/disconnected/reconnecting)写入日志。
- **Modbus TCP / RTU-over-TCP**:连接断开或协议错乱时,单次请求在 `10s` 恢复窗口内重连重试;`IdleTimeout=-1` 保持持久连接,连接断后下次请求自动重新拨号。
- **Modbus RTU(串口)**:`IdleTimeout=-1` 持久占用串口。串口设备节点消失(如 USB-485 拔出)属物理离线,需重新打开串口,当前不自动重开(触发配置 reload 或重启恢复)。

> 重连参数为内置默认,暂未暴露配置项;实采验证后可按需开放。

## 同总线多设备(串口/DTU 挂多台从机)

一个 Connection(同串口/同 DTU)下挂多台设备是推荐用法:底层连接按 Connection 复用,所有请求串行化,同一条总线上同一时刻只有一个请求-响应在飞,不会出现跨设备数据混淆。库层另有帧校验兜底(RTU 校验从机地址 + CRC;TCP 校验 MBAP 事务号 + 单元 ID)。使用时注意:

- **重复端点会被拒绝**:两个 Connection 指向同一串口路径或同一 TCP 端点(DTU)时,保存返回 `409`--不限驱动与连接模式,同一物理总线只允许一个连接(两条并发通道写同一条 485 总线会导致帧碰撞)。
- **超时要大于设备最坏响应时间**:RTU 读响应不回显寄存器地址,若设备响应慢于 `timeout`,迟到帧可能被下一周期同从机、同功能码的请求误当作本次响应,表现为数据"晚一拍"(陈旧值,非跨设备错乱)。DTU 引入几百毫秒网络延迟,建议 `timeout` 调到 2~3s。
- **采集周期要留足总线容量**:同总线 N 台设备串行排队,单请求最坏耗时 ≈ `timeout` + 帧间隙。经验公式:最小采集周期 ≥ 设备数 × 单请求耗时;配得偏小会持续出现 `pool busy, skip collect` 跳采日志。
- **DTU 须配置为纯透传模式**:DTU 侧的"自动打包/打包时间/多主机"等特性会改变帧边界,需关闭。

## 点位地址

**Modbus**:格式 `function:register`,如 `holding:0`、`coil:2`。

| function | Modbus 功能码 | 适用 dataType |
|---|---|---|
| `holding` | 03 读保持寄存器 | int16/uint16/int32/uint32/float32 |
| `input` | 04 读输入寄存器 | int16/uint16/int32/uint32/float32 |
| `coil` | 01 读线圈 | bool |
| `discrete` | 02 读离散输入 | bool |

> `int32`/`uint32`/`float32` 占 2 个寄存器,按大端(ABCD)解析。

**OPC UA**:`address` 直接用 NodeID 字符串,如 `ns=2;s=Temperature`、`ns=0;i=2258`、`i=1234`、`s=Foo`(ns=0 的 string node 可省略 `s=`)。非法 NodeID 在运行时由 server 返回 bad quality。

---

## 接口列表

### 连接

#### 创建连接

`POST /api/v1/connections`

创建或整体覆盖一个连接。`id` 可省略,省略时后台自动生成(格式 `conn-<8位十六进制>`);显式传入同 `id` 则覆盖更新。

**请求体**:Connection 对象

**响应**:`201 Created` + 创建的 Connection(含生成的 `id`)

```bash
curl -X POST http://localhost:8080/api/v1/connections \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "车间 Modbus TCP",
    "driver": "modbus",
    "config": {"mode":"tcp","address":"192.168.1.5:502"}
  }'
```

#### 列出连接

`GET /api/v1/connections`

**响应**:`200 OK` + Connection 数组

#### 获取连接

`GET /api/v1/connections/{connectionId}`

**响应**:`200 OK` + Connection;不存在则 `404`

#### 更新连接

`PUT /api/v1/connections/{connectionId}`

整体更新连接。路径中的 `connectionId` 以 URL 为准。

**请求体**:Connection 对象(可不带 id)

**响应**:`200 OK` + 更新后的 Connection

#### 删除连接

`DELETE /api/v1/connections/{connectionId}`

若连接仍被设备引用,返回 `409 Conflict`;否则删除。

**响应**:`204 No Content`(无响应体)

### 设备

#### 创建设备

`POST /api/v1/devices`

创建或整体覆盖一个设备(含点位)。`id` 可省略,省略时后台自动生成(格式 `dev-<8位十六进制>`);`connectionId` 必须指向已存在的连接。

**请求体**:Device 对象

**响应**:`201 Created` + 创建的 Device(含生成的 `id`)

```bash
curl -X POST http://localhost:8080/api/v1/devices \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "温湿度传感器",
    "connectionId": "conn-1",
    "params": {"slaveId":1},
    "intervalMs": 1000,
    "enabled": true,
    "points": [
      {"name":"temperature","address":"holding:0","dataType":"int16","scale":0.1},
      {"name":"humidity","address":"holding:1","dataType":"int16","scale":0.1}
    ]
  }'
```

#### 列出设备

`GET /api/v1/devices`

**响应**:`200 OK` + Device 数组(每个设备含其点位)

#### 获取设备

`GET /api/v1/devices/{deviceId}`

**响应**:`200 OK` + Device;不存在则 `404`

#### 更新设备

`PUT /api/v1/devices/{deviceId}`

整体更新设备及其点位。路径中的 `deviceId` 以 URL 为准(覆盖请求体中的 id)。**点位列表会被整体替换**,未在请求体中列出的点位将被删除。

**请求体**:Device 对象(可不带 id)

**响应**:`200 OK` + 更新后的 Device

#### 删除设备

`DELETE /api/v1/devices/{deviceId}`

级联删除该设备的所有点位。

**响应**:`204 No Content`(无响应体)

#### 复制设备

`POST /api/v1/devices/{deviceId}/clone`

基于源设备复制出新设备,点表(`points`)整体拷贝,避免同型号设备重复配点。请求体提供新设备 `name`(必填),`id` 可省略(后台自动生成);其余字段未提供则从源设备继承、提供则覆盖。

| 字段 | 必填 | 说明 |
|---|---|---|
| `name` | 是 | 新设备名称 |
| `id` | 否 | 不传则后台自动生成(`dev-<8位十六进制>`) |
| `connectionId` | 否 | 不传则继承源设备 |
| `params` | 否 | 不传则继承源设备(常用于改 `slaveId`) |
| `intervalMs` | 否 | 不传则继承源设备 |
| `enabled` | 否 | 不传则继承源设备 |

**响应**:`201 Created` + 新建的 Device;源设备不存在则 `404`

```bash
# 复制 sensor-01(ID 自动生成),仅改从机地址
curl -X POST http://localhost:8080/api/v1/devices/sensor-01/clone \
  -H 'Content-Type: application/json' \
  -d '{"name":"温湿度-2","params":{"slaveId":2}}'
```

#### 写入设备点位

`POST /api/v1/devices/{deviceId}/write`

即时下发单点写值,不走采集循环。驱动须支持写(modbus/opcua 支持),否则返回 `501`。连接复用驱动的 ConnectionID 池(写完即释放,采集在用则不关)。

**请求体**:

```json
{"point": "setpoint", "value": 42}
```

- `point`:设备上已配置的点位名(按其 `address`/`dataType` 编码下发)
- `value`:工程值。modbus 寄存器写时 `scale` 非零则反向缩放(value/scale)为寄存器原始值;opcua 节点值即工程值,忽略 scale

**响应**:`200 OK` + 写结果数组

```json
[{"point": "setpoint", "ok": true}]
```

- `ok=false`:该点写失败(地址错误/类型不匹配/协议拒绝),不阻断同批其他点

**错误码**:`404` 设备不存在;`400` 点位不存在;`501` 驱动不支持写;`502` 连接或写失败

```bash
curl -X POST http://localhost:8080/api/v1/devices/sensor-01/write \
  -H 'Content-Type: application/json' \
  -d '{"point":"setpoint","value":42}'
```

### 状态

设备运行时健康状态由 scheduler 采集过程中实时上报(内存态,不持久化)。

#### 列出全部设备状态

`GET /api/v1/status`

**响应**:`200 OK` + 状态数组(按 deviceId 排序)

```json
[ { "deviceId": "sensor-01", "online": true, "lastCollect": "2026-08-14T10:00:00Z", "lastError": "", "lastErrorAt": "0001-01-01T00:00:00Z" } ]
```

#### 获取单设备状态

`GET /api/v1/devices/{deviceId}/status`

**响应**:`200 OK` + 单条状态;设备从未被上报过则 `404`

| 字段 | 类型 | 说明 |
|---|---|---|
| `deviceId` | string | 设备 ID |
| `online` | bool | 在线状态:轮询为"最近一次读成功且至少一个点位 good";订阅/监听为"注册成功" |
| `lastCollect` | string | 最近一次成功采集时间(零值=从未) |
| `lastError` | string | 最近一次错误(空=无) |
| `lastErrorAt` | string | 最近一次错误时间 |

### 驱动

#### 列出驱动及其配置结构

`GET /api/v1/drivers`

返回已注册驱动的名称与配置 schema(由驱动声明,前端据此动态渲染表单):

```json
[
  {
    "name": "modbus",
    "config": [
      { "name": "mode", "label": "连接模式", "type": "enum", "required": true, "default": "tcp", "options": ["tcp","rtu","rtu-over-tcp"] }
    ],
    "params": [
      { "name": "slaveId", "label": "从机地址", "type": "int", "default": 1 }
    ]
  }
]
```

| 字段 | 说明 |
|---|---|
| `config` | `Connection.config` 的字段结构 |
| `params` | `Device.params` 的字段结构 |

Field 字段:`name`(JSON key)、`label`(展示名)、`type`(`string`/`int`/`number`/`bool`/`enum`/`json`)、`required`、`default`、`options`(enum 用)、`hint`、`placeholder`。

> 驱动实现 `driver.SchemaProvider` 接口即提供 schema;未实现的驱动 `config`/`params` 为空。

### 北向输出

北向输出(数据上送目标)存 SQLite,经 Web UI 增删改;**保存后立即热重载**,无需重启进程。

#### 列出输出类型及配置结构

`GET /api/v1/outputs/types`

返回已注册输出类型及配置 schema(前端据此动态渲染表单):

```json
[
  {
    "type": "mqtt",
    "label": "MQTT",
    "schema": [
      { "name": "broker", "label": "Broker 地址", "type": "string", "required": true },
      { "name": "password", "label": "密码", "type": "password" },
      { "name": "qos", "label": "QoS", "type": "int", "default": 1 }
    ]
  }
]
```

Field 字段与驱动一致:`name`/`label`/`type`(`string`/`password`/`int`/`number`/`bool`/`enum`/`json`)/`required`/`default`/`options`/`hint`/`placeholder`。

#### 列出输出

`GET /api/v1/outputs`

**响应**:`200 OK` + Output 数组

```json
[ { "id": "mqtt-1", "name": "MQTT 主站", "type": "mqtt", "config": {"broker":"tcp://127.0.0.1:1883","qos":1}, "enabled": true } ]
```

#### 获取输出

`GET /api/v1/outputs/{outputId}`

**响应**:`200 OK` + Output;不存在则 `404`

#### 创建输出

`POST /api/v1/outputs`

创建或整体覆盖一个输出。`type` 必填;`id` 可省略,省略时后台自动生成(格式 `out-<8位十六进制>`)。

**请求体**:Output 对象

**响应**:`201 Created` + Output;若保存后热重载失败(broker 不可达等)返回 `502`(配置已持久化,旧输出保持运行)

```bash
curl -X POST http://localhost:8080/api/v1/outputs \
  -H 'Content-Type: application/json' \
  -d '{"name":"MQTT 主站","type":"mqtt","enabled":true,"config":{"broker":"tcp://127.0.0.1:1883","qos":1}}'
```

#### 更新输出

`PUT /api/v1/outputs/{outputId}`

整体更新输出。路径中的 `outputId` 以 URL 为准。`type` 必填。

**响应**:`200 OK` + 更新后的 Output

#### 删除输出

`DELETE /api/v1/outputs/{outputId}`

删除后立即停止该输出的上送。

**响应**:`204 No Content`(无响应体)

> **鉴权**:输出配置含云端凭据(密码 / Access Token),`outputs:read` / `outputs:write` 属敏感 scope,只应授予可信主体(默认仅管理员持有)。

### 网关设置

网关 ID 存 SQLite(默认 `iot-gateway`,首次启动预置),管理员可经 Web UI(概览页)修改。

#### 获取网关 ID

`GET /api/v1/gateway`

**响应**:`200 OK`

```json
{ "id": "iot-gateway" }
```

#### 修改网关 ID

`PUT /api/v1/gateway`

网关 ID 参与 MQTT 输出 topic(`gateway/{id}/device/{deviceId}/data`)等标识;保存后输出立即热重载以应用新 ID。

**请求体**:

```json
{ "id": "gw-02" }
```

**响应**:`200 OK` + 更新后的 ID;`id` 为空则 `400`;保存成功但热重载失败返回 `502`(旧输出保持运行)

### 点位

#### 添加点位

`POST /api/v1/devices/{deviceId}/points`

向已有设备追加单个点位,不影响其他点位。

**请求体**:Point 对象

**响应**:`201 Created` + 添加的 Point

#### 删除点位

`DELETE /api/v1/devices/{deviceId}/points/{name}`

**响应**:`204 No Content`(无响应体)

---

## 错误响应

所有错误返回统一格式,含人类可读 `error` 与机器可读 `code`(供前端/三方按错误类型分支,如跳转改密页):

```json
{ "error": "错误描述", "code": "bad_request" }
```

`code` 默认按 HTTP 状态码映射,业务有更精确语义时覆盖(如改密门禁返回 `code: "password_change_required"`):

| HTTP 状态码 | 默认 `code` |
|---|---|
| `400` | `bad_request` |
| `401` | `unauthenticated` |
| `403` | `forbidden` |
| `404` | `not_found` |
| `409` | `conflict` |
| `501` | `not_implemented` |
| `502` | `bad_gateway` |
| `503` | `service_unavailable` |
| 其他 | `internal_error` |

| 状态码 | 触发场景 |
|---|---|
| `400 Bad Request` | 请求体 JSON 解析失败;创建时未提供 `id`;非法密码密文 |
| `401 Unauthorized` | 未认证或凭证无效 |
| `403 Forbidden` | 无 scope 权限;首次登录须先改密(`password_change_required`) |
| `404 Not Found` | 获取/更新/删除不存在的连接/设备/点位 |
| `409 Conflict` | 删除仍被设备引用的连接;端点已被其他连接占用 |
| `500 Internal Server Error` | SQLite 读写失败 |
| `502 Bad Gateway` | 输出热重载失败(配置已持久化,旧输出保持运行) |

---

## 端到端测试流程

前置:网关已启动(`./gateway`,需 MQTT broker 可达,因启动时连接)。API 测试本身不依赖 Modbus 设备或 MQTT 数据流,仅操作 SQLite 配置。

```bash
BASE=http://localhost:8080/api/v1

# 0. 先建连接
curl -s -X POST $BASE/connections -H 'Content-Type: application/json' -d '{
  "id":"conn-1","name":"modbus-tcp","driver":"modbus",
  "config":{"mode":"tcp","address":"192.168.1.5:502"}
}'

# 1. 创建设备(引用连接,含两个点位)
curl -s -X POST $BASE/devices -H 'Content-Type: application/json' -d '{
  "id":"sensor-01","name":"温湿度","connectionId":"conn-1","params":{"slaveId":1},
  "intervalMs":1000,"enabled":true,
  "points":[
    {"name":"temperature","address":"holding:0","dataType":"int16","scale":0.1},
    {"name":"humidity","address":"holding:1","dataType":"int16","scale":0.1}
  ]
}'

# 2. 列出设备,确认已创建
curl -s $BASE/devices

# 3. 获取单个设备
curl -s $BASE/devices/sensor-01

# 4. 追加一个点位
curl -s -X POST $BASE/devices/sensor-01/points -H 'Content-Type: application/json' \
  -d '{"name":"pressure","address":"holding:2","dataType":"float32","scale":0}'

# 5. 确认点位已追加(应看到 3 个点位)
curl -s $BASE/devices/sensor-01

# 6. 删除刚追加的点位
curl -s -o /dev/null -w "%{http_code}\n" -X DELETE $BASE/devices/sensor-01/points/pressure
# 期望输出: 204

# 7. 删除被引用的连接(期望 409)
curl -s -o /dev/null -w "%{http_code}\n" -X DELETE $BASE/connections/conn-1
# 期望输出: 409

# 8. 整体更新设备(改采集周期,点位会被替换为只含 temperature)
curl -s -X PUT $BASE/devices/sensor-01 -H 'Content-Type: application/json' -d '{
  "name":"温湿度","connectionId":"conn-1","params":{"slaveId":1},
  "intervalMs":2000,"enabled":true,
  "points":[{"name":"temperature","address":"holding:0","dataType":"int16","scale":0.1}]
}'

# 9. 删除设备后,连接方可删除
curl -s -o /dev/null -w "%{http_code}\n" -X DELETE $BASE/devices/sensor-01
# 期望输出: 204
curl -s -o /dev/null -w "%{http_code}\n" -X DELETE $BASE/connections/conn-1
# 期望输出: 204

# 10. 确认已删除(期望 404)
curl -s -o /dev/null -w "%{http_code}\n" $BASE/devices/sensor-01
# 期望输出: 404
```

### 验证热加载

创建 `enabled:true` 的设备后,若 MQTT broker 与 Modbus 设备就绪,可订阅观察数据上送:

```bash
mosquitto_sub -t 'gateway/+/device/+/data' -v
```

修改设备 `intervalMs` 或增删点位后,采集周期与新点位立即生效,无需重启网关。
