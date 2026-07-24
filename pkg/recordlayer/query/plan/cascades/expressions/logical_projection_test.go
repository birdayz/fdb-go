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
	v2 := values.NewNullValue(values.UnknownType)
	p := NewLogicalProjectionExpression([]values.Value{v1, v2}, q)
	if got := p.GetProjectedValues(); len(got) != 2 {
		t.Fatalf("projected size=%d, want 2", len(got))
	}
	if p.GetInner().GetAlias() != q.GetAlias() {
		t.Fatal("inner alias mismatch")
	}
	if p.CanCorrelate() {
		t.Fatal("projection should not anchor a correlation")
	}
	// GetResultValue passes through to inner's flowed object — must
	// carry the inner's alias.
	resultCorr := p.GetResultValue().(*values.QuantifiedObjectValue).GetCorrelatedTo()
	if _, ok := resultCorr[q.GetAlias()]; !ok {
		t.Fatal("GetResultValue does not carry the inner Quantifier's alias")
	}
}

func TestLogicalProjection_DefensiveCopy(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	src := []values.Value{values.NewBooleanValue(true)}
	p := NewLogicalProjectionExpression(src, q)
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
	p1 := NewLogicalProjectionExpression([]values.Value{v1}, q)
	p1Twin := NewLogicalProjectionExpression([]values.Value{values.NewBooleanValue(true)}, q)
	p2 := NewLogicalProjectionExpression([]values.Value{v2}, q)
	pBoth := NewLogicalProjectionExpression([]values.Value{v1, v2}, q)
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
	p1 := NewLogicalProjectionExpression([]values.Value{v}, q)
	p2 := NewLogicalProjectionExpression([]values.Value{values.NewBooleanValue(true)}, q)
	if p1.HashCodeWithoutChildren() != p2.HashCodeWithoutChildren() {
		t.Fatal("structurally equal projections produced different hash codes")
	}
}

func TestLogicalProjection_AliasesAreSemanticIdentity(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	readA := values.NewFieldValueWithResolvedOrdinal("A", 0, values.UnknownType)
	readB := values.NewFieldValueWithResolvedOrdinal("B", 0, values.UnknownType)
	if !values.SemanticEqualsUnderAliasMap(readA, readB, values.AliasMap{}) {
		t.Fatal("test requires same-ordinal baked reads to be semantically equal")
	}

	aliased := NewLogicalProjectionExpressionWithAliases(
		[]values.Value{readA}, []string{"output_alias"}, q)
	aliasedTwin := NewLogicalProjectionExpressionWithAliases(
		[]values.Value{readB}, []string{"OUTPUT_ALIAS"}, q)
	renamed := NewLogicalProjectionExpressionWithAliases(
		[]values.Value{readB}, []string{"OTHER_ALIAS"}, q)

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
			name:  "quantified object",
			build: func(alias values.CorrelationIdentifier) values.Value { return values.NewQuantifiedObjectValue(alias) },
		},
		{
			name: "scalar subquery",
			build: func(alias values.CorrelationIdentifier) values.Value {
				return values.NewScalarSubqueryValue(alias, values.UnknownType)
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			leftValue, rightValue := tc.build(q0), tc.build(q1)
			if values.OutputColumnName(leftValue, "") == values.OutputColumnName(rightValue, "") {
				t.Fatal("test requires the debug-derived names to expose different internal correlation spellings")
			}
			left := NewLogicalProjectionExpression([]values.Value{leftValue}, inner)
			right := NewLogicalProjectionExpression([]values.Value{rightValue}, inner)
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

	withoutAliases := NewLogicalProjectionExpression(projected, q)
	emptyAliases := NewLogicalProjectionExpressionWithAliases(projected, []string{}, q)
	blankPlaceholder := NewLogicalProjectionExpressionWithAliases(projected, []string{""}, q)

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
	readA := values.NewFieldValueWithResolvedOrdinal("A", 0, values.UnknownType)
	readB := values.NewFieldValueWithResolvedOrdinal("B", 0, values.UnknownType)
	if !values.SemanticEqualsUnderAliasMap(readA, readB, values.AliasMap{}) {
		t.Fatal("test requires same-ordinal baked reads to be semantically equal")
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
					return NewLogicalProjectionExpressionWithAliases(
						[]values.Value{v}, tc.aliases, q)
				}
				return NewLogicalProjectionExpression([]values.Value{v}, q)
			}
			left, right := build(readA), build(readB)
			if left.EqualsWithoutChildren(right, EmptyAliasMap()) {
				t.Fatal("semantic-equal reads with different derived output names reported equal")
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
		Left:  values.NewFieldValueWithResolvedOrdinal("A", 0, values.UnknownType),
		Right: one(),
	}
	rightValue := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  values.NewFieldValueWithResolvedOrdinal("B", 0, values.UnknownType),
		Right: one(),
	}
	if !values.SemanticEqualsUnderAliasMap(leftValue, rightValue, values.AliasMap{}) {
		t.Fatal("test requires same-shape arithmetic over same-ordinal reads to be semantically equal")
	}

	left := NewLogicalProjectionExpression([]values.Value{leftValue}, q)
	right := NewLogicalProjectionExpression([]values.Value{rightValue}, q)
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
	p := NewLogicalProjectionExpressionWithAliases(
		[]values.Value{values.NewBooleanValue(true)}, inputAliases, q)

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
