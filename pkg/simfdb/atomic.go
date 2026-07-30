package simfdb

import "bytes"

// Server-side application of FoundationDB atomic mutations.
//
// This is a faithful port of libfdb_c's storage-server mutation application, as
// captured in two byte-identical references:
//
//   - C++ spec: fdbclient/include/fdbclient/Atomic.h — doLittleEndianAdd, doAnd,
//     doAndV2, doOr, doXor, doMax, doMin, doMinV2, doByteMax, doByteMin,
//     doAppendIfFits, doCompareAndClear.
//   - The pure-Go client's differential-tested port: pkg/fdbgo/client/ryw.go
//     (applyAtomic + do* helpers, lines ~1401-1656), which is proven byte-identical
//     to libfdb_c by the client differential.
//
// SimFDB applies a queued mutation at COMMIT time against the value CURRENTLY in
// the store — not against any read-time value. That is exactly what the storage
// server does, so we port the server-side function, not a read-modify-write.
//
// Missing-key convention (matches the client and C++ Optional<ValueRef>):
//
//	existing == nil          -> key absent  (C++ !present())
//	existing == []byte{}     -> key present with an empty value (C++ present, size()==0)
//
// This distinction is load-bearing for AndV2/MinV2/ByteMax/ByteMin/CompareAndClear,
// which branch on presence rather than on size.
//
// API-version note: the client upgrades And->AndV2 and Min->MinV2 when the API
// version is >= 510 (client/transaction.go:1413-1419, mirroring C++
// ReadYourWrites::atomicOp). A SimFDB backend therefore receives the V2 opcodes,
// so atomicAnd/atomicMin fold to `param` on an absent key. The single-op enum
// below has no V1 variants because Tier-1 SimFDB only ever sees the modern
// (>= 510) wire behaviour; the pre-510 V1 quirk (And/Min on an absent key yielding
// all-zeros) is intentionally not modelled.

// atomicValueSizeLimit is CLIENT_KNOBS->VALUE_SIZE_LIMIT — the 100 KB cap that
// AppendIfFits refuses to exceed (Atomic.h:122; client sizelimits.go valueSizeLimit).
const atomicValueSizeLimit = 100_000

// atomicOp identifies an FDB atomic mutation type.
type atomicOp int

const (
	atomicAdd atomicOp = iota
	atomicAnd
	atomicOr
	atomicXor
	atomicMax
	atomicMin
	atomicByteMax
	atomicByteMin
	atomicAppendIfFits
	atomicCompareAndClear
)

// applyAtomic computes the new stored value for an atomic mutation applied against
// `existing` (the current committed value; nil means the key is absent). It returns
// the new value and, for CompareAndClear when the comparison matches, clear=true —
// in which case the caller must DELETE the key rather than store newVal (newVal is
// nil). For every other op clear is false and newVal is the bytes to store.
//
// For a non-clearing result the returned slice is always non-nil (an atomic op
// never tombstones a key; only CompareAndClear clears). This normalisation mirrors
// the client's resolveAtomics (ryw.go:1391-1392), which coerces a nil fold result
// to []byte{} so an empty-param mutation stores a present empty value rather than a
// spurious tombstone.
func applyAtomic(op atomicOp, existing, param []byte) (newVal []byte, clear bool) {
	switch op {
	case atomicCompareAndClear:
		newVal, clear = doCompareAndClear(existing, param)
	case atomicAdd:
		newVal = doAdd(existing, param)
	case atomicAnd:
		newVal = doAnd(existing, param)
	case atomicOr:
		newVal = doOr(existing, param)
	case atomicXor:
		newVal = doXor(existing, param)
	case atomicMax:
		newVal = doMax(existing, param)
	case atomicMin:
		newVal = doMin(existing, param)
	case atomicByteMax:
		newVal = doByteMax(existing, param)
	case atomicByteMin:
		newVal = doByteMin(existing, param)
	case atomicAppendIfFits:
		newVal = doAppendIfFits(existing, param)
	default:
		// Unknown op: no-op, preserve the existing value (defensive; the caller
		// validates opcodes upstream). Absent stays absent.
		newVal = existing
	}
	// Mirror the client's resolveAtomics normalisation (ryw.go:1391-1392): a clear
	// tombstones the key (nil); any other nil result is a PRESENT empty value, not a
	// tombstone. This matters for CompareAndClear-no-match on a present empty value
	// (doCompareAndClear returns nil for an empty existing), which must stay present.
	if clear {
		return nil, true
	}
	if newVal == nil {
		newVal = []byte{}
	}
	return newVal, false
}

