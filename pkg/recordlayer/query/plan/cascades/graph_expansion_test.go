package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestEmptyGraphExpansion(t *testing.T) {
	t.Parallel()
	ge := EmptyGraphExpansion()
	if len(ge.GetResultColumns()) != 0 {
		t.Fatalf("expected 0 columns, got %d", len(ge.GetResultColumns()))
	}
	if len(ge.GetPredicates()) != 0 {
		t.Fatalf("expected 0 predicates, got %d", len(ge.GetPredicates()))
	}
	if len(ge.GetQuantifiers()) != 0 {
		t.Fatalf("expected 0 quantifiers, got %d", len(ge.GetQuantifiers()))
	}
	if len(ge.GetPlaceholders()) != 0 {
		t.Fatalf("expected 0 placeholders, got %d", len(ge.GetPlaceholders()))
	}
}

func TestBuilderAddColumnsPredicatesQuantifiersPlaceholders(t *testing.T) {
	t.Parallel()

	fv := &values.FieldValue{Field: "A", Typ: values.TypeInt}
	alias := values.UniqueCorrelationIdentifier()
	ph := predicates.NewPlaceholder(alias, fv)

	ref := expressions.InitialOf(&expressions.FullUnorderedScanExpression{})
	q := expressions.ForEachQuantifier(ref)

	pred := predicates.NewConstantPredicate(predicates.TriTrue)

	ge := NewGraphExpansionBuilder().
		AddColumn("A", fv).
		AddPredicate(pred).
		AddQuantifier(q).
		AddPlaceholder(ph).
		Build()

	if len(ge.GetResultColumns()) != 1 {
		t.Fatalf("expected 1 column, got %d", len(ge.GetResultColumns()))
	}
	if ge.GetResultColumns()[0].Name != "A" {
		t.Fatalf("expected column name A, got %q", ge.GetResultColumns()[0].Name)
	}
	if len(ge.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(ge.GetPredicates()))
	}
	if len(ge.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(ge.GetQuantifiers()))
	}
	if len(ge.GetPlaceholders()) != 1 {
		t.Fatalf("expected 1 placeholder, got %d", len(ge.GetPlaceholders()))
	}
}

func TestMergeGraphExpansions(t *testing.T) {
	t.Parallel()

	fvA := &values.FieldValue{Field: "A", Typ: values.TypeInt}
	fvB := &values.FieldValue{Field: "B", Typ: values.TypeString}

	aliasA := values.UniqueCorrelationIdentifier()
	aliasB := values.UniqueCorrelationIdentifier()
	phA := predicates.NewPlaceholder(aliasA, fvA)
	phB := predicates.NewPlaceholder(aliasB, fvB)

	ge1 := NewGraphExpansionBuilder().
		AddColumn("A", fvA).
		AddPlaceholder(phA).
		AddPredicate(phA).
		Build()

	ge2 := NewGraphExpansionBuilder().
		AddColumn("B", fvB).
		AddPlaceholder(phB).
		AddPredicate(phB).
		Build()

	merged := MergeGraphExpansions(ge1, ge2)

	if len(merged.GetResultColumns()) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(merged.GetResultColumns()))
	}
	if merged.GetResultColumns()[0].Name != "A" {
		t.Fatalf("expected first column A, got %q", merged.GetResultColumns()[0].Name)
	}
	if merged.GetResultColumns()[1].Name != "B" {
		t.Fatalf("expected second column B, got %q", merged.GetResultColumns()[1].Name)
	}
	if len(merged.GetPredicates()) != 2 {
		t.Fatalf("expected 2 predicates, got %d", len(merged.GetPredicates()))
	}
	if len(merged.GetPlaceholders()) != 2 {
		t.Fatalf("expected 2 placeholders, got %d", len(merged.GetPlaceholders()))
	}
}

func TestSealDeduplicatesPlaceholdersByAlias(t *testing.T) {
	t.Parallel()

	fv := &values.FieldValue{Field: "X", Typ: values.TypeInt}
	alias := values.UniqueCorrelationIdentifier()

	// Two placeholders with the same alias but added separately —
	// simulates the duplicate-by-alias case from Java's seal().
	ph1 := predicates.NewPlaceholder(alias, fv)
	ph2 := predicates.NewPlaceholder(alias, fv)

	ge := NewGraphExpansion(
		nil,
		[]predicates.QueryPredicate{ph1, ph2},
		nil,
		[]*predicates.Placeholder{ph1, ph2},
	)

	sealed := ge.Seal()

	// After sealing, duplicate-by-alias placeholders should be
	// collapsed to one.
	if len(sealed.GetPlaceholders()) != 1 {
		t.Fatalf("expected 1 deduplicated placeholder, got %d", len(sealed.GetPlaceholders()))
	}
	if sealed.GetPlaceholders()[0].ParameterAlias != alias {
		t.Fatalf("placeholder alias mismatch")
	}
}

func TestSealNoPlaceholders(t *testing.T) {
	t.Parallel()

	fv := &values.FieldValue{Field: "COL", Typ: values.TypeInt}
	pred := predicates.NewConstantPredicate(predicates.TriTrue)

	ge := NewGraphExpansionBuilder().
		AddColumn("COL", fv).
		AddPredicate(pred).
		Build()

	sealed := ge.Seal()

	if len(sealed.GetPlaceholders()) != 0 {
		t.Fatalf("expected 0 placeholders, got %d", len(sealed.GetPlaceholders()))
	}
	if len(sealed.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(sealed.GetPredicates()))
	}
	if sealed.GetResultValue() == nil {
		t.Fatal("expected non-nil result value from columns")
	}
}

