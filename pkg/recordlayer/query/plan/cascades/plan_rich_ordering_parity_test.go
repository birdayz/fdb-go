package cascades

// Parity between the physical wrappers' HintRichOrdering and the plans' own
// (RFC-183 P5). The plans answer the rich-ordering question themselves now,
// but the memo still asks the WRAPPER — so nothing at runtime exercises the
// plan-side bodies yet, and a transcription slip in the port would stay
// invisible until the gated wrapper deletion flips the caller over.
//
// These tests are that missing dimension: they drive both bodies over the
// production-shaped input (an index plan STAMPED with its metadata, exactly
// as ToScanPlan does) and assert the two answers agree structurally. Red here
// means the port diverged; the deletion step is what would otherwise have
// discovered it, one shift too late.
//
// Note the wrapper carries its own copies of columnNames/pkColumnNames/unique
// while the plan reads its stamped fields. Parity therefore also pins that the
// two stay in sync — the invariant stampIndexMetadata exists to maintain.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// assertRichOrderingsEqual compares two rich orderings by key sequence,
// per-key bindings, and distinctness — the whole observable surface.
func assertRichOrderingsEqual(t *testing.T, want, got *properties.RichOrdering) {
	t.Helper()
	if want == nil || got == nil {
		t.Fatalf("nil rich ordering: want=%v got=%v", want == nil, got == nil)
	}
	if want.IsDistinct() != got.IsDistinct() {
		t.Errorf("distinct: want %v, got %v", want.IsDistinct(), got.IsDistinct())
	}
	wk, gk := want.GetKeys(), got.GetKeys()
	if len(wk) != len(gk) {
		t.Fatalf("key count: want %d %v, got %d %v",
			len(wk), explainKeys(wk), len(gk), explainKeys(gk))
	}
	for i := range wk {
		if values.ExplainValue(wk[i]) != values.ExplainValue(gk[i]) {
			t.Errorf("key %d: want %s, got %s",
				i, values.ExplainValue(wk[i]), values.ExplainValue(gk[i]))
		}
	}
	wbm, gbm := want.GetBindingMap(), got.GetBindingMap()
	for i, k := range wk {
		wb, gb := bindingsFor(wbm, k), bindingsFor(gbm, gk[i])
		if len(wb) != len(gb) {
			t.Fatalf("binding count for %s: want %d, got %d",
				values.ExplainValue(k), len(wb), len(gb))
		}
		for j := range wb {
			if wb[j].IsFixed() != gb[j].IsFixed() ||
				wb[j].IsSorted() != gb[j].IsSorted() ||
				wb[j].IsChoose() != gb[j].IsChoose() ||
				wb[j].GetSortOrder() != gb[j].GetSortOrder() {
				t.Errorf("binding %d for %s: want (fixed=%v sorted=%v order=%v), got (fixed=%v sorted=%v order=%v)",
					j, values.ExplainValue(k),
					wb[j].IsFixed(), wb[j].IsSorted(), wb[j].GetSortOrder(),
					gb[j].IsFixed(), gb[j].IsSorted(), gb[j].GetSortOrder())
			}
		}
	}
}

func explainKeys(ks []values.Value) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = values.ExplainValue(k)
	}
	return out
}

// bindingsFor looks a key's bindings up by rendering, since the binding map is
// keyed by Value identity and the two orderings build distinct key objects.
func bindingsFor(bm map[values.Value][]properties.OrderingBinding, key values.Value) []properties.OrderingBinding {
	want := values.ExplainValue(key)
	for k, v := range bm {
		if values.ExplainValue(k) == want {
			return v
		}
	}
	return nil
}

