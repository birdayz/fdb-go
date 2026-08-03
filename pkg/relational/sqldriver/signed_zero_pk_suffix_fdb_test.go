package sqldriver_test

// `WHERE <double> = 0.0 ORDER BY <primary key>` over an index whose LAST key
// column is the bound double. The corpus records what the planner DOES; this
// file records what is TRUE, by comparing an indexed table against an unindexed
// baseline that has no choice but to sort.

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

// A zero-valued FLOAT/DOUBLE equality is ONE logical value spanning TWO physical
// keys: -0.0 and +0.0 are IEEE-equal but pack to distinct adjacent tuple keys,
// so the scan opens two blocks and every coordinate after the bound one RESTARTS
// at the block boundary.
//
// That coordinate therefore answers two DIFFERENT ordering questions two
// different ways. It may claim its OWN order, but only DIRECTIONALLY — the scan
// opens the two zero blocks in key order, so a forward scan delivers them
// ascending and a reverse scan descending. It may NOT carry the order of
// anything AFTER it — the trimmed primary-key suffix included.
//
// It is specifically NOT order-free. "Every admitted row shares one value, so
// any permutation will do" is the reasoning that fits a genuine single-key
// equality and does NOT fit this one: the two comparators disagree here on
// purpose. The PREDICATE comparator makes -0.0 == +0.0, which is why both rows
// are selected; the SORT comparator ranks -0.0 below +0.0, faithful to
// java.lang.Double.compare, which is why they are two distinct ORDER BY values.
//
// The suffix is the case a part COUNT cannot see. A derivation that emits the
// bound coordinate and then decides whether the primary key continues the claim
// by asking "did the loop consume the whole index key?" gets YES whenever the
// bound coordinate is the LAST key column — it was consumed, it was counted —
// and appends a primary-key ordering the scan does not deliver. The index here
// has exactly one key column for that reason: a wider index hides the defect,
// because the count then falls short on its own and the suffix is refused for
// the wrong reason.
//
// The unindexed baseline table is the oracle: with no index on Z it must scan
// and sort, so it computes the answer the SQL semantics require. Asserting the
// indexed table against it is what makes this a statement about correctness
// rather than about plan shape.
func TestFDB_SignedZeroEqualityDoesNotOrderThePKSuffix(t *testing.T) {
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

	const indexedTable = "INDEXED_Z"
	const baselineTable = "BASELINE_Z"
	const indexName = "IDX_Z"
	builder := metadata.NewSchemaTemplateBuilder().SetName("signed_zero_pk_suffix")
	for _, table := range []string{indexedTable, baselineTable} {
		builder.AddTable(table, []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
			metadata.NewColumnSpec("Z", api.NewDoubleType(true), 2),
		}, []string{"ID"})
	}
	// Only the indexed table gets the index; the baseline must sort.
	builder.AddIndex(indexedTable, indexName, []string{"Z"}, false)
	template, buildErr := builder.Build()
	if buildErr != nil {
		t.Fatalf("build schema: %v", buildErr)
	}
	md := template.Underlying()
	ks := subspace.FromBytes(tuple.Tuple{t.Name(), "signed-zero-pk-suffix"}.Pack())

	negZero := math.Copysign(0, -1)

	// The ID assigned to the -0.0 row must be GREATER than the ID assigned to
	// the +0.0 row. -0.0 packs BELOW +0.0, so the physical scan emits the larger
	// ID first and the ID order disagrees with the scan order. With the IDs the
	// other way round the two orders coincide and the test passes with the
	// defect fully present — the same trap as pinning a signed-zero bug with a
	// single column, where the two zeros are adjacent and every plan agrees.
	//
	// Two rows per zero sign, so the defect cannot be mistaken for an off-by-one
	// swap of one adjacent pair.
	rows := []struct {
		id int64
		z  float64
	}{
		{id: 7, z: negZero},
		{id: 9, z: negZero},
		{id: 1, z: 0.0},
		{id: 3, z: 0.0},
		{id: 5, z: 2.5}, // excluded by z = 0.0; keeps the equality genuinely selective
	}

	makeRecord := func(table string, id int64, z float64) proto.Message {
		descriptor := md.GetRecordType(table).Descriptor
		message := dynamicpb.NewMessage(descriptor)
		message.Set(descriptor.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		message.Set(descriptor.Fields().ByName("Z"), protoreflect.ValueOfFloat64(z))
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
				if _, saveErr := store.SaveRecord(makeRecord(table, row.id, row.z)); saveErr != nil {
					return nil, saveErr
				}
			}
		}
		return nil, nil
	})
	if setupErr != nil {
		t.Fatalf("setup: %v", setupErr)
	}

	// Prove the physical counterexample is actually on disk. If FDB tuple
	// encoding ever stopped separating the two zero signs, the ordering claim
	// this test rejects would become sound and the test would be pinning a
	// constraint that no longer exists.
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
	// The -0.0 block (ids 7,9) precedes the +0.0 block (ids 1,3), and 2.5 last.
	// Inside each block the primary key ascends; ACROSS the two it does not,
	// which is precisely the claim the planner must not make.
	if wantPhysical := []int64{7, 9, 1, 3, 5}; !slices.Equal(physical, wantPhysical) {
		t.Fatalf("physical index order = %v, want %v; the two signed-zero blocks must "+
			"straddle the primary-key order for this test to be able to express the defect",
			physical, wantPhysical)
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
		// want is the exact ID sequence when the ORDER BY is total over the
		// selected rows. Left nil when it is not, in which case wantBlocks
		// carries the assertion instead.
		want []int64
		// wantBlocks is the sequence of ID SETS the answer must be partitioned
		// into, used when the ORDER BY leaves ties. Order BETWEEN blocks is
		// asserted; order WITHIN a block is not constrained by the query, and
		// pinning an arbitrary one would pin the sort's implementation rather
		// than the semantics.
		wantBlocks [][]int64
	}{
		{
			// The suffix claim. With the primary key wrongly claimed as ordered
			// through the widened equality the sort is elided and the answer is
			// the raw physical order 7,9,1,3.
			name:    "pk_ascending",
			orderBy: "id",
			want:    []int64{1, 3, 7, 9},
		},
		{
			// The same claim under a reverse scan, which reverses both zero
			// blocks wholesale and yields 3,1,9,7 — still not descending
			// primary-key order.
			name:    "pk_descending",
			orderBy: "id DESC",
			want:    []int64{9, 7, 3, 1},
		},
		{
			// The SELF claim, descending. This is the case that separates
			// "the coordinate is FIXED" from "the coordinate is ORDERED in the
			// scan's direction". FIXED means the coordinate states no order and
			// therefore satisfies EITHER direction from a forward scan — true
			// only where one equality admits one SORT value. Here it admits
			// two: the predicate comparator makes -0.0 and +0.0 equal (which is
			// why both rows are selected), while the sort comparator ranks -0.0
			// BELOW +0.0 (java.lang.Double.compare). A forward scan therefore
			// delivers them ASCENDING and cannot answer this.
			name:       "bound_column_descending",
			orderBy:    "z DESC",
			wantBlocks: [][]int64{{1, 3}, {7, 9}},
		},
		{
			// The direction the forward scan DOES deliver. It must stay green:
			// the defect above is fixed by making the coordinate directional,
			// not by refusing it, and a blanket refusal would be visible here as
			// a materialized sort rather than a wrong answer.
			name:    "bound_column_ascending_then_pk",
			orderBy: "z, id",
			want:    []int64{7, 9, 1, 3},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			indexedSQL := fmt.Sprintf(
				"SELECT id FROM %s WHERE z = 0.0 ORDER BY %s", indexedTable, test.orderBy)
			baselineSQL := fmt.Sprintf(
				"SELECT id FROM %s WHERE z = 0.0 ORDER BY %s", baselineTable, test.orderBy)
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
			// defect fully present.
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
			if test.want != nil && !slices.Equal(indexedIDs, test.want) {
				t.Fatalf(
					"ORDER BY %s: IDs = %v, want %v; a zero-valued float equality spans two "+
						"physical blocks, so nothing may claim an order across them that the "+
						"scan does not deliver\nplan: %s",
					test.orderBy, indexedIDs, test.want, indexedPlan.Explain(),
				)
			}
			if test.wantBlocks != nil {
				at := 0
				for blockIdx, block := range test.wantBlocks {
					if at+len(block) > len(indexedIDs) {
						t.Fatalf("ORDER BY %s: IDs = %v, too short for block %d %v\nplan: %s",
							test.orderBy, indexedIDs, blockIdx, block, indexedPlan.Explain())
					}
					got := slices.Clone(indexedIDs[at : at+len(block)])
					slices.Sort(got)
					if !slices.Equal(got, block) {
						t.Fatalf(
							"ORDER BY %s: IDs = %v; positions %d..%d must be exactly %v (in any "+
								"order — the query ties them), got %v. The two signed-zero blocks "+
								"are two distinct SORT values, so the scan direction must match "+
								"the requested one\nplan: %s",
							test.orderBy, indexedIDs, at, at+len(block)-1, block, got,
							indexedPlan.Explain(),
						)
					}
					at += len(block)
				}
				if at != len(indexedIDs) {
					t.Fatalf("ORDER BY %s: IDs = %v, want exactly %d rows\nplan: %s",
						test.orderBy, indexedIDs, at, indexedPlan.Explain())
				}
			}
		})
	}
}
