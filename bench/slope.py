#!/usr/bin/env python3
"""KiB/conn as in arco-ws docs/engine-spike.md: least-squares slope of container memory
(usage - inactive_file) against live connections over every completed step.
Also prints idle MiB, max stable connections, stop reason and p50/p99 at 10k and 20k.
Usage: bench/slope.py results/<label>.json ..."""
import json, sys

for f in [a for a in sys.argv[1:] if not a.endswith('state.json')]:
    r = json.load(open(f))
    pts = [(s['live'], s['memMiB']) for s in r['steps'] if s.get('memMiB') and s.get('live')]
    n = len(pts)
    mx = sum(x for x, _ in pts) / n
    my = sum(y for _, y in pts) / n
    slope = sum((x - mx) * (y - my) for x, y in pts) / sum((x - mx) ** 2 for x, _ in pts)
    at = {s['live']: s for s in r['steps'] if s.get('live')}
    lat = lambda k: f"{at[k]['p50']}/{at[k]['p99']}" if k in at and at[k].get('p99') is not None else '-'
    cpu = lambda k: f"{at[k]['cpuBroadcast']:.0f}/{at[k]['cpuBroadcastMax']:.0f}" if k in at else '-'
    print(f"{r['label']}: KiB/conn={slope*1024:.2f} idle={r['idle']['mem']:.1f}MiB "
          f"max_stable={max(x for x, _ in pts):,} stop={r.get('stop')!r} "
          f"p50/p99@10k={lat(10000)} @20k={lat(20000)} cpu@10k={cpu(10000)} @20k={cpu(20000)} points={n}")
