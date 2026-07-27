package executor

// RFC-180 A3/A5 — FDB integration pins for the IN-join and IN-union per-row
// continuations, one row per transaction (the strictest per-row paging, same
// harness as the A6 aggregate/sort pins): every emitted row's continuation is
// executed in a FRESH transaction and the concatenation of pages must equal
// the unpaginated output exactly. Before A3/A5 both executors DISCARDED the
// incoming continuation (every page replayed the plan from row 0), so these
// loops never converged.
//
// Java references: RecordQueryInJoinPlan.executePlan (:112-131,
// flatMapPipelined over the in-values ListCursor) and
// RecordQueryInUnionPlan.executePlan (:150-175, UnionCursor over per-binding
// child factories with the UnionContinuation per-child states).

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// priceProbeIndexPlan builds the IN legs' shared inner: an order_price_idx
// equality probe whose comparand is the IN binding (QOV(binding) evaluates to
// the bound in-value through the scan bind context) — the shape the in-join /
// in-union rules push the rewritten `price = binding` SARG into.
func priceProbeIndexPlan(t *testing.T, bindingID values.CorrelationIdentifier) plans.RecordQueryPlan {
	t.Helper()
	res := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: values.NewQuantifiedObjectValue(bindingID),
	})
	if !res.Ok {
		t.Fatal("building the binding equality comparison range failed")
	}
	return plans.NewRecordQueryIndexPlan(
		"order_price_idx",
		[]*predicates.ComparisonRange{res.Range},
		[]string{"Order"},
		nil,
		false,
	)
}

// renderOrderID renders a fetched Order row by its ORDER_ID slot (ordinal 0).
func renderOrderID(qr QueryResult) (string, error) {
	id, ok := qr.Positional.Get(0)
	if !ok {
		return "", fmt.Errorf("order row missing slot 0: %+v", qr.Positional)
	}
	return fmt.Sprintf("%v", id), nil
}

// insertInUnionOrders inserts the shared 5-row fixture: two orders per price
// bucket 50 and 150, one at 250.
func insertInUnionOrders(t *testing.T, store *recordlayer.FDBRecordStore) {
	t.Helper()
	insertOrders(t, store,
		&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50)},
		&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(150)},
		&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(150)},
		&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(250)},
	)
}

// TestIntegration_InJoinRowContinuation_OneRowPerTx pins the IN-join flatMap
// continuation end-to-end: resuming from EVERY emitted row's continuation
// (fresh transaction each time) walks (outer in-value index, inner index-scan
// position) exactly — no replayed leg, no dropped leg tail. The in-values are
// deliberately NOT sorted: output order is in-value order (leg concatenation),
// pinning that the resume lands on the correct outer index.
func TestIntegration_InJoinRowContinuation_OneRowPerTx(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	insertInUnionOrders(t, store)

	bindingName := "in_q_a3join"
	bindingID := values.NamedCorrelationIdentifier(bindingName)
	makePlan := func() plans.RecordQueryPlan {
		p := plans.NewRecordQueryInJoinPlan(priceProbeIndexPlan(t, bindingID), bindingName, false, false)
		p.SetInValues([]any{int64(150), int64(50), int64(250)})
		return p
	}

	got := onePerTxRows(t, store, makePlan, renderOrderID)
	want := "[3 4 1 2 5]" // leg 150 → (3,4), leg 50 → (1,2), leg 250 → (5)
	if fmt.Sprint(got) != want {
		t.Fatalf("IN-join paged one row per tx = %v, want %v (no duplicated/dropped rows across per-row resumes)", got, want)
	}
}

// TestIntegration_InUnionRowContinuation_OneRowPerTx pins the ordered IN-union
// (in_union plan) per-child continuation states end-to-end: the merge resumes
// from EVERY emitted row's UnionContinuation with each leg at its own saved
// position (consumed legs past their rows, peeked legs before theirs) — the
// paged output equals the unpaged merge exactly, in comparison-key order.
func TestIntegration_InUnionRowContinuation_OneRowPerTx(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	insertInUnionOrders(t, store)

	bindingName := "in_q_a5union"
	bindingID := values.NamedCorrelationIdentifier(bindingName)
	// Comparison keys: (PRICE, ORDER_ID) — the fetched Order row's ordinals
	// (order_id@0, price@2), the index's own (price, pk) entry order.
	compKeys := []values.Value{
		values.NewFieldValueWithResolvedOrdinal("PRICE", 2, values.UnknownType),
		values.NewFieldValueWithResolvedOrdinal("ORDER_ID", 0, values.NullableLong),
	}
	makePlan := func() plans.RecordQueryPlan {
		p := plans.NewRecordQueryInUnionPlan(priceProbeIndexPlan(t, bindingID), []string{bindingName}, compKeys, false)
		p.SetInSources([][]any{{int64(250), int64(50), int64(150)}})
		return p
	}

	got := onePerTxRows(t, store, makePlan, renderOrderID)
	want := "[1 2 3 4 5]" // merged by (price, order_id) regardless of in-value order
	if fmt.Sprint(got) != want {
		t.Fatalf("IN-union paged one row per tx = %v, want %v (per-child states must resume the merge exactly)", got, want)
	}
}

// TestIntegration_InUnionDuplicateValues_DedupAcrossResumes pins the
// advance-all-equal dedup under per-row paging with DUPLICATE in-values (two
// legs scanning the identical price bucket): every tied pair is collapsed to
// one emitted row per step, and a resume landing mid-tie (one leg consumed,
// the twin peeked) must not re-emit or drop the tied row.
func TestIntegration_InUnionDuplicateValues_DedupAcrossResumes(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	insertInUnionOrders(t, store)

	bindingName := "in_q_a5dup"
	bindingID := values.NamedCorrelationIdentifier(bindingName)
	compKeys := []values.Value{
		values.NewFieldValueWithResolvedOrdinal("PRICE", 2, values.UnknownType),
		values.NewFieldValueWithResolvedOrdinal("ORDER_ID", 0, values.NullableLong),
	}
	makePlan := func() plans.RecordQueryPlan {
		p := plans.NewRecordQueryInUnionPlan(priceProbeIndexPlan(t, bindingID), []string{bindingName}, compKeys, false)
		p.SetInSources([][]any{{int64(50), int64(150), int64(50)}}) // 50 twice: legs 0 and 2 are identical scans
		return p
	}

	got := onePerTxRows(t, store, makePlan, renderOrderID)
	want := "[1 2 3 4]" // bucket 50 (rows 1,2) exactly once despite two legs; bucket 150 (3,4)
	if fmt.Sprint(got) != want {
		t.Fatalf("IN-union with duplicate in-values paged one row per tx = %v, want %v (mid-tie resumes must neither duplicate nor drop)", got, want)
	}
}
