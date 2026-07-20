// Package rowdiff is the RFC-182 generative row-soundness differential
// harness: seeded random (schema, data, query) cases executed through the
// full production path (sqldriver → Cascades → executor → real FDB), rows
// diffed against Oracle M — a brute-force in-memory full-scan evaluation
// over the generator's own authoritative row set, reusing the engine's
// predicate evaluation (predicates.Comparison.Eval) so the planner is the
// only component removed. Any mismatch is a plan-soundness finding.
//
// The generator deliberately over-weights the matrix cell that hid the
// pk-intersection residual-drop bug (audit 2026-07-18): two or more indexed
// columns bound PLUS an unindexed residual, executed under every projection
// variant (`SELECT *` vs narrow — projection flips cost winners and masked
// that bug).
package rowdiff

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ColType is the P1 column-type universe. DOUBLE is deliberately absent
// (RFC-182 OQ-3); LIKE/IN/BETWEEN arrive with P2.
type ColType int

const (
	ColBigint ColType = iota
	ColString
	ColBoolean
)

// ColumnDef describes one generated column.
type ColumnDef struct {
	Name    string
	Type    ColType
	NotNull bool
}

// IndexDef is a single- or multi-column value index.
type IndexDef struct {
	Name string
	Cols []string
}

// TableDef is the generated table: fixed pk column "ID" plus value columns.
type TableDef struct {
	Name    string
	Cols    []ColumnDef // excludes ID
	Indexes []IndexDef
}

// Row maps UPPER-CASE column name (incl. "ID") to a driver-typed value:
// int64, string, bool, or nil.
type Row map[string]any

// Pred is one comparison leaf: COL <op> literal, COL IS [NOT] NULL,
// COL IN (…), COL BETWEEN lo AND hi, or COL LIKE pattern.
type Pred struct {
	Col string
	Op  predicates.ComparisonType
	Lit any // nil for IS NULL / IS NOT NULL; the pattern for LIKE; lo for BETWEEN

	// InList holds the membership list for Op == ComparisonIn.
	InList []any
	// BetweenHi holds the inclusive upper bound when IsBetween (rendered as
	// BETWEEN; the oracle evaluates it as >=Lit AND <=BetweenHi with the
	// engine's own comparisons, which is the SQL desugaring).
	BetweenHi any
	IsBetween bool
	// RhsCol, when non-empty, makes this a COLUMN-vs-COLUMN comparison
	// (`a < b`) instead of column-vs-literal. Non-sargable, so it forces
	// residual filters and different plan shapes than a literal comparison.
	RhsCol string
	// Qual / RhsQual are the table-alias qualifiers in a JOIN query ("L" or
	// "R"); empty in single-table queries. The oracle keys joined rows by
	// "<QUAL>.<COL>", so these select the side each operand reads.
	Qual    string
	RhsQual string
	// HasArith makes the leaf's LHS an arithmetic expression `Col <ArithOp>
	// ArithCol2` (both read from the leaf's own Qual side) instead of a bare
	// column: `(a - b) <Op> Lit`. Only subtraction is generated — over the
	// value domain ({-1, 2^62, 0..9}) |a-b| < 2^63, so it never overflows and
	// there is no 22003 error path. A NULL in either operand propagates to a
	// NULL LHS, so the comparison is UNKNOWN.
	HasArith  bool
	ArithOp   values.ArithmeticOp
	ArithCol2 string
	// Negated renders `NOT (…)` around the leaf (and `NOT IN` for IN).
	Negated bool
}

// BoolNode is the AND/OR tree over leaves. Exactly one of Leaf / Kids is set.
// Not negates the whole node (Kleene NOT: NOT UNKNOWN stays UNKNOWN).
type BoolNode struct {
	Leaf *Pred
	And  bool // valid when len(Kids) > 0
	Kids []*BoolNode
	Not  bool
}

// NullsPlacement is an ORDER BY key's NULL position. Default follows the
// engine's Java/FDB-parity rule: NULLS FIRST ascending, NULLS LAST
// descending (tuple order). Explicit placements render as NULLS FIRST/LAST.
type NullsPlacement int

const (
	NullsDefault NullsPlacement = iota
	NullsFirst
	NullsLast
)

// OrderKey is one ORDER BY component. Nullable sort keys are allowed; keys
// are always suffixed with ID by the generator so the total order stays
// unique and the ordered comparator exact.
type OrderKey struct {
	Col   string
	Desc  bool
	Nulls NullsPlacement
	// Qual is the table-alias qualifier in a JOIN query ("L"/"R"), empty
	// otherwise.
	Qual string
}

// JoinSpec is a SELF-join of the case's table under aliases L and R, joined
// on `L.<LeftCol> = R.<RightCol>`. A self-join exercises the whole join
// planner (NLJ, join ordering, correlated index access) without needing a
// second table, and keeps the oracle a plain nested loop over one row set.
//
// Join queries always project EXPLICITLY ALIASED columns (l_id, r_a, …):
// unaliased `l.id, r.id` would yield two output columns both named ID, and
// the harness keys rows by column name, so the duplicate would collapse and
// silently weaken the comparison.
type JoinSpec struct {
	LeftCol  string
	RightCol string
	// Inner renders `JOIN … ON …`; when false the join is expressed as a
	// comma cross-join with the equality moved into the WHERE — the same
	// logical query, a different parse path into the planner.
	Inner bool
	// LeftOuter makes the ON-join a LEFT OUTER JOIN: unmatched left rows are
	// kept, NULL-extended on the right. Implies Inner (LEFT OUTER needs the ON
	// form — a comma join has no outer semantics). This reaches the
	// NULL-extension wrong-rows surface INNER never does, and pins the
	// WHERE-vs-ON subtlety: an R-side filter in the WHERE drops the
	// NULL-extended rows (r.col is NULL → predicate UNKNOWN), collapsing a
	// LEFT JOIN back toward inner semantics — the oracle applies the WHERE
	// post-join over the NULL-extended row, exactly as SQL does.
	LeftOuter bool
}

// AggFunc is a supported aggregate. AVG is deliberately absent: its
// result type and rounding are their own semantic axis (integer vs
// floating division), and getting that wrong in the oracle would produce
// false findings rather than real ones.
type AggFunc int

