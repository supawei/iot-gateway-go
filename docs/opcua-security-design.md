# OPC UA 安全模式(Sign / SignAndEncrypt)设计

> **状态**:✅ 已按本设计实施(2026-08-18,见提交 `feat(driver/opcua): 支持安全模式 Sign/SignAndEncrypt`)
> **关联**:`internal/driver/opcua`、`docs/opcua-driver-review.md`(P1-①)
> **依赖**:`github.com/gopcua/opcua v0.9.1`(已确认支持)

---

## 1. 背景与目标

当前 OPC UA 驱动 `securityMode` 仅支持 `none`,无法对接启用签名/加密的服务器(如西门子、Rockwell 等工业 OPC UA 服务器,常要求 `Basic256Sha256 + Sign` 或 `SignAndEncrypt`)。

**目标**:让 `Connection.config` 支持安全模式与安全策略,网关能对启用安全的 OPC UA 服务器完成建连、读写、订阅全链路。

**非目标(本轮不做)**:
- 不实现证书链/PKI 校验(见 §6 信任模型,用指纹校验代替)
- 不新增身份认证方式(用户名/密码已支持,与安全模式可组合)
- 不实现密钥轮换 UI(证书过期后手动更换,见 §10)

---

## 2. OPC UA 安全模型与 gopcua 能力现状

### 2.1 OPC UA 传输安全三要素

| 要素 | 说明 |
|---|---|
| **安全模式** `securityMode` | `None` / `Sign`(仅签名,防篡改)/ `SignAndEncrypt`(签名+加密,防篡改+防窃听) |
| **安全策略** `securityPolicy` | 定义具体算法套件:`Basic128Rsa15`、`Basic256`、`Basic256Sha256`(事实标准)、`Aes128Sha256RsaOaep`、`Aes256Sha256RsaPss` 等 |
| **证书** | 客户端与服务端各持一张 X.509 证书(含公私钥),握手时交换用于签名/加密 |

### 2.2 gopcua 能力核验(v0.9.1 源码)

- ✅ 客户端 option:`SecurityMode`/`SecurityModeString`、`SecurityPolicy`、`Certificate`/`CertificateFile`、`PrivateKey`/`PrivateKeyFile`、`SecurityFromEndpoint(ep, authType)`
- ✅ 端点发现:`opcua.GetEndpoints(ctx, endpoint)` 返回 `[]*EndpointDescription`(含 `ServerCertificate`、`SecurityMode`、`SecurityPolicyURI`、`SecurityLevel`、`UserIdentityTokens`)
- ✅ 服务器侧:`server.EnableSecurity(policy, mode)` + `server.Certificate`/`server.PrivateKey`,可用于集成测试(参照 gopcua `tests/go/security_conformance_test.go`)
- ⚠️ **关键**:gopcua **不做证书信任校验**——`uasc` 直接采用服务器握手时下发/端点描述里的证书(`RemoteCertificate`)用于加解密,**无 x509.Verify、无根证书池、无主机名校验**。即"安全通道加密是真实有效的",但**不具备防中间人(CA/指纹校验)能力**,需驱动层补信任锚点(见 §6)。

---

## 3. 配置模型(Connection.config 扩展)

```json
{
  "endpoint": "opc.tcp://192.168.1.5:4840",
  "securityMode": "none",
  "securityPolicy": "auto",
  "clientCertFile": "",      // 可选,自有证书路径;留空用网关自动生成
  "clientKeyFile": "",
  "serverThumbprint": "",    // 可选,服务器证书指纹(hex);设置后建连前校验
  "timeout": "5s",
  "mode": "poll"
}
```

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `securityMode` | enum | `none` | `none`/`sign`/`signAndEncrypt`;enum 选项随 schema 下发 Web UI 下拉 |
| `securityPolicy` | enum | `auto` | 仅 `securityMode != none` 时生效;`auto`=按服务器端点协商选最强匹配;可选 `Basic128Rsa15`/`Basic256`/`Basic256Sha256`/`Aes128Sha256RsaOaep`/`Aes256Sha256RsaPss` |
| `clientCertFile` / `clientKeyFile` | string | 空 | 用户自有证书(PEM/DER);**留空则网关自动生成并持久化**(见 §5) |
| `serverThumbprint` | string | 空 | 服务器证书 SHA-1 指纹(hex,不区分大小写);留空=不校验(沿用 gopcua 行为) |

