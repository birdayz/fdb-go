#!/usr/bin/env python3
"""Per-transaction wire and server work of one clientbench configuration.

Like servercost.py, runs clientbench with a short and a long iteration count.
These are (long - short) / extra transactions, so setup cancels out:

- cycles and syscalls by type for each fdbserver process, keyed by its
  --class (perf stat --per-thread), and for the client process tree;
- the client's TCP segments and bytes to each server port: the last values
  ss reported for each clientbench socket, sampled every 100 ms while the
  client runs, so sends after the last sample are missed.

From the long run only:

- server request counters (GrvProxyMetrics, ProxyMetrics, StorageMetrics
  totals) from each server's trace logs around the run, summed over servers,
  per measured transaction including warmup;
- throughput and mean latency.

Each server's role, port and trace-log directory come from its command line
(--class, -p, -L). The servers must be otherwise idle and pinned away from
--client-cpus; perf must be allowed to attach to them and to read the
syscalls tracepoints. One JSON object is appended to --out.

Example:
  wirediff.py --server-pid 101,102,103 --binary clientbench_go \\
      --cluster-file fdb.cluster --workload get100 --workers 32 \\
      --short 15000 --long 45000 --label go-r0 --out wire.jsonl
"""

import argparse
import csv
import glob
import json
import os
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import threading
import time

SYSCALLS = ["read", "readv", "recvfrom", "recvmsg", "write", "writev", "sendto", "sendmsg",
            "epoll_wait", "epoll_pwait", "epoll_ctl", "futex", "nanosleep", "clock_nanosleep",
            "sched_yield", "io_submit", "io_getevents", "fdatasync", "pwrite64"]
EVENTS = ["cycles"] + [f"syscalls:sys_enter_{s}" for s in SYSCALLS]
COUNTERS = {
    "GrvProxyMetrics": ["TxnRequestIn", "TxnStartIn", "TxnStartBatch"],
    "ProxyMetrics": ["TxnCommitIn", "CommitBatchIn", "Mutations", "ConflictRanges"],
    "StorageMetrics": ["QueryQueue", "GetValueQueries", "GetRangeQueries"],
}
TRACE_INTERVAL = 11  # seconds: covers one 5 s trace-metrics interval and its flush


def server_roles(pids):
    """Role, port and log directory of each server; thread id -> role."""
    servers, tids = [], {}
    for pid in pids:
        argv = open(f"/proc/{pid}/cmdline").read().split("\x00")
        opt = lambda *names: next(argv[i + 1] for i, a in enumerate(argv) if a in names)
        role = opt("--class", "-c")
        port = int(opt("-p", "--public-address").rsplit(":", 1)[1])
        servers.append({"pid": pid, "role": role, "port": port, "logs": opt("-L", "--logdir")})
        for tid in os.listdir(f"/proc/{pid}/task"):
            tids[int(tid)] = role
    return servers, tids


def parse_perf(path, tids=None):
    """perf stat -x, output: per role when per-thread, else one total."""
    out = {}
    for line in open(path):
        f = line.strip().split(",")
        if tids is None:
            if len(f) > 2 and f[0].replace(".", "").isdigit():
                role, count, event = "client", f[0], f[2]
            else:
                continue
        else:
            # --per-thread: comm-tid,count,unit,event,...
            if len(f) < 4 or not f[1].replace(".", "").isdigit() or "-" not in f[0]:
                continue
            role, count, event = tids.get(int(f[0].rsplit("-", 1)[1]), "unknown"), f[1], f[3]
        event = event.split(":")[-1].replace("sys_enter_", "")
        out.setdefault(role, {}).setdefault(event, 0.0)
        out[role][event] += float(count)
    return out


def sample_segments(stop, ports, store):
    """Last seen segs_out/bytes_sent of each clientbench socket to a server port."""
    addr = re.compile(r"\s*\d+\s+\d+\s+\S+:(\d+)\s+\S+:(\d+)")
    while not stop.is_set():
        lines = subprocess.run(["ss", "-tinpH", "state", "established"],
                               capture_output=True, text=True).stdout.splitlines()
        for line, info in zip(lines, lines[1:]):
            m = addr.match(line)
            if not m or int(m.group(2)) not in ports or "clientbench" not in line:
                continue
            segs = re.search(r"segs_out:(\d+)", info)
            sent = re.search(r"bytes_sent:(\d+)", info)
            if segs:
                store[(m.group(1), int(m.group(2)))] = (int(segs.group(1)), int(sent.group(1)) if sent else 0)
        time.sleep(0.1)


