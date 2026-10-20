# fdb-perf-20261009 rounds

One row per round. **Base** is the Go tree the `old` arm (or the only arm) was built
from; `new` arms add the patch the question names (see [../patches](../patches/)).
**Archive** is the sealed bundle in [../RAW-ARCHIVES.sha256](../RAW-ARCHIVES.sha256),
named by the round's archive script (`bundle=`); the drivers are under
[`tools/benchcampaign/campaigns/fdb-perf-20261009/rounds/`](../../../../tools/benchcampaign/campaigns/fdb-perf-20261009/rounds/),
named after the controller's `work-<round>` directory. Results and their limits are
in each entry document; none is a claim about current master.

## Numbered rounds

| Round | Question | Base | Archive | Entry document |
|---|---|---|---|---|
| R1 | `read1000` A/A variability of the old binary | `affef9372` | `hillclimb-arena-003-read-aa-r1` (drivers: `arena-003-r1`) | [hillclimb-arena-003-read-aa-r1/R1-TABLES.md](hillclimb-arena-003-read-aa-r1/R1-TABLES.md) |
| R2 | Old Go write-path pprof attribution (single arm, no comparison) | `affef9372` | `old-batch-attribution-r2` (drivers: `arena-003-r2`) | [arena-003-r2/ATTRIBUTION-SCOPE.md](arena-003-r2/ATTRIBUTION-SCOPE.md), [old-batch-attribution-r2/](old-batch-attribution-r2/) |
| R3 | Longer `batchset50` old Go vs CGo, plus separate instrumentation | `affef9372` | `old-batch-r3` | [old-batch-r3/R3-SCOPE.md](old-batch-r3/R3-SCOPE.md) |
| R4 | Whole-process Go vs native CPU stack attribution (`perf record`) | `affef9372` | `old-batch-r4` | [old-batch-r4/R4-SCOPE.md](old-batch-r4/R4-SCOPE.md) |
| R5 | PR1: key-range `memcmp` | `9377c0863` | `pr1-r5` | [pr1-r5/R5-SCOPE.md](pr1-r5/R5-SCOPE.md) |
| R6 | PR2: commit-capture borrowing | `9377c0863` | `pr2-r6` | [pr2-r6/R6-SCOPE.md](pr2-r6/R6-SCOPE.md) |
| R7 | PR2 large-value p99 follow-up | `9377c0863` | `pr2-r7` | [pr2-r7/R7-SCOPE.md](pr2-r7/R7-SCOPE.md) |
| R8 | PR3: buffered `readLoop` | `9377c0863` (old binary), new on `590145e31` | `pr3-r8` | [pr3-r8/R8-SCOPE.md](pr3-r8/R8-SCOPE.md) |
| R9 | PR4: inline lone GRV | `9377c0863` (old binary), new on `590145e31` | `pr4-r9` | [pr4-r9/R9-SCOPE.md](pr4-r9/R9-SCOPE.md) |
| R10 | PR5: direct idle writer | `9377c0863` (old binary), new on `590145e31` | `pr5-r10` | [pr5-r10/R10-SCOPE.md](pr5-r10/R10-SCOPE.md) |
| R10b | PR5v2: non-blocking direct write | `8ab5c5a4d` | `pr5-r10b` | [pr5-r10b/pr5-r10b-read1000/TABLES.md](pr5-r10b/pr5-r10b-read1000/TABLES.md) |
| R11 | Stack PR3+PR4+PR5 | `9377c0863` | not retained (superseded by R11v2) | none retained; drivers `stack-r11` |
| R11v2 | Stack PR3+PR4+PR5v2 | `8ab5c5a4d` | `stack-r11v2` | [stack-r11v2/stack-r11v2-read1000/TABLES.md](stack-r11v2/stack-r11v2-read1000/TABLES.md) |
| R12, R12b | PR6: causal commit GRV (`set100`, `batchset50`; R12b repeat and pooled) | `8ab5c5a4d` | `pr6-r12`, `pr6-r12b` | [pr6-r12/pr6-r12-set100/TABLES.md](pr6-r12/pr6-r12-set100/TABLES.md), [pr6-r12b/pr6-r12b-pooled/R12B-POOLED-TABLES.md](pr6-r12b/pr6-r12b-pooled/R12B-POOLED-TABLES.md) |
| R13 | PR7: transaction lease on the stack | `8ab5c5a4d` | `pr7-r13` | [pr7-r13/pr7-r13-batchset50/TABLES.md](pr7-r13/pr7-r13-batchset50/TABLES.md) |
| R14 | CPU profiles of the R11v2 binaries | R11v2 binaries | `stack-r14` | no write-up retained; drivers `stack-r14` |
| R15 | PR9: eager lone Get | `8ab5c5a4d` | `pr9-r15` | [pr9-r15/pr9-r15-read1000/R15-TABLES.md](pr9-r15/pr9-r15-read1000/R15-TABLES.md) |
| R16 | Cumulative full stack | `7922a2e5a` | `fullstack-r16` | [fullstack-r16/fullstack-r16-read1000/R16-TABLES.md](fullstack-r16/fullstack-r16-read1000/R16-TABLES.md) |
| R17 | Confirmatory full stack vs CGo (preregistered) | `7922a2e5a` | `fullstack-r17` | [fullstack-r17/analysis/TABLES.md](fullstack-r17/analysis/TABLES.md), [audit](fullstack-r17/independent-audit/AUDIT.md), [../preregistrations/](../preregistrations/) |
| R18 | GOMAXPROCS diagnostic (descriptive) | `7922a2e5a` | `fullstack-r18` | [fullstack-r18/fullstack-r18-analysis/TABLES.md](fullstack-r18/fullstack-r18-analysis/TABLES.md), [audit](fullstack-r18/independent-audit/AUDIT.md) |
| V7 | Fresh lifecycle CPU accounting, baseline and R11v2 stack | `8ab5c5a4d` | `lifecycle-v7` | [lifecycle-v7/lifecycle-v7-summary/V7-TABLES.md](lifecycle-v7/lifecycle-v7-summary/V7-TABLES.md) |

