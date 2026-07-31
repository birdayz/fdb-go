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
	// An array literal in INSERT … VALUES is handed to the scalar
	// ConvertToProtoValue, whose switch is over the column's proto Kind and has
	// no repeated-field arm, so a slice falls through to the verbatim 22000.
	// Minimal reproducer: check-result-metadata/shouldPass/array-column.yamsql,
	// eight lines — `create table t1(pk integer, x integer array, …)` then
	// `INSERT INTO t1 VALUES (1, [10, 20, 30])`.
	{"check-result-metadata/shouldPass/array-column.yamsql", SkipGapArrayValues, "cannot be assigned to a variable", "CQ-72"},
	{"cast-tests.yamsql", SkipGapArrayValues, "cannot be assigned to a variable", "CQ-72"},
	{"arrays-operators.yamsql", SkipGapArrayValues, "cannot be assigned to a variable", "CQ-72"},
	{"right-deep-plan-tests.yamsql", SkipGapArrayValues, "cannot be assigned to a variable", "CQ-72"},
	{"prepared.yamsql", SkipGapArrayValues, "cannot be assigned to a variable", "CQ-72"},

	// Querying the catalog's own tables (TEMPLATES, SCHEMAS) from a user
	// connection finds no schema metadata to plan against.
	{"create-drop.yamsql", SkipGapCatalogTables, "no schema metadata available", "CQ-72"},
	{"catalog.yamsql", SkipGapCatalogTables, "no schema metadata available", "CQ-72"},

	// `1I` / `3i` / `2L` / `4l` — the width-suffixed integer literals. The
	// constant folder hands the whole token to strconv.ParseInt.
	{"literal-tests.yamsql", SkipGapTypedIntLiteral, `integer parse "1I"`, "CQ-72"},

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

	// `CAST([1, 2, 3] AS STRING ARRAY)` yields NULL instead of the cast array.
	{"documentation-queries/cast-documentation-queries.yamsql", SkipGapCastArray, "actual result set is NULL", "CQ-72"},

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
	{"maxRows.yamsql", SkipConformanceGoAccepts, "however it succeeded", "CQ-72"},

	// A `create schema template` issued as a SETUP STEP rather than as a
	// schema_template block, declaring struct types. Same Phase-3 gap as the
	// block form, reached by a different route, so it is classed with it.
	{"showcasing-tests.yamsql", SkipDDLStruct, "only primitive column types are supported", "CQ-73"},
	{"create-drop-create-template.yamsql", SkipDDLStruct, "only primitive column types are supported", "CQ-73"},
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
