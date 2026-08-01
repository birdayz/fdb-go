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

	// The `__ROW_VERSION` pseudo-column is not exposed to name resolution.
	{"join-tests-row-version.yamsql", SkipGapRowVersion, `column "__ROW_VERSION" does not exist`, "CQ-72"},

	// A table-valued function in FROM (`select * from t1_v(10)`).
	{"versions-with-single-type-tests.yamsql", SkipGapTableValuedFunction, "TableValuedFunctionContext", "CQ-72"},

	// `FROM VALUES (42)` — an inline table as a FROM source.
	{"table-functions.yamsql", SkipGapInlineValues, "InlineTableItemContext", "CQ-72"},

	// A correlated EXISTS whose body is a set operation (UNION ALL).
	{"union-empty-tables.yamsql", SkipGapCorrelatedExistsSetOp, "correlated EXISTS: unsupported query body shape", "CQ-72"},

	// A WITH nested inside a recursive CTE's body.
	{"documentation-queries/with-documentation-queries.yamsql", SkipGapNestedRecursiveWith, "nested WITH inside a recursive CTE body", "CQ-72"},

	// A JOIN-bodied derived table whose ON clause cannot be resolved back to
	// its sources. The engine declines rather than silently returning the
	// cross product, which is the right failure mode — but Java plans it.
	{"documentation-queries/joins-documentation-queries.yamsql", SkipGapDerivedJoinOn, "unsupported FROM shape", "CQ-72"},

	// `UPDATE … RETURNING … OPTIONS(DRY RUN)` produces no result set.
	{"update-delete-returning.yamsql", SkipGapReturningDryRun, "actual result set is NULL", "CQ-72"},

	// An EXISTS over a view inside a temporary-table-valued-function block.
	{"alias-tests.yamsql", SkipGapPlannerDeclines, "Cascades planner could not plan query", "CQ-72"},

	// An oversized record surfaces a raw executor error rather than a mapped
	// SQLSTATE, so the corpus's error-class assertion has nothing to compare.
	{"large-record-fails.yamsql", SkipGapErrorClass, "non-SQLSTATE error", "CQ-72"},

	// `select * from ta limit 5` succeeds in Go where Java raises 0AF00. This
	// is the one entry that is NOT a Go deficiency: Go accepts a query Java
	// declines. It is booked all the same, because the conformance principle
	// governs the SHARED surface and an unreviewed widening of it is exactly
	// the silent divergence the cross-engine harness exists to catch.
	{"maxRows.yamsql", SkipConformanceGoAccepts, `"select * from ta limit 5": expecting statement to throw an error 0AF00, however it succeeded`, "CQ-72"},

	// A `create schema template` issued as a SETUP STEP rather than as a
	// schema_template block, declaring struct types. Same Phase-3 gap as the
	// block form, reached by a different route, so it is classed with it.
	{"showcasing-tests.yamsql", SkipDDLStruct, "only primitive column types are supported", "CQ-73"},
	{"create-drop-create-template.yamsql", SkipDDLStruct, "only primitive column types are supported", "CQ-73"},
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
