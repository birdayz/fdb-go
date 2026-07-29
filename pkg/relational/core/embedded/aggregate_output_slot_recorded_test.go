package embedded

// The DECISION, not the answer.
//
// The FDB scenarios for this conversion
// (sqldriver/aggregate_output_slot_recorded_fdb_test.go) are byte-identical
// with and without it: measured across same-leaf operands, expression operands,
// aliases, ORDER BY and HAVING, every row is the same. That is a real result and
// it is recorded there, but it means no row assertion can detect the conversion's
// absence — the name channel happened to recover the right slot on every shape
// that could be constructed for it.
//
// What DID change is where the slot comes from, and that is the thing worth
// pinning: a post-aggregate reference to an aggregate now leaves
// `rewriteAggregateValue` carrying the ordinal its composition chose
// (`aggregateCallOutputSlot`), instead of leaving as a bare rendered name for
// `groupByOutputBaker` to look up in a map keyed by a SECOND rendering produced
// by different code from a different input (`AggregateResultColumnName` over the
// parse text, versus `canonicalAggName` over the resolved Value).
//
// So these assertions are on the reference's STRUCTURE. Revert the conversion
// and every one of them goes red on `Resolved == nil` — the reference is a bare
// name again — which is exactly the state that made the previous eight bugs of
// this class possible while the suite stayed green.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query/logical"
)

// aggregateFor builds the logical plan for sql against ddl and returns the
// LogicalAggregate it contains, or fails.
func aggregateFor(t *testing.T, sql, ddl string) *logical.LogicalAggregate {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	if sel == nil {
		t.Fatalf("not a SELECT: %q", sql)
	}
	op, err := NewPlanVisitor(tmpl.Underlying()).VisitQuery(sel.Query())
	if err != nil {
		t.Fatalf("build %q: %v", sql, err)
	}
	var found *logical.LogicalAggregate
	var walk func(logical.LogicalOperator)
	walk = func(o logical.LogicalOperator) {
		if o == nil {
			return
		}
		if a, ok := o.(*logical.LogicalAggregate); ok && found == nil {
			found = a
		}
		for _, c := range o.Children() {
			walk(c)
		}
	}
	walk(op)
	if found == nil {
		t.Fatalf("no aggregate in the logical plan for %q", sql)
	}
	return found
}

// recordedSlots collects, for every flat FieldValue in a predicate's value
// trees, its display name and the ordinal it carries — reporting -1 for a
// reference that carries no recorded ordinal at all (a bare name), which is the
// pre-conversion shape.
func recordedSlots(t *testing.T, pred predicates.QueryPredicate) map[string]int {
	t.Helper()
	got := map[string]int{}
	var visitValue func(values.Value)
	visitValue = func(v values.Value) {
		values.WalkValue(v, func(n values.Value) bool {
			fv, ok := n.(*values.FieldValue)
			if !ok {
				return true
			}
			slot := -1
			if fv.Resolved != nil && len(fv.Resolved.Accessors) == 1 {
				slot = fv.Resolved.Accessors[0].Ordinal
				if !fv.Resolved.FrontierPinned {
					// An UNPINNED ordinal is source-relative and dead on the
					// aggregate's output row; recording it as such keeps the
					// two bake kinds distinguishable in the assertion.
					slot = -2 - slot
				}
			}
			got[fv.Field] = slot
			return true
		})
	}
	var visit func(predicates.QueryPredicate)
	visit = func(p predicates.QueryPredicate) {
		switch x := p.(type) {
		case *predicates.ComparisonPredicate:
			visitValue(x.Operand)
			visitValue(x.Comparison.Operand)
		case *predicates.AndPredicate:
			for _, s := range x.SubPredicates {
				visit(s)
			}
		case *predicates.OrPredicate:
			for _, s := range x.SubPredicates {
				visit(s)
			}
		case *predicates.NotPredicate:
			visit(x.Child)
		}
	}
	visit(pred)
	return got
}

