#!/usr/bin/env python3
"""Offline R16 diagnostics. Usage: python3 -B analyze-instruments-r16.py ROOT NEW_OUT.

Only the standard library is used. Inputs are copied unchanged before parsing.
Unknown formats or non-unique phase-marker assignments cause a nonzero exit;
status.json records the failure and raw/ remains available for inspection.
"""
import csv
import hashlib
import io
import json
import re
import shutil
import sys
from collections import Counter, defaultdict
from decimal import Decimal, InvalidOperation
from pathlib import Path

ARMS = ("new", "cgo")
FILTER = (
    "read", "write", "readv", "writev", "pread64", "pwrite64", "sendmsg",
    "recvfrom", "sendto", "recvmsg", "recvmmsg", "sendmmsg", "epoll_pwait",
    "epoll_wait", "epoll_ctl", "futex", "nanosleep", "clock_nanosleep",
    "sched_yield", "getrusage",
)
BASE_EVENTS = {"context-switches", "cpu-migrations", "task-clock"}
WHOLE_SCOPE = (
    "wholeprocess/N incl startup/identity/fixture/warmup/drains/validation/shutdown; "
    "NOT phase counts"
)
PHASE_SCOPE = (
    "phase-window syscall ENTRY counts in [end(B getrusage), start(C getrusage)), "
    "excluding A..E marker calls; divided by 200 measured transactions"
)
ALIGNMENT = (
    "samples.csv has a relative epoch taken just after B, with an unknown "
    "submillisecond gap. No exact per-transaction boundary alignment is claimed. "
    "Do not interpret the merged syscall timeline as transaction ownership."
)
NO_TIMING = (
    "Instrumentation is diagnostic only: no latency, throughput or CPU-performance "
    "claims are made. Raw syscall elapsed values and perf task-clock are retained "
    "for provenance, not used as timed comparisons."
)
MARKERS = {
    "A": ("stats_after_identity",),
    "B": ("stats_before",),
    "C": ("stats_after",),
    "D": ("lifecycle", "stats_after_measurement_drain"),
    "E": ("lifecycle", "stats_after_shutdown"),
}
STAT_KEYS = ("process_user_cpu_us", "process_system_cpu_us", "process_high_water_rss_kb")


class FormatError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise FormatError(message)


def integer(value, where, minimum=0):
    require(type(value) is int and value >= minimum, f"{where}: expected integer >= {minimum}")
    return value


def number(text, where, minimum=Decimal(0)):
    try:
        value = Decimal(text)
    except (InvalidOperation, ValueError, TypeError) as exc:
        raise FormatError(f"{where}: invalid decimal {text!r}") from exc
    require(value.is_finite() and value >= minimum, f"{where}: invalid/nonfinite/negative number {text!r}")
    return value


def stamp(text, where):
    require(re.fullmatch(r"\d+\.\d{1,9}", text) is not None, f"{where}: expected decimal epoch/duration")
    return int(number(text, where) * 1_000_000_000)


def epoch(ns):
    return None if ns is None else f"{ns // 1_000_000_000}.{ns % 1_000_000_000:09d}"


def ratio(value, denominator):
    return None if value is None else float(Decimal(str(value)) / denominator)


def json_read(path):
    def unique(pairs):
        obj = {}
        for key, value in pairs:
            require(key not in obj, f"{path}: duplicate JSON key {key!r}")
            obj[key] = value
        return obj
    with path.open(encoding="utf-8") as stream:
        obj = json.load(stream, object_pairs_hook=unique,
                        parse_constant=lambda text: (_ for _ in ()).throw(FormatError(f"{path}: {text}")))
    require(isinstance(obj, dict), f"{path}: expected JSON object")
    return obj


def json_write(path, obj):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj, indent=2, sort_keys=True, allow_nan=False) + "\n", encoding="utf-8")


def csv_write(path, rows, fields):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="", encoding="utf-8") as stream:
        writer = csv.DictWriter(stream, fieldnames=fields)
        writer.writeheader()
        writer.writerows(rows)