**校验规则**(`parseConnConfig` 扩展):
- `securityMode` 枚举校验;`securityPolicy` 枚举校验(空/`auto` 允许)
- `securityMode == none` 时忽略 `securityPolicy`/`serverThumbprint`(不校验、不影响)
- `securityMode != none` 时若 `clientCertFile`/`clientKeyFile` 只填一个 → 报错(成对)
- `serverThumbprint` 若填:必须是 40 位 hex(SHA-1);否则报错

**ConfigSchema 变更**:`securityMode` 的 Options 从 `["none"]` 扩为 `["none","sign","signAndEncrypt"]`;新增 `securityPolicy`/`clientCertFile`/`clientKeyFile`/`serverThumbprint` 字段(`ShowWhen: securityMode in [sign, signAndEncrypt]`)。

---

## 4. 连接流程(buildClient 扩展)

```
securityMode == none ?
  ├─ 是 → 走现有逻辑(完全不变,兼容)
  └─ 否 → 安全建连:
      1. 确保客户端证书存在(加载配置路径;未配置则 ensureClientCert() 自动生成,见 §5)
      2. GetEndpoints(ctx, endpoint) 取服务器端点列表
      3. 按 securityMode + securityPolicy 匹配端点:
           policy=auto → 在同 mode 端点里选 SecurityLevel 最高
           policy=指定 → 匹配该 policy+mode 的端点
         无匹配 → 返回带原因的明确错误(含服务器可用端点摘要)
      4. 若配置 serverThumbprint:
           计算 ep.ServerCertificate 的 SHA-1 指纹(OPC UA 规范,gopcua uapolicy.Thumbprint)
           与配置比对,不一致 → 拒绝并报"服务器证书指纹不匹配(可能被篡改或配置错误)"
      5. opts = SecurityFromEndpoint(ep, authType)      // 安全模式/策略/服务器证书/用户令牌
             + Certificate(clientCert) + PrivateKey(clientKey)
             + 现有 opts(Timeout/AutoReconnect/StateChangedCh/鉴权)
      6. NewClient(ep.EndpointURL, opts...) → Connect
```

> 说明:`SecurityFromEndpoint(ep, authType)` 会同时把用户身份令牌(UserName/Anonymous)按端点声明的 policy 配置好,故**安全模式可与 username/password 组合**,无需额外处理。
> 重连:gopcua `AutoReconnect` 沿用建连时固化的安全参数,重连仍走安全通道,无需驱动干预。

---

## 5. 客户端证书管理

