# Cleanup-compatible frozen client harness

**Authorized amendment:** `d94b4c613 + #948 lifecycle drain fix + matching run.py validator`.
Production revisions remain old `267a41434a81723b07e7150869d702636a1b91a9`
and master `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`.

This supersedes the unamended client binaries in `EXECUTION-BUILDS.md` for all
three M1 arms. The original execution binaries/logs/audit are preserved under
`unpatched-execution-fa8725f1d/`. The earlier ae14b220 rehearsal is also intact.
No original frozen overlay was modified.

**CPU-heavy rebuilds finished at 2026-10-20T10:18:21Z. Both Bazel servers were
explicitly shut down. The coordinating agent and root were notified immediately;
no further build is planned.**

No tests, benchmarks, cluster connections, cloud mutations, or Git worktree
edits were performed. New test fixtures were included as source, **not executed**.
Python source was syntax-parsed without importing or executing it.

## Failure and exact authorization

The coordinating agent reported that an untimed current-master F0 capacity
probe completed 32 GRVs with zero operation errors but failed at
`measurement_drain`: d94 rejected newly queued runtime cleanups even though
38 were queued and 38 had executed. This observation was reported by the
campaign coordinator, not reproduced in this local build task.

The current production client legitimately registers `runtime.AddCleanup`:
`read_incarnation.go` stops a transaction's timeout timer once the transaction
is unreachable, and `native_commit.go` also registers cleanup ownership.
A GC-triggered increase in completed cleanups is therefore expected, not a
transaction failure.

The initial three-line-only deletion instruction was superseded **before any
three-line-only variant was created**. The implemented Go change is exactly the
upstream fix in commit:

```text
066920ebb8d22c5618b31ef0a56fa7674c773cdb
fix(fdbgo): a transaction's timeout timer no longer keeps it alive (#948)
```

The two Go files are byte-identical to that upstream commit:

- `tools/clientbench/internal/bench/lifecycle.go`: exact 13-line upstream diff.
- `tools/clientbench/internal/bench/lifecycle_test.go`: exact companion
  40-line cleanup dependency-chain test addition, in an already-declared file.

The Go implementation permits new cleanups, but still requires **both**
finalizer and cleanup executed counts to equal their queued counts, and **both
queued counts to stabilize across consecutive drained GC rounds**. Tracking
only finalizer stability would miss a cleanup chain that releases more objects
for the next GC. The existing GC-cycle checks, context cancellation, 16-round
limit, 30-second timeout, before/after counters, JSON schema and shared
measurement/shutdown drain entry points are unchanged.

Go 1.26.9 documents cleanup counts as **estimates**, unlike the exact finalizer
counters. Its cleanup executed count is advanced after a cleanup block has run;
its counter sampling is explicitly approximate. This amendment retains that
metric-based completion contract; it does not turn the estimates into a proof
that every possible cleanup in the process has run. Native barrier and terminal
shutdown checks remain required.

## Matching Python half

Read-only investigation found an upstream latent compatibility bug: although
fa8725f1d already has the Go #948 drain fix, its unchanged
`tools/clientbench/scripts/run.py:validate_lifecycle` still rejects any cleanup
queued-count growth. Its analysis scope prose and `unexpected_cleanup` fixture
retain the same obsolete assumption. Root explicitly authorized correcting
these **campaign copies only**, with no repository edit.

Three additional source changes were applied identically to both exports:

1. `scripts/run.py`: remove only
   `after['cleanups_queued_estimate'] != before['cleanups_queued_estimate']`.
   Executed-equals-queued, counter monotonicity, GC cycles, round bounds,
   completion, resource accounting and all other checks remain intact.
2. `scripts/test_campaign.py`: add a completed-cleanup-growth acceptance fixture
   for both drains; replace the obsolete rejection with pending-cleanup failure
   fixtures for both measurement and shutdown drains. These tests were not run.
3. `scripts/analyze.py`: correct only `LIFECYCLE_CPU_SCOPE` prose. AST comparison
   with that one string constant normalized confirms numeric analysis is
   unchanged.

There are exactly **five changed files** compared with the original 28-file
client harness. No changes to workload code, `run.go`, pacing, `main.go`, BUILD
files, timings, CPU snapshot placement, or output schemas were included.
The delayed cleanup/forced-GC work remains outside per-operation latencies and
inside the existing drained/shutdown CPU scopes. E−A still subtracts the
post-identity snapshot from post-shutdown process CPU; no accounting term was
removed or redefined.

## Patches and new frozen artifact

| Artifact | SHA-256 |
|---|---|
| Exact upstream two-Go-file patch, `cleanup-compat-upstream.patch` | `8d25bd6b4eaa91b6f3eb44da5360d5235f27fbf319dbb0dfab96f1f4397727d3` |
| Combined five-file patch, `cleanup-compat-combined.patch` | `31bc349b7a1ef71fc5bbf300ed728fe536e161f5610a1187edc718e63fddb007` |
| Original unmodified client archive | `b705edda9bb2186c97ed16370347790cf5033ccd0b8882d13b1035e7dc94ad81` |
| New cleanup-compatible client archive | `76990b867f337bdad0132cc69aa3f6f9a9bcf0cb0859cc8c7f8514def684f508` |

New archive:

