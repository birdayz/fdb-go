---
title: "fdb.dev: FoundationDB for Go"
description: "A pure-Go FoundationDB client (no cgo, 2-4x faster reads than libfdb_c), plus a wire-compatible Record Layer and SQL engine. Share a cluster with Java."
layout: hextra-home
toc: false
---

{{< hextra/hero-badge >}}Native Go{{< /hextra/hero-badge >}}
&nbsp;&nbsp;
{{< hextra/hero-badge >}}Java Wire Compatible{{< /hextra/hero-badge >}}

<div class="hx:mt-6 hx:mb-4">
{{< hextra/hero-headline >}}
  FoundationDB for Go
{{< /hextra/hero-headline >}}
</div>

<div class="hx:mb-6">
{{< hextra/hero-subtitle >}}
  A Go ecosystem for FoundationDB, essentials first: a native, pure-Go client and a wire-compatible Record Layer, with a SQL engine on top. No cgo, and **2-4x faster reads** than libfdb_c.
{{< /hextra/hero-subtitle >}}
</div>

<div class="hero-cta">
{{< hextra/hero-button text="Get started  →" link="docs" >}}
{{< hextra/hero-button text="View on GitHub" link="https://github.com/birdayz/fdb-go" style="background:transparent;border:1px solid var(--hextra-primary-color, #888);color:inherit" >}}
</div>

<div class="hero-code">

```go
import "fdb.dev/pkg/fdbgo/fdb" // pure Go. CGO_ENABLED=0. no libfdb_c.

fdb.MustAPIVersion(730)
db := fdb.MustOpenDatabase("/etc/foundationdb/fdb.cluster")

greeting, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
	tx.Set(fdb.Key("greeting"), []byte("hello"))
	return tx.Get(fdb.Key("greeting")).MustGet(), nil
})
// greeting == []byte("hello"), err == nil. Committed atomically.
```

</div>

<div class="hero-note">

Pre-1.0. The wire format is the part to trust first, and it's conformance- and differential-tested against Java. Pin a commit and run the suites before you ship. [Maturity →](docs/maturity)

</div>

<div style="height:4rem"></div>

{{< hextra/hero-section heading="h2" >}}No&nbsp;cgo. Static&nbsp;binary. Faster&nbsp;reads.{{< /hextra/hero-section >}}

<div class="s-body">

Most FDB tooling links Apple's C library through cgo. That means no static binaries, painful cross-compilation, and a glibc dependency. The pure-Go client speaks the FoundationDB wire protocol directly. It's the default backend and produces byte-identical reads and writes. The read speedup comes from skipping the C client's network-thread hop and multi-version-client shim. Writes go through the same commit path, so they run at parity.

</div>

<div class="bench"><div class="bench-head">pure-Go vs libfdb_c · bar length = speedup</div><div class="bench-row"><div class="bench-name">Get <small>100 B</small></div><div class="bench-nums"><b>60</b> vs 218 µs</div><div class="bench-track"><div class="bench-fill" style="width:92%"></div></div><div class="bench-x">3.6x</div></div><div class="bench-row"><div class="bench-name">Get <small>1 KB</small></div><div class="bench-nums"><b>61</b> vs 209 µs</div><div class="bench-track"><div class="bench-fill" style="width:87%"></div></div><div class="bench-x">3.4x</div></div><div class="bench-row"><div class="bench-name">GetRange <small>100 keys</small></div><div class="bench-nums"><b>92</b> vs 363 µs</div><div class="bench-track"><div class="bench-fill" style="width:100%"></div></div><div class="bench-x">3.9x</div></div><div class="bench-row"><div class="bench-name">Read <small>@ 10 ms RTT</small></div><div class="bench-nums"><b>5.3</b> vs 12.6 ms</div><div class="bench-track"><div class="bench-fill" style="width:62%"></div></div><div class="bench-x">2.4x</div></div><div class="bench-row"><div class="bench-name">Set + Commit</div><div class="bench-nums"><b>1.01</b> vs 1.00 ms</div><div class="bench-track"><div class="bench-fill is-par" style="width:26%"></div></div><div class="bench-x is-par">1.0x</div></div></div>

<p class="muted-note">The 10 ms-RTT row is the one that matters: localhost microbenchmarks are syscall-bound, so they flatter the pure-Go client. Under real network latency the read advantage holds (2.4x at 10 ms) and converges to parity at high RTT, where both clients are waiting on the network. Writes run at parity throughout. The numbers are reproducible from <a href="https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/bench/bench_test.go"><code>TestBenchmarkSanity</code></a>, and the method is in <a href="https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/bench/PERFORMANCE.md"><code>PERFORMANCE.md</code></a>. Want Apple's C client instead? One build tag (<code>-tags libfdbc</code>) swaps it in, same bytes on the wire.</p>

