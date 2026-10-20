# Frozen-harness build rehearsal — 2026-10-20

**Cleanup compatibility amendment:** the current M1 client binaries and
matching campaign scripts are documented in
[CLEANUP-COMPAT-BUILDS.md](CLEANUP-COMPAT-BUILDS.md) and
`cleanup-compat-builds.json`; they supersede the unamended client artifacts below.

**Execution update:** owner-authorized builds for current master
`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` are documented in
[EXECUTION-BUILDS.md](EXECUTION-BUILDS.md) and `execution-builds.json`.
The previous rehearsal artifacts are preserved under `rehearsal-ae14b220/`;
the historical text below is unchanged.

This directory is an external, mode-0700 build workspace, not a Git worktree.
No repository file or cloud resource was changed. **No test runner or benchmark
was run.** The only executable checks besides compilation and linkage/version
inspection were the two refusal-only CLI probes documented below. These results
prove build compatibility, not runtime correctness or performance.

## Immutable inputs

| Role | Commit |
|---|---|
| Previous final Go production tree | `267a41434a81723b07e7150869d702636a1b91a9` |
| Frozen benchmark harness | `d94b4c61377d173b7c6884fde426a227dab0f0bb` |
| Current `origin/master` rehearsal, read from `${FDB_GO_WORKTREES}/fdb-go-flake` | `ae14b220883c4985031d5659f8b34cd7ecece8a9` |
| RFC-263 final production tree | **TBD; not built or timed here** |

Authority read through `git -C ${FDB_GO_WORKTREES}/fdb-go-flake show origin/master:PATH`:

- `docs/benchmarks/campaign-harness.md`
- `docs/benchmarks/hcloud-rebench-20261014/README.md`
- `docs/benchmarks/hcloud-multinode-vpc-20261014/README.md`

The previous multi-node campaign explicitly reused the single-node campaign's
compiled final-tree executables (`reuse_only`), with the SHAs above. The previous
rebench also included legacy `9a9a448670a748e4ea721772ba96d70ccc4210a0`, before
`3824f14654edae8431b0085de8ebf83f5a460a08`, and PR2
`5052212f520d1f888429d0bbcc749b657e27f39e`.

## Exact previous build method

The retained authoritative builders are:

```text
${FDB_PERF_HOME}/hcloud-rebench-20261014/build-harness-vm.sh
${FDB_PERF_HOME}/hcloud-rebench-20261014/build-client-vm.sh
```

They extracted immutable production source archives into `/srv/fdb-bench/trees`,
applied the applicable benchmark overlays, invoked **`bazelisk build`** (not
`bazelisk test`), resolved each executable with `bazelisk cquery --output=files`,
and installed the executable outside the source tree. The build used Bazel
9.0.1, `--jobs=16`, one shared disk/repository cache, and Go 1.26.9. Bazel was
shut down between trees.

For the previous final tree, the five testing.B build targets were:

```text
//pkg/recordlayer:recordlayer_test
//pkg/recordlayer/benchfetch:benchfetch_test
//pkg/recordlayer/benchfetch/planskip:planskip_test
//pkg/relational/sqlbench:sqlbench_test
//pkg/relational/sqlbench/fetchmethod:fetchmethod_test
```

The two client targets were:

```text
//tools/clientbench/cmd/clientbench:clientbench_go
//tools/clientbench/cmd/clientbench:clientbench_cgo
```

The previous `actual-source-verification.json` records zero mismatches for each
production-tree-plus-overlay export. Its `final-harness-review.json` records no
Go/dependency-closure delta from the earlier c884 harness; the final d94 change
was scripts/tests/docs and did not require another Go executable rebuild.

## Overlay policy for this rehearsal

The original final overlay archives are retained under:

```text
${FDB_PERF_HOME}/hcloud-rebench-20261014/overlays/final/
```

