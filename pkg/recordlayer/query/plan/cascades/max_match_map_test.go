package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestComputeMaxMatchMap_IdenticalValues(t *testing.T) {
	t.Parallel()

	qv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	cv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping entry for identical values, got %d", mmm.Size())
	}
	if mmm.GetQueryValue() != qv {
		t.Fatal("queryValue mismatch")
	}
	if mmm.GetCandidateValue() != cv {
		t.Fatal("candidateValue mismatch")
	}
}

func TestComputeMaxMatchMap_DifferentValues(t *testing.T) {
	t.Parallel()

	qv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	cv := &values.FieldValue{Field: "col2", Typ: values.TypeInt}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	if mmm.Size() != 0 {
		t.Fatalf("expected 0 mapping entries for different values, got %d", mmm.Size())
	}
}

func TestComputeMaxMatchMap_NilValues(t *testing.T) {
	t.Parallel()

	mmm := ComputeMaxMatchMap(nil, nil, nil)

	if mmm.Size() != 0 {
		t.Fatalf("expected 0 mapping entries for nil values, got %d", mmm.Size())
	}
	if mmm.GetQueryValue() != nil {
		t.Fatal("queryValue should be nil")
	}
	if mmm.GetCandidateValue() != nil {
		t.Fatal("candidateValue should be nil")
	}
}

func TestComputeMaxMatchMap_ChildrenMatch(t *testing.T) {
	t.Parallel()

	// Arithmetic values with matching children but different operators
	// should match at the child level, not root.
	left := &values.FieldValue{Field: "x", Typ: values.TypeInt}
	right := &values.ConstantValue{Value: int64(5), Typ: values.TypeInt}

	qv := &values.ArithmeticValue{Op: values.OpAdd, Left: left, Right: right}
	cv := &values.ArithmeticValue{Op: values.OpAdd, Left: left, Right: right}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	// Root-level structural equality holds (same op, same children),
	// so we get 1 entry for the root.
	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping entry for structurally equal arithmetic values, got %d", mmm.Size())
	}
}

func TestComputeMaxMatchMap_ChildrenPartialMatch(t *testing.T) {
	t.Parallel()

	// Same structure, but one child differs — should match at child
	// level for the matching child.
	leftQ := &values.FieldValue{Field: "x", Typ: values.TypeInt}
	leftC := &values.FieldValue{Field: "x", Typ: values.TypeInt}
	rightQ := &values.ConstantValue{Value: int64(5), Typ: values.TypeInt}
	rightC := &values.ConstantValue{Value: int64(99), Typ: values.TypeInt}

	qv := &values.ArithmeticValue{Op: values.OpAdd, Left: leftQ, Right: rightQ}
	cv := &values.ArithmeticValue{Op: values.OpAdd, Left: leftC, Right: rightC}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	// Root doesn't match (different right child), but left child matches.
	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping entry for partial child match, got %d", mmm.Size())
	}
}

func TestTranslateQueryValueMaybe_IdentityMapping(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("candidate")
	qv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	cv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	result := mmm.TranslateQueryValueMaybe(alias)
	if result == nil {
		t.Fatal("TranslateQueryValueMaybe returned nil for identity mapping")
	}

	// For identity mapping, the result should be a QOV referencing
	// candidateAlias.
	qov, ok := result.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected QuantifiedObjectValue, got %T", result)
	}
	if qov.Correlation != alias {
		t.Fatalf("expected correlation %q, got %q", alias.Name(), qov.Correlation.Name())
	}
}

func TestTranslateQueryValueMaybe_EmptyMapping(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("candidate")
	qv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	cv := &values.FieldValue{Field: "col2", Typ: values.TypeInt}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	result := mmm.TranslateQueryValueMaybe(alias)
	if result != nil {
		t.Fatalf("TranslateQueryValueMaybe should return nil for empty mapping, got %T", result)
	}
}

func TestTranslateQueryValueMaybe_NilQueryValue(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("candidate")
	mmm := ComputeMaxMatchMap(nil, nil, nil)

	result := mmm.TranslateQueryValueMaybe(alias)
	if result != nil {
		t.Fatal("TranslateQueryValueMaybe should return nil for nil queryValue")
	}
}

