# Production Readiness Checklist

This document collects the work needed to make `fdb-record-layer-go` credible as
a production-grade project that users can rely on, and as a near-term public HN
launch candidate.

The bar is not "all possible Record Layer features are complete." The bar is:

- users can understand exactly what is stable, experimental, and unsupported;
- compatibility and performance claims are reproducible;
- failures are explicit errors, not surprising panics or hangs;
- installation, upgrade, and operation paths are documented and tested;
- maintainers can respond quickly to bugs found by external users.

## Launch Gate

Before posting publicly, the project should have one current source of truth that
answers:

- What is production-supported today?
- What is experimental?
- What is intentionally unsupported?
- Which Java Record Layer and FoundationDB versions are compatibility targets?
- Which tests prove wire compatibility, SQL behavior, stress behavior, and pure
  Go FDB client correctness?
- How does a new user run the smallest useful example in under 10 minutes?

If the answer requires reading old audit reports, TODO notes, RFCs, and README
sections that disagree with each other, the launch is not ready.

## P0: Trust And Correctness

These are launch blockers because HN readers and early adopters will check them
first.

### Make Documentation Internally Consistent

Current state has drift between README, TODO, and reports. For example, some docs
say outer joins and subqueries are unsupported while newer notes say they are
implemented; older reports reference Java 4.2.6.0 while the README targets Java
4.11.1.0.

Actions:

- Replace stale feature-completeness reports with a generated or manually
  maintained `FEATURE_MATRIX.md`.
- Mark old reports as historical, with dates and a warning at the top.
- Keep README short and current; move detailed compatibility tables elsewhere.
- Add a "Known limitations" section that is specific, dated, and tested.
- Add a "What is experimental" section for pure-Go FDB client, SQL engine, vector
  indexes, and planner extensions if they are not yet supported as stable APIs.

Done when:

- README, TODO, feature matrix, and conformance docs do not contradict each
  other.
- Every README capability claim links to a test suite, report, or package doc.

### Publish Reproducible Compatibility Evidence

The project's value depends on Go and Java applications reading and writing the
same data safely.

Actions:

- Create a single `just conformance` command that runs the primary Go/Java
  compatibility suite or clearly explains required services.
- Publish the exact Java Record Layer artifact/version under test.
- Include a small bidirectional compatibility demo: Java writes, Go reads; Go
  writes, Java reads.
- Store conformance reports as CI artifacts with commit SHA, FDB version, Java
  version, and Go version.
- Add a "last green conformance" badge or table to README.

Done when:

- A fresh clone can reproduce a minimal compatibility test locally.
- CI has a discoverable, dated report for full compatibility validation.

### Audit Public Panics

Go library users expect invalid inputs and runtime failures to return errors at
public boundaries.

Actions:

- Inventory all `panic(...)` calls in non-test packages.
- Classify each as `internal invariant`, `constructor misuse`, `public input`,
  or `compatibility with FoundationDB Go binding`.
- Convert user-reachable panics to errors.
- Rename deliberately panicking constructors or helpers to `Must...`.
- Add recovery only at executor boundaries where panics model SQL errors, and
  document that behavior.

High-priority examples to review:

- metadata builder lookup for unknown record types;
- typed store scan type mismatches;
- tuple/key-expression encoding of unsupported user values;
- SQL value evaluation panics for type mismatch, invalid cast, overflow, and
  division by zero;
- pure-Go FDB future/database panics.

Done when:

- Public APIs have documented error behavior.
- Tests assert that invalid user input returns errors instead of crashing the
  process.

### Define Stability Boundaries

The codebase contains record-layer APIs, a SQL engine, a pure-Go FDB client,
conformance tools, stress tools, and experimental planner work. Users need to
know which surfaces are stable.

Actions:

- Add package-level docs for each top-level product surface.
- Identify stable APIs versus internal packages.
- Add semantic versioning policy before tagging v1.
- Explain whether SQL semantics are intended to match Java exactly or include
  Go-only extensions.
- Document data-format compatibility guarantees separately from query-planner
  behavior.

