package query

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestOwnedExistsInputRetainsProducerAndScalarEdges(t *testing.T) {
	t.Parallel()
	scalar := logical.ScalarSubquery{
		Alias: values.NamedCorrelationIdentifier("SCALAR"),
		Plan:  logical.NewScan("Order", "S"),
	}
	plan := &logical.LogicalFilter{
		Input: logical.NewScan("Order", "I"), ScalarSubqueries: []logical.ScalarSubquery{scalar},
	}
	input, err := LowerExistsInput(plan, demoMetaData(t))
	if err != nil {
		t.Fatal(err)
	}
	if !input.ResultType().Equals(input.Reference().Get().GetResultValue().Type()) {
		t.Fatal("exact type does not come from the owned producer")
	}
	// No metadata or logical plan is available at consumption: the query owner
	// has already constructed the child. Retyping/retranslating cannot pass.
	translator := &cascadesTranslator{}
	ref := translator.existsInputRef(logical.ExistsSubquery{Input: input})
	if ref != input.Reference() {
		t.Fatal("existential consumer rebuilt its producer")
	}
	if len(translator.scalarSubqueries) != 1 || translator.scalarSubqueries[0].Alias != scalar.Alias || translator.scalarSubqueries[0].Plan != scalar.Plan {
		t.Fatalf("owned scalar registration lost or replaced: %+v", translator.scalarSubqueries)
	}
	returned := input.Scalars()
	returned[0].Plan = nil
	if input.Scalars()[0].Plan != scalar.Plan {
		t.Fatal("caller mutated the owned scalar registration slice")
	}
	badConsumer := &cascadesTranslator{}
	ref = badConsumer.existsInputRef(logical.ExistsSubquery{Input: input, FlowedType: values.NotNullLong})
	var apiErr *api.Error
	if ref != nil || !errors.As(badConsumer.translateErr, &apiErr) || apiErr.Code != api.ErrCodeInternalError || len(badConsumer.scalarSubqueries) != 0 {
		t.Fatalf("mismatched owned type was consumed: ref=%v err=%v scalars=%v", ref, badConsumer.translateErr, badConsumer.scalarSubqueries)
	}
}

func TestOwnedExistsInputPredicateRebaseRetainsChildrenAndRow(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"filter", "select"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Each parallel case owns its memo References. Reference properties
			// are lazily cached by the single-threaded planner, not shared across
			// independent concurrent query constructions.
			leaf, err := expressions.NewLogicalValuesExpression([]values.Value{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}})
			if err != nil {
				t.Fatal(err)
			}
			child := expressions.InitialOf(leaf)
			quantifier := expressions.NamedForEachQuantifier(values.NamedCorrelationIdentifier("I"), child)
			outer := values.NamedCorrelationIdentifier("OUTER")
			box := values.NamedCorrelationIdentifier("BOX")
			before := predicates.NewComparisonPredicate(exactTestNamedField(t, "OUTER", "ID", values.NotNullLong), predicates.Comparison{
				Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong},
			})
			after := predicates.NewComparisonPredicate(exactTestNamedField(t, "BOX", "ID", values.NotNullLong), predicates.Comparison{
				Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong},
			})
			filter, err := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{before}, quantifier)
			if err != nil {
				t.Fatal(err)
			}
			selectNode, err := expressions.NewSelectExpressionWithAliases(filter.GetResultValue(), []expressions.Quantifier{quantifier}, []predicates.QueryPredicate{before}, []string{"I"})
			if err != nil {
				t.Fatal(err)
			}
			var node expressions.RelationalExpression = filter
			if name == "select" {
				node = selectNode
			}
			scalar := logical.ScalarSubquery{Alias: values.NamedCorrelationIdentifier("S"), Plan: logical.NewScan("T", "S")}
			input, err := logical.NewExistsInput(expressions.InitialOf(node), []logical.ScalarSubquery{scalar})
			if err != nil {
				t.Fatal(err)
			}
			edge := logical.ExistsSubquery{Alias: values.NamedCorrelationIdentifier("E"), Input: input, FlowedType: input.ResultType()}
			calls := 0
			rebased, ok := rebaseExistsInputPredicates(edge, func(p predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
				calls++
				if !predicates.PredicateEquals(p, before) {
					t.Fatalf("unexpected predicate: %v", p)
				}
				return after, true
			})
			if !ok || calls != 1 || rebased.Input == input || rebased.Alias != edge.Alias {
				t.Fatalf("failed owned rebase: ok=%v calls=%d", ok, calls)
			}
			if rebased.Input.Reference().Get().GetQuantifiers()[0].GetRangesOver() != child {
				t.Fatal("predicate rebase rebuilt the FROM child")
			}
			if !rebased.Input.ResultType().Equals(input.ResultType()) || !rebased.FlowedType.Equals(edge.FlowedType) {
				t.Fatal("predicate rebase changed the exact flowed row")
			}
			for ref, want := range map[*expressions.Reference]values.CorrelationIdentifier{input.Reference(): outer, rebased.Input.Reference(): box} {
				free := ref.GetCorrelatedTo()
				if _, ok := free[want]; !ok || len(free) != 1 {
					t.Fatalf("free set=%v, want only %s", free, want.Name())
				}
			}
			if got := rebased.Input.Scalars(); len(got) != 1 || got[0].Alias != scalar.Alias || got[0].Plan != scalar.Plan {
				t.Fatal("predicate rebase dropped scalar ownership")
			}
			declined, ok := rebaseExistsInputPredicates(edge, func(predicates.QueryPredicate) (predicates.QueryPredicate, bool) { return nil, false })
			if ok || declined.Input != input {
				t.Fatal("failed rebase changed the owned input")
			}
		})
	}
}

