# Production Readiness Review: fdb-record-layer-go

Review date: 2026-06-19  
Repository: `/home/birdy/projects/fdb-record-layer-go`  
Branch: `fdbgo/c3-conflictrange-workload`  
HEAD: `000e1869d`  
Working tree: not clean at review time; local modification present in `pkg/fdbgo/fdb/conflictrange_workload_test.go`. This report evaluates the checked-out working tree, including that local change.

## Executive Verdict

This project is not ready for a broad "production-ready" declaration yet. That matches the repository's own README and SECURITY policy, both of which call the project pre-1.0 and not yet production-ready.

It is much closer than an early prototype. The record-store layer has substantial compatibility testing, real FoundationDB-backed tests, Java conformance coverage, cross-client differential work, nightly fuzz/stress workflows, a libfdb_c escape hatch, and a serious CI posture. For a controlled internal pilot, the strongest path is:

1. Pin a specific commit.
2. Prefer the record-store API first.
3. Run the repository's Bazel suites plus workload-specific conformance tests against the target FDB version.
4. Keep the libfdb_c backend available as an operational fallback.
5. Treat the SQL engine as usable but not generally production-safe until the pagination and continuation issues below are fixed.

The most important blockers are correctness risks in SQL pagination and physical LIMIT/OFFSET continuation handling. These can cause silent truncation, skipped rows, or duplicate/over-returned rows under paginated execution. Silent wrong results are production blockers.

## Scope

I reviewed:

- Project structure, README, production docs, TODOs, CI workflows, security policy, and release state.
- Record-layer cursor/continuation behavior.
- SQL result pagination and executor plan continuation behavior.
- Pure-Go FoundationDB client launch-readiness signals.
- Build, lint, test, and vulnerability-scan behavior in this checkout.
- Operational readiness areas: observability, resource limits, documentation, reproducibility, release process, and support posture.

I did not run the full nightly fuzz or nightly stress workflows locally. I relied on the workflow definitions for those and ran the main local gates listed below.

## Verification Performed

| Check | Result | Notes |
|---|---:|---|
| `go version` | Passed | `go1.26.4 linux/amd64`, matching README target. |
| `bazelisk --version` | Passed | Bazel 9.0.1, matching `.bazelversion` / README target. |
| `bazelisk test //... --test_tag_filters=-stress --test_summary=detailed --build_event_json_file=/tmp/fdbrl-review-bep.jsonl` | Passed | 51/51 Bazel test targets passed. One target executed, others were cached. `//pkg/fdbgo/fdb:fdb_test` executed and passed in 43.8s, including conflict-range tests in the local checkout. |
| `just lint` | Passed | No lint output. |
| `git diff --check` | Passed | No whitespace errors. |
| `go test ./...` from repo root | Failed / stopped after confirmation | Direct Go test mode is not clean because some tests require Bazel runfiles. `cmd/fdb-stacktester/bindingtester` failed with missing runfiles/Docker context, and conformance tests panic through `NewJavaInvoker()` when runfiles are absent. |
| `go test ./...` in `cmd/frl` | Passed | CLI submodule plain Go tests pass. |
| `govulncheck` over shipped `pkg/...` packages excluding `pkg/testcontainers` | Passed | `No vulnerabilities found.` Exclusion matches CI and SECURITY.md's documented test-only Docker SDK exception. |
| `git tag --list` | No tags found | No release tags observed in this clone. |

Additional size/context signals:

- 1,803 Go files.
- About 625,893 Go LOC under `pkg`, `cmd`, and `conformance`, excluding generated parser/gen paths.
- 8,022 `Test`/`Fuzz` functions found by source scan.
- 155 non-test `panic(` call sites and 22 non-test `recover(` call sites under `pkg` and `cmd`. This is not automatically a defect, but it makes panic-boundary discipline a production-readiness requirement.

## Production Readiness By Layer

