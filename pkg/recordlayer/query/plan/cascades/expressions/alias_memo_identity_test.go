package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Probe: does an ALIAS-ONLY difference already split the memo on master?
func TestAliasOnlyDifferenceSplitsTheMemoDeliberately(t *testing.T) {
	t.Parallel()

	k := &values.FieldValue{Field: "K"}
	a := NewLogicalProjectionExpressionWithAliases([]values.Value{k}, []string{"A"}, Quantifier{})
	b := NewLogicalProjectionExpressionWithAliases([]values.Value{k}, []string{"B"}, Quantifier{})

	eq := a.EqualsWithoutChildren(b, &AliasMap{})
	ha, hb := a.HashCodeWithoutChildren(), b.HashCodeWithoutChildren()
	t.Logf("PROBE alias-only: EqualsWithoutChildren=%v hashA=%d hashB=%d hashEqual=%v", eq, ha, hb, ha == hb)

	none1 := NewLogicalProjectionExpression([]values.Value{k}, Quantifier{})
	none2 := NewLogicalProjectionExpression([]values.Value{k}, Quantifier{})
	t.Logf("PROBE no-alias control: EqualsWithoutChildren=%v hashEqual=%v",
		none1.EqualsWithoutChildren(none2, &AliasMap{}),
		none1.HashCodeWithoutChildren() == none2.HashCodeWithoutChildren())
}
