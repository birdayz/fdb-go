# Native horizontal capacity experiment

This is a new, explicit-approval campaign. It does not change `ingest20k` or
relax that campaign's 100k storage-option authorization. Importing these modules
has no cloud effects. The normal repository `just test` lane runs two fake-only
Bazel targets: policy/runtime and provisioning. Their launchers reject zero tests,
skips, failed tests and syntax failures.

## Population and limits

Exactly five **new** dedicated `ccx43` HEL1 servers: three possible FDB hosts and
two client hosts, one isolated private network, and a spread placement group.
No existing machine is accessed. Every SSH/SCP command pins a generated host key;
remote helpers additionally check the exact new server ID using instance metadata.
Baseline IDs and names are protected. IPv4 ownership and `auto_delete=true` are
proved against the exact new server IDs before admission.

Native FDB 7.3.77 uses host ext4 with normal journaling, barriers, fsync and
checksums; no Docker/Btrfs configuration is changed. Each selected storage host
runs one 4GiB storage process with 2GiB cache. Five fixed control processes remain
on node1 (master/cluster-controller, commit proxy, GRV proxy, resolver, log), with
1GiB per control process and 64MiB caches. Configuration is exactly single R1,
Redwood storage, `ssd-2` log, one log/proxy/GRV proxy/resolver. This is a **non-HA
capacity experiment**, not a fault-tolerance result.

Each FDB host's aggregate slice and each client's slice has CPUs 0–3, CPU quota
400%, MemoryMax 16GiB and MemorySwapMax 0. Client GOMAXPROCS is 4. Runtime
admission/monitoring checks host memory/PSI/swap, disk floors and allocation,
cgroup limits/events, and unchanged native process PID/starttimes. Receipt files
are capped at 1GiB total on the controller. The initial storage free-space floor
is 200GiB + 6% of device size + 64GiB; the ongoing floor preserves 200GiB + 6%.
The benchmark's original transaction/save/estimate/actual-size/retry guards remain
in the sealed application, unmodified by these scripts.

## Fixed sequence and measurement

1. Quiet-admit all five hosts over seven samples, with absolute monotonic starts
   twelve seconds apart. Sampling overhead is included in that cadence; every
   host's observed intervals must still be between ten and fifteen seconds.
2. Run the two ×100k prerequisite on fresh storage-host layouts. The legacy
   default remains **1 → 2 → 3**. An approved plan may explicitly preregister
   `prerequisite_topologies: [3,2,1]` for a larger-first capacity study, so a
   failing small layout cannot prevent observations on the larger layouts.
   Only typed `[1,2,3]` or `[3,2,1]` is accepted; order never adapts to results.
   Both independent runs need strict health, exact record/index census and the
   complete 1600-class query oracle. Preserve every result before promotion.
   An approved `selected_topologies: [3,2]` selects an independent two-versus-three
   storage-host study; otherwise all three layouts remain selected. Both phase
   orders are filtered to that fixed selection. The one-host layout is then
   neither retested nor treated as a passing gate; prior failures remain failures.
3. Only after **all selected** prerequisites pass, run two ×`measured_count` on fresh
   clusters in the approved measured storage-host order. Only typed
   `measured_topologies: [3,2,1]` or `[2,3,1]` is accepted, filtered to the selection;
   ordering is never adaptive. The explicit two-host-first choice is a new
   independent study after both fresh selected prerequisite gates, not a waiver
   or replacement of a previously failed three-host measured wave.

`measured_count` is explicitly 100000 or 1000000. The 1M choice inserts **6.6M
rows total** for the legacy all-three study, or **4.4M** with the explicit `[3,2]`
selection, including prerequisites; both remain below the cumulative 10M ceiling.
An arm failure remains campaign-fatal; there are no health warning waivers or
speculative continuation exceptions. Every selected actual 100k pair must pass
before any measured phase begins.
Initial native recovery is bounded and every raw status is saved; admission needs
three consecutive strict passes. Health failures during load, audit or drain are
immediate failures. All four latency probes must be known and <5 seconds; warnings,
unknown shapes, changed roles/configuration, OOM, swap, disk or identity violations
never count as passes.

