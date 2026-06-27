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

<div style="margin:2rem 0 3rem">
{{< hextra/hero-button text="Get started  →" link="docs" >}}
&nbsp;&nbsp;
{{< hextra/hero-button text="Star on GitHub" link="https://github.com/birdayz/fdb-go" style="background:transparent;border:1px solid var(--hextra-primary-color, #888);color:inherit" >}}
</div>

<div class="hero-code">

```go
import "fdb.dev/pkg/fdbgo/fdbclient" // pure Go. CGO_ENABLED=0. static binary.

db, _ := fdbclient.Open("/etc/foundationdb/fdb.cluster")
```

</div>

<div class="hero-note">

Pre-1.0, built in the open. The wire format is the hard line — conformance- and differential-tested against the Java reference. Pin a commit and run the suites before production. [Maturity →](docs/maturity)

</div>

<div style="height:4rem"></div>

{{< hextra/hero-section heading="h2" >}}No&nbsp;cgo. Static&nbsp;binary. Faster&nbsp;reads.{{< /hextra/hero-section >}}

<div class="s-body">

Most FDB tooling links Apple's C library through cgo — no static binaries, painful cross-compilation, a glibc dependency. The pure-Go client speaks the FoundationDB wire protocol directly and is the default backend, producing byte-identical reads and writes. The Go runtime skips the C client's network-thread hop and multi-version-client shim; writes share the same commit path and run at parity.

</div>

<div class="bench"><div class="bench-head">pure-Go vs libfdb_c · bar length = speedup</div><div class="bench-row"><div class="bench-name">Get <small>100 B</small></div><div class="bench-nums"><b>60</b> vs 218 µs</div><div class="bench-track"><div class="bench-fill" style="width:92%"></div></div><div class="bench-x">3.6×</div></div><div class="bench-row"><div class="bench-name">Get <small>1 KB</small></div><div class="bench-nums"><b>61</b> vs 209 µs</div><div class="bench-track"><div class="bench-fill" style="width:87%"></div></div><div class="bench-x">3.4×</div></div><div class="bench-row"><div class="bench-name">GetRange <small>100 keys</small></div><div class="bench-nums"><b>92</b> vs 363 µs</div><div class="bench-track"><div class="bench-fill" style="width:100%"></div></div><div class="bench-x">3.9×</div></div><div class="bench-row"><div class="bench-name">Read <small>@ 10 ms RTT</small></div><div class="bench-nums"><b>5.3</b> vs 12.6 ms</div><div class="bench-track"><div class="bench-fill" style="width:62%"></div></div><div class="bench-x">2.4×</div></div><div class="bench-row"><div class="bench-name">Set + Commit</div><div class="bench-nums"><b>1.01</b> vs 1.00 ms</div><div class="bench-track"><div class="bench-fill is-par" style="width:26%"></div></div><div class="bench-x is-par">1.0×</div></div></div>

<p class="muted-note">The <b>10 ms-RTT row is the realistic signal</b> — localhost microbenchmarks are syscall/IPC-bound, so they're the optimistic end. The read advantage holds under real latency (2.4× at 10 ms) and converges to parity at extreme RTT, where both clients are network-bound. Writes run at parity. Reproducible from <a href="https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/bench/bench_test.go"><code>TestBenchmarkSanity</code></a>; method in <a href="https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/bench/PERFORMANCE.md"><code>PERFORMANCE.md</code></a>. Prefer Apple's C client? One build tag (<code>-tags libfdbc</code>) swaps it in — same bytes on the wire.</p>

<div style="height:4rem"></div>

{{< hextra/hero-section heading="h2" >}}A&nbsp;client, and&nbsp;layers&nbsp;on&nbsp;top.{{< /hextra/hero-section >}}

<div class="s-body">

FoundationDB is an ordered, transactional key-value store — strict-serializable ACID, fault-tolerant, proven by deterministic simulation testing. It's the storage substrate under systems like Snowflake and Apple's CloudKit. Higher-level data models are built as **layers** on top. `fdb.dev` is the Go client and a growing set of them.

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

<div style="height:4rem"></div>

{{< hextra/hero-section heading="h2" >}}Share&nbsp;a&nbsp;cluster&nbsp;with&nbsp;Java.{{< /hextra/hero-section >}}

<div class="s-body">

Wire compatibility is the project's hard line, not a feature. Record, index, version, continuation, and split-record formats are **byte-identical to Java Record Layer 4.12.11.0**, and the client speaks the FoundationDB **7.3** wire protocol (validated against 7.3.77; 8.0 is future work). Enforced in CI against real FoundationDB by a Java conformance suite, a cross-backend differential, and a binding-stress tester — not mocks.

</div>

<div style="margin:3rem 0 2rem">
{{< hextra/hero-button text="Get started  →" link="docs" >}}
</div>
