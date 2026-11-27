package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeconstructRecord_FromRecordConstructor(t *testing.T) {
	t.Parallel()
	a := LiteralValue(int64(1))
	b := LiteralValue("hello")
	rc := NewRecordConstructorValue(
		RecordConstructorField{Name: "a", Value: a},
		RecordConstructorField{Name: "b", Value: b},
	)
	got, err := DeconstructRecord(rc)
	require.NoError(t, err)
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("DeconstructRecord(rc) = %v, want [a, b]", got)
	}
}

func TestDeconstructRecord_FromRecordTypedValue(t *testing.T) {
	t.Parallel()
	// A FieldValue typed as a RecordType — DeconstructRecord should
	// generate FieldValue accessors per record field.
	rec := &RecordType{
		RecordName: "T",
		Nullable:   false,
		Fields: []Field{
			{Name: "x", FieldType: NotNullLong, Ordinal: 0},
			{Name: "y", FieldType: NotNullString, Ordinal: 1},
		},
	}
	parent := mustQOV(t, NamedCorrelationIdentifier("row"), rec)
	got, err := DeconstructRecord(parent)
	require.NoError(t, err)
	if len(got) != 2 {
		t.Fatalf("DeconstructRecord len = %d, want 2", len(got))
	}
	if fv, ok := got[0].(*fieldValue); !ok || fv.Field != "x" {
		t.Fatalf("got[0] = %v, want FieldValue(x)", got[0])
	}
	if fv, ok := got[1].(*fieldValue); !ok || fv.Field != "y" {
		t.Fatalf("got[1] = %v, want FieldValue(y)", got[1])
	}
}

func TestDeconstructRecord_NilReturnsNil(t *testing.T) {
	t.Parallel()
	if got, err := DeconstructRecord(nil); err != nil || got != nil {
		t.Fatalf("DeconstructRecord(nil) = %v, want nil", got)
	}
}

func TestDeconstructRecord_NonRecordTypedReturnsNil(t *testing.T) {
	t.Parallel()
	// LiteralValue(int64) — non-record typed → returns nil.
	v := LiteralValue(int64(7))
	if got, err := DeconstructRecord(v); err != nil || got != nil {
		t.Fatalf("DeconstructRecord(int) = %v, want nil", got)
	}
}
