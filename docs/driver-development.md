# 南向设备驱动实现指导

> **定位**:面向新增协议的完整实现指引——从接口契约、能力矩阵到分步示例、测试与提交清单。
> **配套**:接口定义 `internal/driver/driver.go`;数据模型 `internal/model/model.go`;参考实现 `internal/driver/{modbus,opcua,modbus_listen}`;监听类协议设计见 [listener.md](listener.md)。
> **更新**:2026-08-19

---

## 1. 总览:驱动在架构中的位置

南向驱动把"一种协议的接入细节"封死在单个子包内,对上暴露协议无关的 `DataPoint`。
调度器、管道、北向输出只面对统一的"设备-点位"模型,**不感知任何协议细节**。

```
Web UI(REST)──▶ core(scheduler/writer/probe)──▶ driver.Driver(南向驱动)
                       │  ▲
                 Driver │  │ DataPoint / WriteResult
                       ▼  │
                    协议设备(Modbus / OPC UA / 自研协议 ...)
```

**插件模型**:驱动是一个**无状态工厂**(实现 `driver.Driver`),`Open` 出绑定单个设备的 `driver.Conn`。
用 `init()` + `driver.Register(name, drv)` 注册,`cmd/gateway/main.go` 以**空导入**加载:

```go
_ "iot-gateway-go/internal/driver/modbus"        // 注册 modbus 驱动
_ "iot-gateway-go/internal/driver/modbus_listen" // 注册 modbus 监听驱动
_ "iot-gateway-go/internal/driver/opcua"         // 注册 opcua 驱动
// 新增驱动只需在此加一行空导入,其余全部零改动
```

### 1.1 生命周期

```
Connection(传输参数) ──┐
                       ├─▶ driver.Open(req) ──▶ Conn(绑定单个设备)
Device(设备参数+点位) ──┘          │
                                  ├─▶ Read(points) ──▶ []DataPoint   (轮询)
                                  ├─▶ [可选] Write / Subscribe / Listen / Probe
                                  └─▶ Close()
```

- `Driver` 是**无状态**的;连接等有状态资源在 `Open` 时创建、`Close` 时释放。
- **同一 `ConnectionID` 上的多个设备共享底层物理连接**(如同一串口 / DTU 下的多个 Modbus 从站),
  由驱动内部维护"连接池 + 引用计数"(见 §4.4),避免重复建连。

### 1.2 能力矩阵

| 能力接口 | 含义 | 检测方式 | 参考实现 |
|---|---|---|---|
| `driver.Conn`(必选) | 轮询批量读取 + 关闭 | 直接持有 | modbus / opcua |
| `driver.Writer` | 支持写点位(北向下行/REST 写) | `conn.(driver.Writer)` | modbus / opcua |
| `driver.Subscriber` | 网关主动订阅、设备推送 | `conn.(driver.Subscriber)` | opcua(subscribe 模式) |
| `driver.Listener` | 网关被动监听、设备主动连入 | `conn.(driver.Listener)` | modbus_listen |
| `driver.Prober` | 设备连通性探测(诊断 DC1003) | `conn.(driver.Prober)` | modbus / opcua |
| `driver.Browser` | 节点树浏览(Web UI 选点位) | `drv.(driver.Browser)` | opcua |
| `driver.EndpointResolver` | 物理端点归一化(防并发冲突) | `drv.(driver.EndpointResolver)` | modbus / opcua / modbus_listen |
| `driver.SchemaProvider` | 连接/设备配置表单结构 | `drv.(driver.SchemaProvider)` | modbus / opcua / modbus_listen |

> 前三个能力(`Writer`/`Subscriber`/`Listener`)作用于 **Conn**,后三个作用于 **Driver** 本身。
> 未实现的能力不会被调用方调用:调度器按类型断言自动选择采集方式,前端按 SchemaProvider 自动渲染表单。

---

## 2. 核心接口契约

全部定义在 `internal/driver/driver.go`,实现前逐条读透注释。

### 2.1 Driver 与 Open

