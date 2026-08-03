package sqldriver_test

// ORDER BY <float-column>, <primary key> over rows whose float column holds raw
// NaN payloads. The corpus records what the planner DOES; this file records what
// is TRUE, by comparing an indexed table against an unindexed baseline that has
// no choice but to sort.

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

// A secondary VALUE index on a FLOAT/DOUBLE column is ordered by its physical
// entry key, and FDB tuple order preserves NaN sign: the negative-NaN block sits
// at the BOTTOM of the key space and the positive-NaN block at the TOP, with the
// finite values between them. SQL order disagrees — CompareFloat64 canonicalizes
// every NaN payload to one value that sorts above everything finite — so all
// NaNs are ONE logical value spread across TWO physical blocks.
//
// The consequence for a two-column sort is the point of this test. In
// ORDER BY e, id the primary key is the tie-breaker WITHIN each logical value of
// e, so inside the NaN tie class it must run 1,2,3,4 across both physical
// blocks. An index scan cannot deliver that: it emits the negative-NaN block
// (ids 1,3) before the finite rows and the positive-NaN block (ids 2,4) after
// them. A plan that consumes the whole index key and then appends the trimmed
// primary-key suffix is therefore claiming an ordering the scan does not have,
// and the rows come back in NaN-payload order rather than in ID order.
//
// The unindexed baseline table is the oracle: with no index on E it must scan
// and sort, so it computes the answer the SQL semantics require. Asserting the
// indexed table against it is what makes this a statement about correctness
// rather than about plan shape.
func TestFDB_FloatOrderByWithPKTieBreakerMatchesUnindexedBaseline(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)

	const indexedTable = "INDEXED_E"
	const baselineTable = "BASELINE_E"
	const indexName = "IDX_E"
	builder := metadata.NewSchemaTemplateBuilder().SetName("float_order_pk_tie")
	for _, table := range []string{indexedTable, baselineTable} {
		builder.AddTable(table, []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
			metadata.NewColumnSpec("E", api.NewDoubleType(true), 2),
		}, []string{"ID"})
	}
	// Only the indexed table gets the index; the baseline must sort.
	builder.AddIndex(indexedTable, indexName, []string{"E"}, false)
	template, buildErr := builder.Build()
	if buildErr != nil {
		t.Fatalf("build schema: %v", buildErr)
	}
	md := template.Underlying()
	ks := subspace.FromBytes(tuple.Tuple{t.Name(), "float-order-pk-tie"}.Pack())

	negNaNa := math.Float64frombits(0xfff8000000000001)
	negNaNb := math.Float64frombits(0xfff8000000000007)
	posNaNa := math.Float64frombits(0x7ff8000000000001)
	posNaNb := math.Float64frombits(0x7ff8000000000007)

	// IDs are assigned so that ID order INSIDE the NaN tie class (1,2,3,4)
	// interleaves the two physical blocks. Without that interleaving the
	// physical order and the logical order coincide and the defect is
	// invisible — the same trap as pinning a signed-zero bug with a single
	// column, where the two zeros are adjacent and every plan agrees.
	//
	// The float coordinate must also be BOUND by a predicate. An unbound
	// FLOAT/DOUBLE coordinate yields an ordering prefix of zero, so the index
	// offers no ordering at all and the planner full-scans and sorts — the
	// branch this test targets is then unreachable and the test would pass with
	// the defect fully present. WHERE e > 5.0 is what makes the index eligible,
	// and it keeps every NaN because NaN is logically greatest.
	rows := []struct {
		id int64
		e  float64
	}{
		{id: 1, e: negNaNa},
		{id: 2, e: posNaNa},
		{id: 3, e: negNaNb},
		{id: 4, e: posNaNb},
		{id: 5, e: 7},
		{id: 6, e: 6},
		{id: 7, e: 1}, // filtered out by e > 5.0; keeps the range genuinely bound
	}
	makeRecord := func(table string, id int64, e float64) proto.Message {
		descriptor := md.GetRecordType(table).Descriptor
		message := dynamicpb.NewMessage(descriptor)
		message.Set(descriptor.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		message.Set(descriptor.Fields().ByName("E"), protoreflect.ValueOfFloat64(e))
		return message
	}
	_, setupErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if openErr != nil {
			return nil, openErr
		}
		for _, row := range rows {
			for _, table := range []string{indexedTable, baselineTable} {
				if _, saveErr := store.SaveRecord(makeRecord(table, row.id, row.e)); saveErr != nil {
					return nil, saveErr
				}
			}
		}
		return nil, nil
	})
	if setupErr != nil {
		t.Fatalf("setup: %v", setupErr)
	}

	// Prove the physical counterexample is actually on disk. If FDB ever stopped
	// splitting NaN by sign, the ordering claim this test rejects would become
	// sound and the test would be pinning a constraint that no longer exists.
	var physical []int64
	_, inspectErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
		if openErr != nil {
			return nil, openErr
		}
		entries, scanErr := recordlayer.AsList(
			ctx,
			store.ScanIndex(md.GetIndex(indexName), recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()),
		)
		if scanErr != nil {
			return nil, scanErr
		}
		for _, entry := range entries {
			pk := entry.PrimaryKey()
			if len(pk) == 0 {
				return nil, fmt.Errorf("empty primary key for index entry %v", entry.Key)
			}
			id, ok := pk[len(pk)-1].(int64)
			if !ok {
				return nil, fmt.Errorf("primary-key ID is %T, want int64", pk[len(pk)-1])
			}
			physical = append(physical, id)
		}
		return nil, nil
	})
	if inspectErr != nil {
		t.Fatalf("inspect physical index: %v", inspectErr)
	}
	// Negative NaNs below the finite rows (7,6,5), positive NaNs above. Inside
	// the negative block the order is 3 then 1: the tuple encoding inverts the
	// bits of a sign-set double, so the LARGER payload sorts first, exactly as
	// -1.7 precedes -1.0. That is why the ID order inside the NaN tie class
	// cannot be recovered from the physical order by any per-block reasoning.
	if wantPhysical := []int64{3, 1, 7, 6, 5, 2, 4}; !slices.Equal(physical, wantPhysical) {
		t.Fatalf("physical index order = %v, want %v; the NaN tie class must straddle "+
			"the finite values for this test to be able to express the defect", physical, wantPhysical)
	}

	execute := func(t *testing.T, plan plans.RecordQueryPlan) []int64 {
		t.Helper()
		var result []int64
		_, runErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, openErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if openErr != nil {
				return nil, openErr
			}
			cursor, executeErr := executor.ExecutePlan(
				ctx, plan, store, executor.EmptyEvaluationContext(), nil,
				recordlayer.DefaultExecuteProperties(),
			)
			if executeErr != nil {
				return nil, executeErr
			}
			defer func() { _ = cursor.Close() }()
			collected, collectErr := executor.CollectAll(ctx, cursor)
			if collectErr != nil {
				return nil, collectErr
			}
			for _, row := range collected {
				mapped, ok := executor.RowValue(row).(map[string]any)
				if !ok {
					return nil, fmt.Errorf("query row is %T, want map[string]any", executor.RowValue(row))
				}
				id, ok := mapped["ID"].(int64)
				if !ok {
					return nil, fmt.Errorf("ID is %T, want int64", mapped["ID"])
				}
				result = append(result, id)
			}
			return nil, nil
		})
		if runErr != nil {
			t.Fatalf("execute %s: %v", plan.Explain(), runErr)
		}
		return result
	}

	tests := []struct {
		name    string
		orderBy string
		want    []int64
	}{
		{
			// Finite ascending (6 then 7), then the NaN tie class in ID order
			// across BOTH physical blocks. The elided plan returns the raw
			// physical order 3,1,6,5,2,4 instead.
			name:    "ascending",
			orderBy: "e, id",
			want:    []int64{6, 5, 1, 2, 3, 4},
		},
		{
			// NULLS FIRST is the corpus's dominant rendering; with no NULL rows
			// it must agree with the plain ascending answer.
			name:    "ascending_nulls_first",
			orderBy: "e NULLS FIRST, id",
			want:    []int64{6, 5, 1, 2, 3, 4},
		},
		{
			// The tie-breaker stays ASCENDING while the float key descends, so
			// the NaN class leads and still runs 1,2,3,4 inside itself.
			//
			// This case is COVERAGE, NOT A DETECTOR, and the distinction is
			// measured rather than assumed: it binds the index but does not
			// elide the sort, so it passes even with the terminal-prefix gate
			// removed. Do not count it as evidence that the descending
			// direction is pinned — only the ascending cases go red under that
			// mutation. If a future change makes DESC elide, this case starts
			// carrying weight and should be re-mutated to find out.
			name:    "float_descending_pk_ascending",
			orderBy: "e DESC, id",
			want:    []int64{1, 2, 3, 4, 5, 6},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			indexedSQL := fmt.Sprintf(
				"SELECT id FROM %s WHERE e > 5.0 ORDER BY %s", indexedTable, test.orderBy)
			baselineSQL := fmt.Sprintf(
				"SELECT id FROM %s WHERE e > 5.0 ORDER BY %s", baselineTable, test.orderBy)
			indexedPlan, planErr := embedded.PlanRecordQueryWithMetadata(indexedSQL, md, nil)
			if planErr != nil {
				t.Fatalf("plan %q: %v", indexedSQL, planErr)
			}
			baselinePlan, baselinePlanErr := embedded.PlanRecordQueryWithMetadata(baselineSQL, md, nil)
			if baselinePlanErr != nil {
				t.Fatalf("plan %q: %v", baselineSQL, baselinePlanErr)
			}

			// Reachability guard. If the indexed side ever stops binding the
			// index it degrades into a second copy of the baseline: both sides
			// full-scan and sort, both agree, and the test passes with the
			// defect fully present. That is exactly how the first version of
			// this test passed under mutation, so the condition is asserted
			// rather than assumed.
			usesIndex := false
			plans.Walk(indexedPlan, func(node plans.RecordQueryPlan) bool {
				if index, ok := node.(*plans.RecordQueryIndexPlan); ok &&
					index.GetIndexName() == indexName {
					usesIndex = true
				}
				return true
			})
			if !usesIndex {
				t.Fatalf(
					"%s did not bind %s: %s\nwithout an index scan this test cannot express the "+
						"ordering claim it exists to reject",
					indexedSQL, indexName, indexedPlan.Explain(),
				)
			}

			indexedIDs := execute(t, indexedPlan)
			baselineIDs := execute(t, baselinePlan)
			if !slices.Equal(indexedIDs, baselineIDs) {
				t.Fatalf(
					"ORDER BY %s: indexed IDs = %v, unindexed baseline IDs = %v; the access path "+
						"changed the ANSWER, so the indexed plan claims an ordering it does not deliver"+
						"\nindexed plan:  %s\nbaseline plan: %s",
					test.orderBy, indexedIDs, baselineIDs, indexedPlan.Explain(), baselinePlan.Explain(),
				)
			}
			if !slices.Equal(indexedIDs, test.want) {
				t.Fatalf(
					"ORDER BY %s: IDs = %v, want %v; the primary key must be ordered in its own "+
						"right INSIDE the NaN tie class, not in NaN-payload order\nplan: %s",
					test.orderBy, indexedIDs, test.want, indexedPlan.Explain(),
				)
			}
		})
	}
}
