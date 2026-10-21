# RFC-264 get/set latency bisect

Portable plans and build/timing drivers for the [latency-bisect evidence](../../../../../docs/benchmarks/rfc-264/latency-bisect.md). Raw binaries, source receipts, logs and samples remain outside the repository. No command here is authorization to build or benchmark.

## Recorded studies

- **v1:** four-block anchor SLOT 1, get100/set100 at w8/w64, 32 processes. Only get100 w8 met its preregistered reproduction gate. The broad original SLOT 2 was **not run**.
- **v2:** a new separately preregistered study, not an amendment of SLOT 2: twelve arms, four rotated blocks, primary get100 w8 p50 and secondary **exploratory** get1000 w8 p50, 96 processes. Both studies retain per-process p99 as descriptive diagnostics.

Both reuse the exact amended frozen M1 client harness: `d94b4c613` plus the authorized lifecycle fix and matching validator. `frozen_run.py` is the archive's unchanged validator implementation; its original standalone runner is not invoked. The archive SHA256 is `76990b867f337bdad0132cc69aa3f6f9a9bcf0cb0859cc8c7f8514def684f508`.

These templates use example paths, loopback ports and owned prefixes, not the historical configuration. Their hashes differ from the expanded historical plans. The original plans/scripts remain in the external raw archive; use those for historical reanalysis. A new run requires a separately approved plan with matching cluster, binary and provenance pins. Never bypass an identity check to reuse the archived results.

## Configure

Set existing absolute paths; unresolved variables fail closed:

```sh
export FDB_PERF_HOME="/path/to/external/perf-data"
export FDB_BISECT_REPOSITORY="$(git rev-parse --show-toplevel)"
export PYTHONDONTWRITEBYTECODE=1
campaign="${FDB_BISECT_REPOSITORY}/tools/benchcampaign/campaigns/rfc-264/latency-bisect"
```

`${FDB_PERF_HOME}/rfc-264-latency/` is the external artifact root. Plans are read from this package; outputs and binaries are written only outside the repository. `${FDB_BISECT_REPOSITORY}` is used only as the source repository for a new dedicated build worktree. The timing scripts never restart, reconfigure or change server affinity.

The recorded study used an existing loopback/tmpfs cluster, memory engine, single redundancy and one storage server. Clients used CPUs `2-5,14-17`, `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off` and empty `GODEBUG`; server CPU partitions were `[8,20]`, `[9,21]` and `[10,22]`. The template checks example loopback ports 4500, 4501 and 4502 for those partitions. These ports are not historical identities. Freeze a new plan and update the input pins for the target cluster before an authorized run.

Restore the external archive and its matching dependencies before running checks. Plans expect:

```text
${FDB_PERF_HOME}/profiles/reference-client/bin/old-go
${FDB_PERF_HOME}/profiles/reference-client/bin/master-go
${FDB_PERF_HOME}/profiles/reference-client/bin/fdbcli
${FDB_PERF_HOME}/cluster/reference.cluster
${FDB_PERF_HOME}/reference-inputs/frozen/cleanup-compat/client-harness.tar.gz
```

Byte-identical copies are included in the raw archive as `bin/old-go`, `bin/master-go`, `inputs/fdbcli`, `inputs/fdb4.cluster`, and `inputs/client-harness.tar.gz`. Restore them to the plan's paths without overwriting another campaign's files. The cluster must already be running and have the pinned identity; archived cluster bytes are not a request to start it. Interior binaries are already under the artifact root's `bin/` directory.

## Offline verification

```sh
python3 "${campaign}/check_offline.py"
python3 "${campaign}/v2/check_offline.py"
python3 "${campaign}/dry_run.py"
```

These checks do not open FDB, invoke Docker or Bazel, or run real benchmark clients. They print reports without overwriting archived reports. The first verifies the v1 census, pins, schedules, synthetic quantiles and correct per-server affinity assertion. The v2 check verifies all twelve reused binary/provenance pins, 96 unique 68-byte prefixes, four blocks/positions per arm/cell, all eleven synthetic adjacent estimates, telescoping to the endpoint, and invalid-run rejection. It runs the actual taskset/mask for all 96 exact argument lists with only the client executable replaced by `/bin/true`.

`dry_run.py` exercises both original controller paths using synthetic client/FDB/Docker stubs, actual taskset and the unchanged raw validators: 32/32 +216/216 fake client processes, eight fake CLI calls, availability-negative checks, cleanup and summaries. Its fixture comes from the archived successful SLOT 1. **Synthetic controller tests are not execution of original SLOT 2 and not performance evidence.** They use temporary files and an isolated lock. Run Python without `-O`/`PYTHONOPTIMIZE`.

These archived-input checks are separate from repository fast/full validation lanes. They require the reference files at the configured paths; a missing or mismatched input is a failure, not permission to skip its identity check.

## Build window

