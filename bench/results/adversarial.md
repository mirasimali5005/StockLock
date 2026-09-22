# Adversarial run: 1,000 shoppers, 50 units, verified from the database

Run on 2026-09-22 on the Phase 9 code (final reserve shape from `optimized.md`).
Per-run verifier output: `adversarial-runs/on-1.txt` … `on-10.txt`, `off-1.txt`, `off-2.txt`.
Script: `bench/run-adversarial.sh`; workload: `bench/adversarial.js`; verifier: `cmd/verify`.

## The scenario

| | |
|---|---|
| Stock | `ADV-SKU`, 50 units |
| Shoppers | 1,000 k6 virtual users, ramped up over 5 s, then held for 55 s (k6 grace 10 s) |
| Hold window | 3 s (`HOLD_WINDOW=3s`), so holds expire and cycle during the run |
| Sweeper | running on a 500 ms interval (10 runs) or **off** (2 runs); one final sweep after every run |
| API | one instance, native, pool 20, port 8081 |
| Each attempt | reserve with a fresh `operation_id`; 15% of shoppers instead send that reserve **10 times at once** (retry storm) |
| On success | one of: confirm within the hold · abandon within the hold · **vanish** · wait past the deadline then confirm (**late**) · send the same confirm **twice at once** (double-click) |

Every request is recorded as an event (operation id, endpoint, status, body, send time). After the run, `cmd/verify` cross-checks the events against the database.

## What the verifier checks

1. The five invariants (`internal/invariant`): units conserved, `available >= 0`, `reserved` = RESERVED rows, `sold` = CONFIRMED rows, reservation rows = successful RESERVE operations.
2. Every `201` reserve names a reservation row that exists, and no reservation was handed to two different operations.
3. Every `200` confirm matches a `CONFIRMED` row; every `200` abandon an `ABANDONED` row; a `409 reservation_expired` never hides a row that is actually confirmed or abandoned.
4. Every operation sent more than once got one answer (same status, same body) and has exactly one operations row that agrees.
5. No `200` confirm or abandon was sent after its reservation's deadline (client send time vs database deadline, 100 ms tolerance).
6. After the final sweep, no `RESERVED` row is past its deadline.

Before any real run, the verifier was shown to fail on six deliberate corruptions: a confirmed row flipped to abandoned, a confirm sent after its deadline, an unswept overdue hold, a retry copy claiming a different answer, a `201` naming a nonexistent reservation, and an extra reservation row.

## Results

**12 of 12 runs: VERIFY OK.** 0 API internal errors, 0 sweeper errors, 0 requests lost at the client, in every run.

| run | events | reserve 201 / 409 | distinct successful reserves = rows | confirmed | abandoned | expired | late/dup confirms refused | ops sent >1× | final avail / reserved / sold |
|---|---|---|---|---|---|---|---|---|---|
| on-1 | 135,563 | 397 / 135,041 | 136 = 136 | 50 | 25 | 61 | 28 | 8,660 | 0 / 0 / 50 |
| on-2 | 134,669 | 307 / 134,244 | 127 = 127 | 50 | 33 | 44 | 20 | 8,564 | 0 / 0 / 50 |
| on-3 | 136,209 | 273 / 135,818 | 120 = 120 | 50 | 25 | 45 | 19 | 8,724 | 0 / 0 / 50 |
| on-4 | 135,833 | 221 / 135,502 | 113 = 113 | 50 | 19 | 44 | 26 | 8,685 | 0 / 0 / 50 |
| on-5 | 136,125 | 346 / 135,659 | 121 = 121 | 50 | 24 | 47 | 25 | 8,732 | 0 / 0 / 50 |
| on-6 | 135,357 | 266 / 134,963 | 122 = 122 | 50 | 21 | 51 | 30 | 8,638 | 0 / 0 / 50 |
| on-7 | 136,819 | 226 / 136,470 | 127 = 127 | 50 | 25 | 52 | 28 | 8,801 | 0 / 0 / 50 |
| on-8 | 135,701 | 369 / 135,184 | 153 = 153 | 50 | 35 | 68 | 39 | 8,682 | 0 / 0 / 50 |
| on-9 | 136,884 | 296 / 136,458 | 125 = 125 | 50 | 25 | 50 | 30 | 8,807 | 0 / 0 / 50 |
| on-10 | 137,147 | 291 / 136,732 | 129 = 129 | 50 | 26 | 53 | 30 | 8,817 | 0 / 0 / 50 |
| off-1 | 134,468 | 129 / 134,274 | 57 = 57 | 31 | 7 | 19 | 10 | 8,534 | 19 / 0 / 31 |
| off-2 | 135,521 | 163 / 135,301 | 64 = 64 | 21 | 14 | 29 | 17 | 8,624 | 29 / 0 / 21 |

"reserve 201" counts responses, including the 10 identical copies a retry storm receives; "distinct successful reserves" counts operations. "expired" is after the final sweep.

## Reading the numbers

- **Never more than 50 units out at once, and every answer was true.** In every run the distinct successful reserve operations equal the reservation rows, every 200 matched its row, and the counters summed to 50.
- **With the sweeper on, all 50 units end sold.** Units abandoned or expired went back on the shelf and were taken again (113–153 reservations for 50 units) until a confirming shopper got each one.
- **With the sweeper off, expired holds stayed held until the end**, so fewer units cycled (57–64 reservations) and 19–29 units were still available after the final sweep. Late confirms (10 and 17 of them) were still refused: the deadline is authoritative with no cleanup running at all.
- **Retries were harmless.** About 8,700 operations per run were sent more than once (10-way reserve storms and double-click confirms); every copy got the same answer and each has one operations row.
- **~135,000 refusals per run** is the expected shape: 1,000 shoppers fighting over 50 units mostly hear "out of stock".

## Reproducing

```
bench/run-adversarial.sh                 # sweeper on, verifier at the end
SWEEPER=off bench/run-adversarial.sh
for i in $(seq 1 100); do bench/run-adversarial.sh > run-$i.txt; done   # ~80 s each
```
