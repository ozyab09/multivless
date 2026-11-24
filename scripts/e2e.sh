#!/usr/bin/env bash
# End-to-end check for Stage 1 without external servers:
#   * two sing-box "exit" nodes with VLESS inbounds on loopback
#   * the gateway under test in front of them
#   * a local origin HTTP server behind the exits
#
# Verifies: VLESS import, per-connection distribution, health checks through the
# tunnel, failover on a dead exit, fail-closed when no exit is alive, and
# recovery when an exit returns.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN="$ROOT/run/e2e"
BIN="$ROOT/bin/multivless"

SING_BOX="${SING_BOX:-$(ls -1 "$ROOT"/.tools/sing-box-*/sing-box 2>/dev/null | head -n1 || true)}"
if [[ -z "${SING_BOX}" || ! -x "${SING_BOX}" ]]; then
  echo "sing-box binary not found; set SING_BOX=/path/to/sing-box" >&2
  exit 1
fi
if [[ ! -x "$BIN" ]]; then
  echo "build the gateway first: make build" >&2
  exit 1
fi

PORT_ORIGIN="${PORT_ORIGIN:-18080}"
PORT_EXIT_A="${PORT_EXIT_A:-20001}"
PORT_EXIT_B="${PORT_EXIT_B:-20002}"
PORT_SOCKS="${PORT_SOCKS:-21080}"
PORT_CLASH="${PORT_CLASH:-19091}"

UUID_A="11111111-2222-4333-8444-555555555551"
UUID_B="11111111-2222-4333-8444-555555555552"

PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  echo "--- gateway log ---" >&2
  tail -n 40 "$RUN/gateway.log" 2>/dev/null >&2 || true
  exit 1
}

pass() { echo "ok - $*"; }

wait_port() {
  local port="$1" tries="${2:-100}"
  for _ in $(seq 1 "$tries"); do
    if nc -z 127.0.0.1 "$port" 2>/dev/null; then return 0; fi
    sleep 0.1
  done
  return 1
}

rm -rf "$RUN"
mkdir -p "$RUN/www"

cat >"$RUN/www/index.html" <<'EOF'
multivless-e2e-ok
EOF

# A VLESS exit node: listens on loopback, sends everything out directly.
make_exit() {
  local port="$1" uuid="$2" file="$3"
  cat >"$file" <<EOF
{
  "log": {"level": "warn"},
  "inbounds": [
    {"type": "vless", "tag": "vless-in", "listen": "127.0.0.1", "listen_port": $port,
     "users": [{"uuid": "$uuid"}]}
  ],
  "outbounds": [{"type": "direct"}]
}
EOF
}

make_exit "$PORT_EXIT_A" "$UUID_A" "$RUN/exit-a.json"
make_exit "$PORT_EXIT_B" "$UUID_B" "$RUN/exit-b.json"
"$SING_BOX" check -c "$RUN/exit-a.json"
"$SING_BOX" check -c "$RUN/exit-b.json"

cat >"$RUN/config.yaml" <<EOF
app:
  name: multivless
  log_level: debug
  data_dir: $RUN/data

engine:
  type: sing-box
  binary: $SING_BOX
  config_path: $RUN/data/sing-box.json
  clash_api: 127.0.0.1:$PORT_CLASH
  lanes: 2
  start_timeout: 15s
  restart_on_failure: false
  log_level: warn

socks5:
  enabled: true
  listen: 127.0.0.1
  port: $PORT_SOCKS
  authentication: true
  allow_no_auth: false
  max_connections: 1000
  connection_timeout: 10s
  idle_timeout: 30s
  users:
    - username: alice
      password: secret

balancer:
  strategy: round-robin
  fallback_strategy: round-robin
  max_attempts: 2
  weights:
    latency: 0.30
    connections: 0.25
    throughput: 0.35
    reliability: 0.10

health_check:
  enabled: true
  interval: 5s
  timeout: 3s
  failures_before_offline: 2
  successes_before_online: 1
  concurrency: 2
  url: http://127.0.0.1:$PORT_ORIGIN/
  fallback_urls: []

servers:
  - vless://$UUID_A@127.0.0.1:$PORT_EXIT_A?security=none&type=tcp#Exit-A
  - vless://$UUID_B@127.0.0.1:$PORT_EXIT_B?security=none&type=tcp#Exit-B
