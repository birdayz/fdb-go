package client

import (
	"bytes"
	"encoding/binary"
	"slices"
	"unsafe"
)

// isUnresolvedVersionstamp reports whether op is a versionstamped mutation, which
// cannot be resolved client-side: the 10-byte stamp is assigned at commit, so the
// resolved key (SetVersionstampedKey) or value (SetVersionstampedValue) is unknown
// pre-commit. C++ marks such a key accessed_unreadable; the RYW read paths must read
// it as ABSENT (our approximation) rather than surface a phantom value.
func isUnresolvedVersionstamp(op MutationType) bool {
	return op == MutSetVersionstampedKey || op == MutSetVersionstampedValue
}

// isNonAssociativeOp mirrors C++ isNonAssociativeOp / NON_ASSOCIATIVE_MASK (CommitTransaction.h:576-578):
// ops that obey the associative law ONLY when all operands have equal length. coalesceOverAtomics folds
// two same-type non-associative ops solely when their operand lengths match; the associative ops
// (And, AndV2, ByteMin, ByteMax, AppendIfFits) always fold on same type.
func isNonAssociativeOp(op MutationType) bool {
	switch op {
	case MutAddValue, MutOr, MutXor, MutMax, MutMin, MutMinV2,
		MutSetVersionstampedKey, MutSetVersionstampedValue, MutCompareAndClear:
		return true
	default:
		return false
	}
}

// coalesceOverAtomics folds newMut into the pending atomic chain, a port of C++ WriteMap::coalesceOver
// (WriteMap.cpp:480-494) + coalesce (:357) restricted to Go's representation: the SetValue base is stored
// separately in rywEntry.value and resolved via the eager value-fold at atomic() sites B/C, so this stack
// is a PURE atomic chain (its top is always an atomic op, never a SetValue). Within that restriction the
// two C++ branches reduce to:
//   - same op type, associative OR equal operand length → FOLD (combine operands via applyAtomic, keep the
//     atomic type) — 150k `ADD 1` (all 8-byte) → one `ADD 150000`;
//   - same op type, non-associative, DIFFERENT operand length → PUSH (keep both);
//   - a DIFFERENT op type → PUSH (two atomics of different type never collapse — C++ else-branch
//     `isAtomicOp(new) && isAtomicOp(existing)` → push).
//
// Versionstamped ops and CompareAndClear are always PUSHED (never folded): CompareAndClear is excluded from
// C++'s same-type fold branch (`newEntry.type != CompareAndClear`) and over another atomic pushes; a
// versionstamped op is kept intact so the entry's unreadable/resolve semantics are byte-for-byte unchanged
// (its 10-byte stamp is assigned at commit, so folding cannot combine operands anyway). The fold is
// read-transparent: resolveAtomics over the folded chain yields the identical value.
//
// The stack may be shared with a write-map snapshot (rywWrites.clone) or a captured
// span: its backing array is written only when inPlace proves this version owns it.
func coalesceOverAtomics(stack []rywMutation, newMut rywMutation, inPlace bool) []rywMutation {
	if !foldsOnto(stack, newMut) {
		if inPlace {
			return append(stack, newMut)
		}
		return append(stack[:len(stack):len(stack)], newMut)
	}
	top := stack[len(stack)-1]
	// Fold: combine the two operands with the op's own primitive, keeping the atomic type
	// (C++ coalesce same-type branch, e.g. AddValue → doAdd(existing, new) → AddValue).
	//
	// An OPERAND is always PRESENT, including a zero-length one: C++ RYWMutation.value for an
	// atomic op is a present Optional, and the only not-present RYWMutation is the synthetic
	// SetValue bottom pushed over a cleared range (WriteMap.cpp:105/143). Go spells not-present
	// as a nil []byte and applyAtomic reads its BASE argument with that convention, so the
	// operand handed to the base slot must be normalized — otherwise an empty operand is read
	// as ABSENT and takes the absent→operand branch of doAndV2 (Atomic.h:71), doByteMin (:229)
	// or doMinV2 (:222), returning the second operand verbatim instead of combining the two.
	// A fold that resolved to empty (ByteMin over "") leaves such a nil as the next fold's base;
	// downstream readers use param only as an operand, where nil and empty are the same.
	var folded rywMutation
	if top.typ == MutAppendIfFits {
		// C++ coalesce AppendIfFits+AppendIfFits → AppendIfFits(doAppendIfFits) (WriteMap.cpp:392-394).
		folded.param, folded.owned = appendIfFitsOwned(existing(top.param), top.owned, newMut.param)
	} else {
		folded.param, _ = applyAtomic(top.typ, existing(top.param), newMut.param)
	}
	folded.typ = top.typ
	if !inPlace {
		stack = slices.Clone(stack)
	}
	stack[len(stack)-1] = folded
	return stack
}