func TestOwnedExistsInputRequiresExplicitConsumerAdmission(t *testing.T) {
	t.Parallel()
	plan := logical.NewScan("Order", "I")
	input, err := LowerExistsInput(plan, demoMetaData(t))
	if err != nil {
		t.Fatal(err)
	}
	edge := logical.ExistsSubquery{Alias: values.NamedCorrelationIdentifier("E"), Plan: plan, Input: input, FlowedType: input.ResultType(), Constraint: logical.ExistsPositivePredicateOnly}
	translator := &cascadesTranslator{}
	if translator.existsInputRef(edge) != nil || translator.translateErr == nil {
		t.Fatal("programmatic edge bypassed explicit consumer admission")
	}
	for _, consumer := range []logical.ExistsConsumer{0, logical.ExistsNegativePredicate, logical.ExistsProjectedValue} {
		if _, err := edge.AdmitConsumer(consumer); err == nil {
			t.Fatalf("unsupported consumer %v was admitted", consumer)
		}
	}
	admitted, err := edge.AdmitConsumer(logical.ExistsPositivePredicate)
	if err != nil {
		t.Fatal(err)
	}
	translator = &cascadesTranslator{}
	if translator.existsInputRef(admitted) != input.Reference() || translator.translateErr != nil {
		t.Fatalf("admitted edge changed the owned input: %v", translator.translateErr)
	}
	if edge.ValidateAdmission() == nil {
		t.Fatal("admission mutated the original edge")
	}
}

func TestOwnedExistsProductKeepsChildReference(t *testing.T) {
	t.Parallel()
	md := demoMetaData(t)
	scalar := logical.ScalarSubquery{Alias: values.NamedCorrelationIdentifier("SCALAR"), Plan: logical.NewScan("Order", "S")}
	right := &logical.LogicalFilter{Input: logical.NewScan("Order", "I"), ScalarSubqueries: []logical.ScalarSubquery{scalar}}
	input, err := LowerExistsInput(right, md)
	if err != nil {
		t.Fatal(err)
	}
	edge := logical.ExistsSubquery{Alias: values.NamedCorrelationIdentifier("E"), Plan: right, Input: input, FlowedType: input.ResultType()}
	product := &logical.LogicalJoin{Left: logical.NewScan("Order", "M"), Right: right, Kind: logical.JoinInner}
	composed, err := LowerExistsInput(product, md, edge)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[*expressions.Reference]bool)
	var walk func(*expressions.Reference)
	walk = func(ref *expressions.Reference) {
		if seen[ref] {
			return
		}
		seen[ref] = true
		for _, member := range ref.Members() {
			for _, quantifier := range member.GetQuantifiers() {
				walk(quantifier.GetRangesOver())
			}
		}
	}
	walk(composed.Reference())
	if !seen[input.Reference()] {
		t.Fatal("existential product rebuilt its retained child producer")
	}
	if len(composed.Scalars()) != 1 || composed.Scalars()[0].Alias != scalar.Alias || composed.Scalars()[0].Plan != scalar.Plan {
		t.Fatalf("product scalar ownership = %+v, want one retained scalar", composed.Scalars())
	}
	if input.Reference().Get() == nil || !input.Reference().Get().GetResultValue().Type().Equals(input.ResultType()) {
		t.Fatal("composition mutated the child's exact row")
	}
}
