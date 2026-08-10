package values

import "testing"

// Probe: RFC-229 §0's central defect claims, measured.
func TestProjectionNamerCollapsesANestedPath(t *testing.T) {
	t.Parallel()

	lazy := &FieldValue{Field: "N"}
	baked := &FieldValue{Field: "N", Resolved: &FieldPath{
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 0}},
	}}

	// §0 claim: a COMPUTED projection over a baked field renders (N#0 + 1)
	// where the same expression pre-bake renders (N + 1).
	one := &ConstantValue{Value: int64(1)}
	lazyExpr := &ArithmeticValue{Left: lazy, Right: one, Op: OpAdd}
	bakedExpr := &ArithmeticValue{Left: baked, Right: one, Op: OpAdd}
	t.Logf("PROBE computed  pre-bake=%q post-bake=%q  ASYMMETRIC=%v",
		ProjectionColumnName(lazyExpr), ProjectionColumnName(bakedExpr),
		ProjectionColumnName(lazyExpr) != ProjectionColumnName(bakedExpr))

	// Plain field arm: returns Field verbatim, so no asymmetry.
	t.Logf("PROBE plain     pre-bake=%q post-bake=%q  ASYMMETRIC=%v",
		ProjectionColumnName(lazy), ProjectionColumnName(baked),
		ProjectionColumnName(lazy) != ProjectionColumnName(baked))

	// §2.3 claim: a fused nested reference is ONE FieldValue whose Field is the
	// struct ROOT; the resolved path is what ColumnNameValue renders.
	nested := &FieldValue{Field: "N", Resolved: &FieldPath{
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 0}, {Field: "SK", Ordinal: 1}},
	}}
	nested2 := &FieldValue{Field: "N", Resolved: &FieldPath{
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 0}, {Field: "CO", Ordinal: 2}},
	}}
	t.Logf("PROBE nested    ProjectionColumnName=%q / %q  COLLAPSE=%v",
		ProjectionColumnName(nested), ProjectionColumnName(nested2),
		ProjectionColumnName(nested) == ProjectionColumnName(nested2))
	t.Logf("PROBE nested    ColumnNameValue=%q / %q  COLLAPSE=%v",
		ColumnNameValue(nested), ColumnNameValue(nested2),
		ColumnNameValue(nested) == ColumnNameValue(nested2))
}
