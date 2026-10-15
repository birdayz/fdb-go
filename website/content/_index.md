---
title: "fdb-go — FoundationDB for Go"
description: "A pure-Go FoundationDB client, a Go port of the Record Layer, and an embedded SQL engine. The client does not require cgo."
layout: launch-home
toc: false
---

<section class="launch-hero" aria-labelledby="hero-heading">
  <div class="launch-intro">
    <p class="launch-eyebrow">Independent open source · Apache-2.0</p>
    <h1 id="hero-heading">FoundationDB,<br>from Go.</h1>
    <p class="launch-lead">A pure-Go FoundationDB client, a Go port of Apple's Record Layer, and an embedded SQL engine.</p>
    <p class="launch-detail">The client does not require cgo or <code>libfdb_c</code>. The SQL engine runs in the application process, so FoundationDB is the only server to operate.</p>
    <div class="launch-actions">
      <a class="launch-button" href="/docs/getting-started/">Get started <span aria-hidden="true">→</span></a>
      <a class="launch-source" href="https://github.com/birdayz/fdb-go">Source on GitHub <span aria-hidden="true">↗</span></a>
      <code class="launch-install">go get fdb.dev/pkg/fdbgo/fdb@master</code>
    </div>
  </div>
  <figure class="launch-stack">
    <figcaption>Application process</figcaption>
    <div class="stack-layer"><span class="stack-entry">database/sql</span><strong>SQL engine</strong><span class="stack-note">Parser · planner · executor</span></div>
    <div class="stack-connector" aria-hidden="true">↓</div>
    <div class="stack-layer"><span class="stack-entry">Protobuf records</span><strong>Record Layer</strong><span class="stack-note">Records · indexes · cursors</span></div>
    <div class="stack-connector" aria-hidden="true">↓</div>
    <div class="stack-layer stack-client"><span class="stack-entry">Key-value transactions</span><strong>Pure-Go FDB client</strong><span class="stack-note">FoundationDB wire protocol</span></div>
    <div class="stack-wire"><span>TCP</span><span aria-hidden="true">↓</span></div>
    <div class="stack-server"><strong>FoundationDB cluster</strong><span>Separate server · 7.3 protocol</span></div>
    <p class="stack-caption">Each layer is usable as an API. The Record Layer and SQL engine can also run on Apple's C client with the <code>libfdbc</code> build tag and cgo.</p>
  </figure>
</section>

<aside class="launch-status" aria-label="Project status">
  <span class="status-label">Pre-1.0</span>
  <p>Not declared production-ready. Evaluate a pinned commit against the target workload.</p>
  <a href="/docs/maturity/">Status &amp; compatibility <span aria-hidden="true">→</span></a>
</aside>

<section class="launch-section" aria-labelledby="apis-heading">
  <div class="section-heading"><h2 id="apis-heading">APIs</h2></div>
  <div class="launch-api-grid">
    <article class="api-card">
      <h3>Pure-Go FDB client</h3>
      <p>Key-value transactions with reads, writes, range reads, and retries on retryable errors. The API is modeled on Apple's Go binding.</p>
      <a class="package-link" href="https://github.com/birdayz/fdb-go/tree/master/pkg/fdbgo/fdb"><code>pkg/fdbgo/fdb</code> <span aria-hidden="true">↗</span></a>
    </article>
    <article class="api-card">
      <h3>Record Layer</h3>
      <p>A Go port of Apple's Record Layer. Stores protobuf records with secondary indexes, schema metadata, and resumable scans.</p>
      <a class="package-link" href="https://github.com/birdayz/fdb-go/tree/master/pkg/recordlayer"><code>pkg/recordlayer</code> <span aria-hidden="true">↗</span></a>
    </article>
    <article class="api-card">
      <h3>SQL engine</h3>
      <p>A <code>database/sql</code> driver over the Record Layer. Queries are planned and executed in the application process. It is not a PostgreSQL-compatible server.</p>
      <a class="package-link" href="https://github.com/birdayz/fdb-go/tree/master/pkg/relational/sqldriver"><code>pkg/relational/sqldriver</code> <span aria-hidden="true">↗</span></a>
    </article>
  </div>
</section>

