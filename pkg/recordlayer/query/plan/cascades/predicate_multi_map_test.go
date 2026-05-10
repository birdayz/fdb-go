package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestMappingKind_Values(t *testing.T) {
	t.Parallel()

	if MappingRegularImpliesCandidate != 0 {
		t.Fatalf("MappingRegularImpliesCandidate = %d, want 0", MappingRegularImpliesCandidate)
	}
	if MappingOrTermImpliesCandidate != 1 {
		t.Fatalf("MappingOrTermImpliesCandidate = %d, want 1", MappingOrTermImpliesCandidate)
	}
}

func TestMappingKind_String(t *testing.T) {
	t.Parallel()

	if got := MappingRegularImpliesCandidate.String(); got != "REGULAR_IMPLIES_CANDIDATE" {
		t.Fatalf("String() = %q, want REGULAR_IMPLIES_CANDIDATE", got)
	}
	if got := MappingOrTermImpliesCandidate.String(); got != "OR_TERM_IMPLIES_CANDIDATE" {
		t.Fatalf("String() = %q, want OR_TERM_IMPLIES_CANDIDATE", got)
	}
	if got := MappingKind(99).String(); got != "MappingKind(99)" {
		t.Fatalf("String() = %q, want MappingKind(99)", got)
	}
}

func TestMappingKey(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)
	mk := NewMappingKey(qp, cp, MappingRegularImpliesCandidate)

	if mk.GetOriginalQueryPredicate() != qp {
		t.Fatal("original query predicate mismatch")
	}
	if mk.GetCandidatePredicate() != cp {
		t.Fatal("candidate predicate mismatch")
	}
	if mk.GetMappingKind() != MappingRegularImpliesCandidate {
		t.Fatal("mapping kind mismatch")
	}
}

func TestPredicateMapping_Construction(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)
	translated := predicates.NewConstantPredicate(predicates.TriTrue)

	alias := values.NamedCorrelationIdentifier("p1")
	cr := predicates.EmptyComparisonRange()
	constraint := &QueryPlanConstraint{}

	mapping := RegularMappingBuilder(qp, translated, cp).
		SetParameterAlias(alias).
		SetComparisonRange(cr).
		SetConstraint(constraint).
		Build()

	if mapping.GetOriginalQueryPredicate() != qp {
		t.Fatal("original query predicate mismatch")
	}
	if mapping.GetCandidatePredicate() != cp {
		t.Fatal("candidate predicate mismatch")
	}
	if mapping.GetMappingKind() != MappingRegularImpliesCandidate {
		t.Fatal("mapping kind should be regular")
	}
	if mapping.GetParameterAlias() == nil {
		t.Fatal("parameter alias should not be nil")
	}
	if *mapping.GetParameterAlias() != alias {
		t.Fatal("parameter alias mismatch")
	}
	if mapping.GetComparisonRange() != cr {
		t.Fatal("comparison range mismatch")
	}
	if mapping.GetConstraint() != constraint {
		t.Fatal("constraint mismatch")
	}
	if mapping.GetTranslatedQueryPredicate() != translated {
		t.Fatal("translated query predicate mismatch")
	}
	if mapping.GetMappingKey().GetMappingKind() != MappingRegularImpliesCandidate {
		t.Fatal("mapping key kind mismatch")
	}
}

func TestPredicateMapping_OrTerm(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)

	mapping := OrTermMappingBuilder(qp, qp, cp).Build()

	if mapping.GetMappingKind() != MappingOrTermImpliesCandidate {
		t.Fatal("mapping kind should be OR_TERM")
	}
}

func TestPredicateMapping_Sargable(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)
	alias := values.NamedCorrelationIdentifier("p1")
	cr := predicates.EmptyComparisonRange()

	mapping := RegularMappingBuilder(qp, qp, cp).
		SetSargable(alias, cr).
		Build()

	if mapping.GetParameterAlias() == nil || *mapping.GetParameterAlias() != alias {
		t.Fatal("sargable alias mismatch")
	}
	if mapping.GetComparisonRange() != cr {
		t.Fatal("sargable range mismatch")
	}
}