// foldsOnto reports whether coalesceOverAtomics folds newMut into the stack top
// rather than pushing it.
func foldsOnto(stack []rywMutation, newMut rywMutation) bool {
	if len(stack) == 0 || isUnresolvedVersionstamp(newMut.typ) || newMut.typ == MutCompareAndClear {
		return false
	}
	top := stack[len(stack)-1]
	if top.typ != newMut.typ || isUnresolvedVersionstamp(top.typ) {
		return false // different type (or the top is an unfoldable versionstamp) → keep both
	}
	// non-associative + operand-size mismatch → keep both
	return !isNonAssociativeOp(top.typ) || len(top.param) == len(newMut.param)
}

// resolveAtomics folds an atomic-mutation chain onto a storage base for the RYW read
// paths (single-key get + range merge). It returns the resolved value and two
// terminal flags:
//
//   - cleared: a CompareAndClear matched → the key is removed from the merged view.
//   - unresolved: the chain contains a versionstamped op → the key is UNREADABLE
//     client-side (C++ accessed_unreadable) and reads as absent. This is terminal and
//     DOMINANT: once a versionstamp appears, a later resolvable op layered on top is
//     still unreadable (an op over an unknown value/key stays unknown), so the rest of
//     the chain is skipped and unresolved wins over cleared.
//
// Convention: base==nil means "key absent" (C++ Optional<ValueRef> not present);
// base==[]byte{} means "present with empty value". A resolved non-clear op normalizes
// a nil result to []byte{} so a later V2 op (MinV2/AndV2) sees "present" and does the
// op rather than the absent→operand path — matching C++ Optional.present() (Atomic.h).
func resolveAtomics(base []byte, atomics []rywMutation) (result []byte, cleared, unresolved bool) {
	for _, m := range atomics {
		if isUnresolvedVersionstamp(m.typ) {
			return nil, false, true
		}
		base, cleared = applyAtomic(m.typ, base, m.param)
		if cleared {
			base = nil
		} else if base == nil {
			base = []byte{}
		}
	}
	return base, cleared, false
}

// atomicsOutcome is resolveAtomics's flags without folding operands, which only a trailing
// CompareAndClear needs, as RYWIterator::type reads types, not values (RYWIterator.cpp:44-49).
func atomicsOutcome(base []byte, atomics []rywMutation) (cleared, unresolved bool) {
	for _, m := range atomics {
		if isUnresolvedVersionstamp(m.typ) {
			return false, true
		}
	}
	if len(atomics) == 0 || atomics[len(atomics)-1].typ != MutCompareAndClear {
		return false, false
	}
	_, cleared, _ = resolveAtomics(base, atomics)
	return cleared, false
}

