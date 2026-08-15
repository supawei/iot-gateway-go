# 开发路线 (ROADMAP)

## 项目定位

面向工控机/网关盒部署的、Go 实现的、插件化开源工业物联网边缘网关。南向接入工业设备,北向上送数据,中间用"设备-点位"模型屏蔽协议差异。

## 路线图总览

| 阶段 | 目标 | 状态 |
|---|---|---|
| **P1** | MVP:Modbus→MQTT 端到端链路 + REST 配置 + SQLite | ✅ 已完成 (2026-08-13) |
| **P2** | 验证扩展性:第二协议 / 第二北向输出 / 能力接口 | ✅ 已完成 (2026-08-16) |
| **P3** | 增强:边缘计算、断网补传、更多云平台、增量热加载、API 鉴权 | ⬜ 待开始 |
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
- API **未鉴权**(review 列为 #5,已推迟;迁输出配置前必须先做)

---

## P3 增强

- [ ] **API 鉴权**:保护配置与凭据(当前未鉴权,属安全欠账,独立且前置)
- [ ] **边缘计算**:规则 / 过滤 / 聚合,插入 pipeline 处理层(目前直通)
- [ ] **断网本地补传**:网络中断时缓存,恢复后补送,保证采集数据不丢(ThingsBoard/TDengine 均需)
- [ ] **云 IoT 平台对接**:阿里云 / 华为云 / AWS IoT 输出插件(ThingsBoard 已作为 P2 验证完成)
- [ ] **增量热加载**:scheduler 对设备/点位做 diff,增删改而非全量重启
- [ ] **MQTT 批量发布**:减少高频场景的发布次数
- [ ] 输出侧连接级重试与退避策略

---

## P4 产品化

- [ ] **更多协议**:Profinet / EtherCAT(工业以太网实时)、现场总线
- [ ] **Sparkplug B 支持**:工业 MQTT 事实标准(topic 命名空间已预留)
- [ ] **运维监控**:指标采集、结构化日志增强、健康检查端点
- [ ] **物模型映射层**:在设备-点位之上加云物模型(TSL)映射,对接云平台语义(草案见 [docs/tsl-mapping.md](docs/tsl-mapping.md))

---

## 暂缓清单(刻意不做,按真实需求驱动)

| 项 | 暂缓理由 | 触发条件 |
|---|---|---|
| `go plugin` / 进程外插件 | 起步无隔离需求,坑多/复杂度高 | 出现不稳定驱动需隔离或第三方插件 |
| 规则引擎 / 断网补传 | 非 MVP 核心 | P3 |
| 工业以太网(Profinet/EtherCAT)/ 现场总线 | P1/P2 聚焦 Modbus、OPC UA | P4 或有真实设备需求 |
| Sparkplug B | JSON 起步快,已留扩展点 | P4 或有互操作需求 |
| 北向输出迁移到 SQLite + Web UI | 分析结论:保持 yaml(鉴权/热重载/引导成本),见 [docs/northbound-output-config.md](docs/northbound-output-config.md) | 先做 API 鉴权再评估 |

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
| 热加载 | 全量重载 | MVP 简化,增量 diff 留 P3 |
| Read 语义 | error 表配置级错误,Quality 表数据质量 | 让北向能感知设备异常(bad/uncertain) |
| Modbus 库 | grid-x/modbus | 原生 RTU over TCP;Client/Connect 带 ctx;纯 Go 不影响交叉编译 |
| 连接实体化 | Connection 与 Device 分离 | 同串口/DTU 多从机共享传输配置不冗余;连接复用以 ConnectionID 为 key |
| OPC UA 库 | gopcua/opcua | 纯 Go 无 CGO,符合交叉编译;轮询复用 scheduler 模型;订阅经 `Subscriber` 推送 |
| 调度模型 | cron 统一调度 + worker pool | 常驻 goroutine 与设备数解耦;pool 限流保护下游;reload 全量重建(增量留 P3) |
| 可选能力接口 | `Writer`/`Subscriber`/`Listener`/`SchemaProvider`/`DeviceNotifier` 经类型断言叠加 | 核心接口(Driver/Conn/Output)保持最小,能力按需声明,不强制所有驱动/输出实现 |
| 设备写链路 | `core.WritePoint` 单点复用 | REST 写接口与 ThingsBoard 下行共用同一"查点位→开连接→Writer.Write"链路,消除重复 |
| 运行时状态 | `status.Registry` + `values.Registry`(内存态) | 可观测性信息不持久化,与 SQLite 配置分离;实时值只记"最新一帧",供界面展示 |
| ThingsBoard 对接 | MQTT Gateway 模式 | 单连接代表 N 个子设备,契合网关"集中接入多设备"定位 |
| TDengine 对接 | taosAdapter REST(强类型超级表 + 按点位建子表) | 无 CGO 保持纯 Go;值按 Go 类型落强类型列;TAGS 承载设备/点位,子表名 hash 保证合法唯一 |
| Web 前端 | Vue 3 + Element Plus,`go:embed` 内嵌 | 独立工程便于前端迭代;内嵌免 nginx,单端口部署 |
| 配置 schema | `SchemaProvider` 由驱动声明,前端动态渲染 | 替代手写 JSON,避免前端硬编码各驱动字段;`showWhen` 处理模式相关字段 |
| 北向输出配置 | 保留在 config.yaml | 未鉴权 API 暴露凭据风险、热重载需 OutputManager、yaml 永不可消(引导配置),见分析文档 |
