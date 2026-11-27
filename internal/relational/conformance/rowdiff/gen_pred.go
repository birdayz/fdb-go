// Portions derived from FoundationDB Record Layer (Type.java,
// SemanticAnalyzer.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rowdiff

import (
	"math/rand/v2"
	"strconv"
	"strings"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
)

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
	// Bitwise makes the leaf's LHS a bitwise expression `(Col <op> BitCol2)`
	// (op ∈ &,|,^; BitOp names the engine function BITAND/BITOR/BITXOR) instead
	// of a bare column: `(a & b) <Op> Lit`. Bitwise ops never overflow (unlike
	// +/*), so the value is order-independent and oracle-safe. A NULL in either
	// operand yields a NULL LHS → the comparison is UNKNOWN. The oracle folds it
	// through the engine's own ScalarFunctionValue, so the bitwise/NULL
	// semantics are shared — the leaf tests the PLANNER's handling of a bitwise
	// predicate expression (pushdown, index residual), not the eval.
	Bitwise bool
	BitOp   string // "BITAND" | "BITOR" | "BITXOR"
	BitCol2 string
	// Case, when non-nil, makes the leaf's LHS a searched CASE expression
	// `(CASE WHEN … THEN … ELSE … END) <Op> Lit` instead of a bare column.
	// Single-table only (unqualified columns).
	Case *CaseSpec
	// StrFn, when non-nil, makes the leaf's LHS a string-function call
	// `<Fn>(<StrCol>) <Op> Lit` (UPPER/LOWER vs a string Lit, LENGTH vs an int
	// Lit). Single-table only. A NULL column makes the result NULL → UNKNOWN.
	StrFn *StrFnSpec
	// NumFn, when non-nil, makes the leaf's LHS a numeric-function call
	// `ABS(col) <Op> Lit` or `MOD(col, k) <Op> Lit`. Single-table only. A NULL
	// column makes the result NULL → UNKNOWN.
	NumFn *NumFnSpec
	// Cast, when non-nil, makes the leaf's LHS a `CAST(col AS STRING) <Op> Lit`
	// over a BIGINT or BOOLEAN column. Single-table only. A NULL column makes
	// the result NULL → UNKNOWN.
	Cast *CastSpec
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

// CaseSpec is a searched CASE used as a predicate LHS:
//
//	(CASE WHEN <When> THEN <then-arm> ELSE <else-arm> END) <Op> <Lit>
//
// A single WHEN (the common shape). Branch selection is SQL's: the WHEN arm is
// taken only when its condition is TRUE — a FALSE or UNKNOWN (NULL-operand)
// condition falls through to ELSE. Each arm is a BIGINT column or literal; a
// selected NULL column makes the CASE result NULL, so the outer comparison is
// UNKNOWN. The condition and the outer comparison both go through the shared
// Comparison eval, so only the trivial branch-pick is restated in the oracle.
type CaseSpec struct {
	When    *Pred  // WHEN condition (col op literal; Qual empty)
	ThenCol string // THEN reads this column; "" ⇒ ThenLit
	ThenLit int64
	ElseCol string // ELSE reads this column; "" ⇒ ElseLit
	ElseLit int64
}

// StrFnKind is a scalar string function whose semantics are unambiguous over
// the generator's ASCII string domain (so the oracle's Go implementation and
// the engine agree by construction — verified by probe).
type StrFnKind int

const (
	StrFnUpper  StrFnKind = iota // UPPER(s) → string
	StrFnLower                   // LOWER(s) → string
	StrFnLength                  // LENGTH(s) → int (character count == byte count for ASCII)
	StrFnSubstr                  // SUBSTR(s, Start, Length) → string (1-based, clamped)
	StrFnConcat                  // CONCAT(s, 'Suffix') → string (NULL operand treated as "")
	StrFnTrim                    // TRIM(s) → string (strips leading/trailing spaces)
)