```go
type OpenRequest struct {
    DeviceID     string          // 设备 ID(DataPoint.DeviceID 来源)
    ConnectionID string          // 连接池复用 key(同连接共享底层资源)
    ConnConfig   json.RawMessage // 传输参数(来自 Connection.config)
    DeviceParams json.RawMessage // 设备级协议参数(来自 Device.params)
}

type Driver interface {
    Open(ctx context.Context, req OpenRequest) (Conn, error)
}
```

- `ConnConfig` 只含**传输参数**(怎么到达总线:地址/串口/超时/凭据);
  `DeviceParams` 只含**设备级协议参数**(怎么寻址该设备:从机地址/字节序等)。
- `Open` 只做"绑定":把 req 中的参数解析进 Conn 字段,并(按需)从驱动连接池取共享资源。
  轮询驱动的 `Open` **不发起任何实际 IO**——真正建连应惰性到首次读取(见 §4.5),或由连接池立即建立(参考 modbus 的 `acquire`)。

### 2.2 Conn:Read / Close

```go
type Conn interface {
    Read(ctx context.Context, points []model.Point) ([]model.DataPoint, error)
    Close() error
}
```

**Read 的语义约定(必须遵守)**:

1. 返回值与 `points` **按序一一对应**(`results[i]` 对应 `points[i]`)。
2. `error` **仅用于整批错误**(此时结果无效):配置级错误或整批传输/会话失败(连接断开、请求超时)。
3. **单点**问题(该点地址非法、通信失败、解码异常)→ 用该点 `DataPoint.Quality` 表达(`bad`/`uncertain`),**不阻断同批其他点位**,`error` 返回 `nil`。
4. `DeviceID`、`Point`、`Timestamp` 必须填充;`Quality` 必须显式赋值(不要留零值)。

### 2.3 可选能力(Conn 层)

```go
// 写:批量下发点位值,单点失败以 WriteResult.Ok 表达,不阻断同批。
type Writer interface {
    Write(ctx context.Context, items []model.WriteItem) ([]WriteResult, error)
}

// 订阅:网关主动订阅,数据变化经 onData 推送;ctx 取消后须停止推送并释放资源。
type Subscriber interface {
    Subscribe(ctx context.Context, points []model.Point, onData func(model.DataPoint)) error
}

// 监听:网关被动 listen,设备主动连入上报;ctx 取消后须停止监听并释放资源。
type Listener interface {
    Listen(ctx context.Context, points []model.Point, onData func(model.DataPoint)) error
}

// 探测:做一次轻量协议级读确认设备可达;nil=可达,非 nil=不可达(含原因)。
type Prober interface {
    Probe(ctx context.Context, points []model.Point) error
}
```

- `Subscriber`/`Listener` 是**二选一**的采集方式:调度器检测到任一时,**不再按 `intervalMs` 定时 Read**。
- 设备路由信息(从机地址等)在 `Open` 时经 `DeviceParams` 封存在 Conn 内,`Subscribe`/`Listen` 只注册点位。
- `Probe` 的 `points` 是设备全部点位,可取代表性点位做真实往返(与采集同寻址语义);
  驱动未实现 `Prober` 时,诊断方(如 smardaten DC1003)回退到在线状态软信号,不会误判。

### 2.4 可选能力(Driver 层)

```go
// 浏览:按连接浏览服务器节点树,供 Web UI "浏览选择"点位。
type Browser interface {
    Browse(ctx context.Context, connectionID string, connConfig json.RawMessage, parentNodeID string) ([]NodeInfo, error)
}

// 端点归一化:从 Connection.config 计算物理端点 key,网关据此阻止两条连接指向同一物理总线。
// key 属于跨驱动共享命名空间:serial|<串口> / tcp|<host:port> / listen|<绑定地址>。
// 返回空串表示无法识别,跳过检查。
type EndpointResolver interface {
    EndpointKey(config json.RawMessage) string
}

// 表单结构:声明 Connection.config 与 Device.params 的字段,Web UI 据此动态渲染表单。
type SchemaProvider interface {
    ConfigSchema() []Field
    ParamSchema() []Field
}
```

- **EndpointResolver 很重要**:同一串口 / 同一 `host:port` 上两条并发通道会同时写同一条 485 总线导致帧碰撞。
  保存连接时 API 会调用它做冲突检测(见 `internal/api/api.go`)。**轮询类驱动必须实现**。
