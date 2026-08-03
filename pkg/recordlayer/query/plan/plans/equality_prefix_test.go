package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestEqualityPrefixLen drives the shared helper directly, including the
// resumed-equality-after-gap shape that neither hand-coded loop it replaced
// agreed on: PKScanOrdering/RecordQueryIndexPlan.HintOrdering's prefix loop
// BROKE at the first non-equality comparison, while RecordQueryScanPlan/
// RecordQueryIndexPlan's HintRichOrdering loop did not — it tested
// comps[i].IsEquality() at every position independently, so a resumed
// equality past a gap would have been marked FixedBinding there while
// HintOrdering kept it as an ordinary (non-dropped) key. That shape is
// unreachable through the sole production constructor
// (ValueIndexScanMatchCandidate.ComputeBoundParameterPrefixMap always stops
// at the first inequality/unbound parameter) but was previously undefined
// by construction rather than deliberately settled — this pins the
// conservative reading equalityPrefixLen now enforces for both callers.
func TestEqualityPrefixLen(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		eqAt []bool // true = equality comparison at that position, false = non-equality (>)
		n    int
		want int
	}{
		{name: "no comparisons", eqAt: nil, n: 3, want: 0},
		{name: "all equality", eqAt: []bool{true, true, true}, n: 3, want: 3},
		{name: "leading non-equality", eqAt: []bool{false}, n: 3, want: 0},
		{name: "equality then gap", eqAt: []bool{true, false}, n: 3, want: 1},
		{
			name: "resumed equality after gap does NOT extend the prefix",
			eqAt: []bool{true, false, true},
			n:    3,
			want: 1,
		},
		{
			name: "n caps the scan even when comps runs longer",
			eqAt: []bool{true, true, true},
			n:    2,
			want: 2,
		},
		{
			name: "comps shorter than n stops at len(comps)",
			eqAt: []bool{true},
			n:    3,
			want: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			comps := make([]*predicates.ComparisonRange, len(tc.eqAt))
			for i, eq := range tc.eqAt {
				if eq {
					comps[i] = pkOrderingEq(t, int64(i))
				} else {
					comps[i] = pkOrderingGT(t, int64(i))
				}
			}
			if got := equalityPrefixLen(comps, tc.n); got != tc.want {
				t.Fatalf("equalityPrefixLen(%v, %d) = %d, want %d", tc.eqAt, tc.n, got, tc.want)
			}
		})
	}
}

// TestPKScanOrdering_ResumedEqualityAfterGap_AgreesWithRichOrdering is the
// end-to-end pin for a primary scan: id=7 (equality), k>3 (range), m=9
// (resumed equality). Before equalityPrefixLen, HintOrdering (breaks at the
// gap) would report [k, m] as ordering keys while HintRichOrdering (no
// break) would mark m FixedBinding — two different answers to "is m an
// ordering key" for the identical scan. Both must now agree: only id is
// dropped/fixed; k and m are ordinary Sorted keys.
func TestPKScanOrdering_ResumedEqualityAfterGap_AgreesWithRichOrdering(t *testing.T) {
	t.Parallel()
	id := &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	k := &values.FieldValue{Field: "K", Typ: values.NotNullLong}
	m := &values.FieldValue{Field: "M", Typ: values.NotNullLong}
	comps := []*predicates.ComparisonRange{
		pkOrderingEq(t, int64(7)),
		pkOrderingGT(t, int64(3)),
		pkOrderingEq(t, int64(9)),
	}
	plan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey([]values.Value{id, k, m}).
		WithScanComparisons(comps)

	plain := plan.HintOrdering()
	if !plain.IsKnown || len(plain.Keys) != 2 || plain.Keys[0] != k || plain.Keys[1] != m {
		t.Fatalf("HintOrdering(id=7,k>3,m=9) = %#v, want [K, M] (only ID dropped, M stays a key despite testing equal)", plain)
	}

	rich := plan.HintRichOrdering()
	rbm := rich.GetBindingMap()
	rkeys := rich.GetKeys()
	if len(rkeys) != 3 {
		t.Fatalf("HintRichOrdering(id=7,k>3,m=9) keys = %#v, want 3 (ID, K, M)", rkeys)
	}
	idBindings := rbm[rkeys[0]]
	kBindings := rbm[rkeys[1]]
	mBindings := rbm[rkeys[2]]
	if len(idBindings) != 1 || !idBindings[0].IsFixed() {
		t.Fatalf("ID binding = %#v, want a single FixedBinding", idBindings)
	}
	if len(kBindings) != 1 || !kBindings[0].IsSorted() {
		t.Fatalf("K binding = %#v, want a single SortedBinding", kBindings)
	}
	if len(mBindings) != 1 || !mBindings[0].IsSorted() {
		t.Fatalf("M binding = %#v, want SortedBinding (a resumed equality after a gap must NOT be FixedBinding)", mBindings)
	}

	// The two derivations must classify the SAME columns as ordering keys:
	// plain's Keys (the columns NOT dropped as equality-bound) must be exactly
	// the rich columns whose binding is Sorted rather than Fixed.
	var richSortedFields []string
	for _, key := range rkeys {
		if rbm[key][0].IsSorted() {
			richSortedFields = append(richSortedFields, key.(*values.FieldValue).Field)
		}
	}
	if len(richSortedFields) != len(plain.Keys) {
		t.Fatalf("plain ordering keys %v disagree with rich ordering's sorted columns %v", plain.Keys, richSortedFields)
	}
	for i, key := range plain.Keys {
		if key.(*values.FieldValue).Field != richSortedFields[i] {
			t.Fatalf("plain ordering keys %v disagree with rich ordering's sorted columns %v at position %d", plain.Keys, richSortedFields, i)
		}
	}
}

