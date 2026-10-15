---
title: Docs
description: "Build fdb-go from source, compare its three FoundationDB APIs, and review readiness and compatibility boundaries."
next: getting-started
weight: 1
---

fdb-go provides three APIs over FoundationDB: a pure-Go key-value client, a
Record Layer for protobuf records and indexes, and an embedded `database/sql`
engine. The client works without either higher layer.

{{< cards >}}
  {{< card link="getting-started" title="Getting started" icon="play" subtitle="Build from source and run the examples locally." >}}
  {{< card link="benchmarks" title="Benchmarks" icon="chart-bar" subtitle="Preregistered single-node and 4-node VPC benchmark campaigns." >}}
  {{< card link="maturity" title="Status & compatibility" icon="shield-check" subtitle="Readiness, version targets, test coverage, and Java interoperability." >}}
  {{< card link="https://github.com/birdayz/fdb-go/blob/master/docs/operations.md" title="Operator guide" icon="book-open" subtitle="Transactions, index builds, backups, and observability." >}}
  {{< card link="https://github.com/birdayz/fdb-go/tree/master/example" title="Source examples" icon="code" subtitle="Complete programs for the Record Layer and SQL APIs." >}}
{{< /cards >}}

## APIs

| Use case | Package |
|---|---|
| FoundationDB key-value transactions from Go, without cgo | [`pkg/fdbgo/fdb`](https://github.com/birdayz/fdb-go/tree/master/pkg/fdbgo/fdb) |
| Protobuf records, secondary indexes, and cursor-based scans | [`pkg/recordlayer`](https://github.com/birdayz/fdb-go/tree/master/pkg/recordlayer) |
| SQL queries through Go's `database/sql` | [`pkg/relational/sqldriver`](https://github.com/birdayz/fdb-go/tree/master/pkg/relational/sqldriver) |

FoundationDB is a separate server. The Record Layer and SQL engine run in your
application; this project is not a PostgreSQL wire-protocol server.

**Pre-1.0; not declared production-ready.** These docs describe the development
tree, not the older v0.1.0 release. Read [status and compatibility](maturity/)
before using an existing store or mixing Go and Java writers.