func TestHavingAggregateReferenceCarriesTheSlotItsCompositionChose(t *testing.T) {
	t.Parallel()

	const ddl = "CREATE TABLE t (k BIGINT NOT NULL, v BIGINT, w BIGINT, PRIMARY KEY (k))"

	cases := []struct {
		name  string
		sql   string
		field string
		want  int
		why   string
	}{
		{
			name:  "first_aggregate",
			sql:   "SELECT k, SUM(v), SUM(w) FROM t GROUP BY k HAVING SUM(v) > 1",
			field: "SUM(V)", want: 1,
			why: "One group key then the calls in order: SUM(v) is call 0, so slot 1. " +
				"Recovered by name this is aggOrds[\"SUM(V)\"], a map whose keys are a " +
				"different rendering of a different input.",
		},
		{
			name:  "second_aggregate",
			sql:   "SELECT k, SUM(v), SUM(w) FROM t GROUP BY k HAVING SUM(w) > 5",
			field: "SUM(W)", want: 2,
			why: "The second call. Asserting only the first would pass for a binder that " +
				"always answers with the lowest aggregate slot.",
		},
		{
			name:  "expression_nested_reference",
			sql:   "SELECT k, SUM(v), SUM(w) FROM t GROUP BY k HAVING SUM(w) + 0 > 5",
			field: "SUM(W)", want: 2,
			why: "Nested one level down in an ArithmeticValue — the shape the ninth bug of " +
				"this class required, because a top-level comparison has a second, " +
				"name-based recovery above it that hides a wrong slot.",
		},
		{
			name:  "count_star_has_no_operand_to_render",
			sql:   "SELECT k, SUM(v), COUNT(*) FROM t GROUP BY k HAVING COUNT(*) > 1",
			field: "COUNT(*)", want: 2,
			why: "COUNT(*) binds by its star-ness, not by an operand; its slot must still be " +
				"the recorded one and not the first star it finds.",
		},
		{
			name:  "min_and_max_over_one_column",
			sql:   "SELECT k, MIN(v), MAX(v) FROM t GROUP BY k HAVING MAX(v) > 1",
			field: "MAX(V)", want: 2,
			why: "Two aggregates whose OPERAND is identical: only the function separates them.",
		},
		{
			name:  "no_group_key_shifts_every_slot",
			sql:   "SELECT SUM(v), SUM(w) FROM t HAVING SUM(w) > 5",
			field: "SUM(W)", want: 1,
			why: "Without a group key the calls start at slot 0, so this catches an offset " +
				"hard-coded past a key that is not there.",
		},
		{
			name:  "two_group_keys_shift_every_slot",
			sql:   "SELECT k, v, SUM(w) FROM t GROUP BY k, v HAVING SUM(w) > 5",
			field: "SUM(W)", want: 2,
			why: "Two keys push the single call to slot 2 — the offset is len(GroupKeys), " +
				"not a constant.",
		},
		{
			// V is the table's SECOND column, so a group-key reference to it
			// carries source-relative ordinal 1; the aggregate output row is
			// [V, SUM(W)] so SUM(w) records ordinal 1 too. The two 1s index
			// DIFFERENT layouts, and the group-key rebase runs over a tree where
			// the aggregate has already become a FieldValue — so the only thing
			// keeping it from matching SUM(w) as if it were the key V is that a
			// recorded slot is not offered to the matcher at all.
			name:  "an_output_slot_colliding_with_a_source_ordinal",
			sql:   "SELECT v, SUM(w) FROM t GROUP BY v HAVING v > SUM(w)",
			field: "SUM(W)", want: 1,
			why: "Two ordinals from two layouts collide at 1. Matched across them, the " +
				"aggregate reference is rewritten into a second reference to the group key, " +
				"the predicate then looks key-only to PushFilterThroughGroupByRule, and " +
				"`HAVING v > SUM(w)` is evaluated on raw scan rows where SUM(w) does not " +
				"exist. Measured, not hypothesised.",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			agg := aggregateFor(t, c.sql, ddl)
			if agg.HavingPredicate == nil {
				t.Fatalf("%s: no HAVING predicate was built", c.sql)
			}
			slots := recordedSlots(t, agg.HavingPredicate)
			got, present := slots[c.field]
			switch {
			case !present:
				t.Fatalf("%s\n  query: %s\n  no reference named %q in the HAVING predicate; saw %v",
					c.name, c.sql, c.field, slots)
			case got == -1:
				t.Errorf("%s\n  query: %s\n  the HAVING reference %q carries NO recorded ordinal.\n"+
					"  %s\n"+
					"  It left rewriteAggregateValue as a bare rendered name, so the slot has to be "+
					"recovered downstream from groupByOutputBaker's output-name map — a second "+
					"rendering, produced by different code from a different input, that is "+
					"last-wins on collision (RFC-197 item 5: aggregateCallOutputSlot).",
					c.name, c.sql, c.field, c.why)
			case got < -1:
				t.Errorf("%s\n  query: %s\n  the HAVING reference %q carries an UNPINNED ordinal %d.\n"+
					"  An unpinned ordinal is SOURCE-relative and dead on the aggregate's output "+
					"row; the binder will re-key it by name. The slot is final against the "+
					"executor's assembled row and must say so.",
					c.name, c.sql, c.field, -2-got)
			case got != c.want:
				t.Errorf("%s\n  query: %s\n  the HAVING reference %q records slot %d, want %d.\n  %s\n"+
					"  The output row is [group keys in GROUP BY order..., calls in call order...] "+
					"(GroupByOutputColumnNames).",
					c.name, c.sql, c.field, got, c.want, c.why)
			}
		})
	}
}

