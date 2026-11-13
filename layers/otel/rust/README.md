# Native Rust OTEL_V1 span writer (experimental)

This is an initial **span-only, in-process** Rust alternative to the Go
`SpanWriter` and custom Collector model. It is not an upstream OpenTelemetry
Collector component, OTLP HTTP/gRPC receiver, SQL engine, or full GenAI Collector
replacement. There are no measured performance or economics results yet.

## Implemented source surface

- The full pinned OTLP and OTEL_V1 protobuf descriptors are generated from the
  Go dependencies and compiled schema, not handwritten reduced OTLP structs.
  Reflection preserves unknown fields, open enums, original attributes and
  Resource/Scope wrappers in each span's stored single-span `ResourceSpans`.
- `row::Schema` builds the compiled SPANS row representation, canonicalizes
  last-value-wins projected attributes within the same caps, and builds the
  eight standard span VALUE index entry shapes. Message decomposition is off.
- `writer::Writer` prepares a whole call before writing, keeps the last duplicate
  identity at its original position, batches at most 64 spans within the 300,000
  byte estimate, reads prior images, retracts old index entries and split values,
  and writes records plus indexes atomically. The actual transaction guard is
  500,000 bytes; oversized replacements are split into smaller transactions or
  fail alone. Committed earlier batches remain committed on later failure.
- `storage` implements uncompressed and Java-readable version-1 zlib transformed
  records and 100,000-byte split chunks (unsplit suffix 0, split suffixes 1+).
  Deflate implementations need not produce identical compressed bytes: compare
  decoded unions and separately report stored/physical bytes. Compression never
  removes index work.
- `native::NativeDatabase` dynamically loads **only an explicit SHA256-matched
  libfdb_c 7.3.77 file**, pinned in `MODULE.bazel`. It uses API 730, a one-shot
  owned network thread, native serializable reads/OnError, retry limit -1 and a
  10-second timeout on the same transaction across retries. Caller cancellation
  cancels the native transaction; failure/uncertain commit is not an ACK.
  It installs nothing globally and starts no FDB server. Executable client code
  stays pinned until process exit (the C runtime is process-global/one-shot).
  Irreversible network-loop failure returns an operation error rather than
  waiting for an unreachable callback. Late callback tokens safely no-op; pending
  **native C allocations may remain until process exit** because C cancellation
  of plain futures itself needs that network thread. This is not live-ABI proof.
- `collector::Collector` is a span-bounded queue including in-flight exports,
  with overflow rejection/blocking, bounded batch sizes, timeout flushing,
  exponential retry, permanent-error request isolation, Flush sequence barriers,
  draining shutdown and cancellation. Admission is **not** a durable ACK.
- The binary is a closed-loop, finite-corpus **writer driver**, not the queued
  Collector benchmark. It requires an explicit JSON job with absolute client,
  cluster, layout, framed corpus and new output paths; corpus/binary SHA256,
  source revision, expected frame count, row budget, batch size, workers,
  record compression and absolute run timeout. Corpus frames have a four-byte
  big-endian length followed by one single-span ResourceSpans. It records raw
  call latencies, acknowledged progress and failures. Its job/manifest interfaces
  are experimental, not a cloud execution admission mechanism.

A layout manifest is an immutable control-plane description of an **exclusively
owned** tenant: record/index prefixes, exact expected header/layout/catalog
values, empty index-state ranges and descriptor digest. Each transaction checks
these. The writer does not resolve, create, upgrade or adopt relational catalogs.
A forthcoming Go adapter must derive and validate this manifest from real
OTEL_V1 metadata, outside measurement; it must never perform Rust ingestion.
Concurrent schema/catalog evolution is not supported by this experiment.

## Validation and limitations

```sh
# Standalone Rust checks, not verification of any Go change.
CARGO_BUILD_JOBS=4 cargo test --locked --manifest-path layers/otel/rust/Cargo.toml
CARGO_BUILD_JOBS=4 cargo build --locked --release --manifest-path layers/otel/rust/Cargo.toml
```

