# Execution build: current master fa8725f1d

Owner-authorized production revision:
`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`.
This replaces the RFC-263 placeholder **for this campaign**, not a claim that
RFC-263 has landed. Old production remains
`267a41434a81723b07e7150869d702636a1b91a9`; frozen harness remains
`d94b4c61377d173b7c6884fde426a227dab0f0bb`.

**All CPU-heavy builds finished at 2026-10-20T09:53:10Z. All three Bazel
invocations succeeded and their servers were explicitly shut down.**
The coordinating agent and root were notified before the lightweight identity,
linkage/refusal checks and this write-up. No further build is planned here.

No tests, benchmark timings, cluster connections, cloud mutations, or Git
worktree edits were performed. Compilation includes Bazel nogo, not a test run.

## Preserved rehearsal

Previous `ae14b220` provenance was moved intact into:

```text
${FDB_PERF_HOME}/campaigns/fdbmn-20261020/build-check/rehearsal-ae14b220/
```

It contains the previous `logs/`, `bin/`, `trees/`, `refusal-probes/`,
`artifact-audit.json`, `revisions.txt`, and copies of the previous README and
build helper. No earlier provenance was deleted.

## Executed builds

All outputs are under `build-check/bin/{old,master}/`.

| Build | Targets | Elapsed | Action-cache hits | Sandbox processes | Internal processes |
|---|---:|---:|---:|---:|---:|
| Old production + frozen harness | 7 | 6.597 s | 1,054 | 0 | 1 |
| fa8725f1d + frozen harness | 7 | 26.776 s | 969 | 93 | 7 |
| fa8725f1d placementprobe_cgo | 1 | 7.199 s | 239 | 63 | 5 |

These are build/cache process counts, not executed or cached test results.
The seven benchmark targets in each production export are:

| Installed basename | Bazel target |
|---|---|
| `rl` | `//pkg/recordlayer:recordlayer_test` |
| `fetch` | `//pkg/recordlayer/benchfetch:benchfetch_test` |
| `planskip` | `//pkg/recordlayer/benchfetch/planskip:planskip_test` |
| `sql` | `//pkg/relational/sqlbench:sqlbench_test` |
| `sqlfetch` | `//pkg/relational/sqlbench/fetchmethod:fetchmethod_test` |
| `client-go` | `//tools/clientbench/cmd/clientbench:clientbench_go` |
| `client-cgo` | `//tools/clientbench/cmd/clientbench:clientbench_cgo` |

The fifteenth output is `bin/master/placementprobe-cgo`, from:

```text
//tools/benchcampaign/multinode/placementprobe/cmd/placementprobe:placementprobe_cgo
```

### Exact commands

The orchestration used was:

```sh
C=${FDB_PERF_HOME}/campaigns/fdbmn-20261020
bash "$C/build-check/build-execution.sh"
```

That helper first invokes the original frozen-harness helper with the explicit
owner-authorized override:

```sh
MASTER_REVISION=fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc \
  bash "$C/build-check/build-frozen.sh"
```

The old source archive and both immutable revisions are exported outside Git.
Each export's entire `tools/clientbench`, `pkg/testutil/benchcluster`,
`pkg/recordlayer/benchfetch`, and `pkg/relational/sqlbench` directories are
replaced with the original frozen archives. The common classic-RL file and
harness documentation are also overlaid. The helper builds seven targets,
resolves them with `cquery --output=files`, copies them into `bin/`, verifies
its pre-build input checksums, and shuts down Bazel for each tree.

The additional current placementprobe was built from the master external
export without overlaying or changing its production placementprobe sources:

```sh
root="$C/build-check"
export BAZELISK_HOME="$root/cache/bazelisk"
export TMPDIR="$root/tmp"
cd "$root/trees/master"
bazelisk --output_user_root="$root/cache/bazel" build \
  --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
  --lockfile_mode=error --jobs=4 \
  --local_resources=cpu=4 --local_resources=memory=8192 \
  //tools/benchcampaign/multinode/placementprobe/cmd/placementprobe:placementprobe_cgo
path=$(bazelisk --output_user_root="$root/cache/bazel" cquery \
  --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
  --lockfile_mode=error --output=files \
  //tools/benchcampaign/multinode/placementprobe/cmd/placementprobe:placementprobe_cgo)
install -m755 "$path" "$root/bin/master/placementprobe-cgo"
bazelisk --output_user_root="$root/cache/bazel" shutdown
```

Full command implementations, stdout/stderr, statuses and completion timestamp
are retained in `build-execution.sh` and `logs/`. The helper is a rebuild
command, not a harmless probe: it recreates its own external export/bin paths.
Do not rerun it during another campaign's machine-quiet interval.

## Source and compiler checks

`verify-execution.py` completed successfully and wrote `execution-builds.json`.
It checked source **names and hashes**, not only a hash manifest's listed files:

- Old export: all **7,103 regular source files** unchanged.
- Master export: all **7,340 regular source files** unchanged after both builds.
- Both exports: exactly the **44 frozen overlay regular files**, byte-identical
  to the original archive union, with no extra files in the replaced trees.
