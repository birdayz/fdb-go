package sqldriver_test

// A differential oracle for the WHOLE sargability path: for any predicate P
// over an indexed column, the rows an index-driven plan returns must be
// EXACTLY the rows a full-scan-plus-residual-filter plan returns — same
// multiset, and same sequence when the query carries an ORDER BY.
//
// The motivating example is RFC-179 finding F11: STARTS_WITH was consumed
// into the index scan range (so the planner dropped the residual filter) but
// scanComparisonsToTupleRange (pkg/recordlayer/query/executor/executor.go)
// had no switch arm for it, so the bound silently vanished and the index scan
// returned extra rows a full scan + WHERE-clause filter would have rejected.
// Any arm of that switch (or of the SARG-extraction/span-construction path
// feeding it) that drops a bound the same way makes this oracle RED — proven
// below by deliberately breaking the ComparisonGreaterThan arm and confirming
// the resulting extra-rows mismatch.
//
// F11's OWN arm is not actually reachable that way: the fix commit
// (89a930a7f) documents it as "latent (not SQL-reachable through today's
// surface — no live producer)... becomes wrong-rows the moment its enabling
// feature (a LIKE->STARTS_WITH rewrite) lands", and this file independently
// confirms that by direct EXPLAIN inspection — `s LIKE 'prefix%'` NEVER
// produces an IndexScan on any table size tried (6 through 150 rows), because
// walk.go's LikePredicateContext arm always calls ResolveLikeWithEscape, never
// ResolveStartsWith/ComparisonStartsWith; grepping the whole tree turns up
// zero non-test callers of ResolveStartsWith and no rewrite rule that
// produces STARTS_WITH from a LIKE pattern. So the LIKE/NOT LIKE cases below
// are still valid full-scan-vs-index-scan differential cases in general (kept
// for the day a LIKE->STARTS_WITH rewrite lands, at which point they start
// actually exercising the index path) but they do not, today, exercise F11's
// arm specifically — the demonstration mutation below uses a comparison the
// SQL surface DOES reach instead.
//
// Mechanism: DISABLED_PLANNER_RULES=[MatchLeafRule] removes index-candidate
// matching from the Cascades rule set (proven in
// TestFDB_PlannerOptions_DisabledPlannerRules, planner_options_fdb_test.go),
// forcing a full table scan with the WHERE clause evaluated by the general
// expression interpreter instead of a TupleRange. Comparing that side against
// the default (index-matched) side is exactly "the same query, two physical
// plans" — a Cascades optimiser's central guarantee, and what this checks.
//
// Every case first confirms the baseline plan actually used an index and
// that disabling MatchLeafRule actually produced a DIFFERENT plan with no
// IndexScan in it. A case that fails that precondition (some OR-across-
// columns shapes never hit the composite index in the first place) is
// skipped with a logged reason, never silently treated as a pass — the
// row-equality assertion below it would prove nothing.
//
// Schema/predicate/boundary generation is deterministic (fixed seed) so a
// failure reproduces byte-for-byte from the test name and logged seed. The
// AND/OR/3-way combo generator (the one dimension with real combinatorial
// depth) is sized by SARG_ORACLE_COMBOS (default kept small so the whole file
// runs in a few seconds) and seeded by SARG_ORACLE_SEED:
//
//	SARG_ORACLE_COMBOS=5000 SARG_ORACLE_SEED=99 go test \
//	  ./pkg/relational/sqldriver/... -run TestFDB_SargabilityDifferentialOracle -v
//
// Two documented compromises:
//
//   - The grammar's inList rule (RelationalParser.g4 `inList: '('
//     (queryExpressionBody | expressions) ')' | preparedStatementParameter |
//     fullColumnName`) requires at least one element or a subquery, and
//     `IN (SELECT ...)` is rejected outright (in_subquery_probe_test.go) —
//     there is no SQL text that spells a literal empty IN list. The reachable
//     next-best is an IN list of values provably absent from the column's
//     domain, which exercises the same "matches nothing" SARG shape without
//     literally being `IN ()`.
//   - A NULL-containing IN list (`k IN (1, NULL, 2)`) is not a sargability
//     case at all: both engines reject it identically at semantic analysis
//     (42809 WRONG_OBJECT_TYPE, "NULL values are not allowed in the IN
//     list" — Java's SemanticAnalyzer.validateInListItems, ported in
//     logical_predicate.go/walk.go and already pinned end-to-end in
//     not_in_null_probe_test.go), so there is no row-level behavior to
//     differential-test — the query never reaches a plan. Confirmed by
//     running this file with the shape included: every such case failed with
//     "42809: NULL values are not allowed in the IN list" at the EXPLAIN
//     step, not a row mismatch. Left out of the generators below.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// --- knobs ---