<section class="launch-section launch-example" aria-labelledby="example-heading">
  <div>
    <h2 id="example-heading">Examples</h2>
    <p><code>Transact</code> runs the function in a transaction and runs it again after a retryable error. Keep external side effects outside the function.</p>
    <p>The repository includes complete <a href="https://github.com/birdayz/fdb-go/blob/master/example/getting_started.go">Record Layer</a> and <a href="https://github.com/birdayz/fdb-go/tree/master/example/sql">SQL</a> programs with setup and error handling.</p>
  </div>
  <div class="launch-code-grid">
    <div class="launch-code">
      <div class="code-caption"><span>Key-value transaction</span><span>fdb.dev/pkg/fdbgo/fdb</span></div>
{{< highlight go >}}
value, err := db.Transact(
    func(tx fdb.WritableTransaction) (any, error) {
        tx.Set(fdb.Key("greeting"), []byte("hello"))
        return tx.Get(fdb.Key("greeting")).Get()
    },
)
if err != nil {
    return err
}
fmt.Println(string(value.([]byte)))
{{< /highlight >}}
      <p class="code-footnote"><a href="https://github.com/birdayz/fdb-go#fdb-client">Client connection setup →</a></p>
    </div>
    <div class="launch-code">
      <div class="code-caption"><span>SQL query</span><span>fdb.dev/pkg/relational/sqldriver</span></div>
{{< highlight go >}}
rows, err := sqlDB.QueryContext(ctx,
    `SELECT id, email FROM users
     WHERE status = ? ORDER BY id LIMIT 10`,
    "active",
)
if err != nil {
    return err
}
defer rows.Close()
{{< /highlight >}}
      <p class="code-footnote"><a href="https://github.com/birdayz/fdb-go/tree/master/example/sql">Complete SQL program →</a></p>
    </div>
  </div>
</section>

<section class="launch-section launch-evidence" aria-labelledby="evidence-heading">
  <div class="section-heading"><h2 id="evidence-heading">Compatibility testing</h2><p>Each suite compares specific cases with an external reference implementation. A passing suite does not establish compatibility outside the tested cases.</p></div>
  <div class="evidence-grid">
    <article><h3>Client behavior</h3><p>Differential tests run the Go client and <code>libfdb_c</code> against the same FoundationDB cluster and compare the results.</p><a href="https://github.com/birdayz/fdb-go/tree/master/pkg/fdbgo/bench">Client differential tests <span aria-hidden="true">↗</span></a></article>
    <article><h3>Record-store data</h3><p>Conformance tests exercise Go and Java reads and writes on shared stores. The Java reference is Record Layer 4.14.2.0.</p><a href="https://github.com/birdayz/fdb-go/tree/master/conformance">Java conformance suite <span aria-hidden="true">↗</span></a></article>
    <article><h3>SQL results and plans</h3><p>Cross-engine scenarios and regression tests compare query behavior with the Java relational layer.</p><a href="https://github.com/birdayz/fdb-go/tree/master/pkg/relational/conformance">SQL conformance scenarios <span aria-hidden="true">↗</span></a></article>
  </div>
  <div class="launch-boundary"><div><strong>Mixing Go and Java writers</strong><p>Collation, TEXT Unicode behavior, Go-only vector layouts, and engine-private SQL continuation tokens have documented exceptions. Existing Go SQL storage may require an explicit migration.</p></div><div class="boundary-links"><a href="https://github.com/birdayz/fdb-go/blob/master/docs/compatibility.md">Compatibility boundaries →</a><a href="https://github.com/birdayz/fdb-go/blob/master/docs/upgrade.md">Upgrading and rolling back →</a></div></div>
  <p class="performance-note">Earlier Go-vs-libfdb_c speedup and write-parity claims were withdrawn. Benchmark results apply only to the measured configurations. <a href="/docs/benchmarks/">Benchmarks →</a> · <a href="https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/bench/PERFORMANCE.md">Correction and methodology →</a></p>
</section>

<section class="launch-section launch-next" aria-labelledby="next-heading">
  <div><h2 id="next-heading">Evaluation</h2><p>This site documents the v0.2.0 candidate on the development branch. Record the commit under evaluation.</p><a class="launch-button" href="/docs/getting-started/">Build and run locally <span aria-hidden="true">→</span></a></div>
  <div class="launch-reading"><a href="https://github.com/birdayz/fdb-go/blob/master/STATUS.md"><span>Project status</span><span>Readiness and test-lane scope ↗</span></a><a href="https://github.com/birdayz/fdb-go/blob/master/docs/operations.md"><span>Operator guide</span><span>Transactions, index builds, backups, observability ↗</span></a><a href="https://github.com/birdayz/fdb-go/issues"><span>Issues</span><span>Bug reports and open work ↗</span></a></div>
</section>

<p class="launch-independent">An unofficial, independent project. Not affiliated with, sponsored by, or endorsed by Apple Inc. or the FoundationDB project.</p>
