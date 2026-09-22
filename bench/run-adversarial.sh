#!/usr/bin/env bash
# One adversarial run, verified from the database afterwards:
#
#   bench/run-adversarial.sh            # sweeper on
#   SWEEPER=off bench/run-adversarial.sh
#
# Resets the database, seeds ADV-SKU with STOCK units, starts the API with a short
# hold window (and the sweeper unless SWEEPER=off), runs bench/adversarial.js with
# every request recorded, stops the sweeper, runs one final sweep, then runs
# cmd/verify against the recorded events and the database.
#
# Environment: VUS (1000), DURATION (55s, after a 5 s ramp), STOCK (50), HOLD (3s),
# SWEEPER (on), SWEEP_INTERVAL (500ms), DB_MAX_CONNS (20), PORT (8081),
# OUT (directory for events, logs and the k6 summary; default bench/results/adversarial-out).
set -euo pipefail
cd "$(dirname "$0")/.."

VUS=${VUS:-1000}
DURATION=${DURATION:-55s}
STOCK=${STOCK:-50}
HOLD=${HOLD:-3s}
SWEEPER=${SWEEPER:-on}
SWEEP_INTERVAL=${SWEEP_INTERVAL:-500ms}
DB_MAX_CONNS=${DB_MAX_CONNS:-20}
PORT=${PORT:-8081}
OUT=${OUT:-bench/results/adversarial-out}
BASE="http://localhost:$PORT"
HOLD_MS=$(python3 -c "import re,sys; s='$HOLD'; m=re.fullmatch(r'(\d+)(ms|s)', s); print(int(m.group(1))*(1 if m.group(2)=='ms' else 1000))")

mkdir -p "$OUT"
psql() { docker compose exec -T postgres psql -U stocklock -d stocklock -qtA -c "$1"; }

go build -o bench/api ./cmd/api
go build -o bench/sweeper ./cmd/sweeper
go build -o bench/verify ./cmd/verify

API_PID=""; SWEEPER_PID=""
cleanup() {
  [ -n "$SWEEPER_PID" ] && kill -INT "$SWEEPER_PID" 2>/dev/null || true
  [ -n "$API_PID" ] && kill "$API_PID" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT

echo "commit $(git rev-parse --short HEAD)  vus=$VUS duration=5s+$DURATION stock=$STOCK hold=$HOLD sweeper=$SWEEPER pool=$DB_MAX_CONNS"

psql "TRUNCATE operations, reservations; DELETE FROM stock WHERE sku = 'ADV-SKU';
      INSERT INTO stock (sku, initial_stock, available, reserved, sold) VALUES ('ADV-SKU', $STOCK, $STOCK, 0, 0);"

LISTEN_ADDR=":$PORT" DB_MAX_CONNS=$DB_MAX_CONNS HOLD_WINDOW=$HOLD ./bench/api >"$OUT/api.log" 2>&1 &
API_PID=$!
for _ in $(seq 1 50); do
  curl -sf "$BASE/inventory/ADV-SKU" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -sf "$BASE/inventory/ADV-SKU" >/dev/null || { echo "API did not start:" >&2; cat "$OUT/api.log" >&2; exit 1; }

if [ "$SWEEPER" = on ]; then
  SWEEP_INTERVAL=$SWEEP_INTERVAL ./bench/sweeper >"$OUT/sweeper.log" 2>&1 &
  SWEEPER_PID=$!
fi

rm -f "$OUT/events.jsonl"
k6 run --quiet --log-format=raw --console-output="$OUT/events.jsonl" \
  --summary-export="$OUT/k6-summary.json" \
  -e VUS="$VUS" -e DURATION="$DURATION" -e BASE="$BASE" -e HOLD_MS="$HOLD_MS" \
  bench/adversarial.js >"$OUT/k6.log" 2>&1 || { echo "k6 failed:" >&2; tail -20 "$OUT/k6.log" >&2; exit 1; }

if [ -n "$SWEEPER_PID" ]; then
  kill -INT "$SWEEPER_PID"; wait "$SWEEPER_PID" 2>/dev/null || true; SWEEPER_PID=""
fi
# Let the last holds expire, then one final sweep with nothing racing it.
sleep "$(python3 -c "print($HOLD_MS/1000 + 0.5)")"
SWEEP_ONCE=1 ./bench/sweeper >>"$OUT/sweeper.log" 2>&1

echo "events recorded: $(grep -c '^{' "$OUT/events.jsonl")   api internal errors: $(grep -c 'internal error' "$OUT/api.log" || true)   sweeper errors: $(grep -c 'sweep:' "$OUT/sweeper.log" || true)"
./bench/verify -events "$OUT/events.jsonl" -sku ADV-SKU -stock "$STOCK"
