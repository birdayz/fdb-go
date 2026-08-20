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
// which is the older gap the file was never reaching. (SUPERSEDED as of the
// exact-ordinal revision noted further down: that computed body now derives, the
// class is retired and its constant deleted.)
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
// groupby-tests.yamsql then rested on `engine-gap:nested-path-group-key` for
// one revision, and RFC-230 RETIRED that class entirely: a grouping key that
// descends into a struct column now answers, the file passes outright, and the
// class is deleted rather than emptied — a label nothing emits reads as a
// covered case.
//
// THAT RETIREMENT IS WHERE THIS REVISION'S `queries` JUMP COMES FROM
// (1775 → 1952), and the mechanism is worth stating because the previous
// revision's note says the opposite about a booking. A gap ENTRY cannot move
// `queries` — Census.accumulate adds f.QueriesRun outside the status switch, so
// booked and unbooked measured the same 1775. What moved coverage is that the
// file no longer ABORTS: its block used to run 33 of 44 queries and stop at the
// refusal on line 61, taking every later block with it. All of it runs now, and
// the inner_skips that grew (plan-assertion 787 → 828, prepared 218 → 219,
// continuation 34 → 36) are the per-query classes of the part that had never
// executed.
//
// `pass` 69 → 70 is groupby-tests.yamsql itself.
//
// RFC-232 closes the inline-VALUES table-source gap. array-join-at and
// table-functions each execute nine additional result-consuming configs before
// reaching their next explicit boundary, so `queries` grows 1952 → 1970 and
// `plan-assertion` grows 828 → 839. Neither file passes yet: the former is now
// booked to multiple lateral unnests and the latter to a table-valued function
// in FROM. The removed inline-values class therefore has no zero-count residue.
//
// EXACT-ORDINAL RESOLUTION THEN CLOSES `engine-gap:derived-table-join-on`
// ENTIRELY (1970 → 1976, plan-assertion 839 → 843) and the class is DELETED
// rather than left at zero. join-tests.yamsql is the sole file that moves — a
// per-file diff of all 168 skip lines and of every file's skip-class histogram
// is otherwise byte-identical — and it moves forward: the JOIN … USING over a
// computed-body derived table now asserts, taking the file from 17 asserted
// queries to 23 before it stops at the parenthesised star it re-books to.
//
// Do NOT reason about that from the two statements' line numbers. The block
// does not execute in file order, so the file's stopping point can move
// EARLIER in the text while the run gets FURTHER; `queries` is the only one of
// the two that measures progress. `struct-query` 1 → 2 is join-tests joining
// star-expression-metadata.yamsql under the identical `0AF00: projection slot
// 0 has no resolved Value`, which that file already produced at the previous
// revision — a pre-existing gap gaining a second carrier, not a new one.
//
// RFC-236 THEN UNBLOCKS A FILE THAT HAD NEVER EXECUTED A SINGLE QUERY.
// `arrays-cardinality.yamsql` declares `CREATE INDEX … AS SELECT
// CARDINALITY("struct"."int_arr")` — a NESTED QUOTED path — which the index
// generator rejected, so `unsupported-DDL:struct-index` claimed the whole file
// at queries=0. With identifiers no longer folded that DDL builds, the file
// runs, and its other 29 queries assert for the first time: `queries` grows
// 1976 → 2005, inner `plan-assertion` 843 → 867 and `unsupported:prepared`
// 219 → 221 as the newly-reached configs book themselves, and
// `unsupported-DDL:struct-index` drops 4 → 3.
//
// It lands on `conformance:java-planner-bug`, a NEW class, and the class is
// new because neither existing one is true: Java does not reject this query
// (so it is not go-accepts-what-java-rejects) and Go is not missing anything
// (so it is not an engine-gap). Java answers `CARDINALITY(«nullable indexed»)
// = NULL` with the NULL row, which the corpus file itself flags
// `# TODO Issue #4170: This should return [].`; Go returns `[]`, as do both
// engines for the three NON-indexed twins in the same block.
//
// The file therefore counts as a skip rather than a pass, and `pass` does NOT
// move: one query in it disagrees with the corpus on purpose. Reading this as
// "a file regressed" is the wrong reading — the run got 29 queries FURTHER
// than it had ever been.
const pinnedLedger = "pass=70 fail=0 skip=168 queries=2005 file_skips{conformance:go-accepts-what-java-rejects=4," +
	"conformance:java-planner-bug=1," +
	"engine-gap:catalog-system-tables=2,engine-gap:comma-join-mixed-from=1," +
	"engine-gap:correlated-exists-setop=1," +
	"engine-gap:dml-returning-result-set=2,engine-gap:error-class=2," +
	"engine-gap:multiple-lateral-unnests=2," +
	"engine-gap:nested-recursive-with=2," +
	"engine-gap:planner-declines=5,engine-gap:result-metadata=3," +
	"engine-gap:returning-dry-run=1,engine-gap:serialization-options=1," +
	"engine-gap:star-group-by-expansion=1,engine-gap:struct-query=2,engine-gap:table-valued-function=1,fragment=2," +
	"no-checks=1,plan-assertion=8,polarity:fixed-version-meta=9," +
	"polarity:negative-execution=26,polarity:negative-parse=25," +
	"unsupported-DDL:function=11,unsupported-DDL:other=11," +
	"unsupported-DDL:struct-index=3,unsupported:continuation=3," +
	"unsupported:multi-cluster=2,unsupported:result-metadata-nested=6," +
	"unsupported:schema-command=8,unsupported:temporary-function=17," +
	"vacuous:all-assertions-skipped=5} inner_skips{conformance:go-accepts-what-java-rejects=4," +
	"conformance:java-planner-bug=1," +
	"engine-gap:catalog-system-tables=2,engine-gap:comma-join-mixed-from=1," +
	"engine-gap:correlated-exists-setop=1," +
	"engine-gap:dml-returning-result-set=2,engine-gap:error-class=2," +
	"engine-gap:multiple-lateral-unnests=2," +
	"engine-gap:nested-recursive-with=2," +
	"engine-gap:planner-declines=5,engine-gap:result-metadata=3," +
	"engine-gap:returning-dry-run=1,engine-gap:serialization-options=1," +
	"engine-gap:star-group-by-expansion=1,engine-gap:struct-query=2,engine-gap:table-valued-function=1," +
	"no-checks=8,plan-assertion=867,polarity:negative-execution=26," +
	"unsupported-DDL:function=11,unsupported-DDL:other=11," +
	"unsupported-DDL:struct-index=3,unsupported:check-cache=145," +
	"unsupported:continuation=36,unsupported:debugger=3," +
	"unsupported:multi-cluster=2,unsupported:prepared=221," +
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
// THE EXACT-ORDINAL REVISION MOVED EXACTLY ONE LINE, and it was diffed rather
// than re-blessed on the hash:
//
//	-join-tests.yamsql  skip engine-gap:derived-table-join-on
//	+join-tests.yamsql  skip engine-gap:struct-query
//
// Nothing else moved or swapped, and that is measured rather than assumed. Two
// independent per-file diffs against the previous revision were taken over the
// SAME 168-file skip set — the `SKIP <path> <class> queries=N` lines, and each
// file's own skip-class histogram — and both are byte-identical apart from the
// join-tests line. The skip set has the same membership on both sides (no file
// added or removed), and `pass` is unchanged at 70, so the pass rows of the
// assignment are identical too.
//
// The previous revision's two lines are still described below it in this file's
// history; they were the inline-VALUES pair.
//
// RFC-236 ALSO MOVED EXACTLY ONE LINE, and it was diffed against the dumped
// assignment rather than re-blessed on the hash:
//
//	-arrays-cardinality.yamsql  skip unsupported-DDL:struct-index
//	+arrays-cardinality.yamsql  skip conformance:java-planner-bug
//
// The file stays a SKIP and the 168-file skip set keeps its membership, so
// `pass` does not move — but its `queries` goes 0 → 29, which is the whole
// event and is invisible in this digest by construction. Read it beside the
// ledger line's 1976 → 2005.
const pinnedAssignmentDigest = "d7fbfa446c3ac3065f5b5aa29869cc64ac5e4d444546ce27f820dfa9d5d3d8ee"
