package query

// RFC-173 S4 F2-LEFT scope boundary — isScanFamilyLeg is the logical proxy for
// the executor's legIsOrdinalSafe. It decides whether a LEFT projected-EXISTS
// fold ordinalizes (both legs scan-family → the commit-2 positional seed + a
// JoinLeftOuter NLJ, no name-model producer) or declines (a buried box falls to
// the name-model :698 path the demolition removes). This white-box pin fixes the
// exact boundary: a functional test that only checks the scan-leg rows is BLIND
// to the boundary silently widening to admit a join leg (which would re-introduce
// the :698 producer for the outer-join fold).

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/logical"
)

func TestRFC173F2Left_IsScanFamilyLeg(t *testing.T) {
	t.Parallel()
	scan := func() logical.LogicalOperator { return &logical.LogicalScan{Table: "T", Alias: "t"} }
	cases := []struct {
		name string
		op   logical.LogicalOperator
		want bool
	}{
		{"bare scan", scan(), true},
		{"filter over scan", &logical.LogicalFilter{Input: scan()}, true},
		{"nested filters over scan", &logical.LogicalFilter{Input: &logical.LogicalFilter{Input: scan()}}, true},
		// The buried box: the preserved leg is a JOIN → NOT ordinal-safe → decline.
		{"join (buried box)", &logical.LogicalJoin{Left: scan(), Right: scan()}, false},
		{"filter over join", &logical.LogicalFilter{Input: &logical.LogicalJoin{Left: scan(), Right: scan()}}, false},
		// Other non-scan leg shapes are conservatively rejected (decline, never
		// a silent name-model fold).
		{"unnest leg", &logical.LogicalUnnest{}, false},
		{"union leg", &logical.LogicalUnion{}, false},
		{"aggregate leg", &logical.LogicalAggregate{}, false},
		{"distinct leg", &logical.LogicalDistinct{}, false},
		// A filter with NO input (defensive): the loop hits a nil default → false.
		{"filter over nil", &logical.LogicalFilter{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isScanFamilyLeg(tc.op); got != tc.want {
				t.Errorf("isScanFamilyLeg(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
