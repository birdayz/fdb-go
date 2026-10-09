package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRequireFlowedObjectValueCachesPerWidening: the QOV prototype is cached
// per member set and per widening, so a plain and a null-on-empty quantifier
// over one group each keep their own nullability whichever asks first, and a
// member change rederives it.
func TestRequireFlowedObjectValueCachesPerWidening(t *testing.T) {
	t.Parallel()
	for _, plainFirst := range []bool{true, false} {
		ref := InitialOf(mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
		plain := ForEachQuantifier(ref)
		widened := ForEachNullOnEmptyQuantifier(ref)
		order := []Quantifier{plain, widened}
		if !plainFirst {
			order = []Quantifier{widened, plain}
		}
		for round := 0; round < 2; round++ {
			for _, q := range order {
				qov := mustExpression(q.RequireFlowedObjectValue())
				if qov.Correlation() != q.GetAlias() {
					t.Fatalf("QOV correlates to %v, want the quantifier's %v", qov.Correlation(), q.GetAlias())
				}
				want := mustExpression(values.NewQuantifiedObjectValue(q.GetAlias(), mustExpression(q.GetFlowedObjectType())))
				if !values.SemanticEqualsUnderAliasMap(qov, want, values.EmptyAliasMap()) || qov.FlowedType().IsNullable() != q.IsNullOnEmpty() {
					t.Fatalf("plainFirst=%t round %d: QOV %v (nullable %t), want %v (null-on-empty %t)",
						plainFirst, round, qov, qov.FlowedType().IsNullable(), want, q.IsNullOnEmpty())
				}
			}
		}
		other := &values.RecordType{Fields: []values.Field{{Name: "NAME", Ordinal: 0, FieldType: values.NotNullString}}}
		ref.ClearFinalMembers() // a member change other than an append
		before := mustExpression(plain.RequireFlowedObjectValue())
		ref.members = []RelationalExpression{mustExpression(NewFullUnorderedScanExpression([]string{"T"}, other))}
		ref.memberVersion++
		after := mustExpression(plain.RequireFlowedObjectValue())
		if values.SemanticEqualsUnderAliasMap(before, after, values.EmptyAliasMap()) {
			t.Fatal("a member change kept the previous member set's QOV prototype")
		}
	}
}
