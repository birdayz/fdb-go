package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/indexfetch"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestToScanPlan_StampsConfiguredFetchMethodOnByValueScansOnly pins the
// candidates' toEquivalentPlan: each passes
// planContext.getPlannerConfiguration().getIndexFetchMethod() to its
// RecordQueryIndexPlan, whose constructor keeps it only for a BY_VALUE scan
// (RecordQueryIndexPlan.java:248-251). The value candidate's scan keeps it
// (ValueIndexScanMatchCandidate.java:241,268); the aggregate's BY_GROUP
// (AggregateIndexMatchCandidate.java:420-430) and the windowed BY_RANK
// (WindowedIndexScanMatchCandidate.java:400-445) scans scan and fetch.
func TestToScanPlan_StampsConfiguredFetchMethodOnByValueScansOnly(t *testing.T) {
	t.Parallel()
	for _, method := range []indexfetch.Method{
		indexfetch.ScanAndFetch, indexfetch.UseRemoteFetch, indexfetch.UseRemoteFetchWithFallback,
	} {
		ctx := rightDeepPlanContext{cfg: PlannerConfiguration{IndexFetchMethod: method}}

		value := newKnownDistinctValueIndexCandidate("ORDERS$STATUS", []string{"ORDERS"},
			[]string{"STATUS"}, rfc219Aliases(1), testRecordRowType("ORDERS", "STATUS", "ID"), false, []string{"ID"})
		if got := stampedFetchMethod(t, value.ToScanPlan(ctx, nil, false)); got != method {
			t.Errorf("value candidate under %v stamped %v", method, got)
		}

		aggregate := NewAggregateIndexMatchCandidate("SUM_AMOUNT_BY_CUSTOMER", []string{"ORDERS"},
			[]string{"CUSTOMER_ID"}, expressions.AggSum, "AMOUNT",
			testRecordRowType("ORDERS", "CUSTOMER_ID", "AMOUNT", "ID"), []values.Type{values.NullableLong}, 1)
		if got := stampedFetchMethod(t, aggregate.ToScanPlan(ctx, nil, false)); got != indexfetch.ScanAndFetch {
			t.Errorf("aggregate (BY_GROUP) candidate under %v stamped %v, want SCAN_AND_FETCH", method, got)
		}

		windowed := NewWindowedIndexScanMatchCandidate("LEADER$SCORE_BY_TEAM", []string{"LEADER"},
			[]string{"TEAM", "SCORE"}, rfc219Aliases(1), values.UniqueCorrelationIdentifier(),
			values.UniqueCorrelationIdentifier(), rfc219Aliases(1), nil,
			testRecordRowType("LEADER", "TEAM", "SCORE", "ID"), false, []string{"ID"})
		if got := stampedFetchMethod(t, windowed.ToScanPlan(ctx, nil, false)); got != indexfetch.ScanAndFetch {
			t.Errorf("windowed (BY_RANK) candidate under %v stamped %v, want SCAN_AND_FETCH", method, got)
		}
	}
}

func stampedFetchMethod(t *testing.T, plan plans.RecordQueryPlan) indexfetch.Method {
	t.Helper()
	idx := extractIndexPlan(plan)
	if idx == nil {
		t.Fatalf("ToScanPlan returned %T, want a plan over a RecordQueryIndexPlan", plan)
	}
	return idx.GetIndexFetchMethod()
}

// TestStablePlanHash_IgnoresFetchMethod pins that the cost tie-break hash
// leaves the method out, fetching and covering alike: Java's cost model never
// reads it, so it cannot decide between cost-tied plans.
func TestStablePlanHash_IgnoresFetchMethod(t *testing.T) {
	t.Parallel()
	scan, err := plans.NewRecordQueryIndexPlan("ORDERS$STATUS", nil, []string{"ORDERS"},
		testRecordRowType("ORDERS", "STATUS", "ID"), true)
	if err != nil {
		t.Fatal(err)
	}
	scan = scan.WithIndexMetadata([]string{"STATUS"}, []string{"ID"}, false)
	hashes := map[uint64]bool{}
	coveringHashes := map[uint64]bool{}
	for _, method := range []indexfetch.Method{
		indexfetch.ScanAndFetch, indexfetch.UseRemoteFetch, indexfetch.UseRemoteFetchWithFallback,
	} {
		stamped := scan.WithIndexFetchMethod(method)
		hashes[stablePlanHash(stamped)] = true
		covering, err := plans.NewRecordQueryCoveringIndexPlan(stamped)
		if err != nil {
			t.Fatal(err)
		}
		coveringHashes[stablePlanHash(covering)] = true
	}
	if len(hashes) != 1 || len(coveringHashes) != 1 {
		t.Fatalf("tie-break hashes by fetch method: %d fetching, %d covering; want one each", len(hashes), len(coveringHashes))
	}
}
