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

	// Leg A is the discriminating one for a WIDENED window, and leg B cannot be.
	// B is the LAST leg, so a window widened past it runs out of merged row and
	// the read misses for the wrong reason — the PARENT's bound covering for the
	// leg's. Widening every window to the whole row's width leaves both B
	// assertions above passing. A is two columns wide with leg B's data directly
	// after it, so its ordinal 2 is the read that distinguishes "bounded by its
	// leg" from "bounded by the row".
	aBound, ok := ctx.GetCorrelationBinding(aAlias)
	if !ok {
		t.Fatal("leg A is not bound")
	}
	if v, present := aBound.(values.OrdinalRow).Get(2); present {
		t.Fatalf("leg A's window served ordinal 2 (=%v) for a TWO-column leg — and "+
			"slot 2 of the merged row is leg B's column. This is the failure the test "+
			"is named for, stated on the only leg that can express it: a window "+
			"bounded by the ROW rather than by its LEG answers a leg-local read with "+
			"the NEXT source's data.", v)
	}
}

// An UNSTATED leg identity (the zero-value CorrelationIdentifier) names nothing,
// and binding under it would put a window in the namespace that no reference can
// legitimately reach — while shadowing whatever a caller had at that key.
//
// Go's zero value has no Java analogue (Quantifier.getAlias() is @Nonnull), so
// holding one here means a producer had no identifier to thread and left it
// blank. values.SameLeg declines such a pair for exactly this reason; this pins
// that the binder declines it too, rather than relying on the comparison to be
// the only guard.
func TestBindMergedOuterLegs_DeclinesAnUnstatedLegIdentity(t *testing.T) {
	t.Parallel()

	bAlias := values.NamedCorrelationIdentifier("B")
	joinAlias := values.UniqueCorrelationIdentifier()

	var unstated values.CorrelationIdentifier
	row := &PositionalRow{
		Type: &values.RecordType{
			Fields: []values.Field{
				{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0},
				{Name: "BK", FieldType: values.NotNullLong, Ordinal: 1},
			},
			Legs: []values.RecordTypeLeg{
				values.NewRecordTypeLeg(unstated, "", 0, 1),
				values.NewRecordTypeLeg(bAlias, bAlias.Name(), 1, 1),
			},
		},
		Slots: []any{int64(10), int64(22)},
	}

	sentinel := int64(999)
	base := EmptyEvaluationContext().WithBinding(unstated, sentinel)
	ctx := bindMergedOuterLegs(base, row, joinAlias)

	bound, ok := ctx.GetCorrelationBinding(unstated)
	if !ok {
		t.Fatal("the enclosing binding at the unstated key vanished entirely")
	}
	if bound != any(sentinel) {
		t.Fatalf("the unstated leg BOUND a window (%T), shadowing the enclosing "+
			"binding at a key that names nothing. A leg with no identity is a "+
			"producer that forgot to state one; serving its slots turns that omission "+
			"into a live namespace entry.", bound)
	}

	// The stated leg beside it still binds — declining the unstated one is
	// per-leg, not a reason to abandon the row.
	if _, ok := ctx.GetCorrelationBinding(bAlias); !ok {
		t.Fatal("leg B stopped binding because an unstated leg preceded it")
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

// TestBindMergedOuterLegs_FirstClaimWinsOnADuplicateAlias pins the precedence
// among legs, which the single-alias fixtures above cannot reach.
//
// concatLegPositionals emits the two TOP-LEVEL leg windows first and appends every
// buried sub-window after them, and a buried source can carry the SAME alias as a
// top-level leg — a self-join whose right side is itself a join over the same
// source, or a re-used correlation on a shape the planner did not rename. Every
// other reader of that table resolves an alias to its FIRST entry
// (addBuriedLegLayouts skips an already-claimed alias; the leg-window binders take
// the first match), so this binder must too.
//
// Last-wins is not a cosmetic difference. The buried window is a SUBSET of the
// top-level one, so the two readers would hand the same alias rows of different
// widths at different offsets — the reference resolves to whichever slots the
// reader that answered happened to hold, and that is the wrong-row failure the
// whole per-leg binding exists to remove.
func TestBindMergedOuterLegs_FirstClaimWinsOnADuplicateAlias(t *testing.T) {
	t.Parallel()

	aAlias := values.NamedCorrelationIdentifier("A")
	bAlias := values.NamedCorrelationIdentifier("B")
	joinAlias := values.UniqueCorrelationIdentifier()

	// A is TOP-LEVEL over [0,3) — the wide window. It reappears as a BURIED
	// sub-window over [2,1) — one column, a different offset — exactly the table
	// concatLegPositionals produces when a leg that is itself a join re-uses an
	// alias.
	row := &PositionalRow{
		Type: &values.RecordType{
			Fields: []values.Field{
				{Name: "A0", FieldType: values.NotNullLong, Ordinal: 0},
				{Name: "A1", FieldType: values.NotNullLong, Ordinal: 1},
				{Name: "A2", FieldType: values.NotNullLong, Ordinal: 2},
				{Name: "BK", FieldType: values.NotNullLong, Ordinal: 3},
			},
			Legs: []values.RecordTypeLeg{
				values.NewRecordTypeLeg(aAlias, aAlias.Name(), 0, 3),
				values.NewRecordTypeLeg(bAlias, bAlias.Name(), 3, 1),
				// The buried re-use, appended after the top-level pair.
				values.NewRecordTypeLeg(aAlias, aAlias.Name(), 2, 1),
			},
		},
		Slots: []any{int64(70), int64(71), int64(72), int64(83)},
	}

	ctx := bindMergedOuterLegs(EmptyEvaluationContext(), row, joinAlias)
	bound, ok := ctx.GetCorrelationBinding(aAlias)
	if !ok {
		t.Fatal("alias A is not bound at all")
	}
	ordRow := bound.(values.OrdinalRow)

	// Slot 0 of A must be the merged row's slot 0 (the WIDE window), not slot 2
	// (the narrow buried one). Every slot value differs, so an offset error is a
	// different answer rather than a coincidence.
	got, present := ordRow.Get(0)
	if !present || got != int64(70) {
		t.Fatalf("A's slot 0 read %v (present=%v), want 70 — the merged row's slot 0. "+
			"Reading 72 means the BURIED [2,1) window claimed the alias, so the binder "+
			"is last-wins while addBuriedLegLayouts and every other leg-table reader "+
			"are first-match. One alias resolving to two different windows depending "+
			"on which reader answered is silent wrong rows.", got, present)
	}
	// And the window must be the wide one end to end: the narrow re-use is 1
	// column, so serving slot 2 proves the top-level window survived whole.
	if got, present := ordRow.Get(2); !present || got != int64(72) {
		t.Fatalf("A's slot 2 read %v (present=%v), want 72. The top-level window is 3 "+
			"columns wide; a 1-column answer is the buried window wearing A's alias.",
			got, present)
	}
	if v, present := ordRow.Get(3); present {
		t.Fatalf("A's window served ordinal 3 (=%v) — it must stop at its own 3 columns "+
			"and not spill into B", v)
	}
	// The duplicate must not cost B its binding.
	if _, ok := ctx.GetCorrelationBinding(bAlias); !ok {
		t.Fatal("leg B lost its binding because A appeared twice — the duplicate is " +
			"per-alias, not a reason to abandon the rest of the namespace")
	}
}

// TestBindMergedOuterLegs_ShadowsWithoutDestroyingTheEnclosingBinding pins the
// interaction with a binding that is ALREADY at a leg's alias in the incoming
// context — an enclosing source whose alias a leg re-uses.
//
// Java settles this by construction. RecordQueryFlatMapPlan builds
// `fromOuterContext = context.withBinding(...)` and then
// `fromOuterContext.withBinding(...)` for the inner
// (RecordQueryFlatMapPlan.java:135-140): each step yields a NEW context whose
// lookup finds the nearest binding and walks to the parent only on a miss
// (Bindings.java:116-134). So the inner binding SHADOWS the outer for everything
// evaluated in the child context, and the parent context object is untouched —
// anything still evaluating against it keeps seeing the enclosing binding.
//
// Both halves matter and they pull in opposite directions. Not shadowing means a
// leg-correlated read below the FlatMap resolves against the ENCLOSING row, which
// is a different source's data. Destroying the enclosing binding means a sibling
// that legitimately reads it gets nothing.
func TestBindMergedOuterLegs_ShadowsWithoutDestroyingTheEnclosingBinding(t *testing.T) {
	t.Parallel()

	aAlias := values.NamedCorrelationIdentifier("A")
	bAlias := values.NamedCorrelationIdentifier("B")
	joinAlias := values.UniqueCorrelationIdentifier()
	row := mergedOuterRowFixture(aAlias, bAlias)

	// An enclosing binding already sits at leg A's alias, carrying values nothing
	// in the merged row uses, so a read that reaches it is unmistakable.
	enclosing := &PositionalRow{
		Type: &values.RecordType{
			Fields: []values.Field{{Name: "AID", FieldType: values.NotNullLong, Ordinal: 0}},
		},
		Slots: []any{int64(999)},
	}
	base := EmptyEvaluationContext().WithBinding(aAlias, enclosing)
	ctx := bindMergedOuterLegs(base, row, joinAlias)

	bound, ok := ctx.GetCorrelationBinding(aAlias)
	if !ok {
		t.Fatal("leg A is not bound in the child context")
	}
	got, present := bound.(values.OrdinalRow).Get(0)
	if !present || got != int64(10) {
		t.Fatalf("leg A's slot 0 read %v (present=%v), want 10 — the merged row's leg "+
			"window. Reading 999 means the ENCLOSING binding survived in the child "+
			"context and the leg did not shadow it, so a read correlated to A below "+
			"the FlatMap answers with an outer source's row.", got, present)
	}

	// The enclosing context is a different object and still answers with the
	// enclosing binding — Java's parent context is never mutated, and a sibling
	// evaluated against it must keep seeing what it always saw.
	outerBound, ok := base.GetCorrelationBinding(aAlias)
	if !ok {
		t.Fatal("the enclosing context lost its binding at A entirely")
	}
	if outerBound != any(enclosing) {
		t.Fatalf("the enclosing context's binding at A changed to %T. Binding the legs "+
			"must produce a CHILD context and leave the parent alone; a sibling still "+
			"evaluating against the parent reads whatever was written over it.",
			outerBound)
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
