# RFC-276: Bounded, failing wire decodes

**Status:** Draft

**Items** (TODO.md, "Quality review 2026-10-24", Go FDB client): decoding can allocate
quadratically through vector element offsets that alias one inner vector, reachable
through `GetMappedKeyValuesReply` (Medium); the generated decoders treat a vector-count
error as an absent vector (follow-up from RFC-271).

## Problem

The generated decoders (`pkg/fdbgo/wire/types/*_generated.go`, written by
`cmd/fdb-schema-extract/main.cpp` `emitReads`) allocate `make([]T, 0, count)` for every
vector of tables and decode each element through its own offset. Nothing stops several
offsets from pointing at the same element. In `GetMappedKeyValuesReply`, N `Data`
offsets that all point at one `MappedKeyValueRef` whose range result holds M rows
decode N×M `KeyValueRef`s from O(N+M) bytes: a reply of a few tens of KB can allocate
gigabytes.

The same decoders swallow every structural error: a failed `ReadVectorCount` leaves the
vector empty, an element whose offset does not resolve is skipped, and a present nested
table whose offset does not resolve is left zero. `UnmarshalFromReader` has no error
result, so a malformed reply decodes as a well-formed shorter one.

## C++

C++ bounds neither. `ObjectReader` holds only a data pointer, not a length
(`flow/include/flow/ObjectSerializer.h:96-128`); a vector load reserves the stored
count and decodes each element through its offset with no bounds check
(`flow/include/flow/flat_buffers.h:1140-1153`). A malformed or aliasing reply is
undefined behaviour in libfdb_c. Go needs its own bound; the requirement is that it
never rejects a reply C++ writes.

## Change

1. **Budget.** `wire.DecodeBudget` holds the number of vector elements a decode may
   still produce, initially `len(buffer)/4`, and `Take(n)` fails with a typed
   `*wire.DecodeBudgetError` once a vector would exceed it. A buffer C++ writes never
   reaches the limit: every element of a vector of tables or strings has its own 4-byte
   offset slot in the buffer (`flat_buffers.h` save of `VectorLike`), so the counts of
   all its vectors sum to at most `len/4`. Aliased offsets are what make the sum larger.
2. **Generated decoders fail.** `emitReads` emits an unexported
   `decode(r *wire.Reader, b *wire.DecodeBudget) error` per type that returns a typed
   `*wire.DecodeError{Type, Field, Err}` when a vector count, a vector element, or a
   present nested, optional or variant-alternative table does not decode, or the budget
   is exhausted. Absent fields keep their zero value, as in C++ (`flat_buffers.h:1041-1046`).
   `UnmarshalFromReader(r) error` and `UnmarshalFDB(data) error` start one budget per
   decode from the whole buffer and call `decode`; `Parse<T>VectorFromReader` returns
   `([]T, error)`. `KeyRangeRef`'s hand-written decoder gets the same shape.
3. **Callers** (`pkg/fdbgo/client` reply parsers, `tools/clientbench`) return the error,
   so a malformed reply is a parse failure, handled where each parser already handles
   one. The hand-written parsers that walk vectors (`coordinator.go`, `metrics.go`)
   charge the same budget.
4. **Fuzzers.** A raw-bytes fuzzer feeds every generated type's `UnmarshalFDB`
   (registry emitted by the generator), seeded with the ground-truth vectors and the
   aliasing `GetMappedKeyValuesReply`; a frame-reader fuzzer covers
   `transport.FrameReader.Read`.

Scalar and byte fields keep their current reads (a scalar past the object reads as
zero, an unreadable byte string as nil); they allocate nothing beyond the buffer.

## Wire compatibility and performance

No bytes change; only malformed input decodes differently. Every ground-truth vector and
the marshal round-trip fuzzers decode as before. The budget is one stack value per
decode and one subtraction per vector; the decode paths the allocation tests pin stay
allocation-free.

## Tests

Each fails on the pre-change code.

- `types_test`: the aliasing `GetMappedKeyValuesReply` (N offsets to one element with
  M rows, N×M above the budget) fails with `*wire.DecodeBudgetError`; a reply with an
  unreadable vector count, element, or present nested table fails with
  `*wire.DecodeError` naming the field; every ground-truth vector still decodes.
- `wire_test`: `DecodeBudget` arms.
- Client parsers return the decode error for a malformed reply.
- Fuzzers run their seeds in the fast lane.
