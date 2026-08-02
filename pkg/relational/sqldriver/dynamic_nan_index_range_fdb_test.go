package sqldriver_test

// A dynamic NaN cannot be represented by a finite exact composite tuple range.
// The runtime binder must return its typed correct-or-loud error while building
// the plan cursor, before any child index range is opened.

import (
	"context"
	"errors"
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
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

func TestFDB_DynamicNaNCompositeIndexCorrectOrLoud(t *testing.T) {
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

	comparisonRange := func(t *testing.T, comparison predicates.Comparison) *predicates.ComparisonRange {
		t.Helper()
		merged := predicates.EmptyComparisonRange().Merge(&comparison)
		if !merged.Ok {
			t.Fatalf("comparison range merge failed: %v", comparison)
		}
		return merged.Range
	}
	parameterRange := comparisonRange(t, predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: values.NewParameterValue(1),
	})
	suffixRange := comparisonRange(t,
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5)))

	for _, width := range []string{"DOUBLE", "FLOAT"} {
		t.Run(width, func(t *testing.T) {
			var columnType api.DataType
			var physicalType values.Type
			if width == "FLOAT" {
				columnType = api.NewFloatType(false)
				physicalType = values.NotNullFloat
			} else {
				columnType = api.NewDoubleType(false)
				physicalType = values.NotNullDouble
			}

			b := metadata.NewSchemaTemplateBuilder().SetName("dynamic_nan_" + width)
			b.AddTable("T", []metadata.ColumnSpec{
				metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
				metadata.NewColumnSpec("V", columnType, 2),
				metadata.NewColumnSpec("W", api.NewLongType(false), 3),
			}, []string{"ID"})
			b.AddIndex("T", "V_W", []string{"V", "W"}, false)
			tmpl, buildErr := b.Build()
			if buildErr != nil {
				t.Fatalf("build schema: %v", buildErr)
			}
			md := tmpl.Underlying()
			ks := subspace.FromBytes(tuple.Tuple{t.Name(), "nan", width}.Pack())
			_, createErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				return nil, openErr
			})
			if createErr != nil {
				t.Fatalf("create store: %v", createErr)
			}

			plan := plans.NewRecordQueryIndexPlan(
				"V_W",
				[]*predicates.ComparisonRange{parameterRange, suffixRange},
				[]string{"T"},
				values.UnknownType,
				false,
			).WithKeyComponentTypes([]values.Type{physicalType, values.NotNullLong})

			var executeErr error
			_, runErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if openErr != nil {
					return nil, openErr
				}
				cursor, err := executor.ExecutePlan(
					ctx,
					plan,
					store,
					executor.EmptyEvaluationContext().WithParams([]any{math.NaN()}),
					nil,
					recordlayer.DefaultExecuteProperties(),
				)
				executeErr = err
				if cursor != nil {
					_ = cursor.Close()
					return nil, errors.New("dynamic NaN unexpectedly constructed a storage cursor")
				}
				return nil, nil
			})
			if runErr != nil {
				t.Fatalf("run: %v", runErr)
			}
			var unsupported *executor.UnsupportedPhysicalFloatEquivalenceError
			if !errors.As(executeErr, &unsupported) {
				t.Fatalf("%s dynamic NaN error = %T %v, want *UnsupportedPhysicalFloatEquivalenceError",
					width, executeErr, executeErr)
			}
		})
	}
}