// applyAtomic applies an atomic mutation to a base value, mirroring the C++
// implementations in fdbclient/include/fdbclient/Atomic.h exactly.
//
// Convention: base==nil means "key absent" (C++ Optional<ValueRef> not present).
// base==[]byte{} means "key present with empty value".
// Returns (result, cleared). cleared=true means the key should be removed
// (only happens for CompareAndClear).
func applyAtomic(op MutationType, base, param []byte) (result []byte, cleared bool) {
	switch op {
	case MutSetValue:
		return append([]byte(nil), param...), false
	case MutAddValue:
		return doAdd(base, param), false
	case MutAnd:
		return doAnd(base, param), false
	case MutAndV2:
		return doAndV2(base, param), false
	case MutOr:
		return doOr(base, param), false
	case MutXor:
		return doXor(base, param), false
	case MutMax:
		return doMax(base, param), false
	case MutMin:
		return doMin(base, param), false
	case MutMinV2:
		return doMinV2(base, param), false
	case MutByteMax:
		return doByteMax(base, param), false
	case MutByteMin:
		return doByteMin(base, param), false
	case MutAppendIfFits:
		return doAppendIfFits(base, param), false
	case MutCompareAndClear:
		return doCompareAndClear(base, param)
	default:
		// Versionstamped mutations can't be resolved client-side.
		if base != nil {
			return append([]byte(nil), base...), false
		}
		return nil, false
	}
}

// existing returns the "present" value for C++ Optional<ValueRef> semantics.
// nil → empty StringRef (for operations that treat absent as empty).
func existing(base []byte) []byte {
	if base == nil {
		return []byte{}
	}
	return base
}

// doAdd — C++ doAdd in Atomic.h. Little-endian addition.
// Result length = len(param), matching C++ which allocates otherOperand.size().
// Base bytes beyond len(param) are silently dropped (carry discarded).
func doAdd(base, param []byte) []byte {
	e := existing(base)
	size := len(param)
	if size == 0 {
		return []byte{}
	}
	a := make([]byte, size)
	copy(a, e) // zero-pads if len(e) < size; truncates if len(e) > size
	b := make([]byte, size)
	copy(b, param)

	if size == 8 {
		result := make([]byte, 8)
		binary.LittleEndian.PutUint64(result, binary.LittleEndian.Uint64(a)+binary.LittleEndian.Uint64(b))
		return result
	}
	result := make([]byte, size)
	var carry uint16
	for i := 0; i < size; i++ {
		sum := carry + uint16(a[i]) + uint16(b[i])
		result[i] = byte(sum)
		carry = sum >> 8
	}
	return result
}

// doAnd — C++ doAnd in Atomic.h. Result length = param length. Missing base → 0x00.
func doAnd(base, param []byte) []byte {
	e := existing(base)
	if len(param) == 0 {
		return []byte{}
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] & param[i]
	}
	// Remaining positions: 0x00 (base bytes beyond existing are 0, 0 & anything = 0)
	return result
}

// doAndV2 — C++ doAndV2. If absent → return param. Otherwise → doAnd.
func doAndV2(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	return doAnd(base, param)
}

// doOr — C++ doOr. Result length = param length. Missing base → 0x00.
func doOr(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] | param[i]
	}
	for i := minLen; i < len(param); i++ {
		result[i] = param[i]
	}
	return result
}

// doXor — C++ doXor. Result length = param length. Missing base → 0x00.
func doXor(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] ^ param[i]
	}
	for i := minLen; i < len(param); i++ {
		result[i] = param[i]
	}
	return result
}

// doMax — C++ doMax. Little-endian unsigned compare. Result length = param length.
func doMax(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	// Compare from MSB of param down. Extra param bytes beyond existing treated as > 0.
	for i := len(param) - 1; i >= len(e); i-- {
		if param[i] != 0 {
			return append([]byte(nil), param...)
		}
	}
	for i := min(len(e), len(param)) - 1; i >= 0; i-- {
		if param[i] > e[i] {
			return append([]byte(nil), param...)
		} else if param[i] < e[i] {
			// Return existing truncated/zero-padded to param length.
			result := make([]byte, len(param))
			copy(result, e)
			return result
		}
	}
	return append([]byte(nil), param...)
}

