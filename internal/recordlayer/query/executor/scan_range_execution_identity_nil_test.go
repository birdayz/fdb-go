package executor

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestScanRangeExecutionIdentityRejectsNilPlans(t *testing.T) {
	t.Parallel()

	flowed := scanIdentityResultType(values.NotNullLong)
	tests := []struct {
		name     string
		wantKind string
		build    func() (string, error)
	}{
		{
			name:     "index fields fetch",
			wantKind: "RecordQueryIndexPlan",
			build: func() (string, error) {
				return indexScanRangeFingerprintSaltFields(nil, recordlayer.IndexScanByValue, false, nil)
			},
		},
		{
			name:     "index fields covering",
			wantKind: "RecordQueryIndexPlan",
			build: func() (string, error) {
				return indexScanRangeFingerprintSaltFields(nil, recordlayer.IndexScanByValue, true, []string{"A"})
			},
		},
		{
			name:     "index fields with unknown flowed type",
			wantKind: "RecordQueryIndexPlan",
			build: func() (string, error) {
				return indexScanRangeFingerprintSaltFieldsWithFlowedType(
					nil, recordlayer.IndexScanByValue, false, nil, values.UnknownType,
				)
			},
		},
		{
			name:     "index fields with concrete flowed type",
			wantKind: "RecordQueryIndexPlan",
			build: func() (string, error) {
				return indexScanRangeFingerprintSaltFieldsWithFlowedType(
					nil, recordlayer.IndexScanByValue, true, []string{"A"}, flowed,
				)
			},
		},
		{
			name:     "index salt",
			wantKind: "RecordQueryIndexPlan",
			build: func() (string, error) {
				return indexScanRangeFingerprintSalt(nil, recordlayer.IndexScanByValue)
			},
		},
		{
			name:     "legacy index salt",
			wantKind: "RecordQueryIndexPlan",
			build: func() (string, error) {
				return legacyIndexScanRangeFingerprintSaltFields(nil, recordlayer.IndexScanByValue, false, nil)
			},
		},
		{
			name:     "primary scan",
			wantKind: "RecordQueryScanPlan",
			build: func() (string, error) {
				return primaryScanRangeFingerprintSalt(nil)
			},
		},
		{
			name:     "covering index",
			wantKind: "RecordQueryCoveringIndexPlan",
			build: func() (string, error) {
				return coveringIndexScanRangeFingerprintSalt(nil, recordlayer.IndexScanByValue)
			},
		},
		{
			name:     "covering index with nil inner plan",
			wantKind: "RecordQueryIndexPlan",
			build: func() (string, error) {
				return coveringIndexScanRangeFingerprintSalt(&plans.RecordQueryCoveringIndexPlan{}, recordlayer.IndexScanByValue)
			},
		},
		{
			name:     "covering index value",
			wantKind: "RecordQueryCoveringIndexValuePlan",
			build: func() (string, error) {
				return coveringIndexValueScanRangeFingerprintSalt(nil, recordlayer.IndexScanByValue)
			},
		},
		{
			name:     "covering index value with nil inner plan",
			wantKind: "RecordQueryCoveringIndexValuePlan.indexPlan",
			build: func() (string, error) {
				return coveringIndexValueScanRangeFingerprintSalt(&plans.RecordQueryCoveringIndexValuePlan{}, recordlayer.IndexScanByValue)
			},
		},
		{
			name:     "aggregate index",
			wantKind: "RecordQueryAggregateIndexPlan",
			build: func() (string, error) {
				return aggregateScanRangeFingerprintSalt(nil, recordlayer.IndexTypeSum, recordlayer.IndexScanByGroup, 1, 1)
			},
		},
		{
			name:     "aggregate index with nil inner plan",
			wantKind: "RecordQueryAggregateIndexPlan.indexPlan",
			build: func() (string, error) {
				return aggregateScanRangeFingerprintSalt(&plans.RecordQueryAggregateIndexPlan{}, recordlayer.IndexTypeSum, recordlayer.IndexScanByGroup, 1, 1)
			},
		},
		{
			name:     "vector index",
			wantKind: "RecordQueryVectorIndexPlan",
			build: func() (string, error) {
				return vectorScanRangeFingerprintSalt(
					nil, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance,
					[]float64{1}, 1, 1, 16, recordlayer.TupleRange{},
				)
			},
		},
	}

	t.Cleanup(func() {
		if n := memoEntriesOf(nil); n != 0 {
			t.Fatalf("indexSaltMemo stored %d entries for nil plan, want 0", n)
		}
	})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			salt, err := tc.build()
			if salt != "" {
				t.Fatalf("salt = %x, want empty on error", salt)
			}
			var planErr *InvalidScanRangeExecutionIdentityPlanError
			if !errors.As(err, &planErr) {
				t.Fatalf("error = %T(%v), want *InvalidScanRangeExecutionIdentityPlanError", err, err)
			}
			if planErr.PlanKind != tc.wantKind {
				t.Fatalf("PlanKind = %q, want %q", planErr.PlanKind, tc.wantKind)
			}
		})
	}
}

func TestIndexScanRangeFingerprintSaltFieldsValidPlanPositiveControl(t *testing.T) {
	t.Parallel()

	plan := indexIdentityTestPlan(
		"idx_nil_positive_control",
		false,
		[]string{"A"},
		values.NotNullDouble,
		[]string{"T"},
		scanIdentityResultType(values.NotNullLong),
	)

	directSalt, err := indexScanRangeFingerprintSaltFieldsWithFlowedType(
		plan, recordlayer.IndexScanByValue, true, []string{"A"}, plan.GetFlowedType(),
	)
	if err != nil {
		t.Fatalf("direct salt: %v", err)
	}
	if len(directSalt) != sha256DigestSize {
		t.Fatalf("direct salt length = %d, want %d", len(directSalt), sha256DigestSize)
	}

	memoSalt1, err := indexScanRangeFingerprintSaltFields(
		plan, recordlayer.IndexScanByValue, true, []string{"A"},
	)
	if err != nil {
		t.Fatalf("cold memo salt: %v", err)
	}
	if memoSalt1 != directSalt {
		t.Fatalf("cold memo salt %x != direct salt %x", memoSalt1, directSalt)
	}

	memoSalt2, err := indexScanRangeFingerprintSaltFields(
		plan, recordlayer.IndexScanByValue, true, []string{"A"},
	)
	if err != nil {
		t.Fatalf("warm memo salt: %v", err)
	}
	if memoSalt2 != directSalt {
		t.Fatalf("warm memo salt %x != direct salt %x", memoSalt2, directSalt)
	}
	if n := memoEntriesOf(plan); n != 1 {
		t.Fatalf("memo entries = %d, want 1", n)
	}
}