def validate_run(folder, count):
    summary = json_read(folder / "summary.json")
    manifest = json_read(folder / "manifest.json")
    result = json_read(folder / "result.json")
    require(summary.get("success") is True, f"{folder}: unsuccessful summary")
    for key, expected in (("completed", count), ("requested", count),
                          ("warmup_completed", 200), ("warmup_requested", 200),
                          ("errors", 0), ("warmup_errors", 0)):
        require(integer(summary.get(key), f"{folder}: {key}") == expected,
                f"{folder}: {key} != {expected}")
    require(summary.get("failure") == "" and summary.get("persisted_validation") == "ok",
            f"{folder}: failure or persisted validation not ok")
    lifecycle = summary.get("lifecycle", {})
    require(lifecycle.get("complete") is True and lifecycle.get("measurement_barrier_complete") is True,
            f"{folder}: incomplete lifecycle")
    config = summary.get("config", {})
    for key, value in (("workload", "read1000"), ("iterations", count), ("warmup", 200), ("workers", 1)):
        require(config.get(key) == value, f"{folder}: unexpected config.{key}")
    require(config == manifest.get("config"), f"{folder}: summary/manifest config mismatch")
    require(summary.get("backend") == manifest.get("backend"), f"{folder}: backend identity mismatch")
    require(integer(result.get("returncode"), f"{folder}: returncode") == 0, f"{folder}: nonzero returncode")
    require(result.get("run_id") == config.get("run_id") and bool(config.get("run_id")),
            f"{folder}: run_id mismatch/missing")
    integer(manifest.get("pid"), f"{folder}: manifest.pid", 1)
    for key in ("retries", "warmup_retries"):
        integer(summary.get(key), f"{folder}: {key}")
    return summary, manifest, result


def run_info(summary, manifest):
    return {
        "measured_transactions": summary["completed"],
        "warmup_transactions": summary["warmup_completed"],
        "measured_callback_retries": summary["retries"],
        "warmup_callback_retries": summary["warmup_retries"],
        "run_id": summary["config"]["run_id"], "pid": manifest["pid"],
        "backend": summary["backend"],
    }


def parse_summary(folder):
    summary, manifest, _ = validate_run(folder, 5000)
    rows, total, header, separators = {}, None, False, 0
    path = folder / "strace-summary.txt"
    for line_no, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        text = line.strip()
        where = f"{path}:{line_no}"
        if not text:
            continue
        if re.fullmatch(r"% time\s+seconds\s+usecs/call\s+calls\s+errors\s+syscall", text):
            require(not header and not rows, f"{where}: duplicate/misplaced header")
            header = True
            continue
        if re.fullmatch(r"[- ]+", text):
            require(header, f"{where}: separator before header")
            separators += 1
            continue
        fields = text.split()
        require(header and total is None and len(fields) in (5, 6), f"{where}: unexpected strace-c row {text!r}")
        for index in (0, 1):
            number(fields[index], where)
        require(fields[2].isdigit() and fields[3].isdigit(), f"{where}: invalid calls/usecs")
        count, errors = int(fields[3]), int(fields[4]) if len(fields) == 6 and fields[4].isdigit() else 0
        require(len(fields) == 5 or fields[4].isdigit(), f"{where}: invalid errors")
        require(0 <= errors <= count, f"{where}: errors exceed calls")
        name = fields[-1]
        require(re.fullmatch(r"[A-Za-z_][A-Za-z_0-9]*", name) is not None, f"{where}: invalid syscall name")
        record = {"calls": count, "errors": errors,
                  "wholeprocess_calls_divided_by_5000": ratio(count, 5000),
                  "wholeprocess_errors_divided_by_5000": ratio(errors, 5000)}
        if name == "total":
            total = record
        else:
            require(name not in rows, f"{where}: duplicate syscall {name}")
            rows[name] = record
    require(header and separators >= 2 and rows and total is not None, f"{path}: missing header/rows/total")
    for key in ("calls", "errors"):
        require(sum(row[key] for row in rows.values()) == total[key], f"{path}: {key} do not sum to total")
    return {**run_info(summary, manifest), "scope": WHOLE_SCOPE, "syscalls": rows, "total": total}


