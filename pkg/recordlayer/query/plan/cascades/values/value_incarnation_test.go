package values

import "testing"

func TestIncarnationValue_Type(t *testing.T) {
	t.Parallel()
	v := NewIncarnationValue()
	if !v.Type().Equals(NotNullInt) {
		t.Fatalf("Type = %v, want NotNullInt", v.Type())
	}
}

func TestIncarnationValue_NoChildren(t *testing.T) {
	t.Parallel()
	v := NewIncarnationValue()
	if got := v.Children(); len(got) != 0 {
		t.Fatalf("Children = %v, want []", got)
	}
}

func TestIncarnationValue_Name(t *testing.T) {
	t.Parallel()
	v := NewIncarnationValue()
	if got := v.Name(); got != "get_versionstamp_incarnation" {
		t.Fatalf("Name = %q, want %q", got, "get_versionstamp_incarnation")
	}
}

func TestIncarnationValue_EvaluateFromMap(t *testing.T) {
	t.Parallel()
	v := NewIncarnationValue()
	row := map[string]any{"incarnation": int64(7)}
	if got := mustEvaluate(v, row); got != int64(7) {
		t.Fatalf("Evaluate = %v, want 7", got)
	}
}

func TestIncarnationValue_EvaluateMissingKeyReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewIncarnationValue()
	row := map[string]any{"other": int64(7)}
	if got := mustEvaluate(v, row); got != nil {
		t.Fatalf("Evaluate(no incarnation) = %v, want nil", got)
	}
}

func TestIncarnationValue_EvaluateNilContextReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewIncarnationValue()
	if got := mustEvaluate(v, nil); got != nil {
		t.Fatalf("Evaluate(nil) = %v, want nil", got)
	}
}

func TestIncarnationValue_EvaluateNonMapContextReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewIncarnationValue()
	if got := mustEvaluate(v, "not-a-map"); got != nil {
		t.Fatalf("Evaluate(string) = %v, want nil", got)
	}
}

func TestIncarnationValue_FreshPerCall(t *testing.T) {
	t.Parallel()
	v1 := NewIncarnationValue()
	v2 := NewIncarnationValue()
	// Different pointers (allocation contract), same semantic value.
	if v1 == v2 {
		t.Fatalf("NewIncarnationValue should return distinct pointers per call")
	}
}