func sargOracleCombos(defaultN int) int {
	if v := os.Getenv("SARG_ORACLE_COMBOS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultN
}

func sargOracleSeed(defaultSeed int64) int64 {
	if v := os.Getenv("SARG_ORACLE_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultSeed
}

// --- generic differential machinery ---

// sargCase is one generated query: a WHERE clause and an optional ORDER BY.
// orderBy == "" means the property is checked as a MULTISET (row order may
// legitimately differ between plans); a non-empty orderBy means the property
// is checked as an exact SEQUENCE.
type sargCase struct {
	name    string
	where   string
	orderBy string
}

// collectRowTuples drains q into one string per row (all projected columns),
// preserving arrival order — callers decide whether order matters.
func collectRowTuples(t *testing.T, ctx context.Context, conn *sql.Conn, q string) []string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("query %s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns %s: %v", q, err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", q, err)
		}
		out = append(out, fmt.Sprintf("%v", vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %s: %v", q, err)
	}
	return out
}

func sargSorted(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

func sargStringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sargOracleCounters tallies how many generated cases actually exercised the
// index-vs-full-scan differential versus were skipped because the baseline
// never used an index for that shape (reported at the end of the test, never
// silently dropped).
type sargOracleCounters struct {
	kept, skipped int
}

// runSargabilityCase runs one case against two pinned connections to the
// same db+schema: the default (index-eligible) planner and one with
// MatchLeafRule disabled (full scan + residual filter). It is the whole
// oracle: precondition (plans differ, baseline used an index), then the
// property (same rows).
func runSargabilityCase(t *testing.T, ctx context.Context, db *sql.DB, table, projection string, c sargCase, ctr *sargOracleCounters) {
	t.Helper()
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s", projection, table, c.where)
	if c.orderBy != "" {
		q += " ORDER BY " + c.orderBy
	}

	idxConn := pinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
	fullConn := pinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) {
		ec.SetOptions(api.NewOptionsBuilder().
			Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}).Build())
	})

	idxPlan := explainOnConn(t, ctx, idxConn, q)
	if !strings.Contains(idxPlan, "IndexScan") {
		ctr.skipped++
		t.Logf("SKIP %s: baseline plan does not use an index (%q); SQL: %s", c.name, idxPlan, q)
		return
	}
	fullPlan := explainOnConn(t, ctx, fullConn, q)
	if fullPlan == idxPlan || strings.Contains(fullPlan, "IndexScan") {
		t.Fatalf("%s: DISABLED_PLANNER_RULES=[MatchLeafRule] did not force a different, index-free plan "+
			"(index plan: %q, full-scan plan: %q) — the row comparison below would prove nothing; SQL: %s",
			c.name, idxPlan, fullPlan, q)
	}
	ctr.kept++

	idxRows := collectRowTuples(t, ctx, idxConn, q)
	fullRows := collectRowTuples(t, ctx, fullConn, q)

	if c.orderBy != "" {
		if !sargStringsEqual(idxRows, fullRows) {
			t.Fatalf("SARGABILITY MISMATCH (ordered) %s\n  SQL: %s\n  index plan:      %s\n  full-scan plan:  %s\n"+
				"  index rows:      %v\n  full-scan rows:  %v",
				c.name, q, idxPlan, fullPlan, idxRows, fullRows)
		}
		return
	}
	a, b := sargSorted(idxRows), sargSorted(fullRows)
	if !sargStringsEqual(a, b) {
		t.Fatalf("SARGABILITY MISMATCH %s\n  SQL: %s\n  index plan:      %s\n  full-scan plan:  %s\n"+
			"  index rows (sorted):     %v\n  full-scan rows (sorted): %v",
			c.name, q, idxPlan, fullPlan, a, b)
	}
}

// --- predicate generation: numeric columns ---

// intColModel is what the generator needs about a column's ACTUAL inserted
// data: the boundary constants (below-min / min / interior / max / above-max)
// come from real data, not guesses, so every generated predicate has a
// meaningful (non-vacuous) answer.
type intColModel struct {
	name   string
	values []int64 // sorted, deduplicated, non-NULL values actually inserted
}

func (c intColModel) min() int64 { return c.values[0] }
func (c intColModel) max() int64 { return c.values[len(c.values)-1] }
func (c intColModel) interior() int64 {
	return c.values[len(c.values)/2]
}

