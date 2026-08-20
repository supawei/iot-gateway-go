# hack/e2e — 真实实例 E2E 验证脚本(A1)

对应 [docs/e2e-verification.md](../../docs/e2e-verification.md)。验证"网关 → 真实平台"的输出链路。

## 脚本

| 脚本 | 验证 | 关键环境变量 |
|---|---|---|
| `verify_thingsboard.sh` | TB 上送/连接/下行(下行人工触发) | `GW` `TB_URL` `TB_MQTT` `TB_ACCESS_TOKEN` `TB_REST_USER/PASS`(可选) `DEVICE_ID` |
| `verify_tdengine.sh` | TDengine taosAdapter 写入 | `GW` `TD_URL` `TD_USER/PASS` `TD_DB/STABLE` `DEVICE_ID` |
| `verify_armv7.sh` | ARMv7 板卡启动/采集/内存 | `GW_BIN` `GW_CFG`(在板卡上跑) |
| `lib.sh` | 公共:鉴权/REST/断言/等待 | `GW` `GW_TOKEN` `E2E_WAIT` |

## 用法

```bash
# 前置:测试网关已在跑(auth.enabled: false,与压测一致;或设 GW_TOKEN),
#       且至少配了一台有真实采集的设备(DEVICE_ID,默认 d1)。
# ⚠️ 鉴权:网关登录需 RSA 加密口令,脚本不支持明文登录——
#    推荐用 auth.enabled: false 的测试网关(自动直通);
#    生产网关(鉴权开启)请设 GW_TOKEN=Web UI 登录后取得的 Bearer。

# TDengine
TD_URL=http://127.0.0.1:6041 bash hack/e2e/verify_tdengine.sh

# ThingsBoard(下行需在 TB UI 人工触发)
TB_URL=http://tb.example.com TB_ACCESS_TOKEN=<token> \
  TB_REST_USER=admin TB_REST_PASS=admin bash hack/e2e/verify_thingsboard.sh

# ARMv7(在板卡上)
bash hack/e2e/verify_armv7.sh
```

退出码:0 = 全部断言通过;非 0 = 有失败项。

## 说明

- OPC UA 安全(sign/signAndEncrypt)与 Sparkplug B 消费端**不在此目录**,见 [docs/e2e-verification.md §6/§8](../../docs/e2e-verification.md)(分别用既有 Go E2E 测试与 `mosquitto_sub` 订阅 `spBv1.0/#`)。
- 脚本会创建 `out-td`/`out-tb` 测试输出并在结尾清理;设备侧请用你已有的真实设备(或 `hack/scalebench` 起的模拟从站)。
