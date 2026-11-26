# OTel span ingest on Kubernetes: Go own client, Go C client and Rust, 1 to 6 database nodes

Date 2026-11-14, Hetzner Cloud `hel1`, `ccx43` hosts (16 dedicated vCPU, 64 GB), k3s `v1.31.5+k3s1` with Cilium in native-routing
mode, fdb-kubernetes-operator `v2.38.0`, FoundationDB `7.3.77`, `single` redundancy, Redwood storage; the stack of the reference
run [k8s-cilium4](otel-ingest-25k-k8s-cilium-scaling.md). The tool that ran the campaigns is
[`layers/otel/bench/ops/k8s`](../../layers/otel/bench/ops/k8s/README.md) (a runbook for repeating them). Raw result files, samples
and logs of the campaigns are not in the repository; this page carries the tables and the caveats.

This is a measurement of the **load generators** (how fast each client writes spans into FoundationDB, and what it costs) on a
database that was sized so that the database is not the first limit where possible. It is not an application benchmark of the
Collector and it is not a sustained test: every number below is one run of 1.1 to 2.25 million spans (20 to 60 seconds of
ingest), never repeated unless stated.

## 1. What was measured

- **Executables** (sealed, built from revision `735512cac837fa9835f59cc420b0b266383db052`): `control-pure` (Go, the own client
  that speaks the FoundationDB wire protocol; also corpus, setup and verify), `control-native` (Go with the C client
  `libfdb_c`), `otel-rust` (Rust, `libfdb_c`).
- **Workload per generator process**: the Stage 0 "Point 2" collector-queue workload (producers, bounded queue, consumers that
  batch 64 to 256 spans and write them in one transaction, `block_on_overflow`), 11 key writes per span, fixture spans from a
  fixed corpus. "In flight" is the number of consumers times writer workers: each consumer writes one batch transaction at a time
  per worker. The Rust executable accepts one writer worker per consumer and at most 32 consumers per process; the Go own
  client accepts up to 8 writer workers per consumer. The production collector caps consumers at 64.
- **Run**: a fresh `FoundationDBCluster` per run (cluster and PVCs deleted first), tenant setup, **conditioning** (the real
  population is first ingested once with the Go own client so data distribution has split the key ranges, then the stores are
  cleared and restored to their fresh state in one transaction; the shard boundaries stay), then the measured wave with all
  generators released together, a drain wait, and a **full read-back and census** per generator (every span read back, key
  census equal). Unless a table says "executable counters only", a number is for a run that passed this verification.
- **Rates**: *acked* spans/s = verified distinct spans over the window from release to the last acknowledgement;
  *durable* spans/s = the same spans over the window to the moment the database queues have drained (storage queue under
  16 MiB, lag under 10 s). Acked above durable means the storage processes were behind when the generators finished.
- **Database figures** come from `fdbcli status json` once per second during the ingest: CPU cores per process (busiest
  process of each role), storage queue and lag, the ratekeeper's limiter, conflicts. **Client figures** are the generator's
  own report (CPU seconds, peak RSS) and, where stated, the per-thread CPU of the process.

## 2. Client comparison on 1, 2 and 3 database nodes (Part A, `lean-measured-1`)

N database nodes plus N client hosts (one generator process per client host), 375,000 spans per generator. Baseline rows run
8 in flight per generator, the other rows 32 or more (the Go own client with writer workers beyond 32). The "in flight" column
is per generator.

