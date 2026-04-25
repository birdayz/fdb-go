package embedded

// Direct unit tests for the lexical-scope helpers in scope.go.
// Per RFC-025 §"Strong unit-test coverage per package": these helpers
// have NO test file today; coverage came incidentally from
// integration tests that drive correlated subqueries through the
// executor. Direct tests catch regressions where they originate
// (qualifier-set construction, row-vs-msg dispatch, push/pop
// stacking) instead of as a downstream subquery-resolution failure.
//
// Most helpers are methods on EmbeddedConnection and use bare (nil-
// session, no-FDB) connections for the tests — the state under test
// is just a few maps + slices on the struct, no IO needed.

import (
	"database/sql/driver"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

// ----- outerScopeFromMapRow ---------------------------------------------

// TestOuterScopeFromMapRow_QualifiersFromKeys pins the qualifier-set
// derivation: every key of form `alias.col` contributes its alias
// (uppercased) to the qualifier set; bare-key entries don't.
func TestOuterScopeFromMapRow_QualifiersFromKeys(t *testing.T) {
	t.Parallel()
	row := map[string]driver.Value{
		"id":         int64(1),
		"a.id":       int64(1),
		"b.name":     "x",
		"a.name":     "y",
		"plain_only": "z",
	}
	s := outerScopeFromMapRow(row)
	if s.row == nil {
		t.Fatal("expected non-nil row in scope")
	}
	wantQuals := []string{"A", "B"}
	for _, q := range wantQuals {
		if !s.qualifiers[q] {
			t.Errorf("qualifier %q missing from %v", q, s.qualifiers)
		}
	}
	if len(s.qualifiers) != 2 {
		t.Errorf("qualifier count: got %d, want 2 (got=%v)", len(s.qualifiers), s.qualifiers)
	}
}

// TestOuterScopeFromMapRow_EmptyRow pins the boundary: nil / empty
// map returns a zero-value scope. Lookups against the resulting
// scope fall through cleanly.
func TestOuterScopeFromMapRow_EmptyRow(t *testing.T) {
	t.Parallel()
	for _, row := range []map[string]driver.Value{nil, {}} {
		s := outerScopeFromMapRow(row)
		if s.row != nil || len(s.qualifiers) != 0 {
			t.Errorf("expected zero-value scope for empty row, got %+v", s)
		}
	}
}

// TestOuterScopeFromMapRow_MixedCaseQualifiersUpcase pins
// case-insensitive qualifier matching: row keys can use any case,
// the qualifier set always stores upper-case.
func TestOuterScopeFromMapRow_MixedCaseQualifiersUpcase(t *testing.T) {
	t.Parallel()
	row := map[string]driver.Value{
		"Emp.id":      int64(1),
		"DEPT.id":     int64(2),
		"customer.id": int64(3),
	}
	s := outerScopeFromMapRow(row)
	for _, q := range []string{"EMP", "DEPT", "CUSTOMER"} {
		if !s.qualifiers[q] {
			t.Errorf("qualifier %q missing", q)
		}
	}
}

// ----- outerScopeFromMsg ------------------------------------------------

// TestOuterScopeFromMsg_NilMsg pins the nil-safety boundary.
func TestOuterScopeFromMsg_NilMsg(t *testing.T) {
	t.Parallel()
	s := outerScopeFromMsg(nil, nil)
	if s.msg != nil || s.row != nil || len(s.qualifiers) != 0 {
		t.Fatalf("expected zero-value scope, got %+v", s)
	}
}

// TestOuterScopeFromMsg_DescriptorOnly pins the simplest case: nil
// connection means the qualifier set comes only from the message's
// descriptor name.
func TestOuterScopeFromMsg_DescriptorOnly(t *testing.T) {
	t.Parallel()
	msg := &gen.Order{} // descriptor name "Order"
	s := outerScopeFromMsg(nil, msg)
	if s.msg != msg {
		t.Fatal("expected msg pointer preserved")
	}
	if !s.qualifiers["ORDER"] {
		t.Errorf("expected ORDER in qualifiers, got %v", s.qualifiers)
	}
	if len(s.qualifiers) != 1 {
		t.Errorf("qualifier count: got %d, want 1", len(s.qualifiers))
	}
}

// TestOuterScopeFromMsg_WithSourceAliases pins the alias-merge
// behaviour: connection's currentSourceAliases combine with the
// descriptor name (uppercased) to form the qualifier set. Used so
// `FROM emp AS e WHERE EXISTS (... WHERE id = e.id)` resolves `e`
// to the outer Emp message.
func TestOuterScopeFromMsg_WithSourceAliases(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{
		currentSourceAliases: map[string]bool{"E": true, "EMP_OUTER": true},
	}
	msg := &gen.Order{}
	s := outerScopeFromMsg(conn, msg)
	for _, q := range []string{"ORDER", "E", "EMP_OUTER"} {
		if !s.qualifiers[q] {
			t.Errorf("qualifier %q missing from %v", q, s.qualifiers)
		}
	}
	if len(s.qualifiers) != 3 {
		t.Errorf("qualifier count: got %d, want 3", len(s.qualifiers))
	}
}

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

// ----- pushCTEScope ------------------------------------------------------

// TestPushCTEScope_InheritsAndIsolates pins the scope-stack contract:
// the new scope inherits the prior scope's CTE bindings (so inner
// queries can reference outer CTEs), but writes to the new scope
// don't leak back to the outer when the pop function runs.
func TestPushCTEScope_InheritsAndIsolates(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{
		ctes: map[string]*cteData{"outer_cte": {}},
	}
	pop := c.pushCTEScope()
	// Inner sees the outer.
	if _, ok := c.ctes["outer_cte"]; !ok {
		t.Fatal("inner should inherit outer CTE")
	}
	// Inner adds its own.
	c.ctes["inner_cte"] = &cteData{}
	pop()
	// Outer is restored — no inner_cte.
	if _, ok := c.ctes["inner_cte"]; ok {
		t.Fatal("inner CTE leaked back into outer scope after pop")
	}
	if _, ok := c.ctes["outer_cte"]; !ok {
		t.Fatal("outer CTE lost after pop")
	}
}

// TestPushCTEScope_NilOuter pins the boundary: a connection with nil
// ctes still produces a working push (the new scope is just an
// empty map).
func TestPushCTEScope_NilOuter(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{}
	pop := c.pushCTEScope()
	if c.ctes == nil {
		t.Fatal("expected non-nil ctes after push")
	}
	c.ctes["x"] = &cteData{}
	pop()
	// After pop the prior nil should be restored.
	if c.ctes != nil {
		t.Fatalf("expected nil ctes after pop, got %v", c.ctes)
	}
}

// ----- pushSourceAliases ------------------------------------------------

// TestPushSourceAliases_StoresUppercase pins case-folding: the alias
// set always stores upper-case so case-insensitive matching works in
// outerScopeFromMsg.
func TestPushSourceAliases_StoresUppercase(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{}
	pop := c.pushSourceAliases("a", "B", "")
	for _, want := range []string{"A", "B"} {
		if !c.currentSourceAliases[want] {
			t.Errorf("expected %q in currentSourceAliases, got %v", want, c.currentSourceAliases)
		}
	}
	if len(c.currentSourceAliases) != 2 {
		t.Errorf("expected 2 entries (empty filtered), got %d", len(c.currentSourceAliases))
	}
	pop()
	if c.currentSourceAliases != nil {
		t.Errorf("expected nil after pop, got %v", c.currentSourceAliases)
	}
}

// ----- pushValidQualifiersScope -----------------------------------------

// TestPushValidQualifiersScope_StackingRestores pins push/pop
// nesting: nested pushes layer correctly, and each pop restores the
// previous layer.
func TestPushValidQualifiersScope_StackingRestores(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{}
	popOuter := c.pushValidQualifiersScope(map[string]bool{"A": true})
	if !c.validQualifiers["A"] {
		t.Fatal("outer push should set A")
	}
	popInner := c.pushValidQualifiersScope(map[string]bool{"B": true})
	if !c.validQualifiers["B"] {
		t.Fatal("inner push should set B")
	}
	if c.validQualifiers["A"] {
		t.Fatal("inner push should mask outer A")
	}
	popInner()
	if !c.validQualifiers["A"] {
		t.Fatal("outer A should be restored after inner pop")
	}
	popOuter()
	if c.validQualifiers != nil {
		t.Fatalf("expected nil after outer pop, got %v", c.validQualifiers)
	}
}

// ----- pushOuterScope ----------------------------------------------------

// TestPushOuterScope_AppendsAndPops pins the slice-stack contract:
// push appends, pop trims back. Multiple nested pushes stack;
// resolveOuterColumn walks them innermost-first.
func TestPushOuterScope_AppendsAndPops(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{}
	if len(c.outerScopes) != 0 {
		t.Fatalf("expected empty stack, got %d entries", len(c.outerScopes))
	}
	pop1 := c.pushOuterScope(outerScope{qualifiers: map[string]bool{"X": true}})
	if len(c.outerScopes) != 1 {
		t.Fatalf("after first push: got len %d, want 1", len(c.outerScopes))
	}
	pop2 := c.pushOuterScope(outerScope{qualifiers: map[string]bool{"Y": true}})
	if len(c.outerScopes) != 2 {
		t.Fatalf("after second push: got len %d, want 2", len(c.outerScopes))
	}
	pop2()
	if len(c.outerScopes) != 1 {
		t.Fatalf("after first pop: got len %d, want 1", len(c.outerScopes))
	}
	if !c.outerScopes[0].qualifiers["X"] {
		t.Fatal("outer scope should still hold X after inner pop")
	}
	pop1()
	if len(c.outerScopes) != 0 {
		t.Fatalf("after outer pop: got len %d, want 0", len(c.outerScopes))
	}
}

// TestPushOuterScope_ZeroValueSafe pins safety with a zero-value
// scope (msg + row both nil): push/pop works and lookups against
// the resulting scope fall through cleanly.
func TestPushOuterScope_ZeroValueSafe(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{}
	pop := c.pushOuterScope(outerScope{})
	if len(c.outerScopes) != 1 {
		t.Fatal("expected one entry after zero-value push")
	}
	if outerScopesContainQualifier(c, "ANY") {
		t.Fatal("zero-value scope should not contain any qualifier")
	}
	pop()
}

// _ ensures proto.Message is referenced (the test imports gen.Order
// which already implements it; this is an explicit safety net so a
// future cleanup that removes the gen import is forced to update
// this test rather than silently break the proto-backed scope tests).
var _ proto.Message = (*gen.Order)(nil)
