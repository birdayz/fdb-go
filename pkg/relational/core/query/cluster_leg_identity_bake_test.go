package query

// The clustered-outer-scalar path used to take a reference that ALREADY carried
// its leg's CorrelationIdentifier — `FieldValue{Child: QOV(leg), Field: COL}`,
// the resolver's own emission — render the identifier and the column into the
// text `LEG.COL`, and leave a lazy carrier for the flat baker to match against
// the seed's dotted column names.
//
// Nothing was gained by that round trip and one thing was lost. Joined text is
// AMBIGUOUS in a way an identifier is not: leg `A` holding a column named `B.C`
// and leg `A.B` holding a column named `C` both render to `A.B.C`, and the flat
// baker takes the first match. A quoted alias and a quoted column name are both
// legal SQL, so the collision is constructible, and under it a reference to one
// leg reads the other leg's row.
//
// This is white-box for the reason every pin on this defect class is: the
// misread needs a name collision across a leg boundary, and every query that
// does not construct one resolves the same either way.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// collidingClusterPullUp builds the two legs whose joined spellings collide.
// Leg "A" declares a column literally named "B.C"; leg "A.B" declares "C".
// Both render to "A.B.C".
func collidingClusterPullUp() *clusterPullUp {
	legA := clusterLegSpan{
		binding: "A",
		start:   0,
		typ:     &values.RecordType{Fields: []values.Field{{Name: "B.C", Ordinal: 0, FieldType: values.NotNullLong}}},
	}
	legAB := clusterLegSpan{
		binding: "A.B",
		start:   1,
		typ:     &values.RecordType{Fields: []values.Field{{Name: "C", Ordinal: 0, FieldType: values.NotNullLong}}},
	}
	return &clusterPullUp{
		outerCorr: values.NamedCorrelationIdentifier("outer"),
		legs:      []clusterLegSpan{legA, legAB},
		legByBinding: map[string]clusterLegSpan{
			"A":   legA,
			"A.B": legAB,
		},
	}
}

// seedCols is the level-2 output spelling clusteredOuterOrdinalSeed produces for
// that pull-up: both legs' columns joined, in leg order.
var seedCols = []string{"A.B.C", "A.B.C"}

func collidingClusterSeedQOV(t testing.TB) values.QuantifiedObjectValue {
	t.Helper()
	return exactTestQOV(t, "SEED", &values.RecordType{Fields: []values.Field{
		{Name: seedCols[0], Ordinal: 0, FieldType: values.NotNullLong},
		{Name: seedCols[1], Ordinal: 1, FieldType: values.NotNullLong},
	}})
}

// TestClusterLegBake_ResolvesTheSecondLegThroughItsIdentity is the pin. The two
// legs' columns are indistinguishable by name; only the identity separates them.
func TestClusterLegBake_ResolvesTheSecondLegThroughItsIdentity(t *testing.T) {
	t.Parallel()
	pu := collidingClusterPullUp()

	// Guard the fixture: the two spellings really do collide, or the test would
	// pass for the wrong reason — nothing to be ambiguous about.
	if seedCols[0] != seedCols[1] {
		t.Fatalf("fixture: seed columns %v do not collide", seedCols)
	}

	leg := pu.legByBinding["A.B"]
	ref := exactTestField(t, exactTestQOV(t, "A.B", leg.typ), 0)

	out := bakeClusterLegRefs(ref, pu, collidingClusterSeedQOV(t))
	fv, isFV := values.AsFieldValue(out)
	if !isFV || fv.Path() == nil {
		t.Fatalf("QOV(A.B).C did not bake (%#v) — the identity names exactly one slot, "+
			"so there is nothing for the bind to decline on", out)
	}
	ordinals := fv.Path().Ordinals()
	if len(ordinals) != 1 || ordinals[0] != 1 {
		t.Fatalf("QOV(A.B).C bound path %v, want [1] — slot 0 is leg A's "+
			"column \"B.C\", which renders to the same text and is a different column",
			ordinals)
	}
	owner, ok := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !ok || owner.Correlation() != values.NamedCorrelationIdentifier("SEED") {
		t.Fatalf("baked owner = %T, want exact SEED QOV", fv.ChildValue())
	}

	wrongSeed := exactTestQOV(t, "WRONG_SEED", &values.RecordType{Fields: []values.Field{
		{Name: seedCols[0], Ordinal: 0, FieldType: values.NotNullLong},
	}})
	if got := bakeClusterLegRefs(ref, pu, wrongSeed); got != ref {
		t.Fatalf("missing seed slot mutated the exact leg reference: %T", got)
	}
}

