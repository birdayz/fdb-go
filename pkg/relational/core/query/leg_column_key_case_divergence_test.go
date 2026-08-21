package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TWO MECHANISMS BUILD THE JOIN-LEG DATUM KEYS AND THEY DISAGREE BY CASE.
// `legColumns` here and `logicalLegFields` in logical_result_type.go both
// produce the per-leg keys the executor's row map is keyed by — `A.K`, `B.K`,
// so two legs' `K` stay distinguishable — and they spell the COLUMN half
// differently:
//
//   - logicalLegFields keeps it VERBATIM and says so at its site: "The COLUMN
//     half is verbatim; only the ALIAS half is folded, and only because a
//     source alias never comes from a descriptor."
//   - legColumns folds it to UPPER.
//
// A leg column declared `"KeepCase"` is therefore `C.KeepCase` on one route and
// `C.KEEPCASE` on the other, and a proto field named `customer_id` is
// `C.customer_id` against `C.CUSTOMER_ID`.
//
// THIS TEST PINS THE DISAGREEMENT RATHER THAN RESOLVING IT, and the measurement
// is why. Removing the fold is invisible from SQL: zero rows of the 2627-query
// plan-shape golden move, and twelve targeted shapes chosen to reach a leg key
// by different routes — a three-way join, a CTE and a derived table over a join
// star, an UNNEST beside one, a grouped and a scalar aggregate over a quoted
// mixed-case leg column, a correlated scalar subquery, and an alias-list
// recursive CTE — produce byte-identical plans either way. What it is NOT
// invisible to is this package's own contracts: TestLegColumns_NestedNoSpurious
// Keys and TestLegColumns_NamingConsistentWithAnchoredRecord both assert the
// FOLDED spelling, one of them under the word "verbatim".
//
// So the two mechanisms have two contracts, both written down, and choosing
// between them changes datum-key spelling engine-wide. That is a decision with
// its own blast radius, not a line to flip inside a naming PR — TODO.md carries
// it and points here.
//
// What this test buys until then: the disagreement cannot widen or silently
// close without a red. It asserts the VALUE on both sides, not merely that they
// differ — a test that only compared them would pass if both routes started
// folding, which is one of the two outcomes it exists to detect.
func TestLegColumnKeysDisagreeWithTheExactRowByCase(t *testing.T) {
	t.Parallel()

	leg := func(alias, colName string) *logical.LogicalInlineValues {
		t.Helper()
		rowType := &values.RecordType{Fields: []values.Field{
			{Name: colName, Ordinal: 0, FieldType: values.NotNullLong},
		}}
		row := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{
				Name:  colName,
				Value: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
			},
		)
		source, err := logical.NewInlineValues(alias,
			values.NewArrayConstructorValue(rowType, []values.Value{row}))
		if err != nil {
			t.Fatalf("NewInlineValues(%q): %v", alias, err)
		}
		return source
	}

	// `KeepCase` is the quoted-DDL spelling — the one where the two routes
	// visibly differ. `PLAIN` is the control: an unquoted name arrives already
	// folded from the parse boundary, so both routes agree on it and the
	// disagreement below is about the FOLD and not about the qualifier.
	join := logical.NewJoin(leg("L", "KeepCase"), leg("R", "PLAIN"), logical.JoinInner, "")

	got := (&cascadesTranslator{}).legColumns(join)
	if len(got) != 2 {
		t.Fatalf("legColumns returned %d fields, want 2: %+v", len(got), got)
	}
	wantLegColumns := []string{"L.KEEPCASE", "R.PLAIN"}
	for i, w := range wantLegColumns {
		if got[i].Name != w {
			t.Errorf("legColumns[%d].Name = %q, want %q. If this moved to the "+
				"verbatim spelling the divergence is CLOSED — delete this test "+
				"rather than relaxing it, and clear the TODO.md entry", i, got[i].Name, w)
		}
	}

	exact, err := ExactLogicalResultType(join, nil)
	if err != nil {
		t.Fatalf("ExactLogicalResultType: %v", err)
	}
	record, ok := exact.(*values.RecordType)
	if !ok {
		t.Fatalf("exact join type is %T, want *values.RecordType", exact)
	}
	if len(record.Fields) != len(got) {
		t.Fatalf("exact row has %d fields, legColumns has %d — the two "+
			"derivations disagree on WIDTH, which is a different and worse "+
			"problem than the case disagreement this test pins",
			len(record.Fields), len(got))
	}
	wantExact := []string{"L.KeepCase", "R.PLAIN"}
	for i, w := range wantExact {
		if record.Fields[i].Name != w {
			t.Errorf("exact row field %d = %q, want %q. If this moved to the "+
				"FOLDED spelling the two routes now agree — but on the answer "+
				"logicalLegFields' own comment argues against, so read that "+
				"comment before accepting it", i, record.Fields[i].Name, w)
		}
	}

	// The disagreement itself, stated once so its LOCATION is pinned and not
	// only its two endpoints: it is the first leg, whose column keeps a case a
	// descriptor authored.
	if got[0].Name == record.Fields[0].Name {
		t.Errorf("the two derivations now AGREE on %q — that is the outcome "+
			"this test watches for; resolve TODO.md's entry and delete it",
			got[0].Name)
	}
	if got[1].Name != record.Fields[1].Name {
		t.Errorf("the two derivations disagree on the UNQUOTED control column "+
			"(%q vs %q) — the divergence has widened past the fold",
			got[1].Name, record.Fields[1].Name)
	}
}