EOF

echo "== starting origin"
(cd "$RUN/www" && exec python3 -m http.server "$PORT_ORIGIN" --bind 127.0.0.1 >"$RUN/origin.log" 2>&1) &
PIDS+=($!)
wait_port "$PORT_ORIGIN" || fail "origin did not start"

echo "== starting exit nodes"
"$SING_BOX" run -c "$RUN/exit-a.json" >"$RUN/exit-a.log" 2>&1 &
EXIT_A_PID=$!
PIDS+=($EXIT_A_PID)
"$SING_BOX" run -c "$RUN/exit-b.json" >"$RUN/exit-b.log" 2>&1 &
EXIT_B_PID=$!
PIDS+=($EXIT_B_PID)
wait_port "$PORT_EXIT_A" || fail "exit A did not start"
wait_port "$PORT_EXIT_B" || fail "exit B did not start"

echo "== starting gateway"
"$BIN" run --config "$RUN/config.yaml" >"$RUN/gateway.log" 2>&1 &
PIDS+=($!)

curl_socks() {
  curl -fsS --max-time 5 --socks5-hostname "127.0.0.1:$PORT_SOCKS" -U alice:secret \
    "http://127.0.0.1:$PORT_ORIGIN/" 2>/dev/null
}

echo "== waiting for the first healthy check"
ready=false
for _ in $(seq 1 60); do
  if [[ "$(curl_socks || true)" == "multivless-e2e-ok" ]]; then ready=true; break; fi
  sleep 0.5
done
[[ "$ready" == true ]] || fail "gateway never served a proxied request"
pass "proxying works through VLESS"

echo "== distributing 10 connections"
for _ in $(seq 1 10); do
  body="$(curl_socks)" || fail "request failed during distribution test"
  [[ "$body" == "multivless-e2e-ok" ]] || fail "unexpected body: $body"
done

used_a="$(grep -c '"server":"Exit-A"' "$RUN/gateway.log" || true)"
used_b="$(grep -c '"server":"Exit-B"' "$RUN/gateway.log" || true)"
echo "   selections: Exit-A=$used_a Exit-B=$used_b"
[[ "$used_a" -ge 1 && "$used_b" -ge 1 ]] || fail "round-robin did not use both exits"
pass "each new connection was balanced independently"

echo "== killing exit A"
kill "$EXIT_A_PID" 2>/dev/null || true
wait "$EXIT_A_PID" 2>/dev/null || true

# Two failed probes at a 5s interval plus margin.
sleep 13
before_a="$(grep -c '"server":"Exit-A"' "$RUN/gateway.log" || true)"
for _ in $(seq 1 5); do
  curl_socks >/dev/null || fail "traffic stopped after a single exit failure"
done
after_a="$(grep -c '"server":"Exit-A"' "$RUN/gateway.log" || true)"
[[ "$before_a" -eq "$after_a" ]] || fail "dead exit was still selected"
pass "dead exit was removed from the pool (failover)"

echo "== killing exit B too"
kill "$EXIT_B_PID" 2>/dev/null || true
wait "$EXIT_B_PID" 2>/dev/null || true
sleep 13

if curl_socks >/dev/null 2>&1; then
  fail "gateway still served traffic with no live exits (direct fallback?)"
fi
# The origin itself is alive, so the rejection comes from the gateway.
curl -fsS --max-time 3 "http://127.0.0.1:$PORT_ORIGIN/" >/dev/null \
  || fail "origin died; fail-closed result is inconclusive"
pass "fail-closed: no direct fallback when every exit is down"

echo "== reviving exit A"
"$SING_BOX" run -c "$RUN/exit-a.json" >"$RUN/exit-a2.log" 2>&1 &
EXIT_A2_PID=$!
PIDS+=($EXIT_A2_PID)
wait_port "$PORT_EXIT_A" || fail "revived exit A did not listen"

recovered=false
for _ in $(seq 1 60); do
  if [[ "$(curl_socks || true)" == "multivless-e2e-ok" ]]; then recovered=true; break; fi
  sleep 0.5
done
[[ "$recovered" == true ]] || fail "gateway did not recover after an exit came back"
pass "recovered exit returned to the pool"

echo
echo "ALL E2E CHECKS PASSED"
