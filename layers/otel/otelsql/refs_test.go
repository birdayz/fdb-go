package otelsql

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Every packed chunk's SPANS_REF_CHUNKS key element stays within
// MaxRefChunkKeyBytes even when the IDs escape a zero byte per byte, the
// chunking does not depend on the IDs, and the chunks unpack to the input.
func TestPackRefChunksKeyBound(t *testing.T) {
	t.Parallel()
	refsWith := func(writer, seq int64) []MessageRef {
		var refs []MessageRef
		for i := range 600 {
			refs = append(refs, MessageRef{Writer: writer, Seq: seq + int64(i%3), Attribute: int64(1 + i%4), Newest: i%2 == 0, Ordinals: []int{i, 256 * (i + 1), 0}})
		}
		return refs
	}
	// 1<<56 encodes as a type code, 0x01 and seven zero bytes: the most a
	// positive int64 grows when the refs element escapes its zeros.
	worst := refsWith(1<<56, 1<<56)
	chunks, err := PackRefChunks(worst)
	if err != nil {
		t.Fatal(err)
	}
	placeholders, err := PackRefChunks(refsWith(math.MaxInt64, math.MaxInt64-3))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 || len(chunks) != len(placeholders) {
		t.Fatalf("%d chunks with zero-heavy IDs, %d with placeholder IDs", len(chunks), len(placeholders))
	}
	var got []MessageRef
	escaped := false
	for i, c := range chunks {
		key := tuple.Tuple{c}.Pack()
		if len(key) > MaxRefChunkKeyBytes {
			t.Fatalf("chunk %d encodes to %d key bytes", i, len(key))
		}
		escaped = escaped || len(key) > len(c)+2+len(c)/4
		// Greedy: every chunk but the last is nearly full at the bound.
		if i < len(chunks)-1 && len(key) < MaxRefChunkKeyBytes-200 {
			t.Errorf("chunk %d holds only %d key bytes", i, len(key))
		}
		refs, err := UnpackRefChunk(c)
		if err != nil {
			t.Fatal(err)
		}
		p, err := UnpackRefChunk(placeholders[i])
		if err != nil || len(p) != len(refs) {
			t.Fatalf("chunk %d: %d refs, %d with placeholders (%v)", i, len(refs), len(p), err)
		}
		got = append(got, refs...)
	}
	if !escaped {
		t.Fatal("fixture IDs did not exercise zero-byte escaping")
	}
	if !reflect.DeepEqual(got, worst) {
		t.Fatal("chunks do not unpack to the references")
	}
	if c, err := PackRefChunks(nil); err != nil || c != nil {
		t.Fatalf("no references: %v %v", c, err)
	}
}

// A reference whose ordinals alone exceed a chunk is split into references
// of the same message with consecutive runs of its ordinals.
func TestPackRefChunksSplitsOrdinals(t *testing.T) {
	t.Parallel()
	ords := make([]int, 5000)
	for i := range ords {
		ords[i] = i * 300
	}
	ref := MessageRef{Writer: 1 << 56, Seq: 9, Attribute: 2, Newest: true, Ordinals: ords}
	chunks, err := PackRefChunks([]MessageRef{{Writer: 1, Seq: 1, Attribute: 1, Ordinals: []int{0}}, ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("%d chunks", len(chunks))
	}
	var merged []int
	for i, c := range chunks {
		if n := len(tuple.Tuple{c}.Pack()); n > MaxRefChunkKeyBytes {
			t.Fatalf("chunk %d: %d key bytes", i, n)
		}
		refs, err := UnpackRefChunk(c)
		if err != nil {
			t.Fatal(err)
		}
		for j, r := range refs {
			if i == 0 && j == 0 {
				continue
			}
			if r.Writer != ref.Writer || r.Seq != ref.Seq || r.Attribute != ref.Attribute || !r.Newest {
				t.Fatalf("split reference %+v", r)
			}
			merged = append(merged, r.Ordinals...)
		}
	}
	if !reflect.DeepEqual(merged, ords) {
		t.Fatal("split ordinals do not concatenate to the reference's")
	}
}

func TestPackRefChunksRefusals(t *testing.T) {
	t.Parallel()
	for _, r := range []MessageRef{
		{Writer: 1, Seq: 1, Attribute: 0, Ordinals: []int{0}},
		{Writer: 1, Seq: 1, Attribute: int64(len(MessageAttributeKeys)), Ordinals: []int{0}},
		{Writer: 0, Seq: 1, Attribute: 1, Ordinals: []int{0}},
		{Writer: 1, Seq: -1, Attribute: 1, Ordinals: []int{0}},
		{Writer: 1, Seq: 1, Attribute: 1},
		{Writer: 1, Seq: 1, Attribute: 1, Ordinals: []int{1, -1}},
	} {
		_, err := PackRefChunks([]MessageRef{r})
		var invalid *InvalidMessageRefError
		if !errors.As(err, &invalid) {
			t.Errorf("%+v: %v, want InvalidMessageRefError", r, err)
		}
	}
	ok := tuple.Tuple{int64(1), int64(2), int64(3), int64(1), tuple.Tuple{int64(0)}}
	if _, err := UnpackRefChunk(ok.Pack()); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"empty":         nil,
		"not a tuple":   {0xff},
		"short":         tuple.Tuple{int64(1), int64(2), int64(3), int64(1)}.Pack(),
		"string writer": tuple.Tuple{"w", int64(2), int64(3), int64(1), tuple.Tuple{int64(0)}}.Pack(),
		"zero seq":      tuple.Tuple{int64(1), int64(0), int64(3), int64(1), tuple.Tuple{int64(0)}}.Pack(),
		"attribute 5":   tuple.Tuple{int64(1), int64(2), int64(5), int64(1), tuple.Tuple{int64(0)}}.Pack(),
		"newest 2":      tuple.Tuple{int64(1), int64(2), int64(3), int64(2), tuple.Tuple{int64(0)}}.Pack(),
		"no ordinals":   tuple.Tuple{int64(1), int64(2), int64(3), int64(1), tuple.Tuple{}}.Pack(),
		"ordinal -1":    tuple.Tuple{int64(1), int64(2), int64(3), int64(1), tuple.Tuple{int64(-1)}}.Pack(),
		"ordinal text":  tuple.Tuple{int64(1), int64(2), int64(3), int64(1), tuple.Tuple{"x"}}.Pack(),
	} {
		_, err := UnpackRefChunk(b)
		var malformed *MalformedRefChunkError
		if !errors.As(err, &malformed) {
			t.Errorf("%s: %v, want MalformedRefChunkError", name, err)
		}
	}
}