def parse_perf(folder, selected):
    summary, manifest, _ = validate_run(folder, 5000)
    path = folder / "perf-stat.csv"
    counters, ancillary = {}, []
    for line_no, fields in enumerate(csv.reader(io.StringIO(path.read_text(encoding="utf-8"))), 1):
        if not fields or all(not cell.strip() for cell in fields):
            continue
        if fields[0].lstrip().startswith("#"):
            continue
        where = f"{path}:{line_no}"
        fields = [cell.strip() for cell in fields]
        require(len(fields) >= 3, f"{where}: short perf row {fields!r}")
        event = fields[2]
        if event in {"seconds time elapsed", "seconds user", "seconds sys"}:
            number(fields[0], where)
            require(not fields[1] and not any(fields[3:]), f"{where}: unexpected elapsed/user/sys format")
            ancillary.append(fields)
            continue
        require(event in selected, f"{where}: event {event!r} not in selected_events")
        require(event not in counters and 5 <= len(fields) <= 8, f"{where}: duplicate event or unexpected column count")
        raw_value, unit = fields[:2]
        unavailable = raw_value in {"<not supported>", "<not counted>"}
        if unavailable:
            value = None
        else:
            value = number(raw_value, where)
            if event in {"context-switches", "cpu-migrations"}:
                require(value == value.to_integral_value(), f"{where}: fractional event count")
            if event == "task-clock":
                require(unit in {"msec", "ms"} and value > 0, f"{where}: task-clock unit/value")
        runtime = number(fields[3], where) if fields[3] else None
        percent = number(fields[4], where) if fields[4] else None
        if not unavailable:
            require(runtime is not None and runtime > 0 and percent is not None and 0 < percent <= 100,
                    f"{where}: missing/invalid runtime or running percentage")
        elif percent is not None:
            require(percent <= 100, f"{where}: running percentage > 100")
        numeric = None if value is None else int(value) if value == value.to_integral_value() else float(value)
        counters[event] = {
            "status": raw_value if unavailable else "counted", "value": numeric,
            "value_decimal": None if value is None else str(value), "unit": unit or "count",
            "runtime_ns_decimal": None if runtime is None else str(runtime),
            "running_percent_decimal": None if percent is None else str(percent),
            "wholeprocess_value_divided_by_5000": ratio(value, 5000),
            "raw_fields": fields,
        }
    require(set(counters) == set(selected), f"{path}: missing selected events {set(selected) - set(counters)}")
    return {**run_info(summary, manifest), "scope": WHOLE_SCOPE,
            "status": "parsed", "counters": counters, "ancillary_raw_rows": ancillary}


def finish_event(tid, path, line_no, start, body, source_lines, resumed=None):
    match = re.fullmatch(r"([a-z][a-z0-9_]*)\((.*)\)\s+=\s+(.*?)(?:\s+<([0-9]+\.[0-9]{1,9}|unavailable)>)?", body)
    require(match is not None, f"{path}:{line_no}: unrecognized syscall completion {body!r}")
    name, arguments, returned, elapsed = match.groups()
    require(name in FILTER, f"{path}:{line_no}: unrequested traced syscall {name}")
    require(re.match(r"(?:-?\d+|0x[0-9a-fA-F]+|\?)(?:\s|$)", returned) is not None,
            f"{path}:{line_no}: invalid syscall result {returned!r}")
    require(elapsed is not None or returned == "?", f"{path}:{line_no}: missing syscall elapsed value")
    duration = None if elapsed in (None, "unavailable") else stamp(elapsed, str(path))
    end = None if duration is None else start + duration
    return {
        "tid": tid, "syscall": name, "entry_ns": start, "end_ns": end,
        "duration_ns": duration, "arguments": arguments, "result": returned,
        "error": bool(re.match(r"-1\s", returned)),
        "restart_pseudo_result": "ERESTART" in returned,
        "completion_known": not returned.startswith("?"),
        "elapsed_available": duration is not None, "unfinished_resumed": resumed is not None,
        "resume_ns": resumed, "source_file": path.name, "source_lines": source_lines,
        "termination_bound_ns": None,
    }


