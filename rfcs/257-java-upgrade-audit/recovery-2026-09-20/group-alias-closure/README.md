### RFC-257 WS-A/E1: GROUP-alias ownership closure and latency attribution

The preceding candidate `664b241c57b71829cb40687a1e26cc3dc3e145e6` received actual
Torvalds/storage implementation ACKs and Graefe/independent NAKs. The latter
identified a real wrong-row path: an ephemeral GROUP alias could overwrite the
semantic owner of a bound star attribute. Those verdicts remain historical;
they do not approve the repair below.

The repair preserves bound star attributes and qualified source references.
GROUP aliases resolve only eligible unresolved bare references, including in
aggregate arguments and ORDER keys. The ORDER visitor retains whether a SELECT
alias already owned a rebasing, preventing a second GROUP-alias substitution.
Named ORDER duplication compares captured identifier segments as well as rendered
spelling: the quoted alias `"P.ID"` is not the qualified source column `P.ID`.
Genuine repeated quoted/qualified keys still reject with 42701.

Fourteen added real-FDB SQL cases retain complete rows/labels for the exact
reported positional-star reproducers, explicit source columns/aggregate arguments,
quoted alias reads/arguments, qualified ORDER, SELECT-alias priority, ungrouped
42803 errors and repeated ORDER 42701 errors. A typed synthetic star-expander
unit separately pins the bound-owner guard even when the expanded name is bare.
Six compiled mutations independently fail their intended controls. An earlier
qualified-output mutant survived the old fixture; the retained unqualified-alias
name collision case closes that missing dimension. Parser-only repair was
insufficient for ORDER; both red intermediate logs are retained.

The retained live-Java probe passes fourteen exact cases against Java 4.14.2.0
`fdacd162a9c8acfadc49082b89185c823ab8ae4a`. Java refuses both positional-star
positive reproducers with 42803; their Go positives are the approved positional
extension, not claimed shared Java behavior. Named GROUP star/source/aggregate
cases and quoted alias reads match exact Java rows/types. Ungrouped and repeated
ORDER negatives match exact SQLStates. The initially assumed Java 0AF00 was
refuted and corrected from the oracle, without weakening Go assertions.

On frozen production/regression tree `ffdcc1138421d53d3b22971a095f619173d87d49`
(6,305 files), **93/93 targets executed uncached and passed**: 40,665 Go RUN markers
= 40,660 PASS + five existing owner-restricted hunt skips. **14/14 affected race
targets executed uncached and passed**, 21,118 RUN = PASS, no Go skips. Per-target
name multisets reconcile, including indented subprocess output. Both Java suites
ran 1,358/1,477 specs, with 119 existing filtered cases. No source hashes changed.

Subsequent source changes are confined to the manual stress target: the optional
observer/cluster-file harness, its BUILD registration and the new diagnostic test.
The observer retains the original fixture/query order, uses a private client handle
and records connection acquisition, planning/execution callbacks, record-store
timer deltas and trace regions. Its spans overlap and must not be added as disjoint
phases. Twelve validation arms run under Bazel; a compiled validation bypass fails
all eleven negative arms plus their parent, and restoration passes all thirteen
RUN markers including the parent.

A real isolation defect in this new diagnostic harness was also fixed: TempDir's
last component is normally `001`, so naming the database from it collided across
independent tests sharing an FDB catalog. The retained parallel real-FDB regression
failed with 42F04 (`/stress_latency_001` already exists). Names now include the
randomized parent and per-test sequence. Isolation plus telemetry validation ran
three uncached repetitions: 48 RUN = PASS. The interrupted pre-fix race run is
explicitly cancelled evidence, not a passing suite. Final code tree is
`be28f57df14cf7edec617bf09216ab828698cd69` (6,306 files); production code is unchanged
from the fully verified tree above.

#### Early-read latency: measured wait attribution, not a parity assertion

Four instrumented observations ran sequentially in ABBA order, two per side:
baseline diagnostic tree `893e4eb4760c1989c195b7be19484dc5961b0dcf` over commit
`e48f5b4965543cd4d99b5578356059e12d969c7c`, versus current diagnostic tree
`97d67fd750c13b8399bdea82774aa26bc8c8f263`. Both shared byte-identical diagnostic
harnesses and Go 1.26.6. Each ran/passed 24 tests/subtests, emitted twenty query
samples and counted exactly one million orders. These are instrumented timings,
not substitutes for the nominal comparison below. Background load was recorded,
not held constant. The baseline overlay has been removed and its worktree is clean.

