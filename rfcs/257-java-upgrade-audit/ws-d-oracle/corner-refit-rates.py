#!/usr/bin/env python3
"""Every GUARDIANN-PEEL-CORNER(-TIMING) pair of every log given: the per-n*d refit rate of
each refit maximum, and each peel's refit total, mechanically, over ALL timing lines."""
import re, sys, statistics
shape_re = re.compile(r'GUARDIANN-PEEL-CORNER (\S+) n=(\d+) d=(\d+)')
tim_re = re.compile(r'GUARDIANN-PEEL-CORNER-TIMING (\S+) (.*)')
seed_re = re.compile(r'(\d+):fit=([\d.]+)ms refitTotal=([\d.]+)ms refitMax=([\d.]+)ms')
rows = []
for path in sys.argv[1:]:
    shapes = {}
    for line in open(path, errors='replace'):
        m = shape_re.search(line)
        if m:
            shapes[m.group(1)] = (int(m.group(2)), int(m.group(3)))
            continue
        m = tim_re.search(line)
        if m and m.group(1) in shapes:
            n, d = shapes[m.group(1)]
            for s in seed_re.finditer(m.group(2)):
                fit, tot, mx = float(s.group(2)), float(s.group(3)), float(s.group(4))
                rows.append((path, m.group(1), n, d, int(s.group(1)), fit, tot, mx))
print(f"{len(rows)} (shape, seed) timings over {len(sys.argv)-1} logs")
rates = [(mx/1000.0/(n*d), log, name, n, d, seed, mx) for log, name, n, d, seed, fit, tot, mx in rows if mx > 0]
rates.sort()
print(f"refit-max rates (s per n*d) over {len(rates)} nonzero: min {rates[0][0]:.3g} median {statistics.median(r[0] for r in rates):.3g} max {rates[-1][0]:.3g}")
for r in rates[-5:]:
    print(f"  top: {r[0]:.3g} {r[1]} {r[2]} n={r[3]} d={r[4]} seed={r[5]} refitMax={r[6]}ms")
tots = sorted(rows, key=lambda r: r[6])
print(f"refit totals: max {tots[-1][6]}ms ({tots[-1][0]} {tots[-1][1]} seed {tots[-1][4]})")
fits = sorted(rows, key=lambda r: r[5])
print(f"target fit: min {fits[0][5]}ms max {fits[-1][5]}ms")
per_log = {}
for r in rates:
    per_log.setdefault(r[1], []).append(r[0])
for log in sorted(per_log):
    v = per_log[log]
    print(f"  {log}: {len(v)} rates, max {max(v):.3g}, median {statistics.median(v):.3g}")
