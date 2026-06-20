# Changelog

All notable changes to `fdb-record-layer-go` are recorded here. Format:
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning per `RELEASE.md`
(pre-1.0 `v0.MINOR.PATCH`).

**This project is pre-1.0.** The **Go API may change across minor versions**; the **FDB wire
format stays compatible with Java `fdb-record-layer-core` 4.11.1.0 across every release** (the
shared-cluster hard line — see `RELEASE.md`). Every entry's **Compatibility** block answers the four
questions a user upgrading between two refs needs: wire format, SQL behaviour, FDB client option
semantics, and required dependency versions.

This changelog starts **2026-06-20**; earlier history is in `git log`. There are **no tagged
releases yet** — cutting the first `v0.x` tag is the maintainer's decision (`RELEASE.md` §Versioning).

## [Unreleased]

### Added
- Statement-wide **memory byte budget** for the SQL executor: opt-in `OptMaxStatementMemoryBytes`
  bounds every cardinality-growing buffer by bytes (not just the 100k-row `MaterializationLimit`);
  breach → SQLSTATE `54F01`. Default `0` = unlimited (RFC-130).
- A documentation-consistency CI guard (`pkg/docscheck`) that fails the build if a living doc drifts
  from the `MODULE.bazel` / `go.mod` version pins or reintroduces a known contradiction (RFC-131/132).
- A public **FDB client option matrix** (`pkg/fdbgo/fdb/OPTIONS.md`) classifying every `Set*` option
  as honored / `UnsupportedOptionError` / safe no-op, each with its `libfdb_c` 7.3.75 reference, plus
  a completeness guard that fails CI if an option is added without a matrix row (RFC-133).
- A **panic-boundary release gate** (RFC-134): a `norecover` nogo analyzer fails the build if a
  `recover()` is added outside the documented panic→error boundary allowlist (`docs/panic-audit.md`
  §2), plus docscheck guards that keep the four input-boundary fuzz nets wired and the doc in lockstep
  with the allowlist. Makes the "untrusted input → error, never crash" discipline self-enforcing.

### Changed
- SQL `LIMIT`/`OFFSET` now flows through a single uniform `RecordQueryLimitPlan` + continuation
  envelope, including for nested derived tables (RFC-128).

### Fixed
- SQL pagination no longer treats a non-terminal `StartContinuation` as end-of-results (a latent
  early-truncation bug); exhaustion is decided off `IsEnd()`, not byte-emptiness (RFC-127).
- Pure-Go FDB client: `Get`/`GetRange` read-conflict ranges are clamped to the data actually returned
  and filtered through the RYW overlay, matching `libfdb_c` (no under-conflict; RFC-121).
- `go test ./...` is clean from a fresh checkout (the Bazel-runfiles-only suites are build-tagged so a
  plain `go test` no longer panics; RFC-129).
- Pure-Go FDB client: three **database-level transaction defaults** that change read semantics are now
  honored instead of silently dropped — `SetSnapshotRywDisable`/`Enable` (a cumulative counter,
  matching `libfdb_c`), `SetTransactionBypassUnreadable`, and `SetTransactionCausalReadRisky`. They
  propagate to each new transaction via `applyTxDefaults` and are replayed idempotently across retries
  (RFC-133).

### Compatibility
- **Wire format:** unchanged. Records, indexes, versions, continuations, split records remain
  byte-identical to Java `fdb-record-layer-core` 4.11.1.0. *Known gap:* no read path yet for
  format-version-<6 record versions on legacy Java stores (tracked in `TODO.md`).
- **SQL behaviour:** net additions only (memory-budget option; the LIMIT-envelope and pagination
  fixes correct latent bugs, they don't change correct-query results).
- **FDB client option semantics:** now documented option-by-option in `pkg/fdbgo/fdb/OPTIONS.md`
  (honored / `UnsupportedOptionError` / safe no-op, vs `libfdb_c` 7.3.75). **One behavioural change:**
  three database-level defaults (`snapshot_ryw_disable`/`enable`, `transaction_bypass_unreadable`,
  `transaction_causal_read_risky`) that were previously silent no-ops now take effect on each new
  transaction — a caller that set them and relied on them being ignored will now see them applied
  (this is the faithful `libfdb_c` behaviour). The unsafe access/auth/quota family still fails loud
  with `UnsupportedOptionError`; no option's *wire* behaviour changed (RFC-133).
- **Required versions:** Java `fdb-record-layer-core` **4.11.1.0**, FDB C++ client **7.3.75**, Go
  **1.26.x** (the `MODULE.bazel` / `go.mod` pins; the CI doc-guard enforces docs match them).
