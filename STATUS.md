# Project status

**Pre-1.0; not declared production-ready.** This readiness statement covers
the v0.2.0 release candidate. No v0.2.0 tag or deployment certification is
asserted here. Pin a revision and evaluate it against your workload.

The project contains three independently useful layers:

| Layer | Scope | What to evaluate |
|---|---|---|
| Pure-Go FoundationDB client (`pkg/fdbgo`) | FDB 7.3 protocol implementation; default backend | Retries, cancellation, recovery and transaction semantics against libfdb_c and your cluster topology |
| Record Layer (`pkg/recordlayer`) | Record storage, indexes, schema evolution and cursors | Bidirectional Java interoperability for the exact metadata, index types and store format you use |
| SQL (`pkg/relational`) | Embedded SQL engine and `database/sql` driver | Query correctness, resource limits, plans and performance for your queries; SQL continuations are engine-private |

The Java reference is **Record Layer / Relational 4.14.2.0**; the FDB client and
test-cluster pin is **7.3.77**. These are compatibility targets, not blanket
compatibility with every feature or every server release. See
[compatibility](docs/compatibility.md) for exceptions and source references and
[upgrading](docs/upgrade.md) before opening existing data. `MODULE.bazel`,
`go.mod` and `.bazelversion` are the dependency/build pins.

## Known gaps in v0.2.0

These five launch gaps remain open in the release candidate. Further work is
planned for v0.3.0; each has a mitigation where one exists.

- **Per-RPC 5 s read timeouts, which libfdb_c does not have.** A read reply
  slower than 5 s is abandoned and re-sent, to another replica first; libfdb_c
  keeps waiting. After 10 timed-out re-sends the read fails with a retryable
  `transaction_too_old` (1007). Location lookups have the same 5 s cap per
  attempt, where libfdb_c waits for the reply or a proxy change. Commits have
  no such timeout (#882).
- **Read-only transaction allocation remains a launch concern.** The historical
  observation was about 38 KB per transaction before allocation cuts; there is
  no current-cut allocation measurement here. Go heap counters exclude the
  native C++ heap, so they do not establish total-memory ratios. Measure your
  workload; batching reads can amortize transaction overhead.
- **Limited real-cluster fault testing.** The `test-full` coordinator-restart
  regression kills every fdbserver and checks recovery with the Go client first.
  Broader role failures, partitions and coordinator reconfiguration are not
  established by that case. Rehearse them on your cluster (below).
- **Planning cost remains a launch concern.** Historical timings do not establish
  latency for the cut or your queries. Prepared statements reuse plans across
  bindings; bound planning with a context deadline and opt into task/queue/match
  budgets, since there is no task budget by default (as in Java).
- **No resource limits by default** (as in Java). Set them in the DSN:
  `max_rows`, `execution_scanned_rows_limit`, `execution_scanned_bytes_limit`,
  `execution_time_limit`, `max_statement_memory_bytes` and `transaction_timeout`.

## Evidence and its limits

- `just test` is the **fast lane**: unit tests, nogo and bounded integration
  tests. It excludes `test-full`, `conformance_java`, `stress` and manual targets.
- `just test-full` includes all Bazel test targets, including the heavyweight
  Java conformance, client differential, SQL and stress suites at their default
  budgets. It requires Docker and the declared external dependencies. It is not
  every extended seed, race or active-fuzz configuration.
- `conformance/` tests shared record-store data with Java;
  `pkg/relational/conformance/` tests SQL behavior;
  `pkg/fdbgo/bench/` contains client differentials against libfdb_c.
  A passing case establishes its tested scope, not universal parity.
- [CI](https://github.com/birdayz/fdb-go/actions/workflows/ci.yml) and its artifacts
  are evidence for their **exact commit and executed lanes**. A badge alone does
  not establish a full-suite pass. Report cached results as cached.

No fresh full-suite, recovery or workload result is asserted by this page.
Historical production-tier verdicts relied in part on scheduled workflows that
have since been removed. They are not current readiness evidence. The former
`PORT.md`, `road-to-prod.md` and `PRODUCTION_READINESS.md` remain in git history;
`TODO-production.md` is historical material, not a competing current verdict.

## Before relying on a deployment

Review the compatibility and upgrade pages; test your schemas and queries with
both clients/engines where you intend to mix them. Exercise recovery, timeouts,
retries and load on a representative cluster. Rehearse backup/restore and rollback
before admitting writes from a new binary. Set explicit workload limits rather
than assuming defaults protect your service. Consult the [operator guide](docs/operations.md)
and [multi-tenant guide](docs/mt-saas.md).

Use [RELEASE.md](RELEASE.md) for release policy, [CHANGELOG.md](CHANGELOG.md) for
changes, [TODO.md](TODO.md) for engineering work, and [SECURITY.md](SECURITY.md) for
vulnerability reporting. None of these substitutes for a deployment's acceptance
tests or a maintainer's release decision.
