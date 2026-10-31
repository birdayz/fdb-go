# OTel span ingest at 25k spans/s: 1x/2x/3x scaling on Kubernetes + Cilium

Campaign `otel-cilium4-1793415401` (run name `k8s-cilium4`), 2026-10-31,
Hetzner Cloud `hel1`. Application binary built from source revision
`4af32c3bc` ("Add guarded bulk saves and explicit Record Layer Brotli
compression"), binary SHA-256 `3ed9c25a…501fe`, `fdbserver`/`fdbcli` FoundationDB
7.3.77.

This is a **single, preregistered weak-scaling run**. Each stage has its own
FoundationDB cluster with `single` redundancy. Every producer writes to its own
independent Record Layer store. The harness writes its own boundary into the
result: *"independent namespaces in one single-replica FDB cluster per stage;
fixed six billed hosts, active pairs scale 1/2/3; no shared-corpus, HA,
statistical repeatability or sustained-120-second 20k certification"*. Read every
number below inside that boundary. [Limitations](#5-limitations-and-discrepancies)
lists what the run does not prove.

## 1. Executive summary and headline results

At 3x scale (3 producer hosts writing to 3 FoundationDB hosts with 18 Redwood
storage servers), **1,125,000 spans were committed at 25,387.9 ACKed spans/s and
made durable on storage at 21,697.6 drained spans/s**. Both rates exceed the
20k spans/s target. The harness's primary metric is the drained rate, and its
verdict is `NEAR_LINEAR` (threshold: ≥ 90% efficiency at every stage):

| Stage | Measured spans | ACK spans/s | Drained spans/s | Drained scale factor | Scaling efficiency (drained) | ACK scale factor | ACK efficiency |
|---|---:|---:|---:|---:|---:|---:|---:|
| 1x | 375,000 | 9,456.2 | 6,677.4 | 1.000 | 100.0% | 1.000 | 100.0% |
| 2x | 750,000 | 16,972.8 | 14,585.7 | 2.184 | **109.2%** | 1.795 | 89.7% |
| 3x | 1,125,000 | **25,387.9** | **21,697.6** | 3.249 | **108.3%** | 2.685 | 89.5% |

Efficiency is `(rate_N / rate_1x) / N`. The harness's `summarize_scaling`
([`scaling.py`](../../layers/otel/bench/ops/horizontal/scaling.py)) computes it
from the drained rate, and the ACK factor is reported next to it. The drained
efficiency is above 100% because the 1x stage had a long durability tail: 16.5 s
from last ACK to stable drain, against 7.2 s at 2x and 7.5 s at 3x. That tail
lowers the 1x drained baseline. Measured on ACK rate, scaling is just under
linear (89.7% and 89.5%). The run is evidence of near-linear scaling. It does
not show a superlinear effect.

### What every span costs

Each ACK is a committed FoundationDB transaction. Nothing is buffered on the
client or written asynchronously:

- **11 KV entries per span, written in the same transaction**, at most 64 spans
  per transaction (8,334 transactions for 375,000 spans, about 45 spans each):
  - 1 primary record: the span protobuf, compressed by the Record Layer's
    transformed serializer with **zlib level 1** (`--record-compression zlib-1`).
    About 392 B of value and 95 B of key per span.
  - 2 native Record Layer VALUE indexes: `span_id_v2` and `span_time_v2`
    ([`intrinsic.go`](../../layers/otel/intrinsic.go)).
  - 8 covering custom secondary postings, one per attribute
    ([`index.go`](../../layers/otel/index.go)). The key is
    `("eq_v1", tenant, scope, sha256(key)[:8], type, sha256(value)[:8], -start, ^trace, span_id, ordinal)`.
    The value is the compact 5-element FDB tuple `(scope, key, type, value, start)`
    (`--posting-value-format tuple`, `--hash-bytes 8`). Each query decodes the
    covering value and drops any candidate whose full attribute differs from the
    predicate, so an 8-byte digest collision cannot return a wrong result.
- **Old images are read authoritatively.** `PrefetchRecordBatch` starts one wave
  of snapshot reads that fills only this transaction's read-your-writes cache,
  overlapping the store-open reads. `SaveRecordBatchWithOptions` then loads and
  validates every old image through the normal Record Layer path, which adds
  the standard conflicts. `CoalesceIndexWriteConflicts` comes from
  `--bulk-index-conflicts=true`.
  Code: [`ingest.go`](../../layers/otel/ingest.go) (`prefetchIngestRecords`,
  `saveIngestBatchRun`) and
  [`store_batch_prefetch.go`](../../pkg/recordlayer/store_batch_prefetch.go).
- **Transaction guards** ([`ingest.go`](../../layers/otel/ingest.go),
  [`fdb.go`](../../layers/otel/fdb.go)):
  - 1,500 ms FDB transaction timeout and a retry limit of 10.
  - A conservative 300 KB admission estimate per batch.
  - An approximate-size guard of 500 KB, measured with `GetApproximateSize`
    after the save.
  - A 1 s elapsed limit covering prefetch, save and guard.
  - An authoritative read of the corpus-owner manifest in every transaction.
- **Native durability.** Before any work, provisioning checked every host's data
  filesystem: ext4, no `nobarrier`, `data=writeback`, `noload` or `ro` mount
  options. The provisioning receipt records `fsync_preserved: true` and
  `checksums_preserved: true` (`provision.py`). Redwood's page checksums are
  enabled by default.
- **Complete census.** After drain, `audit` mode re-reads every record and
  compares it field by field with the deterministic generator. It counts every
  owned key. Each 375k producer owns exactly **4,125,002 keys = 11 × 375,000 + 2**
  (the 2 are store-level metadata keys):

  | Structure | Keys | Key bytes | Value bytes |
  |---|---:|---:|---:|
  | records | 375,000 | 35,625,000 | 147,186,751 |
  | `span_id_v2` | 375,000 | 43,125,000 | 0 |
  | `span_time_v2` | 375,000 | 43,875,000 | 0 |
  | covering postings (`eq_secondary`) | 3,000,000 | 498,023,159 | 175,709,586 |
  | **owned total** | **4,125,002** | 620,648,238 | 322,896,592 |

  At 3x that is **12,375,006 keys**. Every producer in every wave passed the
  census.
- **Oracle-checked queries.** `query` mode ran **1,600 queries per producer**
  (16 classes × 100: trace, span, time-range and GenAI attribute lookups,
  present and absent). Results were compared with the deterministic oracle, and
  every producer in every wave recorded 0 failures.

At 3x the cluster acknowledged about 279k KV sets/s. That is about 64 MB/s of
logical KV payload (2,516 B per span), or 173 MB/s of client-side
approximate transaction size (6,831 B per span).

### Complete stage table

Prerequisite waves write 100k spans per producer into a **cold** key range.
Measured waves write 375k spans per producer into a range that was
**pre-split** first (see [Bottleneck 2](#bottleneck-2-cold-key-range-shard-hotspot-and-pre-split-range-conditioning)).
The harness computes scale factor and efficiency for measured waves only.
Prerequisite factors were computed for this report from the same JSON, relative
to the 1x prerequisite.

| Stage | Wave | Spans | ACK spans/s | Drained spans/s | Scale factor (drained) | Efficiency | Max queue (MB) | End queue (KB) | Max lag (s) | Drain time (s) | Transient unhealthy samples |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1x | prerequisite | 100,000 | 7,486.9 | 3,217.7 | 1.000 | 100.0% | 919.5 | 1,073.2 | 18.10 | 17.72 | 0 |
| 1x | **measured** | 375,000 | 9,456.2 | 6,677.4 | 1.000 | 100.0% | 372.5 | 24.6 | 13.87 | 16.50 | 0 |
| 2x | prerequisite | 200,000 | 7,270.6 | 3,264.5 | 1.015 | 50.7% | 1,533.5 | 368.7 | 35.36 | 33.76 | 0 |
| 2x | **measured** | 750,000 | 16,972.8 | 14,585.7 | 2.184 | 109.2% | 385.0 | 354.8 | 14.97 | 7.23 | 0 |
| 3x | prerequisite | 300,000 | 9,970.1 | 4,786.7 | 1.488 | 49.6% | 1,536.4 | 46.0 | 34.91 | 32.58 | 0 |
| 3x | **measured** | 1,125,000 | 25,387.9 | 21,697.6 | 3.249 | 108.3% | 328.7 | 675.9 | 13.09 | 7.54 | 0 |

MB and KB are decimal (10⁶ and 10³ bytes). Queue is the worst storage-server
queue reported by `status json` (`qos.worst_queue_bytes_storage_server`). Lag is
the worst storage durability lag. The end lag is about 5 s in every wave, which
is FoundationDB's normal MVCC durability window. All 6 waves had **0 transient
unhealthy samples**: every `status json` sample during load and drain reported
a healthy database. All 6 waves passed health, ingest validation, census and
oracle.

Timing boundary, copied from the result: *"controller common release through
last complete ACK and through final three-sample strict stable drain; includes
SSH/observation overhead"*. Every producer waits at a file barrier. The
controller releases all producers together and starts the clock at release.
**ACK end** is when the last producer process exits after its final commit.
**Drained end** is the first of three consecutive 1 s samples that are healthy,
with storage queue < 16 MiB and lag < 10 s. Measured ACK windows were 39.7 s,
44.2 s and 44.3 s, and drained windows 56.2 s, 51.4 s and 51.9 s.

Client-side transaction health in the measured waves: **0 retries, 0
not-committed and 0 too-old transactions for every producer**. Commit latency p50
was 8.0–12.5 ms and p99 22.5–39.8 ms. Whole-transaction latency p99 was
75–98 ms. The three 3x producers finished at different times (process
wall-clock 43.4 s, 29.3 s and 39.3 s). The aggregate rate is therefore set by
the slowest producer.

## 2. System architecture and stack (`k8s-cilium4`)

### Hardware

| Role | Count | Instance | Notes |
|---|---:|---|---|
| FoundationDB hosts | 3 (`node1`..`node3`) | Hetzner `ccx43` | 16 dedicated vCPUs, 64 GiB RAM, 360 GB local disk (`storage_type: local`), ext4 |
| Load producers | 3 (`client1`..`client3`) | Hetzner `ccx43` | same |

All six hosts are in `hel1` on a fresh private network `10.99.0.0/24`. The
`ccx43` vCPUs are AMD EPYC: an earlier campaign on the same server type verified
EPYC Milan topology, see
[hcloud-multinode-vpc-20261014](hcloud-multinode-vpc-20261014/README.md). This
run did not record CPU model or private-network bandwidth. All six hosts were
billed for every stage. A stage at scale N uses `node1..N` and `client1..N`.
The FDB nodes outside the stage are unlabelled, so no FDB pods are scheduled on
them.

### Kubernetes and CNI

- **k3s `v1.31.5+k3s1`**, single master on `node1`, started with
  `--flannel-backend=none --disable-network-policy --disable=traefik,servicelb`.
  All five other hosts joined as agents, including the three producers.
- **Cilium** (cilium-cli `v0.20.2`) in native-routing mode:
  `ipam.mode=kubernetes`, `routingMode=native`,
  `ipv4NativeRoutingCIDR=10.42.0.0/16`, `autoDirectNodeRoutes=false`,
  `bpf.masquerade=true`, `operator.replicas=1`.
- **Hetzner VPC native L3 routes.** For each node's `/24` PodCIDR the runner ran
  `hcloud network add-route <net> --destination <podCIDR> --gateway <node 10.99.0.x>`:

  | PodCIDR | Gateway | Node |
  |---|---|---|
  | 10.42.0.0/24 | 10.99.0.11 | node1 |
  | 10.42.6.0/24 | 10.99.0.12 | node2 |
  | 10.42.5.0/24 | 10.99.0.13 | node3 |
  | 10.42.3.0/24 | 10.99.0.21 | client1 |
  | 10.42.1.0/24 | 10.99.0.22 | client2 |
  | 10.42.4.0/24 | 10.99.0.23 | client3 |

  Every host then ran `ip route replace 10.42.0.0/16 via 10.99.0.1`, so pod
  traffic leaves through the VPC gateway and is routed to the owning node.
  There is no VXLAN or other overlay encapsulation.
- Producers run **directly on the client hosts**, not in pods. They reach FDB
  pods by pod IP over these routes. The runner rewrites the operator's
  connection string from pod DNS names to pod IPs.

### Operator and FDB topology

Apple's **fdb-kubernetes-operator `v2.38.0`** (CRDs and sample deployment at that
tag, monitor image pinned to 7.3.77, controller pinned to `node1`) manages one
`FoundationDBCluster` per stage, named `otel-fdb-s{N}`. Before each stage the
runner deletes the previous cluster and its PVCs. Images were pre-pulled.

Settings shared by every stage:

- FDB `7.3.77` with `imageType: split`
- `storageServersPerPod: 6`
- `redundancy_mode: single`
- `storage_engine: ssd-redwood-1`
- `faultDomain: kubernetes.io/hostname`
- `trace_format=json`
- `useExplicitListenAddress: true`

Roles scale with the active FDB host count N:

| Role | Per stage | 1x | 2x | 3x | Process args | Pod limits |
|---|---|---:|---:|---:|---|---|
| storage pods (6 Redwood SS each) | N | 1 (6 SS) | 2 (12 SS) | 3 (**18 SS**) | `--memory=4GiB --cache_memory=2GiB` | 16 CPU / 32 Gi, 64 G PVC |
| logs (TLog pods) | 2N | 2 | 4 | 6 | `--memory=4GiB --cache_memory=128MiB` | 4 CPU / 6 Gi, 32 G PVC |
| commit proxies | 2N | 2 | 4 | 6 | (stateless) | 4 CPU / 4 Gi |
| GRV proxies | N | 1 | 2 | 3 | (stateless) | 4 CPU / 4 Gi |
| resolvers | N | 1 | 2 | 3 | (stateless) | 4 CPU / 4 Gi |
| stateless pods | 4N+1 | 5 | 9 | 13 | | 4 CPU / 4 Gi, 16 G PVC |
| cluster controller | 1 | 1 | 1 | 1 | | 4 CPU / 4 Gi |

Operator-reported placement at 3x: one storage pod and two log pods on each FDB
node, with stateless pods spread 4/5/4. In total there were 23 process groups,
and the cluster was `available`, `healthy` and `fullReplication` at
reconciliation. The PVCs set no StorageClass, so k3s's default local-path
provisioner backed them on each node's ext4 root disk.

### Record Layer ingest pipeline

Each producer ran:

```
env GOMAXPROCS=16 otel-demo --backend fdb --order time --run <run> --seed 283 \
  --count 375000 --tenants 1 --attrs 8 --match-every 100 \
  --batch 64 --save-mode batch --workers 8 --diagnostics \
  --record-compression zlib-1 --posting-value-format tuple \
  --bulk-index-conflicts=true --hash-bytes 8 --mode ingest --timeout 120s
```

`audit` and `query --suite all --iterations 1600` ran with the same identity
after drain. Workers prepare bounded, disjoint batches outside the retry
closure. Each batch is one `db.Run` transaction with these steps:

1. Read the owner manifest.
2. `PrefetchRecordBatch` (single read wave).
3. `CreateOrOpen` the store.
4. `SaveRecordBatchWithOptions`.
5. Apply the size and time guard.
6. Commit.

Run names `prof20k93xy` come from `scaling.run_name`. Each name selects an
independent store subspace under `otel_otelbench_v2`.

## 3. Chronological root-cause engineering and bottlenecks solved

The run above is the end of a sequence of runs on the same six-host shape. The
runs below are listed in order, with their retained receipts under
`/var/tmp/fdb-perf-scaling.v4O6xf/`.

| Run | Infrastructure | 1x measured ACK / drained | 2x measured ACK / drained | 3x measured ACK / drained | Outcome |
|---|---|---|---|---|---|
| native `otel-perf-scaling` | systemd units, 3 SS/host, TLog `--memory 1GiB`, client 4-CPU cgroup | 6,278 / 3,756 | — | — | 2x measured killed by TLog exit 20 (B1) |
| native `log2g` follow-up | as above, TLog 2 GiB | — | — | — | 2x measured `ARM_FAILURE_STRICT_HEALTH_STORAGE_QUEUE` (B2) |
| `k8s-fdb`..`k8s-fdb6` | k3s + flannel, operator bring-up | — | — | — | harness/operator integration failures, no waves |
| `k8s-fdb7` | flannel, no pre-split | 9,186 / 4,826 | — | — | 2x measured producer exit 1 |
| `k8s-fdb8` | flannel, partial pre-split | 10,696 / 5,240 | 10,389 / 6,483 (5 unhealthy) | — | 3x verification failure |
| `k8s-fdb9` | flannel, full 375k pre-split | 11,391 / 5,925 | 15,083 / 8,098 (1 unhealthy) | 20,093 / 12,651 | all pass, **`BELOW_NEAR_LINEAR`** (68.3%, 71.2%) |
| `k8s-cilium1` | Cilium native routing, `--workers 16` | — | — | — | producer exited with status 2 at 1x prerequisite |
| `k8s-cilium2` | Cilium, final topology | 10,327 / 7,209 | 20,684 / 14,061 | — | 3x prerequisite: status probe reported unavailable, run fatal |
| `k8s-cilium3` | — | — | — | — | stopped after the inventory snapshot, no hosts |
| **`k8s-cilium4`** | Cilium, final topology | 9,456 / 6,677 | 16,973 / 14,586 | **25,388 / 21,698** | all pass, **`NEAR_LINEAR`** (109.2%, 108.3%) |

### Bottleneck 1: TLog (port 4504) 1 GiB resident-memory monitor, exit code 20

In the native (non-Kubernetes) campaign `otel-perf-scaling-1793353766`, the 2x
measured wave failed. On `node1` the transaction-class process at
`10.97.0.11:4504` (role `TL`, PID 15874, started with `--memory 1GiB
--cache-memory 64MiB`) exited at 2026-10-30T10:19:09Z. Evidence from the
retained receipts:

- **Native trace:** `OutOfMemory` (Severity 40) with
  `Backtrace: addr2line -e fdbserver.debug -p -C -f -i 0x569c9dd 0x569cca3 0x5696eb4 0x5660ad5 0x565c776 0x56832ec 0x561ec28 0x3345189 …`.
- **Journal**, same PID and the same systemd invocation
  `cfc9d461158e454a916ad7b9c50e584c`: `ERROR: Out of memory`, then
  `Main process exited, code=exited, status=20/n/a`.
- **Kernel cgroup counters:** `oom 0`, `oom_kill 0`. This was **not** a kernel
  OOM kill.
- **RSS timeline** (`/proc` frames and `ProcessMetrics`): resident memory grew
  from 424 MB through 651 MB and 871 MB to about 1,009 MB in the last samples
  before the 1 GiB (1,073,741,824 B) limit. Over the same period the TLog's
  `SharedBytesInput` rose from 1.33 GB to 1.85 GB while `SharedBytesDurable`
  stayed near 0.9 GB.
- **Symbolization.** The upstream `fdbserver.debug.x86_64.gz` for 7.3.77 has
  ELF BuildID `90a98d0d3315bc2281da8402f64cacc08dc0b37b`, which matches the
  sealed server binary. A positive control resolved
  `_Z13updateStorageRKP13StorageServer` to `storageserver.actor.cpp:11691`. The
  decisive frame is
  `0x56832ec → RecurringActorState<startMemoryUsageMonitor(unsigned long)::$_0>`,
  which calls `platform::outOfMemory()` and then `criticalError`.
- **Source** (FoundationDB tag 7.3.77, `3ea44ce1d`):
  - `flow/SystemMonitor.cpp:462-474`: `startMemoryUsageMonitor` checks
    `getResidentMemoryUsage() > memLimit` on a timer and calls
    `platform::outOfMemory()`.
  - `fdbclient/ServerKnobs.cpp:887`:
    `init( TLOG_SPILL_THRESHOLD, 1500e6 )`. A TLog keeps up to about 1.5 GB of
    un-spilled mutations in memory before it spills to disk.

**Root cause:** `--memory 1GiB` is below the TLog's designed in-memory
working set (`TLOG_SPILL_THRESHOLD` = 1.5 GB). Under sustained ingest the
resident-memory monitor stopped the process on purpose, with exit code 20. Raising
the log budget to 2 GiB removed that stop: the follow-up observed a peak log RSS
of 1,452,507,136 B, with 14 of 55 load frames above the old limit. The final
topology uses `--memory=4GiB` for logs. The analysis records a limit on this
fix: it addresses the observed process-budget stop but proves neither general
memory safety nor 20k capacity.

### Bottleneck 2: Cold key-range shard hotspot and pre-split range conditioning

With the log fixed, the `log2g` follow-up's 2x measured wave failed the strict
health gate on storage queue (`storage_server_write_queue_size`). Each run
writes into a fresh `(otel_otelbench_v2, <run>, …)` prefix. A new prefix starts
inside one existing shard, so early writes land on whichever storage server owns
that shard. Data distribution splits it only after the shard has grown. The
follow-up's 2x stage had six storage servers on two hosts. In its first
per-storage counter interval, **85.1%** of storage input went to a single
storage server, `10.97.0.11:4512`
(`log2g/native-analysis.json`; the completion record stores it as
`early_observed_native_input_share_hot_ss: 0.851153`). That server's share fell
to 38.6% in the next interval and to 12.6% in the one after, as data
distribution split the range. The 1x baseline of the first native campaign
showed the same pattern: its hot SS (`:4510`) reached a raw storage queue of
about 1.5 GB while `:4512` was barely used. The completion record notes that
the early delta also contains setup-time DD background, and that the cold
prefix was not proven to be the sole cause.

**Fix: pre-split range conditioning**, `K8sScalingRunner.prewarm_measured_ranges`
in the external runner:

1. Every active producer ingests its full 375k measured population into its
   real measured run prefix, with the same binary and options as the measured
   wave.
2. Wait until the cluster is healthy and stable **and** data distribution is
   quiescent: `moving_data.in_flight_bytes == 0` and `in_queue_bytes == 0` for 3
   consecutive samples. The resulting partition counts were 54, 102 and 143 at
   1x, 2x and 3x.
3. `clearrange` each measured run prefix (`\x02otel_otelbench_v2\x00\x02<run>\x00`
   to `…\x00\xff`), then wait for a stable drain.
4. Run the measured wave into the empty, pre-split range.

The shard boundaries survive the clear because FoundationDB merges shards
slowly. Merging an empty, low-bandwidth shard waits for
`DD_LOW_BANDWIDTH_DELAY` (240 s) and `DD_MERGE_COALESCE_DELAY` (300 s)
(`ServerKnobs.cpp:306-307`). These delays add up to about 540 s. The
measured wave starts long before that, so its writes spread across all 6N
storage servers from the first batch. The measured wave still writes every key,
census and oracle check from empty. Pre-split changes only shard placement,
never the data.

Evidence that conditioning matters, from the same run. The cold prerequisite
waves do not scale and the conditioned measured waves do:

| | Cold prerequisite (2x / 3x) | Pre-split measured (2x / 3x) |
|---|---|---|
| Drained efficiency | 50.7% / 49.6% | 109.2% / 108.3% |
| Max storage queue | 1,533 MB / 1,536 MB | 385 MB / 329 MB |
| Max lag | 35.4 s / 34.9 s | 15.0 s / 13.1 s |
| Producer transaction retries | 608–1,116 / 653–942 per producer | 0 / 0 |

The two wave types also differ in size (100k vs 375k per producer) and order.
The comparison shows strong agreement with the hotspot explanation, but it is
not a controlled single-factor test.

### Bottleneck 3: single TLog / single proxy, flannel VXLAN and a 4-CPU client cgroup

`k8s-fdb9` was the first Kubernetes run to complete all three stages. It scaled
poorly (`BELOW_NEAR_LINEAR`, drained efficiency 68.3% and 71.2%; 3x reached
20,093 ACK/s but only 12,651 drained/s). Its configuration had four limits that
the final topology removes:

| | `k8s-fdb9` | `k8s-cilium2` / `k8s-cilium4` |
|---|---|---|
| Pod network | k3s default flannel (VXLAN overlay, `--flannel-iface`) | Cilium eBPF, native routing over Hetzner VPC routes, no encapsulation |
| Producer CPU | `systemd-run --scope` with `CPUQuota=400%`, `AllowedCPUs=0-3`, `GOMAXPROCS=4` on a 16-vCPU host | uncapped, `GOMAXPROCS=16` |
| TLogs / commit proxies / GRV proxies / resolvers | N / N / N / 1 (**one TLog and one commit proxy at 1x**) | 2N / 2N / N / N |
| Storage servers per FDB host | 3 (`storageServersPerPod: 3`) | 6 |
| Storage pod / log pod / stateless pod CPU limit | 8 / 4 / 2 | 16 / 4 / 4 |
| Storage / log process memory | 3 GiB (1 GiB cache) / 2 GiB (64 MiB cache) | 4 GiB (2 GiB cache) / 4 GiB (128 MiB cache) |

These changes went in together. The data cannot assign the gain to any single
one of them, for example the VXLAN overhead.

Steps between `k8s-fdb9` and the final run:

- **`k8s-cilium1`** introduced Cilium native routing and tried `--workers 16`.
  The producer exited with status 2 at the 1x prerequisite gate, so the
  campaign returned to the fixed `--workers 8`.
- **`k8s-cilium2`** first ran the final topology above. Its 1x and 2x measured
  waves passed (2x: 20,684 ACK/s, 14,061 drained/s, 0 unhealthy samples). During
  the 3x prerequisite load, one `status json` sample came back as
  `database_status: {available: false}` with messages `unreadable_configuration`
  and `status_incomplete` ("Unable to retrieve version epoch information
  (Operation timed out)"). In the same document `recovery_state` was
  `fully_recovered`. The status probe had timed out under the cold-range
  storage-queue peak. The database had not failed. The runner treated that
  sample as fatal.
- **`k8s-cilium4`** used the same approved plan and a byte-identical cluster
  manifest. The runner's `fdb_status` now records an unavailable or incomplete
  sample as an *unhealthy* sample that is counted and reported, instead of
  aborting. A lost coordinator quorum is still fatal, and the final drain still
  needs three strictly healthy samples. This relaxation never applied in the
  run: all six waves recorded **0** unhealthy samples.

Run-to-run spread is visible between the two runs of the final topology. At 1x
measured, `k8s-cilium2` reached 10,327 ACK/s and 7,209 drained/s against
9,456 and 6,677 for `k8s-cilium4`. At 2x it reached 20,684 ACK/s and 14,061
drained/s against 16,973 and 14,586. The near-linear verdict is from one run, as
the boundary says.

### Methodology

- **Four golden signals per wave**, from `scaling.wave_metrics`:
  - traffic: ACK and drained rows/s
  - saturation: max/end storage queue, max/end durability lag, drain seconds
  - errors: transient unhealthy samples, producer exit codes, census and
    oracle failures
  - latency: client `CommitLatency`, `GRVLatency`, `ReadLatency` and
    `TransactionLatency` histograms in each `ingest.json`

  `status json` is sampled every 1 s during load and drain.
- **Pre-release forensic sealing**, `otel_native_forensic_evidence_v1`, used in
  the native systemd campaigns (`ScalingArm.wave` in `scaling.py`,
  `diagnostic.py`). Before the common release, every FDB process is bound by
  PID and `/proc` `starttime_ticks`. Every native trace file is bound by
  `(st_dev, st_ino)` through its open file descriptor. Trace reads are charged
  to a shared 16 MiB budget split across waves. Bottleneck 1 was proven with
  this sealing: the crash trace, the systemd invocation and the live identity
  sealed before release all name the same PID 15874. The Kubernetes runner does
  not run these samplers. Its evidence is `status json`, operator
  `FoundationDBCluster` status and pod JSON per stage.
- **Reusing VMs across arms**, the pattern in
  [`session.py`](../../layers/otel/bench/ops/horizontal/session.py) and
  `ScalingSession`. One immutable lease holds exactly six newly created,
  unprotected servers for all stages. Between stages only the workload changes
  (here, the `FoundationDBCluster` and its PVCs). After a failed arm, fresh host,
  source and deadline checks run before any reuse. A stale result from before
  the fault never authorizes reuse.
- **Sealed inputs.** The source manifest (8,624 files,
  `input_manifest_sha256 f8dbaacb…`) is checked before and after. The binary
  SHA-256 is checked on every host. Producer grants are validated and bound by
  `scaling.producer_grant`. Run names are fixed per stage and population.
- **Verified cloud teardown.** Before admission the controller took a snapshot
  of every pre-existing resource in the account: **17 IDs across 10 kinds**
  (3 servers, 1 network, 3 firewalls, 2 SSH keys, 2 volumes, 6 primary IPs, 0
  floating IPs, 0 load balancers, 0 placement groups, 0 certificates). Teardown
  deleted every owned resource. A read-only re-inventory then had to match
  the baseline exactly, with no missing and no unexpected IDs. For
  `k8s-cilium4` the six `hcloud server delete` calls exceeded the CLI's 10 s
  wait, but the re-inventory passed:
  `PASS: exactly the pre-existing set across 10 kinds (17 IDs)`. Every run in
  the history table passed the same check. The lease lasted about 51 minutes
  (6 × `ccx43` at the `hel1` list price of €0.526/h gross, one billed hour each;
  estimate, not an invoice).

## 4. Query latency after ingest (3x, `client1`, 375k spans)

These numbers come from the oracle-checked `query` run on a drained store and
are reported for context. They were not part of the scaling verdict. Each class
has 100 samples.

| Class | p50 (ms) | p95 (ms) | p99 (ms) |
|---|---:|---:|---:|
| trace | 3.35 | 4.22 | 5.86 |
| span | 3.13 | 3.91 | 5.82 |
| time_narrow | 3.27 | 4.23 | 5.41 |
| time_broad | 4.87 | 5.75 | 6.67 |
| genai_model | 5.73 | 6.65 | 7.79 |
| genai_operation | 5.80 | 8.28 | 9.46 |

## 5. Limitations and discrepancies

- **One run per stage, with no repeats and no confidence intervals.**
  `k8s-cilium2` shows that the same topology can vary by up to about 20% between
  runs (2x ACK).
- **Not a sustained-throughput certification.** Measured ACK windows were about
  40–44 s, not the 120 s the plan names.
- **`single` redundancy.** Each mutation is fsynced to one TLog and one storage
  server. Nothing here measures HA, replication or failure recovery.
- **Independent stores per producer.** Producers do not contend on one shared
  corpus or index. Each stage scales independent namespaces inside one cluster.
- **The timing includes controller overhead** (SSH release, 1 s polling, the
  three-sample drain rule). Drain-time resolution is about 1 s.
- **The pre-split step is part of the measured configuration.** Writing into a
  never-touched range performs like the prerequisite rows in the stage table.
- **The labels in the receipts are not all accurate.** Where they disagree with
  what ran, this report states what ran:
  - The plan's `gomaxprocs: 4` is a fixed validation field. The runner overrides
    it, and every producer command ran with `env GOMAXPROCS=16`.
  - The result's `log_memory_gib: 2` is a constant written by the runner. The
    applied log processes ran with `--memory=4GiB --cache_memory=128MiB`.
  - The plan's `study_scope` text (`storageServersPerPod=3, logs=N, …`)
    describes the earlier flannel topology. The applied manifests use 6 SS per
    pod and 2N logs.
  - The runner's pre-warm log message says "across 3N storage servers", but
    `status json` counted 6, 12 and 18.
- **Record compression was `zlib-1`.** Source revision `4af32c3bc` also
  supports Go-only Brotli (`brotli-0/1/3/4`,
  [`record_compression.go`](../../layers/otel/record_compression.go)), but this
  campaign did not measure Brotli.
- **The runner is not in this repository.** `k8s_operator_scaling.py` is a
  campaign-local file that imports `driver`, `policy`, `provision` and
  `scaling` from [`layers/otel/bench/ops/horizontal`](../../layers/otel/bench/ops/horizontal/).

## 6. Receipts and code references

Local receipts (not committed), root `/var/tmp/fdb-perf-scaling.v4O6xf/`:

| Path | Content |
|---|---|
| `k8s-cilium4/run/scaling-result.json` | schema `otel_k8s_operator_scaling_result_v1`, all stages, verdict |
| `k8s-cilium4/run/stage{1,2,3}/{prerequisite,measured}/client*-{ingest,audit,query}.json` | per-producer evidence (client metrics, census structures, 1,600 query samples) |
| `k8s-cilium4/run/fdb-cluster-s{1,2,3}.manifest.yaml`, `stage*-fdbcluster-status.json`, `stage*-k8s-pods.json` | applied CRDs, operator status, pod placement |
| `k8s-cilium4/run/commands/*.json` | each remote command: argv, exit code, stdout/stderr (513 records) |
| `k8s-cilium4/run/cleanup.json`, `k8s-cilium4/controller.log` | teardown actions and the 17-ID/10-kind verification |
| `k8s-cilium4/plan-approved.json` (+ `.sha256`) | approved plan |
| `k8s_operator_scaling.py` | Kubernetes/Cilium runner |
| `native-analysis.json`, `crash-evidence.json` | Bottleneck 1 analysis and symbolized stack |
| `completion-final.json`, `log2g/` | native follow-up and the hot-SS share |
| `k8s-fdb*/`, `k8s-cilium{1,2,3}/` | earlier runs in the history table |

Code:

- Ingest path: [`layers/otel/ingest.go`](../../layers/otel/ingest.go)
  (`prepareIngestBatch` 300 KB estimate, `prefetchIngestRecords`,
  `saveIngestBatchRun` with the 500 KB / 1 s guard)
- Transaction options: [`layers/otel/fdb.go`](../../layers/otel/fdb.go)
  (`SetTimeout(1500)`, `SetRetryLimit(10)`, `Audit`)
- Covering postings: [`layers/otel/index.go`](../../layers/otel/index.go)
  (`termPrefix`, `encodeCoveringValue`, collision-checking `Query`)
- Intrinsic indexes: [`layers/otel/intrinsic.go`](../../layers/otel/intrinsic.go)
- Batch prefetch: [`pkg/recordlayer/store_batch_prefetch.go`](../../pkg/recordlayer/store_batch_prefetch.go)
- Harness: [`layers/otel/bench/ops/horizontal/scaling.py`](../../layers/otel/bench/ops/horizontal/scaling.py)
  (`run_name`, `producer_grant`, `validate_ingest`, `wave_metrics`,
  `summarize_scaling`, `ScalingArm`, `ScalingSession`),
  [`session.py`](../../layers/otel/bench/ops/horizontal/session.py)
  (`Session`, `classify_arm_failure`)
