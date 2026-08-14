package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestSpecializedMatchCandidatesDeclineUnresolvedFlowedType(t *testing.T) {
	t.Parallel()

	t.Run("windowed", func(t *testing.T) {
		candidate := NewWindowedIndexScanMatchCandidate(
			"rank_idx", []string{"T"}, nil, nil,
			values.CorrelationIdentifier{}, values.CorrelationIdentifier{}, nil, nil,
			values.UnknownType, false, nil,
		)
		if plan := candidate.ToScanPlan(nil, false); plan != nil {
			t.Fatalf("ToScanPlan() = %T, want declined candidate", plan)
		}
	})

	t.Run("vector", func(t *testing.T) {
		candidate := NewVectorIndexScanMatchCandidate(
			"vector_idx", []string{"T"}, []string{"embedding"}, 0,
			values.DistanceEuclidean, values.UnknownType, false, nil,
		)
		if plan := candidate.ToScanPlan(nil, false); plan != nil {
			t.Fatalf("ToScanPlan() = %T, want declined candidate", plan)
		}
	})
}
