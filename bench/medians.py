"""Summarizes run-hot.sh logs: per VU level, the median over runs of each figure.

    python3 bench/medians.py bench/results/baseline-hot.log [more logs...]
"""
import re
import statistics
import sys


def parse(path):
    runs = {}
    vus = None
    for line in open(path):
        if m := re.match(r"=== VUs=(\d+) run", line):
            vus = int(m.group(1))
            runs.setdefault(vus, []).append({})
        elif m := re.match(r"requests:\s+\d+\s+\(([\d.]+) req/s\)", line):
            runs[vus][-1]["req/s"] = float(m.group(1))
        elif m := re.match(r"latency ms:\s+p50=([\d.]+)\s+p95=([\d.]+)\s+p99=([\d.]+)", line):
            runs[vus][-1].update({"k6 p50": float(m.group(1)), "k6 p95": float(m.group(2)), "k6 p99": float(m.group(3))})
        elif m := re.match(r"\s+RESERVE\.(\w+)\s+n=\d+\s+mean=([\d.]+)\s+p50=([\d.]+)\s+p95=([\d.]+)\s+p99=([\d.]+)", line):
            t = {"critical_section": "cs", "lock_wait": "lw", "transaction": "tx"}[m.group(1)]
            runs[vus][-1].update({f"{t} mean": float(m.group(2)), f"{t} p50": float(m.group(3)), f"{t} p99": float(m.group(5))})
        elif m := re.match(r"failed other:\s+(\d+)", line):
            runs[vus][-1]["failed"] = int(m.group(1))
    return runs


for path in sys.argv[1:]:
    print(f"## {path}")
    for vus, rs in sorted(parse(path).items()):
        keys = [k for k in ("req/s", "k6 p50", "k6 p95", "k6 p99", "cs mean", "cs p50", "cs p99", "lw mean", "tx p50", "tx p99", "failed") if all(k in r for r in rs)]
        med = {k: statistics.median(r[k] for r in rs) for k in keys}
        print(f"VUs={vus:<5} runs={len(rs)}  " + "  ".join(f"{k}={med[k]:g}" for k in keys))
    print()