// presentBytes returns the C++ "present" view of an Optional<ValueRef>: an absent
// key (nil) is seen as an empty StringRef. Used by the ops that treat absent and
// empty-value identically (Add/Or/Xor/Max/AppendIfFits).
func presentBytes(existing []byte) []byte {
	if existing == nil {
		return []byte{}
	}
	return existing
}

// doAdd — C++ doLittleEndianAdd (Atomic.h:27). Little-endian addition; the result
// width equals len(param). Existing is zero-extended (if shorter) or truncated (if
// longer) to param width first; carry past the top byte is discarded (overflow
// wraps). Absent/empty existing folds to param (0 + param).
func doAdd(existing, param []byte) []byte {
	e := presentBytes(existing)
	size := len(param)
	if size == 0 {
		return []byte{} // C++ returns the (empty) operand
	}
	result := make([]byte, size)
	var carry uint16
	for i := 0; i < size; i++ {
		var ev uint16
		if i < len(e) {
			ev = uint16(e[i]) // bytes of existing beyond param width are dropped
		}
		sum := carry + ev + uint16(param[i])
		result[i] = byte(sum)
		carry = sum >> 8
	}
	return result
}

// doAnd — C++ doAndV2 (Atomic.h:70) over doAnd (Atomic.h:54). AndV2: an absent key
// stores param. Otherwise bitwise AND to len(param); positions past the existing
// value are 0 (0 & x == 0), and existing bytes past param width are dropped.
func doAnd(existing, param []byte) []byte {
	if existing == nil {
		return append([]byte(nil), param...) // AndV2: absent -> param
	}
	if len(param) == 0 {
		return []byte{}
	}
	result := make([]byte, len(param))
	n := min(len(existing), len(param))
	for i := 0; i < n; i++ {
		result[i] = existing[i] & param[i]
	}
	// result[n:] stays 0x00 (existing has no bytes there; 0 & param[i] == 0).
	return result
}

