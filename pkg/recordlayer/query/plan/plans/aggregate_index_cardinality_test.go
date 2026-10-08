package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// TestAggregateIndexPlan_CardinalityReadsCandidateEvidence pins Java's
// CardinalitiesVisitor.visitRecordQueryAggregateIndexPlan: a plan with no
// candidate evidence has an unknown maximum, an ungrouped candidate yields at
// most one row, and a grouped one an unknown maximum (Java's equality-bound
// arm compares a record value against per-column values and never fires).
func TestAggregateIndexPlan_CardinalityReadsCandidateEvidence(t *testing.T) {
	t.Parallel()
	plan := aggregateIdentityFixture(t)
	for name, tc := range map[string]struct {
		plan *RecordQueryAggregateIndexPlan
		want properties.Cardinalities
	}{
		"no_evidence": {plan, properties.UnknownMaxCardinality()},
		"ungrouped":   {plan.WithCandidateGroupingCount(0), properties.AtMostOne()},
		"grouped":     {plan.WithCandidateGroupingCount(2), properties.UnknownMaxCardinality()},
	} {
		got := tc.plan.ProvenCardinalities(nil)
		if !got.Equal(tc.want) {
			t.Errorf("%s: cardinalities = %v, want %v", name, got, tc.want)
		}
	}
}
