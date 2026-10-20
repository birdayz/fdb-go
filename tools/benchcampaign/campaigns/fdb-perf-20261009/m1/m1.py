#!/usr/bin/env python3
"""M1 record-layer paired timing harness (standalone; stdlib only).

  m1.py plan     CONFIG.json OUTDIR   deterministic plan + schedule (run locally, then pinned)
  m1.py run      OUTDIR               execute the pinned schedule on the VM; prints no metrics
  m1.py analyze  OUTDIR               only after a complete, valid campaign
"""
import fractions, hashlib, itertools, json, math, os, random, re, signal, subprocess, sys, threading, time
from pathlib import Path

SCHEMA = "m1-v1"


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def dump(path, obj):
    Path(path).write_text(json.dumps(obj, indent=1, sort_keys=True) + "\n")


# ---------------------------------------------------------------- plan
def plan(cfg_path, out):
    cfg = json.loads(Path(cfg_path).read_text())
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    assert cfg["schema"] == SCHEMA
    assert re.fullmatch(r"[a-z0-9]{8}", cfg["campaign"]), "campaign id must be 8 [a-z0-9]"
    arms = [a["name"] for a in cfg["arms"]]
    assert arms[0] == cfg["denominator"] == "base" and len(set(arms)) == len(arms) >= 2
    for a in cfg["arms"]:
        assert re.fullmatch(r"[0-9a-f]{40}", a["commit"]), a
        assert sha256(out / a["binary"]) == a["binary_sha256"], a["name"]
    perms = list(itertools.permutations(arms))
    P = cfg["pairs"]
    assert P % len(perms) == 0, "pairs must be a multiple of k! for exact arm-order balance"
    benches = [b["name"] for b in cfg["benches"]]
    rng = random.Random(cfg["schedule_seed"])
    # Arm order per bench: each permutation exactly P/k! times across measured blocks.
    order = {}
    for b in benches:
        lst = perms * (P // len(perms))
        rng.shuffle(lst)
        order[b] = lst
    sched, seq = [], 0
    blocks = [("warmup", w) for w in range(cfg["warmup_blocks"])] + [("measured", i) for i in range(P)]
    for kind, idx in blocks:
        border = benches[:]
        rng.shuffle(border)
        for b in border:
            aorder = list(perms[rng.randrange(len(perms))]) if kind == "warmup" else list(order[b][idx])
            for a in aorder:
                sched.append({"seq": seq, "kind": kind, "block": idx, "bench": b, "arm": a,
                              "prefix": "m1/%s/%05d" % (cfg["campaign"], seq)})
                seq += 1
    assert len({s["prefix"] for s in sched}) == len(sched)
    assert len({len(s["prefix"].encode()) for s in sched}) == 1
    for b in benches:  # balance self-check
        firsts = [s["arm"] for s in sched if s["kind"] == "measured" and s["bench"] == b][:: len(arms)]
        assert all(firsts.count(a) == P // len(arms) for a in arms), b
    dump(out / "plan.json", cfg)
    dump(out / "schedule.json", sched)
    print("plan.json", sha256(out / "plan.json"))
    print("schedule.json", sha256(out / "schedule.json"), "processes", len(sched))


# ---------------------------------------------------------------- run
def cpu_jiffies(cpus):
    """Sum of (total, steal, busy) jiffies over the listed CPUs from /proc/stat."""
    tot = steal = busy = 0
    want = {"cpu%d" % c for c in cpus}
    for line in Path("/proc/stat").read_text().splitlines():
        f = line.split()
        if f and f[0] in want:
            v = list(map(int, f[1:9]))  # user nice system idle iowait irq softirq steal
            tot += sum(v)
            steal += v[7]
            busy += sum(v) - v[3] - v[4]
    return tot, steal, busy


def cpulist(spec):
    r = []
    for part in spec.split(","):
        a, _, b = part.partition("-")
        r += list(range(int(a), int(b or a) + 1))
    return r


class ControllerSignal(Exception):
    pass


CURRENT = {"pgid": None}


def kill_group(pgid, grace):
    """TERM the process group, then KILL after grace seconds; caller reaps."""
    if not pgid:
        return
    try:
        os.killpg(pgid, signal.SIGTERM)
    except ProcessLookupError:
        return
    end = time.monotonic() + grace
    while time.monotonic() < end:
        try:
            os.killpg(pgid, 0)
        except ProcessLookupError:
            return
        time.sleep(0.05)
    try:
        os.killpg(pgid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def spawn_watched(cmd, env, out_file, cwd, hard_wall_s, grace_s):
    """Run cmd in its own session/process group; external wall watchdog (TERM -> bounded KILL).
    Returns (wait status, child rusage from wait4, watchdog verdict or "")."""
    p = subprocess.Popen(cmd, env=env, stdout=out_file, stderr=subprocess.STDOUT, cwd=cwd,
                         start_new_session=True)
    CURRENT.update(pgid=p.pid, grace=grace_s)
    verdict = []

    def fire():
        verdict.append("hard_wall_%gs" % hard_wall_s)
        kill_group(p.pid, grace_s)
    t = threading.Timer(hard_wall_s, fire)
    t.daemon = True
    t.start()
    try:
        _, status, ru = os.wait4(p.pid, 0)
    except BaseException:
        t.cancel()
        kill_group(p.pid, grace_s)
        try:
            os.wait4(p.pid, 0)
        except ChildProcessError:
            pass
        raise
    t.cancel()
    t.join()
    p.returncode = os.waitstatus_to_exitcode(status)
    try:  # stragglers in the group (none expected) are killed too
        os.killpg(p.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    CURRENT["pgid"] = None
    return status, ru, verdict[0] if verdict else ""


RESULT_RE = re.compile(r"^(Benchmark\S+)\s+(\d+)\s+(.*)$")


def parse_bench(stdout, want_name):
    rows = []
    for line in stdout.splitlines():
        m = RESULT_RE.match(line)
        if m and m.group(1) == want_name:
            toks = m.group(3).split()
            metrics = {toks[i + 1]: float(toks[i]) for i in range(0, len(toks) - 1, 2)}
            rows.append((int(m.group(2)), metrics))
    return rows


def run(out):
    out = Path(out).resolve()
    cfg = json.loads((out / "plan.json").read_text())
    sched = json.loads((out / "schedule.json").read_text())
    pins = json.loads((out / "pins.json").read_text())  # written by m1-run.sh before timing
    assert pins["plan_sha256"] == sha256(out / "plan.json")
    assert pins["schedule_sha256"] == sha256(out / "schedule.json")
    bench = {b["name"]: b for b in cfg["benches"]}
    arm = {a["name"]: a for a in cfg["arms"]}
    client = cpulist(cfg["client_cpus"])
    rawdir = out / "raw"
    rawdir.mkdir(exist_ok=True)
    recf = out / "runs.jsonl"
    assert not recf.exists(), "runs.jsonl exists: a campaign never resumes or appends"
    env = {"PATH": "/usr/bin:/bin", "HOME": str(out), "TMPDIR": str(out / "tmp"),
           "M1_FDB_CLUSTER_FILE": cfg["cluster_file"]}
    env.update(cfg["go_env"])
    (out / "tmp").mkdir(exist_ok=True)
    deadline = float(os.environ.get("M1_DEADLINE_EPOCH", "0"))
    assert deadline > time.time(), "M1_DEADLINE_EPOCH (absolute VM-slot end) required"
    def on_signal(signum, _frame):
        raise ControllerSignal(signum)
    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)
    signal.signal(signal.SIGHUP, on_signal)
    try:
        return _run_loop(out, cfg, sched, bench, arm, client, rawdir, recf, env, deadline)
    except ControllerSignal as e:
        kill_group(CURRENT["pgid"], CURRENT.get("grace", 3.0))
        print("CONTROLLER_SIGNAL %d: campaign incomplete" % e.args[0], flush=True)
        return 5


def _run_loop(out, cfg, sched, bench, arm, client, rawdir, recf, env, deadline):
    t0 = time.monotonic()
    with open(recf, "w") as rf:
        for s in sched:
            # Abort before (never during) a process that could end past the cap or the VM deadline.
            worst = cfg["cooldown_s"] + cfg["hard_wall_s"] + cfg["kill_grace_s"] + 2
            if time.monotonic() - t0 + worst > cfg["campaign_cap_s"] or (
                    deadline and time.time() + worst + cfg["post_reserve_s"] > deadline):
                print("ABORT_BEFORE_SEQ %05d: cap/deadline; campaign incomplete" % s["seq"], flush=True)
                return 3
            time.sleep(cfg["cooldown_s"] - cfg["quiescence_s"])
            q0 = cpu_jiffies(client)
            time.sleep(cfg["quiescence_s"])
            q1 = cpu_jiffies(client)
            b, a = bench[s["bench"]], arm[s["arm"]]
            cmd = ["taskset", "--cpu-list", cfg["client_cpus"], str(out / a["binary"]),
                   "-test.run=^$", "-test.bench=" + b["regex"], "-test.benchtime=%dx" % b["n"],
                   "-test.count=1", "-test.cpu=%s" % cfg["go_env"]["GOMAXPROCS"], "-test.benchmem",
                   "-test.timeout=%ds" % cfg["process_timeout_s"], "--ginkgo.skip=.*"]
            penv = dict(env, M1_KEY_PREFIX=s["prefix"])
            j0 = cpu_jiffies(client)
            w0 = time.time()
            m0 = time.monotonic()
            so = open(rawdir / ("%05d.out" % s["seq"]), "wb")
            status, ru, killed = spawn_watched(cmd, penv, so, str(out / "tmp"), cfg["hard_wall_s"], cfg["kill_grace_s"])
            p = subprocess.CompletedProcess(cmd, os.waitstatus_to_exitcode(status))
            m1 = time.monotonic()
            j1 = cpu_jiffies(client)
            so.close()
            text = (rawdir / ("%05d.out" % s["seq"])).read_text(errors="replace")
            rows = parse_bench(text, b["result"])
            rec = dict(s, start_utc=w0, wall_s=m1 - m0, exit=p.returncode,
                       ru_user_s=ru.ru_utime, ru_sys_s=ru.ru_stime, ru_maxrss_kb=ru.ru_maxrss,
                       ru_nvcsw=ru.ru_nvcsw, ru_nivcsw=ru.ru_nivcsw,
                       client_jiffies=[j1[i] - j0[i] for i in range(3)],
                       quiescence_jiffies=[q1[i] - q0[i] for i in range(3)],
                       n=rows[0][0] if len(rows) == 1 else None,
                       metrics=rows[0][1] if len(rows) == 1 else None)
            rec["watchdog"] = killed
            problems = []
            if killed:
                problems.append("watchdog " + killed)
            if p.returncode != 0:
                problems.append("exit %d" % p.returncode)
            if len(rows) != 1:
                problems.append("%d result rows" % len(rows))
            elif rows[0][0] != b["n"]:
                problems.append("N %d != %d" % (rows[0][0], b["n"]))
            elif not {"ns/op", "proc-cpu-ns/op", "proc-user-ns/op", "proc-sys-ns/op",
                      "B/op", "allocs/op"} <= rows[0][1].keys():
                problems.append("missing metric")
            if "\nPASS\n" not in "\n" + text and not text.rstrip().endswith("PASS"):
                problems.append("no PASS")
            rec["problems"] = problems
            rf.write(json.dumps(rec, sort_keys=True) + "\n")
            rf.flush()
            # Operational status only: never a timing value.
            print("seq=%05d %s blk=%s %s %s %s" % (s["seq"], s["kind"], s["block"], s["bench"], s["arm"],
                  "OK" if not problems else "INVALID " + ";".join(problems)), flush=True)
            if problems:
                return 4
    print("CAMPAIGN_COMPLETE elapsed_s=%.0f" % (time.monotonic() - t0), flush=True)
    return 0


# ---------------------------------------------------------------- analyze
def qindex(p, m):
    """Registered nearest-rank index max(0, ceil(p*M)-1), with p an exact Fraction."""
    assert isinstance(p, fractions.Fraction)
    return max(0, math.ceil(p * m) - 1)


def decide(ci_a, ci_b, host_ok):
    if not host_ok:
        return "INVALID_HOST_GATE"
    if ci_a[1] < 1 and ci_b[1] < 1:
        return "CONFIRMED_IMPROVEMENT"
    if ci_a[0] > 1 and ci_b[0] > 1:
        return "CONFIRMED_REGRESSION"
    return "INCONCLUSIVE"


def cmbb(logs_by_ep, L, resamples, seed, p_lo):
    """Circular moving-block bootstrap of the mean log ratio, joint over endpoints."""
    eps = list(logs_by_ep)
    n = len(logs_by_ep[eps[0]])
    assert n % L == 0 and all(len(v) == n for v in logs_by_ep.values())
    starts = n // L
    win = {e: [sum(v[(s + j) % n] for j in range(L)) for s in range(n)] for e, v in logs_by_ep.items()}
    rng = random.Random(seed)
    acc = {e: [] for e in eps}
    for _ in range(resamples):
        idx = [rng.randrange(n) for _ in range(starts)]
        for e in eps:
            w = win[e]
            acc[e].append(math.exp(sum(w[i] for i in idx) / n))
    i_lo, i_hi = qindex(p_lo, resamples), qindex(1 - p_lo, resamples)
    res = {}
    for e in eps:
        v = sorted(acc[e])
        res[e] = (v[i_lo], v[i_hi])
    return res


def analyze(out):
    out = Path(out)
    cfg = json.loads((out / "plan.json").read_text())
    sched = json.loads((out / "schedule.json").read_text())
    recs = [json.loads(l) for l in (out / "runs.jsonl").read_text().splitlines()]
    assert len(recs) == len(sched), "incomplete campaign: no inference (%d/%d)" % (len(recs), len(sched))
    for r, s in zip(recs, sched):
        assert all(r[k] == s[k] for k in s), "record/schedule mismatch at %d" % s["seq"]
        assert not r["problems"], "invalid process %d: %s" % (r["seq"], r["problems"])
    meas = [r for r in recs if r["kind"] == "measured"]
    P = cfg["pairs"]
    arms = [a["name"] for a in cfg["arms"]]
    get = {}
    for r in meas:
        get[(r["block"], r["bench"], r["arm"])] = r
    nplan = {b["name"]: b["n"] for b in cfg["benches"]}
    assert len(get) == len(meas) == P * len(nplan) * len(arms), "duplicate or missing measured cell"
    assert all(r["n"] == nplan[r["bench"]] and r["metrics"] for r in meas), "N/metrics mismatch"

    def value(r, metric):
        return r["metrics"][metric]

    # Host-validity gate (evaluated after completion; cannot be tuned).
    tot = sum(r["client_jiffies"][0] for r in meas)
    steal = sum(r["client_jiffies"][1] for r in meas)
    worst = max(r["client_jiffies"][1] / max(1, r["client_jiffies"][0]) for r in meas)
    host = {"client_steal_frac": steal / max(1, tot), "worst_process_steal_frac": worst,
            "gate": steal / max(1, tot) <= cfg["gates"]["campaign_steal_max"]
            and worst <= cfg["gates"]["process_steal_max"]}
    metrics = cfg["primary_metrics_universe"] + cfg["secondary_metrics"]
    endpoints = {}
    for arm in arms[1:]:
        for b in cfg["benches"]:
            for m in metrics:
                logs = []
                for blk in range(P):
                    x, y = value(get[(blk, b["name"], arm)], m), value(get[(blk, b["name"], "base")], m)
                    logs.append(math.log(x / y) if x > 0 and y > 0 else float("nan"))
                endpoints["%s/%s/%s" % (arm, b["name"], m)] = logs
    prim = ["%s/%s/%s" % (p["arm"], p["bench"], p["metric"]) for p in cfg["primary"]]
    alpha = fractions.Fraction(str(cfg["familywise_alpha"])) / len(prim)
    p_lo = alpha / 2
    level = float(1 - alpha)
    finite = {e: v for e, v in endpoints.items() if all(math.isfinite(x) for x in v)}
    assert all(e in finite for e in prim), "non-finite primary log ratio"
    bs = cfg["bootstrap"]
    ci_a = cmbb(finite, bs["primary_block_len"], bs["resamples"], bs["primary_seed"], p_lo)
    ci_b = cmbb(finite, bs["sensitivity_block_len"], bs["resamples"], bs["sensitivity_seed"], p_lo)
    rows = []
    for e, v in endpoints.items():
        est = math.exp(sum(v) / len(v)) if e in finite else float("nan")
        lo_a, hi_a = ci_a.get(e, (float("nan"),) * 2)
        lo_b, hi_b = ci_b.get(e, (float("nan"),) * 2)
        role = "PRIMARY" if e in prim else "secondary"
        if role == "PRIMARY":
            dec = decide((lo_a, hi_a), (lo_b, hi_b), host["gate"])
        else:
            dec = "descriptive"
        rows.append({"endpoint": e, "role": role, "gmr": est, "ci_L%d" % bs["primary_block_len"]: [lo_a, hi_a],
                     "ci_L%d" % bs["sensitivity_block_len"]: [lo_b, hi_b], "decision": dec,
                     "log_ratios": v})
    # Process-lifetime CPU/wall: absolute per arm only, never paired or claimed.
    absolute = {}
    for b in cfg["benches"]:
        for a in arms:
            rs = [get[(blk, b["name"], a)] for blk in range(P)]
            for name, f in (("process_cpu_s", lambda r: r["ru_user_s"] + r["ru_sys_s"]), ("process_wall_s", lambda r: r["wall_s"])):
                v = sorted(f(r) for r in rs)
                absolute["%s/%s/%s" % (a, b["name"], name)] = {"min": v[0], "median": (v[(len(v) - 1) // 2] + v[len(v) // 2]) / 2, "max": v[-1], "mean": sum(v) / len(v)}
    res = {"schema": SCHEMA, "level": level, "p_lo": str(p_lo), "host": host, "pairs": P, "endpoints": rows,
           "absolute_lifetime": absolute,
           "plan_sha256": sha256(out / "plan.json"), "schedule_sha256": sha256(out / "schedule.json"),
           "runs_sha256": sha256(out / "runs.jsonl")}
    dump(out / "analysis.json", res)
    lines = ["# M1 analysis (%d pairs, %.4g%% CIs, host gate %s)" % (P, 100 * level, host["gate"]), "",
             "| endpoint | role | GMR new/base | CI L%d | CI L%d | decision |" % (bs["primary_block_len"], bs["sensitivity_block_len"]),
             "|---|---|---:|---|---|---|"]
    for r in sorted(rows, key=lambda r: (r["role"] != "PRIMARY", r["endpoint"])):
        ca, cb = r["ci_L%d" % bs["primary_block_len"]], r["ci_L%d" % bs["sensitivity_block_len"]]
        lines.append("| %s | %s | %.4f | %.4f–%.4f | %.4f–%.4f | %s |" % (r["endpoint"], r["role"], r["gmr"], ca[0], ca[1], cb[0], cb[1], r["decision"]))
    (out / "analysis.md").write_text("\n".join(lines) + "\n")
    print("\n".join(lines))
    return 0


if __name__ == "__main__":
    cmd = sys.argv[1]
    if cmd == "plan":
        plan(sys.argv[2], sys.argv[3])
    elif cmd == "run":
        sys.exit(run(sys.argv[2]))
    elif cmd == "analyze":
        sys.exit(analyze(sys.argv[2]))
    else:
        sys.exit("unknown command")