func TestTranslateQueryValueMaybe_RangedOverAliasValidation(t *testing.T) {
	t.Parallel()

	candidateAlias := values.NamedCorrelationIdentifier("candidate")
	forbiddenAlias := values.NamedCorrelationIdentifier("forbidden")

	// Build a query value that has a QOV referencing forbiddenAlias
	// as a child, but the mapping only covers the non-forbidden part.
	forbidden := &values.QuantifiedObjectValue{Correlation: forbiddenAlias, Typ: values.TypeInt}
	qv := forbidden
	cv := forbidden // identity mapping

	rangedOver := map[values.CorrelationIdentifier]struct{}{forbiddenAlias: {}}
	mmm := ComputeMaxMatchMap(qv, cv, rangedOver)

	// The identity case returns QOV(candidateAlias), which doesn't
	// contain forbiddenAlias, so validation should pass.
	result := mmm.TranslateQueryValueMaybe(candidateAlias)
	if result == nil {
		t.Fatal("TranslateQueryValueMaybe should succeed when rangedOverAlias is fully substituted")
	}
}

func TestPullUpMaybe_IdentityMapping(t *testing.T) {
	t.Parallel()

	queryAlias := values.NamedCorrelationIdentifier("query")
	candidateAlias := values.NamedCorrelationIdentifier("candidate")

	qv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	cv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	mmm := ComputeMaxMatchMap(qv, cv, nil)

	tm, ok := mmm.PullUpMaybe(queryAlias, candidateAlias)
	if !ok {
		t.Fatal("PullUpMaybe returned false for identity mapping")
	}
	if tm == nil {
		t.Fatal("PullUpMaybe returned nil translation map for identity mapping")
	}

	if !tm.ContainsSourceAlias(queryAlias) {
		t.Fatal("translation map should contain queryAlias")
	}

	// Apply the translation function — should return the translated value.
	leaf := &values.QuantifiedObjectValue{Correlation: queryAlias, Typ: values.TypeInt}
	translated := tm.ApplyTranslationFunction(queryAlias, leaf)
	if translated == nil {
		t.Fatal("ApplyTranslationFunction returned nil")
	}
	if qov, ok := translated.(*values.QuantifiedObjectValue); ok {
		if qov.Correlation != candidateAlias {
			t.Fatalf("expected correlation %q, got %q", candidateAlias.Name(), qov.Correlation.Name())
		}
	}
}

func TestPullUpMaybe_EmptyMapping(t *testing.T) {
	t.Parallel()

	queryAlias := values.NamedCorrelationIdentifier("query")
	candidateAlias := values.NamedCorrelationIdentifier("candidate")

	qv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	cv := &values.FieldValue{Field: "col2", Typ: values.TypeInt}
	mmm := ComputeMaxMatchMap(qv, cv, nil)

	tm, ok := mmm.PullUpMaybe(queryAlias, candidateAlias)
	if ok {
		t.Fatal("PullUpMaybe should return false for empty mapping")
	}
	if tm != nil {
		t.Fatal("PullUpMaybe should return nil for empty mapping")
	}
}

func TestAdjustMaybe_IdentityMapping(t *testing.T) {
	t.Parallel()

	upperAlias := values.NamedCorrelationIdentifier("upper")
	qv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	cv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	upperResult := &values.FieldValue{Field: "col1", Typ: values.TypeInt}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	adjusted, ok := mmm.AdjustMaybe(upperAlias, upperResult, nil)
	if !ok {
		t.Fatal("AdjustMaybe returned false for identity mapping")
	}
	if adjusted == nil {
		t.Fatal("AdjustMaybe returned nil for identity mapping")
	}

	// The adjusted map's query value is the translated value (QOV
	// through upperAlias), and the candidate value is upperResult.
	if adjusted.GetCandidateValue() != upperResult {
		t.Fatal("adjusted candidateValue should be upperResult")
	}
}

