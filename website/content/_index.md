---
title: "fdb.dev — FoundationDB for Go"
layout: hextra-home
toc: false
---

{{< hextra/hero-badge link="docs" >}}
  <div class="hx:w-2 hx:h-2 hx:rounded-full hx:bg-primary-400"></div>
  <span>Pure Go · CGO_ENABLED=0 · wire-compatible with Java Record Layer 4.12.11.0</span>
{{< /hextra/hero-badge >}}

<div class="hx:mt-6 hx:mb-4">
{{< hextra/hero-headline >}}
  FoundationDB for Go
{{< /hextra/hero-headline >}}
</div>

<div class="hx:mb-6">
{{< hextra/hero-subtitle >}}
  A from-scratch FoundationDB client in pure Go — no cgo, **2–4× faster reads** than libfdb_c. Plus a wire-compatible Record&nbsp;Layer and SQL engine, so Go and Java services share one cluster, byte-for-byte.
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx:mb-16">
{{< hextra/hero-button text="Get started  →" link="docs" >}}
&nbsp;&nbsp;
{{< hextra/hero-button text="Star on GitHub" link="https://github.com/birdayz/fdb-go" style="background:transparent;border:1px solid var(--hextra-primary-color, #888);color:inherit" >}}
</div>

<div class="hx:max-w-2xl hx:mx-auto hx:mb-6 hx:w-full">

```go
import "fdb.dev/pkg/fdbgo/fdbclient" // pure Go. CGO_ENABLED=0. static binary.

db, _ := fdbclient.Open("/etc/foundationdb/fdb.cluster")
```

</div>

<div class="hx:max-w-2xl hx:mx-auto hx:mb-20 hx:w-full hx:text-center hx:text-sm hx:opacity-70">

Pre-1.0, built in the open. The wire format is the hard line — conformance- and differential-tested
against the Java reference. Pin a commit and run the suites before production. [Maturity →](docs/maturity)

</div>

{{< hextra/hero-section heading="h2" >}}No&nbsp;cgo. Static&nbsp;binary. Faster&nbsp;reads.{{< /hextra/hero-section >}}

<div class="hx:max-w-3xl hx:mx-auto hx:w-full">

Most FDB tooling links Apple's C library through cgo — no static binaries, painful cross-compilation,
a glibc dependency. The pure-Go client speaks the FoundationDB wire protocol directly (validated
against libfdb_c **7.3.77**) and is the default backend. It produces byte-identical reads and writes.
On the read path it is **2–4× faster** — the Go runtime skips the C client's network-thread hop and
multi-version-client shim; writes go through the same commit path and run at parity.

| Benchmark | pure-Go | libfdb_c | Speedup |
|---|---:|---:|:--|
| Get (100 B) | 60 µs | 218 µs | **3.6×** |
| Get (1 KB) | 61 µs | 209 µs | **3.4×** |
| GetRange (100 keys) | 92 µs | 363 µs | **3.9×** |
| Read @ 10 ms RTT (tc netem) | 5 254 µs | 12 635 µs | **2.4×** |
| Set + Commit | 1 008 µs | 1 005 µs | 1.0× |

The **10 ms-RTT row is the realistic signal** — localhost microbenchmarks are syscall/IPC-bound, so
they're the optimistic end. The read advantage holds under real network latency (2.4× at 10 ms) and
converges to parity at extreme RTT, where both clients are network-bound. Writes are at parity.
Reproducible from [`TestBenchmarkSanity`](https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/bench/bench_test.go);
method and analysis in [`PERFORMANCE.md`](https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/bench/PERFORMANCE.md).
Prefer Apple's C client? One build tag (`-tags libfdbc`) swaps it in — same bytes on the wire either way.

</div>

{{< hextra/hero-section heading="h2" >}}A&nbsp;client, and&nbsp;layers&nbsp;on&nbsp;top.{{< /hextra/hero-section >}}

<div class="hx:max-w-3xl hx:mx-auto hx:w-full hx:text-center hx:mb-6">

FoundationDB is an ordered, transactional key-value store — strict-serializable ACID, fault-tolerant,
proven by deterministic simulation testing. It's the storage substrate under systems like Snowflake
and Apple's CloudKit. Higher-level data models are built as **layers** on top. `fdb.dev` is the Go
client and a growing set of them.

</div>

{{< hextra/feature-grid cols="3" >}}

  {{< hextra/feature-card
    title="Pure-Go Client"
    icon="lightning-bolt"
    subtitle="A from-scratch FDB wire-protocol client — no cgo, faster reads, read-your-writes, retries, commit_unknown_result handling. Validated against libfdb_c 7.3.77."
  >}}

  {{< hextra/feature-card
    title="Record Layer"
    icon="database"
    subtitle="Structured records, secondary indexes, versions, continuations, split records, schema evolution. Record format byte-identical to Java — share a cluster."
  >}}

  {{< hextra/feature-card
    title="SQL Engine"
    icon="search"
    subtitle="A database/sql driver backed by a Cascades optimizer ported from Java's fdb-relational-core: index selection, sort elimination, streaming aggregation."
  >}}

{{< /hextra/feature-grid >}}

{{< hextra/hero-section heading="h2" >}}Share&nbsp;a&nbsp;cluster&nbsp;with&nbsp;Java.{{< /hextra/hero-section >}}

<div class="hx:max-w-3xl hx:mx-auto hx:w-full hx:text-center">

Wire compatibility is the project's hard line, not a feature. Record, index, version, continuation,
and split-record formats are **byte-identical to Java Record Layer 4.12.11.0**, and the client speaks
the FoundationDB **7.3** wire protocol (validated against 7.3.77; 8.0 is future work). Enforced in CI
against real FoundationDB by a Java conformance suite, a cross-backend differential, and a
binding-stress tester — not mocks.

</div>

<div class="hx:mt-12 hx:mb-8 hx:text-center">
{{< hextra/hero-button text="Get started  →" link="docs" >}}
</div>
