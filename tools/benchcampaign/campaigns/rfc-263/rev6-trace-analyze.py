#!/usr/bin/env python3
"""Bounded full-state trace/CSV attribution; traced diagnostics, never timing prices."""
from __future__ import annotations
import argparse
import bisect
from collections import Counter, defaultdict
from concurrent.futures import ProcessPoolExecutor
import csv
import hashlib
import json
import os
from pathlib import Path
import re
import unittest

CPUS = {0, 1, 6, 7, 11, 12, 13, 18, 19, 23}


def save(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def sha(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for b in iter(lambda: f.read(1024 * 1024), b""):
            h.update(b)
    return h.hexdigest()


def overlap(a, b, s, e):
    return max(0, min(b, e) - max(a, s))


def bounds(a, b, s, e, lo, hi):
    if not (a <= b and s <= e and lo <= hi):
        raise ValueError("invalid interval/epoch bounds")
    f = lambda epoch: overlap(a, b, epoch + s, epoch + e)
    ends = [f(lo), f(hi)]
    candidates = ends + [f(max(lo, min(hi, a-s))), f(max(lo, min(hi, b-e)))]
    return min(ends), max(candidates)


def merged(pairs):
    result = []
    for a,b in sorted(pairs):
        if result and a <= result[-1][1]: result[-1] = (result[-1][0], max(b,result[-1][1]))
        else: result.append((a,b))
    return result


def cleanup_execution(stack):
    return re.search(r"^runtime\.(?:runCleanups|callCleanup)(?:[ .]|$)", stack, re.MULTILINE) is not None


def validate_indices(rows, count, workers):
    if not rows or len(rows) != count or len({r["index"] for r in rows}) != count or {r["index"] for r in rows} != set(range(count)):
        raise ValueError("empty/duplicate/missing CSV indices")
    if {r["worker"] for r in rows} != set(range(workers)) or any(r["worker"] != r["index"] % workers for r in rows):
        raise ValueError("missing/wrong CSV worker population")


def contribution_bounds(a, b, s, e, lo, hi, margin):
    low = bounds(a+margin,b-margin,s,e,lo,hi)[0] if b-a >= 2*margin else 0
    high = bounds(a-margin,b+margin,s,e,lo,hi)[1]
    return low,high


def alignment(workers, by_worker, margin):
    if not workers or len(workers) != len(by_worker) or set(w["worker"] for w in workers) != set(range(len(workers))):
        raise ValueError("empty/inconsistent worker mapping")
    for w in workers:
        if not by_worker[w["worker"]] or not (w["created"] < w["first_running"] <= w["start_wait"] < w["release"] <= w["after_release_running"]):
            raise ValueError("missing actual worker barrier/first sample")
    lo = max(w["after_release_running"] - by_worker[w["worker"]][0]["start"] for w in workers) - margin
    hi = min(w["release"] for w in workers) + margin
    if lo > hi:
        raise ValueError(f"inconsistent bounded epoch {lo}>{hi}")
    return lo, hi


class Guards(unittest.TestCase):
    def test_overlap_bounds_values(self):
        self.assertEqual(bounds(100, 140, 0, 100, 50, 90), (40, 40))
        self.assertEqual(bounds(100, 140, 0, 20, 90, 110), (10, 20))
        self.assertEqual(bounds(100, 140, 0, 20, 0, 50), (0, 0))
        self.assertEqual(bounds(100, 140, 0, 20, 80, 80), (0, 0))
        with self.assertRaises(ValueError): bounds(2, 1, 0, 1, 0, 1)
        with self.assertRaises(ValueError): bounds(0, 1, 0, 1, 2, 1)

    def test_union_values(self):
        self.assertEqual(merged([]), [])
        self.assertEqual(merged([(3,5),(1,4),(8,9)]), [(1,5),(8,9)])

    def test_reason_categories(self):
        self.assertEqual(category("GC mark assist wait for work", ""), "gc_assist_wait")
        self.assertEqual(category("GC weak to strong wait", ""), "weak_promotion_wait")
        self.assertEqual(category("sync", "timeoutQueue.schedule"), "queue_path_sync")
        self.assertEqual(category("sync", "other"), "wait:sync")
        self.assertEqual(category("select", ""), "wait:select")

    def test_cleanup_executor_not_registration(self):
        self.assertTrue(cleanup_execution("runtime.runCleanups @ 0x123\n GOROOT/src/runtime/mcleanup.go:716"))
        self.assertTrue(cleanup_execution("runtime.callCleanup @ 0x123\n GOROOT/src/runtime/mcleanup.go:202"))
        self.assertFalse(cleanup_execution("runtime.AddCleanup @ 0x123\n GOROOT/src/runtime/mcleanup.go:76"))
        self.assertFalse(cleanup_execution("runtime.gcAssistAlloc\nruntime.AddCleanup\n GOROOT/src/runtime/mcleanup.go:76"))
        self.assertFalse(cleanup_execution("runtime.runCleanupsRegistration @0x123"))

    def test_csv_population_and_clock_edges(self):
        valid=[{"index":i,"worker":i%2} for i in range(4)]
        validate_indices(valid,4,2)
        for rows in ([],valid[:-1],valid[:3]+[valid[2]],valid[:3]+[{"index":9,"worker":1}]):
            with self.assertRaises(ValueError): validate_indices(rows,4,2)
        with self.assertRaises(ValueError): validate_indices(valid,4,3)
        self.assertEqual(contribution_bounds(100,140,0,100,50,90,1),(38,42))

    def test_alignment_population_and_barrier(self):
        w = {"worker": 0, "created": 1, "first_running": 2, "start_wait": 3, "release": 100, "after_release_running": 110}
        self.assertEqual(alignment([w], [[{"start": 10}]], 1), (99, 101))
        for ws, rows in (([], []), ([w], [[]]), ([dict(w, start_wait=0)], [[{"start": 10}]]), ([dict(w, worker=1)], [[{"start": 10}]])):
            with self.assertRaises(ValueError): alignment(ws, rows, 1)
        with self.assertRaises(ValueError): alignment([w], [[{"start": 0}]], 1)


def category(reason, stack):
    if "GC mark assist" in reason: return "gc_assist_wait"
    if "weak to strong" in reason: return "weak_promotion_wait"
    if reason == "sync" and "timeoutQueue" in stack: return "queue_path_sync"
    return "wait:" + (reason or "unspecified")


def intervals(events, stack_table):
    result, previous, reconciled = [], None, 0
    for event in events:
        t, fr, to, reason, sid, actor, esid = event
        if previous is not None:
            a, state, meta = previous
            if fr == "Undetermined" and to == state:
                continue
            if fr != state:
                reconciled += 1
            if t > a:
                data = dict(meta)
                data.update(a=a, b=t, state=state)
                if state == "Waiting": data.update(waker=actor, wake_stack=esid)
                result.append(data)
        meta = {"stack": sid, "reason": reason, "waker": actor, "wake_stack": esid}
        if to == "Runnable" and previous is not None and previous[1] == "Waiting":
            meta["reason"], meta["stack"] = previous[2]["reason"], previous[2]["stack"]
        previous = (t, to, meta)
    return result, reconciled


def analyze(payload):
    evidence, record, margin = payload
    evidence = Path(evidence)
    run, event_path = Path(record["run"]), Path(record["events"])
    arm = run.name.rsplit("-", 1)[-1]
    out = evidence / "rev6-trace-analysis" / arm
    out.mkdir(parents=True, exist_ok=False)
    manifest = json.loads((run / "manifest.json").read_text())
    expected_workers = manifest["config"]["workers"]
    if expected_workers != 8 or manifest["go_version"] != "go1.26.9" or manifest["config"]["profile"] != "trace" or manifest["config"].get("rate", 0) != 0:
        raise ValueError("not this unpaced pinned8w diagnostic population")
    rows, by_worker = [], [[] for _ in range(expected_workers)]
    with (run / "samples.csv").open() as f:
        for row in csv.DictReader(f):
            if row["phase"] != "measure" or row["error"] or row["check_status"] != "ok": raise ValueError("failed/nonmeasured sample")
            s = {"index": int(row["index"]), "worker": int(row["worker"]), "start": int(row["start_offset_ns"]), "latency": int(row["latency_ns"])}
            if s["worker"] != s["index"] % expected_workers: raise ValueError("worker/index mismatch")
            s["end"] = s["start"] + s["latency"]
            rows.append(s); by_worker[s["worker"]].append(s)
    validate_indices(rows,manifest["config"]["iterations"],expected_workers)
    for ss in by_worker:
        if any(a["end"] > b["start"] for a, b in zip(ss, ss[1:])): raise ValueError("overlapping worker operations")
    stacks, stack_ids = [], {}
    def intern(text):
        if text not in stack_ids:
            stack_ids[text] = len(stacks); stacks.append(text)
        return stack_ids[text]
    goroutines, workers, syncs, range_events = defaultdict(list), [], [], []
    cleanup, callback = set(), set()
    event_hash, kinds, end = hashlib.sha256(), Counter(), None
    for line in event_path.open("rb"):
        event_hash.update(line)
        r = json.loads(line)
        typ = r["type"]
        if typ == "worker": workers.append(r["data"]); continue
        if typ == "end": end = r; continue
        kinds[r["kind"]] += 1
        if typ == "sync":
            if "mono" in r: syncs.append(r["mono"] - r["trace"])
        elif typ == "range": range_events.append(r)
        elif typ == "go_state":
            stack, es = r["stack"], r["ev_stack"]
            if cleanup_execution(stack): cleanup.add(r["gid"])
            if cleanup_execution(es) and r["ev_gid"] >= 0: cleanup.add(r["ev_gid"])
            if "(*timeoutQueue).fire" in stack or "(*timeoutQueueRoot).fire" in stack: callback.add(r["gid"])
            goroutines[r["gid"]].append((r["time"], r["from"], r["to"], r["reason"], intern(stack), r["ev_gid"], intern(es)))
    if not end or dict(kinds) != end["event_counts"] or event_hash.hexdigest() != record["events_sha256"]:
        raise ValueError("missing/changed/incomplete full event stream")
    if sha(run / "samples.csv") != record["samples_sha256"] or sha(run / "manifest.json") != record["manifest_sha256"]:
        raise ValueError("CSV/manifest identity changed")
    if not syncs or max(abs(x) for x in syncs) > margin: raise ValueError("sync clock bound failed")
    lo, hi = alignment(workers, by_worker, margin)
    worker_ids = {w["gid"]: w["worker"] for w in workers}
    if len(worker_ids) != 8 or end["workers"] != 8: raise ValueError("worker mapping not8")
    per_worker, cleanup_running = {}, []
    worker_reconciles, cleanup_reconciles, ambiguous_cleanup = 0, 0, []
    for gid in set(worker_ids) | cleanup:
        spans, rc = intervals(goroutines[gid], stacks)
        if gid in worker_ids:
            worker_reconciles += rc
            for span in spans:
                state = span["state"]
                span["category"] = category(span["reason"], stacks[span["stack"]]) if state == "Waiting" else "runnable" if state == "Runnable" else state.lower()
                span["cleanup_waker"] = span["waker"] in cleanup or cleanup_execution(stacks[span["wake_stack"]])
            per_worker[worker_ids[gid]] = spans
        if gid in cleanup:
            cleanup_reconciles += rc
            if rc: ambiguous_cleanup.append(gid)
            else: cleanup_running += [(s["a"], s["b"]) for s in spans if s["state"] == "Running"]
    if worker_reconciles:
        raise ValueError(f"worker state discontinuities {worker_reconciles}; refusing direct state attribution")
    names = Counter(r["name"] for r in range_events)
    active, ranges, partial = {}, defaultdict(list), Counter()
    for r in range_events:
        key = (r["name"], r["scope"])
        if r["kind"] == "RangeBegin":
            if key in active: raise ValueError("duplicate range begin")
            active[key] = r["time"]
        elif r["kind"] == "RangeActive":
            if key not in active: partial[r["name"]] += 1
        else:
            if key in active: ranges[key].append((active.pop(key), r["time"]))
            else: partial[r["name"]] += 1
    for name, scope in active: partial[name] += 1
    stw = sorted(pair for (name, scope), ps in ranges.items() if name.startswith("stop-the-world") for pair in ps)
    assists = defaultdict(list)
    for (name, scope), ps in ranges.items():
        if "assist" in name.lower():
            match = re.fullmatch(r"Goroutine\((\d+)\)", scope)
            if match and int(match[1]) in worker_ids: assists[worker_ids[int(match[1])]] += ps
    extras = {"stw_overlap": merged(stw), "cleanup_running_overlap": merged(cleanup_running)}
    extras.update({"assist_"+str(w): merged(ps) for w,ps in assists.items()})
    extra_indexes = {key: (ps, [a for a,b in ps], [b for a,b in ps]) for key,ps in extras.items()}
    sorted_rows = sorted(rows, key=lambda s: (-s["latency"], s["index"]))
    top1, top01 = {s["index"] for s in sorted_rows[:max(1,len(rows)//100)]}, {s["index"] for s in sorted_rows[:max(1,len(rows)//1000)]}
    group_totals = {name: {"count": 0, "latency_ns": 0, "bounds_ns": defaultdict(lambda: [0,0])} for name in ("all", "tail1pct", "tail01pct")}
    stack_groups = defaultdict(lambda: {"count":0,"duration_ns":0,"tail1pct_overlap_min_ns":0,"tail1pct_overlap_max_ns":0})
    span_starts = {w: [s["a"] for s in ss] for w,ss in per_worker.items()}
    span_ends = {w: [s["b"] for s in ss] for w,ss in per_worker.items()}
    def contribute(total, key, a, b, sample):
        low, high = contribution_bounds(a,b,sample["start"],sample["end"],lo,hi,margin)
        if high:
            total[key][0] += low; total[key][1] += high
        return low, high
    with (out / "per-sample.jsonl").open("w") as f:
        for sample in rows:
            w = sample["worker"]
            totals = defaultdict(lambda:[0,0])
            left = bisect.bisect_right(span_ends[w], lo+sample["start"]-margin)
            right = bisect.bisect_left(span_starts[w], hi+sample["end"]+margin)
            for span in per_worker[w][left:right]:
                low, high = contribute(totals,span["category"],span["a"],span["b"],sample)
                if span["cleanup_waker"] and span["state"] in ("Waiting","Runnable"):
                    contribute(totals,"cleanup_waker_"+span["state"].lower(),span["a"],span["b"],sample)
                if high and span["state"] in ("Waiting","Runnable"):
                    key = (span["category"], span["stack"])
                    if sample["index"] in top1:
                        stack_groups[key]["tail1pct_overlap_min_ns"] += low; stack_groups[key]["tail1pct_overlap_max_ns"] += high
            for key in ("assist_"+str(w), "stw_overlap", "cleanup_running_overlap"):
                ps, starts, ends = extra_indexes.get(key, ([],[],[]))
                first = bisect.bisect_right(ends, lo+sample["start"]-margin)
                last = bisect.bisect_left(starts, hi+sample["end"]+margin)
                for a,b in ps[first:last]: contribute(totals,"gc_assist_range" if key.startswith("assist_") else key,a,b,sample)
            for g in ("all",) + (("tail1pct",) if sample["index"] in top1 else ()) + (("tail01pct",) if sample["index"] in top01 else ()):
                target = group_totals[g]; target["count"] += 1; target["latency_ns"] += sample["latency"]
                for key,pair in totals.items():
                    target["bounds_ns"][key][0] += pair[0]; target["bounds_ns"][key][1] += pair[1]
            f.write(json.dumps(dict(sample, contributions_ns=dict(totals)))+"\n")
    reasons = Counter()
    with (out / "worker-spans.jsonl").open("w") as f:
        for w,ss in per_worker.items():
            for s in ss:
                if s["state"] in ("Waiting","Runnable"):
                    reasons[s["category"]] += 1
                    key = (s["category"],s["stack"])
                    stack_groups[key]["count"] += 1; stack_groups[key]["duration_ns"] += s["b"]-s["a"]
                    f.write(json.dumps(dict(s,worker=w))+"\n")
    top_stacks = sorted((dict(v,category=k[0],stack_id=k[1],stack=stacks[k[1]]) for k,v in stack_groups.items()),key=lambda v:-v["tail1pct_overlap_max_ns"])[:40]
    save(out/"stacks.json",stacks)
    save(out/"workers.json",workers)
    result = {"arm":arm,"scope":"one instrumented8w trace, not timing pricing or causal proof", "samples":len(rows),
              "events":end["events"],"epoch_bounds_ns":[lo,hi],"epoch_width_ns":hi-lo,"sync_mono_minus_trace_ns":syncs,
              "input_hashes":{k:record[k] for k in ("events_sha256","samples_sha256","manifest_sha256","trace_sha256")},
              "decoder_binary_sha256":record["decoder_binary_sha256"],"worker_state_reconciliations":worker_reconciles,
              "cleanup_state_reconciliations":cleanup_reconciles,"ambiguous_cleanup_gids_excluded":ambiguous_cleanup,
              "script_sha256":sha(Path(__file__)),"event_edge_margin_ns":margin,
              "cleanup_gids":sorted(cleanup),"cleanup_running_intervals":len(cleanup_running),"cleanup_running_total_ns":sum(b-a for a,b in cleanup_running),
              "observed_callback_gids":sorted(callback),"callback_absence_limit":"no observed callback stack is not proof of absence; use process-age bound",
              "range_names":dict(names),"partial_ranges":dict(partial),"worker_wait_counts":dict(reasons),
              "trace_latency_ns":{q:sorted(s["latency"] for s in rows)[int((len(rows)-1)*p)] for q,p in (("p50",.5),("p99",.99),("p999",.999))},
              "groups":group_totals,"top_tail_stacks":top_stacks,
              "interpretation":"Waiting/Runnable are direct worker state spans; Running is NOT CPU time. GC assist/STW/cleanup overlaps are nonadditive associations. Per-category min/max sums are conservative over one common unknown epoch."}
    save(out/"summary.json",result)
    save(out/"artifact-sha256.json",{p.name:sha(p) for p in out.iterdir() if p.is_file() and p.name!="artifact-sha256.json"})
    return json.loads(json.dumps(result))


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("--evidence",required=True,type=Path)
    p.add_argument("--self-check",action="store_true")
    p.add_argument("--clock-margin-ns",type=int,default=64)
    args=p.parse_args()
    os.sched_setaffinity(0,CPUS);os.setpriority(os.PRIO_PROCESS,0,19)
    out=args.evidence/"rev6-trace-analysis";out.mkdir(exist_ok=True)
    with (out/"guard-tests.log").open("w") as log:
        tests=unittest.TextTestRunner(stream=log,verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(Guards))
    if not tests.wasSuccessful() or tests.testsRun!=6: raise RuntimeError("guard tests failed")
    if args.self_check:return
    records=json.loads((args.evidence/"decoder/extraction-inputs.json").read_text())["runs"]
    if len(records)!=3:raise ValueError("expected3 frozen traces")
    with ProcessPoolExecutor(max_workers=3) as pool:
        results=list(pool.map(analyze,[(str(args.evidence),r,args.clock_margin_ns) for r in records]))
    save(out/"summary.json",{"script_sha256":sha(Path(__file__)),"runs":results})
    print(out/"summary.json")

if __name__=="__main__":main()
