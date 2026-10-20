#!/usr/bin/env python3
"""Offline self-tests for m1.py (no FDB, no timing). Usage: python3 -B m1_selftest.py"""
import importlib.util, json, math, os, random, signal, subprocess, sys, tempfile, time
from fractions import Fraction
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("m1", HERE / "m1.py")
m1 = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m1)
FAILS = []


def check(cond, name):
    print(("ok   " if cond else "FAIL ") + name)
    if not cond:
        FAILS.append(name)


def raises(fn, name):
    try:
        fn()
    except (AssertionError, KeyError):
        check(True, name)
        return
    check(False, name + " (did not raise)")


# 1. exact registered quantile indices
p = Fraction(1, 200)
check(m1.qindex(p, 100000) == 499 and m1.qindex(1 - p, 100000) == 99499, "qindex 0.005/0.995 @1e5 = 499/99499")
check(m1.qindex(p, 1000) == 4 and m1.qindex(1 - p, 1000) == 994, "qindex @1e3 = 4/994")
check(m1.qindex(Fraction(1, 2), 1) == 0, "qindex floor at 0")

# 2. bootstrap vs direct reference implementation (explicit index concatenation)
def reference(logs, L, R, seed, p_lo):
    n = len(logs); rng = random.Random(seed); out = []
    for _ in range(R):
        idx = []
        for _ in range(n // L):
            s = rng.randrange(n)
            idx += [(s + j) % n for j in range(L)]
        out.append(math.exp(sum(logs[i] for i in idx) / n))
    out.sort()
    return out[m1.qindex(p_lo, R)], out[m1.qindex(1 - p_lo, R)]

g = random.Random(5)
for n, L in ((48, 8), (48, 16), (64, 8)):
    a = [g.gauss(-0.03, 0.05) for _ in range(n)]
    b = [g.gauss(0.01, 0.02) for _ in range(n)]
    got = m1.cmbb({"a": a, "b": b}, L, 3000, 919707, p)
    ra, rb = reference(a, L, 3000, 919707, p), reference(b, L, 3000, 919707, p)
    check(all(abs(x - y) < 1e-12 for x, y in zip(got["a"] + got["b"], ra + rb)), "cmbb == reference n=%d L=%d" % (n, L))

# 3. strict decision gate
d = m1.decide
check(d((0.9, 1.0), (0.9, 0.99), True) == "INCONCLUSIVE", "upper == 1.0 (L8) is not a claim")
check(d((0.9, 0.99), (0.9, 1.0), True) == "INCONCLUSIVE", "upper == 1.0 (L16) is not a claim")
check(d((0.9, 0.9999), (0.9, 0.9999), True) == "CONFIRMED_IMPROVEMENT", "both upper < 1 confirms")
check(d((1.0, 1.1), (1.01, 1.1), True) == "INCONCLUSIVE", "lower == 1.0 is not a regression")
check(d((1.01, 1.1), (1.01, 1.1), True) == "CONFIRMED_REGRESSION", "both lower > 1 regression")
check(d((0.8, 0.9), (0.8, 0.9), False) == "INVALID_HOST_GATE", "host gate overrides")

# 4. analyze on synthetic campaigns (no run): positive + negatives
def campaign(tmp, pairs=8):
    tmp = Path(tmp); (tmp / "bin").mkdir(parents=True)
    for a in ("base", "new"):
        (tmp / "bin" / ("recordlayer_test." + a)).write_text("#!/bin/sh\n")
    c = json.loads((HERE / "m1-config.template.json").read_text())
    c["arms"][1]["commit"] = "0" * 40
    for a in c["arms"]:
        a["binary_sha256"] = m1.sha256(tmp / a["binary"])
    c["pairs"] = pairs
    c["bootstrap"].update(resamples=2000, primary_block_len=2, sensitivity_block_len=4)
    (tmp / "c.json").write_text(json.dumps(c))
    import contextlib, io
    with contextlib.redirect_stdout(io.StringIO()):
        m1.plan(tmp / "c.json", tmp)
    return tmp, c

def synth(tmp, c, factor):
    sched = json.loads((tmp / "schedule.json").read_text()); g = random.Random(1); recs = []
    nb = {b["name"]: b["n"] for b in c["benches"]}
    for s in sched:
        base = 1000 * (1 + 0.01 * g.random())
        f = factor(s) if s["arm"] == "new" else 1.0
        recs.append(dict(s, problems=[], n=nb[s["bench"]], wall_s=1.0, ru_user_s=0.5, ru_sys_s=0.1,
                         client_jiffies=[1000, 0, 500], quiescence_jiffies=[50, 0, 1],
                         metrics={"ns/op": base * f, "proc-cpu-ns/op": 2 * base * f, "proc-user-ns/op": base,
                                  "proc-sys-ns/op": 10.0, "B/op": 500.0, "allocs/op": 7.0}))
    return recs

def write(tmp, recs):
    (tmp / "runs.jsonl").write_text("".join(json.dumps(r) + "\n" for r in recs))

def quiet_analyze(tmp):
    import contextlib, io
    with contextlib.redirect_stdout(io.StringIO()):
        return m1.analyze(tmp)

with tempfile.TemporaryDirectory() as t:
    tmp, c = campaign(t)
    recs = synth(tmp, c, lambda s: 0.9)
    write(tmp, recs); quiet_analyze(tmp)
    res = json.loads((tmp / "analysis.json").read_text())
    prim = [e for e in res["endpoints"] if e["role"] == "PRIMARY"]
    check(len(prim) == 5 and all(e["decision"] == "CONFIRMED_IMPROVEMENT" for e in prim), "0.9x effect -> 5 confirmed")
    check(res["level"] == 0.99 and res["p_lo"] == "1/200", "level 99%, p_lo exactly 1/200")
    check(not any("process_" in e["endpoint"] for e in res["endpoints"]), "lifetime metrics not in paired table")
    check(any(k.endswith("process_cpu_s") for k in res["absolute_lifetime"]), "lifetime metrics reported absolute")
    # identical arms -> every CI is exactly [1,1] -> upper == 1.0 -> inconclusive
    same = synth(tmp, c, lambda s: 1.0)
    bykey = {}
    for r in same:
        if r["kind"] == "measured" and r["arm"] == "base":
            bykey[(r["block"], r["bench"])] = r["metrics"]
    for r in same:
        if r["kind"] == "measured" and r["arm"] == "new":
            r["metrics"] = dict(bykey[(r["block"], r["bench"])])
    write(tmp, same); quiet_analyze(tmp)
    res = json.loads((tmp / "analysis.json").read_text())
    check(all(e["decision"] == "INCONCLUSIVE" for e in res["endpoints"] if e["role"] == "PRIMARY"), "exact 1.0 CI -> inconclusive")
    # negatives
    write(tmp, recs[:-1]); raises(lambda: quiet_analyze(tmp), "missing row refused")
    write(tmp, recs[:-1] + [recs[-2]]); raises(lambda: quiet_analyze(tmp), "duplicate row refused")
    bad = [dict(r) for r in recs]; bad[20]["problems"] = ["exit 1"]
    write(tmp, bad); raises(lambda: quiet_analyze(tmp), "invalid process refused")
    bad = [dict(r) for r in recs]; bad[30]["n"] = 1
    write(tmp, bad); raises(lambda: quiet_analyze(tmp), "wrong N refused")
    bad = [dict(r) for r in recs]; bad[30]["metrics"] = dict(bad[30]["metrics"]); del bad[30]["metrics"]["proc-cpu-ns/op"]
    write(tmp, bad); raises(lambda: quiet_analyze(tmp), "missing metric refused")
    bad = [dict(r) for r in recs]; bad[12]["arm"], bad[13]["arm"] = bad[13]["arm"], bad[12]["arm"]
    write(tmp, bad); raises(lambda: quiet_analyze(tmp), "schedule mismatch refused")

# 5. parse_bench
txt = "BenchmarkScanIndexRecords/serial-8 \t 10\t 5 ns/op\t 7 proc-cpu-ns/op\t 1 B/op\t 2 allocs/op\nPASS\n"
rows = m1.parse_bench(txt, "BenchmarkScanIndexRecords/serial-8")
check(rows == [(10, {"ns/op": 5.0, "proc-cpu-ns/op": 7.0, "B/op": 1.0, "allocs/op": 2.0})], "parse_bench row")
check(len(m1.parse_bench(txt + txt, "BenchmarkScanIndexRecords/serial-8")) == 2, "parse_bench sees duplicate rows")

# 6. watchdog: hung child, TERM-ignoring child, rusage preserved
with tempfile.TemporaryDirectory() as t:
    f = open(Path(t) / "o", "wb")
    t0 = time.monotonic()
    st, ru, v = m1.spawn_watched(["sleep", "100"], {"PATH": "/usr/bin:/bin"}, f, t, 0.5, 1.0)
    el = time.monotonic() - t0
    check(v.startswith("hard_wall") and os.WIFSIGNALED(st) and os.WTERMSIG(st) == signal.SIGTERM and el < 2, "hung child TERMed in %.2fs" % el)
    t0 = time.monotonic()
    st, ru, v = m1.spawn_watched(["bash", "-c", "trap '' TERM; while :; do :; done"], {"PATH": "/usr/bin:/bin"}, f, t, 0.5, 0.5)
    el = time.monotonic() - t0
    check(v and os.WIFSIGNALED(st) and os.WTERMSIG(st) == signal.SIGKILL and el < 2.5, "TERM-ignoring child KILLed in %.2fs" % el)
    check(ru.ru_utime > 0.2, "wait4 rusage preserved for killed child (utime %.2fs)" % ru.ru_utime)
    st, ru, v = m1.spawn_watched(["true"], {"PATH": "/usr/bin:/bin"}, f, t, 5, 1)
    check(v == "" and os.waitstatus_to_exitcode(st) == 0, "normal child untouched")

# 7. controller signal: runner TERMed mid-process kills the child's group and exits 5
with tempfile.TemporaryDirectory() as t:
    tmp, c = campaign(t, pairs=2)
    pidf = tmp / "child.pid"
    for a in ("base", "new"):
        b = tmp / "bin" / ("recordlayer_test." + a)
        b.write_text("#!/bin/bash\necho $$ > %s\nsleep 100 & wait\n" % pidf); b.chmod(0o755)
    c = json.loads((tmp / "c.json").read_text())
    for a in c["arms"]:
        a["binary_sha256"] = m1.sha256(tmp / a["binary"])
    c["cooldown_s"], c["quiescence_s"] = 0.02, 0.01
    (tmp / "c.json").write_text(json.dumps(c))
    for f_ in ("plan.json", "schedule.json"):
        (tmp / f_).unlink()
    import contextlib, io
    with contextlib.redirect_stdout(io.StringIO()):
        m1.plan(tmp / "c.json", tmp)
    (tmp / "pins.json").write_text(json.dumps({"plan_sha256": m1.sha256(tmp / "plan.json"), "schedule_sha256": m1.sha256(tmp / "schedule.json")}))
    env = dict(os.environ, M1_DEADLINE_EPOCH=str(time.time() + 3600))
    r = subprocess.Popen([sys.executable, "-B", str(HERE / "m1.py"), "run", str(tmp)], env=env, stdout=subprocess.PIPE, text=True)
    for _ in range(100):
        if pidf.exists() and pidf.read_text().strip():
            break
        time.sleep(0.05)
    child = int(pidf.read_text())
    r.send_signal(signal.SIGTERM)
    outp, _ = r.communicate(timeout=10)
    time.sleep(0.2)
    alive = True
    try:
        os.kill(child, 0)
    except ProcessLookupError:
        alive = False
    check(r.returncode == 5 and "CONTROLLER_SIGNAL" in outp and not alive, "controller SIGTERM -> child group killed, rc=5")

print("SELFTEST", "FAILED: %s" % FAILS if FAILS else "PASSED")
sys.exit(1 if FAILS else 0)
