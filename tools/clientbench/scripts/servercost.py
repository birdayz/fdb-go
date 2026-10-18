#!/usr/bin/env python3
"""Server CPU per transaction for one clientbench configuration.

Runs clientbench twice per repetition, with a short and a long iteration count,
under `perf stat -p <fdbserver pid>`. The difference between the two runs,
divided by the difference in transactions, is the marginal server cost per
transaction: setup, connection and validation work cancel out. Client CPU,
latency and throughput come from the long run's summary.

The server must already be running, on CPUs disjoint from --client-cpus, and
`perf stat` must be allowed to attach to it (all processes of a multi-process
cluster are counted together; pass their PIDs with --server-pid) (kernel.perf_event_paranoid <= 1
for a process of the same user). One JSON object per repetition is appended to
--out; the median of each metric is printed.

--no-syscall-count drops the raw_syscalls tracepoint and counts cycles,
instructions and task-clock only. A tracepoint on every server syscall slows
the servers enough to change how closed-loop rounds batch; use this mode when
comparing batching-sensitive workloads. Per-transaction values divide by the
operations both runs completed, warmup included.

Example:
  servercost.py --binary bazel-bin/.../clientbench_go --cluster-file fdb.cluster \\
      --workload get100 --workers 8 --short 15000 --long 45000 --reps 3 \\
      --label go-base --out results.jsonl
"""

import argparse
import csv
import json
import os
import shutil
import statistics
import subprocess
import sys
import tempfile

EVENTS = ["task-clock", "cycles", "instructions", "raw_syscalls:sys_enter"]


def server_pid(name):
    out = subprocess.run(["pgrep", "-u", str(os.getuid()), "-x", name], capture_output=True, text=True).stdout.split()
    if len(out) != 1:
        sys.exit(f"want exactly one {name} process of this user, found {len(out)}; pass --server-pid")
    return int(out[0])