// doOr — C++ doOr (Atomic.h:77). Result width = len(param). Absent/empty existing
// folds to param; positions past the existing value take param's byte verbatim.
func doOr(existing, param []byte) []byte {
	e := presentBytes(existing)
	if len(e) == 0 || len(param) == 0 {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	n := min(len(e), len(param))
	for i := 0; i < n; i++ {
		result[i] = e[i] | param[i]
	}
	for i := n; i < len(param); i++ {
		result[i] = param[i] // 0 | param[i]
	}
	return result
}

// doXor — C++ doXor (Atomic.h:95). Result width = len(param). Absent/empty existing
// folds to param; positions past the existing value take param's byte verbatim.
func doXor(existing, param []byte) []byte {
	e := presentBytes(existing)
	if len(e) == 0 || len(param) == 0 {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	n := min(len(e), len(param))
	for i := 0; i < n; i++ {
		result[i] = e[i] ^ param[i]
	}
	for i := n; i < len(param); i++ {
		result[i] = param[i] // 0 ^ param[i]
	}
	return result
}

// doMax — C++ doMax (Atomic.h:139). Unsigned little-endian comparison; the winner is
// stored at param width. Absent/empty existing folds to param (absent == 0, and
// max(0, param) == param). Existing bytes beyond param width are ignored by the
// comparison and dropped from the result — faithful to the C++ loop bounds.
func doMax(existing, param []byte) []byte {
	e := presentBytes(existing)
	if len(e) == 0 || len(param) == 0 {
		return append([]byte(nil), param...)
	}
	i := len(param) - 1
	// param's high bytes that existing lacks: any non-zero makes param the larger.
	for ; i >= len(e); i-- {
		if param[i] != 0 {
			return append([]byte(nil), param...)
		}
	}
	for ; i >= 0; i-- {
		if param[i] > e[i] {
			return append([]byte(nil), param...)
		} else if param[i] < e[i] {
			result := make([]byte, len(param))
			copy(result, e) // existing, zero-padded / truncated to param width
			return result
		}
	}
	return append([]byte(nil), param...) // equal -> param
}

// doMin — C++ doMinV2 (Atomic.h:221) over doMin (Atomic.h:183). MinV2: an absent key
// stores param. Otherwise unsigned little-endian min at param width. NOTE the
// nil-vs-empty split: an absent key folds to param, but a PRESENT empty value runs
// the V1 doMin body (e.g. min(empty, non-zero-param) yields all-zeros at param width).
func doMin(existing, param []byte) []byte {
	if existing == nil {
		return append([]byte(nil), param...) // MinV2: absent -> param
	}
	return doMinPresent(existing, param)
}

// doMinPresent ports C++ doMin (Atomic.h:183) for a present existing value.
func doMinPresent(e, param []byte) []byte {
	if len(param) == 0 {
		return []byte{} // C++ returns the (empty) operand
	}
	i := len(param) - 1
	// param's high bytes that existing lacks: any non-zero makes param the larger,
	// so the min is existing (zero-padded to param width).
	for ; i >= len(e); i-- {
		if param[i] != 0 {
			result := make([]byte, len(param))
			copy(result, e)
			return result
		}
	}
	for ; i >= 0; i-- {
		if param[i] > e[i] {
			result := make([]byte, len(param))
			copy(result, e)
			return result
		} else if param[i] < e[i] {
			return append([]byte(nil), param...)
		}
	}
	return append([]byte(nil), param...) // equal -> param
}

// doByteMax — C++ doByteMax (Atomic.h:172). Lexicographic (memcmp) comparison of the
// raw byte strings; the greater is stored verbatim. Absent existing folds to param.
func doByteMax(existing, param []byte) []byte {
	if existing == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(existing, param) > 0 {
		return append([]byte(nil), existing...)
	}
	return append([]byte(nil), param...)
}

// doByteMin — C++ doByteMin (Atomic.h:228). Lexicographic (memcmp) comparison; the
// lesser is stored verbatim. Absent existing folds to param.
func doByteMin(existing, param []byte) []byte {
	if existing == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(existing, param) < 0 {
		return append([]byte(nil), existing...)
	}
	return append([]byte(nil), param...)
}

// doAppendIfFits — C++ doAppendIfFits (Atomic.h:114). Appends param to existing iff
// the concatenation fits within VALUE_SIZE_LIMIT (100 KB); otherwise it is a NO-OP
// and existing is left unchanged. Absent/empty existing folds to param; empty param
// leaves existing unchanged.
func doAppendIfFits(existing, param []byte) []byte {
	e := presentBytes(existing)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), e...)
	}
	if len(e)+len(param) > atomicValueSizeLimit {
		return append([]byte(nil), e...) // no-op: truncation would be required
	}
	result := make([]byte, 0, len(e)+len(param))
	result = append(result, e...)
	result = append(result, param...)
	return result
}

// doCompareAndClear — C++ doCompareAndClear (Atomic.h:239). Clears the key iff it is
// absent OR its value equals param; otherwise the value is unchanged. Both-absent
// (existing == nil) always clears, matching C++ !present().
func doCompareAndClear(existing, param []byte) (newVal []byte, clear bool) {
	if existing == nil || bytes.Equal(existing, param) {
		return nil, true // caller deletes the key
	}
	return append([]byte(nil), existing...), false // no change
}
