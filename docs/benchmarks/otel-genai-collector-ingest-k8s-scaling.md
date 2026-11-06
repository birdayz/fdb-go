# GenAI/OTel custom Collector-model ingest: 1x/2x/3x scaling on Kubernetes + Cilium

This report covers the benchmark campaigns behind PR #1023 (branch
`perf/otel-ingest-20k`). PR #1022's client changes merged into master as
`cac2de7a355d3d41a1ff137883f583f4fc0bf82a`; #1023 is now based on master,
not on an active #1022 branch. There are nine measured cloud campaigns, `k8s-fts6` through
`k8s-fts13` and `k8s-fts14c`, run 2026-11-01 to 2026-11-03 in Hetzner `hel1`.
These are performance experiments, not correctness or merge-readiness evidence;
the lower/upper stack still requires its own conformance/correctness acceptance. A profiling
session on a separate small box ("box 3") explains where the remaining CPU goes.

Each campaign measures a sustained **custom in-process Collector-model queue**,
not an upstream OpenTelemetry Collector pipeline. A synthetic multi-turn GenAI
corpus goes through the repository's local batching/retry implementation and the
`fdb_otel` exporter into the Record Layer. No upstream Collector component,
exporterhelper integration, receiver transport, or Collector overhead is measured.
Actual upstream Collector integration remains required for the larger product goal,
not established by these historical rates. The exporter stores:

- spans
- correlated logs
- deduplicated message records with a TEXT index

Two query workers run the eight-class query suite while ingest runs, and the
same suite runs again after ingest. Each campaign measures the same tree at 1x,
2x and 3x scale.