func TestBuildSelectWithResultValue(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(&expressions.FullUnorderedScanExpression{})
	q := expressions.ForEachQuantifier(ref)

	pred := predicates.NewConstantPredicate(predicates.TriTrue)

	// No columns — BuildSelectWithResultValue requires empty columns.
	ge := NewGraphExpansionBuilder().
		AddPredicate(pred).
		AddQuantifier(q).
		Build()

	sealed := ge.Seal()

	resultValue := &values.FieldValue{Field: "RESULT", Typ: values.TypeInt}
	sel := sealed.BuildSelectWithResultValue(resultValue)

	if sel == nil {
		t.Fatal("expected non-nil SelectExpression")
	}
	if len(sel.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(sel.GetQuantifiers()))
	}
	if len(sel.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(sel.GetPredicates()))
	}
	if sel.GetResultValue() != resultValue {
		t.Fatal("result value should be the one passed to BuildSelectWithResultValue")
	}
}

func TestBuildSelectWithResultValuePanicsOnNonEmptyColumns(t *testing.T) {
	t.Parallel()

	fv := &values.FieldValue{Field: "A", Typ: values.TypeInt}
	ge := NewGraphExpansionBuilder().
		AddColumn("A", fv).
		Build()

	sealed := ge.Seal()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic from BuildSelectWithResultValue with non-empty columns")
		}
	}()
	sealed.BuildSelectWithResultValue(fv)
}

func TestBuildSelect(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(&expressions.FullUnorderedScanExpression{})
	q := expressions.ForEachQuantifier(ref)

	fv := &values.FieldValue{Field: "Y", Typ: values.TypeInt}

	ge := NewGraphExpansionBuilder().
		AddColumn("Y", fv).
		AddQuantifier(q).
		Build()

	sealed := ge.Seal()
	sel := sealed.BuildSelect()

	if sel == nil {
		t.Fatal("expected non-nil SelectExpression")
	}
	if len(sel.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(sel.GetQuantifiers()))
	}
	// The result value should be a RecordConstructorValue.
	rv := sel.GetResultValue()
	if rv == nil {
		t.Fatal("expected non-nil result value")
	}
	if rv.Name() != "record" {
		t.Fatalf("expected record result value, got %q", rv.Name())
	}
}

func TestSealDuplicateColumnNames(t *testing.T) {
	t.Parallel()

	// Two columns with the same name should both become unnamed
	// after sealing (Java behavior).
	fv1 := &values.FieldValue{Field: "A", Typ: values.TypeInt}
	fv2 := &values.FieldValue{Field: "B", Typ: values.TypeString}

	ge := NewGraphExpansionBuilder().
		AddColumn("DUP", fv1).
		AddColumn("DUP", fv2).
		AddColumn("UNIQUE", &values.FieldValue{Field: "C", Typ: values.TypeBool}).
		Build()

	sealed := ge.Seal()
	cols := sealed.GetResultColumns()

	if len(cols) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(cols))
	}
	// The two "DUP" columns should be unnamed now.
	if cols[0].Name != "" {
		t.Fatalf("expected first DUP column to be unnamed, got %q", cols[0].Name)
	}
	if cols[1].Name != "" {
		t.Fatalf("expected second DUP column to be unnamed, got %q", cols[1].Name)
	}
	// The unique column should keep its name.
	if cols[2].Name != "UNIQUE" {
		t.Fatalf("expected UNIQUE column to keep name, got %q", cols[2].Name)
	}
}

func TestGetPlaceholdersOnSealed(t *testing.T) {
	t.Parallel()

	fv := &values.FieldValue{Field: "Z", Typ: values.TypeInt}
	alias1 := values.UniqueCorrelationIdentifier()
	alias2 := values.UniqueCorrelationIdentifier()
	ph1 := predicates.NewPlaceholder(alias1, fv)
	ph2 := predicates.NewPlaceholder(alias2, &values.FieldValue{Field: "W", Typ: values.TypeString})

	ge := NewGraphExpansion(
		nil,
		[]predicates.QueryPredicate{ph1, ph2},
		nil,
		[]*predicates.Placeholder{ph1, ph2},
	)

	sealed := ge.Seal()
	phs := sealed.GetPlaceholders()

	if len(phs) != 2 {
		t.Fatalf("expected 2 placeholders, got %d", len(phs))
	}
	// Both should have distinct aliases.
	if phs[0].ParameterAlias == phs[1].ParameterAlias {
		t.Fatal("placeholders should have distinct aliases")
	}
}

func TestNewGraphExpansionDefensiveCopy(t *testing.T) {
	t.Parallel()

	cols := []GraphExpansionColumn{{Name: "A", Value: &values.FieldValue{Field: "A"}}}
	ge := NewGraphExpansion(cols, nil, nil, nil)

	// Mutate the original slice — should not affect the expansion.
	cols[0].Name = "MUTATED"
	if ge.GetResultColumns()[0].Name != "A" {
		t.Fatal("NewGraphExpansion should defensively copy columns")
	}
}

func TestBuilderDefensiveCopy(t *testing.T) {
	t.Parallel()

	b := NewGraphExpansionBuilder().
		AddColumn("X", &values.FieldValue{Field: "X"})

	ge := b.Build()

	// Adding to builder after Build should not affect the built expansion.
	b.AddColumn("Y", &values.FieldValue{Field: "Y"})
	if len(ge.GetResultColumns()) != 1 {
		t.Fatalf("expected 1 column after Build, got %d", len(ge.GetResultColumns()))
	}
}