| nodes | variant | in flight / load generator | acked spans/s | durable spans/s | client CPU ms/span | client cores used of 16 (per host) | client peak MiB | DB CPU cores mean/peak | busiest DB process per role (cores) | storage queue MB / lag s | ratekeeper throttling | verified |
|---|---|---|---:|---:|---:|---:|---:|---|---|---|---|---|
| 1 | Go own client | 8 | 11494 | 9406 | 0.479 | 6.1 | 214 | 4.4 / 5.7 | storage 0.79, resolver 0.58, log 0.40 | 330 / 5.7 | not recorded | yes (full read-back + census) |
| 1 | Go C client | 8 | 7271 | 6402 | 0.608 | 4.7 | 278 | 3.6 / 4.5 | storage 0.59, resolver 0.50, log 0.28 | 146 / 5.5 | not recorded | yes (full read-back + census) |
| 1 | Rust (C client) | 8 | 12981 | 9876 | 0.200 | 2.7 | 155 | 4.4 / 5.6 | storage 0.85, resolver 0.65, log 0.41 | 294 / 5.8 | not recorded | yes (full read-back + census) |
| 2 | Go own client | 8 | 21495 | 17082 | 0.486 | 6.1 | 227 | 8.2 / 10.5 | storage 0.84, resolver 0.67, coordinator 0.54 | 313 / 5.8 | not recorded | yes (full read-back + census) |
| 2 | Go C client | 8 | 14339 | 12996 | 0.594 | 4.6 | 288 | 6.1 / 7.4 | storage 0.66, resolver 0.52, log 0.42 | 189 / 5.5 | not recorded | yes (full read-back + census) |
| 2 | Rust (C client) | 8 | 21367 | 17740 | 0.211 | 2.5 | 157 | 7.9 / 9.9 | storage 0.81, resolver 0.63, log 0.49 | 407 / 5.5 | not recorded | yes (full read-back + census) |
| 3 | Go own client | 8 | 27442 | 25136 | 0.527 | 5.6 | 210 | 13.1 / 16.3 | resolver 0.86, storage 0.75, log 0.64 | 244 / 5.6 | not recorded | yes (full read-back + census) |
| 3 | Go own client | 32 | 41208 | 32047 | 0.415 | 6.5 | 474 | 14.2 / 20.4 | resolver 1.00, storage 0.89, log 0.64 | 362 / 7.7 | no (limited by workload) | yes (full read-back + census) |
| 3 | Go own client | 64 (32x2 workers, Go-only) | 45821 | 34852 | 0.381 | 7.2 | 521 | 14.3 / 22.6 | resolver 1.00, storage 0.98, log 0.60 | 558 / 9.4 | no (limited by workload) | yes (full read-back + census) |
| 3 | Go own client | 128 (32x4 workers, Go-only) | 54615 | 35097 | 0.346 | 7.6 | 573 | 14.0 / 23.4 | storage 0.96, resolver 0.95, coordinator 0.59 | 606 / 8.3 | batch priority held back by storage_server_write_bandwidth_mvcc | yes (full read-back + census) |
| 3 | Go own client | 256 (32x8 workers, Go-only) | 56059 | 31708 | 0.328 | 7.9 | 684 | 14.5 / 22.3 | resolver 1.00, storage 0.97, log 0.65 | 649 / 10.0 | batch priority held back by storage_server_write_bandwidth_mvcc | yes (full read-back + census) |
| 3 | Go C client | 8 | 21741 | 18405 | 0.591 | 4.5 | 299 | 10.8 / 12.7 | storage 0.61, log 0.57, coordinator 0.57 | 169 / 5.5 | not recorded | yes (full read-back + census) |
| 3 | Rust (C client) | 8 | 27332 | 22990 | 0.214 | 2.3 | 155 | n/a | n/a | 272 / 0.0 | not recorded | yes (full read-back + census) |
| 3 | Rust (C client) | 32 | 41241 | 29187 | 0.229 | 3.3 | 371 | 13.4 / 18.0 | resolver 0.95, storage 0.88, log 0.61 | 354 / 7.0 | no (limited by workload) | yes (full read-back + census) |

