package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestAggregateNaming_StableUnderOrdinalBind pins that the canonical
// aggregate/group-key OUTPUT column names do not change when the operand
// references are bound to plan-time ordinals: the naming
// authority renders through values.ColumnNameValue, so a baked and a lazy
// instance of the same reference derive the same column name. Drift here
// broke the HAVING lockstep ("SUM(AMOUNT#2)" ref vs "SUM(AMOUNT)" output
// slot).
func TestAggregateNaming_StableUnderOrdinalBind(t *testing.T) {
	t.Parallel()

	lazyOp := testField("AMOUNT", values.NotNullLong)
	bakedOp := testFieldAt("AMOUNT", 2, values.NotNullLong)

	lazyName := AggregateResultColumnName(AggregateSpec{Function: AggSum, Operand: lazyOp})
	bakedName := AggregateResultColumnName(AggregateSpec{Function: AggSum, Operand: bakedOp})
	if lazyName != bakedName {
		t.Fatalf("aggregate name drift: lazy %q vs baked %q", lazyName, bakedName)
	}
	if lazyName != "SUM(TEST_FIELD.AMOUNT)" {
		t.Fatalf("canonical name = %q, want SUM(TEST_FIELD.AMOUNT)", lazyName)
	}

	// COMPUTED operand (no FieldValue shortcut — the ColumnNameValue arm).
	lazyExpr := &values.ArithmeticValue{Op: values.OpMul, Left: lazyOp, Right: testField("QTY", values.NotNullLong)}
	bakedExpr := &values.ArithmeticValue{Op: values.OpMul, Left: bakedOp, Right: testFieldAt("QTY", 3, values.NotNullLong)}
	if l, b := AggregateResultColumnName(AggregateSpec{Function: AggSum, Operand: lazyExpr}),
		AggregateResultColumnName(AggregateSpec{Function: AggSum, Operand: bakedExpr}); l != b {
		t.Fatalf("computed-operand aggregate name drift: lazy %q vs baked %q", l, b)
	}

	// Group-key naming: computed keys render ordinal-free too.
	if l, b := AggregateKeyColumnName(lazyExpr), AggregateKeyColumnName(bakedExpr); l != b {
		t.Fatalf("group-key name drift: lazy %q vs baked %q", l, b)
	}
}

