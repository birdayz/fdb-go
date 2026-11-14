#!/usr/bin/env python3
"""Parallelism ladder on one client host: how far does one load-generator host get, and what gives out first.

Per variant (rust_native, go_pure, go_native): one fresh cluster (`climb_stage` database nodes); P_max tenants (one per
load-generator process) are set up and pre-split ONCE (conditioning with the biggest per-process population, always with the
Go own-client tool, in rounds of `presplit_round` tenants with a drain wait between rounds, then wait for data distribution to
go quiet and clear-and-restore every store to its fresh-setup state; the shard boundaries stay).

Steps: with `climb_steps[arm]` an explicit list of {procs, consumers, workers} (spans per process `climb_count[arm]`), every
step is measured; otherwise one process with 8, 16, 32 in flight, then P = 2, 4, 8, 16 processes of 32 in flight, stopping
when two steps in a row are less than 5% better than the best. All processes of a step are released together on the client
host, each with its own tenant; between steps the stores in use are cleared and restored. Rates and client CPU/memory are
summed over the processes. While a step runs, `threadcpu.py` samples every thread of every generator process; the step
record carries the busiest thread (name, mean cores, highest 1 s interval). Verification (full read-back + census, all
processes) runs after the first step and, in the searched ladder, again on a repeat of the best step.
Optional extra rows (Go own client only, searched ladder): one process, 32 consumers x writer_workers 4 / 8.

Usage:  python3 -B climb.py run --config C [--variants rust_native,go_pure,go_native]    (after `leanrun.py up`)
"""
import argparse
import concurrent.futures as cf_
import json
import math
import shlex
import statistics
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import leanrun as L  # noqa: E402
import condition  # noqa: E402

PRODUCERS = 16                                        # in-process producers per load-generator process
SINGLE = [(8, 8), (16, 16), (32, 32)]                 # (in flight, consumers) for one process, writer_workers 1
MIN_WINDOW_S = 30.0


def apply_step(ctx, consumers, workers=1):
    ctx.cfg["workload_params"] = {"consumers": consumers, "producers": PRODUCERS}
    ctx.cfg["writer_workers"] = workers


def gen_corpus(ctx, n):
    """Generate corpus <n> on the client host (control-pure corpus mode); register path + sha256 in the config assets."""
    key = "corpus_%d" % n
    if key in ctx.cfg["assets"]:
        return
    path = "%s/corpus/%d.bin" % (L.REMOTE, n)
    wdir = "%s/runs/corpus%d" % (L.REMOTE, n)
    if not ctx.dry and ctx.ssh("client1", ["test", "-f", path], check=False)[0] == 0:   # restart-safe
        ctx.cfg["assets"][key] = {"path": path, "sha256": ctx.ssh("client1", ["sha256sum", path])[1].split()[0]}
        return
    name, text = L.make_job(ctx.cfg, ctx.builds, "corpus", "go_pure", count=n, output=wdir + "/corpus.report.json", corpus_file=path, corpus_sha=None)
    L.write_remote(ctx, "client1", wdir + "/corpus.job.json", text)
    envs = ["%s=%s" % kv for kv in L.launch_env(ctx.builds, name).items()]
    rc, _, err = ctx.ssh("client1", ["env", "-i", *envs, ctx.builds.exe_path(name), wdir + "/corpus.job.json"], timeout=900, check=False)
    if rc != 0:
        raise L.Stop("corpus generation failed: " + err[-300:])
    sha = ctx.ssh("client1", ["sha256sum", path], fake="0" * 64 + "  x")[1].split()[0]
    ctx.cfg["assets"][key] = {"path": path, "sha256": sha}
    ctx.log("corpus", n=n, sha=sha)


def per_process_population(ctx, prev_rate, procs, cap):
    floor, unit = ctx.cfg.get("climb_floor", 200000), ctx.cfg.get("climb_unit", 50000)
    if not prev_rate:
        return floor
    n = int(math.ceil(prev_rate * 2.0 * (ctx.cfg.get("climb_min_window_s", MIN_WINDOW_S) + 6) / procs / unit)) * unit
    return max(ctx.cfg.get("climb_low", 100000), min(cap, n))


