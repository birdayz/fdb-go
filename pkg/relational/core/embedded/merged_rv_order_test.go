package embedded

import (
	"fmt"
	"strconv"
	"testing"

	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// mergedInRVOrder decides, on a plain multi-way `SELECT *`, whether the
// qualified leg-merge can be re-sequenced into the query's own column order.
// Every arm is driven here rather than only through the corpus: which grouping
// the planner picks for equal-cost legs is an arbitrary tie, so the corpus
// reaches an arm only by accident of costing, and a refusal arm that is rare
// today is one a later change makes live.
//
// The leg resolver is injected, so these drive the permutation assembly itself.
// The production resolver (legSlotIndex, which walks a path down a leg's own
// join tree) is exercised e2e by TestStarMetadataPlainThreeWayKeepsQualifiedNames
// and TestFDB_DuplicateFromAliases — the two shapes whose leg trees disagree
// with their column order in opposite directions.

func mergedRVTestRoot(t *testing.T, alias string, fields ...values.Field) values.QuantifiedObjectValue {
	t.Helper()
	row := values.NewRecordType("", false, fields)
	root, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias), row)
	if err != nil {
		t.Fatalf("root %s: %v", alias, err)
	}
	return root
}

func mergedRVTestField(t *testing.T, root values.Value, ordinals ...int) values.Value {
	t.Helper()
	field, err := values.ResolveFieldOrdinals(root, ordinals)
	if err != nil {
		t.Fatalf("resolve %v: %v", ordinals, err)
	}
	return field
}

func mergedRVTestColumns(names ...string) []executor.ColumnDef {
	cols := make([]executor.ColumnDef, 0, len(names))
	for _, name := range names {
		cols = append(cols, executor.ColumnDef{Name: name, Label: name})
	}
	return cols
}

func mergedRVTestNames(cols []executor.ColumnDef) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Name)
	}
	return out
}

// mergedRVTestSlots is a leg resolver keyed on the path's own spelling. Using
// the path rather than the order of appearance is what a real leg does: a
// sub-join's row slots follow its PHYSICAL leg order while its columns come
// back in SQL order, so the two sequences are independent and the assembly must
// not assume either one.
func mergedRVTestSlots(byPath map[string]int) func(string, []int) (int, bool) {
	return func(alias string, path []int) (int, bool) {
		key := alias + ":"
		for i, ordinal := range path {
			if i > 0 {
				key += "."
			}
			key += strconv.Itoa(ordinal)
		}
		index, ok := byPath[key]
		return index, ok
	}
}

// Two legs interleaved across the RC, the way `SELECT * FROM TA, TB, TC` is
// when the planner groups `TB ⋈ (TA ⋈ TC)`: the merge holds the sub-join's
// four columns first and TB's two after, while the query wants them alternating.
func TestMergedInRVOrderResequencesInterleavedLegs(t *testing.T) {
	t.Parallel()
	long := values.NullableLong
	tb := mergedRVTestRoot(t, "TB",
		values.Field{Name: "TBID", Ordinal: 0, FieldType: long},
		values.Field{Name: "K", Ordinal: 1, FieldType: long})
	subRow := values.NewRecordType("", false, []values.Field{
		{Name: "TAID", Ordinal: 0, FieldType: long},
		{Name: "K", Ordinal: 1, FieldType: long},
	})
	subRow2 := values.NewRecordType("", false, []values.Field{
		{Name: "TCID", Ordinal: 0, FieldType: long},
		{Name: "K", Ordinal: 1, FieldType: long},
	})
	merge := mergedRVTestRoot(t, "$M1",
		values.Field{Name: "_0", Ordinal: 0, FieldType: subRow},
		values.Field{Name: "_1", Ordinal: 1, FieldType: subRow2})

	rc := &values.RecordConstructorValue{Fields: []values.RecordConstructorField{
		{Name: "TAID", Value: mergedRVTestField(t, merge, 0, 0)},
		{Name: "K", Value: mergedRVTestField(t, merge, 0, 1)},
		{Name: "TBID", Value: mergedRVTestField(t, tb, 0)},
		{Name: "K", Value: mergedRVTestField(t, tb, 1)},
		{Name: "TCID", Value: mergedRVTestField(t, merge, 1, 0)},
		{Name: "K", Value: mergedRVTestField(t, merge, 1, 1)},
	}}
	merged := mergedRVTestColumns("TA.TAID", "TA.K", "TC.TCID", "TC.K", "TB.TBID", "TB.K")
	slots := mergedRVTestSlots(map[string]int{
		"$M1:0.0": 0, "$M1:0.1": 1, "$M1:1.0": 2, "$M1:1.1": 3,
		"TB:0": 0, "TB:1": 1,
	})

	got, ok := mergedInRVOrder(rc, merged, "$M1", "TB", 4, slots)
	if !ok {
		t.Fatal("interleaved legs were not re-sequenced; the qualified datum keys would be dropped")
	}
	want := []string{"TA.TAID", "TA.K", "TB.TBID", "TB.K", "TC.TCID", "TC.K"}
	if fmt.Sprintf("%v", mergedRVTestNames(got)) != fmt.Sprintf("%v", want) {
		t.Fatalf("re-sequenced = %v, want %v", mergedRVTestNames(got), want)
	}
}