| Archive | SHA-256 | Regular files checked against d94 Git blobs |
|---|---|---:|
| `common.tar.gz` | `d773b59aaa4e5876336e89c3b354466a091fb1f4d20628c578eca8d2952b82ab` | 7 |
| `remote.tar.gz` | `6f393cbcce028f74a6d8413a9855ca522a1deb829e4e56db02fd250234f39e6e` | 3 |
| `final-only.tar.gz` | `f7c70d481adb8171017d78164fb72dbd4060eafe2b2811573634bd3518448335` | 5 |
| `client-harness.tar.gz` | `b705edda9bb2186c97ed16370347790cf5033ccd0b8882d13b1035e7dc94ad81` | 28 |
| `harness-doc.tar.gz` | `4a15139b57842d058276124c16be0b267640703c649ebbe4da8ccf7957abc846` | 1 |

All five archive hashes match the original source receipt; all 44 regular-file
members match the frozen harness commit byte for byte. The retained
`sources/final.tar.gz` has SHA-256
`df5794e6ea7b541d5367e11bc4579c755d482d87dca59bd048acb6121e40730a`
and Git archive PAX comment `267a41434a81723b07e7150869d702636a1b91a9`.
The other three production source archives also remain available and match
their receipt hashes; see `artifact-audit.json`.

**Freeze all of `tools/clientbench`, not just its scripts.** The production
clientbench harness has changed since d94 (including lifecycle, run and pacing
code). Before extracting `client-harness.tar.gz`, this rehearsal removed the
entire `tools/clientbench` directory **in each external export**. Therefore no
new-master clientbench source files survived alongside the frozen harness.
The common/remote/final-only directories were similarly replaced in the
external exports. The d94 M2, SQL and classic RL benchmark paths are already
byte-identical to rehearsal master, but applying the same overlay makes the
source policy explicit.

## Reproducible build commands

The executed helper, with only later rerun-safety/parameterization edits, is:

```sh
C=${FDB_PERF_HOME}/campaigns/fdbmn-20261020
bash "$C/build-check/build-frozen.sh"
```

It uses the original final source archive for `old`, and the following command
for rehearsal `master`:

```sh
git -C ${FDB_GO_WORKTREES}/fdb-go-flake archive \
  ae14b220883c4985031d5659f8b34cd7ecece8a9 | tar -xf - -C "$C/build-check/trees/master"
```

It replaces only benchmark directories inside those exports, extracts all five
frozen overlay archives, then runs this build in each exported tree:

```sh
root="$C/build-check"
export BAZELISK_HOME="$root/cache/bazelisk"
export TMPDIR="$root/tmp"
bazelisk --output_user_root="$root/cache/bazel" build \
  --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
  --lockfile_mode=error --jobs=4 \
  --local_resources=cpu=4 --local_resources=memory=8192 \
  //pkg/recordlayer:recordlayer_test \
  //pkg/recordlayer/benchfetch:benchfetch_test \
  //pkg/recordlayer/benchfetch/planskip:planskip_test \
  //pkg/relational/sqlbench:sqlbench_test \
  //pkg/relational/sqlbench/fetchmethod:fetchmethod_test \
  //tools/clientbench/cmd/clientbench:clientbench_go \
  //tools/clientbench/cmd/clientbench:clientbench_cgo
```

For example, the executable resolution/install step is:

```sh
path=$(bazelisk --output_user_root="$root/cache/bazel" cquery \
  --repository_cache="$root/cache/repository" --disk_cache="$root/cache/disk" \
  --lockfile_mode=error --output=files \
  //pkg/recordlayer/benchfetch:benchfetch_test)
install -m755 "$path" "$root/bin/master/fetch"
bazelisk --output_user_root="$root/cache/bazel" shutdown
```

