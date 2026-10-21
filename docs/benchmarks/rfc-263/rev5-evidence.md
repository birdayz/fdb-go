# RFC-263 revision 5 disposable renewal evidence

**Historical gate at this Rev5 freeze: range tail latency was a merge blocker.** CPU improvements did not offset it. The later [fixed confirmation](rev6-confirm-results.md) cleared the 8w p999 blocker by the owner's preregistered rule, with power/epoch/order caveats; this older population is retained unchanged as evidence.

This directory is local retained evidence, not a main-branch implementation. Main remained at 6585b291905c92ae3fed693b3ac20a62ec131593 before the RFC-only Rev5 commit. The detached scratch starts at 0a57d49cc, applies the sealed Rev4 reclaim design, then adds immutable same-shard weak-association renewal and controlled probes.

## Frozen source / executable

- `rev5-tree.txt`: tree 3f800737de95d937cd1f12babc60b5588814d781.
- `rev5-source.tar.gz`: full Git source archive, SHA256 8e67955510f465f6e3e898afc8d2b975e62ad3ccf18e7e0f7dadfde7787599c4.
- Raw `rev5-prototype.patch`: complete patch including the untracked disposable probe source; SHA256 f19b33d49470f99e93f4cb7ecb46191605d55db6513b630ce183ae62c86a4844. The equivalent in-tree `tools/benchcampaign/campaigns/rfc-263/rev5-prototype.patch` is zero-context (`git apply --unidiff-zero`) to avoid context-prefix whitespace errors; SHA256 a9796ba9ee8af830e513b3022104d7755e9064121f0e3857bceb3e2099f30d3d. Reverse-apply checks against the frozen scratch pass. This is disposable proposal source, not authorization to apply production code.
- `rev5-inputs.sha256`: exact changed BUILD/Go input hashes; checked around tests, build and timing.
- `bin/rev5`: Bazel Go 1.26.9 opt binary SHA256 d2b9f82132acd6d4c10d3a694003c0c00094181b90f648d70f38f5b7f18d965e. `rev5-opt-build.log` is the final source's cached opt build; its executable was byte-compared with the initial executed build in `prior-freeze-before-two-extra-probes/rev5-opt-build.log`. Only probes changed between those freezes; production source and executable are identical.
- `bin/rev4`: exact sealed selected Rev4 executable 2f0993bd332a9d7085b1c1053659b68867286de95ad1cbe2bf91fe327f088e95, source tree 61d2597e0231dfbedcd898a27c078c3412efaf6a.
- `bin/baseline`: exact original fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc executable 2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e.
- `pinned-go-runtime-semantics.txt`: actual Bazel SDK 1.26.9 weak/finalizer/cleanup semantics and source hashes, not the host 1.27.1 toolchain's documentation.

## Correctness and compiled mutation

`renewal20-fixed-race.log`, `renewal20-restored-race.log` and post-timing `renewal20-opt.log`: twenty top-level probes plus two collected-node subtests; target actually executed green in each. Real controlled finalizer cases: weak-nil dispatch then SetTimeout, Reset with positive default, popped-before-weak-read, immutable-ready-record present before GC. Distinct fresh node/weak identity and shard, old nil identity and retirement, correct budget and fresh public 1031 delivery required. Eventual old cleanup body is invoked synthetically, not represented as a real cleanup firing while the receiver is retained.

`mutant-no-renewal-20.patch`, `mutant-no-renewal-20-inputs.sha256`, `.log`, `.status`: entire renewal branch removed. Compiles, then all four resurrection scenarios fail `RESURRECTION_NODE_NOT_RENEWED`; source restored exactly before the green rerun. Earlier 18-probe proof, mutation and stronger-identity passes remain preserved.

Creation 9; existing-live-node actual arm 0 allocations; fresh association helper 5 total minus 1 fresh Transaction carrier = 4 (node, fresh weak handle, cleanup 2). Node 64 / shard 112 / root 320 / ready 24; root allocation class still 320 and cold construction 49. Every real renewal can leave another old cleanup argument/node/weak handle retained until final owner death, cleanup and later GC; no fixed repeated-resurrection bound.

## Invalid allocation attempt (retained)

`invalid-repeated-owner-allocation-probe.go` + `renewal20-probes-race.log`: synthetic repetition restored the same cleared old identity onto one already-resurrected live owner. Expected 4, observed 3; invalid for true renewal cost because after warmup weak.Make reuses that owner's fresh current weak handle. Replaced with separately priced fresh Transaction carriers, not suppressed or used as resurrection cost evidence.

## Pricing

