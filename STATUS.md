# Project status

**Pre-1.0; not declared production-ready.** This readiness statement covers
v0.2.0 and the development tree after it. It certifies neither a release nor a
particular deployment. Pin a version and evaluate it against your workload.

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

These are open and planned for v0.3.0. Each has a workaround where one exists.

- **Per-RPC 5 s timeouts on commits and reads, which libfdb_c does not have.** A
  commit whose reply takes longer fails with `commit_unknown_result` (1021); a
  slower read reply is abandoned and re-sent, to another replica first. Make
  transactions idempotent and retry 1021 as maybe-committed, as with libfdb_c.
- **A commit's wait for its reply ignores the transaction timeout and `Cancel`.**
  The 5 s limit above ends it. Bound the caller with a context deadline.
- **Read-only transactions allocate more than libfdb_c's.** It was 38 KB per
  transaction before v0.2.0's allocation cuts and has not been re-measured. Batch
  reads into fewer transactions on allocation-sensitive paths.
- **Limited real-cluster fault testing.** A coordinator-restart recovery
  regression runs in CI; process kills, partitions and coordinator changes are not
  yet tested against a real cluster. Rehearse them on your cluster (below).
- **Planning cost.** Planning a query takes milliseconds, and a large join can
  take seconds. Prepared statements reuse plans across bindings; a context
  deadline bounds planning, which has no budget of its own by default (as in Java).
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
have since been removed. They are not current readiness evidence. `PORT.md`,
`road-to-prod.md`, `PRODUCTION_READINESS.md` and `TODO-production.md` are historical
material, not competing current verdicts.

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