## Earlier campaigns on the same VM

| Campaign | Question | Base | Archive | Entry document |
|---|---|---|---|---|
| baseline-complete-v3 | Unprofiled 24-block baseline and frozen protocol | `26abdbaac` | `baseline-complete-v3` | no write-up retained; its frozen runner is `tools/…/frozen-protocol/` |
| diagnostics-v3-control-v4 | Baseline diagnostics: local RYW issuing GRVs, CPU/wait attribution | `26abdbaac` | `diagnostics-v3-control-v4` | [diagnostics-v3-control-v4/astra-profile-findings.md](diagnostics-v3-control-v4/astra-profile-findings.md) |
| quick-ryw-fix-001 | RYW correctness fix, short measurement | `26abdbaac` | `quick-ryw-fix-001` | [quick-ryw-fix-001/RESULTS.md](quick-ryw-fix-001/RESULTS.md) |
| quick-grv-idle-001 | GRV idle candidate, short `get1000` | `26abdbaac` | `quick-grv-idle-001` | [quick-grv-idle-001/grv-idle-001-results.md](quick-grv-idle-001/grv-idle-001-results.md) |
| quick-grv-controls-001 | GRV candidate, `set100`/`range100` controls | `26abdbaac` | `quick-grv-controls-001` | [quick-grv-controls-001/grv-idle-set-range-001-results.md](quick-grv-controls-001/grv-idle-set-range-001-results.md) |
| quick-grv-read-controls-001 | GRV candidate, `read1000`/`get10000` controls | `26abdbaac` | `quick-grv-read-controls-001` | [quick-grv-read-controls-001/grv-idle-read-controls-001-results.md](quick-grv-read-controls-001/grv-idle-read-controls-001-results.md) |
| quick-grv-final-controls-001 | GRV candidate, final controls and `read1000` profiles | `26abdbaac` | `quick-grv-final-controls-001` | [quick-grv-final-controls-001/grv-idle-final-controls-001-results.md](quick-grv-final-controls-001/grv-idle-final-controls-001-results.md) |
| conflictbuf-512-001-build | 512-byte initial conflict buffer: build only, no measurement | `26abdbaac` | `conflictbuf-512-001-build` | [conflictbuf-512-001-build/BUILD-RESULTS.md](conflictbuf-512-001-build/BUILD-RESULTS.md) |
| quick-conflictbuf-read1000-001 | 512-byte conflict buffer, short `read1000` | `26abdbaac` | `quick-conflictbuf-read1000-001` | [quick-conflictbuf-read1000-001/RESULTS.md](quick-conflictbuf-read1000-001/RESULTS.md) |
| quick-conflictbuf-batchset50-001 | Small-buffer promotion control, short `batchset50` | `26abdbaac` | `quick-conflictbuf-batchset50-001` | [quick-conflictbuf-batchset50-001/RESULTS.md](quick-conflictbuf-batchset50-001/RESULTS.md) |
| conflictbuf-batchset50-profiles-001 | CPU and heap profile of that candidate | `26abdbaac` | `conflictbuf-batchset50-profiles-001` | [conflictbuf-batchset50-profiles-001/RESULTS.md](conflictbuf-batchset50-profiles-001/RESULTS.md) |
| ownership-batches-001 | Ownership correction, short two-batch comparison | `26abdbaac` | `ownership-batches-001` | [ownership-batches-001/ownership-001-build/REPORT.md](ownership-batches-001/ownership-001-build/REPORT.md) |
| hillclimb-replay-001-002 | Plain-Set replay optimization, two experimental candidates | read-order WIP export (no commit) | `hillclimb-replay-001-002` | [hillclimb-replay-001-002/hillclimb-replay-001-build/REPORT.md](hillclimb-replay-001-002/hillclimb-replay-001-build/REPORT.md) |
| hillclimb-arena-003-initial | Arena allocation proposal on the replay002 tree | replay002 manifest (no commit) | `hillclimb-arena-003-initial` | [hillclimb-arena-003-initial/STATUS.md](hillclimb-arena-003-initial/STATUS.md) |
| hillclimb-arena-003-api-compatible | Arena003 adapted to the PR789 API | replay002 manifest (no commit) | `hillclimb-arena-003-api-compatible` | [hillclimb-arena-003-api-compatible/STATUS.md](hillclimb-arena-003-api-compatible/STATUS.md) |
| hillclimb-arena-003-published-292 | Arena003 prepared on published `292b79226` (no campaign) | `292b79226` | `hillclimb-arena-003-published-292` | [hillclimb-arena-003-published-292/STATUS.md](hillclimb-arena-003-published-292/STATUS.md) |
| hillclimb-arena-003-recovery-001, -002 | Arena003 vs old and CGo, and the unresolved `read1000` regression | `affef9372` | `hillclimb-arena-003-recovery-001`, `-002` (drivers: `arena-003-recovery`) | [arena-003-recovery/READ-FINDINGS.md](arena-003-recovery/READ-FINDINGS.md), [hillclimb-arena-003-recovery-002/…/ALL-TABLES.md](hillclimb-arena-003-recovery-002/hillclimb-arena-003-diagnostic-report/ALL-TABLES.md) |