func TestRichOrderingParity_IndexScan(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		columnNames   []string
		pkColumnNames []string
		unique        bool
		reverse       bool
		ranges        []*predicates.ComparisonRange
	}{
		{
			name:          "eq-prefix forward with PK suffix",
			columnNames:   []string{"STATUS"},
			pkColumnNames: []string{"ID"},
			ranges:        []*predicates.ComparisonRange{equalityRange(t, "active")},
		},
		{
			name:          "eq-prefix reverse with PK suffix",
			columnNames:   []string{"STATUS"},
			pkColumnNames: []string{"ID"},
			reverse:       true,
			ranges:        []*predicates.ComparisonRange{equalityRange(t, "active")},
		},
		{
			name:          "trimmed PK suffix",
			columnNames:   []string{"B"},
			pkColumnNames: []string{"A", "B", "C"},
			ranges:        []*predicates.ComparisonRange{equalityRange(t, int64(20))},
		},
		{
			name:          "unique index, no comparisons",
			columnNames:   []string{"EMAIL"},
			pkColumnNames: []string{"ID"},
			unique:        true,
		},
		{
			name:          "multi-column index, partial equality prefix",
			columnNames:   []string{"A", "B", "C"},
			pkColumnNames: []string{"ID"},
			ranges:        []*predicates.ComparisonRange{equalityRange(t, int64(1))},
		},
		{
			name:          "fan-out index (empty PK columns)",
			columnNames:   []string{"TAG"},
			pkColumnNames: nil,
			ranges:        []*predicates.ComparisonRange{equalityRange(t, "x")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			idx := plans.NewRecordQueryIndexPlan(
				"IDX", tc.ranges, []string{"T"}, values.UnknownType, tc.reverse,
			).WithIndexMetadata(tc.columnNames, tc.pkColumnNames, tc.unique)

			w := &physicalIndexScanWrapper{
				plan:          idx,
				columnNames:   tc.columnNames,
				pkColumnNames: tc.pkColumnNames,
				unique:        tc.unique,
			}

			assertRichOrderingsEqual(t, w.HintRichOrdering(), idx.HintRichOrdering())
		})
	}
}

func TestRichOrderingParity_PrimaryScan(t *testing.T) {
	t.Parallel()

	pk := []values.Value{
		&values.FieldValue{Field: "A", Typ: values.UnknownType},
		&values.FieldValue{Field: "B", Typ: values.UnknownType},
	}

	cases := []struct {
		name    string
		reverse bool
		ranges  []*predicates.ComparisonRange
	}{
		{name: "forward, no comparisons"},
		{name: "reverse, no comparisons", reverse: true},
		{
			name:   "forward, eq-bound PK prefix",
			ranges: []*predicates.ComparisonRange{equalityRange(t, int64(1))},
		},
		{
			name:    "reverse, eq-bound PK prefix",
			reverse: true,
			ranges:  []*predicates.ComparisonRange{equalityRange(t, int64(1))},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scan := plans.NewRecordQueryScanPlan(
				[]string{"T"}, values.UnknownType, tc.reverse,
			).WithScanComparisons(tc.ranges).WithPrimaryKey(pk)

			w := &physicalScanWrapper{plan: scan}
			assertRichOrderingsEqual(t, w.HintRichOrdering(), scan.HintRichOrdering())
		})
	}
}

// A primary scan with no PK values models no ordering — both sides empty.
func TestRichOrderingParity_PrimaryScanWithoutPK(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	w := &physicalScanWrapper{plan: scan}

	assertRichOrderingsEqual(t, w.HintRichOrdering(), scan.HintRichOrdering())
	if len(scan.HintRichOrdering().GetKeys()) != 0 {
		t.Fatal("PK-less primary scan must provide no ordering keys")
	}
}

// A K-NN probe's neighbours come back in distance order, which is not a column
// ordering — both sides must report EMPTY rather than synthesizing one. This is
// the case where "no ordering" is the answer, and where a delegating
// re-derivation would be most tempting to get wrong by falling through to the
// caller's synthesize-from-HintOrdering fallback.
func TestRichOrderingParity_VectorIndexScan(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		prefixCompare []*predicates.ComparisonRange
	}{
		{name: "unprefixed K-NN"},
		{
			name:          "partition-prefixed K-NN",
			prefixCompare: []*predicates.ComparisonRange{equalityRange(t, "tenant-a")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			vec := plans.NewRecordQueryVectorIndexPlan(
				"VIDX", tc.prefixCompare,
				values.LiteralValue([]float64{1, 2, 3}),
				values.LiteralValue(10),
				predicates.ComparisonDistanceRankLessThanOrEq,
				nil, nil, []string{"DOCS"}, values.UnknownType,
			)
			// RFC-184 W2: the vector scan is its own bare plan expression (no
			// physicalVectorIndexScanWrapper); the memo member IS the plan, so its
			// HintRichOrdering is the plan's directly.
			assertRichOrderingsEqual(t, vec.HintRichOrdering(), properties.EmptyOrdering())
			if len(vec.HintRichOrdering().GetKeys()) != 0 {
				t.Fatal("a K-NN probe must advertise no ordering keys")
			}
		})
	}
}

