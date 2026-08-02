package javacorpus

import "strings"

// EngineGap records a corpus file the Go engine cannot run today, together
// with the EXACT rejection it produces.
//
// The signature is what keeps this table from becoming a mute list. A gap
// entry converts a failure into a counted skip only while the engine still
// produces that specific rejection; any other failure at the same path stays a
// hard failure. So the table cannot absorb a NEW bug in an already-known file,
// and when a gap is closed the entry stops matching and the run goes red until
// someone deletes it — the pin fails in the direction of noticing.
//
// Every entry names the TODO booking that closes it. A gap with no booking is
// a gap nobody owns.
type EngineGap struct {
	Path      string
	Class     SkipClass
	Signature string
	Booking   string
}

// engineGaps is the measured Phase-1 ledger of Go-engine divergences the
// vendored corpus surfaces. Each was found by running the file, not predicted.
var engineGaps = []EngineGap{
	// The array-literal INSERT gap (engine-gap:array-literal-values) is
	// CLOSED: ConvertToProtoValue converts a repeated field element-wise and
	// walkArrayConstructor builds Java's LightArrayConstructorValue shape
	// (TestFDB_ArrayLiteralInsertValues pins it). Four of its six files
	// progressed to DISTINCT next gaps, each re-measured below at its exact
	// new rejection; array-column.yamsql passes outright and
	// wrong-array-element-type.yamsql now reaches its resultMetadata
	// assertion, where the CQ-74 metadata truncation declines the comparison
	// (unsupported:result-metadata-nested).
	//
	// cast-tests progresses past its array inserts and dies planning the
	// FIRST test: an array subscript (`arr[1]`) inside an array constructor
	// under CAST … AS STRING ARRAY — Cascades declines with 0AF00.
	{"cast-tests.yamsql", SkipGapPlannerDeclines, "select cast([ arr[1] + arr[2], arr[2] + arr[3] ] as string array)", "CQ-72"},
	// Array COMPARISON semantics are closed (`[1] = [1]` is TRUE, the
	// NULL/NONE matrix and the 42804 rejections match Java — pinned by
	// TestFDB_ArrayComparison and the live-Java ArrayComparisonJavaProbe).
	// The file progressed 36 queries to its stored-row block, where the
	// RFC-143 §3a wire divergence surfaces: Go writes a nullable array as
	// a PLAIN repeated field (no `values` wrapper message), so the stored
	// `[]` at pk 0 is byte-identical to NULL and reads back NULL — the
	// row's IS_NULL/IS_EMPTY answers invert. Closes with the §3a
	// nullable-array-wrapper WRITE follow-up (TODO R6).
	{
		"arrays-operators.yamsql", SkipGapNullableArrayWrapper,
		"actual: {ARR: <NULL>, IS_NULL: true, IS_NOT_NULL: false, IS_EMPTY: <NULL>}", "RFC-143 §3a",
	},
	// A JOIN mixed into a comma-separated FROM list.
	{"right-deep-plan-tests.yamsql", SkipGapCommaJoinFrom, "JOIN clauses on comma-separated FROM sources are not supported", "CQ-72"},
	// UPDATE … RETURNING executes the mutation (the array SET now converts)
	// but yields no result set — the driver's DML surface returns a row
	// count only (dml_returning_probes.yaml pins the driver behaviour).
	{"prepared.yamsql", SkipGapDMLReturning, `update ta set e = [10, 100, 1000] where a = 1 returning`, "CQ-72"},

	// Querying the catalog's own tables (TEMPLATES, SCHEMAS) from a user
	// connection finds no schema metadata to plan against.
	{"create-drop.yamsql", SkipGapCatalogTables, "no schema metadata available", "CQ-72"},
	{"catalog.yamsql", SkipGapCatalogTables, "no schema metadata available", "CQ-72"},

	// The width-suffixed numeric literals (`1I`/`2L`/`1.0f`) are CLOSED:
	// resolveDecimalText ports ParseHelpers.parseDecimal, and
	// literal-tests.yamsql passes outright (TestFDB_TypedNumericLiterals +
	// the walker suffix pins). union.yamsql's float-suffix sibling is
	// still DDL-blocked here (AS-SELECT value indexes) and re-arms on PR
	// #577's branch.

	// The `__ROW_VERSION` pseudo-column gap (engine-gap:row-version-
	// pseudocolumn) is CLOSED by RFC-202 S4: the version-storing catalog
	// exposes the ephemeral pseudo-column, VERSION indexes generate/plan/
	// execute, and join-tests-row-version.yamsql (JOIN USING on the
	// pseudo-field) passes outright. pseudo-field-clash.yamsql progressed to
	// a DIFFERENT, pre-existing decline, re-measured below at its exact new
	// rejection.
	//
	// pseudo-field-clash's last query projects EXPLICIT-COLUMN struct
	// constructors — `SELECT (t1.id, t1.col1, t1."__ROW_VERSION"), … FROM
	// t1, t2, t3` — and the expression walker supports only the
	// single-element unwrap (walkRecordConstructor); a multi-element record
	// constructor in a projection has no Value lowering. NOT a row-version
	// gap: `SELECT (t3.id, t3.col1) FROM t1, t3 WHERE t3.id = t1.id` fails
	// with the same 0AF00 on a template with no store_row_versions option at
	// all. Every other query in the file (real-column-wins, version ISCAN /
	// COVERING reads, qualified stars over the 3-way join) passes.
	// (The signature stops before the quoted pseudo-field name: the runner's
	// failure text carries the query %q-quoted, so inner quotes appear
	// escaped.)
	{"pseudo-field-clash.yamsql", SkipGapPlannerDeclines, `SELECT (t1.id, t1.col1, t1.`, "CQ-72"},

	// `FROM VALUES (42)` — an inline table as a FROM source.
	{"table-functions.yamsql", SkipGapInlineValues, "InlineTableItemContext", "CQ-72"},

	// A correlated EXISTS whose body is a set operation (UNION ALL).
	{"union-empty-tables.yamsql", SkipGapCorrelatedExistsSetOp, "correlated EXISTS: unsupported query body shape", "CQ-72"},

	// A WITH nested inside a recursive CTE's body.
	{"documentation-queries/with-documentation-queries.yamsql", SkipGapNestedRecursiveWith, "nested WITH inside a recursive CTE body", "CQ-72"},
	// recursive-cte.yamsql ran its schema DDL for the first time when the
	// sparse-index predicate arm landed (RFC-202 S5 — its CHILDIDXNONULLS
	// declares `where parent is not null`); the file then progresses to its
	// line-185 statement, a WITH nested inside a recursive CTE body — the
	// same gap as the entry above, reached from a second carrier.
	{"recursive-cte.yamsql", SkipGapNestedRecursiveWith, "nested WITH inside a recursive CTE body", "CQ-72"},

	// A JOIN-bodied derived table whose ON clause cannot be resolved back to
	// its sources. The engine declines rather than silently returning the
	// cross product, which is the right failure mode — but Java plans it.
	{"documentation-queries/joins-documentation-queries.yamsql", SkipGapDerivedJoinOn, "unsupported FROM shape", "CQ-72"},

	// alias-tests.yamsql's EXISTS-over-a-view decline is masked now: the
	// template's CREATE VIEW fails closed (unsupported-DDL:other) instead of
	// being silently dropped, so the file never reaches the planner. The
	// planner-declines class stays witnessed by exists-in-select.yamsql.

	// An oversized record surfaces a raw executor error rather than a mapped
	// SQLSTATE, so the corpus's error-class assertion has nothing to compare.
	{"large-record-fails.yamsql", SkipGapErrorClass, "non-SQLSTATE error", "CQ-72"},

	// `select * from ta limit 5` succeeds in Go where Java raises 0AF00. This
	// is the one entry that is NOT a Go deficiency: Go accepts a query Java
	// declines. It is booked all the same, because the conformance principle
	// governs the SHARED surface and an unreviewed widening of it is exactly
	// the silent divergence the cross-engine harness exists to catch.
	{"maxRows.yamsql", SkipConformanceGoAccepts, `"select * from ta limit 5": expecting statement to throw an error 0AF00, however it succeeded`, "CQ-72"},

	// `USE INDEX (i1)` where i1 is SPARSE: Java threads the hint as
	// AccessHints on the scan (QueryVisitor.visitAtomTableItem:679-681 →
	// LogicalOperator.generateTableAccess), the hint excludes every
	// non-hinted access path including the full scan, the sparse index
	// cannot serve the unfiltered query, and planning fails 0AF00. Go
	// PARSES the hint and silently ignores it — a pre-existing
	// accept-and-ignore divergence with 13 corpus carriers, exposed here for
	// the first time because the sparse-index predicate arm (RFC-202 S5) let
	// this file's DDL through. The other 11 carriers pass because their
	// hinted plans return the same rows either way; only this file asserts
	// the hint's REJECTION semantics. Closing it = porting AccessHints into
	// the Go candidate matcher.
	{"sparse-index-tests.yamsql", SkipConformanceGoAccepts, `"select id from t1 use index (i1)": expecting statement to throw an error 0AF00, however it succeeded`, "CQ-72"},

	// ORDER BY on the null-extended (right) side of a LEFT JOIN: Java's
	// Cascades cannot satisfy the ordering from any access path and, having
	// no physical sort, fails to plan (0AF00 — RemoveSortRule must eliminate
	// the sort or planning fails). Go answers the same query through its
	// SANCTIONED in-memory sort fallback (RecordQueryInMemorySortPlan) — a
	// deliberate read-side capability beyond Java, wire-neutral. Reached for
	// the first time when the quoted-alias USING fix let this file's later
	// blocks run.
	// The %q-formatted statement text escapes the embedded quotes, so the
	// signature matches the escaped form.
	{"join-tests-outer.yamsql", SkipConformanceGoAccepts, `ORDER BY \"d\".\"name\";": expecting statement to throw an error 0AF00, however it succeeded`, "CQ-72"},

	// ---- engine-gap:struct-query (RFC-204 Phase 2 → Phase 3) ----
	//
	// PHASE 2 CLOSED engine-gap:struct-dml (23 files): struct and
	// array-of-struct literals write through the typed row-constructor
	// push-down, UPDATE SET assigns a whole struct, INSERT … SELECT carries
	// one, and a struct column reads back as an api.Struct the corpus
	// matcher compares against a nested YAML map. Fifteen of the carriers
	// pass outright; the eight below advanced to their NEXT rejection, and
	// each is pinned at that exact statement so an unrelated new failure in
	// the same file stays a hard failure.

	// PHASE 3 (resolver) closed nested field access against a TABLE source:
	// struct-type-nullability-variants.yamsql passes, because the semantic
	// scope now applies Java's fifth matching rule — lookupNestedField
	// (SemanticAnalyzer.java:481-488, :548-602) — so `home_address.city`
	// descends into a struct COLUMN instead of demanding a FROM source named
	// HOME_ADDRESS. The remaining five are the shapes that rule does not
	// reach.
	//
	// RE-BOOKED, not closed-by-relabel: `item.sku` (item = a lateral unnest
	// of a struct array) now RESOLVES — the unnest binding's virtual column
	// carries the array ELEMENT's field list, so the same lookupNestedField
	// descent that serves a struct column reaches it. The file then hits a
	// gap that has nothing to do with structs: its FROM clause unnests TWO
	// arrays, and the translator lowers only one. Booked to that gap, at its
	// own exact rejection, so the struct class no longer claims it and the
	// real blocker is counted under its own name.
	{"arrays-unnesting-documentation-queries.yamsql", SkipGapMultipleLateralUnnests, "multiple lateral array unnests in one FROM clause are not yet supported", "RFC-142"},
	// inserts-updates-deletes.yamsql PASSES: the record constructor now builds
	// in EXPRESSION position (Java's ExpressionVisitor.visitRecordConstructor
	// → RecordConstructorValue.ofColumns), and its `UPDATE … SET b3 =
	// coalesce(b3, (b1, b2), …)` binds the anonymous literal to the target
	// struct POSITIONALLY, which is what Java's parseRecordFields does with a
	// target type in hand.
	//
	// functions.yamsql advanced but did not close, and its rejection changed
	// KIND: it is no longer a planning decline, it is a RESULT-SURFACING gap.
	// The query plans and evaluates CORRECTLY — the row carries
	// {_0: int64(1), _1: float64(1.0), _2: "a", _3: true}, pinned in
	// pkg/relational/sqldriver/record_constructor_expression_fdb_test.go — but
	// a COMPUTED record reaches the driver as a bare map[string]any, so it is
	// not an api.Struct and the matcher's nested comparison declines it. Java
	// does not have this shape at all: RecordConstructorValue.eval builds a
	// dynamic proto Message (RecordConstructorValue.java:113-139, the same
	// structural walk Go's values.BuildStructMessage performs on the DML side),
	// so the driver's ProtoMessage → RelationalStruct conversion
	// (RowStruct.java:184-197) fires for a computed record exactly as it does
	// for a stored one. Go's RecordConstructorValue.Evaluate returns a map, so
	// materializeDriverValue's ProtoMessage arm never sees it.
	{"functions.yamsql", SkipGapStructQuery, "actual row at 1 does not match any expected records", "RFC-204 P3"},
	// `SELECT (*)` — the parenthesised star, a record constructor over the
	// expanded row (ExpressionVisitor.visitRecordConstructor's STAR arm,
	// :902-916).
	{"star-expression-metadata.yamsql", SkipGapStructQuery, `"SELECT (*) FROM foo"`, "RFC-204 P3"},
	// RE-BOOKED, not closed-by-relabel: the duplicate qualified star this file
	// was booked for is FIXED. Java's expandStar has no uniqueness rule, so
	// `SELECT A.*, A.* FROM A` is legal and the 42702 comes from the OUTER
	// reference finding two matching attributes (SemanticAnalyzer.java:417,
	// :422); Go raised its own 22023 at the producer instead. Removing that
	// rejection, deriving the derived table's schema THROUGH the star, and
	// counting matches per-attribute makes Go answer the inner query with rows
	// and the outer reference with 42702 and Java's exact message text — both
	// halves live-JVM verified (conformance/duplicate_star_java_probe_test.go).
	//
	// The file then reaches a gap that has nothing to do with structs, and one
	// this run measured rather than inferred: a qualified star in a SELECT list
	// that ALSO carries GROUP BY. Java expands the star first and requires each
	// expanded output to be composable from the grouping expressions plus the
	// aggregates plus the outer correlations (LogicalOperator.java:435-441), so
	// a star covering exactly the grouping list is legal and only a star
	// exceeding it is 42803. Go rejects the shape unconditionally in the
	// classifier, which runs before any schema is available to expand against.
	// Booked to that gap at its own exact rejection, so the struct class no
	// longer claims the file and the real blocker is counted under its own name.
	{"select-a-star.yamsql", SkipGapStarGroupBy, "SELECT qualifier.* expands to columns not in GROUP BY", "CQ-72"},

	// NOT struct-related, re-armed by the struct DML landing (these files'
	// later blocks run for the first time):
	// ORDER BY on a UUID column, which Java declines to plan (0AF00) and Go
	// answers through its sanctioned in-memory sort — the same
	// Go-accepts-what-Java-rejects class join-tests-outer.yamsql carries.
	{"uuid-non-prepared.yamsql", SkipConformanceGoAccepts, `"select * from ta where b is not null order by b": expecting statement to throw an error 0AF00, however it succeeded`, "CQ-72"},
	// A lateral unnest of a DERIVED TABLE's array column with AT — the
	// correlated-unnest-over-subquery shape Cascades declines.
	{"array-join-at.yamsql", SkipGapPlannerDeclines, "Cascades planner could not plan query", "CQ-72"},

	// NULL into a NOT NULL ARRAY column: Go raises the clean 23502 at plan
	// time (the type-nullability gate, ExpressionVisitor:1067 semantics
	// applied to the literal), where Java lets the NULL reach message
	// coercion and dies with an internal XX000 — the code class differs on
	// a shared-surface statement, so it stays a counted divergence.
	{"arrays.yamsql", SkipGapErrorClass, "expecting 'XX000' error code, got '23502'", "RFC-204 P2"},

	// RE-ARMED by struct DDL landing (this class was masked while the file's
	// template failed at its struct declarations): UPDATE … RETURNING with
	// OPTIONS(DRY RUN) yields no result set through the driver.
	{"update-delete-returning.yamsql", SkipGapReturningDryRun, "actual result set is NULL, expecting non-NULL result set", "RFC-201 Phase 5"},

	// ---- Gaps armed by RFC-202 S2: these files' index DDL now succeeds, so
	// their queries run for the first time and each reaches its own
	// pre-existing engine gap. Every entry pins the exact statement so an
	// unrelated new failure in the same file stays a hard failure.

	// The width-suffixed literal gaps and the JOIN … USING star gap that used
	// to pin union.yamsql, null-extraction-tests.yamsql and join-tests.yamsql
	// are CLOSED on master (the error-class batch). Each file therefore runs
	// further than it used to and stops at its own next pre-existing decline,
	// re-measured here:
	//
	//   - null-extraction-tests.yamsql now passes OUTRIGHT — its entry is gone
	//     and it is counted in `pass`.
	//   - union.yamsql reaches a bare `select * from t1` as a UNION ALL branch.
	//   - join-tests.yamsql reaches a comma join whose right side is a table
	//     and whose left is a derived table the predicate references by alias.
	//
	// Both remaining signatures pin the exact statement, so a DIFFERENT failure
	// in either file stays a hard failure rather than hiding under the entry.
	{"union.yamsql", SkipGapPlannerDeclines, "select id as W, col1 as X, col2 as Y from t1 union all (select * from t1)", "CQ-72"},
	{"join-tests.yamsql", SkipGapPlannerDeclines, "select sq.name, project.name from (select dept.name, emp.id from emp, dept where emp.dept_id = dept.id) as sq, project", "CQ-72"},
	// Schema-template serialization options (encryption): Go's store layer
	// does not implement encrypted serialization, so a read that Java fails
	// with XXF01 (missing/wrong key) succeeds.
	{"serialization-options.yamsql", SkipGapSerializationOptions, "expecting statement to throw an error XXF01, however it succeeded", "CQ-72"},
	// A correlated EXISTS in the SELECT projection combined with a WHERE
	// EXISTS — Cascades declines the double-EXISTS shape.
	{"exists-in-select.yamsql", SkipGapPlannerDeclines, "Cascades planner could not plan query", "CQ-72"},
	// The result-set metadata pipeline truncates column types to one flat
	// string (CQ-74), so `resultMetadata:` assertions on aggregate outputs
	// mismatch.
	{"aggregate-empty-table.yamsql", SkipGapResultMetadata, "result metadata mismatch", "CQ-74"},
	{"aggregate-index-tests-count.yamsql", SkipGapResultMetadata, "result metadata mismatch", "CQ-74"},
	{"aggregate-index-tests-count-empty.yamsql", SkipGapResultMetadata, "result metadata mismatch", "CQ-74"},

	// The bitmap aggregate QUERY surface: RFC-202 S3 builds the BITMAP_VALUE
	// index metadata (the file's DDL now succeeds and the key expression is
	// pinned byte-exact), but the translator has no bitmap_construct_agg
	// aggregate — every query over it declines with 0AF00.
	//
	// The pin used to sit on a duplicate-grouping NEGATIVE, where the decline
	// surfaced as "expecting '42702', got '0AF00'". Master's error-class batch
	// now raises that 42702 correctly BEFORE planning (Expressions.pullUp,
	// Expressions.java:112), so those negatives pass and the file advances to
	// its first POSITIVE query — which is where the missing aggregate actually
	// bites. Re-pinned there. Measured to be independent of the bitmap arity
	// check landed alongside it: reverting that check reproduces this exact
	// statement and error.
	{"bitmap-aggregate-index.yamsql", SkipGapPlannerDeclines, "SELECT bitmap_construct_agg(bitmap_bit_position(id)) as bitmap, bitmap_bucket_offset(id) as offset FROM T2 GROUP BY bitmap_bucket_offset(id)", "CQ-72"},
}