Two ordinary `otelbench` producers ingest **different Run namespaces**, not a
shared corpus or tenant. Each owns its complete generated population and ready
marker. Same-run concurrency is deliberately unsupported. Producer grants bind
run, count, source revision, binary SHA, exact storage options and a token. Both
ready receipts must bind to their grants before parallel releases. Aggregate
ACK/s is total ACK rows divided by one common controller monotonic interval from
before the first release until both SSH producer completions are observed. It
**includes release, producer startup, SSH completion and observation overhead**;
it is not a sum of client rates or exact pure-ACK timing. Audit/query runs happen
outside that ingest interval, with host/native monitoring throughout. Raw app
argv, stdout/stderr, exit receipts, evidence and native status remain local before
teardown.

Per-storage role IDs, native `input_bytes.counter`, optional
`durable_bytes.counter`, stored bytes and shard fields are captured
before/during/post-drain. Counter values must be nonnegative exact integers;
finite integral floats are accepted only within the safe-integer range. Missing
required input counters, unknown IDs, role changes, counter resets (including
post-drain), or <1MiB input/stored observations produce **INCONCLUSIVE**. If any
sample provides an optional durable counter, all its samples must be known and
monotone. Absent shard fields remain unknown, never inferred as zero. Sufficient counters establish only observed per-host data
processing: DD/background traffic is included. They are **not complete key
placement, run-specific mutation coverage, or proof extra storage helped**. Raw
ACK rates are retained; the scaling-proof field remains inconclusive.

Topology cost is explicitly labelled **SERVER-ONLY**: live captured HEL1 server
hourly net/gross prices and measured common ACK/s for logical active **3/4/5-node**
layouts (two clients plus selected FDB hosts), excluding IPv4 and traffic charges. All **five held servers are actually billable throughout**. The separate
rounded-server estimate uses API creation/delete-ACK receipts, plus a conservative
first-create-through-verified-cleanup bill bound. Neither is an invoice; IPv4 is
excluded. Currency/VAT provenance must be supplied from sealed pricing metadata,
not invented from the server-type response.

## Approval, source and teardown

The approved plan's required fields are enforced by `policy.validate_plan` and
`provision.validate_plan`: schema `otel_horizontal_v1`, approved true, unique
campaign/prefix, HEL1/ccx43, single replication, `topologies: [1,2,3]`,
`measured_topologies: [3,2,1]`, prerequisite count, explicit measured count,
batch 32/64, workers 2/4/8, GOMAXPROCS 4, exact storage profile, source revision,
absolute first/deadline epochs, one controller /32 or /128, absolute hcloud wrapper,
controller public/private keys, source root and binary/native/manifest paths with
SHA-256. Source manifest lines are `sha256  relative-path`; actual nonempty source
files are hashed before and after the campaign, not merely the manifest text.
Optional `prerequisite_topologies` preregisters `[1,2,3]` (the unchanged default)
or `[3,2,1]`; `topologies` stays `[1,2,3]`. Measured order explicitly accepts only
`[3,2,1]` or `[2,3,1]`.
Optional `selected_topologies` accepts only typed `[3,2]`, bound in the approved
JSON before data; omission preserves the all-three study. A larger-first selected
study still explicitly preregisters `prerequisite_topologies: [3,2,1]`, then filters
the prerequisite order to `[3,2]` and the measured order to `[3,2]` or `[2,3]`. It is not an adaptive retry or a waiver of the known
one-host failure. Authorized rows are `2 * selected_layouts * (100000 + measured_count)`.
A failed prerequisite is still campaign-fatal: accepted earlier gates remain
receipts, not authorization to skip a failed gate or enter the measured phase.
Optional `inventory_dir` binds to the root-sealed baseline; optional
`pricing_currency`, `pricing_vat_rate`, `pricing_metadata_source` identify currency
and tax provenance. Use a private output directory outside every Git worktree.

Local validation, no effects:

```sh
python3 layers/otel/bench/ops/horizontal/driver.py run --plan /private/approved.json
```

**Root must first arm and acknowledge a genuinely independent supervisor** as a
separately tracked foreground command (`functions.bash(background=true)`), without
`&`, `nohup`, `setsid`, detached sessions or hidden shell jobs. It invokes immediate
cleanup below at absolute `first_epoch+6600` or controller failure. Provision's
ordinary owned child is auxiliary only and is not an independent guard.

Only after root authorization, tests, immutable inputs and independent guard:

```sh
python3 layers/otel/bench/ops/horizontal/driver.py run \
  --plan /private/approved.json --output /private/new-campaign --execute-approved
```