const (
	AggCountStar AggFunc = iota // COUNT(*)   — counts rows, NULLs included
	AggCountCol                 // COUNT(col) — counts NON-NULL values only
	AggSum
	AggMin
	AggMax
)

// AggSpec is one aggregate query: optional GROUP BY key plus one aggregate
// over a BIGINT column.
//
// HONESTY NOTE (RFC-182 §7 / §12): unlike every other oracle path, the
// aggregate evaluation here is a REIMPLEMENTATION — aggregation is not
// expressible through the engine's per-row Comparison evaluation, so this
// is the one place the oracle restates SQL semantics rather than sharing
// the engine's. The restated rules are the well-defined ones (SUM/MIN/MAX
// ignore NULLs and yield NULL for an all-NULL or empty group; COUNT(*)
// counts rows; COUNT(col) counts non-NULLs; a NULL grouping key is its own
// group), and a divergence here is investigated on BOTH sides before it is
// called an engine bug.
type AggSpec struct {
	Func     AggFunc
	Col      string   // aggregated column ("" for COUNT(*))
	GroupBy  []string // empty = scalar aggregate over the whole input; 1+ = grouped
	Having   *Pred    // optional filter on the aggregate result
	HavingOn bool
}

// groupKeyCol names the i-th GROUP BY key's output column. Key 0 stays "G" so
// single-key grouping is byte-identical to the pre-multi-key schema; extra keys
// are "G1", "G2", … Both aggSQL (the aliases) and the oracle emit these names.
func groupKeyCol(i int) string {
	if i == 0 {
		return "G"
	}
	return "G" + strconv.Itoa(i)
}

// ExistsSpec is a CORRELATED [NOT] EXISTS subquery appended to a single-table
// query's WHERE, over the SAME table under alias `r`:
//
//	[NOT] EXISTS (SELECT 1 FROM t AS r WHERE r.<CorrCol> <CorrOp> t.<CorrCol> [AND <Inner>])
//
// The correlation equality and the optional simple inner comparison go through
// the engine's own Comparison eval in the oracle (evalLeaf / EvalAgainst), so
// scalar/NULL semantics are shared by construction — a `r.c = t.c` with a NULL
// on either side is UNKNOWN, so a NULL correlation value matches no inner row.
// What is under test is the PLANNER's correlated-EXISTS handling (semi-join,
// decorrelation, correlation binding), and — since EXISTS is a WHERE filter
// with no output-schema change — it composes with the paging sweep to test
// EXISTS continuation soundness too.
type ExistsSpec struct {
	CorrCol string                    // correlation column: r.CorrCol <CorrOp> t.CorrCol
	CorrOp  predicates.ComparisonType // correlation operator (zero value = equals)
	Inner   *Pred                     // optional extra filter on r (Qual empty; col op literal only)
	Negated bool                      // NOT EXISTS
}

// ScalarSubSpec is a NON-correlated aggregate scalar subquery in a WHERE
// comparison:
//
//	<OuterCol> <Op> (SELECT <Func>(<Col>) FROM t [WHERE <Filter>])
//
// This is a Go read-side extension — Java's grammar has no scalar subquery in
// an expressionAtom (RelationalParser.g4) — so the oracle is the sole authority
// on correctness, exactly the deep coverage the extension needs. The aggregate
// makes the subquery single-valued (no cardinality trap); an optional Filter
// lets the subquery be EMPTY, so MIN/MAX yield NULL and the outer comparison
// `col <op> NULL` is UNKNOWN → the row drops. That NULL-when-empty path is a
// documented past defect here (`id = (SELECT MIN(id) …)` once built `id=NULL`),
// so it is the axis most worth pinning. Func is restricted to MIN/MAX/COUNT so
// the oracle never hits SUM's int64-overflow sentinel.
type ScalarSubSpec struct {
	OuterCol string                    // outer column compared against the scalar
	Op       predicates.ComparisonType // comparison operator
	Func     AggFunc                   // AggMin / AggMax / AggCountStar / AggCountCol
	Col      string                    // aggregated inner column ("" for COUNT(*))
	Filter   *Pred                     // optional inner WHERE (Qual empty; col op literal); nil = whole table
}

// Query is one generated query body; the runner executes it under every
// projection variant and cross-checks row identity via ID.
type Query struct {
	Agg       *AggSpec       // nil = not an aggregate query
	Join      *JoinSpec      // nil = single-table query
	Exists    *ExistsSpec    // non-nil = append a correlated [NOT] EXISTS to WHERE
	ScalarSub *ScalarSubSpec // non-nil = append a scalar-subquery comparison to WHERE
	Where     *BoolNode      // nil = no WHERE
	OrderBy   []OrderKey
	Limit     int  // 0 = no LIMIT
	Offset    int  // 0 = no OFFSET (only emitted alongside LIMIT + ORDER BY)
	Distinct  bool // SELECT DISTINCT (ORDER BY keys ⊆ projection enforced by generator)
}

// Case is everything one seed produces.
type Case struct {
	Seed    uint64
	Table   TableDef
	Rows    []Row
	Queries []Query
}

// Projections returns the projection variants every query runs under:
// star, pk-only, a narrow subset, and the narrow subset reversed.
func (c *Case) Projections() [][]string {
	narrow := []string{"ID"}
	for i, col := range c.Table.Cols {
		if i%2 == 0 {
			narrow = append(narrow, col.Name)
		}
	}
	reversed := make([]string, len(narrow))
	for i, n := range narrow {
		reversed[len(narrow)-1-i] = n
	}
	return [][]string{nil /* SELECT * */, {"ID"}, narrow, reversed}
}

// joinProjections are the projection variants for a JOIN query. Every entry
// is a qualified column ("L.ID"); the renderer emits a unique alias per
// entry so no two output columns share a name.
func (c *Case) joinProjections() [][]string {
	var wide []string
	for _, side := range []string{"L", "R"} {
		wide = append(wide, side+".ID")
		for _, col := range c.Table.Cols {
			wide = append(wide, side+"."+col.Name)
		}
	}
	return [][]string{
		wide,
		{"L.ID", "R.ID"},
		{"R.ID", "L.ID", "L.B"},
	}
}

