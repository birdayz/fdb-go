package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestLogicalProjection_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	v1 := values.NewBooleanValue(true)
	v2 := values.NewNullValue(values.NullableLong)
	p := mustExpression(NewLogicalProjectionExpression([]values.Value{v1, v2}, q))
	if got := p.GetProjectedValues(); len(got) != 2 {
		t.Fatalf("projected size=%d, want 2", len(got))
	}
	if p.GetInner().GetAlias() != q.GetAlias() {
		t.Fatal("inner alias mismatch")
	}
	if p.CanCorrelate() {
		t.Fatal("projection should not anchor a correlation")
	}
	// GetResultValue states the row the projection PRODUCES — a record
	// constructor over its two projected columns.
	//
	// INVERTED. This asserted the value was a *QuantifiedObjectValue carrying
	// the INNER's alias, because the expression passed its inner's flowed
	// object through. That was the falsehood: a projection outputs its
	// projected columns, not its inner's row, and stating the inner's row is
	// what let a reader refuse to serve a source-relative ordinal against a
	// multi-leg row.
	rv, ok := p.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("GetResultValue() = %T, want *values.RecordConstructorValue — a projection "+
			"must state its own columns, not pass its inner's row through", p.GetResultValue())
	}
	if len(rv.Fields) != 2 {
		t.Fatalf("stated %d field(s), want 2 (one per projected column)", len(rv.Fields))
	}
	rt, ok := rv.Type().(*values.RecordType)
	if !ok {
		t.Fatalf("result value type %T, want *values.RecordType", rv.Type())
	}
	for i, f := range rt.Fields {
		if f.Name == "" {
			t.Errorf("slot %d is UNNAMED; every slot must carry a name", i)
		}
	}
}

func TestLogicalProjection_DefensiveCopy(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	src := []values.Value{values.NewBooleanValue(true)}
	p := mustExpression(NewLogicalProjectionExpression(src, q))
	src[0] = values.NewBooleanValue(false)
	if got, err := p.GetProjectedValues()[0].(*values.BooleanValue).Evaluate(nil); err != nil || got != true {
		t.Fatal("constructor failed to defensively copy projection list")
	}
	got := p.GetProjectedValues()
	got[0] = values.NewBooleanValue(false)
	if preserved, err := p.GetProjectedValues()[0].(*values.BooleanValue).Evaluate(nil); err != nil || preserved != true {
		t.Fatal("GetProjectedValues exposed mutable semantic identity")
	}
}

func TestLogicalProjection_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	v1 := values.NewBooleanValue(true)
	v2 := values.NewBooleanValue(false)
	p1 := mustExpression(NewLogicalProjectionExpression([]values.Value{v1}, q))
	p1Twin := mustExpression(NewLogicalProjectionExpression([]values.Value{values.NewBooleanValue(true)}, q))
	p2 := mustExpression(NewLogicalProjectionExpression([]values.Value{v2}, q))
	pBoth := mustExpression(NewLogicalProjectionExpression([]values.Value{v1, v2}, q))
	if !p1.EqualsWithoutChildren(p1Twin, EmptyAliasMap()) {
		t.Fatal("structurally identical projections reported unequal")
	}
	if p1.EqualsWithoutChildren(p2, EmptyAliasMap()) {
		t.Fatal("projections with different values reported equal")
	}
	if p1.EqualsWithoutChildren(pBoth, EmptyAliasMap()) {
		t.Fatal("projections with different lengths reported equal")
	}
	if p1.EqualsWithoutChildren(leaf, EmptyAliasMap()) {
		t.Fatal("projection reported equal to non-projection expression")
	}
}

func TestLogicalProjection_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	v := values.NewBooleanValue(true)
	p1 := mustExpression(NewLogicalProjectionExpression([]values.Value{v}, q))
	p2 := mustExpression(NewLogicalProjectionExpression([]values.Value{values.NewBooleanValue(true)}, q))
	if p1.HashCodeWithoutChildren() != p2.HashCodeWithoutChildren() {
		t.Fatal("structurally equal projections produced different hash codes")
	}
}