The historical receipts called the local path a “Collector sending queue”; that
label overstated the implementation boundary. The accurate boundary is synthetic
deterministic multi-turn GenAI conversations and correlated logs through the custom
`fdb_otel` Collector-model queue into one FDB run, one producer. Each stage runs
one such producer per client host. [Section 6](#6-limitations) lists what the runs do not show.

## 1. Summary

The last measured run, `k8s-fts14c` (plan revision `326c23e66`), ingested at the
following steady-state rates, with 0 ingest/query failures. Its original runner
exited unsuccessfully on cleanup, not on a benchmark stage (section 7):

| Stage | Producers | Spans/s | Messages/s | Raw input MB/s | Scale factor | Efficiency |
|---|---:|---:|---:|---:|---:|---:|
| 1x | 1 | 2,785.5 | 44,568 | 29.8 | 1.000 | 100.0% |
| 2x | 2 | 5,517.4 | 88,279 | 59.0 | 1.981 | 99.0% |
| 3x | 3 | **8,177.9** | **130,846** | 87.4 | 2.936 | **97.9%** |

Efficiency is `(rate_N / rate_1x) / N`.

- **Every producer is client-CPU bound.** Each producer process used 15.06–15.21
  of its host's 16 cores (94.1–95.1% utilization) at 5.41–5.59 ms of CPU per
  span. GC took 11.4–11.6% of that CPU.
- **Storage is not the limit.** FoundationDB reported `performance_limited_by:
  workload` in every steady-state status sample at every stage. Mutation bytes
  reaching storage were 6.5–6.6 KB per span (decimal KB).

Against `fts13`, throughput rose **8.1% / 8.6% / 8.1%** at 1x / 2x / 3x.
Across the series, 1x throughput rose 4.2x (665.6 to 2,785.5 spans/s). Storage
mutation bytes per span fell about 5.2x (34.0 to 6.6 KB at 1x).

### Progression

| Run | Historical plan revision | Historical pre-restack change label | What changed since the previous run | 1x | 2x | 3x | 3x efficiency | Mutation KB/span (1x) | Messages per txn |
|---|---|---|---|---:|---:|---:|---:|---:|---:|
| `k8s-fts6` | `e2bc4d95c` | `7a030eb06` | old custom blob/posting/back-reference layout | 665.6 | 1,339.2 | – | – | 34.0 | – |
| `k8s-fts7` | `e48b98793` | `6e18ab5c0` | messages as Record Layer records with a TEXT index | 849.6 | 1,579.6 | 2,393.0 | 93.9% | 10.3 | 22.9 |
| `k8s-fts8` | `7cf0eda3c` | `9edee5846` | conflicts coalesced over written indexes only; persistent RYW structures | 1,070.9 | 2,111.8 | 3,159.5 | 98.3% | 10.4 | 22.9 |
| `k8s-fts9` | `cd1ee1615` | `c4e387a39` | BunchedMap on packed bytes; RYW reads outside the lock; tokenize once | 1,554.9 | 2,809.6 | 4,077.7 | 87.4% | 10.5 | 22.9 |
| `k8s-fts10` | `b39c920f8` | `4677c3ffa` | pipelined TEXT reads on client futures; C++ conflict maps | 1,482.7 | 2,817.7 | 3,887.4 | 87.4% | 10.5 | 22.9 |
| `k8s-fts11` | `62ac14e16` | `0d3597919` | one reference row per span; up to 128 messages per transaction | 2,040.9 | 4,098.0 | 6,138.1 | 100.3% | 7.8 | 85.2 |
| `k8s-fts12` | `859458f7b` | `7054d07cc` | directory-layer key prefixes; group-committed messages | 2,060.8 | 4,217.3 | 6,317.5 | 102.2% | 6.4 | 102.1 |
| `k8s-fts13` | `0776ed68e` | `61658c916` | batch TEXT bunch prefetch; one `SaveRecordBatch` per message transaction | 2,576.0 | 5,081.7 | 7,565.6 | 97.9% | 6.4 | 102.1 |
| `k8s-fts14c` | `326c23e66` | `326c23e66` | rebased client tree; CPU-utilization correction and campaign asset transport | 2,785.5 | 5,517.4 | 8,177.9 | 97.9% | 6.6 | 102.1 |

Notes on the columns:

- **Rates** are steady-state spans/s summed over the stage's producers.
- **Plan revision** preserves each measured plan's historical `source_revision`.
  The next column preserves old pre-restack labels, **not current branch hashes**
  or proof of byte-identical trees. These historical objects are not all durably
  published/reachable from a fresh clone, so the report alone does not let a new
  reader inspect or reconstruct every measured source tree. No new refs or fresh
  measurements have been created to disguise that provenance limitation.
  Section 3 uses the historical labels; the subject mapping below locates the
  corresponding changes in current history, not the measured trees.
- **`k8s-fts6` at 3x is incomplete.** One of `client2`'s under-load
  `fts_multi_turn_history` queries failed, so the runner aborted the stage
  before collecting `client3`'s report.
- **Messages per txn** is message records written divided by message
  transactions. In `k8s-fts6` the messages were blobs, not records, so the
  column does not apply.

Change between consecutive runs, 1x / 2x / 3x:

| Run | Change |
|---|---|
| `fts7` vs `fts6` | +27.7% / +18.0% / (3x n/a) |
| `fts8` vs `fts7` | +26.0% / +33.7% / +32.0% |
| `fts9` vs `fts8` | +45.2% / +33.0% / +29.1% |
| `fts10` vs `fts9` | −4.6% / +0.3% / −4.7% |
| `fts11` vs `fts10` | +37.6% / +45.4% / +57.9% |
| `fts12` vs `fts11` | +1.0% / +2.9% / +2.9% |
| `fts13` vs `fts12` | +25.0% / +20.5% / +19.8% |
| `fts14c` vs `fts13` | +8.1% / +8.6% / +8.1% |

### Change subjects after restacking (not measured-tree identities)

Locate these subjects with `git log --all --fixed-strings --grep='<subject>'`;
a restacked copy is not proof of equal source bytes. Client-only subjects from
the old #1022 history were folded into its published merge commit above.

| Historical label | Change subject in the lower history |
|---|---|
| `7a030eb06` | `fix(otel): exempt exporter span commits from the 1s elapsed guard` |
| `6e18ab5c0` | `perf(otel): store GenAI messages as Record Layer records with a TEXT index` |
| `9edee5846` | `perf(recordlayer): coalesce batch write conflicts only over written indexes` |
| `c4e387a39` | `perf(recordlayer): BunchedMap puts on packed bytes, as Java does` |
| `4677c3ffa` | `perf(recordlayer): pipelined TEXT reads use the client's futures` |
| `0d3597919` | `perf(otel): one reference row per span; larger message transactions` |
| `7054d07cc` | `perf(otel): directory-layer key prefixes and group-committed messages` |
| `61658c916` | `perf(otel): save message records with one SaveRecordBatch` |
| `326c23e66` | historical measured tree; no current-byte identity claimed |

### Benchmark checks (not conformance certification)

Totals over `fts7`–`fts13`, all stages and producers:

- **Errors:** 0 tool failures, 0 failed export or log calls, and 0 query
  failures. Post-ingest the suite ran 2,400 queries per run, and under load
  between 29,010 and 90,395 per run.
- **Collector counts:** 0 rejected, failed or duplicate spans, and
  `accepted_spans == stored_spans` for every producer.
- **Fidelity:** 0 mismatches in the read-back check (32 spans per producer).
- **Ordering:** 0 watermark anomalies.

`fts14c` separately reports the same zero-error checks across its six producer
reports: 0 tool failures, failed export/log calls, query failures, rejected,
failed or duplicate spans, fidelity mismatches (32 checked spans per producer),
and watermark anomalies; accepted and stored span counts agree. It completed
28,271 under-load queries and 2,400 post-ingest queries across all three stages.

`k8s-fts6` is the exception. It had 41 and 7 failed export calls at 1x and 2x,
which the Collector retried, so every accepted span was still stored. It also
had the one failed under-load query at 3x.

## 2. Setup

The infrastructure is the same as in
[otel-ingest-25k-k8s-cilium-scaling.md](otel-ingest-25k-k8s-cilium-scaling.md#2-system-architecture-and-stack-k8s-cilium4):

- **Hosts:** six Hetzner `ccx43` hosts (16 dedicated vCPUs, 64 GiB). Three are
  FoundationDB nodes and three are client nodes.
- **Kubernetes:** k3s `v1.31.5+k3s1` with Cilium native routing over Hetzner
  VPC routes.
- **FoundationDB:** fdb-kubernetes-operator `v2.38.0` running FDB 7.3.77, with
  `single` redundancy and `ssd-redwood-1`.
- **Cluster per stage:** one operator-managed cluster, `otel-fdb-s{N}`, across N
  FDB nodes:
  - storage: one pod per node, `storageServersPerPod: 6`, so 6, 12 or 18
    storage servers (confirmed by `storage_process_count` in the status
    samples)
  - 2N logs, 2N commit proxies, N GRV proxies and N resolvers
- **Stage manifests:** the same topology from `fts6` through `fts14c`.

**Stage Nx** is N FDB nodes plus N producers, all in one FoundationDB cluster.
Each producer writes its own run (`cbench_s{N}_p{i}`), so each has an independent
Record Layer store in that cluster. No producer reads or writes another's data.

**Producer.** One `otelbench` process per client host runs directly on the
host, not in a pod, with `GOMAXPROCS=16`:

```
otelbench --backend fdb --mode collector-bench --run cbench_s<N>_p<i> \
  --duration 600s --warmup 60s --report-interval 10s --turns 12 \
  --producers 8 --consumers 16 --query-workers 2 --iterations 50 \
  --batch 64 --workers 8 --record-compression zlib-1 \
  --posting-value-format tuple --hash-bytes 8 --order time \
  --enable-message-deltas=true --enable-message-fts=true
```

The process contains the whole pipeline:

- **Load:** 8 load-generator goroutines feed the custom Collector-model queue
  (10,000 spans).
- **Export:** 16 consumers call the exporter. Each export batch holds 64 spans,
  and at most 32 write transactions are in flight, each with a 10 s timeout.
- **Queries:** 2 query workers run continuously during load.
- **After ingest:** 50 iterations of each query class.

**Corpus** (`fts13`, 3x, `client1`):

- 125,284 conversations of 12 turns, giving 1,503,408 spans and 125,284
  correlated logs.
- 16 message elements per span (24,054,528 in total).
- Delta dedup stores 3,071,345 unique messages, about 2.0 per span. It saved
  13.3 GB of 15.4 GB raw message bytes (86%).

**Measurement windows.**

- **Steady state:** each run lasts 600 s, of which the first 60 s are warmup.
  The steady-state rate (`rates.steady_state`) sums the load-phase 10 s windows
  that start at or after 60 s: nominally 540 s (55 windows, including a short
  final window) per producer. Reported window wall times include scheduling
  overrun; the run is 600 s **including**, not plus, the 60 s warmup.
- **CPU** (from `fts13` on): `rates.steady_state.cpu` is the process's
  `getrusage` user and system time over the same windows. `ms_per_span` is that
  CPU divided by the exported spans. `gc_fraction` is the Go runtime's GC CPU
  over total CPU. GOMAXPROCS equals the core count here, so the per-P GC
  estimate does not overstate it.
- **Storage:** the runner sampled `status json` every 5 s
  (`fdb-status-samples.json`). Mutation KB/span is the summed storage-server
  `mutation_bytes` increase between the first and last samples inside
  75–600 s, divided by their actual timestamp interval and the stage's steady
  spans/s. This is storage mutation traffic, not physical disk I/O or raw
  input bytes.

## 3. Bottlenecks, in order

### 3.1 Storage-bound old layout (`fts6`)

The old layout wrote custom message blobs, token postings and per-span
back-references. Each span cost **34.0 KB of storage mutations** at 1x (34.7 KB
at 2x). The storage servers could not keep up. In the steady window of every
stage:

- `performance_limited_by` was `storage_server_write_queue_size` in all but
  one or two samples per stage (44–46 of 46–47).
- The worst storage queue held at about 1.0 GB (median), with durability lag up
  to 50–60 s.
- Every sample was unhealthy, because the batch-priority probe timed out.

The 41 and 7 failed export calls at 1x and 2x are this saturation reaching the
exporter.

`6e18ab5c0` (perf(otel): store GenAI messages as Record Layer records with a
TEXT index) replaced the custom structures with:

- a `Message` record type, with a unique hash index for dedup
- a standard TEXT index sharded by writer
- a span reference index

Queries moved to the ported `TextScan` (`6e4c4623f`). `fts7` wrote
**10.3 KB/span**, 3.3x less. FoundationDB reported `workload` as the limit in
all steady samples of every stage, the median storage queue fell to about
80 MB, and lag stayed at 5.5 s or less. From `fts7` on, no run was storage
bound in steady state.

### 3.2 Client CPU (`fts7` to `fts10`)

From `fts7` to `fts10` the storage cost per span stayed flat at 10.3–10.5 KB and
storage stayed workload-limited. Throughput moved with client CPU only. A
profile at `7cf0eda3c` put TEXT index maintenance at about 60% of client CPU
(see the `TODO.md` entry "TEXT index maintenance CPU in message transactions").

| Interval | Historical labels (pre-restack change, plan-tree hash) | Effect at 1x |
|---|---|---|
| `fts7` to `fts8` | `9edee5846`/`7cf0eda3c` coalesce batch write conflicts only over written indexes; `1f4913b8a`/`bc250da14` persistent RYW structures | +26.0% |
| `fts8` to `fts9` | `c4e387a39`/`cd1ee1615` BunchedMap puts on packed bytes, as Java does; `5d0f43515`/`b9324f74c` RYW reads outside the lock; `36e8763ff`/`2e5c0dcc5` tokenize each message once | +45.2% |
| `fts9` to `fts10` | `4677c3ffa`/`b39c920f8` pipelined TEXT reads use the client's futures; `f93654f81`/`92b700c63` C++ conflict maps on versioned pseq nodes | −4.6% |

`fts10` did not gain. `TODO.md` records why for the conflict-map round: 5% less
CPU, but 7% more wall time spent waiting on locks.

`fts9` and `fts10` also scaled worse at 3x (87.4%). At 3x their producers saw
290–375 and 85–97 message-transaction retries respectively, against 0 at 1x.

### 3.3 Single-flight message writers (`fts10`)

The pipeline has 16 collector consumers. Each consumer persisted its own export
call's messages, one transaction at a time, about 23 messages per transaction
(`written / transactions` = 22.9 from `fts7` to `fts10`). The summed wall time
of the message-persist phase, divided by the ingest wall time, was **15.3–15.5**
in every producer. On average about 15.3 of the 16 consumers were inside
message persistence, so the writers themselves set the ingest rate, not storage
or a saturated process.

`0d3597919` (perf(otel): one reference row per span; larger message
transactions) changed two things:

- **One reference row per span.** Every span used to index a row for each
  message in its conversation history, which is O(n²) per conversation. Each
  span now has one `envelope_refs_v1` row, plus a marker per
  (message, trace).
- **Bigger message transactions.** The cap is now 128 messages per
  transaction, which merges TEXT appends.

`fts11` results:

- 85 messages per transaction.
- 7.8 KB/span of mutations, down from 10.5.
- **+37.6% / +45.4% / +57.9%**, with linear scaling (3x efficiency 100.3%).
- The persist-phase busy figure fell to 14.4–14.6.

### 3.4 Directory prefixes and group commit (`fts12`)

Two commits:

- `128646d77`: KeySpace subspaces flatten like Java; adds a directory-layer
  resolver.
- `7054d07cc`: new runs use directory-layer key prefixes with integer subspace
  keys (`key_layout directory_v1`). Message transactions take every queued
  message up to 128 messages or 1 MB, not just one call's messages.

Effects:

- Messages per transaction rose from 85 to 102.
- Mutation bytes per span fell from **7.8 to 6.4 KB** (−18%), and stored bytes
  per span from 6.7 to 5.6 KB.
- Throughput rose only **1.0–2.9%**.

Storage cost and the single-flight bound were no longer what limited ingest.
Each message's client CPU was.

### 3.5 Batched TEXT reads (`fts13`)

Two commits:

- `17316ecc5` (perf(recordlayer): prefetch a batch's TEXT bunch reads together):
  `SaveRecordBatch` and `PrefetchRecordBatch` issue one snapshot read per token
  map. Together these cover every BunchedMap read that the batch's TEXT
  maintenance makes. Only the read-your-writes cache is filled, so the commit
  (mutations and conflict ranges) is identical to sequential saves; a
  differential test pins this. For 128 records of 57 tokens, read round trips
  fall from 205 to 3.
- `f61bc8366` (perf(otel): save message records with one SaveRecordBatch): one
  call per message transaction does the existence reads and TEXT reads for all
  of its records.

`fts13` gained **+25.0% / +20.5% / +19.8%**. It is the first run with process CPU
in the report (`61658c916`). Every producer at every stage ran at 15.1 of 16
cores, 5.9 ms CPU per span, with 11–12% GC:

| Stage | Producer | Cores used | Utilization | CPU ms/span | GC | Peak heap in use |
|---|---|---:|---:|---:|---:|---:|
| 1x | p1 | 15.08 | 94.2% | 5.85 | 11.3% | 0.90 GiB |
| 2x | p1 | 15.14 | 94.6% | 5.86 | 11.3% | 0.89 GiB |
| 2x | p2 | 15.17 | 94.8% | 6.07 | 11.7% | 0.89 GiB |
| 3x | p1 | 15.08 | 94.3% | 5.91 | 11.2% | 0.93 GiB |
| 3x | p2 | 15.15 | 94.7% | 6.12 | 11.6% | 0.93 GiB |
| 3x | p3 | 15.14 | 94.6% | 5.96 | 11.7% | 0.90 GiB |

At 3x, storage took 49.3 MB/s of mutations across 18 storage servers. No server
received more than 6.9% of them. In the steady window storage was still
workload-limited, with a median queue of 151 MB (max 181 MB) and lag of at most
5.9 s.

The only queue peaks, up to 1.57 GB, came in the first 40 s, when the fresh run
prefixes were still in single shards. Those samples fall inside the warmup.
`fts13` 2x has the series' one unhealthy sample (at 6 s, a batch-priority probe
timeout). At 2x and 3x the producers retried 580–889 of their roughly 29,000
message transactions; none failed.

### 3.6 Where the CPU goes now (box 3)

To split the remaining CPU, the `fts13` tree (`0776ed68e`) was profiled against
`daa69f001`. `daa69f001` is `0776ed68e` plus the pre-review "perf(fdbgo): C++
RYW iteration, conflict maps and QueueModel locking" change. The historical
#1022 revision `01e83d34b` and its follow-ups were later merged in `cac2de7a3`;
neither that old label nor the merged tree is byte-identical to this measured tree.

The receipts are local and uncommitted, like the cloud ones, under
`/var/tmp/box3-receipts/2026-11-03/` (indexed by its `README.md`; see
[section 7](#7-receipts-and-code-references)). Every figure below was checked
against them, including the unrounded `tn/` reports for the fits. Read the
profile and throughput figures as a comparison between
the two trees, not as absolute rates (see [Limitations](#6-limitations)).

**Setup** (exact commands in the receipts' `commands.txt`):

- **Machine:** box 3 is a Hetzner `cpx32` with 4 *shared* vCPUs (x86_64).
- **Binaries:** `otelbench` built from each revision with go1.27.3,
  `CGO_ENABLED=0`, and `vcs.modified=false` stamped:
  - `daa69f001`: sha256 `9cffe43e…5636c`
  - `0776ed68e`: sha256 `4bb6e6cc…3cee6`

  The copies on box 3 have the same sha256s.
- **Cluster:** `measure.py` starts a local FDB 7.3.77 cluster with one
  transaction process, one stateless process and two storage processes:
  `single ssd-redwood-1`, with 1 log, 1 commit proxy, 1 GRV proxy and 1
  resolver.
- **Load:** a 120 s collector-bench (8 consumers, 20 s warmup, otherwise the
  cloud flags) with `GOMAXPROCS=16`.
- **CPU figure:** `measure.py` reports client CPU as the `otelbench` process's
  user+sys time (`RUSAGE_CHILDREN`) over the run's spans.
- **Arms:** `d0` adds no network delay. `d2` adds a 2 ms `tc netem` delay on
  `lo`, for a 4 ms RTT.
- **Runs:** eight throughput runs, interleaved: `0776` d0, `daa` d0, `0776` d2,
  `daa` d2, `daa` d0, `0776` d0, `daa` d2, `0776` d2. Then two profile runs at
  d0 (`daa`, then `0776`). Each profile run took a 90 s CPU profile over
  ingest seconds 20–110.

**Throughput and client CPU** (receipts `box3/runs/m-c-*.out`; every run had
0 failures, and storage was `workload`-limited at the end; post-ingest query
p50s were within run-to-run noise of each other, see `box3/summaries.txt`):

| Arm | `0776ed68e` (run 1 / 2) | `daa69f001` (run 1 / 2) | Change |
|---|---|---|---|
| d0 spans/s | 679 / 711 | 780 / 791 | +14.9% / +11.3% |
| d2 spans/s | 709 / 635 | 740 / 714 | +4.4% / +12.4% |
| d0 client CPU ms/span | 5.07 / 4.88 | 4.32 / 4.21 | −14.8% / −13.7% |
| d2 client CPU ms/span | 4.87 / 5.49 | 4.56 / 4.76 | −6.4% / −13.3% |

**Whole process per span.** These are the CPU-profile phase rows from
`analysis/<rev>.phase2.txt`. The profile window holds:

- `daa69f001`: 66,196 spans and 132,394 stored messages
- `0776ed68e`: 62,584 spans and 125,170 stored messages

The stored-message count is the run's `messages.written` scaled by the window's
share of spans (`analysis/window_counts.py`). The phases are disjoint, so the
rows sum to the profile total.

| Phase | `daa69f001` ms/span | Share | `0776ed68e` ms/span | Share |
|---|---:|---:|---:|---:|
| message persist (FDB transactions) | 2.958 | 66.0% | 3.340 | 68.1% |
| message decompose, tokenize, plan | 0.206 | 4.6% | 0.201 | 4.1% |
| span ingest path | 0.184 | 4.1% | 0.186 | 3.8% |
| FDB client network and background | 0.408 | 9.1% | 0.417 | 8.5% |
| GC background | 0.591 | 13.2% | 0.623 | 12.7% |
| load generator and corpus | 0.064 | 1.4% | 0.064 | 1.3% |
| bench sink accounting | 0.016 | 0.3% | 0.016 | 0.3% |
| query workers | 0.013 | 0.3% | 0.016 | 0.3% |
| async Record Layer reads (pipelined, unattributed) | 0.004 | 0.1% | 0.004 | 0.1% |
| logs export | 0.003 | 0.1% | 0.002 | 0.0% |
| collector pipeline | 0.000 | 0.0% | 0.000 | 0.0% |
| other runtime (scheduler, syscalls, timers) | 0.037 | 0.8% | 0.037 | 0.8% |
| **rows summed** | **4.484** | | **4.906** | |
| **profile total** | **4.485** | | **4.907** | |

The two totals differ from the summed rows only by rounding.

**GC.** GC in total is 0.618 ms/span (13.8%) for `daa69f001` and 0.649 ms/span
(13.2%) for `0776ed68e`. Of that, 0.027 and 0.026 ms/span is mark assist, which
is already counted inside the phases where it ran. An earlier version of this
table added the assist a second time in a "GC (background + assist)" row and
left out the async-reads row, so its rows summed to 4.504 against a 4.485 total
(`analysis/reconciliation.txt`).

The report's own `cpu.gc_fraction` (0.47–0.51 on box 3) is not usable here.
With 16 Ps sharing 4 CPUs, the runtime's per-P estimate overstates GC, so the
profile is the source for GC.

**Exporter versus harness.** At `daa69f001`, 4.35 of the 4.485 ms/span (97%) is
the exporter: its message and span paths, its FDB client and the GC it causes.
About 0.10 ms/span (2%) is the harness: the load generator, bench sink and query
workers. The rest is other runtime. The cloud producers' CPU bound is the
exporter's own cost.

**Per stored message** (about 2 per span; 2 × 1.479 ≈ the 2.958 ms of message
persist above). From `analysis/<rev>.ops3.txt`:

- Message-path CPU per stored message fell from 1.670 ms to 1.479 ms.
- TEXT index maintenance, its largest operation, fell from 1.425 ms to
  1.222 ms.

By component:

| Component | `daa69f001` ms/message | Share | `0776ed68e` ms/message | Share |
|---|---:|---:|---:|---:|
| client RYW | 0.925 | 62.6% | 1.097 | 65.7% |
| allocation (`mallocgc`, assist) | 0.215 | 14.6% | 0.244 | 14.6% |
| Record Layer own code | 0.145 | 9.8% | 0.138 | 8.2% |
| tokenize | 0.054 | 3.7% | 0.056 | 3.3% |
| client, other | 0.048 | 3.2% | 0.048 | 2.9% |
| compression | 0.043 | 2.9% | 0.041 | 2.4% |
| tuple | 0.025 | 1.7% | 0.023 | 1.4% |
| protobuf | 0.020 | 1.3% | 0.021 | 1.2% |
| `layers/otel` | 0.003 | 0.2% | 0.003 | 0.2% |

The RYW cost broken down by the Record Layer call that reaches it is in
`analysis/<rev>.rywapi.txt`:

- **Selector reads through RYW are the largest remaining client cost:**
  0.626 ms per message at `daa69f001` (`Snapshot.GetRange` 0.584 plus its
  allocation 0.042), against 0.651 at `0776ed68e`.
- **The conflict and write side fell from 0.463 to 0.302 ms per message.**
  These are BunchedMap's `AddReadConflictRange`, `AddWriteConflictKey`,
  `AppendIfFits` and `AddWriteConflictRange` calls.

**Message transaction time.** At 4 message TEXT shards (W4) and 4 ms RTT,
whole-run mean message transaction time for n stored messages gives the
two-point fit `t(n) = a + c·n`. Each pair uses message caps 32 and 128;
`n = written / transactions`, `t = txn_ns / transactions`. These are six
earlier runs in `tn/runs/`, recomputed from unrounded reports by `tn/fit.py`
(`tn/fit.txt`):

| Build pair | a (fixed, ms) | c (per message, ms) |
|---|---:|---:|
| pre-commit directory-layout work, before group commit (`b4c32`, `b4c128`) | 104.9 | 4.11 |
| batch TEXT prefetch (`pf4c32`, `pf4`) | 45.3 | 2.63 |
| plus one `SaveRecordBatch` (`sb4c32`, `sb4`) | 44.0 | 2.46 |

These are **not clean-commit comparisons**. The baseline binaries have no VCS
stamps and their exact build commands were not recorded; their reported lineage
is pre-commit `859458f7b` work, not measured commit `859458f7b`. They also used
`CGO_ENABLED=1`, versus 0 for pf/sb. The pf/sb binaries are `vcs.modified=true`,
with bench-only W4/c32 edits. `pf4` is stamped `859458f7b` plus copied,
then-uncommitted prefetch code; `pf4c32` and both sb binaries are stamped
`ff5040d25`. The sb change was later committed as `0b63e8419`.
`tn/binaries.txt` records hashes/stamps; `tn/commands.txt` records the available
build and run commands. This limits attribution: the fits are consistent with
lower fixed read-round-trip cost, but do not isolate a clean commit's effect
or establish a general linear law from just two points.

The `0776ed68e` cloud run predates the measured `daa69f001` client round. The
later cloud follow-up below uses a rebased client tree, not that exact box-3
binary. New `BenchmarkRYWTextMaintenance` results in the client PR are a
separate microbenchmark population, not these `daa69f001` measurements.

### 3.7 Cloud follow-up on the rebased client tree (`fts14c`)

The approved plan and all six producer reports identify revision `326c23e66`
(full `326c23e6644dfaa4b0601276d26e972ac4850616`); the plan seals binary SHA-256
`b3d423f4e4d190ef76f79b11821953eea168b4f4f21ab513cb247b6fcfcfd420`.
The retained binary reports go1.27.3, linux/amd64, `CGO_ENABLED=1` and no VCS
build stamp. This is a precise historical experiment identity, not current or
final HEAD: it is based on an earlier #1022 client tree and excludes later
correctness-review fixes. The intervening OTel commits correct utilization
accounting (`2b46c95ad`, with no denominator change on these 16-core hosts),
update CPU-work notes (`30083c9d4`) and change campaign asset transport
(`326c23e66`). The rebased client changes are not isolated by this comparison.

The same 600 s load duration includes 60 s warmup, leaving a nominal 540 s
steady window. Each stage has 1, 2 or 3 FDB hosts **in one cluster**, with the
same number of producers, each writing an independent store in that shared
cluster. No stage uses a separate cluster per producer.

| Stage | Producer | Cores used | Utilization | CPU ms/span | GC |
|---|---|---:|---:|---:|---:|
| 1x | p1 | 15.14 | 94.6% | 5.44 | 11.6% |
| 2x | p1 | 15.12 | 94.5% | 5.41 | 11.5% |
| 2x | p2 | 15.21 | 95.1% | 5.59 | 11.5% |
| 3x | p1 | 15.06 | 94.1% | 5.44 | 11.4% |
| 3x | p2 | 15.07 | 94.2% | 5.59 | 11.5% |
| 3x | p3 | 15.09 | 94.3% | 5.56 | 11.6% |

Storage mutation counters, summed across all 6/12/18 storage processes, and
query counts summed across the stage's 1/2/3 producer reports:

| Stage | Storage sample interval (s) | Mutation bytes increased | Mutation MB/s | Mutation KB/span | Under-load queries | Post-ingest queries |
|---|---|---:|---:|---:|---:|---:|
| 1x | 76.943–599.407 | 9,579,107,230 | 18.33 | 6.58 | 4,707 | 400 |
| 2x | 77.098–596.313 | 18,664,602,003 | 35.95 | 6.52 | 9,593 | 800 |
| 3x | 76.883–599.092 | 27,639,012,973 | 52.93 | 6.47 | 13,971 | 1,200 |

All 91/90/90 status samples inside 75–600 s at 1x/2x/3x respectively report
`performance_limited_by: workload`. Message transaction retries totaled
0/901/1,269 at 1x/2x/3x, but no ingest or query call failed. See section 5 for
latency: more ingest throughput did not uniformly improve query latency.

## 4. Conformance behind the optimizations

The repository contains the following Java/libfdb_c conformance and differential
tests for the changed surfaces. Their presence is not evidence that every
measured binary passed them, nor that review is complete. Correctness review of
the lower/upper stack remains a separate acceptance requirement; these historical
performance runs do not resolve it:

- **Record Layer (Java), PR #1023:**
  - TEXT query execution, continuations, merges and index contents:
    [`text_query_conformance_test.go`](../../conformance/text_query_conformance_test.go),
    [`text_index_pipeline_conformance_test.go`](../../conformance/text_index_pipeline_conformance_test.go)
  - BunchedMap operations, per-put conflict ranges and tuple order; the read
    mode of each pipelined read:
    [`bunched_map_conformance_test.go`](../../conformance/bunched_map_conformance_test.go)
  - KeySpace path bytes, containment, directory-layer mappings across engines,
    and the TEXT tokenizer-version key:
    [`keyspace_conformance_test.go`](../../conformance/keyspace_conformance_test.go)
  - Batch TEXT prefetch commit identical to sequential saves (differential):
    [`store_batch_text_prefetch_test.go`](../../pkg/recordlayer/store_batch_text_prefetch_test.go)
- **Pure-Go client (libfdb_c), merged PR #1022 and lower follow-ups:**
  - Selector range reads:
    [`selector_range_conformance_test.go`](../../pkg/fdbgo/client/selector_range_conformance_test.go)
  - `report_conflicting_keys`:
    [`conflicting_keys_conformance_test.go`](../../pkg/fdbgo/libfdbc/conflicting_keys_conformance_test.go)
  - Connect failures and read retries:
    [`connrefused_conformance_test.go`](../../pkg/fdbgo/libfdbc/connrefused_conformance_test.go)
  - The RYW script target
    [`ryw_script_conformance_test.go`](../../conformance/ryw_script_conformance_test.go)
    found and pinned three fixes:
    - a snapshot `Get` is issued at the call, and `Commit` waits for it
    - an explicit write conflict keeps `NEXT_WRITE_NO_WRITE_CONFLICT_RANGE`
    - an illegal write becomes the deferred error

## 5. Query latency at 3x

Each producer queries its own store. In the tables, p50 and p95 are the range
across the three producers, and p99 is the highest of the three. "Rows" is rows
per query.

**After ingest** (store drained, no load; 50 iterations per class per producer):

| Class | Rows | `fts11` p50 (ms) | `fts11` p95 | `fts11` p99 max | `fts13` p50 (ms) | `fts13` p95 | `fts13` p99 max |
|---|---:|---|---|---:|---|---|---:|
| correlated_logs_by_trace | 1 | 2.9–3.1 | 3.6–4.0 | 14.8 | 2.8–3.0 | 3.6–3.9 | 13.5 |
| trace_graph_traversal | 12 | 5.6–5.9 | 6.9–9.6 | 18.3 | 5.1–5.4 | 7.5–9.1 | 17.0 |
| same_span_conjunction | 12 | 5.4–5.8 | 7.7–9.6 | 11.3 | 5.1–5.2 | 6.2–9.6 | 17.7 |
| fts_multi_turn_history | 12 | 24.0–24.6 | 31.4–60.2 | 86.3 | 22.9–23.4 | 39.0–49.9 | 89.3 |
| fts_phrase_first_turn | 1 | 30.0–40.1 | 70.0–72.6 | 109.2 | 25.8–32.9 | 89.2–99.8 | 133.6 |
| fts_single_token | 100 | 47.8–60.3 | 82.1–96.0 | 108.2 | 41.4–44.6 | 80.0–101.1 | 144.3 |
| trace_existential_3way | 50 | 48.5–51.7 | 62.5–68.9 | 73.3 | 44.7–46.1 | 54.5–65.0 | 69.9 |
| fts_same_span_predicates | 100 | 51.3–53.7 | 62.4–68.6 | 74.5 | 49.0–50.4 | 63.6–80.7 | 105.2 |

`fts13`'s stores hold 21–22% more spans than `fts11`'s, because it ingested faster
over the same 600 s. Its post-ingest p50s are equal or lower in every class.

**Under load** (2 query workers in the ingesting process, for the whole 600 s):

| Class | `fts11` p50 (ms) | `fts11` p95 | `fts13` p50 (ms) | `fts13` p95 | `fts13` p99 max |
|---|---|---|---|---|---:|
| correlated_logs_by_trace | 5.3–5.5 | 21.0–26.0 | 15.2–19.7 | 61.2–66.6 | 136.8 |
| trace_graph_traversal | 24.4–26.4 | 70.5–77.2 | 69.6–73.2 | 157.2–169.2 | 295.0 |
| trace_existential_3way | 90.4–95.0 | 215.9–227.4 | 184.2–197.8 | 488.3–498.5 | 629.0 |
| fts_same_span_predicates | 111.0–114.2 | 205.8–208.0 | 242.4–250.3 | 476.0–506.5 | 613.3 |
| fts_single_token | 145.3–147.8 | 234.5–239.4 | 327.8–354.8 | 513.3–554.6 | 665.6 |
| same_span_conjunction | 154.7–158.1 | 224.0–229.5 | 356.1–369.3 | 480.9–536.4 | 676.7 |
| fts_phrase_first_turn | 58.3–62.0 | 138.1–163.4 | 157.2–174.7 | 444.2–499.8 | 611.1 |
| fts_multi_turn_history | 224.0–229.0 | 298.3–304.3 | 499.5–523.5 | 654.6–697.6 | 816.2 |

Under-load p50s doubled or more from `fts11` to `fts13`, and the query workers
completed 29,010 queries in `fts13` against 71,305 in `fts11`. The queries run
inside the ingesting process, which `fts13` drives to 94% of its cores. They
compete with ingest for CPU, and storage is not the cause: storage stayed
workload-limited. The post-ingest numbers show the store itself.

**`fts14c` at 3x.** Same populations: 50 post-ingest iterations per class per
producer; under-load histograms cover each producer's two query workers over
the whole 600 s, including warmup. Ranges are per-producer p50/p95, not pooled
quantiles; p99 is the highest producer p99.

| Class | Post p50 (ms) | Post p95 | Post p99 max | Under-load p50 (ms) | Under-load p95 | Under-load p99 max |
|---|---|---|---:|---|---|---:|
| correlated_logs_by_trace | 2.9–3.1 | 3.4–5.2 | 5.3 | 15.4–16.7 | 60.8–67.3 | 106.1 |
| trace_graph_traversal | 5.3–5.9 | 16.4–17.7 | 23.1 | 63.1–66.5 | 161.8–165.0 | 296.3 |
| same_span_conjunction | 5.3–6.2 | 7.0–9.4 | 15.3 | 353.8–393.3 | 516.0–554.0 | 644.7 |
| fts_multi_turn_history | 22.8–23.7 | 54.4–61.8 | 100.3 | 516.3–558.1 | 723.6–759.8 | 910.0 |
| fts_phrase_first_turn | 22.9–25.5 | 58.7–67.4 | 95.9 | 162.8–187.8 | 473.3–520.4 | 667.1 |
| fts_single_token | 51.3–58.4 | 102.7–114.8 | 144.3 | 331.2–362.6 | 531.2–576.4 | 715.5 |
| trace_existential_3way | 45.6–46.6 | 56.2–61.5 | 66.5 | 195.1–210.6 | 518.8–537.4 | 664.6 |
| fts_same_span_predicates | 50.1–52.7 | 62.5–68.4 | **2,014.7** | 261.7–271.9 | 514.6–536.3 | 697.7 |

`fts14c` completed 28,271 under-load queries across all stages versus 29,010 in
`fts13` (−2.5%), despite ingesting faster. The 2,014.7 ms post-ingest p99 is
`client3`'s `fts_same_span_predicates` tail in a population of 50 queries; it
must not be hidden by the otherwise similar medians. These single-run results
do not establish a query-latency improvement.

## 6. Limitations

- **One run per stage per tree.** There are no repeats and no confidence
  intervals. With one run per tree, a 1–3% change, such as `fts12`'s, cannot be
  told apart from run-to-run spread.
- **Independent stores per producer.** Each stage is one FoundationDB cluster
  across N nodes, and each producer writes its own store in it. Producers
  share the cluster's logs, proxies and storage servers, but no data, index or
  conflict ranges. This is horizontal scaling of independent tenants, not of
  one shared corpus.
- **`single` redundancy.** Each mutation is stored once. Nothing here measures
  replication, HA or recovery.
- **Synthetic GenAI corpus.** Deterministic conversations of 12 turns with high
  message reuse (86% of raw message bytes deduplicated). A real workload's
  reuse, message sizes and token distribution will change the cost per span.
- **Under-load query latency includes CPU contention** with ingest in the same
  saturated process (section 5). It is not a measure of query cost on an idle
  store.
- **Steady state is 540 s per stage.** The 60 s warmup, which contains the
  cold-prefix storage-queue peaks, is excluded.
- **Box 3 is a 4-vCPU shared-CPU machine** running `otelbench` with
  `GOMAXPROCS=16`, and its storage cluster is on the same box. Its throughput
  and CPU figures are for comparing the two trees with each other, not with
  the cloud runs.
  `fts13` does not include its client round; `fts14c` uses an earlier rebased
  client tree, not all subsequent correctness-review fixes.
- **Process CPU exists from `fts13` only.** The earlier runs' client-CPU
  attribution rests on profiles and on the storage samples showing `workload`
  as the limit.

## 7. Receipts and code references

Local receipts (not committed), root `/var/tmp/fdb-perf-scaling.v4O6xf/`:

| Path | Content |
|---|---|
| `k8s-fts{6..13}/plan-approved.json`, `k8s-fts14c/plan-approved.json` (+ `.sha256`) | approved plan, `source_revision`, binary SHA-256 |
| `k8s-fts{6,8..13}/run/`, `k8s-fts7/run3/`, `k8s-fts14c/run/` | campaign output |
| `…/stage{1,2,3}/collector_bench/client*-collector-bench.json` | per-producer report: `rates`, `ingest.export.messages`, `queries`, `under_load`, `memory`, `rates.steady_state.cpu` (`fts13`, `fts14c`) |
| `…/stage{1,2,3}/collector_bench/fdb-status-samples.json` | 5 s FoundationDB status samples |
| `…/fdb-cluster-s{1,2,3}.manifest.yaml`, `stage*-fdbcluster-status.json` | applied cluster manifests and operator status |
| `k8s-fts*/controller.log`, `…/cleanup.json` | runner log, teardown and original inventory check |
| `k8s-fts14c/inventory-final-recheck/summary.json`, `before-*.ids`, `after-*.ids`, `*.diff` | later read-only exact-baseline recheck (22 IDs, 10 kinds) |
| `k8s_operator_scaling.py` | runner (`run_collector_bench_wave`) |

Box-3 receipts (local, uncommitted), root `/var/tmp/box3-receipts/2026-11-03/`,
indexed by `README.md`:

| Path | Content |
|---|---|
| `commands.txt` | exact build, `measure.py`, profile and analysis commands |
| `binaries.txt` | revisions, Go build stamps and sha256s of both `otelbench` binaries |
| `box3/runs/m-c-<rev>-d{0,2}-{1,2}/`, `m-c-*.out` | throughput runs: collector-bench `report.json`, fdbserver traces, `measure.py` result |
| `box3/runs/m-p-{daa,0776}/`, `m-p-*.out` | the two profile runs |
| `box3/summaries.txt`, `box3/scripts/` | per-run summaries; `measure.py`, `lrun.sh` and `cpu.py` as run |
| `profiles/` | 90 s CPU profiles, the binaries that symbolize them, pprof `top`/`tree`/`traces` text |
| `analysis/` | `phase2.py`, `ops3.py`, `rywapi.py`, `window_counts.py` and their outputs per revision; `reconciliation.txt` |
| `tn/` | six fit-input runs plus comparison runs; `fit.py`, `fit.txt`, `binaries.txt`, `commands.txt` (including provenance gaps) |

Run history, for completeness:

- **`k8s-fts1` to `k8s-fts5`:** collector-bench bring-up runs. `fts5` failed at
  2x on the span commit's 1 s elapsed guard, which `fts6`'s revision exempts for
  exporter commits (`7a030eb06`).
- **`k8s-fts7`:** the first two attempts (`run`, `run2`) failed at server
  creation with `dedicated core limit exceeded`. `run3` is the reported run.
- **`k8s-fts9`:** cleanup failed its exact-baseline check. One firewall was left
  after the 10 s `hcloud server delete` waits timed out. It was gone by
  `fts10`'s admission snapshot, and `eebf0b969` (keep tearing down while Hetzner
  deletes servers) is in `fts10`'s tree.
- **`k8s-fts14`, `k8s-fts14b`:** operational failures (SSH failure and scp
  timeout respectively); neither produced benchmark numbers.
- **`k8s-fts14c`:** all three benchmark stages completed, but the original
  runner exited with `exact ten-kind baseline cleanup did not pass`.
  `run/cleanup.json` records firewall ID `11958319` still in use after server
  deletion, through attempts 0–39 (40 attempts); its original cleanup gate
  **failed**. The firewall was later gone without further deletion or other
  intervention by the session that resumed the work. The subsequent read-only `inventory-final-recheck/` verifies
  exactly the 22 pre-existing IDs across all 10 kinds, with no missing or
  unexpected IDs. This successful later recheck does not retroactively pass
  the original gate. The generic `scaling-result.json` verdict is
  `INCONCLUSIVE` (its drained-row stage list is empty in collector-only mode);
  the rate ratios here are computed from the six collector reports, not that
  generic verdict.
- **Other reported campaigns (`fts6`–`fts13`, except `fts9`):** the inventory
  check passed (exactly the pre-existing 17 or 22 IDs across 10 kinds).
- **Final resource cleanup:** box 3, its firewall and its SSH key were deleted
  after its remaining benchmark binaries were archived and all 11 hashes
  verified against `tn/binaries.txt`. Its two primary IPs were auto-deleted.
  `/var/tmp/benchbox3/inventory-after-delete/summary.json` verifies exactly
  the original 17 IDs across 10 kinds; no benchmark hosts remain.

Code:

- Custom Collector-model benchmark, rate windows and CPU accounting:
  [`layers/otel/collector/bench.go`](../../layers/otel/collector/bench.go),
  [`layers/otel/collector/bench_cpu.go`](../../layers/otel/collector/bench_cpu.go)
- Message storage, TEXT index and persist statistics:
  [`layers/otel/messages.go`](../../layers/otel/messages.go)
- Batch TEXT prefetch:
  [`pkg/recordlayer/store_batch_text_prefetch.go`](../../pkg/recordlayer/store_batch_text_prefetch.go)
- Profiling flags (`--cpuprofile`, `--profile-start`, …):
  [`layers/otel/README.md`](../../layers/otel/README.md)
- Open CPU work: [`TODO.md`](../../TODO.md), "RYW CPU in TEXT-maintenance-shaped transactions"
  and "TEXT index maintenance CPU in message transactions"