All API/SSH/SCP active timeouts are clamped to **first+6000** (100 minutes), never
reset by stages. Guard cleanup begins at **first+6600** (110 minutes), leaving ten
minutes before the hard **first+7200** limit. Immediate finally cleanup runs on
success or failure. Cleanup deletes only exact new owned IDs/names/labels, never
baseline resources, then verifies exact baseline equality for all ten resource
kinds. A failed cleanup remains armed. No guard is cancelled on a partial proof.

Immediate cleanup entry point for the independent supervisor:

```sh
python3 layers/otel/bench/ops/horizontal/provision.py \
  --teardown-plan /private/approved.json \
  --teardown-out /private/new-campaign/provision
```

The teardown output argument must exactly match the `prepare(plan, OUT)` argument;
the driver uses `<campaign-output>/provision`. State currently resides one further
`provision` subdirectory beneath that argument. Successful first cleanup proof is
preserved for subsequent supervisor/controller checks.

## Two-host native diagnostic (not a capacity result)

`diagnostic.py` is a separate, explicitly approved `otel_native_diagnostic_v1`
protocol for exactly **one FDB host (`node1`) and one producer (`client1`)**. It
uses the same sealed application/native binaries, single-R1 default six-process layout,
4-CPU/16-GiB slices, host ext4/fsync/checksum checks, transaction/save/retry guards,
strict health rules, independent teardown supervisor and absolute lifetime limits.
It does not change the five-node horizontal profile or adopt any existing host.
It cannot be compared with the two-producer throughput study.

Use the common plan's asset, source, hardware, storage, CIDR, pricing, inventory
and lifetime fields, but set `schema: otel_native_diagnostic_v1`, typed
`count: 100000`, `probe_count: 1000000`, and `probe_seconds: 120`. Comparative fields
(`topologies`, `selected_topologies`, either phase-order field and
`measured_count`) are forbidden. The total configured insertion upper bound is
**1.1M rows**. The 100k run must pass strict health, its full `11N+2` census and all
1600 oracle checks before the distinct-Run 1M probe can start. The probe's ingest
is bounded to 120 seconds; it may stop short. Empty/incomplete ingest JSON leaves
the partial ACK count unknown. Diagnostic summaries never publish an accepted
ACK-rate or capacity result, even if all ingestion and verification complete.

The optional diagnostic-only `storage_max_outstanding` is an exact integer **64
or 128**, default **64**. Omitted/64 adds no argument and preserves the original
native unit/argv bytes. At 128, only storage-4510 receives
`--knob_max_outstanding 128`; the five control/log processes stay at default 64.
The effective value is recorded in the layout and diagnostic result, bound to the
approved-plan receipt hash; the sampler verifies the full native cmdline. This is
a **combined storage-process Flow/Redwood I/O-pipeline** perturbation, not a pure
Redwood knob or evidence of physical-disk saturation. There is no
`REDWOOD_IO_CONCURRENCY` knob and no arbitrary knob dictionary.

Pinned FDB 7.3.77 source `3ea44ce1d9003ad095e408039e1f755c319c4dfb` defines
`MAX_OUTSTANDING=64` in `flow/Knobs.cpp:181`, uses it for Redwood's pager I/O lock
in `fdbserver/VersionedBTree.actor.cpp:2020` and for KAIO context/submission/
completion limits in `fdbrpc/include/fdbrpc/AsyncFileKAIO.actor.h`. The argv spelling
is accepted by `fdbserver.actor.cpp:1162,1214–1219` and `flow/include/flow/ArgParseUtil.h`'s
hyphen/underscore normalization. `REDWOOD_IO_PRIORITIES` values are weights, not
capacity. Every hardware, memory/cache, CPU, durability, transaction, row, health
and absolute-time guard remains unchanged; a fresh full 100k gate is still
required before the 120-second probe, and the earlier 64 failure stays failed.

Optional diagnostic-only `storage_process_count` is an exact integer **1 or 2**,
with default **1**. Only `(count, max_outstanding)` profiles **(1,64), (1,128),
(2,64)** are allowed. Two storage processes use ports4510/4511 on `node1`, each
with **2GiB memory/1GiB cache**: aggregate storage remains **4GiB/2GiB**, controls
remain five unchanged 1GiB processes, total configured FDB memory remains9GiB,
and role CPU/memory limits stay **4CPU/16GiB**. Default one-process native argv
and unit bytes remain unchanged. Two default64 processes provide128 aggregate
I/O slots while adding actor parallelism, but also change cache partitioning,
pagers and DD scheduling; this is not a pure actor-versus-I/O causal test.
Diagnostic status preserves both exact SS addresses/IDs and counters, with
explicit all-process host sums (unknown counters never become zero). Both SS
identities must remain stable with positive input activity; an idle/missing/
replaced process is **not exercised / INCONCLUSIVE**, not a two-actor result.

