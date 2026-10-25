# RFC-278: Re-verify the Go-only extensions and the SQL conformance matrix on Java 4.14.2.0

**Status: Implemented**

## Problem

DIVERGENCES.md's "Go-Only Extensions" section was classified against Java 4.12.11 and never
re-verified on the 4.14.2.0 target. SQL_CONFORMANCE.md's matrix was measured against 4.12.11.0,
and its one corpus number ("150 of Java's own yamsql files pass") gave no population: not the
skipped files, not the plan assertions Go never runs. Both documents state claims about Java that
no test checks, so a Java bump cannot show which of them went stale.

## Investigation

Each Java claim was read against the 4.14.2.0 source and then measured on a live 4.14.2.0 JVM with
the cross-engine runner (`plandiff.NewJavaRunnerHTTP` beside `NewGoSQLSetupRunner`). The
measurement contradicts several claims:

- GROUP BY is planned whenever an index or the primary key orders the grouping columns; only
  unordered input is refused (0AF00).
- SELECT DISTINCT is not rejected: no SQL-layer visitor reads the keyword, and duplicates come back.
- Java has depth-first recursive CTEs (`TRAVERSAL ORDER PRE_ORDER`, since 4.7.3.0), and runs a
  recursive UNION as UNION ALL.
- LIMIT/OFFSET, scalar subqueries and CROSS JOIN are not supported by Java (0AF00, 42601, XXXXX),
  although the matrix marked them Y or the summary said both engines answer.
- Go now refuses scalar NOT NULL with Java's 0A000.
- Several SQLSTATE rows were wrong in both engines' columns (IN-list mismatch is 22000, an unknown
  scalar function 0AF00, INT32 overflow 22000) or Java's (integer overflow is XXXXX).

The corpus number comes from `pinnedLedger`, which the Java corpus run already measures and pins.

## Fix

1. `DocClaimsJavaProbe` (`conformance/doc_claims_java_probe_test.go`, in
   `//conformance:conformance_probes_test`): one case per Java claim of both documents, run on both
   engines, with Java's and Go's answers pinned. A Java bump that changes an answer fails the probe
   and names the case, which names the doc row.
2. The documents cite the probe cases; rows not probed say which test measures them or that they
   are read from source. `TestDocClaimsCited` (`//conformance:doc_claims_cited_test`, fast lane)
   checks that every case is cited and pinned, that every name the two documents cite is a case or
   a cross-engine corpus entry, and that each matrix row's Java and Go cells agree with the pins of
   the cases it cites (Y requires correct answers, N only wrong results or rejection, P mixed
   support; documented source-backed aggregate-index support supplies the unprobed arm).
   Negative-syntax and duplicate-free-tree controls do not establish feature support;
   error rows check the SQLSTATE in each engine's pin.
3. `TestSQLConformanceStatesTheLedger` (javacorpus) renders SQL_CONFORMANCE.md's corpus block from
   `pinnedLedger`, the vendored `VERSION` and the corpus's plan-assertion census, and fails when the
   committed block differs. The block states passing, failing and skipped files by class, and the
   skipped assertions. The same test fails when either document's heading names a Java version
   other than the vendored one.

## Performance

Test-only. The probe adds about 25 s to the Java conformance lane; the guard is a unit test.

## Test plan

- The probe passes three consecutive runs (`--runs_per_test=3`). `TestDocClaimHelperArms` drives
  every arm of the citation, pin and matrix-cell checks with synthetic documents, including the
  vacuity checks. Wrong-result Y/P and mixed-support Y/N relabelings are negative controls.
- The plan-assertion census has fixture controls for all three directive kinds, file populations,
  negative-parse filtering, errors and empty/all-negative populations.
- The guard fails on a block that differs from the rendering (checked with a placeholder) and
  passes with the rendered block; `TestLedgerStatementArms` drives the parser's error arms, empty
  class lists and the skipped-assertion filter.
- `just test`, then `just test-full` for the Java conformance target.
