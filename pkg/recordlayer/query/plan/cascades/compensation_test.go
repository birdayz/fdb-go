package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
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

	predMap := NewPredicateCompensationMap(1)
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
		NewPredicateCompensationMap(1),
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
		NewPredicateCompensationMap(1),
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
		NewPredicateCompensationMap(2),
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
		NewPredicateCompensationMap(1),
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
		NewPredicateCompensationMap(1),
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
		NewPredicateCompensationMap(1),
		[]expressions.Quantifier{q1},
		nil,
		nil,
		NoResultCompensation(),
		EmptyGroupByMappings(),
	)

	derived := parent.Derived(
		false,
		NewPredicateCompensationMap(2),
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
		NewPredicateCompensationMap(1),
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

	withEntries := NewPredicateCompensationMap(3)
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
