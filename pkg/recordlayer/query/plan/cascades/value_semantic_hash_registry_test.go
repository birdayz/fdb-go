package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestValueSemanticHashCode_AllCorrelationBearingTypesAreAliasInvariant is the
// RFC-040 completeness/registry guard: EVERY value type that
// embeds a CorrelationIdentifier must hash with the alias EXCLUDED, else it
// silently breaks the hash↔equality consistency the memo dedup gate relies on.
// Each entry builds two instances differing ONLY by alias and asserts equal
// hashes. A type that falls through to the structural default (hashing Name(),
// which may include the alias) fails here. Adding a new correlation-bearing
// value type without a ValueSemanticHashCode case must fail this test.
func TestValueSemanticHashCode_AllCorrelationBearingTypesAreAliasInvariant(t *testing.T) {
	t.Parallel()
	a := values.NamedCorrelationIdentifier("alias_a")
	b := values.NamedCorrelationIdentifier("alias_b")
	rowType := semanticHashRowType()

	cases := []struct {
		name string
		mk   func(testing.TB, values.CorrelationIdentifier) values.Value
	}{
		{"QuantifiedObjectValue", func(t testing.TB, c values.CorrelationIdentifier) values.Value {
			return requireSemanticHashQOV(t, c)
		}},
		{"QuantifiedRecordValue", func(_ testing.TB, c values.CorrelationIdentifier) values.Value {
			return values.NewQuantifiedRecordValue(c, rowType)
		}},
		{"ObjectValue", func(_ testing.TB, c values.CorrelationIdentifier) values.Value {
			return values.NewObjectValue(c, rowType)
		}},
		{"ConstantObjectValue", func(_ testing.TB, c values.CorrelationIdentifier) values.Value {
			return values.NewConstantObjectValue(c, "k", values.NotNullLong)
		}},
		{"ExistsValue", func(t testing.TB, c values.CorrelationIdentifier) values.Value {
			value, err := values.NewExistsValue(c, rowType)
			if err != nil {
				t.Fatalf("construct exact ExistsValue: %v", err)
			}
			return value
		}},
		{"ScalarSubqueryValue", func(_ testing.TB, c values.CorrelationIdentifier) values.Value {
			return values.NewScalarSubqueryValue(c, values.NotNullLong)
		}},
		{"UnmatchedAggregateValue", func(_ testing.TB, c values.CorrelationIdentifier) values.Value {
			return values.NewUnmatchedAggregateValue(c)
		}},
		{"IndexEntryObjectValue", func(_ testing.TB, c values.CorrelationIdentifier) values.Value {
			return values.NewIndexEntryObjectValue(c, values.TupleSourceKey, []int{0}, values.NotNullLong)
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			va := tc.mk(t, a)
			vb := tc.mk(t, b)
			if values.SemanticHashCode(va) != values.SemanticHashCode(vb) {
				t.Fatalf("%s: hash depends on the alias — must be alias-invariant (missing ValueSemanticHashCode case → falls to default)", tc.name)
			}
		})
	}
}