// TestHavingGroupKeyReferenceCarriesTheSlotItsCompositionChose is the group-key
// half of the same contract, and it is a SEPARATE direction: the aggregate arm
// and the key arm of the binder are matched by different code, so a conversion
// that satisfies one and not the other must be visible.
func TestHavingGroupKeyReferenceCarriesTheSlotItsCompositionChose(t *testing.T) {
	t.Parallel()

	const ddl = "CREATE TABLE t (k BIGINT NOT NULL, v BIGINT, w BIGINT, PRIMARY KEY (k))"

	cases := []struct {
		name  string
		sql   string
		field string
		want  int
	}{
		{
			name:  "first_of_two_keys",
			sql:   "SELECT k, v, SUM(w) FROM t GROUP BY k, v HAVING k + SUM(w) > 5",
			field: "K", want: 0,
		},
		{
			name:  "second_of_two_keys",
			sql:   "SELECT k, v, SUM(w) FROM t GROUP BY k, v HAVING v + SUM(w) > 5",
			field: "V", want: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			agg := aggregateFor(t, c.sql, ddl)
			if agg.HavingPredicate == nil {
				t.Fatalf("%s: no HAVING predicate was built", c.sql)
			}
			slots := recordedSlots(t, agg.HavingPredicate)
			got, present := slots[c.field]
			if !present {
				t.Fatalf("%s\n  query: %s\n  no reference named %q in the HAVING predicate; saw %v",
					c.name, c.sql, c.field, slots)
			}
			if got != c.want {
				t.Errorf("%s\n  query: %s\n  the HAVING group-key reference %q records slot %v, want %d.\n"+
					"  A negative value means no recorded ordinal (-1) or an UNPINNED, "+
					"source-relative one (-2-ordinal). The key's INDEX in GROUP BY order is its "+
					"output slot; recovering it from the rendered output name instead is what "+
					"bound two same-leaf keys to one slot (RFC-197 item 5).",
					c.name, c.sql, c.field, got, c.want)
			}
		})
	}
}
