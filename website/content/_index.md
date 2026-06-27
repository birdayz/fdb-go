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
  A from-scratch FoundationDB client in pure Go — no cgo, **2–4× faster reads** than libfdb_c — with a wire-compatible Record&nbsp;Layer and SQL engine. Go and Java services share one cluster, byte-for-byte.
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
against the Java reference. Pin a commit and run the suites before production. [Maturity →](docs)

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
| Get (10 KB) | 69 µs | 217 µs | **3.1×** |
| GetRange (100 keys) | 92 µs | 363 µs | **3.9×** |
| Sustained read throughput | 430 MB/s | 191 MB/s | **2.3×** |
| Read @ 10 ms RTT (tc netem) | 5 254 µs | 12 635 µs | **2.4×** |
| Set + Commit | 1 008 µs | 1 005 µs | 1.0× |

The **10 ms-RTT row is the realistic signal** — localhost microbenchmarks are syscall/IPC-bound. The
read advantage holds under real network latency (2.4× at 10 ms) and converges to parity at extreme
RTT, where both clients are network-bound. Writes are at parity throughout. Reproducible from
`TestBenchmarkSanity`; analysis in `PERFORMANCE.md`. Prefer Apple's C client? One build tag
(`-tags libfdbc`) swaps it in — same bytes on the wire either way.

</div>

{{< hextra/hero-section heading="h2" >}}A&nbsp;client, and&nbsp;layers&nbsp;on&nbsp;top.{{< /hextra/hero-section >}}

<div class="hx:max-w-3xl hx:mx-auto hx:w-full hx:text-center hx:mb-6">

FoundationDB is an ordered, transactional key-value store — strict-serializable ACID, fault-tolerant,
proven by deterministic simulation testing. It's the storage substrate under systems like Snowflake
and Apple's CloudKit. Higher-level data models are built as **layers** on top of that core. `fdb.dev`
provides the Go client and a growing set of layers.

</div>

{{< hextra/feature-grid cols="3" >}}

  {{< hextra/feature-card
    title="Pure-Go Client"
    icon="lightning-bolt"
    subtitle="A from-scratch FDB wire-protocol client — no cgo, faster reads, read-your-writes, retries, commit_unknown_result handling. Validated against libfdb_c 7.3.77 by a binding tester."
  >}}

  {{< hextra/feature-card
    title="Record Layer"
    icon="database"
    subtitle="Structured records, secondary indexes, record versions, continuations, split records, transactional schema evolution. Record format byte-identical to Java — share a cluster."
  >}}

  {{< hextra/feature-card
    title="SQL Engine"
    icon="search"
    subtitle="A database/sql driver backed by a Cascades optimizer ported from Java's fdb-relational-core: index selection, sort elimination, streaming aggregation."
  >}}

{{< /hextra/feature-grid >}}

{{< hextra/hero-section heading="h2" >}}Records&nbsp;or&nbsp;SQL.{{< /hextra/hero-section >}}

<div class="hx:max-w-3xl hx:mx-auto hx:w-full">

{{< tabs >}}

  {{< tab name="Record Layer" >}}

```go
db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
    store, err := recordlayer.NewStoreBuilder().
        SetMetaDataProvider(metadata).
        SetContext(rtx).
        SetSubspace(keyspace).
        CreateOrOpen()
    if err != nil {
        return nil, err
    }
    return store.SaveRecord(order)
})

// Type-safe access via generics.
typed := recordlayer.NewTypedFDBRecordStore[*pb.Order](store)
order, err := typed.LoadRecord(ctx, primaryKey)
```

  {{< /tab >}}

  {{< tab name="SQL" >}}

```go
import _ "fdb.dev/pkg/relational/sqldriver"

db, _ := sql.Open("fdbsql", "fdbsql:///mydb?cluster_file=/etc/foundationdb/fdb.cluster&schema=main")

db.Exec(`CREATE SCHEMA TEMPLATE app_tmpl
    CREATE TABLE Users (id BIGINT NOT NULL, name STRING, email STRING, PRIMARY KEY (id))
    CREATE INDEX idx_email ON Users (email)`)

db.Exec("INSERT INTO Users (id, name, email) VALUES (1, 'Alice', 'alice@example.com')")

// The Cascades optimizer picks the index scan, eliminates the sort, streams the aggregate.
rows, _ := db.Query("SELECT name FROM Users WHERE email = ?", "alice@example.com")
```

  {{< /tab >}}

{{< /tabs >}}

</div>

{{< hextra/hero-section heading="h2" >}}Tested&nbsp;against&nbsp;the&nbsp;reference.{{< /hextra/hero-section >}}

<div class="hx:max-w-3xl hx:mx-auto hx:w-full">

Every claim here is enforced in CI against **real FoundationDB** (testcontainers), not mocks:

- **Java conformance suite** — the same operations run against Java Record Layer 4.12.11.0; records must round-trip between the two engines.
- **Cross-backend differential** — the pure-Go and libfdb_c clients run in one process against one cluster; every read, write, index entry, and continuation must be byte-identical.
- **Binding-stress tester** — randomized operation sequences validate the client against libfdb_c, replayable by seed.

</div>

{{< hextra/hero-section heading="h2" >}}Share&nbsp;a&nbsp;cluster&nbsp;with&nbsp;Java.{{< /hextra/hero-section >}}

<div class="hx:max-w-3xl hx:mx-auto hx:w-full hx:text-center">

Wire compatibility is the project's hard line, not a feature. Record, index, version, continuation,
and split-record formats are **byte-identical to Java Record Layer 4.12.11.0**, and the client speaks
the FoundationDB **7.3** wire protocol (validated against 7.3.77; 8.0 is future work). A Go service
and a Java service read and write each other's records on the same cluster.

</div>

<div class="hx:mt-12 hx:mb-8 hx:text-center">
{{< hextra/hero-button text="Read the docs  →" link="docs" >}}
</div>