// TestAggregateKeyColumnName_NestedKeyTakesTheResolvedPath pins the group-key
// half of RFC-229 §2.3, and it is the half with a wrong-ROWS consequence rather
// than a wrong-label one.
//
// AggregateKeyColumnName is THE naming authority for a grouping key, and the
// name it returns is a MAP KEY in three last-wins maps (the translator's
// group-key ordinal registry, the executor's per-group datum map, and the
// requested-ordering push-through). A fused nested reference carries the struct
// ROOT in Field, so reading Field spells `n.sk` and `n.co` alike and the second
// grouping column overwrites the first: `GROUP BY n.sk, n.co` returns 2 groups
// where the data has 4.
//
// Nested-path GROUP BY is refused at the SQL layer today
// (groupby_nested_key_collapse_fdb_test.go pins the refusal), so this
// conversion is what makes implementing that feature SAFE rather than what
// makes it work. The order is the point: converting after the feature lands
// means shipping the collapse, and its symptom — missing groups — is silent.
func TestAggregateKeyColumnName_NestedKeyTakesTheResolvedPath(t *testing.T) {
	t.Parallel()

	nestedType := rowOfTypes("N", rowOfTypes("SK", values.NotNullLong, "CO", values.NotNullLong))
	base := mustExpression(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("BASE"), nestedType))
	nestedSK := mustResolvedField(base, "N", "SK")
	nestedCO := mustResolvedField(base, "N", "CO")

	sk, co := AggregateKeyColumnName(nestedSK), AggregateKeyColumnName(nestedCO)
	if sk == co {
		t.Fatalf("GROUP BY n.sk and GROUP BY n.co both name their output column %q — "+
			"the group-key authority is reading the flat struct root again. Two "+
			"grouping columns now share one map key and the later overwrites the "+
			"earlier: `GROUP BY n.sk, n.co` returns 2 groups where the data has 4.", sk)
	}
	if sk != "BASE.N.SK" || co != "BASE.N.CO" {
		t.Fatalf("nested group-key names are %q and %q, want %q and %q — merely "+
			"being distinct is satisfiable by a positional fallback, which is a "+
			"different contract and would not survive the round trip through the "+
			"translator's name-keyed registry", sk, co, "BASE.N.SK", "BASE.N.CO")
	}

	// The whole-row name vector is what the plan and the executor actually
	// consume, so drive it too: a fix applied to the authority but not visible
	// through GroupByOutputColumnNames would leave the collapse in place where
	// it bites.
	names := GroupByOutputColumnNames([]values.Value{nestedSK, nestedCO}, nil)
	if len(names) != 2 || names[0] != "BASE.N.SK" || names[1] != "BASE.N.CO" {
		t.Fatalf("GroupByOutputColumnNames over two nested keys = %v, want [BASE.N.SK BASE.N.CO]", names)
	}

	// CONTROL — a FLAT key must keep its bare Field. Without this the assertions
	// above pass for an implementation that routed EVERY key through the path
	// renderer, which would qualify a lateral-unnest shadowing key as `V.V` and
	// break the RFC-142 lockstep the flat arm exists for.
	flat := testField("STATUS", values.NotNullLong)
	if got := AggregateKeyColumnName(flat); got != "STATUS" {
		t.Fatalf("a FLAT group key now names its column %q, want STATUS — the "+
			"nested arm has widened past the multi-accessor predicate", got)
	}
	qualifiedBase := mustExpression(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("Q$DUP1"),
		rowOfTypes("QID", values.NotNullLong),
	))
	qualified := mustResolvedField(qualifiedBase, "QID")
	if got := AggregateKeyColumnName(qualified); got != "QID" {
		t.Fatalf("a CORRELATION-qualified group key now names its column %q, want "+
			"QID — the executor keys that row by the bare field (RFC-142), so a "+
			"qualified name here serves NULL for a correctly-grouped result", got)
	}

	// THE QUALIFIED NESTED KEY, driven because nothing else drives it. The
	// control above has Resolved == nil, so it never reaches the nested arm at
	// all: it pins the FLAT arm's treatment of a child and says nothing about
	// what a child does to a PATH. That arm is user-reachable — with ≥2 FROM
	// sources the resolver emits every reference through its quantifier, so a
	// nested key really does arrive here carrying QOV(T1).
	//
	// It names `T1.N.SK`, qualified, and that is the DECIDED behaviour rather
	// than an accident of the renderer: over `FROM t1, t2` where both declare an
	// `n`, a bare `N.SK` would collapse two different columns in the same
	// last-wins maps the nested arm exists to protect. Asserting the exact
	// spelling, not merely "distinct": distinct is satisfiable by a positional
	// fallback.
	qualifiedNested := mustResolvedField(
		mustExpression(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T1"), nestedType)),
		"N", "SK",
	)
	if got := AggregateKeyColumnName(qualifiedNested); got != "T1.N.SK" {
		t.Fatalf("a QUALIFIED nested group key names its column %q, want T1.N.SK — "+
			"the path is rendered THROUGH the child by design, so that two sources "+
			"each declaring an `n` cannot collapse `T1.N.SK` and `T2.N.SK` into one "+
			"map key. If this now answers N.SK the qualifier was dropped, which "+
			"re-creates the collapse one level up; if it answers N or T1.N the "+
			"nested arm stopped firing for a childful reference altogether", got)
	}
	// The cross-source distinctness the qualifier buys, asserted directly rather
	// than left as an argument in a comment.
	other := mustResolvedField(
		mustExpression(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("T2"), nestedType)),
		"N", "SK",
	)
	if a, b := AggregateKeyColumnName(qualifiedNested), AggregateKeyColumnName(other); a == b {
		t.Fatalf("the same nested path off TWO different sources both name %q — "+
			"`t1.n.sk` and `t2.n.sk` are different columns and this is the map key "+
			"they are stored under", a)
	}
}
