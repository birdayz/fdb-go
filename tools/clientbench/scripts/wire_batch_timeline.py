#!/usr/bin/env python3
"""Commit batch sizes over one wirecapture run, as runs of whole/split batches.

Usage: wire_batch_timeline.py <wirecapture.sh output dir> [workers=32]. A batch
is whole (W) when it carries one commit per worker, else split (S).
"""
import csv, sys
from collections import defaultdict
WORKERS=int(sys.argv[2]) if len(sys.argv)>2 else 32
ev=list(csv.DictReader(open(sys.argv[1]+'/events.csv')))
b=defaultdict(list)
for e in ev:
    if e['kind']=='commit' and int(e['reply_ns'])>0: b[int(e['version'])].append(e)
bs=sorted(b.values(), key=lambda x: min(int(e['sent_ns']) for e in x))
t0=min(int(e['sent_ns']) for e in bs[0])
sizes=[len(x) for x in bs]
# compress runs: show transitions
out=[]; prev=None; cnt=0
for i,s in enumerate(sizes):
    full = s==WORKERS
    if prev is None or full!=prev:
        if prev is not None: out.append(f"{'W' if prev else 'S'}x{cnt}")
        prev=full; cnt=0
    cnt+=1
out.append(f"{'W' if prev else 'S'}x{cnt}")
print(sys.argv[1].split('/')[-1], "first 40 sizes:", sizes[:40])
print("  whole(W)/split(S) runs:", ' '.join(out[:30]), '...' if len(out)>30 else '')
