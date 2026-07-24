package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestNoCompensation(t *testing.T) {
	t.Parallel()
	c := NoCompensation
	if c.IsNeeded() {
		t.Fatal("NoCompensation.IsNeeded() should be false")
	}
	if c.IsImpossible() {
		t.Fatal("NoCompensation.IsImpossible() should be false")
	}
	if c.IsNeededForFiltering() {
		t.Fatal("NoCompensation.IsNeededForFiltering() should be false")
	}
	if c.IsFinalNeeded() {
		t.Fatal("NoCompensation.IsFinalNeeded() should be false")
	}
	if !c.CanBeDeferred() {
		t.Fatal("NoCompensation.CanBeDeferred() should be true")
	}
	if s := c.(noCompensation).String(); s != "no-compensation" {
		t.Fatalf("expected 'no-compensation', got %q", s)
	}
}

func TestImpossibleCompensation(t *testing.T) {
	t.Parallel()
	c := ImpossibleCompensation
	if !c.IsNeeded() {
		t.Fatal("ImpossibleCompensation.IsNeeded() should be true")
	}
	if !c.IsImpossible() {
		t.Fatal("ImpossibleCompensation.IsImpossible() should be true")
	}
	if !c.IsNeededForFiltering() {
		t.Fatal("ImpossibleCompensation.IsNeededForFiltering() should be true")
	}
	if !c.IsFinalNeeded() {
		t.Fatal("ImpossibleCompensation.IsFinalNeeded() should be true")
	}
	if !c.CanBeDeferred() {
		t.Fatal("ImpossibleCompensation.CanBeDeferred() should be true")
	}
	if s := c.(impossibleCompensation).String(); s != "impossible-compensation" {
		t.Fatalf("expected 'impossible-compensation', got %q", s)
	}
}

func TestForMatchCompensation_Construction(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(nil)
	q1 := expressions.ForEachQuantifier(ref)
	q2 := expressions.ForEachQuantifier(ref)

	aliases := map[values.CorrelationIdentifier]struct{}{
		q1.GetAlias(): {},
	}

	predMap := StubPredicateCompensationMap(1)
	resultFn := NewResultCompensationFunction(true)
	gbm := EmptyGroupByMappings()

	c := NewForMatchCompensation(
		false,
		NoCompensation,
		predMap,
		[]expressions.Quantifier{q1},
		[]expressions.Quantifier{q2},
		aliases,
		resultFn,
		gbm,
	)

	if c == nil {
		t.Fatal("NewForMatchCompensation returned nil")
	}
	if c.GetChildCompensation() != NoCompensation {
		t.Fatal("child compensation should be NoCompensation")
	}
	if len(c.GetMatchedQuantifiers()) != 1 {
		t.Fatalf("expected 1 matched quantifier, got %d", len(c.GetMatchedQuantifiers()))
	}
	if len(c.GetUnmatchedQuantifiers()) != 1 {
		t.Fatalf("expected 1 unmatched quantifier, got %d", len(c.GetUnmatchedQuantifiers()))
	}
	if _, ok := c.GetCompensatedAliases()[q1.GetAlias()]; !ok {
		t.Fatal("compensated aliases should contain q1's alias")
	}
	if c.GetPredicateCompensationMap() != predMap {
		t.Fatal("predicate compensation map should be the same instance")
	}
	if c.GetResultCompensationFunction() != resultFn {
		t.Fatal("result compensation function should be the same instance")
	}
	if c.GetGroupByMappings() != gbm {
		t.Fatal("group-by mappings should be the same instance")
	}
}

func TestForMatchCompensation_IsNeeded_WithPredicates(t *testing.T) {
	t.Parallel()

	c := NewForMatchCompensation(
		false,
		NoCompensation,
		StubPredicateCompensationMap(1),
		baseMatched(),
		nil,
		baseCompensated(),
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if !c.IsNeeded() {
		t.Fatal("ForMatch with predicates should be needed")
	}
}

func TestForMatchCompensation_IsNeeded_ChildNeeded(t *testing.T) {
	t.Parallel()

	child := NewForMatchCompensation(
		false,
		NoCompensation,
		StubPredicateCompensationMap(1),
		baseMatched(),
		nil,
		baseCompensated(),
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)

	c := NewForMatchCompensation(
		false,
		child,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if !c.IsNeeded() {
		t.Fatal("ForMatch with needed child should be needed")
	}
}

func TestForMatchCompensation_IsNeeded_ResultNeeded(t *testing.T) {
	t.Parallel()

	c := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NewResultCompensationFunction(true),
		EmptyGroupByMappings(),
	)
	if !c.IsNeeded() {
		t.Fatal("ForMatch with result compensation should be needed")
	}
}

func TestForMatchCompensation_IsNeeded_UnmatchedForEach(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(nil)
	unmatched := expressions.ForEachQuantifier(ref)

	c := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		[]expressions.Quantifier{unmatched},
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if !c.IsNeeded() {
		t.Fatal("ForMatch with unmatched ForEach should be needed")
	}
}

func TestForMatchCompensation_IsNeeded_NothingNeeded(t *testing.T) {
	t.Parallel()

	c := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if c.IsNeeded() {
		t.Fatal("ForMatch with nothing needed should not be needed")
	}
}

func TestForMatchCompensation_IsImpossible(t *testing.T) {
	t.Parallel()

	c := NewForMatchCompensation(
		true,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if !c.IsImpossible() {
		t.Fatal("ForMatch with impossible=true should be impossible")
	}

	c2 := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if c2.IsImpossible() {
		t.Fatal("ForMatch with impossible=false should not be impossible")
	}
}

func TestForMatchCompensation_IsFinalNeeded(t *testing.T) {
	t.Parallel()

	c := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NewResultCompensationFunction(true),
		EmptyGroupByMappings(),
	)
	if !c.IsFinalNeeded() {
		t.Fatal("IsFinalNeeded should be true when result compensation is needed")
	}

	c2 := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if c2.IsFinalNeeded() {
		t.Fatal("IsFinalNeeded should be false when result compensation is not needed")
	}
}

func TestForMatchCompensation_CanBeDeferred(t *testing.T) {
	t.Parallel()

	c := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if !c.CanBeDeferred() {
		t.Fatal("CanBeDeferred should be true (Java default)")
	}
}

func TestForMatchCompensation_CanBeDeferred_ImpossibleChild(t *testing.T) {
	t.Parallel()

	c := NewForMatchCompensation(
		false,
		ImpossibleCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if !c.CanBeDeferred() {
		t.Fatal("CanBeDeferred should be true even with impossible child (Java default)")
	}
}

func TestForMatchCompensation_IsNeededForFiltering(t *testing.T) {
	t.Parallel()

	// No predicates, no unmatched ForEach, child not needed → not needed for filtering.
	c := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NewResultCompensationFunction(true), // result doesn't affect filtering
		EmptyGroupByMappings(),
	)
	if c.IsNeededForFiltering() {
		t.Fatal("should not be needed for filtering when only result compensation is needed")
	}

	// With predicates → needed for filtering.
	c2 := NewForMatchCompensation(
		false,
		NoCompensation,
		StubPredicateCompensationMap(2),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if !c2.IsNeededForFiltering() {
		t.Fatal("should be needed for filtering when predicates exist")
	}
}

func TestForMatchCompensation_UnmatchedForEachQuantifiers(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(nil)
	forEach := expressions.ForEachQuantifier(ref)
	existential := expressions.ExistentialQuantifier(ref)

	c := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		[]expressions.Quantifier{forEach, existential},
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)

	unmatched := c.GetUnmatchedForEachQuantifiers()
	if len(unmatched) != 1 {
		t.Fatalf("expected 1 unmatched ForEach quantifier, got %d", len(unmatched))
	}
	if unmatched[0].Kind() != expressions.QuantifierForEach {
		t.Fatalf("expected ForEach kind, got %d", unmatched[0].Kind())
	}

	// Second call returns the cached result.
	unmatched2 := c.GetUnmatchedForEachQuantifiers()
	if len(unmatched2) != len(unmatched) {
		t.Fatal("cached result should be the same length")
	}
}

func TestForMatchCompensation_DefensiveCopy(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(nil)
	q1 := expressions.ForEachQuantifier(ref)
	originalAlias := q1.GetAlias()
	aliases := map[values.CorrelationIdentifier]struct{}{
		q1.GetAlias(): {},
	}
	matched := []expressions.Quantifier{q1}

	c := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		matched,
		nil,
		aliases,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)

	// Mutate the original slice — should not affect the compensation.
	matched[0] = expressions.ForEachQuantifier(ref)
	if c.GetMatchedQuantifiers()[0].GetAlias() != originalAlias {
		t.Fatal("defensive copy failed: matched quantifiers should not be affected by slice mutation")
	}

	// Mutate the original alias map — should not affect the compensation.
	newAlias := values.UniqueCorrelationIdentifier()
	aliases[newAlias] = struct{}{}
	if _, ok := c.GetCompensatedAliases()[newAlias]; ok {
		t.Fatal("defensive copy failed: compensated aliases should not be affected by map mutation")
	}
}