Before producer release, both owned nodes start a one-second sampler inside their
existing fixed role slice and supply two baseline frames. Frames bind the exact
node, run, grant hash, token, boot ID and monotonic clock. They capture cgroup
`cpu.stat` (including throttling) / `io.stat`, fixed resource limits, exact FDB
PID/start/cmdline plus per-process I/O and per-thread CPU/wchan, filesystem and
parent-device diskstats including available flush fields, and host CPU/memory/I/O
pressure. The sampler's observational overhead is included in these slices;
these are not old-profile performance measurements. Missing/changed identities,
counter resets or incomplete required data produce **INCONCLUSIVE**. A usable
capture also needs at least ten load frames spanning nine seconds, verified
producer ready/release and actual PID/start receipt, and positive native input
activity. Idle frames alone are not evidence of a CPU constraint. `CAPTURED` is a
data-coverage label, not a health pass or automatic CPU-versus-I/O verdict.

Frames are incrementally copied during load, not first requested after an abort.
`finally` aborts the producer, stops/reaps samplers and attempts bounded collection
before owned-resource deletion; it never postpones an absolute cleanup deadline
to fill a profile. The exact arm's regular, non-symlink storage-4510/log-4504 trace
tails (plus storage-4511 for two SS) share a **16-MiB aggregate** budget across the run. File/address/log-group
mapping is backed by verified native PID/start/cmdline; trace `ThreadID` is not a
Linux TID. All NDJSON event types are retained, including Redwood, KAIO sync and
Net2 run-loop records. Missing/zero-count histograms remain unavailable evidence.
Native input-minus-durable is MVCC memory accounting, not physical disk bytes;
KAIO sync latency can include EIO/actor scheduling, not only kernel fdatasync.

The remote harness contains only `diagnostic.py`, `driver.py` and `policy.py`;
provider tooling is imported lazily by the controller execution branch only.
Validation has no effects:

```sh
python3 -B layers/otel/bench/ops/horizontal/diagnostic.py run --plan /private/approved.json
```

After explicit approval, frozen tests/assets and independently acknowledged guard:

```sh
python3 -B layers/otel/bench/ops/horizontal/diagnostic.py run \
  --plan /private/approved.json --output /private/new-diagnostic --execute-approved
```

The existing immediate teardown CLI applies, with `--teardown-out` equal to
`<new-diagnostic>/provision`; all ten resource kinds must exactly match this
campaign's protected baseline. Post-run source verification runs even when
cleanup fails or raises; it does not turn a failed inventory gate into a pass.
No profiling permission, package installation,
perf invocation, sysctl, native knob or durability-setting change is implicit.

## Bounded two-host session

`session.py` uses explicit `schema: otel_native_session_v1` to prepare exactly two
new owned VMs **once**, not once per arm. The immutable approved lease retains its
original `first_epoch`/`deadline_epoch`, independent watchdog, protected inventory,
source/asset seals and all existing hardware, CPU4/memory16GiB, disk/fsync/checksum,
transaction, size/retry, health and cleanup guards. It is not an interactive pool:
both cases and helper bytes are preregistered and frozen before any resource starts.

The session requires typed `count:100000`, `probe_count:1000000`,
`probe_seconds:120`, `storage_process_count:2`, `storage_max_outstanding:64`, exact
ordered `cases:["capacity16g","baseline"]`, and nonnegative integer
`prior_rows_upper`. Its configured upper bound is2.2M rows; prior plus session must
not exceed10M (4.8M prior gives7M). Case objects, arbitrary arguments and per-case
clock overrides are not supported. `storage_memory_profile` belongs to each
composed diagnostic arm, not the top-level session plan:

- `capacity16g`: two SS, each4GiB memory/2GiB cache; configured native memory is
  8GiB storage plus5GiB controls =13GiB within the unchanged16GiB slice.
- `baseline`: two SS, each2GiB memory/1GiB cache; configured native memory is9GiB.