- Each complete `tools/clientbench` inventory: exactly **28 frozen files**,
  identical contents and member set. No master pacing/lifecycle source leaked
  into the frozen harness.
- Current placementprobe source inventory equals the authorized master's Git
  archive exactly.

Bazel `cquery` creates/updates `.bazel-bep.jsonl` in each export. It is generated
output, not a source input; that one filename is explicitly separated in
`execution-builds.json`. Its recorded hashes are retained rather than reported
as unchanged source. No other source additions, removals or content changes
were accepted by the verification.

All **15 installed executables report Go 1.26.9**. Both Bazel SDK `bin/go`
executables match the original campaign compiler SHA-256:

```text
e90c4e12df236ea0c2e7536fd68da8a47fc0b585c19588bf6c1a70cfc296a0f9
```

The authorized master still declares `go 1.26.0` and `toolchain go1.26.9`.
The host's unrelated Go 1.27.1 was not used to compile the targets.
The old `client-go` remains byte-identical to the original campaign artifact.

## Installed executable identities

All fifteen full SHA-256 values and absolute paths are retained in:

```text
logs/execution-binaries.sha256
execution-builds.json
```

Key campaign artifacts:

| Output | SHA-256 |
|---|---|
| `old/client-go` | `505d7fe8de93824cf844645dcd58be5e931a05dd0e2866c14f4bb0933d2c479e` |
| `master/client-go` | `390b56fa9961fea43e64ad2f9ba6d4bb5c4799bd26fa4eb55694b32f11c9d4a6` |
| `old/client-cgo` and `master/client-cgo` | `82d6dd5a779586b1a7799b1eae315048eb7ff34be4652bca33fc23b1f2dbb12c` |
| `master/fetch` | `360f03d66a4afa217db03359b8f657ee43a288eb7a42150c1c615323af1af118` |
| `master/planskip` | `b2020a2044a5feba3e674d5e12a26ed76ff5b114be1bd38aa74491f5d53c29a4` |
| `master/placementprobe-cgo` | `1af0b563f4169d116a7c975bb2a15e9323853d267c219ce1ddd7a75bad5387cc` |

## Native runtime linkage

The current CGo client and placementprobe retain this machine's local rpath
from `${HOME}/.bazelrc`:

```text
${HOME}/.local/share/foundationdb/7.3.77/lib
```

That local official-release library has SHA-256
`617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa`,
not the historical package payload. Runtime checks explicitly overrode it with
the **exact original runtime**, recovered at:

```text
${FDB_PERF_HOME}/results/fdb-perf-20261009/baseline-complete-v3/artifacts/binaries/
```

| Payload | Verified SHA-256, equal to historical receipt |
|---|---|
| `fdbserver` | `179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f` |
| `fdbcli` | `6dfe3328389fe8ff8175ddc080e278c5b07d1d2601f97079cba1194bfb2e41fb` |
| `libfdb_c.so` | `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1` |

For both new native executables, `LD_LIBRARY_PATH` to this directory makes
`ldd` resolve `libfdb_c.so` to that exact original file. Default linkage,
ELF dynamic entries, and original-runtime linkage are captured separately in
`logs/{client-cgo,placementprobe-cgo}-{ldd-default,ldd-original-runtime,dynamic}.txt`.
Do not silently time the local official-release library, and do not assume a
host-specific rpath will exist on a cloud host. Provisioning must seal the
runtime actually deployed and loaded.

## Current placementprobe refusal checks

The **newly built current-master** `bin/master/placementprobe-cgo`, loaded with
`LD_LIBRARY_PATH` pointing at the original runtime above, was invoked twice:

| Probe | Exit | Stdout | Receipt output |
|---|---:|---:|---|
| no arguments | 2 | 0 bytes | none |
| `probe --store benchfetch/nope` with synthetic cluster/fixture inputs | 2 | 0 bytes | absent |

Expected diagnostics are on stderr; “no output” does not mean silent stderr.
These paths reject before opening the cluster. The synthetic cluster file uses
`10.89.20.11/12/13:4500`; no live cluster interaction was attempted.

Exact invocations and environment are in `execution-builds.json`; input files
and raw stdout/stderr are in `execution-refusal-probes/`. Reproduction:

```sh
runtime=${FDB_PERF_HOME}/results/fdb-perf-20261009/baseline-complete-v3/artifacts/binaries
LD_LIBRARY_PATH="$runtime" "$root/bin/master/placementprobe-cgo"
# exit 2, empty stdout; usage on stderr
LD_LIBRARY_PATH="$runtime" "$root/bin/master/placementprobe-cgo" probe \
  --cluster-file "$root/execution-refusal-probes/synthetic.cluster" \
  --store benchfetch/nope \
  --fixture-receipt "$root/execution-refusal-probes/fixture.json" \
  --output "$root/execution-refusal-probes/unowned-output.json"
# exit 2, empty stdout; unowned-store error on stderr; no output JSON
```

This is build/linkage/refusal evidence only. The coordinating agent owns the
external protocol, provisioning, and subsequent campaign execution.
