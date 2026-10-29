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

`source-context.json` pins all nine complete original and patched file hashes,
patch hash and Go-control hash. It retains original patch-context snippets (the
small BUILD file is complete), not the whole harness. `measurement-fences.patch`
changes `internal/bench/run.go`, appends real Go controls to the already-declared
`internal/bench/bench_test.go`, adds four existing-script exports in
`scripts/BUILD.bazel`, and updates only the expected compiler literal in
`matrix_plan.py`, `r3_plan.py` and their `test_campaign.py` fixture. Its seventh
file, `integration/smoke_test.go`, drops the obsolete rejection of completed
cleanup-count growth, matching the already-declared #948 drain contract, and adds
measured-fence publication assertions to every existing Go/CGo workload cell.
The eighth and ninth files, `cmd/clientbench/backend_selection_test.go` and its
`BUILD.bazel`, add a test-only real-CGo `Run` fence control using the existing
production opener and backend. The four original backend-selection controls and
both executable BUILD declarations remain unchanged. No current production
checkout is patched.

The scripts BUILD-only compatibility amendment exposes exactly `analyze.py`,
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
The appended bench and real-CGo command controls are formatted with the
repository-pinned gofumpt v0.12.0. The command test uses the Bazel-built formatter
SHA256 `52700e112445146466f3c40b1b5f3770866b5fab30d36d36c27af1b491285ec6`,
whose build info identifies go1.27.3 and mvdan.cc/gofumpt v0.12.0. Pre-existing
test bodies are unchanged.

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
28 regular files. It verifies the patch SHA and all nine complete original
source hashes before applying any hunk; it applies with exact old/new line
positions, no offsets/fuzz, then verifies all nine complete patched hashes. Tar members must
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
preparer verifies no regular members were added/removed, exactly the nine
declared files changed, and every other file's bytes stayed unchanged. Existing output
directories refuse without overwriting; repeated verification is read-only.
Prepare **once**, preserve this archive and receipt, and reuse the exact same
archive bytes for every A/B/C external export. Re-preparing the same inputs at
different locations produces identical archive and receipt bytes. The synthetic
control uses two wall clocks 100 seconds apart, checks the gzip header's mtime is
zero independently, and checks every tar member's mtime is zero; it does not rely
on two preparations happening within the same clock second.

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
three-arm qualification need their own nonempty execution receipts; the renewed
F1 control/build receipts are summarized below.

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
above 2^53 and UTC/Unix nanosecond agreement. Both UTC endpoints are checked in an
ordered slice, not a map keyed by timestamp text: equal or missing strings cannot
collapse the two checks.

The existing integration smoke executes the actual `Run` path in each of its 22
Go/CGo workload cells, reads the published `summary.json` into int64/string
fields, and requires the format, increasing nanosecond timestamps above 2^53,
positive measured wall time, canonical UTC agreement for each endpoint, and
containment in the child's external invocation bracket. It does not equate
realtime endpoint subtraction with the monotonic wall duration. Removing fence
publication from `Run` therefore fails the real smoke rather than merely leaving
helper tests green.

The direct invalid-`Phase` controls alone do **not** execute `Run`'s fenceErr-only
propagation branch. The new command test covers that branch with
real FoundationDB, not an in-memory database or injected transaction result. Its
parent owns one real testcontainer under a two-minute context. Each positive/zero
case runs in a fresh bounded child because native network shutdown is terminal.
The child calls the existing production CGo `openBackend` outside a supported
`testing/synctest` bubble, then calls actual `bench.Run` inside it, using one real
`read1000` operation, no warmup and one worker. Native futures wait through
callback-unlocked mutexes, which do not advance the bubble clock. The zero case
therefore expects a zero measured wall; the positive wrapper only sleeps 1ns
after a successful delegated read-only transaction and expects a 1ns wall.
Neither case substitutes data, errors, transactions, lifecycle methods or fences.

Both cases require completed=1, errors=0, successful persisted validation and
complete drains/barrier/database-close/native-network-stop. The zero case must
return exactly `invalid measured phase fences`, publish success=false and
failure_stage=`measurement_fences`, and omit all five fence keys. The positive
case must return nil, publish success=true and all five valid integer/UTC fence
fields. The existing four backend-selection controls remain intact. The command
test target gains only the sources, native linking and dependencies required to
reuse the actual CGo backend; neither executable target changes.

## Renewed F1 execution evidence