func TestPredicateMapping_WithTranslatedQueryPredicate(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)
	translated1 := predicates.NewConstantPredicate(predicates.TriTrue)
	translated2 := predicates.NewConstantPredicate(predicates.TriFalse)

	mapping := RegularMappingBuilder(qp, translated1, cp).Build()
	mapping2 := mapping.WithTranslatedQueryPredicate(translated2)

	if mapping.GetTranslatedQueryPredicate() != translated1 {
		t.Fatal("original should still have translated1")
	}
	if mapping2.GetTranslatedQueryPredicate() != translated2 {
		t.Fatal("new mapping should have translated2")
	}
	// Original and candidate predicates should be preserved.
	if mapping2.GetOriginalQueryPredicate() != qp {
		t.Fatal("original query predicate should be preserved")
	}
	if mapping2.GetCandidatePredicate() != cp {
		t.Fatal("candidate predicate should be preserved")
	}
}

func TestPredicateMapping_DefaultCompensation(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)

	mapping := RegularMappingBuilder(qp, qp, cp).Build()

	comp := mapping.GetPredicateCompensation()
	if comp == nil {
		t.Fatal("default compensation should not be nil")
	}
	fn := comp(nil, nil)
	if fn.IsNeeded() {
		t.Fatal("default compensation should not be needed")
	}
	if fn.IsImpossible() {
		t.Fatal("default compensation should not be impossible")
	}
}

func TestPredicateCompensationFunc_Sentinels(t *testing.T) {
	t.Parallel()

	no := NoPredicateCompensationNeeded()
	if no.IsNeeded() {
		t.Fatal("NoPredicateCompensationNeeded should not be needed")
	}
	if no.IsImpossible() {
		t.Fatal("NoPredicateCompensationNeeded should not be impossible")
	}

	imp := ImpossiblePredicateCompensation()
	if !imp.IsNeeded() {
		t.Fatal("ImpossiblePredicateCompensation should be needed")
	}
	if !imp.IsImpossible() {
		t.Fatal("ImpossiblePredicateCompensation should be impossible")
	}
}

func TestPredicateMultiMap_ZeroValue(t *testing.T) {
	t.Parallel()

	// Zero value must be valid and empty — existing tests rely on
	// &PredicateMultiMap{}.
	m := &PredicateMultiMap{}
	if m.Size() != 0 {
		t.Fatalf("Size() = %d, want 0", m.Size())
	}
	if m.PredicateCount() != 0 {
		t.Fatalf("PredicateCount() = %d, want 0", m.PredicateCount())
	}
	if got := m.Get(predicates.NewConstantPredicate(predicates.TriTrue)); got != nil {
		t.Fatal("Get on empty map should return nil")
	}
	if got := m.KeySet(); got != nil {
		t.Fatal("KeySet on empty map should return nil")
	}
	if got := m.Values(); got != nil {
		t.Fatal("Values on empty map should return nil")
	}
	if got := m.Entries(); got != nil {
		t.Fatal("Entries on empty map should return nil")
	}
}

func TestPredicateMultiMap_NilSafe(t *testing.T) {
	t.Parallel()

	var m *PredicateMultiMap
	if m.Size() != 0 {
		t.Fatal("nil map Size should be 0")
	}
	if m.PredicateCount() != 0 {
		t.Fatal("nil map PredicateCount should be 0")
	}
	if m.Get(predicates.NewConstantPredicate(predicates.TriTrue)) != nil {
		t.Fatal("nil map Get should return nil")
	}
}