// The leg whose ROW order and COLUMN order disagree — `SELECT * FROM p, q, p`
// planned `(P ⋈ Q) ⋈ P`, where the sub-join emits `<_0 Q, _1 P>` and derives
// `[P.ID P.V Q.QID]`. Ranking a leg's slots by path order, or by their order of
// appearance in the RC, both get this wrong; only asking the leg does.
func TestMergedInRVOrderFollowsTheLegRatherThanTheSlotOrder(t *testing.T) {
	t.Parallel()
	long := values.NullableLong
	p := mergedRVTestRoot(t, "P",
		values.Field{Name: "ID", Ordinal: 0, FieldType: long},
		values.Field{Name: "V", Ordinal: 1, FieldType: long})
	qRow := values.NewRecordType("", false, []values.Field{
		{Name: "QID", Ordinal: 0, FieldType: long},
	})
	dupRow := values.NewRecordType("", false, []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: long},
		{Name: "V", Ordinal: 1, FieldType: long},
	})
	merge := mergedRVTestRoot(t, "$M5",
		values.Field{Name: "_0", Ordinal: 0, FieldType: qRow},
		values.Field{Name: "_1", Ordinal: 1, FieldType: dupRow})

	rc := &values.RecordConstructorValue{Fields: []values.RecordConstructorField{
		{Name: "ID", Value: mergedRVTestField(t, p, 0)},
		{Name: "V", Value: mergedRVTestField(t, p, 1)},
		{Name: "QID", Value: mergedRVTestField(t, merge, 0, 0)},
		{Name: "ID", Value: mergedRVTestField(t, merge, 1, 0)},
		{Name: "V", Value: mergedRVTestField(t, merge, 1, 1)},
	}}
	merged := mergedRVTestColumns("P.ID", "P.V", "Q$DUP2.ID", "Q$DUP2.V", "Q.QID")
	// The sub-join's own answer: row slot [0] is Q (its LAST column) and row
	// slot [1] is the dup P (its FIRST two).
	slots := mergedRVTestSlots(map[string]int{
		"P:0": 0, "P:1": 1,
		"$M5:0.0": 2, "$M5:1.0": 0, "$M5:1.1": 1,
	})

	got, ok := mergedInRVOrder(rc, merged, "P", "$M5", 2, slots)
	if !ok {
		t.Fatal("reversed sub-join leg was not re-sequenced")
	}
	want := []string{"P.ID", "P.V", "Q.QID", "Q$DUP2.ID", "Q$DUP2.V"}
	if fmt.Sprintf("%v", mergedRVTestNames(got)) != fmt.Sprintf("%v", want) {
		t.Fatalf("re-sequenced = %v, want %v — the labels this produces are Java's "+
			"[ID V QID ID V] star layout", mergedRVTestNames(got), want)
	}
}

// TestMergedInRVOrderRootGuardRestsOnFieldValueAdmission pins the premise that
// makes mergedInRVOrder's "root is not a quantified object" guard unreachable
// rather than untested: AsFieldValue admits nothing whose child is not an exact
// QOV, so no resolved read can present a non-QOV root. The guard stays as the
// structural response if that admission is ever loosened, and this is what
// would go red first.
func TestMergedInRVOrderRootGuardRestsOnFieldValueAdmission(t *testing.T) {
	t.Parallel()
	root := mergedRVTestRoot(t, "L",
		values.Field{Name: "A", Ordinal: 0, FieldType: values.NullableLong})
	nested := mergedRVTestField(t, root, 0)
	field, isField := values.AsFieldValue(nested)
	if !isField {
		t.Fatal("fixture is not an admitted FieldValue")
	}
	if _, isRoot := values.AsQuantifiedObjectValue(field.ChildValue()); !isRoot {
		t.Fatal("an admitted FieldValue presented a non-QOV child — mergedInRVOrder's " +
			"root guard just became reachable and needs its own refusal arm")
	}
}

