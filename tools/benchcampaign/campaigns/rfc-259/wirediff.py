#!/usr/bin/env python3
"""Per-transaction wire behaviour of one clientbench configuration.

Short and long runs, like servercost.py; every count is (long - short) / extra
transactions. Counts syscalls by type for each fdbserver process (per-thread
perf stat, mapped to its process) and for the client process tree, and the
client's TCP segments to each server port (ss, sampled while running).
Server request counters come from trace-log totals around the long run.
"""
import argparse, glob, json, os, re, subprocess, sys, tempfile, threading, time, shutil

SYSCALLS = ["read", "readv", "recvfrom", "recvmsg", "write", "writev", "sendto", "sendmsg",
            "epoll_wait", "epoll_pwait", "epoll_ctl", "futex", "nanosleep", "clock_nanosleep",
            "sched_yield", "io_submit", "io_getevents", "fdatasync", "pwrite64"]
EVENTS = ["cycles"] + [f"syscalls:sys_enter_{s}" for s in SYSCALLS]
W = "${FDB_PERF_HOME}/cpu-work"
LOGS = {"stateless": f"{W}/fdb3/s/logs", "transaction": f"{W}/fdb3/t/logs", "storage": f"{W}/fdb3/d/logs"}
COUNTERS = {
    "GrvProxyMetrics": ["TxnRequestIn", "TxnStartIn", "TxnStartBatch"],
    "ProxyMetrics": ["TxnCommitIn", "CommitBatchIn", "Mutations", "ConflictRanges"],
    "StorageMetrics": ["QueryQueue", "GetValueQueries", "GetRangeQueries"],
}


def parse_perf(path, tid2role):
    out = {}
    for line in open(path):
        f = line.strip().split(",")
        # per-thread: comm-tid,count,unit,event,...
        if len(f) < 4 or not f[1].replace(".", "").isdigit():
            continue
        tid = int(f[0].rsplit("-", 1)[1]) if "-" in f[0] else None
        role = tid2role.get(tid, "client") if tid is not None else "client"
        ev = f[3].split(":")[-1].replace("sys_enter_", "")
        out.setdefault(role, {}).setdefault(ev, 0.0)
        out[role][ev] += float(f[1])
    return out


def parse_perf_total(path):
    out = {}
    for line in open(path):
        f = line.strip().split(",")
        if len(f) > 2 and f[0].replace(".", "").isdigit():
            ev = f[2].split(":")[-1].replace("sys_enter_", "")
            out[ev] = out.get(ev, 0.0) + float(f[0])
    return out


def sample_ss(stop, ports, store):
    pat = re.compile(r"(\S+):(\d+)\s+(\S+):(\d+).*?bytes_sent:(\d+).*?segs_out:(\d+)", re.S)
    while not stop.is_set():
        out = subprocess.run(["ss", "-tinpH", "state", "established"], capture_output=True, text=True).stdout
        # ss prints the info on the line after the address line
        lines = out.splitlines()
        for i in range(0, len(lines) - 1):
            m = re.match(r"\s*\d+\s+\d+\s+(\S+):(\d+)\s+(\S+):(\d+)", lines[i])
            if not m or int(m.group(4)) not in ports or "clientbench" not in lines[i]:
                continue
            info = lines[i + 1]
            so = re.search(r"segs_out:(\d+)", info)
            bs = re.search(r"bytes_sent:(\d+)", info)
            if so:
                store[(m.group(2), m.group(4))] = (int(so.group(1)), int(bs.group(1)) if bs else 0)
        time.sleep(0.1)