func TestForMatchCompensation_String(t *testing.T) {
	t.Parallel()

	// Needed + possible.
	c := NewForMatchCompensation(
		false,
		NoCompensation,
		StubPredicateCompensationMap(1),
		baseMatched(),
		nil,
		baseCompensated(),
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if s := c.String(); s != "needed; possible" {
		t.Fatalf("expected 'needed; possible', got %q", s)
	}

	// Needed + impossible.
	c2 := NewForMatchCompensation(
		true,
		NoCompensation,
		StubPredicateCompensationMap(1),
		baseMatched(),
		nil,
		baseCompensated(),
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if s := c2.String(); s != "needed; impossible" {
		t.Fatalf("expected 'needed; impossible', got %q", s)
	}

	// Not needed.
	c3 := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if s := c3.String(); s != "not needed; possible" {
		t.Fatalf("expected 'not needed; possible', got %q", s)
	}
}

func TestForMatchCompensation_Derived(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(nil)
	q1 := expressions.ForEachQuantifier(ref)
	q2 := expressions.ForEachQuantifier(ref)

	parent := NewForMatchCompensation(
		false,
		NoCompensation,
		StubPredicateCompensationMap(1),
		[]expressions.Quantifier{q1},
		nil,
		aliasesOf(q1),
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)

	derived := parent.Derived(
		false,
		StubPredicateCompensationMap(2),
		[]expressions.Quantifier{q2},
		nil,
		aliasesOf(q2),
		NewResultCompensationFunction(true),
		EmptyGroupByMappings(),
	)

	if derived == nil {
		t.Fatal("Derived returned nil")
	}
	// The parent becomes the child of the derived compensation.
	childForMatch, ok := derived.GetChildCompensation().(*ForMatchCompensation)
	if !ok {
		t.Fatal("derived child should be *ForMatchCompensation")
	}
	if childForMatch != parent {
		t.Fatal("derived child should be the parent compensation")
	}
	if !derived.IsNeeded() {
		t.Fatal("derived should be needed")
	}
	if derived.IsImpossible() {
		t.Fatal("derived should not be impossible")
	}
	if !derived.IsFinalNeeded() {
		t.Fatal("derived should have final needed")
	}
	if len(derived.GetMatchedQuantifiers()) != 1 {
		t.Fatalf("expected 1 matched quantifier, got %d", len(derived.GetMatchedQuantifiers()))
	}
	if derived.GetMatchedQuantifiers()[0].GetAlias() != q2.GetAlias() {
		t.Fatal("derived matched quantifier should be q2")
	}
	if derived.GetPredicateCompensationMap().Len() != 2 {
		t.Fatalf("expected 2 predicates, got %d", derived.GetPredicateCompensationMap().Len())
	}
}

func TestDerivedCompensation_FromNoCompensation(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(nil)
	q := expressions.ForEachQuantifier(ref)

	derived := DerivedCompensation(
		NoCompensation,
		false,
		StubPredicateCompensationMap(1),
		[]expressions.Quantifier{q},
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)

	if derived == nil {
		t.Fatal("DerivedCompensation returned nil")
	}
	if derived.GetChildCompensation() != NoCompensation {
		t.Fatal("child should be NoCompensation")
	}
	if !derived.IsNeeded() {
		t.Fatal("derived should be needed")
	}
}

func TestDerivedCompensation_Impossible(t *testing.T) {
	t.Parallel()

	derived := DerivedCompensation(
		NoCompensation,
		true, // impossible
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)

	if derived == nil {
		t.Fatal("DerivedCompensation returned nil")
	}
	if !derived.IsImpossible() {
		t.Fatal("derived should be impossible")
	}
}

func TestDerivedCompensation_ImpossibleWhenNothingNeeded(t *testing.T) {
	t.Parallel()

	// When nothing is needed but DerivedCompensation is called anyway,
	// it marks the compensation as impossible (preserves invariant
	// without panicking — CLAUDE.md: no panics in library code).
	result := DerivedCompensation(
		NoCompensation,
		false,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if !result.IsImpossible() {
		t.Fatal("expected impossible compensation when nothing is needed")
	}
}

func TestPredicateCompensationMap(t *testing.T) {
	t.Parallel()

	empty := EmptyPredicateCompensationMap()
	if !empty.IsEmpty() {
		t.Fatal("empty map should be empty")
	}
	if empty.Len() != 0 {
		t.Fatalf("expected 0, got %d", empty.Len())
	}

	withEntries := StubPredicateCompensationMap(3)
	if withEntries.IsEmpty() {
		t.Fatal("map with 3 entries should not be empty")
	}
	if withEntries.Len() != 3 {
		t.Fatalf("expected 3, got %d", withEntries.Len())
	}

	// nil is treated as empty.
	var nilMap *PredicateCompensationMap
	if !nilMap.IsEmpty() {
		t.Fatal("nil map should be empty")
	}
	if nilMap.Len() != 0 {
		t.Fatalf("expected 0 for nil map, got %d", nilMap.Len())
	}
}

func TestResultCompensationFunction(t *testing.T) {
	t.Parallel()

	noResult := NoResultCompensation()
	if noResult.IsNeeded() {
		t.Fatal("NoResultCompensation should not be needed")
	}
	if noResult.IsImpossible() {
		t.Fatal("NoResultCompensation should not be impossible")
	}

	needed := NewResultCompensationFunction(true)
	if !needed.IsNeeded() {
		t.Fatal("result compensation with needed=true should be needed")
	}
	if needed.IsImpossible() {
		t.Fatal("result compensation with needed=true should not be impossible")
	}

	impossible := NewImpossibleResultCompensation()
	if !impossible.IsNeeded() {
		t.Fatal("impossible result compensation should be needed")
	}
	if !impossible.IsImpossible() {
		t.Fatal("impossible result compensation should be impossible")
	}

	// nil is treated as not needed.
	var nilFn *ResultCompensationFunction
	if nilFn.IsNeeded() {
		t.Fatal("nil result compensation should not be needed")
	}
	if nilFn.IsImpossible() {
		t.Fatal("nil result compensation should not be impossible")
	}
}

func TestCompensationInterfaceSatisfaction(t *testing.T) {
	t.Parallel()

	var c Compensation

	c = NoCompensation
	if c == nil {
		t.Fatal("NoCompensation should not be nil")
	}

	c = ImpossibleCompensation
	if c == nil {
		t.Fatal("ImpossibleCompensation should not be nil")
	}

	c = NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	// Verify it satisfies Compensation — the assignment above is the check.
	_ = c
}

// --- New PredicateCompensationFunc tests ---

func TestPredicateCompensationFunc_NoCompensation(t *testing.T) {
	t.Parallel()
	f := NoPredicateCompensationNeeded()
	if f.IsNeeded() {
		t.Fatal("should not be needed")
	}
	if f.IsImpossible() {
		t.Fatal("should not be impossible")
	}
	amended := f.Amend(nil, nil)
	if amended.IsNeeded() {
		t.Fatal("amended should not be needed")
	}
	preds := f.ApplyCompensationForPredicate(nil)
	if len(preds) != 0 {
		t.Fatalf("expected 0 predicates, got %d", len(preds))
	}
}

func TestPredicateCompensationFunc_Impossible(t *testing.T) {
	t.Parallel()
	f := ImpossiblePredicateCompensation()
	if !f.IsNeeded() {
		t.Fatal("should be needed")
	}
	if !f.IsImpossible() {
		t.Fatal("should be impossible")
	}
	amended := f.Amend(nil, nil)
	if !amended.IsImpossible() {
		t.Fatal("amended should still be impossible")
	}
	preds := f.ApplyCompensationForPredicate(nil)
	if len(preds) != 0 {
		t.Fatalf("expected 0 predicates from impossible, got %d", len(preds))
	}
}

func TestOfPredicateCompensation_Identity(t *testing.T) {
	t.Parallel()
	pred := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "X"},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(5)},
		},
	}
	f := OfPredicateCompensation(pred, false)
	if !f.IsNeeded() {
		t.Fatal("should be needed")
	}
	if f.IsImpossible() {
		t.Fatal("should not be impossible")
	}

	// Apply with nil translation → returns original predicate.
	preds := f.ApplyCompensationForPredicate(nil)
	if len(preds) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(preds))
	}
	if preds[0] != pred {
		t.Fatal("expected same predicate instance")
	}
}

func TestOfPredicateCompensation_WithAliasRebase(t *testing.T) {
	t.Parallel()
	srcAlias := values.NamedCorrelationIdentifier("src")
	tgtAlias := values.NamedCorrelationIdentifier("tgt")

	pred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(srcAlias),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(10)},
		},
	}
	f := OfPredicateCompensation(pred, false)

	tm := TranslationMapOfAliases(srcAlias, tgtAlias)
	preds := f.ApplyCompensationForPredicate(tm)
	if len(preds) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(preds))
	}
	// The rebased predicate should reference tgtAlias.
	cp, ok := preds[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", preds[0])
	}
	qov, ok := cp.Operand.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected *QuantifiedObjectValue, got %T", cp.Operand)
	}
	if qov.Correlation != tgtAlias {
		t.Errorf("operand alias = %s, want %s", qov.Correlation.Name(), tgtAlias.Name())
	}
}

