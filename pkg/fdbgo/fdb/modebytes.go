package fdb

// This file is the ONE place a StreamingMode is converted into libfdb_c's per-fetch BYTE
// target. It exists as a named site rather than as an expression at each use because the
// conversion crosses two different enum numberings, and getting it wrong is silent: it
// compiles, it runs, and it produces a division that no row-count assertion can see.
//
// C   (bindings/c/fdb_c_options.g.h:526-544): WANT_ALL=-2 ITERATOR=-1 EXACT=0 SMALL=1 MEDIUM=2 LARGE=3 SERIAL=4
// Go  (fdb/range.go:93-116):                  WantAll=-1  Iterator=0   Exact=1 Small=2  Medium=3  Large=4  Serial=5
//
// Go's value is the C value PLUS ONE. Indexing mode_bytes_array with a Go mode hands SMALL the
// MEDIUM target, MEDIUM the LARGE target, LARGE the SERIAL target, and reads out of bounds for
// Serial. modeBytesTable below is therefore indexed by the C value, and cModeIndex is the only
// thing permitted to do the subtraction.

// ByteLimitUnlimited mirrors GetRangeLimits::BYTE_LIMIT_UNLIMITED (FDBTypes.h). A range read
// carrying it has no byte target: nothing truncates the reply on bytes beyond the flat
// per-reply ceiling, and the soft byte limit that ends a byte-bounded call can never fire.
const ByteLimitUnlimited = -1

// modeBytesTable is bindings/c/fdb_c.cpp:1002 verbatim:
//
//	const int mode_bytes_array[] = { GetRangeLimits::BYTE_LIMIT_UNLIMITED, 256, 1000, 4096, 80000 };
//
// indexed by the C streaming mode: EXACT=0, SMALL=1, MEDIUM=2, LARGE=3, SERIAL=4.
var modeBytesTable = [...]int{ByteLimitUnlimited, 256, 1000, 4096, 80000}

// iterationProgression is bindings/c/fdb_c.cpp:1006 verbatim — "Goes 1.5 * previous". It is the
// per-fetch byte target for FDB_STREAMING_MODE_ITERATOR, indexed by iteration-1.
var iterationProgression = [...]int{4096, 6144, 9216, 13824, 20736, 31104, 46656, 69984, 80000, 120000}

// maxIteration is fdb_c.cpp:1009 — `sizeof(iteration_progression) / sizeof(int)`. fdb_c.cpp:1019
// CLAMPS iteration to it before indexing, so past the end of the table the C client re-uses the
// LAST target rather than continuing to grow.
const maxIteration = len(iterationProgression)

// cModeIndex converts a Go (Apple-numbered) StreamingMode to libfdb_c's numbering. This is the
// only subtraction of 1 in the tree for this purpose; every other site must go through here.
func cModeIndex(mode StreamingMode) int { return int(mode) - 1 }

// ModeTargetBytes exposes the per-fetch byte target for a streaming mode and iteration.
// Exported for the same reason BatchSize is: the batching rule must have ONE definition. The
// cross-client differential in pkg/fdbgo/libfdbc drives the production range path with it to
// compare this client's division against libfdb_c's, and a second copy of the table there
// would let the two drift while still agreeing with each other.
func ModeTargetBytes(mode StreamingMode, iteration int) (int, error) {
	return modeTargetBytes(mode, iteration)
}

// modeTargetBytes returns the per-fetch byte target libfdb_c would use for this mode and
// iteration, porting fdb_c.cpp:1011-1029. iteration is 1-based, as it is in the C API.
//
// The returned value is a TARGET for the request (which the storage server truncates against),
// not a ceiling on what the client will accumulate — see readpath.go's soft byte limit for the
// half that ends the call.
//
// It reports ByteLimitUnlimited for EXACT, which is not a Go shortfall but agreement with C:
// mode_bytes_array[EXACT] is BYTE_LIMIT_UNLIMITED, so an EXACT read has no byte target and stays
// bounded by rows alone.
func modeTargetBytes(mode StreamingMode, iteration int) (int, error) {
	// fdb_c.cpp:1011 — WANT_ALL is rewritten to SERIAL before any target is derived, so it
	// shares SERIAL's 80000 rather than having an entry of its own.
	if mode == StreamingModeWantAll {
		mode = StreamingModeSerial
	}

	if mode == StreamingModeIterator {
		// fdb_c.cpp:1017 — ITERATOR with iteration <= 0 is client_invalid_operation.
		if iteration <= 0 {
			return 0, Error{Code: 2005} // client_invalid_operation
		}
		if iteration > maxIteration {
			iteration = maxIteration // fdb_c.cpp:1019
		}
		return iterationProgression[iteration-1], nil
	}

	// fdb_c.cpp:1022 — `mode >= 0 && mode <= FDB_STREAMING_MODE_SERIAL` in C numbering.
	// Anything else is client_invalid_operation, which is also what keeps the index below in
	// bounds: the guard is the bounds check, so it must be expressed on the C value.
	i := cModeIndex(mode)
	if i < 0 || i >= len(modeBytesTable) {
		return 0, Error{Code: 2005} // client_invalid_operation
	}
	return modeBytesTable[i], nil
}

// fdb_c.cpp:1026-1029 also COMBINES an explicitly requested target_bytes with the mode's own
// target by taking the minimum. That rule is deliberately not ported: it has only one operand
// here. The C API takes target_bytes as a per-call argument, whereas this client's RangeOptions
// — like Apple's Go binding, which it mirrors — exposes Limit, Mode and Reverse and no way to
// name a byte target, so `requested` is always unset and the combination always reduces to the
// mode's own value.
//
// The clamp that IS live is a different one, against the per-reply CEILING rather than against
// a caller's target: sendRangeRPC takes min(replyByteLimit, byteTarget) when it builds the
// request, porting transformRangeLimits (NativeAPI.actor.cpp:4223). If a byte-target option is
// ever added to RangeOptions, restore the min() from the lines cited above rather than folding
// it into that clamp — they answer different questions.