Region-filtered profiles cover four region types / five query instances from each
of baseline-1 and current-2. All nonoverlapping goroutine-time columns reconcile;
profile synchronization waits reconcile with the region tables. Three scoped examples:

| Trace / query | Query region | Blocked in select | Goroutine execution | Scheduler wait |
|---|---:|---:|---:|---:|
| baseline-1, last PK | 25.670ms | 23.346ms | 2.255ms | 0.069ms |
| baseline-1, index equality | 23.736ms | 19.883ms | 3.729ms | 0.124ms |
| current-2, index equality | 32.860ms | 29.120ms | 3.601ms | 0.139ms |

Critical-path traces identify read-version waits during record-store opening, not
an unexplained planner CPU charge. The baseline last-PK query spent 13.705ms in
one GRV wait: 2.033ms before its flusher ran, then 11.586ms blocked for the RPC
reply. Baseline index equality had a 9.582ms GRV wait with 7.397ms for the reply.
Current index equality had a 16.580ms GRV wait: 1.027ms before the flusher ran,
then 15.478ms for the reply. In each case the network reader resumed from
`internal/poll.(*FD).Read` and delivered the reply; flusher/caller resume delays
were microseconds. This is not evidence for blaming adaptive batching alone.
The client subtree is byte-identical on both measured sides (`448b10552b6b6259671c44cfbc29e3f0ac0c8cd6`).

Scope limits matter: these client traces cannot separate FDB-server execution from
network/kernel delay, do not retroactively assign causes to the earlier untraced
52ms/41ms outliers, and do not prove universal performance parity or a speedup.
No warm-up, retry relaxation, cache-option change or timing threshold adjustment
was introduced. No client/transport production change was made. Raw trace hashes,
region profiles, compressed related-goroutine event extracts and exact critical
flow events are retained with the executable diagnostic test.

Final manual stress race verification executed uncached on the final code tree:
**40 RUN = PASS**, comprising the 1M diagnostic's 24 tests/subtests, thirteen
validation markers and three isolation markers. All twenty telemetry samples
were present and the fixture count was exactly 1,000,000. Follow-up `just test`
passed all 93 targets (one executed, 92 cached); this is not substituted for the
preceding 93-target uncached production verification.

#### Stress test 1M baseline — final GROUP-alias code, nominal harness

Fresh baseline commit `e48f5b4965543cd4d99b5578356059e12d969c7c` (the recorded
merge-base on 2026-09-20) versus final current source tree
`be28f57df14cf7edec617bf09216ab828698cd69`. Four sequential observations ran
baseline1/current1/current2/baseline2, without overlapping verification, trace
servers or other benchmark runs. Both used Go 1.26.6 on the same filesystem
(`/dev/nvme0n1p2`, 8% used before the runs). Source hashes remained unchanged.
Each ran/passed 24 tests/subtests, counted exactly one million orders and agreed
on all twenty-two result-row counts and eleven EXPLAIN lines.

One-minute load start/end, in execution order: baseline-1 3.46/5.56; current-1 5.56/7.48; current-2 7.48/11.10; baseline-2 11.10/16.08. Background load was not constant.