// ProjectionsFor returns the projection variants appropriate to the query
// (join queries never use `SELECT *` — see JoinSpec; an aggregate's output
// list is fixed by its own shape).
func (c *Case) ProjectionsFor(q Query) [][]string {
	switch {
	case q.Agg != nil:
		return [][]string{nil}
	case q.Join != nil:
		return c.joinProjections()
	}
	return c.Projections()
}

// aggOutputCols is the aggregate query's output column list, in order:
// the grouping key (when present) followed by the aggregate, both aliased.
func aggOutputCols(a *AggSpec) []string {
	cols := make([]string, 0, len(a.GroupBy)+1)
	for i := range a.GroupBy {
		cols = append(cols, groupKeyCol(i))
	}
	return append(cols, "AGG")
}

// genExists builds a correlated [NOT] EXISTS: correlation on a BIGINT column
// plus, half the time, a simple `col op literal` filter on the inner row. The
// inner Pred keeps Qual empty (it reads a plain-keyed inner row in the oracle)
// and is a bare column-vs-literal so both the SQL render (existsSQL) and
// evalLeaf handle it with no BETWEEN/IN/qualifier machinery.
func genExists(rng *rand.Rand) *ExistsSpec {
	bigints := []string{"A", "B", "C"}
	// Correlation op: mostly equi (the common semi-join the equi-join rules
	// target), but ~40% a non-equi correlation. A self-equi correlation always
	// self-matches (r == outer row), so a bare equi EXISTS is trivially true for
	// every non-NULL row; a non-equi one (r.c < t.c) does NOT self-match and
	// forces a genuine per-outer inner scan — the planner's non-equi semi-join
	// path, which the equi-join rules cannot short-circuit.
	corrOps := []predicates.ComparisonType{
		predicates.ComparisonEquals, predicates.ComparisonEquals,
		predicates.ComparisonEquals, // weight equi ~3/5
		predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
		predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
	}
	e := &ExistsSpec{
		CorrCol: bigints[rng.IntN(len(bigints))],
		CorrOp:  corrOps[rng.IntN(len(corrOps))],
		Negated: rng.IntN(2) == 0,
	}
	if rng.IntN(2) == 0 {
		ops := []predicates.ComparisonType{
			predicates.ComparisonEquals, predicates.ComparisonNotEquals,
			predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
			predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
		}
		e.Inner = &Pred{
			Col: bigints[rng.IntN(len(bigints))],
			Op:  ops[rng.IntN(len(ops))],
			Lit: int64(rng.IntN(10)),
		}
	}
	return e
}

// existsSQL renders the correlated [NOT] EXISTS clause. tbl is the outer table
// name (also the inner table); the inner aliases it `r` and correlates against
// the unqualified outer `tbl.<col>`.
func existsSQL(e *ExistsSpec, tbl string) string {
	var b strings.Builder
	if e.Negated {
		b.WriteString("NOT ")
	}
	fmt.Fprintf(&b, "EXISTS (SELECT 1 FROM %s AS r WHERE r.%s %s %s.%s",
		tbl, strings.ToLower(e.CorrCol), opSQL(e.CorrOp), tbl, strings.ToLower(e.CorrCol))
	if e.Inner != nil {
		fmt.Fprintf(&b, " AND r.%s %s %s",
			strings.ToLower(e.Inner.Col), opSQL(e.Inner.Op), renderLiteral(e.Inner.Lit))
	}
	b.WriteString(")")
	return b.String()
}

// genScalarSub builds a non-correlated aggregate scalar subquery comparison.
// The outer and aggregated columns are BIGINTs; ~half the time an inner filter
// (often selective enough to leave the subquery empty) exercises the
// MIN/MAX-returns-NULL path.
func genScalarSub(rng *rand.Rand) *ScalarSubSpec {
	bigints := []string{"A", "B", "C"}
	ops := []predicates.ComparisonType{
		predicates.ComparisonEquals, predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
		predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
	}
	s := &ScalarSubSpec{
		OuterCol: bigints[rng.IntN(len(bigints))],
		Op:       ops[rng.IntN(len(ops))],
	}
	switch rng.IntN(4) {
	case 0:
		s.Func = AggCountStar
	case 1:
		s.Func, s.Col = AggCountCol, bigints[rng.IntN(len(bigints))]
	case 2:
		s.Func, s.Col = AggMin, bigints[rng.IntN(len(bigints))]
	default:
		s.Func, s.Col = AggMax, bigints[rng.IntN(len(bigints))]
	}
	if rng.IntN(2) == 0 {
		s.Filter = &Pred{
			Col: bigints[rng.IntN(len(bigints))],
			Op:  ops[rng.IntN(len(ops))],
			Lit: int64(rng.IntN(10)),
		}
	}
	return s
}

// scalarSubSQL renders `<outerCol> <op> (SELECT <agg> FROM tbl [WHERE <filter>])`.
func scalarSubSQL(s *ScalarSubSpec, tbl string) string {
	inner := &AggSpec{Func: s.Func, Col: s.Col}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (SELECT %s FROM %s",
		strings.ToLower(s.OuterCol), opSQL(s.Op), aggExprSQL(inner), tbl)
	if s.Filter != nil {
		fmt.Fprintf(&b, " WHERE %s %s %s",
			strings.ToLower(s.Filter.Col), opSQL(s.Filter.Op), renderLiteral(s.Filter.Lit))
	}
	b.WriteString(")")
	return b.String()
}