def run_once(args, iterations, outdir, tid2role):
    env = {"PATH": "/usr/bin:/bin", "HOME": os.environ["HOME"], "GOMAXPROCS": "8", "GOGC": "100", "GOMEMLIMIT": "off"}
    if args.ld_library_path:
        env["LD_LIBRARY_PATH"] = args.ld_library_path
    for kv in args.env:
        k, _, v = kv.partition("=")
        env[k] = v
    srv, cli = outdir + ".srv", outdir + ".cli"
    ev = ",".join(EVENTS)
    cmd = ["perf", "stat", "-x,", "--per-thread", "-p", args.server_pid, "-e", ev, "-o", srv, "--",
           "perf", "stat", "-x,", "-e", ev, "-o", cli, "--",
           "env", "-i", *[f"{k}={v}" for k, v in env.items()], "taskset", "-c", "0-7", args.binary,
           "-cluster-file", args.cluster_file, "-workload", args.workload, "-workers", str(args.workers),
           "-iterations", str(iterations), "-warmup", str(iterations // 20), "-allow-writes", "-output", outdir, *args.client_arg]
    store, stop = {}, threading.Event()
    th = threading.Thread(target=sample_ss, args=(stop, {4721, 4722, 4723}, store))
    th.start()
    t0 = time.time()
    p = subprocess.run(cmd, capture_output=True, text=True)
    t1 = time.time()
    stop.set(); th.join()
    if p.returncode != 0:
        sys.exit(f"run failed {p.returncode}\n{p.stdout[-2000:]}\n{p.stderr[-2000:]}")
    s = json.load(open(os.path.join(outdir, "summary.json")))
    if not s.get("success"):
        sys.exit(f"clientbench failure {s.get('failure')}")
    import csv, statistics
    with open(os.path.join(outdir, "samples.csv")) as f:
        lat = [int(r["latency_ns"]) for r in csv.DictReader(f)]
    s["mean_latency_us"] = statistics.fmean(lat) / 1e3
    seg = {}
    for (sport, dport), (so, bs) in store.items():
        a = seg.setdefault(dport, [0, 0, 0])
        a[0] += so; a[1] += bs; a[2] += 1
    srvc = parse_perf(srv, tid2role)
    srvc["client"] = parse_perf_total(cli)
    return {"perf": srvc, "segs": seg, "t0": t0, "t1": t1, "summary": s}


def trace_totals():
    tot = {}
    for role, d in LOGS.items():
        files = sorted(glob.glob(d + "/*.xml"), key=os.path.getmtime)[-2:]
        for f in files:
            for line in open(f, errors="replace"):
                for typ, names in COUNTERS.items():
                    if f'Type="{typ}"' in line:
                        t = float(re.search(r' Time="([\d.]+)"', line).group(1))
                        for n in names:
                            m = re.search(rf' {n}="[^ ]+ [^ ]+ ([\d.e+]+)"', line)
                            if m:
                                tot.setdefault(f"{typ}.{n}", []).append((t, float(m.group(1))))
    return tot


def at(series, t):
    before = [v for (ts, v) in series if ts <= t]
    return before[-1] if before else None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True); ap.add_argument("--label", required=True)
    ap.add_argument("--workload", required=True); ap.add_argument("--workers", type=int, required=True)
    ap.add_argument("--short", type=int, required=True); ap.add_argument("--long", type=int, required=True)
    ap.add_argument("--server-pid", required=True); ap.add_argument("--cluster-file", required=True)
    ap.add_argument("--ld-library-path", default=""); ap.add_argument("--env", action="append", default=[])
    ap.add_argument("--out", required=True); ap.add_argument("--client-arg", action="append", default=[])
    args = ap.parse_args()
    roles = {}
    for pid in args.server_pid.split(","):
        cmdline = open(f"/proc/{pid}/cmdline").read()
        role = cmdline.split("--class\x00")[1].split("\x00")[0]
        for tid in os.listdir(f"/proc/{pid}/task"):
            roles[int(tid)] = role
    tmp = tempfile.mkdtemp(prefix="wirediff-", dir=f"{W}/tmp")
    try:
        a = run_once(args, args.short, os.path.join(tmp, "short"), roles)
        time.sleep(11)  # keep the short run out of the long run's trace interval
        b = run_once(args, args.long, os.path.join(tmp, "long"), roles)
        time.sleep(11)  # one more 5 s trace interval after the long run, flushed
        tr = trace_totals()
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    tx = (args.long - args.short) * 21 / 20
    row = {"label": args.label, "workload": args.workload, "workers": args.workers,
           "short": args.short, "long": args.long, "env": args.env, "client_arg": args.client_arg}
    for role in sorted(set(a["perf"]) | set(b["perf"])):
        for ev in sorted(set(a["perf"].get(role, {})) | set(b["perf"].get(role, {}))):
            d = (b["perf"].get(role, {}).get(ev, 0) - a["perf"].get(role, {}).get(ev, 0)) / tx
            if abs(d) > 1e-4 or ev == "cycles":
                row[f"{role}.{ev}"] = d
    for port in sorted(set(a["segs"]) | set(b["segs"])):
        sa, sb = a["segs"].get(port, [0, 0, 0]), b["segs"].get(port, [0, 0, 0])
        row[f"client_segs_to_{port}"] = (sb[0] - sa[0]) / tx
        row[f"client_bytes_to_{port}"] = (sb[1] - sa[1]) / tx
        row[f"client_conns_to_{port}"] = sb[2]
    # trace totals: first event after start minus last event before... long run only
    n = b["summary"]["completed"] * 21 / 20
    for k, series in tr.items():
        v0, v1 = at(series, b["t0"]), at(series, b["t1"] + 10.5)
        if v0 is not None and v1 is not None:
            row[f"trace.{k}"] = (v1 - v0) / n
    lat = b["summary"]
    row["tx_per_s"] = lat["completed"] / (lat["measured_wall_ns"] / 1e9)
    row["mean_latency_us"] = lat["mean_latency_us"]
    with open(args.out, "a") as f:
        f.write(json.dumps(row) + "\n")
    keys = [k for k in row if k != "client_arg" and k.startswith(("stateless.", "transaction.", "storage.", "client", "trace.")) or k in ("tx_per_s", "mean_latency_us")]
    print(args.label, args.workload, args.workers, " ".join(f"{k}={row[k]:.3f}" for k in keys))


if __name__ == "__main__":
    main()