// Fetch is the divergent one: it is the only rich-ordering DELEGATOR, so the
// two bodies resolve through structurally DIFFERENT references. The wrapper's
// quantifier ranges over the shared memo group it was built over — whose member
// is the index WRAPPER — while the plan's quantifier is the fresh singleton
// QuantifierOverPlan minted, whose member is the index PLAN itself. Parity here
// is the assertion that those two paths land on the same answer, which is
// exactly what the gated wrapper deletion would rely on and what nothing else
// covers.
func TestRichOrderingParity_FetchFromPartialRecord(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		columnNames   []string
		pkColumnNames []string
		unique        bool
		reverse       bool
		ranges        []*predicates.ComparisonRange
	}{
		{
			name:          "covering scan, eq prefix, PK suffix",
			columnNames:   []string{"STATUS"},
			pkColumnNames: []string{"ID"},
			ranges:        []*predicates.ComparisonRange{equalityRange(t, "active")},
		},
		{
			name:          "reverse covering scan",
			columnNames:   []string{"STATUS"},
			pkColumnNames: []string{"ID"},
			reverse:       true,
			ranges:        []*predicates.ComparisonRange{equalityRange(t, "active")},
		},
		{
			name:          "unique covering scan, no comparisons",
			columnNames:   []string{"EMAIL"},
			pkColumnNames: []string{"ID"},
			unique:        true,
		},
		{
			name:          "multi-column index, partial eq prefix",
			columnNames:   []string{"A", "B", "C"},
			pkColumnNames: []string{"ID"},
			ranges:        []*predicates.ComparisonRange{equalityRange(t, int64(1))},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			idx := plans.NewRecordQueryIndexPlan(
				"IDX", tc.ranges, []string{"T"}, values.UnknownType, tc.reverse,
			).WithIndexMetadata(tc.columnNames, tc.pkColumnNames, tc.unique)

			// The plan's own child edge: QuantifierOverPlan mints a fresh
			// singleton holding the index PLAN.
			fetch := plans.NewRecordQueryFetchFromPartialRecordPlan(
				idx, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
			)

			// The wrapper's child edge: a memo group holding the index WRAPPER,
			// as ImplementFetch builds it.
			idxWrapper := &physicalIndexScanWrapper{
				plan:          idx,
				columnNames:   tc.columnNames,
				pkColumnNames: tc.pkColumnNames,
				unique:        tc.unique,
			}
			w := plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(
					expressions.FinalOfAtStage(idxWrapper, expressions.StageCanonical)),
				fetch.GetTranslateValueFunction(), fetch.GetResultType(), fetch.GetFetchIndexRecords(),
			)

			assertRichOrderingsEqual(t, w.HintRichOrdering(), fetch.HintRichOrdering())

			// And both must equal the source scan's ordering — a fetch preserves
			// it. Without this a mutually-empty pair would pass vacuously.
			assertRichOrderingsEqual(t, idx.HintRichOrdering(), fetch.HintRichOrdering())
		})
	}
}

// A fetch over a source that provides no rich ordering yields empty on both
// sides — the nil/empty-source arm of the delegator.
func TestRichOrderingParity_FetchOverUnorderedSource(t *testing.T) {
	t.Parallel()

	// A PK-less primary scan models no ordering.
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	fetch := plans.NewRecordQueryFetchFromPartialRecordPlan(
		scan, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	w := plans.NewRecordQueryFetchFromPartialRecordPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(
			expressions.FinalOfAtStage(&physicalScanWrapper{plan: scan}, expressions.StageCanonical)),
		fetch.GetTranslateValueFunction(), fetch.GetResultType(), fetch.GetFetchIndexRecords(),
	)

	assertRichOrderingsEqual(t, w.HintRichOrdering(), fetch.HintRichOrdering())
	if len(fetch.HintRichOrdering().GetKeys()) != 0 {
		t.Fatal("a fetch over an unordered source must provide no ordering keys")
	}
}
