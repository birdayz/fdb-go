# Resolved attribute names and UNION output cardinality

Repairs Torvalds/independent NAKs on c48ee4de085fbc77ebe98b857c002b6d00a6300d;
actual reports (including Graefe ACK) retained in ../alias-contract-reviews/.

SELECT name eligibility now resolves the complete identifier path. A qualified
reference X.X can return Java's original unqualified whole-object attribute X;
it is not excluded because its authored reference has two segments. Nested-field
access remains qualified, and ordinary member controls retain exact owners.
Qualified projection binding accepts exact whole-element QOVs for records as
well as scalars. Both constructors require42702/Ambiguous alias X for the exact
qualified reproducer. A real-FDB derived-member query exercises its successful
whole-record publication and returns3,9 rather than merely compiling it.

UNION label cardinality is retained: ambiguous joined-star ID cannot select an
arbitrary slot, and42702 precedes repeated-key42701. Merely publishing repeated
labels remains legal. Explicit positions and unique aliases are positive controls.
The positional-star control exposed two additional defects, both fixed:
- lifted right-branch classification lacked the source-backed star width;
- when normalization was necessary, its common row retained duplicate datum
  keys although the projection constructor uniquified them.
The classifier now prepares the source scope before classification, gating star
expansion on the same GROUP/positional/mixed-star shapes as ordinary SELECT.
The common UNION row uses existing DedupFieldNames ONLY when normalization is
required; already-matching branch rows retain the no-projection Java path.
SQL labels stay duplicated. Exact-result and FDB tests pin both populations.

An initial unconditional star expansion added unwanted projections; a direct
projection-elision regression pins the gate. An initial unconditional row-name
normalization broke existing cte_published_row_names#39/#41. Restoring the
already-aligned fast path fixed those exact queries, without changing corpus,
allowlist, or golden expectations. The new exact-row test covers unchanged rows
with repeated names, required normalization with distinct private names, and the
actual positional-star reproducer; all retain original SQL labels.

Java50 records PASS; five full affected targets PASS uncached, population
{"run": 9886, "passed": 9886, "skip": 0, "failed": 0}. The full explaindiff golden is unchanged from the single
previously Java-verified diagnostic edit. Six applied/compiled/killed/restored
mutations retained: resolved-attribute eligibility, record QOV admission, union
cardinality, lifted star width, normalized field names, aligned-row preservation.
One attempt was interrupted by the user mid-mutation; the remaining exact guard
was inspected/restored before resuming. The final six-mutation run completed and
restored every file. A nonunique mutation pattern aborted BEFORE applying that
mutation; it receives no credit. Prior build-only and invalid mutations remain
uncredited, scoped historical evidence only.

Previous full suite was deliberately interrupted at the NAK; no partial green.
Final frozen full/race/fuzz/just-test/ABBA and delta ACKs remain pending. No commit,
push or merge; WS-B–K remain open; historical missing startup-timeout artifacts
and accepted bounded latency conclusions unchanged.
