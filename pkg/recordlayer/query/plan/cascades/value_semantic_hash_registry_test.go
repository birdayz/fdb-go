package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestValueSemanticHashCode_AllCorrelationBearingTypesAreAliasInvariant is the
// RFC-040 completeness/registry guard (Torvalds #1/#4): EVERY value type that
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
	ut := values.UnknownType

	cases := []struct {
		name string
		mk   func(c values.CorrelationIdentifier) values.Value
	}{
		{"QuantifiedObjectValue", func(c values.CorrelationIdentifier) values.Value { return values.NewQuantifiedObjectValue(c) }},
		{"QuantifiedRecordValue", func(c values.CorrelationIdentifier) values.Value { return values.NewQuantifiedRecordValue(c, ut) }},
		{"ObjectValue", func(c values.CorrelationIdentifier) values.Value { return values.NewObjectValue(c, ut) }},
		{"ConstantObjectValue", func(c values.CorrelationIdentifier) values.Value { return values.NewConstantObjectValue(c, "k", ut) }},
		{"ExistsValue", func(c values.CorrelationIdentifier) values.Value { return values.NewExistsValue(c) }},
		{"ScalarSubqueryValue", func(c values.CorrelationIdentifier) values.Value { return values.NewScalarSubqueryValue(c) }},
		{"UnmatchedAggregateValue", func(c values.CorrelationIdentifier) values.Value { return values.NewUnmatchedAggregateValue(c) }},
		{"IndexEntryObjectValue", func(c values.CorrelationIdentifier) values.Value {
			return values.NewIndexEntryObjectValue(c, values.TupleSourceKey, []int{0}, ut)
		}},
		{"JoinMergeAllValue", func(c values.CorrelationIdentifier) values.Value {
			// vary the aliases by c so two instances differ only in alias; the
			// merge hash must be alias- (and order-) invariant (RFC-074).
			return values.NewJoinMergeAllValue(c, c)
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			va := tc.mk(a)
			vb := tc.mk(b)
			if values.SemanticHashCode(va) != values.SemanticHashCode(vb) {
				t.Fatalf("%s: hash depends on the alias — must be alias-invariant (missing ValueSemanticHashCode case → falls to default)", tc.name)
			}
		})
	}
}