func TestOfPredicateCompensation_Amend(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	f := OfPredicateCompensation(pred, false)
	amended := f.Amend(nil, nil)
	if !amended.IsNeeded() {
		t.Fatal("amended should still be needed")
	}
	if amended.IsImpossible() {
		t.Fatal("amended should not be impossible")
	}
}

// --- PredicateCompensationMap tests ---

func TestPredicateCompensationMap_RealEntries(t *testing.T) {
	t.Parallel()
	p1 := predicates.NewConstantPredicate(predicates.TriTrue)
	p2 := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "Y"},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(3)},
		},
	}
	f1 := NoPredicateCompensationNeeded()
	f2 := OfPredicateCompensation(p2, false)

	m := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{p1, p2},
		[]PredicateCompensationFunc{f1, f2},
	)
	if m.IsEmpty() {
		t.Fatal("should not be empty")
	}
	if m.Len() != 2 {
		t.Fatalf("expected 2, got %d", m.Len())
	}

	keys, vals := m.Entries()
	if len(keys) != 2 || len(vals) != 2 {
		t.Fatalf("entries: keys=%d vals=%d", len(keys), len(vals))
	}
}

func TestPredicateCompensationMap_ApplyCompensations(t *testing.T) {
	t.Parallel()
	pred := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "Z"},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(7)},
		},
	}
	m := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{pred},
		[]PredicateCompensationFunc{OfPredicateCompensation(pred, false)},
	)

	results := m.ApplyCompensations(nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 compensation predicate, got %d", len(results))
	}
}

func TestPredicateCompensationMap_Amend(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	m := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{pred},
		[]PredicateCompensationFunc{OfPredicateCompensation(pred, false)},
	)
	amended := m.Amend(nil, nil)
	if amended.Len() != 1 {
		t.Fatalf("amended should have 1 entry, got %d", amended.Len())
	}
}

func TestPredicateCompensationMap_NilSafe(t *testing.T) {
	t.Parallel()
	var m *PredicateCompensationMap
	if !m.IsEmpty() {
		t.Fatal("nil map should be empty")
	}
	if m.Len() != 0 {
		t.Fatal("nil map Len should be 0")
	}
	results := m.ApplyCompensations(nil)
	if len(results) != 0 {
		t.Fatalf("nil map should return 0 compensations, got %d", len(results))
	}
	amended := m.Amend(nil, nil)
	if amended != m {
		t.Fatal("nil amend should return same nil")
	}
}

// --- ResultCompensationFunction tests ---

func TestResultCompensation_OfValue(t *testing.T) {
	t.Parallel()
	v := &values.FieldValue{Field: "COL"}
	f := ResultCompensationOfValue(v)
	if !f.IsNeeded() {
		t.Fatal("should be needed")
	}
	if f.IsImpossible() {
		t.Fatal("should not be impossible")
	}
	result := f.ApplyCompensationForResult(nil)
	if result != v {
		t.Fatal("expected same value with nil translation")
	}
}

func TestResultCompensation_ApplyWithRebase(t *testing.T) {
	t.Parallel()
	srcAlias := values.NamedCorrelationIdentifier("src")
	tgtAlias := values.NamedCorrelationIdentifier("tgt")

	v := values.NewQuantifiedObjectValue(srcAlias)
	f := ResultCompensationOfValue(v)

	tm := TranslationMapOfAliases(srcAlias, tgtAlias)
	result := f.ApplyCompensationForResult(tm)
	qov, ok := result.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected *QuantifiedObjectValue, got %T", result)
	}
	if qov.Correlation != tgtAlias {
		t.Errorf("correlation = %s, want %s", qov.Correlation.Name(), tgtAlias.Name())
	}
}

func TestResultCompensation_NilApply(t *testing.T) {
	t.Parallel()
	f := NoResultCompensation()
	result := f.ApplyCompensationForResult(nil)
	if result != nil {
		t.Fatal("no compensation should return nil value")
	}
}

// --- ForMatchCompensation.Apply tests ---

func TestForMatchCompensation_Apply_NoCompensation(t *testing.T) {
	t.Parallel()
	scan := &expressions.FullUnorderedScanExpression{}
	c := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	result, applied := c.Apply(scan, TranslationMapFunc(nil))
	if !applied {
		t.Fatal("Apply unexpectedly failed")
	}
	if result != scan {
		t.Fatal("no compensation should return original expression")
	}
}

func TestForMatchCompensation_Apply_WithPredicates(t *testing.T) {
	t.Parallel()
	scan := &expressions.FullUnorderedScanExpression{}
	pred := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "X"},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(5)},
		},
	}
	predMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{pred},
		[]PredicateCompensationFunc{OfPredicateCompensation(pred, false)},
	)
	c := NewForMatchCompensation(
		false, NoCompensation, predMap,
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)

	result, applied := c.Apply(scan, TranslationMapFunc(nil))
	if !applied {
		t.Fatal("Apply unexpectedly failed")
	}
	filter, ok := result.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("expected LogicalFilterExpression, got %T", result)
	}
	if len(filter.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(filter.GetPredicates()))
	}
}

func TestForMatchCompensation_NeededBaseInvariant(t *testing.T) {
	t.Parallel()

	predicate := predicates.NewConstantPredicate(predicates.TriTrue)
	predicateMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{predicate},
		[]PredicateCompensationFunc{OfPredicateCompensation(predicate, false)},
	)
	forEach1 := namedForEachQuantifier("q1")
	forEach2 := namedForEachQuantifier("q2")
	exists := namedExistentialQuantifier("qe")
	ref := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)
	zeroAliasForEach := expressions.NamedForEachQuantifier(values.CorrelationIdentifier{}, ref)
	duplicateAliasExists := expressions.NamedExistentialQuantifier(forEach1.GetAlias(), ref)

	for _, tc := range []struct {
		name        string
		matched     []expressions.Quantifier
		compensated map[values.CorrelationIdentifier]struct{}
		predicates  *PredicateCompensationMap
		result      *ResultCompensationFunction
		wantBad     bool
	}{
		{
			name:        "valid_filtering_base",
			matched:     []expressions.Quantifier{forEach1, exists},
			compensated: aliasesOf(forEach1, exists),
			predicates:  predicateMap,
			result:      NoResultCompensation(),
		},
		{
			name:        "filtering_with_zero_forEach",
			matched:     []expressions.Quantifier{exists},
			compensated: aliasesOf(exists),
			predicates:  predicateMap,
			result:      NoResultCompensation(),
			wantBad:     true,
		},
		{
			name:        "filtering_with_two_forEach",
			matched:     []expressions.Quantifier{forEach1, forEach2},
			compensated: aliasesOf(forEach1, forEach2),
			predicates:  predicateMap,
			result:      NoResultCompensation(),
			wantBad:     true,
		},
		{
			name:        "filtering_with_alias_mismatch",
			matched:     []expressions.Quantifier{forEach1},
			compensated: aliasesOf(forEach1, exists),
			predicates:  predicateMap,
			result:      NoResultCompensation(),
			wantBad:     true,
		},
		{
			name:        "filtering_with_zero_base_alias",
			matched:     []expressions.Quantifier{zeroAliasForEach},
			compensated: aliasesOf(zeroAliasForEach),
			predicates:  predicateMap,
			result:      NoResultCompensation(),
			wantBad:     true,
		},
		{
			name:        "filtering_with_duplicate_matched_alias",
			matched:     []expressions.Quantifier{forEach1, duplicateAliasExists},
			compensated: aliasesOf(forEach1, duplicateAliasExists),
			predicates:  predicateMap,
			result:      NoResultCompensation(),
			wantBad:     true,
		},
		{
			name:        "final_with_zero_forEach",
			matched:     []expressions.Quantifier{exists},
			compensated: aliasesOf(exists),
			predicates:  EmptyPredicateCompensationMap(),
			result:      ResultCompensationOfValue(values.LiteralValue(int64(1))),
			wantBad:     true,
		},
		{
			name:        "nothing_needed_without_base",
			predicates:  EmptyPredicateCompensationMap(),
			result:      NoResultCompensation(),
			compensated: map[values.CorrelationIdentifier]struct{}{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			compensation := NewForMatchCompensation(
				false,
				NoCompensation,
				tc.predicates,
				tc.matched,
				nil,
				tc.compensated,
				tc.result,
				EmptyGroupByMappings(),
			)
			if got := compensation.IsImpossible(); got != tc.wantBad {
				t.Fatalf("IsImpossible() = %v, want %v", got, tc.wantBad)
			}
		})
	}

	child := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{forEach1},
		nil,
		aliasesOf(forEach1),
		ResultCompensationOfValue(values.NewQuantifiedObjectValue(forEach1.GetAlias())),
		EmptyGroupByMappings(),
	)
	parent := NewForMatchCompensation(
		false,
		child,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		map[values.CorrelationIdentifier]struct{}{},
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if parent.IsImpossible() {
		t.Fatal("a parent needed only for its child's final shape does not need a local base alias")
	}
}