// SetupNegatives are the execution-level negatives whose upstream-asserted
// failure happens in a `setup:` block rather than in a test block.
//
// The polarity accounting otherwise treats a setup death as proof the file
// never reached its assertion — which is right for every other negative and
// caught a real mis-credit — but it is wrong here, because for these files the
// setup step IS the assertion. That cannot be derived: only the manifest knows
// where upstream expects the failure, and its reason is prose. So it is
// declared, one line per file, and asserted reachable: an entry whose file
// stops failing in setup fails the run rather than sitting here unread.
var SetupNegatives = map[string]string{
	"include-block/shouldFail/verify-all-includes-execute.yamsql": "the file exists to prove every " +
		"include EXECUTES, and it proves it by including a fragment twice — the second pass " +
		"re-inserts an existing primary key from the fragment's own setup block, so the 23505 " +
		"raised there is the assertion, not an accident on the way to one",
}

// gapFor returns the gap entry covering a failure, if the failure is the one
// the entry records.
func gapFor(path string, err error) (EngineGap, bool) {
	if err == nil {
		return EngineGap{}, false
	}
	msg := err.Error()
	for _, g := range engineGaps {
		if g.Path == path && strings.Contains(msg, g.Signature) {
			return g, true
		}
	}
	return EngineGap{}, false
}

// EngineGaps exposes the table so a test can assert every entry is still
// reachable — an entry whose file stopped failing is a closed gap that nobody
// deleted, and it would otherwise keep a working file counted as broken.
func EngineGaps() []EngineGap { return engineGaps }
