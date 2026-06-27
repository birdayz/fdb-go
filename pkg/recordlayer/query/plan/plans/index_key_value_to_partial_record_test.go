package plans

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestFieldCopier_Key(t *testing.T) {
	t.Parallel()
	c := &FieldCopier{Field: "name", Source: TupleSourceKey, OrdinalPath: []int{0}}
	out := map[string]any{}
	key := tuple.Tuple{"Alice"}
	ok := c.Copy(out, key, nil)
	if !ok {
		t.Fatal("expected copy to succeed")
	}
	if out["NAME"] != "Alice" {
		t.Fatalf("expected Alice, got %v", out["NAME"])
	}
}

func TestFieldCopier_Value(t *testing.T) {
	t.Parallel()
	c := &FieldCopier{Field: "score", Source: TupleSourceValue, OrdinalPath: []int{0}}
	out := map[string]any{}
	val := tuple.Tuple{int64(42)}
	ok := c.Copy(out, nil, val)
	if !ok {
		t.Fatal("expected copy to succeed")
	}
	if out["SCORE"] != int64(42) {
		t.Fatalf("expected 42, got %v", out["SCORE"])
	}
}

func TestFieldCopier_NestedOrdinal(t *testing.T) {
	t.Parallel()
	c := &FieldCopier{Field: "city", Source: TupleSourceKey, OrdinalPath: []int{1}}
	out := map[string]any{}
	key := tuple.Tuple{"Alice", "NYC", int64(100)}
	ok := c.Copy(out, key, nil)
	if !ok {
		t.Fatal("expected copy to succeed")
	}
	if out["CITY"] != "NYC" {
		t.Fatalf("expected NYC, got %v", out["CITY"])
	}
}

func TestFieldCopier_NullValue(t *testing.T) {
	t.Parallel()
	c := &FieldCopier{Field: "val", Source: TupleSourceKey, OrdinalPath: []int{0}}
	out := map[string]any{}
	key := tuple.Tuple{nil}
	ok := c.Copy(out, key, nil)
	if ok {
		t.Fatal("expected copy to refuse on nil")
	}
}

func TestFieldCopier_OutOfBounds(t *testing.T) {
	t.Parallel()
	c := &FieldCopier{Field: "val", Source: TupleSourceKey, OrdinalPath: []int{5}}
	out := map[string]any{}
	key := tuple.Tuple{"only-one"}
	ok := c.Copy(out, key, nil)
	if ok {
		t.Fatal("expected copy to refuse on out-of-bounds")
	}
}

func TestToRecord_MultipleFields(t *testing.T) {
	t.Parallel()
	b := NewIndexKeyValueToPartialRecordBuilder()
	b.AddField("name", TupleSourceKey, []int{0})
	b.AddField("age", TupleSourceKey, []int{1})
	b.AddField("score", TupleSourceValue, []int{0})
	r := b.Build()

	key := tuple.Tuple{"Alice", int64(30)}
	val := tuple.Tuple{int64(95)}
	out := r.ToRecord(key, val)
	if out == nil {
		t.Fatal("expected non-nil record")
	}
	if out["NAME"] != "Alice" {
		t.Fatalf("NAME: expected Alice, got %v", out["NAME"])
	}
	if out["AGE"] != int64(30) {
		t.Fatalf("AGE: expected 30, got %v", out["AGE"])
	}
	if out["SCORE"] != int64(95) {
		t.Fatalf("SCORE: expected 95, got %v", out["SCORE"])
	}
}

func TestToRecord_AllRefused_NotRequired(t *testing.T) {
	t.Parallel()
	b := NewIndexKeyValueToPartialRecordBuilder()
	b.AddField("val", TupleSourceKey, []int{0})
	b.SetRequired(false)
	r := b.Build()

	key := tuple.Tuple{nil}
	out := r.ToRecord(key, nil)
	if out != nil {
		t.Fatalf("expected nil when all copiers refuse, got %v", out)
	}
}

func TestToRecord_Required(t *testing.T) {
	t.Parallel()
	b := NewIndexKeyValueToPartialRecordBuilder()
	b.AddField("name", TupleSourceKey, []int{0})
	b.SetRequired(true)
	r := b.Build()

	key := tuple.Tuple{"Alice"}
	out := r.ToRecord(key, nil)
	if out == nil {
		t.Fatal("required record should not be nil")
	}
	if out["NAME"] != "Alice" {
		t.Fatalf("expected Alice, got %v", out["NAME"])
	}
}

func TestGetForOrdinalPath_Empty(t *testing.T) {
	t.Parallel()
	tup := tuple.Tuple{"a", "b"}
	got := getForOrdinalPath(tup, nil)
	if got == nil {
		t.Fatal("empty path should return the tuple itself")
	}
}
