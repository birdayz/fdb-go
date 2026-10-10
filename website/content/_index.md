---
title: "fdb-go — FoundationDB for Go"
description: "A native Go FoundationDB client, a Record Layer, and an embedded SQL engine. No cgo required. Explore the APIs, examples, and compatibility boundaries."
layout: launch-home
toc: false
---

<section class="launch-hero" aria-labelledby="hero-heading">
  <div class="launch-intro">
    <p class="launch-eyebrow">Independent open source · Apache-2.0</p>
    <h1 id="hero-heading">FoundationDB,<br>from Go.</h1>
    <p class="launch-lead">A native client, structured records, and SQL on FoundationDB. Use the layer your application needs.</p>
    <p class="launch-detail">No cgo required. No separate SQL server. FoundationDB remains your database; these are the Go libraries on top.</p>
    <div class="launch-actions">
      <a class="launch-button" href="/docs/getting-started/">Read the quickstart <span aria-hidden="true">→</span></a>
      <a class="launch-source" href="https://github.com/birdayz/fdb-go">Browse source <span aria-hidden="true">↗</span></a>
    </div>
    <a class="hero-mobile-status" href="/docs/maturity/">Pre-1.0 · Status &amp; compatibility →</a>
  </div>
  <figure class="launch-stack">
    <figcaption>Inside your Go application</figcaption>
    <div class="stack-layer"><span class="stack-entry">database/sql</span><strong>SQL engine</strong><span class="stack-note">Parser · planner · executor</span></div>
    <div class="stack-connector" aria-hidden="true">↓</div>
    <div class="stack-layer"><span class="stack-entry">Protobuf records</span><strong>Record Layer</strong><span class="stack-note">Records · indexes · cursors</span></div>
    <div class="stack-connector" aria-hidden="true">↓</div>
    <div class="stack-layer stack-client"><span class="stack-entry">Key-value transactions</span><strong>Pure-Go FDB client</strong><span class="stack-note">FoundationDB wire protocol</span></div>
    <div class="stack-wire"><span>TCP</span><span aria-hidden="true">↓</span></div>
    <div class="stack-server"><strong>FoundationDB cluster</strong><span>Separate server · FDB 7.3 target</span></div>
    <p class="stack-caption">Enter at any layer. Record Layer and SQL can also use Apple's C client via a build tag.</p>
  </figure>
</section>

<aside class="launch-status" aria-label="Project status">
  <span class="status-label">Pre-1.0</span>
  <p>Under active development, not declared production-ready. Evaluate a pinned commit against your workload.</p>
  <a href="/docs/maturity/">Status &amp; limits <span aria-hidden="true">→</span></a>
</aside>

<section class="launch-section" aria-labelledby="apis-heading">
  <div class="section-heading"><p class="launch-eyebrow">Choose your entry point</p><h2 id="apis-heading">One cluster. Three APIs.</h2></div>
  <div class="launch-api-grid">
    <article class="api-card">
      <span class="api-number" aria-hidden="true">01 / CLIENT</span>
      <h3>Key-value transactions</h3>
      <p>Use FoundationDB directly: reads, writes, ranges, and transaction retries through an API modeled on Apple's Go binding.</p>
      <a class="package-link" href="https://github.com/birdayz/fdb-go/tree/master/pkg/fdbgo/fdb"><code>pkg/fdbgo/fdb</code> <span aria-hidden="true">↗</span></a>
    </article>
    <article class="api-card">
      <span class="api-number" aria-hidden="true">02 / RECORD LAYER</span>
      <h3>Records and indexes</h3>
      <p>Store protobuf records with secondary indexes, schema metadata, and resumable scans. A Go port of Apple's Record Layer.</p>
      <a class="package-link" href="https://github.com/birdayz/fdb-go/tree/master/pkg/recordlayer"><code>pkg/recordlayer</code> <span aria-hidden="true">↗</span></a>
    </article>
    <article class="api-card">
      <span class="api-number" aria-hidden="true">03 / SQL</span>
      <h3>Queries from Go</h3>
      <p>Use <code>database/sql</code> over the Record Layer. The embedded engine plans and executes queries; it is not a PostgreSQL-compatible server.</p>
      <a class="package-link" href="https://github.com/birdayz/fdb-go/tree/master/pkg/relational/sqldriver"><code>pkg/relational/sqldriver</code> <span aria-hidden="true">↗</span></a>
    </article>
  </div>
