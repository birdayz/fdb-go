---
title: Maturity & Status
weight: 5
---

Straight answers to the questions you should ask before depending on this.

## Is it production-ready?

**Not yet — it's pre-1.0.** Maturity varies by layer:

| Layer | Maturity | Trust |
|---|---|---|
| **Record store** (CRUD, indexes, versions, continuations, split records) | Most mature | The wire format is the hard line, exercised by the Java conformance + binding-stress suites. Trust this first. |
| **Pure-Go client** (`pkg/fdbgo`) | Maturing | Reimplements the FDB wire protocol; validated against `libfdb_c` 7.3.77 by the binding tester and a cross-backend differential. |
| **Cascades SQL engine** | Usable, evolving | Wide SQL surface validated by a cross-engine differential harness, but with open correctness items on specific query shapes. |

Before production: pin a commit, run the conformance + differential + stress suites against your
workload, and review the readiness docs in the repo.

## How is correctness established?

Against the reference, in CI, on real FoundationDB (testcontainers) — never mocks:

- **Java conformance suite** — the same operations run against Java Record Layer 4.12.11.0; records
  must round-trip between engines.
- **Cross-backend differential** — pure-Go and `libfdb_c` clients run in one process against one
  cluster; every read, write, index entry, and continuation must be byte-identical.
- **Binding-stress tester** — randomized op sequences validated against `libfdb_c`, replayable by seed.
- **Model-based chaos testing** — an in-memory model shadows the store and fault injection runs at
  transaction boundaries.

## Compatibility targets

| Component | Version |
|---|---|
| FoundationDB wire protocol | 7.3 (validated against 7.3.77) |
| Java Record Layer | 4.12.11.0 |
| Go | 1.26+ |

FDB 8.0 is future work — the wire protocol is not a stable third-party contract and changes between
releases.

## Who's behind it?

An independent open-source project ([github.com/birdayz/fdb-go](https://github.com/birdayz/fdb-go)),
not affiliated with or endorsed by Apple. "FoundationDB" is Apple's trademark; this project uses the
name to describe compatibility, not endorsement. Report issues and security reports per the repo's
`SECURITY.md`.