The revised nine-file overlay was prepared into fresh frozen A/B exports using
the pinned Go 1.27.3 compiler. Each whole fast lane passed **172/172 targets,
172 actual executions and zero cached results**. On each export, actual execution
passed both real-CGo positive/zero controls, all four original backend-selection
controls, all three bench fence controls and all 22 integration smoke cells.
Both real-CGo controls reached complete native lifecycle shutdown; the zero case
exercised the fenceErr-only failure-publication branch.

The three authorized frozen B benchmark/probe builds passed. All **seven**
deployed ELF files were compared byte-for-byte with the previous qualification
and were identical. Runtime `run.go`, `backend_cgo.go`, `backend_go.go`, `main.go`
and lifecycle implementation bytes remain unchanged by these test-only controls.
The renewed archive SHA256 is
`5ced06a69c21d1e24c4e5627b2656176a74427e6963b36f50128b88430e0f656`;
its source-receipt SHA256 is
`ad21d5056093a0cc90ed04d445e851b82429765b38d3f75d5e7b0d6ae9e512ed`.

A separate fresh export removed only the assignment of `fenceErr` to `runErr`.
Its whole fast lane executed all 172 targets: 171 passed and only the command
test target failed. The positive control passed; the zero control failed the
specific publication assertion with completed=1, errors=0, persisted validation
ok, complete database/network shutdown, wall=0, success=true and a nil returned
error. This is a **compiled, applied, branch-only RED**, not a drain, setup or
earlier-guard failure. Exact source restoration then passed the whole fast lane:
**172/172 targets, 3 actual executions and 169 cached results**. The real-CGo
command, integration and constfoldcensus targets actually reran; bench fence
controls reused their passing cache. Every phase retained unchanged
source/helper/archive/runtime seals and Bazel shutdown exit 0.

The final F1 qualification packet SHA256 is
`47b3737b7b734d3a017a87b567414a9f2100ead2ef2338214c1fc5419ce5577a`.
It binds these receipts:

| Receipt | SHA256 |
|---|---|
| A corrected offline artifact discovery | `1a3e9b453fad7f8b0cb91d4586c4ef2d658286b1ae08336d56e594c04910c47c` |
| B whole fast lane and seven ELF comparisons | `19ccd02a4fcf275f626f4e75c1571ffe0e2ab308f9b7daebf0e41414553ee4f0` |
| Compiled propagation-removal RED | `3a75494b33f698dbc93cb854bbc258a60d25ea97ffa7fdea93f1537f3ed7e691` |
| Exact restored GREEN | `a535cb6eb98845789132cd672294fa2aa5c8dedaf7ee0a934ad923a014b59af2` |

Failure history is preserved, not overwritten. The first A attempt failed nogo's
gofumpt check: 12 actual passing targets, one build failure and 159 skipped
targets, with **zero** positive/zero child executions and no runtime/drain
outcome. The pinned formatter corrected only the new multiline fence-key literal.
The next A whole lane passed all 172 actual targets, but its supervisor then
failed artifact collection by assuming a default testlogs configuration instead
of the CGo transition. That original FAIL script/log/receipt/BEP remains intact.
The supplemental A receipt discovers and checks the existing passing log/XML
through their pinned BEP URIs; **A was not rerun** to repair postprocessing.

Measured endpoints are saved before drains, while fence validation runs after
shutdown. `DrainFinalizers` uses bubble timers and runtime finalizers execute
outside the bubble. The recorded executions completed both drains; this does
not authorize hiding any future timeout with a rerun or weaker assertion.
Earlier qualification receipts still describe only their own pinned bytes.
The qualified overlay input copy is preserved separately; this results update
is a bounded README-only change, not a new harness or source receipt.

These results close the tested F1 error-publication gap. They do **not** establish
VPC placement, four-node F0/P0, performance, server CPU accuracy, campaign launch
readiness or authorization to create paid resources. Whole fast lanes are not
full-suite verification. The new repository integrity/preparer controls still
require whole-repository `just test` after integration; frozen A/B validation
does not run the current campaign fences package.

Repository validation remains `just test` (and the authorized full lane at the
workstream boundary), not direct Python tests or `go test`. On the shared
machine use `tools/lanes/run-verified-lane.sh` with the owned clean worktree.
External overlay application, frozen-export builds/controls and untimed
qualification must be separately recorded before any measured run. Preparing
this overlay does not execute tests, builds or benchmarks; neither Python-only
controls nor a failed/empty whole-lane analysis is a Go execution receipt.
