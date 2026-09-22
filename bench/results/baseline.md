# Hot-SKU baseline (before optimization)

Measured on 2026-09-21 on the code tagged `baseline-locking`. Raw output: `baseline-hot.log`
(its header says `commit 8d25ebd` because the instrumentation was not yet committed when it ran; the
measured API code is exactly what the tag points at).

This is the deliberately naive reserve transaction: `SELECT ... FOR UPDATE`, check
`available` in Go, `UPDATE stock`, `INSERT reservation`, `UPDATE operations`
(store the answer), `COMMIT`. Four round trips happen while the stock row is locked.

## Setup

| | |
|---|---|
| Machine | Apple M5, 10 cores, 24 GB, macOS 27.0 |
| Postgres | 17.11 in Docker Desktop 28.5.1 (VM: 10 cpus, 8 GB), default config: `synchronous_commit=on`, `fsync=on`, `wal_sync_method=fdatasync`, `shared_buffers=128MB`, `max_connections=100` |
| Commit floor | `pg_test_fsync` in the container: fdatasync 56 µs/op (~17,800 ops/s). Docker Desktop's virtual disk makes flushes nearly free, so disk is **not** the ceiling here; on a real disk it would be. |
| API | one instance, run natively (`go build ./cmd/api`), `DB_MAX_CONNS=20`, `LISTEN_ADDR=:8081` |
| Load | k6 v2.3.0 on the same machine, `bench/hot-sku.js`: every VU reserves `HOT-SKU` in a loop with a fresh `operation_id`; 30 s per run; 3 runs per VU level |
| Stock | `HOT-SKU` seeded with 1,000,000 units, so every request is a real mutation |
| Sweeper | off (5-minute hold, nothing expires within a run) |
| Command | `OUT=... bench/run-hot.sh 10 100 1000` |

## Definitions

Server-side timers, recorded per successful mutation (`GET /stats`):

- **lock_wait**: duration of the `SELECT ... FOR UPDATE` statement, i.e. queueing for the stock row lock
- **critical_section**: from the row lock being acquired until `COMMIT` returned, i.e. how long the row is held
- **transaction**: from `BEGIN` until `COMMIT` returned; includes waiting for a pool connection, which is why it tracks k6 latency at high VU counts

k6 latency is the full HTTP round trip as seen by the client.

## Results (median of 3 runs)

| VUs | reserves/s | k6 p50 | k6 p95 | k6 p99 | critical section mean / p50 / p99 | lock wait mean / p50 | transaction p50 / p99 |
|---|---|---|---|---|---|---|---|
| 10 | **1,475** | 2.02 ms | 30.1 ms | 55.9 ms | **0.659 / 0.615 / 1.31 ms** | 5.8 / 1.0 ms | 1.96 / 55.8 ms |
| 100 | **1,343** | 69.3 ms | 108.7 ms | 138.4 ms | **0.742 / 0.690 / 1.54 ms** | 13.8 / 9.7 ms | 69.2 / 138.3 ms |
| 1000 | **1,434** | 688 ms | 745 ms | 804 ms | **0.697 / 0.672 / 1.28 ms** | 12.9 / 9.2 ms | 688 / 804 ms |

All runs: every k6 `201` matched a reservation row in the database, and the invariant checker passed after each run.

Per-run spread (reserves/s): 10 VUs 1,395–1,510; 100 VUs 1,312–1,410; 1000 VUs 1,397–1,446.

## Reading the numbers

- **The hot row is the ceiling.** Throughput is flat at roughly 1,400–1,500 reserves/s regardless of VU count, and 1 ÷ 0.7 ms ≈ 1,430/s. Each shopper holds the stock row for about 0.7 ms and they are served one at a time; adding clients only adds queueing.
- **Where the 0.7 ms goes.** The critical section is four round trips over the Docker socket (`UPDATE stock`, `INSERT reservation`, `UPDATE operations`, `COMMIT`); the commit's flush itself is ~56 µs. The critical section is round-trip bound, not disk bound.
- **Lock wait ≈ 13 ms at 100+ VUs** is about 19 transactions ahead in line × 0.7 ms: the pool of 20 keeps ~20 transactions in flight, and all but one are waiting for the same row.
- **Latency above 100 VUs is pool queueing.** At 1000 VUs the transaction timer (688 ms) equals k6 latency: requests spend almost all their time waiting for one of the 20 connections. Raising the pool would not raise throughput, since the row serializes everything anyway.

## Anomaly

1000-VU run 2 had 91 responses (0.2% of 44,360) that were neither 201 nor 409. The run script was not recording unexpected statuses at the time, so their status is unknown. They caused no inventory effect: the run's 201 count equals its reservation-row count and the checker passed. A rerun at 1000 VUs with logging added (`k6-recheck`) produced 0 such responses and 0 API internal errors. The script now prints the first unexpected responses and keeps the API log.