What it shows: at the same 8 or 32 in flight the Go own client and Rust write at the same acknowledged rate on three nodes
(41.2k spans/s at 32 in flight each) but Rust needs 3.3 client cores to the Go client's 6.5 (0.23 against 0.42 ms of CPU per
span); the Go C client is slower than both at 8 in flight. The Go own client can be pushed further with writer workers (64: 45.8k,
128: 54.6k, 256: 56.1k acknowledged), but the durable rate does not follow (35.1k at 128, 31.7k at 256) because the storage processes
(0.9 to 0.98 core) and the resolver (about 1.0 core) are saturated. The first 3-node Go C client attempt hit
`transaction_too_old` in the read-back of one generator (verifier limitation under storage lag); the rerun verified clean.

## 3. One database node (`lean-climb-1`)

One `ccx43` database node and one `ccx43` client host (FoundationDB with one node: 6 storage processes). The key range was
pre-split once per variant (16 tenants of 300,000 spans in rounds of four, 305 and 318 partitions), then each step cleared and
restored the stores. Two earlier attempts failed in the pre-split itself because 16 tenants at 32 in flight overloaded one node
(storage queue about 400 MB, storage RSS 3.7 of 4 GiB); those are not client results. The first step and the repeat of the best step were verified by read-back; the other steps are
checked by the executables' counters only.

**Rust (C client)** (pre-split once: 758 s, 305 partitions)

| step | acked spans/s | durable spans/s | client cores / ms per span | busiest database process per role (cores) | storage queue MB / lag s | limiter samples | conflicts | checked |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| 8 in flight (1 process) | 8501 | 6002 | 2.1 / 0.250 | storage 0.89, resolver 0.54, log 0.34, coordinator 0.33 | 394 / 8.8 | workload: 14 | 2 | full read-back + census |
| 16 in flight (1 process) | 11858 | 9022 | 2.8 / 0.236 | storage 0.72, resolver 0.55, log 0.33, coordinator 0.33 | 236 / 5.6 | workload: 15 | 1 | executable counters only |
| 32 in flight (1 process) | 14743 | 10550 | 3.3 / 0.226 | storage 0.78, resolver 0.59, coordinator 0.40, log 0.38 | 252 / 5.9 | workload: 12 | 0 | executable counters only |
| 64 in flight (2 processes x 32) | 10985 | 7025 | 2.8 / 0.251 | storage 0.99, resolver 0.77, log 0.38, coordinator 0.35 | 1528 / 35.6 | workload: 7, storage_server_write_bandwidth_mvcc: 1, storage_server_write_queue_size: 11 | 25 | executable counters only |
| 128 in flight (4 processes x 32) | 12666 | 7780 | 2.8 / 0.222 | storage 1.00, resolver 0.89, coordinator 0.43, log 0.39 | 1194 / 39.1 | workload: 4, storage_server_write_queue_size: 6, storage_server_write_bandwidth_mvcc: 3 | 33 | executable counters only |
| 32 in flight (1 process) | 14497 | 8311 | 3.3 / 0.229 | storage 0.90, resolver 0.60, coordinator 0.37, log 0.36 | 603 / 10.1 | workload: 12 | 0 | full read-back + census |

End of ladder: flat: less than 5% better than the best so far for two steps in a row (best 14743 spans/s at 32 in flight (1 process))

**Go own client** (pre-split once: 762 s, 318 partitions)

