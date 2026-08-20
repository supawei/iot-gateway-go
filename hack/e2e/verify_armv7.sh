#!/usr/bin/env bash
# ARMv7 目标板真机验证(在板卡上执行,或 ssh <板卡> 'bash -s' < 本脚本)
# 对应 docs/armv7-compatibility-review.md §6 与 docs/e2e-verification.md §9。
#
# 用法:
#   将 dist/gateway_linux_arm_7/gateway 与 config.yaml 放到同一目录,
#   在本目录执行:
#     bash hack/e2e/verify_armv7.sh
# 可选:IMPLIES_CHECK=0 跳过资源检查直接跑(调试用)。
set -u

# 在板卡上于 gateway 二进制所在目录执行(GW_BIN/GW_CFG 可用绝对路径覆盖)。
GW_BIN="${GW_BIN:-./gateway}"
GW_CFG="${GW_CFG:-./config.yaml}"
IMPLIES_CHECK="${IMPLIES_CHECK:-1}"

echo "== ARMv7 真机验证 $(date) =="
FAIL=0
ok()  { echo "  ✅ $1"; }
bad() { echo "  ❌ $1"; FAIL=1; }

echo "[1] 架构与浮点"
arch=$(uname -m)
echo "  uname -m = $arch"
[ "$arch" = "armv7l" ] && ok "架构 armv7l" || bad "非 armv7l(当前 $arch,请确认部署目标)"

if grep -qo vfpv3 /proc/cpuinfo 2>/dev/null; then
  ok "CPU 支持 VFPv3(GOARM=7 硬浮点可用)"
else
  bad "无 vfpv3 → 需用 make ARM32_GOARM=6 build-all 重编软浮点/旧 ABI"
fi

echo "[2] 内核与 /proc"
kver=$(uname -r)
echo "  内核 $kver"
[ "$(uname -r | cut -d. -f1)" -ge 3 ] && ok "内核 >= 3.x" || bad "内核过旧"
mount | grep -q ' /proc ' && ok "/proc 已挂载(监控指标可见)" || bad "/proc 未挂载(仅监控指标缺失,不影响采集)"

echo "[3] 二进制与冒烟"
if [ ! -x "$GW_BIN" ]; then
  bad "未找到 $GW_BIN(请放置 dist/gateway_linux_arm_7/gateway)"
else
  file "$GW_BIN" | head -1
  "$GW_BIN" -version
  echo "  -- 20s 冒烟(应见 SQLite 打开 + HTTP 监听) --"
  timeout 20 "$GW_BIN" "$GW_CFG" 2>&1 | grep -iE "sqlite|schema|listening|ready" | head -5 && ok "冒烟启动正常" || bad "冒烟异常(见上方日志)"
fi

echo "[4] 内存基线(进程运行中采样 RSS)"
cat <<EOF
  提示:接入设备后,建议用压测交叉编译版测内存上限(见 docs/scale-testing.md §7):
    GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o /tmp/scalebench-arm ./hack/scalebench
    同机跑: -devices 500/1000/2000,采样 RSS/goroutines,确认 ≤ 内存预算。
EOF

echo "== 结果: ${FAIL:+FAIL}${FAIL:-PASS} =="
exit "$FAIL"
