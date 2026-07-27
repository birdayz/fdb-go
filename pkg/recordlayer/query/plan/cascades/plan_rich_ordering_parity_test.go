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

// nonEqualityRange builds a non-equality (>) ComparisonRange, the gap
// shape equalityPrefixLen (plans/ordering.go) must stop a prefix at.
func nonEqualityRange(t *testing.T, literal any) *predicates.ComparisonRange {
	t.Helper()
	cmp := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, literal)
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatal("failed to build non-equality comparison range")
	}
	return res.Range
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
		// wantFields/wantFixed describe the FULL expected rich ordering, in
		// output order: each key's field name and whether its binding is
		// FixedBinding (true, equality-bound) or SortedBinding (false).
		wantFields []string
		wantFixed  []bool
	}{
		{
			name:          "eq-prefix forward with PK suffix",
			columnNames:   []string{"STATUS"},
			pkColumnNames: []string{"ID"},
			ranges:        []*predicates.ComparisonRange{equalityRange(t, "active")},
			wantFields:    []string{"STATUS", "ID"},
			wantFixed:     []bool{true, false},
		},
		{
			name:          "eq-prefix reverse with PK suffix",
			columnNames:   []string{"STATUS"},
			pkColumnNames: []string{"ID"},
			reverse:       true,
			ranges:        []*predicates.ComparisonRange{equalityRange(t, "active")},
			wantFields:    []string{"STATUS", "ID"},
			wantFixed:     []bool{true, false},
		},
		{
			name:          "trimmed PK suffix",
			columnNames:   []string{"B"},
			pkColumnNames: []string{"A", "B", "C"},
			ranges:        []*predicates.ComparisonRange{equalityRange(t, int64(20))},
			wantFields:    []string{"B", "A", "C"},
			wantFixed:     []bool{true, false, false},
		},
		{
			name:          "unique index, no comparisons",
			columnNames:   []string{"EMAIL"},
			pkColumnNames: []string{"ID"},
			unique:        true,
			wantFields:    []string{"EMAIL", "ID"},
			wantFixed:     []bool{false, false},
		},
		{
			name:          "multi-column index, partial equality prefix",
			columnNames:   []string{"A", "B", "C"},
			pkColumnNames: []string{"ID"},
			ranges:        []*predicates.ComparisonRange{equalityRange(t, int64(1))},
			wantFields:    []string{"A", "B", "C", "ID"},
			wantFixed:     []bool{true, false, false, false},
		},
		{
			name:          "fan-out index (empty PK columns)",
			columnNames:   []string{"TAG"},
			pkColumnNames: nil,
			ranges:        []*predicates.ComparisonRange{equalityRange(t, "x")},
			wantFields:    []string{"TAG"},
			wantFixed:     []bool{true},
		},
		{
			// The shape that was previously unreachable-but-undefined: an
			// equality resumes AFTER a non-equality gap (A=1, B>3, C=9). The
			// gap already breaks the contiguous-range guarantee, so C must
			// stay Sorted despite testing equal — see equalityPrefixLen's doc
			// in plans/ordering.go for why FixedBinding here would be wrong.
			name:          "resumed equality after gap stays Sorted, not Fixed",
			columnNames:   []string{"A", "B", "C"},
			pkColumnNames: []string{"ID"},
			ranges: []*predicates.ComparisonRange{
				equalityRange(t, int64(1)),
				nonEqualityRange(t, int64(3)),
				equalityRange(t, int64(9)),
			},
			wantFields: []string{"A", "B", "C", "ID"},
			wantFixed:  []bool{true, false, false, false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			idx := plans.NewRecordQueryIndexPlan(
				"IDX", tc.ranges, []string{"T"}, values.UnknownType, tc.reverse,
			).WithIndexMetadata(tc.columnNames, tc.pkColumnNames, tc.unique)

			// The index scan is its own cascades expression now (RFC-184 W2): the
			// memo asks the PLAN's HintRichOrdering directly (no physicalIndexScanWrapper
			// to compare against). Exercise the plan-side body over the stamped-
			// metadata input against an INDEPENDENTLY stated expected shape (not
			// just non-emptiness) — the same structural bar TestRichOrderingParity_PrimaryScan
			// holds itself to. The end-to-end ordering shapes are additionally
			// pinned by the yamsql sort-elimination corpus.
			got := idx.HintRichOrdering()
			if got == nil {
				t.Fatal("nil rich ordering")
			}
			gkeys := got.GetKeys()
			if len(gkeys) != len(tc.wantFields) {
				t.Fatalf("keys = %v, want %v", explainKeys(gkeys), tc.wantFields)
			}
			dir := properties.ProvidedSortOrderAscending
			if tc.reverse {
				dir = properties.ProvidedSortOrderDescending
			}
			gbm := got.GetBindingMap()
			for i, field := range tc.wantFields {
				fv, ok := gkeys[i].(*values.FieldValue)
				if !ok || fv.Field != field {
					t.Fatalf("key %d = %s, want field %q", i, values.ExplainValue(gkeys[i]), field)
				}
				bindings := bindingsFor(gbm, gkeys[i])
				if len(bindings) != 1 {
					t.Fatalf("field %q bindings = %v, want exactly one", field, bindings)
				}
				b := bindings[0]
				switch {
				case tc.wantFixed[i] && !b.IsFixed():
					t.Errorf("field %q binding = %#v, want FixedBinding", field, b)
				case !tc.wantFixed[i] && (!b.IsSorted() || b.GetSortOrder() != dir):
					t.Errorf("field %q binding = %#v, want SortedBinding(%v)", field, b, dir)
				}
			}
			if got.IsDistinct() {
				t.Errorf("IsDistinct() = true, want false (strictlySorted is only set via WithStrictlySorted, which none of these cases call)")
			}
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

			w := scan
			assertRichOrderingsEqual(t, w.HintRichOrdering(), scan.HintRichOrdering())
		})
	}
}

// A primary scan with no PK values models no ordering — both sides empty.
func TestRichOrderingParity_PrimaryScanWithoutPK(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	w := scan

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

			// The fetch's child edge: QuantifierOverPlan mints a fresh singleton
			// holding the index PLAN, which is its own cascades expression now
			// (RFC-184 W2 — no physicalIndexScanWrapper).
			fetch := plans.NewRecordQueryFetchFromPartialRecordPlan(
				idx, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
			)

			// The fetch must delegate the source index scan's rich ordering — a fetch
			// preserves it. Non-vacuous: the index carries a real ordering from its
			// stamped metadata.
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
			expressions.FinalOfAtStage(scan, expressions.StageCanonical)),
		fetch.GetTranslateValueFunction(), fetch.GetResultType(), fetch.GetFetchIndexRecords(),
	)

	assertRichOrderingsEqual(t, w.HintRichOrdering(), fetch.HintRichOrdering())
	if len(fetch.HintRichOrdering().GetKeys()) != 0 {
		t.Fatal("a fetch over an unordered source must provide no ordering keys")
	}
}
