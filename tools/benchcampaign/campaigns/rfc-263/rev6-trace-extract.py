#!/usr/bin/env python3
"""Full-state RFC263 trace extraction with the pinned SDK's internal reader.

Build BEFORE the quiet slot: --sdk SDK --evidence RAW --build-only [--output DIR].
Decode AFTER the slot: same arguments, without --build-only, plus repeated
--run RUN_DIRECTORY and optionally --expect-workers 8. Decode never builds.
The SDK, benchmark executables and traces are never modified.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

CPUS = {0, 1, 6, 7, 11, 12, 13, 18, 19, 23}
SCHEMA = 1
FUNCTION = r'''func debugProcessedEvents(trc io.Reader) error {
 tr, err := trace.NewReader(trc)
 if err != nil { return err }
 out := bufio.NewWriterSize(os.Stdout, 1024*1024)
 enc := json.NewEncoder(out)
 emit := func(value any) error { return enc.Encode(value) }
 type worker struct {
  Index int `json:"worker"`
  G int64 `json:"gid"`
  CreatorG int64 `json:"creator_gid"`
  Created int64 `json:"created"`
  FirstRunning int64 `json:"first_running"`
  StartWait int64 `json:"start_wait"`
  Release int64 `json:"release"`
  AfterReleaseRunning int64 `json:"after_release_running"`
  StartStack string `json:"start_stack"`
  CreatorStack string `json:"creator_stack"`
 }
 workerFrame := os.Getenv("RFC263_TRACE_WORKER_FRAME")
 barrierFrame := os.Getenv("RFC263_TRACE_BARRIER_FRAME")
 if workerFrame == "" { return fmt.Errorf("missing worker frame selector") }
 workers := map[trace.GoID]*worker{}
 ordered := []*worker{}
 counts := map[string]int64{}
 waitReasons := map[string]int64{}
 var count, transitions, syncs, ranges int64
 for {
  ev, err := tr.ReadEvent()
  if err == io.EOF { break }
  if err != nil { return err }
  count++
  kind, t := ev.Kind().String(), int64(ev.Time())
  counts[kind]++
  base := map[string]any{"type":"event", "kind":kind, "time":t,
   "ev_gid":int64(ev.Goroutine()), "proc":int64(ev.Proc()), "thread":int64(ev.Thread())}
  switch ev.Kind() {
  case trace.EventSync:
   syncs++
   base["type"] = "sync"
   base["text"] = ev.String()
   if s := ev.Sync().ClockSnapshot; s != nil {
    base["trace"] = int64(s.Trace)
    base["mono"] = s.Mono
    base["wall"] = s.Wall.UTC().Format(time.RFC3339Nano)
   }
  case trace.EventRangeBegin, trace.EventRangeActive, trace.EventRangeEnd:
   ranges++
   r := ev.Range()
   base["type"], base["name"], base["scope"] = "range", r.Name, r.Scope.String()
   base["stack"], base["text"] = ev.Stack().String(), ev.String()
  case trace.EventStateTransition:
   st := ev.StateTransition()
   base["resource"] = st.Resource.String()
   if st.Resource.Kind != trace.ResourceGoroutine {
    base["type"], base["text"] = "resource_state", ev.String()
    break
   }
   transitions++
   gid := st.Resource.Goroutine()
   from, to := st.Goroutine()
   stack, eventStack := st.Stack.String(), ev.Stack().String()
   if from == trace.GoNotExist && to == trace.GoRunnable && strings.Contains(stack, workerFrame) {
    if _, exists := workers[gid]; exists { return fmt.Errorf("duplicate worker creation %d", gid) }
    w := &worker{Index:len(ordered), G:int64(gid), CreatorG:int64(ev.Goroutine()), Created:t,
     StartStack:stack, CreatorStack:eventStack}
    workers[gid], ordered = w, append(ordered,w)
   }
   wi := -1
   if w := workers[gid]; w != nil {
    wi = w.Index
    if to == trace.GoRunning && w.FirstRunning == 0 { w.FirstRunning=t }
    if to == trace.GoWaiting && w.Release == 0 && barrierFrame != "" && strings.Contains(stack, barrierFrame) {
     if w.StartWait == 0 { w.StartWait=t }
    }
    if from == trace.GoWaiting && to == trace.GoRunnable && w.StartWait != 0 && w.Release == 0 { w.Release=t }
    if to == trace.GoRunning && w.Release != 0 && w.AfterReleaseRunning == 0 { w.AfterReleaseRunning=t }
   }
   if from == trace.GoRunning && to == trace.GoWaiting { waitReasons[st.Reason]++ }
   base["type"], base["gid"], base["worker"] = "go_state", int64(gid), wi
   base["from"], base["to"], base["reason"] = from.String(), to.String(), st.Reason
   base["stack"], base["ev_stack"] = stack, eventStack
  default:
   // Retain metrics, labels, samples, user annotations and future event kinds.
   base["text"] = ev.String()
  }
  if err := emit(base); err != nil { return err }
 }
 for _, w := range ordered {
  if err := emit(map[string]any{"type":"worker", "data":w}); err != nil { return err }
 }
 if err := emit(map[string]any{"type":"end", "schema":1, "events":count, "event_counts":counts,
  "goroutine_transitions":transitions, "sync_events":syncs, "range_events":ranges,
  "workers":len(ordered), "worker_frame":workerFrame, "barrier_frame":barrierFrame,
  "running_to_waiting_reasons":waitReasons}); err != nil { return err }
 return out.Flush()
}'''


def sha(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def save(path: Path, value: object) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def sdk_inputs(sdk: Path) -> dict[str, object]:
    version = (sdk / "VERSION").read_text()
    if version.splitlines()[0] != "go1.26.9":
        raise RuntimeError(f"expected pinned go1.26.9 SDK, found {version!r}")
    files = ("VERSION", "bin/go", "pkg/tool/linux_amd64/compile", "pkg/tool/linux_amd64/link",
             "src/cmd/trace/main.go", "src/internal/trace/event.go", "src/internal/trace/reader.go")
    return {"root": str(sdk), "version": version, "files_sha256": {rel: sha(sdk / rel) for rel in files}}


def validate_end(value: dict[str, object], expected_workers: int | None = None) -> dict[str, object]:
    if value.get("type") != "end" or value.get("schema") != SCHEMA or value.get("events", 0) <= 0:
        raise RuntimeError("missing nonempty complete end record")
    if value.get("goroutine_transitions", 0) <= 0 or value.get("sync_events", 0) <= 0:
        raise RuntimeError("missing scheduler/sync population")
    if expected_workers is not None and (type(value.get("workers")) is not int or value["workers"] != expected_workers):
        raise RuntimeError(f"worker creation mapping expected {expected_workers}, found {value.get('workers')}")
    return value


def validate_run_manifest(value: dict[str, object], expected_workers: int | None) -> int:
    config = value.get("config")
    if not isinstance(config, dict) or value.get("go_version") != "go1.26.9" or config.get("profile") != "trace" or config.get("rate", 0) != 0:
        raise RuntimeError("not a pinned unpaced trace run manifest")
    workers = config.get("workers")
    if type(workers) is not int or workers <= 0 or (expected_workers is not None and workers != expected_workers):
        raise RuntimeError(f"unexpected declared worker population {workers}")
    return workers


class GuardTests(unittest.TestCase):
    def test_positive_end(self):
        value = {"type": "end", "schema": 1, "events": 10, "goroutine_transitions": 8, "sync_events": 1}
        self.assertEqual(validate_end(value), value)

    def test_empty_partial_and_missing_populations(self):
        base = {"type": "end", "schema": 1, "events": 10, "goroutine_transitions": 8, "sync_events": 1}
        for value in ({}, dict(base, type="go_state"), dict(base, schema=0), dict(base, events=0),
                      dict(base, goroutine_transitions=0), dict(base, sync_events=0)):
            with self.assertRaises(RuntimeError):
                validate_end(value)

    def test_last_record_file_controls(self):
        end = {"type": "end", "schema": 1, "events": 10, "goroutine_transitions": 8, "sync_events": 1, "workers": 8}
        with tempfile.TemporaryDirectory(prefix="rfc263-trace-guards-") as directory:
            path = Path(directory) / "events.jsonl"
            path.write_text('{"type":"event"}\n' + json.dumps(end) + "\n")
            self.assertEqual(last_record(path, 8)["events"], 10)
            path.write_bytes(b"")
            with self.assertRaises(RuntimeError):
                last_record(path, 8)
            path.write_text('{"type":"end",')
            with self.assertRaises(json.JSONDecodeError):
                last_record(path, 8)
            for value in (dict(end, type="go_state"), dict(end, goroutine_transitions=0), dict(end, sync_events=0),
                          dict(end, workers=0), dict(end, workers=32), {k: v for k, v in end.items() if k != "workers"}):
                path.write_text(json.dumps(value) + "\n")
                with self.assertRaises(RuntimeError):
                    last_record(path, 8)

    def test_manifest_worker_and_profile_controls(self):
        value = {"go_version": "go1.26.9", "config": {"workers": 8, "profile": "trace", "rate": 0}}
        self.assertEqual(validate_run_manifest(value, 8), 8)
        self.assertEqual(validate_run_manifest(value, None), 8)
        with self.assertRaises(RuntimeError):
            validate_run_manifest({}, 8)
        with self.assertRaises(RuntimeError):
            validate_run_manifest(dict(value, go_version="go1.27.1"), 8)
        for field, bad in (("workers", 0), ("workers", True), ("workers", 32), ("profile", "none"), ("rate", 1)):
            with self.assertRaises(RuntimeError):
                validate_run_manifest(dict(value, config=dict(value["config"], **{field: bad})), 8)


def last_record(path: Path, expected_workers: int | None = None) -> dict[str, object]:
    with path.open("rb") as stream:
        stream.seek(0, os.SEEK_END)
        size = stream.tell()
        stream.seek(max(0, size - 1024 * 1024))
        lines = stream.read().splitlines()
    if not lines:
        raise RuntimeError(f"empty decoder output: {path}")
    return validate_end(json.loads(lines[-1]), expected_workers)


def build(sdk: Path, output: Path, inputs: dict[str, object]) -> None:
    binary = output / "trace-extract"
    if binary.exists() or (output / "decoder-manifest.json").exists():
        raise RuntimeError("refusing to replace an existing decoder; choose a new --output")
    original = sdk / "src/cmd/trace/main.go"
    text = original.read_text()
    begin = text.index("func debugProcessedEvents(trc io.Reader) error {")
    end = text.index("\nfunc debugRawEvents(", begin)
    patched = text[:begin] + FUNCTION + "\n" + text[end:]
    if patched.count("import (\n") != 1:
        raise RuntimeError("SDK import anchor is not unique")
    patched = patched.replace("import (\n", 'import (\n\t"bufio"\n\t"encoding/json"\n\t"strings"\n', 1)
    overlay_source, overlay = output / "trace-main-overlay.go", output / "overlay.json"
    overlay_source.write_text(patched)
    save(overlay, {"Replace": {str(original): str(overlay_source)}})
    argv = [str(sdk / "bin/go"), "build", "-overlay", str(overlay), "-o", str(binary), "cmd/trace"]
    env = dict(os.environ, GOTOOLCHAIN="local", GO111MODULE="off")
    save(output / "build-command.json", {"argv": argv, "cwd": str(sdk), "GOTOOLCHAIN": "local", "GO111MODULE": "off",
         "nice": os.getpriority(os.PRIO_PROCESS, 0), "cpus": sorted(os.sched_getaffinity(0))})
    with (output / "build.log").open("wb") as log:
        result = subprocess.run(argv, cwd=sdk, env=env, stdout=log, stderr=subprocess.STDOUT)
    save(output / "build-status.json", {"exit": result.returncode})
    if result.returncode:
        raise RuntimeError(f"decoder build failed; see {output / 'build.log'}")
    if sdk_inputs(sdk) != inputs:
        raise RuntimeError("SDK changed during decoder build")
    save(output / "decoder-manifest.json", {"schema": SCHEMA, "sdk": inputs, "binary": str(binary),
         "binary_sha256": sha(binary), "overlay_source_sha256": sha(overlay_source), "overlay_sha256": sha(overlay),
         "extractor_script_sha256": sha(Path(__file__)), "mode": "all-events-no-reason-filter"})


def decode(run: Path, output: Path, decoder: dict[str, object], args) -> dict[str, object]:
    manifest = json.loads((run / "manifest.json").read_text())
    workers = validate_run_manifest(manifest, args.expect_workers)
    trace, samples = run / "trace.out", run / "samples.csv"
    before = {"trace_sha256": sha(trace), "samples_sha256": sha(samples), "manifest_sha256": sha(run / "manifest.json")}
    destination = output / "runs" / run.name
    destination.mkdir(parents=True, exist_ok=False)
    partial, final = destination / "events.partial.jsonl", destination / "events.jsonl"
    argv = [decoder["binary"], "-d=parsed", str(trace)]
    env = dict(os.environ, RFC263_TRACE_WORKER_FRAME=args.worker_frame, RFC263_TRACE_BARRIER_FRAME=args.barrier_frame)
    save(destination / "decode-command.json", {"argv": argv, "worker_frame": args.worker_frame, "barrier_frame": args.barrier_frame,
         "nice": os.getpriority(os.PRIO_PROCESS, 0), "cpus": sorted(os.sched_getaffinity(0))})
    with partial.open("wb") as stream, (destination / "decode.stderr.log").open("wb") as error:
        result = subprocess.run(argv, env=env, stdout=stream, stderr=error)
    save(destination / "decode-status.json", {"exit": result.returncode})
    if result.returncode:
        raise RuntimeError(f"decoder failed; partial output retained: {destination}")
    end = last_record(partial, workers)
    after = {"trace_sha256": sha(trace), "samples_sha256": sha(samples), "manifest_sha256": sha(run / "manifest.json")}
    if after != before or sha(Path(decoder["binary"])) != decoder["binary_sha256"]:
        raise RuntimeError("trace/samples/manifest/decoder changed during extraction")
    partial.rename(final)
    record = {"run": str(run), "events": str(final), "events_sha256": sha(final), "expected_workers": workers,
              "decoder_binary_sha256": decoder["binary_sha256"], "extractor_script_sha256": sha(Path(__file__)),
              "end": end, **before}
    save(destination / "extraction-manifest.json", record)
    return record


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sdk", required=True, type=Path)
    parser.add_argument("--evidence", required=True, type=Path)
    parser.add_argument("--output", type=Path, help="decoder/decoded artifact directory outside the source tree")
    parser.add_argument("--build-only", action="store_true", help="build decoder only; never read traces")
    parser.add_argument("--self-check", action="store_true", help="run only embedded end-record/manifest fixture tests; no build/decode")
    parser.add_argument("--run", action="append", type=Path, default=[], help="explicit trace run directory; repeat per arm")
    parser.add_argument("--expect-workers", type=int, help="require this population in every run manifest and decoded worker map")
    parser.add_argument("--worker-frame", default="bench.runPhase.func2", help="creation-stack substring identifying benchmark workers")
    parser.add_argument("--barrier-frame", default="bench/run.go:167", help="frozen harness start-barrier frame; raw states remain available if unmatched")
    args = parser.parse_args()
    if not args.self_check and args.build_only == bool(args.run):
        parser.error("choose --build-only OR one or more explicit --run directories")
    if args.self_check and (args.build_only or args.run):
        parser.error("--self-check cannot build or decode")
    if not args.worker_frame or (args.expect_workers is not None and args.expect_workers <= 0):
        parser.error("worker selector must be nonempty and --expect-workers positive")
    repo = Path(__file__).resolve().parents[4]
    evidence = args.evidence.resolve()
    output = args.output.resolve() if args.output else evidence / "trace-analysis" / "rev6-full"
    if evidence.is_relative_to(repo) or output.is_relative_to(repo):
        parser.error("raw evidence and --output must be outside the source repository")
    os.sched_setaffinity(0, CPUS)
    os.setpriority(os.PRIO_PROCESS, 0, 19)
    output.mkdir(parents=True, exist_ok=True)
    if args.self_check:
        with (output / "guard-tests.log").open("w") as log:
            result = unittest.TextTestRunner(stream=log, verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(GuardTests))
        if not result.wasSuccessful() or result.testsRun != 4:
            raise RuntimeError("nonempty end-record guard tests did not pass")
        return 0
    sdk = args.sdk.resolve()
    inputs = sdk_inputs(sdk)
    if args.build_only:
        build(sdk, output, inputs)
        print(output / "decoder-manifest.json")
        return 0
    decoder_path = output / "decoder-manifest.json"
    if not decoder_path.is_file():
        parser.error("decoder is not built; use --build-only BEFORE the quiet slot")
    decoder = json.loads(decoder_path.read_text())
    if decoder["schema"] != SCHEMA or decoder["sdk"] != inputs or sha(Path(decoder["binary"])) != decoder["binary_sha256"]:
        raise RuntimeError("frozen decoder/SDK identity mismatch; no build attempted")
    records = []
    for run in args.run:
        records.append(decode(run.resolve(), output, decoder, args))
        save(output / "extraction-inputs.json", {"schema": SCHEMA, "decoder_manifest_sha256": sha(decoder_path), "runs": records})
    print(output / "extraction-inputs.json")
    return 0


if __name__ == "__main__":
    sys.exit(main())
