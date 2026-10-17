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


def event_name(event):
    return event.split(":")[-1].replace("sys_enter_", "")


EXPECTED = [event_name(e) for e in EVENTS]


def parse_perf(lines, tids=None, roles=("client",)):
    """perf stat -x, output: per role when per-thread, else one total. Every role
    must have a numeric count of every event: a missing, unsupported or uncounted
    one is an error, not a zero. Per thread, <not counted> marks a thread that did
    not run and adds nothing, provided another thread of the role was counted."""
    out, counted = {}, set()
    for line in lines:
        f = line.strip().split(",")
        if tids is None:
            if len(f) < 3 or not f[2]:
                continue
            role, count, event = "client", f[0], f[2]
        else:
            # --per-thread: comm-tid,count,unit,event,...
            if len(f) < 4 or "-" not in f[0] or not f[3]:
                continue
            role, count, event = tids.get(int(f[0].rsplit("-", 1)[1]), "unknown"), f[1], f[3]
        event = event_name(event)
        out.setdefault(role, {}).setdefault(event, 0.0)
        if count == "<not counted>" and tids is not None:
            continue
        if not count.replace(".", "").isdigit():
            raise ValueError(f"{role}: {event} {count}")
        out[role][event] += float(count)
        counted.add((role, event))
    for role in roles:
        missing = [e for e in EXPECTED if (role, e) not in counted]
        if missing:
            raise ValueError(f"{role}: no count for {', '.join(missing)}")
    return out


def socket_counters(lines, ports, store):
    """Records segs_out/bytes_sent of each clientbench socket to a server port from
    `ss -tinpH` output, where each socket's counters follow on the next line."""
    addr = re.compile(r"\s*\d+\s+\d+\s+\S+:(\d+)\s+\S+:(\d+)")
    for line, info in zip(lines, lines[1:]):
        m = addr.match(line)
        if not m or int(m.group(2)) not in ports or "clientbench" not in line:
            continue
        segs = re.search(r"segs_out:(\d+)", info)
        sent = re.search(r"bytes_sent:(\d+)", info)
        if not segs or not sent:
            raise ValueError(f"ss reported no segs_out/bytes_sent for {line.strip()}")
        store[(m.group(1), int(m.group(2)))] = (int(segs.group(1)), int(sent.group(1)))


def sample_segments(stop, ports, store, errors):
    """Last seen counters of each clientbench socket, sampled every 100 ms."""
    try:
        while not stop.is_set():
            p = subprocess.run(["ss", "-tinpH", "state", "established"], capture_output=True, text=True, check=True)
            socket_counters(p.stdout.splitlines(), ports, store)
            time.sleep(0.1)
    except Exception as e:  # reported by the run, which must not pass with partial samples
        errors.append(e)


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
    store, stop, errors = {}, threading.Event(), []
    sampler = threading.Thread(target=sample_segments, args=(stop, {s["port"] for s in servers}, store, errors))
    sampler.start()
    try:
        start = time.time()
        p = subprocess.run(cmd, capture_output=True, text=True)
        end = time.time()
    finally:
        stop.set()
        sampler.join()
    if errors:
        raise errors[0]
    if p.returncode != 0:
        sys.exit(f"run failed (exit {p.returncode}):\n{p.stdout[-2000:]}\n{p.stderr[-2000:]}")
    summary = json.load(open(os.path.join(outdir, "summary.json")))
    if not summary.get("success"):
        sys.exit(f"clientbench reported failure: {summary.get('failure')}")
    with open(os.path.join(outdir, "samples.csv")) as f:
        latency = [int(r["latency_ns"]) for r in csv.DictReader(f)]
    counts = parse_perf(open(srv), tids, sorted({s["role"] for s in servers}))
    counts.update(parse_perf(open(cli)))
    if not store:
        raise ValueError("ss sampled no clientbench socket to a server")
    segments = {}
    for (_, port), (segs, sent) in store.items():
        total = segments.setdefault(port, [0, 0, 0])
        total[0] += segs
        total[1] += sent
        total[2] += 1
    return {"counts": counts, "segments": segments, "start": start, "end": end, "summary": summary,
            "mean_latency_us": statistics.fmean(latency) / 1e3}


def trace_series(server, lines, series):
    """Adds server's (time, total) series of each counter from trace-log lines,
    one per role instance (the event ID): a process can host several, each with
    its own totals."""
    for line in lines:
        for kind, names in COUNTERS.items():
            if f'Type="{kind}"' not in line:
                continue
            t = float(re.search(r' Time="([\d.]+)"', line).group(1))
            instance = re.search(r' ID="([^"]*)"', line).group(1)
            for name in names:
                m = re.search(rf' {name}="\S+ \S+ ([\d.e+]+)"', line)
                if m:
                    series.setdefault((server, instance, f"{kind}.{name}"), []).append((t, float(m.group(1))))


