#!/usr/bin/env bash
# ThingsBoard 真实实例 E2E 验证
# 前置:网关已配置至少一台有真实采集的设备(DEVICE_ID,默认 d1)。
# 环境变量:
#   GW                  网关地址(默认 http://127.0.0.1:8080)
#   TB_URL              ThingsBoard HTTP 地址,如 http://tb.example.com(必填)
#   TB_MQTT             TB MQTT broker,如 tcp://tb.example.com:1883(默认由 TB_URL 推导)
#   TB_ACCESS_TOKEN     网关设备 Access Token(MQTT 用户名)(必填)
#   TB_REST_USER/PASS   TB 管理员(查遥测用,可选;不给则只验连接+给出 UI 检查指引)
#   DEVICE_ID           网关侧已存在的设备 ID(默认 d1)
set -euo pipefail
cd "$(dirname "$0")"
source ./lib.sh

: "${TB_URL:?需设置 TB_URL(ThingsBoard HTTP,如 http://tb.example.com)}"
: "${TB_ACCESS_TOKEN:?需设置 TB_ACCESS_TOKEN(网关设备 token)}"
TB_MQTT="${TB_MQTT:-tcp://${TB_URL#*://}:1883}"
DEVICE_ID="${DEVICE_ID:-d1}"

echo "== ThingsBoard 真实实例 E2E ($TB_URL) =="
gw_login

echo "[1] 创建 ThingsBoard 输出(幂等:先删后建;accessToken 经 RSA-OAEP 加密)"
gw_req DELETE /outputs/out-tb >/dev/null 2>&1 || true
ENC_TB_TOKEN=$(encrypt_field "$TB_ACCESS_TOKEN") || exit 1
body="{\"id\":\"out-tb\",\"name\":\"e2e-tb\",\"type\":\"thingsboard\",\"enabled\":true,\
\"config\":{\"broker\":\"$TB_MQTT\",\"accessToken\":\"$ENC_TB_TOKEN\",\"deviceNamePrefix\":\"\"}}"
code=$(gw_code POST /outputs "$body")
if [ "$code" = "201" ] || [ "$code" = "200" ]; then ok "输出创建成功"; else bad "输出创建失败 http=$code: $(gw_req GET /outputs/out-tb)"; fi

echo "[2] 等待与真实 TB broker 建连(/outputs/status 报 connected)"
if wait_until "out_ready out-tb connected"; then
  ok "已连上真实 ThingsBoard broker(connect 链路通)"
else
  bad "${E2E_WAIT}s 内未连上 TB(检查 TB_URL/accessToken,看网关日志)"
fi

echo "[3] 网关侧确认点值在采集"
if wait_until "gw_req GET /devices/$DEVICE_ID/values | grep -q '\"value\"'"; then
  ok "网关设备 $DEVICE_ID 有实时值"
else
  bad "网关设备 $DEVICE_ID 无值(先确认采集配置)"
fi

echo "[4] 上送验证(TB 侧)"
if [ -n "${TB_REST_USER:-}" ] && [ -n "${TB_REST_PASS:-}" ]; then
  jwt=$(curl -sS -X POST "$TB_URL/api/auth/login" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$TB_REST_USER\",\"password\":\"$TB_REST_PASS\"}" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin).get("token",""))')
  if [ -n "$jwt" ]; then
    # 网关设备 = access token 对应设备;经 /api/customer 或设备名查询较繁琐,
    # 这里取"网关设备最近遥测"作代表性检查:先按名称查设备。
    did=$(curl -sS "$TB_URL/api/tenant/devices?pageSize=100&page=0&textSearch=" \
      -H "X-Authorization: Bearer $jwt" \
      | python3 -c "import sys,json; d=json.load(sys.stdin); print([x['id']['id'] for x in d.get('data',[])][0] if d.get('data') else '')" 2>/dev/null || true)
    if [ -n "$did" ]; then
      ts=$(curl -sS "$TB_URL/api/plugins/telemetry/DEVICE/$did/values/timeseries?keys=value" \
        -H "X-Authorization: Bearer $jwt" | head -c 200)
      if echo "$ts" | grep -q 'value'; then ok "TB 遥测 API 已见子设备数据"; else bad "TB 遥测未查到(检查 deviceNamePrefix/TB 子设备自动建档配置)"; fi
    else
      echo "  ⚠️ 未能在 TB REST 定位设备,请在 TB UI 设备详情页确认遥测"
    fi
  else
    echo "  ⚠️ TB REST 登录失败(检查 TB_REST_USER/PASS),请在 TB UI 确认遥测"
  fi
else
  echo "  ℹ️ 未提供 TB_REST_USER/PASS,请在 TB UI 设备详情页确认子设备遥测出现"
fi

echo "[5] 下行验证(需人工在 TB 侧触发)"
cat <<EOF
  ▸ 在 TB UI 向网关设备发送共享属性或 RPC → 观察网关日志出现 attributes/rpc 处理,
    设备被写入(GET /api/v1/devices/$DEVICE_ID/status 或在日志确认)。
  ▸ 断连自愈:停 TB(或断网)→ 日志出现重连退避;恢复后自动重连并补发缓冲数据。
EOF

echo "[6] 清理:删除测试输出"
gw_req DELETE /outputs/out-tb >/dev/null 2>&1 || true
ok "已清理输出 out-tb"

summary
