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
	{"cast-tests.yamsql", SkipGapErrorClass, `"select [] from test_cast where id = 1": expecting 'XXXXX' error code, got '0AF00'`, "CQ-72"},
	// Array COMPARISON semantics are closed (`[1] = [1]` is TRUE, the
	// NULL/NONE matrix and the 42804 rejections match Java — pinned by
	// TestFDB_ArrayComparison and the live-Java ArrayComparisonJavaProbe).
	// Its NOT NULL column `arr_nn` is closed too: stored flat repeated
	// (Java's layout), empty is absent on the wire, and the row builder now
	// reads a repeated field before any presence test, so absent
	// materializes as [] where the type forbids NULL — Java's
	// MessageHelpers.getFieldOnMessage isRepeated()-first branch. The file
	// passes outright.
	// A JOIN mixed into a comma-separated FROM list.
	{"right-deep-plan-tests.yamsql", SkipGapCommaJoinFrom, "JOIN clauses on comma-separated FROM sources are not supported", "CQ-72"},

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
	// pseudo-field) passes outright, and pseudo-field-clash.yamsql passes
	// since a record constructor's fields take their elements' names.

	// Inline VALUES now parses, plans and executes, including nested authored
	// column definitions and derived-table predicates. The file progresses to
	// its first table-valued function in FROM, which the source parser still
	// rejects explicitly. Pin the statement because the file contains several
	// later range() queries and only this first blocker is measured here.

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

	// joins-documentation-queries.yamsql used to stop at a JOIN-bodied derived
	// table whose ON clause could not be resolved back to its sources. CLOSED:
	// the body's output row is derived from its own legs, the ON resolves, and
	// the file passes outright — its entry is gone and it is counted in `pass`.

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
	// Java cannot use the unnesting index for this ORDER BY (Java issue #3896);
	// Go sorts in memory.
	{"arrays-unnesting.yamsql", SkipConformanceGoAccepts, `line 143: "SELECT SQ.\"item\" FROM \"T1_indexed\" AS \"row\", (SELECT \"item\" FROM \"row\".\"items\" AS \"item\") AS SQ ORDER BY SQ.\"item\"": expecting statement to throw an error 0AF00, however it succeeded`, "Java issue #3896"},
	{"array-agg-tests.yamsql", SkipConformanceGoAccepts, `line 423: "SELECT m.mid, r.rid, (SELECT ARRAY_AGG(a.url) FROM doc_asset a WHERE a.rid = r.rid) AS assets`, "scalar subquery in the SELECT list is a Go grammar extension"},
	{"groupby-tests.yamsql", SkipConformanceGoAccepts, `line 339: "SELECT col1 FROM T1 GROUP BY col1 ORDER BY COUNT((T1.*))": expecting statement to throw an error 0AF00, however it succeeded`, "ORDER BY an aggregate is a Go extension"},
	{"user-defined-macro-function-tests.yamsql", SkipConformanceGoAccepts, `line 329: "select temp_constructor(r.u.w) from nested where id = 1": expecting statement to throw an error 42F18, however it succeeded`, "Java issue #4317"},
	{"maxRows.yamsql", SkipConformanceGoAccepts, `"select p.* FROM ta as p where exists (select * from ta where ta.a = p.a limit 1);": expecting statement to throw an error 0AF00, however it succeeded`, "RFC-128; TestCorpusReadSideExtensions"},

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

	// ---- engine-gap:struct-query (RFC-204 Phase 2 → Phase 3; closed when
	// record fields took their elements' names) ----
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
	// arrays-unnesting-documentation-queries.yamsql PASSES: a lateral
	// subquery reading an outer AT unnest's element plans once the AT
	// Explode names its slots after the AS/AT aliases its references read.
	// inserts-updates-deletes.yamsql PASSES: the record constructor now builds
	// in EXPRESSION position (Java's ExpressionVisitor.visitRecordConstructor
	// → RecordConstructorValue.ofColumns), and its `UPDATE … SET b3 =
	// coalesce(b3, (b1, b2), …)` binds the anonymous literal to the target
	// struct POSITIONALLY, which is what Java's parseRecordFields does with a
	// target type in hand.
	//
	// Java's T3 leg is a full scan of the version index T3_VERSION_WITH_COL1
	// (PREFER_INDEX, col1 rides in its value) and answers in version order; Go
	// prunes the unrestricted index scan and reads T3 in primary-key order.
	{"versions-tests.yamsql", SkipConformanceScanChoiceOrder, `line 575: "select t3.\"__ROW_VERSION\" AS version3, t3.id AS id3, t4.\"__ROW_VERSION\" AS version4, t4.id AS id4, t3.col2, t4.col4 from t3, t4 where t3.col1 = 'b' AND t4.col1 …": cell mismatch at row 5, cell ID3: expected 7 (Integer), got 4 (Long)`, "abstract_data_access_rule.go"},
	// The seeded schedule reaches the EXISTS LIMIT extension first.
	{"orderby.yamsql", SkipConformanceGoAccepts, `"select b from t1 where exists (select * from t1 order by b limit 1)": expecting statement to throw an error 0AF00, however it succeeded`, "RFC-128; TestCorpusReadSideExtensions"},
	// Java cannot satisfy both join-leg orderings from indexes; Go sorts the joined rows.
	{"join-with-order-by-tests.yamsql", SkipConformanceGoAccepts, `"select (t1.*), (t2.*) from t1, t2 where t1.a1 = 1 and t2.b1 = 1 order by t1.a2, t2.b3": expecting statement to throw an error 0AF00, however it succeeded`, "sanctioned in-memory sort; TestCorpusReadSideExtensions"},
	{"in-predicate.yamsql", SkipGapErrorClass, `"select a, e from ta where e in ('foo' , 35 + 4)": expecting '22000' error code, got '42804' instead`, "CQ-72"},
	{"valid-identifiers.yamsql", SkipGapCatalogTables, `"select count(*) from \"TEMPLATES\" where template_name = 'टेम्पलेट'": 0AF00: no schema metadata available`, "CQ-72"},
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
	// exceeding it is 42803.
	//
	// GO NOW EXPANDS ONE OF THE TWO STAR SHAPES, so the old wording here — "Go
	// rejects the shape unconditionally in the classifier, which runs before any
	// schema is available to expand against" — describes a state that no longer
	// exists and would read as a stale entry. A WHOLE-LIST star, `SELECT *` or
	// `SELECT q.*` as the entire select list, is expanded against the semantic
	// scope BEFORE the grouping rules are applied and then validated per
	// expanded column, which is Java's order.
	//
	// WHAT STILL RESTS HERE IS THE MIXED ARM: `SELECT a, q.*` keeps a blanket
	// refusal in the reclassification block, and that — not the whole-list
	// shape — is what this file lands on. The entry is LIVE; the count of 1 is
	// this shape, not a leftover.
	//
	// Booked to that gap at its own exact rejection, so the struct class no
	// longer claims the file and the real blocker is counted under its own name.
	{"select-a-star.yamsql", SkipGapStarGroupBy, "SELECT qualifier.* expands to columns not in GROUP BY", "CQ-72"},

	// NOT struct-related, re-armed by the struct DML landing (these files'
	// later blocks run for the first time):
	// ORDER BY on a UUID column, which Java declines to plan (0AF00) and Go
	// answers through its sanctioned in-memory sort — the same
	// Go-accepts-what-Java-rejects class join-tests-outer.yamsql carries.
	{"uuid-non-prepared.yamsql", SkipConformanceGoAccepts, `"select * from ta where b is not null order by b": expecting statement to throw an error 0AF00, however it succeeded`, "CQ-72"},
	{"uuid-prepared.yamsql", SkipConformanceGoAccepts, `"select * from ta where b is not null order by b": expecting statement to throw an error 0AF00, however it succeeded`, "RFC-257: same sanctioned UUID sort as the simple-statement case"},
	// Every PartiQL AT shape before it executes, the lateral subqueries reading
	// an outer AT unnest's element and ordinal included. The file stops where
	// Java declines ORDER BY over the AT ordinal (0AF00) and Go answers through
	// its sanctioned in-memory sort. Pin the exact statement because this file
	// has thirty PartiQL AT shapes.
	{"array-join-at.yamsql", SkipConformanceGoAccepts, `line 280: "SELECT \"id\", \"val\", \"at\" FROM T1, T1.\"arr1_nn\" AS \"val\" AT \"at\" WHERE T1.\"id\" = 2 ORDER BY \"at\" DESC": expecting statement to throw an error 0AF00, however it succeeded`, "CQ-72"},

	// GO IS CORRECT AND JAVA IS NOT, and the corpus file says so in place:
	// `# TODO Issue #4170: This should return [].` On a NULLABLE indexed
	// column Java maps `«indexed» = NULL` to an IS-NULL index range instead of
	// constant-folding the comparison to UNKNOWN, so it returns the row whose
	// value is NULL.
	//
	// The block has FOUR `= NULL` arms and only this one expects a row. Two are
	// NON-indexed (`tab1`, `tab1_nn`) and expect []; the fourth is
	// `tab1_indexed_nn` — INDEXED but NOT NULL, so it has no null index entry
	// and expects [] for a different reason. That fourth arm is the
	// discriminating variable of the whole finding, which is why it is named
	// here rather than counted as a third "non-indexed twin".
	//
	// Go answers [] for the two that run before this one. It is NOT claimed for
	// the fourth: this arm aborts the block, so the arms after it never execute
	// and there is no output to describe.
	//
	// Go's [] comes from the executor's scan-range binder, which makes an
	// equality against a NULL comparand an EMPTY range — SQL three-valued
	// logic, and the property the null-rejecting ordering proof rests on.
	// Matching Java here would mean breaking 3VL on the index path.
	//
	// This file was invisible until the identifier change (RFC-237) made its
	// nested quoted index DDL — `CARDINALITY("struct"."int_arr")` — build: the
	// whole file was skipped as unsupported-DDL:struct-index with queries=0, so
	// its other 29 queries had never executed either.
	//
	// COST OF BOOKING IT FILE-LEVEL, stated because the runner has no
	// per-query override: the block's last query is the ONLY one exercising
	// `tab2_index`, the nested quoted struct index this change unblocked, and
	// it sits two arms after this one — so it does not run. That shape is
	// covered Go-side instead by `nested_struct_index_never_matches_gap.yaml`,
	// which asserts on the PLAN — and asserts the WRONG one, because that index
	// is built and never matched. Its file name says so.
	{"documentation-queries/array-agg-documentation-queries.yamsql", SkipConformanceScanChoiceOrder, `line 55: "SELECT ARRAY_AGG(amount IGNORE NULLS) AS amounts FROM sales": cell mismatch`, "abstract_data_access_rule.go"},
	{"arrays-cardinality.yamsql", SkipConformanceJavaPlannerBug, `line 187: "SELECT \"id\" FROM \"tab1_indexed\" WHERE CARDINALITY(\"int_arr\") = NULL": result does not contain all expected rows, expected 1 row(s), got 0 row(s)`, "Issue #4170"},

	// NULL into a NOT NULL ARRAY column: Go raises the clean 23502 at plan
	// time (the type-nullability gate, ExpressionVisitor:1067 semantics
	// applied to the literal), where Java lets the NULL reach message
	// coercion and dies with an internal XX000 — the code class differs on
	// a shared-surface statement, so it stays a counted divergence.
	{"arrays.yamsql", SkipGapErrorClass, "expecting 'XX000' error code, got '23502'", "RFC-204 P2"},

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
	//   - join-tests.yamsql reached a comma join whose right side is a table and
	//     whose left is a derived table the predicate references by alias. That
	//     one is CLOSED — a join-bodied derived table's output row is now
	//     derived from its own legs, so the comma join plans — and the file
	//     advanced to a JOIN … USING over a derived table whose body PROJECTS A
	//     COMPUTED EXPRESSION (`select c3 - 2 as c11`). THAT one is closed too
	//     under exact-ordinal resolution; the file's live signature is now the
	//     parenthesised star, booked at its own entry below.
	//
	// Both remaining signatures pin the exact statement, so a DIFFERENT failure
	// in either file stays a hard failure rather than hiding under the entry.
	{"union.yamsql", SkipGapPlannerDeclines, "select id as W, col1 as X, col2 as Y from t1 union all (select * from t1)", "CQ-72"},
	// The file's setup runs under CASE_SENSITIVE_IDENTIFIERS, so Java's DDL
	// stores the schema `test1` as written and the verbatim connect URI
	// (`?schema=test1`) reaches it; Go ignores the option, stores TEST1, and
	// the connect names a schema Go never stored. The runner upper-cased
	// Java's URIs until the fold was confined to the names it generates, which
	// is what hid this gap.
	{"case-sensitivity.yamsql", SkipGapCaseSensitiveIdentifiers, "42F59: table with name 'TABLE1' already exists", "TODO.md, Go ignores CASE_SENSITIVE_IDENTIFIERS"},
	{"keyword-case-insensitivity.yamsql", SkipGapCaseSensitiveIdentifiers, `column names "COLUMN" and "column" collide case-insensitively`, "TODO.md, Go folds quoted identifiers in the positional row layout"},
	{"setup-with-connection-options.yamsql", SkipGapCaseSensitiveIdentifiers, "42F51: Schema </FRL/CASE_SENSITIVE_TEMPLATE/test1> does not exist in the catalog!", "TODO.md, Go ignores CASE_SENSITIVE_IDENTIFIERS"},
	// A correlated EXISTS in the SELECT projection combined with a WHERE
	// EXISTS — Cascades declines the double-EXISTS shape.
	{"exists-in-select.yamsql", SkipGapPlannerDeclines, "Cascades planner could not plan query", "CQ-72"},
	// Go's in-memory sort extension plans this grouped empty-input shape where
	// Java's Cascades planner declines it.
	{"aggregate-empty-table.yamsql", SkipConformanceGoAccepts, "expecting statement to throw an error 0AF00, however it succeeded", "RFC-256"},
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