| Layer | Readiness | Assessment |
|---|---|---|
| Record store | Close to controlled production pilot | Strongest layer. README identifies it as the most mature. It has broad cursor/index/record behavior tests and Java compatibility focus. Still requires pinned commit, conformance run, and workload-specific validation. |
| Pure-Go FDB client | Promising, controlled rollout only | Much stronger than expected for a from-scratch wire client: topology refresh, transaction timeout mapping, metrics/logging, differential tests, and libfdb_c backend exist. Remaining adversarial workload gaps still matter. |
| SQL engine | Not ready for general production | Large surface and good tests, but I found release-blocking pagination/continuation correctness risks. Use only for bounded, tested query shapes until fixed. |
| CLI / tooling | Useful but not a primary production interface yet | `cmd/frl` tests pass. Operational runbooks and release packaging still need work. |
| CI / test infrastructure | Strong | Main Bazel gate, generated-code check, Docker no-op guard, wire oracle, govulncheck, race, nightly fuzz, nightly stress, and libfdbc workflows are better than typical pre-1.0 projects. |
| Release/support process | Not production-ready | No tags found. README and SECURITY.md say latest master only / pin commit. Several docs are stale or contradictory. |

## Release-Blocking Findings

### P0 Candidate: SQL Pagination Can Treat Non-Terminal StartContinuation As End Of Results

> **RESOLVED — RFC-127 / PR #325 (2026-06-20).** Fixed: `paginatingRows.fetchPage` now decides exhaustion
> off `IsEnd()` (≡ `SOURCE_EXHAUSTED`, matching Java `RecordLayerIterator.java:91`), never `ToBytes()==nil`;
> a non-end nil-byte continuation routes through `IsOutOfBand()` (the `errIfDrainTruncated` precedent) →
> scan/time/byte → 54F01, in-band `ReturnLimitReached` (LIMIT 0) → clean exhaustion. **Severity
> downgraded from release-blocking to a latent/correctness hardening:** the reachability trace (in the PR)
> showed *no current Go cursor emits a no-next out-of-band+StartContinuation* — every leaf guards
> out-of-band with `scanned>0` → a resumable `BytesContinuation` (which the old code handled correctly),
> and composites carry serialized bytes or error-first (`mergeSort`, RFC-106a). So the only reachable
> nil-byte case was `LIMIT 0`, already correct. The fix still lands because the old logic violated Java's
> invariant and was a landmine for any future cursor emitting the state Java's Union/Sort/MapWhile produce.
> The audit's own "P0 *Candidate*" / "may not block" hedge was right to be cautious. Reviewed: Graefe +
> Torvalds + /code-review + codex + @claude, all green.

Impact: possible silent result-set truncation under paginated SQL execution.

Evidence:

- `pkg/recordlayer/cursor.go:88-101` defines `StartContinuation` as explicitly non-terminal: `IsEnd()` returns false, but `ToBytes()` returns nil because no position information is available.
- `pkg/relational/core/embedded/cascades_generator.go:1342-1355` gets the result-set continuation after each page. If `cont.ToBytes()` returns nil, it sets `r.exhausted = true`.
- `pkg/recordlayer/key_value_cursor.go:645-652` returns `StartContinuation` when a limit is hit before a concrete byte continuation exists.
- `pkg/recordlayer/cursor_combinators.go:106-123` also emits `ReturnLimitReached` with `StartContinuation` when the limit is reached before any previous result exists.

Why this matters:

`StartContinuation` means "not terminal, but no resumable byte position exists." The SQL pagination layer currently collapses that state into "exhausted." That is a semantic mismatch. In any plan shape where a cursor consumes work but produces no first row before a limit/time/scan boundary, SQL can stop early and report a complete result set.

This is especially dangerous because it fails closed from the API's perspective: the caller sees normal end-of-rows rather than an error.

Recommended fix:

- Never infer exhaustion from `ToBytes() == nil`.
- Treat non-terminal nil-byte continuations as a distinct state: either a typed "no resumable progress" error, or a real encoded continuation that can resume safely.
- Add SQL-level regression tests with an operator that consumes input before emitting its first row, then forces a row/scan/time limit before the first emitted row.
- Add a cursor contract test asserting that every caller checks `IsEnd()` before interpreting nil bytes.

