# 真实实例 E2E 验证(A1)

> **状态**:待执行(v0.1.0 已发布;A1 是发 v1.0.0 的唯一前置,见 [ROADMAP 发布与演进路线](../ROADMAP.md))
> **关联**:[scale-testing.md](scale-testing.md)(压测)、[armv7-compatibility-review.md](armv7-compatibility-review.md)、[thingsboard.md](thingsboard.md)、[tdengine.md](tdengine.md)、[smardaten-iot.md](smardaten-iot.md)、[sparkplugb.md](sparkplugb.md)、[opcua-security-design.md](opcua-security-design.md)
> **脚本**:[hack/e2e/](../hack/e2e/)(TB / TDengine / ARMv7 可运行脚本 + 公共库)
> **目标**:把 v0.1.0 里"已实现但仅模拟器/自测验证"的对外集成,逐项在**真实平台实例**上验证通过,作为 v1.0.0 发布依据。

## 1. 目的与范围(A1 六项)

| # | 验证项 | 方式 | 交付 |
|---|---|---|---|
| 1 | ThingsBoard 上送 / 下行 / connect-disconnect | `verify_thingsboard.sh` + TB REST/UI | §4 |
| 2 | TDengine taosAdapter 写入(超级表/子表) | `verify_tdengine.sh` + taos REST | §5 |
| 3 | OPC UA 安全模式(sign / signAndEncrypt) | 既有 Go E2E 测试(进程内 gopcua server) | §6 |
| 4 | smardaten 全双工 | 平台侧观察 + 网关日志 | §7 |
| 5 | Sparkplug B 消费端(STATE/NBIRTH/DBIRTH/DDATA) | 真实 broker + `mosquitto_sub` 订阅 `spBv1.0/#` | §8 |
| 6 | ARMv7 真机(启动/采集/RSS/内存上限) | `verify_armv7.sh`(目标板运行) | §9 |

## 2. 环境准备矩阵

| 实例 | 用途 | 建议来源 | 需要账号 |
|---|---|---|---|
| ThingsBoard CE 实例 | TB 上送/下行 | 官方 docker / 云端 | 网关设备 access token、REST 管理员 |
| TDengine + taosAdapter | 时序写入 | `docker run tdengine/tdengine`(6041 端口) | root/taosdata |
| OPC UA 服务器(可加签) | 安全模式 | gopcua(测试自带) | 无(自签证书) |
| smardaten 平台 | 全双工 | 平台申请 | 平台管理员 |
| MQTT broker + Sparkplug 消费端 | Sparkplug B | mosquitto + `mosquitto_sub` | 无 |
| ARMv7 网关盒 | 真机 | 目标硬件 | root(串口/ssh) |

## 3. 通用前置

1. **测试网关**:准备一台装有 v0.1.0(或当前 develop)的网关,Web UI 配好**至少一台设备 + 点位**且有真实数据采集(本地 Modbus 模拟器亦可,`make smoke` 用的 hack/scalebench 可起模拟从站);脚本只验证"输出→真实平台"这一段。
2. **鉴权**:网关登录要求**口令 RSA 加密**,脚本不支持明文登录。**推荐用 `auth.enabled: false` 的测试网关**(与压测一致,脚本自动直通);生产网关(鉴权开启)需设 `GW_TOKEN` = Web UI 登录后取得的 Bearer token;否则脚本给出指引并退出。
3. **脚本用法**:所有 `hack/e2e/verify_*.sh` 用环境变量传参,`source lib.sh` 复用 REST/断言/等待;退出码非 0 表示有断言失败。
4. 每项验证通过后,在 §10 表格勾选并记录版本/日期/实例地址。

## 4. ThingsBoard 验证(`verify_thingsboard.sh`)

**环境变量**:`GW`、`TB_URL`(如 `http://tb.example.com`)、`TB_ACCESS_TOKEN`(网关设备 token,即 MQTT 用户名)、`TB_REST_USER`/`TB_REST_PASS`(查遥测用,可选)、`DEVICE_ID`(默认 d1)。

1. 创建 TB 输出(`type: thingsboard`,broker `tcp://<tb>:1883` + accessToken)→ `GET /api/v1/outputs/status` 应 `connected`(真实 broker 已接受 MQTT 连接,connect-disconnect 链路通)。
2. **上送**:等 10–30s,`GET /api/v1/devices/{DEVICE_ID}/values` 确认网关侧有点值;经 TB REST(登录 JWT → 查网关设备 timeseries)或 TB UI 设备详情页确认**子设备 telemetry 出现**。
3. **下行(手动)**:TB 侧向网关设备发共享属性/RPC → 观察网关日志出现 `attributes`/`rpc` 处理 → 设备被写入(日志或 `GET /api/v1/status`)。脚本给出检查点指引。
4. **断连通知**:停掉 TB 或断网 → 网关日志记重连退避;恢复后自动重连并补发缓冲数据(复用 mqttutil 韧性)。

## 5. TDengine 验证(`verify_tdengine.sh`)