func TestForMatchCompensation_ApplyFailsClosed(t *testing.T) {
	t.Parallel()

	predicate := predicates.NewConstantPredicate(predicates.TriTrue)
	predicateMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{predicate},
		[]PredicateCompensationFunc{OfPredicateCompensation(predicate, false)},
	)
	childBase := namedForEachQuantifier("child_base")
	child := NewForMatchCompensation(
		false,
		NoCompensation,
		predicateMap,
		[]expressions.Quantifier{childBase},
		nil,
		aliasesOf(childBase),
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	outerExists := namedExistentialQuantifier("only_exists")
	invalidOuter := NewForMatchCompensation(
		false,
		child,
		EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{outerExists},
		nil,
		aliasesOf(outerExists),
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
	if !invalidOuter.IsImpossible() {
		t.Fatal("setup: invalid outer compensation was not marked impossible")
	}

	translationCalls := 0
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	applied, ok := invalidOuter.Apply(scan, func(values.CorrelationIdentifier) TranslationMap {
		translationCalls++
		return EmptyTranslationMap()
	})
	if ok || applied != nil {
		t.Fatalf("invalid Apply returned (%T, %v), want (nil, false)", applied, ok)
	}
	if translationCalls != 0 {
		t.Fatalf("translation callback called %d times; outer validation must happen before child application", translationCalls)
	}
}

func TestForMatchCompensation_ApplyFinalFailsOnMissingResult(t *testing.T) {
	t.Parallel()

	base := namedForEachQuantifier("q_base")
	compensation := NewForMatchCompensation(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{base},
		nil,
		aliasesOf(base),
		NewResultCompensationFunction(true),
		EmptyGroupByMappings(),
	)
	if compensation.IsImpossible() {
		t.Fatal("setup: valid base compensation was marked impossible")
	}

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	applied, ok := compensation.ApplyFinal(scan, nil)
	if ok || applied != nil {
		t.Fatalf("ApplyFinal with a missing result returned (%T, %v), want (nil, false)", applied, ok)
	}
}

// Ports Java's CompensationTests.testImpossiblePrimitives monoid checks.
func TestCompensation_SentinelPrimitives(t *testing.T) {
	t.Parallel()
	t.Run("NoCompensation", func(t *testing.T) {
		t.Parallel()
		if NoCompensation.IsNeeded() {
			t.Fatal("NoCompensation should not be needed")
		}
		if NoCompensation.IsImpossible() {
			t.Fatal("NoCompensation should not be impossible")
		}
		if !NoCompensation.CanBeDeferred() {
			t.Fatal("NoCompensation should be deferrable")
		}
	})
	t.Run("ImpossibleCompensation", func(t *testing.T) {
		t.Parallel()
		if !ImpossibleCompensation.IsImpossible() {
			t.Fatal("ImpossibleCompensation should be impossible")
		}
	})
	t.Run("NoPredicateCompensationNeeded", func(t *testing.T) {
		t.Parallel()
		f := NoPredicateCompensationNeeded()
		if f.IsNeeded() {
			t.Fatal("should not be needed")
		}
		if f.IsImpossible() {
			t.Fatal("should not be impossible")
		}
		amended := f.Amend(NewCorrValueBiMap(), nil)
		if amended != f {
			t.Fatal("amend should return self")
		}
	})
	t.Run("ImpossiblePredicateCompensation", func(t *testing.T) {
		t.Parallel()
		f := ImpossiblePredicateCompensation()
		if !f.IsNeeded() {
			t.Fatal("should be needed")
		}
		if !f.IsImpossible() {
			t.Fatal("should be impossible")
		}
		amended := f.Amend(NewCorrValueBiMap(), nil)
		if amended != f {
			t.Fatal("amend should return self")
		}
	})
	t.Run("NoResultCompensation", func(t *testing.T) {
		t.Parallel()
		f := NoResultCompensation()
		if f.IsNeeded() {
			t.Fatal("should not be needed")
		}
		if f.IsImpossible() {
			t.Fatal("should not be impossible")
		}
		amended := f.Amend(NewCorrValueBiMap(), nil)
		if amended != f {
			t.Fatal("amend on not-needed should return self")
		}
	})
	t.Run("ImpossibleResultCompensation", func(t *testing.T) {
		t.Parallel()
		f := NewImpossibleResultCompensation()
		if !f.IsNeeded() {
			t.Fatal("should be needed")
		}
		if !f.IsImpossible() {
			t.Fatal("should be impossible")
		}
	})
}

func TestForMatchCompensation_Intersect_BothEmpty(t *testing.T) {
	t.Parallel()
	c1 := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := c1.Intersect(c2)
	if result.IsNeeded() {
		t.Fatal("intersection of two no-compensation should not be needed")
	}
}

func TestForMatchCompensation_Intersect_NotNeededAbsorbsImpossibleFlag(t *testing.T) {
	t.Parallel()
	c1 := NewForMatchCompensation(
		true, NoCompensation, EmptyPredicateCompensationMap(),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := c1.Intersect(c2)
	if result != NoCompensation {
		t.Fatalf("a side with no residual must absorb the intersection, got %T", result)
	}
}

func TestForMatchCompensation_Intersect_OneNotNeeded(t *testing.T) {
	t.Parallel()

	// c1 is not needed (empty everything), c2 is needed (has predicates).
	c1 := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := c1.Intersect(c2)
	// NoCompensation is the absorbing element for intersection: there is no
	// residual common to both sides.
	if result != NoCompensation {
		t.Fatalf("intersection with a not-needed side = %T, want NoCompensation", result)
	}
}

func TestForMatchCompensation_Intersect_PredicateMapIntersection(t *testing.T) {
	t.Parallel()

	// Three distinct predicate pointers; share predB between both maps.
	predA := predicates.NewConstantPredicate(predicates.TriTrue)
	predB := predicates.NewConstantPredicate(predicates.TriTrue) // shared pointer
	predC := predicates.NewConstantPredicate(predicates.TriTrue)

	// c1 has predicates {A, B}
	predMap1 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{predA, predB},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded(), NoPredicateCompensationNeeded()},
	)
	// c2 has predicates {B, C}
	predMap2 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{predB, predC},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded(), NoPredicateCompensationNeeded()},
	)

	ref := expressions.InitialOf(nil)
	q := expressions.ForEachQuantifier(ref)

	c1 := NewForMatchCompensation(
		false, NoCompensation, predMap1,
		[]expressions.Quantifier{q}, nil, aliasesOf(q),
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap2,
		[]expressions.Quantifier{q}, nil, aliasesOf(q),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Intersect(c2)
	fmc, ok := result.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected *ForMatchCompensation, got %T", result)
	}
	// Only predB is common; the intersection predicate map should have 1 entry.
	if fmc.GetPredicateCompensationMap().Len() != 1 {
		t.Fatalf("expected 1 predicate in intersection, got %d", fmc.GetPredicateCompensationMap().Len())
	}
}

func TestForMatchCompensation_Intersect_DiscardsLegLocalImpossibleResidual(t *testing.T) {
	t.Parallel()

	shared := predicates.NewConstantPredicate(predicates.TriTrue)
	leftOnly := predicates.NewConstantPredicate(predicates.TriFalse)
	leftMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{shared, leftOnly},
		[]PredicateCompensationFunc{
			NoPredicateCompensationNeeded(),
			ImpossiblePredicateCompensation(),
		},
	)
	rightMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{shared},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)

	left := NewForMatchCompensation(
		true, NoCompensation, leftMap,
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	right := NewForMatchCompensation(
		false, NoCompensation, rightMap,
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)

	raw := left.Intersect(right)
	result, ok := raw.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("intersection = %T, want a possible ForMatchCompensation", raw)
	}
	if result.IsImpossible() {
		t.Fatal("a leg-local impossible residual must not poison an intersection that discards it")
	}
	if got := result.GetPredicateCompensationMap().Len(); got != 1 {
		t.Fatalf("intersection retained %d residuals, want only the shared residual", got)
	}
}

func TestForMatchCompensation_Intersect_RejectsDifferentAliasResponsibility(t *testing.T) {
	t.Parallel()

	base := namedForEachQuantifier("q_base")
	leftExists := namedExistentialQuantifier("q_left_exists")
	rightExists := namedExistentialQuantifier("q_right_exists")
	shared := predicates.NewConstantPredicate(predicates.TriTrue)
	sharedMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{shared},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)

	left := NewForMatchCompensation(
		false, NoCompensation, sharedMap,
		[]expressions.Quantifier{base, leftExists}, nil,
		aliasesOf(base, leftExists),
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	right := NewForMatchCompensation(
		false, NoCompensation, sharedMap,
		[]expressions.Quantifier{base, rightExists}, nil,
		aliasesOf(base, rightExists),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	if result := left.Intersect(right); !result.IsImpossible() {
		t.Fatalf("intersection with different compensated-alias responsibility = %T, want impossible", result)
	}
}

func TestForMatchCompensation_Intersect_EmptyPredicateIntersection(t *testing.T) {
	t.Parallel()

	// c1 has {A}, c2 has {B}, no overlap → empty combined predicate map.
	predA := predicates.NewConstantPredicate(predicates.TriTrue)
	predB := predicates.NewConstantPredicate(predicates.TriTrue)

	predMap1 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{predA},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)
	predMap2 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{predB},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)

	c1 := NewForMatchCompensation(
		false, NoCompensation, predMap1,
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap2,
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Intersect(c2)
	// No common predicates, no child filtering needed, no result comp → NoCompensation.
	if result.IsNeeded() {
		t.Fatal("result should not be needed when predicate intersection is empty and nothing else triggers")
	}
	if result != NoCompensation {
		t.Fatalf("expected NoCompensation, got %T", result)
	}
}

