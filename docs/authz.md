# API 鉴权与授权设计 (AuthN & AuthZ)

> **状态**:草案
> **关联阶段**:P3(API 鉴权,列为 P3 首项且前置)
> **更新**:2026-08-16

## 1. 背景与目标

当前所有 `/api/v1/**` 接口**未鉴权**:局域网内任何人都能读改连接/设备配置、下发写值、查看运行时状态。设备配置与连接参数(含地址、从机号)暴露无门禁;若将来把北向输出配置迁入 API(见 [northbound-output-config.md](northbound-output-config.md)),云端凭据也会一并暴露。

本设计引入**鉴权(认证) + 授权(访问控制)**两层:

- **认证(AuthN)**:确认"你是谁"——管理员登录,或三方系统出示 API Key。
- **授权(AuthZ)**:确认"你能干什么"——按 **scope(权限范围)** 控制"哪个主体能调哪些接口"。

关键诉求:**后续要把某个接口授权给三方系统对接**(例如只授 `devices:read` + `values:read`,让 MES/SCADA 只读取数,不给配置权)。因此授权粒度到"接口",而非"要么全给、要么不给"。

## 2. 核心概念

| 术语 | 含义 |
|---|---|
| Principal | 主体:调用 API 的"人"或"三方系统" |
| Credential | 凭证:人的密码、三方系统的 API Key |
| Scope | 权限范围,形如 `resource:action`,如 `devices:read` |
| Role | 角色:一组预定义 scope(管理员 = 全部) |
| Token | 认证后颁发的短期访问凭证(Bearer) |

两类主体:

| 主体 | 凭证 | 授权来源 | 生命周期 |
|---|---|---|---|
| 管理员(人) | 用户名 + 密码 | 角色(全 scope) | 登录发 session token,短期 |
| 三方系统 | API Key | 绑定 scope 列表 | 长期,可吊销 |

> 反过度工程取舍:第一版**只做管理员 + 三方 client 两类主体**,不预先抽象"操作员/只读"等中间角色——等有真实需求再加。这与"暂缓清单"原则一致。

## 3. Scope 模型

Scope 命名 `资源:动作`。动作细分:

- `read` 读配置/数据
- `write` 增删改配置
- `command` 下发写值(控制设备)——比"改配置"更敏感,单列,便于授权"可控制但不可改点表"

### 3.1 Scope 清单与接口矩阵

| Scope | 覆盖接口 |
|---|---|
| `connections:read` | `GET /connections`、`GET /connections/{id}` |
| `connections:write` | `POST/PUT/DELETE /connections` |
| `devices:read` | `GET /devices`、`GET /devices/{id}` |
| `devices:write` | `POST/PUT/DELETE /devices`、`POST /devices/{id}/clone`、`POST /devices/{id}/points`、`DELETE /devices/{id}/points/{name}` |
| `devices:command` | `POST /devices/{id}/write` |
| `status:read` | `GET /status`、`GET /devices/{id}/status` |
| `values:read` | `GET /devices/{id}/values` |
| `drivers:read` | `GET /drivers` |
| `clients:read` | `GET /clients` |
| `clients:write` | `POST/PUT/DELETE /clients`(管理三方 API Key) |

### 3.2 认证相关接口(不属于任何 scope)

| 接口 | 说明 |
|---|---|
| `POST /auth/login` | 用户名密码换取 token,匿名可调;响应含 `mustChangePassword` 标志 |
| `POST /auth/logout` | 注销当前 token,需已认证 |
| `GET /auth/me` | 返回当前主体身份与 scope(含 `mustChangePassword`),需已认证 |
| `PUT /auth/password` | 修改当前用户密码(旧密码 + 新密码),需已认证;成功后清除改密标志 |

### 3.3 通配

- 管理员角色持有通配 scope `*`,匹配一切。
- `*` 仅赋予"人"的角色,三方 client 只授显式 scope,不授 `*`。

