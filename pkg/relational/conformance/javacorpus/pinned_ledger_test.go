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
// Deriving a JOIN-BODIED derived table's output row from its own legs (a
// derived table over a join used to have no enumerable schema, which dropped
// the whole outer resolver) moved two files:
// documentation-queries/joins-documentation-queries PASSES — its ON over a
// join-bodied derived table resolves, so `engine-gap:derived-table-join-on`
// changed carrier rather than size; and join-tests runs 11 more queries before
// resting on that same class, at a JOIN … USING over a derived table whose body
// projects a COMPUTED expression. That body still has no derivable output type,
// which is the older gap the file was never reaching.
//
// `polarity:negative-execution` appears in BOTH groups on purpose: the file
// entry says the negative failed, and the inner entry carries the failure text
// so the log shows WHY. A negative credited for failing the wrong way — dying
// in setup before the assertion upstream is testing — is otherwise
// indistinguishable from one that failed as designed, and two were.
//
// RFC-204 Phase 3 (duplicate star) moved one file OUT of the struct class
// without closing it: select-a-star.yamsql's duplicate qualified star is
// fixed — the producer is legal and the outer reference is 42702 with Java's
// exact text — and the file now rests on `engine-gap:star-group-by-expansion`,
// a grouping-validation gap this run MEASURED against the live JVM and which
// has nothing to do with structs. Java expands a star before applying the
// grouping rule, so a star covering exactly the GROUP BY list is legal; Go
// rejects every star-with-GROUP-BY in the classifier, which has no schema to
// expand against.
//
// RFC-204 Phase 3 (record constructor in expression position) CLOSED
// inserts-updates-deletes.yamsql — the struct literal builds as a value, not
// only as a DML target, and an anonymous one binds positionally to its target
// struct.
//
// RFC-204 §4.5.1 (the plan-time descriptor bake) then closed the struct-query
// half for functions.yamsql: a COMPUTED record reaches the driver as an
// api.Struct, so `engine-gap:struct-query` drops 2 → 1 (the survivor is
// `SELECT (*)`). The file RE-BOOKS rather than passing —
// `engine-gap:dml-returning-result-set` 1 → 2 — because clearing the struct
// blocker exposed an unrelated one: DML RETURNING produces no result set.
//
// The `queries` jump (1408 → 1516) is the honest signal of that movement: 108
// more result-consuming configs are now asserted against the engine, nearly
// all of them functions.yamsql statements that previously died at its first
// struct query and never ran.
//
// `queries` counts result-consuming configs actually asserted against the
// engine. It is the honest denominator behind `pass`, and it deliberately
// EXCLUDES `noChecks` queries: those execute but assert nothing, so counting
// them would let a file whose only query is config-less report a pass.
//
// THE THREE-SEGMENT RESOLVER UNBLOCKED TWO FILES AT THE DDL, which is where
// this run's `queries` jump comes from (1597 → 1775).
// `unsupported-DDL:struct-index` drops 6 → 4: groupby-tests.yamsql and
// nested-with-nulls.yamsql both declare an index over a three-segment nested
// path, and their CREATE SCHEMA TEMPLATE used to die with `42703: column
// reference with qualifier "R.V" cannot be resolved` — queries=0, nothing ran.
// THE ALARM ON THAT ENTRY IS NOW INVERTED: 4 is the steady state and 6 coming
// back means the resolver regressed at the DDL, not that a file was added.
//
// groupby-tests.yamsql then rests on `engine-gap:nested-path-group-key`, a NEW
// class for a grouping key that descends into a struct column. It is NOT a
// re-skip of the file this commit just unblocked: the file executes 33 of its
// 44 queries before the block's shuffle reaches the refusal and aborts, and
// those 33 are in the `queries` total above — booked and unbooked both measure
// queries=1775, so the entry moves the file's STATUS and not its coverage. That
// is the whole difference from the queries=0 DDL state it came from. RFC-230's
// upgradeAggregateOperands arm retires it; see the entry in gaps.go for what
// goes red when it lands.
//
// `pass` 68 → 69 is nested-with-nulls.yamsql, which now runs and passes
// outright.
const pinnedLedger = "pass=69 fail=0 skip=169 queries=1775 file_skips{conformance:go-accepts-what-java-rejects=4," +
	"engine-gap:catalog-system-tables=2,engine-gap:comma-join-mixed-from=1," +
	"engine-gap:correlated-exists-setop=1,engine-gap:derived-table-join-on=1," +
	"engine-gap:dml-returning-result-set=2,engine-gap:error-class=2," +
	"engine-gap:inline-values-table=1,engine-gap:multiple-lateral-unnests=1," +
	"engine-gap:nested-path-group-key=1,engine-gap:nested-recursive-with=2," +
	"engine-gap:planner-declines=6,engine-gap:result-metadata=3," +
	"engine-gap:returning-dry-run=1,engine-gap:serialization-options=1," +
	"engine-gap:star-group-by-expansion=1,engine-gap:struct-query=1,fragment=2," +
	"no-checks=1,plan-assertion=8,polarity:fixed-version-meta=9," +
	"polarity:negative-execution=26,polarity:negative-parse=25," +
	"unsupported-DDL:function=11,unsupported-DDL:other=11," +
	"unsupported-DDL:struct-index=4,unsupported:continuation=3," +
	"unsupported:multi-cluster=2,unsupported:result-metadata-nested=6," +
	"unsupported:schema-command=8,unsupported:temporary-function=17," +
	"vacuous:all-assertions-skipped=5} inner_skips{conformance:go-accepts-what-java-rejects=4," +
	"engine-gap:catalog-system-tables=2,engine-gap:comma-join-mixed-from=1," +
	"engine-gap:correlated-exists-setop=1,engine-gap:derived-table-join-on=1," +
	"engine-gap:dml-returning-result-set=2,engine-gap:error-class=2," +
	"engine-gap:inline-values-table=1,engine-gap:multiple-lateral-unnests=1," +
	"engine-gap:nested-path-group-key=1,engine-gap:nested-recursive-with=2," +
	"engine-gap:planner-declines=6,engine-gap:result-metadata=3," +
	"engine-gap:returning-dry-run=1,engine-gap:serialization-options=1," +
	"engine-gap:star-group-by-expansion=1,engine-gap:struct-query=1," +
	"no-checks=8,plan-assertion=787,polarity:negative-execution=26," +
	"unsupported-DDL:function=11,unsupported-DDL:other=11," +
	"unsupported-DDL:struct-index=4,unsupported:check-cache=145," +
	"unsupported:continuation=34,unsupported:debugger=3," +
	"unsupported:multi-cluster=2,unsupported:prepared=218," +
	"unsupported:random-injection=25,unsupported:result-metadata-nested=85," +
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
//
// THIS REVISION MOVED EXACTLY TWO LINES, and they were diffed rather than
// re-blessed on the hash — the assignment was captured at the parent commit and
// at this one and compared line by line:
//
//	-groupby-tests.yamsql      skip unsupported-DDL:struct-index
//	+groupby-tests.yamsql      skip engine-gap:nested-path-group-key
//	-nested-with-nulls.yamsql  skip unsupported-DDL:struct-index
//	+nested-with-nulls.yamsql  pass -
//
// Both files were blocked at CREATE SCHEMA TEMPLATE by a three-segment
// nested-path index; both now run. Nothing else moved and nothing swapped.
const pinnedAssignmentDigest = "b7f9223cb11d368caa3e5ee0589092c1bf135498f437fa7e67d8d7e500d5e8d6"
