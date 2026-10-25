#!/usr/bin/env python3
"""One benchmark cell for the single-network-goroutine lab.

For each arm in --order, runs clientbench twice (short and long iteration
counts) under `perf stat -p <fdbserver pids>`. Server cycles per transaction are
the long-minus-short difference (as tools/clientbench/scripts/servercost.py);
client metrics come from the long run alone: ops/s over the measured window,
latency p50/p99 from samples.csv, errors and retries, and client CPU/op from the
harness's getrusage snapshots around the measured window. Before every run it
waits (at most 30 min) for the 1-minute load average to fall to --load-max,
and records it. It also measures the CPU other processes used on the client
and server CPUs and their SMT siblings during each run ("foreign cores"). A run
above --foreign-max is redone (up to --retries times). Rejected attempts go to
<out>-rejected.jsonl; the last attempt is kept with clean=false. One JSON line
per arm run is appended to --out.

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
import resource
import tempfile
import time

# A = today's design; B = the network goroutine; W = variant (a), one writer
# goroutine per process with per-connection readLoops; S = variant (b), today's
# design with the coalescing delay timed from the sender.
ARMS = {
    "A": [],
    "B": ["FDBGO_NETWORK_GOROUTINE=1"],
    "W": ["FDBGO_NETWORK_GOROUTINE=writer"],
    "S": ["FDBGO_COALESCE_FROM_SENDER=1"],
}
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


def arm_spec(arm):
    """An arm is a list of env assignments, or a dict with env, args, gomaxprocs, cpus."""
    spec = ARMS[arm]
    return spec if isinstance(spec, dict) else {"env": spec}


def cpu_list(spec):
    out = set()
    for part in spec.split(","):
        lo, _, hi = part.partition("-")
        out |= set(range(int(lo), int(hi or lo) + 1))
    return out


# Client and server CPUs and their SMT siblings (NETG_MY_CPUS, e.g. 2-15).
MY_CPUS = cpu_list(os.environ.get("NETG_MY_CPUS", "1-11,13-23"))


def cpu_busy(cpus):
    """Busy jiffies of the given CPUs from /proc/stat."""
    total = 0
    for line in open("/proc/stat"):
        if line.startswith("cpu") and line[3].isdigit():
            f = line.split()
            if int(f[0][3:]) in cpus:
                v = list(map(int, f[1:]))
                # user + nice + system: irq/softirq time (loopback TCP of our own run) is
                # not charged to any process, so it would read as another tenant's.
                total += v[0] + v[1] + v[2]
    return total


def proc_cpu(pids):
    t = 0
    for pid in pids:
        f = open(f"/proc/{pid}/stat").read().rsplit(")", 1)[1].split()
        t += int(f[11]) + int(f[12])
    return t


def run(args, arm, iterations, outdir, perf_target):
    spec = arm_spec(arm)
    env = ["PATH=/usr/bin:/bin", f"HOME={os.environ.get('HOME', '/')}",
           f"GOMAXPROCS={spec.get('gomaxprocs', args.gomaxprocs)}",
           "GOGC=100", "GOMEMLIMIT=off", *spec.get("env", [])]
    client = ["env", "-i", *env, "taskset", "-c", spec.get("cpus", args.client_cpus), spec.get("binary", args.binary),
              "-cluster-file", args.cluster_file, "-workload", args.workload, "-workers", str(args.workers),
              "-iterations", str(iterations), "-warmup", str(iterations // 20), "-allow-writes", "-output", outdir,
              *spec.get("args", [])]
    if perf_target == "server":
        cmd = ["perf", "stat", "-x,", "-p", args.server_pids, "-e", "task-clock,cycles,instructions", "--", *client]
        want = 3
    else:
        cmd = ["perf", "stat", "-x,", "-e", ",".join(CLIENT_EVENTS), "--", *client]
        want = len(CLIENT_EVENTS)
    waited = wait_quiet(args.load_max, args.max_wait)
    la = load1()
    hz = os.sysconf("SC_CLK_TCK")
    pids = [int(x) for x in args.server_pids.split(",")]
    b0, s0, c0, t0 = cpu_busy(MY_CPUS), proc_cpu(pids), resource.getrusage(resource.RUSAGE_CHILDREN), time.time()
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=600)
    b1, s1, c1, t1 = cpu_busy(MY_CPUS), proc_cpu(pids), resource.getrusage(resource.RUSAGE_CHILDREN), time.time()
    mine = (c1.ru_utime + c1.ru_stime - c0.ru_utime - c0.ru_stime) + (s1 - s0) / hz
    foreign = max(0.0, ((b1 - b0) / hz - mine) / (t1 - t0))
    counters = {}
    for line in p.stderr.splitlines():
        f = line.split(",")
        if len(f) > 2 and f[0] and f[0][0].isdigit():
            counters[f[2]] = float(f[0])
    if p.returncode != 0 or len(counters) != want:
        sys.exit(f"run failed (exit {p.returncode}):\n{p.stdout[-2000:]}\n{p.stderr[-2000:]}")
    s = json.load(open(os.path.join(outdir, "summary.json")))
    for line in p.stderr.splitlines():  # lab writer counters (FDBGO_NETG_STATS=1)
        if line.startswith("netgstats "):
            kv = dict(f.split("=") for f in line.split()[1:])
            s["lab_writes"], s["lab_frames"] = int(kv["writes"]), int(kv["frames"])
    if not s.get("success"):
        sys.exit(f"clientbench failure: {s.get('failure')}")
    return counters, s, (la, waited, foreign)


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
    ap.add_argument("--round-len", type=int, default=2, help="slots per pairing round (one of each arm)")
    ap.add_argument("--round-base", type=int, default=0)
    ap.add_argument("--client-cpus", default="1-8")
    ap.add_argument("--gomaxprocs", type=int, default=8)
    ap.add_argument("--syscalls", action="store_true")
    ap.add_argument("--load-max", type=float, default=16.0, help="wait for load1 <= this before each run")
    ap.add_argument("--foreign-max", type=float, default=1.0,
                    help="other tenants' busy cores on the client/server CPUs and their siblings; above this a run is redone")
    ap.add_argument("--retries", type=int, default=3, help="redos of a contended run; the last attempt is kept regardless")
    ap.add_argument("--max-wait", type=int, default=1800, help="seconds to wait for --load-max before running anyway")
    ap.add_argument("--out", required=True)
    ap.add_argument("--arms", help="JSON file of arm label -> spec, replacing the built-in arms")
    args = ap.parse_args()
    if args.arms:
        ARMS.clear()
        ARMS.update(json.load(open(args.arms)))
    order = args.order.split(",") if "," in args.order else list(args.order)
    for i, arm in enumerate(order):
        for attempt in range(args.retries + 1):
            row = measure(args, arm, i, attempt)
            clean = row["foreign_cores"] <= args.foreign_max
            row["clean"] = clean
            out = args.out if clean or attempt == args.retries else args.out.replace(".jsonl", "") + "-rejected.jsonl"
            with open(out, "a") as f:
                f.write(json.dumps(row) + "\n")
            print(json.dumps({k: (round(v, 2) if isinstance(v, float) else v) for k, v in row.items()}), flush=True)
            if clean:
                break


def measure(args, arm, i, attempt):
    tmp = tempfile.mkdtemp(prefix="netg-")
    try:
        target = "client" if args.syscalls else "server"
        cs, ss, la_s = run(args, arm, args.short, os.path.join(tmp, "short"), target)
        cl, sl, la_l = run(args, arm, args.long, os.path.join(tmp, "long"), target)
        tx = (sl["completed"] + sl["warmup_completed"]) - (ss["completed"] + ss["warmup_completed"])
        row = {"workload": args.workload, "workers": args.workers, "arm": arm, "slot": i,
               "round": args.round_base + i // args.round_len, "attempt": attempt,
               "load1_before_short": la_s[0], "load1_before_long": la_l[0],
               "waited_s": la_s[1] + la_l[1], "foreign_cores": max(la_s[2], la_l[2]),
               "foreign_cores_long": la_l[2]}
        if args.syscalls:
            row.update({k.split(":")[-1].replace("sys_enter_", "") + "_per_op": (cl[k] - cs[k]) / tx for k in cl})
        else:
            row["server_cycles_per_tx"] = (cl["cycles"] - cs["cycles"]) / tx
            row["server_task_clock_us_per_tx"] = (cl["task-clock"] - cs["task-clock"]) * 1e3 / tx
        row.update(client_metrics(os.path.join(tmp, "long"), sl))
        if sl.get("lab_writes"):
            row["lab_frames_per_write"] = sl["lab_frames"] / sl["lab_writes"]
        return row
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

if __name__ == "__main__":
    main()
