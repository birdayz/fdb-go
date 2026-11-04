package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestImplementSimpleSelectTextAccessQuantifiers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		kind        expressions.QuantifierKind
		nullOnEmpty bool
	}{
		{name: "for each", kind: expressions.QuantifierForEach},
		{name: "existential", kind: expressions.QuantifierExistential},
		{name: "null on empty", kind: expressions.QuantifierForEach, nullOnEmpty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := values.NewRecordType("T", false, []values.Field{{Name: "BODY", Ordinal: 0, FieldType: values.NullableString}})
			scan, err := plans.NewRecordQueryScanPlan([]string{"T"}, row, false)
			if err != nil {
				t.Fatal(err)
			}
			inner := expressions.InitialOf(scan)
			pm := NewPlanPropertiesMap()
			pm.Add(scan)
			inner.SetPlanProperties(pm)
			q := expressions.ForEachQuantifier(inner)
			if tc.kind == expressions.QuantifierExistential {
				q = expressions.ExistentialQuantifier(inner)
			} else if tc.nullOnEmpty {
				q = expressions.ForEachNullOnEmptyQuantifier(inner)
			}
			object, err := q.RequireFlowedObjectValue()
			if err != nil {
				t.Fatal(err)
			}
			body, err := values.ResolveFieldOrdinals(object, []int{0})
			if err != nil {
				t.Fatal(err)
			}
			predicate := predicates.NewComparisonPredicate(body, predicates.Comparison{
				Type: predicates.ComparisonTextContainsAll, Operand: values.LiteralValue("hello"),
			})
			selectExpr, err := expressions.NewSelectExpression(object, []expressions.Quantifier{q}, []predicates.QueryPredicate{predicate})
			if err != nil {
				t.Fatal(err)
			}
			ctx := NewPlanContextFromMatchCandidates([]MatchCandidate{&TextIndexMatchCandidate{
				Name: "text", Columns: []string{"BODY"}, RecordTypes: []string{"T"}, TokenizerName: "default", RowType: row,
			}})
			results, err := FireImplementationRuleWithContext(NewImplementSimpleSelectRule(), expressions.InitialOf(selectExpr), ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) == 0 {
				t.Fatal("rule yielded no implementations")
			}
			wantText := tc.kind == expressions.QuantifierForEach && !tc.nullOnEmpty
			foundText := false
			for _, result := range results {
				plan, ok := result.(plans.RecordQueryPlan)
				if !ok {
					t.Fatalf("implementation is not a physical plan: %T", result)
				}
				var visit func(plans.RecordQueryPlan)
				visit = func(node plans.RecordQueryPlan) {
					if _, ok := node.(*plans.RecordQueryTextIndexPlan); ok {
						foundText = true
						if !wantText {
							t.Fatalf("text access bypassed the quantifier wrapper: %s", plan.Explain())
						}
					}
					for _, child := range node.GetChildren() {
						visit(child)
					}
				}
				visit(plan)
				if wantText {
					continue
				}
				filter, ok := plan.(*plans.RecordQueryPredicatesFilterPlan)
				if !ok {
					t.Fatalf("expected residual text filter, got %T", plan)
				}
				wrapped := filter.GetInner()
				if tc.kind == expressions.QuantifierExistential {
					if _, ok := wrapped.(*plans.RecordQueryFirstOrDefaultPlan); !ok {
						t.Fatalf("expected FirstOrDefault under filter, got %T", wrapped)
					}
				} else if _, ok := wrapped.(*plans.RecordQueryDefaultOnEmptyPlan); !ok {
					t.Fatalf("expected DefaultOnEmpty under filter, got %T", wrapped)
				}
				children := wrapped.GetChildren()
				if len(children) != 1 || children[0] != scan {
					t.Fatal("quantifier wrapper did not retain the base scan")
				}
			}
			if wantText && !foundText {
				t.Fatal("for-each positive control did not offer text access")
			}
		})
	}
}