| step | acked spans/s | durable spans/s | client cores / ms per span | busiest database process per role (cores) | storage queue MB / lag s | limiter samples | conflicts | checked |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| 8 in flight (1 process) | 11832 | 7491 | 5.5 / 0.463 | storage 0.87, resolver 0.72, log 0.41, coordinator 0.37 | 407 / 7.9 | workload: 10 | 1 | full read-back + census |
| 16 in flight (1 process) | 18039 | 8063 | 6.3 / 0.348 | storage 0.99, resolver 0.95, coordinator 0.42, log 0.40 | 903 / 11.8 | workload: 10 | 1 | executable counters only |
| 32 in flight (1 process) | 19958 | 10504 | 6.3 / 0.315 | resolver 0.97, storage 0.94, coordinator 0.41, log 0.38 | 489 / 7.1 | workload: 9 | 0 | executable counters only |
| 64 in flight (2 processes x 32) | 14738 | 9160 | 5.2 / 0.353 | storage 1.00, resolver 0.98, coordinator 0.43, log 0.42 | 1117 / 28.4 | workload: 6, storage_server_write_queue_size: 2, storage_server_write_bandwidth_mvcc: 2 | 42 | executable counters only |
| 128 in flight (4 processes x 32) | 10363 | 7581 | 4.9 / 0.469 | storage 1.00, resolver 0.95, coordinator 0.34, log 0.32 | 1401 / 57.9 | workload: 7, storage_server_write_queue_size: 11, storage_server_write_bandwidth_mvcc: 2 | 74 | executable counters only |
| 32 in flight (1 process) | 20259 | 11213 | 6.3 / 0.313 | resolver 0.99, storage 0.97, coordinator 0.43, log 0.42 | 522 / 8.1 | workload: 9 | 0 | full read-back + census |
| EXTRA Go-only: 1 process, 32 consumers x 4 writer_workers = 128 in flight | 21701 | 10830 | 6.5 / 0.300 | resolver 1.00, storage 0.98, coordinator 0.44, log 0.43 | 573 / 8.4 | workload: 8 | 0 | executable counters only |
| EXTRA Go-only: 1 process, 32 consumers x 8 writer_workers = 256 in flight | 22932 | 10479 | 6.9 / 0.300 | resolver 1.00, storage 0.97, coordinator 0.39, log 0.37 | 550 / 8.3 | workload: 7 | 0 | executable counters only |

End of ladder: flat: less than 5% better than the best so far for two steps in a row (best 19958 spans/s at 32 in flight (1 process))

On one node the storage process (0.97 to 1.0 core) and, for Go, the resolver (about 1.0 core) are the limit at 32 in flight; more
processes made it worse (storage queue 1.1 to 1.5 GB, lag 28 to 58 s, ratekeeper limiting on the storage write queue,
conflicts 25 to 74). The client was never short of CPU (Rust 2 to 3.3, Go 5 to 7 of 16 cores).

## 4. Six database nodes, generators on the database nodes (`lean-six-1`)

Six `ccx43` database nodes, **one load generator per database node, on the same machine** (the account allowed no further client
hosts, see section 8), so database and generator shared the 16 cores of each node: this is not the setup of sections 2 and 3.
375,000 spans per generator, six generators, conditioned, verified.

| run | acked spans/s | durable spans/s | client cores mean (per host) / ms per span | database cores mean / peak | busiest database process per role (cores) | storage queue MB / lag s | limiter samples | conflicts | verification |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Rust (C client) [six_8] | 47309 | 40648 | 1.5 / 0.170 | 25.5 / 29.9 | storage 0.70, log 0.63, resolver 0.62, coordinator 0.51, commit_proxy 0.42 | 212 / 5.6 | workload: 24 | 1 | full read-back + census passed |
| Go own client [six_8] | 49216 | 42627 | 2.9 / 0.308 | 25.7 / 30.4 | storage 0.68, resolver 0.62, log 0.53, coordinator 0.49, master 0.35 | 195 / 5.5 | workload: 23 | 7 | full read-back + census passed |
| Rust (C client) [six_32] | 69626 | 57845 | 2.1 / 0.159 | 27.6 / 36.0 | storage 0.85, resolver 0.76, commit_proxy 0.67, coordinator 0.51, log 0.50 | 339 / 6.4 | workload: 16 | 13 | full read-back + census passed |
| Go own client [six_32] | 83992 | 55306 | 4.8 / 0.309 | 30.5 / 42.5 | resolver 0.94, storage 0.89, log 0.52, coordinator 0.51, commit_proxy 0.45 | 519 / 9.6 | workload: 13 | 22 | full read-back + census passed |
| Go own client [six_128_go_workers4] | 91679 | 58064 | 5.4 / 0.297 | 29.6 / 43.8 | resolver 0.97, storage 0.92, log 0.51, coordinator 0.49, commit_proxy 0.47 | 689 / 12.0 | workload: 11 | 41 | full read-back + census passed |

