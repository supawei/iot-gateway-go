# 开发路线 (ROADMAP)

## 项目定位

面向工控机/网关盒部署的、Go 实现的、插件化开源工业物联网边缘网关。南向接入工业设备,北向上送数据,中间用"设备-点位"模型屏蔽协议差异。

## 路线图总览

| 阶段 | 目标 | 状态 |
|---|---|---|
| **P1** | MVP:Modbus→MQTT 端到端链路 + REST 配置 + SQLite | ✅ 已完成 (2026-08-13) |
| **P2** | 验证扩展性:加第二协议,检验插件化承诺 | 🟡 进行中(OPC UA 已接入,第二北向输出待做) |
| **P3** | 增强:边缘计算、断网补传、云平台对接 | ⬜ 待开始 |
| **P4** | 产品化:Web UI、更多协议、运维监控 | ⬜ 待开始 |

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
- 无连接级重试/退避(依赖 paho 自动重连)

---

## P2 验证扩展性

目标:加第二个协议,验证"加一个子包即可、Core/北向零改动"的插件化承诺。

- [x] 选定第二协议:OPC UA(2026-08-13)
- [x] 实现 `internal/driver/opcua` 子包(轮询读取,gopcua 库)
- [x] OPC UA 订阅(Subscription)推送:连接配置 `mode:"subscribe"` 开启,driver 实现 `driver.Subscriber`,scheduler 检测能力后走推送而非轮询
- [x] 验证 scheduler / pipeline / output 接入新协议无需改动(仅 model 加 3 个类型常量,core/store/api/config 零改动)
- [ ] 加第二个北向输出(候选:时序数据库 InfluxDB / TDengine),验证 Output 接口
- [ ] 若扩展中暴露接口缺陷,回填修正 P1

**OPC UA 接入验收(2026-08-13)**:加子包 + main.go 一行 import 即接入;`make check` + `make build-all`(三平台静态)通过;同 ConnectionID 共享 session(引用计数)。实采验证待连真实 OPC UA server。

**OPC UA 订阅(Subscription)**:连接配置 `mode:"subscribe"` 开启订阅式推送;驱动实现 `driver.Subscriber` 能力,scheduler 按类型断言切换到推送采集,`intervalMs` 在订阅模式下被忽略。同一 `endpoint` 的多个设备共享一个 gopcua 订阅,按 ClientHandle 分派回各自设备。Core/北向零改动,协议差异封死在 opcua 子包内部。

**退出标准**:新增协议与输出均在不改动 Core 的前提下接入并通过测试。

---

## P3 增强

- [ ] **边缘计算**:规则 / 过滤 / 聚合,插入 pipeline 处理层(目前直通)
- [ ] **断网本地补传**:网络中断时缓存,恢复后补送,保证采集数据不丢
- [ ] **云 IoT 平台对接**:阿里云 / 华为云 / AWS IoT 输出插件
- [ ] **增量热加载**:scheduler 对设备/点位做 diff,增删改而非全量重启
- [ ] **MQTT 批量发布**:减少高频场景的发布次数
- [ ] 连接级重试与退避策略

---

## P4 产品化

- [ ] **Web 管理界面**:可视化设备/协议配置(API 已就绪,前端独立)
- [ ] **更多协议**:Profinet / EtherCAT(工业以太网实时)、现场总线
- [ ] **Sparkplug B 支持**:工业 MQTT 事实标准(topic 命名空间已预留)
- [ ] **运维监控**:指标采集、结构化日志、健康检查端点
- [ ] **物模型映射层**:在设备-点位之上加云物模型(TSL)映射,对接云平台语义

---

## 暂缓清单(刻意不做,按真实需求驱动)

| 项 | 暂缓理由 | 触发条件 |
|---|---|---|
| OPC UA / 工业以太网 / 现场总线 | P1 聚焦 Modbus | P2 起按需 |
| `go plugin` / 进程外插件 | 起步无隔离需求,坑多/复杂度高 | 出现不稳定驱动需隔离或第三方插件 |
| 规则引擎 / 断网补传 | 非 MVP 核心 | P3 |
| Web UI | API 优先,UI 后补 | P4 |
| Sparkplug B | JSON 起步快,已留扩展点 | P4 或有互操作需求 |

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
| Modbus 库 | grid-x/modbus | 原生 RTU over TCP;Client/Connect 带 ctx,阻塞读可取消;纯 Go 不影响交叉编译 |
| 连接实体化 | Connection 与 Device 分离 | 同串口/DTU 多从机共享传输配置不冗余;连接复用以 ConnectionID 为 key |
| OPC UA 库 | gopcua/opcua | 纯 Go 无 CGO,符合交叉编译;轮询 Read 复用 scheduler 模型;订阅经 `driver.Subscriber` 推送 |
| 调度模型 | cron 统一调度 + worker pool | 常驻 goroutine 与设备数解耦;pool 限流保护下游;reload 全量重建(增量留 P3) |