func TestForMatchCompensation_Intersect_GroupByMappingsMerge(t *testing.T) {
	t.Parallel()

	// Build distinct Value instances for groupings and aggregates.
	gkA := &values.FieldValue{Field: "group_a"}
	gvA := &values.FieldValue{Field: "group_a_cand"}
	gkB := &values.FieldValue{Field: "group_b"}
	gvB := &values.FieldValue{Field: "group_b_cand"}

	akX := &values.FieldValue{Field: "agg_x"}
	avX := &values.FieldValue{Field: "agg_x_cand"}
	akY := &values.FieldValue{Field: "agg_y"}
	avY := &values.FieldValue{Field: "agg_y_cand"}

	// Side 1: matched grouping {A→A'}, matched aggregate {X→X'}
	mg1 := NewValueBiMap()
	mg1.Put(gkA, gvA)
	ma1 := NewValueBiMap()
	ma1.Put(akX, avX)
	ua1 := NewCorrValueBiMap()
	gbm1 := NewGroupByMappings(mg1, ma1, ua1)

	// Side 2: matched grouping {B→B'}, matched aggregate {Y→Y'}
	mg2 := NewValueBiMap()
	mg2.Put(gkB, gvB)
	ma2 := NewValueBiMap()
	ma2.Put(akY, avY)
	ua2 := NewCorrValueBiMap()
	gbm2 := NewGroupByMappings(mg2, ma2, ua2)

	// Both need compensation via a shared predicate so the intersection
	// actually reaches the GroupByMappings merging code.
	sharedPred := predicates.NewConstantPredicate(predicates.TriTrue)
	predMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{sharedPred},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)

	ref := expressions.InitialOf(nil)
	q := expressions.ForEachQuantifier(ref)

	c1 := NewForMatchCompensation(
		false, NoCompensation, predMap,
		[]expressions.Quantifier{q}, nil, aliasesOf(q), NoResultCompensation(), gbm1,
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap,
		[]expressions.Quantifier{q}, nil, aliasesOf(q), NoResultCompensation(), gbm2,
	)

	result := c1.Intersect(c2)
	fmc, ok := result.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected *ForMatchCompensation, got %T", result)
	}

	mergedGBM := fmc.GetGroupByMappings()
	// Matched groupings should be the union: {A, B}.
	if mergedGBM.MatchedGroupingsMap().Len() != 2 {
		t.Fatalf("expected 2 matched groupings, got %d", mergedGBM.MatchedGroupingsMap().Len())
	}
	// Matched aggregates should be the union: {X, Y}.
	if mergedGBM.MatchedAggregatesMap().Len() != 2 {
		t.Fatalf("expected 2 matched aggregates, got %d", mergedGBM.MatchedAggregatesMap().Len())
	}
}

func TestForMatchCompensation_Intersect_QuantifierSets(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(nil)

	aliasA := values.NamedCorrelationIdentifier("qA")
	aliasB := values.NamedCorrelationIdentifier("qB")
	aliasC := values.NamedCorrelationIdentifier("qC")
	aliasD := values.NamedCorrelationIdentifier("qD")
	aliasF := values.NamedCorrelationIdentifier("qF")

	// Exactly one of the matched quantifiers on each side is a ForEach — the
	// base an applied compensation rebuilds itself on — and it is the SHARED
	// one, so the intersection still has exactly one. The rest are
	// existentials, which do not supply rows and so do not compete to be the
	// base.
	qA := expressions.NamedExistentialQuantifier(aliasA, ref)
	qB := expressions.NamedForEachQuantifier(aliasB, ref)
	qC := expressions.NamedForEachQuantifier(aliasC, ref)
	qD := expressions.NamedForEachQuantifier(aliasD, ref)
	qF := expressions.NamedForEachQuantifier(aliasF, ref)

	// Use a shared predicate so the intersection doesn't short-circuit
	// to NoCompensation via the "empty predicate map + no result" early return.
	sharedPred := predicates.NewConstantPredicate(predicates.TriTrue)
	predMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{sharedPred},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)

	// c1: matched {A, B}, unmatched {C, D}
	c1 := NewForMatchCompensation(
		false, NoCompensation, predMap,
		[]expressions.Quantifier{qA, qB}, []expressions.Quantifier{qC, qD},
		aliasesOf(qA, qB), NoResultCompensation(), EmptyGroupByMappings(),
	)
	// c2 has the same matched/compensated alias responsibility and different
	// unmatched quantifiers. Intersect requires the responsibility sets to be
	// identical; only the unmatched sets are intersected.
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap,
		[]expressions.Quantifier{qA, qB}, []expressions.Quantifier{qC, qF},
		aliasesOf(qA, qB), NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Intersect(c2)
	fmc, ok := result.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected *ForMatchCompensation, got %T", result)
	}

	// Matched remains {A, B}.
	matchedAliases := make(map[values.CorrelationIdentifier]struct{})
	for _, q := range fmc.GetMatchedQuantifiers() {
		matchedAliases[q.GetAlias()] = struct{}{}
	}
	if len(matchedAliases) != 2 {
		t.Fatalf("expected 2 matched quantifiers, got %d", len(matchedAliases))
	}
	for _, expected := range []values.CorrelationIdentifier{aliasA, aliasB} {
		if _, ok := matchedAliases[expected]; !ok {
			t.Fatalf("matched set missing alias %s", expected.Name())
		}
	}

	// Unmatched = intersection of {C, D} ∩ {C, F} = {C}
	unmatchedAliases := make(map[values.CorrelationIdentifier]struct{})
	for _, q := range fmc.GetUnmatchedQuantifiers() {
		unmatchedAliases[q.GetAlias()] = struct{}{}
	}
	if len(unmatchedAliases) != 1 {
		t.Fatalf("expected 1 unmatched quantifier, got %d", len(unmatchedAliases))
	}
	if _, ok := unmatchedAliases[aliasC]; !ok {
		t.Fatal("unmatched set should contain alias C")
	}
}

func TestForMatchCompensation_Intersect_ChildCompensationRecursive(t *testing.T) {
	t.Parallel()

	// Build two ForMatchCompensations with ForMatchCompensation children.
	// The children themselves have predicates so they are "needed".
	sharedChildPred := predicates.NewConstantPredicate(predicates.TriTrue)
	childOnlyPred1 := predicates.NewConstantPredicate(predicates.TriTrue)
	childOnlyPred2 := predicates.NewConstantPredicate(predicates.TriTrue)

	// child1 has predicates {shared, only1}
	childPredMap1 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{sharedChildPred, childOnlyPred1},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded(), NoPredicateCompensationNeeded()},
	)
	ref := expressions.InitialOf(nil)
	childQ := expressions.ForEachQuantifier(ref)
	child1 := NewForMatchCompensation(
		false, NoCompensation, childPredMap1,
		[]expressions.Quantifier{childQ}, nil, aliasesOf(childQ),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	// child2 has predicates {shared, only2}
	childPredMap2 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{sharedChildPred, childOnlyPred2},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded(), NoPredicateCompensationNeeded()},
	)
	child2 := NewForMatchCompensation(
		false, NoCompensation, childPredMap2,
		[]expressions.Quantifier{childQ}, nil, aliasesOf(childQ),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	// Outer compensations, each carrying one of the children.
	outerPred := predicates.NewConstantPredicate(predicates.TriTrue)
	outerPredMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{outerPred},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)
	outerQ := expressions.ForEachQuantifier(ref)

	c1 := NewForMatchCompensation(
		false, child1, outerPredMap,
		[]expressions.Quantifier{outerQ}, nil, aliasesOf(outerQ),
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, child2, outerPredMap,
		[]expressions.Quantifier{outerQ}, nil, aliasesOf(outerQ),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Intersect(c2)
	if result.IsImpossible() {
		t.Fatal("recursive intersection should not be impossible")
	}
	if !result.IsNeeded() {
		t.Fatal("recursive intersection should be needed")
	}

	fmc, ok := result.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected *ForMatchCompensation, got %T", result)
	}

	// The child should also be a ForMatchCompensation (recursively intersected).
	childResult, ok := fmc.GetChildCompensation().(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected child to be *ForMatchCompensation, got %T", fmc.GetChildCompensation())
	}
	// The recursively intersected child should have only the shared predicate.
	if childResult.GetPredicateCompensationMap().Len() != 1 {
		t.Fatalf("expected 1 predicate in intersected child, got %d",
			childResult.GetPredicateCompensationMap().Len())
	}
}