Per-node total CPU in use (database processes and generator together, 16 cores per node, averaged over the measured wave; steal
time was 0.0% everywhere):

| run | node CPU busy, mean (min to max over the six nodes) | busiest node, cores of 16 | steal % |
|---|---:|---:|---:|
| Rust (C client) [six_8] | 36% (33 to 38) | 6.1 | 0.0 |
| Go own client [six_8] | 43% (40 to 46) | 7.4 | 0.0 |
| Rust (C client) [six_32] | 43% (40 to 45) | 7.3 | 0.0 |
| Go own client [six_32] | 61% (59 to 66) | 10.5 | 0.0 |
| Go own client [six_128_go_workers4] | 62% (57 to 64) | 10.2 | 0.0 |

The nodes were never short of cores on average (at most 66% of the 16 cores), but a generator and the database processes on one
node contend for caches and for the same network card, so the numbers are not comparable one to one with the separate-client rows.
Rust at 32 in flight was not limited by the database (storage 0.85, resolver 0.76); the Go own client at 32 and 128 was
(resolver 0.94 to 0.97, storage 0.89 to 0.92), reaching 84.0k and 91.7k acknowledged but 55.3k and 58.1k durable. One run
(Rust at 32 in flight) failed first because two entries of the plan with the same stage and arm produced the same run name; the
tool now uses a fresh name per run and the run was repeated.

## 5. Question 1: is a Rust process limited by the C client's single network thread? (`lean-tune-1`)

Three database nodes (baseline layout, six storage processes per node) and **one** client host, so the database has headroom for
one host's load. The key range was pre-split once per variant in one round (4 tenants of 500,000 spans for Rust, 2 of 700,000
for Go). The per-thread CPU of every generator process was sampled from `/proc/<pid>/task/*/stat` once per second; the table
reports the busiest thread of the busiest process over the steady part of the step (first 5 s and last 4 samples dropped). The
first step of each variant was verified by read-back, the other steps by the executables' counters.

**Rust (C client)** (pre-split once: 127 s, 165 partitions)

| step | acked spans/s | durable spans/s | client cores / ms per span | busiest database process per role (cores) | storage queue MB / lag s | limiter samples | conflicts | checked | busiest generator thread (mean / highest 1 s, cores) | that process: total cores, threads |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 process(es) x 32 consumers x 1 workers = 32 in flight | 20977 | 16527 | 2.4 / 0.114 | storage 0.73, resolver 0.69, log 0.58, commit_proxy 0.34 | 242 / 5.5 | workload: 13 | 2 | full read-back + census | fdb-native-netw 0.87 / 0.97 | 2.676, 50 |
| 2 process(es) x 16 consumers x 1 workers = 32 in flight | 27607 | 21454 | 3.2 / 0.115 | storage 0.95, resolver 0.65, log 0.61, coordinator 0.41 | 437 / 11.6 | workload: 20 | 8 | executable counters only | fdb-native-netw 0.60 / 0.82 | 1.74, 34 |
| 2 process(es) x 32 consumers x 1 workers = 64 in flight | 30906 | 25684 | 3.7 / 0.120 | storage 0.91, resolver 0.87, log 0.64, commit_proxy 0.46 | 325 / 9.9 | workload: 18 | 14 | executable counters only | fdb-native-netw 0.78 / 0.85 | 2.213, 50 |
| 4 process(es) x 16 consumers x 1 workers = 64 in flight | 36649 | 24941 | 4.6 / 0.126 | storage 0.93, resolver 0.87, log 0.56, commit_proxy 0.41 | 774 / 23.7 | workload: 27 | 24 | executable counters only | fdb-native-netw 0.49 / 0.69 | 1.315, 34 |

