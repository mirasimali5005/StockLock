#!/usr/bin/env bash
# Runs the hot-SKU benchmark end to end and prints one result block per run:
#
#   bench/run-hot.sh [VUS ...]        default: 10 100 1000
#
# For each VU level, REPEATS times:
#   reset the database, seed HOT-SKU with a deep stock, start the API on
#   LISTEN_ADDR with DB_MAX_CONNS, run k6, print k6's numbers and the API's
#   server-side timers, verify k6's 201 count against the database, run the
#   invariant checker, stop the API.
#
# Environment: DURATION (30s), REPEATS (3), DB_MAX_CONNS (20), PORT (8081),
# STOCK (1000000), OUT (directory for k6 JSON summaries; optional).
set -euo pipefail
cd "$(dirname "$0")/.."

DURATION=${DURATION:-30s}
REPEATS=${REPEATS:-3}
DB_MAX_CONNS=${DB_MAX_CONNS:-20}
PORT=${PORT:-8081}
STOCK=${STOCK:-1000000}
LEVELS=${*:-10 100 1000}
BASE="http://localhost:$PORT"

psql() { docker compose exec -T postgres psql -U stocklock -d stocklock -qtA -c "$1"; }

reset_db() {
  psql "TRUNCATE operations, reservations; DELETE FROM stock WHERE sku = 'HOT-SKU';
        INSERT INTO stock (sku, initial_stock, available, reserved, sold) VALUES ('HOT-SKU', $STOCK, $STOCK, 0, 0);"
}

start_api() {
  LISTEN_ADDR=":$PORT" DB_MAX_CONNS=$DB_MAX_CONNS ./bench/api >"$API_LOG" 2>&1 &
  API_PID=$!
  for _ in $(seq 1 50); do
    curl -sf "$BASE/inventory/HOT-SKU" >/dev/null 2>&1 && return
    sleep 0.1
  done
  echo "API did not start; log:" >&2; cat "$API_LOG" >&2; exit 1
}

stop_api() { kill "$API_PID" 2>/dev/null || true; wait "$API_PID" 2>/dev/null || true; }

go build -o bench/api ./cmd/api
API_LOG=$(mktemp)
trap 'stop_api' EXIT

echo "commit $(git rev-parse --short HEAD)  duration=$DURATION repeats=$REPEATS pool=$DB_MAX_CONNS stock=$STOCK"
echo

for vus in $LEVELS; do
  for rep in $(seq 1 "$REPEATS"); do
    echo "=== VUs=$vus run $rep/$REPEATS"
    reset_db
    start_api
    curl -sf -X POST "$BASE/stats/reset" >/dev/null

    summary=""
    [ -n "${OUT:-}" ] && { mkdir -p "$OUT"; summary="$OUT/hot-vus$vus-run$rep.json"; }
    api_log_copy=""
    [ -n "${OUT:-}" ] && api_log_copy="$OUT/hot-vus$vus-run$rep.api.log"
    k6 run --quiet -e VUS="$vus" -e DURATION="$DURATION" -e BASE="$BASE" -e SUMMARY="$summary" bench/hot-sku.js

    echo "server-side timers (ms):"
    curl -s "$BASE/stats" | python3 bench/stats.py

    rows=$(psql "SELECT COUNT(*) FROM reservations WHERE sku = 'HOT-SKU'")
    echo "reservation rows in db: $rows"
    go run ./cmd/checker
    stop_api
    [ -n "$api_log_copy" ] && cp "$API_LOG" "$api_log_copy"
    echo "api internal errors: $(grep -c 'internal error' "$API_LOG" || true)"
    echo
  done
done
rm -f bench/api "$API_LOG"