**自动生成**(默认路径,免用户操作):
- 首次进入安全建连且未配置 `clientCertFile` 时,在 **SQLite 同目录**(`storage.sqlitePath` 所在目录)下生成并持久化:
  - `opcua-client-cert.pem`(自签名 X.509,RSA 2048,有效期 10 年)
  - `opcua-client-key.pem`(PKCS#8 PEM)
- 证书模板(参照 gopcua `tests/python/generate_cert.go`):
  - `KeyUsage`: DigitalSignature | KeyEncipherment | DataEncipherment | ContentCommitment
  - `ExtKeyUsage`: ClientAuth(部分服务器也认 ServerAuth)
  - `ApplicationURI`: `urn:iot-gateway:<hostname>`(经 `opcua.ApplicationURI` 或证书自动识别,gopcua 从证书 URI 读取)
  - `DNSNames/IPAddresses`: 本机主机名与 IP
- 生成后**只生成一次**,复用同一文件;密钥文件权限 `0600`。

**导入自有证书**:配置 `clientCertFile`/`clientKeyFile`(绝对路径或相对网关工作目录),加载失败给出明确错误。

**安全考量**:私钥仅存网关本地文件,不经 API 传输;DB 中连接配置只存**文件路径**,不存密钥内容。

---

## 6. 信任模型(服务器证书)

因 gopcua 不做证书链校验,驱动补**指纹校验**作为信任锚点:

- **指纹校验(默认启用,当配置了 `serverThumbprint`)**:
  - 建连前经 GetEndpoints 取得服务器证书 → 计算 SHA-1 指纹(OPC UA Thumbprint 定义 = 证书 DER 的 SHA-1)
  - 与 `serverThumbprint` 比对;不匹配拒绝建连
  - 效果:防中间人/防证书被换;对自签名服务器证书是合适的信任锚
- **未配置 `serverThumbprint`**:沿用 gopcua 行为(不校验,接受服务器证书)——便于先跑通;UI 提供"获取服务器证书指纹"辅助按钮填充该字段后即自动开启校验。

**UI 辅助(可选增强)**:连接配置页对 OPC UA 增加"获取指纹"按钮 → 调 `GetEndpoints`(新增一个只读辅助端点或在 browse 端点基础上扩展)返回服务器证书指纹,一键填入 `serverThumbprint`。

---

## 7. 错误处理与诊断

| 场景 | 行为 |
|---|---|
| 证书生成/加载失败 | `Open` 返回带原因错误,设备离线原因可见(如 `opcua security: 加载客户端证书失败: ...`) |
| GetEndpoints 失败 | 返回错误(服务器不可达/不支持端点发现) |
| 无匹配端点(模式/策略) | 返回错误并附服务器可用端点摘要(helpful 诊断日志) |
| 指纹不匹配 | 拒绝建连,明确提示"服务器证书指纹不匹配,可能被中间人篡改或配置错误" |
| 安全握手失败 | 由 gopcua 返回状态码,透传到日志/离线原因 |

日志在安全模式下额外记录:`securityMode`、`securityPolicy`、`endpoint.SecurityLevel`、指纹校验结果。

---

## 8. 兼容性

- `securityMode` 缺省或 `none`:连接配置与现有行为**完全一致**,无需迁移。
- 存量连接配置不含新字段 → 解析默认 `none`/`auto`/空,不受影响。
- 订阅(poll/subscribe)、自动重连、多设备共享 session 均基于 client,安全模式只影响建连参数,不影响上层逻辑。

---

## 9. 测试方案

复用 gopcua 进程内 server(`server.EnableSecurity("Basic256Sha256", Sign/SignAndEncrypt)` + 服务器证书/私钥),客户端证书用自签生成(参照 `genSelfSignedCert`):

**集成测试(新增 `security_e2e_test.go`)**
1. `Sign + Basic256Sha256`:连接成功,Read/Write 真实往返成功
2. `SignAndEncrypt + Basic256Sha256`:同上
3. 指纹校验:配置正确 `serverThumbprint` 成功;错误指纹**拒绝建连**
4. 客户端证书自动生成:未配置路径时首次建连自动生成证书文件(断言文件存在)
5. 无匹配端点(如请求不支持的 policy)→ 明确错误
6. `none` 模式回归:现有 e2e 已覆盖,确认不受影响

**单元测试**
- `parseConnConfig` 扩展字段校验(枚举/成对/指纹 hex 格式)
- 指纹计算(SHA-1 of DER)
- 端点匹配逻辑(mode+policy → 选端)

---

## 10. 风险与边界

| 风险/边界 | 说明与对策 |
|---|---|
| **服务器拒绝未知客户端证书** | 部分服务器要求客户端证书已在其信任列表;需把生成的 `opcua-client-cert.pem` 导入服务器信任库——属部署侧操作,文档说明 |
| **证书过期** | 自动生成证书有效期 10 年;过期后删除文件重新生成或导入新证书(手动),日志提示 |
| **gopcua 安全策略支持面** | `Basic256Sha256` 为事实标准,优先支持并作为默认建议;`Aes128Sha256RsaOaep` 等取决于 gopcua `uapolicy.SupportedPolicies()`(已含),按枚举下发 |
| **自签名 vs CA 签名** | 指纹校验不区分自签/CA;若用户导入 CA 签名证书,指纹仍可校验,但不做链验证(记录为非目标) |
| **握手开销** | 加密握手较 none 更慢(毫秒级),影响可忽略 |
| **证书文件安全** | 私钥 0600;路径不入库外泄(仅存路径) |
| **GetEndpoints 引入额外网络往返** | 每次安全建连多一次端点发现;可接受(建连低频) |

---

## 11. 实施计划(评审通过后)

| 阶段 | 内容 | 验证 |
|---|---|---|
| **S1** | `ConfigSchema`/`ParamSchema` 扩展 + `parseConnConfig` 校验 + 证书自动生成 `ensureClientCert()` | 单测:配置校验、证书生成 |
| **S2** | `buildClient` 安全分支:GetEndpoints 端点匹配 + 指纹校验 + `SecurityFromEndpoint` 建连 | `security_e2e_test.go` Sign/SignAndEncrypt 读写 + 指纹分支 |
| **S3** | 文档(README/api.md 安全节、评审文档标记完成)+ 可选 UI 字段与"获取指纹"辅助 | 全量 `go test`、前端构建 |

**预计改动文件**:`internal/driver/opcua/opcua.go`、`opcua_test.go`、新增 `security_e2e_test.go`、`internal/driver/driver.go`(无需)、`docs/*`、`web/*`(可选)。
