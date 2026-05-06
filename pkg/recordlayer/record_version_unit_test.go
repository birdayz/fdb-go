package recordlayer

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

func rvMakeGlobal(b ...byte) []byte {
	g := make([]byte, GlobalVersionBytes)
	copy(g, b)
	return g
}

func rvGlobalAllFF() []byte {
	g := make([]byte, GlobalVersionBytes)
	for i := range g {
		g[i] = 0xFF
	}
	return g
}

func TestNewCompleteVersion(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		gv := rvMakeGlobal(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
		v, err := NewCompleteVersion(gv, 7)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.IsComplete() {
			t.Fatal("expected complete")
		}
		if v.GetLocalVersion() != 7 {
			t.Fatalf("local version: got %d, want 7", v.GetLocalVersion())
		}
	})

	t.Run("wrong length global", func(t *testing.T) {
		t.Parallel()
		for _, n := range []int{0, 3, 9, 11} {
			_, err := NewCompleteVersion(make([]byte, n), 0)
			if err == nil {
				t.Errorf("expected error for global len %d", n)
			}
		}
	})

	t.Run("incomplete marker rejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewCompleteVersion(rvGlobalAllFF(), 0)
		if err == nil {
			t.Fatal("expected error for all-0xFF global")
		}
	})

	t.Run("local version bounds", func(t *testing.T) {
		t.Parallel()
		gv := rvMakeGlobal(1)
		cases := []struct {
			local   int
			wantErr bool
		}{
			{0, false},
			{1, false},
			{0xFFFF, false},
			{-1, true},
			{0x10000, true},
		}
		for _, tc := range cases {
			_, err := NewCompleteVersion(gv, tc.local)
			if tc.wantErr && err == nil {
				t.Errorf("local=%d: expected error", tc.local)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("local=%d: unexpected error: %v", tc.local, err)
			}
		}
	})
}

func TestCompleteVersionFromBytes(t *testing.T) {
	t.Parallel()

	t.Run("valid 12 bytes", func(t *testing.T) {
		t.Parallel()
		b := make([]byte, VersionBytes)
		b[0] = 1
		b[11] = 5
		v, err := CompleteVersionFromBytes(b)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.IsComplete() {
			t.Fatal("expected complete")
		}
	})

	t.Run("wrong length", func(t *testing.T) {
		t.Parallel()
		for _, n := range []int{0, 11, 13, 1} {
			_, err := CompleteVersionFromBytes(make([]byte, n))
			if err == nil {
				t.Errorf("len=%d: expected error", n)
			}
		}
	})

	t.Run("incomplete marker rejected", func(t *testing.T) {
		t.Parallel()
		b := make([]byte, VersionBytes)
		for i := 0; i < GlobalVersionBytes; i++ {
			b[i] = 0xFF
		}
		_, err := CompleteVersionFromBytes(b)
		if err == nil {
			t.Fatal("expected error for all-0xFF global bytes")
		}
	})
}

func TestIncompleteVersion(t *testing.T) {
	t.Parallel()

	t.Run("valid global marker", func(t *testing.T) {
		t.Parallel()
		v, err := IncompleteVersion(42)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v.IsComplete() {
			t.Fatal("expected incomplete")
		}
		if v.GetLocalVersion() != 42 {
			t.Fatalf("local: got %d, want 42", v.GetLocalVersion())
		}
		raw := v.ToBytes()
		for i := 0; i < GlobalVersionBytes; i++ {
			if raw[i] != 0xFF {
				t.Fatalf("byte %d: got %02x, want 0xFF", i, raw[i])
			}
		}
	})

	t.Run("bounds", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			local   int
			wantErr bool
		}{
			{0, false},
			{0xFFFF, false},
			{-1, true},
			{0x10000, true},
		}
		for _, tc := range cases {
			_, err := IncompleteVersion(tc.local)
			if tc.wantErr && err == nil {
				t.Errorf("local=%d: expected error", tc.local)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("local=%d: unexpected error: %v", tc.local, err)
			}
		}
	})
}

