package fnv64

import (
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"strings"
	"testing"
)

var _ io.StringWriter = (*digest)(nil)

func TestDigestMatchesFNVAcrossWrites(t *testing.T) {
	t.Parallel()
	got, want := New(), fnv.New64a()
	if got.Sum64() != want.Sum64() {
		t.Fatal("empty hash differs from FNV-1a")
	}
	for i, chunk := range []string{"", "qov:", "\x00\xff\x80", "雪", "fieldpath:", "(0)", ""} {
		var n int
		var err error
		if i%2 == 0 {
			n, err = io.WriteString(got, chunk)
		} else {
			n, err = got.Write([]byte(chunk))
		}
		if err != nil || n != len(chunk) {
			t.Fatalf("write %d = %d, %v, want %d, nil", i, n, err, len(chunk))
		}
		_, _ = want.Write([]byte(chunk))
		if got.Sum64() != want.Sum64() {
			t.Fatalf("after write %d: hash = %x, stdlib FNV = %x", i, got.Sum64(), want.Sum64())
		}
	}
}

func TestDigestNumericWrites(t *testing.T) {
	t.Parallel()
	for _, n := range []int64{math.MinInt64, math.MinInt32, -100, -10, -1, 0, 1, 9, 10, 15, 16, 99, 100, math.MaxInt32, math.MaxInt64} {
		checkDigestNumericWrites(t, "qov:\x00\xff雪", n, uint64(n))
	}
}

func checkDigestNumericWrites(t testing.TB, prefix string, signed int64, unsigned uint64) {
	t.Helper()
	got, want := New(), fnv.New64a()
	var fallback strings.Builder
	text := fmt.Sprintf("%s%d:%x[]", prefix, signed, unsigned)
	_, _ = io.WriteString(want, text)
	for _, writer := range []io.Writer{got, &fallback} {
		_, _ = io.WriteString(writer, prefix)
		WriteInt(writer, signed)
		_, _ = io.WriteString(writer, ":")
		WriteHex(writer, unsigned)
		_, _ = io.WriteString(writer, "[]")
	}
	if got.Sum64() != want.Sum64() || fallback.String() != text {
		t.Fatalf("numeric stream %q: hash %x, want %x; fallback %q", text, got.Sum64(), want.Sum64(), fallback.String())
	}
}

func FuzzDigestNumericWrites(f *testing.F) {
	f.Add("", int64(0), uint64(0))
	f.Add("qov:\x00\xff雪", int64(math.MinInt64), uint64(math.MaxUint64))
	f.Fuzz(func(t *testing.T, prefix string, signed int64, unsigned uint64) {
		t.Parallel()
		checkDigestNumericWrites(t, prefix, signed, unsigned)
	})
}

func BenchmarkDigestNumericWrites(b *testing.B) {
	h := New()
	b.ReportAllocs()
	for b.Loop() {
		WriteInt(h, math.MinInt64)
		WriteHex(h, math.MaxUint64)
	}
}

func FuzzDigestMatchesFNV(f *testing.F) {
	f.Add("", uint8(0))
	f.Add("fieldpath:\x00\xff\x80雪(0)", uint8(3))
	f.Fuzz(func(t *testing.T, data string, width uint8) {
		t.Parallel()
		got, want := New(), fnv.New64a()
		for i := 0; i < len(data); {
			end := min(len(data), i+int(width)+1)
			chunk := data[i:end]
			var n int
			var err error
			if i%2 == 0 {
				n, err = io.WriteString(got, chunk)
			} else {
				n, err = got.Write([]byte(chunk))
			}
			if err != nil || n != len(chunk) {
				t.Fatalf("write = %d, %v, want %d, nil", n, err, len(chunk))
			}
			_, _ = want.Write([]byte(chunk))
			if got.Sum64() != want.Sum64() {
				t.Fatalf("hash = %x, stdlib FNV = %x", got.Sum64(), want.Sum64())
			}
			i = end
		}
	})
}
