package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuantifiedRecordValue_Type(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q")
	v := NewQuantifiedRecordValue(alias, NotNullLong)
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", v.Type())
	}
}

func TestQuantifiedRecordValue_NilTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q")
	v := NewQuantifiedRecordValue(alias, nil)
	if !v.Type().Equals(UnknownType) {
		t.Fatalf("Type = %v, want UnknownType", v.Type())
	}
}

func TestQuantifiedRecordValue_Children(t *testing.T) {
	t.Parallel()
	v := NewQuantifiedRecordValue(NamedCorrelationIdentifier("q"), NotNullLong)
	if got := v.Children(); len(got) != 0 {
		t.Fatalf("Children = %v, want []", got)
	}
}

func TestQuantifiedRecordValue_Name(t *testing.T) {
	t.Parallel()
	v := NewQuantifiedRecordValue(NamedCorrelationIdentifier("q"), NotNullLong)
	if got := v.Name(); got != "qrv" {
		t.Fatalf("Name = %q, want qrv", got)
	}
}

func TestQuantifiedRecordValue_EvaluateFromRowMap(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q")
	v := NewQuantifiedRecordValue(alias, NotNullLong)
	row := map[string]any{
		alias.Name(): map[string]any{"id": int64(7), "name": "alice"},
	}
	got, errEv0 := v.Evaluate(row)
	require.NoError(t, errEv0)
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("Evaluate = %v, want map", got)
	}
	if gotMap["id"] != int64(7) {
		t.Fatalf("Evaluate.id = %v, want 7", gotMap["id"])
	}
}

func TestQuantifiedRecordValue_EvaluateMissingAliasReturnsNil(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q")
	v := NewQuantifiedRecordValue(alias, NotNullLong)
	row := map[string]any{"other": int64(7)}
	got, errEv0 := v.Evaluate(row)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate(missing alias) = %v, want nil", got)
	}
}

func TestQuantifiedRecordValue_EvaluateNilCtxReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewQuantifiedRecordValue(NamedCorrelationIdentifier("q"), NotNullLong)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate(nil) = %v, want nil", got)
	}
}

func TestQuantifiedRecordValue_EvaluateNonMapCtxReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewQuantifiedRecordValue(NamedCorrelationIdentifier("q"), NotNullLong)
	got, errEv0 := v.Evaluate("not-a-map")
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate(string) = %v, want nil", got)
	}
}

func TestQuantifiedRecordValue_GetCorrelatedToReturnsAlias(t *testing.T) {
	t.Parallel()
	alias := NamedCorrelationIdentifier("q")
	v := NewQuantifiedRecordValue(alias, NotNullLong)
	got := v.GetCorrelatedTo()
	if len(got) != 1 {
		t.Fatalf("GetCorrelatedTo size = %d, want 1", len(got))
	}
	if _, ok := got[alias]; !ok {
		t.Fatalf("GetCorrelatedTo missing alias %v", alias)
	}
}
