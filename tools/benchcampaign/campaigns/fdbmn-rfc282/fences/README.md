# Measured-phase fence overlay

Prepared source amendment, not a measurement or authorization to launch a campaign.
Apply identically to every A/B/C arm's frozen client harness in external source
exports, never to production `tools/clientbench` in a Git worktree. No operation,
pacing, quota, warmup, CPU-snapshot, sample or estimator changes are included.

## Frozen input and patch

Base harness: `d94b4c61377d173b7c6884fde426a227dab0f0bb`, with the historical
five-file cleanup compatibility amendment. Complete original hashes refer to
that amended archive, including its amended `test_campaign.py`, not unamended
Git blobs. The retained amended archive was read-only inspected:

```text
frozen/cleanup-compat/client-harness.tar.gz
archive sha256: 76990b867f337bdad0132cc69aa3f6f9a9bcf0cb0859cc8c7f8514def684f508
run.go sha256: b82341a5652afa1a94931ff687519c262065f592b85b16e5b832182efea00214
```

`source-context.json` pins all seven complete original and patched file hashes,
patch hash and Go-control hash. It retains original patch-context snippets (the
small BUILD file is complete), not the whole harness. `measurement-fences.patch`
changes `internal/bench/run.go`, appends real Go controls to the already-declared
`internal/bench/bench_test.go`, adds four existing-script exports in
`scripts/BUILD.bazel`, and updates only the expected compiler literal in
`matrix_plan.py`, `r3_plan.py` and their `test_campaign.py` fixture. Its seventh
file, `integration/smoke_test.go`, drops only the obsolete rejection of completed
cleanup-count growth, matching the already-declared #948 drain contract. No
current production checkout is patched.

The BUILD-only compatibility amendment exposes exactly `analyze.py`,
`matrix_plan.py`, `r3_plan.py` and `run.py` to
`//tools/benchcampaign:__subpackages__`. The current campaign tests legitimately
consume those frozen scripts; without the declaration, whole `just test` stops
during visibility analysis before the Go controls run. No script content, target
population, visibility check, numerical gate, quota or pacing is changed or
bypassed. The old/new complete BUILD hashes are pinned alongside both Go files.

The new production trees and all A/B/C builds select Go **1.27.3**, not the
historical Go 1.26.9 compiler. The two plan defaults and one corresponding unit
fixture change only `go1.26.9` to `go1.27.3`; the strict plan/runtime compiler
comparison is unchanged. The frozen-source literal census had exactly four
`1.26.9` references: `matrix_plan.py:100`, `r3_plan.py:59`,
`test_campaign.py:194` and `README.md:22`. Only the three executable/fixture
metadata literals change. The historical README stays byte-identical; its old
compiler statement describes the original harness and is superseded for this
new campaign by these explicit source/compiler receipts. Nothing asserts that
the October campaign used Go 1.27.3. Both external SDK receipts identify compiler
SHA256 `5f5eb6d0b2fb6aa0bb5d5f8c5f1c944e0de955c925f0111af92a9d8f79680b5b`.
The appended Go test's four field-alignment lines were formatted with gofumpt;
its statements, strings and existing test-file prefix are unchanged.

The smoke-test-only cleanup correction removes exactly
`drain.After.CleanupsQueued != drain.Before.CleanupsQueued`. It preserves
`Complete`, rounds >=2, positive wall time, finalizers executed == queued,
cleanups executed == queued, and every other lifecycle/CPU check. The historical
runtime implementation already accepts fully executed cleanup growth; the stale
smoke assertion did not. Runtime `lifecycle.go`, its pending-cleanup negative
controls and `run.py` validators are untouched. Prior failed whole lanes and
noncached Go fence-test passes remain retained evidence, not a whole-lane pass.

Before any build, verify the *complete* external source files against
`original_sha256`, apply the patch with exact context and zero fuzz, and verify
*complete* patched files against `patched_sha256`. Patch context matching alone
is insufficient. Hash and archive the resulting entire harness; compare the
regular-file member population and every member hash across all arm exports.
Freeze inputs before building and verify them afterward. Preserve the original
archive and both amendment receipts. Never apply only the runtime hunk or use
different overlay bytes for one arm.

## Fence semantics

Originally `runPhase` captured `epoch = time.Now()` immediately before
`close(start)`, waited for every worker via `done.Wait()`, and evaluated
`time.Since(epoch).Nanoseconds()` for `WallNS`. The overlay replaces the implicit
end clock read inside `time.Since` with one explicit `end := time.Now()`, and
computes `end.Sub(epoch).Nanoseconds()`. The exact start and explicit end
`time.Time` values are retained in `Phase.Started` / `Phase.Ended` **after** the
end clock read. This keeps one Go-level end-boundary clock call at the original
location and makes no per-operation change. It is not zero underlying clock
work: `time.Since` can use a monotonic-only fast path, whereas `time.Now` also
obtains realtime correlation. This small per-process instrumentation overhead is
applied and hash-pinned identically in A/B/C; no end UTC is extrapolated from the
start or sampled later.

The successful measured phase, not the warmup phase, adds these summary fields:

| Field | Meaning |
|---|---|
| `measured_fences_format` | `fdb-bench-measured-fences/v1` |
| `measured_started_unix_ns` | Original start fence's `UnixNano()`, JSON int64 integer |
| `measured_ended_unix_ns` | Original end fence's `UnixNano()`, JSON int64 integer |
| `measured_started_utc` | Same start value's UTC `RFC3339Nano` string |
| `measured_ended_utc` | Same end value's UTC `RFC3339Nano` string |