`rust-toolchain.toml` declares Rust 1.99.1; Cargo.lock seals dependencies. Default
Cargo tests start no database, network listener, Docker container or service.
`tests/compatibility.rs` compares nine nonempty vectors against Go
`BuildCompiled` rows and the **metadata expressions'** complete index/primary
keys, not a second handwritten Go index model. `fixture_test.go` checks the
committed vectors/descriptors for drift in the whole `just test` lane. Generate
into a fresh directory with the Bazel-built `otelrust-fixture` and inspect/copy
both outputs; it has no database access.

The vectors include fixture V2 and unknown tag 536870911 on wrappers, resource,
scope and span, open enums, duplicate/empty/NUL/long/capped attributes, NaN and
negative zero, non-string attributes, events/links and uint64 end timestamps.
Transaction and queue tests are deterministic fakes testing the state machines;
**they do not establish live FDB, native ABI, conflict, durability, cancellation
or Go/Java cross-read conformance**. Native FDB integration and independent Go
record/index census plus the complete query oracle remain mandatory before a
benchmark can be accepted. No service/cloud execution is authorized by this file.

Still missing: the live tenant/corpus/control adapter, matched Go-native driver,
live cross-read and fault cases, producer/cluster telemetry and a validated
comparison runner; logs, GenAI message dedup/restoration, message TEXT indexes,
joined queries, metrics, upstream Collector integration and OTLP transport.
The binary does not yet drive the queue. These are not silently replaced by a
null sink, a decode-only benchmark, or a Go-backed Rust ingest proxy.

## Comparison boundary to preregister

Read [the horizontal protocol](../bench/ops/horizontal/README.md),
[the historical span campaign](../../../docs/benchmarks/otel-ingest-25k-k8s-cilium-scaling.md)
and [the GenAI Collector report](../../../docs/benchmarks/otel-genai-collector-ingest-k8s-scaling.md).
The latter measured correlated logs and deduplicated messages with TEXT indexes;
its historical profile put roughly two-thirds of CPU in message persistence and
only roughly 4% in span ingest. This span-only milestone cannot claim to remove
that bottleneck or establish full Collector economics.

The initial comparison must distinguish:

1. Rust native-C writer versus Go native-C `SpanWriter` (matched client control,
   using existing `pkg/fdbgo/libfdbc` / `NewFDBDatabaseWithBackend`).
2. Rust native-C versus the Go production pure client (whole implementation
   comparison; **not** causal evidence for Rust, GC or language economics).
3. A later equal queued in-process comparison, separately from any future
   matched OTLP receiver comparison.

For `scaling.py` methodology, Nx means **N FDB/storage hosts + N producer
hosts**, independent tenant stores in one single-R1/non-HA FDB cluster, not N
Collector replicas against an unchanged or hidden storage budget. Load-generator
placement, all held hosts and every active host must be explicit. Do not copy old
approvals, historical rates, expired deadlines or obsolete custom-store flags.
Preregister fixed source/binary/compiler/client seals, common payload bytes,
compression, complete indexes, batch/concurrency/guards, fresh-state paired and
interleaved repeats, warmup/steady windows, latency/error objective, p50/p95/p99,
CPU/RSS/allocation availability, physical network/storage counters, exact durable
census, 1,600-query source oracle, ACK and strict durable-drained timing. The
historical 600-second run **included** its 60-second warmup, not 600+60.

A paid campaign still needs new explicit admission covering exact hardware,
topology, current captured prices, total cost ceiling, absolute lifetime, global
row budget, source seals and independent cleanup. Cost includes **all billable
hosts for their held duration**, not merely active-stage hosts; report cost per
million audited spans and per actual ingested source GB with that denominator.
A lower Rust cost is a hypothesis, not an acceptance criterion or assumed result.
