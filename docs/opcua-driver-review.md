# OPC UA 驱动实现评审

> **评审时间**:2026-08-18(评审后随问题修复补充)
> **评审对象**:`internal/driver/opcua/opcua.go`(678 行)+ `opcua_test.go`(275 行)
> **依赖**:`github.com/gopcua/opcua v0.9.1`(纯 Go,无 CGO)
> **结论**:核心采集链路(轮询/订阅/读写/探测/重连)实现**完善**,可满足基本接入;**生产化缺口**集中在安全模式、节点浏览与数据类型覆盖三处,另有若干低危语义/健壮性问题。
>
> **⚠️ 评审后修复一例**:初次评审认为 Write 正确,随后按真实服务器复现发现**写值请求未携带值内容**(DataValue 缺 `EncodingMask=DataValueValue`,gopcua 编码时不序列化 Value,服务端收到空写),已修复并加端到端回归测试,见 [2.3](#23-写入-write-已修复bug) 与 [附:测试覆盖清单](#附测试覆盖清单)。

---

## 1. 实现概览(能力矩阵)

OPC UA 驱动实现了框架定义的全部可选能力接口,是当前项目里能力最完整的南向驱动:

| 能力接口 | 实现 | 说明 |
|---|---|---|
| `driver.Driver`(Open) | ✅ | 按 `ConnectionID` 复用共享 session |
| `driver.Conn`(Read/Close) | ✅ | 批量读,单点失败用 Quality 表达 |
| `driver.Writer`(Write) | ✅ | 批量写,单点失败用 WriteResult.Ok 表达 |
| `driver.Subscriber`(Subscribe) | ✅ | 同 endpoint 多设备共享一个 gopcua 订阅,按 ClientHandle 分派 |
| `driver.Prober`(Probe) | ✅ | 真实读往返判设备可达(DC1003) |
| `driver.EndpointResolver`(EndpointKey) | ✅ | `tcp\|host:port`,默认端口 4840,跨驱动查重 |
| `driver.SchemaProvider`(ConfigSchema/ParamSchema) | ✅ | 表单动态渲染 + `showWhen` 条件显示 |
| 自动重连 | ✅ | gopcua `AutoReconnect` + 订阅/监控项重建(库内完成) |

代码结构:`opcuaDriver`(工厂+连接池) → `sharedSession`(共享 client/subscription) → `opcuaConn`(轮询/读写/探测) → `opcuaSubConn`(订阅,内嵌 opcuaConn 复用 Write/Close)。

---

## 2. 已实现能力核验

### 2.1 连接复用与生命周期(正确,已补一处竞态修复)

- `acquire`/`release` 用引用计数管理 `sharedSession`,同 endpoint 多设备共享一个 TCP 连接与会话,最后引用释放时撤销订阅并 `client.Close()`。
- 释放顺序:先 `subCancel()` 撤销订阅分发 goroutine(其 defer 里 `sub.Cancel`),**等待 `DeleteSubscriptions` 落地(`dispatchDone` 关闭)后再 `client.Close`**——既保证撤销订阅时 client 尚可用,也避免删除订阅请求晚于会话关闭到达服务端触发 gopcua server panic(详见 [3.3](#33-健壮性工程性低危))。
- 配置变更触发 scheduler 全量 reload → 所有 conn `Close` → 引用归零 → session 拆除重建,**配置更新后会话不会残留旧状态**。

### 2.2 轮询读取 Read(正确,含语义对齐)

- `planReads` 解析失败的点位跳过(保持 bad),成功的合并为一次 `ReadRequest` 批量读;`applyReadResults` 按响应逐点回填,单点 `status != OK` 或类型不匹配只影响该点 Quality,不阻断同批——完全对齐 `driver.Conn` 的约定。
- 通信层错误(整批)按约定**不作为配置级 error 返回**,全部点标记 bad 让北向感知设备异常(scheduler 侧据此判离线)。

### 2.3 写入 Write(已修复 Bug)

**评审后发现并修复的问题**:Write 构造 `WriteValue.Value` 时只给了 `&ua.DataValue{Value: variant}`,**漏设 `EncodingMask=ua.DataValueValue`**。gopcua 的 `DataValue.Encode()` 仅在 `EncodingMask & DataValueValue` 置位时才序列化 `Value` 字段(见 gopcua `ua/datatypes.go`),缺省(0)导致发出的 `WriteRequest` **不含任何值内容**——服务端收到空写,`Write` 返回 `Ok=true` 表面成功,实际未写入。这与前端"写值后读不回新值"的现象完全吻合。

修复:`internal/driver/opcua/opcua.go` 的 `Write` 提取出 `buildWriteValue`,显式设置 `EncodingMask: ua.DataValueValue`(与 gopcua 官方 write 示例一致)。

验证:
- 回归测试 `TestBuildWriteValue`(编码后 DataValue 必须含值内容、解码回读完好);
- 端到端测试 `TestWriteE2E`(gopcua 进程内 server):修复后写 `int32/float64/bool/string` 四种类型,server 侧节点值逐一核对**全部真实更新**;还原旧代码则该测试立即失败(`ns=1;i=101 = <nil>, want 42`),证明其有效防回归。

其余语义保持:单点 NodeID 解析失败/类型不匹配标记 `Ok=false`,不阻断同批;`scale` 忽略(节点值即工程值,与文档一致);轮询与订阅两种连接都经内嵌 `opcuaConn` 支持写,北向下行与 REST 写复用同链路。

### 2.4 订阅 Subscribe + 分派(正确,设计合理)

- 同 endpoint 共享一个 `gopcua.Subscription`;`ClientHandle` 跨设备全局递增分配,`targets` 表反查设备点位回调;分发 goroutine 消费通知通道,`subCtx` 取消即退出并 `sub.Cancel`。
- 单点被服务端拒绝(bad status)只记日志不登记 target,不阻断同批(语义对齐 Read)。
- 监控项支持 `samplingInterval`/`queueSize` 配置;通知缓冲 128 吸收发布突发。

### 2.5 自动重连与订阅恢复(正确,已对库源码核验)

gopcua v0.9.1 的重连状态机在断线后:

1. 重建安全通道与会话(session);
2. 优先 `TransferSubscriptions` 转移订阅,失败则 `Republish` 补发或**整订阅重建**;
3. 重建后 `recreate_monitoredItems` **恢复全部监控项**(单点失败丢弃并打印,不阻断)。

通知通道 `Notifs` 绑定在 `Subscription` 结构上,跨重建保持不变,因此驱动的 `dispatch` goroutine 无需感知重连,`notifyCh` 持续生效。**README/api 文档"断线由 gopcua 自动重连并重建订阅"的描述准确。**

### 2.6 探测 Probe(正确)

对可解析点位做一次真实读往返;能收到响应(含单点 bad status)即证明 server 可达,仅传输/会话错误判不可达——判定语义合理。

### 2.7 配置校验与 schema(正确)

`parseConnConfig` 校验:endpoint 必填、`securityMode` 仅 `none`、timeout/publishInterval 可解析、mode 枚举、订阅默认值(publishInterval=1s、queueSize=10)。`showWhen` 使订阅参数仅在 `mode=subscribe` 时显示。

---

## 3. 缺口与风险(按严重度排序)

### 3.1 功能缺口(生产化优先级高)

| 缺口 | 影响 | 说明 |
|---|---|---|
| **安全模式仅 `none`** | 无法对接启用签名/加密的服务器 | `Sign`/`SignAndEncrypt` 需证书管理,当前 `securityMode` 枚举只提供 `none`,用户名/密码(UsernameIdentityToken)已支持。多数 PLC/上位机默认 `none`,短期可接受 |
| **无节点浏览(Browse)/发现** | 点位地址只能手填 NodeID,无法从服务器浏览选取 | 生产 OPC UA 网关必备;需引入 Browse 服务 + Web UI 选择器,属较大功能项 |
| **无方法调用(Method)** | 无法调用服务器方法(如复位/校零) | 需要 `driver` 增加方法调用能力或专用下行接口 |
| **无历史数据(HistoryRead)** | 无法读取服务器历史库 | 多数场景可后置 |

**数据类型覆盖不足**:`decodeValue`/`encodeValue` 只处理标量(`bool/string/整数/float/double`),不支持数组/矩阵、`DateTime`、`ByteString`、`Enumeration`、结构体等。遇到上述类型时:数组会落入 `default` 分支(见 3.2),其余标量之外的类型无法表达。OPC UA 服务器变量多为标量,普通接入够用。

### 3.2 语义/一致性问题(低危)

1. **时间戳来源不一致**:订阅模式 `notificationToDataPoint` 取服务端 `SourceTimestamp`(缺失时退回 `ServerTimestamp`/本地时间),而轮询模式 `Read` 一律用本地 `time.Now()`。同一设备切换模式后时间戳语义会变,北向按时间戳做时序判断时需注意。
2. **~~Read 通信失败吞错误~~(已修复)**:原 `client.Read` 出错仅 `slog.Error`、整批返回 bad,离线原因退化为 `"all points bad"`。现整批传输/会话失败直接返回带原因的 error(如 `opcua read: ...`),scheduler 以真实错误标记离线;服务端可达时的单点错误仍走逐点 Quality。同步更新 `driver.Conn` 契约注释(整批错误含传输/会话失败)。注:modbus 仍为 all-bad + nil error,可后续对齐。
3. **`encodeValue` 经 `toFloat64` 转 `int64`**:超过 2^53 的 int64 写入会丢精度(OPC UA int64 常规场景罕见,低危)。
4. **~~`decodeValue` 未知 dataType 分支~~(已修复)**:原 `default: return raw, true` 在未知类型 + nil 值时会产生 `Quality=good` + `Value=nil` 的异常组合。已改为未知类型一律 `ok=false`(uncertain),并加单测(`TestDecodeValue`)。
5. **~~Monitor 结果长度不匹配静默~~(已修复)**:原 `if row < len(resp.Results)` 对缺失结果行静默跳过;已改为缺失行显式 `slog.Warn`。
6. **订阅失效(部分修复)**:已把 `StatusChangeNotification` 透出为 `slog.Warn`(订阅丢失/重建失败可见);"重建失败时设备转为离线"需驱动→scheduler 的订阅级状态通道,仍待做(低优先)。

### 3.3 健壮性/工程性(低危)

1. **~~release 顺序竞态~~(已修复)**:原 `release` 里 `subCancel()` 后不等分发 goroutine 的 `sub.Cancel`(DeleteSubscriptions)落地就 `client.Close`(CloseSession),删除订阅请求可能晚于会话关闭到达服务端——真实 server 下会触发 gopcua server 对已关闭会话的 nil 解引用 panic。已新增 `dispatchDone` 通道,release 在关闭 client 前等待订阅删除落地。`TestSubscribeE2E` 覆盖该路径(修复前收尾即 panic)。
2. **`stateCh` 满 + 取消后 `Close` 可能阻塞**:`monitorConnState` 在 `ctx.Done` 即退出且不再消费 `stateCh`(缓冲 8);若此刻缓冲恰好满,`release → client.Close → setState(Closed)` 会因发送阻塞(传入 `context.Background`)而卡住。概率极低(需取消瞬间恰逢状态突发),建议 stateCh 缓冲加大或 monitor 退出前排空。
3. **订阅通知背压**:gopcua `notify` 在缓冲满时阻塞其 publish 循环;驱动用 128 缓冲吸收,但若北向输出慢导致 `emit` 阻塞 → `dispatch` 阻塞 → 缓冲满,会一路背压到客户端 publish 处理(注释已说明,属取舍)。
4. **无 per-device 退订**:`driver.Subscriber` 无 Unsubscribe,点位/设备删除的清理依赖 reload 全量拆除 session(当前 scheduler 每次配置变更都全量 reload,实际可自愈;仅热更改为增量时才会暴露)。
5. **测试缺口(已补齐)**:原测试无真实 server 集成往返测试。现基于 gopcua 进程内 server(包级共享单例摊薄 ~13s 标准 nodeset 导入成本)补齐:**`TestWriteE2E`、`TestReadE2E`、`TestProbeE2E`、`TestSubscribeE2E`**,覆盖读写/探测/订阅推送四类端到端往返,均通过。

---

## 4. 文档一致性核对

对 README.md 与 docs/api.md 中 OPC UA 相关描述逐条核对,均与代码一致:

| 文档声明 | 核验结果 |
|---|---|
| 数据类型 `bool/int16/uint16/int32/uint32/int64/float32/float64/string`,scale 非零缩放为 float64 | ✅ 与 `decodeValue` 一致 |
| 写忽略 scale(节点值即工程值) | ✅ `encodeValue` 不应用 scale |
| 断线由 gopcua 自动重连并重建订阅 | ✅ 已核验库内 republish/recreate 逻辑 |
| 单点被拒绝只记日志不阻断同批 | ✅ Monitor 逐项 status 处理 |
| `s=Foo` 可省略 `s=`(ns=0 的 string node) | ✅ gopcua 默认分支将裸字符串解析为 ns=0 string node |

> **补充提示**(未写入现有文档):NodeID 裸字符串一律按 **string node** 解析,即手填 `1234`(不带 `i=`)会被当作 string 节点而非数值节点,`i=1234` 才是数值节点。建议在 README 点位地址处补充一句,避免误配。

---

## 5. 结论与建议

**结论**:OPC UA 驱动实现质量较高——接口能力完整、语义对齐框架约定、连接/订阅生命周期与自动重连处理正确,单元测试通过、`go vet`/`go build` 干净。**"实现是否完善"的答案是:核心链路完善,可投入实际接入;距离"产品化完善"还差安全模式、节点浏览与数据类型覆盖三大项。** 评审后按真实 server 复现并修复了写值空请求 Bug(见 [2.3](#23-写入-write-已修复bug)),其余建议按下方优先级推进。

**建议优先级**:

| 优先级 | 事项 |
|---|---|
| ~~P0~~(已完成) | ① `decodeValue` 未知类型 `ok=false` ✅;② Monitor 结果缺失补日志 ✅;③ README NodeID 裸字符串陷阱 ✅ |
| ~~可观测性~~(已完成) | Read 整批失败透出真实错误原因 ✅;订阅 `StatusChangeNotification` 透出日志 ✅ |
| ~~节点浏览/发现~~(已完成) | `driver.Browser` 能力 + opcua Browse(层次引用,懒加载)+ `POST /api/v1/connections/{id}/browse` + Web UI 点位"浏览选择"节点树 ✅ |
| P1(生产化前) | ① 安全模式 `Sign`/`SignAndEncrypt` + 证书配置 |
| P2(按需) | 方法调用、历史数据、数组/扩展类型、订阅失效→设备离线联动、订阅背压、stateCh 健壮性、modbus 错误透出对齐、浏览返回变量 DataType 提示 |

> 集成测试(原 P2)已补齐:基于 gopcua 进程内 server 的 `TestReadE2E`/`TestProbeE2E`/`TestSubscribeE2E`/`TestBrowseE2E` 均已通过。

---

## 附:测试覆盖清单

`go test ./internal/driver/opcua/ -v` 全部通过(共享单例 server 启动约 13s 一次,包内测试总计约 1s):

- `TestParseConnConfig` / `TestParseConnConfigDefaults` / `TestParseConnConfigSubscribe` / `TestParseConnConfigSubscribeErrors`
- `TestParseNodeIDAddress`
- `TestDecodeValue` / `TestEncodeValue`
- `TestBuildWriteValue`(写值回归:DataValue 编码必须携带值内容,防 `EncodingMask` 缺失复发)
- `TestHandleNotification`(订阅通知分派:DataChange 回调/未登记 handle 忽略/状态变更与错误通知不 panic)
- `TestBuildMonitoredItems`
- `TestNotificationToDataPoint`
- `TestConnectionPoolAcquireRelease`
- `TestEndpointKey`
- `TestWriteE2E`(写 int32/float64/bool/string 并核对 server 节点值)
- `TestReadE2E`(写已知值→批量读回,含无效地址点位保持 bad 不阻断同批)
- `TestProbeE2E`(可达/无可解析点位/未监听端口不可达)
- `TestSubscribeE2E`(订阅后从另一连接写值,onData 推送目标值;覆盖 release 顺序修复)
- `TestBrowseE2E`(从命名空间 Objects 浏览取到挂接的变量子节点,核对 NodeID/名称;根浏览不报错)

> 另含 `internal/api` 的 `TestBrowseConnection`(浏览端点:正常返回/驱动不支持 501/连接不存在 404/浏览失败 502)。
