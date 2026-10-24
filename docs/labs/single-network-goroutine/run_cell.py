#!/usr/bin/env python3
"""One benchmark cell for the single-network-goroutine lab.

For each arm in --order, runs clientbench twice (short and long iteration
counts) under `perf stat -p <fdbserver pids>`. Server cycles per transaction are
the long-minus-short difference (as tools/clientbench/scripts/servercost.py);
client metrics come from the long run alone: ops/s over the measured window,
latency p50/p99 from samples.csv, errors and retries, and client CPU/op from the
harness's getrusage snapshots around the measured window. Before every run it
waits (at most 30 min) for the 1-minute load average to fall to --load-max,
and records it. One JSON line per arm run is appended to --out.

--syscalls instead runs one client-side `perf stat` per arm with syscall
tracepoints and reports per-operation counts (short/long difference).
"""

import argparse
import csv
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time

ARMS = {"A": "0", "B": "1"}  # A = today's design, B = FDBGO_NETWORK_GOROUTINE=1
CLIENT_EVENTS = ["raw_syscalls:sys_enter", "syscalls:sys_enter_futex", "syscalls:sys_enter_epoll_pwait",
                 "syscalls:sys_enter_epoll_wait", "syscalls:sys_enter_nanosleep", "syscalls:sys_enter_read",
                 "syscalls:sys_enter_write", "syscalls:sys_enter_timerfd_settime", "context-switches"]


def load1():
    return float(open("/proc/loadavg").read().split()[0])


def wait_quiet(limit, max_wait=1800):
    """Shared machine: wait (bounded) until the 1-minute load average is at most limit."""
    waited = 0
    while load1() > limit and waited < max_wait:
        time.sleep(10)
        waited += 10
    return waited


def run(args, arm, iterations, outdir, perf_target):
    env = ["PATH=/usr/bin:/bin", f"HOME={os.environ.get('HOME', '/')}", f"GOMAXPROCS={args.gomaxprocs}",
           "GOGC=100", "GOMEMLIMIT=off", f"FDBGO_NETWORK_GOROUTINE={ARMS[arm]}"]
    client = ["env", "-i", *env, "taskset", "-c", args.client_cpus, args.binary,
              "-cluster-file", args.cluster_file, "-workload", args.workload, "-workers", str(args.workers),
              "-iterations", str(iterations), "-warmup", str(iterations // 20), "-allow-writes", "-output", outdir]
    if perf_target == "server":
        cmd = ["perf", "stat", "-x,", "-p", args.server_pids, "-e", "task-clock,cycles,instructions", "--", *client]
        want = 3
    else:
        cmd = ["perf", "stat", "-x,", "-e", ",".join(CLIENT_EVENTS), "--", *client]
        want = len(CLIENT_EVENTS)
    waited = wait_quiet(args.load_max)
    la = load1()
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=600)
    counters = {}
    for line in p.stderr.splitlines():
        f = line.split(",")
        if len(f) > 2 and f[0] and f[0][0].isdigit():
            counters[f[2]] = float(f[0])
    if p.returncode != 0 or len(counters) != want:
        sys.exit(f"run failed (exit {p.returncode}):\n{p.stdout[-2000:]}\n{p.stderr[-2000:]}")
    s = json.load(open(os.path.join(outdir, "summary.json")))
    if not s.get("success"):
        sys.exit(f"clientbench failure: {s.get('failure')}")
    return counters, s, (la, waited)


def client_metrics(outdir, s):
    n = s["completed"]
    b, a = s["stats_before"], s["stats_after"]
    cpu = lambda x: x["process_user_cpu_us"] + x["process_system_cpu_us"]
    with open(os.path.join(outdir, "samples.csv")) as f:
        lat = sorted(int(r["latency_ns"]) for r in csv.DictReader(f) if r["phase"] == "measure" and not r["error"])
    wall = s["measured_wall_ns"] / 1e9
    return {
        "ops_per_s": n / wall,
        "p50_us": lat[len(lat) // 2] / 1e3,
        "p99_us": lat[int(len(lat) * 0.99)] / 1e3,
        "errors": s["errors"], "retries": s["retries"], "completed": n,
        "client_cpu_us_per_op": (cpu(a) - cpu(b)) / n,
        "client_cores_busy": (cpu(a) - cpu(b)) / 1e6 / wall,
        "user_cpu_us_per_op": (a["process_user_cpu_us"] - b["process_user_cpu_us"]) / n,
        "sys_cpu_us_per_op": (a["process_system_cpu_us"] - b["process_system_cpu_us"]) / n,
        "mallocs_per_op": (a["go_mallocs"] - b["go_mallocs"]) / n,
        "gc_cycles": a["go_gc_cycles"] - b["go_gc_cycles"],
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--binary", required=True)
    ap.add_argument("--cluster-file", required=True)
    ap.add_argument("--server-pids", required=True)
    ap.add_argument("--workload", required=True)
    ap.add_argument("--workers", type=int, required=True)
    ap.add_argument("--short", type=int, required=True)
    ap.add_argument("--long", type=int, required=True)
    ap.add_argument("--order", default="ABAB")
    ap.add_argument("--client-cpus", default="1-8")
    ap.add_argument("--gomaxprocs", type=int, default=8)
    ap.add_argument("--syscalls", action="store_true")
    ap.add_argument("--load-max", type=float, default=16.0, help="wait for load1 <= this before each run")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    for i, arm in enumerate(args.order):
        tmp = tempfile.mkdtemp(prefix="netg-")
        try:
            target = "client" if args.syscalls else "server"
            cs, ss, la_s = run(args, arm, args.short, os.path.join(tmp, "short"), target)
            cl, sl, la_l = run(args, arm, args.long, os.path.join(tmp, "long"), target)
            tx = (sl["completed"] + sl["warmup_completed"]) - (ss["completed"] + ss["warmup_completed"])
            row = {"workload": args.workload, "workers": args.workers, "arm": arm, "slot": i,
                   "load1_before_short": la_s[0], "load1_before_long": la_l[0],
                   "waited_s": la_s[1] + la_l[1]}
            if args.syscalls:
                row.update({k.split(":")[-1].replace("sys_enter_", "") + "_per_op": (cl[k] - cs[k]) / tx for k in cl})
            else:
                row["server_cycles_per_tx"] = (cl["cycles"] - cs["cycles"]) / tx
                row["server_task_clock_us_per_tx"] = (cl["task-clock"] - cs["task-clock"]) * 1e3 / tx
            row.update(client_metrics(os.path.join(tmp, "long"), sl))
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
        with open(args.out, "a") as f:
            f.write(json.dumps(row) + "\n")
        print(json.dumps({k: (round(v, 2) if isinstance(v, float) else v) for k, v in row.items()}), flush=True)


if __name__ == "__main__":
    main()