def parse_trace_file(path):
    tid = int(path.name.split(".")[1])
    events, pending, signals, terminal, last = [], None, Counter(), None, -1
    for line_no, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        where = f"{path}:{line_no}"
        match = re.fullmatch(r"(\d+\.\d{1,9})\s+(.*)", line)
        require(match is not None, f"{where}: missing timestamp or unexpected line {line!r}")
        now, body = stamp(match[1], where), match[2]
        require(now >= last and terminal is None, f"{where}: nonmonotonic timestamp or data after termination")
        last = now
        sig = re.fullmatch(r"--- (SIG[A-Z0-9]+) (.*) ---", body)
        if sig:
            signals[sig[1]] += 1
            continue
        if re.fullmatch(r"\+\+\+ (?:exited with \d+|killed by SIG[A-Z0-9]+(?: \(core dumped\))?) \+\+\+", body):
            terminal = {"timestamp_ns": now, "text": body, "source_line": line_no}
            continue
        resumed = re.fullmatch(r"<\.\.\. ([a-z][a-z0-9_]*) resumed>(.*)", body)
        if resumed:
            require(pending is not None and resumed[1] == pending["name"], f"{where}: unmatched resumed syscall")
            event = finish_event(tid, path, line_no, pending["entry"],
                                 pending["prefix"] + resumed[2], [pending["line"], line_no], now)
            events.append(event)
            pending = None
            continue
        require(pending is None, f"{where}: new entry while a syscall remains unfinished")
        if body.endswith("<unfinished ...>"):
            prefix = body[:-len("<unfinished ...>")].rstrip()
            entry = re.match(r"([a-z][a-z0-9_]*)\(", prefix)
            require(entry is not None and entry[1] in FILTER, f"{where}: invalid unfinished syscall")
            # Preserve the separator before <unfinished ...>: resumed text may continue a token.
            prefix = body[:-len("<unfinished ...>")]
            pending = {"name": entry[1], "prefix": prefix, "entry": now, "line": line_no}
            continue
        events.append(finish_event(tid, path, line_no, now, body, [line_no]))
    require(terminal is not None, f"{path}: missing terminal record (possibly truncated trace)")
    if pending is not None:
        events.append({
            "tid": tid, "syscall": pending["name"], "entry_ns": pending["entry"],
            "end_ns": None, "duration_ns": None, "arguments": pending["prefix"].split("(", 1)[1],
            "result": None, "error": False, "restart_pseudo_result": False,
            "completion_known": False, "elapsed_available": False, "unfinished_resumed": False,
            "resume_ns": None, "source_file": path.name, "source_lines": [pending["line"]],
            "termination_bound_ns": terminal["timestamp_ns"], "unfinished_at_termination": True,
        })
    for event in events:
        if event["end_ns"] is None:
            event["termination_bound_ns"] = terminal["timestamp_ns"]
    return events, {"tid": tid, "signals": dict(signals), "termination": terminal,
                    "syscall_entries": len(events), "raw_file": path.name}


def rusage_signature(event):
    if event["syscall"] != "getrusage" or not event["arguments"].startswith("RUSAGE_SELF,"):
        return None
    require(event["result"] == "0" and event["end_ns"] is not None,
            f"{event['source_file']}:{event['source_lines']}: incomplete/failed RUSAGE_SELF marker")
    text = event["arguments"]
    values = []
    for name in ("ru_utime", "ru_stime"):
        matches = re.findall(r"\b" + name + r"=\{tv_sec=(\d+),\s*tv_usec=(\d+)\}", text)
        require(len(matches) == 1, f"{event['source_file']}: malformed {name}")
        sec, usec = map(int, matches[0])
        require(usec < 1_000_000, f"{event['source_file']}: invalid tv_usec")
        values.append(sec * 1_000_000 + usec)
    rss = re.findall(r"\bru_maxrss=(\d+)(?=[,}])", text)
    require(len(rss) == 1, f"{event['source_file']}: malformed ru_maxrss")
    values.append(int(rss[0]))
    return tuple(values)


def identify_markers(events, summary):
    signatures = {}
    for label, keys in MARKERS.items():
        obj = summary
        for key in keys:
            require(isinstance(obj, dict) and key in obj, f"missing marker summary field {'.'.join(keys)}")
            obj = obj[key]
        signatures[label] = tuple(integer(obj.get(key), f"marker {label}.{key}") for key in STAT_KEYS)
    candidates = {label: [] for label in MARKERS}
    for index, event in enumerate(events):
        signature = rusage_signature(event)
        if signature is not None:
            event["rusage_signature"] = dict(zip(STAT_KEYS, signature))
            for label, expected in signatures.items():
                if signature == expected:
                    candidates[label].append(index)
    solutions = []
    labels = list(MARKERS)
    def search(chosen):
        if len(solutions) > 1:
            return
        if len(chosen) == len(labels):
            solutions.append(chosen[:])
            return
        for index in candidates[labels[len(chosen)]]:
            if chosen and (index <= chosen[-1] or events[index]["entry_ns"] < events[chosen[-1]]["end_ns"]):
                continue
            search(chosen + [index])
    search([])
    require(len(solutions) == 1,
            f"A..E markers lack a unique chronological exact match; candidate indices={candidates}, solutions={len(solutions)} (capped at 2)")
    markers = {}
    for label, index in zip(labels, solutions[0]):
        event = events[index]
        event["marker"] = label
        markers[label] = {
            "summary_field": ".".join(MARKERS[label]), "event_index": index,
            "tid": event["tid"], "source_file": event["source_file"], "source_lines": event["source_lines"],
            "entry_epoch": epoch(event["entry_ns"]), "end_epoch": epoch(event["end_ns"]),
            "rusage_signature": event["rusage_signature"],
            "exact_signature_candidate_count": len(candidates[label]),
        }
    return markers, events[solutions[0][1]]["end_ns"], events[solutions[0][2]]["entry_ns"]