The exact executed runner is in-tree as `tools/benchcampaign/campaigns/rfc-263/rev5-price-executed.py`; its historical import path refers to the original sealed Phase 1 primary wrapper. The in-tree replay entrypoint `rev5-price.py` imports `rev5-primary.py` and accepts evidence-directory options; the primary metric arithmetic is unchanged, while portable path validation is explicit. Historical executed primary bytes are preserved separately from the portable adaptation; [reproduction.md](reproduction.md) distinguishes their SHA256 values. `rev5-analyze-price.py` computes independent phase summaries. Evidence metadata/raw archives may remain outside the tree; scripts and write-ups do not. Predeclared 42 runs: Rev4-vs-Rev5 three alternating 8-worker pairs of get/set/range (20k measured + 2k warmup), original-fa-vs-Rev5 the same 18 runs, then three alternating original-fa-vs-Rev5 unprofiled 32-worker range pairs (500k measured + 2k warmup). No pooling with the sealed Rev4 phases. Same original harness archive identity, CPU affinity 2,3,14,15; GOMAXPROCS 4, GOGC 100, GOMEMLIMIT off, empty GODEBUG; real existing tmpfs FDB cluster; timeout 30000 ms/retry 10.

Each run retains exact argv/environment, SHA, source identity, UTC start/end, load/idle, uptime before/after, stdout, samples and summary. Measured-window client process user+system CPU is the primary metric; whole-process CPU is separately labelled. Bazel shut down before timing. No builds/tests/profiles overlapped timing; quiet window hard-stops new runs at 16:55Z, safely before owner's 17:00Z limit. Host background service load is retained, not claimed to be zero.

Timing completed 2026-10-20 12:19:32–12:24:11 UTC, 42/42 valid runs. Original baseline→Rev5 median CPU: 8w get 50.31775→49.65190 µs (−1.32%), set 57.15865→56.78540 (−0.65%), range 95.26725→93.83655 (−1.50%); 32w range 61.312968→59.894066 (−2.31%). All 12 paired CPU comparisons favorable. Rev4→Rev5 8w CPU median get −1.62%, set +0.74%, range −1.02%; set/range mixed and all arm ranges overlap. Load 5.44–7.97; sampled idle 0.9727–0.9909.

**Adverse latency retained:** 32w baseline→Rev5 median mean 782.144→786.766 µs (+0.59%), p99 2311.419→2760.679 µs (+19.44%), p99 adverse all 3 pairs. Incremental Rev4→Rev5 8w range mean 475.573→482.178 µs and p99 950.351→1050.510 µs, adverse all 3 pairs. This blocks merge pending explanation/resolution, not an accepted latency trade-off. The next step is retained-data comparison, then at least 6 alternating baseline/R4/R5 comparisons with full histograms; profiling/trace runs stay separate from timing.

`pricing-raw.log`, `pricing-rows.json`, `run-metrics.jsonl`, `pricing-analysis.json`, `pricing-start-utc.txt` and `pricing-finish-utc.txt` index timing. Rev4 mutex/GC/retention profiles are historical, not fresh Rev5 validation. Standard/full/race implementation gates still belong to the eventual reviewed production change.

## Raw artifact SHA256 index

Raw paths below are relative to `/tmp/rfc263-rev5.QgGUbU`.

| Artifact | SHA256 |
|---|---|
| `pricing-rows.json` | `b36e9fe2e79f554cd214e718ca698ad7f62b567ee76ea65f99a6ea3e24167b1a` |
| `pricing-analysis.json` | `9065739a787af86109a9500b9edb3b59bc6c6a26675ddc9fcadfbce62bb7a6cf` |
| `renewal20-restored-race.log` | `a6939fe0dd937202a2a1ee075b999e68cb6428a39c7953fe32c79e27701d1b23` |
| `renewal20-opt.log` | `b0d2f42e6126f3210c4e1b0bb81960f6316daa97214c6a9b6c4c1701b2b5c155` |
| `mutant-no-renewal-20.log` | `29fb821f857025bbfa760f6746f467dd0eb1598640693ea7b6ffcfcf3e7d2b2c` |
| `finalizer-census-and-baseline.txt` | `c353c87e1db113ebc8b14ee978e54035a3c415942ff5ca335ed8b936cec917c2` |
| `worktree-preservation-verification.json` | `8c2d1af02ad6fbc58481da6cf7d30dc28ac26bb83053ab4452c674dc120ea321` |

## Census / preservation

`finalizer-census-and-baseline.txt`: own main 6585 census 169 production Go files, 26 facade production files; no SetFinalizer/go:linkname; sole native commit AddCleanup; full native weak-publication excerpt and old-fa timer reset/disarm excerpt. Baseline idle loss is inherited; after-fire Stop false permits conditional fresh-weak recovery, before-fire reuse may not.

The in-tree `rev5-verify-preservation.py` byte-compares every full source archive member against the scratch and records `worktree-preservation-verification.json` before removal. `base-source.tar` and historical initial freeze are retained. `evidence-files.sha256` is the final recursive manifest, excluding itself, built only after artifacts stop changing and verified before scratch removal. The RFC copy and fast-lane/hook outputs will be retained alongside it. No PR/push or production Go commit is authorized by this evidence.