func TestPredicateMultiMap_SingleMapping(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)

	mapping := RegularMappingBuilder(qp, qp, cp).Build()

	builder := NewPredicateMultiMapBuilder()
	if !builder.Put(qp, mapping) {
		t.Fatal("Put should return true for new mapping")
	}

	m := builder.Build()

	if m.Size() != 1 {
		t.Fatalf("Size() = %d, want 1", m.Size())
	}
	if m.PredicateCount() != 1 {
		t.Fatalf("PredicateCount() = %d, want 1", m.PredicateCount())
	}

	got := m.Get(qp)
	if len(got) != 1 {
		t.Fatalf("Get returned %d mappings, want 1", len(got))
	}
	if got[0].GetCandidatePredicate() != cp {
		t.Fatal("mapping candidate predicate mismatch")
	}
}

func TestPredicateMultiMap_MultipleMappingsPerPredicate(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp1 := predicates.NewConstantPredicate(predicates.TriFalse)
	cp2 := predicates.NewConstantPredicate(predicates.TriTrue)

	m1 := RegularMappingBuilder(qp, qp, cp1).Build()
	m2 := RegularMappingBuilder(qp, qp, cp2).Build()

	builder := NewPredicateMultiMapBuilder()
	builder.Put(qp, m1)
	builder.Put(qp, m2)

	m := builder.Build()

	if m.Size() != 2 {
		t.Fatalf("Size() = %d, want 2", m.Size())
	}
	if m.PredicateCount() != 1 {
		t.Fatalf("PredicateCount() = %d, want 1", m.PredicateCount())
	}

	got := m.Get(qp)
	if len(got) != 2 {
		t.Fatalf("Get returned %d mappings, want 2", len(got))
	}
}

func TestPredicateMultiMap_MultiplePredicates(t *testing.T) {
	t.Parallel()

	qp1 := predicates.NewConstantPredicate(predicates.TriTrue)
	qp2 := predicates.NewConstantPredicate(predicates.TriFalse)
	cp1 := predicates.NewConstantPredicate(predicates.TriTrue)
	cp2 := predicates.NewConstantPredicate(predicates.TriFalse)

	m1 := RegularMappingBuilder(qp1, qp1, cp1).Build()
	m2 := RegularMappingBuilder(qp2, qp2, cp2).Build()

	builder := NewPredicateMultiMapBuilder()
	builder.Put(qp1, m1)
	builder.Put(qp2, m2)

	m := builder.Build()

	if m.Size() != 2 {
		t.Fatalf("Size() = %d, want 2", m.Size())
	}
	if m.PredicateCount() != 2 {
		t.Fatalf("PredicateCount() = %d, want 2", m.PredicateCount())
	}

	// KeySet preserves insertion order.
	keys := m.KeySet()
	if len(keys) != 2 {
		t.Fatalf("KeySet len = %d, want 2", len(keys))
	}
	if keys[0] != qp1 || keys[1] != qp2 {
		t.Fatal("KeySet order mismatch")
	}

	// Values returns all mappings.
	vals := m.Values()
	if len(vals) != 2 {
		t.Fatalf("Values len = %d, want 2", len(vals))
	}

	// Entries returns all pairs.
	entries := m.Entries()
	if len(entries) != 2 {
		t.Fatalf("Entries len = %d, want 2", len(entries))
	}
}

func TestPredicateMultiMap_DuplicateMapping(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)

	mapping := RegularMappingBuilder(qp, qp, cp).Build()

	builder := NewPredicateMultiMapBuilder()
	if !builder.Put(qp, mapping) {
		t.Fatal("first Put should return true")
	}
	// Same mapping pointer again — should be a no-op.
	if builder.Put(qp, mapping) {
		t.Fatal("duplicate Put should return false")
	}

	m := builder.Build()
	if m.Size() != 1 {
		t.Fatalf("Size() = %d, want 1 (no duplicates)", m.Size())
	}
}

