# Attribute qualification and UNION output-contract closure

Repairs NAKs on tree8d3544bffc4d5b322626616a3051b39fb8b2584a; actual reviews
are preserved verbatim in ../alias-source-reviews/.

Source-wide qualification is not sufficient: Java prepends an unqualified whole
struct ephemeral alias after qualifying a named unnest operator's ordinary
members (LogicalOperator.generateExplode). Column.UnqualifiedOutput preserves
this independent attribute property; Ephemeral remains visibility only. Eligible
names now consider both source-wide and attribute-specific qualification.
A member-column control still resolves the explicit output alias's exact Value.

UNION ordering reads ExactLogicalOutputLabels for the published LEFT output,
including the enclosing CTE registry, instead of searching for an optional
LogicalProject. It resolves names before checking authored duplicates. Bare-star
unions (both legs or left only), missing-name precedence, distinct-key success,
exact output ordinals and FDB rows are pinned. Qualified multi-segment keys use
their bare structural segment; a quoted name containing a dot remains one name.
The two-key quoted/qualified control asserts output slots1,2, not just success.

Java probe47 cases PASS, including exact whole-object precedence42702 and bare
star UNION42701/42703 cases. Four uncached affected targets PASS, population
{"run": 9352, "passed": 9352, "skip": 0, "failed": 0}. Three mutations applied/compiled/killed/restored: attribute
qualification, optional-projection bypass, and qualified-vs-quoted union binding.
An initial attribute mutation failed compilation (unused column), ran zero tests,
and receives NO mutation credit. The compiled retry logs are included.

Preceding frozen full verification was interrupted at NAK with SIGINT to its
owned Bazel client; sources remained frozen until runner exit/hash check. Its
partial execution is NOT a full green. Current full/race/fuzz/just-test/ABBA and
implementation delta ACKs remain pending. Prior invalid-mutation and bounded
latency qualifications stand. No commit/push/merge; WS-B–K remain open.
