# FDB Record Layer Go — Performance Analysis

**Date:** 2026-04-16
**Cluster:** Hetzner cx23 (2 vCPU, 4GB RAM, SSD) — 3 and 6 node configurations
**FDB version:** 7.3.46, Go Record Layer with pure Go FDB client (no CGo)

## Executive Summary

The Go Record Layer processes records at **1us CPU per record** in batch mode. Real-world throughput is limited by FDB server CPU, not the Go client. On a 3-node cx23 cluster, we achieve **12-13K events/sec** via HTTP; on 6 nodes, **18K events/sec**. The Go backend uses only 22-24% CPU at peak load — FDB consumes the rest.

## Micro-benchmarks (local FDB testcontainer)

### Store construction modes

Three modes matching Java's `FDBRecordStore.Builder`:

| Mode | Go API | Java equivalent | FDB reads per tx | Use case |
|---|---|---|---|---|
| Full open | `Open()` | `uncheckedOpenAsync()` | 2 GetRange | Dev/testing |
| Lazy-load | `Build()` | `build()` | 0 (deferred to first index op) | General production |
| Zero-read | `Build()` + `SetAssumeAllIndexesReadable(true)` | No equivalent | **0** | High-throughput production |

### Single-record operations

| Operation | Open() | Build+Assume | Improvement |
|---|---|---|---|
| SaveRecord | 81 allocs, 1,009us | **40 allocs, 1,007us** | -51% allocs |
| LoadRecord | 71 allocs, 171us | **27 allocs, 59us** | -62% allocs, 2.9x faster |
| DeleteRecord | 78 allocs, 1,010us | — | — |

SaveRecord and DeleteRecord latency is dominated by FDB round trips (loadExistingRecord Get + Commit). LoadRecord latency drops 2.9x with Build+Assume because the 2 store-state GetRange reads (~120us) are eliminated.

### Batch operations

| Operation | Allocs | Per-record | Records/sec |
|---|---|---|---|
| InsertBatch(50+indexes) | 495 | 20.6us | **48,309** |
| InsertBatch(500+indexes) | 4,073 | 13.3us | **75,415** |
| SaveRecordBatch(10) | 373 | 186.4us | 5,368 |

InsertBatch is the maximum-throughput path: no read-before-write, disabled RYW cache, disabled write conflict ranges. Safe when primary keys are guaranteed unique (UUIDs, monotonic IDs).

### Scan operations

| Operation | Allocs | Per-entry | Latency |
|---|---|---|---|
| ScanIndex(100) | 673 | 6.7 | 547us |
| ScanRecords(100) | 1,294 | 12.9 | 628us |

ScanRecordsByType uses prefix scan when primary key has RecordTypeKey() as first component — O(records of that type) instead of O(all records). This gave **100x speedup** on a store with 400K+ events (5-7s → 60ms for ListCustomers).

## Allocation floor analysis

At 40 allocs for SaveRecord+Build+Assume, every remaining allocation is structural:

| Category | Allocs | What |
|---|---|---|
| FDB client I/O | 8 | Reply channel, frame payload, buffer pool, pending future |
| RYW cache | 4 | Map init + value copy + string key + map entry (per tx.Set) |
| Key/tuple packing | 4 | Record key, index key, PK eval, concat |
| Transaction + store | 3 | Tx struct, store struct, indexStates map |
| Conflict ranges | 2 | Read + write conflict for the record key |
| Proto serialization | 1 | Marshal buffer for FDB value |
| Test harness | 4 | Proto message creation (not production) |

CPU profile of SaveRecord shows **69% network I/O, 5% GC, 26% Go code**. The record layer processes records at **1us each** in InsertBatch — application code is negligible.

## Real-world throughput (HTTP stack)

### Test setup
- Client: local machine → internet → Hetzner LB
- Backend: metrognome (ConnectRPC + FDB Record Layer Go)
- Stack: HTTP client → Envoy LB → Go net/http → ConnectRPC proto deser → Record Layer → FDB client → FDB server

### 3-node cluster results

| Path | Workers | Batch | events/sec |
|---|---|---|---|
| Bulk (InsertBatch, no dedup) | 200 | 200 | **13,721** |
| Bulk | 100 | 200 | 12,835 |
| Bulk | 50 | 200 | 11,225 |
| Standard (with idempotency dedup) | 10 | 100 | 130 |

Standard ingest with dedup: each event does a pipelined GetRange on the unique idempotency index + InsertBatch for non-duplicates. The dedup reads are pipelined (all N fired at once), but each GetRange response still takes ~1ms from the storage server.

### 6-node cluster results

| Config | Commit proxies | GRV proxies | Peak events/sec |
|---|---|---|---|
| 3 nodes, defaults | 1 | 1 | 13,721 |
| 6 nodes, defaults | 3 | 1 | 15,954 |
| **6 nodes, tuned** | **3** | **3** | **18,179** |

Scaling from 3→6 nodes gave 1.3-1.5x throughput (not 2x) due to uneven FDB role distribution — one node saturated at 99% CPU running grv_proxy + 2x log.

### Read performance under write load

During 18K events/sec bulk ingest:

| Query | Latency |
|---|---|
| ListCustomers (600K+ events) | **45-115ms** |
| GetUsage (O(1) atomic SUM index) | **52ms** |

FDB's MVCC isolation keeps reads consistent without blocking on concurrent writes.

## CPU breakdown during load

### 3-node cluster at 12K events/sec

