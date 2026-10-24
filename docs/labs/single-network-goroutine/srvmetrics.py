#!/usr/bin/env python3
"""Server trace-metric deltas over a time window, per transaction.

Usage: srvmetrics.py START_UNIX END_UNIX OPS LOGDIR...

Cumulative counters ("rate roughness total") use the last event at or before
START and the first at or after END. NetworkMetrics are per-interval values,
summed over intervals that end in (START, END + 5.5 s]; the windows are
5 s, so the caller leaves idle margins of at least 6 s on both sides.
"""

import glob
import json
import sys

CUMULATIVE = {
    "GrvProxyMetrics": ["TxnRequestIn", "TxnStartIn", "TxnStartBatch"],
    "ProxyMetrics": ["TxnCommitIn", "CommitBatchIn"],
    "StorageMetrics": ["QueryQueue", "GetValueQueries", "FinishedQueries"],
}
NETWORK = ["RunLoopIterations", "TasksExecuted", "ASIOEventsProcessed", "ReadCalls", "WriteCalls",
           "PacketsRead", "PacketsGenerated", "WouldBlock", "TimersExecuted"]


def events(logdirs):
    for d in logdirs:
        for path in sorted(glob.glob(f"{d}/trace.*.json")):
            for line in open(path):
                try:
                    e = json.loads(line)
                except ValueError:
                    continue
                if e.get("Type") in CUMULATIVE or e.get("Type") == "NetworkMetrics":
                    yield d, e


def main():
    start, end, ops = float(sys.argv[1]), float(sys.argv[2]), int(sys.argv[3])
    before, after, net = {}, {}, {}
    for d, e in events(sys.argv[4:]):
        t, typ = float(e["Time"]), e["Type"]
        role = d.rstrip("/").split("/")[-2]
        if typ == "NetworkMetrics":
            if start < t <= end + 5.5:
                acc = net.setdefault(role, {k: 0.0 for k in NETWORK})
                for k in NETWORK:
                    acc[k] += float(e.get(k, 0))
            continue
        key = (role, typ, e["ID"])
        vals = {k: float(e[k].split()[2]) for k in CUMULATIVE[typ] if k in e}
        if t <= start:
            before[key] = vals
        elif t >= end and key not in after:
            after[key] = vals
    out = {}
    for key, b in before.items():
        if key in after:
            role, typ, _ = key
            for k, v in b.items():
                name = f"{role}.{typ}.{k}"
                out[name] = out.get(name, 0) + (after[key][k] - v) / ops
    for role, acc in net.items():
        for k, v in acc.items():
            out[f"{role}.Network.{k}"] = v / ops
    print(json.dumps(out, indent=1, sort_keys=True))


if __name__ == "__main__":
    main()
