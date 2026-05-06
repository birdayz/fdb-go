package values

import "testing"

func TestQueriedValue_LeafShape(t *testing.T) {
	t.Parallel()
	v := NewQueriedValue([]string{"Order"}, UnknownType)
	if len(v.Children()) != 0 {
		t.Fatal("QueriedValue should be a leaf")
	}
}

func TestQueriedValue_DedupTypes(t *testing.T) {
	t.Parallel()
	v := NewQueriedValue([]string{"B", "A", "B"}, UnknownType)
	rts := v.RecordTypes
	if len(rts) != 2 || rts[0] != "A" || rts[1] != "B" {
		t.Fatalf("RecordTypes = %v, want [A, B]", rts)
	}
}

func TestQueriedValue_NilTypeFallsBackToUnknown(t *testing.T) {
	t.Parallel()
	v := NewQueriedValue([]string{"T"}, nil)
	if v.Type() != UnknownType {
		t.Fatalf("Type=%v, want UnknownType", v.Type())
	}
}

func TestQueriedValue_EvaluateReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewQueriedValue([]string{"T"}, UnknownType)
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate = %v, want nil (placeholder)", got)
	}
}