// StrFnSpec is a string-function predicate LHS: `<Fn>(<Col>) <Op> Lit`, or for
// SUBSTR `SUBSTR(<Col>, Start, Length) <Op> Lit`, or for CONCAT
// `CONCAT(<Col>, 'Suffix') <Op> Lit`.
type StrFnSpec struct {
	Fn     StrFnKind
	Col    string // the STRING column (only "S" today)
	Start  int64  // SUBSTR: 1-based start position (>=1)
	Length int64  // SUBSTR: length (>=0); the result is clamped to the string bounds
	Suffix string // CONCAT: the literal appended to the column
}

// NumFnKind is a scalar numeric function whose semantics match Go's over the
// generator's BIGINT domain (verified by probe): ABS never overflows (no
// MinInt64 in {-1, 2^62, 0..9}), and MOD follows the dividend's sign exactly as
// Go's % does. MOD's divisor is always a nonzero literal, so there is no
// division-by-zero path.
type NumFnKind int

const (
	NumFnAbs      NumFnKind = iota // ABS(col)
	NumFnMod                       // MOD(col, Mod)
	NumFnCoalesce                  // COALESCE(col, Default) — col's value, or Default when NULL
)

// NumFnSpec is a numeric-function predicate LHS: `ABS(col) <Op> Lit`,
// `MOD(col, Mod) <Op> Lit`, or `COALESCE(col, Default) <Op> Lit`.
type NumFnSpec struct {
	Fn      NumFnKind
	Col     string // BIGINT column
	Mod     int64  // nonzero divisor for MOD (unused otherwise)
	Default int64  // COALESCE fallback when the column is NULL (never NULL itself)
}

// CastSpec is a `CAST(Col AS STRING) <Op> Lit` predicate LHS over a BIGINT or
// BOOLEAN column. Only the STRING target is generated: int→string is Go's
// strconv.FormatInt and bool→string is "true"/"false" (both verified to match
// the engine over the domain). CAST(string AS BIGINT) is excluded — it raises a
// plan-order-dependent parse error on non-numeric input, which the full-scan
// oracle cannot model (the arithmetic-overflow constraint).
type CastSpec struct {
	Col     string // BIGINT or BOOLEAN column
	FromInt bool   // true = BIGINT source (FormatInt); false = BOOLEAN source ("true"/"false")
}