// doMin — C++ doMin. Little-endian unsigned compare. Result length = param length.
func doMin(base, param []byte) []byte {
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	e := existing(base)
	// Compare from MSB of param down.
	for i := len(param) - 1; i >= len(e); i-- {
		if param[i] != 0 {
			result := make([]byte, len(param))
			copy(result, e)
			return result
		}
	}
	for i := min(len(e), len(param)) - 1; i >= 0; i-- {
		if param[i] > e[i] {
			result := make([]byte, len(param))
			copy(result, e)
			return result
		} else if param[i] < e[i] {
			return append([]byte(nil), param...)
		}
	}
	return append([]byte(nil), param...)
}

// doMinV2 — C++ doMinV2. If absent → return param. Otherwise → doMin.
func doMinV2(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	return doMin(base, param)
}

// doByteMax — C++ doByteMax. Lexicographic (big-endian byte) comparison.
func doByteMax(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(base, param) > 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// doByteMin — C++ doByteMin. Lexicographic comparison.
func doByteMin(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(base, param) < 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// doAppendIfFits — C++ doAppendIfFits. Concatenates if within 100KB limit.
func doAppendIfFits(base, param []byte) []byte {
	e := existing(base)
	if kept, concat := appendIfFitsKept(e, param); !concat {
		return append([]byte(nil), kept...)
	}
	result := make([]byte, len(e)+len(param))
	copy(result, e)
	copy(result[len(e):], param)
	return result
}

// appendIfFitsKept is doAppendIfFits's choice (Atomic.h:114-137) for an existing value e:
// the operand it returns unchanged, or concat when it concatenates e and param.
func appendIfFitsKept(e, param []byte) (kept []byte, concat bool) {
	if len(e) == 0 {
		return param, false
	}
	if len(param) == 0 || len(e)+len(param) > valueSizeLimit { // CLIENT_KNOBS->VALUE_SIZE_LIMIT (pkg const)
		return e, false
	}
	return nil, true
}

// appendIfFitsOwned is doAppendIfFits (Atomic.h:114-137) appending in place past len(v), into bytes only
// the live lineage owns (owned; v is capped), so versions sharing v see it unchanged (WriteMap.h:158-160).
func appendIfFitsOwned(v []byte, owned int32, param []byte) ([]byte, int32) {
	if kept, concat := appendIfFitsKept(v, param); !concat {
		if len(v) == 0 {
			return append([]byte(nil), kept...), 0
		}
		return v, owned
	}
	n := len(v) + len(param)
	if int(owned) >= n {
		buf := unsafe.Slice(unsafe.SliceData(v), owned)
		copy(buf[len(v):n], param)
		return buf[:n:n], owned
	}
	c := min(max(2*n, 64), valueSizeLimit)
	buf := make([]byte, n, c)
	copy(buf, v)
	copy(buf[len(v):], param)
	return buf[:n:n], int32(c)
}

// doCompareAndClear — C++ doCompareAndClear. If absent or equal → clear.
func doCompareAndClear(base, param []byte) ([]byte, bool) {
	if base == nil || bytes.Equal(base, param) {
		return nil, true // Clear the value.
	}
	return append([]byte(nil), base...), false // No change.
}

// resolveAtomicsBypass resolves a chain CONTAINING versionstamped ops for a
// BYPASS_UNREADABLE read: each versionstamped op applies as a plain Set of its
// operand exactly as written — placeholder bytes unfilled, trailing 4-byte
// offset suffix INCLUDED (C++ kv() under bypass returns the write-map value;
// the RYWIterator.cpp:433-449 unit pins kv->value == metadataVersionRequiredValue,
// all 14 bytes). Mirrors resolveAtomics for everything else. Never cached.
func resolveAtomicsBypass(base []byte, atomics []rywMutation) (result []byte, cleared bool) {
	for _, m := range atomics {
		if isUnresolvedVersionstamp(m.typ) {
			base = m.param
			cleared = false
			continue
		}
		val, clr := applyAtomic(m.typ, base, m.param)
		if !clr && val == nil {
			val = []byte{} // present-empty, matching resolveAtomics normalization
		}
		base = val
		cleared = clr
	}
	return base, cleared
}
