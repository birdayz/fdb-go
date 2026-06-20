# RFC-132 — Tagged-release policy, support window & changelog

**Status:** Implemented (PR #330 — Torvalds + codex + @claude ACK; CI green)
**Item:** prod-readiness-audit-2026-06-19.md **P2** — "No Tagged Release Or Clear Support Window."
**Reviewers:** Torvalds (doc/process honesty) + codex + @claude. *Not* a query-engine or wire code
change → no Graefe gate, no FDB-client gate.

---

## 1. Problem (verified)

A production user "has no semver artifact, changelog, or supported-version policy beyond 'pin latest
master'":

- `git tag --list` is empty — no releases.
- `SECURITY.md` "Supported versions" says only the latest `master` is supported until a tagged `v1`.
- There is **no `CHANGELOG.md`** and **no release/support-window policy doc**. A user upgrading
  between two commits cannot see what changed for the four things that actually matter here: the
  **record wire format**, **SQL behaviour**, **FDB client option semantics**, and the **required
  FDB / Java Record Layer versions**.

## 2. The hard constraint: cutting the tag is the maintainer's call

This RFC provides the **machinery** for releases — a changelog, a versioning/support policy, a
release checklist — but does **not** cut a `v0.x` git tag. Whether/when the project takes its first
pre-1.0 tag (and what it asserts about stability) is the repository owner's decision, not an
automated one. The deliverables make that decision *ready to take in one command*; they do not take
it.

## 3. Change

1. **`CHANGELOG.md`** (Keep-a-Changelog format, pre-1.0 conventions). A short "how to read" header,
   then an **`## [Unreleased]`** section with the standard `Added / Changed / Fixed` buckets **plus a
   fixed `### Compatibility` block** carrying the four audit-required notes every entry must address:
   - **Wire format** — did record/index/version/continuation/split bytes change? (Default: *no* — wire
     compat with Java `fdb-record-layer-core` 4.11.1.0 is the hard line.)
   - **SQL behaviour** — added/changed query surface or results.
   - **FDB client option semantics** — any option now honoured / newly rejected / still no-op.
   - **Required versions** — Java 4.11.1.0, FDB C++ 7.3.75, Go 1.26.x (the `MODULE.bazel` pins).

   `Unreleased` is seeded with the current notable surface (the audit hardening: RFC-127 pagination,
   RFC-128 LIMIT envelope, RFC-129 `go test` cleanliness, RFC-130 statement memory budget, RFC-131
   doc reconciliation) so the first cut release has real content. It does **not** back-fill all git
   history — a "this changelog starts 2026-06-20; earlier history is in `git log`" note covers that.

2. **`RELEASE.md`** — the release & support-window policy:
   - **Versioning** — pre-1.0 semver semantics: `v0.MINOR.PATCH`; minor may carry breaking *API*
     changes (Go signatures), patch is fixes only; **the FDB wire format is NOT covered by the
     pre-1.0 API-instability caveat — it stays compatible with Java 4.11.1.0 across every tag** (the
     project's whole point).
   - **Support window** — the latest tagged minor is supported; security fixes land on the latest
     minor; older minors are best-effort until `v1`. Until the first tag, `master` is the only
     supported ref (consistent with `SECURITY.md`).
   - **Release checklist** — green CI on the tag commit; `docs_consistency_test` (RFC-131) green;
     `CHANGELOG.md` `Unreleased` → `vX.Y.Z (date)`; the four compatibility notes filled; `MODULE.bazel`
     version pins confirmed; **then** the maintainer cuts `git tag vX.Y.Z` + a GitHub release.
   - Explicit "**cutting the tag is the maintainer's decision**" line (§2).

3. **`SECURITY.md`** — point "Supported versions" at `RELEASE.md` so the support policy has one home
   and the two docs can't drift.

## 4. Executable spec (tests)

This is policy + changelog prose; the load-bearing guard already exists (RFC-131
`docs_consistency_test.go`). Extend it minimally so the new docs can't rot:

- `CHANGELOG.md` and `RELEASE.md` exist and are non-empty.
- `CHANGELOG.md` has an `## [Unreleased]` section and a `### Compatibility` block (the four notes
  aren't silently dropped).
- The Java target named in `RELEASE.md` / `CHANGELOG.md` equals the `MODULE.bazel` pin (they're now
  living docs — add them to the guard's `livingDocs` set so the version anchor covers them).
- **Close the C++/Go gap (Torvalds):** the existing anchor only catches the 4-part record-layer
  version. Add two context-anchored checks so the docs can't assert a stale **FDB C++** or **Go**
  version either: (a) parse the `foundationdb` `version =` pin from `MODULE.bazel`; any FDB/`libfdb_c`-
  qualified 3-part version in a living doc must equal it; (b) parse the `go` directive from `go.mod`;
  any `Go`-qualified version in a living doc must share that major.minor (`1.26.x` and `1.26.4` both
  pass; `1.25` fails). Now every version a living doc asserts is pinned to a source of truth — no
  "asserts a version no test enforces" hole.

Revert-proven: delete the `Compatibility` block, or stale any of the three versions (Java / FDB / Go)
→ red.

## 5. Wire/behaviour impact

**None.** New docs + a doc-only test extension. No product code, no persisted bytes, **no git tag
cut** (that stays the maintainer's action).

## 6. Scope

One PR: `CHANGELOG.md`, `RELEASE.md`, the `SECURITY.md` pointer, and the `docs_consistency_test.go`
extension. The actual `v0.x` tag + GitHub release is explicitly **out of scope** (maintainer's call).
The FDB option honoured/unsupported/no-op matrix is its **own** P2 (separate RFC) — `CHANGELOG.md`'s
"FDB client option semantics" note will link to it once it lands.
