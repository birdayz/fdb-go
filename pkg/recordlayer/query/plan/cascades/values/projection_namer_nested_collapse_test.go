package values

import "testing"

// TestProjectionNamerCollapsesANestedPath was a CHARACTERIZATION of two
// derivation defects. RFC-229 §2.3 closed the second one, so that half is
// FLIPPED here — it now asserts the fixed property, exactly as the old failure
// message instructed — and the first half stays a characterization, because
// §2.3 is deliberately nested-only and does not touch the fallback arm.
//
// The fixtures are not invented. A fused nested reference really is ONE
// FieldValue whose `Field` is the struct ROOT with a multi-accessor `Resolved`:
// the SQL resolver's fuseNestedAccessors copies the node whole, updates `Typ`
// to the leaf type, and leaves `Field` alone. Java's resolver fuses identically
// (SemanticAnalyzer.java:598, FieldValue.ofFieldsAndFuseIfPossible) and then
// names the result by the REQUESTED IDENTIFIER `n.sk` rather than by the fused
// value (SemanticAnalyzer.java:599) — which is the whole of §2.3 in one line of
// Java.
func TestProjectionNamerCollapsesANestedPath(t *testing.T) {
	t.Parallel()

	// DEFECT 1 — STILL A CHARACTERIZATION. The same expression renders
	// differently either side of the bake, because the fallback arm renders
	// ordinals. Writers and readers agree only if they derive on the same side
	// of it. RFC-229 §2.3 is nested-only and leaves this arm alone; the mask
	// that keeps it from reaching a user is isPlainColumnRef in the result-set
	// layer, pinned separately.
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

	// A SINGLE-accessor resolved reference is the other control, and it is the
	// one that stops §2.3 from being over-applied: it is NOT nested, so it keeps
	// the flat Field rendering. Without this, "store the path" could be widened
	// to every resolved reference and nothing here would say so.
	if got, want := ProjectionColumnName(baked), "N"; got != want {
		t.Fatalf("a single-accessor resolved reference now renders %q, want %q — "+
			"the nested arm has widened past the multi-accessor predicate", got, want)
	}

	// DEFECT 2 — FIXED (RFC-229 §2.3). Two references reading DIFFERENT leaves
	// of one struct root used to take the SAME output name, because the namer
	// read the flat root. They now take their resolved PATH.
	nested := &FieldValue{Field: "N", Resolved: &FieldPath{
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 0}, {Field: "SK", Ordinal: 1}},
	}}
	nested2 := &FieldValue{Field: "N", Resolved: &FieldPath{
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 0}, {Field: "CO", Ordinal: 2}},
	}}

	a, b := ProjectionColumnName(nested), ProjectionColumnName(nested2)
	if a == b {
		t.Fatalf("n.sk and n.co BOTH render as %q — the projection namer has gone "+
			"back to reading the flat struct root. Two grouping/projected columns "+
			"then share one output name and a name-keyed reader serves whichever "+
			"was written last; `SELECT n.sk, n.co` emits duplicate labels over "+
			"correct data, and `GROUP BY n.sk, n.co` would return 2 groups where "+
			"the data has 4.", a)
	}
	// Asserting the exact spellings, not merely that they differ: "differ" is
	// satisfiable by a positional fallback, which would be a different answer
	// from the one §2.3 specifies and would silently drop the path.
	if a != "N.SK" || b != "N.CO" {
		t.Fatalf("nested projection names are %q and %q, want %q and %q — the "+
			"stored name must be the RESOLVED PATH (Java names the fused "+
			"reference by the requested identifier, SemanticAnalyzer.java:599), "+
			"not merely something distinct", a, b, "N.SK", "N.CO")
	}

	// The renderer §2.3 mints from, asserted as the same string rather than
	// merely as "distinct": if these ever disagree the authority is no longer
	// reading the path renderer and the two would drift apart silently.
	if got := ColumnNameValue(nested); got != a {
		t.Fatalf("ColumnNameValue(n.sk) = %q but the projection namer says %q — "+
			"the namer has stopped minting from the path renderer", got, a)
	}
	if x, y := ColumnNameValue(nested), ColumnNameValue(nested2); x == y {
		t.Fatalf("ColumnNameValue collapsed n.sk and n.co to %q — the renderer "+
			"RFC-229 §2.3 mints from no longer distinguishes the two leaves, so the "+
			"prescribed fix stores the same defect it exists to remove", x)
	}

	// NestedResolvedPath is the single predicate all five naming authorities
	// read. Drive both of its arms here so a change to it cannot pass on the
	// strength of the callers alone.
	if path, nested := NestedResolvedPath(nested); !nested || path != "N.SK" {
		t.Fatalf("NestedResolvedPath(n.sk) = (%q, %v), want (%q, true)", path, nested, "N.SK")
	}
	if path, isNested := NestedResolvedPath(baked); isNested || path != "" {
		t.Fatalf("NestedResolvedPath(single-accessor) = (%q, %v), want (\"\", false) — "+
			"a one-accessor reference is not nested and must keep the flat rendering",
			path, isNested)
	}
	if path, isNested := NestedResolvedPath(lazyExpr); isNested || path != "" {
		t.Fatalf("NestedResolvedPath(a computed expression) = (%q, %v), want (\"\", false)",
			path, isNested)
	}
}
