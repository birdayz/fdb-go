package semantic

import (
	"errors"
	"testing"
)

func TestFunctionCatalog_RegisterDefaults(t *testing.T) {
	t.Parallel()
	c := NewFunctionCatalog()
	c.RegisterDefaults()

	for _, name := range []string{"COUNT", "SUM", "MIN", "MAX", "AVG"} {
		spec, ok := c.Lookup(NewUnquoted(name))
		if !ok {
			t.Fatalf("default %s should be registered", name)
		}
		if spec.Kind != FunctionAggregate {
			t.Fatalf("%s Kind: got %v, want aggregate", name, spec.Kind)
		}
	}
}

func TestFunctionCatalog_CaseInsensitive(t *testing.T) {
	t.Parallel()
	c := NewFunctionCatalog()
	c.RegisterDefaults()

	spec, ok := c.Lookup(NewUnquoted("count"))
	if !ok {
		t.Fatal("count (lower) should match COUNT")
	}
	if spec.Name != "COUNT" {
		t.Fatalf("Name: got %q, want COUNT", spec.Name)
	}
}

func TestFunctionCatalog_Contains(t *testing.T) {
	t.Parallel()
	c := NewFunctionCatalog()
	c.RegisterDefaults()

	if !c.Contains(NewUnquoted("sum")) {
		t.Fatal("SUM should be present")
	}
	if c.Contains(NewUnquoted("unknown_fn")) {
		t.Fatal("unknown function should not be present")
	}
}

func TestFunctionCatalog_DuplicateRegistrationErrors(t *testing.T) {
	t.Parallel()
	c := NewFunctionCatalog()
	if err := c.Register(FunctionSpec{Name: "FOO", Kind: FunctionScalar, MinArgs: 1, MaxArgs: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Register(FunctionSpec{Name: "foo", Kind: FunctionScalar, MinArgs: 1, MaxArgs: 1}); err == nil {
		t.Fatal("expected duplicate error (case-insensitive)")
	}
}

func TestFunctionSpec_ValidateArity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		spec    FunctionSpec
		argc    int
		wantErr bool
	}{
		{"COUNT 1 arg", FunctionSpec{Name: "COUNT", MinArgs: 1, MaxArgs: 1}, 1, false},
		{"COUNT 0 args", FunctionSpec{Name: "COUNT", MinArgs: 1, MaxArgs: 1}, 0, true},
		{"COUNT 2 args", FunctionSpec{Name: "COUNT", MinArgs: 1, MaxArgs: 1}, 2, true},
		{"variadic no upper", FunctionSpec{Name: "F", MinArgs: 1, MaxArgs: -1}, 10, false},
		{"variadic too few", FunctionSpec{Name: "F", MinArgs: 1, MaxArgs: -1}, 0, true},
		{"zero-arg func", FunctionSpec{Name: "NOW", MinArgs: 0, MaxArgs: 0}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.spec.ValidateArity(tc.argc)
			if (err != nil) != tc.wantErr {
				t.Fatalf("got err=%v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				var ae *FunctionArityError
				if !errors.As(err, &ae) {
					t.Fatalf("expected FunctionArityError, got %T", err)
				}
			}
		})
	}
}

func TestFunctionArityError_Messages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err      *FunctionArityError
		contains string
	}{
		{&FunctionArityError{Function: "COUNT", Got: 0, Min: 1, Max: 1}, "expects 1 argument"},
		{&FunctionArityError{Function: "F", Got: 5, Min: 2, Max: 4}, "expects 2..4 arguments"},
		{&FunctionArityError{Function: "F", Got: 0, Min: 1, Max: -1}, "at least 1 argument"},
	}
	for _, tc := range cases {
		msg := tc.err.Error()
		if !stringContains(msg, tc.contains) {
			t.Fatalf("message %q missing %q", msg, tc.contains)
		}
	}
}

// tiny Contains-helper so tests don't pull in strings for this one
// call (keeps the package test dep surface small).
func stringContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestFunctionCatalog_AllowsStar(t *testing.T) {
	t.Parallel()
	c := NewFunctionCatalog()
	c.RegisterDefaults()

	count, _ := c.Lookup(NewUnquoted("COUNT"))
	if !count.AllowsStar {
		t.Fatal("COUNT should accept star")
	}
	sum, _ := c.Lookup(NewUnquoted("SUM"))
	if sum.AllowsStar {
		t.Fatal("SUM should NOT accept star")
	}
}

func TestFunctionCatalog_AllowsDistinct(t *testing.T) {
	t.Parallel()
	c := NewFunctionCatalog()
	c.RegisterDefaults()

	// All standard SQL aggregates accept DISTINCT.
	for _, name := range []string{"COUNT", "SUM", "MIN", "MAX", "AVG"} {
		spec, _ := c.Lookup(NewUnquoted(name))
		if !spec.AllowsDistinct {
			t.Fatalf("%s should accept DISTINCT", name)
		}
	}
}
