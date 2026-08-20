#!/usr/bin/env bash
# TDengine 真实实例 E2E 验证
# 前置:1) TDengine+taosAdapter 已启动(输出创建时会同步做连通性校验,不可达返回 502);
#       2) 网关已配置至少一台有真实采集的设备(DEVICE_ID,默认 d1)。
# 环境变量:
#   GW                      网关地址(默认 http://127.0.0.1:8080)
#   TD_URL                  taosAdapter REST,如 http://127.0.0.1:6041(必填)
#   TD_USER / TD_PASS       默认 root / taosdata
#   TD_DB / TD_STABLE       默认 iot_gateway / data_points
#   DEVICE_ID               网关侧已存在的设备 ID(默认 d1)
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

: "${TD_URL:?需设置 TD_URL(taosAdapter REST,如 http://127.0.0.1:6041)}"
TD_USER="${TD_USER:-root}"
TD_PASS="${TD_PASS:-taosdata}"
TD_DB="${TD_DB:-iot_gateway}"
TD_STABLE="${TD_STABLE:-data_points}"
DEVICE_ID="${DEVICE_ID:-d1}"

echo "== TDengine 真实实例 E2E ($TD_URL) =="
gw_login

echo "[1] 创建 TDengine 输出(幂等:先删后建;password 经 RSA-OAEP 加密)"
gw_req DELETE /outputs/out-td >/dev/null 2>&1 || true
ENC_TD_PASS=$(encrypt_field "$TD_PASS") || exit 1
body="{\"id\":\"out-td\",\"name\":\"e2e-td\",\"type\":\"tdengine\",\"enabled\":true,\
\"config\":{\"url\":\"$TD_URL\",\"username\":\"$TD_USER\",\"password\":\"$ENC_TD_PASS\",\
\"database\":\"$TD_DB\",\"stable\":\"$TD_STABLE\"}}"
code=$(gw_code POST /outputs "$body")
if [ "$code" = "201" ] || [ "$code" = "200" ]; then ok "输出创建成功"; else bad "输出创建失败 http=$code: $(gw_req GET /outputs/out-td)"; fi

echo "[2] 等待输出构建(输出启动时自动建库/建表)"
wait_until "out_ready out-td" && ok "输出已构建" || bad "输出未构建(检查输出日志)"

echo "[3] 等待数据落库(输出自动建库/建表)"
td_query() { curl -sS -u "$TD_USER:$TD_PASS" -H 'Content-Type: text/plain' \
  --data-binary "$1" "$TD_URL/rest/sql"; }
if wait_until "count=\$(td_query 'SELECT count(*) FROM $TD_DB.$TD_STABLE' | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d[\"data\"][0][0])'); [ \"\$count\" -gt 0 ] 2>/dev/null"; then
  ok "超级表 $TD_DB.$TD_STABLE 已有数据"
else
  bad "等待数据超时(${E2E_WAIT}s):$TD_DB.$TD_STABLE 无数据(检查采集与输出日志)"
fi

echo "[4] 抽样最近数据"
td_query "SELECT LAST(*) FROM $TD_DB.$TD_STABLE" | python3 -m json.tool 2>/dev/null | head -30 || true

echo "[5] 清理:删除测试输出"
gw_req DELETE /outputs/out-td >/dev/null 2>&1 || true
ok "已清理输出 out-td"

summary