// genAggQuery builds `SELECT [g,] <agg> FROM t [WHERE …] [GROUP BY g]
// [HAVING …]`. The WHERE is biased to indexed columns so the aggregate can
// sit over an index access — the shape where a residual filter dropped by
// the aggregate data-access path produced silently wrong sums.
func genAggQuery(rng *rand.Rand, table TableDef) Query {
	indexed := map[string]bool{}
	for _, idx := range table.Indexes {
		for _, c := range idx.Cols {
			indexed[c] = true
		}
	}
	bigints := []string{"A", "B", "C"}

	spec := &AggSpec{}
	switch rng.IntN(5) {
	case 0:
		spec.Func = AggCountStar
	case 1:
		spec.Func, spec.Col = AggCountCol, bigints[rng.IntN(len(bigints))]
	case 2:
		spec.Func, spec.Col = AggSum, bigints[rng.IntN(len(bigints))]
	case 3:
		spec.Func, spec.Col = AggMin, bigints[rng.IntN(len(bigints))]
	default:
		spec.Func, spec.Col = AggMax, bigints[rng.IntN(len(bigints))]
	}
	// GROUP BY two thirds of the time; prefer an indexed key so the
	// aggregate-index / streaming-aggregation paths are reachable.
	if rng.IntN(3) != 0 {
		keys := []string{}
		for _, col := range table.Cols {
			if col.Type == ColBigint && indexed[col.Name] {
				keys = append(keys, col.Name)
			}
		}
		if len(keys) == 0 {
			keys = bigints
		}
		// One grouping key, or two distinct keys ~1/3 of the time — the
		// multi-column grouping path (composite-key equality and a NULL in
		// either key column forming its own group).
		first := keys[rng.IntN(len(keys))]
		spec.GroupBy = []string{first}
		if len(keys) > 1 && rng.IntN(3) == 0 {
			others := make([]string, 0, len(keys)-1)
			for _, k := range keys {
				if k != first {
					others = append(others, k)
				}
			}
			spec.GroupBy = append(spec.GroupBy, others[rng.IntN(len(others))])
		}
	}

	q := Query{Agg: spec}
	// A WHERE over an INDEXED column plus (often) an unindexed residual —
	// the AGG-RESIDUAL shape.
	if rng.IntN(3) != 0 {
		var leaves []*Pred
		for _, col := range table.Cols {
			if indexed[col.Name] && col.Type == ColBigint {
				leaves = append(leaves, &Pred{
					Col: col.Name, Op: predicates.ComparisonEquals, Lit: int64(rng.IntN(6)),
				})
				break
			}
		}
		if len(leaves) > 0 && rng.IntN(2) == 0 {
			leaves = append(leaves, &Pred{
				Col: "S", Op: predicates.ComparisonEquals,
				Lit: []string{"alpha", "beta", "gamma"}[rng.IntN(3)],
			})
		}
		switch len(leaves) {
		case 0:
		case 1:
			q.Where = &BoolNode{Leaf: leaves[0]}
		default:
			kids := make([]*BoolNode, len(leaves))
			for i, l := range leaves {
				kids[i] = &BoolNode{Leaf: l}
			}
			q.Where = &BoolNode{And: true, Kids: kids}
		}
	}
	// HAVING on the aggregate result, only for grouped queries.
	if len(spec.GroupBy) > 0 && rng.IntN(3) == 0 {
		spec.HavingOn = true
		spec.Having = &Pred{
			Op:  []predicates.ComparisonType{predicates.ComparisonGreaterThan, predicates.ComparisonLessThanOrEq}[rng.IntN(2)],
			Lit: int64(rng.IntN(4)),
		}
	}
	return q
}

// Generate builds the Case for a seed. Same seed → identical Case.
func Generate(seed uint64) *Case {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))

	table := genTable(rng)
	rows := genRows(rng, table)
	nQueries := 4 + rng.IntN(5)
	queries := make([]Query, 0, nQueries)
	for i := 0; i < nQueries; i++ {
		// ~1/4 self-joins: the join planner is a large surface and the
		// oracle stays a plain nested loop over the same authoritative rows.
		switch {
		case rng.IntN(4) == 0:
			queries = append(queries, genJoinQuery(rng, table))
			continue
		case rng.IntN(4) == 0:
			// Aggregates: the surface where a dropped residual produced
			// silently wrong SUMs (the AGG-RESIDUAL class).
			queries = append(queries, genAggQuery(rng, table))
			continue
		}
		queries = append(queries, genQuery(rng, table))
	}
	return &Case{Seed: seed, Table: table, Rows: rows, Queries: queries}
}