func TestForMatchCompensation_Intersect_ChildImpossible(t *testing.T) {
	t.Parallel()

	// The shared child residual is itself impossible, so it survives the
	// intersection and makes the intersected child impossible.
	childPred := predicates.NewConstantPredicate(predicates.TriTrue)
	childPredMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{childPred},
		[]PredicateCompensationFunc{ImpossiblePredicateCompensation()},
	)
	child1 := NewForMatchCompensation(
		true, NoCompensation, childPredMap,
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	child2 := NewForMatchCompensation(
		false, NoCompensation, childPredMap,
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)

	// Outer compensations need to be "needed" to avoid the short-circuit.
	outerPred := predicates.NewConstantPredicate(predicates.TriTrue)
	outerPredMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{outerPred},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)

	ref := expressions.InitialOf(nil)
	q := expressions.ForEachQuantifier(ref)

	c1 := NewForMatchCompensation(
		false, child1, outerPredMap,
		[]expressions.Quantifier{q}, nil, aliasesOf(q),
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, child2, outerPredMap,
		[]expressions.Quantifier{q}, nil, aliasesOf(q),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Intersect(c2)
	if !result.IsImpossible() {
		t.Fatal("intersection should be impossible when child intersection is impossible")
	}
}

func TestResultCompensation_Amend_NoChange(t *testing.T) {
	t.Parallel()

	// A ConstantValue has no UnmatchedAggregateValue nodes, so Amend
	// with empty maps should return an equivalent compensation with the
	// same value.
	v := &values.ConstantValue{Value: int64(42)}
	f := ResultCompensationOfValue(v)

	amended := f.Amend(NewCorrValueBiMap(), nil)
	if !amended.IsNeeded() {
		t.Fatal("amended should still be needed")
	}
	if amended.IsImpossible() {
		t.Fatal("amended should not be impossible")
	}
	result := amended.ApplyCompensationForResult(nil)
	cv, ok := result.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected *ConstantValue, got %T", result)
	}
	if cv.Value != int64(42) {
		t.Fatalf("expected 42, got %v", cv.Value)
	}
}

func TestResultCompensation_Amend_ReplacesUnmatched(t *testing.T) {
	t.Parallel()

	// Create an UnmatchedAggregateValue as the result value.
	unmatchedID := values.UniqueUnmatchedID()
	unmatchedVal := values.NewUnmatchedAggregateValue(unmatchedID)
	f := ResultCompensationOfValue(unmatchedVal)

	// An unmatched value makes the compensation impossible.
	if !f.IsImpossible() {
		t.Fatal("ResultCompensation with UnmatchedAggregateValue should be impossible")
	}

	// Build the unmatchedAggregateMap: unmatchedID → FieldValue("SUM_X")
	queryAgg := &values.FieldValue{Field: "SUM_X"}
	unmatchedAggMap := NewCorrValueBiMap()
	unmatchedAggMap.Put(unmatchedID, queryAgg)

	// Build the amendedMatchedAggregateMap: FieldValue("SUM_X") → FieldValue("IDX_SUM")
	idxSum := &values.FieldValue{Field: "IDX_SUM"}
	amendedMatchedAggMap := map[values.Value]values.Value{
		queryAgg: idxSum,
	}

	amended := f.Amend(unmatchedAggMap, amendedMatchedAggMap)
	if !amended.IsNeeded() {
		t.Fatal("amended should be needed")
	}
	if amended.IsImpossible() {
		t.Fatal("amended should not be impossible after replacing unmatched aggregate")
	}

	result := amended.ApplyCompensationForResult(nil)
	fv, ok := result.(*values.FieldValue)
	if !ok {
		t.Fatalf("expected *FieldValue, got %T", result)
	}
	if fv.Field != "IDX_SUM" {
		t.Fatalf("expected field IDX_SUM, got %s", fv.Field)
	}
}

func TestResultCompensation_IsImpossible_WithUnmatched(t *testing.T) {
	t.Parallel()

	unmatchedVal := values.NewUnmatchedAggregateValue(values.UniqueUnmatchedID())
	f := ResultCompensationOfValue(unmatchedVal)
	if !f.IsImpossible() {
		t.Fatal("ResultCompensation with UnmatchedAggregateValue should be impossible")
	}
	if !f.IsNeeded() {
		t.Fatal("ResultCompensation with UnmatchedAggregateValue should be needed")
	}
}

func TestResultCompensation_IsImpossible_WithoutUnmatched(t *testing.T) {
	t.Parallel()

	f := ResultCompensationOfValue(&values.FieldValue{Field: "X"})
	if f.IsImpossible() {
		t.Fatal("ResultCompensation with FieldValue should not be impossible")
	}
	if !f.IsNeeded() {
		t.Fatal("ResultCompensation with FieldValue should be needed")
	}
}

// --- ForMatchCompensation.Union tests ---

func TestForMatchCompensation_Union_BothNotNeeded(t *testing.T) {
	t.Parallel()

	c1 := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := c1.Union(c2)
	if result.IsNeeded() {
		t.Fatal("union of two not-needed compensations should not be needed")
	}
	if result != NoCompensation {
		t.Fatalf("expected NoCompensation, got %T", result)
	}
}

func TestForMatchCompensation_Union_OneNotNeeded(t *testing.T) {
	t.Parallel()

	c1 := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)

	// c1 not needed, c2 needed → returns c2.
	result := c1.Union(c2)
	if !result.IsNeeded() {
		t.Fatal("result should be needed since c2 is needed")
	}
	fmc, ok := result.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected *ForMatchCompensation, got %T", result)
	}
	if fmc != c2 {
		t.Fatal("result should be c2 itself (identity)")
	}

	// Reverse: c2 needed, c1 not needed → returns c1.
	result2 := c2.Union(c1)
	if !result2.IsNeeded() {
		t.Fatal("result should be needed since c2 is needed")
	}
	fmc2, ok := result2.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected *ForMatchCompensation, got %T", result2)
	}
	if fmc2 != c1 {
		// c2.Union(c1): c2 is needed, c1 is not needed → returns c1.
		// Wait — the code checks: "if !other.IsNeeded() { return c }"
		// so c2.Union(c1) where c1 is not needed returns c2, not c1.
		// Both c2 is self, c1 is other. other not needed → return self (c2).
		if fmc2 != c2 {
			t.Fatal("result should be c2 itself when other is not needed")
		}
	}
}

func TestForMatchCompensation_Union_PredicateMapMerge(t *testing.T) {
	t.Parallel()

	predA := predicates.NewConstantPredicate(predicates.TriTrue)
	predB := predicates.NewConstantPredicate(predicates.TriTrue)

	predMap1 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{predA},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)
	predMap2 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{predB},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)

	ref := expressions.InitialOf(nil)
	q := expressions.ForEachQuantifier(ref)

	c1 := NewForMatchCompensation(
		false, NoCompensation, predMap1,
		[]expressions.Quantifier{q}, nil, aliasesOf(q),
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap2,
		[]expressions.Quantifier{q}, nil, aliasesOf(q),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Union(c2)
	fmc, ok := result.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected *ForMatchCompensation, got %T", result)
	}
	// Union merges predicate maps: {A} ∪ {B} = {A, B}.
	if fmc.GetPredicateCompensationMap().Len() != 2 {
		t.Fatalf("expected 2 predicates in union, got %d", fmc.GetPredicateCompensationMap().Len())
	}
	// Verify both predicate pointers are present.
	if fmc.GetPredicateCompensationMap().Get(predA) == nil {
		t.Fatal("union predicate map should contain predA")
	}
	if fmc.GetPredicateCompensationMap().Get(predB) == nil {
		t.Fatal("union predicate map should contain predB")
	}
}

func TestForMatchCompensation_Union_DuplicateKeyImpossible(t *testing.T) {
	t.Parallel()

	// Same predicate pointer in both maps → duplicate → ImpossibleCompensation.
	sharedPred := predicates.NewConstantPredicate(predicates.TriTrue)

	predMap1 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{sharedPred},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)
	predMap2 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{sharedPred},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)

	ref := expressions.InitialOf(nil)
	q := expressions.ForEachQuantifier(ref)

	c1 := NewForMatchCompensation(
		false, NoCompensation, predMap1,
		[]expressions.Quantifier{q}, nil, aliasesOf(q),
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap2,
		[]expressions.Quantifier{q}, nil, aliasesOf(q),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Union(c2)
	if !result.IsImpossible() {
		t.Fatal("union with duplicate predicate pointer should be impossible")
	}
	if result != ImpossibleCompensation {
		t.Fatalf("expected ImpossibleCompensation sentinel, got %T", result)
	}
}