// Every REFUSAL arm. Each one leaves the caller on its existing RC-derived
// answer, so a refusal that silently became an acceptance would emit a
// confidently WRONG column order rather than a visibly bare-labelled one.
func TestMergedInRVOrderRefusesWhatItCannotAlign(t *testing.T) {
	t.Parallel()
	long := values.NullableLong
	left := mergedRVTestRoot(t, "L",
		values.Field{Name: "A", Ordinal: 0, FieldType: long},
		values.Field{Name: "B", Ordinal: 1, FieldType: long})
	right := mergedRVTestRoot(t, "R",
		values.Field{Name: "C", Ordinal: 0, FieldType: long},
		values.Field{Name: "D", Ordinal: 1, FieldType: long})
	foreign := mergedRVTestRoot(t, "FOREIGN",
		values.Field{Name: "X", Ordinal: 0, FieldType: long})
	merged := mergedRVTestColumns("L.A", "L.B", "R.C", "R.D")
	slots := mergedRVTestSlots(map[string]int{
		"L:0": 0, "L:1": 1, "R:0": 0, "R:1": 1,
	})

	balanced := func() *values.RecordConstructorValue {
		return &values.RecordConstructorValue{Fields: []values.RecordConstructorField{
			{Name: "A", Value: mergedRVTestField(t, left, 0)},
			{Name: "C", Value: mergedRVTestField(t, right, 0)},
			{Name: "B", Value: mergedRVTestField(t, left, 1)},
			{Name: "D", Value: mergedRVTestField(t, right, 1)},
		}}
	}
	// The control: the very same input DOES align, so every refusal below is
	// attributable to the one thing it mutates and not to the fixture.
	if got, ok := mergedInRVOrder(balanced(), merged, "L", "R", 2, slots); !ok {
		t.Fatal("control input refused; the refusal arms below would all be vacuous")
	} else if want := []string{"L.A", "R.C", "L.B", "R.D"}; fmt.Sprintf("%v", mergedRVTestNames(got)) != fmt.Sprintf("%v", want) {
		t.Fatalf("control re-sequenced = %v, want %v", mergedRVTestNames(got), want)
	}

	notAField := balanced()
	notAField.Fields[0].Value = &values.ConstantValue{Value: int64(1), Typ: long}

	foreignRoot := balanced()
	foreignRoot.Fields[0].Value = mergedRVTestField(t, foreign, 0)

	duplicatePath := balanced()
	duplicatePath.Fields[2].Value = mergedRVTestField(t, left, 0)

	shortRC := &values.RecordConstructorValue{Fields: balanced().Fields[:3]}

	// A resolver that cannot answer at all: the leg's path is outside anything
	// it derives. This is legSlotIndex declining, which a wrapper it does not
	// recognize or a non-positional leg row both produce.
	refusingSlots := mergedRVTestSlots(map[string]int{"L:0": 0, "L:1": 1, "R:0": 0})
	// A resolver whose index is outside the merge — a leg that reported more
	// columns than the merge carries for it.
	outOfRangeSlots := mergedRVTestSlots(map[string]int{
		"L:0": 0, "L:1": 1, "R:0": 7, "R:1": 1,
	})

	for _, tc := range []struct {
		name                    string
		rc                      *values.RecordConstructorValue
		firstAlias, secondAlias string
		firstWidth              int
		slots                   func(string, []int) (int, bool)
		why                     string
	}{
		{
			"count mismatch", shortRC, "L", "R", 2, slots,
			"an RC that does not account for every merged column cannot state a permutation",
		},
		{
			"field is not a resolved read", notAField, "L", "R", 2, slots,
			"a computed slot has no ordinal to align on",
		},
		{
			"root is neither leg", foreignRoot, "L", "R", 2, slots,
			"a third correlation means the merge is not this join's two legs",
		},
		{
			"two slots claim one position", duplicatePath, "L", "R", 2, slots,
			"a repeated position leaves some merged column unreferenced",
		},
		{
			"leg cannot resolve its own path", balanced(), "L", "R", 2, refusingSlots,
			"an unresolved slot means the leg's row shape is not understood",
		},
		{
			"leg reports a position outside the merge", balanced(), "L", "R", 2, outOfRangeSlots,
			"an index past the merge would read another leg's column or panic",
		},
		{
			"width outside the merge", balanced(), "L", "R", 9, slots,
			"a first-block width past the merge would index out of it",
		},
		{
			"one alias missing", balanced(), "L", "", 2, slots,
			"an unnamed leg cannot be told from the other",
		},
		{
			"both aliases equal", balanced(), "L", "L", 2, slots,
			"indistinguishable legs make every root ambiguous",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, ok := mergedInRVOrder(
				tc.rc, merged, tc.firstAlias, tc.secondAlias, tc.firstWidth, tc.slots,
			); ok {
				t.Fatalf("aligned %v, want refusal — %s", mergedRVTestNames(got), tc.why)
			}
		})
	}
}