// genExists builds a correlated [NOT] EXISTS: correlation on a BIGINT column
// plus, half the time, a simple `col op literal` filter on the inner row. The
// inner Pred keeps Qual empty (it reads a plain-keyed inner row in the oracle)
// and is a bare column-vs-literal so both the SQL render (existsSQL) and
// evalLeaf handle it with no BETWEEN/IN/qualifier machinery.
func genExists(rng *rand.Rand, table TableDef) *ExistsSpec {
	bigints := bigintCols(table)
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

// genScalarSub builds a non-correlated aggregate scalar subquery comparison.
// The outer and aggregated columns are BIGINTs; ~half the time an inner filter
// (often selective enough to leave the subquery empty) exercises the
// MIN/MAX-returns-NULL path.
func genScalarSub(rng *rand.Rand, table TableDef) *ScalarSubSpec {
	bigints := bigintCols(table)
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

// floatColLit returns one predicate-position literal for a DOUBLE/FLOAT
// column. A quarter of the time it is a bare INTEGER literal (int64) —
// exactly the cross-WIDTH shape that silently matched ZERO rows against an
// indexed FLOAT/DOUBLE column before narrowConstAgainstFloatColumn /
// widenConstAgainstDoubleColumn (a same-typed float literal alone can never
// reach that code path, since nothing needs promoting). The rest is a
// float64 drawn from the row-generation domain, including the float32
// width-narrowing boundary (0.1) and the double-exactness boundary (2^53) —
// the same constants genRows seeds into the data, so a boundary predicate
// has a real, reachable row to match or exclude.
func floatColLit(rng *rand.Rand) any {
	switch rng.IntN(4) {
	case 0:
		return int64(rng.IntN(10))
	case 1:
		return 0.1
	case 2:
		return 9007199254740992.0 // 2^53
	default:
		return float64(rng.IntN(10))
	}
}

// floatListElem returns a float64 element for an ALL-FLOAT IN list (see
// floatColLit's doc comment for why a list may never mix int64 and float64
// elements): the same domain and boundary values as floatColLit minus the
// bare-integer branch.
func floatListElem(rng *rand.Rand) float64 {
	switch rng.IntN(3) {
	case 0:
		return 0.1
	case 1:
		return 9007199254740992.0 // 2^53
	default:
		return float64(rng.IntN(10))
	}
}

func genPred(rng *rand.Rand, table TableDef, col ColumnDef, indexBiased bool) *Pred {
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
		return genInPred(rng, col)
	case col.Type == ColBigint && rng.IntN(10) == 0:
		lo := int64(rng.IntN(10)) - 1
		hi := lo + int64(rng.IntN(6))
		return &Pred{Col: col.Name, Op: predicates.ComparisonGreaterThanEq, Lit: lo, BetweenHi: hi, IsBetween: true}
	case (col.Type == ColDouble || col.Type == ColFloat) && rng.IntN(10) == 0:
		lo := float64(rng.IntN(8))
		hi := lo + float64(1+rng.IntN(4))
		return &Pred{Col: col.Name, Op: predicates.ComparisonGreaterThanEq, Lit: lo, BetweenHi: hi, IsBetween: true}
	case col.Type == ColString && rng.IntN(6) == 0:
		pat := []string{"%a%", "al%", "%ta", "_eta", "%e%a%", "alpha", "%"}[rng.IntN(7)]
		return &Pred{Col: col.Name, Op: predicates.ComparisonLike, Lit: pat}
	case col.Type == ColBigint && rng.IntN(9) == 0:
		return genArithPred(rng, table, col)
	case col.Type == ColBigint && rng.IntN(8) == 0:
		return genBitwisePred(rng, table, col)
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
	case ColDouble, ColFloat:
		lit = floatColLit(rng)
	}
	return &Pred{Col: col.Name, Op: op, Lit: lit}
}

func genInPred(rng *rand.Rand, col ColumnDef) *Pred {
	n := 2 + rng.IntN(3)
	list := make([]any, 0, n)
	// A DOUBLE/FLOAT column's list is ALL-INTEGER or ALL-FLOAT for the
	// WHOLE list, decided once — never per element. Java's constant-array
	// type unification (SemanticAnalyzer.resolveArrayTypeFromElementTypes,
	// ported at expr.go's inElemClass) requires every non-NULL IN-list
	// element to be the SAME class (LONG vs DOUBLE are different classes),
	// rejecting a mixed list with 42804 by DESIGN — so `d IN (1, 2.5)` is
	// invalid SQL, not a shape this harness should generate. An ALL-INTEGER
	// list against a DOUBLE/FLOAT column still exercises the intended
	// per-element ResolveIn narrowing (narrowToFloat32/widenIntToDouble),
	// since every element gets the SAME int64->float treatment.
	intList := col.Type == ColDouble || col.Type == ColFloat
	if intList {
		intList = rng.IntN(2) == 0
	}
	for i := 0; i < n; i++ {
		switch col.Type {
		case ColBigint:
			list = append(list, int64(rng.IntN(12)))
		case ColDouble, ColFloat:
			if intList {
				list = append(list, int64(rng.IntN(10)))
			} else {
				list = append(list, floatListElem(rng))
			}
		default:
			list = append(list, []string{"", "alpha", "beta", "gamma", "delta", "zeta"}[rng.IntN(6)])
		}
	}
	return &Pred{Col: col.Name, Op: predicates.ComparisonIn, InList: list}
}

func genArithPred(rng *rand.Rand, table TableDef, col ColumnDef) *Pred {
	// Arithmetic LHS: (col - col2) <cmp> lit. SUBTRACTION ONLY, and not
	// merely because it can't overflow over the value domain — +/* are
	// fundamentally out of scope for this differential. Overflow inside a
	// predicate is evaluated per row, so whether it raises 22003 depends on
	// the PLAN's evaluation order (a filter-first plan never computes the
	// arithmetic for rows it excludes — verified plan-stable in
	// TestFDB_ArithOverflowInPredicate_PlanStable). The oracle is a
	// full-scan that evaluates the arithmetic for EVERY row, so on
	// `g=20 AND (a+b)>0` it would overflow while a filter-first engine plan
	// returns rows — a false divergence. The oracle deliberately does not
	// model the planner, so it cannot replicate that order; subtraction (no
	// overflow, order-independent) is the only arithmetic it can check.
	bigints := bigintCols(table)
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

func genBitwisePred(rng *rand.Rand, table TableDef, col ColumnDef) *Pred {
	// Bitwise LHS: (col <op> col2) <cmp> lit. AND/OR/XOR over two bigints —
	// no overflow (order-independent, oracle-safe unlike +/*). Exercises the
	// planner's handling of a bitwise predicate expression.
	bigints := bigintCols(table)
	bitOps := []string{"BITAND", "BITOR", "BITXOR"}
	cmp := []predicates.ComparisonType{
		predicates.ComparisonEquals, predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
		predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
	}
	return &Pred{
		Col:     col.Name,
		Bitwise: true,
		BitOp:   bitOps[rng.IntN(len(bitOps))],
		BitCol2: bigints[rng.IntN(len(bigints))],
		Op:      cmp[rng.IntN(len(cmp))],
		Lit:     int64(rng.IntN(16)), // 0..15: straddles typical AND/OR/XOR results
	}
}

// genCaseLeaf builds a single-table searched-CASE predicate leaf
// `(CASE WHEN col op lit THEN <arm> ELSE <arm> END) op lit`, each arm a BIGINT
// column or literal.
func genCaseLeaf(rng *rand.Rand, table TableDef) *Pred {
	bigints := bigintCols(table)
	ops := []predicates.ComparisonType{
		predicates.ComparisonEquals, predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
		predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
	}
	arm := func() (col string, lit int64) {
		if rng.IntN(2) == 0 {
			return bigints[rng.IntN(len(bigints))], 0
		}
		return "", int64(rng.IntN(10))
	}
	cs := &CaseSpec{
		When: &Pred{Col: bigints[rng.IntN(len(bigints))], Op: ops[rng.IntN(len(ops))], Lit: int64(rng.IntN(10))},
	}
	cs.ThenCol, cs.ThenLit = arm()
	cs.ElseCol, cs.ElseLit = arm()
	return &Pred{Case: cs, Op: ops[rng.IntN(len(ops))], Lit: int64(rng.IntN(10))}
}

// genCastLeaf builds `CAST(col AS STRING) op 'lit'` over a BIGINT or BOOLEAN
// column; the literal is drawn from the value domain's string image so a match
// is reachable.
func genCastLeaf(rng *rand.Rand, table TableDef) *Pred {
	ops := []predicates.ComparisonType{
		predicates.ComparisonEquals, predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
		predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
	}
	op := ops[rng.IntN(len(ops))]
	if rng.IntN(2) == 0 {
		bigints := bigintCols(table)
		col := bigints[rng.IntN(len(bigints))]
		return &Pred{Cast: &CastSpec{Col: col, FromInt: true}, Op: op, Lit: strconv.FormatInt(int64(rng.IntN(10)), 10)}
	}
	return &Pred{Cast: &CastSpec{Col: "F", FromInt: false}, Op: op, Lit: []string{"true", "false"}[rng.IntN(2)]}
}

// genStrFnLeaf builds a single-table string-function predicate leaf. UPPER/LOWER
// compare against a case-folded literal from the string domain so a match is
// reachable; LENGTH compares against a small integer.
func genStrFnLeaf(rng *rand.Rand) *Pred {
	// Includes the whitespace-padded strings from the data domain so TRIM
	// literals (and case/substr images) reach real matches.
	domain := []string{"", "alpha", "beta", "gamma", "delta", "epsilon", "zeta", " a", "b ", " c "}
	ops := []predicates.ComparisonType{
		predicates.ComparisonEquals, predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
		predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
	}
	op := ops[rng.IntN(len(ops))]
	spec := &StrFnSpec{Col: "S"}
	switch rng.IntN(6) {
	case 5:
		spec.Fn = StrFnTrim
		// Literal = a trimmed domain word so a padded row can match.
		return &Pred{StrFn: spec, Op: op, Lit: strings.Trim(domain[rng.IntN(len(domain))], " ")}
	case 0:
		spec.Fn = StrFnUpper
		return &Pred{StrFn: spec, Op: op, Lit: strings.ToUpper(domain[rng.IntN(len(domain))])}
	case 1:
		spec.Fn = StrFnLower
		return &Pred{StrFn: spec, Op: op, Lit: strings.ToLower(domain[rng.IntN(len(domain))])}
	case 2:
		spec.Fn = StrFnLength
		return &Pred{StrFn: spec, Op: op, Lit: int64(rng.IntN(9))}
	case 3:
		spec.Fn = StrFnSubstr
		spec.Start = int64(1 + rng.IntN(6)) // 1..6 (some beyond the string → "")
		spec.Length = int64(rng.IntN(6))    // 0..5
		// Literal = SUBSTR of a domain word so a match is reachable.
		w := domain[rng.IntN(len(domain))]
		return &Pred{StrFn: spec, Op: op, Lit: substrVal(w, spec.Start, spec.Length)}
	default:
		spec.Fn = StrFnConcat
		spec.Suffix = []string{"x", "z", "q", "", "ab"}[rng.IntN(5)]
		// Literal = word+suffix so a match is reachable; a NULL-s row yields
		// just the suffix (CONCAT treats NULL as "").
		w := domain[rng.IntN(len(domain))]
		return &Pred{StrFn: spec, Op: op, Lit: w + spec.Suffix}
	}
}

// substrVal computes SQL SUBSTR(s, start, length): 1-based start with
// bound-clamping — an out-of-range start yields "", a length past the end is
// truncated. Matches the engine over the ASCII domain (verified by probe).
func substrVal(s string, start, length int64) string {
	n := int64(len(s))
	i := start - 1
	if i < 0 {
		i = 0
	}
	if i > n {
		i = n
	}
	j := i + length
	if j > n {
		j = n
	}
	if j < i {
		j = i
	}
	return s[i:j]
}

// genNumFnLeaf builds a single-table numeric-function predicate leaf. ABS
// compares against a small int; MOD uses a nonzero literal divisor (2..8) and
// compares against a literal spanning the signed remainder range.
func genNumFnLeaf(rng *rand.Rand, table TableDef) *Pred {
	bigints := bigintCols(table)
	ops := []predicates.ComparisonType{
		predicates.ComparisonEquals, predicates.ComparisonNotEquals,
		predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
		predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
	}
	spec := &NumFnSpec{Col: bigints[rng.IntN(len(bigints))]}
	switch rng.IntN(3) {
	case 0:
		spec.Fn = NumFnAbs
		return &Pred{NumFn: spec, Op: ops[rng.IntN(len(ops))], Lit: int64(rng.IntN(10))}
	case 1:
		spec.Fn = NumFnMod
		spec.Mod = int64(2 + rng.IntN(7)) // 2..8, nonzero → no divzero
		// The Go/SQL remainder ranges -(Mod-1)..(Mod-1); span it so matches occur.
		lit := int64(rng.IntN(int(2*spec.Mod-1))) - (spec.Mod - 1)
		return &Pred{NumFn: spec, Op: ops[rng.IntN(len(ops))], Lit: lit}
	default:
		spec.Fn = NumFnCoalesce
		// Prefer a nullable column so the NULL→Default path is exercised.
		spec.Col = []string{"A", "C"}[rng.IntN(2)]
		spec.Default = int64(rng.IntN(10))
		return &Pred{NumFn: spec, Op: ops[rng.IntN(len(ops))], Lit: int64(rng.IntN(10))}
	}
}
