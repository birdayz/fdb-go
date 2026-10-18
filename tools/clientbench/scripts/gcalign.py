#!/usr/bin/env python3
"""Line up a closed-loop run's commit batches with its Go GC starts.

Reads one wirecapture.sh output directory of a client run with GODEBUG=gctrace=1.
Its start_ns file holds the wall-clock nanoseconds just before the client command
started (gctrace times are relative to process start). The alignment carries the
uncalibrated exec-to-start offset and gctrace's millisecond resolution. Prints
JSON with:

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
    with open(d + "/start_ns") as f:
        start = int(f.read())
    gcs = []
    with open(d + "/clientbench.log") as f:
        for line in f:
            m = re.match(r"gc \d+ @([0-9.]+)s ", line)
            if m:
                gcs.append(start + int(float(m.group(1)) * 1e9))
    batches = defaultdict(list)
    with open(d + "/events.csv") as f:
        events = list(csv.DictReader(f))
    for e in events:
        # Successful commits only: answered, no error, decoded (malformed column
        # required, so an older events file fails instead of counting failures).
        if e["kind"] == "commit" and int(e["reply_ns"]) > 0 and e["error"] == "0" and e["malformed"] == "false":
            batches[int(e["version"])].append(int(e["sent_ns"]))
    ordered = sorted(batches.values(), key=min)
    ms = lambda t: round((t - start) / 1e6, 3)
    out = {
        "gc_start_ms": [ms(g) for g in gcs],
        "batches": [[ms(min(b)), len(b)] for b in ordered],
    }
    first_whole = None
    for i, b in enumerate(ordered):
        if len(b) == round_size:
            if first_whole is None:
                first_whole = i
        elif first_whole is not None:
            t = min(b)
            before = [g for g in gcs if g <= t]
            out["first_split"] = {
                "index": i,
                "last_gc_ms_before": round(ms(t) - ms(before[-1]), 3) if before else None,
                "gcs_in_previous_round": sum(1 for g in gcs if min(ordered[i - 1]) <= g <= t),
                # From the first whole batch, not setup or warmup batches before it.
                "gcs_during_whole_batches": sum(1 for g in gcs if min(ordered[first_whole]) <= g < min(ordered[i - 1])),
            }
            break
    return out


if __name__ == "__main__":
    print(json.dumps(analyze(sys.argv[1], int(sys.argv[2]))))
