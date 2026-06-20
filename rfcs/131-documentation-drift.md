# RFC-131 — Reconcile documentation & source-of-truth drift

**Status:** Implemented (PR #329 — Torvalds + codex + @claude ACK; CI green)
**Item:** prod-readiness-audit-2026-06-19.md **P2** — "Documentation And Source-Of-Truth Drift."
**Reviewers:** Torvalds (doc consistency / honesty) + codex + @claude. *Not* a query-engine or wire
change (docs only) → no Graefe gate, no FDB-client gate.

---

## 1. Problem (verified)

A production user "cannot reliably tell which statements are current without reading source and CI"
(audit). Concretely, on the current tree:

1. **README self-contradiction.** `README.md:19` (client-maturity row) says the pure-Go client has
   "**no drop-in escape hatch to the C client yet**", while `README.md:104-106` documents exactly
   that escape hatch: `CGO_ENABLED=1 go build -tags libfdbc  # Apple's libfdb_c client (the escape
   hatch)`. Line 19 is simply false.
2. **Six stale `reports/*.md`, all dated 2026-03-09** — point-in-time snapshots that now read as
   current truth and contradict the live README + conformance suite:
   - `feature_completeness.md:5` — "34/70 core methods (49%)"; `:48` cites **Java 4.2.6.0** (the
     target is **4.11.1.0**, MODULE.bazel:116); lists RANK/TEXT/BITMAP_VALUE/… as MISSING though
     README:179-194 + the conformance suite show them implemented.
   - `conformance_coverage.md:5` — "149 conformance specs, 453 unit tests" (README:232 now cites 434
     conformance specs / 5320 Go test funcs); `:36-43` flags the SUM index as "CRITICAL … needs ~6-8
     specs" though it already has 38.
   - `behavior_compat_audit.md`, `go_style_audit.md`, `subspace_wire_compat.md`, `wire_compat_audit.md`
     — research-only / point-in-time audits with no "snapshot, not current" marker.
3. **`TODO-production.md` P1.7** (the README-rewrite item) is marked done, but the README:19
   contradiction it was supposed to fix survives — the rewrite was incomplete.
4. **Stale dates** — `README.md:146` "accurate as of 2026-06-07".

`PRODUCTION_READINESS.md` is current and well-structured (it is the doc that diagnosed this drift) —
it becomes the single authoritative current-status page.

## 2. Principle

Two doc classes, never mixed:
- **Living docs** (current truth, dated to HEAD): `README.md`, `PRODUCTION_READINESS.md`, `TODO.md`,
  `DIVERGENCES.md`. These must never contradict each other or MODULE.bazel.
- **Historical snapshots** (point-in-time, never edited to stay current): the `reports/*.md` audits.
  These must carry a visible "snapshot as of DATE @ COMMIT; superseded by the live docs + tests"
  header so a reader never mistakes them for current status.

The fix is to (a) move the snapshots out of the way and stamp them, (b) remove the living-doc
contradictions, and (c) add a cheap guard so the *specific* drift that bit us (a wrong Java version
leaking into a living doc, the README contradiction) can't silently return.

## 3. Change

0. **Lift the one LIVE finding out of the snapshots before archiving (Torvalds NAK fix).** The
   2026-03-09 reports are mostly superseded, but `wire_compat_audit.md` / `behavior_compat_audit.md` /
   `subspace_wire_compat.md` record a **real, untracked wire-compat gap**: Go reads record versions
   **only inline** at suffix `-1` (store.go:350; format version ≥ 6) and `formatVersionCurrent` is the
   newest format — there is **no read path for the legacy `RecordVersionKey=8` version subspace and no
   `omitUnsplitRecordSuffix` concept**, so a Go client opening a Java store created at **format version
   < 6 silently cannot see record-version (or unsplit-record-suffix) data**. That is the project's hard
   line. Archiving the only doc that records it under a "superseded — read the living docs" header would
   *bury* it. So **first** add it to `TODO.md` as an open wire-compat item ("Go has no read path for
   format-version-<6 record versions / unsplit records — subspace-8 + `omitUnsplitRecordSuffix`
   unimplemented; a Go client silently misses version data on legacy Java stores"). *Fixing* the read
   path is a separate wire-compat RFC (fdb-client/Graefe-not-applicable, C++/Java spec review) — this
   RFC only ensures the gap is tracked in a living doc, not lost to the archive.

1. **Archive the six `reports/*.md`** → `docs/archive/reports-2026-03-09/` unchanged, with a new
   `docs/archive/reports-2026-03-09/README.md` header: "Point-in-time audit snapshots taken
   2026-03-09 against an *earlier* Java target (4.2.6.0) and a much smaller test suite. **Superseded**
   — for current status read `PRODUCTION_READINESS.md`, `README.md`, and run the conformance +
   cross-engine differential + binding-stress suites. Kept for historical provenance only." `git mv`
   preserves history.
2. **Fix the README contradiction** (`README.md:19`): reword the pure-Go client note to reference the
   documented `-tags libfdbc` escape hatch instead of denying it. The maturity claim ("youngest,
   validated against libfdb_c via the binding tester") stays; only the false "no escape hatch yet"
   clause is corrected.
3. **De-stale README dates**: `README.md:146` — replace the hard-coded "accurate as of 2026-06-07"
   with a commit/suite-anchored phrasing ("validated by the yamsql corpus at HEAD") so it can't go
   stale by the day.
4. **Reconcile `TODO-production.md` P1.7**: note that the README escape-hatch contradiction is closed
   by *this* RFC (not the earlier partial rewrite).
5. **Make `DIVERGENCES.md` target explicit**: add a one-line "Divergences vs Java
   fdb-record-layer-core **4.11.1.0**" header.
6. **`PRODUCTION_READINESS.md` becomes the authoritative current page**: add a top banner with the
   target versions (Java 4.11.1.0, FDB C++ 7.3.75, Go 1.26.x), a "this is the single current-status
   source; `reports/` are archived snapshots" pointer, and links to the authoritative test suites.
7. **Doc-consistency guard (cheap, deterministic; Torvalds "anchor it" fix)**: a
   `docs_consistency_test.go` (pure unit test, no Docker) that:
   - **Derives the current Java target from `MODULE.bazel`** (parse the
     `fdb_record_layer_artifact`/version pin — the single source of truth) and asserts each living doc
     (README / PRODUCTION_READINESS / TODO / DIVERGENCES) that mentions a Java record-layer version
     mentions **that** version — not merely that the one magic string `4.2.6.0` is absent (which
     `4.2.6.x` / `4.3.x` would walk straight past). Anchored, not freeform.
   - Asserts the README does **not** contain a "no … escape hatch" clause while it also documents
     `-tags libfdbc` (the exact contradiction this RFC removes — the load-bearing revert-proof).
   - Asserts the six reports moved to `docs/archive/reports-2026-03-09/` and that the archive `README.md`
     header exists.

   This is the "release checklist item that fails a release if status docs disagree" from the audit,
   mechanized as a test so it runs every CI.

## 4. Executable spec (tests)

- `docs_consistency_test.go`: the Java target parsed from `MODULE.bazel` (currently `4.11.1.0`) is the
  version every living doc cites (anchored, not a `4.2.6.0`-absence check); README does not assert
  "no … escape hatch" while documenting `libfdbc`; `docs/archive/reports-2026-03-09/README.md` exists
  and the six reports moved there. Revert-proven (reintroduce the contradiction, or bump MODULE.bazel
  without updating a living doc → red).
- `TODO.md` carries the format-version-<6 read gap as an open wire-compat item (so archiving the
  reports loses no live finding).
- Manual: `grep -rn "4.2.6.0" --include=*.md .` returns hits only under `docs/archive/`.

## 5. Wire/behaviour impact

**None.** Docs + a doc-only unit test. No product code, no persisted bytes, no Bazel test-coverage
change to runtime targets. `git mv` keeps report history.

## 6. Scope

One PR: the `git mv` + the archive header, the README/TODO-production/DIVERGENCES edits, the
PRODUCTION_READINESS banner, and the `docs_consistency_test.go` guard. Cutting a real release / semver
tag is a **separate** P2 (RFC for "tagged release / support window") — not this RFC.