func TestPredicateMultiMap_IdentityKeying(t *testing.T) {
	t.Parallel()

	// Two different pointers with the same Explain() should be treated
	// as different predicates (identity semantics).
	qp1 := predicates.NewConstantPredicate(predicates.TriTrue)
	qp2 := predicates.NewConstantPredicate(predicates.TriTrue) // same Explain(), different pointer
	cp1 := predicates.NewConstantPredicate(predicates.TriFalse)
	cp2 := predicates.NewConstantPredicate(predicates.TriFalse)

	m1 := RegularMappingBuilder(qp1, qp1, cp1).Build()
	m2 := RegularMappingBuilder(qp2, qp2, cp2).Build()

	builder := NewPredicateMultiMapBuilder()
	builder.Put(qp1, m1)
	builder.Put(qp2, m2)

	m := builder.Build()

	if m.PredicateCount() != 2 {
		t.Fatalf("PredicateCount() = %d, want 2 (identity keying)", m.PredicateCount())
	}
	if m.Get(qp1) == nil {
		t.Fatal("Get(qp1) should not be nil")
	}
	if m.Get(qp2) == nil {
		t.Fatal("Get(qp2) should not be nil")
	}
}

func TestPredicateMultiMap_ConflictDetection(t *testing.T) {
	t.Parallel()

	qp1 := predicates.NewConstantPredicate(predicates.TriTrue)
	qp2 := predicates.NewConstantPredicate(predicates.TriFalse)
	// Same candidate predicate mapped from two different query predicates.
	cp := predicates.NewConstantPredicate(predicates.TriTrue)

	m1 := RegularMappingBuilder(qp1, qp1, cp).Build()
	m2 := RegularMappingBuilder(qp2, qp2, cp).Build()

	builder := NewPredicateMultiMapBuilder()
	builder.Put(qp1, m1)
	builder.Put(qp2, m2)

	result := builder.BuildMaybe()
	if result != nil {
		t.Fatal("BuildMaybe should return nil for conflicting mappings")
	}
}

func TestPredicateMultiMap_ConflictPanics(t *testing.T) {
	t.Parallel()

	qp1 := predicates.NewConstantPredicate(predicates.TriTrue)
	qp2 := predicates.NewConstantPredicate(predicates.TriFalse)
	cp := predicates.NewConstantPredicate(predicates.TriTrue)

	m1 := RegularMappingBuilder(qp1, qp1, cp).Build()
	m2 := RegularMappingBuilder(qp2, qp2, cp).Build()

	builder := NewPredicateMultiMapBuilder()
	builder.Put(qp1, m1)
	builder.Put(qp2, m2)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Build should panic on conflicting mappings")
		}
	}()
	builder.Build()
}

func TestPredicateMultiMap_PutAll(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)
	mapping := RegularMappingBuilder(qp, qp, cp).Build()

	builder1 := NewPredicateMultiMapBuilder()
	builder1.Put(qp, mapping)
	m1 := builder1.Build()

	builder2 := NewPredicateMultiMapBuilder()
	if !builder2.PutAll(m1) {
		t.Fatal("PutAll should return true when adding entries")
	}
	m2 := builder2.Build()

	if m2.Size() != 1 {
		t.Fatalf("PutAll result Size() = %d, want 1", m2.Size())
	}
}

func TestPredicateMultiMap_PutAllMappings(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp1 := predicates.NewConstantPredicate(predicates.TriFalse)
	cp2 := predicates.NewConstantPredicate(predicates.TriTrue)

	mappings := []*PredicateMapping{
		RegularMappingBuilder(qp, qp, cp1).Build(),
		RegularMappingBuilder(qp, qp, cp2).Build(),
	}

	builder := NewPredicateMultiMapBuilder()
	if !builder.PutAllMappings(qp, mappings) {
		t.Fatal("PutAllMappings should return true")
	}

	m := builder.Build()
	if m.Size() != 2 {
		t.Fatalf("Size() = %d, want 2", m.Size())
	}
}

func TestPredicateMap_SingleMapping(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)

	mapping := RegularMappingBuilder(qp, qp, cp).Build()

	builder := NewPredicateMapBuilder()
	builder.Put(qp, mapping)

	m := builder.Build()

	got, ok := m.GetMappingOptional(qp)
	if !ok {
		t.Fatal("GetMappingOptional should find the mapping")
	}
	if got.GetCandidatePredicate() != cp {
		t.Fatal("mapping candidate mismatch")
	}
}