func TestIsComplete(t *testing.T) {
	t.Parallel()

	c, _ := NewCompleteVersion(rvMakeGlobal(1), 0)
	if !c.IsComplete() {
		t.Fatal("complete version: IsComplete should be true")
	}

	inc, _ := IncompleteVersion(0)
	if inc.IsComplete() {
		t.Fatal("incomplete version: IsComplete should be false")
	}
}

func TestGetLocalVersion(t *testing.T) {
	t.Parallel()

	gv := rvMakeGlobal(1)
	for _, lv := range []int{0, 1, 0xFFFF} {
		v, err := NewCompleteVersion(gv, lv)
		if err != nil {
			t.Fatalf("complete local=%d: unexpected error: %v", lv, err)
		}
		if got := v.GetLocalVersion(); got != lv {
			t.Errorf("complete local=%d: got %d", lv, got)
		}

		iv, err := IncompleteVersion(lv)
		if err != nil {
			t.Fatalf("incomplete local=%d: unexpected error: %v", lv, err)
		}
		if got := iv.GetLocalVersion(); got != lv {
			t.Errorf("incomplete local=%d: got %d", lv, got)
		}
	}
}

func TestGetGlobalVersion(t *testing.T) {
	t.Parallel()

	t.Run("complete", func(t *testing.T) {
		t.Parallel()
		gv := rvMakeGlobal(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
		v, _ := NewCompleteVersion(gv, 0)
		got, err := v.GetGlobalVersion()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i, b := range gv {
			if got[i] != b {
				t.Fatalf("byte %d: got %02x, want %02x", i, got[i], b)
			}
		}
	})

	t.Run("incomplete errors", func(t *testing.T) {
		t.Parallel()
		v, _ := IncompleteVersion(0)
		_, err := v.GetGlobalVersion()
		if err == nil {
			t.Fatal("expected error for incomplete version")
		}
	})
}

func TestGetDBVersion(t *testing.T) {
	t.Parallel()

	t.Run("complete big-endian", func(t *testing.T) {
		t.Parallel()
		gv := make([]byte, GlobalVersionBytes)
		binary.BigEndian.PutUint64(gv[:8], 123456789)
		v, _ := NewCompleteVersion(gv, 0)
		dbv, err := v.GetDBVersion()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dbv != 123456789 {
			t.Fatalf("got %d, want 123456789", dbv)
		}
	})

	t.Run("incomplete errors", func(t *testing.T) {
		t.Parallel()
		v, _ := IncompleteVersion(0)
		_, err := v.GetDBVersion()
		if err == nil {
			t.Fatal("expected error for incomplete version")
		}
	})
}

func TestToBytes_RoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("complete round-trip", func(t *testing.T) {
		t.Parallel()
		gv := rvMakeGlobal(0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x11, 0x22, 0x33, 0x44, 0x55)
		v, err := NewCompleteVersion(gv, 0x1234)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		b := v.ToBytes()
		if len(b) != VersionBytes {
			t.Fatalf("len: got %d, want %d", len(b), VersionBytes)
		}
		v2, err := CompleteVersionFromBytes(b)
		if err != nil {
			t.Fatalf("round-trip error: %v", err)
		}
		if !v.Equal(v2) {
			t.Fatal("round-trip: not equal")
		}
	})

	t.Run("incomplete bytes layout", func(t *testing.T) {
		t.Parallel()
		v, _ := IncompleteVersion(99)
		b := v.ToBytes()
		if len(b) != VersionBytes {
			t.Fatalf("len: got %d, want %d", len(b), VersionBytes)
		}
		for i := 0; i < GlobalVersionBytes; i++ {
			if b[i] != 0xFF {
				t.Fatalf("byte %d: got %02x, want 0xFF", i, b[i])
			}
		}
		lv := int(binary.BigEndian.Uint16(b[GlobalVersionBytes:]))
		if lv != 99 {
			t.Fatalf("local: got %d, want 99", lv)
		}
	})
}

