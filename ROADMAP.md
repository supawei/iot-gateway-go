# 开发路线 (ROADMAP)

## 项目定位

面向工控机/网关盒部署的、Go 实现的、插件化开源工业物联网边缘网关。南向接入工业设备,北向上送数据,中间用"设备-点位"模型屏蔽协议差异。

## 路线图总览

| 阶段 | 目标 | 状态 |
|---|---|---|
| **P1** | MVP:Modbus→MQTT 端到端链路 + REST 配置 + SQLite | ✅ 已完成 (2026-08-13) |
| **P2** | 验证扩展性:第二协议 / 第二北向输出 / 能力接口 | ✅ 已完成 (2026-08-16) |
| **P3** | 增强:边缘计算、断网补传、更多云平台、增量热加载、API 鉴权 | 🔶 进行中(仅剩云平台对接;边缘计算/输出重试退避已完成) |
| **P4** | 产品化:更多协议、Sparkplug B、运维监控、物模型映射 | ⬜ 待开始 |

---

## P1 MVP(已完成)

### 已实现模块

- **model** — Device / Point / DataPoint 数据模型,设备-点位抽象
- **driver** — Driver/Conn 接口 + registry + Modbus RTU/TCP/RTU over TCP 实现
- **output** — Output 接口 + MQTT 输出
- **core** — scheduler 周期采集 + pipeline 分发 + 配置热加载
- **store** — SQLite 持久化,设备/点位 CRUD + OnChange 通知
- **api** — REST 配置 API(标准库 net/http)
- **config** — YAML 配置 + 默认值
- **测试** — 协议解析、SQLite CRUD、管道分发、调度采集

### 验收(2026-08-13 实采通过)

- [x] 端到端联调:mosquitto + Modbus 模拟器跑通真实链路
- [x] 热加载验证:API 改配置后采集自动重载
- [x] 优雅关闭验证:SIGINT/SIGTERM 后连接与 output 正确释放

### P1 已知简化(留待后续优化)

- 热加载为**全量重载**,非增量 diff
- MQTT 输出**单条发布**,无批量
- 输出侧无连接级重试/退避(依赖 paho 自动重连;南向驱动自动重连已做)

---

## P2 验证扩展性(已完成)

目标:加第二协议、第二北向输出,并补齐能力接口,验证"加一个子包即可、Core/北向零改动"的插件化承诺。

### 第二协议与采集方式

- [x] 第二协议:OPC UA(2026-08-13),`internal/driver/opcua`(轮询读取,gopcua 库)
- [x] OPC UA 订阅(Subscription)推送:`driver.Subscriber` 能力,同 endpoint 多设备共享订阅
- [x] 监听类协议:`driver.Listener` 能力 + `internal/driver/modbus_listen` 参考驱动(设备主动连网关上送)

### 第二/第三北向输出

- [x] ThingsBoard 输出(2026-08-15):MQTT Gateway 模式,上送 + 下行(共享属性 / RPC → 设备写)
- [x] TDengine 输出(2026-08-16):taosAdapter REST 写入,时序库持久化

### 能力接口与框架演进

- [x] 写接口:`driver.Writer` + `core.WritePoint`,REST 写入口 `POST /devices/{id}/write`;下行与 REST 写复用同一链路
- [x] 设备生命周期通知:`output.DeviceNotifier`(上线/离线事件 → ThingsBoard connect/disconnect)
- [x] 设备运行时状态:`status.Registry`(在线/离线/最近采集/最近错误)+ REST 查询
- [x] 设备实时值:`values.Registry`(各点位最新值)+ REST 查询 + Web UI 查看
- [x] 驱动声明配置 schema:`driver.SchemaProvider` + 条件显示(`showWhen`),Web 表单动态渲染
- [x] 调度模型:改为 cron 统一调度 + worker pool,并行打开设备连接
- [x] 日志:slog 结构化输出 + lumberjack 文件轮转
- [x] 落地框架 review 修复(背压隔离 / 设备状态 / 并行打开 / 离线可见性)

### Web 管理界面(提前完成)

- [x] Vue 3 + Element Plus 独立工程(概览 / 连接 / 设备管理:点位、克隆、写值、属性值)
- [x] 前端产物经 `go:embed` 内嵌进二进制,单端口提供界面与 API,无需 nginx

