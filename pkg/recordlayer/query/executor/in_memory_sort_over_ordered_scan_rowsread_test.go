package executor

// RFC-184 W2 — FDB rows-read validation of the InMemorySort finale: the
// cost==extraction fix (findBestPhysicalPlan for the sort's inner) PLUS the
// depth-rung gate (a redundant InMemorySort no longer wins the type-filter /
// fetch / distinct DEPTH tiebreaks — those abstain when a sort is present, so
// criterion #12 "fewer in-memory sorts" restores elimination).
//
// Two things are proven on real FDB, measured at the leaf cursor via the
// ScannedRecordsLimit tripwire (NOT the cost model's estimate):
//
//  1. FIX — a primary-key-ordered `ORDER BY pk` under a LIMIT elides the sort.
//     The elided `Limit(1, Scan)` (the shape the planner now picks — confirmed
//     separately by the corpus explain-differ, where pk_pushdown /
//     order_by_elimination#34 flip BACK to elimination) STREAMS and reads ≤5
//     rows. The materialized `Limit(1, InMemorySort(Scan))` (the pre-fix wrong
//     choice) reads the ENTIRE input, because executeInMemorySort clears the
//     limit before its inner. Same single min-PK result row — pure rows-read.
//
//  2. IMPROVEMENT — a selective index scan reads FEWER rows than the full scan
//     it replaces (the ~20 genuine full-scan→selective-index flips).

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestFDB_InMemorySortRowsRead_RFC184(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := setupStore(t)
	// setupStore keyed the store on the OUTER test's subspace; the subtests below
	// change t.Name(), so capture that subspace and reuse it (never testSubspace(subtest-t)).
	ks := testSubspace(t)

	// Seed enough rows that "reads ≤5" vs "reads all" is unambiguous.
	const seeded = 400
	orders := make([]*gen.Order, 0, seeded)
	for i := 1; i <= seeded; i++ {
		orders = append(orders, &gen.Order{
			OrderId: proto.Int64(int64(i)),
			Price:   proto.Int32(int32(i)),
		})
	}
	insertOrders(t, store, orders...)

	// scansAtMost reports whether `plan` scans at most k leaf records: it runs the
	// plan with a hard scanned-records cap that FAILS the scan when exceeded, and
	// reports true when the plan completes without tripping it. Measures rows
	// ACTUALLY read at the leaf cursor, not the planner's cost estimate.
	scansAtMost := func(t *testing.T, plan plans.RecordQueryPlan, k int) bool {
		t.Helper()
		var tripped bool
		_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			s, oerr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
				SetSubspace(ks).Open()
			if oerr != nil {
				return nil, oerr
			}
			props := recordlayer.DefaultExecuteProperties().WithScannedRecordsLimit(k)
			props.FailOnScanLimitReached = true
			cursor, cerr := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), nil, props)
			if cerr != nil {
				return nil, cerr
			}
			defer cursor.Close()
			_, cerr = CollectAll(ctx, cursor)
			var scanLimit *recordlayer.ScanLimitReachedError
			if errors.As(cerr, &scanLimit) {
				tripped = true
				return nil, nil
			}
			return nil, cerr
		})
		if err != nil {
			t.Fatalf("scansAtMost(k=%d): %v", k, err)
		}
		return !tripped
	}

	scan := func() plans.RecordQueryPlan {
		return plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false)
	}
	sortByPK := func(inner plans.RecordQueryPlan) plans.RecordQueryPlan {
		return plans.NewRecordQueryInMemorySortPlan(inner, []plans.SortKey{{
			Field:      "ORDER_ID",
			ValueExpr:  values.NewFieldValueWithResolvedOrdinal("ORDER_ID", 0, values.UnknownType),
			NullsFirst: true,
		}})
	}

	// (1) FIX — PK-ordered ORDER BY under a LIMIT: elimination reads ≤5, the
	// redundant materialized sort reads the whole input.
	t.Run("pk_ordered_limit_elides_reads_few", func(t *testing.T) {
		elided := plans.NewRecordQueryLimitPlan(scan(), 1, 0)                 // planner's post-fix choice
		materialized := plans.NewRecordQueryLimitPlan(sortByPK(scan()), 1, 0) // pre-fix wrong choice

		if !scansAtMost(t, elided, 5) {
			t.Fatalf("elided Limit(1, Scan) scanned > 5 of %d rows — expected a streaming ~1-row read", seeded)
		}
		if scansAtMost(t, materialized, 100) {
			t.Fatalf("materialized Limit(1, InMemorySort(Scan)) scanned <= 100 of %d rows — expected it to read the whole input", seeded)
		}

		// Correctness preserved — both return the identical single min-PK row.
		minRow := func(plan plans.RecordQueryPlan) int64 {
			var got int64
			_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				s, oerr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
					SetSubspace(ks).Open()
				if oerr != nil {
					return nil, oerr
				}
				cursor, cerr := ExecutePlan(ctx, plan, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
				if cerr != nil {
					return nil, cerr
				}
				defer cursor.Close()
				rows, cerr := CollectAll(ctx, cursor)
				if cerr != nil {
					return nil, cerr
				}
				if len(rows) != 1 {
					t.Fatalf("plan returned %d rows, want 1", len(rows))
				}
				id, _ := rowMap(rows[0])["ORDER_ID"].(int64)
				got = id
				return nil, nil
			})
			if err != nil {
				t.Fatalf("minRow: %v", err)
			}
			return got
		}
		if e, m := minRow(elided), minRow(materialized); e != m || m != 1 {
			t.Fatalf("plans disagree or wrong min: elided=%d materialized=%d (want both 1)", e, m)
		}
	})

	// (2) IMPROVEMENT — a selective index scan reads FEWER rows than the full scan.
	// price >= 398 matches 3 of 400 rows via order_price_idx.
	t.Run("selective_index_reads_fewer_than_full_scan", func(t *testing.T) {
		res := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
			Type:    predicates.ComparisonGreaterThanEq,
			Operand: values.LiteralValue(int64(398)),
		})
		if !res.Ok {
			t.Fatal("build price>=398 comparison range")
		}
		selectiveIndex := plans.NewRecordQueryIndexPlan(
			"order_price_idx", []*predicates.ComparisonRange{res.Range}, []string{"Order"}, nil, false)

		if scansAtMost(t, scan(), 50) {
			t.Fatalf("full Scan(Order) scanned <= 50 of %d rows — expected it to read the whole table", seeded)
		}
		if !scansAtMost(t, selectiveIndex, 50) {
			t.Fatal("selective IndexScan(order_price_idx, price>=398) scanned > 50 — expected it to read only the 3 matching rows")
		}
	})
}
