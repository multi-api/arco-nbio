#!/usr/bin/env python3
"""Print Markdown tables from results/*.json (one row per step)."""
import json, glob, sys

files = sys.argv[1:] or sorted(glob.glob('results/*.json'))
for f in files:
    if f.endswith('state.json'):
        continue
    r = json.load(open(f))
    print(f"\n#### {r['label']}\n")
    print(f"Idle: {r['idle']['mem']:.1f} MiB. Stop: {r.get('stop')}. Broadcast limit: {r.get('broadcastLimit') or 'none'}\n")
    print("| Connections | Mem MiB | KiB/conn (avg) | KiB/conn (marginal) | CPU idle % | CPU broadcast avg/max % | p50 ms | p99 ms | max ms | Delivered | Accept conn/s |")
    print("|---|---|---|---|---|---|---|---|---|---|---|")
    for s in r['steps']:
        if 'memMiB' not in s or s.get('live', 0) == 0:
            if not s.get('stop'):
                continue  # the step's broadcast row after a crash; the stop row follows
            print(f"| {s['step']} | | | | | | | | | | {s.get('stop', '')} |")
            continue
        m = s.get('kbPerConnMarginal')
        print(f"| {s['live']:,} | {s['memMiB']:.0f} | {s['kbPerConn']:.1f} | {'' if m is None else f'{m:.1f}'} | {s['cpuIdle']:.0f} | {s['cpuBroadcast']:.0f}/{s['cpuBroadcastMax']:.0f} | {s['p50']} | {s['p99']} | {s['max']} | {s['delivered']*100:.1f} % | {s['acceptRate']:.0f} |")