- 未实现 `SchemaProvider` 的驱动,前端退化为原始 JSON 编辑——能用,但体验差,建议实现。

---

## 3. 数据模型

### 3.1 Connection / Device / Point

| 模型 | 语义 | 协议驱动如何消费 |
|---|---|---|
| `Connection{ID, Name, Driver, Config}` | 传输层连接(怎么到达总线),可被多设备共享 | `Open.ConnConfig` 解析 |
| `Device{ID, Name, ConnectionID, Params, Points, IntervalMs, Enabled}` | 一个物理/逻辑设备,引用一个连接 | `Open.DeviceParams` 解析 |
| `Point{Name, Address, DataType, Scale, Processing}` | 单个采集点(采什么、怎么采) | `Read` 按 `Address`+`DataType` 读写 |

### 3.2 DataType

`model.DataType`:`bool / int16 / uint16 / int32 / uint32 / float32 / float64 / int64 / string`。

- 驱动只需支持其协议能表达的子集;`Read` 对不支持的类型返回该点 `QualityUncertain`,并可在 `Write` 中标记 `Ok=false`。
- 参考映射(modbus 系):bool/int16/uint16/int32/uint32/float32(多寄存器 32 位按设备级 `byteOrder` 组合);float64/int64/string 通常不支持。

### 3.3 DataPoint / Quality

```go
type DataPoint struct {
    DeviceID  string      `json:"deviceId"`
    Point     string      `json:"point"`     // 点位名(来自 Point.Name)
    Value     interface{} `json:"value"`
    Timestamp time.Time   `json:"timestamp"`
    Quality   Quality     `json:"quality"`   // good | bad | uncertain
}
```

- **值类型**:数值应统一为 `float64`(北向契约、缩放、死区都按 float64 处理);bool 用 `bool`;字符串用 `string`。
- **Quality 语义**:`good`=读到有效值;`bad`=该点未读到(地址非法/请求失败);`uncertain`=协议级不确定(如解码异常)。
- **缩放**:`Point.Scale` 是乘性系数。驱动读回原始值时应用 `value * Scale`(bool 不缩放);
  `Write` 时反向:`rawValue = value / Scale`。参考 `modbus.applyScale` / `encodeWriteValue`。

---

## 4. 分步实现:一个最小轮询驱动

以"新增协议 X"为例,按下列顺序实现即可被网关完整使用。

### 4.1 包布局与注册

```
internal/driver/xyz/
├── xyz.go        # 驱动实现
├── xyz_test.go   # 单元测试(桩式)
└── xyz_e2e_test.go  # 端到端(可选)
```

```go
package xyz

func init() {
    driver.Register("xyz", &xyzDriver{pool: make(map[string]*sharedConn)})
}
```

### 4.2 配置表单(SchemaProvider)

先声明配置结构,让 Web UI 能渲染表单(连接页 + 设备页):

```go
func (*xyzDriver) ConfigSchema() []driver.Field {
    return []driver.Field{
        {Name: "address", Label: "地址", Type: driver.FieldString, Required: true, Placeholder: "192.168.1.5:502"},
        {Name: "timeout", Label: "请求超时", Type: driver.FieldString, Default: "1s"},
        {Name: "username", Label: "用户名", Type: driver.FieldString},
        {Name: "password", Label: "密码", Type: driver.FieldString},
    }
}

func (*xyzDriver) ParamSchema() []driver.Field {
    return []driver.Field{
        {Name: "slaveId", Label: "从机地址", Type: driver.FieldInt, Default: 1, Hint: "0-255"},
    }
}
```

字段控件类型:`string / int / number / bool / enum(配 Options) / json(复杂嵌套)`;`ShowWhen` 支持按另一字段值动态显隐(如 opcua 按 `securityMode` 显示证书项)。

> **凭据字段自动加密**:把凭据字段命名为 `password / passwd / token / secret / apiKey`(不区分大小写),
> API 层会按命名约定自动识别为敏感字段——落库加密存储、返回时掩码、修改留空继承旧值
> (见 `internal/api/crypto.go` 的 `isSensitiveField`/`driverSensitiveFields`),驱动实现无需自管。

