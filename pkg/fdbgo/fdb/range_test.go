package fdb_test

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
)

func TestStrinc_Basic(t *testing.T) {
	t.Parallel()
	got, err := fdb.Strinc([]byte{0x01, 0x02})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []byte{0x01, 0x03}
	if string(got) != string(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStrinc_TrailingFF(t *testing.T) {
	t.Parallel()
	got, err := fdb.Strinc([]byte{0x01, 0xFF})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []byte{0x02}
	if string(got) != string(want) {
		t.Errorf("got %v, want %v (should strip trailing FF and increment)", got, want)
	}
}

func TestStrinc_MultipleTrailingFF(t *testing.T) {
	t.Parallel()
	got, err := fdb.Strinc([]byte{0x01, 0xFF, 0xFF})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []byte{0x02}
	if string(got) != string(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStrinc_AllFF(t *testing.T) {
	t.Parallel()
	_, err := fdb.Strinc([]byte{0xFF, 0xFF})
	if err == nil {
		t.Fatal("expected error for all-0xFF prefix")
	}
}

func TestStrinc_SingleByte(t *testing.T) {
	t.Parallel()
	got, err := fdb.Strinc([]byte{0x00})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != 0x01 {
		t.Errorf("got %v, want [0x01]", got)
	}
}

func TestStrinc_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	input := []byte{0x01, 0x02}
	orig := make([]byte, len(input))
	copy(orig, input)
	_, _ = fdb.Strinc(input)
	if string(input) != string(orig) {
		t.Error("Strinc should not mutate the input slice")
	}
}

func TestFirstGreaterOrEqual(t *testing.T) {
	t.Parallel()
	ks := fdb.FirstGreaterOrEqual(fdb.Key{0x01})
	if ks.OrEqual {
		t.Error("OrEqual should be false")
	}
	if ks.Offset != 1 {
		t.Errorf("Offset = %d, want 1", ks.Offset)
	}
}

func TestFirstGreaterThan(t *testing.T) {
	t.Parallel()
	ks := fdb.FirstGreaterThan(fdb.Key{0x01})
	if !ks.OrEqual {
		t.Error("OrEqual should be true")
	}
	if ks.Offset != 1 {
		t.Errorf("Offset = %d, want 1", ks.Offset)
	}
}

func TestLastLessOrEqual(t *testing.T) {
	t.Parallel()
	ks := fdb.LastLessOrEqual(fdb.Key{0x01})
	if !ks.OrEqual {
		t.Error("OrEqual should be true")
	}
	if ks.Offset != 0 {
		t.Errorf("Offset = %d, want 0", ks.Offset)
	}
}

func TestLastLessThan(t *testing.T) {
	t.Parallel()
	ks := fdb.LastLessThan(fdb.Key{0x01})
	if ks.OrEqual {
		t.Error("OrEqual should be false")
	}
	if ks.Offset != 0 {
		t.Errorf("Offset = %d, want 0", ks.Offset)
	}
}

func TestKeySelector_FDBKeySelector(t *testing.T) {
	t.Parallel()
	ks := fdb.FirstGreaterOrEqual(fdb.Key{0x42})
	got := ks.FDBKeySelector()
	if string(got.Key.FDBKey()) != string(fdb.Key{0x42}) {
		t.Error("FDBKeySelector should return itself")
	}
}

func TestSelectorRange_FDBRangeKeySelectors(t *testing.T) {
	t.Parallel()
	begin := fdb.FirstGreaterOrEqual(fdb.Key{0x01})
	end := fdb.FirstGreaterOrEqual(fdb.Key{0xFF})
	sr := fdb.SelectorRange{Begin: begin, End: end}
	gotB, gotE := sr.FDBRangeKeySelectors()
	if gotB == nil || gotE == nil {
		t.Fatal("selectors should not be nil")
	}
}

func TestKeyRange_FDBRangeKeys(t *testing.T) {
	t.Parallel()
	kr := fdb.KeyRange{Begin: fdb.Key{0x01}, End: fdb.Key{0xFF}}
	b, e := kr.FDBRangeKeys()
	if string(b.FDBKey()) != string(fdb.Key{0x01}) {
		t.Error("begin mismatch")
	}
	if string(e.FDBKey()) != string(fdb.Key{0xFF}) {
		t.Error("end mismatch")
	}
}

func BenchmarkStrinc(b *testing.B) {
	prefix := []byte{0x01, 0x02, 0x03, 0x04}
	for b.Loop() {
		_, _ = fdb.Strinc(prefix)
	}
}

func BenchmarkPrefixRange(b *testing.B) {
	prefix := []byte{0x01, 0x02, 0x03, 0x04}
	for b.Loop() {
		_, _ = fdb.PrefixRange(prefix)
	}
}

func BenchmarkPrintable_ASCII(b *testing.B) {
	data := []byte("hello world this is a test key")
	for b.Loop() {
		_ = fdb.Printable(data)
	}
}

func BenchmarkPrintable_Mixed(b *testing.B) {
	data := []byte{0x01, 0x41, 0x42, 0x00, 0xFF, 0x5C, 0x43}
	for b.Loop() {
		_ = fdb.Printable(data)
	}
}