Existing `measured_wall_ns` remains the authoritative **monotonic** difference
of those exact values. Do not calculate throughput from Unix/UTC subtraction or
parse nanosecond integers through float64. Realtime and monotonic differences
need not be identical: clock steps/slews and clock correlation uncertainty must
be handled explicitly by the server-window estimator. Unix endpoints must be
strictly increasing; a clock inversion refuses fence publication and marks an
otherwise successful run failed. The serializer also refuses missing endpoints,
nonpositive wall/count, operation errors or a `Phase.WallNS` inconsistent with
the original endpoint subtraction. No guessed timestamps or manifest-start
fallback is emitted on failure.

Fences exclude fixture/setup, profiling setup, warmup, stats collection and
post-measurement validation, output and shutdown. They include the existing
worker-release-to-last-worker-finished window: scheduling, operation-loop
bookkeeping and the finishing tail remain included. They identify a clock window;
they do not measure server CPU themselves or establish that deferred server work
is contained in it.

## Preparation and frozen-export qualification

`prepare_harness.py` is an offline source preparer, not a build/test runner. Its
CLI fixes the original cleanup-compatible archive SHA above and requires exactly
28 regular files. It verifies the patch SHA and all seven complete original
source hashes before applying any hunk; it applies with exact old/new line
positions, no offsets/fuzz, then verifies all seven complete patched hashes. Tar members must
be canonical regular files/directories under `tools/clientbench`; traversal,
links, special files, duplicate members and file/directory collisions refuse.
No tar extraction API or archive-supplied owner metadata is used.

Preparation requires a fresh directory outside every Git worktree. Example
commands below are **recipes, not execution receipts**:

```sh
FENCES=/absolute/path/to/pinned/fdbmn-rfc282/fences
python3 -B "$FENCES/prepare_harness.py" \
  --archive /absolute/path/to/retained/cleanup-compat/client-harness.tar.gz \
  --output-dir /absolute/path/to/fresh/fence-harness
```

The fresh directory receives:

- `tree/`: the complete amended 28-file frozen client harness;
- `client-harness.tar.gz`: deterministic sorted USTAR+gzip with fixed timestamps,
  ownership and gzip filename; no controller paths or current timestamps;
- `source-receipt.json`: supplemental input/output archive, context, patch,
  complete original/patched member hashes, file modes and directory population;
- `qualify-harness.sh`: executable **unexecuted** qualification recipe.

The receipt explicitly says `prepared-not-runtime-qualified`. It is supplemental
and does not replace the campaign protocol/source/build/compiler receipts. The
preparer verifies no regular members were added/removed, exactly the seven
declared files changed, and every other file's bytes stayed unchanged. Existing output
directories refuse without overwriting; repeated verification is read-only.
Prepare **once**, preserve this archive and receipt, and reuse the exact same
archive bytes for every A/B/C external export. Re-preparing the same inputs at
different locations produces identical archive and receipt bytes.

Each production export must replace its entire `tools/clientbench` directory
with this archive, rather than layering onto a moving harness containing extra
files. Extract with preserved safe file modes (for example GNU tar
`--no-same-owner --same-permissions`) and never add symlinks. Check the final
export before and after its separately authorized `just test`:

```sh
python3 -B "$FENCES/prepare_harness.py" \
  --verify-tree /absolute/path/to/external/production-export \
  --receipt /absolute/path/to/fence-harness/source-receipt.json
```

`--verify-tree` checks the **entire** `tools/clientbench` subtree against the
receipt: filenames, bytes, modes, directory population and absence of links or
special files. Unrelated production files and build outputs outside that subtree
are intentionally out of its scope. It does not replace the controller's full
tracked/untracked production-input freeze. The generated recipe runs this
checker, `just test` in one explicitly selected external production export, then
the checker again; it requires explicit `FROZEN_EXPORT`, `HARNESS_RECEIPT` and
`FENCES_PREPARER` paths. No source export, extraction, compilation, tests or
qualification run is performed merely by creating the recipe. Successful real
Go controls on both production exports, pinned binary builds and untimed
three-arm qualification still need their own nonempty execution receipts.

## Controls and validation boundary

`apply_patch.py` supplies the exact same patch application to the preparer and
controls. `test_prepare_harness.py` uses tiny independently written synthetic
archives and trusted fixture pins only; it never reads the private historical
archive. It covers archive/source/patch/post-hash mismatches, traversal,
symlink/hardlink/special/duplicate members, extra files, unchanged binary bytes,
post-build source/mode drift, Git-worktree refusal, deterministic outputs and
idempotent verification. These are source-tool controls, not Go execution.

`test_fences.py` is an offline patch-integrity test. It reconstructs sparse
source-context fixtures, applies every hunk exactly, checks pins and population,
refuses stale/malformed context, and checks that only the one original end clock
is made explicit. It does **not** compile Go, run the frozen harness, prove full
source-file identity or qualify benchmark output.

`fence-tests.go.txt` is included verbatim by the patch in the frozen harness's
existing `bench_test.go`. Its real Go tests exercise `RunPhase`, sample/window
bounds, separate warmup and measured phases, fence inversion/missing/zero cases,
wall-duration mismatch, failed/empty populations, exact int64 JSON round-trip
above 2^53 and UTC/Unix nanosecond agreement. These behavioral tests require an
explicitly authorized later frozen-export Bazel validation; their presence in
this patch is **not** an execution receipt.

Repository validation remains `just test` (and the authorized full lane at the
workstream boundary), not direct Python tests or `go test`. On the shared
machine use `tools/lanes/run-verified-lane.sh` with the owned clean worktree.
External overlay application, frozen-export builds/controls and untimed
qualification must be separately recorded before any measured run. Preparing
this overlay does not execute tests, builds or benchmarks; neither Python-only
controls nor a failed/empty whole-lane analysis is a Go execution receipt.