func TestLogicalProjection_AliasesAreSemanticIdentity(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	readA := testFieldAt("A", 0, values.NotNullLong)

	aliased := mustExpression(NewLogicalProjectionExpressionWithAliases(
		[]values.Value{readA}, []string{"OUTPUT_ALIAS"}, q))

	aliasedTwin := mustExpression(NewLogicalProjectionExpressionWithAliases(
		[]values.Value{readA}, []string{"OUTPUT_ALIAS"}, q))

	renamed := mustExpression(NewLogicalProjectionExpressionWithAliases(
		[]values.Value{readA}, []string{"OTHER_ALIAS"}, q))

	// TWO SPELLINGS OF AN ALIAS ARE TWO ALIASES. This arm used to pass
	// `output_alias` and `OUTPUT_ALIAS` and require them EQUAL, because the
	// output-name authority folded and both minted OUTPUT_ALIAS. It does not
	// fold any more: an alias arrives already normalized by the parse capture,
	// so a case difference surviving to this layer means the two came from
	// DIFFERENT quoted aliases and name different columns. The direction of
	// the guard inverts with the expected value.
	caseTwin := mustExpression(NewLogicalProjectionExpressionWithAliases(
		[]values.Value{readA}, []string{"output_alias"}, q))
	if aliased.EqualsWithoutChildren(caseTwin, EmptyAliasMap()) {
		t.Fatal(`AS "output_alias" and AS "OUTPUT_ALIAS" name different columns and must not be one identity`)
	}

	if !aliased.EqualsWithoutChildren(aliasedTwin, EmptyAliasMap()) {
		t.Fatal("aliases producing the same executor-visible output name reported unequal")
	}
	if aliased.HashCodeWithoutChildren() != aliasedTwin.HashCodeWithoutChildren() {
		t.Fatal("equal aliased projections produced different hash codes")
	}
	if aliased.EqualsWithoutChildren(renamed, EmptyAliasMap()) {
		t.Fatal("projections with different executor-visible output names reported equal")
	}
	if aliased.HashCodeWithoutChildren() == renamed.HashCodeWithoutChildren() {
		t.Fatal("different executor-visible output names produced the same projection hash")
	}
}

func TestLogicalProjection_InternalCorrelationNamesRemainAliasMapped(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	inner := ForEachQuantifier(InitialOf(leaf))
	q0 := values.NamedCorrelationIdentifier("q0")
	q1 := values.NamedCorrelationIdentifier("q1")
	mapping := AliasMapOf(q0, q1)

	testCases := []struct {
		name  string
		build func(values.CorrelationIdentifier) values.Value
	}{
		{
			name: "field access",
			build: func(alias values.CorrelationIdentifier) values.Value {
				return testCorrelatedField(alias, "ID", values.NotNullLong)
			},
		},
		{
			name: "scalar subquery",
			build: func(alias values.CorrelationIdentifier) values.Value {
				return values.NewScalarSubqueryValue(alias, values.NotNullLong)
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leftValue, rightValue := tc.build(q0), tc.build(q1)
			left := mustExpression(NewLogicalProjectionExpression([]values.Value{leftValue}, inner))
			right := mustExpression(NewLogicalProjectionExpression([]values.Value{rightValue}, inner))
			if !left.EqualsWithoutChildren(right, mapping) {
				t.Fatal("projection identity treated alpha-renamed internal binders as output-schema aliases")
			}
			if left.HashCodeWithoutChildren() != right.HashCodeWithoutChildren() {
				t.Fatal("alpha-renamed projection twins must share an alias-invariant memo hash")
			}
			if left.EqualsWithoutChildren(right, EmptyAliasMap()) {
				t.Fatal("internal correlations must remain distinct without the establishing AliasMap")
			}
		})
	}
}

