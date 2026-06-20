# Archived audit snapshots — 2026-03-09

> **These are point-in-time snapshots. Do NOT read them as current status.**

The reports in this directory were generated on **2026-03-09**, against an **earlier Java target
(`fdb-record-layer-core` 4.2.6.0)** and a much smaller test suite than the project carries today
(e.g. `feature_completeness.md` reports "34/70 methods, 49%" and `conformance_coverage.md` reports
"149 conformance specs / 453 unit tests" — both long since overtaken). Several of their "missing" /
"at risk" / "untested" claims are **superseded** by work landed since.

**For current status, read the living docs and run the suites:**

- `PRODUCTION_READINESS.md` — the single authoritative current-status page (target versions,
  completed/unsupported features, links to authoritative tests).
- `README.md` — current feature surface and maturity.
- `DIVERGENCES.md` — current Go-vs-Java divergence ledger (target: Java fdb-record-layer-core
  **4.11.1.0**).
- `TODO.md` — open issues, including any wire-compat gaps these snapshots first noted that are
  still open (e.g. the format-version-<6 record-version read path).
- The conformance (`//conformance:conformance_test`), cross-engine differential, and binding-stress
  suites — the executable source of truth for wire/behaviour compatibility.

These files are kept only for historical provenance (they document the code paths examined at the
time). They were authored across several commits on **2026-03-09** (e.g. `055b65bd`, `ee48ec77`);
run `git log --follow <file>` for a given file's exact origin and its move from `reports/` to here.
