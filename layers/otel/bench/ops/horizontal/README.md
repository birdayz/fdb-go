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
3. Only after **all three** prerequisites pass, run two ×`measured_count` on fresh
   clusters in preregistered **3 → 2 → 1** storage-host order. The approved plan
   must contain typed `measured_topologies: [3,2,1]`; ordering is never adaptive.
   This order was fixed before any campaign result so a known small-layout 1M
   health failure cannot prevent measurements on the larger layouts.

`measured_count` is explicitly 100000 or 1000000. The 1M choice inserts **6.6M
rows total**, including prerequisites, below the cumulative 10M ceiling. An arm
failure remains campaign-fatal; there are no health warning waivers or speculative
continuation exceptions. All three actual 100k layouts precede the 1M phase.
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

Per-storage role IDs, native input-byte counters, stored bytes and shard fields
are captured before/during/post-drain. Missing values, unknown IDs, role changes,
counter resets (including post-drain), or <1MiB input/stored observations produce
**INCONCLUSIVE**. Sufficient counters establish only observed per-host data
processing: DD/background traffic is included. They are **not complete key
placement, run-specific mutation coverage, or proof extra storage helped**. Raw
ACK rates are retained; the scaling-proof field remains inconclusive.

Topology cost uses live captured HEL1 hourly net/gross prices and measured common
ACK/s for logical active **3/4/5-node** layouts (two clients plus selected FDB
hosts). All **five held servers are actually billable throughout**. The separate
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
or `[3,2,1]`; `topologies` stays `[1,2,3]` and measured order stays `[3,2,1]`.
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
