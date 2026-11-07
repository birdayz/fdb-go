package otelsql

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// MaxRefChunkKeyBytes bounds the refs column of one ref_chunks element as the
// SPANS_REF_CHUNKS index key encodes it, tuple.Tuple{refs}.Pack(): the key's
// other columns (subspace, trace, start, span, chunk) leave it below FDB's
// 10 KB key limit.
const MaxRefChunkKeyBytes = 8000

// refIDBoundBytes is the most bytes a positive int64 takes in a refs element
// of a key: its type code and up to 8 bytes, with every zero byte after the
// first escaped. Sizing writer and seq at this bound makes the chunking of a
// span's references independent of the message IDs, so placeholder IDs give
// the same chunks as the IDs a commit later assigns.
const refIDBoundBytes = 1 + 8 + 7

// MessageRef is a span's back reference to one message: the message, the
// GenAI attribute (MessageAttributeCode) whose value holds it, its positions
// in that value, and whether it is the value's newest element.
type MessageRef struct {
	Writer, Seq int64
	Attribute   int64
	Newest      bool
	Ordinals    []int
}

// MalformedRefChunkError is a refs value that is not a packed reference list.
type MalformedRefChunkError struct {
	Reason string
}

func (e *MalformedRefChunkError) Error() string {
	return "otelsql: malformed message reference chunk: " + e.Reason
}

// InvalidMessageRefError is a reference PackRefChunks refuses.
type InvalidMessageRefError struct {
	Ref    MessageRef
	Reason string
}

func (e *InvalidMessageRefError) Error() string {
	return fmt.Sprintf("otelsql: message reference %d.%d: %s", e.Ref.Writer, e.Ref.Seq, e.Reason)
}

// PackRefChunks packs refs, in order, into ref_chunks values: each a
// concatenation of tuple-packed (writer, seq, attribute, newest, (ordinals))
// whose index key element stays within MaxRefChunkKeyBytes. A reference
// whose ordinals alone exceed a chunk is split into several references of
// the same message, each with a run of its ordinals.
func PackRefChunks(refs []MessageRef) ([][]byte, error) {
	var chunks [][]byte
	var cur []byte
	bound := 2 // the refs element's type code and terminator
	for _, r := range refs {
		if r.Attribute <= 0 || r.Attribute >= int64(len(MessageAttributeKeys)) {
			return nil, &InvalidMessageRefError{r, "unknown attribute code"}
		}
		if r.Writer <= 0 || r.Seq <= 0 {
			return nil, &InvalidMessageRefError{r, "writer and seq must be positive"}
		}
		if len(r.Ordinals) == 0 {
			return nil, &InvalidMessageRefError{r, "no ordinals"}
		}
		for rest := r.Ordinals; len(rest) > 0; {
			n, size, err := refPrefix(r, rest)
			if err != nil {
				return nil, err
			}
			if len(cur) > 0 && bound+size > MaxRefChunkKeyBytes {
				chunks = append(chunks, cur)
				cur, bound = nil, 2
			}
			cur = append(cur, packRef(r, rest[:n])...)
			bound += size
			rest = rest[n:]
		}
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks, nil
}

// refPrefix is the longest prefix of ordinals that fits one chunk as a
// reference of r, and that reference's key bound.
func refPrefix(r MessageRef, ordinals []int) (int, int, error) {
	newest := int64(0)
	if r.Newest {
		newest = 1
	}
	// The ordinals' nested tuple adds its type code and a terminator, a zero
	// byte that the refs element escapes.
	size := 2*refIDBoundBytes + escapedLen(tuple.Tuple{r.Attribute, newest}.Pack()) + 3
	n := 0
	for _, o := range ordinals {
		if o < 0 {
			return 0, 0, &InvalidMessageRefError{r, "negative ordinal"}
		}
		e := escapedLen(tuple.Tuple{int64(o)}.Pack())
		if 2+size+e > MaxRefChunkKeyBytes {
			break
		}
		size += e
		n++
	}
	if n == 0 {
		return 0, 0, &InvalidMessageRefError{r, "an ordinal exceeds a chunk"}
	}
	return n, size, nil
}

func packRef(r MessageRef, ordinals []int) []byte {
	ords := make(tuple.Tuple, len(ordinals))
	for i, o := range ordinals {
		ords[i] = int64(o)
	}
	newest := int64(0)
	if r.Newest {
		newest = 1
	}
	return tuple.Tuple{r.Writer, r.Seq, r.Attribute, newest, ords}.Pack()
}

// escapedLen is len(b) inside a tuple-encoded byte string, which escapes each
// zero byte as two.
func escapedLen(b []byte) int {
	n := len(b)
	for _, c := range b {
		if c == 0 {
			n++
		}
	}
	return n
}

// UnpackRefChunk decodes one ref_chunks value of PackRefChunks.
func UnpackRefChunk(b []byte) ([]MessageRef, error) {
	t, err := tuple.Unpack(b)
	if err != nil {
		return nil, &MalformedRefChunkError{Reason: err.Error()}
	}
	if len(t) == 0 || len(t)%5 != 0 {
		return nil, &MalformedRefChunkError{Reason: fmt.Sprintf("%d tuple elements", len(t))}
	}
	out := make([]MessageRef, 0, len(t)/5)
	for i := 0; i < len(t); i += 5 {
		w, wok := t[i].(int64)
		s, sok := t[i+1].(int64)
		a, aok := t[i+2].(int64)
		n, nok := t[i+3].(int64)
		ords, ook := t[i+4].(tuple.Tuple)
		if !wok || !sok || !aok || !nok || !ook || w <= 0 || s <= 0 || a <= 0 || a >= int64(len(MessageAttributeKeys)) || (n != 0 && n != 1) || len(ords) == 0 {
			return nil, &MalformedRefChunkError{Reason: fmt.Sprintf("reference %d", i/5)}
		}
		r := MessageRef{Writer: w, Seq: s, Attribute: a, Newest: n == 1, Ordinals: make([]int, len(ords))}
		for j, o := range ords {
			v, ok := o.(int64)
			if !ok || v < 0 {
				return nil, &MalformedRefChunkError{Reason: fmt.Sprintf("reference %d ordinal %d", i/5, j)}
			}
			r.Ordinals[j] = int(v)
		}
		out = append(out, r)
	}
	return out, nil
}
