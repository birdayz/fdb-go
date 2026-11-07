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

// RefChunkReader reads one ref_chunks value's references in order without
// allocating per reference, for scans that keep few of them: Ref is a
// reference's message, attribute and newest flag, and AppendOrdinals
// decodes its ordinals only when asked. It yields exactly UnpackRefChunk's
// references and fails exactly when UnpackRefChunk does: a value outside
// PackRefChunks' encoding is decoded by UnpackRefChunk instead.
type RefChunkReader struct {
	b    []byte // a value in PackRefChunks' encoding, already validated
	pos  int
	cur  MessageRef // Ordinals unset in b's references
	ords []byte     // cur's ordinals: the nested tuple's elements
	slow []MessageRef
	next int
	err  error
}

// Reset starts reading the ref_chunks value b.
func (r *RefChunkReader) Reset(b []byte) {
	*r = RefChunkReader{}
	if packedRefChunk(b) {
		r.b = b
		return
	}
	r.slow, r.err = UnpackRefChunk(b)
}

// Next advances to the next reference, reporting whether there is one.
func (r *RefChunkReader) Next() bool {
	if r.err != nil {
		return false
	}
	if r.b == nil {
		if r.next == len(r.slow) {
			return false
		}
		r.cur, r.next = r.slow[r.next], r.next+1
		return true
	}
	if r.pos == len(r.b) {
		return false
	}
	r.cur, r.ords, r.pos = readPackedRef(r.b, r.pos)
	return true
}

// Ref is the current reference without its ordinals.
func (r *RefChunkReader) Ref() MessageRef {
	ref := r.cur
	ref.Ordinals = nil
	return ref
}

// AppendOrdinals appends the current reference's ordinals to dst.
func (r *RefChunkReader) AppendOrdinals(dst []int) []int {
	if r.b == nil {
		return append(dst, r.cur.Ordinals...)
	}
	for pos := 0; pos < len(r.ords); {
		var v int64
		v, pos = readPackedInt(r.ords, pos)
		dst = append(dst, int(v))
	}
	return dst
}

// Err is UnpackRefChunk's error for a malformed value.
func (r *RefChunkReader) Err() error { return r.err }

// packedRefChunk reports whether b is a nonempty concatenation of valid
// references exactly as packRef encodes them: four nonnegative ints
// (writer, seq, attribute, newest) of at most 8 bytes that fit an int64, and
// a nested tuple of at least one such int. UnpackRefChunk accepts every such
// b with the values readPackedRef reads.
func packedRefChunk(b []byte) bool {
	pos := 0
	for pos < len(b) {
		var head [4]int64
		for i := range head {
			v, next, ok := checkPackedInt(b, pos)
			if !ok {
				return false
			}
			head[i], pos = v, next
		}
		if head[0] <= 0 || head[1] <= 0 || head[2] <= 0 || head[2] >= int64(len(MessageAttributeKeys)) || head[3] > 1 {
			return false
		}
		if pos == len(b) || b[pos] != 0x05 {
			return false
		}
		pos++
		n := 0
		for {
			if pos == len(b) {
				return false
			}
			if b[pos] == 0x00 {
				// A zero byte followed by 0xff is a null element, not
				// the nested tuple's end.
				if pos+1 < len(b) && b[pos+1] == 0xff {
					return false
				}
				pos++
				break
			}
			_, next, ok := checkPackedInt(b, pos)
			if !ok {
				return false
			}
			pos = next
			n++
		}
		if n == 0 {
			return false
		}
	}
	return len(b) > 0
}

// checkPackedInt reads the nonnegative int at b[pos] if it is one tuple
// int of at most 8 bytes that fits an int64.
func checkPackedInt(b []byte, pos int) (int64, int, bool) {
	if pos == len(b) || b[pos] < 0x14 || b[pos] > 0x1c {
		return 0, pos, false
	}
	n := int(b[pos] - 0x14)
	if pos+n >= len(b) && n > 0 || n == 8 && b[pos+1] >= 0x80 {
		return 0, pos, false
	}
	v, next := readPackedInt(b, pos)
	// tuple.Unpack decodes a zero of one or more bytes as uint64.
	if n > 0 && v == 0 {
		return 0, pos, false
	}
	return v, next, true
}

// readPackedInt reads the int checkPackedInt accepted at b[pos].
func readPackedInt(b []byte, pos int) (int64, int) {
	n := int(b[pos] - 0x14)
	var v int64
	for _, c := range b[pos+1 : pos+1+n] {
		v = v<<8 | int64(c)
	}
	return v, pos + 1 + n
}

// readPackedRef reads the reference packedRefChunk accepted at b[pos]: its
// head, its encoded ordinals and the position after it.
func readPackedRef(b []byte, pos int) (MessageRef, []byte, int) {
	var head [4]int64
	for i := range head {
		head[i], pos = readPackedInt(b, pos)
	}
	start := pos + 1
	end := start
	for b[end] != 0x00 {
		_, end = readPackedInt(b, end)
	}
	return MessageRef{Writer: head[0], Seq: head[1], Attribute: head[2], Newest: head[3] == 1}, b[start:end], end + 1
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
