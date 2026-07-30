package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Java binds ONE correlation per quantifier and chains the contexts:
// RecordQueryFlatMapPlan binds the outer quantifier's alias, then the inner
// quantifier's alias over a context chained off that one
// (RecordQueryFlatMapPlan.java:135-140), and Bindings resolves a miss by walking
// to its parent (Bindings.java:116-134). The consequence Go needs is that every
// ENCLOSING source stays resolvable by its OWN alias at arbitrary depth, so
// QuantifiedObjectValue.eval is a map lookup (QuantifiedObjectValue.java:84-85)
// and a leg-correlated read needs no rewriting on the way in.
//
// Go's two-level NLJ→FlatMap lowering collapses a multi-source outer into one
// join whose row is a merged concat, so binding only the join's own alias lost
// the source aliases below the FlatMap — which is why leg-correlated reads used
// to be re-anchored onto the merge correlation with the leg packed into a column
// NAME ("LEG.COL"). bindMergedOuterLegs restores the namespace: each leg of the
// merged row binds under its own correlation, over its own window.
//
// These tests pin the three ways that can regress, separately, because a fix
// satisfying one and not the others leaves the defect live: the leg must bind at
// all, it must bind to ITS OWN window (not a neighbour's), and the join's own
// alias must keep the WHOLE merged row.

// mergedOuterRowFixture is a two-leg merged concat: leg A occupies slots [0,2)
// and leg B slot [2,3). The values differ per slot so a window off by one is a
// different answer rather than a coincidence.
func mergedOuterRowFixture(aAlias, bAlias values.CorrelationIdentifier) *PositionalRow {
	return &PositionalRow{
		Type: &values.RecordType{
			Fields: []values.Field{
				{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0},
				{Name: "ACAT", FieldType: values.NotNullLong, Ordinal: 1},
				{Name: "BK", FieldType: values.NotNullLong, Ordinal: 2},
			},
			Legs: []values.RecordTypeLeg{
				values.NewRecordTypeLeg(aAlias, aAlias.Name(), 0, 2),
				values.NewRecordTypeLeg(bAlias, bAlias.Name(), 2, 1),
			},
		},
		Slots: []any{int64(10), int64(11), int64(22)},
	}
}

func TestBindMergedOuterLegs_EachLegBindsItsOwnWindow(t *testing.T) {
	t.Parallel()

	aAlias := values.NamedCorrelationIdentifier("A")
	bAlias := values.NamedCorrelationIdentifier("B")
	joinAlias := values.UniqueCorrelationIdentifier()
	row := mergedOuterRowFixture(aAlias, bAlias)

	base := EmptyEvaluationContext().WithBinding(joinAlias, row)
	ctx := bindMergedOuterLegs(base, row, joinAlias)

	// A leg-correlated read is an alias plus an ordinal in the leg's OWN layout.
	// Leg A's slot 1 is the merged row's slot 1; leg B's slot 0 is the merged
	// row's slot 2. The second is the one a whole-row binding gets wrong.
	for _, tc := range []struct {
		name    string
		alias   values.CorrelationIdentifier
		ordinal int
		want    int64
	}{
		{"leg A slot 0", aAlias, 0, 10},
		{"leg A slot 1", aAlias, 1, 11},
		{"leg B slot 0 is the merged row's slot 2", bAlias, 0, 22},
	} {
		bound, ok := ctx.GetCorrelationBinding(tc.alias)
		if !ok {
			t.Fatalf("%s: leg %s is not bound. Java keeps every quantifier's alias "+
				"resolvable below the FlatMap; an unbound leg is what forced the "+
				"qualified-name channel to exist.", tc.name, tc.alias.Name())
		}
		ordRow, isRow := bound.(values.OrdinalRow)
		if !isRow {
			t.Fatalf("%s: leg %s bound to %T, want an ordinal row — a non-row binding "+
				"cannot serve an ordinal read and trips the frontier-contract guard",
				tc.name, tc.alias.Name(), bound)
		}
		got, present := ordRow.Get(tc.ordinal)
		if !present {
			t.Fatalf("%s: leg-local ordinal %d is out of the bound window", tc.name, tc.ordinal)
		}
		if got != tc.want {
			t.Fatalf("%s: leg-local ordinal %d read %v, want %v — the window is offset "+
				"wrong, so the read answers with a NEIGHBOURING leg's value. That is "+
				"silent wrong rows, which is exactly the failure per-leg binding exists "+
				"to make impossible.", tc.name, tc.ordinal, got, tc.want)
		}
	}
}