**退出标准**:新增协议(OPC UA)、监听驱动、三个北向输出(MQTT/ThingsBoard/TDengine)均在不改动 Core 的前提下接入并通过测试 —— 已达成。

**P2 已知简化**:

- ThingsBoard / TDengine **尚未对真实实例验证**(需 broker / taosAdapter)
- ThingsBoard 断网本地补传、deviceName 映射未做(归 P3)

---

## P3 增强

- [x] **API 鉴权**:scope/RBAC + 三方 API Key + 强制改密(设计见 [docs/authz.md](docs/authz.md),2026-08-16)
- [x] **北向输出迁移 SQLite + Web UI**:输出注册表 + OutputManager 热重载(见 [docs/northbound-output-config.md](docs/northbound-output-config.md),2026-08-17)
- [x] **断网本地补传**:网络中断/上送失败/缓冲满时数据持久化到 SQLite,恢复后按序重放,保证数据不丢(ThingsBoard/TDengine/smardaten/MQTT 全覆盖,设计见 [docs/offline-backfill-design.md](docs/offline-backfill-design.md),2026-08-18)
- [x] **增量热加载**:配置变更时 scheduler 对设备/点位 diff,只增删改受影响部分;未变设备连接与采集零打扰,轮询点位/间隔原地更新,订阅监听组增删整组重开(设计见 [docs/incremental-hot-reload-design.md](docs/incremental-hot-reload-design.md),2026-08-18)
- [x] **MQTT 批量发布**:默认即时单条(向后兼容);配置 `flushInterval` 启用批量——同设备窗口内多点聚合为一条(数组 payload),`batchMax` 拆条,减少高频发布次数;并已接入断网补传(失败落库恢复重放),数据完整性闭环(设计见 [docs/mqtt-batch-publish-design.md](docs/mqtt-batch-publish-design.md),2026-08-18)
- [x] **边缘计算**:过滤(死区/阈值/质量)+ 时间窗口聚合,插入 pipeline 处理层;配置随设备点位存 SQLite + Web UI 编辑 + 增量热重载;派生点走完整输出链路(设计见 [docs/edge-computing-design.md](docs/edge-computing-design.md),2026-08-18)
- [ ] **云 IoT 平台对接**:阿里云 / 华为云 / AWS IoT 输出插件(ThingsBoard 已作为 P2 验证完成;smardaten 已接入,见 [docs/smardaten-iot.md](docs/smardaten-iot.md))
- [x] **输出侧连接级重试与退避**:MQTT 类输出(mqtt/thingsboard/smardaten)非阻塞建连 + ConnectRetry 指数退避 + 发布有界等待 + 构建失败隔离(设计见 [docs/mqtt-resilience-design.md](docs/mqtt-resilience-design.md),2026-08-18)

---

## P4 产品化

- [ ] **更多协议**:Profinet / EtherCAT(工业以太网实时)、现场总线
- [x] **Sparkplug B 支持**:工业 MQTT 事实标准——网关作为边缘节点发布 STATE/NBIRTH/DBIRTH/DDATA/DDEATH/NDEATH,手写 protobuf 编码 + 别名压缩,复用 mqttutil 韧性,接入设备上下线通知与断网补传(设计见 [docs/sparkplugb.md](docs/sparkplugb.md),2026-08-18)
- [ ] **运维监控**:指标采集、结构化日志增强、健康检查端点
- [ ] **物模型映射层**:在设备-点位之上加云物模型(TSL)映射,对接云平台语义(草案见 [docs/tsl-mapping.md](docs/tsl-mapping.md))

---

## 暂缓清单(刻意不做,按真实需求驱动)

| 项 | 暂缓理由 | 触发条件 |
|---|---|---|
| `go plugin` / 进程外插件 | 起步无隔离需求,坑多/复杂度高 | 出现不稳定驱动需隔离或第三方插件 |
| 规则引擎 | 非 MVP 核心,复杂度高 | P3 或真实需求驱动(断网补传已 P3 完成) |
| 工业以太网(Profinet/EtherCAT)/ 现场总线 | P1/P2 聚焦 Modbus、OPC UA | P4 或有真实设备需求 |
| Sparkplug B 下行(NCMD/DCMD) | 出生/数据/死亡已完成(2026-08-18),下行暂缓 | 有 host 下发指令的真实需求 |

