package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestBuildMatchMaxMatchMap_RebasesQueryValueToCandidateAlias pins the
// load-bearing rebase in buildMatchMaxMatchMap (@claude RFC-076-step-1 finding 3):
// the query result value must be translated from the query-side alias to the
// candidate-side alias (via the bound alias map's source->target) BEFORE
// MaxMatchMap.compute, and the ranged-over set must be the candidate-side
// targets. Without the rebase, ComputeMaxMatchMap compares mismatched aliases,
// produces an empty map, PartialMatch.PullUp returns nil, and the data-access
// path emits no scan (ImpossibleCompensation).
func TestBuildMatchMaxMatchMap_RebasesQueryValueToCandidateAlias(t *testing.T) {
	t.Parallel()

	queryAlias := values.NamedCorrelationIdentifier("q")
	candidateAlias := values.NamedCorrelationIdentifier("c")
	boundAliasMap := AliasMapOfAliases(queryAlias, candidateAlias)

	queryResultValue := values.NewQuantifiedObjectValue(queryAlias)
	candidateResultValue := values.NewQuantifiedObjectValue(candidateAlias)

	mmm := buildMatchMaxMatchMap(queryResultValue, candidateResultValue, boundAliasMap)
	if mmm == nil {
		t.Fatal("buildMatchMaxMatchMap returned nil — PullUp would fail -> ImpossibleCompensation -> no scan")
	}

	// The query value stored is the REBASED one (query alias -> candidate alias),
	// so it now references the candidate alias.
	qv, ok := mmm.GetQueryValue().(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected query value to be a *QuantifiedObjectValue, got %T", mmm.GetQueryValue())
	}
	if qv.Correlation != candidateAlias {
		t.Fatalf("query value should be rebased to the candidate alias %q, got %q",
			candidateAlias.Name(), qv.Correlation.Name())
	}

	if mmm.GetCandidateValue() != candidateResultValue {
		t.Fatal("candidate value should be passed through unchanged")
	}

	// The rebased query value now equals the candidate value, so the map must
	// be non-empty (the candidate covers the whole query result).
	if mmm.Size() == 0 {
		t.Fatal("expected a non-empty MaxMatchMap (candidate covers the query result value)")
	}
}

// TestBuildMatchMaxMatchMap_FieldOverQuantifier pins the realistic index-column
// shape: a FieldValue over the source quantifier on both sides. The rebase must
// rewrite the query field's QOV child alias to the candidate alias so the two
// field accesses match structurally.
func TestBuildMatchMaxMatchMap_FieldOverQuantifier(t *testing.T) {
	t.Parallel()

	queryAlias := values.NamedCorrelationIdentifier("q")
	candidateAlias := values.NamedCorrelationIdentifier("c")
	boundAliasMap := AliasMapOfAliases(queryAlias, candidateAlias)

	queryField := &values.FieldValue{
		Child: values.NewQuantifiedObjectValue(queryAlias),
		Field: "COL",
	}
	candidateField := &values.FieldValue{
		Child: values.NewQuantifiedObjectValue(candidateAlias),
		Field: "COL",
	}

	mmm := buildMatchMaxMatchMap(queryField, candidateField, boundAliasMap)
	if mmm == nil {
		t.Fatal("buildMatchMaxMatchMap returned nil for a field-over-quantifier result value")
	}
	// The rebased query field references the candidate alias.
	for k := range values.GetCorrelatedToOfValue(mmm.GetQueryValue()) {
		if k == queryAlias {
			t.Fatal("query value still references the query alias — rebase did not run")
		}
	}
	if mmm.Size() == 0 {
		t.Fatal("expected a non-empty MaxMatchMap for matching FieldValue-over-quantifier")
	}
}
