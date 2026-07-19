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