### 4.3 配置解析

定义传输/设备参数结构体,从 `json.RawMessage` 解析,字段给默认值:

```go
type connConfig struct {
    Address  string `json:"address"`
    Timeout  string `json:"timeout"`
    Username string `json:"username"`
    Password string `json:"password"`
}

func parseConnConfig(raw json.RawMessage) (connConfig, error) {
    cfg := connConfig{Timeout: "1s"} // 默认值
    if len(raw) > 0 {
        if err := json.Unmarshal(raw, &cfg); err != nil {
            return connConfig{}, fmt.Errorf("parse xyz connection: %w", err)
        }
    }
    if cfg.Address == "" {
        return connConfig{}, errors.New("xyz connection address is required")
    }
    return cfg, nil
}
```

> 配置解析错误应在 `Open` 时立即返回,让调度器把设备标记离线并记录原因,而不是在 `Read` 里反复失败。

### 4.4 Open:共享连接池 + 引用计数

同一 `ConnectionID` 的多个设备共享底层连接。标准做法:

```go
type xyzDriver struct {
    mu   sync.Mutex
    pool map[string]*sharedConn // ConnectionID -> 共享连接
}

type sharedConn struct {
    connectionID string
    conn         *net.Conn   // 或协议客户端
    mu           sync.Mutex  // 串行化请求(半双工总线不允许帧交错)
    refCount     int
}

func (d *xyzDriver) Open(_ context.Context, req driver.OpenRequest) (driver.Conn, error) {
    cfg, err := parseConnConfig(req.ConnConfig)
    if err != nil {
        return nil, err
    }
    params, err := parseDeviceParams(req.DeviceParams)
    if err != nil {
        return nil, err
    }
    shared, err := d.acquire(req.ConnectionID, cfg)
    if err != nil {
        return nil, err
    }
    return &xyzConn{
        deviceID: req.DeviceID,
        shared:   shared,
        slaveID:  params.SlaveID,
    }, nil
}

func (d *xyzDriver) acquire(connectionID string, cfg connConfig) (*sharedConn, error) {
    d.mu.Lock()
    defer d.mu.Unlock()
    if shared, ok := d.pool[connectionID]; ok {
        shared.refCount++
        return shared, nil
    }
    conn, err := dial(cfg) // 建立物理连接
    if err != nil {
        return nil, err
    }
    shared := &sharedConn{connectionID: connectionID, conn: conn, refCount: 1}
    d.pool[connectionID] = shared
    return shared, nil
}

func (d *xyzDriver) release(shared *sharedConn) error {
    d.mu.Lock()
    defer d.mu.Unlock()
    shared.refCount--
    if shared.refCount > 0 {
        return nil
    }
    delete(d.pool, shared.connectionID)
    return shared.conn.Close()
}
```

### 4.5 Read:批量读取 + 质量语义

核心实现。要点:

- 先按传入 points 建好结果切片,默认 `QualityBad`(保证"未读到的点也是 bad 而非零值")。
- 单点地址解析失败 → 保持 bad,跳过不阻断。
- 同批可合并的请求尽量合并(如 modbus 把同 function 连续地址连读),减少往返。
- 逐点解码失败 → 该点 `QualityUncertain`;整批传输失败 → 返回 `error`。

```go
type xyzConn struct {
    deviceID string
    slaveID  byte
    shared   *sharedConn
}

func (c *xyzConn) Read(ctx context.Context, points []model.Point) ([]model.DataPoint, error) {
    timestamp := time.Now()
    results := make([]model.DataPoint, len(points))
    for i, p := range points {
        results[i] = model.DataPoint{DeviceID: c.deviceID, Point: p.Name, Timestamp: timestamp, Quality: model.QualityBad}
    }

    c.shared.mu.Lock() // 同连接请求串行化
    defer c.shared.mu.Unlock()

    for i, p := range points {
        addr, ok := c.parseAddress(p.Address) // 协议地址解析
        if !ok {
            continue // 解析失败:保持 bad
        }
        raw, err := c.shared.read(ctx, addr) // 真实协议读
        if err != nil {
            return nil, err // 整批传输/会话失败:整批无效
        }
        value, err := c.decode(p.DataType, raw)
        if err != nil {
            results[i].Quality = model.QualityUncertain
            continue
        }
        results[i].Value = applyScale(value, p.Scale, p.DataType)
        results[i].Quality = model.QualityGood
    }
    return results, nil
}
```