def serial_event(event):
    row = dict(event)
    for key in ("entry_ns", "end_ns", "resume_ns", "termination_bound_ns"):
        row[key.replace("_ns", "_epoch")] = epoch(row.pop(key))
    return row


def count_population(events, tids, normalized_key, denominator):
    aggregate, per_tid = {}, {}
    def count(subset):
        data = {name: {"calls": 0, "errors": 0, "unknown_completions": 0,
                       "restart_pseudo_results": 0} for name in FILTER}
        for event in subset:
            row = data[event["syscall"]]
            row["calls"] += 1
            row["errors"] += int(event["error"])
            row["unknown_completions"] += int(not event["completion_known"])
            row["restart_pseudo_results"] += int(event["restart_pseudo_result"])
        for row in data.values():
            row[normalized_key] = ratio(row["calls"], denominator)
        return data
    aggregate = count(events)
    grouped = defaultdict(list)
    for event in events:
        grouped[event["tid"]].append(event)
    for tid in tids:
        per_tid[str(tid)] = count(grouped[tid])
    return {"aggregate": aggregate, "per_tid": per_tid,
            "total_entries": len(events), normalized_key: ratio(len(events), denominator)}


def parse_samples(path):
    with path.open(newline="", encoding="utf-8") as stream:
        reader = csv.reader(stream)
        header = next(reader, None)
        require(header == ["phase", "index", "worker", "start_offset_ns", "latency_ns", "attempts", "error", "check_status"],
                f"{path}: unexpected V7 sample header")
        rows = list(reader)
        require(len(rows) == 200 and all(len(row) == len(header) for row in rows),
                f"{path}: expected exactly 200 rectangular measured sample rows")
        previous_start = -1
        for index, row in enumerate(rows):
            require(row[0] == "measure" and row[1] == str(index) and row[2] == "0"
                    and row[6] == "" and row[7] == "ok", f"{path}: invalid sample identity/result at row {index + 2}")
            require(all(cell.isdigit() for cell in row[3:6]) and int(row[4]) > 0 and int(row[5]) >= 1,
                    f"{path}: invalid sample numeric fields at row {index + 2}")
            require(int(row[3]) >= previous_start, f"{path}: nonmonotonic sample start offsets")
            previous_start = int(row[3])
    return {"raw_path": str(path), "header": header, "rows": len(rows), "alignment": ALIGNMENT}


def parse_sequence(folder):
    summary, manifest, _ = validate_run(folder, 200)
    paths = sorted(folder.glob("trace.*"))
    require(paths and all(re.fullmatch(r"trace\.[1-9]\d*", path.name) and path.is_file() for path in paths),
            f"{folder}: missing trace.<tid> files or unexpected trace.* path")
    events, threads = [], []
    for path in paths:
        parsed, thread = parse_trace_file(path)
        events.extend(parsed)
        threads.append(thread)
    events.sort(key=lambda event: (event["entry_ns"], event["tid"], event["source_lines"][0]))
    require(events, f"{folder}: no traced syscall entries")
    require(manifest["pid"] in {thread["tid"] for thread in threads}, f"{folder}: main PID trace missing")
    markers, lower, upper = identify_markers(events, summary)
    require(lower < upper, f"{folder}: empty/reversed measured window")
    selected, timeline, boundaries = [], [], []
    for event in events:
        start, end = event["entry_ns"], event["end_ns"]
        entered = lower <= start < upper and "marker" not in event
        event["entry_in_measured_window"] = entered
        event["boundary_overlap"] = []
        for label, boundary in (("B_end", lower), ("C_start", upper)):
            if start < boundary and (end is None or end > boundary):
                if end is None and event["termination_bound_ns"] <= boundary:
                    continue
                event["boundary_overlap"].append(label + (":possible_unknown_end" if end is None else ":observed"))
        if entered:
            selected.append(event)
        if event["boundary_overlap"]:
            boundaries.append(serial_event(event))
        if entered or event["boundary_overlap"] or event.get("marker") in ("B", "C"):
            timeline.append(serial_event(event))
    tids = sorted(thread["tid"] for thread in threads)
    samples = parse_samples(folder / "samples.csv")
    samples["raw_path"] = f"raw/strace-sequence/{folder.name}/samples.csv"
    return {
        **run_info(summary, manifest), "trace_filter": list(FILTER),
        "whole_short_process_scope": "selected traced syscalls only; " + WHOLE_SCOPE.replace("/N", "/200"),
        "measured_window_scope": PHASE_SCOPE, "markers": markers,
        "window": {"lower_inclusive_epoch": epoch(lower), "upper_exclusive_epoch": epoch(upper),
                   "rule": "end(B getrusage) <= syscall entry < start(C getrusage)",
                   "end_timestamp_rule": "entry -ttt timestamp plus -T elapsed; no float timestamp arithmetic",
                   "timestamp_precision": "nine-place serialization does not add precision to strace's printed timestamps or elapsed values",
                   "tie_order": "entry timestamp, numeric TID, original line; equal timestamps do not establish inter-thread order",
                   "marker_assignment": "unique chronological A..E path with exact CPU-us/RSS signatures"},
        "whole_short_process": count_population(events, tids, "wholeprocess_calls_divided_by_200", 200),
        "measured_window": count_population(selected, tids, "phase_window_entry_calls_divided_by_200", 200),
        "boundary_overlap_events": boundaries,
        "boundary_overlap_counts": dict(Counter(label for event in events for label in event["boundary_overlap"])),
        "unknown_completion_count": sum(not event["completion_known"] for event in events),
        "unfinished_resumed_count": sum(event["unfinished_resumed"] for event in events),
        "threads": threads, "samples": samples,
    }, timeline