This memory/cache profile does **not** raise `STORAGE_HARD_LIMIT_BYTES`, remove the
independent storage-queue e-brake, or relax health limits. Both arms have two
reactors and default64 I/O slots per SS; this is not a parallelism-isolating test.
Every SS ID must remain stable, with positive input **and durable** counter deltas
for `CAPTURED`; idle/unknown/reset evidence remains INCONCLUSIVE.

Each case uses a fresh FDB arm/datadir and its own complete100k strict-health,
11N+2 census/1600-oracle gate before its own1M/120s probe. Case runs are9800/9801
then9802/9803. Old data and local receipts are retained. Between cases only exact
owned native units/application scopes are stopped, with PID/start/cmdline/cgroup
binding and quiescence proof; a fresh quiet admission precedes the next cluster.
The two cases share the session-wide1GiB receipt ceiling and16MiB native-trace
budget, and retain host/swap/resource observation history without resetting time.

A failed case never becomes PASS or promotes its unpassed gate. Only the exact
known batch-priority probe warning with storage-queue QoS, verified identities,
released producer binding and freshly passing independent fatal guards is an
`ARM_FAILURE`: collect, stop and prove the old cluster quiescent, then try the
other fresh case. Every host/resource/OOM/swap/disk/source/identity/deadline or
unknown-shape fault is `SESSION_FATAL` and immediately enters final cloud cleanup.
No workload continues on an unhealthy old cluster. Final status separately lists
failed cases and all-cases outcome; neither is an accepted throughput result.
Final cleanup always runs, including unexpected controller failure via the
independent watchdog. Failed global inventory gates stay failed; post-run source
verification still runs if cleanup fails or raises.

Validation has no provisioning effects:

```sh
python3 -B layers/otel/bench/ops/horizontal/session.py run --plan /private/session.json
```

After clean hooked validation, immutable inputs and independent guard readiness:

```sh
python3 -B layers/otel/bench/ops/horizontal/session.py run \
  --plan /private/session.json --output /private/campaign/run --execute-approved
```

Output is used directly: prepare/teardown-out is `/private/campaign/run/provision`,
state is `/private/campaign/run/provision/provision`, and case receipts are
`/private/campaign/run/cases/<profile>`. There is no extra automatic `run` level.
Standalone `diagnostic.py` retains its ordinary prepare/final-teardown lifecycle.

### Fatal-process forensic evidence

Before producer release, each sampler seals its exact live PID/start/full argv,
boot identity, owned unit InvocationID/cgroup, and each target trace filename
actually open by that PID. File seals include device/inode, minimum size and a
bounded prefix hash; SSIDs are bound from the preceding strict status. The ready
receipt includes the immutable identity-file SHA; the controller retains
`<role>-native-identity.json` and verifies that SHA before release. Frames append
`host_page_size` from actual `SC_PAGESIZE`; RSS pages must not assume4KiB.

`native-traces.json` now also holds exact-unit exit/failure properties and bounded
journal JSON. Only the sealed owned unit/arm and original sealed time range may
be queried; journal ownership/invocation/time and trace Machine/LogGroup/SSID
checks remain fail-closed. No core, dump, profiler, shell-embedded trace command,
restart or health-recovery action is added. All unit/property/journal/prefix/raw
trace bytes, including startup seals, count against the existing shared16MiB
native-evidence allowance; the session-wide1GiB receipt ceiling is unchanged.

If a native PID has died, read-only collection can retain **only previously
sealed filenames**, never discover post-death siblings by address/prefix. Device,
inode, prefix, path and grant bindings must still match. New rotations require
another positive live identity/open-FD check. Reused PIDs, changed commands,
foreign paths, symlinks and SSID mismatches refuse raw tails. Missing live identity
is explicit `DEAD_OR_MISSING`, `forensic_only:true`, `coverage:INCONCLUSIVE`;
collected bytes do not restore a health PASS or authorize `ARM_FAILURE` reuse.
Unknown/truncated unit or journal facts remain unknown, not zero/success. Exit20
can support native out-of-memory termination, but does not uniquely identify its
RSS monitor; SIGKILL alone is not kernel-OOM proof. Old failed results and unknown
causes remain unchanged. This postmortem path is offline-tested, runtime-unverified
until a separately authorized bounded capture exercises it.

### Controlled independent-namespace weak scaling

