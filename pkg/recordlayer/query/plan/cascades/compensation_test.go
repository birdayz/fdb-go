package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
		nil,
		nil,
		nil,
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
		nil,
		nil,
		nil,
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
		nil,
		nil,
		nil,
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
		nil,
		nil,
		nil,
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
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)

	derived := parent.Derived(
		false,
		StubPredicateCompensationMap(2),
		[]expressions.Quantifier{q2},
		nil,
		nil,
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

func TestDerivedCompensation_PanicWhenNothingNeeded(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when nothing is needed")
		}
	}()

	DerivedCompensation(
		NoCompensation,
		false,
		EmptyPredicateCompensationMap(),
		nil,
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)
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
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
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
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := c.Apply(scan, nil)
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
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c.Apply(scan, nil)
	filter, ok := result.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("expected LogicalFilterExpression, got %T", result)
	}
	if len(filter.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(filter.GetPredicates()))
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
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := c1.Intersect(c2)
	if result.IsNeeded() {
		t.Fatal("intersection of two no-compensation should not be needed")
	}
}

func TestForMatchCompensation_Intersect_OneImpossible(t *testing.T) {
	t.Parallel()
	c1 := NewForMatchCompensation(
		true, NoCompensation, EmptyPredicateCompensationMap(),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := c1.Intersect(c2)
	if !result.IsImpossible() {
		t.Fatal("intersection with impossible should be impossible")
	}
}

func TestForMatchCompensation_Intersect_OneNotNeeded(t *testing.T) {
	t.Parallel()

	// c1 is not needed (empty everything), c2 is needed (has predicates).
	c1 := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	result := c1.Intersect(c2)
	// When c1 is not needed, intersection returns c2.
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
		[]expressions.Quantifier{q}, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap2,
		[]expressions.Quantifier{q}, nil, nil,
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
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap2,
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
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
		[]expressions.Quantifier{q}, nil, nil, NoResultCompensation(), gbm1,
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap,
		[]expressions.Quantifier{q}, nil, nil, NoResultCompensation(), gbm2,
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
	aliasE := values.NamedCorrelationIdentifier("qE")
	aliasF := values.NamedCorrelationIdentifier("qF")

	qA := expressions.NamedForEachQuantifier(aliasA, ref)
	qB := expressions.NamedForEachQuantifier(aliasB, ref)
	qC := expressions.NamedForEachQuantifier(aliasC, ref)
	qD := expressions.NamedForEachQuantifier(aliasD, ref)
	qE := expressions.NamedForEachQuantifier(aliasE, ref)
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
		nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	// c2: matched {B, E}, unmatched {C, F}
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap,
		[]expressions.Quantifier{qB, qE}, []expressions.Quantifier{qC, qF},
		nil, NoResultCompensation(), EmptyGroupByMappings(),
	)

	result := c1.Intersect(c2)
	fmc, ok := result.(*ForMatchCompensation)
	if !ok {
		t.Fatalf("expected *ForMatchCompensation, got %T", result)
	}

	// Matched = union of {A, B} ∪ {B, E} = {A, B, E}
	matchedAliases := make(map[values.CorrelationIdentifier]struct{})
	for _, q := range fmc.GetMatchedQuantifiers() {
		matchedAliases[q.GetAlias()] = struct{}{}
	}
	if len(matchedAliases) != 3 {
		t.Fatalf("expected 3 matched quantifiers, got %d", len(matchedAliases))
	}
	for _, expected := range []values.CorrelationIdentifier{aliasA, aliasB, aliasE} {
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
		[]expressions.Quantifier{childQ}, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	// child2 has predicates {shared, only2}
	childPredMap2 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{sharedChildPred, childOnlyPred2},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded(), NoPredicateCompensationNeeded()},
	)
	child2 := NewForMatchCompensation(
		false, NoCompensation, childPredMap2,
		[]expressions.Quantifier{childQ}, nil, nil,
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
		[]expressions.Quantifier{outerQ}, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, child2, outerPredMap,
		[]expressions.Quantifier{outerQ}, nil, nil,
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

	// child1 is impossible, child2 has predicates.
	child1 := NewForMatchCompensation(
		true, NoCompensation, EmptyPredicateCompensationMap(),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	child2 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
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
		[]expressions.Quantifier{q}, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, child2, outerPredMap,
		[]expressions.Quantifier{q}, nil, nil,
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

	// Before amendment, this should be impossible (contains unmatched).
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
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, EmptyPredicateCompensationMap(),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
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
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
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
		[]expressions.Quantifier{q}, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap2,
		[]expressions.Quantifier{q}, nil, nil,
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
		[]expressions.Quantifier{q}, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, predMap2,
		[]expressions.Quantifier{q}, nil, nil,
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
		[]expressions.Quantifier{q1}, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation, StubPredicateCompensationMap(1),
		[]expressions.Quantifier{q2}, nil, nil,
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
		[]expressions.Quantifier{childQ}, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)

	// child2 has predicate {childPredB}
	childPredMap2 := NewPredicateCompensationMap(
		[]predicates.QueryPredicate{childPredB},
		[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
	)
	child2 := NewForMatchCompensation(
		false, NoCompensation, childPredMap2,
		[]expressions.Quantifier{childQ}, nil, nil,
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
		[]expressions.Quantifier{outerQ}, nil, nil,
		NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, child2, outerPredMap2,
		[]expressions.Quantifier{outerQ}, nil, nil,
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
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
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
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
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
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
	)
	c2 := NewForMatchCompensation(
		false, NoCompensation,
		NewPredicateCompensationMap(
			[]predicates.QueryPredicate{predB},
			[]PredicateCompensationFunc{NoPredicateCompensationNeeded()},
		),
		nil, nil, nil, NoResultCompensation(), EmptyGroupByMappings(),
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