The helper now accepts `MASTER_REVISION=FULL_SHA` for a later final-tree build;
that version has passed `bash -n`. It deletes/recreates only its own exported
`trees/{old,master}` and copied `bin/{old,master}`, not any Git worktree. Do not
rerun it until existing rehearsal artifacts have been copied if needed.
A future override is **not** evidence until its logs and output identities are
verified. The complete seven-target rehearsal is broader than the eventual
M2-only new-master build needs.

This local rehearsal read the existing `${HOME}/.bazelrc`. Its native build
flags point at the local official 7.3.77 headers/library under
`${HOME}/.local/share/foundationdb/7.3.77`, including a host-specific rpath.
Do not copy those absolute paths into cloud build flags. The previous cloud
builder instead installed the pinned 7.3.77 client/server Debian packages.

## Build results and compiler

| Export | Result | Elapsed | Actual sandbox processes | Disk-cache processes | Internal processes |
|---|---|---:|---:|---:|---:|
| Old final + full frozen harness | seven targets built | 102.299 s | 999 | 0 | 56 |
| Rehearsal master + full frozen harness | seven targets built | 85.442 s | 136 | 877 | 56 |

These are build process counts, **not test execution counts**. No test target
was executed, no correctness lane was run, and no timing sample was taken.
Source manifests verified all 7,103 old-export and 7,340 master-export regular
input files unchanged after compilation. `--lockfile_mode=error` also left the
module lock unchanged. `${FDB_GO_WORKTREES}/fdb-go-flake` remained clean.

All 14 copied executables report **Go 1.26.9**. Both resolved Bazel SDK
`bin/go` executables have SHA-256:

```text
e90c4e12df236ea0c2e7536fd68da8a47fc0b585c19588bf6c1a70cfc296a0f9
```

That equals the original rebench compiler receipt. The host's `go version`
reports Go 1.27.1, but it did not compile these Bazel targets. Both immutable
production revisions use `go 1.26.0` plus `toolchain go1.26.9`, and their
`MODULE.bazel` uses `go_sdk.from_file(go_mod = "//:go.mod")`. In rules_go 0.60.0,
`go/private/go_mod.bzl:version_from_go_mod` prefers the `toolchain` directive,
then the `go` directive. The suspected Go 1.26.10 mismatch was checked and is
not present in the inspected master. Re-check the actual RFC-263 final SHA.

The rebuilt old `client-go` is **bit-identical** to the prior executable:
`505d7fe8de93824cf844645dcd58be5e931a05dd0e2866c14f4bb0933d2c479e`.
The six other old executable hashes differ from the prior artifacts; no
bit-reproduction claim is made for them. The rebuilt CGo binary contains the
local native-library rpath absent from the archived binary. The rebuilt
old/master CGo executables are identical to each other, SHA-256
`82d6dd5a779586b1a7799b1eae315048eb7ff34be4652bca33fc23b1f2dbb12c`.
All output hashes are in `logs/{old,master}-binaries.sha256` and
`artifact-audit.json`.

## Original executables and runtime recovery

All **14 executables in the original rebench `all-binaries.sha256` manifest**
remain under:

```text
${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014/binaries/rebench-20261014/bin/
```

Every one matches its original SHA-256. That directory also retains both
placementprobe executables; their two hashes match their separate manifest.
The original M1 final Go/CGo identities are:

```text
505d7fe8de93824cf844645dcd58be5e931a05dd0e2866c14f4bb0933d2c479e  client-final-go
9305c28f3f66319b127fb0dad526ea4539f499eddefb832c3b5427df2b0c1e37  client-final-cgo
```

The multi-node/rebench archives contain runtime hashes, not the runtime
payloads. However the **exact three original payloads were recovered** here:

```text
${FDB_PERF_HOME}/results/fdb-perf-20261009/baseline-complete-v3/artifacts/binaries/
```

| File | SHA-256, equal to previous multi-node runtime receipt |
|---|---|
| `fdbserver` | `179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f` |
| `fdbcli` | `6dfe3328389fe8ff8175ddc080e278c5b07d1d2601f97079cba1194bfb2e41fb` |
| `libfdb_c.so` | `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1` |