func TestPredicateMap_EmptyGet(t *testing.T) {
	t.Parallel()

	m := EmptyPredicateMap()
	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	_, ok := m.GetMappingOptional(qp)
	if ok {
		t.Fatal("GetMappingOptional on empty map should return false")
	}
}

func TestPredicateMap_NonUniquePanics(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	// Two different candidate predicates for the same query predicate.
	cp1 := predicates.NewConstantPredicate(predicates.TriFalse)
	cp2 := predicates.NewConstantPredicate(predicates.TriTrue)

	m1 := RegularMappingBuilder(qp, qp, cp1).Build()
	m2 := OrTermMappingBuilder(qp, qp, cp2).Build() // different kind

	builder := NewPredicateMapBuilder()
	builder.Put(qp, m1)
	builder.Put(qp, m2)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Build should panic for non-unique mappings")
		}
	}()
	builder.Build()
}

func TestPredicateMap_NonUniqueBuildMaybe(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp1 := predicates.NewConstantPredicate(predicates.TriFalse)
	cp2 := predicates.NewConstantPredicate(predicates.TriTrue)

	m1 := RegularMappingBuilder(qp, qp, cp1).Build()
	m2 := OrTermMappingBuilder(qp, qp, cp2).Build()

	builder := NewPredicateMapBuilder()
	builder.Put(qp, m1)
	builder.Put(qp, m2)

	result := builder.BuildMaybe()
	if result != nil {
		t.Fatal("BuildMaybe should return nil for non-unique mappings")
	}
}

func TestPredicateMap_EquivalentDuplicatesDedup(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp1 := predicates.NewConstantPredicate(predicates.TriFalse)
	cp2 := predicates.NewConstantPredicate(predicates.TriFalse) // same Explain()

	// Same mapping kind, same Explain on candidate.
	m1 := RegularMappingBuilder(qp, qp, cp1).Build()
	m2 := RegularMappingBuilder(qp, qp, cp2).Build()

	builder := NewPredicateMapBuilder()
	builder.Put(qp, m1)
	builder.Put(qp, m2)

	pm := builder.Build()
	got, ok := pm.GetMappingOptional(qp)
	if !ok {
		t.Fatal("GetMappingOptional should find deduplicated mapping")
	}
	if got == nil {
		t.Fatal("deduplicated mapping should not be nil")
	}
}

func TestPredicateMapping_ToBuilder(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)
	alias := values.NamedCorrelationIdentifier("p1")
	cr := predicates.EmptyComparisonRange()

	mapping := RegularMappingBuilder(qp, qp, cp).
		SetParameterAlias(alias).
		SetComparisonRange(cr).
		Build()

	rebuilt := mapping.ToBuilder().Build()

	if rebuilt.GetOriginalQueryPredicate() != qp {
		t.Fatal("rebuilt original query predicate mismatch")
	}
	if rebuilt.GetCandidatePredicate() != cp {
		t.Fatal("rebuilt candidate predicate mismatch")
	}
	if rebuilt.GetMappingKind() != MappingRegularImpliesCandidate {
		t.Fatal("rebuilt mapping kind mismatch")
	}
	if rebuilt.GetParameterAlias() == nil || *rebuilt.GetParameterAlias() != alias {
		t.Fatal("rebuilt parameter alias mismatch")
	}
	if rebuilt.GetComparisonRange() != cr {
		t.Fatal("rebuilt comparison range mismatch")
	}
}

func TestPredicateMultiMap_DefensiveCopy(t *testing.T) {
	t.Parallel()

	qp := predicates.NewConstantPredicate(predicates.TriTrue)
	cp := predicates.NewConstantPredicate(predicates.TriFalse)
	mapping := RegularMappingBuilder(qp, qp, cp).Build()

	builder := NewPredicateMultiMapBuilder()
	builder.Put(qp, mapping)
	m := builder.Build()

	// Adding to builder after Build should not affect the built map.
	cp2 := predicates.NewConstantPredicate(predicates.TriTrue)
	mapping2 := RegularMappingBuilder(qp, qp, cp2).Build()
	builder.Put(qp, mapping2)

	if m.Size() != 1 {
		t.Fatalf("built map should not be affected by subsequent builder mutations, Size() = %d", m.Size())
	}
}