| Query | Rows | Baseline ms [sample 1, sample 2] | Current ms [sample 1, sample 2] | Median ratio |
|---|---:|---:|---:|---:|
| PK lookup id=0 | 1 | 38.325, 11.306 | 9.880, 11.073 | 0.422x |
| PK lookup id=N/2 | 1 | 16.435, 9.878 | 8.940, 13.280 | 0.844x |
| PK lookup id=N-1 | 1 | 14.784, 9.489 | 7.643, 9.812 | 0.719x |
| idx_customer eq | 8 | 26.724, 7.135 | 8.103, 7.303 | 0.455x |
| idx_amount range >9000 | 100,017 | 292.008, 271.342 | 292.072, 236.199 | 0.938x |
| idx_status count pending | 1 | 357.264, 495.302 | 377.865, 539.568 | 1.076x |
| full scan filter amount>5000 | 1 | 607.938, 740.032 | 852.675, 726.728 | 1.172x |
| GROUP BY status | 4 | 7.494, 11.712 | 7.907, 6.367 | 0.743x |
| GROUP BY status COUNT only | 4 | 5.617, 11.875 | 6.117, 6.106 | 0.699x |
| SUM by status (aggregate index) | 4 | 5.893, 17.716 | 5.896, 7.125 | 0.552x |
| GROUP BY customer HAVING | 47,271 | 631.669, 918.197 | 674.662, 926.550 | 1.033x |
| JOIN 10 orders x customers | 10 | 21.577, 29.626 | 22.795, 29.632 | 1.024x |
| ORDER BY PK (full) | 1,000,000 | 7284.464, 4840.667 | 7451.633, 4590.010 | 0.993x |
| ORDER BY PK + index filter | 8 | 10.442, 9.242 | 10.993, 12.826 | 1.210x |
| scan all rows ordered | 1,000,000 | 7868.122, 4773.991 | 3920.500, 4469.153 | 0.664x |
| scan all rows wide | 1,000,000 | 4319.570, 5105.722 | 4227.646, 4757.774 | 0.953x |
| IN-list 5 values | 46 | 22.916, 24.128 | 21.278, 23.789 | 0.958x |
| PK needle id=999999 | 1 | 5.284, 7.386 | 6.246, 6.172 | 0.980x |
| PK+filter needle id=500000 | 1 | 9.052, 7.927 | 8.816, 11.173 | 1.177x |
| full scan sparse filter | 97 | 3719.738, 4405.585 | 3670.111, 4158.929 | 0.964x |
| UPDATE by index | 8 | 10.131, 12.900 | 10.165, 11.473 | 0.940x |
| DELETE single row | 1 | 8.552, 16.592 | 8.531, 10.812 | 0.769x |

These are two observations per side, with all samples retained. The previous
population's directional early-read deterioration did not recur in this ABBA
population: the baseline itself contains a 38.325ms first PK and 26.724ms index
lookup. Other current medians are worse, including the count filter and ordered
index-filter query. Neither these ratios nor the trace attribution establishes
universal parity or causal speedups/regressions. The <5ms point-read aspiration
remains unmet on both sides. No expectation, threshold or retry policy changed.

Final implementation delta reconfirmation is pending against the documented
candidate; earlier ACKs do not cover these code changes. The historical executor
startup-timeout cause remains unavailable from recovered evidence and is not
explained by the CLI networking fix or these greens. WS-B–K and whole-upgrade
completion remain open. GitHub authentication is restored, but the owner has not
authorized commit/push this session. PR #786 remains draft at `71ccd8cf8`; no
commit, push or merge was performed, and no automatic merge is authorized.


## Evidence and reproduction

`verification.json` records exact trees, commands, per-target results, all stress
samples and artifact hashes. `latency/region-analysis.json` contains the exact
nonoverlapping region budgets and admitted critical flow events; compressed JSON
extracts and raw pprof profiles preserve their supporting input. Full 264–275MB
Go traces stay under `/var/tmp/fdb-upgrade-recovery/runtime/latency-comparison/`,
with SHA-256 hashes in the manifest, rather than bloating the repository.

The executable regressions are `TestFDB_NoFromSelectProbe`,
`TestGroupAliasPreservesBareBoundStar`, `GroupAliasIdentityJavaProbe`,
`TestLatencySampleValidation`, `TestFDB_LatencyHarnessIsolation` and
`TestFDB_Stress_1M_LatencyAttribution`. The latter lives in the manual stress
Bazel target and must be explicitly selected; wildcard `just test` does not run it.
Capture with `--test_arg=-test.trace=/absolute/path/file.trace` and declare its
parent through `--sandbox_writable_path`. Serve one capture at a time with the
pinned Go 1.26.6 `go tool trace -http=127.0.0.1:6067 file.trace`; run
`latency/extract-regions.py baseline-1` or `current-2` to collect region profiles.
`latency/summarize-regions.py` validates the recorded tables/profiles/flow chain;
expand the adjacent `*-events.json.gz` first with `gzip -dk` when replaying it
from this directory. The scripts retain the original machine's SDK/runtime paths
and exact measured tree names; do not silently relabel new observations as these
old samples. Server-side versus network/kernel latency is outside this trace.