func genTable(rng *rand.Rand) TableDef {
	// Fixed column pool: A,B,C BIGINT (B NOT NULL so it is sortable in P1),
	// S STRING, F BOOLEAN. Small and stable so index/predicate interplay —
	// not schema exotica — is what varies.
	cols := []ColumnDef{
		{Name: "A", Type: ColBigint},
		{Name: "B", Type: ColBigint, NotNull: true},
		{Name: "C", Type: ColBigint},
		{Name: "S", Type: ColString},
		{Name: "F", Type: ColBoolean},
	}

	pool := []IndexDef{
		{Name: "IDX_A", Cols: []string{"A"}},
		{Name: "IDX_B", Cols: []string{"B"}},
		{Name: "IDX_C", Cols: []string{"C"}},
		{Name: "IDX_S", Cols: []string{"S"}},
		{Name: "IDX_AB", Cols: []string{"A", "B"}},
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	// ≥2 indexes with high probability: that is where intersections and
	// cost ties live. 0-1 indexes still occur so full scans stay covered.
	nIdx := 2 + rng.IntN(3)
	if rng.IntN(10) == 0 {
		nIdx = rng.IntN(2)
	}
	idxs := make([]IndexDef, nIdx)
	copy(idxs, pool[:nIdx])

	return TableDef{Name: "T_RD", Cols: cols, Indexes: idxs}
}

func genRows(rng *rand.Rand, table TableDef) []Row {
	n := 20 + rng.IntN(101)
	rows := make([]Row, 0, n)
	stringDomain := []string{"", "alpha", "beta", "gamma", "delta", "epsilon"}
	for i := 0; i < n; i++ {
		r := Row{"ID": int64(i + 1)}
		for _, col := range table.Cols {
			if !col.NotNull && rng.IntN(6) == 0 {
				r[col.Name] = nil
				continue
			}
			switch col.Type {
			case ColBigint:
				// Domain 0..9 → heavy duplicates → intersections do real work,
				// plus boundary values at low probability.
				switch rng.IntN(20) {
				case 0:
					r[col.Name] = int64(-1)
				case 1:
					r[col.Name] = int64(1) << 62
				default:
					r[col.Name] = int64(rng.IntN(10))
				}
			case ColString:
				r[col.Name] = stringDomain[rng.IntN(len(stringDomain))]
			case ColBoolean:
				r[col.Name] = rng.IntN(2) == 0
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// genJoinQuery builds a self-join: `t AS l JOIN t AS r ON l.X = r.Y`, plus a
// small conjunction of qualified single-sided predicates. Both operands of
// the join key are drawn from the indexed columns where possible so the
// planner has a correlated-access option, not only a nested-loop scan.
func genJoinQuery(rng *rand.Rand, table TableDef) Query {
	indexed := map[string]bool{}
	for _, idx := range table.Indexes {
		for _, c := range idx.Cols {
			indexed[c] = true
		}
	}
	// Join key: both sides must be the SAME TYPE — the engine rejects a
	// cross-type comparison with 42804, which is correct behaviour, so
	// generating one tests nothing but the error path. BIGINT columns only
	// (ID plus the numeric value columns): they are the join-key shape real
	// schemas use, and STRING/BOOLEAN keys add no planner coverage.
	// Prefer an indexed column on the right (the probed side) so a
	// correlated index access is available; ID is always indexed (pk).
	rightCandidates := []string{"ID"}
	for _, col := range table.Cols {
		if col.Type == ColBigint && indexed[col.Name] {
			rightCandidates = append(rightCandidates, col.Name)
		}
	}
	leftCandidates := []string{"ID"}
	for _, col := range table.Cols {
		if col.Type == ColBigint {
			leftCandidates = append(leftCandidates, col.Name)
		}
	}

	spec := &JoinSpec{
		LeftCol:  leftCandidates[rng.IntN(len(leftCandidates))],
		RightCol: rightCandidates[rng.IntN(len(rightCandidates))],
	}
	// Join syntax/type: comma cross-join, INNER … ON, or LEFT OUTER … ON
	// (each ~1/3). LEFT OUTER needs the ON form, so it sets Inner too.
	switch rng.IntN(3) {
	case 0:
		spec.Inner = false // comma cross-join (equality moves to WHERE)
	case 1:
		spec.Inner = true // INNER … ON
	default:
		spec.Inner, spec.LeftOuter = true, true // LEFT OUTER … ON
	}

	// 1-2 qualified single-sided filter leaves, biased to indexed columns so
	// each side can drive an index access.
	var leaves []*Pred
	n := 1 + rng.IntN(2)
	for i := 0; i < n; i++ {
		col := table.Cols[rng.IntN(len(table.Cols))]
		p := genPred(rng, col, indexed[col.Name])
		p.Qual = []string{"L", "R"}[rng.IntN(2)]
		leaves = append(leaves, p)
	}

	// A CROSS-SIDE column comparison (`l.a > r.b`) — a theta/non-equi
	// residual on top of the join equality, which the single-sided leaves
	// above can never produce. BIGINT columns only, so the two operands stay
	// type-compatible.
	if rng.IntN(3) == 0 {
		bigints := []string{"A", "B", "C"}
		ops := []predicates.ComparisonType{
			predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
			predicates.ComparisonNotEquals, predicates.ComparisonLessThanOrEq,
		}
		leaves = append(leaves, &Pred{
			Col: bigints[rng.IntN(len(bigints))], Qual: "L",
			Op:     ops[rng.IntN(len(ops))],
			RhsCol: bigints[rng.IntN(len(bigints))], RhsQual: "R",
		})
	}

	kids := make([]*BoolNode, len(leaves))
	for i, l := range leaves {
		kids[i] = &BoolNode{Leaf: l}
	}
	var where *BoolNode
	if len(kids) == 1 {
		where = kids[0]
	} else {
		where = &BoolNode{And: true, Kids: kids}
	}

	q := Query{Join: spec, Where: where}
	// A total ORDER BY over both sides' ids keeps the ordered comparison exact.
	if rng.IntN(2) == 0 {
		q.OrderBy = []OrderKey{
			{Col: "ID", Qual: "L", Desc: rng.IntN(2) == 0},
			{Col: "ID", Qual: "R"},
		}
		if rng.IntN(3) == 0 {
			q.Limit = 1 + rng.IntN(20)
		}
	} else if rng.IntN(4) == 0 {
		// UNORDERED join + LIMIT — the exact shape that exposed the dropped
		// projection aliases. Without this arm the generator could never
		// regenerate its own finding; the comparator handles it via the
		// sub-multiset membership rule.
		q.Limit = 1 + rng.IntN(20)
	}
	return q
}

func genQuery(rng *rand.Rand, table TableDef) Query {
	indexed := map[string]bool{}
	for _, idx := range table.Indexes {
		for _, c := range idx.Cols {
			indexed[c] = true
		}
	}
	var indexedCols, unindexedCols []ColumnDef
	for _, c := range table.Cols {
		if indexed[c.Name] {
			indexedCols = append(indexedCols, c)
		} else {
			unindexedCols = append(unindexedCols, c)
		}
	}

	// The killer-cell bias: with ≥2 indexed columns available, half of all
	// queries take ≥2 indexed equality/range leaves PLUS one unindexed
	// residual leaf.
	var leaves []*Pred
	if len(indexedCols) >= 2 && rng.IntN(2) == 0 {
		perm := rng.Perm(len(indexedCols))
		nIdxLeaves := 2 + rng.IntN(len(indexedCols)-1)
		if nIdxLeaves > len(indexedCols) {
			nIdxLeaves = len(indexedCols)
		}
		for _, pi := range perm[:nIdxLeaves] {
			leaves = append(leaves, genPred(rng, indexedCols[pi], true))
		}
		if len(unindexedCols) > 0 && rng.IntN(4) != 0 {
			leaves = append(leaves, genPred(rng, unindexedCols[rng.IntN(len(unindexedCols))], false))
		}
	} else {
		nLeaves := 1 + rng.IntN(3)
		for i := 0; i < nLeaves; i++ {
			col := table.Cols[rng.IntN(len(table.Cols))]
			leaves = append(leaves, genPred(rng, col, indexed[col.Name]))
		}
	}

	// Column-vs-column leaf (~1/7): non-sargable, so it forces a residual
	// filter and reaches plan shapes literal comparisons never do. Same-typed
	// columns only — cross-type comparison semantics are their own axis.
	if rng.IntN(7) == 0 {
		bigints := []string{"A", "B", "C"}
		l := bigints[rng.IntN(len(bigints))]
		r := bigints[rng.IntN(len(bigints))]
		if l != r {
			ops := []predicates.ComparisonType{
				predicates.ComparisonEquals, predicates.ComparisonNotEquals,
				predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
				predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
			}
			leaves = append(leaves, &Pred{Col: l, Op: ops[rng.IntN(len(ops))], RhsCol: r})
		}
	}

	// NOT on a leaf (~1/8 each): `NOT (a = 1)` / `a NOT IN (…)`.
	for _, lf := range leaves {
		if rng.IntN(8) == 0 {
			lf.Negated = true
		}
	}

	var where *BoolNode
	if len(leaves) == 1 {
		where = &BoolNode{Leaf: leaves[0]}
	} else if rng.IntN(4) != 0 {
		// AND of all leaves (the majority — conjunctive index planning).
		kids := make([]*BoolNode, len(leaves))
		for i, l := range leaves {
			kids[i] = &BoolNode{Leaf: l}
		}
		where = &BoolNode{And: true, Kids: kids}
	} else {
		// One OR splitting the leaves into two conjuncts.
		split := 1 + rng.IntN(len(leaves)-1)
		left := make([]*BoolNode, 0, split)
		for _, l := range leaves[:split] {
			left = append(left, &BoolNode{Leaf: l})
		}
		right := make([]*BoolNode, 0, len(leaves)-split)
		for _, l := range leaves[split:] {
			right = append(right, &BoolNode{Leaf: l})
		}
		wrap := func(kids []*BoolNode) *BoolNode {
			if len(kids) == 1 {
				return kids[0]
			}
			return &BoolNode{And: true, Kids: kids}
		}
		where = &BoolNode{And: false, Kids: []*BoolNode{wrap(left), wrap(right)}}
	}

	var orderBy []OrderKey
	switch rng.IntN(4) {
	case 0:
		orderBy = []OrderKey{{Col: "ID", Desc: rng.IntN(2) == 0}}
	case 1:
		// B is the NOT NULL sortable value column; append ID for a total
		// order so the oracle's expected sequence is unique.
		orderBy = []OrderKey{{Col: "B", Desc: rng.IntN(2) == 0}, {Col: "ID"}}
	case 2:
		// NULLABLE sort key (A, C, or S) with a NULLS placement drawn from
		// {default, FIRST, LAST}; ID suffix keeps the total order unique.
		col := []string{"A", "C", "S"}[rng.IntN(3)]
		orderBy = []OrderKey{
			{Col: col, Desc: rng.IntN(2) == 0, Nulls: NullsPlacement(rng.IntN(3))},
			{Col: "ID"},
		}
	}

	q := Query{Where: where, OrderBy: orderBy}

	// Correlated [NOT] EXISTS on ~1/4 of plain queries: a rich wrong-rows
	// surface (semi-join, decorrelation, correlation binding) the generator
	// otherwise never reaches.
	if rng.IntN(4) == 0 {
		q.Exists = genExists(rng)
	}

	// Non-correlated aggregate scalar subquery comparison on ~1/5 of plain
	// queries — a Go-only read extension (no Java equivalent) whose scalar
	// evaluation and NULL-when-empty semantics need their own coverage.
	if rng.IntN(5) == 0 {
		q.ScalarSub = genScalarSub(rng)
	}

	// DISTINCT on ~1/6 of queries. ORDER BY is dropped for DISTINCT — the
	// projection variants do not all contain every sort key, and SQL
	// requires DISTINCT's ORDER BY ⊆ projection; the unordered multiset
	// comparison stays exact.
	if rng.IntN(6) == 0 {
		q.Distinct = true
		q.OrderBy = nil
	}

	// LIMIT on ~1/4 of queries: small values exercise the boundary, large
	// values exercise the |M| < k clamp.
	if rng.IntN(4) == 0 {
		q.Limit = 1 + rng.IntN(30)
		// OFFSET only with a total ORDER BY: without one the skipped prefix
		// is implementation-defined and the result is not a checkable set.
		if len(q.OrderBy) > 0 && rng.IntN(3) == 0 {
			q.Offset = rng.IntN(5)
		}
	}

	// NOT around the whole WHERE (~1/10) — exercises the negation
	// normalization (DeMorgan) the planner applies before matching.
	if q.Where != nil && rng.IntN(10) == 0 {
		q.Where = &BoolNode{Not: true, And: true, Kids: []*BoolNode{q.Where}}
	}

	return q
}

func genPred(rng *rand.Rand, col ColumnDef, indexBiased bool) *Pred {
	// NULL-comparison leaves at low probability on nullable columns.
	if !col.NotNull && rng.IntN(10) == 0 {
		op := predicates.ComparisonIsNull
		if rng.IntN(2) == 0 {
			op = predicates.ComparisonIsNotNull
		}
		return &Pred{Col: col.Name, Op: op}
	}

	// P2 leaf kinds: IN (~1/8), BETWEEN (~1/10, BIGINT), LIKE (~1/6, STRING).
	switch {
	case col.Type != ColBoolean && rng.IntN(8) == 0:
		n := 2 + rng.IntN(3)
		list := make([]any, 0, n)
		for i := 0; i < n; i++ {
			if col.Type == ColBigint {
				list = append(list, int64(rng.IntN(12)))
			} else {
				list = append(list, []string{"", "alpha", "beta", "gamma", "delta", "zeta"}[rng.IntN(6)])
			}
		}
		return &Pred{Col: col.Name, Op: predicates.ComparisonIn, InList: list}
	case col.Type == ColBigint && rng.IntN(10) == 0:
		lo := int64(rng.IntN(10)) - 1
		hi := lo + int64(rng.IntN(6))
		return &Pred{Col: col.Name, Op: predicates.ComparisonGreaterThanEq, Lit: lo, BetweenHi: hi, IsBetween: true}
	case col.Type == ColString && rng.IntN(6) == 0:
		pat := []string{"%a%", "al%", "%ta", "_eta", "%e%a%", "alpha", "%"}[rng.IntN(7)]
		return &Pred{Col: col.Name, Op: predicates.ComparisonLike, Lit: pat}
	case col.Type == ColBigint && rng.IntN(9) == 0:
		// Arithmetic LHS: (col - col2) <cmp> lit. Subtraction only (no overflow
		// over the value domain), so no 22003 path; a NULL operand → UNKNOWN.
		bigints := []string{"A", "B", "C"}
		cmp := []predicates.ComparisonType{
			predicates.ComparisonEquals, predicates.ComparisonNotEquals,
			predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
			predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
		}
		return &Pred{
			Col:       col.Name,
			HasArith:  true,
			ArithOp:   values.OpSub,
			ArithCol2: bigints[rng.IntN(len(bigints))],
			Op:        cmp[rng.IntN(len(cmp))],
			Lit:       int64(rng.IntN(12)) - 2, // -2..9: differences straddle the literal
		}
	}

	var ops []predicates.ComparisonType
	if indexBiased && rng.IntN(3) != 0 {
		// Equality dominates on indexed columns — the sargable sweet spot.
		ops = []predicates.ComparisonType{predicates.ComparisonEquals}
	} else {
		ops = []predicates.ComparisonType{
			predicates.ComparisonEquals,
			predicates.ComparisonNotEquals,
			predicates.ComparisonLessThan,
			predicates.ComparisonLessThanOrEq,
			predicates.ComparisonGreaterThan,
			predicates.ComparisonGreaterThanEq,
		}
	}
	op := ops[rng.IntN(len(ops))]

	var lit any
	switch col.Type {
	case ColBigint:
		lit = int64(rng.IntN(10))
	case ColString:
		lit = []string{"", "alpha", "beta", "gamma", "delta"}[rng.IntN(5)]
	case ColBoolean:
		// Only =/<> make sense; force equality.
		op = predicates.ComparisonEquals
		lit = rng.IntN(2) == 0
	}
	return &Pred{Col: col.Name, Op: op, Lit: lit}
}

// --- SQL rendering ---------------------------------------------------------

// DDL renders the schema-template body (CREATE TABLE + CREATE INDEX lines).
func (c *Case) DDL() string {
	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	b.WriteString(c.Table.Name)
	b.WriteString(" (id BIGINT NOT NULL")
	for _, col := range c.Table.Cols {
		b.WriteString(", ")
		b.WriteString(strings.ToLower(col.Name))
		switch col.Type {
		case ColBigint:
			b.WriteString(" BIGINT")
		case ColString:
			b.WriteString(" STRING")
		case ColBoolean:
			b.WriteString(" BOOLEAN")
		}
	}
	b.WriteString(", PRIMARY KEY (id))")
	for _, idx := range c.Table.Indexes {
		fmt.Fprintf(&b, " CREATE INDEX %s ON %s (%s)",
			strings.ToLower(idx.Name), c.Table.Name, strings.ToLower(strings.Join(idx.Cols, ", ")))
	}
	return b.String()
}

// InsertSQL renders one multi-row INSERT for the dataset.
func (c *Case) InsertSQL() string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(c.Table.Name)
	b.WriteString(" VALUES ")
	for i, r := range c.Rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(")
		b.WriteString(renderLiteral(r["ID"]))
		for _, col := range c.Table.Cols {
			b.WriteString(", ")
			b.WriteString(renderLiteral(r[col.Name]))
		}
		b.WriteString(")")
	}
	return b.String()
}

// SQL renders one query under a projection (nil = SELECT *).
func (c *Case) SQL(q Query, projection []string) string {
	if q.Agg != nil {
		return c.aggSQL(q)
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	if q.Distinct {
		b.WriteString("DISTINCT ")
	}
	if projection == nil {
		b.WriteString("*")
	} else if q.Join != nil {
		// Qualified columns, each with a unique alias (l_id, r_a, …) so no
		// two output columns share a name.
		outs := make([]string, len(projection))
		for i, p := range projection {
			outs[i] = fmt.Sprintf("%s AS %s", strings.ToLower(p), joinOutputAlias(p))
		}
		b.WriteString(strings.Join(outs, ", "))
	} else {
		lower := make([]string, len(projection))
		for i, p := range projection {
			lower[i] = strings.ToLower(p)
		}
		b.WriteString(strings.Join(lower, ", "))
	}
	b.WriteString(" FROM ")
	tbl := strings.ToLower(c.Table.Name)
	switch {
	case q.Join == nil:
		b.WriteString(tbl)
	case q.Join.LeftOuter:
		fmt.Fprintf(&b, "%s AS l LEFT JOIN %s AS r ON l.%s = r.%s",
			tbl, tbl, strings.ToLower(q.Join.LeftCol), strings.ToLower(q.Join.RightCol))
	case q.Join.Inner:
		fmt.Fprintf(&b, "%s AS l JOIN %s AS r ON l.%s = r.%s",
			tbl, tbl, strings.ToLower(q.Join.LeftCol), strings.ToLower(q.Join.RightCol))
	default:
		// Comma cross-join; the equality moves into the WHERE below.
		fmt.Fprintf(&b, "%s AS l, %s AS r", tbl, tbl)
	}
	// WHERE: the comma-join form carries the join equality as its first
	// conjunct, ahead of any generated filters.
	joinEq := ""
	if q.Join != nil && !q.Join.Inner {
		joinEq = fmt.Sprintf("l.%s = r.%s",
			strings.ToLower(q.Join.LeftCol), strings.ToLower(q.Join.RightCol))
	}
	// Build the WHERE as a list of top-level AND conjuncts: the join equality
	// (comma-join queries), the boolean predicate tree, and the appended
	// subquery filters (EXISTS, scalar comparison). Each appended subquery is a
	// separate top-level conjunct in the oracle, so when one follows the
	// predicate tree the tree is parenthesized — otherwise a top-level OR in it
	// would capture the trailing `AND <subquery>` (AND binds tighter than OR),
	// a rendering-only divergence.
	{
		var conj []string
		if joinEq != "" {
			conj = append(conj, joinEq)
		}
		if q.Where != nil {
			var wb strings.Builder
			renderBool(&wb, q.Where)
			w := wb.String()
			if q.Exists != nil || q.ScalarSub != nil {
				w = "(" + w + ")"
			}
			conj = append(conj, w)
		}
		if q.Exists != nil {
			conj = append(conj, existsSQL(q.Exists, tbl))
		}
		if q.ScalarSub != nil {
			conj = append(conj, scalarSubSQL(q.ScalarSub, tbl))
		}
		if len(conj) > 0 {
			b.WriteString(" WHERE ")
			b.WriteString(strings.Join(conj, " AND "))
		}
	}
	if len(q.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, k := range q.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			if k.Qual != "" {
				b.WriteString(strings.ToLower(k.Qual) + ".")
			}
			b.WriteString(strings.ToLower(k.Col))
			if k.Desc {
				b.WriteString(" DESC")
			}
			switch k.Nulls {
			case NullsFirst:
				b.WriteString(" NULLS FIRST")
			case NullsLast:
				b.WriteString(" NULLS LAST")
			}
		}
	}
	if q.Limit > 0 {
		fmt.Fprintf(&b, " LIMIT %d", q.Limit)
		if q.Offset > 0 {
			fmt.Fprintf(&b, " OFFSET %d", q.Offset)
		}
	}
	return b.String()
}

// aggExprSQL renders the aggregate call.
func aggExprSQL(a *AggSpec) string {
	col := strings.ToLower(a.Col)
	switch a.Func {
	case AggCountStar:
		return "COUNT(*)"
	case AggCountCol:
		return "COUNT(" + col + ")"
	case AggSum:
		return "SUM(" + col + ")"
	case AggMin:
		return "MIN(" + col + ")"
	default:
		return "MAX(" + col + ")"
	}
}

// aggSQL renders an aggregate query. Both output columns are aliased so the
// harness's name-keyed rows have stable, unique keys regardless of how the
// engine spells a computed column.
func (c *Case) aggSQL(q Query) string {
	a := q.Agg
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, key := range a.GroupBy {
		// Alias each key to its output column (g, g1, g2, …) so the harness's
		// name-keyed rows line up with aggOutputCols / the oracle.
		fmt.Fprintf(&b, "%s AS %s, ", strings.ToLower(key), strings.ToLower(groupKeyCol(i)))
	}
	fmt.Fprintf(&b, "%s AS agg FROM %s", aggExprSQL(a), strings.ToLower(c.Table.Name))
	if q.Where != nil {
		b.WriteString(" WHERE ")
		renderBool(&b, q.Where)
	}
	if len(a.GroupBy) > 0 {
		lowered := make([]string, len(a.GroupBy))
		for i, key := range a.GroupBy {
			lowered[i] = strings.ToLower(key)
		}
		fmt.Fprintf(&b, " GROUP BY %s", strings.Join(lowered, ", "))
		if a.HavingOn && a.Having != nil {
			fmt.Fprintf(&b, " HAVING %s %s %s", aggExprSQL(a), opSQL(a.Having.Op), renderLiteral(a.Having.Lit))
		}
	}
	return b.String()
}

// joinOutputAlias turns a qualified column ("L.ID") into a unique output
// alias ("l_id").
func joinOutputAlias(qualified string) string {
	return strings.ToLower(strings.ReplaceAll(qualified, ".", "_"))
}

func renderBool(b *strings.Builder, n *BoolNode) {
	if n.Not {
		b.WriteString("NOT (")
		defer b.WriteString(")")
	}
	if n.Leaf != nil {
		p := n.Leaf
		col := strings.ToLower(p.Col)
		if p.Qual != "" {
			col = strings.ToLower(p.Qual) + "." + col
		}
		// NOT IN renders inline; every other negated leaf wraps in NOT (…).
		negWrap := p.Negated && p.Op != predicates.ComparisonIn
		if negWrap {
			b.WriteString("NOT (")
		}
		switch {
		case p.HasArith:
			col2 := strings.ToLower(p.ArithCol2)
			if p.Qual != "" {
				col2 = strings.ToLower(p.Qual) + "." + col2
			}
			fmt.Fprintf(b, "(%s %s %s) %s %s", col, arithSQL(p.ArithOp), col2, opSQL(p.Op), renderLiteral(p.Lit))
		case p.IsBetween:
			fmt.Fprintf(b, "%s BETWEEN %s AND %s", col, renderLiteral(p.Lit), renderLiteral(p.BetweenHi))
		case p.Op == predicates.ComparisonIsNull:
			fmt.Fprintf(b, "%s IS NULL", col)
		case p.Op == predicates.ComparisonIsNotNull:
			fmt.Fprintf(b, "%s IS NOT NULL", col)
		case p.Op == predicates.ComparisonIn:
			lits := make([]string, len(p.InList))
			for i, v := range p.InList {
				lits[i] = renderLiteral(v)
			}
			kw := "IN"
			if p.Negated {
				kw = "NOT IN"
			}
			fmt.Fprintf(b, "%s %s (%s)", col, kw, strings.Join(lits, ", "))
		case p.Op == predicates.ComparisonLike:
			fmt.Fprintf(b, "%s LIKE %s", col, renderLiteral(p.Lit))
		case p.RhsCol != "":
			rhs := strings.ToLower(p.RhsCol)
			if p.RhsQual != "" {
				rhs = strings.ToLower(p.RhsQual) + "." + rhs
			}
			fmt.Fprintf(b, "%s %s %s", col, opSQL(p.Op), rhs)
		default:
			fmt.Fprintf(b, "%s %s %s", col, opSQL(p.Op), renderLiteral(p.Lit))
		}
		if negWrap {
			b.WriteString(")")
		}
		return
	}
	sep := " AND "
	if !n.And {
		sep = " OR "
	}
	for i, kid := range n.Kids {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString("(")
		renderBool(b, kid)
		b.WriteString(")")
	}
}

func opSQL(op predicates.ComparisonType) string {
	switch op {
	case predicates.ComparisonEquals:
		return "="
	case predicates.ComparisonNotEquals:
		return "<>"
	case predicates.ComparisonLessThan:
		return "<"
	case predicates.ComparisonLessThanOrEq:
		return "<="
	case predicates.ComparisonGreaterThan:
		return ">"
	case predicates.ComparisonGreaterThanEq:
		return ">="
	}
	panic(fmt.Sprintf("rowdiff: unrenderable comparison op %d", op))
}

// arithSQL renders an arithmetic operator. Only OpSub is generated today; the
// others are handled so a future extension (with overflow handling) is a
// one-line generator change, not a renderer change.
func arithSQL(op values.ArithmeticOp) string {
	switch op {
	case values.OpAdd:
		return "+"
	case values.OpSub:
		return "-"
	case values.OpMul:
		return "*"
	}
	panic(fmt.Sprintf("rowdiff: unrenderable arith op %d", op))
}

func renderLiteral(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return fmt.Sprintf("%d", t)
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	case bool:
		if t {
			return "TRUE"
		}
		return "FALSE"
	}
	panic(fmt.Sprintf("rowdiff: unrenderable literal %T", v))
}