def restore_many(ctx, cf, tenants):
    cmds = [condition.restore_cmd(t["S"], t["snap"]) if not ctx.dry else "<restore>" for t in tenants]
    out = L.fdbcli_exec(ctx, cf, "; ".join(cmds), timeout=300)
    if not ctx.dry and out.count("Committed") < len(tenants):
        raise L.Stop("restore not committed for every store: " + out[-300:])
    if not ctx.dry:
        for t in tenants:
            after = condition.parse_getrange(L.fdbcli_exec(ctx, cf, condition.getrange_cmd(t["S"], t["S"] + b"\xff")))
            if after != t["snap"]:
                raise L.Stop("store %s is not back at its fresh-setup state" % t["run"])
    L.wait_stable(ctx, cf, cap=600, interval=2.0)


def setup_tenants(ctx, arm, n, cf, sha, cap):
    def one(i):
        run = "climb" + L.hashlib.sha256((ctx.campaign + ctx.nonce + arm + str(i)).encode()).hexdigest()[:20]
        lv = {"run": run, "cluster_file": cf, "cluster_sha256": sha, "layout_file": "%s/runs/%s/layout.json" % (L.REMOTE, run),
              "verification": ctx.cfg["verification_job"]}
        rep, _, _ = L.offline_job(ctx, "client1", "setup", arm, run, cap, lv)
        if not ctx.dry and rep.get("status") != "ok":
            raise L.Stop("setup failed (%d): %s" % (i, rep.get("error")))
        lv["layout_sha256"] = ctx.ssh("client1", ["sha256sum", lv["layout_file"]], fake="0" * 64 + " x")[1].split()[0]
        lay = json.loads(ctx.ssh("client1", ["cat", lv["layout_file"]], fake=json.dumps({"record_prefix": "15131507151f1501"}))[1])
        return {"idx": i, "run": run, "lv": lv, "S": condition.store_prefix(lay)}
    apply_step(ctx, 8, 1)
    with cf_.ThreadPoolExecutor(min(n, 8)) as ex:
        tenants = list(ex.map(one, range(n)))
    for t in tenants:
        S = t["S"]
        t["snap"] = condition.parse_getrange(L.fdbcli_exec(ctx, cf, condition.getrange_cmd(S, S + b"\xff"))) if not ctx.dry else []
    return tenants


def launch_script(ctx, arm, tenants, count, tag, timeout=900):
    """One ssh that starts one load-generator process per tenant at the same moment and waits for all of them."""
    lines = ["pids=()"]
    outputs = []
    for t in tenants:
        wdir = "%s/runs/%s" % (L.REMOTE, t["run"])
        output = "%s/ingest-%s.report.json" % (wdir, tag)
        name, text = L.make_job(ctx.cfg, ctx.builds, "ingest", arm, count=count, output=output, corpus_file="%s/corpus/%d.bin" % (L.REMOTE, count),
                                corpus_sha=ctx.cfg["assets"]["corpus_%d" % count]["sha256"], live=t["lv"], timeout=timeout)
        job_path = "%s/ingest-%s.job.json" % (wdir, tag)
        L.write_remote(ctx, "client1", job_path, text)
        envs = " ".join(shlex.quote("%s=%s" % kv) for kv in L.launch_env(ctx.builds, name).items())
        lines.append("env -i %s %s %s >/dev/null 2>>/tmp/lean-%s-%d.err & pids+=($!)" % (envs, shlex.quote(ctx.builds.exe_path(name)), shlex.quote(job_path), tag, t["idx"]))
        outputs.append(output)
    tpath = "%s/runs/threads-%s.json" % (L.REMOTE, tag)
    lines.append("python3 %s/bin/threadcpu.py %s \"${pids[@]}\" & spid=$!" % (L.REMOTE, shlex.quote(tpath)))
    lines += ["rc=0", 'for p in "${pids[@]}"; do wait $p || rc=1; done', "wait $spid || true", "exit $rc"]
    return "\n".join(lines), outputs, tpath