### 4.6 Close

```go
func (c *xyzConn) Close() error {
    return c.driver.release(c.shared)
}
```

### 4.7 注册进 main.go

在 `cmd/gateway/main.go` 的 driver 空导入区追加一行:

```go
_ "iot-gateway-go/internal/driver/xyz" // 注册 xyz 驱动
```

### 4.8 Web UI 与 API 接入(零改动)

- `GET /api/v1/drivers` 返回全部已注册驱动及 schema(见 `driver.List()`),前端据此渲染表单。
- 保存连接时自动做端点冲突检测(实现了 `EndpointResolver` 时)。
- 实现了 `Browser` 的驱动自动获得 `POST /api/v1/connections/{id}/browse` 节点浏览。
- 写入能力经 `POST /api/v1/devices/{id}/write` 暴露,北向下行(ThingsBoard RPC / smardaten set)复用 `core.WritePoint`。

---

## 5. 能力接口实现要点

### 5.1 Writer(写)

- 逐项处理,单点失败置 `WriteResult.Ok=false`,不阻断同批;`error` 仅整批错误。
- 值编码:工程值 → 反向缩放 → 按 `DataType` 编码为线格式;bool 写开关,数值写寄存器/属性。
- 只读区域(如 modbus input/discrete)标记 `Ok=false` 并给出原因。

### 5.2 Prober(探测)

- 取代表性点位做一次真实读往返;**协议异常码响应算"设备可达"**(证明设备在总线上,只是该地址非法),仅传输/超时算不可达(参考 `modbus.probeRead`)。
- 返回错误要含可读原因(诊断会展示给用户)。

### 5.3 Subscriber(订阅)

- `Subscribe` 注册订阅后返回 nil;数据到达时经 `onData` 推送 `DataPoint`(质量/时间戳同样要正确)。
- `ctx` 取消后必须取消订阅、释放资源(调度器热加载/进程关闭依赖它)。
- 同一 Connection 的多设备通常共享会话/订阅,按设备参数(如 NodeID 前缀)过滤投递(参考 opcua subscribe)。

### 5.4 Listener(监听)

- 网关被动 listen:首次 `Listen` 惰性 `net.Listen`,后续设备复用;帧到达按协议字段(如 UnitID)路由到设备。
- `ctx` 取消后关闭 listener 使 accept 退出;设备连接靠读超时回收,避免僵尸 goroutine(参考 `modbus_listen`)。

### 5.5 Browser(浏览)

- `parentNodeID` 空串表示根;返回 `[]NodeInfo`,其 `NodeID` 可直接写入 `Point.Address`。
- 通常复用连接池的共享 session(参考 `opcua.Browse`),用完 `release`。

### 5.6 EndpointResolver(端点防冲突)

- 返回跨驱动共享命名空间的物理端点 key(见 §2.4 注释),建议轮询驱动都实现:
  - 串口:`"serial|" + serialPort`
  - TCP/DTU:`"tcp|" + strings.ToLower(host:port)`
  - 监听:`"listen|" + normalizedBindAddr`(通配 host 归一为空,见 modbus_listen)
- 解析失败返回空串(跳过检查),不要 panic。

---

## 6. 契约与约定(务必遵守)