func TestEqual(t *testing.T) {
	t.Parallel()

	gv1 := rvMakeGlobal(1, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	gv2 := rvMakeGlobal(2, 0, 0, 0, 0, 0, 0, 0, 0, 0)

	v1, _ := NewCompleteVersion(gv1, 5)
	v1b, _ := NewCompleteVersion(gv1, 5)
	v2, _ := NewCompleteVersion(gv2, 5)
	v3, _ := NewCompleteVersion(gv1, 6)

	inc1, _ := IncompleteVersion(5)
	inc1b, _ := IncompleteVersion(5)
	inc2, _ := IncompleteVersion(6)

	t.Run("same complete", func(t *testing.T) {
		t.Parallel()
		if !v1.Equal(v1b) {
			t.Fatal("same complete versions should be equal")
		}
	})

	t.Run("different complete global", func(t *testing.T) {
		t.Parallel()
		if v1.Equal(v2) {
			t.Fatal("different global: should not be equal")
		}
	})

	t.Run("different complete local", func(t *testing.T) {
		t.Parallel()
		if v1.Equal(v3) {
			t.Fatal("different local: should not be equal")
		}
	})

	t.Run("same incomplete local", func(t *testing.T) {
		t.Parallel()
		if !inc1.Equal(inc1b) {
			t.Fatal("same incomplete local should be equal")
		}
	})

	t.Run("different incomplete local", func(t *testing.T) {
		t.Parallel()
		if inc1.Equal(inc2) {
			t.Fatal("different incomplete local should not be equal")
		}
	})

	t.Run("nil both", func(t *testing.T) {
		t.Parallel()
		var a, b *FDBRecordVersion
		if !a.Equal(b) {
			t.Fatal("nil == nil")
		}
	})

	t.Run("nil one side", func(t *testing.T) {
		t.Parallel()
		var a *FDBRecordVersion
		if a.Equal(v1) {
			t.Fatal("nil should not equal non-nil")
		}
		if v1.Equal(a) {
			t.Fatal("non-nil should not equal nil")
		}
	})

	t.Run("complete != incomplete", func(t *testing.T) {
		t.Parallel()
		if v1.Equal(inc1) {
			t.Fatal("complete and incomplete should not be equal")
		}
		if inc1.Equal(v1) {
			t.Fatal("incomplete and complete should not be equal")
		}
	})
}

func TestLess(t *testing.T) {
	t.Parallel()

	gv1 := rvMakeGlobal(1, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	gv2 := rvMakeGlobal(2, 0, 0, 0, 0, 0, 0, 0, 0, 0)

	c1, _ := NewCompleteVersion(gv1, 0)
	c2, _ := NewCompleteVersion(gv2, 0)
	c1lo, _ := NewCompleteVersion(gv1, 4)
	c1hi, _ := NewCompleteVersion(gv1, 5)

	inc0, _ := IncompleteVersion(0)
	inc1, _ := IncompleteVersion(1)

	t.Run("complete before incomplete", func(t *testing.T) {
		t.Parallel()
		if !c1.Less(inc0) {
			t.Fatal("complete should sort before incomplete")
		}
		if inc0.Less(c1) {
			t.Fatal("incomplete should not sort before complete")
		}
	})

	t.Run("lex within complete by global", func(t *testing.T) {
		t.Parallel()
		if !c1.Less(c2) {
			t.Fatal("c1 < c2 expected")
		}
		if c2.Less(c1) {
			t.Fatal("c2 > c1 expected")
		}
	})

	t.Run("lex within complete by local", func(t *testing.T) {
		t.Parallel()
		if !c1lo.Less(c1hi) {
			t.Fatal("same global, lower local should be less")
		}
	})

	t.Run("equal is not less", func(t *testing.T) {
		t.Parallel()
		if c1.Less(c1) {
			t.Fatal("v.Less(v) should be false")
		}
		if inc0.Less(inc0) {
			t.Fatal("v.Less(v) should be false for incomplete")
		}
	})

	t.Run("lex within incomplete", func(t *testing.T) {
		t.Parallel()
		if !inc0.Less(inc1) {
			t.Fatal("inc0 < inc1 expected")
		}
		if inc1.Less(inc0) {
			t.Fatal("inc1 > inc0 expected")
		}
	})

	t.Run("nil handling", func(t *testing.T) {
		t.Parallel()
		var nilV *FDBRecordVersion
		if !nilV.Less(c1) {
			t.Fatal("nil should be less than non-nil")
		}
		if c1.Less(nilV) {
			t.Fatal("non-nil should not be less than nil")
		}
		if nilV.Less(nilV) {
			t.Fatal("nil.Less(nil) should be false")
		}
	})
}

func TestString(t *testing.T) {
	t.Parallel()

	t.Run("complete", func(t *testing.T) {
		t.Parallel()
		gv := rvMakeGlobal(0, 0, 0, 0, 0, 0, 0, 0, 0, 1)
		v, _ := NewCompleteVersion(gv, 0)
		s := v.String()
		if !strings.HasPrefix(s, "FDBRecordVersion(") {
			t.Errorf("unexpected format: %s", s)
		}
		if !strings.Contains(s, "complete=true") {
			t.Errorf("string should contain complete=true: %s", s)
		}
	})

	t.Run("incomplete", func(t *testing.T) {
		t.Parallel()
		v, _ := IncompleteVersion(0)
		s := v.String()
		if !strings.Contains(s, "complete=false") {
			t.Errorf("string should contain complete=false: %s", s)
		}
	})

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		var v *FDBRecordVersion
		if v.String() != "FDBRecordVersion(nil)" {
			t.Errorf("unexpected nil string: %s", v.String())
		}
	})
}