</section>

<section class="launch-section launch-example" aria-labelledby="example-heading">
  <div>
    <p class="launch-eyebrow">Start with a transaction</p>
    <h2 id="example-heading">An ordinary Go callback.<br>A FoundationDB transaction.</h2>
    <p>Use the client on its own. The callback may run again on a retryable error, so keep external side effects outside it.</p>
    <p>Prefer structured records or SQL? The repository includes complete examples for both, with setup and error handling.</p>
    <div class="example-links"><a href="https://github.com/birdayz/fdb-go/blob/master/example/getting_started.go">Record example <span aria-hidden="true">↗</span></a><a href="https://github.com/birdayz/fdb-go/tree/master/example/sql">SQL example <span aria-hidden="true">↗</span></a></div>
  </div>
  <div class="launch-code">
    <div class="code-caption"><span>Transaction excerpt</span><span>Go</span></div>
{{< highlight go >}}
// With an open fdb.Database:
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
    <p class="code-footnote">Import <code>fdb.dev/pkg/fdbgo/fdb</code>. <a href="https://github.com/birdayz/fdb-go#fdb-client">Full connection setup →</a></p>
  </div>
</section>

<section class="launch-section launch-evidence" aria-labelledby="evidence-heading">
  <div class="section-heading"><p class="launch-eyebrow">Read the evidence, not a parity claim</p><h2 id="evidence-heading">Tested against the implementations<br>it needs to work with.</h2><p>Interoperability is the goal. Tests exercise specific cases against external references; they do not establish universal compatibility.</p></div>
  <div class="evidence-grid">
    <article><h3>FoundationDB client behavior</h3><p>Differential tests compare the Go client with <code>libfdb_c</code> against the same cluster.</p><a href="https://github.com/birdayz/fdb-go/tree/master/pkg/fdbgo/bench">Read the client differentials <span aria-hidden="true">↗</span></a></article>
    <article><h3>Shared record-store data</h3><p>Conformance tests exercise Go and Java reads and writes. The Java reference is Record Layer 4.14.2.0.</p><a href="https://github.com/birdayz/fdb-go/tree/master/conformance">Read the Java conformance suite <span aria-hidden="true">↗</span></a></article>
    <article><h3>SQL results and plans</h3><p>Cross-engine scenarios and regression tests check query behavior against the Java relational layer.</p><a href="https://github.com/birdayz/fdb-go/tree/master/pkg/relational/conformance">Read the SQL scenarios <span aria-hidden="true">↗</span></a></article>
  </div>
  <div class="launch-boundary"><strong>Before mixing Go and Java writers</strong><p>Review the supported formats and exceptions: collation, TEXT behavior, vector layouts, and SQL continuation tokens need particular care. Existing Go SQL storage may require an explicit migration.</p><a href="https://github.com/birdayz/fdb-go/blob/master/docs/compatibility.md">Compatibility boundaries →</a><a href="https://github.com/birdayz/fdb-go/blob/master/docs/upgrade.md">Upgrade guidance →</a></div>
  <p class="performance-note">Earlier Go-vs-libfdb_c speedup and write-parity claims have been withdrawn. No current speed comparison is claimed. <a href="https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/bench/PERFORMANCE.md">Correction &amp; benchmark methodology →</a></p>
</section>

<section class="launch-section launch-next" aria-labelledby="next-heading">
  <div><p class="launch-eyebrow">Try it, then inspect it</p><h2 id="next-heading">Start from the source.</h2><p>The website describes the development tree, not the older v0.1.0 release. Record the commit you evaluate.</p><a class="launch-button" href="/docs/getting-started/">Build &amp; run locally <span aria-hidden="true">→</span></a></div>
  <div class="launch-reading"><a href="https://github.com/birdayz/fdb-go/blob/master/STATUS.md"><span>Project status</span><span>Readiness and test-lane scope ↗</span></a><a href="https://github.com/birdayz/fdb-go/blob/master/docs/operations.md"><span>Operator guide</span><span>Transactions, indexes, backups ↗</span></a><a href="https://github.com/birdayz/fdb-go/issues"><span>Issues &amp; contributions</span><span>Report a reproducer or explore the work ↗</span></a></div>
</section>

<p class="launch-independent">An unofficial, independent project. Not affiliated with, sponsored by, or endorsed by Apple Inc. or the FoundationDB project.</p>
