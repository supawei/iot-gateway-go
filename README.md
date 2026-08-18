# iot-gateway-go

Go 实现的开源工业物联网边缘网关。插件化架构,南向接入工业设备,北向上送数据,中间用统一的"设备-点位"模型屏蔽协议差异。

## 架构

```
┌──────────────────────────────────────────────────┐
│  Northbound 北向输出层（输出插件）                  │
│  已实现: MQTT / ThingsBoard / TDengine  预留: 云平台 │
├──────────────────────────────────────────────────┤
│  Processing 处理层（预留,目前直通）                 │
├──────────────────────────────────────────────────┤
│  Core 核心: 调度器 + 管道 + 设备/点表管理            │
│           + REST配置API + SQLite                  │
├──────────────────────────────────────────────────┤
│  Southbound 南向接入层（驱动插件）                  │
│  已实现: Modbus / OPC UA / 监听  预留: 总线/以太网   │
└──────────────────────────────────────────────────┘
```

数据流:`scheduler 采集 ──DataPoint──▶ channel ──▶ pipeline ──▶ Output`

协议差异封死在南向驱动内部,Core 与北向只面对统一的 `DataPoint`。加新协议 = 加一个实现 `Driver` 接口的子包。

## 快速开始

```bash
cp config.example.yaml config.yaml   # 编辑 MQTT broker 等
go build -o gateway ./cmd/gateway    # 或 make build(自动注入版本号)
./gateway
```

启动后可通过 Web 管理界面操作设备/连接(默认 `http://localhost:8080` 提供 API,前端见 [web/](web/))。

## 命令行参数与版本

```bash
./gateway -h          # 显示程序当前版本与用法
./gateway -v          # 仅显示当前版本(等价于 --version)
./gateway             # 使用默认 config.yaml 启动
./gateway /etc/gateway.yaml   # 指定配置文件启动
./gateway -config /etc/gateway.yaml
```

版本号(语义化版本 + git 提交哈希 + 构建时间 + Go 版本)在编译时自动注入:`make build` 会取最近的 git tag(如 `v1.2.0`),无 tag 时回退到短提交哈希;`go build` 直编则显示占位版本 `dev`。要打版本 tag,执行 `git tag v1.2.0 && make build` 即可。

## 网关配置

`config.yaml` 配置网关自身参数(设备/点位配置走 REST API + SQLite):

> **注意**:`config.yaml` 只在启动时读取一次,修改后需重启进程;设备/点位/连接配置走 REST API + SQLite,**写入即自动热加载,无需重启**。两种配置的热加载行为不同。

| 字段 | 说明 | 默认 |
|---|---|---|
| `gateway.id` | 网关 ID,用于 MQTT topic | `iot-gateway` |
| `http.addr` | REST API 监听地址 | `:8080` |
| `mqtt.*` | MQTT broker 连接 | - |
| `thingsboard.*` | ThingsBoard 平台对接(可选,见 [docs/thingsboard.md](docs/thingsboard.md)) | - |
| `tdengine.*` | TDengine 时序库对接(可选,见 [docs/tdengine.md](docs/tdengine.md)) | - |
| `storage.sqlitePath` | SQLite 路径 | `./gateway.db` |
| `scheduler.poolSize` | 采集 worker 池大小(最大并发采集数) | `16` |
| `log.level` | 日志级别(debug/info/warn/error) | `info` |
| `log.format` | 日志格式(text/json) | `text` |
| `log.file.*` | 文件轮转(path/maxSize/maxBackups/maxAge/compress),path 留空只输出 stdout | - |

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

MQTT 输出默认**即时单条发布**(payload 为单个 `DataPoint` 对象);设置 `flushInterval`
(如 `200ms`)可启用**批量模式**:同一设备在一个窗口内到达的点聚合为一条消息,
payload 为 `DataPoint` 数组,`batchMax` 控制单条最大点数(超出拆分)。详见
[docs/mqtt-batch-publish-design.md](docs/mqtt-batch-publish-design.md)。

## Modbus 驱动

**连接配置** (`Connection.config` 字段,传输参数;从机地址 `slaveId` 在 `Device.params`):

| 模式 | config 字段 |
|---|---|
| TCP | `mode:"tcp"`, `address:"host:502"` |
| RTU | `mode:"rtu"`, `serialPort:"/dev/ttyS0"`, `baudRate`, `dataBits`, `parity`, `stopBits` |
| RTU over TCP | `mode:"rtu-over-tcp"`, `address:"host:502"` |

**点位地址** (`address` 字段):`function:register`,function 为 `holding`/`input`/`coil`/`discrete`。

**设备参数** (`Device.params`):`slaveId`(从机地址)、`byteOrder`(32 位值字节序,默认 `ABCD`)、`pollBlocks`(固定读取块)。