func TestMinVersion(t *testing.T) {
	t.Parallel()
	v := MinVersion()
	if !v.IsComplete() {
		t.Fatal("MinVersion should be complete")
	}
	b := v.ToBytes()
	for i, byt := range b {
		if byt != 0x00 {
			t.Fatalf("byte %d: got %02x, want 0x00", i, byt)
		}
	}
}

func TestMaxVersion(t *testing.T) {
	t.Parallel()
	v := MaxVersion()
	if !v.IsComplete() {
		t.Fatal("MaxVersion should be complete")
	}
	b := v.ToBytes()
	for i := 0; i < 9; i++ {
		if b[i] != 0xFF {
			t.Fatalf("byte %d: got %02x, want 0xFF", i, b[i])
		}
	}
	if b[9] != 0xFE {
		t.Fatalf("byte 9: got %02x, want 0xFE", b[9])
	}
	if b[10] != 0xFF || b[11] != 0xFF {
		t.Fatalf("local bytes: got %02x %02x, want FF FF", b[10], b[11])
	}
}

func TestMinMaxVersionOrdering(t *testing.T) {
	t.Parallel()
	if !MinVersion().Less(MaxVersion()) {
		t.Fatal("MinVersion should be less than MaxVersion")
	}
}

func TestFirstInDBVersion(t *testing.T) {
	t.Parallel()
	v := FirstInDBVersion(42)
	if !v.IsComplete() {
		t.Fatal("should be complete")
	}
	b := v.ToBytes()
	dbv := int64(binary.BigEndian.Uint64(b[:8]))
	if dbv != 42 {
		t.Fatalf("db version: got %d, want 42", dbv)
	}
	for i := 8; i < VersionBytes; i++ {
		if b[i] != 0x00 {
			t.Fatalf("byte %d: got %02x, want 0x00", i, b[i])
		}
	}
}

func TestLastInDBVersion(t *testing.T) {
	t.Parallel()
	v := LastInDBVersion(42)
	if !v.IsComplete() {
		t.Fatal("should be complete")
	}
	b := v.ToBytes()
	dbv := int64(binary.BigEndian.Uint64(b[:8]))
	if dbv != 42 {
		t.Fatalf("db version: got %d, want 42", dbv)
	}
	for i := 8; i < VersionBytes; i++ {
		if b[i] != 0xFF {
			t.Fatalf("byte %d: got %02x, want 0xFF", i, b[i])
		}
	}
}

func TestFirstLastInDBVersionOrdering(t *testing.T) {
	t.Parallel()
	first := FirstInDBVersion(100)
	last := LastInDBVersion(100)
	if !first.Less(last) {
		t.Fatal("first should be less than last for same DB version")
	}
}