func TestTextCandidateEligibility(t *testing.T) {
	t.Parallel()
	row := values.NewRecordType("T", false, []values.Field{{Name: "TENANT", Ordinal: 0, FieldType: values.NullableLong}, {Name: "BODY", Ordinal: 1, FieldType: values.NullableString}})
	alias := values.UniqueCorrelationIdentifier()
	object, err := values.NewQuantifiedObjectValue(alias, row)
	if err != nil {
		t.Fatal(err)
	}
	body, err := values.ResolveFieldOrdinals(object, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := values.ResolveFieldOrdinals(object, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	scan, err := plans.NewRecordQueryScanPlan([]string{"T"}, row, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                string
		typ                 predicates.ComparisonType
		omit, group, bind   bool
		tokenizer           string
		version, maxVersion int
		strictPrefix        bool
		want                bool
	}{
		{name: "all", typ: predicates.ComparisonTextContainsAll, want: true},
		{name: "phrase", typ: predicates.ComparisonTextContainsPhrase, want: true},
		{name: "omit all", typ: predicates.ComparisonTextContainsAll, omit: true, want: true},
		{name: "omit any", typ: predicates.ComparisonTextContainsAny, omit: true, want: true},
		{name: "omit prefix", typ: predicates.ComparisonTextContainsPrefix, omit: true, want: true},
		{name: "omit phrase", typ: predicates.ComparisonTextContainsPhrase, omit: true},
		{name: "omit within", typ: predicates.ComparisonTextContainsAllWithin, omit: true},
		{name: "omit all prefixes", typ: predicates.ComparisonTextContainsAllPrefixes, omit: true},
		{name: "omit any prefix", typ: predicates.ComparisonTextContainsAnyPrefix, omit: true},
		{name: "group bound", typ: predicates.ComparisonTextContainsAll, group: true, bind: true, want: true},
		{name: "group missing", typ: predicates.ComparisonTextContainsAll, group: true},
		{name: "explicit default", typ: predicates.ComparisonTextContainsAll, tokenizer: "default", want: true},
		{name: "tokenizer mismatch", typ: predicates.ComparisonTextContainsAll, tokenizer: "other"},
		{name: "named tokenizer older version", typ: predicates.ComparisonTextContainsAll, tokenizer: "default", maxVersion: 1},
		{name: "named tokenizer latest version with historical postings", typ: predicates.ComparisonTextContainsAll, tokenizer: "default", version: 1, maxVersion: 1},
		{name: "any tokenizer older version", typ: predicates.ComparisonTextContainsAll, maxVersion: 1, want: true},
		{name: "strict all prefixes", typ: predicates.ComparisonTextContainsAllPrefixes, strictPrefix: true, want: true},
		{name: "all prefixes", typ: predicates.ComparisonTextContainsAllPrefixes, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &TextIndexMatchCandidate{Name: "text", Columns: []string{"BODY"}, RecordTypes: []string{"T"}, TokenizerName: "default", OmitPositions: tc.omit, RowType: row, TokenizerVersion: tc.version, TokenizerMaxVersion: tc.maxVersion}
			if tc.group {
				c.Columns = []string{"TENANT", "BODY"}
				c.GroupingCount = 1
			}
			context := NewPlanContextFromMatchCandidates([]MatchCandidate{c})
			preds := []predicates.QueryPredicate{predicates.NewComparisonPredicate(body, predicates.Comparison{Type: tc.typ, Operand: values.LiteralValue("hello"), TextTokenizerName: tc.tokenizer, TextStrictPrefix: tc.strictPrefix})}
			if tc.bind {
				preds = append(preds, predicates.NewComparisonPredicate(tenant, predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))))
			}
			alternatives, residuals := textAccessAlternatives(context, scan, preds, alias)
			if (len(alternatives) > 0) != tc.want {
				t.Fatalf("alternatives=%d want eligible=%v", len(alternatives), tc.want)
			}
			if tc.want {
				// Only a strict probable intersection keeps its text predicate.
				kept := len(residuals[0]) == 1
				if kept {
					cp, ok := residuals[0][0].(*predicates.ComparisonPredicate)
					kept = ok && cp.Comparison.Type == predicates.ComparisonTextContainsAllPrefixes
				}
				if kept != tc.strictPrefix || !tc.strictPrefix && len(residuals[0]) != 0 {
					t.Fatalf("residual predicates=%v", residuals[0])
				}
				if !computeStoredRecord(alternatives[0]) {
					t.Fatal("text access does not publish stored records")
				}
			}
			wrong, _ := textAccessAlternatives(context, scan, preds, values.UniqueCorrelationIdentifier())
			if len(wrong) != 0 {
				t.Fatal("matched a field from another input alias")
			}
		})
	}
}
