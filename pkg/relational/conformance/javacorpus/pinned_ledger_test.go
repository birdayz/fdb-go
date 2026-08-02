package javacorpus_test

// pinnedLedger is the MEASURED outcome of running all 238 vendored corpus
// files against the Go engine.
//
// It is a measurement, not a target. RFC-201 §8 makes it the public statement
// of what is and is not supported: `pass` is the count of files whose every
// assertion held, `fail` must stay zero, and each skip class is a named
// specification gap with a size. A phase that closes a gap moves counts between
// classes and updates this line in the same commit; a count moving for any
// other reason is exactly the drift this pin exists to catch.
//
// Read it as three groups:
//
//   - `polarity:*` and `fragment` — the corpus's own meta-tests. 25 files the
//     parser must refuse, 24 that must fail at execution and do, 9 whose
//     polarity Java defines only against a version it pins, 2 include-only
//     fragments. None of these measure the engine.
//   - `unsupported-DDL:*` and `unsupported:*` — capabilities a later RFC-201
//     phase adds, plus the two directives ruling 2 declines permanently.
//     `value-index-as-select` — once the corpus's largest class at 42 files —
//     is DELETED: RFC-202 S2 (value arm) took it to 3 and S3 (aggregate arm)
//     to 0. Its last three files landed measured: composite-aggregates now
//     PASSES, bitmap-aggregate-index is the bitmap-aggregate-QUERY gap
//     (engine-gap:planner-declines, pinned in gaps.go), and
//     index-ddl-aggregates-only waits on CREATE VIEW (unsupported-DDL:other).
//     `struct` is now the largest class. RFC-202 S4 closed
//     `engine-gap:row-version-pseudocolumn` (the class is deleted):
//     join-tests-row-version now PASSES, and pseudo-field-clash runs its
//     version ISCAN/COVERING blocks and dies only at a pre-existing
//     multi-element record-constructor decline (engine-gap:planner-declines,
//     pinned in gaps.go) — measured version-independent.
//   - `engine-gap:*` and `conformance:*` — divergences this run FOUND, each
//     pinned to its exact rejection in gaps.go and each booked.
//
// RFC-202 S5 (the sparse-index predicate arm) moved seven files: boolean-ddl
// and filter-index PASS (their WHERE indexes build, and the quoted-column
// storage-name fix let their key expressions validate); join-tests-outer's
// earlier blocks pass after the quoted-alias USING desugar fix and the file
// books to go-accepts on its ORDER-BY-nullable-side polarity row;
// sparse-index-tests books to go-accepts on the USE INDEX hint polarity;
// recursive-cte progresses to the nested-recursive-WITH gap;
// simple-include-different-env reaches its explain directive
// (plan-assertion); index-ddl-values-only stays unsupported-DDL:other, now
// blocked on CREATE VIEW.
//
// RFC-202 S6 (the ON-source front end) moved one file:
// documentation-queries/index-documentation-queries PASSES — its five
// ordered ON-source indexes (DESC / NULLS LAST / DESC NULLS FIRST) build
// through the generator with their OrderFunctionKeyExpression wrappers.
// index-ddl, index-ddl-values-only and index-ddl-aggregates-only progress
// past their INCLUDE/orderClause rejections and now rest at CREATE VIEW
// (unsupported-DDL:other), the class's remaining cause.
//
// `polarity:negative-execution` appears in BOTH groups on purpose: the file
// entry says the negative failed, and the inner entry carries the failure text
// so the log shows WHY. A negative credited for failing the wrong way — dying
// in setup before the assertion upstream is testing — is otherwise
// indistinguishable from one that failed as designed, and two were.
//
// `queries` counts result-consuming configs actually asserted against the
// engine. It is the honest denominator behind `pass`, and it deliberately
// EXCLUDES `noChecks` queries: those execute but assert nothing, so counting
// them would let a file whose only query is config-less report a pass.
const pinnedLedger = "pass=54 fail=0 skip=184 queries=1256 file_skips{conformance:go-accepts-what-java-rejects=3," +
	"engine-gap:catalog-system-tables=2,engine-gap:comma-join-mixed-from=1," +
	"engine-gap:correlated-exists-setop=1,engine-gap:derived-table-join-on=1," +
	"engine-gap:dml-returning-result-set=1,engine-gap:error-class=1," +
	"engine-gap:inline-values-table=1,engine-gap:nested-recursive-with=2," +
	"engine-gap:nullable-array-wrapper=1,engine-gap:planner-declines=6,engine-gap:result-metadata=3," +
	"engine-gap:serialization-options=1,fragment=2,no-checks=1,plan-assertion=8," +
	"polarity:fixed-version-meta=9,polarity:negative-execution=24,polarity:negative-parse=25," +
	"unsupported-DDL:function=7,unsupported-DDL:other=10,unsupported-DDL:struct=41," +
	"unsupported:continuation=3,unsupported:multi-cluster=2,unsupported:result-metadata-nested=1," +
	"unsupported:schema-command=8,unsupported:temporary-function=17," +
	"vacuous:all-assertions-skipped=2} inner_skips{conformance:go-accepts-what-java-rejects=3," +
	"engine-gap:catalog-system-tables=2,engine-gap:comma-join-mixed-from=1," +
	"engine-gap:correlated-exists-setop=1,engine-gap:derived-table-join-on=1," +
	"engine-gap:dml-returning-result-set=1,engine-gap:error-class=1," +
	"engine-gap:inline-values-table=1,engine-gap:nested-recursive-with=2," +
	"engine-gap:nullable-array-wrapper=1,engine-gap:planner-declines=6,engine-gap:result-metadata=3," +
	"engine-gap:serialization-options=1,no-checks=8,plan-assertion=595," +
	"polarity:negative-execution=24,unsupported-DDL:function=7,unsupported-DDL:other=10," +
	"unsupported-DDL:struct=41,unsupported:check-cache=119,unsupported:continuation=34," +
	"unsupported:debugger=3,unsupported:multi-cluster=2,unsupported:prepared=173," +
	"unsupported:random-injection=25,unsupported:result-metadata-nested=10," +
	"unsupported:schema-command=16,unsupported:temporary-function=197}"

// pinnedFileTotal closes the ledger: every corpus file lands in exactly one of
// pass / fail / skip. Asserting the sum separately means a file that vanished
// from the run fails with an obvious message instead of a 2,000-column diff.
const pinnedFileTotal = 238

// pinnedAssignmentDigest is sha256 over the sorted `path status class` lines.
//
// It exists because the counts above are blind to a SWAP: two files trading
// classes leaves every total identical, so the census stays green while the
// corpus's meaning changes underneath it. The digest is deliberately opaque —
// on mismatch the test dumps the full assignment, which is the artefact worth
// diffing.
const pinnedAssignmentDigest = "158351c3df6cff22820233cbab07af9713d3a73d21ae24e413604e2480393502"