```text
${FDB_PERF_HOME}/campaigns/fdbmn-20261020/frozen/cleanup-compat/client-harness.tar.gz
```

It preserves exactly the original **28 regular-file paths**, with only the five
listed contents changed. Both external client harnesses match every archive
member byte for byte. The Go and CGo backends therefore consume identical
amended client-harness source bytes across all three arms. The original five
frozen overlay archives remain unchanged.

Original/new lifecycle file SHA-256 values:

| File | Original d94 | Cleanup-compatible, exact #948 |
|---|---|---|
| `lifecycle.go` | `c6cfcef4efdfac6e53aea3a1d474a5519a95717105b4135048cad4e0abd03ccf` | `4374b421d96b4d88ed24486e5adefbea030cca66ed10aac7414bc22819681235` |
| `lifecycle_test.go` | `e7e8125722aecfd968778d8a9da9845aab4ca41bde3731b5bf6cf923dc5453ef` | `cb908bd3801d419ccbcb0c1e6186b2e165bc60f8fe10175479e4c0e8dce29b17` |

All five original/new source hashes are recorded in
`cleanup-compat-source-amendment.json` and `cleanup-compat-builds.json`.
The combined patch and archive were generated and checked **before** either
build's source freeze; no export was edited while a build was running.

## Rebuild commands and results

The exact client rebuild helper is `build-cleanup-compat.sh`:

```sh
C=${FDB_PERF_HOME}/campaigns/fdbmn-20261020
bash "$C/build-check/build-cleanup-compat.sh"
```

For each existing, amended external export, it invokes:

```sh
root="$C/build-check"
export BAZELISK_HOME="$root/cache/bazelisk"
export TMPDIR="$root/tmp"
cd "$root/trees/old" # then repeat for trees/master
bazelisk --output_user_root="$root/cache/bazel" build \
  --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
  --lockfile_mode=error --jobs=4 \
  --local_resources=cpu=4 --local_resources=memory=8192 \
  //tools/clientbench/cmd/clientbench:clientbench_go \
  //tools/clientbench/cmd/clientbench:clientbench_cgo
```

It resolves both targets using `cquery --output=files`, installs the binaries
into `bin/{old,master}/`, checks its pre-build source checksum manifest, and
shuts down Bazel before moving to the other export.

| Export | Result | Elapsed | Action-cache hits | Disk-cache hits | Sandbox | Internal |
|---|---|---:|---:|---:|---:|---:|
| old | both client targets built | 5.064 s | 300 | 0 | 10 | 3 |
| master | both client targets built | 4.765 s | 308 | 7 | 3 | 3 |

Four client files were rebuilt, representing three distinct arm executables
because the two CGo outputs are identical. The eleven non-client outputs,
including M2 binaries and current-master placementprobe, remain byte-identical
to the preserved unpatched execution artifacts. This was a client-only rebuild,
not another seven-target build or a test run.

Do not rerun `build-frozen.sh` and deploy its outputs without reapplying this
new cleanup-compatible archive: that older helper restores the unamended d94
client source. To reconstruct from unamended external exports, apply
`cleanup-compat-combined.patch` outside Git or extract the new archive over the
original frozen 28-file client tree, then run the client rebuild helper.

## Output identities, compiler and source verification

| Arm/output | New SHA-256 |
|---|---|
| `bin/old/client-go` | `c70e4d275d586f73430f0129c536a8fa6bcc950678c83d06a98f8c65a09f7e34` |
| `bin/master/client-go` | `187a282a8bb337a60ae88e5d54c227bed3bc79e1dc172e4184120df14408d252` |
| `bin/old/client-cgo` and `bin/master/client-cgo` | `9032ee8f58e34b616b710dbaeb15061d5f3c322c9fbf6395e51a1bd9b78f7cbe` |

All fifteen installed artifacts, including the eleven unchanged outputs, are
listed with original/new hashes in `cleanup-compat-builds.json` and with current
hashes in `logs/cleanup-compat-binaries.sha256`.

Every installed executable reports **Go 1.26.9**. Both Bazel SDK `bin/go` hashes
still equal the original compiler:

```text
e90c4e12df236ea0c2e7536fd68da8a47fc0b585c19588bf6c1a70cfc296a0f9
```

Verification checked all **7,103 old / 7,340 master source filenames and hashes**
after building against the corresponding pre-build freezes. It also verified
that the only differences from unpatched execution source are the five
explicitly authorized files. Generated `.bazel-bep.jsonl` is separated from
source identity, as in the previous build audit.

The rebuilt CGo binaries retain the local native-library rpath. With
`LD_LIBRARY_PATH` set to the recovered original runtime directory, both resolve
the exact historical library:

```text
${FDB_PERF_HOME}/results/fdb-perf-20261009/baseline-complete-v3/artifacts/binaries/libfdb_c.so
e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1
```

Default/override linkage captures are in
`logs/{old,master}-cleanup-cgo-ldd-{default,original-runtime}.txt`.
`verify-cleanup-compat.py` completed successfully; its stdout is retained in
`logs/cleanup-compat-verification.log`. A separate combined-patch roundtrip onto
the original frozen archive reconstructed exactly the new 28-file archive.
The coordinating agent owns the new
cloud identity seal, updated script deployment, and untimed F0 requalification;
this local task did not rerun F0 or take performance samples.
