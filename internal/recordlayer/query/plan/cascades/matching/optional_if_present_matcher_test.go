package matching

import (
	"testing"
)

// alwaysTrueMatcher binds to any non-nil input — used in tests where
// we only care about presence/absence + the binding plumbing, not
// what the downstream actually checks.
func alwaysTrueMatcher() BindingMatcher {
	return NewSatisfyingMatcher[any]("AlwaysTrue", func(any) bool { return true })
}

// TestOptionalIfPresent_Present pins the happy path: a non-nil
// input is forwarded to the downstream and the downstream's
// bindings flow through.
func TestOptionalIfPresent_Present(t *testing.T) {
	t.Parallel()
	inner := alwaysTrueMatcher()
	outer := NewOptionalIfPresentMatcher(inner)
	in := "hello"
	matches := outer.BindMatches(NewBindings(), in)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if got := matches[0].Get(outer); got != "hello" {
		t.Errorf("matcher binding: got %v, want %q", got, "hello")
	}
}

// TestOptionalIfPresent_NilInterface pins absence: a bare nil
// interface yields no match.
func TestOptionalIfPresent_NilInterface(t *testing.T) {
	t.Parallel()
	inner := alwaysTrueMatcher()
	outer := NewOptionalIfPresentMatcher(inner)
	if outer.BindMatches(NewBindings(), nil) != nil {
		t.Error("nil interface should not match")
	}
}

// TestOptionalIfPresent_TypedNilPointer pins typed-nil: an `any`
// holding a `(*T)(nil)` is absent.
func TestOptionalIfPresent_TypedNilPointer(t *testing.T) {
	t.Parallel()
	inner := alwaysTrueMatcher()
	outer := NewOptionalIfPresentMatcher(inner)
	var p *struct{} // typed nil
	if outer.BindMatches(NewBindings(), p) != nil {
		t.Error("typed-nil pointer should not match")
	}
}

// TestOptionalIfPresent_NonNilPointer pins that a non-nil pointer
// IS present and forwards as-is.
func TestOptionalIfPresent_NonNilPointer(t *testing.T) {
	t.Parallel()
	inner := alwaysTrueMatcher()
	outer := NewOptionalIfPresentMatcher(inner)
	v := struct{ X int }{X: 42}
	matches := outer.BindMatches(NewBindings(), &v)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if got := matches[0].Get(outer); got != &v {
		t.Errorf("matcher binding: got %v, want %p", got, &v)
	}
}

// TestOptionalIfPresent_DownstreamRejects pins that a downstream
// failure propagates as no-match (the "if present" guard isn't a
// pass-through; the downstream must also succeed).
func TestOptionalIfPresent_DownstreamRejects(t *testing.T) {
	t.Parallel()
	// Build a downstream that only matches `bool`.
	inner := NewSatisfyingMatcher[any]("BoolOnly", func(v any) bool {
		_, ok := v.(bool)
		return ok
	})
	outer := NewOptionalIfPresentMatcher(inner)
	if outer.BindMatches(NewBindings(), 42) != nil {
		t.Error("downstream rejection should propagate")
	}
}

// TestOptionalIfPresent_NilDownstreamPanics pins the construction
// guard — a present-matcher without something to match is useless.
func TestOptionalIfPresent_NilDownstreamPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil downstream")
		}
	}()
	_ = NewOptionalIfPresentMatcher(nil)
}

// TestOptionalIfPresent_DistinctIdentity pins that two
// NewOptionalIfPresentMatcher calls produce distinct identities so
// they bind to separate slots in PlannerBindings.
func TestOptionalIfPresent_DistinctIdentity(t *testing.T) {
	t.Parallel()
	inner := alwaysTrueMatcher()
	a := NewOptionalIfPresentMatcher(inner)
	b := NewOptionalIfPresentMatcher(inner)
	bindings := NewBindings().Bind(a, "first").Bind(b, "second")
	if got := bindings.Get(a); got != "first" {
		t.Errorf("a binding: got %v", got)
	}
	if got := bindings.Get(b); got != "second" {
		t.Errorf("b binding: got %v", got)
	}
}
