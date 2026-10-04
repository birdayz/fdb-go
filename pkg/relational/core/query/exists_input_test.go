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
			outerType := &values.RecordType{Fields: []values.Field{{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong}}}
			baked, err := values.ResolveOrdinalSeedAccess(exactTestQOV(t, "BOX", outerType), 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			after := predicates.NewComparisonPredicate(baked, before.Comparison)
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
			var expectedBefore, expectedAfter predicates.QueryPredicate = before, after
			if name == "select" {
				builder := predicates.NewRangeConstraintsBuilder()
				if !builder.AddComparisonMaybe(before.Comparison) {
					t.Fatal("equality cannot form a range")
				}
				ranges := []*predicates.RangeConstraints{builder.Build()}
				expectedBefore = predicates.NewPredicateWithValueAndRanges(before.Operand, ranges)
				expectedAfter = predicates.NewPredicateWithValueAndRanges(after.Operand, ranges)
			}
			calls := 0
			rebased, ok := rebaseExistsInputPredicates(edge, func(p predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
				calls++
				if !predicates.PredicateEquals(p, expectedBefore) {
					t.Fatalf("unexpected predicate: %v", p)
				}
				out, ok := rebaseUnnestOuterLegPredicateOrdinal(p, outerType, outerType, map[string]struct{}{"OUTER": {}}, box)
				if !ok || !predicates.PredicateEquals(out, expectedAfter) {
					t.Fatalf("owned predicate was not rebased: ok=%v predicate=%v", ok, out)
				}
				return out, ok
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

func TestExistentialAttachmentKeepsCorrelationInsideInput(t *testing.T) {
	t.Parallel()
	for _, owned := range []bool{false, true} {
		name := "programmatic"
		if owned {
			name = "owned"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			md := demoMetaData(t)
			plan := logical.NewScan("Order", "I")
			input, err := LowerExistsInput(plan, md)
			if err != nil {
				t.Fatal(err)
			}
			existential := values.UniqueCorrelationIdentifier()
			join := predicates.NewComparisonPredicate(exactDemoRef(t, "I", "order_id"), predicates.Comparison{
				Type: predicates.ComparisonEquals, Operand: exactDemoRef(t, "O", "order_id"),
			})
			edge := logical.ExistsSubquery{Alias: existential, Plan: plan, JoinPredicate: join}
			if owned {
				edge.Input, edge.FlowedType = input, input.ResultType()
			}
			marker, err := predicates.NewExistentialAlias(existential, input.ResultType())
			if err != nil {
				t.Fatal(err)
			}
			ref, _, err := TranslateToCascadesWithError(&logical.LogicalFilter{
				Input: logical.NewScan("Order", "O"), Predicate: marker, ExistsSubqueries: []logical.ExistsSubquery{edge},
			}, md)
			if err != nil || ref == nil {
				t.Fatalf("translation: ref=%v err=%v", ref, err)
			}
			ref = queryBody(t, ref)
			sel, ok := ref.Get().(*expressions.SelectExpression)
			if !ok || len(sel.GetPredicates()) != 1 {
				t.Fatalf("correlation escaped the existential input: %T %v", ref.Get(), ref.Get())
			}
			inner := sel.GetQuantifiers()[1].GetRangesOver()
			filter, ok := inner.Get().(*expressions.LogicalFilterExpression)
			if !ok || len(filter.GetPredicates()) != 1 {
				t.Fatalf("existential input=%T, want the correlation filter", inner.Get())
			}
			free := inner.GetCorrelatedTo()
			if _, hasOuter := free[values.NamedCorrelationIdentifier("O")]; !hasOuter || len(free) != 1 {
				t.Fatalf("existential input correlations=%v, want only O", free)
			}
			if owned && filter.GetInner().GetRangesOver() != input.Reference() {
				t.Fatal("correlation attachment rebuilt the owned child")
			}
			if !filter.GetResultValue().Type().Equals(input.ResultType()) {
				t.Fatal("correlation attachment changed the exact input row")
			}
			if _, unchanged := predicates.GetCorrelatedToOfPredicate(join)[values.NamedCorrelationIdentifier("I")]; !unchanged {
				t.Fatal("attachment mutated the original predicate")
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

// A WHERE with a nested scalar lowers to stacked filters, and the outer-only
// conjunct can sit in the lower one. Every bound filter of the chain is
// rebased onto the box row; the FROM child is kept.
func TestRebaseExistsInputPredicatesReachesStackedFilters(t *testing.T) {
	t.Parallel()
	leaf, err := expressions.NewLogicalValuesExpression([]values.Value{&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}})
	if err != nil {
		t.Fatal(err)
	}
	child := expressions.InitialOf(leaf)
	inner := values.NamedCorrelationIdentifier("I")
	box := values.NamedCorrelationIdentifier("BOX")
	outerOnly := predicates.NewComparisonPredicate(exactTestNamedField(t, "OUTER", "ID", values.NotNullLong), predicates.Comparison{
		Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong},
	})
	lower, err := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{outerOnly},
		expressions.NamedForEachQuantifier(inner, child))
	if err != nil {
		t.Fatal(err)
	}
	local := predicates.NewConstantPredicate(predicates.TriTrue)
	upper, err := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{local},
		expressions.NamedForEachQuantifier(inner, expressions.InitialOf(lower)))
	if err != nil {
		t.Fatal(err)
	}
	input, err := logical.NewExistsInput(expressions.InitialOf(upper), nil)
	if err != nil {
		t.Fatal(err)
	}
	edge := logical.ExistsSubquery{Alias: values.NamedCorrelationIdentifier("E"), Input: input, FlowedType: input.ResultType()}
	outerType := &values.RecordType{Fields: []values.Field{{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong}}}
	rebased, ok := rebaseExistsInputPredicates(edge, func(p predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
		if predicates.PredicateEquals(p, local) {
			return p, true
		}
		return rebaseUnnestOuterLegPredicateOrdinal(p, outerType, outerType, map[string]struct{}{"OUTER": {}}, box)
	})
	if !ok || rebased.Input == input {
		t.Fatalf("stacked filter chain was not rebased: ok=%v", ok)
	}
	free := rebased.Input.Reference().GetCorrelatedTo()
	if _, stillOuter := free[values.NamedCorrelationIdentifier("OUTER")]; stillOuter {
		t.Fatalf("lower filter kept its OUTER read: free set=%v", free)
	}
	if _, onBox := free[box]; !onBox {
		t.Fatalf("lower filter does not read the box row: free set=%v", free)
	}
	lowerRef := rebased.Input.Reference().Get().GetQuantifiers()[0].GetRangesOver()
	if lowerRef.Get().GetQuantifiers()[0].GetRangesOver() != child {
		t.Fatal("predicate rebase rebuilt the FROM child")
	}
	if !rebased.Input.ResultType().Equals(input.ResultType()) {
		t.Fatal("predicate rebase changed the exact flowed row")
	}
}