def run_once(args, iterations, outdir, servers, tids):
    env = {"PATH": "/usr/bin:/bin", "HOME": os.environ.get("HOME", "/"), "GOMAXPROCS": str(args.gomaxprocs),
           "GOGC": args.gogc, "GOMEMLIMIT": "off"}
    if args.ld_library_path:
        env["LD_LIBRARY_PATH"] = args.ld_library_path
    for kv in args.env:
        k, _, v = kv.partition("=")
        env[k] = v
    events = ",".join(EVENTS)
    srv, cli = outdir + ".server", outdir + ".client"
    cmd = ["perf", "stat", "-x,", "--per-thread", "-p", ",".join(s["pid"] for s in servers), "-e", events, "-o", srv, "--",
           "perf", "stat", "-x,", "-e", events, "-o", cli, "--",
           "env", "-i", *[f"{k}={v}" for k, v in env.items()], "taskset", "-c", args.client_cpus, args.binary,
           "-cluster-file", args.cluster_file, "-workload", args.workload, "-workers", str(args.workers),
           "-iterations", str(iterations), "-warmup", str(iterations // 20), "-allow-writes", "-output", outdir,
           *args.client_arg]
    store, stop = {}, threading.Event()
    sampler = threading.Thread(target=sample_segments, args=(stop, {s["port"] for s in servers}, store))
    sampler.start()
    start = time.time()
    p = subprocess.run(cmd, capture_output=True, text=True)
    end = time.time()
    stop.set()
    sampler.join()
    if p.returncode != 0:
        sys.exit(f"run failed (exit {p.returncode}):\n{p.stdout[-2000:]}\n{p.stderr[-2000:]}")
    summary = json.load(open(os.path.join(outdir, "summary.json")))
    if not summary.get("success"):
        sys.exit(f"clientbench reported failure: {summary.get('failure')}")
    with open(os.path.join(outdir, "samples.csv")) as f:
        latency = [int(r["latency_ns"]) for r in csv.DictReader(f)]
    counts = parse_perf(srv, tids)
    counts.update(parse_perf(cli))
    segments = {}
    for (_, port), (segs, sent) in store.items():
        total = segments.setdefault(port, [0, 0, 0])
        total[0] += segs
        total[1] += sent
        total[2] += 1
    return {"counts": counts, "segments": segments, "start": start, "end": end, "summary": summary,
            "mean_latency_us": statistics.fmean(latency) / 1e3}


def trace_totals(servers):
    """(time, total) series of each counter, per server, from its two newest trace files."""
    series = {}
    for s in servers:
        for path in sorted(glob.glob(os.path.join(s["logs"], "*.xml")), key=os.path.getmtime)[-2:]:
            for line in open(path, errors="replace"):
                for kind, names in COUNTERS.items():
                    if f'Type="{kind}"' not in line:
                        continue
                    t = float(re.search(r' Time="([\d.]+)"', line).group(1))
                    for name in names:
                        m = re.search(rf' {name}="\S+ \S+ ([\d.e+]+)"', line)
                        if m:
                            series.setdefault((s["pid"], f"{kind}.{name}"), []).append((t, float(m.group(1))))
    return series


def at(series, t):
    before = [v for ts, v in sorted(series) if ts <= t]
    return before[-1] if before else None


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--binary", required=True)
    ap.add_argument("--cluster-file", required=True)
    ap.add_argument("--workload", required=True)
    ap.add_argument("--workers", type=int, required=True)
    ap.add_argument("--short", type=int, required=True, help="iterations of the short run")
    ap.add_argument("--long", type=int, required=True, help="iterations of the long run")
    ap.add_argument("--label", required=True)
    ap.add_argument("--out", required=True, help="JSON lines file to append to")
    ap.add_argument("--server-pid", required=True, help="comma-separated PIDs of every server process")
    ap.add_argument("--client-cpus", default="0-7")
    ap.add_argument("--gomaxprocs", type=int, default=8)
    ap.add_argument("--gogc", default="100")
    ap.add_argument("--ld-library-path", default="", help="for the libfdb_c (CGo) binary")
    ap.add_argument("--env", action="append", default=[], help="extra KEY=VALUE for the client")
    ap.add_argument("--client-arg", action="append", default=[], help="extra clientbench argument")
    args = ap.parse_args()
    if args.long <= args.short:
        sys.exit("--long must exceed --short")
    servers, tids = server_roles(args.server_pid.split(","))
    tmp = tempfile.mkdtemp(prefix="wirediff-")
    try:
        short = run_once(args, args.short, os.path.join(tmp, "short"), servers, tids)
        time.sleep(TRACE_INTERVAL)  # keep the short run out of the long run's trace interval
        long = run_once(args, args.long, os.path.join(tmp, "long"), servers, tids)
        time.sleep(TRACE_INTERVAL)
        series = trace_totals(servers)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    tx = (args.long - args.short) * 21 / 20  # warmup is iterations/20
    row = {"label": args.label, "workload": args.workload, "workers": args.workers, "short": args.short,
           "long": args.long, "gogc": args.gogc, "env": args.env, "client_arg": args.client_arg}
    for role in sorted(set(short["counts"]) | set(long["counts"])):
        a, b = short["counts"].get(role, {}), long["counts"].get(role, {})
        for event in sorted(set(a) | set(b)):
            d = (b.get(event, 0) - a.get(event, 0)) / tx
            if abs(d) > 1e-4 or event == "cycles":
                row[f"{role}.{event}"] = d
    for port in sorted(set(short["segments"]) | set(long["segments"])):
        a, b = short["segments"].get(port, [0, 0, 0]), long["segments"].get(port, [0, 0, 0])
        row[f"client_segs_to_{port}"] = (b[0] - a[0]) / tx
        row[f"client_bytes_to_{port}"] = (b[1] - a[1]) / tx
        row[f"client_conns_to_{port}"] = b[2]
    measured = long["summary"]["completed"] * 21 / 20
    deltas = {}
    for (_, name), values in series.items():
        v0, v1 = at(values, long["start"]), at(values, long["end"] + TRACE_INTERVAL - 0.5)
        if v0 is not None and v1 is not None:
            deltas[name] = deltas.get(name, 0.0) + v1 - v0
    for name, delta in deltas.items():
        row[f"trace.{name}"] = delta / measured
    row["tx_per_s"] = long["summary"]["completed"] / (long["summary"]["measured_wall_ns"] / 1e9)
    row["mean_latency_us"] = long["mean_latency_us"]
    with open(args.out, "a") as f:
        f.write(json.dumps(row) + "\n")
    shown = [k for k, v in row.items() if isinstance(v, float)]
    print(args.label, args.workload, args.workers, " ".join(f"{k}={row[k]:.3f}" for k in shown))


if __name__ == "__main__":
    main()
