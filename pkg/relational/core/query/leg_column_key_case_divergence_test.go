package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// TWO MECHANISMS BUILD THE JOIN-LEG DATUM KEYS AND THEY DISAGREE BY CASE.
// `legColumns` here and `logicalLegFields` in logical_result_type.go both
// produce the per-leg keys `rowSlotForLegColumn` resolves against at runtime —
// `A.K`, `B.K`, so two legs' `K` stay distinguishable — and they spell the
// COLUMN half differently:
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
// THIS TEST PINS THE DISAGREEMENT RATHER THAN RESOLVING IT, and the reason is
// that the folding line does TWO JOBS. tableColumns names a scan's columns
// through ToUserIdentifier, which un-escapes and does NOT fold, so for a
// hand-authored proto field `order_id` the fold in legColumns is
// descriptor-to-SQL NORMALIZATION. For a DDL-declared `"KeepCase"`, which
// arrives already canonical from the parse boundary, the same line is
// RE-normalization — the thing RFC-237 exists to delete. Removing it therefore
// breaks a real test AND keeping it breaks a real invariant, which means
// neither spelling is the answer and "pick one" is the wrong question.
//
// The answer is to collapse the two producers: legColumns' join arm defers to
// logicalLegFields, and the descriptor-name decision moves to that single
// boundary. Whichever spelling wins, two producers of one datum key will drift
// again. TODO.md carries that plan; this test watches the gap until then.
//
// The measurement that sized it: removing the fold moves ZERO rows of the
// 2627-query plan-shape golden, and twelve targeted shapes chosen to reach a leg
// key by different routes (a three-way join, a CTE and a derived table over a
// join star, an UNNEST beside one, a grouped and a scalar aggregate over a
// quoted mixed-case leg column, a correlated scalar subquery, an alias-list
// recursive CTE) plan byte-identically either way. What it is not invisible to
// is TestLegColumns_NestedNoSpuriousKeys, whose `order_id` is the normalization
// job above.
//
// WHAT THAT ZERO DOES AND DOES NOT ESTABLISH, because two wrong readings of it
// have already been written down here.
//
// It is a PLANNING measurement. The plan-shape golden is generated through
// `embedded.PlanPhysicalForTest` and never runs the executor, so a zero there
// says nothing about runtime behaviour in either direction.
//
// It was then read as MASKING — that `rowSlotForLegColumn` compares both halves
// of a leg key with `strings.EqualFold` and so cannot tell the two producers
// apart. That is refuted by a standing gate rather than by argument:
// `AssertLegColumnProvenanceCensus` fails the build if that reader receives ANY
// call, because the exact-ordinal seed retired its only driver. The comparators
// are in code that does not run, so they mask nothing.
//
// What the zero actually establishes is narrow and worth stating exactly: no
// PLANNED shape in the corpus, and none of the twelve probed by hand, carries a
// leg key whose case a planner decision depends on. The consumers that would
// care are either retired or downstream of planning. That is why the divergence
// is a latent producer disagreement rather than a live defect — and why the
// fix is to collapse the producers, not to chase a symptom that has none.
//
// TestLegColumns_NamingConsistentWithAnchoredRecord also reddens and is NOT
// evidence for either side: it builds its expectation by applying
// `strings.ToUpper(c.Name)` itself, so it mirrors the implementation, reddens
// for any change, and asserts nothing about which spelling is right.
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
