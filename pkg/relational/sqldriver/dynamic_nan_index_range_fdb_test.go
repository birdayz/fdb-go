package sqldriver_test

// A dynamic NaN followed by a bound component over a composite index (RFC-257
// WS-E 5.3): the scan reads both NaN key blocks and filters each entry's later
// component below the continuation. It used to refuse with
// UnsupportedPhysicalFloatEquivalenceError.

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
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

func TestFDB_DynamicNaNCompositeIndexKeyFilter(t *testing.T) {
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
		if !merged.Complete() {
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

			plan, planErr := plans.NewRecordQueryIndexPlan(
				"V_W",
				[]*predicates.ComparisonRange{parameterRange, suffixRange},
				[]string{"T"},
				executor.PositionalTypeForDescriptor(md.GetRecordType("T").Descriptor),
				false,
			)
			if planErr != nil {
				t.Fatalf("construct exact %s index plan: %v", width, planErr)
			}
			plan = plan.WithKeyComponentTypes([]values.Type{physicalType, values.NotNullLong})

			// NaNs of both signs and several payloads, beside the
			// infinities, a number, and a NaN of the wrong suffix.
			type row struct {
				id   int64
				bits uint64
				w    int64
			}
			rows := []row{
				{1, 0x7ff8000000000000, 5},
				{2, 0xfff8000000000000, 5},
				{3, 0x7ff8000000000000, 6},
				{4, math.Float64bits(1.5), 5},
				{5, math.Float64bits(math.Inf(1)), 5},
				{6, 0x7ff800000000abcd, 5},
				{7, math.Float64bits(math.Inf(-1)), 5},
				{8, 0xfff800000000abcd, 4},
			}
			_, saveErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, openErr := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if openErr != nil {
					return nil, openErr
				}
				descriptor := md.GetRecordType("T").Descriptor
				for _, r := range rows {
					message := dynamicpb.NewMessage(descriptor)
					message.Set(descriptor.Fields().ByName("ID"), protoreflect.ValueOfInt64(r.id))
					if width == "FLOAT" {
						message.Set(descriptor.Fields().ByName("V"),
							protoreflect.ValueOfFloat32(float32(math.Float64frombits(r.bits))))
					} else {
						message.Set(descriptor.Fields().ByName("V"),
							protoreflect.ValueOfFloat64(math.Float64frombits(r.bits)))
					}
					message.Set(descriptor.Fields().ByName("W"), protoreflect.ValueOfInt64(r.w))
					if _, err := store.SaveRecord(message); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			if saveErr != nil {
				t.Fatalf("save: %v", saveErr)
			}

			// run executes the plan for the NaN probe, resuming from each
			// page's continuation; scanLimit 1 makes most pages end on an
			// entry the key filter rejected.
			run := func(t *testing.T, reverse bool, scanLimit int) []int64 {
				t.Helper()
				p := plan
				if reverse {
					var err error
					p, err = plans.NewRecordQueryIndexPlan(
						"V_W", []*predicates.ComparisonRange{parameterRange, suffixRange}, []string{"T"},
						executor.PositionalTypeForDescriptor(md.GetRecordType("T").Descriptor), true)
					if err != nil {
						t.Fatal(err)
					}
					p = p.WithKeyComponentTypes([]values.Type{physicalType, values.NotNullLong})
				}
				var ids []int64
				var continuation []byte
				for page := 0; ; page++ {
					if page > 100 {
						t.Fatal("the scan does not terminate")
					}
					props := recordlayer.DefaultExecuteProperties()
					props.ScannedRecordsLimit = scanLimit
					var done bool
					_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
						store, err := recordlayer.NewStoreBuilder().
							SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
						if err != nil {
							return nil, err
						}
						cursor, err := executor.ExecutePlan(ctx, p, store,
							executor.EmptyEvaluationContext().WithParams([]any{math.Float64frombits(0x7ff8000000000001)}),
							continuation, props)
						if err != nil {
							return nil, err
						}
						defer cursor.Close()
						for {
							r, err := cursor.OnNext(ctx)
							if err != nil {
								return nil, err
							}
							if !r.HasNext() {
								c, err := r.GetContinuation().ToBytes()
								if err != nil {
									return nil, err
								}
								continuation, done = c, r.GetContinuation().IsEnd()
								return nil, nil
							}
							ids = append(ids, r.GetValue().PrimaryKey[0].(int64))
						}
					})
					if err != nil {
						t.Fatalf("page %d: %v", page, err)
					}
					if done {
						return ids
					}
				}
			}
			for _, reverse := range []bool{false, true} {
				for _, limit := range []int{0, 1, 2} {
					ids := run(t, reverse, limit)
					sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
					if !slices.Equal(ids, []int64{1, 2, 6}) {
						t.Fatalf("%s reverse=%t scan limit %d: ids %v, want [1 2 6] (every NaN with W = 5, once)",
							width, reverse, limit, ids)
					}
				}
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
					// planBindsBoundedScanOn sees through a COVERING wrapper. This
					// guard used to walk with a concrete *RecordQueryIndexPlan
					// assertion, which is BLIND BY CONSTRUCTION since RFC-220: the
					// covering plan holds its index plan as a field, plans.Walk
					// recurses through GetChildren(), and GetChildren() returns nil.
					// The guard then reported "no bounded scan" for
					// `IndexScan(VALUE_IDX, [<>] COVERING)` — a plan carrying exactly
					// the bounded inequality range it demands. See
					// covering_plan_assertions_test.go for the general rule.
					_, usedBoundedIndex := planBindsBoundedScanOn(indexedPlan, indexName)
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