Done when:

- A user can decide whether to depend on `recordlayer`, `sqldriver`, `fdbgo`, or
  none of them without reading implementation files.

## P1: Reliability And Operations

These are not all HN launch blockers, but they matter before recommending the
project for serious workloads.

### Make The Full Test Pyramid Obvious

The project has a large test surface, but it should be easy to understand what
each tier proves.

Actions:

- Document test tiers:
  - unit tests;
  - FDB integration tests;
  - Go/Java conformance tests;
  - SQL corpus tests;
  - fuzz tests;
  - stress and benchmark tests.
- Add `just test-fast`, `just test-integration`, `just test-conformance`,
  `just test-stress`, and `just test-all` if missing.
- Ensure each test tier has expected runtime and prerequisites.
- Keep default PR CI fast but make excluded suites visible and regularly green.

Done when:

- External contributors know which command to run before opening a PR.
- Maintainers know which command must pass before a release.

### Strengthen CI For Release Confidence

The default CI already checks generated code and Bazel tests. Release confidence
needs separate heavyweight gates.

Actions:

- Add scheduled conformance CI with Java server and real FDB.
- Add scheduled stress CI with durable historical reports.
- Add race-detector jobs for packages that are concurrency-heavy.
- Add fuzzing budget and crash corpus publishing.
- Add release workflow that runs the heavyweight suites before tagging.
- Fail release builds if generated code, Bazel files, or protobuf outputs drift.

Done when:

- A release tag points at a commit with green unit, integration, conformance,
  stress, and generation checks.

### Document Operational Failure Modes

FoundationDB systems fail through retries, conflicts, timeouts, transaction size
limits, stale metadata, index states, and long-running online builds.

Actions:

- Add an operator guide covering:
  - FDB cluster file handling;
  - transaction retry behavior;
  - context cancellation;
  - transaction size and time limits;
  - online index build lifecycle;
  - index state transitions;
  - schema evolution safety;
  - backup/restore expectations;
  - observability hooks and metrics.
- Provide troubleshooting recipes for common FDB errors.
- Document how to roll forward after interrupted index builds.
- Document safe upgrade order for Go library, Java Record Layer, and FDB client
  versions.

Done when:

- A production user can operate the library without reading source code or Java
  Record Layer internals.

### Improve Observability Defaults

Users need enough visibility to debug slow queries, planner choices, FDB
conflicts, and index builds.

Actions:

- Ensure plan generation logs are easy to enable from `database/sql`.
- Expose timers/counters through a small stable interface.
- Add examples for OpenTelemetry or a generic metrics sink.
- Include query hash, plan hash, plan text, cache hit/miss, duration, rows read,
  records returned, and FDB retry/conflict counts where available.
- Add structured logging examples for online indexer progress.

Done when:

- A user can answer "why was this query slow?" from logs and metrics.

## P2: Usability And Adoption

These shape first impressions and reduce support load.

### Make Installation Boring

Actions:

- Add a minimal quickstart that works on Linux and macOS.
- Separate CGo FoundationDB binding setup from pure-Go client setup.
- Provide Docker Compose or a single script for local FDB.
- Document required FDB client library versions and where they must be installed.
- Add a "known platform support" table.
- Add examples for both embedded record-layer API and `database/sql`.

Done when:

- A new user can clone, start FDB, run an example, and run a small test without
  guessing.

### Clarify The Pure-Go FDB Client Story

The pure-Go client is a major differentiator and will attract scrutiny.

Actions:

- State clearly whether it is production-supported, beta, or experimental.
- Publish protocol coverage and known gaps.
- Document compatibility with FDB 7.3.x and future upgrade plan.
- Add benchmark methodology, hardware, FDB config, and raw outputs.
- Explain when to use the pure-Go client versus Apple's CGo bindings.

Done when:

- Performance claims are reproducible and scoped.
- Users understand the risk profile before replacing the official binding.

### Make API Examples Realistic

Actions:

- Add examples for:
  - schema definition and evolution;
  - save/load/delete;
  - secondary indexes;
  - online index build;
  - transactional retry;
  - pagination/continuations;
  - SQL DDL/DML/query;
  - Java/Go interoperability.
- Ensure examples compile in CI.
- Avoid examples that ignore errors in ways users may copy into production.

Done when:

- Documentation examples are tested and idiomatic.

### Reduce Repository Noise

Actions:

- Ensure local agent/worktree/cache directories are ignored.
- Keep generated code in predictable directories.
- Mark generated files clearly.
- Move historical shift logs and old audits out of the main discovery path, or
  index them under `docs/archive`.
- Keep top-level files focused on active project documentation.

Done when:

- A new contributor can understand the repository layout in a few minutes.

## P3: API Polish Before v1

These can happen after an HN post if clearly tracked, but should happen before a
stable v1 promise.

### Make Go APIs More Idiomatic

Actions:

- Revisit Java-style accessor names on public APIs.
- Prefer Go naming where compatibility does not require Java names.
- Avoid returning mutable internal maps/slices from getters.
- Use typed options for builders instead of long chains where appropriate.
- Add package examples and doc comments for exported identifiers.

Done when:

- `go doc` output reads like a native Go library, not a line-by-line Java port.

### Decide Compatibility Versus Idiom Explicitly

Some Java-shaped APIs may be worth keeping for porting familiarity.

Actions:

- Document which APIs intentionally mirror Java names.
- Provide idiomatic Go aliases where useful.
- Avoid breaking data-format compatibility for API aesthetics.

Done when:

- Naming choices look deliberate rather than unfinished.

### Publish A Versioning And Deprecation Policy

Actions:

- Define what breaks before v1.
- Define what will remain stable after v1.
- Document deprecation timeline and migration style.
- Add changelog discipline before public launch.

Done when:

- Users can assess upgrade risk.

## Security And Supply Chain

Actions:

- Run `govulncheck` in CI.
- Publish dependency update policy.
- Document how security issues should be reported.
- Keep generated code provenance clear: proto source, generator versions, and
  regeneration command.
- Avoid downloading tools at CI runtime without checksums where practical.

Done when:

- A production adopter can pass a basic dependency/security review.

## HN Launch Preparation

HN feedback will likely focus on correctness, compatibility, performance claims,
why this exists, and whether the project is production-ready.

Before launch:

- Update README to be honest and current.
- Add a short architecture page explaining the Record Layer, wire compatibility,
  and pure-Go FDB client.
- Publish a reproducible benchmark page with raw data and caveats.
- Publish a conformance page with current green results.
- Add a "production status" statement near the top of README.
- Prepare answers for:
  - Why not use Java Record Layer?
  - How compatible is this really?
  - Is the pure-Go FDB client safe?
  - What happens on FDB version upgrades?
  - Can I use this with existing Java-written data?
  - Which APIs are stable?
  - What is missing?

Suggested README status wording:

> `fdb-record-layer-go` is a Go implementation of the FoundationDB Record Layer
> data model and wire format. Core record storage, indexes, continuations, and
> Java interoperability are actively tested. The SQL engine and pure-Go FDB
> client are usable but still evolving; see the feature matrix and conformance
> report before using them for production workloads.

## Suggested Milestones

### Milestone 1: HN-Ready Public Preview

- Docs reconciled.
- Feature matrix current.
- Minimal Java/Go interoperability demo published.
- Quickstart works from a fresh clone.
- Public panics audited and high-risk cases fixed or documented.
- Current conformance and benchmark reports linked from README.

### Milestone 2: Production Beta

- Scheduled conformance and stress CI green.
- Operator guide complete.
- Observability examples complete.
- Race detector and fuzz workflows active.
- Release checklist documented.
- Security scanning active.

### Milestone 3: v1 Candidate

- Stable API surface declared.
- Public error behavior hardened.
- Versioning/deprecation policy published.
- Upgrade guide published.
- Compatibility matrix covers supported FDB and Java Record Layer versions.
- At least one real workload or external adopter case study documented.