Historical binaries and provenance belong to the original archive, not this template's renamed layout. A new build requires separate owner approval, a fresh artifact/build-manifest destination, and a fresh dedicated worktree:

```sh
python3 "${campaign}/build.py" --approved-build-window 'OWNER-APPROVAL-REFERENCE'
```

The build checks out ten interior source commits sequentially in `${FDB_PERF_HOME}/worktrees/rfc-264-latency`, overlays only the exact 28-file amended harness, and invokes `bazelisk --batch` for build/cquery. Build, cquery, install and build-info inspection use nice19 and CPUs `0,1,6,7,11,12,13,18,19,23`; builds use jobs=10 and the existing disk/repository caches. They do **not** acquire the timing validation lock. Obtain an exclusive build window; never overlap builds with a measurement.

`build-manifest.json` records source SHA, copied binary SHA256, harness file hashes, Go build information and commands. Existing worktrees/manifests are refused, not overwritten. The runners require binary paths to match their build manifest; moving archived files does not relocate its pinned path strings. The v2 template retains the historical manifest/source-plan pins, so a new layout or build series requires a newly frozen v2 provenance plan before any checks or timing. Do not rewrite an archived manifest or relabel new binaries as the recorded ones.

## Separately approved quiet slots

Before settling, the runner checks per-address affinity and every executable it launches: Docker, taskset, fdbcli and each selected client binary. It retains process wall/exit, harness CPU/lifecycle statistics and telemetry.

All timing requires the shared `${FDB_PERF_HOME}/validation/fast.lock`, no detected Bazel clients/actions or testcontainers/reapers, a continuously quiet 60-second settle, and load1<=7.5. Idle Bazel servers may remain. The load threshold is a start gate, not subsequent sample censoring. Activity during timing invalidates the attempt. Preserve failed attempts; do not poll/retry until a gate happens to pass.

For a newly authorized v1 anchor reproduction:

```sh
python3 "${campaign}/timing.py" run --slot slot1 \
  --output "${FDB_PERF_HOME}/rfc-264-latency/slot1-NEW" \
  --approved-quiet-slot 'OWNER-SLOT-REFERENCE'
```

The cap is15 minutes including the gate, with30 seconds reserved for cleanup. The original broad SLOT 2 requires a complete valid SLOT 1 meeting **all four** preregistered reproduction gates. The recorded SLOT 1 did not meet that prerequisite, so the recorded broad SLOT 2 remains unrun.

For a separately authorized v2 run, after freezing a matching provenance plan:

```sh
python3 "${campaign}/v2/timing.py" run --slot narrow \
  --output "${FDB_PERF_HOME}/rfc-264-latency/v2/quiet-NEW" \
  --approved-quiet-slot 'OWNER-V2-SLOT-REFERENCE'
```

The v2 cap is 20 minutes including the gate, with 30 seconds reserved for cleanup. Both cells remain fixed at 12000 measured +800 warmup transactions per process. Every process has a unique 68-byte prefix: v1 owns `clientbench/rfc-264-latency/`, v2 owns the separate `clientbench/rfc-264-latency-v2/`. Only the owned range is cleared and verified empty, including after attempted-process failure. Timeouts/signals terminate only the owned process group before cleanup. No resume, adaptive sampling, dropped blocks or favorable reruns.

Process quantiles use empirical inverse-CDF `ceil(q*N)-1`, measured samples only, never pooled. Paired estimates are geometric means of within-block ratios. Ranges are **four-block min/max, not CIs**. No comparative summary is allowed without every process, raw validation, persistence/lifecycle/identity/GRV checks, unchanged pins and verified cleanup. Successful runs write `summary.json` automatically. A portable `summary` invocation checks its own exact plan, valid verdict and receipts; use the archived original scripts for historical raw-run reanalysis.

## Recorded raw-data manifest

```text
${FDB_PERF_HOME}/rfc-264-latency/SHA256SUMS
SHA256: f4c160f29ef5d943e57cbfc0b06d2d8c275c14104b2e35cc7c8e3683aa1dce4b
```

This covers **1262 regular artifact files**: original plans/scripts and amendments, both failed startups, successful v1 and v2 measurements, all twelve binary payloads, build receipts/logs, pinned CLI/cluster/harness input copies and the nested v2 manifest. Python bytecode and Bazel cache contents are excluded, as is the root manifest itself. Verification requires the unmodified archived subtree: moving its root is safe because entries are relative, but renaming internal paths to this template's layout is not. Fresh template outputs are not covered by the recorded manifest. Raw logs, samples, binaries and host/process identities are not committed here.

```sh
cd "${FDB_PERF_HOME}/rfc-264-latency"
printf '%s  %s\n' \
  f4c160f29ef5d943e57cbfc0b06d2d8c275c14104b2e35cc7c8e3683aa1dce4b \
  SHA256SUMS | sha256sum -c -
sha256sum -c SHA256SUMS
```