func TestAdjustMaybe_EmptyMapping(t *testing.T) {
	t.Parallel()

	upperAlias := values.NamedCorrelationIdentifier("upper")
	qv := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	cv := &values.FieldValue{Field: "col2", Typ: values.TypeInt}
	upperResult := &values.FieldValue{Field: "col1", Typ: values.TypeInt}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	adjusted, ok := mmm.AdjustMaybe(upperAlias, upperResult, nil)
	if ok {
		t.Fatal("AdjustMaybe should return false for empty mapping")
	}
	if adjusted != nil {
		t.Fatal("AdjustMaybe should return nil for empty mapping")
	}
}

func TestMaxMatchMap_GetMap_GetQueryValue_GetCandidateValue(t *testing.T) {
	t.Parallel()

	q := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	c := &values.ConstantValue{Value: int64(2), Typ: values.TypeInt}
	mapping := map[values.Value]values.Value{q: c}

	mmm := NewMaxMatchMap(mapping, q, c)

	if mmm.GetQueryValue() != q {
		t.Fatal("queryValue mismatch")
	}
	if mmm.GetCandidateValue() != c {
		t.Fatal("candidateValue mismatch")
	}

	gotMap := mmm.GetMap()
	if len(gotMap) != 1 {
		t.Fatalf("mapping len=%d, want 1", len(gotMap))
	}
}

func TestMaxMatchMap_NewMaxMatchMap_DefensiveCopy(t *testing.T) {
	t.Parallel()

	q := &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}
	c := &values.ConstantValue{Value: int64(2), Typ: values.TypeInt}
	mapping := map[values.Value]values.Value{q: c}

	mmm := NewMaxMatchMap(mapping, q, c)

	// Mutate the original map.
	extra := &values.ConstantValue{Value: int64(3), Typ: values.TypeInt}
	mapping[extra] = extra

	if mmm.Size() != 1 {
		t.Fatal("mutation of original mapping leaked into MaxMatchMap")
	}
}

func TestMaxMatchMap_NewMaxMatchMap_NilInputs(t *testing.T) {
	t.Parallel()

	mmm := NewMaxMatchMap(nil, nil, nil)

	if mmm.GetQueryValue() != nil {
		t.Fatal("nil queryValue should stay nil")
	}
	if mmm.GetCandidateValue() != nil {
		t.Fatal("nil candidateValue should stay nil")
	}
	gotMap := mmm.GetMap()
	if gotMap == nil {
		t.Fatal("map should be non-nil even when input is nil")
	}
	if len(gotMap) != 0 {
		t.Fatal("map should be empty when input is nil")
	}
}

func TestComputeMaxMatchMap_QOVIdentity(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("q1")
	candidateAlias := values.NamedCorrelationIdentifier("candidate")

	qv := &values.QuantifiedObjectValue{Correlation: alias, Typ: values.TypeInt}
	cv := &values.QuantifiedObjectValue{Correlation: alias, Typ: values.TypeInt}

	mmm := ComputeMaxMatchMap(qv, cv, nil)

	if mmm.Size() != 1 {
		t.Fatalf("expected 1 mapping entry for identical QOVs, got %d", mmm.Size())
	}

	// Translate should produce QOV(candidateAlias).
	result := mmm.TranslateQueryValueMaybe(candidateAlias)
	if result == nil {
		t.Fatal("TranslateQueryValueMaybe returned nil")
	}
	qov, ok := result.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected QuantifiedObjectValue, got %T", result)
	}
	if qov.Correlation != candidateAlias {
		t.Fatalf("expected correlation %q, got %q", candidateAlias.Name(), qov.Correlation.Name())
	}
}

func TestMaxMatchMap_Size(t *testing.T) {
	t.Parallel()

	mmm := ComputeMaxMatchMap(nil, nil, nil)
	if mmm.Size() != 0 {
		t.Fatalf("expected size 0, got %d", mmm.Size())
	}

	qv := &values.FieldValue{Field: "a", Typ: values.TypeInt}
	cv := &values.FieldValue{Field: "a", Typ: values.TypeInt}
	mmm2 := ComputeMaxMatchMap(qv, cv, nil)
	if mmm2.Size() != 1 {
		t.Fatalf("expected size 1, got %d", mmm2.Size())
	}
}