**数据类型**:`bool` `int16` `uint16` `int32` `uint32` `float32`。`scale` 非零时数值类型按系数缩放为 float64。`int32`/`uint32`/`float32` 跨 2 个寄存器,字节序由设备参数 `byteOrder` 控制,支持全部四种:`ABCD`(大端,默认)、`CDAB`(字交换/小端)、`BADC`(字节交换)、`DCBA`(字节+字交换);16 位与线圈恒为大端,不受影响。

## OPC UA 驱动

**连接配置** (`Connection.config`,`driver` 设为 `opcua`):

| 字段 | 说明 |
|---|---|
| `endpoint` | OPC UA 端点,如 `opc.tcp://192.168.1.5:4840` |
| `securityMode` | 安全模式:`none`(默认)/ `sign`(签名防篡改)/ `signAndEncrypt`(签名+加密) |
| `securityPolicy` | 安全策略:`auto`(默认,按端点协商选最强)/ `Basic128Rsa15` / `Basic256` / `Basic256Sha256`(工业界事实标准)/ `Aes128Sha256RsaOaep` / `Aes256Sha256RsaPss`(仅安全模式生效) |
| `clientCertFile`/`clientKeyFile` | 客户端证书/私钥文件;留空自动生成自签证书(网关工作目录),仅生成一次 |
| `serverThumbprint` | 服务器证书 SHA-1 指纹(40 位 hex);设置后建连前校验,防中间人/防证书被换 |
| `username`/`password` | 可选,留空则匿名(可与安全模式组合) |
| `timeout` | 请求超时,默认 `5s` |
| `mode` | 采集模式:`poll`(默认,按周期轮询)或 `subscribe`(订阅,数据变化即推送) |
| `publishInterval` | 订阅发布间隔,如 `1s`、`500ms`,默认 `1s`(仅 `subscribe` 生效) |
| `samplingInterval` | 监控项采样间隔(毫秒),`0` 表示沿用发布间隔(仅 `subscribe` 生效) |
| `queueSize` | 每个点位在服务端的队列长度,默认 `10`(仅 `subscribe` 生效) |

安全连接示例:

```json
{ "endpoint": "opc.tcp://192.168.1.5:4840", "securityMode": "signAndEncrypt",
  "securityPolicy": "Basic256Sha256", "serverThumbprint": "0123...abcd" }
```

> **部署提示**:启用安全模式后,网关自动生成的客户端证书 `opcua-client-cert.pem` 需导入服务器信任库(部分服务器拒绝未知客户端证书);`serverThumbprint` 建议配置以开启服务器证书校验。详见 [docs/opcua-security-design.md](docs/opcua-security-design.md)。

**点位地址** (`address` 字段):OPC UA NodeID,如 `ns=2;s=Temperature`、`ns=0;i=2258`、`i=1234`。

> NodeID 裸字符串一律按 **string node** 解析:`1234`(不带 `i=`)是 ns=0 的 string 节点,数值节点须写 `i=1234`。

**节点浏览选择**:设备点位编辑时对 OPC UA 连接点击「浏览」按钮,从服务器节点树懒加载选点,自动回填 NodeID(基于 `driver.Browser` 能力 + `POST /api/v1/connections/{id}/browse`)。

**数据类型**:`bool` `int16` `uint16` `int32` `uint32` `int64` `float32` `float64` `string`。`scale` 非零时数值类型按系数缩放为 float64。

### 轮询与订阅

- **轮询(`poll`,默认)**:与 Modbus 同模型,scheduler 按 `intervalMs` 定时调用 `Read` 批量读取。
- **订阅(`subscribe`)**:driver 实现 `driver.Subscriber` 推送能力,scheduler 检测到后注册一次 OPC UA 订阅,数据变化即上送,不再按 `intervalMs` 轮询(该字段在订阅模式下被忽略)。**同一 `endpoint` 下的多个设备共享一个订阅**,按 ClientHandle 分派回各自设备;单点被服务端拒绝(bad status)只记日志不阻断同批;连接断开由 gopcua 自动重连并重建订阅。

订阅连接配置示例:

```json
{ "endpoint": "opc.tcp://192.168.1.5:4840", "mode": "subscribe",
  "publishInterval": "1s", "samplingInterval": 250, "queueSize": 10 }
```

> **已知限制与缺口**:数据类型仅标量(不支持数组/DateTime/结构体等);无方法调用与历史读取;安全模式不做 CA 证书链校验(以 `serverThumbprint` 指纹为信任锚)。实现完整性分析见 [docs/opcua-driver-review.md](docs/opcua-driver-review.md)。

## 监听类驱动(modbus_listen)

网关作为 **TCP 服务端被动 listen**,设备/主站主动连入并按 Modbus TCP(MBAP)推帧,驱动按从机地址(UnitID)路由到已配置设备。这是 `driver.Listener` 监听能力的参考实现,详见 [docs/listener.md](docs/listener.md)。

**连接配置** (`Connection.config`,`driver` 设为 `modbus_listen`):

| 字段 | 说明 |
|---|---|
| `listen` | 本地监听地址,如 `:502` 或 `0.0.0.0:502` |
| `timeout` | 设备连接的空闲读超时(超时断开,设备需重连),默认 `60s` |