// TestRecordQueryIndexPlan_ResumedEqualityAfterGap_AgreesWithRichOrdering is
// the index-scan analog: A=1 (equality), B>3 (range), C=9 (resumed
// equality), over index (A, B, C) with no trimmed PK suffix (PK IS the index
// prefix). Same disagreement risk as the PK-scan case above, just walking
// columnNames instead of PK Values.
func TestRecordQueryIndexPlan_ResumedEqualityAfterGap_AgreesWithRichOrdering(t *testing.T) {
	t.Parallel()
	comps := []*predicates.ComparisonRange{
		pkOrderingEq(t, int64(1)),
		pkOrderingGT(t, int64(3)),
		pkOrderingEq(t, int64(9)),
	}
	plan := NewRecordQueryIndexPlan("IDX", comps, []string{"T"}, values.UnknownType, false).
		WithIndexMetadata([]string{"A", "B", "C"}, []string{"A", "B", "C"}, false)

	plain := plan.HintOrdering()
	if !plain.IsKnown || len(plain.Keys) != 2 {
		t.Fatalf("HintOrdering(A=1,B>3,C=9) = %#v, want [B, C] (only A dropped)", plain)
	}
	wantPlain := []string{"B", "C"}
	for i, w := range wantPlain {
		fv, ok := plain.Keys[i].(*values.FieldValue)
		if !ok || fv.Field != w {
			t.Fatalf("HintOrdering(A=1,B>3,C=9).Keys[%d] = %#v, want field %q", i, plain.Keys[i], w)
		}
	}

	rich := plan.HintRichOrdering()
	rbm := rich.GetBindingMap()
	rkeys := rich.GetKeys()
	if len(rkeys) != 3 {
		t.Fatalf("HintRichOrdering(A=1,B>3,C=9) keys = %#v, want 3 (A, B, C)", rkeys)
	}
	wantRich := []struct {
		field string
		fixed bool
	}{
		{"A", true},
		{"B", false},
		{"C", false}, // resumed equality: must stay Sorted, not Fixed
	}
	for i, w := range wantRich {
		fv, ok := rkeys[i].(*values.FieldValue)
		if !ok || fv.Field != w.field {
			t.Fatalf("HintRichOrdering.Keys[%d] = %#v, want field %q", i, rkeys[i], w.field)
		}
		bindings := rbm[rkeys[i]]
		if len(bindings) != 1 {
			t.Fatalf("%s bindings = %#v, want exactly one", w.field, bindings)
		}
		if bindings[0].IsFixed() != w.fixed || bindings[0].IsSorted() != !w.fixed {
			t.Fatalf("%s binding = %#v, want fixed=%v sorted=%v", w.field, bindings[0], w.fixed, !w.fixed)
		}
	}
}
