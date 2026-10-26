#!/bin/bash
# 冒烟测试：验证“第一条线路死掉时自动换到第二条”以及请求体透传。
#
# 用法：
#   LINKGATE_UPSTREAM=http://<host>:8090 ./scripts/smoke-test.sh
#
# 不会碰你的正式配置与正式端口（用 18765/18766）。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UPSTREAM="${LINKGATE_UPSTREAM:-}"
if [ -z "$UPSTREAM" ]; then
  echo "请设置 LINKGATE_UPSTREAM，例如：LINKGATE_UPSTREAM=http://203.0.113.10:8090 $0"
  exit 2
fi

TMP="$(mktemp -d)"
BIN="$TMP/linkgate"
CFG="$TMP/config.yaml"
trap 'kill "${PID:-0}" 2>/dev/null || true; rm -rf "$TMP"' EXIT

(cd "$ROOT" && go build -o "$BIN" ./cmd/linkgate)

cat > "$CFG" <<YAML
version: 1
data_dir: $TMP/data
admin:
  listen: 127.0.0.1:18766
log:
  level: info
  format: text
buffer:
  max_request_bytes: 134217728
  spill_threshold: 8388608
  max_concurrent: 8
http:
  connect_timeout: 1s
  response_header_timeout: 60s
  stream_idle_timeout: 60s
retry:
  max_attempts: 3
  mode: connect
  backoff: 20ms
health:
  enabled: false
groups:
  - name: smoke
    listen: 127.0.0.1:18765
    upstreams:
      - name: dead
        url: http://127.0.0.1:59998
        priority: 1
      - name: real
        url: $UPSTREAM
        priority: 10
YAML

"$BIN" -config "$CFG" > "$TMP/kernel.log" 2>&1 &
PID=$!
sleep 1.5

fail=0
code=$(curl -s --noproxy '*' -m 20 -o "$TMP/body.json" -w '%{http_code}' http://127.0.0.1:18765/health || true)
echo "1) 换路后 /health → ${code}"
[ "$code" = "200" ] || { echo "   失败：期望 200"; fail=1; }

token=$(cat "$TMP/data/admin.token" 2>/dev/null || true)
if [ -n "$token" ]; then
  ev=$(curl -s --noproxy '*' -m 5 -H "X-Linkgate-Token: $token" "http://127.0.0.1:18766/__linkgate/v1/events?limit=20")
  if echo "$ev" | grep -q '"kind":"failover"'; then
    echo "2) 事件里记录了 failover"
  else
    echo "   失败：事件里没有 failover"; fail=1
  fi
fi

head -c 9000000 /dev/zero | tr '\0' 'a' > "$TMP/big.bin"
code=$(curl -s --noproxy '*' -m 60 -o /dev/null -w '%{http_code}' -X POST \
  -H 'Content-Type: application/json' --data-binary @"$TMP/big.bin" \
  http://127.0.0.1:18765/v1/chat/completions || true)
echo "3) 9MB 请求体 → ${code}（401/400 都算通路正常，只要不是 413/502）"
case "$code" in
  413|502) echo "   失败：请求体没有被正确转发"; fail=1 ;;
esac

if [ "$fail" = "0" ]; then
  echo "冒烟测试通过。"
else
  echo "冒烟测试失败，内核日志："
  tail -30 "$TMP/kernel.log"
  exit 1
fi
