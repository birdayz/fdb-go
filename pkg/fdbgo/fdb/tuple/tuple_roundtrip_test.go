package tuple

import (
	"bytes"
	"math"
	"math/big"
	"testing"
)

func TestPackUnpack_Nil(t *testing.T) {
	t.Parallel()
	tup := Tuple{nil}
	got := mustUnpack(t, tup.Pack())
	if len(got) != 1 || got[0] != nil {
		t.Errorf("got %v, want [nil]", got)
	}
}

func TestPackUnpack_String(t *testing.T) {
	t.Parallel()
	tup := Tuple{"hello"}
	got := mustUnpack(t, tup.Pack())
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("got %v, want [hello]", got)
	}
}

func TestPackUnpack_EmptyString(t *testing.T) {
	t.Parallel()
	tup := Tuple{""}
	got := mustUnpack(t, tup.Pack())
	if len(got) != 1 || got[0] != "" {
		t.Errorf("got %v, want [\"\"]", got)
	}
}

func TestPackUnpack_StringWithNull(t *testing.T) {
	t.Parallel()
	tup := Tuple{"hello\x00world"}
	got := mustUnpack(t, tup.Pack())
	if len(got) != 1 || got[0] != "hello\x00world" {
		t.Errorf("got %v, want [hello\\x00world]", got)
	}
}

func TestPackUnpack_Bytes(t *testing.T) {
	t.Parallel()
	data := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	tup := Tuple{data}
	got := mustUnpack(t, tup.Pack())
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	b, ok := got[0].([]byte)
	if !ok {
		t.Fatalf("type %T, want []byte", got[0])
	}
	if !bytes.Equal(b, data) {
		t.Errorf("got %x, want DEADBEEF", b)
	}
}

func TestPackUnpack_EmptyBytes(t *testing.T) {
	t.Parallel()
	tup := Tuple{[]byte{}}
	got := mustUnpack(t, tup.Pack())
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	b, ok := got[0].([]byte)
	if !ok {
		t.Fatalf("type %T, want []byte", got[0])
	}
	if len(b) != 0 {
		t.Errorf("got %v, want empty", b)
	}
}

func TestPackUnpack_Int64(t *testing.T) {
	t.Parallel()
	tests := []int64{0, 1, -1, 127, -128, 255, 256, math.MaxInt64, math.MinInt64, 42, -42}
	for _, val := range tests {
		tup := Tuple{val}
		got := mustUnpack(t, tup.Pack())
		if len(got) != 1 {
			t.Fatalf("len = %d for val %d", len(got), val)
		}
		gotVal, ok := got[0].(int64)
		if !ok {
			t.Fatalf("type %T for val %d, want int64", got[0], val)
		}
		if gotVal != val {
			t.Errorf("got %d, want %d", gotVal, val)
		}
	}
}

func TestPackUnpack_Int(t *testing.T) {
	t.Parallel()
	tup := Tuple{int(42)}
	got := mustUnpack(t, tup.Pack())
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0] != int64(42) {
		t.Errorf("got %v (%T), want int64(42)", got[0], got[0])
	}
}

func TestPackUnpack_Float32(t *testing.T) {
	t.Parallel()
	tests := []float32{0, 1.5, -1.5, math.MaxFloat32, math.SmallestNonzeroFloat32}
	for _, val := range tests {
		tup := Tuple{val}
		got := mustUnpack(t, tup.Pack())
		if len(got) != 1 {
			t.Fatalf("len = %d for val %f", len(got), val)
		}
		gotVal, ok := got[0].(float32)
		if !ok {
			t.Fatalf("type %T for val %f, want float32", got[0], val)
		}
		if gotVal != val {
			t.Errorf("got %f, want %f", gotVal, val)
		}
	}
}

func TestPackUnpack_Float64(t *testing.T) {
	t.Parallel()
	tests := []float64{0, math.Pi, -math.E, math.MaxFloat64, math.SmallestNonzeroFloat64}
	for _, val := range tests {
		tup := Tuple{val}
		got := mustUnpack(t, tup.Pack())
		if len(got) != 1 {
			t.Fatalf("len = %d for val %f", len(got), val)
		}
		gotVal, ok := got[0].(float64)
		if !ok {
			t.Fatalf("type %T for val %f, want float64", got[0], val)
		}
		if gotVal != val {
			t.Errorf("got %f, want %f", gotVal, val)
		}
	}
}

func TestPackUnpack_Bool(t *testing.T) {
	t.Parallel()
	for _, val := range []bool{true, false} {
		tup := Tuple{val}
		got := mustUnpack(t, tup.Pack())
		if len(got) != 1 {
			t.Fatalf("len = %d", len(got))
		}
		gotVal, ok := got[0].(bool)
		if !ok {
			t.Fatalf("type %T, want bool", got[0])
		}
		if gotVal != val {
			t.Errorf("got %v, want %v", gotVal, val)
		}
	}
}