def one_step(ctx, arm, label, procs_n, consumers, workers, count, tenants, cf, verify):
    inflight = consumers * workers * procs_n
    apply_step(ctx, consumers, workers)
    use = tenants[:procs_n]
    rec = {"label": label, "in_flight_total": inflight, "processes": procs_n, "consumers_per_process": consumers, "writer_workers": workers,
           "spans_per_process": count, "verified": False, "later_steps_on_presplit_ranges": True}
    t0w = time.time()
    restore_many(ctx, cf, use)
    rec["restore_s"] = round(time.time() - t0w, 1)
    tag = "s%d-%d" % (inflight, int(time.time()))
    script, outputs, tpath = launch_script(ctx, arm, use, count, tag)
    before = L.fdb_status(ctx, cf)
    argv = ctx.ssh_argv("client1", ["bash", "-c", script])
    t_release, samples = time.time(), []
    t0 = time.monotonic_ns()
    if ctx.dry:
        ctx.say("DRY  ssh client1 <start %d load-generator processes at once> ..." % procs_n)
        proc = None
    else:
        proc = subprocess.Popen(argv, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    while True:
        samples.append(L.fdb_status(ctx, cf))
        if proc is None or ctx.dry or proc.poll() is not None:
            break
        if time.time() - t_release > 900:
            proc.kill()
            raise L.Stop("step timed out")
        time.sleep(1.0)
    ack_end = time.monotonic_ns()
    t_ack_end = time.time()
    drained, first = L.wait_stable(ctx, cf, cap=300, interval=1.0)
    drained_end = max(ack_end, first)
    reps = []
    for out_path in outputs:
        rc, out, _ = ctx.ssh("client1", ["cat", out_path], check=False, fake=json.dumps({"status": "ok", "accepted_spans": count, "ingest_wall_ns": 1,
                                                                                       "ingest_resources": {"cpu_seconds": 1, "peak_rss_bytes": 1}, "admission_latency": {}}))
        try:
            reps.append(json.loads(out))
        except ValueError:
            reps.append({})
    try:
        thr = json.loads(ctx.ssh("client1", ["cat", tpath], check=False, fake="{}")[1] or "{}")
    except ValueError:
        thr = {}
    rec["threads"] = thr.get("processes", [])
    top = [(p["threads"][0], p) for p in rec["threads"] if p.get("threads")]
    if top:
        best = max(top, key=lambda tp: tp[0]["cores_mean"])
        rec["busiest_thread"] = {"name": best[0]["name"], "cores_mean": best[0]["cores_mean"], "cores_peak_1s": best[0]["cores_peak_1s"], "pid": best[1]["pid"],
                                 "process_cores_mean": best[1].get("process_cores_mean"), "thread_count": best[1].get("thread_count")}
    bad = [i for i, r in enumerate(reps) if r.get("status") != "ok"]
    if (proc is not None and proc.returncode != 0) or bad:
        rec.update(status="failed", error="exit %s; reports not ok: %s (%s)" % (proc.returncode if proc else 0, bad, [reps[i].get("error") for i in bad][:2]))
        return rec
    total = sum(r["accepted_spans"] for r in reps)
    ack_s = (ack_end - t0) / 1e9
    rec["spans_total"] = total
    rec["ack_window_s"] = round(ack_s, 1)
    rec["short_window"] = ack_s < ctx.cfg.get("climb_min_window_s", MIN_WINDOW_S)
    rec["ack_spans_per_s"] = total / ack_s
    rec["sum_of_process_rates"] = sum(r["accepted_spans"] / (r["ingest_wall_ns"] / 1e9) for r in reps)
    rec["durable_spans_per_s"] = total / ((drained_end - t0) / 1e9)
    cpu = sum(r["ingest_resources"]["cpu_seconds"] for r in reps)
    rec["client_cores_used_of_16"] = cpu / ack_s
    rec["client_cpu_ms_per_span"] = cpu * 1000 / total
    rec["client_peak_rss_mib_sum"] = sum(r["ingest_resources"]["peak_rss_bytes"] for r in reps) / 2 ** 20
    adm = [r.get("admission_latency") or {} for r in reps]
    rec["admission_us_median_of_processes"] = {k: round(statistics.median([(a.get(k) or 0) for a in adm]) / 1000, 1) for k in ("p50_ns", "p95_ns", "p99_ns")}
    rec["admission_us_worst_process_p99"] = round(max((a.get("p99_ns") or 0) for a in adm) / 1000, 1)
    rec["retries"] = {k: sum(((r.get(k) or {}).get("value") or 0) for r in reps) for k in ("native_retries", "queue_retries", "export_attempts")}
    win = [s for s in samples if t_release <= s["t"] <= t_ack_end] or samples
    busiest = {}
    for sm in win:
        for p in sm["processes"]:
            role = (p["roles"] or [p.get("class") or "?"])[0]
            busiest[role] = max(busiest.get(role, 0), p["cpu_cores"] or 0)
    rec["db_busiest_cores_by_role"] = {k: round(v, 2) for k, v in sorted(busiest.items(), key=lambda kv: -kv[1])[:6]}
    rec["db_cpu_mean_cores"] = sum(sum(p["cpu_cores"] or 0 for p in sm["processes"]) for sm in win) / len(win)
    rec["storage_queue_max_mb"] = max(s["queue_bytes"] for s in win) / 1e6
    rec["storage_lag_max_s"] = max(s["lag_seconds"] for s in win)
    rec["unhealthy_samples"] = sum(1 for s in win if not s["healthy"])
    rec["limiter"] = {k: {str(v): sum(1 for s in win if s.get(k) == v) for v in {s.get(k) for s in win}} for k in ("limited_by", "batch_limited_by")}
    rec["conflicts"] = (drained["conflicted"] or 0) - (before["conflicted"] or 0) if before["conflicted"] is not None else None
    rec["committed"] = (drained["committed"] or 0) - (before["committed"] or 0) if before["committed"] is not None else None
    rec["status"] = "ok"
    if verify:
        def v1(t):
            return L.offline_job(ctx, "client1", "verify", arm, t["run"], count, t["lv"], tag="-" + tag)[0]
        with cf_.ThreadPoolExecutor(min(len(use), 8)) as ex:
            vs = list(ex.map(v1, use))
        ok = all(v.get("status") == "ok" and (v.get("census") or {}).get("matched") is True and ((v.get("census") or {}).get("actual") or {}).get("unexpected_kvs") == 0
                 and v.get("distinct_spans") == count for v in vs)
        rec["verification"] = {"processes_checked": len(vs), "all_ok": ok, "errors": [v.get("error") for v in vs if v.get("status") != "ok"][:2]}
        rec["verified"] = ok
        if not ok:
            rec["status"] = "failed"
            rec["error"] = "verification failed: %s" % rec["verification"]
    return rec


def climb_variant(ctx, arm, out, soft_epoch):
    cfg = ctx.cfg
    fixed = (cfg.get("climb_steps") or {}).get(arm)    # explicit step list for this arm (lean-tune-1): no stop rule, fixed population per process
    cap = (cfg.get("climb_count") or {}).get(arm) or cfg["climb_cap"] if fixed else cfg["climb_cap"]   # per-process population used for the pre-split and as the largest wave
    procs_ladder = cfg.get("climb_procs", [2, 4, 8, 16]) if not fixed else [st["procs"] for st in fixed]
    result = {"arm": arm, "started": L.now_iso(), "steps": [], "stop_reason": None}
    out["variants"].append(result)
    if cfg.get("local_mode"):
        cf = cfg["local_cluster_file"]
        sha = L.sha256_file(cf)
    else:
        cf, sha, pods = L.deploy_cluster(ctx, cfg.get("climb_stage", 1))
    L.write_remote(ctx, "client1", L.REMOTE + "/bin/threadcpu.py", (HERE / "threadcpu.py").read_text())
    gen_corpus(ctx, cap)
    pmax = max(procs_ladder)
    tenants = setup_tenants(ctx, arm, pmax, cf, sha, cap)
    ctx.say("   [%s] pre-split once: %d tenants x %d spans with the Go own client" % (arm, pmax, cap))
    t0 = time.time()
    apply_step(ctx, 32, 1)
    def pre(t):
        r = L.ingest_blocking(ctx, "client1", "go_pure", t["lv"], cap, "presplit", 1800)
        if r.get("status") != "ok":
            raise L.Stop("pre-split ingest failed (%d): %s" % (t["idx"], r.get("error")))
    rnd = int(cfg.get("presplit_round", pmax))          # tenants ingested concurrently; a drain wait between rounds
    for k in range(0, len(tenants), rnd):
        with cf_.ThreadPoolExecutor(rnd) as ex:
            list(ex.map(pre, tenants[k:k + rnd]))
        if k + rnd < len(tenants):
            L.wait_stable(ctx, cf, cap=900, interval=2.0)
        ctx.say("   [%s] pre-split round %d/%d done" % (arm, k // rnd + 1, -(-len(tenants) // rnd)))
    result["presplit_ingest_s"] = round(time.time() - t0, 1)
    L.wait_stable(ctx, cf, cap=1200, interval=2.0, wait_dd=True)
    restore_many(ctx, cf, tenants)
    result["presplit_total_s"] = round(time.time() - t0, 1)
    result["partitions_after_presplit"] = L.fdb_status(ctx, cf).get("partitions")
    plan = [(1, c, 1, "%d in flight (1 process)" % f) for f, c in SINGLE] + [(p, 32, 1, "%d in flight (%d processes x 32)" % (32 * p, p)) for p in procs_ladder]
    if fixed:
        plan = [(st["procs"], st["consumers"], st.get("workers", 1), "%d process(es) x %d consumers x %d workers = %d in flight" % (
            st["procs"], st["consumers"], st.get("workers", 1), st["procs"] * st["consumers"] * st.get("workers", 1))) for st in fixed]
    best, best_plan, flat, prev_rate = 0.0, None, 0, None
    for i, (p, c, w, label) in enumerate(plan):
        if not ctx.dry and time.time() > soft_epoch:
            result["stop_reason"] = "time: soft cutoff reached"
            break
        count = cap if fixed else per_process_population(ctx, prev_rate, p, cap)
        gen_corpus(ctx, count)
        rec = one_step(ctx, arm, label, p, c, w, count, tenants, cf, verify=(i == 0))
        result["steps"].append(rec)
        ctx.log("climb_step", arm=arm, label=label, status=rec["status"], rate=rec.get("ack_spans_per_s"), error=rec.get("error"))
        if rec["status"] != "ok":
            result["stop_reason"] = "step failed: %s" % rec.get("error")
            if fixed:
                continue                                   # a fixed list: the remaining steps are still measured
            break
        rate = rec["ack_spans_per_s"]
        prev_rate = rate
        flat = 0 if rate > best * 1.05 else flat + 1
        if rate > best:
            best, best_plan = rate, (p, c, w, label)
        if flat >= 2 and not fixed:
            result["stop_reason"] = "flat: less than 5%% better than the best so far for two steps in a row (best %.0f spans/s at %s)" % (best, best_plan[3])
            break
    else:
        result["stop_reason"] = "reached the end of the step list" if fixed else "reached the top of the ladder (%d processes x 32 in flight)" % pmax
    if best_plan is not None and not fixed and not ctx.dry and time.time() < soft_epoch:
        p, c, w, label = best_plan
        count = per_process_population(ctx, prev_rate, p, cap)
        gen_corpus(ctx, count)
        rec = one_step(ctx, arm, label, p, c, w, count, tenants, cf, verify=True)
        rec["repeat_of_best"] = True
        result["steps"].append(rec)
    if arm == "go_pure" and cfg.get("climb_extra_workers") and not ctx.dry and time.time() < soft_epoch:
        for w in (4, 8):                                  # extra rows, Go own client only: more workers per consumer in ONE process
            if time.time() > soft_epoch:
                break
            count = per_process_population(ctx, prev_rate, 1, cap)
            gen_corpus(ctx, count)
            rec = one_step(ctx, arm, "EXTRA Go-only: 1 process, 32 consumers x %d writer_workers = %d in flight" % (w, 32 * w), 1, 32, w, count, tenants, cf, verify=False)
            result["steps"].append(rec)
            if rec["status"] != "ok":
                break
    result["best_label"] = best_plan[3] if best_plan else None
    result["best_ack_spans_per_s"] = best
    result["ended"] = L.now_iso()
    return result


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("mode", choices=["run"])
    ap.add_argument("--config", required=True)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--variants", default="rust_native,go_pure,go_native")
    args = ap.parse_args(argv)
    cfg = L.load_cfg(args.config)
    if cfg.get("remote_root"):
        L.REMOTE = cfg["remote_root"]
    ctx = L.Ctx(cfg, args.dry_run)
    ctx.builds = L.Builds(cfg)
    first = ctx.s.get("first_server_epoch", int(time.time()))
    soft = first + cfg["soft_cutoff_minutes"] * 60
    out = {"campaign": ctx.campaign, "kind": "parallelism_ladder_1_node", "variants": []}
    for arm in args.variants.split(","):
        if not ctx.dry and out["variants"] and time.time() + cfg.get("climb_variant_min_s", 25 * 60) > soft:
            ctx.say("STOP: not enough time left for variant %s" % arm)
            break
        try:
            climb_variant(ctx, arm, out, soft)
        except L.Stop as exc:
            ctx.say("VARIANT FAILED (%s): %s" % (arm, str(exc)[:500]))
            out["variants"][-1]["stop_reason"] = "variant failed: %s" % str(exc)[:400]
        if not ctx.dry:
            (ctx.state / "climb-results.json").write_text(json.dumps(out, indent=1, default=str))
    ctx.say("== climb finished; %s ==" % (ctx.state / "climb-results.json"))


if __name__ == "__main__":
    sys.exit(main())