def run_once(args, pid, iterations, outdir):
    events = EVENTS[:3] if args.no_syscall_count else EVENTS
    env = {"PATH": "/usr/bin:/bin", "HOME": os.environ.get("HOME", "/"), "GOMAXPROCS": str(args.gomaxprocs),
           "GOGC": args.gogc, "GOMEMLIMIT": "off"}
    if args.ld_library_path:
        env["LD_LIBRARY_PATH"] = args.ld_library_path
    for kv in args.env:
        k, _, v = kv.partition("=")
        env[k] = v
    cmd = ["perf", "stat", "-x,", "-p", pid, "-e", ",".join(events), "--",
           "env", "-i", *[f"{k}={v}" for k, v in env.items()],
           "taskset", "-c", args.client_cpus, args.binary,
           "-cluster-file", args.cluster_file, "-workload", args.workload, "-workers", str(args.workers),
           "-iterations", str(iterations), "-warmup", str(iterations // 20), "-allow-writes", "-output", outdir,
           *args.client_arg]
    p = subprocess.run(cmd, capture_output=True, text=True)
    counters = {}
    for line in p.stderr.splitlines():
        f = line.split(",")
        if len(f) > 2 and f[0] and f[0][0].isdigit():
            counters[f[2].split(":")[-1]] = float(f[0])
    if p.returncode != 0 or len(counters) != len(events):
        sys.exit(f"run failed (exit {p.returncode}):\n{p.stdout[-2000:]}\n{p.stderr[-2000:]}")
    return counters


def summary(outdir):
    s = json.load(open(os.path.join(outdir, "summary.json")))
    if not s.get("success"):
        sys.exit(f"clientbench reported failure: {s.get('failure')}")
    return s


def operations(s):
    """Operations a run completed, warmup included: what its server counters cover."""
    return s["completed"] + s["warmup_completed"]


def client_metrics(outdir):
    s = summary(outdir)
    n = s["completed"]
    b, a = s["stats_before"], s["stats_after"]
    cpu = lambda x: x["process_user_cpu_us"] + x["process_system_cpu_us"]
    with open(os.path.join(outdir, "samples.csv")) as f:
        lat = sorted(int(r["latency_ns"]) for r in csv.DictReader(f))
    return {
        "client_cpu_us": (cpu(a) - cpu(b)) / n,
        "mallocs": (a["go_mallocs"] - b["go_mallocs"]) / n,
        "gc_cycles": a["go_gc_cycles"] - b["go_gc_cycles"],
        "tx_per_s": n / (s["measured_wall_ns"] / 1e9),
        "mean_latency_us": statistics.fmean(lat) / 1e3,
        "p99_latency_us": lat[int(len(lat) * 0.99)] / 1e3,
        "pacing": s.get("pacing"),  # paced runs: start lag and gap variation; None in a closed loop
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--binary", required=True)
    ap.add_argument("--cluster-file", required=True)
    ap.add_argument("--workload", required=True)
    ap.add_argument("--workers", type=int, required=True)
    ap.add_argument("--short", type=int, required=True, help="iterations of the short run")
    ap.add_argument("--long", type=int, required=True, help="iterations of the long run")
    ap.add_argument("--reps", type=int, default=3)
    ap.add_argument("--label", required=True)
    ap.add_argument("--out", required=True, help="JSON lines file to append to")
    ap.add_argument("--server-pid", help="PID, or comma-separated PIDs, of the server processes")
    ap.add_argument("--server-name", default="fdbserver")
    ap.add_argument("--client-cpus", default="0-7")
    ap.add_argument("--gomaxprocs", type=int, default=8)
    ap.add_argument("--gogc", default="100")
    ap.add_argument("--ld-library-path", default="", help="for the libfdb_c (CGo) binary")
    ap.add_argument("--env", action="append", default=[], help="extra KEY=VALUE for the client")
    ap.add_argument("--client-arg", action="append", default=[], help="extra clientbench argument, e.g. -rate=2000")
    ap.add_argument("--no-syscall-count", action="store_true", help="count cycles, instructions and task-clock only")
    args = ap.parse_args()
    if args.long <= args.short:
        sys.exit("--long must exceed --short")
    pid = args.server_pid or str(server_pid(args.server_name))
    rows = []
    for rep in range(args.reps):
        tmp = tempfile.mkdtemp(prefix="servercost-")
        try:
            a = run_once(args, pid, args.short, os.path.join(tmp, "short"))
            b = run_once(args, pid, args.long, os.path.join(tmp, "long"))
            tx = operations(summary(os.path.join(tmp, "long"))) - operations(summary(os.path.join(tmp, "short")))
            if tx <= 0:
                sys.exit("the long run completed no more operations than the short run")
            row = {"label": args.label, "workload": args.workload, "workers": args.workers, "rep": rep,
                   "short": args.short, "long": args.long, "gogc": args.gogc, "env": args.env,
                   "client_arg": args.client_arg, "syscall_count": not args.no_syscall_count,
                   "server_task_clock_us": (b["task-clock"] - a["task-clock"]) * 1e3 / tx,  # perf reports ms
                   "server_cycles": (b["cycles"] - a["cycles"]) / tx,
                   "server_instructions": (b["instructions"] - a["instructions"]) / tx,
                   "server_syscalls": (b["sys_enter"] - a["sys_enter"]) / tx if "sys_enter" in b else None}
            row.update(client_metrics(os.path.join(tmp, "long")))
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
        rows.append(row)
        with open(args.out, "a") as f:
            f.write(json.dumps(row) + "\n")
    keys = ["server_task_clock_us", "server_cycles", "server_syscalls", "client_cpu_us", "mean_latency_us", "p99_latency_us", "tx_per_s"]
    med = {k: statistics.median(r[k] for r in rows) for k in keys if rows[0][k] is not None}
    print(f"{args.label} {args.workload} w={args.workers} " + " ".join(f"{k}={v:.1f}" for k, v in med.items()))


if __name__ == "__main__":
    main()