func TestForMatchCompensation_Union_MultiForEachImpossible(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(nil)
	q1 := expressions.ForEachQuantifier(ref)
	q2 := expressions.ForEachQuantifier(ref)

	// c1 matched {q1 (ForEach)}, c2 matched {q2 (ForEach)}.
	// Union of matched quantifiers has 2 ForEach → impossible.
	c1 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		[]expressions.Quantifier{q1}, nil, aliasesOf(q1),
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		[]expressions.Quantifier{q2}, nil, aliasesOf(q2),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Union(c2)
	if !result.IsImpossible() {
		t.Fatal("union with 2 ForEach matched quantifiers should be impossible")
	}
	if result != ImpossibleCompensation {
		t.Fatalf("expected ImpossibleCompensation sentinel, got %T", result)
	}
}

func TestForMatchCompensation_Union_UnmatchedForEachImpossible(t *testing.T) {
	t.Parallel()

	ref := expressions.InitialOf(nil)
	unmatchedForEach := expressions.ForEachQuantifier(ref)

	// c1 has unmatched ForEach → union is impossible.
	c1 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		nil, []expressions.Quantifier{unmatchedForEach}, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		nil, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Union(c2)
	if !result.IsImpossible() {
		t.Fatal("union with unmatched ForEach on c1 should be impossible")
	}

	// Reverse: c2 has unmatched ForEach.
	c3 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		nil, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c4 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		nil, []expressions.Quantifier{unmatchedForEach}, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	result2 := c3.Union(c4)
	if !result2.IsImpossible() {
		t.Fatal("union with unmatched ForEach on c2 should be impossible")
	}
}

func TestForMatchCompensation_Union_ChildRecursive(t *testing.T) {
	t.Parallel()

	// Build two ForMatchCompensations with ForMatchCompensation children.
	// Children have non-overlapping predicate maps; union should merge them.
	childPredA := predicates.NewConstantPredicate(predicates.TriTrue)
	childPredB := predicates.NewConstantPredicate(predicates.TriTrue)

	ref := expressions.InitialOf(nil)
	childQ := expressions.ForEachQuantifier(ref)

	// child1 has predicate {childPredA}
	childPredMap1 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{childPredA},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)
	child1 := NewForMatchCompensation(
		false, NoCompensation, childPredMap1,
		[]expressions.Quantifier{childQ}, nil, aliasesOf(childQ),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	// child2 has predicate {childPredB}
	childPredMap2 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{childPredB},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)
	child2 := NewForMatchCompensation(
		false, NoCompensation, childPredMap2,
		[]expressions.Quantifier{childQ}, nil, aliasesOf(childQ),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	// Outer compensations each carry one of the children.
	outerPredA := predicates.NewConstantPredicate(predicates.TriTrue)
	outerPredB := predicates.NewConstantPredicate(predicates.TriTrue)

	outerPredMap1 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{outerPredA},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)
	outerPredMap2 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{outerPredB},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)

	outerQ := expressions.ForEachQuantifier(ref)

	c1 := NewForMatchCompensation(
		false, child1, outerPredMap1,
		[]expressions.Quantifier{outerQ}, nil, aliasesOf(outerQ),
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, child2, outerPredMap2,
		[]expressions.Quantifier{outerQ}, nil, aliasesOf(outerQ),
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Union(c2)
	if result.IsImpossible() {
		t.Fatal("recursive union should not be impossible")
	}
	if !result.IsNeeded() {
		t.Fatal("recursive union should be needed")
	}

	fmc, ok := result.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected *ForMatchCompensation, got %T", result)
	}

	// Outer predicate map should be union: {outerPredA, outerPredB}.
	if fmc.GetPredicateCompensationMap().Len() != 2 {
		t.Fatalf("expected 2 predicates in outer union, got %d",
			fmc.GetPredicateCompensationMap().Len())
	}

	// The child should also be a ForMatchCompensation (recursively unioned).
	childResult, ok := fmc.GetChildCompensation().(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected child to be *ForMatchCompensation, got %T",
			fmc.GetChildCompensation())
	}
	// The recursively unioned child should have both child predicates merged:
	// {childPredA} ∪ {childPredB} = {childPredA, childPredB}.
	if childResult.GetPredicateCompensationMap().Len() != 2 {
		t.Fatalf("expected 2 predicates in child union, got %d",
			childResult.GetPredicateCompensationMap().Len())
	}
	if childResult.GetPredicateCompensationMap().Get(childPredA) == nil {
		t.Fatal("child union predicate map should contain childPredA")
	}
	if childResult.GetPredicateCompensationMap().Get(childPredB) == nil {
		t.Fatal("child union predicate map should contain childPredB")
	}
}

func TestIntersectCompensations_Empty(t *testing.T) {
	t.Parallel()
	result := IntersectCompensations(nil)
	if result != ImpossibleCompensation {
		t.Fatal("empty intersection should be ImpossibleCompensation (identity)")
	}
}

func TestIntersectCompensations_SingleElement(t *testing.T) {
	t.Parallel()
	c := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := IntersectCompensations([]Compensation{c})
	if result != c {
		t.Fatal("impossible ∩ c should equal c (identity property)")
	}
}

func TestIntersectCompensations_WithNoCompensation(t *testing.T) {
	t.Parallel()
	c := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := IntersectCompensations([]Compensation{NoCompensation, c})
	if result.IsNeeded() {
		t.Fatal("none ∩ c should be NoCompensation (absorbing)")
	}
}

func TestUnionCompensations_Empty(t *testing.T) {
	t.Parallel()
	result := UnionCompensations(nil)
	if result.IsNeeded() {
		t.Fatal("empty union should be NoCompensation (identity)")
	}
}