1. **Read 的 error 只用于整批**:配置级错误 / 整批传输失败。单点问题一律走 `Quality`。
2. **结果与输入按序对应**,且未读到的点必须填 `QualityBad`(禁止零值 `good`)。
3. **并发安全**:同 `ConnectionID` 的共享连接必须互斥访问(半双工总线不允许帧交错);驱动内部所有可变状态(池、引用计数)要加锁。
4. **ctx 贯穿**:所有 IO 方法接收 `ctx`,超时/取消要可响应;订阅/监听在 `ctx` 取消后必须释放资源。
5. **Open 不做重 IO**(轮询驱动):建连在 `acquire` 或首次读取时进行,让配置保存/热加载不受网络影响。
6. **默认值兜底**:配置解析给默认值(timeout 等),空配置可解析成可用默认。
7. **日志用 `log/slog`**:错误含设备/连接上下文,如 `"device", deviceID, "err", err`;避免裸 `fmt.Println`。
8. **不动 Processing**:点位边缘处理(过滤/聚合)在 pipeline 层,驱动只管采原始值并应用 `Scale`。
9. **不依赖北向包**:驱动只 import `internal/driver`、`internal/model`(及协议第三方库),不允许 import `output`/`core`(避免依赖倒置)。

---

## 7. 测试

### 7.1 单元测试(桩式,推荐)

用 stub/mock 替身协议客户端,隔离网络因素,覆盖编解码与质量语义:

- `parseAddress` / 配置解析的边界(非法地址、缺省默认值)。
- `decodeValue` 按类型/字节序解码的黄金用例。
- `applyScale` / 编码写值(含反向缩放)。
- Read 的按序对应、单点失败标 bad/uncertain、整批失败返回 error。
- 连接池引用计数:同 ConnectionID 复用、最后一个 Close 释放。

参考 `internal/driver/modbus/modbus_test.go`(桩实现 `modbus.Client`)与 `internal/driver/modbus/probe_test.go`。

### 7.2 契约测试

- Read 返回长度 == 输入点数;每个点 Quality 非零值。
- 未实现的能力不被误调用(如未实现 `Writer` 时 `WritePoint` 返回 `ErrNotWritable`)。
- `EndpointKey` 对同端点不同大小写/尾空格归一化。

### 7.3 端到端(可选但推荐)

用真实协议 server(simulator)打通 Open→Read(→Write→Probe→Subscribe/Listen)全链路。
参考 `internal/driver/opcua/write_e2e_test.go`、`security_e2e_test.go`。

### 7.4 验收用例清单

| 场景 | 期望 |
|---|---|
| 设备无点位 / 禁用 | scheduler 不采集,不 Open |
| 单点地址非法 | 该点 bad,同批其他点正常,error=nil |
| 连接断开 | Read 返回 error,scheduler 标记离线;恢复后自动重连(轮询天然自愈) |
| 同连接多设备 | 只建一条物理连接(池复用),请求串行不交错 |
| 同端点两条连接 | API 保存时拒绝(EndpointResolver 冲突) |
| 热加载点位变化 | 轮询设备原地更新,不重连(增量热加载) |
| 订阅/监听设备 | 不再定时 Read,数据经 onData 推送 |

---

## 8. 提交前检查清单

- [ ] `init()` 注册 + `main.go` 空导入,`driver.List()` 能看到驱动
- [ ] `ConfigSchema`/`ParamSchema` 齐全(字段 Label/Type/Required/Default/Hint 合理)
- [ ] `EndpointResolver` 已实现(轮询/监听驱动)
- [ ] Read 按序返回、error 仅整批、单点走 Quality、时间戳/DeviceID/Point 齐全
- [ ] 连接池引用计数正确(Close 幂等,最后一个关闭底层连接)
- [ ] 并发安全(共享连接加锁,池加锁)
- [ ] ctx 可取消;订阅/监听 ctx 取消释放资源
- [ ] 配置解析默认值兜底,非法配置返回可读错误
- [ ] 单元测试覆盖解析/解码/缩放/质量语义;`go build ./...`、`go test ./...` 通过
- [ ] `go vet ./...` 无告警
- [ ] README 与本文档的驱动列表同步

---

## 9. 参考

- 接口:`internal/driver/driver.go`
- 数据模型:`internal/model/model.go`
- 调度器(采集方式选择/增量热加载):`internal/core/scheduler.go`
- 写与探测入口:`internal/core/writer.go`
- 监听类协议设计:`docs/listener.md`
- 参考实现:`internal/driver/modbus`(轮询+写+探测+池)、`internal/driver/opcua`(轮询/订阅+写+探测+浏览)、`internal/driver/modbus_listen`(监听)
- 端点冲突检测、驱动列表 API:`internal/api/api.go`
