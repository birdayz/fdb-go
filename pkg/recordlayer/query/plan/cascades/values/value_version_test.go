package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVersionValue_ExtractsVersionFromMap(t *testing.T) {
	t.Parallel()
	// Wrap a row map in a child Value via FieldValue. For the test we
	// pass the map directly via a thin wrapper.
	child := &constMapValue{m: map[string]any{
		"version": []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 0, 0},
		"id":      int64(42),
	}}
	v := NewVersionValue(child)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	gb, ok := got.([]byte)
	if !ok {
		t.Fatalf("VersionValue.Evaluate = %v (%T), want []byte", got, got)
	}
	if len(gb) != 12 {
		t.Fatalf("version length = %d, want 12", len(gb))
	}
}

func TestVersionValue_NilChildReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewVersionValue(nil)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("VersionValue with nil child = %v, want nil", got)
	}
}

func TestVersionValue_MissingVersionKeyReturnsNil(t *testing.T) {
	t.Parallel()
	child := &constMapValue{m: map[string]any{"id": int64(42)}}
	v := NewVersionValue(child)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("VersionValue with no version key = %v, want nil", got)
	}
}

func TestVersionValue_TypeIsNullableVersion(t *testing.T) {
	t.Parallel()
	v := NewVersionValue(LiteralValue(nil))
	if !v.Type().Equals(NullableVersion) {
		t.Fatalf("Type=%v, want NullableVersion", v.Type())
	}
}

func TestVersionValue_Children(t *testing.T) {
	t.Parallel()
	child := LiteralValue(nil)
	v := NewVersionValue(child)
	cs := v.Children()
	if len(cs) != 1 || cs[0] != child {
		t.Fatalf("Children=%v, want [child]", cs)
	}
}

// constMapValue is a test-only Value that evaluates to a fixed map.
// The seed doesn't expose a generic "object literal" Value; the
// existing LiteralValue wraps scalars only.
type constMapValue struct {
	m map[string]any
}

func (c *constMapValue) Children() []Value         { return nil }
func (*constMapValue) Name() string                { return "constmap" }
func (*constMapValue) Type() Type                  { return UnknownType }
func (c *constMapValue) Evaluate(any) (any, error) { return c.m, nil }
