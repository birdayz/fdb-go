package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// projLeg is a derived-table leg as the planner builds it: a projection of two
// columns over a scan. This is what `FROM (SELECT id, v FROM t1) AS d` and a
// non-inlined CTE reference lower to, and it is the leg shape the step-1 seed
// used to refuse.
func projLeg(t *testing.T) *plans.RecordQueryProjectionPlan {
	t.Helper()
	scan := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID", "V"), false)
	q := plans.QuantifierOverPlan(scan)
	idV, err := values.NewFieldValueOfOrdinal(values.NewQuantifiedObjectValueOfType(q.GetAlias(), commit2RecType("T1", "ID", "V")), 0)
	if err != nil {
		t.Fatalf("ofOrdinal(0): %v", err)
	}
	vV, err := values.NewFieldValueOfOrdinal(values.NewQuantifiedObjectValueOfType(q.GetAlias(), commit2RecType("T1", "ID", "V")), 1)
	if err != nil {
		t.Fatalf("ofOrdinal(1): %v", err)
	}
	return plans.NewRecordQueryProjectionPlan([]values.Value{idV, vV}, scan)
}

// A PROJECTION leg is ordinal-safe and contributes its OWN stated row to the
// step-1 leg-concat seed.
//
// It used to fall to legOrdinalSafety's default refusal. The seed was then never
// built, the materialized join kept the SELECT's folded projection as its merged
// row, and a projected EXISTS over a derived source died in the executor with
// "multi-leg row cannot serve a source-relative ordinal" — while the identical
// query over a base table (two scan legs) returned correct rows. The end-to-end
// witness is TestFDB_ProjectionResultTypeProbe's derived_exists arm; this pins
// the decision itself, which a functional test cannot distinguish from any other
// route to a working plan.
func TestProjectionLegSeedsStep1(t *testing.T) {
	t.Parallel()
	proj := projLeg(t)
	if safe, node := legOrdinalSafety(proj); !safe {
		t.Fatalf("a projection leg must be ordinal-safe: refused at %T — executeProjection "+
			"always emits a dense PositionalRow with one slot per projection, which is "+
			"exactly what the positional merge needs", node)
	}

	// The seed covers BOTH legs: the projection's two stated columns first, then
	// the scan's two. Arity is asserted because a projection admitted with the
	// WRONG row (its inner's, say) would still be "safe" and would window the
	// merged row against columns the leg never emits.
	scan := plans.NewRecordQueryScanPlan([]string{"T3"}, commit2RecType("T3", "ID", "T1_ID"), false)
	seed, decline := reconstructFoldStep1Seed(proj, scan,
		values.NamedCorrelationIdentifier("D"), values.NamedCorrelationIdentifier("T3"))
	if seed == nil {
		t.Fatalf("projection + scan legs must reconstruct a seed; declined shape=%v witness=%q",
			decline.Shape, decline.Witness)
	}
	rc, ok := seed.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("seed is %T, want *values.RecordConstructorValue", seed)
	}
	if len(rc.Fields) != 4 {
		t.Fatalf("seed has %d slots, want 4 (the projection's ID,V then the scan's ID,T1_ID)", len(rc.Fields))
	}
	wantNames := []string{"ID", "V", "ID", "T1_ID"}
	for i, want := range wantNames {
		if rc.Fields[i].Name != want {
			t.Errorf("seed slot %d is %q, want %q", i, rc.Fields[i].Name, want)
		}
	}

	// The window derivation must ACCEPT it — a seed no windows source accepts is
	// declined one step later and the leg silently keeps the broken row.
	if w, _ := ordinalSeedLegWindowsAcceptingNestedOf(seed); w == nil {
		t.Fatal("the reconstructed seed must be accepted by the leg-window derivation")
	}
}

// A projection whose row is UNSTATABLE is refused, not guessed at. The safety
// walk and the concat builder read that decision through ONE helper, so they
// cannot admit a leg whose fields the merged row then cannot enumerate.
func TestProjectionLegWithNoStatableRowIsRefused(t *testing.T) {
	t.Parallel()
	scan := plans.NewRecordQueryScanPlan([]string{"T1"}, commit2RecType("T1", "ID", "V"), false)
	empty := plans.NewRecordQueryProjectionPlan(nil, scan)
	if projectionLegRowType(empty) != nil {
		t.Fatal("a projection with no columns states no row")
	}
	if safe, _ := legOrdinalSafety(empty); safe {
		t.Fatal("a projection that cannot state its row must NOT be ordinal-safe — seeding " +
			"ordinals off a row nothing describes addresses slots by guess")
	}
	if seed, _ := reconstructFoldStep1Seed(empty, scan,
		values.NamedCorrelationIdentifier("D"), values.NamedCorrelationIdentifier("T1")); seed != nil {
		t.Fatal("an unstatable projection leg must decline the seed reconstruction")
	}
}

// AN UNSTATED FIELD TYPE IS NOT A DIFFERENCE.
//
// The orientation gate identifies which physical leg occupies which slot of a
// baked seed by comparing the seed's leg windows against each leg plan's own
// row. A window built for a derived-table leg carries the column NAMES with no
// inferred type, while the leg's plan states LONG. Requiring type equality there
// turned that absence into a mismatch and declined BOTH orientations — and since
// this gate is what admits the materialized join at all, the query was left with
// no physical plan rather than with one fewer alternative.
//
// The direction that must NOT be relaxed with it: two legs whose stated types
// genuinely disagree are still different legs.
func TestOrientationGateIgnoresUnstatedFieldTypes(t *testing.T) {
	t.Parallel()
	stated := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	}}
	unstated := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
		{Name: "V", FieldType: values.UnknownType, Ordinal: 1},
	}}
	if !recordFieldsMatch(unstated, stated) || !recordFieldsMatch(stated, unstated) {
		t.Fatal("a window whose field types were never inferred must still match a leg " +
			"that states them — UNKNOWN means 'nothing to say', not 'different'")
	}
	if !typeUnstated(nil) || !typeUnstated(values.UnknownType) {
		t.Fatal("nil and UnknownType are both 'unstated'")
	}
	if typeUnstated(values.NotNullLong) {
		t.Fatal("a real type is stated")
	}

	// The discriminating power that must survive: same arity, same ordinals,
	// different names — still a mismatch, because that is how the gate tells one
	// leg from the other.
	renamed := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
		{Name: "T1_ID", FieldType: values.UnknownType, Ordinal: 1},
	}}
	if recordFieldsMatch(renamed, stated) {
		t.Fatal("legs with different column names must NOT match — relaxing the type " +
			"comparison must not relax the name comparison with it")
	}
	// And two legs that BOTH state a type still have to agree on it.
	other := &values.RecordType{Fields: []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullString, Ordinal: 1},
	}}
	if recordFieldsMatch(other, stated) {
		t.Fatal("two legs that both state a field type must still agree on it")
	}
}