func TestPredicateMultiMap_Entries(t *testing.T) {
	t.Parallel()

	qp1 := predicates.NewConstantPredicate(predicates.TriTrue)
	qp2 := predicates.NewConstantPredicate(predicates.TriFalse)
	cp1 := predicates.NewConstantPredicate(predicates.TriTrue)
	cp2 := predicates.NewConstantPredicate(predicates.TriFalse)
	cp3 := predicates.NewConstantPredicate(predicates.TriTrue)

	m1 := RegularMappingBuilder(qp1, qp1, cp1).Build()
	m2 := RegularMappingBuilder(qp1, qp1, cp2).Build()
	m3 := RegularMappingBuilder(qp2, qp2, cp3).Build()

	builder := NewPredicateMultiMapBuilder()
	builder.Put(qp1, m1)
	builder.Put(qp1, m2)
	builder.Put(qp2, m3)

	m := builder.Build()

	entries := m.Entries()
	if len(entries) != 3 {
		t.Fatalf("Entries() len = %d, want 3", len(entries))
	}

	// First two should be qp1's mappings, third should be qp2's.
	if entries[0].Predicate != qp1 || entries[1].Predicate != qp1 {
		t.Fatal("first two entries should be for qp1")
	}
	if entries[2].Predicate != qp2 {
		t.Fatal("third entry should be for qp2")
	}
}

func TestPredicateCompensation_Amend_ReplacesUnmatched(t *testing.T) {
	t.Parallel()

	// Create a ComparisonPredicate whose operand is an UnmatchedAggregateValue.
	unmatchedID := values.UniqueUnmatchedID()
	unmatchedVal := values.NewUnmatchedAggregateValue(unmatchedID)
	pred := &predicates.ComparisonPredicate{
		Operand: unmatchedVal,
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(10)},
		},
	}
	f := OfPredicateCompensation(pred, false)

	// Before amend: should be impossible (contains unmatched aggregate).
	if !f.IsImpossible() {
		t.Fatal("compensation with UnmatchedAggregateValue operand should be impossible")
	}

	// Build unmatchedAggMap: unmatchedID → FieldValue("SUM_X")
	queryAgg := &values.FieldValue{Field: "SUM_X"}
	unmatchedAggMap := NewCorrValueBiMap()
	unmatchedAggMap.Put(unmatchedID, queryAgg)

	// Build amendedMatchedAggMap: FieldValue("SUM_X") → FieldValue("IDX_SUM")
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

	// Apply and verify the operand was replaced.
	preds := amended.ApplyCompensationForPredicate(nil)
	if len(preds) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(preds))
	}
	cp, ok := preds[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", preds[0])
	}
	fv, ok := cp.Operand.(*values.FieldValue)
	if !ok {
		t.Fatalf("expected operand to be *FieldValue, got %T", cp.Operand)
	}
	if fv.Field != "IDX_SUM" {
		t.Fatalf("expected operand field IDX_SUM, got %s", fv.Field)
	}
}

func TestPredicateCompensation_IsImpossible_WithUnmatched(t *testing.T) {
	t.Parallel()

	pred := &predicates.ComparisonPredicate{
		Operand: values.NewUnmatchedAggregateValue(values.UniqueUnmatchedID()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}
	f := OfPredicateCompensation(pred, false)
	if !f.IsImpossible() {
		t.Fatal("predicate compensation with UnmatchedAggregateValue should be impossible")
	}
	if !f.IsNeeded() {
		t.Fatal("predicate compensation with UnmatchedAggregateValue should be needed")
	}
}

func TestPredicateCompensation_IsImpossible_Normal(t *testing.T) {
	t.Parallel()

	pred := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "X"},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(5)},
		},
	}
	f := OfPredicateCompensation(pred, false)
	if f.IsImpossible() {
		t.Fatal("predicate compensation with normal values should not be impossible")
	}
	if !f.IsNeeded() {
		t.Fatal("predicate compensation should be needed")
	}
}