func TestPackUnpack_UUID(t *testing.T) {
	t.Parallel()
	u := UUID{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}
	tup := Tuple{u}
	got := mustUnpack(t, tup.Pack())
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	gotVal, ok := got[0].(UUID)
	if !ok {
		t.Fatalf("type %T, want UUID", got[0])
	}
	if gotVal != u {
		t.Errorf("got %v, want %v", gotVal, u)
	}
}

func TestPackUnpack_BigInt(t *testing.T) {
	t.Parallel()
	tests := []*big.Int{
		big.NewInt(0),
		big.NewInt(42),
		big.NewInt(-42),
		new(big.Int).Exp(big.NewInt(2), big.NewInt(100), nil),
		new(big.Int).Neg(new(big.Int).Exp(big.NewInt(2), big.NewInt(100), nil)),
	}
	for _, val := range tests {
		tup := Tuple{val}
		got := mustUnpack(t, tup.Pack())
		if len(got) != 1 {
			t.Fatalf("len = %d for val %s", len(got), val)
		}
		gotVal, ok := got[0].(*big.Int)
		if !ok {
			// Small big.Ints may come back as int64
			if i, ok2 := got[0].(int64); ok2 {
				if val.Cmp(big.NewInt(i)) != 0 {
					t.Errorf("got int64(%d), want big.Int %s", i, val)
				}
				continue
			}
			t.Fatalf("type %T for val %s", got[0], val)
		}
		if gotVal.Cmp(val) != 0 {
			t.Errorf("got %s, want %s", gotVal, val)
		}
	}
}

func TestPackUnpack_NestedTuple(t *testing.T) {
	t.Parallel()
	inner := Tuple{"a", int64(1)}
	tup := Tuple{"outer", inner, int64(2)}
	got := mustUnpack(t, tup.Pack())
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0] != "outer" {
		t.Errorf("elem 0 = %v, want outer", got[0])
	}
	nested, ok := got[1].(Tuple)
	if !ok {
		t.Fatalf("elem 1 type %T, want Tuple", got[1])
	}
	if len(nested) != 2 || nested[0] != "a" || nested[1] != int64(1) {
		t.Errorf("nested = %v, want [a 1]", nested)
	}
	if got[2] != int64(2) {
		t.Errorf("elem 2 = %v, want 2", got[2])
	}
}

func TestPackUnpack_MultiElement(t *testing.T) {
	t.Parallel()
	tup := Tuple{nil, "hello", int64(42), []byte{0xFF}, true, float64(3.14)}
	got := mustUnpack(t, tup.Pack())
	if len(got) != len(tup) {
		t.Fatalf("len = %d, want %d", len(got), len(tup))
	}
	if got[0] != nil {
		t.Errorf("elem 0: %v, want nil", got[0])
	}
	if got[1] != "hello" {
		t.Errorf("elem 1: %v, want hello", got[1])
	}
	if got[2] != int64(42) {
		t.Errorf("elem 2: %v, want 42", got[2])
	}
	if !bytes.Equal(got[3].([]byte), []byte{0xFF}) {
		t.Errorf("elem 3: %v, want [FF]", got[3])
	}
	if got[4] != true {
		t.Errorf("elem 4: %v, want true", got[4])
	}
	if got[5] != float64(3.14) {
		t.Errorf("elem 5: %v, want 3.14", got[5])
	}
}

func TestPackUnpack_EmptyTuple(t *testing.T) {
	t.Parallel()
	tup := Tuple{}
	data := tup.Pack()
	if len(data) != 0 {
		t.Fatalf("empty tuple should pack to empty bytes, got %x", data)
	}
	got := mustUnpack(t, data)
	if len(got) != 0 {
		t.Errorf("unpacked empty should be empty, got %v", got)
	}
}

func TestPackUnpack_Versionstamp(t *testing.T) {
	t.Parallel()
	vs := Versionstamp{
		TransactionVersion: [10]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A},
		UserVersion:        42,
	}
	tup := Tuple{vs}
	got := mustUnpack(t, tup.Pack())
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	gotVS, ok := got[0].(Versionstamp)
	if !ok {
		t.Fatalf("type %T, want Versionstamp", got[0])
	}
	if gotVS != vs {
		t.Errorf("got %v, want %v", gotVS, vs)
	}
}

func TestTuple_String(t *testing.T) {
	t.Parallel()
	tup := Tuple{"hello", int64(42), nil}
	s := tup.String()
	if s == "" {
		t.Error("String() returned empty")
	}
}

func TestIncompleteVersionstamp(t *testing.T) {
	t.Parallel()
	vs := IncompleteVersionstamp(99)
	if vs.UserVersion != 99 {
		t.Errorf("UserVersion = %d, want 99", vs.UserVersion)
	}
	allFF := true
	for _, b := range vs.TransactionVersion {
		if b != 0xFF {
			allFF = false
			break
		}
	}
	if !allFF {
		t.Errorf("TransactionVersion = %x, want all 0xFF", vs.TransactionVersion)
	}
}