def side_by_side(new, cgo, key, normalization, scope):
    rows = []
    for name in sorted(set(new) | set(cgo)):
        n, c = new.get(name, {}).get(key, 0), cgo.get(name, {}).get(key, 0)
        rows.append({"syscall": name, "new_calls": n, "cgo_calls": c,
                     "new_" + normalization[0]: ratio(n, normalization[1]),
                     "cgo_" + normalization[0]: ratio(c, normalization[1]), "scope": scope})
    return rows


def hash_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def render_tables(report, comparisons):
    text = ["# R16 offline instrument diagnostics\n", WHOLE_SCOPE + ".\n", PHASE_SCOPE + ".\n",
            ALIGNMENT + "\n", NO_TIMING + "\n",
            "The short trace covers only the explicit trace filter; strace-c covers all traced syscalls. "
            "Whole-process and measured-window populations are separate. Boundary-crossing calls are "
            "counted by ENTRY, not completion or elapsed-time apportionment. Unknown ends are marked possible overlaps.\n"]
    for name, rows in comparisons.items():
        text += [f"\n## {name}\n", "|Syscall|NEW calls|CGo calls|NEW normalized|CGo normalized|\n",
                 "|---|---:|---:|---:|---:|\n"]
        for row in rows:
            norm = [key for key in row if key.startswith("new_") and key != "new_calls"][0]
            text.append(f"|{row['syscall']}|{row['new_calls']}|{row['cgo_calls']}|{row[norm]:.6f}|{row[norm.replace('new_', 'cgo_', 1)]:.6f}|\n")
    text.append("\n## Perf\n\nWhole-process counters / 5000, never phase counts. "
                "Unsupported/not-counted events remain null, not zero; task-clock is not phase getrusage CPU.\n")
    for arm in ARMS:
        perf = report["perf_stat"][arm]
        if perf["status"] == "skipped":
            text.append(f"\n{arm.upper()}: skipped; see raw/perf-stat/{arm}/SKIPPED.json and raw/perf-capability.json.\n")
    if report["perf_side_by_side"]:
        text += ["\n|Event|NEW value|CGo value|NEW / 5000|CGo / 5000|Unit|\n",
                 "|---|---:|---:|---:|---:|---|\n"]
        for row in report["perf_side_by_side"]:
            text.append(f"|{row['event']}|{row['new_value']}|{row['cgo_value']}|{row['new_wholeprocess_value_divided_by_5000']}|{row['cgo_wholeprocess_value_divided_by_5000']}|{row['unit']}|\n")
    text.append("\n## Trace evidence\n\nEach sequence arm has markers.json, whole-process-counts.csv, "
                "measured-window-counts.csv, measured-window-timeline.json/csv/txt, and boundary-overlaps.json. "
                "Timeline includes carry-in/carry-out overlaps and B/C reference markers; only rows with "
                "entry_in_measured_window=true contribute to measured counts. Original trace.<tid> and samples.csv "
                "are preserved under raw/strace-sequence/{new,cgo}/. No absolute timestamp is synthesized for samples.\n")
    return "\n".join(text)