func (c intColModel) boundaries() []int64 {
	raw := []int64{c.min() - 1, c.min(), c.interior(), c.max(), c.max() + 1}
	seen := map[int64]bool{}
	var out []int64
	for _, v := range raw {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

var intCmpOps = []struct{ sym, tag string }{
	{"=", "eq"}, {"<>", "ne"}, {"<", "lt"}, {"<=", "le"}, {">", "gt"}, {">=", "ge"},
}

// intAtoms generates the single-column predicate shapes item 2 of the RFC
// asks for, over col: comparisons at every boundary constant, IS [NOT] NULL,
// IN (present / absent-from-domain / NULL-containing), BETWEEN, NOT(...),
// and the `= NULL` / `< NULL` degenerate-comparand shapes that exercise the
// explicit empty-range branches in scanComparisonsToTupleRange.
func intAtoms(col intColModel) []sargCase {
	var cs []sargCase
	for _, b := range col.boundaries() {
		for _, op := range intCmpOps {
			cs = append(cs, sargCase{
				name:  fmt.Sprintf("%s_%s_%d", col.name, op.tag, b),
				where: fmt.Sprintf("%s %s %d", col.name, op.sym, b),
			})
		}
	}
	min, max, interior := col.min(), col.max(), col.interior()
	cs = append(cs,
		sargCase{name: col.name + "_is_null", where: col.name + " IS NULL"},
		sargCase{name: col.name + "_is_not_null", where: col.name + " IS NOT NULL"},
		sargCase{name: col.name + "_between_full", where: fmt.Sprintf("%s BETWEEN %d AND %d", col.name, min, max)},
		sargCase{name: col.name + "_between_partial", where: fmt.Sprintf("%s BETWEEN %d AND %d", col.name, min-5, interior)},
		sargCase{name: col.name + "_in_present", where: fmt.Sprintf("%s IN (%d, %d, %d)", col.name, min, interior, max)},
		// No SQL text spells a literal empty IN list (see file header); the
		// reachable next-best is an IN list of values absent from the domain.
		sargCase{name: col.name + "_in_absent", where: fmt.Sprintf("%s IN (%d, %d)", col.name, min-1000, max+1000)},
		sargCase{name: col.name + "_not_in", where: fmt.Sprintf("%s NOT IN (%d, %d)", col.name, min, max)},
		sargCase{name: col.name + "_not_eq_interior", where: fmt.Sprintf("NOT (%s = %d)", col.name, interior)},
		// `col = NULL` / `col < NULL`: a NULL COMPARAND (not IS NULL) is
		// UNKNOWN for every row in SQL 3VL — the explicit empty-range guards
		// at executor.go:732 and :840.
		sargCase{name: col.name + "_eq_null_literal", where: col.name + " = NULL"},
		sargCase{name: col.name + "_lt_null_literal", where: col.name + " < NULL"},
	)
	return cs
}

// --- predicate generation: string columns ---

type strColModel struct {
	name   string
	values []string // sorted, deduplicated, non-NULL values actually inserted
}

func (c strColModel) min() string      { return c.values[0] }
func (c strColModel) max() string      { return c.values[len(c.values)-1] }
func (c strColModel) interior() string { return c.values[len(c.values)/2] }

func (c strColModel) boundaries() []string {
	raw := []string{c.min(), c.interior(), c.max()}
	seen := map[string]bool{}
	var out []string
	for _, v := range raw {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// strAtoms mirrors intAtoms for a string column: comparisons at real boundary
// values, IS [NOT] NULL, BETWEEN, IN, and LIKE 'prefix%' / NOT LIKE. LIKE is
// the shape F11 was ABOUT (see file header), but today it is always a residual
// filter, never sargable, so on this single-column-index schema the baseline
// plan for a standalone LIKE never uses the index — runSargabilityCase logs
// SKIP for both, not a false pass. They stay in the generator so the day a
// LIKE->STARTS_WITH rewrite lands, this file starts exercising it for free.
func strAtoms(col strColModel) []sargCase {
	var cs []sargCase
	for _, b := range col.boundaries() {
		for _, op := range intCmpOps {
			cs = append(cs, sargCase{
				name:  fmt.Sprintf("%s_%s_%s", col.name, op.tag, b),
				where: fmt.Sprintf("%s %s '%s'", col.name, op.sym, b),
			})
		}
	}
	sample := col.interior()
	prefix := sample
	if len(sample) > 2 {
		prefix = sample[:2]
	}
	cs = append(cs,
		sargCase{name: col.name + "_is_null", where: col.name + " IS NULL"},
		sargCase{name: col.name + "_is_not_null", where: col.name + " IS NOT NULL"},
		sargCase{name: col.name + "_between", where: fmt.Sprintf("%s BETWEEN '%s' AND '%s'", col.name, col.min(), col.max())},
		sargCase{name: col.name + "_in", where: fmt.Sprintf("%s IN ('%s', '%s')", col.name, col.min(), col.max())},
		sargCase{name: col.name + "_like_prefix", where: fmt.Sprintf("%s LIKE '%s%%'", col.name, prefix)},
		sargCase{name: col.name + "_not_like_prefix", where: fmt.Sprintf("NOT (%s LIKE '%s%%')", col.name, prefix)},
		sargCase{name: col.name + "_eq_null_literal", where: col.name + " = NULL"},
	)
	return cs
}

// --- predicate generation: floating-point columns ---

// floatColModel mirrors intColModel/strColModel for a DOUBLE/FLOAT column:
// real boundary constants come from data actually inserted, never guesses.
type floatColModel struct {
	name   string
	values []float64 // sorted, deduplicated, non-NULL values actually inserted
}

func (c floatColModel) min() float64      { return c.values[0] }
func (c floatColModel) max() float64      { return c.values[len(c.values)-1] }
func (c floatColModel) interior() float64 { return c.values[len(c.values)/2] }

// renderFloat formats v as a SQL REAL_LITERAL. The grammar
// (RelationalLexer.g4) requires a decimal point or an exponent, so an
// integral value like 2^53 needs an explicit ".0" appended or it lexes as a
// DECIMAL_LITERAL (BIGINT), a different literal TYPE. strconv's shortest
// round-trip format reproduces the exact float64 bit pattern, which matters
// for the float32 width-narrowing boundary values (their exact decimal
// expansion runs many digits, e.g. float64(float32(0.1))).
func renderFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// sargFloatTag turns a float64 into a test-name-safe fragment.
func sargFloatTag(v float64) string {
	return strings.NewReplacer(".", "p", "-", "n").Replace(strconv.FormatFloat(v, 'f', -1, 64))
}

// floatAtoms mirrors intAtoms for a DOUBLE/FLOAT column: comparisons at real
// boundary values (rendered as float literals), IS [NOT] NULL, BETWEEN, IN,
// NOT(...) — plus two shapes intAtoms/strAtoms have no analogue for, both
// aimed squarely at the bug this file exists to catch:
//
//   - bare INTEGER-literal comparisons (`f = 1`, `f > 0`, …): the exact
//     cross-WIDTH shape that matched ZERO rows for EVERY comparison against
//     an indexed FLOAT column before narrowConstAgainstFloatColumn /
//     widenConstAgainstDoubleColumn — a same-typed float literal alone can
//     never reach that promotion code path at all.
//   - the float32-inexact boundary (0.1) at every ordered comparison: 0.1
//     has no exact float32 representation, so a FLOAT column's stored value
//     for a row inserted with 0.1 is the REAL number
//     float64(float32(0.1)) ≈ 0.10000000149011612 — strictly greater than
//     the double literal 0.1. A naive "round the literal to float32 and
//     keep the operator" rewrite gets `f > 0.1` / `f < 0.1` wrong at that
//     row (see indexed_float_sarg_probe_test.go, which pins the identical
//     case against a fixed expectation; this file re-derives the same
//     property differentially instead).
func floatAtoms(col floatColModel) []sargCase {
	var cs []sargCase
	min, max, interior := col.min(), col.max(), col.interior()
	for _, b := range []float64{min - 1, min, interior, max, max + 1} {
		for _, op := range intCmpOps {
			cs = append(cs, sargCase{
				name:  fmt.Sprintf("%s_%s_%s", col.name, op.tag, sargFloatTag(b)),
				where: fmt.Sprintf("%s %s %s", col.name, op.sym, renderFloat(b)),
			})
		}
	}
	// Bare INTEGER literal comparisons — see doc comment above.
	for _, iv := range []int64{-1, 0, 1, 2} {
		for _, op := range intCmpOps {
			cs = append(cs, sargCase{
				name:  fmt.Sprintf("%s_intlit_%s_%d", col.name, op.tag, iv),
				where: fmt.Sprintf("%s %s %d", col.name, op.sym, iv),
			})
		}
	}
	// The float32-inexact boundary (0.1) — see doc comment above.
	for _, op := range intCmpOps {
		cs = append(cs, sargCase{
			name:  col.name + "_boundary01_" + op.tag,
			where: fmt.Sprintf("%s %s 0.1", col.name, op.sym),
		})
	}
	cs = append(cs,
		sargCase{name: col.name + "_is_null", where: col.name + " IS NULL"},
		sargCase{name: col.name + "_is_not_null", where: col.name + " IS NOT NULL"},
		sargCase{name: col.name + "_between_full", where: fmt.Sprintf("%s BETWEEN %s AND %s", col.name, renderFloat(min), renderFloat(max))},
		sargCase{name: col.name + "_between_partial", where: fmt.Sprintf("%s BETWEEN %s AND %s", col.name, renderFloat(min-5), renderFloat(interior))},
		sargCase{name: col.name + "_in_present", where: fmt.Sprintf("%s IN (%s, %s, %s)", col.name, renderFloat(min), renderFloat(interior), renderFloat(max))},
		sargCase{name: col.name + "_in_intlit", where: fmt.Sprintf("%s IN (0, 1, 2)", col.name)},
		sargCase{name: col.name + "_not_in", where: fmt.Sprintf("%s NOT IN (%s, %s)", col.name, renderFloat(min), renderFloat(max))},
		sargCase{name: col.name + "_not_eq_interior", where: fmt.Sprintf("NOT (%s = %s)", col.name, renderFloat(interior))},
		sargCase{name: col.name + "_eq_null_literal", where: col.name + " = NULL"},
		sargCase{name: col.name + "_lt_null_literal", where: col.name + " < NULL"},
	)
	return cs
}

// composeCombos builds the AND/OR/3-way combinations item 2 asks for, drawing
// atoms from a's, b's (and, for the 3-way shape, c's) generated atoms. AND-2
// stays within the composite (a,b) index's sargable prefix; OR-2 and AND-3
// (the latter adding a predicate on a column NOT in the index, exercising
// residual-filter placement) are exactly the shapes most likely to fall off
// the sargable path — which is why runSargabilityCase's precondition check,
// not this generator, decides whether a given combo actually reached the
// index.
func composeCombos(rng *rand.Rand, n int, atomsA, atomsB, atomsC []sargCase) []sargCase {
	var out []sargCase
	for i := 0; i < n; i++ {
		a := atomsA[rng.Intn(len(atomsA))]
		b := atomsB[rng.Intn(len(atomsB))]
		switch rng.Intn(3) {
		case 0:
			out = append(out, sargCase{name: fmt.Sprintf("and2_%d", i), where: fmt.Sprintf("(%s) AND (%s)", a.where, b.where)})
		case 1:
			out = append(out, sargCase{name: fmt.Sprintf("or2_%d", i), where: fmt.Sprintf("(%s) OR (%s)", a.where, b.where)})
		case 2:
			c := atomsC[rng.Intn(len(atomsC))]
			out = append(out, sargCase{name: fmt.Sprintf("and3_%d", i), where: fmt.Sprintf("(%s) AND (%s) AND (%s)", a.where, b.where, c.where)})
		}
	}
	return out
}

// --- schema setup ---

// sargOracleSchema creates the seven table shapes the RFC asks for in one
// schema: single-column PK + nullable secondary index, composite PK +
// secondary index, a composite secondary index with a residual (unindexed)
// column, a UNIQUE index, an indexed STRING column (the STARTS_WITH/LIKE
// carrier), and nullable indexed DOUBLE / FLOAT columns — the type class this
// file could not exercise at all until now (no generator here could emit a
// floating-point column, exactly the blind spot that hid the indexed-FLOAT
// SARG bug and the cross-type-numeric-index bug). It returns the *sql.DB plus
// the per-column models the generators need, all derived from data actually
// inserted (never guessed).
func sargOracleSchema(t *testing.T) (db *sql.DB, singleK, compositeK, idxA, idxB, idxC intColModel, uniqU intColModel, idxS strColModel, idxD, idxF floatColModel) {
	t.Helper()
	// SAFETY INVARIANT (do not violate without reading TODO.md CQ-28 first):
	// the ONLY composite index below, composite_idx_ab, is BIGINT-only —
	// dbl_col/flt_col each carry a single-column index. dbl_col/flt_col's
	// row generation now seeds -0.0 (CQ-27 closed the single-column
	// signed-zero SARG gap), which is safe ONLY because no composite index
	// puts a FLOAT/DOUBLE column in a NON-TERMINAL position: CQ-28 is the
	// narrower, still-open sibling gap for exactly that shape
	// (negative_zero_composite_index_sarg_probe_test.go has the
	// reproducer). Adding a DOUBLE/FLOAT column to composite_idx_ab (or a
	// new composite index) before CQ-28 closes would reopen a
	// deterministic, ALWAYS-RUN red for an already-known reason.
	const dbPath = "/testdb_sargoracle"
	setup := openTestDB(t, dbPath)
	ctx := context.Background()
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE sargoracle "+
			"CREATE TABLE single_col (id BIGINT NOT NULL, k BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX single_col_k ON single_col (k) "+
			"CREATE TABLE composite_pk (id1 BIGINT NOT NULL, id2 BIGINT NOT NULL, k BIGINT NOT NULL, PRIMARY KEY (id1, id2)) "+
			"CREATE INDEX composite_pk_k ON composite_pk (k) "+
			"CREATE TABLE composite_idx (id BIGINT NOT NULL, a BIGINT, b BIGINT, c BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX composite_idx_ab ON composite_idx (a, b) "+
			"CREATE TABLE uniq_col (id BIGINT NOT NULL, u BIGINT, PRIMARY KEY (id)) "+
			"CREATE UNIQUE INDEX uniq_col_u ON uniq_col (u) "+
			"CREATE TABLE str_col (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id)) "+
			"CREATE INDEX str_col_s ON str_col (s) "+
			"CREATE TABLE dbl_col (id BIGINT NOT NULL, d DOUBLE, PRIMARY KEY (id)) "+
			"CREATE INDEX dbl_col_d ON dbl_col (d) "+
			"CREATE TABLE flt_col (id BIGINT NOT NULL, f FLOAT, PRIMARY KEY (id)) "+
			"CREATE INDEX flt_col_f ON flt_col (f)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE sargoracle")
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rng := rand.New(rand.NewSource(sargOracleSeed(20260726)))

	// single_col: k in roughly [-40, 60], ~10% NULL.
	singleK.name = "k"
	seenK := map[int64]bool{}
	for i := 0; i < 150; i++ {
		if rng.Intn(10) == 0 {
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO single_col (id) VALUES (%d)", i))
			continue
		}
		v := int64(rng.Intn(101) - 40)
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO single_col (id, k) VALUES (%d, %d)", i, v))
		if !seenK[v] {
			seenK[v] = true
			singleK.values = append(singleK.values, v)
		}
	}
	sort.Slice(singleK.values, func(i, j int) bool { return singleK.values[i] < singleK.values[j] })

	// composite_pk: k NOT NULL in [0, 30]; id1/id2 form a unique composite key.
	compositeK.name = "k"
	seenCK := map[int64]bool{}
	for i := 0; i < 100; i++ {
		v := int64(rng.Intn(31))
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO composite_pk (id1, id2, k) VALUES (%d, %d, %d)", i/10, i%10, v))
		if !seenCK[v] {
			seenCK[v] = true
			compositeK.values = append(compositeK.values, v)
		}
	}
	sort.Slice(compositeK.values, func(i, j int) bool { return compositeK.values[i] < compositeK.values[j] })

	// composite_idx: a heavy-dup [0,6], b [0,12], both ~10% NULL; c [0,3]
	// NOT NULL is the residual column outside the (a,b) index.
	idxA.name, idxB.name, idxC.name = "a", "b", "c"
	seenA, seenB, seenC := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	for i := 0; i < 150; i++ {
		cols, vals := "id", fmt.Sprintf("%d", i)
		if rng.Intn(10) != 0 {
			v := int64(rng.Intn(7))
			cols += ", a"
			vals += fmt.Sprintf(", %d", v)
			if !seenA[v] {
				seenA[v] = true
				idxA.values = append(idxA.values, v)
			}
		}
		if rng.Intn(10) != 0 {
			v := int64(rng.Intn(13))
			cols += ", b"
			vals += fmt.Sprintf(", %d", v)
			if !seenB[v] {
				seenB[v] = true
				idxB.values = append(idxB.values, v)
			}
		}
		{
			v := int64(rng.Intn(4))
			cols += ", c"
			vals += fmt.Sprintf(", %d", v)
			if !seenC[v] {
				seenC[v] = true
				idxC.values = append(idxC.values, v)
			}
		}
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO composite_idx (%s) VALUES (%s)", cols, vals))
	}
	sort.Slice(idxA.values, func(i, j int) bool { return idxA.values[i] < idxA.values[j] })
	sort.Slice(idxB.values, func(i, j int) bool { return idxB.values[i] < idxB.values[j] })
	sort.Slice(idxC.values, func(i, j int) bool { return idxC.values[i] < idxC.values[j] })

	// uniq_col: 60 rows, unique non-NULL u values, ~15% forced NULL (a UNIQUE
	// index permits multiple NULLs — pinned in unique_null_batch_probe_test.go).
	uniqU.name = "u"
	perm := rng.Perm(200)
	uIdx := 0
	for i := 0; i < 60; i++ {
		if rng.Intn(100) < 15 {
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO uniq_col (id) VALUES (%d)", i))
			continue
		}
		v := int64(perm[uIdx])
		uIdx++
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO uniq_col (id, u) VALUES (%d, %d)", i, v))
		uniqU.values = append(uniqU.values, v)
	}
	sort.Slice(uniqU.values, func(i, j int) bool { return uniqU.values[i] < uniqU.values[j] })

	// str_col: 100 rows from a small alphabet of prefixes x suffixes, so LIKE
	// 'prefix%' has a real, non-trivial match set; ~10% NULL.
	idxS.name = "s"
	prefixes := []string{"ap", "ba", "ca", "do", "el"}
	suffixes := []string{"ple", "nana", "t", "g", "k"}
	seenS := map[string]bool{}
	for i := 0; i < 100; i++ {
		if rng.Intn(10) == 0 {
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO str_col (id) VALUES (%d)", i))
			continue
		}
		v := prefixes[rng.Intn(len(prefixes))] + suffixes[rng.Intn(len(suffixes))]
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO str_col (id, s) VALUES (%d, '%s')", i, v))
		if !seenS[v] {
			seenS[v] = true
			idxS.values = append(idxS.values, v)
		}
	}
	sort.Strings(idxS.values)

	// dbl_col: nullable indexed DOUBLE — random fractional values in
	// roughly [-40, 60] ~10% NULL, plus deliberate boundary rows: the
	// double-exactness boundary 2^53 and its negation (beyond which not
	// every integer is exactly representable, probed on both sides of
	// zero), the float32-inexact fraction 0.1 (meaningful here as a plain
	// double value, and directly comparable against flt_col's rounded
	// copy), and signed zero (TODO.md CQ-27, FIXED: an indexed column's SARG
	// range/probe construction used to disagree with the residual-filter
	// path — cmpAny's IEEE equality (-0.0 == +0.0) — on a query landing
	// exactly on zero; scanComparisonsToTupleRange now widens the zero
	// endpoint of a range to span both adjacent keys, so the two physical
	// plans this file's differential mechanism compares AGREE. floatAtoms'
	// bare-integer-literal sweep already runs `= 0`, `>= 0`, `< 0`, … against
	// this column on every invocation, so this row is exercised on EVERY
	// run, not merely at random — the dedicated pin
	// (negative_zero_index_sarg_probe_test.go) remains the fast, targeted
	// regression).
	idxD.name = "d"
	seenD := map[float64]bool{}
	addD := func(v float64) {
		if !seenD[v] {
			seenD[v] = true
			idxD.values = append(idxD.values, v)
		}
	}
	for i := 0; i < 150; i++ {
		if rng.Intn(10) == 0 {
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO dbl_col (id) VALUES (%d)", 2000+i))
			continue
		}
		v := float64(rng.Intn(101)-40) + float64(rng.Intn(10))/10
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO dbl_col (id, d) VALUES (%d, %s)", 2000+i, renderFloat(v)))
		addD(v)
	}
	for i, sp := range []float64{0.1, 9007199254740992, -9007199254740992, math.Copysign(0, -1)} {
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO dbl_col (id, d) VALUES (%d, %s)", 2500+i, renderFloat(sp)))
		addD(sp)
	}
	sort.Float64s(idxD.values)

	// flt_col: nullable indexed FLOAT (32-bit). The model's authoritative
	// values are the ROUNDED (float64(float32(x))) copies — the exact value
	// the column actually stores and reads back (row-domain convention, see
	// executor.coerceTupleElement) — including the float32 mantissa-
	// exactness boundary 2^24 (DOUBLE's 2^53 one width narrower) and 0.1
	// (NOT exactly representable in float32 — the width-narrowing boundary
	// that hid the original bug: see indexed_float_sarg_probe_test.go and
	// floatAtoms' doc comment).
	idxF.name = "f"
	seenF := map[float64]bool{}
	addF := func(v float64) float64 {
		v = float64(float32(v))
		if !seenF[v] {
			seenF[v] = true
			idxF.values = append(idxF.values, v)
		}
		return v
	}
	for i := 0; i < 150; i++ {
		if rng.Intn(10) == 0 {
			mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO flt_col (id) VALUES (%d)", 3000+i))
			continue
		}
		v := float64(rng.Intn(101)-40) + float64(rng.Intn(10))/10
		stored := addF(v)
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO flt_col (id, f) VALUES (%d, %s)", 3000+i, renderFloat(stored)))
	}
	for i, sp := range []float64{0.1, 16777216, -16777216, math.Copysign(0, -1)} {
		stored := addF(sp)
		mwjoMustExec(t, db, ctx, fmt.Sprintf("INSERT INTO flt_col (id, f) VALUES (%d, %s)", 3500+i, renderFloat(stored)))
	}
	sort.Float64s(idxF.values)

	return db, singleK, compositeK, idxA, idxB, idxC, uniqU, idxS, idxD, idxF
}

// TestFDB_SargabilityDifferentialOracle is the property test itself: schema x
// predicate x boundary, index-plan rows == full-scan-plan rows.
func TestFDB_SargabilityDifferentialOracle(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db, singleK, compositeK, idxA, idxB, idxC, uniqU, idxS, idxD, idxF := sargOracleSchema(t)
	ctr := &sargOracleCounters{}

	run := func(table, projection string, cases []sargCase) {
		for _, c := range cases {
			c := c
			t.Run(table+"/"+c.name, func(t *testing.T) {
				runSargabilityCase(t, ctx, db, table, projection, c, ctr)
			})
		}
	}

	// Single-column PK, nullable indexed column: the base sargability case,
	// every comparison + IN/BETWEEN/NULL shape at real boundary constants.
	run("single_col", "id", intAtoms(singleK))

	// Composite PK: the same shapes, but the index-side plan must fetch the
	// full (id1, id2) composite primary key after the range scan.
	run("composite_pk", "id1, id2", intAtoms(compositeK))

	// UNIQUE index, nullable column.
	run("uniq_col", "id", intAtoms(uniqU))

	// Indexed STRING column: comparisons, BETWEEN, IN, and LIKE 'prefix%'/NOT
	// LIKE (currently residual-only — see strAtoms — so these two log SKIP).
	run("str_col", "id", strAtoms(idxS))

	// Composite (a,b) index: single-column atoms on the leading column stay
	// sargable; AND/OR/3-way combos (with c as the residual, unindexed
	// column) exercise span construction and residual-filter placement.
	run("composite_idx", "id", intAtoms(idxA))
	rng := rand.New(rand.NewSource(sargOracleSeed(20260726) + 1))
	run("composite_idx", "id", composeCombos(rng, sargOracleCombos(30), intAtoms(idxA), intAtoms(idxB), intAtoms(idxC)))

	// Nullable indexed DOUBLE / FLOAT columns: real boundary constants plus
	// bare-INTEGER-literal comparisons — the cross-width shape
	// (narrowConstAgainstFloatColumn / widenConstAgainstDoubleColumn) that
	// matched ZERO rows for every comparison against an indexed FLOAT
	// column, and returned empty for every DOUBLE comparison whose constant
	// arrived as an int, before that fix.
	run("dbl_col", "id", floatAtoms(idxD))
	run("flt_col", "id", floatAtoms(idxF))

	// Reverse scans: ORDER BY forces the index-side plan to either scan the
	// (ascending-only; Go has no per-column DESC index storage) index in
	// REVERSE or to fall back to an in-memory sort, while the full-scan side
	// always sorts explicitly — comparing as a SEQUENCE, not a multiset.
	min, max, interior := singleK.min(), singleK.max(), singleK.interior()
	orderedCases := []sargCase{
		{name: "order_desc_gt", where: fmt.Sprintf("k > %d", interior), orderBy: "k DESC"},
		{name: "order_asc_ge", where: fmt.Sprintf("k >= %d", min), orderBy: "k ASC"},
		{name: "order_desc_between", where: fmt.Sprintf("k BETWEEN %d AND %d", min, max), orderBy: "k DESC"},
		{name: "order_asc_le", where: fmt.Sprintf("k <= %d", interior), orderBy: "k ASC"},
	}
	run("single_col", "k, id", orderedCases)

	t.Logf("sargability oracle: %d cases exercised the index-vs-full-scan differential, %d skipped "+
		"(baseline never reached an index for that shape)", ctr.kept, ctr.skipped)
	if ctr.kept == 0 {
		t.Fatal("zero cases exercised the differential — the whole oracle would be vacuous")
	}
}