## 4. 主体与凭证

### 4.1 管理员(用户)

- 凭证:用户名 + 密码。密码只存 **bcrypt 哈希**(`golang.org/x/crypto/bcrypt`),不存明文。
- 登录 `POST /auth/login` 成功 → 颁发随机 **session token**(256-bit,`crypto/rand`),短期有效(默认 24h)。
- session 存**内存态**(带过期时间与滚动刷新):网关单实例,重启即失效,够用且避免持久化 session 的复杂度;后续需要多实例/跨重启会话再评估持久化。

### 4.2 预置管理员与首次改密

- **预置**:`store.Open` 建表后,若 `user` 表为空,则插入一个内置管理员(用户名 `admin`、默认密码 `admin123`),并置 `must_change_password=1`。预置动作发生在**数据库内**,与配置文件无关,配置里不出现任何默认凭据。
- **首次登录强制改密**:`must_change_password=1` 的用户登录成功后,除 `PUT /auth/password`、`POST /auth/logout`、`GET /auth/me` 外,其余接口一律返回 403(附可识别错误码),强制其先改密。
- 改密成功后置 `must_change_password=0`,恢复正常访问。
- 默认密码作为常量硬编码在 Go 侧(仅用于首次预置),文档与发布说明中明确提示部署后立即登录改密。

### 4.3 三方系统(client)

- 凭证:**API Key**——服务端生成 256-bit 随机 secret,以 `cgw_` 前缀 + hex 展示一次,**库中只存 SHA-256 哈希**,后续无法找回(只能重新生成)。
- 调用方以 `Authorization: Bearer <apiKey>` 直接调用,无需登录流程。
- 每个 client 绑定一个 **scope 列表**(存 JSON),创建时指定,可改可吊销(`enabled=false` 或删除)。

## 5. 存储设计(SQLite)

在现有 `store` 中新增表(与 `connection`/`device`/`point` 并列):

```sql
CREATE TABLE IF NOT EXISTS user (
    id                   TEXT PRIMARY KEY,          -- 如 "admin"
    password_hash        TEXT NOT NULL,             -- bcrypt
    must_change_password INTEGER NOT NULL DEFAULT 1,-- 首次登录强制改密标志
    enabled              INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS client (
    id            TEXT PRIMARY KEY,          -- 如 "mes-readonly"
    name          TEXT NOT NULL,
    api_key_hash  TEXT NOT NULL,             -- SHA-256(apiKey),hex
    scopes        TEXT NOT NULL DEFAULT '[]',-- JSON 数组,如 ["devices:read","values:read"]
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL
);
```

- 管理员无 role 列:第一版只有一个管理员角色(全 scope),`user.enabled` 控制是否可登录。
- `must_change_password` 由 `store.Open` 预置管理员时置 1,`PUT /auth/password` 成功后置 0(见 §4.2)。
- client 的 `api_key_hash` 保证 API Key 泄露数据库文件也不可逆推出明文。
- session 不入库(内存态,见 §4.1)。

## 6. 中间件与请求流

```
请求 → authMiddleware
          │  1. 从 Authorization: Bearer <token> 提取凭证
          │  2. 匹配:client(api_key_hash)或 session(token)
          │  3. 解析出 Principal + scope 集合
          │  4. 路由声明所需 scope,做集合包含判断
          ▼
       handler(放行/401/403)
```

- **401 Unauthorized**:无凭证 / 凭证无效 / 已吊销 / 已过期。
- **403 Forbidden**:凭证有效但缺少所需 scope。
- 路由注册改为声明式:`require("devices:read", a.listDevices)`,scope 与接口同处声明,避免集中式大表漂移。
- 认证接口(`/auth/login` 等)与 scope 校验接口走同一中间件,只是所需 scope 为"已认证"或"匿名"。

## 7. 配置

`config.yaml` 增加:

