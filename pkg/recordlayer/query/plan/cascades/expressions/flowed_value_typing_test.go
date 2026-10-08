package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestExactQOVTypeParticipatesInIdentity(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("Q")
	one := mustExpression(values.NewQuantifiedObjectValue(alias, values.NotNullLong))
	two := mustExpression(values.NewQuantifiedObjectValue(alias, values.NotNullString))
	if values.EqualsWithoutChildren(one, two) {
		t.Fatal("same-correlation QOVs with different exact types compared equal")
	}
	if values.SemanticEqualsUnderAliasMap(one, two, values.EmptyAliasMap()) {
		t.Fatal("alias-aware equality erased the QOV exact-type discriminator")
	}
	if values.SemanticHashCode(one) == values.SemanticHashCode(two) {
		t.Fatal("differently typed QOVs produced the same semantic hash")
	}
}