<div style="height:4rem"></div>

{{< hextra/hero-section heading="h2" >}}Quickstart{{< /hextra/hero-section >}}

<div class="s-body">

Install the driver, then open a database, create a schema, and read and write, all from Go. The pure-Go client is the default backend, so you only need a cluster file.

</div>

{{< callout type="info" >}}
  No cluster handy? Grab the CLI (`curl -fsSL https://fdb.dev/install.sh | sh`), then `frl fdb up` starts a single-node FoundationDB in Docker (the only prerequisite). Remove it with `frl fdb down`. The installer ships a checksum-verified static binary; `go install fdb.dev/cmd/frl@latest` builds the same thing from source.
{{< /callout >}}

<div class="s-steps">

{{% steps %}}

### Install

```sh
go get fdb.dev/pkg/relational/sqldriver
```

### Open a database, create a schema, write and read, in Go

```go
package main

import (
	"database/sql"
	"fmt"
	"log"

	_ "fdb.dev/pkg/relational/sqldriver"
)

func main() {
	db, err := sql.Open("fdbsql",
		"fdbsql:///myapp?cluster_file=/etc/foundationdb/fdb.cluster&schema=main")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Create the database, a schema template, and a schema.
	db.Exec(`CREATE DATABASE /myapp`)
	db.Exec(`CREATE SCHEMA TEMPLATE app
	    CREATE TABLE users (id BIGINT, name STRING, email STRING, PRIMARY KEY (id))
	    CREATE INDEX by_email ON users (email)`)
	db.Exec(`CREATE SCHEMA /myapp/main WITH TEMPLATE app`)

	// Write a row, then read it back.
	db.Exec(`INSERT INTO users (id, name, email)
	    VALUES (1, 'Alice', 'alice@example.com')`)

	var name string
	if err := db.QueryRow(
		`SELECT name FROM users WHERE email = ?`, "alice@example.com",
	).Scan(&name); err != nil {
		log.Fatal(err)
	}
	fmt.Println(name) // Alice
}
```

### Or query the same data from the CLI

```text
$ frl sql --database /myapp --schema main
fdb> SELECT name, email FROM users WHERE email = 'alice@example.com';
NAME  │ EMAIL
──────┼───────────────────
Alice │ alice@example.com
(1 row)
```

{{% /steps %}}

</div>

<p class="muted-note">The Cascades planner uses the <code>by_email</code> index for that query, so it isn't a full scan. The same store is reachable from Go's typed record API if you'd rather store protobuf records directly. See the <a href="https://github.com/birdayz/fdb-go">record-layer guide</a>.</p>

<div style="height:4rem"></div>

{{< hextra/hero-section heading="h2" >}}A client, and layers on top.{{< /hextra/hero-section >}}

<div class="s-body">

FoundationDB is an ordered, transactional key-value store with strict-serializable ACID. Snowflake's metadata store and Apple's CloudKit are built on it. Higher-level data models are built as **layers** on top of it. fdb.dev is the Go client plus a growing set of those layers.

</div>

{{< hextra/feature-grid cols="3" >}}

  {{< hextra/feature-card
    title="Pure-Go Client"
    icon="lightning-bolt"
    subtitle="A from-scratch FDB wire-protocol client. No cgo, faster reads, read-your-writes, retries, commit_unknown_result handling. Validated against libfdb_c 7.3.77."
  >}}

  {{< hextra/feature-card
    title="Record Layer"
    icon="database"
    subtitle="Structured records, secondary indexes, versions, continuations, split records, schema evolution. The record format is byte-identical to Java, so you can share a cluster."
  >}}

  {{< hextra/feature-card
    title="SQL Engine"
    icon="search"
    subtitle="A database/sql driver backed by a Cascades optimizer ported from Java's fdb-relational-core: index selection, sort elimination, streaming aggregation."
  >}}

{{< /hextra/feature-grid >}}

<div style="height:4rem"></div>

{{< hextra/hero-section heading="h2" >}}Share a cluster with Java.{{< /hextra/hero-section >}}

<div class="s-body">

Wire compatibility is the whole point of the project. Record, index, version, continuation, and split-record formats are **byte-identical to Java Record Layer 4.12.11.0**, and the client speaks the FoundationDB **7.3** wire protocol (validated against 7.3.77; 8.0 is future work). CI enforces all of this against real FoundationDB with a Java conformance suite, a cross-backend differential, and a binding-stress tester. No mocks.

</div>

<div style="margin:3rem 0 2rem">
{{< hextra/hero-button text="Get started  →" link="docs" >}}
</div>
