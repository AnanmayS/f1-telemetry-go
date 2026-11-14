#!/usr/bin/env bash
# Load test for /api/compare. Starts the server, generates a target mix
# (80% popular comparisons, 20% random laps), attacks at increasing rates with
# vegeta and prints a Markdown table. The highest rate whose p99 stays under
# 50 ms is the headline number in docs/BENCHMARKS.md.
#
#   bench/load/run.sh                 # default rates
#   RATES="500 1000" DURATION=30s bench/load/run.sh
#   PROCS=2 bench/load/run.sh         # restrict the server to 2 CPUs (small VPS)
#
# Needs: go, vegeta (go install github.com/tsenart/vegeta/v12@latest), curl.
set -euo pipefail
cd "$(dirname "$0")/../.."

DATA=${DATA:-data}
PORT=${PORT:-8097}
RATES=${RATES:-"250 500 1000 2000 3000 4000 6000"}
DURATION=${DURATION:-20s}
PROCS=${PROCS:-}
CACHE_MB=${CACHE_MB:-256}
OUT=${OUT:-bench/load/out}
BASE="http://127.0.0.1:$PORT"

command -v vegeta >/dev/null || { echo "vegeta not found: go install github.com/tsenart/vegeta/v12@latest" >&2; exit 1; }
mkdir -p "$OUT"
go build -o bin/server ./cmd/server
go build -o bin/gentargets ./bench/load/gentargets

start_server() {
  if [ -n "$PROCS" ]; then export GOMAXPROCS=$PROCS; fi
  ./bin/server -addr "127.0.0.1:$PORT" -data "$DATA" -static "" -rate 0 -cache-mb "$CACHE_MB" 2>"$OUT/server.log" &
  SERVER_PID=$!
  unset GOMAXPROCS
  for _ in $(seq 50); do curl -sf "$BASE/api/health" >/dev/null && return; sleep 0.1; done
  echo "server did not start; see $OUT/server.log" >&2; exit 1
}
stop_server() { kill "$SERVER_PID" 2>/dev/null || true; wait "$SERVER_PID" 2>/dev/null || true; }
trap stop_server EXIT

metric() { curl -s "$BASE/metrics" | awk -v m="$1" '$1==m {print $2}'; }

start_server
./bin/gentargets -base "$BASE" > "$OUT/targets.txt" 2>"$OUT/gentargets.log"
cat "$OUT/gentargets.log" >&2
stop_server

echo "| Rate (req/s) | Sent | Success | p50 | p95 | p99 | max | Server-side <= 50 ms | Response cache hits | Lap cache hits |"
echo "|---|---|---|---|---|---|---|---|---|---|"
for rate in $RATES; do
  start_server   # fresh process per rate: every run starts with an empty lap cache
  vegeta attack -targets "$OUT/targets.txt" -rate "$rate" -duration "$DURATION" -timeout 5s -max-workers 512 > "$OUT/result-$rate.bin"
  vegeta report -type json "$OUT/result-$rate.bin" > "$OUT/report-$rate.json"
  total=$(metric 'ghostlap_http_request_duration_seconds_count{route="compare"}')
  fast=$(metric 'ghostlap_http_request_duration_seconds_bucket{route="compare",le="0.05"}')
  hits=$(metric ghostlap_lap_cache_hits_total); misses=$(metric ghostlap_lap_cache_misses_total)
  rhits=$(metric ghostlap_response_cache_hits_total); rmisses=$(metric ghostlap_response_cache_misses_total)
  python3 - "$OUT/report-$rate.json" "$rate" "$total" "$fast" "$hits" "$misses" "$rhits" "$rmisses" <<'PY'
import json, sys
r = json.load(open(sys.argv[1])); rate, total, fast, hits, misses, rhits, rmisses = sys.argv[2], *map(float, sys.argv[3:9])
ms = lambda ns: f"{ns/1e6:.2f} ms"
l = r["latencies"]
print(f"| {rate} | {r['requests']} | {r['success']*100:.2f}% | {ms(l['50th'])} | {ms(l['95th'])} | {ms(l['99th'])} | {ms(l['max'])} | {fast/total*100:.2f}% of {int(total)} | {rhits/max(rhits+rmisses,1)*100:.1f}% | {hits/max(hits+misses,1)*100:.1f}% |")
PY
  stop_server
done
trap - EXIT
