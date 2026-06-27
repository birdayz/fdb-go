package subspace

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"github.com/onsi/gomega"
)

func TestSubspaceString(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ss := Sub("test")
	// String() is on the concrete type; use fmt.Sprint to invoke it via Stringer
	str := fmt.Sprint(ss)
	g.Expect(str).To(gomega.ContainSubstring("Subspace"))
}

func TestSubspaceContains(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ss := Sub("myapp")
	key := ss.Pack(tuple.Tuple{int64(42)})

	g.Expect(ss.Contains(key)).To(gomega.BeTrue())
	g.Expect(ss.Contains(fdb.Key([]byte{0x01, 0x02}))).To(gomega.BeFalse())
}

func TestSubspaceUnpackError(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ss := Sub("myapp")
	// Key from a different subspace — should error
	other := Sub("other")
	key := other.Pack(tuple.Tuple{int64(1)})

	_, err := ss.Unpack(key)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("not in subspace"))
}

func TestSubspaceFDBRangeKeySelectors(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	ss := Sub("test")
	begin, end := ss.FDBRangeKeySelectors()
	g.Expect(begin).NotTo(gomega.BeNil())
	g.Expect(end).NotTo(gomega.BeNil())
}

func TestAllKeys_EmptyPrefix(t *testing.T) {
	t.Parallel()
	ss := AllKeys()
	if len(ss.Bytes()) != 0 {
		t.Fatalf("AllKeys prefix should be empty, got %v", ss.Bytes())
	}
}

func TestAllKeys_ContainsAnyKey(t *testing.T) {
	t.Parallel()
	ss := AllKeys()
	for _, key := range []fdb.Key{{0x00}, {0xFF}, {0x01, 0x02, 0x03}} {
		if !ss.Contains(key) {
			t.Errorf("AllKeys should contain %v", key)
		}
	}
}

func TestSub_SingleElement(t *testing.T) {
	t.Parallel()
	ss := Sub("myapp")
	packed := tuple.Tuple{"myapp"}.Pack()
	if string(ss.Bytes()) != string(packed) {
		t.Errorf("prefix = %v, want %v", ss.Bytes(), packed)
	}
}

func TestSub_MultipleElements(t *testing.T) {
	t.Parallel()
	ss := Sub("app", int64(1))
	packed := tuple.Tuple{"app", int64(1)}.Pack()
	if string(ss.Bytes()) != string(packed) {
		t.Errorf("prefix = %v, want %v", ss.Bytes(), packed)
	}
}

func TestFromBytes_CopiesInput(t *testing.T) {
	t.Parallel()
	orig := []byte{0x01, 0x02, 0x03}
	ss := FromBytes(orig)
	orig[0] = 0xFF
	if ss.Bytes()[0] == 0xFF {
		t.Error("FromBytes must copy the input, not alias it")
	}
}

func TestFromBytes_Roundtrip(t *testing.T) {
	t.Parallel()
	prefix := []byte{0xAA, 0xBB, 0xCC}
	ss := FromBytes(prefix)
	if string(ss.Bytes()) != string(prefix) {
		t.Errorf("got %v, want %v", ss.Bytes(), prefix)
	}
}

func TestSubspace_Sub_ExtendsPrefix(t *testing.T) {
	t.Parallel()
	root := Sub("root")
	child := root.Sub("child")
	if len(child.Bytes()) <= len(root.Bytes()) {
		t.Error("child should be longer than root")
	}
	for i := range root.Bytes() {
		if child.Bytes()[i] != root.Bytes()[i] {
			t.Errorf("child prefix diverges at byte %d", i)
			break
		}
	}
}

func TestSubspace_PackUnpack_Roundtrip(t *testing.T) {
	t.Parallel()
	ss := Sub("app")
	original := tuple.Tuple{"record", int64(42)}
	key := ss.Pack(original)
	unpacked, err := ss.Unpack(fdb.Key(key))
	if err != nil {
		t.Fatalf("Unpack error: %v", err)
	}
	if len(unpacked) != 2 {
		t.Fatalf("len = %d, want 2", len(unpacked))
	}
	if unpacked[0].(string) != "record" {
		t.Errorf("elem 0 = %v, want \"record\"", unpacked[0])
	}
	if unpacked[1].(int64) != 42 {
		t.Errorf("elem 1 = %v, want 42", unpacked[1])
	}
}

func TestSubspace_PackUnpack_EmptyTuple(t *testing.T) {
	t.Parallel()
	ss := Sub("app")
	key := ss.Pack(tuple.Tuple{})
	unpacked, err := ss.Unpack(fdb.Key(key))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unpacked) != 0 {
		t.Errorf("expected empty tuple, got %v", unpacked)
	}
}

func TestSubspace_FDBKey_EqualBytes(t *testing.T) {
	t.Parallel()
	ss := Sub("test")
	if string(ss.FDBKey()) != string(ss.Bytes()) {
		t.Error("FDBKey should equal Bytes")
	}
}

func TestSubspace_FDBRangeKeys_Boundaries(t *testing.T) {
	t.Parallel()
	ss := Sub("test")
	begin, end := ss.FDBRangeKeys()
	bk := begin.FDBKey()
	ek := end.FDBKey()
	if bk[len(bk)-1] != 0x00 {
		t.Errorf("begin should end with 0x00, got 0x%02x", bk[len(bk)-1])
	}
	if ek[len(ek)-1] != 0xFF {
		t.Errorf("end should end with 0xFF, got 0x%02x", ek[len(ek)-1])
	}
}

func TestSubspace_NestedSub_ThreeLevels(t *testing.T) {
	t.Parallel()
	l0 := Sub("app")
	l1 := l0.Sub("db")
	l2 := l1.Sub("schema")
	if len(l2.Bytes()) <= len(l1.Bytes()) {
		t.Error("l2 should be longer than l1")
	}
	if len(l1.Bytes()) <= len(l0.Bytes()) {
		t.Error("l1 should be longer than l0")
	}
	key := l2.Pack(tuple.Tuple{"rec"})
	if !l2.Contains(fdb.Key(key)) {
		t.Error("l2 should contain its own key")
	}
	if !l1.Contains(fdb.Key(key)) {
		t.Error("l1 should contain l2's key (prefix nesting)")
	}
	if !l0.Contains(fdb.Key(key)) {
		t.Error("l0 should contain l2's key (prefix nesting)")
	}
}

func TestSubspace_Contains_EmptyKey(t *testing.T) {
	t.Parallel()
	ss := Sub("app")
	if ss.Contains(fdb.Key(nil)) {
		t.Error("non-empty subspace should not contain nil key")
	}
	if ss.Contains(fdb.Key([]byte{})) {
		t.Error("non-empty subspace should not contain empty key")
	}
}

func BenchmarkSub(b *testing.B) {
	for b.Loop() {
		_ = Sub("myapp", int64(42))
	}
}

func BenchmarkPack(b *testing.B) {
	ss := Sub("myapp")
	t := tuple.Tuple{"record", int64(42)}
	for b.Loop() {
		_ = ss.Pack(t)
	}
}

func BenchmarkUnpack(b *testing.B) {
	ss := Sub("myapp")
	key := fdb.Key(ss.Pack(tuple.Tuple{"record", int64(42)}))
	for b.Loop() {
		_, _ = ss.Unpack(key)
	}
}

func BenchmarkContains(b *testing.B) {
	ss := Sub("myapp")
	key := fdb.Key(ss.Pack(tuple.Tuple{"record", int64(42)}))
	for b.Loop() {
		_ = ss.Contains(key)
	}
}
