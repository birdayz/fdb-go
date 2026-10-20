# R4 — whole-process Go/native CPU stack attribution

Existing old Go (`candidate-hc03-base`, SHA256 `7976a1892c6de2d33d51c12ea6fd63430e0f8c78f9fb2fb82d9c24cff27f0419`) and CGo (`candidate-v6-cgo`, SHA256 `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`), no builds. Old source affef93729368a3fb183df90c5332b5bd2289b25, frozen V6 harness, Go1.26.9/FDB7.3.77/API730. Source/binary/runtime checks passed before and after.

One run per arm, batchset50 (50×100B Sets+Commit),3000 measured transactions+200 warmup,workers1,GOMAXPROCS8,harness profile=none. Wrapper: `perf record -F 1999 -g --call-graph fp`. Both runs passed raw/persistence/counter/quiescence checks; collection21:30:20–21:30:41 UTC (21 seconds), with180-second cap. No other builds/workloads/profilers. The scope is the whole child process, including setup/seed,warmup,measurement and validation; these are not unprofiled performance results.

## Deliverables

Use `old-batch-r4-analysis-fp-v3/`:
- `old-flat-top80.txt`, `cgo-flat-top80.txt`: first80 symbol rows (or all available if fewer) from `perf report --no-children --sort dso,sym --percent-limit 0.3`.
- `old-children-top60.txt`, `cgo-children-top60.txt`: first60 symbol rows from `perf report --children --sort sym`.
- Full reports retain callchain bodies. Reports/scripts use `--no-inline`; actual symbol and frame-pointer callchains remain, but inline-function expansion is disabled.
- `*-samples.folded`: root→leaf, one unit per sample. `*-period.folded`: the same stacks weighted by event sample period. Frames are DSO-qualified; prefix includes arm/leaf CPU mode/comm. Symbol semicolons are escaped to colons; unknown frames retain addresses. Exact original names/addresses remain in `*-stacks.perf-script`.
- `*-leaves.perf-script`, `*-raw-events.txt`, `sample-split-and-quality.json`, native-stack examples and build IDs allow independent bucketing/quality checks.
- Original `perf.data` remains in each run under `old-batch-r4-fp/` and is copied locally in this archive.

## Sample CPU-mode split

Classified from raw `PERF_RECORD_SAMPLE` CPU-mode bits, cross-checked against perf-script leaf misc fields. Raw records, record stderr totals, script records and folded sample totals all match; mode/period totals match independently.

|Arm|All samples|User samples|Kernel samples|User sample %|Kernel sample %|User period %|Kernel period %|
|---|---:|---:|---:|---:|---:|---:|---:|
|old Go|6840|3351|3489|48.9912|51.0088|54.3227|45.6773|
|CGo|8613|4277|4336|49.6575|50.3425|51.7603|48.2397|

Sample counts and period-weighted fractions are different estimands; neither is the measured-phase getrusage CPU split. Total periods are old4051005196/CGo4826812226. Both have zero LOST records, **but old has65 THROTTLE records and CGo34**, so the requested1999Hz stream contains disabled intervals. Do not describe it as constant-rate/lossless or an exact unbiased CPU fraction. Throttle durations/events are preserved in the raw event dumps. No lower-rate workload was rerun.

## Unwind decision

Frame pointers were used for both arms. No DWARF fallback was needed or executed. Among2873 CGo samples whose leaf is in libfdb_c, median native-frame depth is9 and median user-frame depth18; none has only one user frame,21 have just one native frame, and none of those native-leaf stacks contains an unknown frame. This is usable native callchain evidence, not proof every stack is perfect. Across all CGo samples,2724 have an unknown frame and354 an unknown leaf; old170/70. Full histograms/examples are supplied rather than hiding these limitations.

## Postprocessing failures, preserved

The first postprocessor asked perf script for a CPU-index field absent from this recording and stopped (exit255). CPU-mode misc is present and independent of CPU index. A second pass removed that field but native inline expansion repeatedly failed in addr2line (`could not read first record`) and became slow; its controller SSH timed out while the owned remote postprocessor continued. Only that postprocessor was terminated; no measurement was rerun. The final v3 pass disables inline expansion and succeeds, preserving normal symbols/callchains. Initial/v2 output, scripts and stop log remain archived.

`perf report --header-only` still warns `Invalid HEADER_EVENT_DESC: nre=1 sz=144 (min64)`. Sample decoding, event names, symbol reports and the independent counts above succeed; this metadata warning is retained, not suppressed. This is a profile-tool limitation, not main-module correctness validation. Native internal counters remain unobserved; no physical-host isolation claim is made.