Production decision:

This should block general SQL production readiness. It may not block record-store-only use if the affected SQL pagination path is not used.

### P1: Physical LIMIT/OFFSET Plan Continuation Does Not Preserve Skip/Remaining State

> **ANALYZED — RFC-128 (2026-06-20).** The literal finding (executor re-skip on resume) is a **real but
> SQL-unreachable** latent bug, mirroring the P0: Java's `SkipCursor`/`RowLimitedCursor` also do not
> envelope skip/limit (Go matches Java), Java never combines a non-zero skip with a resume continuation,
> the top-level SQL LIMIT is handled correctly by `paginatingRows`, and the only SQL-reachable nested
> `RecordQueryLimitPlan` (FlatMap inner) is re-run fresh per outer row — never resumed mid-window. **Not a
> Java divergence, not a prod blocker.** HOWEVER, verifying it surfaced a *genuinely reachable,
> deterministic wrong-results bug* in the same area — a **nested derived-table `LIMIT/OFFSET` is silently
> dropped and mis-hoisted** (`SELECT id FROM (… LIMIT 5 OFFSET 2) AS s WHERE id>4` returns wrong rows).
> That is the real defect; it is fixed under **RFC-128**, which also keeps the re-skip path shielded.

Impact: paginated execution of physical LIMIT/OFFSET plans can skip rows again, duplicate rows, or over-return rows across resumes.

Evidence:

- `pkg/recordlayer/query/executor/executor.go:792-823` implements `executeLimit`.
- It passes the incoming continuation directly to the child plan at `executor.go:818`.
- It then wraps the child cursor with `recordlayer.SkipThenLimit(innerCursor, offset, limit)` at `executor.go:823`.
- There is no plan-specific continuation that records how many rows have already been skipped or how many rows remain in the limit window.

Why this matters:

A continuation for `LIMIT 10 OFFSET 5` must represent both the child cursor position and the local state of the LIMIT/OFFSET operator. If a page boundary happens after some rows are skipped or returned, resuming with only the child continuation re-applies the original offset and resets the limit. Depending on where the page boundary occurs, this can drop rows or return too many rows across pages.

The top-level SQL wrapper may shield some simple user-facing cases by enforcing max rows externally, but nested/physical LIMIT plans still need continuation-correct semantics.

Recommended fix:

- Encode a LIMIT/OFFSET continuation envelope containing:
  - child continuation
  - number of offset rows already skipped, or remaining offset
  - number of rows already emitted, or remaining limit
- Add tests for LIMIT, OFFSET, and LIMIT+OFFSET with page boundaries after:
  - zero rows emitted
  - some offset skipped but no result emitted
  - some results emitted but limit not exhausted
  - exact limit boundary
  - nested LIMIT under another operator

Production decision:

This should block SQL GA and any workload that depends on paginated LIMIT/OFFSET correctness.

### P1: Plain `go test ./...` Is Not A Clean Contributor Or Adoption Path

Impact: reproducibility and adoption risk. Users and contributors running the standard Go test command get failures/panics unrelated to product correctness.

Evidence:

- `conformance/java_invoker_test.go:93-100` panics if the Java server cannot start. When run outside Bazel, missing runfiles caused `panic: Failed to start Java server: failed to create runfiles: runfiles: no runfiles found`.
- `cmd/fdb-stacktester/bindingtester/binding_test.go:56-59` requires a Docker context built from Bazel runfiles and fails outside the Bazel test environment.
- The root `go test ./...` run failed for those reasons and was stopped after the failure mode was confirmed.

Why this matters:

README appropriately emphasizes Bazel/just for full testing. Still, for a Go library, `go test ./...` failure is a production-readiness and contributor-experience smell unless it is clearly routed around. It also creates noise for downstream security scanners, package indexers, and vendor validation workflows.

Recommended fix:

- Put Bazel-runfiles-dependent tests behind a build tag such as `bazel`, `integration`, or `conformance`.
- Or skip cleanly when `TEST_SRCDIR` / runfiles are absent.
- Provide a documented plain-Go command for the supported subset, for example a `go list` filter that excludes runfile-dependent packages.
- Avoid panics in test helpers for missing environment; use `t.Skip` or explicit failure where possible.

Production decision:

This does not block runtime adoption if Bazel is the blessed test path, but it should be fixed before presenting the project as broadly consumable Go infrastructure.

## Important Non-Blocking Gaps

### P1: No Full Statement-Wide Memory Byte Budget Yet

Impact: resource exhaustion risk for large rows or cardinality-growing operators in multi-tenant deployments.

Evidence of existing protections:

- SQL options include row/time/scan limits: `pkg/relational/api/options.go:42-67`.
- `ExecuteProperties` includes `MaterializationLimit`: `pkg/recordlayer/scan_properties.go:161-168`.
- `CollectAllBounded` caps buffered row count and errors on out-of-band truncation: `pkg/recordlayer/query/executor/executor.go:2657-2678`.
- Buffered operators detect scan/time truncation and error rather than silently returning partial buffers: `pkg/recordlayer/query/executor/executor.go:2683-2699`.
- Returned result bytes are capped in SQL result delivery: `pkg/relational/core/embedded/cascades_generator.go:1063-1072`.

Gap:

`MaterializationLimit` is row-count based and defaults to 100,000 rows. That is not equivalent to a heap or byte budget. A query that buffers 100,000 large rows can still create unacceptable memory pressure. TODO.md also explicitly says the per-query memory byte budget was deferred to RFC-106b.

Recommended fix:

- Add statement-wide memory accounting for every cardinality-growing buffer.
- Charge approximate tuple/value payload bytes, not just row count.
- Enforce a configurable SQLSTATE-mapped resource error.
- Add CI coverage or a lint/test pattern that prevents new buffered operators from bypassing accounting.

### P2: Documentation And Source-Of-Truth Drift

Impact: production users cannot reliably tell which statements are current without reading source and CI.

Examples:

- README line 19 still says there is no drop-in escape hatch to the C client, but README lines 100-115 document a `libfdbc` build-tag escape hatch.
- `reports/feature_completeness.md` appears stale relative to the README's current Java target and supported feature list.
- `reports/wire_compat_audit.md` appears stale relative to the current continuation serialization work noted elsewhere.
- `TODO-production.md` contains older open items that are contradicted by current code and `TODO.md` entries.

Recommended fix:

- Mark historical reports as archived with date/commit headers.
- Create one generated/current readiness page with commit SHA, target versions, completed features, unsupported features, and links to authoritative tests.
- Keep `README.md`, `PRODUCTION_READINESS.md`, `TODO.md`, and `TODO-production.md` from contradicting each other.
- Add a release checklist item that fails a release if status docs disagree.

### P2: No Tagged Release Or Clear Support Window

Impact: production users have no semver artifact, changelog, or supported-version policy beyond "pin latest master."

Evidence:

- `git tag --list` returned no tags in this clone.
- `SECURITY.md:35-38` says only latest `master` is supported until a tagged v1 and users should pin a commit.
- README says pre-1.0 and not yet production-ready.

Recommended fix:

- Cut pre-1.0 tags once the SQL blockers are resolved, for example `v0.x`.
- Publish a changelog with compatibility notes for:
  - Record wire format
  - SQL behavior
  - FDB client option semantics
  - Required FDB / Java Record Layer versions
- Define support windows for tagged releases and security fixes.

### P2: FDB Option Semantics Need A Public Honored/Unsupported/No-Op Matrix

Impact: users can mistakenly believe a libfdb option is active when the pure-Go backend ignores it.

Positive evidence:

- Some unsafe options fail explicitly:
  - `SetReportConflictingKeys()` returns `UnsupportedOptionError`: `pkg/fdbgo/fdb/options.go:247-255`.
  - `SetRawAccess()` returns `UnsupportedOptionError`: `pkg/fdbgo/fdb/options.go:266-274`.
  - `SetAutomaticIdempotency()` returns `UnsupportedOptionError`: `pkg/fdbgo/fdb/options.go:281-285`.