End of ladder: reached the end of the step list

**Go own client** (pre-split once: 139 s, 119 partitions)

| step | acked spans/s | durable spans/s | client cores / ms per span | busiest database process per role (cores) | storage queue MB / lag s | limiter samples | conflicts | checked | busiest generator thread (mean / highest 1 s, cores) | that process: total cores, threads |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 process(es) x 32 consumers x 1 workers = 32 in flight | 30236 | 25119 | 8.0 / 0.266 | resolver 1.00, storage 0.78, log 0.69, coordinator 0.63 | 260 / 5.7 | workload: 13 | 4 | full read-back + census | control-pure 0.59 / 0.64 | 9.832, 19 |
| 1 process(es) x 32 consumers x 2 workers = 64 in flight | 35904 | 24912 | 9.5 / 0.264 | resolver 0.93, storage 0.90, log 0.60, coordinator 0.59 | 374 / 6.1 | workload: 11 | 0 | executable counters only | control-pure 0.64 / 0.77 | 10.692, 19 |
| 2 process(es) x 32 consumers x 1 workers = 64 in flight | 37962 | 24624 | 10.4 / 0.275 | resolver 0.96, storage 0.94, log 0.66, coordinator 0.58 | 672 / 14.9 | workload: 20 | 24 | executable counters only | control-pure 0.34 / 0.42 | 5.709, 22 |

End of ladder: reached the end of the step list

**Answer.** Yes for one Rust process at 32 in flight: its `libfdb_c` network thread (`fdb-native-network`) ran at 0.87 core
on average and 0.97 core in the busiest second, while every `otel-export` thread of the process sat at about 0.04 core and the
database still had headroom (busiest storage process 0.73, resolver 0.69). The cross-check agrees: the same 32 transactions
spread over two processes of 16 acknowledged 27.6k instead of 21.0k spans/s (+32%), each network thread at 0.60 core, and four
processes of 16 acknowledged 36.6k. From there the database, not the client thread, was the limit (storage 0.93, resolver 0.87,
storage queue 774 MB, lag 23.7 s; durable 24.9k against 36.6k acknowledged). The Go own client has no such single thread: its
busiest OS thread ran at 0.34 to 0.64 core in a process of 5.7 to 10.7 cores, one process of 32 consumers wrote 30.2k spans/s,
32 consumers with two writer workers 35.9k, and two processes of 32 consumers 38.0k (resolver 0.96, storage 0.94; durable about
24.6k to 25.1k in all three).

Thread names are the kernel's 15-character names: Rust threads are named (`fdb-native-network`, `otel-export-N`); Go's
runtime threads all carry the executable's name. The sampled window is the steady part of a 20 to 55 second step.

## 6. Question 2: does a database layout with more storage processes and resolvers raise the 3-node ceiling? (`lean-tune-1`)

Three database nodes and three client hosts as in Part A, Go own client, 375,000 spans per generator, conditioned, verified.
Layout A is the baseline of section 2. **Layout B**: 12 storage server processes per node instead of 6, each with
`memory=3GiB`, `cache_memory=1.5GiB` (4 and 2 GiB in layout A; pod limit 44 GiB, request 8 GiB; the 12 processes stay near 36
GiB of the node's 64 GB). **Layout C**: layout B plus twice the resolvers (6 instead of 3) and twice the commit proxies (12
instead of 6); the stateless class grows from 13 to 22 processes.