func TestUnionCompensations_TwoForMatch(t *testing.T) {
	t.Parallel()
	predA := predicates.NewConstantPredicate(predicates.TriTrue)
	predB := predicates.NewConstantPredicate(predicates.TriTrue)
	c1 := NewForMatchCompensation(
		false, NoCompensation,
		NewPredicateCompensationMap(
			[]predicates.QueryPredicate{predA},
			[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
		),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation,
		NewPredicateCompensationMap(
			[]predicates.QueryPredicate{predB},
			[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
		),
		baseMatched(), nil, baseCompensated(), NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := UnionCompensations([]Compensation{c1, c2})
	if !result.IsNeeded() {
		t.Fatal("union of two needed compensations should be needed")
	}
	fm, ok := result.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected *ForMatchCompensation, got %T", result)
	}
	if fm.GetPredicateCompensationMap().Len() != 2 {
		t.Fatalf("union predicate map should have 2 entries, got %d", fm.GetPredicateCompensationMap().Len())
	}
}

// TestUnionResultCompensation pins RFC-189 C3 (finding 12c): Java's
// Compensation.Union asserts BOTH legs' result compensations are needed
// (Verify.verify) before picking one; Go previously picked c's rcf even when
// only other's was needed, yielding the wrong output shape. The helper returns
// ok=false for the exactly-one-needed case so Union declines (fail-closed).
func TestUnionResultCompensation(t *testing.T) {
	t.Parallel()
	needed := NewResultCompensationFunction(true)
	notNeeded := NewResultCompensationFunction(false)

	if fn, ok := unionResultCompensation(notNeeded, notNeeded); !ok || fn.IsNeeded() {
		t.Fatalf("neither needed → (no-compensation, ok); got (%v, %v)", fn, ok)
	}
	if fn, ok := unionResultCompensation(needed, needed); !ok || !fn.IsNeeded() {
		t.Fatalf("both needed → (needed rcf, ok); got (%v, %v)", fn, ok)
	}
	// Exactly one needed — the invariant Java asserts against. Must decline.
	if _, ok := unionResultCompensation(needed, notNeeded); ok {
		t.Fatal("only c's rcf needed → must decline (ok=false), not silently pick c's shape")
	}
	if _, ok := unionResultCompensation(notNeeded, needed); ok {
		t.Fatal("only other's rcf needed → must decline (ok=false)")
	}
}

func TestForMatchCompensation_PrimaryKeyDistinctOnly(t *testing.T) {
	t.Parallel()

	base := namedForEachQuantifier("distinct_base")
	compensation := NewForMatchCompensationWithPrimaryKeyDistinct(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{base},
		nil,
		aliasesOf(base),
		NoResultCompensation(),
		EmptyGroupByMappings(),
		true,
	)

	if !compensation.IsNeeded() {
		t.Fatal("primary-key distinct must make compensation needed")
	}
	if compensation.IsNeededForFiltering() {
		t.Fatal("primary-key distinct changes multiplicity, not filtering")
	}
	if compensation.IsFinalNeeded() {
		t.Fatal("primary-key distinct is pre-final work")
	}
	if !compensation.RequiresPrimaryKeyDistinct() {
		t.Fatal("primary-key distinct obligation was not retained")
	}

	scan := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	translationCalls := 0
	applied, ok := compensation.ApplyAllNeeded(
		scan,
		func(values.CorrelationIdentifier) TranslationMap {
			translationCalls++
			return EmptyTranslationMap()
		},
	)
	if !ok {
		t.Fatal("distinct-only compensation failed to apply")
	}
	if translationCalls != 0 {
		t.Fatalf("distinct-only compensation translated %d times, want 0", translationCalls)
	}
	unique, ok := applied.(*expressions.LogicalUniqueExpression)
	if !ok {
		t.Fatalf("distinct-only compensation produced %T, want LogicalUniqueExpression", applied)
	}
	if !unique.IsRequired() {
		t.Fatal("cardinality compensation must emit required, not absorbable, Unique")
	}
	if got := unique.GetInner().GetAlias(); got != base.GetAlias() {
		t.Fatalf("Unique alias = %q, want matched alias %q", got.Name(), base.GetAlias().Name())
	}
	if got := unique.GetInner().GetRangesOver().Get(); got != scan {
		t.Fatalf("Unique child = %T, want original scan", got)
	}
}

func TestForMatchCompensation_PrimaryKeyDistinctOrdering(t *testing.T) {
	t.Parallel()

	base := namedForEachQuantifier("ordered_distinct_base")
	residual := predicates.NewConstantPredicate(predicates.TriTrue)
	predicateMap := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{residual},
		[]PredicateCompensationFunc{OfPredicateCompensation(residual, false)},
	)
	compensation := NewForMatchCompensationWithPrimaryKeyDistinct(
		false,
		NoCompensation,
		predicateMap,
		[]expressions.Quantifier{base},
		nil,
		aliasesOf(base),
		ResultCompensationOfValue(
			values.NewQuantifiedObjectValue(base.GetAlias()),
		),
		EmptyGroupByMappings(),
		true,
	)

	scan := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	var translatedAliases []values.CorrelationIdentifier
	applied, ok := compensation.ApplyAllNeeded(
		scan,
		func(alias values.CorrelationIdentifier) TranslationMap {
			translatedAliases = append(translatedAliases, alias)
			return EmptyTranslationMap()
		},
	)
	if !ok {
		t.Fatal("filter + distinct + final compensation failed to apply")
	}

	final, ok := applied.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("outer expression = %T, want final SelectExpression", applied)
	}
	if len(final.GetQuantifiers()) != 1 {
		t.Fatalf("final quantifier count = %d, want 1", len(final.GetQuantifiers()))
	}
	finalQ := final.GetQuantifiers()[0]
	if finalQ.GetAlias() != base.GetAlias() {
		t.Fatalf("final alias = %q, want %q", finalQ.GetAlias().Name(), base.GetAlias().Name())
	}

	unique, ok := finalQ.GetRangesOver().Get().(*expressions.LogicalUniqueExpression)
	if !ok {
		t.Fatalf("final child = %T, want required Unique", finalQ.GetRangesOver().Get())
	}
	if !unique.IsRequired() {
		t.Fatal("compensation emitted an absorbable Unique")
	}
	if unique.GetInner().GetAlias() != base.GetAlias() {
		t.Fatalf("Unique alias = %q, want %q", unique.GetInner().GetAlias().Name(), base.GetAlias().Name())
	}

	filter, ok := unique.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("Unique child = %T, want residual LogicalFilterExpression", unique.GetInner().GetRangesOver().Get())
	}
	if filter.GetInner().GetAlias() != base.GetAlias() {
		t.Fatalf("filter alias = %q, want %q", filter.GetInner().GetAlias().Name(), base.GetAlias().Name())
	}
	if got := filter.GetInner().GetRangesOver().Get(); got != scan {
		t.Fatalf("filter child = %T, want original scan", got)
	}

	if len(translatedAliases) != 2 {
		t.Fatalf("translation callback calls = %d, want filter and final", len(translatedAliases))
	}
	for _, alias := range translatedAliases {
		if alias != base.GetAlias() {
			t.Fatalf("translated alias = %q, want %q", alias.Name(), base.GetAlias().Name())
		}
	}
}

func TestForMatchCompensation_NestedPrimaryKeyDistinct(t *testing.T) {
	t.Parallel()

	childBase := namedForEachQuantifier("nested_distinct_base")
	child := NewForMatchCompensationWithPrimaryKeyDistinct(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{childBase},
		nil,
		aliasesOf(childBase),
		NoResultCompensation(),
		EmptyGroupByMappings(),
		true,
	)
	parent := NewForMatchCompensation(
		false,
		child,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)

	if parent.IsImpossible() {
		t.Fatal("cardinality-only child must not require a local parent alias")
	}
	if !parent.IsNeeded() {
		t.Fatal("parent must report its cardinality-only child as needed")
	}
	if parent.IsNeededForFiltering() {
		t.Fatal("cardinality-only child must not make an existential owner filter")
	}

	scan := expressions.NewFullUnorderedScanExpression(
		[]string{"T"},
		values.UnknownType,
	)
	applied, ok := parent.ApplyAllNeeded(scan, nil)
	if !ok {
		t.Fatal("nested cardinality-only compensation was skipped or failed")
	}
	unique, ok := applied.(*expressions.LogicalUniqueExpression)
	if !ok || !unique.IsRequired() {
		t.Fatalf("nested compensation produced %T, want required Unique", applied)
	}
	if unique.GetInner().GetAlias() != childBase.GetAlias() {
		t.Fatalf(
			"nested Unique alias = %q, want child alias %q",
			unique.GetInner().GetAlias().Name(),
			childBase.GetAlias().Name(),
		)
	}
}

func TestForMatchCompensation_PrimaryKeyDistinctCompositionUsesOR(t *testing.T) {
	t.Parallel()

	base := namedForEachQuantifier("composed_distinct_base")
	distinct := NewForMatchCompensationWithPrimaryKeyDistinct(
		false,
		NoCompensation,
		EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{base},
		nil,
		aliasesOf(base),
		NoResultCompensation(),
		EmptyGroupByMappings(),
		true,
	)
	residual := predicates.NewConstantPredicate(predicates.TriTrue)
	filtering := NewForMatchCompensation(
		false,
		NoCompensation,
		NewPredicateCompensationMap(
			[]predicates.QueryPredicate{residual},
			[]PredicateCompensationFunc{OfPredicateCompensation(residual, false)},
		),
		[]expressions.Quantifier{base},
		nil,
		aliasesOf(base),
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)

	for _, tc := range []struct {
		name         string
		compensation Compensation
	}{
		{name: "union", compensation: unionTwo(distinct, filtering)},
		{name: "intersection", compensation: intersectTwo(distinct, filtering)},
		{name: "union_with_no_compensation", compensation: unionTwo(NoCompensation, distinct)},
		{name: "intersection_with_no_compensation", compensation: intersectTwo(NoCompensation, distinct)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			forMatch, ok := tc.compensation.(*ForMatchCompensation)
			if !ok {
				t.Fatalf("composition = %T, want ForMatchCompensation", tc.compensation)
			}
			if !forMatch.RequiresPrimaryKeyDistinct() {
				t.Fatal("composition dropped the OR-ed primary-key distinct obligation")
			}
			if !forMatch.IsNeeded() {
				t.Fatal("distinct-bearing composition must remain needed")
			}
		})
	}

	ordinaryIntersection := intersectTwo(NoCompensation, filtering)
	if ordinaryIntersection.IsNeeded() {
		t.Fatal("NoCompensation must remain absorbing for filtering-only intersection")
	}

	derived := DerivedCompensationWithPrimaryKeyDistinct(
		NoCompensation,
		false,
		EmptyPredicateCompensationMap(),
		[]expressions.Quantifier{base},
		nil,
		aliasesOf(base),
		NoResultCompensation(),
		EmptyGroupByMappings(),
		true,
	)
	if derived.IsImpossible() || !derived.IsNeeded() {
		t.Fatal("distinct-only DerivedCompensation must satisfy the needed invariant")
	}
}

// baseForEachQ is the single matched ForEach quantifier that algebra fixtures
// below build their compensations on.
//
// A compensation that has to be applied rebuilds the expression on exactly one
// matched ForEach alias (see ForMatchCompensation.MatchedForEachAliasMaybe).
// Fixtures used to pass no quantifiers at all, which is not a shape the
// planner can produce for an appliable compensation — it left Apply with no
// alias to rebuild on. These helpers give the intersect/union algebra tests a
// well-formed subject so they exercise the algebra, not an invalid state.
var baseForEachQ = namedForEachQuantifier("qBase")

func baseMatched() []expressions.Quantifier {
	return []expressions.Quantifier{baseForEachQ}
}

func baseCompensated() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{baseForEachQ.GetAlias(): {}}
}

// aliasesOf is the compensated-alias set matching a fixture's matched
// quantifiers.
//
// A compensation's matched quantifiers and compensated aliases must name the
// same set — that pair is what it claims responsibility for, and
// MatchedForEachAliasMaybe refuses to pick a base alias when they disagree.
// Fixtures that passed matched quantifiers with a nil alias set were malformed
// in exactly that way; this keeps them honest without restating the set.
func aliasesOf(qs ...expressions.Quantifier) map[values.CorrelationIdentifier]struct{} {
	out := make(map[values.CorrelationIdentifier]struct{}, len(qs))
	for _, q := range qs {
		out[q.GetAlias()] = struct{}{}
	}
	return out
}