// TestClusterLegBake_ResolvesTheFirstLegToo is the other half: the rule is "the
// identity decides", not "prefer the later leg". Both must land on their own
// slot, or the pin above would pass under a bind that simply reversed the bias.
func TestClusterLegBake_ResolvesTheFirstLegToo(t *testing.T) {
	t.Parallel()
	pu := collidingClusterPullUp()

	leg := pu.legByBinding["A"]
	ref := exactTestField(t, exactTestQOV(t, "A", leg.typ), 0)

	fv, isFV := values.AsFieldValue(bakeClusterLegRefs(ref, pu, collidingClusterSeedQOV(t)))
	if !isFV || fv.Path() == nil {
		t.Fatalf("QOV(A).\"B.C\" did not bake — a column whose NAME contains a dot is " +
			"still a column, and its leg is stated by the correlation")
	}
	if got := fv.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("QOV(A).\"B.C\" bound path %v, want [0]", got)
	}
}

// TestClusterSeedSlot_MirrorsTheSeedBuildersOrder pins the one assumption the
// bind rests on: the slot is derived by walking pu.legs exactly as
// clusteredOuterOrdinalSeed emits them, NOT from clusterLegSpan.start.
//
// start is a CONCAT ordinal. The seed row re-emits only the plain leg spans, so
// the two indexings agree only while every concat column belongs to a leg —
// which is a property of today's producers, not of the representation. Here the
// legs deliberately start at 4 and 9, and the seed slots must still be 0 and 1.
func TestClusterSeedSlot_MirrorsTheSeedBuildersOrder(t *testing.T) {
	t.Parallel()
	legA := clusterLegSpan{
		binding: "A", start: 4,
		typ: &values.RecordType{Fields: []values.Field{{Name: "X", Ordinal: 0, FieldType: values.NotNullLong}, {Name: "Y", Ordinal: 1, FieldType: values.NotNullLong}}},
	}
	legB := clusterLegSpan{
		binding: "B", start: 9,
		typ: &values.RecordType{Fields: []values.Field{{Name: "Z", Ordinal: 0, FieldType: values.NotNullLong}}},
	}
	pu := &clusterPullUp{
		legs:         []clusterLegSpan{legA, legB},
		legByBinding: map[string]clusterLegSpan{"A": legA, "B": legB},
	}

	for _, tc := range []struct {
		binding, col string
		want         int
	}{
		{"A", "X", 0},
		{"A", "Y", 1},
		{"B", "Z", 2},
	} {
		got, ok := clusterSeedSlot(pu, tc.binding, tc.col)
		if !ok || got != tc.want {
			t.Errorf("clusterSeedSlot(%s.%s) = (%d,%v), want (%d,true) — the slot came "+
				"from the leg's CONCAT start instead of its position in the seed row",
				tc.binding, tc.col, got, ok, tc.want)
		}
	}

	// A column the leg does not declare has no slot; the caller must be able to
	// tell that from slot zero.
	if _, ok := clusterSeedSlot(pu, "A", "MISSING"); ok {
		t.Error("a column absent from the leg reported a slot")
	}
	if _, ok := clusterSeedSlot(pu, "NOSUCHLEG", "X"); ok {
		t.Error("an unknown binding reported a slot")
	}
}
