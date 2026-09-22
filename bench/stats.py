"""Prints the API's /stats JSON (read from stdin) as one line per timer."""
import json
import sys

for name, s in sorted(json.load(sys.stdin).items()):
    print(f"  {name:28s} n={s['count']:<7d} mean={s['mean_ms']:.3f}  p50={s['p50_ms']:.3f}"
          f"  p95={s['p95_ms']:.3f}  p99={s['p99_ms']:.3f}  max={s['max_ms']:.1f}")