**设备参数** (`Device.params`):`{ "slaveId": 1, "byteOrder": "ABCD" }` —— `slaveId` 为 Modbus 从机地址(UnitID),用于把上报帧路由到对应设备;`byteOrder` 为 32 位值字节序(默认 `ABCD`,支持 `ABCD`/`BADC`/`CDAB`/`DCBA`)。

**点位地址** (`address` 字段):寄存器偏移(十进制),`int32`/`uint32`/`float32` 占 2 个寄存器,字节序由设备参数 `byteOrder` 控制;16 位恒为大端。

```jsonc
// Connection
{ "listen": ":502", "timeout": "60s" }
// Device.params
{ "slaveId": 1, "byteOrder": "CDAB" }
// Point: address 为寄存器偏移
{ "name": "level", "address": "0", "dataType": "float32", "scale": 0.1 }
```

> 已知缺口:加性校准(`value = raw*multiple + calibration` 中的 `calibration`)暂未实现,详见 listener 设计文档。

## ThingsBoard 输出

采用 ThingsBoard **MQTT Gateway** 模式:网关作为一个"网关设备",单连接上报 N 个子设备的数据。每个设备映射为一个子设备(设备名 = `Device.ID` + 可选前缀),点位值为遥测、`Quality` 为客户端属性 `quality`。详见 [docs/thingsboard.md](docs/thingsboard.md)。

```yaml
# config.yaml
thingsboard:
  broker: "tcp://tb.example.com:1883"
  accessToken: "gateway-access-token"
  clientId: "iot-gateway-tb"
  qos: 1
  deviceNamePrefix: ""   # 可选
  reportQuality: true    # 可选,默认 true
```

配置 `broker + accessToken` 后即启用(可与 mqtt 输出并存)。

## TDengine 输出

通过 taosAdapter REST API 写入 TDengine 时序库:所有点位写入一张超级表,值按类型落到强类型列(DOUBLE/BIGINT/BOOL/NCHAR),设备与点位作为 TAGS,子表按 (设备, 点位) 自动建表。详见 [docs/tdengine.md](docs/tdengine.md)。

```yaml
# config.yaml
tdengine:
  url: "http://127.0.0.1:6041"  # taosAdapter REST 地址
  username: "root"               # 默认 root
  password: "taosdata"           # 默认 taosdata
  database: "iot_gateway"        # 默认 iot_gateway
  stable: "data_points"          # 默认 data_points
  flushInterval: "1s"            # 默认 1s
```

配置 `url` 后即启用(可与 mqtt / thingsboard 输出并存)。

## REST API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/connections` | 创建连接 |
| GET | `/api/v1/connections` | 列出连接 |
| GET | `/api/v1/connections/{connectionId}` | 获取连接 |
| POST | `/api/v1/connections/{connectionId}/browse` | 浏览服务器节点树(OPC UA 等) |
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

## Web 管理界面

基于 Vue 3 + Element Plus 的前端(独立工程,位于 `web/`),提供概览、连接管理、设备管理(点位/克隆/写值)。**构建产物经 `go:embed` 打进网关二进制**,启动网关后访问 `http://<网关IP>:8080/` 即为管理界面,无需额外静态服务器或 nginx。

```bash
make build          # 先编译前端再编译二进制(前端已内嵌)
# 或前端开发模式:
cd web && npm install && npm run dev   # http://localhost:5173, /api 代理到 :8080
```

> 前端构建产物 `web/dist/` 不纳入 git(`.gitignore` 已忽略)。`go build`/`go test`/`go vet` 依赖该目录存在(经 `go:embed` 内嵌),请先 `make web` 生成,或直接 `make build` 一步完成。

详见 [web/README.md](web/README.md)。

## 扩展开发

**新增南向驱动**:实现 `driver.Driver` 接口,在 `init()` 中 `driver.Register(name, drv)`,main 导入该包。按需叠加可选能力接口:`driver.Writer`(写)、`driver.Subscriber`(网关主动订阅)、`driver.Listener`(网关被动监听)。

**新增北向输出**:实现 `output.Output` 接口,在 main 中构造并加入 outputs 列表。

## 项目结构

```
cmd/gateway/          入口
internal/
  model/              Connection/Device/Point/DataPoint 数据模型
  driver/             Driver/Conn 接口 + registry
    modbus/           Modbus 驱动(轮询)
    modbus_listen/    Modbus 监听驱动(设备主动连入上报)
    opcua/            OPC UA 驱动(轮询/订阅)
  output/             Output 接口
    mqtt/             MQTT 输出
    thingsboard/      ThingsBoard 输出
    tdengine/         TDengine 输出
  core/               scheduler + pipeline
  store/              SQLite 持久化
  api/                REST 配置 API
  status/             设备运行时状态
  config/             YAML 配置
web/                  管理界面(Vue 3 + Element Plus,dist 经 go:embed 内嵌)
```

## License

[MIT](LICENSE) © 2026 iot-gateway-go contributors