Per node (2 vCPU):

| Process | CPU% | Role |
|---|---|---|
| fdbserver | **83-91%** | Commit proxy + storage + log |
| metrognome (Go) | **22-26%** | HTTP + proto + record layer + FDB client |
| envoy | 2-3% | HTTP proxy |
| kernel (softirq) | 8% | Network |
| **Total** | **~80%** of 200% | |

**FDB is the bottleneck, not Go.** The Go backend has 3-4x headroom. FDB's single-threaded commit proxy + log processes saturate one core.

### 6-node cluster at 18K events/sec

| Node | FDB CPU | Go CPU | Saturated? |
|---|---|---|---|
| 10.0.1.10 | 41% | ~22% | No |
| 10.0.1.11 | 36% | ~22% | No |
| 10.0.1.12 | 75% | ~22% | No |
| 10.0.1.13 | 59% | ~22% | No |
| **10.0.1.14** | **99%** | ~22% | **Yes — grv_proxy + 2x log** |
| 10.0.1.15 | 56% | ~22% | No |

One node saturated due to FDB concentrating grv_proxy + 2 log roles. The Go backend was at 22% across all nodes — nowhere near saturation.

## Bottleneck analysis

### What limits throughput

1. **FDB commit proxy CPU** — single-threaded actor processing all commits. Configurable via `commit_proxies=N`.
2. **FDB GRV proxy CPU** — single-threaded, handles all GetReadVersion requests. Configurable via `grv_proxies=N`.
3. **FDB log CPU** — transaction log writes. More logs = more write capacity.
4. **FDB role co-location** — all roles share the same 2 vCPUs per node.

### What does NOT limit throughput

1. **Go Record Layer** — 22-26% CPU at 12-18K events/sec. Has 3-4x headroom.
2. **Disk I/O** — SSDs at 14-49% utilization. Not saturated.
3. **Network** — 0.05-0.1 Gbps per node. cx23 has 1 Gbps NIC.
4. **Memory** — 3.2-3.3 GB available per node. No pressure.

### Scaling projections

| Config | Est. events/sec | Rationale |
|---|---|---|
| 3× cx23, current | 12-13K | Measured |
| 6× cx23, tuned | 18K | Measured |
| 6× cx23, optimal role distribution | ~25K | Eliminates hot-node bottleneck |
| 6× dedicated FDB + 6× app-only | ~50K | FDB gets full CPU, no sharing |
| 12× cx23 | ~35-40K | Linear scaling of commit proxies |
| 3× cx43 (8 vCPU) | ~40K | FDB gets 4+ cores per node |

## Optimizations applied

### FDB client (pkg/fdbgo)
- ReplyHandle pool — eliminates closure alloc per RPC
- FrameReader persistent header buffer — eliminates 2 stack escapes per frame read
- Read conflict buffer pooling — addReadConflict + addReadConflictForKey use shared conflictBuf
- isFutureVersionOrProcessBehind nil short-circuit — avoids errors.As on happy path
- Deep-copy selfConflicts in OnError — fixes conflict buffer lifecycle on MAYBE_COMMITTED retry
- sync.Map → map+RWMutex for subspace key cache — compiler optimizes string([]byte) lookup
- Remove sync.Once from pendingFutureByteSlice — single-goroutine future doesn't need it

### Record layer (pkg/recordlayer)
- **ScanRecordsByType prefix scan** — 100x speedup for typed queries on multi-type stores
- evaluateKeyFlat in SaveRecord — flat evaluator for PK extraction
- PackConcatWithPrefix in loadWithSplit — eliminates appendToTuple intermediate alloc
- Inline SUM mutation in InsertBatch — avoids encodeRecordCount allocation
- StoreBuilder subspace key caching — avoids sync.Map lookup per Open
- ensureStoreStateLoaded with sync.Once — Java-compatible lazy store state loading
- SetAssumeAllIndexesReadable — explicit opt-in to skip lazy-load entirely
- FieldKeyExpression atomic cache — fixes pre-existing data race with atomic.Pointer
- API key auth cache — 60s TTL in-memory cache eliminates FDB read per HTTP request

### Data race fixes
- FieldKeyExpression.cachedFD — concurrent read/write on shared metadata. Fixed with atomic.Pointer.
- All changes verified with `-race` flag across recordlayer, client, conformance, and chaos tests (0 races).

## Future optimization opportunities (require API changes)

1. **vtprotobuf message pooling** — save ~4 allocs on deserialization. Requires caller lifecycle management (ReturnToPool). Needs design approval.
2. **Iterator-style scan API** — reuse IndexEntry/continuation across entries. Requires callers to acknowledge they won't hold references.
3. **Typed tuple decode** — avoid int64→any boxing in fastDecodeInt. Requires splitting tuple API into typed/untyped variants.
4. **Transaction struct pooling** — save 1 alloc. Complex reset logic with sync primitives.

## Java compatibility

All optimizations maintain Java wire compatibility and semantic behavior:
- Build() matches Java's Builder.build() — zero FDB reads, lazy-load on first index operation
- Open() matches Java's uncheckedOpenAsync() — reads store state
- CreateOrOpen() matches Java's createOrOpenAsync() — reads state + version check + possible rebuild
- SetAssumeAllIndexesReadable goes beyond Java (Java always lazy-loads)
- ScanRecordsByType prefix scan matches Java's RecordTypeKeyComparison optimization
- 433 conformance specs pass (Go↔Java cross-compatibility verified)
