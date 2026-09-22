# Hot-SKU optimization: before and after

Measured on 2026-09-21, same machine, container, pool (20), workload and script as
`baseline.md`. Before = tag `baseline-locking`. After = this commit. Raw logs:
`baseline-hot*.log`, `stage1-hot*.log`, `stage2-hot*.log`, `stage3-hot*.log`.
Medians computed by `bench/medians.py`.

## What changed

The baseline held the hot stock row through four round trips after a separate
`SELECT ... FOR UPDATE`: `UPDATE stock`, `INSERT reservation`, `UPDATE operations`
(store the answer), `COMMIT`. The commit's disk flush is ~56 µs here, so the section
was round-trip bound (a bare round trip measured 0.105 ms).

Three stages, each measured on its own:

| Stage | Reserve while the row is held | Round trips under the lock |
|---|---|---|
| Baseline | `SELECT ... FOR UPDATE` → check in Go → `UPDATE stock` → `INSERT reservation` → `UPDATE operations` → `COMMIT` | 4 (+ the tail of the SELECT) |
| 1 | `UPDATE stock ... WHERE available > 0` takes the lock and decides → `INSERT reservation` → `UPDATE operations` → `COMMIT` | 4 |
| 2 | one CTE: `UPDATE stock` + `INSERT reservation` → `UPDATE operations` → `COMMIT` | 3 |
| **3 (final)** | one CTE: `UPDATE stock` + `INSERT reservation` + `UPDATE operations` (answer built with `jsonb_build_object`) → `COMMIT` | **2** |

Stage 1 looks like no change in the critical-section column because the timer's
start moved: with a separate `SELECT ... FOR UPDATE` the timer started when the lock
was already held; with the lock taken inside the `UPDATE`, the timer starts when the
statement is sent, so it includes the request's own trip. Stage 1 did remove one round
trip from the transaction (0.82 → 0.71 ms at 1 VU).

## Critical section (1 VU: no contention, no lock wait)

| | critical section mean / p50 / p99 | transaction p50 | reserves/s (1 VU) |
|---|---|---|---|
| Baseline | 0.540 / 0.514 / 1.08 ms | 0.82 ms | 1,076 |
| Stage 1 | 0.527 / 0.507 / 1.04 ms | 0.71 ms | 1,239 |
| Stage 2 | 0.450 / 0.427 / 0.79 ms | 0.64 ms | 1,338 |
| **Stage 3** | **0.362 / 0.325 / 0.87 ms** | **0.54 ms** | **1,484** |

Critical section **0.54 → 0.36 ms (−33%)**; transaction 0.82 → 0.54 ms (−34%).

## Throughput and latency under contention (median of 3 × 30 s)

| VUs | | reserves/s | k6 p50 | k6 p95 | k6 p99 |
|---|---|---|---|---|---|
| 2 | before | 1,648 | 1.13 ms | 1.42 ms | 2.49 ms |
| | **after** | **2,640** | 0.61 ms | 1.16 ms | 2.01 ms |
| 10 | before | 1,475 | 2.02 ms | 30.1 ms | 55.9 ms |
| | **after** | **2,713** | 2.75 ms | 9.1 ms | 14.4 ms |
| 100 | before | 1,343 | 69.3 ms | 108.7 ms | 138.4 ms |
| | **after** | **1,873** | 44.7 ms | 85.5 ms | 122.4 ms |
| 1000 | before | 1,434 | 688 ms | 745 ms | 804 ms |
| | **after** | **1,993** | 492 ms | 591 ms | 654 ms |

Per-run spread (reserves/s), before → after: 10 VUs 1,395–1,510 → 2,711–2,729;
100 VUs 1,312–1,410 → 1,831–2,285; 1000 VUs 1,397–1,446 → 1,869–2,154.

Throughput-implied time per reservation at 10 VUs: 1000 ÷ 1,475 = 0.68 ms before,
1000 ÷ 2,713 = 0.37 ms after, consistent with the measured critical sections.

Every run: k6's 201 count equalled the reservation rows created, and the invariant
checker passed. The full test suite (concurrency, idempotency, deadline and sweeper
tests) passes unchanged on the final code, and removing `AND available > 0` makes the
last-unit test fail in 50 of 50 rounds.

## Where the remaining time goes

- **Up to ~10 concurrent transactions, round trips were the cost.** Each stage that
  removed a round trip from under the lock bought a step in throughput: 1,475 → 1,945
  → 2,022 → 2,713 reserves/s at 10 VUs.
- **At 100–1000 VUs the ceiling flattens near 1,900–2,000/s** even though the
  critical section is 0.36 ms (which alone would allow ~2,700/s). With 20 connections
  all queued on one row, each commit must wake the next waiting Postgres backend and
  that backend must run before the row moves on; that hand-off, inside a Docker VM on a
  shared laptop, is now the larger cost, and fewer round trips cannot remove it. This is
  the hot-row contention floor for this setup.
- **Latency above 100 VUs is pool queueing**, as in the baseline: the transaction timer
  equals k6 latency because requests wait for one of the 20 connections.

## Not done

- Pipelining the statement and `COMMIT` into one network write would take the lock
  down to a single round trip. Not attempted.
- Confirm and abandon are unchanged; they still lock the reservation row with a
  separate `SELECT ... FOR UPDATE`.

## Connection resets at 1000 VUs

Some 1000-VU runs record a few dozen requests with `status 0: dial: connection reset
by peer` (baseline run 2: 91; stage 1: 134; stage 3: 42; stage 2: 0). k6 opens 1,000
connections at once and macOS's default listen backlog (128) drops some SYNs. These
never reach the API (0 internal errors in its log) and have no inventory effect. They
are excluded from throughput only in the sense that they were never served.