def server_trace_files(paths, port):
    """The server's own files among paths: trace.<address>.<port>.<start>.<id>.<n>.<m>.xml,
    so servers sharing a log directory never read each other's events."""
    pattern = re.compile(rf"^trace\..+\.{port}\.\d+\.[^.]+\.\d+\.\d+\.xml$")
    return [p for p in paths if pattern.match(os.path.basename(p))]


# Counters every server of a role reports, whatever else it hosts.
ROLE_COUNTERS = {"storage": "StorageMetrics"}


def trace_totals(servers):
    """(time, total) series of each counter, per server, from its two newest trace files."""
    series = {}
    for s in servers:
        paths = server_trace_files(glob.glob(os.path.join(s["logs"], "*.xml")), s["port"])
        if not paths:
            raise ValueError(f"server {s['pid']} ({s['role']}): no trace files in its log directory")
        for path in sorted(paths, key=os.path.getmtime)[-2:]:
            trace_series(s["pid"], open(path, errors="replace"), series)
    check_role_coverage(servers, series)
    return series


def check_role_coverage(servers, series):
    """Every server must report the counters of its role, so none is left out of a total."""
    for s in servers:
        kind = ROLE_COUNTERS.get(s["role"])
        if kind and not any(server == s["pid"] and name.startswith(kind + ".") for server, _, name in series):
            raise ValueError(f"server {s['pid']} ({s['role']}): no {kind} in its trace files")


def trace_deltas(series, start, end, cutoff):
    """Each counter's increase over the run [start, end], summed over servers. Every
    series needs a sample at or before start and one at or after end, taken by
    cutoff: stale, truncated or one-sided coverage is an error, as is a counter no
    server reported."""
    deltas = {}
    for (server, instance, name), values in series.items():
        first, last = at(values, start), at(values, cutoff)
        if first is None or last is None or last[0] < end:
            raise ValueError(f"server {server} {instance}: {name} has no samples on both sides of the run")
        deltas[name] = deltas.get(name, 0.0) + last[1] - first[1]
    missing = [f"{k}.{n}" for k, names in COUNTERS.items() for n in names if f"{k}.{n}" not in deltas]
    if missing:
        raise ValueError(f"no trace totals around the run for {', '.join(missing)}")
    return deltas


def at(series, t):
    """The last (time, total) sample at or before t."""
    before = [sample for sample in sorted(series) if sample[0] <= t]
    return before[-1] if before else None


def operations(summary):
    """Operations a run completed, warmup included: what its counters cover."""
    return summary["completed"] + summary["warmup_completed"]


def differences(short, long, tx):
    """(long - short) / tx of every count; both runs must have the same roles,
    events and server ports, so nothing absent is read as zero."""
    for what in ("counts", "segments"):
        if set(short[what]) != set(long[what]):
            raise ValueError(f"{what} differ between runs: {sorted(set(short[what]) ^ set(long[what]))}")
    row = {}
    for role in sorted(long["counts"]):
        a, b = short["counts"][role], long["counts"][role]
        if set(a) != set(b):
            raise ValueError(f"{role}: events differ between runs: {sorted(set(a) ^ set(b))}")
        for event in sorted(b):
            d = (b[event] - a[event]) / tx
            if abs(d) > 1e-4 or event == "cycles":
                row[f"{role}.{event}"] = d
    return row


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
    tx = operations(long["summary"]) - operations(short["summary"])
    if tx <= 0:
        sys.exit("the long run completed no more operations than the short run")
    row = {"label": args.label, "workload": args.workload, "workers": args.workers, "short": args.short,
           "long": args.long, "gogc": args.gogc, "env": args.env, "client_arg": args.client_arg}
    row.update(differences(short, long, tx))
    for port in sorted(long["segments"]):
        a, b = short["segments"][port], long["segments"][port]
        row[f"client_segs_to_{port}"] = (b[0] - a[0]) / tx
        row[f"client_bytes_to_{port}"] = (b[1] - a[1]) / tx
        row[f"client_conns_to_{port}"] = b[2]
    measured = operations(long["summary"])
    for name, delta in trace_deltas(series, long["start"], long["end"], long["end"] + TRACE_INTERVAL - 0.5).items():
        row[f"trace.{name}"] = delta / measured
    row["tx_per_s"] = long["summary"]["completed"] / (long["summary"]["measured_wall_ns"] / 1e9)
    row["mean_latency_us"] = long["mean_latency_us"]
    with open(args.out, "a") as f:
        f.write(json.dumps(row) + "\n")
    shown = [k for k, v in row.items() if isinstance(v, float)]
    print(args.label, args.workload, args.workers, " ".join(f"{k}={row[k]:.3f}" for k in shown))


if __name__ == "__main__":
    main()
