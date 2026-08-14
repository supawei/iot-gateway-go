# iot-gateway-go

Go 实现的开源工业物联网边缘网关。插件化架构,南向接入工业设备,北向上送数据,中间用统一的"设备-点位"模型屏蔽协议差异。

## 架构

```
┌──────────────────────────────────────────────┐
│  Northbound 北向输出层（输出插件）              │
│  已实现: MQTT        预留: 云IoT平台            │
├──────────────────────────────────────────────┤
│  Processing 处理层（预留,目前直通）             │
├──────────────────────────────────────────────┤
│  Core 核心: 调度器 + 管道 + 设备/点表管理        │
│           + REST配置API + SQLite              │
├──────────────────────────────────────────────┤
│  Southbound 南向接入层（驱动插件）              │
│  已实现: Modbus/OPC UA  预留: 总线/以太网          │
└──────────────────────────────────────────────┘
```

数据流:`scheduler 采集 ──DataPoint──▶ channel ──▶ pipeline ──▶ Output`

协议差异封死在南向驱动内部,Core 与北向只面对统一的 `DataPoint`。加新协议 = 加一个实现 `Driver` 接口的子包。

## 快速开始

```bash
cp config.example.yaml config.yaml   # 编辑 MQTT broker 等
go build -o gateway ./cmd/gateway
./gateway
```

## 网关配置

`config.yaml` 配置网关自身参数(设备/点位配置走 REST API + SQLite):

| 字段 | 说明 | 默认 |
|---|---|---|
| `gateway.id` | 网关 ID,用于 MQTT topic | `iot-gateway` |
| `http.addr` | REST API 监听地址 | `:8080` |
| `mqtt.*` | MQTT broker 连接 | - |
| `storage.sqlitePath` | SQLite 路径 | `./gateway.db` |
| `scheduler.poolSize` | 采集 worker 池大小(最大并发采集数) | `16` |

## 配置设备

通过 REST API 配置一个 Modbus TCP 设备:

```bash
# 1. 先建连接(传输参数,可被多个从机设备共享)
curl -X POST http://localhost:8080/api/v1/connections -H 'Content-Type: application/json' -d '{
  "id": "conn-1",
  "name": "车间 Modbus TCP",
  "driver": "modbus",
  "config": {"mode":"tcp","address":"192.168.1.5:502"}
}'

# 2. 再建设备,引用连接并配从机地址
curl -X POST http://localhost:8080/api/v1/devices -H 'Content-Type: application/json' -d '{
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

数据即按周期采集并发布到 MQTT topic `gateway/{gatewayId}/device/sensor-01/data`。

## Modbus 驱动

**连接配置** (`Connection.config` 字段,传输参数;从机地址 `slaveId` 在 `Device.params`):

| 模式 | config 字段 |
|---|---|
| TCP | `mode:"tcp"`, `address:"host:502"` |
| RTU | `mode:"rtu"`, `serialPort:"/dev/ttyS0"`, `baudRate`, `dataBits`, `parity`, `stopBits` |
| RTU over TCP | `mode:"rtu-over-tcp"`, `address:"host:502"` |

**点位地址** (`address` 字段):`function:register`,function 为 `holding`/`input`/`coil`/`discrete`。

**数据类型**:`bool` `int16` `uint16` `int32` `uint32` `float32`。`scale` 非零时数值类型按系数缩放为 float64。

## OPC UA 驱动

**连接配置** (`Connection.config`,`driver` 设为 `opcua`):

| 字段 | 说明 |
|---|---|
| `endpoint` | OPC UA 端点,如 `opc.tcp://192.168.1.5:4840` |
| `securityMode` | 安全模式,目前仅支持 `none`(默认) |
| `username`/`password` | 可选,留空则匿名 |
| `timeout` | 请求超时,默认 `5s` |

**点位地址** (`address` 字段):OPC UA NodeID,如 `ns=2;s=Temperature`、`ns=0;i=2258`、`i=1234`。

**数据类型**:`bool` `int16` `uint16` `int32` `uint32` `int64` `float32` `float64` `string`。`scale` 非零时数值类型按系数缩放为 float64。

> OPC UA 驱动轮询读取(与 Modbus 同模型);订阅(Subscription)留作未来扩展。

## REST API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/connections` | 创建连接 |
| GET | `/api/v1/connections` | 列出连接 |
| GET | `/api/v1/connections/{connectionId}` | 获取连接 |
| PUT | `/api/v1/connections/{connectionId}` | 更新连接 |
| DELETE | `/api/v1/connections/{connectionId}` | 删除连接(被设备引用时 409) |
| POST | `/api/v1/devices` | 创建设备(含点位) |
| GET | `/api/v1/devices` | 列出设备 |
| GET | `/api/v1/devices/{deviceId}` | 获取设备 |
| PUT | `/api/v1/devices/{deviceId}` | 更新设备 |
| DELETE | `/api/v1/devices/{deviceId}` | 删除设备 |
| POST | `/api/v1/devices/{deviceId}/clone` | 复制设备(点表整体拷贝) |
| POST | `/api/v1/devices/{deviceId}/points` | 添加点位 |
| DELETE | `/api/v1/devices/{deviceId}/points/{name}` | 删除点位 |

配置写入后自动热加载,scheduler 全量重启采集,无需重启进程。

## 扩展开发

**新增南向驱动**:实现 `driver.Driver` 接口,在 `init()` 中 `driver.Register(name, drv)`,main 导入该包。

**新增北向输出**:实现 `output.Output` 接口,在 main 中构造并加入 outputs 列表。

## 项目结构

```
cmd/gateway/          入口
internal/
  model/              Connection/Device/Point/DataPoint 数据模型
  driver/             Driver/Conn 接口 + registry
    modbus/           Modbus 驱动
    opcua/            OPC UA 驱动
  output/             Output 接口
    mqtt/             MQTT 输出
  core/               scheduler + pipeline
  store/              SQLite 持久化
  api/                REST 配置 API
  config/             YAML 配置
```