func TestLogicalProjection_EmptyAliasesAreEquivalent(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	projected := []values.Value{values.NewBooleanValue(true)}

	withoutAliases := mustExpression(NewLogicalProjectionExpression(projected, q))
	emptyAliases := mustExpression(NewLogicalProjectionExpressionWithAliases(projected, []string{}, q))
	blankPlaceholder := mustExpression(NewLogicalProjectionExpressionWithAliases(projected, []string{""}, q))

	for _, candidate := range []*LogicalProjectionExpression{emptyAliases, blankPlaceholder} {
		if !withoutAliases.EqualsWithoutChildren(candidate, EmptyAliasMap()) {
			t.Fatal("missing and empty aliases must use the same derived output-name semantics")
		}
		if withoutAliases.HashCodeWithoutChildren() != candidate.HashCodeWithoutChildren() {
			t.Fatal("semantically equal empty-alias representations produced different hashes")
		}
	}
}

func TestLogicalProjection_DerivedOutputNamesAreSemanticIdentity(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	readA := testFieldAt("A", 0, values.NotNullLong)
	readB := testFieldAt("B", 0, values.NotNullLong)
	if values.SemanticEqualsUnderAliasMap(readA, readB, values.EmptyAliasMap()) {
		t.Fatal("exact field identity must include the root descriptor")
	}

	testCases := []struct {
		name        string
		aliases     []string
		useAliasAPI bool
	}{
		{name: "nil"},
		{name: "empty slice", aliases: []string{}, useAliasAPI: true},
		{name: "trailing empty", aliases: []string{""}, useAliasAPI: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			build := func(v values.Value) *LogicalProjectionExpression {
				if tc.useAliasAPI {
					return mustExpression(NewLogicalProjectionExpressionWithAliases(
						[]values.Value{v}, tc.aliases, q))
				}
				return mustExpression(NewLogicalProjectionExpression([]values.Value{v}, q))
			}
			left, right := build(readA), build(readB)
			if left.EqualsWithoutChildren(right, EmptyAliasMap()) {
				t.Fatal("reads from different exact roots reported equal")
			}
			if left.HashCodeWithoutChildren() == right.HashCodeWithoutChildren() {
				t.Fatal("different derived output names produced the same projection hash")
			}
		})
	}
}

func TestLogicalProjection_NestedFieldNamesAreSemanticIdentity(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	one := func() values.Value {
		return &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}
	}
	leftValue := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  testFieldAt("A", 0, values.NotNullLong),
		Right: one(),
	}
	rightValue := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  testFieldAt("B", 0, values.NotNullLong),
		Right: one(),
	}
	if values.SemanticEqualsUnderAliasMap(leftValue, rightValue, values.EmptyAliasMap()) {
		t.Fatal("arithmetic over fields from different exact roots reported equal")
	}

	left := mustExpression(NewLogicalProjectionExpression([]values.Value{leftValue}, q))
	right := mustExpression(NewLogicalProjectionExpression([]values.Value{rightValue}, q))
	if left.EqualsWithoutChildren(right, EmptyAliasMap()) {
		t.Fatal("nested baked field display names changed output schema but compared equal")
	}
	if left.HashCodeWithoutChildren() == right.HashCodeWithoutChildren() {
		t.Fatal("different nested field display names produced the same projection hash")
	}
}

func TestLogicalProjection_AliasesDefensiveCopy(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	inputAliases := []string{"ORIGINAL"}
	p := mustExpression(NewLogicalProjectionExpressionWithAliases(
		[]values.Value{values.NewBooleanValue(true)}, inputAliases, q))

	inputAliases[0] = "MUTATED_INPUT"
	got := p.GetAliases()
	if len(got) != 1 || got[0] != "ORIGINAL" {
		t.Fatalf("constructor retained mutable alias input: got %v", got)
	}
	got[0] = "MUTATED_GETTER"
	if second := p.GetAliases(); len(second) != 1 || second[0] != "ORIGINAL" {
		t.Fatalf("GetAliases exposed mutable semantic identity: got %v", second)
	}
}
