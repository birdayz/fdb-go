#!/usr/bin/env python3
"""Line up a closed-loop run's commit batches with its Go GC starts.

Reads one wirecapture.sh output directory, captured with GODEBUG=gctrace=1 and
a start_ns file holding the wall-clock nanoseconds just before the client was
exec'd (gctrace times are relative to process start). Prints JSON with:

- gc_start_ms: each GC's start, ms after process start;
- batches: for each commit batch (successful commits sharing a version), in order
  of its first commit, [first commit sent ms after process start, commit count];
- first_split: index of the first batch after a whole one (round_size commits)
  that is not whole, and how long before it the most recent GC started.

Usage: gcalign.py DIR ROUND_SIZE
"""

import csv
import json
import re
import sys
from collections import defaultdict


def analyze(d, round_size):
    start = int(open(d + "/start_ns").read())
    gcs = []
    for line in open(d + "/clientbench.log"):
        m = re.match(r"gc \d+ @([0-9.]+)s ", line)
        if m:
            gcs.append(start + int(float(m.group(1)) * 1e9))
    batches = defaultdict(list)
    for e in csv.DictReader(open(d + "/events.csv")):
        if e["kind"] == "commit" and int(e["reply_ns"]) > 0 and e["error"] == "0":
            batches[int(e["version"])].append(int(e["sent_ns"]))
    ordered = sorted(batches.values(), key=min)
    ms = lambda t: round((t - start) / 1e6, 3)
    out = {
        "gc_start_ms": [ms(g) for g in gcs],
        "batches": [[ms(min(b)), len(b)] for b in ordered],
    }
    seen = False
    for i, b in enumerate(ordered):
        if len(b) == round_size:
            seen = True
        elif seen:
            t = min(b)
            before = [g for g in gcs if g <= t]
            out["first_split"] = {
                "index": i,
                "last_gc_ms_before": round(ms(t) - ms(before[-1]), 3) if before else None,
                "gcs_in_previous_round": sum(1 for g in gcs if min(ordered[i - 1]) <= g <= t),
                "gcs_during_whole_batches": sum(1 for g in gcs if min(ordered[0]) <= g < min(ordered[i - 1])),
            }
            break
    return out


if __name__ == "__main__":
    print(json.dumps(analyze(sys.argv[1], int(sys.argv[2]))))