The recovered `fdbserver --version` and `fdbcli --version` report 7.3.77,
source `3ea44ce1d9003ad095e408039e1f755c319c4dfb`, protocol
`fdb00b073000000`. Their dynamic dependencies resolve on this host. The same
exact library also exists in the r17/r18 extracted `inputs/usr/lib/` sibling
archives under `results/fdb-perf-20261009`.

The unrelated local official release library under
`${HOME}/.local/share/foundationdb/7.3.77/lib/libfdb_c.so` has SHA-256
`617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa`,
**not** the original package's library hash. Do not silently substitute it in
a historical-runtime reproduction. Without a runtime override, the archived
CGo executable on this host reports `libfdb_c.so => not found` from `ldd`.
The recovered exact library fixes that:

```sh
runtime=${FDB_PERF_HOME}/results/fdb-perf-20261009/baseline-complete-v3/artifacts/binaries
bin=${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014/binaries/rebench-20261014/bin
LD_LIBRARY_PATH="$runtime" ldd "$bin/client-final-cgo"
```

Original Debian packages were not found by the bounded search
`find ${FDB_PERF_HOME} ${HOME}/Downloads /tmp /var/cache -maxdepth 4`
for `.deb` filenames containing `fdb` or `foundation`; that is not a claim of
absence from the whole machine. Their original pinned download identities
remain in `hcloud-rebench-20261014/setup-vm.sh`:

```text
https://github.com/apple/foundationdb/releases/download/7.3.77/foundationdb-clients_7.3.77-1_amd64.deb
b6c263723009bddbc30b6ae8cf9854340bd35cef14152abb0da8fb5af2b91254
https://github.com/apple/foundationdb/releases/download/7.3.77/foundationdb-server_7.3.77-1_amd64.deb
789f1ed4cbacc0fd7043da2217c9e57516e4ec94fa7140853ec22a7e715785cc
```

No download or installation was performed here. The exact extracted payloads
are sufficient for runtime identity recovery; OS libraries and a new cluster
configuration still need their own campaign seal.

## Refusal-only placementprobe checks

Using the retained `placementprobe-cgo`, SHA-256
`3ac36d47aedd0b30439aac20e0edbdba94d4e164afd41415d656479a8de98494`,
with `LD_LIBRARY_PATH` pointing to the **exact recovered original runtime**:

| Invocation | Exit | Stdout | Output receipt |
|---|---:|---:|---|
| no arguments | 2 | 0 bytes | none |
| `probe` with store `benchfetch/nope` | 2 | 0 bytes | absent |

Both emitted the expected diagnostics on **stderr**; “no output” here means no
stdout or receipt, not silent stderr. Inputs, stdout/stderr captures, and exact
argv are retained in `refusal-probes/` and `artifact-audit.json`. The unowned
probe used a synthetic cluster file and a deliberately unowned store. Source
review confirms both paths return before `openCluster`; no cluster connection,
mutation, or campaign measurement was attempted.

The unowned invocation was:

```sh
LD_LIBRARY_PATH="$runtime" "$bin/placementprobe-cgo" probe \
  --cluster-file "$C/build-check/refusal-probes/synthetic.cluster" \
  --store benchfetch/nope \
  --fixture-receipt "$C/build-check/refusal-probes/fixture.json" \
  --output "$C/build-check/refusal-probes/unowned-output.json"
# exits 2; stdout empty; unowned-output.json is not created
```

## Handoff boundary

The new campaign label/prefix is `fdbmn-20261020-NONCE`, VPC CIDR
`10.89.20.0/24`. Current owner direction is new-master-only M2 first, then M1
with old Go/new Go/CGo, six balanced blocks. No plan, resource, or timing run
for that campaign was created here. RFC-263's final master SHA remains the
input to replace and rebuild before any final benchmark seal.