func TestFirstInGlobalVersion(t *testing.T) {
	t.Parallel()
	gv := rvMakeGlobal(0, 0, 0, 0, 0, 0, 0, 0, 0, 1)
	v, err := FirstInGlobalVersion(gv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.GetLocalVersion() != 0 {
		t.Fatalf("local: got %d, want 0", v.GetLocalVersion())
	}
	got, _ := v.GetGlobalVersion()
	for i, b := range gv {
		if got[i] != b {
			t.Fatalf("global byte %d: got %02x, want %02x", i, got[i], b)
		}
	}
}

func TestLastInGlobalVersion(t *testing.T) {
	t.Parallel()
	gv := rvMakeGlobal(0, 0, 0, 0, 0, 0, 0, 0, 0, 1)
	v, err := LastInGlobalVersion(gv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.GetLocalVersion() != 0xFFFF {
		t.Fatalf("local: got %d, want 0xFFFF", v.GetLocalVersion())
	}
}

func TestFirstLastInGlobalVersionOrdering(t *testing.T) {
	t.Parallel()
	gv := rvMakeGlobal(0, 0, 0, 0, 0, 0, 0, 0, 0, 1)
	first, _ := FirstInGlobalVersion(gv)
	last, _ := LastInGlobalVersion(gv)
	if !first.Less(last) {
		t.Fatal("first should be less than last for same global version")
	}
}

func TestNext(t *testing.T) {
	t.Parallel()

	t.Run("complete increments local", func(t *testing.T) {
		t.Parallel()
		gv := rvMakeGlobal(0, 0, 0, 0, 0, 0, 0, 0, 0, 1)
		v, _ := NewCompleteVersion(gv, 5)
		next, err := v.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !next.IsComplete() {
			t.Fatal("next should be complete")
		}
		if next.GetLocalVersion() != 6 {
			t.Fatalf("next local: got %d, want 6", next.GetLocalVersion())
		}
		if !v.Less(next) {
			t.Fatal("v should be less than v.Next()")
		}
	})

	t.Run("carry propagation into global", func(t *testing.T) {
		t.Parallel()
		gv := rvMakeGlobal(0, 0, 0, 0, 0, 0, 0, 0, 0, 1)
		v, _ := NewCompleteVersion(gv, 0xFFFF)
		next, err := v.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		b := next.ToBytes()
		if b[9] != 2 {
			t.Fatalf("byte 9 after carry: got %02x, want 0x02", b[9])
		}
		if b[10] != 0 || b[11] != 0 {
			t.Fatalf("local bytes after carry: got %02x %02x, want 00 00", b[10], b[11])
		}
	})

	t.Run("max version overflow errors", func(t *testing.T) {
		t.Parallel()
		_, err := MaxVersion().Next()
		if err == nil {
			t.Fatal("expected error for max version increment")
		}
	})

	t.Run("incomplete increments local", func(t *testing.T) {
		t.Parallel()
		v, _ := IncompleteVersion(3)
		next, err := v.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if next.IsComplete() {
			t.Fatal("next of incomplete should be incomplete")
		}
		if next.GetLocalVersion() != 4 {
			t.Fatalf("next local: got %d, want 4", next.GetLocalVersion())
		}
	})

	t.Run("incomplete at max local errors", func(t *testing.T) {
		t.Parallel()
		v, _ := IncompleteVersion(0xFFFF)
		_, err := v.Next()
		if err == nil {
			t.Fatal("expected error for max incomplete local")
		}
	})
}

func TestPrev(t *testing.T) {
	t.Parallel()

	t.Run("complete decrements local", func(t *testing.T) {
		t.Parallel()
		gv := rvMakeGlobal(0, 0, 0, 0, 0, 0, 0, 0, 0, 1)
		v, _ := NewCompleteVersion(gv, 5)
		prev, err := v.Prev()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !prev.IsComplete() {
			t.Fatal("prev should be complete")
		}
		if prev.GetLocalVersion() != 4 {
			t.Fatalf("prev local: got %d, want 4", prev.GetLocalVersion())
		}
		if !prev.Less(v) {
			t.Fatal("v.Prev() should be less than v")
		}
	})

	t.Run("borrow propagation from global", func(t *testing.T) {
		t.Parallel()
		gv := rvMakeGlobal(0, 0, 0, 0, 0, 0, 0, 0, 0, 2)
		v, _ := NewCompleteVersion(gv, 0)
		prev, err := v.Prev()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		b := prev.ToBytes()
		if b[9] != 1 {
			t.Fatalf("byte 9 after borrow: got %02x, want 0x01", b[9])
		}
		if b[10] != 0xFF || b[11] != 0xFF {
			t.Fatalf("local bytes after borrow: got %02x %02x, want FF FF", b[10], b[11])
		}
	})

	t.Run("min version underflow errors", func(t *testing.T) {
		t.Parallel()
		_, err := MinVersion().Prev()
		if err == nil {
			t.Fatal("expected error for min version decrement")
		}
	})

	t.Run("incomplete decrements local", func(t *testing.T) {
		t.Parallel()
		v, _ := IncompleteVersion(3)
		prev, err := v.Prev()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if prev.IsComplete() {
			t.Fatal("prev of incomplete should be incomplete")
		}
		if prev.GetLocalVersion() != 2 {
			t.Fatalf("prev local: got %d, want 2", prev.GetLocalVersion())
		}
	})

	t.Run("incomplete local=0 errors", func(t *testing.T) {
		t.Parallel()
		v, _ := IncompleteVersion(0)
		_, err := v.Prev()
		if err == nil {
			t.Fatal("expected error for incomplete local=0")
		}
	})
}

func TestFromVersionstampToVersionstampRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("round-trip", func(t *testing.T) {
		t.Parallel()
		var tv [10]byte
		tv[0] = 0xAB
		tv[9] = 0x01
		vs := tuple.Versionstamp{
			TransactionVersion: tv,
			UserVersion:        0x1234,
		}
		v := FromVersionstamp(vs)
		if !v.IsComplete() {
			t.Fatal("FromVersionstamp should produce complete version")
		}
		if v.GetLocalVersion() != 0x1234 {
			t.Fatalf("local: got %d, want 0x1234", v.GetLocalVersion())
		}
		gv, err := v.GetGlobalVersion()
		if err != nil {
			t.Fatalf("GetGlobalVersion error: %v", err)
		}
		for i, b := range tv {
			if gv[i] != b {
				t.Fatalf("global byte %d: got %02x, want %02x", i, gv[i], b)
			}
		}
		vs2, err := v.ToVersionstamp()
		if err != nil {
			t.Fatalf("ToVersionstamp error: %v", err)
		}
		if vs2.TransactionVersion != tv {
			t.Fatal("TransactionVersion mismatch after round-trip")
		}
		if vs2.UserVersion != 0x1234 {
			t.Fatalf("UserVersion: got %d, want 0x1234", vs2.UserVersion)
		}
	})

	t.Run("incomplete ToVersionstamp errors", func(t *testing.T) {
		t.Parallel()
		v, _ := IncompleteVersion(0)
		_, err := v.ToVersionstamp()
		if err == nil {
			t.Fatal("expected error for incomplete version")
		}
	})
}

func TestWithCommittedVersion(t *testing.T) {
	t.Parallel()

	t.Run("completes incomplete preserving local", func(t *testing.T) {
		t.Parallel()
		inc, _ := IncompleteVersion(7)
		committed := rvMakeGlobal(0, 0, 0, 0, 0, 0, 0, 42, 0, 1)
		v, err := inc.WithCommittedVersion(committed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.IsComplete() {
			t.Fatal("should be complete after commit")
		}
		if v.GetLocalVersion() != 7 {
			t.Fatalf("local preserved: got %d, want 7", v.GetLocalVersion())
		}
		gv, _ := v.GetGlobalVersion()
		for i, b := range committed {
			if gv[i] != b {
				t.Fatalf("global byte %d: got %02x, want %02x", i, gv[i], b)
			}
		}
	})

	t.Run("errors if already complete", func(t *testing.T) {
		t.Parallel()
		c, _ := NewCompleteVersion(rvMakeGlobal(1), 0)
		_, err := c.WithCommittedVersion(rvMakeGlobal(2))
		if err == nil {
			t.Fatal("expected error for already-complete version")
		}
	})

	t.Run("wrong length errors", func(t *testing.T) {
		t.Parallel()
		inc, _ := IncompleteVersion(0)
		for _, n := range []int{0, 9, 11} {
			_, err := inc.WithCommittedVersion(make([]byte, n))
			if err == nil {
				t.Errorf("committed len=%d: expected error", n)
			}
		}
	})
}