def analyze(root, out):
    summary_data = {arm: parse_summary(root / "strace-summary" / arm) for arm in ARMS}
    capability = json_read(root / "perf-capability.json")
    selected = capability.get("selected_events")
    require(isinstance(selected, list) and all(isinstance(event, str) and event for event in selected)
            and len(selected) == len(set(selected)), "perf-capability: invalid selected_events")
    require(not selected or BASE_EVENTS <= set(selected), "perf-capability: missing required base events")
    require(isinstance(capability.get("probes"), list) and capability["probes"], "perf-capability: missing probes")
    for probe in capability["probes"]:
        require(isinstance(probe, dict) and all(isinstance(probe.get(key), str) for key in ("name", "stdout", "stderr"))
                and type(probe.get("returncode")) is int and isinstance(probe.get("events"), list)
                and all(isinstance(event, str) and event for event in probe["events"]),
                "perf-capability: unexpected probe schema")
    require(not selected or any(probe["events"] == selected and probe["returncode"] == 0 for probe in capability["probes"]),
            "perf-capability: selected events do not match any successful probe")
    perf_data = {}
    for arm in ARMS:
        folder = root / "perf-stat" / arm
        if not selected:
            skipped = json_read(folder / "SKIPPED.json")
            require(isinstance(skipped.get("reason"), str) and skipped["reason"], f"{folder}: missing skip reason")
            require(not (folder / "perf-stat.csv").exists(), f"{folder}: selected_events empty but perf-stat.csv exists")
            perf_data[arm] = {"status": "skipped", "reason": skipped["reason"], "counters": {}}
        else:
            require(not (folder / "SKIPPED.json").exists(), f"{folder}: skip contradicts selected events")
            perf_data[arm] = parse_perf(folder, selected)
    sequences, timelines = {}, {}
    for arm in ARMS:
        sequences[arm], timelines[arm] = parse_sequence(root / "strace-sequence" / arm)
    comparisons = {
        "strace-summary-wholeprocess-over-5000": side_by_side(
            summary_data["new"]["syscalls"], summary_data["cgo"]["syscalls"], "calls",
            ("wholeprocess_calls_divided_by_5000", 5000), WHOLE_SCOPE),
        "sequence-wholeprocess-over-200": side_by_side(
            sequences["new"]["whole_short_process"]["aggregate"], sequences["cgo"]["whole_short_process"]["aggregate"], "calls",
            ("wholeprocess_calls_divided_by_200", 200), "selected traced syscalls only; " + WHOLE_SCOPE),
        "sequence-measured-window-over-200": side_by_side(
            sequences["new"]["measured_window"]["aggregate"], sequences["cgo"]["measured_window"]["aggregate"], "calls",
            ("phase_window_entry_calls_divided_by_200", 200), PHASE_SCOPE),
    }
    perf_comparison = []
    for event in selected:
        n, c = (perf_data[arm]["counters"][event] for arm in ARMS)
        require(n["unit"] == c["unit"], f"perf {event}: NEW/CGo unit mismatch")
        perf_comparison.append({"event": event, "new_value": n["value"], "cgo_value": c["value"],
                                "new_status": n["status"], "cgo_status": c["status"], "unit": n["unit"],
                                "new_wholeprocess_value_divided_by_5000": n["wholeprocess_value_divided_by_5000"],
                                "cgo_wholeprocess_value_divided_by_5000": c["wholeprocess_value_divided_by_5000"],
                                "scope": WHOLE_SCOPE})
    report = {"schema": "r16-offline-instruments-v1", "strace_summary": summary_data,
              "perf_capability": capability, "perf_stat": perf_data, "strace_sequence": sequences,
              "syscall_side_by_side": comparisons, "perf_side_by_side": perf_comparison,
              "sample_alignment": ALIGNMENT, "instrumentation_caveat": NO_TIMING}
    json_write(out / "analysis.json", report)
    for name, rows in comparisons.items():
        csv_write(out / (name + ".csv"), rows, list(rows[0]))
    perf_fields = ["event", "new_value", "cgo_value", "new_status", "cgo_status", "unit",
                   "new_wholeprocess_value_divided_by_5000", "cgo_wholeprocess_value_divided_by_5000", "scope"]
    csv_write(out / "perf-wholeprocess-over-5000.csv", perf_comparison, perf_fields)
    for arm in ARMS:
        dest = out / "strace-sequence" / arm
        data, timeline = sequences[arm], timelines[arm]
        json_write(dest / "markers.json", {"markers": data["markers"], "window": data["window"], "samples": data["samples"]})
        json_write(dest / "measured-window-timeline.json", {"scope": PHASE_SCOPE, "alignment": ALIGNMENT, "events": timeline})
        json_write(dest / "boundary-overlaps.json", {"events": data["boundary_overlap_events"]})
        for population, filename in (("whole_short_process", "whole-process-counts.csv"), ("measured_window", "measured-window-counts.csv")):
            rows = []
            norm = "phase_window_entry_calls_divided_by_200" if population == "measured_window" else "wholeprocess_calls_divided_by_200"
            scope = data["measured_window_scope"] if population == "measured_window" else data["whole_short_process_scope"]
            for tid, counts in [("ALL", data[population]["aggregate"]), *data[population]["per_tid"].items()]:
                for name, counts_row in counts.items():
                    rows.append({"tid": tid, "syscall": name, **counts_row, "scope": scope})
            csv_write(dest / filename, rows, ["tid", "syscall", "calls", "errors", "unknown_completions", "restart_pseudo_results", norm, "scope"])
        fields = ["tid", "entry_epoch", "end_epoch", "duration_ns", "syscall", "arguments", "result",
                  "entry_in_measured_window", "boundary_overlap", "marker", "completion_known", "elapsed_available",
                  "unfinished_resumed", "resume_epoch", "termination_bound_epoch", "source_file", "source_lines"]
        rows = [{key: json.dumps(event.get(key)) if key in ("source_lines", "boundary_overlap") else event.get(key, "")
                 for key in fields} for event in timeline]
        csv_write(dest / "measured-window-timeline.csv", rows, fields)
        text = ["# " + PHASE_SCOPE, "# " + ALIGNMENT,
                "# Sorted by syscall entry; overlap/reference rows are not measured entry counts unless flagged."]
        for event in timeline:
            flags = f"entry_count={event['entry_in_measured_window']} marker={event.get('marker', '-')} overlaps={','.join(event['boundary_overlap']) or '-'}"
            text.append(f"{event['entry_epoch']} [tid {event['tid']}] {event['syscall']}({event['arguments']}) = {event['result']} "
                        f"[end={event['end_epoch']} elapsed_ns={event['duration_ns']} {flags} "
                        f"raw={event['source_file']}:{','.join(map(str, event['source_lines']))}]")
        (dest / "measured-window-timeline.txt").write_text("\n".join(text) + "\n", encoding="utf-8")
    (out / "TABLES.md").write_text(render_tables(report, comparisons), encoding="utf-8")


