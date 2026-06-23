package embedded

// Direct unit tests for the lexical-scope read helpers in scope.go.
// After RFC-145 removed the legacy embedded interpreter, the scope
// push-helpers it drove are gone; the read side (outerScopesContainQualifier,
// resolveOuterColumn) is retained for the shared map/proto evaluators and
// exercised directly here.

import (
	"testing"
)

// ----- outerScopesContainQualifier --------------------------------------

// TestOuterScopesContainQualifier_FoundInOnlyScope pins the simplest
// hit path: a single scope on the stack with the queried qualifier
// returns true.
func TestOuterScopesContainQualifier_FoundInOnlyScope(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{
		outerScopes: []outerScope{
			{qualifiers: map[string]bool{"A": true}},
		},
	}
	if !outerScopesContainQualifier(c, "A") {
		t.Fatal("expected A in qualifier")
	}
	if outerScopesContainQualifier(c, "B") {
		t.Fatal("did not expect B in qualifier")
	}
}

// TestOuterScopesContainQualifier_FoundInOuterScope pins multi-level
// stack: the search walks every scope, so a qualifier in any scope
// hits.
func TestOuterScopesContainQualifier_FoundInOuterScope(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{
		outerScopes: []outerScope{
			{qualifiers: map[string]bool{"OUTER": true}},
			{qualifiers: map[string]bool{"INNER": true}},
		},
	}
	if !outerScopesContainQualifier(c, "OUTER") {
		t.Fatal("expected OUTER in qualifiers (deeper scope)")
	}
	if !outerScopesContainQualifier(c, "INNER") {
		t.Fatal("expected INNER in qualifiers (shallower scope)")
	}
}

// TestOuterScopesContainQualifier_EmptyStack pins the empty-stack
// boundary: no scopes means nothing contains the qualifier.
func TestOuterScopesContainQualifier_EmptyStack(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{outerScopes: nil}
	if outerScopesContainQualifier(c, "ANYTHING") {
		t.Fatal("empty stack should not contain anything")
	}
}
