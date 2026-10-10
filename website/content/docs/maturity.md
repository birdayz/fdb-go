---
title: Status & compatibility
description: "fdb-go is pre-1.0 and not declared production-ready. Read its version targets, test evidence, and Java interoperability boundaries."
weight: 5
---

**Pre-1.0; not declared production-ready.** Evaluate a pinned commit against your
schemas, queries, recovery requirements, and load. The repository's
[`STATUS.md`](https://github.com/birdayz/fdb-go/blob/master/STATUS.md) is the current
readiness statement; this page is an overview, not a release certification.

## What each layer needs to establish

| Layer | Evaluate |
|---|---|
| **Pure-Go FDB client** | Transaction semantics, retries, cancellation, and recovery with your cluster topology |
| **Record Layer** | Go ↔ Java reads and writes for your exact metadata, index types, and store formats |
| **SQL engine** | Query correctness, plans, resource limits, and performance for your workload |

These docs describe the development tree, not the older v0.1.0 release. Pinning
a version does not by itself establish readiness.

## Interoperability has boundaries

Sharing record-store data with Java is a central project goal. It is not a
promise that every index type, query, or continuation works across engines.

Before mixing writers, read the
[compatibility matrix](https://github.com/birdayz/fdb-go/blob/master/docs/compatibility.md).
It covers collation, TEXT Unicode behavior, Go-only vector layouts, synthetic
record types, and other format-specific exceptions. SQL continuation tokens
are engine-private, not a Go ↔ Java interchange format.

Read the [upgrade guide](https://github.com/birdayz/fdb-go/blob/master/docs/upgrade.md)
before opening an existing store. Older Go SQL storage is not automatically
migrated. Rehearse backup, migration, and rollback before allowing new writers.

## What the tests establish

Tests use several external references:

- [Client differentials](https://github.com/birdayz/fdb-go/tree/master/pkg/fdbgo/bench)
  compare pure Go with `libfdb_c` against the same FoundationDB cluster.
- [Record Layer conformance](https://github.com/birdayz/fdb-go/tree/master/conformance)
  exercises shared record-store data with Java.
- [SQL conformance](https://github.com/birdayz/fdb-go/tree/master/pkg/relational/conformance)
  compares query behavior with the Java relational engine.

A passing case establishes the tested scope, not universal parity. Check the
[CI run](https://github.com/birdayz/fdb-go/actions/workflows/ci.yml) for the exact
commit, executed lanes, and artifacts rather than treating a badge as proof.

`just test` runs the fast lane: unit tests, nogo, and bounded integration tests.
`just test-full` includes the heavyweight client differential, Java conformance,
SQL, and stress targets at their default budgets. Neither is a statement that
every extended fuzz, race, or recovery configuration has run. No fresh full-suite
or production-workload result is asserted by this page.

Earlier Go-vs-libfdb_c speedup and write-parity claims were withdrawn. See the
[benchmark correction and methodology](https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/bench/PERFORMANCE.md);
there is no replacement speed claim.

## Version targets

| Component | Reference |
|---|---|
| FoundationDB | 7.3 protocol; client and test-cluster pin 7.3.77 |
| Java Record Layer / Relational | 4.14.2.0 |
| Go toolchain | [`go.mod`](https://github.com/birdayz/fdb-go/blob/master/go.mod) |
| Build tooling | [`MODULE.bazel`](https://github.com/birdayz/fdb-go/blob/master/MODULE.bazel) and [`.bazelversion`](https://github.com/birdayz/fdb-go/blob/master/.bazelversion) |

The pure-Go client is not a multi-version client. FDB 8.0 compatibility is not
established by this tree; consult the upgrade guide before changing the server.

## Project and reporting

This is an unofficial, independent open-source project, not affiliated with,
sponsored by, or endorsed by Apple Inc. or the FoundationDB project. Upstream
names identify projects and compatibility targets, not endorsement.

Use [GitHub issues](https://github.com/birdayz/fdb-go/issues) for reproducible bugs
and [SECURITY.md](https://github.com/birdayz/fdb-go/blob/master/SECURITY.md) for
vulnerability reporting. For deployment concerns, see the
[operator guide](https://github.com/birdayz/fdb-go/blob/master/docs/operations.md).