// A FLOAT/DOUBLE inequality cannot be represented by one FDB tuple range when
// raw NaN payloads are present. Predicate comparison canonicalizes every NaN
// as one logical greatest value, while the tuple codec physically places
// sign-set NaNs below -Inf and keeps every payload distinct. The planner must
// therefore represent each inequality as an EXACT range set rather than one
// raw range: an upper bound starts at -Inf so it cannot sweep in the sign-set
// NaNs below it, and a lower bound adds a second range for them because every
// NaN is logically greatest. Leaving the inequalities as residual predicates
// over an unbounded scan — which this test used to require — cost the index
// and fixed nothing, since both paths already returned the same rows.
// This test writes exact positive and negative payloads
// through the record-store API, verifies their bits in the real index, and
// differentially compares the indexed table with an otherwise identical table
// that has no secondary index.
func TestFDB_FloatInequalitiesWithRawNaNPayloadsUseExactIndexRanges(t *testing.T) {
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

	type corpusRow struct {
		id     int64
		double float64
		float  float32
	}
	corpus := []corpusRow{
		{id: 1, double: math.Float64frombits(0xfff8000000000001), float: math.Float32frombits(0xffc00001)},
		{id: 2, double: math.Float64frombits(0xfff8000000000002), float: math.Float32frombits(0xffc00002)},
		{id: 3, double: math.Inf(-1), float: float32(math.Inf(-1))},
		{id: 4, double: -1, float: -1},
		{id: 5, double: math.Float64frombits(0x8000000000000000), float: math.Float32frombits(0x80000000)},
		{id: 6, double: 0, float: 0},
		{id: 7, double: 1, float: 1},
		{id: 8, double: math.Inf(1), float: float32(math.Inf(1))},
		{id: 9, double: math.Float64frombits(0x7ff8000000000001), float: math.Float32frombits(0x7fc00001)},
		{id: 10, double: math.Float64frombits(0x7ff8000000000002), float: math.Float32frombits(0x7fc00002)},
	}

	for _, width := range []string{"DOUBLE", "FLOAT"} {
		width := width
		t.Run(width, func(t *testing.T) {
			var columnType api.DataType
			if width == "FLOAT" {
				columnType = api.NewFloatType(false)
			} else {
				columnType = api.NewDoubleType(false)
			}

			const indexedTable = "INDEXED_VALUES"
			const baselineTable = "BASELINE_VALUES"
			const indexName = "VALUE_IDX"
			builder := metadata.NewSchemaTemplateBuilder().SetName("raw_nan_inequality_" + width)
			for _, table := range []string{indexedTable, baselineTable} {
				builder.AddTable(table, []metadata.ColumnSpec{
					metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
					metadata.NewColumnSpec("V", columnType, 2),
				}, []string{"ID"})
			}
			builder.AddIndex(indexedTable, indexName, []string{"V"}, false)
			template, buildErr := builder.Build()
			if buildErr != nil {
				t.Fatalf("build schema: %v", buildErr)
			}
			md := template.Underlying()
			ks := subspace.FromBytes(tuple.Tuple{t.Name(), "raw-nan-inequality", width}.Pack())

			makeRecord := func(table string, row corpusRow) proto.Message {
				descriptor := md.GetRecordType(table).Descriptor
				message := dynamicpb.NewMessage(descriptor)
				message.Set(descriptor.Fields().ByName("ID"), protoreflect.ValueOfInt64(row.id))
				if width == "FLOAT" {
					message.Set(descriptor.Fields().ByName("V"), protoreflect.ValueOfFloat32(row.float))
				} else {
					message.Set(descriptor.Fields().ByName("V"), protoreflect.ValueOfFloat64(row.double))
				}
				return message
			}
			_, setupErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if openErr != nil {
					return nil, openErr
				}
				for _, row := range corpus {
					for _, table := range []string{indexedTable, baselineTable} {
						if _, saveErr := store.SaveRecord(makeRecord(table, row)); saveErr != nil {
							return nil, saveErr
						}
					}
				}
				return nil, nil
			})
			if setupErr != nil {
				t.Fatalf("setup: %v", setupErr)
			}

			// Prove this is the adversarial physical corpus, rather than relying
			// on protobuf/index serialization to preserve the input NaN bits.
			var gotNaNBits []uint64
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
					value := entry.IndexValues()[0]
					if width == "FLOAT" {
						floating, ok := value.(float32)
						if !ok {
							return nil, fmt.Errorf("FLOAT index value is %T, want float32", value)
						}
						if math.IsNaN(float64(floating)) {
							gotNaNBits = append(gotNaNBits, uint64(math.Float32bits(floating)))
						}
					} else {
						floating, ok := value.(float64)
						if !ok {
							return nil, fmt.Errorf("DOUBLE index value is %T, want float64", value)
						}
						if math.IsNaN(floating) {
							gotNaNBits = append(gotNaNBits, math.Float64bits(floating))
						}
					}
				}
				return nil, nil
			})
			if inspectErr != nil {
				t.Fatalf("inspect physical index: %v", inspectErr)
			}
			sort.Slice(gotNaNBits, func(i, j int) bool { return gotNaNBits[i] < gotNaNBits[j] })
			var wantNaNBits []uint64
			if width == "FLOAT" {
				wantNaNBits = []uint64{0x7fc00001, 0x7fc00002, 0xffc00001, 0xffc00002}
			} else {
				wantNaNBits = []uint64{
					0x7ff8000000000001, 0x7ff8000000000002,
					0xfff8000000000001, 0xfff8000000000002,
				}
			}
			if !slices.Equal(gotNaNBits, wantNaNBits) {
				t.Fatalf("physical %s NaN bits = %#x, want %#x", width, gotNaNBits, wantNaNBits)
			}

			execute := func(t *testing.T, plan plans.RecordQueryPlan) []int64 {
				t.Helper()
				var ids []int64
				_, runErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, openErr := recordlayer.NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
					if openErr != nil {
						return nil, openErr
					}
					cursor, execErr := executor.ExecutePlan(
						ctx,
						plan,
						store,
						executor.EmptyEvaluationContext(),
						nil,
						recordlayer.DefaultExecuteProperties(),
					)
					if execErr != nil {
						return nil, execErr
					}
					defer func() { _ = cursor.Close() }()
					results, collectErr := executor.CollectAll(ctx, cursor)
					if collectErr != nil {
						return nil, collectErr
					}
					for _, result := range results {
						row, ok := executor.RowValue(result).(map[string]any)
						if !ok {
							return nil, fmt.Errorf("query row is %T, want map[string]any", executor.RowValue(result))
						}
						id, ok := row["ID"].(int64)
						if !ok {
							return nil, fmt.Errorf("ID is %T, want int64", row["ID"])
						}
						ids = append(ids, id)
					}
					return nil, nil
				})
				if runErr != nil {
					t.Fatalf("execute %s: %v", plan.Explain(), runErr)
				}
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
				return ids
			}

			cases := []struct {
				name     string
				operator string
				want     []int64
			}{
				{name: "less_than", operator: "<", want: []int64{3, 4}},
				{name: "less_than_or_equal", operator: "<=", want: []int64{3, 4, 5, 6}},
				{name: "greater_than", operator: ">", want: []int64{1, 2, 7, 8, 9, 10}},
				{name: "greater_than_or_equal", operator: ">=", want: []int64{1, 2, 5, 6, 7, 8, 9, 10}},
			}
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					indexedSQL := fmt.Sprintf("SELECT id FROM %s WHERE v %s 0.0", indexedTable, test.operator)
					baselineSQL := fmt.Sprintf("SELECT id FROM %s WHERE v %s 0.0", baselineTable, test.operator)
					indexedPlan, planErr := embedded.PlanRecordQueryWithMetadata(indexedSQL, md, nil)
					if planErr != nil {
						t.Fatalf("plan indexed query: %v", planErr)
					}
					baselinePlan, baselinePlanErr := embedded.PlanRecordQueryWithMetadata(baselineSQL, md, nil)
					if baselinePlanErr != nil {
						t.Fatalf("plan baseline query: %v", baselinePlanErr)
					}

					// The float index MUST be used with a real bound. This
					// test previously asserted the opposite — that the planner
					// refused the index and fell back to a residual scan — and
					// that refusal was a strict downgrade: it turned every
					// float inequality into a full table scan without fixing a
					// single wrong row. The safety property was never "decline
					// the index"; it is "return the same rows as a full scan
					// even with raw NaN payloads stored", which the baseline
					// comparison below asserts directly.
					usedBoundedIndex := false
					plans.Walk(indexedPlan, func(node plans.RecordQueryPlan) bool {
						concrete, ok := node.(*plans.RecordQueryIndexPlan)
						if !ok || concrete.GetIndexName() != indexName {
							return true
						}
						for _, comparisonRange := range concrete.GetScanComparisons() {
							if comparisonRange != nil && !comparisonRange.IsEmpty() {
								usedBoundedIndex = true
							}
						}
						return true
					})
					if !usedBoundedIndex {
						t.Fatalf(
							"%s did not bind a bounded scan on %s: %s\n"+
								"an ordered float comparison is representable as one or two exact ranges; "+
								"falling back to an unbounded scan is the access-path regression this pins against",
							indexedSQL, indexName, indexedPlan.Explain(),
						)
					}

					indexedIDs := execute(t, indexedPlan)
					baselineIDs := execute(t, baselinePlan)
					if !slices.Equal(indexedIDs, baselineIDs) {
						t.Fatalf(
							"%s indexed IDs = %v, baseline IDs = %v\nindexed plan: %s\nbaseline plan: %s",
							test.name, indexedIDs, baselineIDs, indexedPlan.Explain(), baselinePlan.Explain(),
						)
					}
					if !slices.Equal(indexedIDs, test.want) {
						t.Fatalf(
							"%s %s IDs = %v, want %v; plan: %s",
							width, test.name, indexedIDs, test.want, indexedPlan.Explain(),
						)
					}
				})
			}
		})
	}
}

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