`scaling.py run --plan /private/scaling-plan.json` performs local-only validation.
Add `--output /private/scaling/run --execute-approved` only after sealing the
approved inputs, the immutable two-hour lease and an independent cleanup watchdog.
The output path is used directly; there is no automatic extra `run` directory.
The schema is `otel_native_scaling_v1`, with the usual sealed hardware, source,
binary, controller and asset fields plus these exact typed fields:

```json
{
  "stages": [[1, 1], [2, 2], [3, 3]],
  "count": 100000,
  "measured_count": 375000,
  "measured_seconds": 120,
  "prior_rows_upper": 7000000,
  "batch": 64,
  "workers": 8,
  "gomaxprocs": 4,
  "storage_process_count": 3,
  "storage_max_outstanding": 64,
  "storage_memory_profile": "scaling3ss3g"
}
```

Prepare exactly `node1..3` and `client1..3` once and retain all six through all
stages. Stage N activates the first N FDB hosts and first N independent producers
in one shared single-replica cluster. Each stage has a fresh cluster/datadir;
every producer has distinct prerequisite and measured namespaces. Its own full
100k health/census/1600-oracle gate precedes its 375k measured wave. The maximum
new population is `(1+2+3)*(100000+375000)=2850000`; a 7M prior upper bound gives
9.85M cumulative, with no failed-row reclamation or increased ceiling.

The scaling-only geometry is three SS per active FDB host, ports4510..4512,
each3GiB memory/1GiB cache/MAX64. Node1 retains the five1GiB control processes
(total configured14GiB); other active FDB hosts configure9GiB. Every role host
still uses CPU4/memory16GiB/swap0; every producer uses GOMAXPROCS4. This exercises
three native storage main threads under fixed4CPU, not pure worker isolation or
proven memory safety. Existing horizontal/diagnostic/session profiles and native
argv are unchanged. Application/index/conflict/durability/timeout/retry/byte guards
remain sealed and unchanged; complete committed rows and nonzero finite source
phase histograms are required before rate calculation.

All producers must be ready before the common release. ACK rate uses release to
last controller-observed complete ACK; the **primary durable-drained rate** uses
the same start through three strict stable drain observations (queue<16MiB,
lag<10s). Census and oracle verification must then pass for every producer before
any comparison is eligible. Queue/lag start/end/observed maxima and drain seconds
are retained. Efficiency is drained rate divided by N times the passing1x baseline;
near-linear requires all three eligible stages and efficiency>=0.90. ACK scaling
is reported separately. A missing/failed/inconclusive baseline permits no scaling
inference. This is one preregistered weak-scaling sample, not shared-corpus
linearity, statistical repeatability, HA, or a sustained120s20k certificate.
Active2N host resource counts are separate from all six held/billed hosts; raw
pricing, creation and deletion receipts remain in the provision ledger.

Only the canonical receipt-bound load-phase batch-probe/storage-queue failure,
with freshly passing fatal guards and complete live evidence, may end one stage
and proceed. Every other fault is SESSION_FATAL. Exact owned unit/scope/PID
quiescence and fresh quiet admission separate stages without VM deletion; prior
unit identity is rechecked before the next setup. Final cleanup and source checks
always run, including cleanup failure. Host history and the original lease never
reset. The shared16MiB native evidence budget reserves `remaining/remaining_waves`
for each of six expected waves and splits that reservation fairly among active
FDB hosts; unused bytes carry forward. Startup seals and every unit/journal/
prefix/tail byte are charged. The shared1GiB receipt cap is unchanged.

Receipts are rooted at `scaling-result.json`, with stage records under
`stages/pairsN/{stage-result,session-stage,quiescence}.json`; wave evidence is under
`stages/pairsN/{prerequisite,measured}/`. Each wave retains per-client producer
plans/ingest/audit/query receipts, common `release.json`, frames and per-role
pre-release native identity seals. Native captures are
`nodeN-native-traces.json` (`otel_native_forensic_evidence_v1` with
`otel_native_unit_forensics_v1`). Profiles require independent stable IDs and
positive input/durable activity for all3/6/9 SS; unknown/reset/idle counters remain
INCONCLUSIVE. No dead PID, truncated evidence or diagnostic capture restores health
or authorizes reuse. The candidate remains **runtime UNVERIFIED** until a separate
approved lease actually exercises these stages; prior failures remain unchanged.
