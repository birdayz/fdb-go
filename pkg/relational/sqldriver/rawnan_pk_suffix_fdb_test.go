package sqldriver_test

// A raw NaN primary key is TWO facts at once, and the planner must respect both.
//
// Storage keeps NaN sign and payload, so 0xfff8000000000001 and
// 0x7ff8000000000001 are DISTINCT primary tuple keys that pack on opposite
// sides of every finite value. SQL's comparator (values.CompareFloat64,
// faithful to java.lang.Double.compare) canonicalizes them to ONE value ranked
// greatest. So record identity is finer than logical equality here, and the two
// consequences pull in opposite directions:
//
//   - DISTINCT may NOT be elided from primary-key coverage. Two records with
//     distinct PKs collapse to one logical row, so the row-value distinct
//     operator stays.
//   - ORDER BY may NOT be answered from storage order. The physical PK suffix
//     is not in CompareFloat64 order, so an explicit sort survives — in both
//     directions, and before LIMIT.
//
// This file exercises both against an indexed table and an unindexed baseline
// that has no choice but to sort, so a lost claim shows up as a DISAGREEMENT
// rather than as a plan-shape assertion alone.

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
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

// A secondary VALUE index is ordered by its full physical entry key:
// (index root, TrimPrimaryKey(primary key)). A DOUBLE PK suffix containing a
// negative NaN is therefore not in SQL's CompareFloat64 order even when the
// index root is equality-fixed. The planner must retain an explicit sort for
// ORDER BY ID (and before LIMIT), in both directions.
func TestFDB_RawNaNPrimaryKeySuffixRetainsLogicalSort(t *testing.T) {
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

	const indexedTable = "INDEXED_PK"
	const baselineTable = "BASELINE_PK"
	const indexName = "STATUS_IDX"
	builder := metadata.NewSchemaTemplateBuilder().SetName("raw_nan_pk_suffix")
	for _, table := range []string{indexedTable, baselineTable} {
		builder.AddTable(table, []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewDoubleType(false), 1),
			metadata.NewColumnSpec("STATUS", api.NewStringType(false), 2),
		}, []string{"ID"})
	}
	builder.AddIndex(indexedTable, indexName, []string{"STATUS"}, false)
	template, buildErr := builder.Build()
	if buildErr != nil {
		t.Fatalf("build schema: %v", buildErr)
	}
	md := template.Underlying()
	ks := subspace.FromBytes(tuple.Tuple{t.Name(), "raw-nan-pk-suffix"}.Pack())

	ids := []float64{
		math.Float64frombits(0xfff8000000000001),
		-1,
		math.Copysign(0, -1),
		0,
		1,
		math.Float64frombits(0x7ff8000000000001),
	}
	makeRecord := func(table string, id float64) proto.Message {
		descriptor := md.GetRecordType(table).Descriptor
		message := dynamicpb.NewMessage(descriptor)
		message.Set(descriptor.Fields().ByName("ID"), protoreflect.ValueOfFloat64(id))
		message.Set(descriptor.Fields().ByName("STATUS"), protoreflect.ValueOfString("x"))
		return message
	}
	_, setupErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if openErr != nil {
			return nil, openErr
		}
		for _, id := range ids {
			for _, table := range []string{indexedTable, baselineTable} {
				if _, saveErr := store.SaveRecord(makeRecord(table, id)); saveErr != nil {
					return nil, saveErr
				}
			}
		}
		return nil, nil
	})
	if setupErr != nil {
		t.Fatalf("setup: %v", setupErr)
	}

	// Prove the physical counterexample is present: forward tuple order places
	// the sign-set NaN before finite values and the positive NaN after them.
	var physical []float64
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
			id, ok := pk[len(pk)-1].(float64)
			if !ok {
				return nil, fmt.Errorf("primary-key ID is %T, want float64", pk[len(pk)-1])
			}
			physical = append(physical, id)
		}
		return nil, nil
	})
	if inspectErr != nil {
		t.Fatalf("inspect physical index: %v", inspectErr)
	}
	if len(physical) != len(ids) || !math.IsNaN(physical[0]) ||
		!math.Signbit(physical[0]) || !math.IsNaN(physical[len(physical)-1]) ||
		math.Signbit(physical[len(physical)-1]) {
		t.Fatalf("physical PK suffix order = %#v, want negative NaN ... positive NaN", physical)
	}

	label := func(value float64) string {
		switch {
		case math.IsNaN(value):
			return "nan"
		case value == 0 && math.Signbit(value):
			return "-0"
		case value == 0:
			return "+0"
		default:
			return fmt.Sprintf("%g", value)
		}
	}
	execute := func(t *testing.T, plan plans.RecordQueryPlan) []string {
		t.Helper()
		var result []string
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
			rows, collectErr := executor.CollectAll(ctx, cursor)
			if collectErr != nil {
				return nil, collectErr
			}
			for _, row := range rows {
				mapped, ok := executor.RowValue(row).(map[string]any)
				if !ok {
					return nil, fmt.Errorf("query row is %T, want map[string]any", executor.RowValue(row))
				}
				id, ok := mapped["ID"].(float64)
				if !ok {
					return nil, fmt.Errorf("ID is %T, want float64", mapped["ID"])
				}
				result = append(result, label(id))
			}
			return nil, nil
		})
		if runErr != nil {
			t.Fatalf("execute %s: %v", plan.Explain(), runErr)
		}
		return result
	}

	// Raw NaN payload/sign variants are distinct primary tuple keys but one SQL
	// DISTINCT value. Exercise both the logical PK-coverage shortcut (explicit
	// projection) and the record-distinct partition shortcut (SELECT DISTINCT
	// *) through primary and secondary access paths. Both must retain the
	// row-value distinct operator; signed zero deliberately remains two values.
	for _, table := range []string{indexedTable, baselineTable} {
		for _, projection := range []string{"id, status", "*"} {
			query := fmt.Sprintf(
				"SELECT DISTINCT %s FROM %s WHERE status = 'x'", projection, table,
			)
			plan, planErr := embedded.PlanRecordQueryWithMetadata(query, md, nil)
			if planErr != nil {
				t.Fatalf("plan %q: %v", query, planErr)
			}
			hasDistinct := false
			plans.Walk(plan, func(node plans.RecordQueryPlan) bool {
				if _, ok := node.(*plans.RecordQueryDistinctPlan); ok {
					hasDistinct = true
				}
				return true
			})
			if !hasDistinct {
				t.Fatalf("raw NaN primary keys incorrectly eliminated DISTINCT: %s", plan.Explain())
			}
			got := execute(t, plan)
			sort.Strings(got)
			want := []string{"+0", "-0", "-1", "1", "nan"}
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Fatalf("%s = %v, want %v; plan: %s", query, got, want, plan.Explain())
			}
		}
	}

	// GROUP BY currently preserves raw storage identity for floating keys so it
	// can agree with maintained aggregate-index groups. Consequently two NaN
	// payload groups can surface the same logical (ID, COUNT) row, and the outer
	// DISTINCT remains semantically necessary.
	groupDistinctSQL := fmt.Sprintf(
		"SELECT DISTINCT id, COUNT(*) FROM %s GROUP BY id", baselineTable,
	)
	groupDistinctPlan, planErr := embedded.PlanRecordQueryWithMetadata(groupDistinctSQL, md, nil)
	if planErr != nil {
		t.Fatalf("plan %q: %v", groupDistinctSQL, planErr)
	}
	hasGroupDistinct := false
	plans.Walk(groupDistinctPlan, func(node plans.RecordQueryPlan) bool {
		if _, ok := node.(*plans.RecordQueryDistinctPlan); ok {
			hasGroupDistinct = true
		}
		return true
	})
	if !hasGroupDistinct {
		t.Fatalf("floating GROUP BY incorrectly eliminated outer DISTINCT: %s", groupDistinctPlan.Explain())
	}
	groupDistinctGot := execute(t, groupDistinctPlan)
	sort.Strings(groupDistinctGot)
	groupDistinctWant := []string{"+0", "-0", "-1", "1", "nan"}
	sort.Strings(groupDistinctWant)
	if !slices.Equal(groupDistinctGot, groupDistinctWant) {
		t.Fatalf("%s = %v, want %v; plan: %s",
			groupDistinctSQL, groupDistinctGot, groupDistinctWant, groupDistinctPlan.Explain())
	}

	cases := []struct {
		name     string
		order    string
		limit    string
		expected []string
	}{
		{name: "ascending", order: "ASC", expected: []string{"-1", "-0", "+0", "1", "nan", "nan"}},
		{name: "descending", order: "DESC", expected: []string{"nan", "nan", "1", "+0", "-0", "-1"}},
		{name: "ascending_limit", order: "ASC", limit: " LIMIT 2", expected: []string{"-1", "-0"}},
		{name: "descending_limit", order: "DESC", limit: " LIMIT 2", expected: []string{"nan", "nan"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			indexedSQL := fmt.Sprintf(
				"SELECT id FROM %s WHERE status = 'x' ORDER BY id %s%s",
				indexedTable, test.order, test.limit,
			)
			baselineSQL := fmt.Sprintf(
				"SELECT id FROM %s WHERE status = 'x' ORDER BY id %s%s",
				baselineTable, test.order, test.limit,
			)
			indexedPlan, planErr := embedded.PlanRecordQueryWithMetadata(indexedSQL, md, nil)
			if planErr != nil {
				t.Fatalf("plan indexed query: %v", planErr)
			}
			baselinePlan, baselinePlanErr := embedded.PlanRecordQueryWithMetadata(baselineSQL, md, nil)
			if baselinePlanErr != nil {
				t.Fatalf("plan baseline query: %v", baselinePlanErr)
			}

			hasStatusIndex := false
			hasLogicalSort := false
			plans.Walk(indexedPlan, func(node plans.RecordQueryPlan) bool {
				switch concrete := node.(type) {
				case *plans.RecordQueryIndexPlan:
					hasStatusIndex = hasStatusIndex || concrete.GetIndexName() == indexName
				case *plans.RecordQueryInMemorySortPlan:
					hasLogicalSort = true
				}
				return true
			})
			if !hasStatusIndex {
				t.Fatalf("query did not exercise %s: %s", indexName, indexedPlan.Explain())
			}
			if !hasLogicalSort {
				t.Fatalf("raw DOUBLE PK suffix falsely satisfied ORDER BY: %s", indexedPlan.Explain())
			}

			indexed := execute(t, indexedPlan)
			baseline := execute(t, baselinePlan)
			if !slices.Equal(indexed, baseline) {
				t.Fatalf("indexed order = %v, baseline order = %v\nindexed: %s\nbaseline: %s",
					indexed, baseline, indexedPlan.Explain(), baselinePlan.Explain())
			}
			if !slices.Equal(indexed, test.expected) {
				t.Fatalf("logical %s order = %v, want %v; plan: %s",
					test.name, indexed, test.expected, indexedPlan.Explain())
			}
		})
	}
}