Gap:

Many options still return nil while doing nothing or only partially mapping behavior, for example transaction logging, auto throttle tag, debug retry logging, include-port-in-address, causal read/write, and durability options in `pkg/fdbgo/fdb/options.go:198-204`, `238-240`, and `288-314`.

Some of these may be harmless compatibility stubs. The production issue is that the contract is not obvious to users.

Recommended fix:

- Publish a table with columns: option, pure-Go behavior, libfdb_c behavior, unsafe if ignored, test coverage, and recommendation.
- Convert any unsafe silent no-op into `UnsupportedOptionError`.
- Add tests that pin the table.

### P2: Panic Boundary Discipline Needs To Stay A Release Gate

Impact: process crashes are denial-of-service risks for library users and SQL frontends.

Evidence:

- Source scan found 155 non-test `panic(` call sites and 22 non-test `recover(` call sites under `pkg` and `cmd`.
- `SECURITY.md:30-32` explicitly scopes crash/DoS and says untrusted input must produce errors, never process crashes.

Assessment:

Many panics are likely intentional internal invariants or `Must*` APIs, and the project has recovery tests in places. This is acceptable only if all public/untrusted boundaries consistently translate panics to errors.

Recommended fix:

- Keep `docs/panic-audit.md` current and tie it to release gating.
- Add focused tests for parser, SQL planning, tuple/continuation decoding, and wire decoding panic boundaries.
- Avoid adding new production panics outside clearly documented internal invariants.

## Strengths

### Strong CI And Test Discipline

The CI setup is better than many projects at this maturity level.

- `.github/workflows/ci.yml:32-38` checks generated code and fails on diffs.
- `.github/workflows/ci.yml:40-47` has a Docker preflight so FDB testcontainers cannot silently skip in the primary merge gate.
- `.github/workflows/ci.yml:48-50` runs the main Bazel test gate.
- `.github/workflows/ci.yml:101-149` promotes deterministic wire-oracle testing to the PR path.
- `.github/workflows/ci.yml:151-171` runs `govulncheck` over shipped packages.
- `.github/workflows/ci.yml:173-231` gates race tests for the SQL layer and FDB client scopes, with no-op guards.
- Additional workflows exist for hosted smoke, nightly coverage, nightly fuzz, nightly libfdbc, and nightly stress.

The local Bazel gate passed in this checkout.

### Compatibility Is Treated As A Core Requirement

README claims Java Record Layer 4.11.1.0 wire compatibility and FDB 7.3.75 as the target. The repo has conformance tests, plandiff/yamsql tests, wire oracle work, and cross-client/libfdbc workflows. This is the right testing strategy for this kind of storage library.

### Pure-Go Client Has Important Operational Hardening

The pure-Go FDB client has several production-relevant fixes already present:

- Coordinator/topology refresh handles forwarded connection strings and on-disk cluster-file rereads: `pkg/fdbgo/client/topology.go:73-99` and `pkg/fdbgo/client/clusterfile.go:228-253`.
- Transaction read operations are bounded by transaction timeout and map the timeout to FDB error 1031: `pkg/fdbgo/client/readpath.go:89-115`.
- Metrics and structured logging hooks exist: `pkg/fdbgo/client/database.go:673`, `pkg/fdbgo/fdbmetrics/fdbmetrics.go:29`, `pkg/fdbgo/client/options.go:119-122`, and `pkg/fdbgo/client/clientmetrics.go:258-264`.
- The README documents a `libfdbc` build-tag backend escape hatch at lines 100-115.

### Security Process Exists

`SECURITY.md` gives private reporting guidance, scopes wire/data-integrity issues, pure-Go client divergences, crash/DoS, and SQL wrong results as security-relevant, and documents the test-only Docker SDK vulnerability exception. Local `govulncheck` over shipped packages found no vulnerabilities.