**环境变量**:`GW`、`TD_URL`(如 `http://127.0.0.1:6041`)、`TD_USER`/`TD_PASS`/`TD_DB`/`TD_STABLE`(默认 root/taosdata/iot_gateway/data_points)。

1. 创建 TD 输出(`type: tdengine`)→ 输出自动 `CREATE DATABASE/STABLE/TABLE IF NOT EXISTS`。
   > ⚠️ **TDengine 输出在创建时做同步连通性校验**(`New()` 内建库建表,不可达即构建失败返回 502,与 MQTT 类输出的非阻塞建连不同):**必须先启动 TD 再创建输出**。
2. 等若干采集周期(默认 flush 1s),脚本用 taos REST `POST /rest/sql`(Basic)查:
   - `SELECT count(*) FROM <db>.<stable>` → **count > 0**;
   - `SELECT LAST(*) FROM ...` → 打印一行抽样,确认 TAGS(device/point)与值类型正确。

## 6. OPC UA 安全验证(既有测试)

```bash
go test -run 'TestOPCUASecurity|Test.*Security' ./internal/driver/opcua/ -v
```
进程内真实 gopcua server 启用 `Basic256Sha256`(Sign / SignAndEncrypt),验证证书/指纹/加签握手——**自包含,无需外部服务器**。另可手工对真机:`make build` 后配 OPC UA 连接 `security: signAndEncrypt` + `serverThumbprint`,观察上线。

## 7. smardaten 全双工验证

平台侧逐项确认(网关日志佐证):
1. **属性上报**:Web UI 配好点位后,平台设备属性页出现实时值;
2. **设备状态**:DeviceOnline/Offline → 平台状态同步;
3. **服务调用(下行)**:平台下发服务 → 网关 `core.WritePoint` 写设备 → 回执;
4. **配置下发/同步**:平台 `application.json` 变更 → 网关自动 upsert 连接/设备(平台为权威源),孤儿自动删除;
5. **设备诊断**:平台发诊断请求 → modbus/opcua 驱动 `Probe` 真读 → 回包。

详见 [smardaten-iot.md](smardaten-iot.md) §11;逐项通过后勾选 §10。

## 8. Sparkplug B 验证

```bash
# 在消费端机器订阅全部 spB topic(需安装 mosquitto-clients)
mosquitto_sub -h <broker> -p 1883 -t 'spBv1.0/#' -v
```
网关配置 `type: sparkplugb` 输出(broker/groupId/edgeNodeId)。预期观察:
- 网关启动 → `spBv1.0/<grp>/STATE/<edge>` retained `ONLINE`;
- 设备上线 → `NBIRTH/<edge>`(seq=1, node metrics)与 `DBIRTH/<edge>/<dev>`(点位 name/alias/datatype);
- 采集 → `DDATA/<edge>/<dev>` 周期出现(alias + 类型正确的编码);
- 设备下线/关闭 → `DDEATH`/`NDEATH` + `STATE OFFLINE`。
可用 `sparkplugb_test.go` 单测先行(self-check),再对真实 broker 复验。

## 9. ARMv7 真机验证(`verify_armv7.sh`,目标板运行)

```bash
# 板卡上执行(或 ssh):
bash hack/e2e/verify_armv7.sh
```
检查项(与 [armv7-compatibility-review.md](armv7-compatibility-review.md) §6 对齐):
1. `uname -m` = `armv7l`;`/proc/cpuinfo` 有 `vfpv3`(否则 `make ARM32_GOARM=6 build-all` 重编);
2. 内核 ≥ 3.2;`/proc` 已挂载;
3. 冒烟:`timeout 20 ./gateway config.yaml` → 日志见 SQLite 打开 + HTTP 监听;
4. 采集:配 1 台设备(可用 scalebench 模拟从站),确认 online + 点值;
5. **内存上限**:交叉编译 `hack/scalebench` 到板卡同机跑(见 [scale-testing.md §7](scale-testing.md)),记录 500/1000/2000 设备时的 RSS,确认 ≤ 内存预算。

## 10. 结果记录表

| # | 验证项 | 实例地址 | 结果 | 备注(版本/日期/现象) |
|---|---|---|---|---|
| 1 | ThingsBoard 上送 | | ⬜ | |
| 1b | ThingsBoard 下行(RPC/属性) | | ⬜ | |
| 1c | ThingsBoard connect-disconnect | | ⬜ | |
| 2 | TDengine 写入 | | ⬜ | |
| 3 | OPC UA sign / signAndEncrypt | | ⬜ | |
| 4 | smardaten 全双工 | | ⬜ | |
| 5 | Sparkplug B 消费端 | | ⬜ | |
| 6 | ARMv7 启动/采集 | | ⬜ | |
| 6b | ARMv7 内存上限 | | ⬜ | |

> 全部通过后:更新本文件勾选、记录版本,即可进入 v1.0.0 发布流程(ROADMAP A 完成后标记 A1 done)。
