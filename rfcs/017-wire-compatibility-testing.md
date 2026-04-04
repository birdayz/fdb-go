# RFC 017: Wire Compatibility Testing Strategy

## Status: Accepted

## Problem

The pure Go FDB client must produce **byte-identical wire messages** to the C client (`libfdb_c.so`) and behave identically for all observable operations. "Works against the server" is necessary but insufficient — the server may accept slightly malformed messages today and reject them in a future version.

We need a testing strategy that gives high confidence in wire compatibility without requiring us to port FDB's internal simulation framework.

## Current state

The FDB binding tester (seeded PRNG fuzz test, `bindingtester.py`) validates behavioral correctness: 145 seeds x 1000 ops = 145,000 operations, 0 failures. This covers GET/SET/CLEAR/GET_RANGE, atomic ops, conflict ranges, key selectors, error handling, retry semantics, and versionstamps.

Ground truth size comparison: 10/10 message types match C++ ObjectWriter output size. Byte differences limited to ReplyPromise tokens (expected — each client generates unique reply routing UIDs).

### What the binding tester does NOT cover

- **Byte-level serialization identity** — server accepting our bytes != bytes identical to C client. Fields we serialize incorrectly but the server ignores today could break on FDB upgrades.
- **Large payloads** — binding tester uses small random keys/values. No testing near FDB's 100KB value limit or 10MB transaction limit. Alignment/padding bugs may only manifest at certain sizes.
- **All message types** — only exercises GET_VALUE, GET_KEY, GET_KEY_VALUES, COMMIT, GET_READ_VERSION. No WATCH, GET_MAPPED_KEY_VALUES, tenant operations.
- **Multi-node clusters** — proxy failover, shard splits mid-transaction, resolver conflicts. Single-node Docker only.
- **Concurrent connections** — `--no-threads` mode only. No connection pooling, no concurrent RPCs.
- **FDB version compatibility** — only tested against 7.3.75.

## Considered: FDB Deterministic Simulation Testing (DST)

FDB's DST framework (`fdbserver -r simulation`) is the gold standard for testing distributed database behavior. It replaces all I/O with deterministic simulated I/O, allowing replay of entire cluster scenarios including disk failures, network partitions, and process kills.

### Why we can't use DST

**Flow runtime incompatibility.** Flow is FDB's cooperative single-threaded async framework. The simulation replaces `INetwork` with `Sim2` which controls time, scheduling, disk I/O, and network. Integrating our Go client would require:

1. **Running Go inside the C++ process** (cgo reverse: C++ calls Go). This works mechanically but the Go runtime brings its own thread pool, GC, and scheduler. Flow's determinism depends on everything running in one thread with cooperative scheduling. The Go GC alone introduces nondeterminism.

2. **Replacing goroutines with Flow tasks.** Goroutines use the Go runtime scheduler, which is fundamentally incompatible with Flow's cooperative model. Options:
   - Rewrite Go client in callback style using Flow futures — defeats the purpose of having a Go client.
   - Run Go in a separate OS thread synchronized with Flow via message queue — breaks determinism.

3. **Link-time syscall replacement.** FDB's simulation literally replaces `malloc`, `clock_gettime`, `send`, `recv` at link time. Go's runtime doesn't go through those — it has its own `mmap`-based allocator and `epoll` loop.

### What DST would give us that we don't need

DST's power is testing **server-side** behavior under fault injection. That tests FDB itself, not our client. For client wire compatibility, we need to verify:
- Our serialized bytes match C++ (serialization correctness)
- Our observable behavior matches the C client (behavioral correctness)

Neither requires simulating server faults. We can test fault tolerance separately with a Go-native TCP proxy that injects faults — much simpler than porting Flow.

### Verdict

The effort to integrate with DST (partial Flow port, cgo reverse bindings, determinism guarantees) is disproportionate to the value. DST tests the server; we need to test the client. The tools below achieve the same client-side confidence at a fraction of the cost.

## Proposed tools

### Tool 1: Differential serialization fuzzer (HIGH priority)

**Goal:** For every message type, verify that Go `MarshalFDB` produces byte-identical output to C++ `ObjectWriter` for arbitrary inputs.

**Architecture:**

```
                    ┌───────────────┐
  random fields ──→ │ C++ harness   │ ──→ bytes ──┐
  (JSON stdin)      │ (ObjectWriter)│              │
                    └───────────────┘              ├── compare
                    ┌───────────────┐              │
  random fields ──→ │ Go harness    │ ──→ bytes ──┘
  (JSON stdin)      │ (MarshalFDB)  │
                    └───────────────┘
```

