# SQL output-name qualification and lifted ORDER validation

Supersedes the AS-only ordinary eligibility claim in ../alias-phase-closure/README.md
and candidate e1496e0d87778d57a90b5fd3287f39a38da04dec. Actual NAKs are preserved
verbatim in ../alias-phase-reviews/. Named table attributes remain qualified;
unnamed VALUES attributes do not. ScopeSource.UnqualifiedOutput preserves this
SQL property independently of private runtime aliases. Bound star columns carry
it through expansion. Both ambiguity checking and output-slot binding now share
orderByOutputAliasNames. Removed the competing alias-to-column/index maps from
upgradeSortKeyValues. Expanded slots stay aligned with expanded projections,
including parse-only shell placeholders. UNION lifting retains authored ORDER
clauses for post-resolution duplicate checking; no early parser check restored.

Regressions cover unnamed column/constant/star collisions, named inline control,
explicit-alias-before-star exact output Value/source ordinal and discriminating
FDB rows, both catalog UNION paths, missing-name precedence and quoted segment
controls. Four applied, compiled, killed and restored mutations are retained.

Live Java probe: 43 asserted records. Java refuses unnamed mixed-star and named
inline-sort shapes with 0AF00; Go retains its existing read-side extensions. The
ordinary unnamed inherited/constant alias cases agree at42702, the table star
case agrees on exact ordered rows, and UNION repeated/missing names agree at
42701/42703. Do not turn the two Java refusals into positive parity claims.

The preceding frozen full run completed 93 uncached targets:92 passed,1 failed.
Only plan_shape.golden drifted: ambiguous_group_key_reread.yaml#1 had the old
42702/Ambiguous reference ID instead of42702/Ambiguous alias ID. The exact SQL
is now a retained Java/Go probe asserting BOTH exact messages before the single
golden line was changed. No blanket refresh, corpus admission or weakened gate.

Current affected execution passed four uncached targets (embedded, semantic,
sqldriver, explaindiff), population {"run": 9342, "passed": 9342, "skip": 0, "failed": 0}. Final full/race/fuzz/
stress verification and final implementation ACKs remain pending. Earlier tree
and invalid mutation qualifications in ../alias-phase-closure/README.md stand.
No commit/push/merge; WS-B–K and whole-upgrade completion remain open.
