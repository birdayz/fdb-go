package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

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