---

## 架构决策记录

| 决策点 | 选择 | 理由 |
|---|---|---|
| 数据模型 | 设备-点位 | 最小够用,屏蔽协议差异;TSL / EdgeX 模型对 MVP 过重 |
| 插件机制 | `interface` + `init()` 注册 | 编译期类型安全,零运行时负担;`go plugin` 限 Linux 且坑多 |
| MQTT 格式 | 自定义 JSON + 分层 topic | 起步快,topic 命名空间预留 Sparkplug B |
| SQLite 驱动 | `modernc.org/sqlite` | 纯 Go 无 CGO,工控机交叉编译友好 |
| REST 框架 | 标准库 `net/http` | 反过度工程,Go 1.25 ServeMux 够用 |
| output registry | 去掉,main 直接构造 | 网关级单例无需注册表,更克制 |
| 热加载 | 全量重载 | MVP 简化,增量 diff 已于 P3 完成(2026-08-18) |
| Read 语义 | error 表配置级错误,Quality 表数据质量 | 让北向能感知设备异常(bad/uncertain) |
| Modbus 库 | grid-x/modbus | 原生 RTU over TCP;Client/Connect 带 ctx;纯 Go 不影响交叉编译 |
| 连接实体化 | Connection 与 Device 分离 | 同串口/DTU 多从机共享传输配置不冗余;连接复用以 ConnectionID 为 key |
| OPC UA 库 | gopcua/opcua | 纯 Go 无 CGO,符合交叉编译;轮询复用 scheduler 模型;订阅经 `Subscriber` 推送 |
| 调度模型 | cron 统一调度 + worker pool | 常驻 goroutine 与设备数解耦;pool 限流保护下游;reload 增量 reconcile(2026-08-18) |
| 可选能力接口 | `Writer`/`Subscriber`/`Listener`/`SchemaProvider`/`DeviceNotifier` 经类型断言叠加 | 核心接口(Driver/Conn/Output)保持最小,能力按需声明,不强制所有驱动/输出实现 |
| 设备写链路 | `core.WritePoint` 单点复用 | REST 写接口与 ThingsBoard 下行共用同一"查点位→开连接→Writer.Write"链路,消除重复 |
| 运行时状态 | `status.Registry` + `values.Registry`(内存态) | 可观测性信息不持久化,与 SQLite 配置分离;实时值只记"最新一帧",供界面展示 |
| ThingsBoard 对接 | MQTT Gateway 模式 | 单连接代表 N 个子设备,契合网关"集中接入多设备"定位 |
| TDengine 对接 | taosAdapter REST(强类型超级表 + 按点位建子表) | 无 CGO 保持纯 Go;值按 Go 类型落强类型列;TAGS 承载设备/点位,子表名 hash 保证合法唯一 |
| Web 前端 | Vue 3 + Element Plus,`go:embed` 内嵌 | 独立工程便于前端迭代;内嵌免 nginx,单端口部署 |
| 配置 schema | `SchemaProvider` 由驱动声明,前端动态渲染 | 替代手写 JSON,避免前端硬编码各驱动字段;`showWhen` 处理模式相关字段 |
| 北向输出配置 | 迁移到 SQLite + Web UI(原"保留 yaml"结论反转) | 完成 API 鉴权与 OutputManager 后,产品化诉求(UI 免重启配置)成为主导;见分析文档 |
| 配置/数据库变更 | 开发期不做迁移,直接改结构 | 未发布无存量部署;发布后再引入版本化迁移,见 [docs/development-conventions.md](docs/development-conventions.md) |
| 网关 ID | 从 config.yaml 迁到 SQLite(默认预置,Web UI 可改) | 属运行时配置而非引导配置,与输出配置一致走数据库 + 热重载;yaml 只留引导项 |
| 边缘处理 | 插入 pipeline 处理层(`processing.Engine`),配置挂在设备点位 | 单点位过滤 + 聚合,避免通用规则引擎;派生点走完整输出链路;原始值仍进 values 实时值 |
| Sparkplug B | 手写最小 protobuf 编码器 + 独立输出插件,别名压缩 + 出生/死亡生命周期 | 避免引入 protobuf 运行时依赖;复用 mqttutil/补传/DeviceNotifier;先发布后消费(下行按需扩展) |