// frozenSchemaRenamesSlot is the one thing separating a RENAME from a
// machinery DEDUP when a projection's frozen output schema disagrees with the
// name a projected item carries. Both look identical at the slot — `N` over a
// read of `ID`, `X_2` over an alias `X` — and only the NATURAL schema (the same
// program and aliases, deduplicated by the same rule) says which one it is: a
// frozen name the natural schema also produces is the dedup; one it does not is
// a rename. The earlier answer looked for the label at an EARLIER slot, which
// is what a dedup leaves behind but not what it is: a rename of a repeated
// alias (`WITH r(a, b) AS (SELECT x AS a, y AS a …)`) was misread as a dedup
// and the second column kept the alias the column list had replaced. Driven
// directly because the readings are one boolean apart and the corpus reaches
// each through a different query shape.
func TestFrozenSchemaRenamesSlotSeparatesRenameFromDedup(t *testing.T) {
	t.Parallel()
	repeatedAlias := values.DedupFieldNames([]string{"A", "A"})
	single := values.DedupFieldNames([]string{"ID"})
	for _, tc := range []struct {
		name    string
		natural []string
		slot    int
		frozen  string
		want    bool
		why     string
	}{
		{
			"dedup of a repeated alias", repeatedAlias, 1, "A_2", false,
			"the natural schema names the second A exactly so; the label keeps the alias",
		},
		{
			"first of the repeated pair", repeatedAlias, 0, "A", false,
			"nothing was renamed at slot 0",
		},
		{
			"column-list rename of the repeated alias", repeatedAlias, 1, "B", true,
			"B is a name the natural schema never produces, so the schema renamed the slot",
		},
		{
			"column-list rename of a lone reference", single, 0, "N", true,
			"N over a read of ID is a rename, not a dedup",
		},
		{
			"slot past the natural schema", single, 1, "N", true,
			"a slot the natural schema does not describe cannot be a dedup of it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := frozenSchemaRenamesSlot(tc.natural, tc.slot, tc.frozen); got != tc.want {
				t.Fatalf("frozenSchemaRenamesSlot(natural, %d, %q) = %v, want %v — %s",
					tc.slot, tc.frozen, got, tc.want, tc.why)
			}
		})
	}
}

// The name-model leg merge publishes a repeated leg column under its SQL label
// twice ([G G]) while the ordinal RC names the second slot by the
// name-addressability suffix (G_2). That pair is the SAME sequence, exactly as
// positionalAligned reads it at serve time; reading it as a divergence routed
// every duplicate-name leg through the RC-derived fallback, which publishes
// the suffix as a column label. A genuinely different name, or a reordering,
// still diverges.
func TestMergedRVSequenceDivergesToleratesARepeatedDisplayName(t *testing.T) {
	t.Parallel()
	rc := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "G", Value: values.LiteralValue(int64(0))},
		values.RecordConstructorField{Name: "G_2", Value: values.LiteralValue(int64(0))},
		values.RecordConstructorField{Name: "ID", Value: values.LiteralValue(int64(0))},
		values.RecordConstructorField{Name: "W", Value: values.LiteralValue(int64(0))},
	)
	col := func(name, label string) executor.ColumnDef { return executor.ColumnDef{Name: name, Label: label} }
	for _, tc := range []struct {
		name   string
		merged []executor.ColumnDef
		want   bool
		why    string
	}{
		{
			"repeated display over the suffixed slot",
			[]executor.ColumnDef{col("U.G", "G"), col("U.G_2", "G"), col("C.ID", "ID"), col("C.W", "W")},
			false,
			"the second G is the same sequence position the RC names G_2",
		},
		{
			"a different second name",
			[]executor.ColumnDef{col("U.G", "G"), col("U.X", "X"), col("C.ID", "ID"), col("C.W", "W")},
			true,
			"X is not G_2 under any reading",
		},
		{
			"a repeated display over a slot the RC names otherwise",
			[]executor.ColumnDef{col("U.G", "G"), col("U.G", "G"), col("C.G", "G"), col("C.W", "W")},
			true,
			"the third G deduplicates to G_3 and the RC names that slot ID; skipping repeated displays would have accepted it",
		},
		{
			"reordered legs",
			[]executor.ColumnDef{col("C.ID", "ID"), col("C.W", "W"), col("U.G", "G"), col("U.G_2", "G")},
			true,
			"the merge walked the physical leg order, not FROM order",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mergedRVSequenceDiverges(rc, tc.merged); got != tc.want {
				t.Fatalf("mergedRVSequenceDiverges = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}