### Resource Limits Are Partially In Place

The SQL path has real controls for time, row, scan, materialization row count, out-of-band truncation, and returned result bytes. The remaining gap is memory byte accounting for intermediate buffers, not absence of resource-limit thinking.

## Operational Readiness Assessment

| Area | Status | Notes |
|---|---|---|
| Build reproducibility | Mostly ready with Bazel | Bazel path is strong. Plain `go test ./...` needs cleanup or documented exclusion. |
| Runtime configuration | Partial | SQL options and client options exist. Need public unsupported/no-op option matrix. |
| Observability | Partial to good | Metrics and slog hooks exist. Need operator guide, metric catalog, and alert suggestions. |
| Security | Good for pre-1.0 | Vulnerability scan clean for shipped packages. Security policy exists. Release support policy still immature. |
| Data compatibility | Strong intent, good coverage | Java/FDB compatibility is central and tested. Stale reports need cleanup so users can trust the current claim. |
| Failure recovery | Promising | Client retry/timeout/topology work is present. Remaining adversarial workloads in TODO.md should be completed for confidence. |
| Resource isolation | Partial | Row/time/scan/result-byte controls exist. Full memory byte budget is still missing. |
| Release management | Not ready | No tags observed, no semver/changelog/support window. |
| Docs | Mixed | README is useful and honest, but source-of-truth drift is a real adoption risk. |

## Recommended Production Gate

Before declaring broad production readiness:

1. Fix SQL pagination's handling of non-terminal nil-byte continuations.
2. Fix physical LIMIT/OFFSET continuations to preserve local operator state.
3. Add regression tests for both issues across page boundaries and resource-limit boundaries.
4. Make root `go test ./...` either pass for the supported subset or fail only behind explicit integration tags.
5. Complete or explicitly defer statement-wide memory byte budgets with clear deployment guidance.
6. Publish an FDB option behavior matrix for pure-Go and libfdb_c backends.
7. Consolidate production-readiness docs into one current source of truth.
8. Cut a tagged pre-1.0 release with changelog and support expectations.
9. Create an operator runbook covering:
   - supported FDB versions
   - cluster-file rotation
   - backup/restore expectations
   - metrics and alerts
   - recommended SQL limits
   - when to use `-tags libfdbc`
   - incident rollback procedure

Before a controlled production pilot:

1. Pin the reviewed commit or a later commit with the SQL blockers fixed.
2. Run `bazelisk test //... //cmd/fdb-stacktester/bindingtester:bindingtester_test --test_tag_filters=-stress` on production-like infrastructure.
3. Run the nightly stress/fuzz/libfdbc suites at least once on the candidate commit.
4. Add workload-specific differential tests against Java/libfdb_c for the exact record schemas and query shapes used.
5. Enable conservative SQL limits: transaction timeout, scanned rows/bytes, max rows, result bytes, and materialization row cap.
6. Keep `libfdbc` backend binaries available for rollback until the pure-Go client has more production mileage.

## Suggested Priority Plan

### Next 1-2 PRs

- Fix `StartContinuation` handling in SQL pagination.
- Fix LIMIT/OFFSET continuation state.
- Add SQL pagination regression tests that prove no silent truncation, duplicate rows, or row loss across resume.

### Next hardening wave

- Clean up root `go test ./...`.
- Add the option behavior matrix and tests.
- Add memory byte accounting RFC/implementation for buffered operators.
- Update stale docs and archive old reports.

### Release wave

- Decide v0 release criteria.
- Tag a release.
- Publish changelog and compatibility matrix.
- Add an operator runbook and production checklist.

## Bottom Line

This is a serious, well-tested pre-1.0 storage project with unusually strong compatibility and CI work. The record-store layer is the best candidate for controlled production use after a pinned-commit validation pass.

The project should not yet be marketed as generally production-ready because the SQL engine still has plausible silent-wrong-result risks around continuations and pagination, and the release/support/docs story is not mature enough for external production consumers. Fixing the two SQL continuation issues would materially change the readiness picture.

