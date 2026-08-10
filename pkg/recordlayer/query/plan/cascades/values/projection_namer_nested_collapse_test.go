package values

import "testing"

// TestProjectionNamerCollapsesANestedPath pins the two derivation defects
// RFC-229 exists to close, as failing properties rather than as prose.
//
// Both are CHARACTERIZATIONS of behaviour that is wrong: they assert the
// current, defective answers, so that the fix breaks them loudly and the
// failure message says what to assert instead. Neither is a contract to keep.
//
// The fixtures are not invented. A fused nested reference really is ONE
// FieldValue whose `Field` is the struct ROOT with a multi-accessor `Resolved`:
// the SQL resolver's fuseNestedAccessors copies the node whole, updates `Typ`
// to the leaf type, and leaves `Field` alone. The planner's own rewrite
// machinery does the opposite — it sets `Field` to the last accessor's name —
// which is why the doc comment claiming that as an invariant is true of one
// producer and false of the other, and why nothing detects the difference.
func TestProjectionNamerCollapsesANestedPath(t *testing.T) {
	t.Parallel()

	// DEFECT 1 — the same expression renders differently either side of the bake,
	// because the fallback arm renders ordinals. Writers and readers agree only
	// if they derive on the same side of it.
	lazy := &FieldValue{Field: "N"}
	baked := &FieldValue{Field: "N", Resolved: &FieldPath{
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 0}},
	}}
	one := &ConstantValue{Value: int64(1)}
	lazyExpr := &ArithmeticValue{Left: lazy, Right: one, Op: OpAdd}
	bakedExpr := &ArithmeticValue{Left: baked, Right: one, Op: OpAdd}

	preBake, postBake := ProjectionColumnName(lazyExpr), ProjectionColumnName(bakedExpr)
	if preBake == postBake {
		t.Fatalf("a computed projection now renders %q on BOTH sides of the bake.\n"+
			"  THE ASYMMETRY THIS PINS IS FIXED — the fallback arm no longer leaks "+
			"ordinals into an output name. Replace this half with an assertion that "+
			"the two agree, and drop the pre/post-bake language from RFC-229 §0.",
			preBake)
	}

	// The plain-field arm is the control: it returns Field verbatim, so it has no
	// asymmetry to lose. If this ever diverges, the defect above has SPREAD to the
	// common path rather than being fixed, and reading the first assertion as
	// still-defective would be wrong.
	if got, want := ProjectionColumnName(baked), ProjectionColumnName(lazy); got != want {
		t.Fatalf("a PLAIN field reference now renders %q baked and %q lazy — the "+
			"bake asymmetry has spread from computed expressions to the common "+
			"path, which is a regression, not the fix this file waits for", got, want)
	}

	// DEFECT 2 — two references reading DIFFERENT leaves of one struct root take
	// the SAME output name, because the namer reads the flat root. This is the
	// collapse RFC-227 fixed on the sort-key side and RFC-229 §2.3 closes here.
	nested := &FieldValue{Field: "N", Resolved: &FieldPath{
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 0}, {Field: "SK", Ordinal: 1}},
	}}
	nested2 := &FieldValue{Field: "N", Resolved: &FieldPath{
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 0}, {Field: "CO", Ordinal: 2}},
	}}

	if a, b := ProjectionColumnName(nested), ProjectionColumnName(nested2); a != b {
		t.Fatalf("n.sk and n.co now render as %q and %q.\n"+
			"  THE COLLAPSE THIS PINS IS FIXED: the projection namer distinguishes "+
			"two leaves of one struct root. FLIP this to assert they differ, and "+
			"check the group-key namer went with it — the tripwire at "+
			"groupby_nested_key_collapse_fdb_test.go covers that half.", a, b)
	}

	// The other half of the same fixture, and the reason the fix is available
	// rather than hypothetical: the path renderer already keeps them distinct.
	// If THIS collapses, §2.3 has no renderer to mint from and the RFC's
	// prescription is void.
	if a, b := ColumnNameValue(nested), ColumnNameValue(nested2); a == b {
		t.Fatalf("ColumnNameValue collapsed n.sk and n.co to %q — the renderer "+
			"RFC-229 §2.3 mints from no longer distinguishes the two leaves, so the "+
			"prescribed fix would store the same defect it exists to remove", a)
	}
}
