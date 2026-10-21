# RFC-264 mapped-fetch bisect

This is the build and timing driver for the [mapped-fetch bisect evidence](../../../../../docs/benchmarks/rfc-264/mapped-fetch-bisect.md). It builds seven interior commits and reuses the archived old/master planskip binaries. It does not run correctness tests or establish a causal mechanism.

Raw binaries, logs and measurements stay under `FDB_PERF_HOME`, outside the repository. The committed plan preserves the measured arms, schedule, budgets and estimators; `${NAME}` fields are expanded from environment variables at runtime. The original expanded plan remains in the raw archive, so its hash differs from this portable copy.

## Configure

From the repository root, set these variables to existing absolute paths:

```sh
export FDB_PERF_HOME="/path/to/external/perf-data"
export FDB_BISECT_REPOSITORY="$(git rev-parse --show-toplevel)"
export FDB_BENCH_CLUSTER_FILE="${FDB_PERF_HOME}/cluster/fdb.cluster"
export FDBCLI="${FDB_PERF_HOME}/bin/fdbcli"
export PYTHONDONTWRITEBYTECODE=1
campaign="${FDB_BISECT_REPOSITORY}/tools/benchcampaign/campaigns/rfc-264/mapped-bisect"
```

The local recorded campaign used one loopback storage server, single redundancy and the memory engine. The plan pins benchmark clients to CPUs `2-5,14-17`, with `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off` and no `GODEBUG` overrides. Reproducing the recorded configuration requires those CPUs and the same Go 1.26.9 SDK. Do not silently change the plan for a different host: freeze and label a new plan before building or timing.

Restore the archived anchor binaries to:

```text
${FDB_PERF_HOME}/profiles/reference-client/bin/old-planskip
${FDB_PERF_HOME}/profiles/reference-client/bin/master-planskip
```

Their SHA256 values are pinned in `plan.json`. Offline parser checks additionally use the recorded A2 outputs at `${FDB_PERF_HOME}/profiles/reference-client/reference-output/`. Those files are read, not executed.

## Offline checks

```sh
python3 "${campaign}/timing.py" check
```

This checks all 144 scheduled jobs, four blocks per arm/cell, four recorded parser fixtures and twelve malformed-output controls. A fake-process SIGTERM regression also verifies child reaping, failed-job receipts, owned-range cleanup and refusal to summarize an interrupted run. Its printed invalid verdict is expected. The checks do not open the cluster, run a benchmark or invoke Bazel. Run without Python `-O` or `PYTHONOPTIMIZE`; both scripts refuse disabled assertions.

## Build window

Obtain an exclusive build window first. Never run this concurrently with the repository's race, full or fast validation lanes.

```sh
python3 "${campaign}/build.py" --approved-build-window
```

The script holds `${FDB_PERF_HOME}/validation/fast.lock`, uses one dedicated detached worktree at `${FDB_PERF_HOME}/worktrees/rfc-264-mapped-bisect`, and checks out the seven interior commits sequentially. Bazel's on-disk action cache is reused in that worktree. Build, cquery and install commands all use nice 19 and CPUs `0,1,6,7,11,12,13,18,19,23`; builds use `bazelisk --batch`, default configuration and `--jobs=10`.

Each commit's five relevant harness files must match frozen harness `d94b4c613`. Binaries and logs go to `${FDB_PERF_HOME}/rfc-264-mapped/`; `build-manifest.json` records source commits, binary hashes, harness hashes, SDK version and commands. An existing build manifest is a stop, not permission to overwrite evidence. For a new reproduction use a fresh external data root with the archived anchors restored. An existing dedicated worktree is accepted only when clean, detached and at the approved starting master commit.

## Quiet timing slot

After builds finish, obtain a separate exclusive timing slot:

```sh
python3 "${campaign}/timing.py" run --approved-quiet-slot \
  --out "${FDB_PERF_HOME}/rfc-264-mapped/quiet-NEW-RUN"
```

The output directory must not exist. The driver takes the validation lock, checks for active Bazel/testcontainer work, settles for 60 seconds, then requires load1 at most 7.5, one storage server and an empty owned benchmark range. It executes the fixed 144-process schedule without resuming, rerunning or adaptive sampling. The hard limit is 20 minutes including the gate, with 30 seconds reserved for cleanup.

A failed gate or interrupted/failed process invalidates the attempt. Preserve it and obtain explicit authorization before another attempt in a fresh directory; do not loop. SIGTERM reaps the active child and enters the cleanup path. Successful runs clear and verify the owned range. No comparative summary is published unless all 144 processes and their audits pass.

Each process supplies the median of exactly three `testing.B` repeats at `-test.benchtime=1s`. Comparisons are geometric means of four within-block ratios, with every block ratio and its min/max retained. The ranges are **four-block min/max, not CIs**. See `plan.json` for the fixed primary, adjacent-arm, remote/ordinary and difference-in-differences estimators.

`summary.json` is written automatically after a valid run. To regenerate it from a completed run made by this portable driver:

```sh
python3 "${campaign}/timing.py" summary \
  --out "${FDB_PERF_HOME}/rfc-264-mapped/quiet-NEW-RUN"
```

Standalone `summary` checks the raw run's plan bytes, a valid verdict and the parsed receipts. It does not rehash binaries or revalidate build provenance; retain the original binary/build receipts for that audit. The historical raw runs retain their original expanded scripts and plan; use those archived scripts for historical reanalysis rather than bypassing the plan check.

## Recorded raw-data manifest

```text
${FDB_PERF_HOME}/rfc-264-mapped/SHA256SUMS
SHA256: d0ad9a9bf014c58b469a1494d5555e725c42cab58761a0dff82520d60b01989f
```

The manifest covers 473 regular artifact files (excluding Python bytecode caches and the manifest itself), including both timing attempts, the seven built binaries, build receipts, original plan/scripts and derived results. Verify only the unmodified archived subtree, not fresh template outputs. Its root may move because manifest entries are relative; internal paths must remain unchanged. From the external artifact directory:

```sh
cd "${FDB_PERF_HOME}/rfc-264-mapped"
printf '%s  %s\n' \
  d0ad9a9bf014c58b469a1494d5555e725c42cab58761a0dff82520d60b01989f \
  SHA256SUMS | sha256sum -c -
sha256sum -c SHA256SUMS
```