| run | acked spans/s | durable spans/s | client cores mean (per host) / ms per span | database cores mean / peak | busiest database process per role (cores) | storage queue MB / lag s | limiter samples | conflicts | verification |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Go own client [B_32] | 46044 | 34296 | 6.3 / 0.342 | 17.5 / 28.3 | resolver 0.94, storage 0.83, log 0.63, coordinator 0.60, commit_proxy 0.41 | 322 / 5.7 | workload: 12 | 15 | full read-back + census passed |
| Go own client [B_128_workers4] | 53114 | 37839 | 6.8 / 0.327 | 16.9 / 24.8 | resolver 1.00, storage 0.79, log 0.68, coordinator 0.54, commit_proxy 0.43 | 250 / 5.5 | workload: 10 | 10 | full read-back + census passed |

**Incomplete.** The campaign was stopped on request after layout B; layout C (twice the resolvers and commit proxies) and the
Rust run on the better layout were not run. Baseline for comparison (layout A, section 2, three nodes, same workload): Go own
client at 32 in flight 41,208 acked / 32,047 durable, at 128 (32 consumers x 4 workers) 54,615 / 35,097.

What layout B showed: doubling the storage processes lowered the busiest storage process from 0.89 to 0.96 (layout A) to 0.79 to
0.83 core, raised the durable rate by about 7% (32,047 to 34,296 at 32 in flight; 35,097 to 37,839 at 128) and the acknowledged
rate by 12% at 32 in flight (41,208 to 46,044). The acknowledged rate at 128 in flight was not higher (53,114 against 54,615;
single runs, so this is within what one run can resolve). With layout B the resolver is the one process at about one core
(0.94 and 1.00), so a layout with more resolvers is the test that could show a higher ceiling; it was not measured.

## 7. Costs

| campaign | what | servers, billed hours | EUR |
|---|---|---|---:|
| rehearsals 1 and 2 | stack and tool rehearsals, small profile | small hosts | 0.26 |
| `lean-measured-1` | Part A, section 2 | 6 `ccx43`, 3 h | 8.07 |
| aborted six-node start | the dedicated vCPU limit refused the sixth `ccx43`; four servers torn down | 4 `ccx43`, 1 h | 1.79 |
| `lean-climb-1` | one-node ladder, section 3 (three attempts) | 2 `ccx43`, 2 h | 1.79 |
| `lean-six-1` | six nodes with co-located generators, section 4 | 6 `ccx43`, 3 h, plus one `cpx62` for 1 h and a probe server | 8.39 |
| `lean-tune-1` | sections 5 and 6, stopped on request | 6 `ccx43`, 1 h | 2.69 |
| total | | | 22.99 |

Price per hour with the IPv4 (gross, `hel1`): `ccx43` 0.448 EUR, `cpx62` 0.212 EUR. Every started hour is billed. Each campaign was
torn down by label and the account listing compared with a snapshot taken before it started.

## 8. Limits and caveats

- One run per row, 20 to 60 seconds of ingest, no repeats except where a table says so. Differences of a few percent are not
  resolved by these numbers; the differences discussed above are 20% and more.
- **Acked is not durable.** With the database saturated the acknowledged rate runs ahead of the durable rate; the durable rate
  is the one to plan with.
- The comparison is on the Stage 0 collector-queue workload at 8 to 256 in flight per generator with 11 key writes per span;
  it says nothing about other workloads, redundancy modes or sustained load.
- Section 4 shares each machine between database and generator; sections 2, 3, 5 and 6 use separate client hosts.
  Where the table says "executable counters only", the run was not read back.
- **Account limits met**: dedicated vCPU limit 96 (six `ccx43` and nothing else dedicated); shared vCPU limit between 34 and 37
  in total on the account used (a `cpx62` plus other servers hit it with the second `cpx62`); `cx*` server types had no capacity
  in `hel1`; a spread placement group holds at most ten servers; a burst of parallel SSH connections to one host is refused and
  retried.
- Per-process memory of the baseline storage processes was 3.0 to 3.8 GB RSS against a 4 GiB budget; the one-node pre-split of 16
  tenants at once exceeded what the node could take.