func TestReplacePredicateValues_ComparisonPredicate(t *testing.T) {
	t.Parallel()

	targetA := &values.FieldValue{Field: "A"}
	pred := &predicates.ComparisonPredicate{
		Operand: targetA,
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}

	replacementB := &values.FieldValue{Field: "B"}
	replaced := replacePredicateValues(pred, func(v values.Value) values.Value {
		if fv, ok := v.(*values.FieldValue); ok && fv.Field == "A" {
			return replacementB
		}
		return v
	})

	cp, ok := replaced.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected *ComparisonPredicate, got %T", replaced)
	}
	fv, ok := cp.Operand.(*values.FieldValue)
	if !ok {
		t.Fatalf("expected operand to be *FieldValue, got %T", cp.Operand)
	}
	if fv.Field != "B" {
		t.Fatalf("expected operand field B, got %s", fv.Field)
	}
	// Comparison operand should be unchanged.
	cv, ok := cp.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected comparison operand to be *ConstantValue, got %T", cp.Comparison.Operand)
	}
	if cv.Value != int64(1) {
		t.Fatalf("expected comparison operand 1, got %v", cv.Value)
	}
}

func TestReplacePredicateValues_And(t *testing.T) {
	t.Parallel()

	targetA := &values.FieldValue{Field: "A"}
	child1 := &predicates.ComparisonPredicate{
		Operand: targetA,
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}
	child2 := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "C"},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(2)},
		},
	}
	andPred := predicates.NewAnd(child1, child2)

	replacementB := &values.FieldValue{Field: "B"}
	replaced := replacePredicateValues(andPred, func(v values.Value) values.Value {
		if fv, ok := v.(*values.FieldValue); ok && fv.Field == "A" {
			return replacementB
		}
		return v
	})

	andResult, ok := replaced.(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected *AndPredicate, got %T", replaced)
	}
	if len(andResult.SubPredicates) != 2 {
		t.Fatalf("expected 2 sub-predicates, got %d", len(andResult.SubPredicates))
	}

	// First child should have been replaced.
	cp1, ok := andResult.SubPredicates[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected first child *ComparisonPredicate, got %T", andResult.SubPredicates[0])
	}
	fv, ok := cp1.Operand.(*values.FieldValue)
	if !ok {
		t.Fatalf("expected first child operand *FieldValue, got %T", cp1.Operand)
	}
	if fv.Field != "B" {
		t.Fatalf("expected first child operand field B, got %s", fv.Field)
	}

	// Second child should be unchanged (same pointer).
	if andResult.SubPredicates[1] != child2 {
		t.Fatal("second child should be the same pointer (unchanged)")
	}
}

func TestValueContainsUncompensatable_Positive(t *testing.T) {
	t.Parallel()

	// IndexOnlyAggregateValue is uncompensatable.
	v := values.NewIndexOnlyAggregateValue(values.IndexOnlyMaxEverLong,
		&values.FieldValue{Field: "qty"})
	if !valueContainsUncompensatable(v) {
		t.Fatal("IndexOnlyAggregateValue should be uncompensatable")
	}

	// UnmatchedAggregateValue is also uncompensatable.
	u := values.NewUnmatchedAggregateValue(values.UniqueUnmatchedID())
	if !valueContainsUncompensatable(u) {
		t.Fatal("UnmatchedAggregateValue should be uncompensatable")
	}
}

func TestValueContainsUncompensatable_Negative(t *testing.T) {
	t.Parallel()

	v := &values.FieldValue{Field: "X"}
	if valueContainsUncompensatable(v) {
		t.Fatal("FieldValue should not be uncompensatable")
	}

	cv := &values.ConstantValue{Value: int64(42)}
	if valueContainsUncompensatable(cv) {
		t.Fatal("ConstantValue should not be uncompensatable")
	}
}
