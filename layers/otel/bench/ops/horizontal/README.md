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