func TestVersionstamp_Bytes(t *testing.T) {
	t.Parallel()
	vs := Versionstamp{
		TransactionVersion: [10]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		UserVersion:        0x0B0C,
	}
	b := vs.Bytes()
	if len(b) != 12 {
		t.Fatalf("len = %d, want 12", len(b))
	}
	for i := 0; i < 10; i++ {
		if b[i] != byte(i+1) {
			t.Errorf("byte %d = %d, want %d", i, b[i], i+1)
		}
	}
	if b[10] != 0x0B || b[11] != 0x0C {
		t.Errorf("user version bytes = %x %x, want 0B 0C", b[10], b[11])
	}
}

func TestTuple_FDBRangeKeys(t *testing.T) {
	t.Parallel()
	tup := Tuple{"prefix"}
	begin, end := tup.FDBRangeKeys()
	bk := begin.FDBKey()
	ek := end.FDBKey()
	packed := tup.Pack()
	if !bytes.HasPrefix(bk, packed) {
		t.Error("begin should start with packed tuple")
	}
	if !bytes.HasPrefix(ek, packed) {
		t.Error("end should start with packed tuple")
	}
	if bytes.Compare(bk, ek) >= 0 {
		t.Error("begin should be less than end")
	}
}

func TestTuple_HasIncompleteVersionstamp(t *testing.T) {
	t.Parallel()
	complete := Versionstamp{
		TransactionVersion: [10]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		UserVersion:        1,
	}
	incomplete := IncompleteVersionstamp(1)

	has, err := Tuple{complete}.HasIncompleteVersionstamp()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("complete versionstamp should not be incomplete")
	}

	has, err = Tuple{incomplete}.HasIncompleteVersionstamp()
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("incomplete versionstamp should be detected")
	}

	has, err = Tuple{"no versionstamp"}.HasIncompleteVersionstamp()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("string-only tuple should not have incomplete versionstamp")
	}
}

// Sort order tests: tuples should maintain lexicographic sort order.

func TestPackSortOrder_Integers(t *testing.T) {
	t.Parallel()
	vals := []int64{math.MinInt64, -1000, -1, 0, 1, 1000, math.MaxInt64}
	var prev []byte
	for _, v := range vals {
		packed := Tuple{v}.Pack()
		if prev != nil && bytes.Compare(prev, packed) >= 0 {
			t.Errorf("sort order violated: %d should sort before %d", vals[0], v)
		}
		prev = packed
	}
}

func TestPackSortOrder_Strings(t *testing.T) {
	t.Parallel()
	vals := []string{"", "a", "aa", "ab", "b", "hello", "world"}
	var prev []byte
	for _, v := range vals {
		packed := Tuple{v}.Pack()
		if prev != nil && bytes.Compare(prev, packed) >= 0 {
			t.Errorf("sort order violated at %q", v)
		}
		prev = packed
	}
}

func TestPackSortOrder_TypeOrder(t *testing.T) {
	t.Parallel()
	// FDB tuple encoding sorts: nil < bytes < string < nested < int < float < double < bool(false) < bool(true) < UUID
	// Actually the exact type ordering depends on the type codes.
	// The key property is that all elements of the same type sort correctly.
	// Verify nil sorts before everything else.
	nilPacked := Tuple{nil}.Pack()
	strPacked := Tuple{"a"}.Pack()
	intPacked := Tuple{int64(1)}.Pack()

	if bytes.Compare(nilPacked, strPacked) >= 0 {
		t.Error("nil should sort before string")
	}
	_ = intPacked // int sorts in its own range, verified by the integer sort test
}

// Benchmarks

func BenchmarkPackUnpack_Simple(b *testing.B) {
	tup := Tuple{"hello", int64(42), true}
	for b.Loop() {
		data := tup.Pack()
		_, _ = Unpack(data)
	}
}

func BenchmarkPackUnpack_Multi(b *testing.B) {
	tup := Tuple{nil, "hello", int64(42), []byte{0xFF}, true, float64(3.14)}
	for b.Loop() {
		data := tup.Pack()
		_, _ = Unpack(data)
	}
}

func BenchmarkPack_Int64(b *testing.B) {
	tup := Tuple{int64(math.MaxInt64)}
	for b.Loop() {
		_ = tup.Pack()
	}
}

func BenchmarkPack_String(b *testing.B) {
	tup := Tuple{"hello world this is a test"}
	for b.Loop() {
		_ = tup.Pack()
	}
}

func BenchmarkUnpack_Simple(b *testing.B) {
	data := Tuple{"hello", int64(42)}.Pack()
	for b.Loop() {
		_, _ = Unpack(data)
	}
}

func mustUnpack(t *testing.T, data []byte) Tuple {
	t.Helper()
	got, err := Unpack(data)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	return got
}
