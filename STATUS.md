# Project status

**Pre-1.0; not declared production-ready.** This is the current readiness
statement for the development tree, not a certification of a tagged release or
of a particular deployment. Pin a commit and evaluate it against your workload.

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
