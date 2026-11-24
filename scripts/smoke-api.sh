#!/usr/bin/env bash
# Stage 2/3 smoke test: SQLite + subscriptions + REST API + embedded UI.
# Uses local sing-box exit nodes and a local subscription file server, so no
# external infrastructure is required.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN="$ROOT/run/smoke"
BIN="$ROOT/bin/multivless"
SING_BOX="${SING_BOX:-$(ls -1 "$ROOT"/.tools/sing-box-*/sing-box 2>/dev/null | head -n1 || true)}"
[[ -n "$SING_BOX" && -x "$SING_BOX" ]] || { echo "sing-box not found (set SING_BOX)" >&2; exit 1; }
[[ -x "$BIN" ]] || { echo "run make build first" >&2; exit 1; }

PORT_ORIGIN=18080
PORT_SUB=18081
PORT_EXIT_A=20011
PORT_EXIT_B=20012
PORT_SOCKS=21081
PORT_API=18090
PORT_CLASH=19092
TOKEN="smoke-token"

UUID_A="11111111-2222-4333-8444-555555555561"
UUID_B="11111111-2222-4333-8444-555555555562"

PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  echo "--- gateway log ---" >&2
  tail -n 30 "$RUN/gateway.log" >&2 || true
  exit 1
}
pass() { echo "ok - $*"; }

wait_http() {
  local url="$1" tries="${2:-100}"
  for _ in $(seq 1 "$tries"); do
    if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}

api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -fsS --max-time 30 -X "$method" -H "Authorization: Bearer $TOKEN" \
      -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$PORT_API$path"
  else
    curl -fsS --max-time 30 -X "$method" -H "Authorization: Bearer $TOKEN" \
      "http://127.0.0.1:$PORT_API$path"
  fi
}

socks_get() {
  curl -fsS --max-time 5 --socks5-hostname "127.0.0.1:$PORT_SOCKS" -U "$1" \
    "http://127.0.0.1:$PORT_ORIGIN/" 2>/dev/null
}

rm -rf "$RUN"
mkdir -p "$RUN/www" "$RUN/data"

echo "multivless-smoke-ok" >"$RUN/www/index.html"

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

# Subscription body: base64 of two vless URIs.
SUB_PLAIN="vless://$UUID_A@127.0.0.1:$PORT_EXIT_A?security=none&type=tcp#Exit-A
vless://$UUID_B@127.0.0.1:$PORT_EXIT_B?security=none&type=tcp#Exit-B"
printf '%s' "$SUB_PLAIN" | base64 >"$RUN/www/sub.txt"

cat >"$RUN/config.yaml" <<EOF
app:
  name: multivless
  log_level: info
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

storage:
  path: $RUN/data/multivless.db

api:
  enabled: true
  listen: 127.0.0.1
  port: $PORT_API
  token: $TOKEN

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

subscriptions:
  interval: 1h
  timeout: 10s
  max_size: 1048576
  allow_private_hosts: true
EOF

echo "== starting origin, subscription server and exits"
(cd "$RUN/www" && exec python3 -m http.server "$PORT_ORIGIN" --bind 127.0.0.1 >"$RUN/origin.log" 2>&1) &
PIDS+=($!)
(cd "$RUN/www" && exec python3 -m http.server "$PORT_SUB" --bind 127.0.0.1 >"$RUN/subsrv.log" 2>&1) &
PIDS+=($!)
wait_http "http://127.0.0.1:$PORT_ORIGIN/" || fail "origin did not start"
wait_http "http://127.0.0.1:$PORT_SUB/" || fail "subscription server did not start"

"$SING_BOX" run -c "$RUN/exit-a.json" >"$RUN/exit-a.log" 2>&1 &
EXIT_A_PID=$!
PIDS+=($EXIT_A_PID)
"$SING_BOX" run -c "$RUN/exit-b.json" >"$RUN/exit-b.log" 2>&1 &
EXIT_B_PID=$!
PIDS+=($EXIT_B_PID)

echo "== starting gateway"
"$BIN" run --config "$RUN/config.yaml" >"$RUN/gateway.log" 2>&1 &
PIDS+=($!)
wait_http "http://127.0.0.1:$PORT_API/api/v1/health" 100 || fail "API did not start"

echo "== auth is enforced"
code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT_API/api/v1/servers")"
[[ "$code" == "401" ]] || fail "unauthenticated request returned $code, want 401"
pass "API requires the bearer token"

echo "== embedded web UI is served"
curl -fsS "http://127.0.0.1:$PORT_API/" | grep -q 'id="root"' || fail "web UI index is missing"
pass "web UI assets are embedded and served"

echo "== importing a subscription"
before="$(api GET /api/v1/servers)"
echo "$before" | grep -q '"servers":\[\]' || fail "expected an empty pool before import: $before"
result="$(api POST /api/v1/subscriptions "{\"name\":\"local\",\"url\":\"http://127.0.0.1:$PORT_SUB/sub.txt\",\"interval_seconds\":3600}")"
echo "$result" | grep -q '"server_count":2' || fail "subscription import result: $result"
pass "subscription imported 2 servers into SQLite and the engine"

echo "== waiting for health checks through the tunnels"
ready=false
for _ in $(seq 1 60); do
  if [[ "$(socks_get alice:secret || true)" == "multivless-smoke-ok" ]]; then ready=true; break; fi
  sleep 0.5
done
[[ "$ready" == true ]] || fail "proxying did not work after import"
pass "proxying works with the imported subscription servers"

servers="$(api GET /api/v1/servers)"
echo "$servers" | grep -q '"status":"ONLINE"' || fail "no ONLINE server in API: $servers"
echo "$servers" | grep -q '"source_id":"sub_' || fail "server does not reference its subscription"
pass "servers are ONLINE and linked to the subscription"

echo "== metrics"
api GET /api/v1/metrics | grep -q '^multivless_server_up{server="Exit-' || fail "prometheus metrics missing"
pass "Prometheus metrics are exposed"

echo "== SOCKS5 user management"
api POST /api/v1/users '{"username":"bob","password":"hunter2","max_connections":5}' >/dev/null
bob=false
for _ in $(seq 1 20); do
  if [[ "$(socks_get bob:hunter2 || true)" == "multivless-smoke-ok" ]]; then bob=true; break; fi
  sleep 0.3
done
[[ "$bob" == true ]] || fail "newly created user cannot authenticate"
if socks_get bob:wrong >/dev/null 2>&1; then fail "wrong password was accepted"; fi
pass "users are created with Argon2id hashes and authenticate over SOCKS5"

echo "== failover through the API-managed pool"
kill "$EXIT_A_PID" 2>/dev/null || true
wait "$EXIT_A_PID" 2>/dev/null || true
sleep 12
for _ in $(seq 1 5); do
  [[ "$(socks_get alice:secret || true)" == "multivless-smoke-ok" ]] || fail "traffic stopped after one exit died"
done
pass "healthy servers keep serving after a failure"

echo "== connection accounting"
stats="$(api GET /api/v1/stats)"
echo "$stats" | grep -q '"total":[1-9]' || fail "connection counter did not move: $stats"
echo "$stats" | grep -q '"online":[1-9]' || fail "no online server in stats: $stats"
conns="$(api GET /api/v1/connections)"
echo "$conns" | grep -q '"connections"' || fail "connections endpoint broken"
pass "stats and live connections are reported"

echo
echo "ALL API/WEB SMOKE CHECKS PASSED"