**Implementation:**
1. Extend `cmd/fdb-schema-extract` to accept field values via stdin (JSON), serialize with C++ ObjectWriter, emit raw bytes to stdout.
2. Go side: same JSON input, call `MarshalFDB`, emit raw bytes.
3. Fuzz loop: generate random struct values → feed to both → compare output bytes.
4. Use Go's native fuzz testing (`go test -fuzz`) for the Go side, with the C++ harness as an oracle.

**Properties:**
- No FDB cluster needed. Pure serialization comparison.
- Deterministic — same input = same output, no timing dependencies.
- Fast — millions of iterations per second (no network, no Docker).
- Covers ALL message types, not just the ones the binding tester exercises.
- Catches: padding, alignment, field ordering, vtable packing, optional handling, vector encoding, empty field sentinels, nested struct recursion, KeyRangeRef optimizations.

**What it doesn't cover:** Protocol framing, connection negotiation, endpoint token routing — these are tested by the binding tester.

### Tool 2: Cross-client interop tests (MEDIUM priority)

**Goal:** Verify that data written by the Go client is readable by the C client and vice versa.

**Architecture:**

```
  Go pure client ──write──→ FDB ←──read── Go CGo client
  Go CGo client  ──write──→ FDB ←──read── Go pure client
```

**Implementation:** Go integration test using both `pkg/fdbgo/client` (pure Go) and `github.com/apple/foundationdb/bindings/go/src/fdb` (CGo) against the same FDB container.

**Test cases:**
- Key/value round-trip (Go writes, C reads, values match)
- Range scan consistency (both clients see same key order and values)
- Versionstamp round-trip (Go writes versionstamped key, C reads it)
- Conflict detection (Go read conflict range, C write in range, Go commit fails)
- Atomic operations (Go ADD, C reads result)
- Transaction isolation (snapshot reads, concurrent writes)

**Properties:**
- Tests real shared state, not just serialization.
- No timing/nondeterminism issues — interop is within a single test function.
- Runs against testcontainers (real FDB).

### Dropped: Wire proxy comparator

Capturing TCP frames from both clients running the same binding tester seed and diffing them does not work. The frame sequences diverge due to:

- **GRV values** — each client gets its own read version. Different version numbers = different `ReadSnapshot` in commits.
- **Reply tokens** — each client generates unique UIDs for reply routing.
- **Retry timing** — backoff delays differ. Different retry counts = different frame sequences.
- **Shard cache state** — location cache misses depend on timing. Different miss patterns = different `GetKeyServerLocationsRequest` frames.
- **Connection state** — TCP buffering, PING timing, keepalive intervals.

A semantic normalization layer could theoretically strip all dynamic fields and compare structure, but the complexity exceeds the value when tools 1 and 2 already cover serialization identity and behavioral correctness independently.

## Fault injection (future)

For testing client resilience to server failures (connection drops, commit_unknown_result, proxy failover), we don't need DST. A Go-native TCP proxy that injects faults at the wire level is simpler and tests our actual production code path:

```
  Go client ──→ fault proxy ──→ FDB
                    │
              drop/delay/corrupt frames
```

We already have a chaos testing framework (`pkg/recordlayer/chaos/`) that tests at the FDB transaction level. Wire-level fault injection would complement it by testing the transport layer.

This is lower priority than tools 1 and 2. The binding tester already exercises error handling and retry logic through the ON_ERROR instruction.

## Implementation plan

| Phase | Tool | Effort | Confidence gain |
|---|---|---|---|
| 1 | Differential serialization fuzzer | 2-3 days | Byte-identical serialization for all types |
| 2 | Cross-client interop tests | 1 day | Shared-state correctness |
| 3 | Wire fault proxy | 2 days | Transport resilience (future) |

Phase 1 is the foundation — if the serialized bytes aren't identical, nothing else matters. Phase 2 validates end-to-end correctness with real shared state. Phase 3 is optional until we need multi-node or high-availability testing.

## References

- `pkg/fdbgo/client/CRASH_BUG.md` — debugging playbook (addr2line, wire log capture, FDB debug symbols)
- `cmd/fdb-binding-stress/` — binding tester stress tool (multi-seed, artifact collection, JSON report)
- `pkg/fdbgo/wire/types/ground_truth_test.go` — current ground truth size comparison (10/10 match)
- `cmd/fdb-schema-extract/main.cpp` — C++ extractor/serializer (basis for differential fuzzer)
- FDB DST paper: "Testing Distributed Systems w/ Deterministic Simulation" (FoundationDB, 2014)
- `flow/flat_buffers.h` — C++ ObjectWriter (the serialization oracle)