func TestBindMergedOuterLegs_LegWindowIsBoundedByItsLeg(t *testing.T) {
	t.Parallel()

	aAlias := values.NamedCorrelationIdentifier("A")
	bAlias := values.NamedCorrelationIdentifier("B")
	joinAlias := values.UniqueCorrelationIdentifier()
	row := mergedOuterRowFixture(aAlias, bAlias)
	ctx := bindMergedOuterLegs(EmptyEvaluationContext(), row, joinAlias)

	// Leg B is ONE column wide. Reading its slot 1 must MISS rather than spill
	// forward into whatever follows in the merged row — a window that does not
	// bound its reads is not a window.
	bound, ok := ctx.GetCorrelationBinding(bAlias)
	if !ok {
		t.Fatal("leg B is not bound")
	}
	if v, present := bound.(values.OrdinalRow).Get(1); present {
		t.Fatalf("leg B's window served ordinal 1 (=%v) for a ONE-column leg. An "+
			"unbounded window reads past its leg, and past its leg is another "+
			"source's data.", v)
	}
	// And backwards: leg B must not be able to reach leg A's slots.
	if v, present := bound.(values.OrdinalRow).Get(-1); present {
		t.Fatalf("leg B's window served ordinal -1 (=%v)", v)
	}
}

func TestBindMergedOuterLegs_JoinAliasKeepsTheWholeMergedRow(t *testing.T) {
	t.Parallel()

	aAlias := values.NamedCorrelationIdentifier("A")
	bAlias := values.NamedCorrelationIdentifier("B")
	row := mergedOuterRowFixture(aAlias, bAlias)

	// The join's own alias IS one of the legs' — the degenerate case where a
	// naive "bind every leg" would narrow the join's binding to one window and a
	// read of the join's whole object would silently lose the other leg's columns.
	base := EmptyEvaluationContext().WithBinding(aAlias, row)
	ctx := bindMergedOuterLegs(base, row, aAlias)

	bound, ok := ctx.GetCorrelationBinding(aAlias)
	if !ok {
		t.Fatal("the join's own alias lost its binding")
	}
	if bound != any(row) {
		t.Fatalf("the join's alias no longer binds the WHOLE merged row (got %T). A "+
			"leg whose identity IS the join's alias must be skipped, not allowed to "+
			"narrow the join's object to one leg's window: an unqualified read of the "+
			"join flows the concat, and narrowing it drops every other leg's columns.",
			bound)
	}
	// The OTHER leg still binds — skipping the collision must not skip the rest.
	if _, ok := ctx.GetCorrelationBinding(bAlias); !ok {
		t.Fatal("leg B stopped binding because leg A collided with the join alias — " +
			"the collision is per-leg, not a reason to abandon the whole namespace")
	}
}

func TestBindMergedOuterLegs_DeclinesARowWithNoLegs(t *testing.T) {
	t.Parallel()

	joinAlias := values.UniqueCorrelationIdentifier()
	plain := &PositionalRow{
		Type: &values.RecordType{
			Fields: []values.Field{{Name: "K", FieldType: values.NotNullLong, Ordinal: 0}},
		},
		Slots: []any{int64(1)},
	}
	base := EmptyEvaluationContext()
	if got := bindMergedOuterLegs(base, plain, joinAlias); got != base {
		t.Fatal("a row with no leg table gained bindings. There are no legs to bind " +
			"— inventing one would stamp a source identity the row never claimed.")
	}
	if got := bindMergedOuterLegs(base, nil, joinAlias); got != base {
		t.Fatal("a nil binding gained bindings")
	}
	if got := bindMergedOuterLegs(base, "not a row", joinAlias); got != base {
		t.Fatal("a non-row binding gained bindings")
	}
}
