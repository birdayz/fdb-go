"""Extract bounded RFC-263 trace evidence with the pinned SDK's own trace reader.

The overlay changes only cmd/trace's debug printer, never the SDK on disk or a
benchmark binary. Raw trace files and extracted NDJSON stay outside the tree.
Run with uv run --no-project python; --extract-only is safe to stop after decode.
"""
import argparse
import hashlib
import json
import pathlib
import subprocess

FUNCTION = r'''func debugProcessedEvents(trc io.Reader) error {
 tr, err := trace.NewReader(trc); if err != nil { return err }
 out := bufio.NewWriterSize(os.Stdout, 1024*1024); defer out.Flush()
 enc := json.NewEncoder(out)
 type worker struct { Index int `json:"worker"`; G int64 `json:"gid"`; Created int64 `json:"created"`; FirstRunning int64 `json:"first_running"`; StartWait int64 `json:"start_wait"`; Release int64 `json:"release"`; AfterReleaseRunning int64 `json:"after_release_running"` }
 type wait struct { Type string `json:"type"`; G int64 `json:"gid"`; Worker int `json:"worker"`; Begin int64 `json:"begin"`; Unpark int64 `json:"unpark"`; Running int64 `json:"running"`; Stack string `json:"stack"` }
 workers := map[trace.GoID]*worker{}; ordered := []*worker{}; pending := map[trace.GoID]*wait{}
 var count int64
 for {
  ev, err := tr.ReadEvent(); if err == io.EOF { break }; if err != nil { return err }; count++
  t := int64(ev.Time())
  switch ev.Kind() {
  case trace.EventSync:
   if s := ev.Sync().ClockSnapshot; s != nil { if err := enc.Encode(map[string]any{"type":"sync", "time":t, "trace":int64(s.Trace), "mono":s.Mono}); err != nil { return err } }
  case trace.EventRangeBegin, trace.EventRangeActive, trace.EventRangeEnd:
   r := ev.Range(); if r.Name == "GC concurrent mark phase" || strings.HasPrefix(r.Name, "stop-the-world") {
    if err := enc.Encode(map[string]any{"type":"range", "kind":ev.Kind().String(), "time":t, "name":r.Name, "scope":r.Scope.String()}); err != nil { return err }
   }
  case trace.EventStateTransition:
   st := ev.StateTransition(); if st.Resource.Kind != trace.ResourceGoroutine { continue }; gid := st.Resource.Goroutine(); from, to := st.Goroutine()
   if from == trace.GoNotExist && to == trace.GoRunnable && strings.Contains(st.Stack.String(), "bench.runPhase.func2") {
    w := &worker{Index:len(ordered), G:int64(gid), Created:t}; workers[gid]=w; ordered=append(ordered,w)
   }
   if w := workers[gid]; w != nil {
    if to == trace.GoRunning && w.FirstRunning == 0 { w.FirstRunning=t }
    if to == trace.GoWaiting && w.Release == 0 && strings.Contains(st.Stack.String(), "bench/run.go:167") { w.StartWait=t }
    if from == trace.GoWaiting && to == trace.GoRunnable && w.StartWait != 0 && w.Release == 0 { w.Release=t }
    if to == trace.GoRunning && w.Release != 0 && w.AfterReleaseRunning == 0 { w.AfterReleaseRunning=t }
   }
   if from == trace.GoRunning && to == trace.GoWaiting && st.Reason == "GC weak to strong wait" {
    wi := -1; if w := workers[gid]; w != nil { wi=w.Index }
    if _, exists := pending[gid]; exists { return fmt.Errorf("duplicate pending weak wait %d", gid) }
    pending[gid]=&wait{Type:"weak_wait", G:int64(gid), Worker:wi, Begin:t, Stack:st.Stack.String()}
   } else if p := pending[gid]; p != nil {
    if from == trace.GoWaiting && to == trace.GoRunnable { p.Unpark=t }
    if to == trace.GoRunning { p.Running=t; if err := enc.Encode(p); err != nil { return err }; delete(pending,gid) }
   }
  }
 }
 for _, w := range ordered { if err := enc.Encode(map[string]any{"type":"worker", "data":w}); err != nil { return err } }
 for _, w := range pending { if err := enc.Encode(w); err != nil { return err } }
 return enc.Encode(map[string]any{"type":"end", "events":count, "workers":len(ordered), "unfinished_weak_waits":len(pending)})
}'''


def sha(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for b in iter(lambda: f.read(1024 * 1024), b''):
            h.update(b)
    return h.hexdigest()


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('evidence', type=pathlib.Path)
    p.add_argument('--sdk', required=True, type=pathlib.Path)
    p.add_argument('--arms', nargs='+', default=['rev5', 'rev4', 'baseline'])
    p.add_argument('--extract-only', action='store_true')
    p.add_argument('--manifest-name', default='extraction-inputs.json')
    args = p.parse_args()
    assert (args.sdk / 'VERSION').read_text().splitlines()[0] == 'go1.26.9'
    output = args.evidence / 'trace-analysis'
    output.mkdir(exist_ok=True)
    original = args.sdk / 'src/cmd/trace/main.go'
    text = original.read_text()
    begin = text.index('func debugProcessedEvents(trc io.Reader) error {')
    end = text.index('\nfunc debugRawEvents(', begin)
    patched = text[:begin] + FUNCTION + '\n' + text[end:]
    patched = patched.replace('import (\n', 'import (\n\t"bufio"\n\t"encoding/json"\n\t"strings"\n', 1)
    overlay_source = output / 'trace-main-overlay.go'
    overlay_source.write_text(patched)
    overlay = output / 'overlay.json'
    overlay.write_text(json.dumps({'Replace': {str(original.resolve()): str(overlay_source.resolve())}}))
    binary = output / 'trace-extract'
    subprocess.run([str(args.sdk / 'bin/go'), 'build', '-overlay', str(overlay), '-o', str(binary), 'cmd/trace'], check=True)
    manifest = {'sdk_version': 'go1.26.9', 'sdk_trace_main_sha256': sha(original),
                'extractor_source_sha256': sha(overlay_source), 'script_sha256': sha(pathlib.Path(__file__)), 'runs': {}}
    for arm in args.arms:
        run = args.evidence / 'runs' / ('tail-diag-trace-range100-p0-' + arm)
        trace_path = run / 'trace.out'
        compact = output / (arm + '.events.jsonl')
        with compact.open('w') as f:
            subprocess.run([str(binary), '-d=parsed', str(trace_path)], stdout=f, check=True)
        manifest['runs'][arm] = {'run': str(run), 'trace_sha256': sha(trace_path),
                                'samples_sha256': sha(run / 'samples.csv'), 'events_sha256': sha(compact)}
        print(arm, compact, flush=True)
    (output / args.manifest_name).write_text(json.dumps(manifest, indent=2) + '\n')


if __name__ == '__main__':
    main()
