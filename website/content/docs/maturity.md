---
title: Maturity & Status
weight: 5
---

Straight answers to the questions you should ask before depending on this.

## Is it production-ready?

Not yet. It's pre-1.0, and maturity varies by layer:

| Layer | Maturity | Trust |
|---|---|---|
| **Record store** (CRUD, indexes, versions, continuations, split records) | Most mature | The wire format is the hard line, exercised by the Java conformance and binding-stress suites. Trust this first. |
| **Pure-Go client** (`pkg/fdbgo`) | Maturing | Reimplements the FDB wire protocol. Validated against `libfdb_c` 7.3.77 by the binding tester and a cross-backend differential. |
| **Cascades SQL engine** | Usable, evolving | Wide SQL surface validated by a cross-engine differential harness, with open correctness items on specific query shapes. |

Before production, pin a commit, run the conformance, differential, and stress suites against your workload, and review the readiness docs in the repo.

## How is correctness established?

Against the reference, in CI, on real FoundationDB (testcontainers). No mocks.

- **Java conformance suite.** The same operations run against Java Record Layer 4.12.11.0, and records must round-trip between the two engines.
- **Cross-backend differential.** The pure-Go and `libfdb_c` clients run in one process against one cluster, and every read, write, index entry, and continuation must be byte-identical.
- **Binding-stress tester.** Randomized operation sequences validated against `libfdb_c`, replayable by seed.
- **Model-based chaos testing.** An in-memory model shadows the store while fault injection runs at transaction boundaries.

## Compatibility targets

| Component | Version |
|---|---|
| FoundationDB wire protocol | 7.3 (validated against 7.3.77) |
| Java Record Layer | 4.12.11.0 |
| Go | 1.26+ |

FDB 8.0 is future work. The wire protocol is not a stable third-party contract and changes between releases.

## Who's behind it?

An independent open-source project ([github.com/birdayz/fdb-go](https://github.com/birdayz/fdb-go)), not affiliated with or endorsed by Apple. "FoundationDB" is Apple's trademark, and this project uses the name to describe compatibility, not endorsement. Report issues and security reports per the repo's `SECURITY.md`.