def main():
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    out = None
    try:
        root = Path(sys.argv[1]).resolve(strict=True)
        target = Path(sys.argv[2]).absolute()
        require(root.is_dir(), "diagnostics-root is not a directory")
        require(not target.exists() and not target.is_symlink(), "new-output-dir must not already exist")
        require(not target.resolve().is_relative_to(root), "output must not be inside diagnostics-root")
        for path in root.rglob("*"):
            require(not path.is_symlink(), f"raw input contains a symlink: {path}; supply regular files")
            require(path.is_dir() or path.is_file(), f"raw input contains a special file: {path}")
        target.mkdir(parents=True, exist_ok=False)
        out = target
        json_write(out / "status.json", {"status": "incomplete", "input_root": str(root)})
        shutil.copytree(root, out / "raw")
        inventory = []
        for path in sorted((out / "raw").rglob("*")):
            if path.is_file():
                relative = path.relative_to(out / "raw")
                copied_hash = hash_file(path)
                require(copied_hash == hash_file(root / relative), f"input changed during raw copy: {relative}")
                inventory.append({"path": str(relative), "size_bytes": path.stat().st_size, "sha256": copied_hash})
        json_write(out / "inputs.json", {"source_root": str(root), "files": inventory,
                   "analyzer_path": str(Path(__file__).resolve()), "analyzer_sha256": hash_file(Path(__file__))})
        analyze(out / "raw", out)
        json_write(out / "status.json", {"status": "complete", "input_root": str(root), "raw_files": len(inventory),
                   "no_timing_claims": True, "sample_alignment": ALIGNMENT})
        print(f"R16 diagnostics parsed: {out / 'analysis.json'}; tables: {out / 'TABLES.md'}")
        return 0
    except (FormatError, OSError, ValueError, KeyError, TypeError, csv.Error) as exc:
        if out is not None:
            json_write(out / "status.json", {"status": "failed", "error": str(exc), "error_type": type(exc).__name__,
                       "note": "No valid analysis may be claimed; copied raw inputs are retained."})
        print(f"R16 analysis FAILED: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