```yaml
auth:
  enabled: true            # 是否启用鉴权;默认 true
  sessionTtl: "24h"        # 管理员 session 有效期
```

- `enabled=false` 提供逃生舱:关闭鉴权以兼容旧部署/排障(与现状行为一致)。
- **管理员与初始密码不写在配置里**:默认管理员(`admin` / `admin123`)由 `store.Open` 预置进 SQLite,首次登录强制改密(见 §4.2);配置文件只承载"开关"与"会话时长",不出现任何凭据。

## 8. Web UI 联动

- 新增登录页:输入用户名密码 → `POST /auth/login` → token 存 `localStorage`。
- axios 拦截器:自动附带 `Authorization: Bearer`;收到 401 跳登录页。
- 登录响应 `mustChangePassword=true` 时,前端强制跳转**改密页**,改密成功前不进入主界面。
- 登录后 `GET /auth/me` 校验 token 有效性,供侧边栏显示登录态。

## 9. 分阶段实施

| 阶段 | 内容 |
|---|---|
| ① 后端鉴权 | user/client 表 + 预置管理员 + 登录/登出/me + 改密接口 + API Key 管理 + scope 中间件 + bcrypt |
| ② 前端登录 | 登录页 + 改密页 + axios 拦截 + 401 跳转 |
| ③ 授权管理 UI | 三方 client 的增删改查与 scope 编辑(Web UI) |

## 10. 安全考量

- 密码只存 bcrypt,API Key 只存 SHA-256 哈希,任何泄露不可逆。
- API Key 生成后仅展示一次,无法找回,只能重新生成(降低长期驻留风险)。
- 对比时用 `crypto/subtle.ConstantTimeCompare` 防时序侧信道。
- 证书/凭据不落日志;登录失败不区分"用户不存在/密码错"(防枚举)。
- 限流(登录暴力破解)留作后续;第一版靠强随机 token + 短期 session 缓解。
- 默认凭据(`admin` / `admin123`)仅存在于数据库预置阶段,且首次登录强制改密;改密前所有业务接口被 403 阻断,缩小默认口令暴露窗口。

## 11. 开放问题

- **默认开关**:`auth.enabled` 默认 true 会让现有部署升级后需先登录——需在发布说明中明确;是否默认 false 待定。
- **多管理员/角色**:第一版单管理员;多用户与"操作员/只读"角色按真实需求再加。
- **session 持久化**:内存态重启失效;若需跨重启/多实例再评估入 SQLite。
- **限流与审计日志**:登录尝试限流、操作审计,后续按需。
- **token 传输安全**:生产环境建议网关前置 TLS(反代或自签);本设计不内置 HTTPS。
- **默认口令强度**:`admin123` 是示例弱口令;首次改密强制缓解,但预置口令本身是否要更长/随机待定。

## 12. 决策记录

| 决策点 | 选择 | 理由 |
|---|---|---|
| 授权粒度 | 到接口的 scope(`resource:action`) | 满足"授权某接口给三方",而非全给/不给 |
| 框架 | 自研轻量 scope/RBAC,不引 Casbin | 网关接口面小且稳定,scope 枚举可控;避免 model/enforcer 概念负担与依赖 |
| 三方凭证 | API Key(只存 hash)+ 绑定 scope | 长生命周期、可吊销、细粒度授权;符合三方系统对接诉求 |
| 密码哈希 | bcrypt | 防爆破的慢哈希;x/crypto 属 Go 官方扩展库,引入可控 |
| session | 内存态 + TTL | 网关单实例,重启失效够用;避免持久化 session 复杂度 |
| 主体类型 | 仅管理员 + client 两类 | 反过度工程,中间角色按需再加 |
| 管理员预置 | 数据库预置 + 首次登录强制改密,不进配置文件 | 默认凭据不落 yaml(免泄露/免误改);改密状态入库可追踪;首次强制改密缩小默认口令窗口 |
