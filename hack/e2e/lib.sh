#!/usr/bin/env bash
# E2E 验证公共函数库(供 hack/e2e/verify_*.sh source)
#
# 环境变量:
#   GW        网关地址,默认 http://127.0.0.1:8080
#   GW_USER   管理员用户名(鉴权开启时登录用),默认 admin
#   GW_PASS   管理员密码(默认 admin123)
#   GW_TOKEN  (可选)直接提供 Bearer token,跳过登录
#   E2E_WAIT  轮询等待秒数,默认 30
#
# 依赖:curl + python3 + openssl(RSA-OAEP-SHA256 加密)。
# 说明:网关的登录口令与输出配置敏感字段(passwd/accessToken/token/secret/apikey)
# 均要求 RSA-OAEP-SHA256 加密(base64)传输,公钥经 GET /api/v1/crypto/publicKey 下发;
# 本库用 openssl pkeyutl 实现,与 Web 前端(node-forge)及后端(rsa.DecryptOAEP)一致。
set -u

GW="${GW:-http://127.0.0.1:8080}"
API="$GW/api/v1"
GW_USER="${GW_USER:-admin}"
GW_PASS="${GW_PASS:-admin123}"
E2E_WAIT="${E2E_WAIT:-30}"

# TOKEN 为空 = 鉴权关闭(直通)或未登录。
TOKEN=""
_PUBKEY=""

# ---- 敏感字段 RSA-OAEP-SHA256 加密 ----

# get_pubkey 拉取并缓存会话 RSA 公钥(SPKI PEM)。
get_pubkey() {
  if [ -z "$_PUBKEY" ]; then
    _PUBKEY=$(curl -sS "$API/crypto/publicKey" \
      | python3 -c 'import sys,json; print(json.load(sys.stdin)["publicKey"])') || return 1
  fi
}

# encrypt_field 用 RSA-OAEP-SHA256(base64)加密一个明文;空串原样返回。
encrypt_field() {
  local plain="${1:-}"
  if [ -z "$plain" ]; then echo ""; return 0; fi
  get_pubkey || return 1
  local pemf
  pemf=$(mktemp) && trap 'rm -f "$pemf"' RETURN
  printf '%s\n' "$_PUBKEY" > "$pemf"
  local enc
  enc=$(printf '%s' "$plain" | openssl pkeyutl -encrypt -pubin -inkey "$pemf" \
    -pkeyopt rsa_padding_mode:oaep \
    -pkeyopt rsa_oaep_md:sha256 \
    -pkeyopt rsa_mgf1_md:sha256 2>/dev/null | base64 -w0)
  rm -f "$pemf" && trap - RETURN
  if [ -z "$enc" ]; then
    echo "!! RSA-OAEP 加密失败(检查 openssl)" >&2
    return 1
  fi
  echo "$enc"
}

# ---- 鉴权 ----

# gw_login:GW_TOKEN 直通 > 鉴权开启则 RSA 登录(检测强制改密)> 鉴权关闭(503)则直通。
gw_login() {
  if [ -n "${GW_TOKEN:-}" ]; then
    TOKEN="$GW_TOKEN"
    echo "  ℹ️ 使用 GW_TOKEN 直连"
    return 0
  fi
  local enc resp code must
  enc=$(encrypt_field "$GW_PASS") || return 1
  resp=$(curl -sS -w '\n%{http_code}' -X POST "$API/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"$GW_USER\",\"password\":\"$enc\"}")
  code="${resp##*$'\n'}"
  if [ "$code" = "503" ]; then
    TOKEN=""
    echo "  ℹ️ 网关鉴权关闭,直通"
    return 0
  fi
  if [ "$code" != "200" ]; then
    echo "!! 登录失败 http=$code(检查 GW_USER/GW_PASS)" >&2
    return 1
  fi
  TOKEN=$(printf '%s' "$resp" | sed '$d' | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
  must=$(printf '%s' "$resp" | sed '$d' | python3 -c 'import sys,json; print(json.load(sys.stdin).get("mustChangePassword", False))')
  if [ "$must" = "True" ]; then
    echo "!! 管理员 $GW_USER 被强制要求修改初始密码(首登改密)。" >&2
    echo "   请先在 Web UI 改掉初始密码,或设置 GW_USER/GW_PASS 为已改密的凭证后重跑。" >&2
    return 1
  fi
  echo "  ℹ️ 已登录($GW_USER)"
  return 0
}

# ---- REST ----

# gw_req METHOD PATH [BODY] —— 带鉴权访问网关 REST,输出响应体。
gw_req() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -X "$method" -H 'Content-Type: application/json')
  [ -n "$TOKEN" ] && args+=(-H "Authorization: Bearer $TOKEN")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}" "$API$path"
}

# gw_code METHOD PATH [BODY] —— 只输出 HTTP 状态码。
gw_code() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -o /dev/null -w '%{http_code}' -X "$method" -H 'Content-Type: application/json')
  [ -n "$TOKEN" ] && args+=(-H "Authorization: Bearer $TOKEN")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}" "$API$path"
}

# ---- 断言与等待 ----

PASS=0
FAIL=0
ok()  { echo "  ✅ $1"; PASS=$((PASS+1)); }
bad() { echo "  ❌ $1"; FAIL=$((FAIL+1)); }

# out_ready OUTPUT_ID [connected] —— 检查输出已构建(/outputs/status 中出现该 id);
# 第二个参数为 connected 时额外要求连接态为真。退出码 0 = 就绪。
out_ready() {
  local oid="${1:?需要 outputId}" req="${2:-}"
  gw_req GET /outputs/status | python3 -c '
import sys, json
oid, req = sys.argv[1], sys.argv[2]
d = [s for s in json.load(sys.stdin) if s.get("outputId") == oid]
if not d:
    sys.exit(1)
if req == "connected" and not d[0].get("connected"):
    sys.exit(1)
sys.exit(0)' "$oid" "$req"
}

# wait_until "命令串" —— 轮询直到命令退出码为 0(默认 E2E_WAIT 秒)。
wait_until() {
  local i
  for i in $(seq 1 "$E2E_WAIT"); do
    if eval "$1" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

# summary —— 打印汇总,失败时退出码 1。
summary() {
  echo "== 结果: PASS=$PASS FAIL=$FAIL =="
  [ "$FAIL" -eq 0 ]
}
