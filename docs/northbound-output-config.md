# 北向输出配置:yaml vs SQLite 分析

> **状态**:已迁移(原结论"保持 yaml"已反转,前提条件满足后实施)
> **更新**:2026-08-17
>
> 本文前半部分保留当时的利弊分析;最终在完成 API 鉴权(原 §6 阶段①)后,按 §6 阶段②③推进,把北向输出配置迁入 SQLite 并经 Web UI 管理。实施细节见文末「§7 实施记录」。

## 1. 背景与现状

网关的配置分两类,本质不同:

| 配置 | 位置 | 特点 | 热加载 |
|---|---|---|---|
| 引导/bootstrap 配置(存储路径、HTTP 端口、日志) | config.yaml | 启动前就必需 | 否 |
| 运行时配置(连接/设备/点位) | SQLite | 可动态增删改 | 是(API 写入即重载) |
| **北向输出**(MQTT/TB broker、凭据) | config.yaml | 部署期一次配好 | 否 |

北向输出当前被归到"引导配置"一类,符合 ROADMAP 的初衷:"output 是网关级单例,main 直接构造"。

## 2. 支持迁移的理由(弱)

1. **一致性**:设备与输出都走 Web UI,体验统一。
2. **免重启**:改 broker 不用 ssh 到盒子改 yaml。
3. **产品化**:面向不懂 yaml 的用户,UI 配置更友好。

## 3. 反对迁移的理由(强)

### 3.1 安全:凭据会暴露在未鉴权的 API 上 🔴

MQTT 密码、ThingsBoard accessToken 目前躺在 yaml 里,**API 不会返回**。一旦迁到 SQLite 并加"列出输出"接口,这些凭据就会通过 HTTP API 暴露 —— 而当前 API **尚未鉴权**(review 中列为 #5,已推迟)。等于把网关的云端凭据变成"局域网内任何人都能读/改"。

> **结论:必须先做 API 鉴权,否则不能迁。**

### 3.2 输出热重载复杂度远高于设备热加载 🔴

设备的"热加载"是 scheduler 内部重载(停 cron、重建采集)。输出热加载要动的链路完全不同:

- 输出在 `RunPipeline` 和 `NewScheduler` 里都是**启动时一次性传入**的切片;
- 热切换输出需引入"可变输出注册表"(`OutputManager`):关闭旧连接 → 重建新输出 → **原子替换**;pipeline 按需读取、scheduler 的 `DeviceNotifier` 也要跟着换。

这是一块新架构,不是加字段那么简单。

### 3.3 引导的鸡生蛋问题 🟠

SQLite 路径本身在 yaml 里。输出放 SQLite 后,启动变成两阶段(yaml 引导 → 开库 → 读输出配置)。yaml **永远消不掉**,只是少了几行,收益有限。

### 3.4 运维/审计视角 🟠

broker 地址、凭据属于"部署环境"信息,天然适合 yaml 这类**可版本控制、可 diff、可审计、可备份**的文件;SQLite 是运行时状态,改起来不易追踪、不易回滚。

## 4. 关键判断点

核心问题:**输出配置多久变一次?**

- 一台网关盒子部署后 broker 基本不变 → yaml 是**正确且更安全**的选择,迁移是过度设计。
- 交付给最终用户、由他们自行配置云端平台的产品 → Web UI 管理有真实价值,但必须先解决鉴权 + 热重载。

## 5. 结论

**已反转:北向输出迁移到 SQLite,通过 Web UI 配置。**

原始"保持 yaml"结论基于两个前置:API 未鉴权、无输出热重载架构。这两点现已解决(鉴权见 [authz.md](authz.md),输出热重载见 §7),且"交付给最终用户、由他们自行配置云端平台"的产品化诉求成为主导,因此执行迁移。

## 6. 分阶段推进(已完成)

| 阶段 | 内容 | 状态 |
|---|---|---|
| ① API 鉴权 | 保护凭据,不迁移输出也是刚需 | ✅ 已完成 |
| ② OutputManager | 输出热重载(关闭旧连接 → 重建 → 原子替换) | ✅ 已完成 |
| ③ 迁移 SQLite + Web UI | 输出配置入库、UI 增删改 | ✅ 已完成 |

## 7. 实施记录

### 7.1 存储与模型

- `model.Output{ID, Name, Type, Config, Enabled}`:一条记录对应一个输出插件实例。
- SQLite 新增 `output` 表(`id/name/type/config/enabled`);`Type` 取值 `mqtt` / `thingsboard` / `tdengine`。

### 7.2 输出注册表与热重载(OutputManager)

- `internal/output` 新增**注册表**(`Register`/`ListTypes`/`Build`):各输出插件在 `init()` 里声明类型、配置 schema 与构造器。与南向 `driver` 的 `SchemaProvider` 同思路,前端据此动态渲染表单,新增输出插件零前端改动。
- `output.Manager` 维护活跃输出集合,提供 `Reload` / `Publish` / `Notifiers` / `Close`:
  - **扇出**:每个输出独立队列 + 发布 goroutine,输出间背压隔离(接管原 `core.RunPipeline` 的分发职责)。
  - **热重载**:构建新输出集 → 原子替换 → 关闭旧输出;构建失败保留旧输出(返回错误)。
  - **设备通知**:scheduler 经 `Manager.Notifiers()` 动态获取实现 `DeviceNotifier` 的输出,热重载后自动跟随最新输出。
- `BuildContext{GatewayID, Write, Store, LatestValues}` 由 main 注入:gatewayID 用于 topic/标识,Write 为下行写回调(落到 `core.WritePoint`),Store 供插件自动同步配置,LatestValues 为查询设备点位最新采集值的回调(基于 `values.Registry`,服务调用 get 等场景用),这些均属网关上下文而非输出自身配置,故不进注册表。

### 7.3 配置来源切换

- 北向输出配置从 config.yaml 移除,改由 Web UI 写入 SQLite;项目未对外发布,故不做旧 yaml 迁移,配置文件输出段直接删除。
- config.yaml 保留的仅剩引导配置(存储路径 / HTTP / 日志 / 鉴权开关 / 调度池大小)。

### 7.4 API 与权限

- 新增 `GET/POST/PUT/DELETE /outputs` 与 `GET /outputs/types`,受 `outputs:read` / `outputs:write` scope 保护(见 [authz.md](authz.md))。
- 写/删输出后立即触发 `Manager.Reload()`;若新配置激活失败(如 broker 连不上),配置已持久化但旧输出保持运行,接口返回 `502` 以便用户感知。
- 输出配置含云端凭据(密码/Token),故 `outputs:*` 属敏感 scope,只应授予可信主体;凭据脱敏留作后续评估。

### 7.5 反转过度的取舍

- 输出注册表重新引入(原 ROADMAP 曾"去掉 output registry,main 直接构造"):因配置源变为可变的 SQLite + 需 Web UI 动态渲染,注册表成为必需,故恢复。
- 凭据脱敏(列表/详情对敏感字段打码)未做第一版:鉴权已解决"未鉴权暴露"的原始风险,脱敏属纵深防御,待真实的多角色只读场景再加。
