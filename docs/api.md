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

**dataType**:`bool` `int16` `uint16` `int32` `uint32` `float32`

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

## 点位地址

格式 `function:register`,如 `holding:0`、`coil:2`。

| function | Modbus 功能码 | 适用 dataType |
|---|---|---|
| `holding` | 03 读保持寄存器 | int16/uint16/int32/uint32/float32 |
| `input` | 04 读输入寄存器 | int16/uint16/int32/uint32/float32 |
| `coil` | 01 读线圈 | bool |
| `discrete` | 02 读离散输入 | bool |

> `int32`/`uint32`/`float32` 占 2 个寄存器,按大端(ABCD)解析。

---

## 接口列表

### 连接

#### 创建连接

`POST /api/v1/connections`

创建或整体覆盖一个连接。`id` 必填。

**请求体**:Connection 对象

**响应**:`201 Created` + 创建的 Connection

```bash
curl -X POST http://localhost:8080/api/v1/connections \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "conn-1",
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

创建或整体覆盖一个设备(含点位)。`id` 必填,`connectionId` 必须指向已存在的连接。

**请求体**:Device 对象

**响应**:`201 Created` + 创建的 Device

```bash
curl -X POST http://localhost:8080/api/v1/devices \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "sensor-01",
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

所有错误返回统一格式:

```json
{ "error": "错误描述" }
```

| 状态码 | 触发场景 |
|---|---|
| `400 Bad Request` | 请求体 JSON 解析失败;创建时未提供 `id` |
| `404 Not Found` | 获取/更新/删除不存在的连接/设备/点位 |
| `409 Conflict` | 删除仍被设备引用的连接 |
| `500 Internal Server Error` | SQLite 读写失败 |

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
