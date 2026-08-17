package embedded

// Post-aggregate Values remain structural in the logical draft. The translated
// HAVING filter is the first layer that owns the aggregate output quantifier,
// and therefore the first layer allowed to mint its exact FieldValues. These
// tests inspect that real owner and its native [group keys..., calls...] layout.

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/logical"
)

func aggregateFixtureFor(t *testing.T, sql, ddl string) (logical.LogicalOperator, *logical.LogicalAggregate, *recordlayer.RecordMetaData) {
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
	return op, found, tmpl.Underlying()
}

// translatedHavingFor returns the logical aggregate together with the HAVING
// predicate after translation and the exact QOV that owns its output row.
func translatedHavingFor(t *testing.T, sql, ddl string) (*logical.LogicalAggregate, predicates.QueryPredicate, values.QuantifiedObjectValue) {
	t.Helper()
	op, aggregate, md := aggregateFixtureFor(t, sql, ddl)
	ref, _, err := query.TranslateToCascadesWithError(op, md)
	if err != nil {
		t.Fatalf("translate %q: %v", sql, err)
	}
	if ref == nil {
		t.Fatalf("translate %q returned no reference", sql)
	}

	var having *expressions.LogicalFilterExpression
	seen := map[*expressions.Reference]bool{}
	var walkRef func(*expressions.Reference)
	walkRef = func(r *expressions.Reference) {
		if r == nil || seen[r] {
			return
		}
		seen[r] = true
		for _, member := range r.Members() {
			if filter, ok := member.(*expressions.LogicalFilterExpression); ok {
				inner := filter.GetInner().GetRangesOver()
				if inner != nil {
					for _, innerMember := range inner.Members() {
						if _, isGroupBy := innerMember.(*expressions.GroupByExpression); isGroupBy {
							having = filter
						}
					}
				}
			}
			for _, quantifier := range member.GetQuantifiers() {
				walkRef(quantifier.GetRangesOver())
			}
		}
	}
	walkRef(ref)
	if having == nil {
		t.Fatalf("no translated HAVING filter over a GroupBy for %q", sql)
	}
	preds := having.GetPredicates()
	if len(preds) != 1 || preds[0] == nil {
		t.Fatalf("translated HAVING predicates = %v, want one", preds)
	}
	owner, err := having.GetInner().RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("translated HAVING owner: %v", err)
	}
	return aggregate, preds[0], owner
}

// translatedSlots collects every aggregate-output FieldValue and checks the
// part no logical draft can honestly state: the field is owned by the HAVING
// quantifier and its ordinal domain is that owner's exact output row.
func translatedSlots(t *testing.T, pred predicates.QueryPredicate, owner values.QuantifiedObjectValue) map[string]int {
	t.Helper()
	got := map[string]int{}
	var visitValue func(values.Value)
	visitValue = func(v values.Value) {
		values.WalkValue(v, func(n values.Value) bool {
			fv, ok := values.AsFieldValue(n)
			if !ok {
				return true
			}
			fieldOwner, hasOwner := values.AsQuantifiedObjectValue(fv.ChildValue())
			if !hasOwner || fieldOwner.Correlation() != owner.Correlation() ||
				!fieldOwner.FlowedType().Equals(owner.FlowedType()) {
				t.Errorf("translated HAVING field %q is owned by %v, want aggregate output owner %v",
					fv.DisplayName(), fieldOwner, owner.Correlation())
				return true
			}
			path := fv.Path()
			if path == nil || path.Len() != 1 {
				t.Errorf("translated HAVING field %q has path %v, want one exact native slot",
					fv.DisplayName(), path)
				return true
			}
			wantDomain := values.OrdinalDomainOfType(owner.FlowedType())
			if !wantDomain.IsKnown() || path.RootDomain() != wantDomain {
				t.Errorf("translated HAVING field %q domain = %v, want owner layout %v",
					fv.DisplayName(), path.RootDomain(), wantDomain)
			}
			got[fv.DisplayName()] = path.Ordinals()[0]
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

	const ddl = "CREATE TABLE t (k BIGINT, v BIGINT, w BIGINT, PRIMARY KEY (k))"

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
			_, having, owner := translatedHavingFor(t, c.sql, ddl)
			slots := translatedSlots(t, having, owner)
			got, present := slots[c.field]
			switch {
			case !present:
				t.Fatalf("%s\n  query: %s\n  no translated aggregate-output reference named %q; saw %v",
					c.name, c.sql, c.field, slots)
			case got != c.want:
				t.Errorf("%s\n  query: %s\n  the translated HAVING reference %q addresses slot %d, want %d.\n  %s\n"+
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

	const ddl = "CREATE TABLE t (k BIGINT, v BIGINT, w BIGINT, PRIMARY KEY (k))"

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
			_, having, owner := translatedHavingFor(t, c.sql, ddl)
			slots := translatedSlots(t, having, owner)
			got, present := slots[c.field]
			if !present {
				t.Fatalf("%s\n  query: %s\n  no translated aggregate-output reference named %q; saw %v",
					c.name, c.sql, c.field, slots)
			}
			if got != c.want {
				t.Errorf("%s\n  query: %s\n  the translated HAVING group-key reference %q addresses slot %v, want %d.\n"+
					"  The key's INDEX in GROUP BY order is its output slot; the translated "+
					"FieldValue must address that slot through the aggregate output owner.",
					c.name, c.sql, c.field, got, c.want)
			}
		})
	}
}
