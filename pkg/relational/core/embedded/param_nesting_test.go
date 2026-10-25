package embedded

import (
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser"
)

// selfValuer's Value returns itself.
type selfValuer struct{ n int }

func (v selfValuer) Value() (driver.Value, error) { return v, nil }

// pingValuer and pongValuer return each other.
type (
	pingValuer struct{}
	pongValuer struct{}
)

func (pingValuer) Value() (driver.Value, error) { return pongValuer{}, nil }
func (pongValuer) Value() (driver.Value, error) { return pingValuer{}, nil }

// arrayValuer's Value returns an array holding itself.
type arrayValuer struct{}

func (v arrayValuer) Value() (driver.Value, error) { return []any{v}, nil }

// chainValuer returns the next link until the chain ends in a string.
type chainValuer int

func (v chainValuer) Value() (driver.Value, error) {
	if v == 0 {
		return "end", nil
	}
	return v - 1, nil
}

// A parameter whose typing never ends — a Valuer returning itself, two Valuers
// returning each other, a Valuer returning an array holding itself, a pointer
// to itself — is refused with 22023 naming the parameter, instead of recursing
// until the goroutine stack overflows (a fatal error no recover catches).
func TestBindStatementParameters_UnboundedNestingRefused(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT id FROM t WHERE id = ?")
	if err != nil {
		t.Fatal(err)
	}
	var selfPointer any
	selfPointer = &selfPointer
	for name, v := range map[string]any{
		"self-returning valuer":       selfValuer{},
		"valuer cycle":                pingValuer{},
		"valuer returning own array":  arrayValuer{},
		"pointer to itself":           selfPointer,
		"chain past the bound":        chainValuer(maxParameterNesting + 1),
		"pointer to self-returning":   &selfValuer{},
		"array of a self-returning":   []any{selfValuer{}},
		"array of a too-long chain":   []driver.Valuer{chainValuer(maxParameterNesting)},
		"valuer cycle behind pointer": func() any { p := pingValuer{}; return &p }(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release, err := bindStatementParameters(root, []driver.NamedValue{{Ordinal: 1, Value: v}})
			release()
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidParameter {
				t.Fatalf("want 22023, got %v", err)
			}
			if !strings.Contains(apiErr.Message, "parameter 1") || !strings.Contains(apiErr.Message, "levels deep") {
				t.Fatalf("error %q does not name the parameter and the nesting bound", apiErr.Message)
			}
		})
	}
	// A chain within the bound still binds.
	release, err := bindStatementParameters(root, []driver.NamedValue{{Ordinal: 1, Value: chainValuer(maxParameterNesting - 1)}})
	release()
	if err != nil {
		t.Fatalf("a %d-link Valuer chain was refused: %v", maxParameterNesting-1, err)
	}
}
