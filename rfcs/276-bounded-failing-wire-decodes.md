# RFC-276: Bounded, failing wire decodes

**Status:** Draft

**Base:** stacked on PR #997 (RFC-271), which makes `ReadVectorCount` fail with
`*wire.VectorCountError` on a count the buffer cannot hold and makes the locations-reply
parser fail instead of skipping malformed entries. Both changes are prerequisites here.

**Items** (the Go FDB client entries of the 2026-10-24 quality review, open PR #994):

1. Decoding can allocate quadratically through vector element offsets that alias one
   inner vector (`pkg/fdbgo/wire/reader.go` `ReadVectorCount`, reachable through
   `GetMappedKeyValuesReply`).
2. Follow-up from RFC-271: the generated decoders treat a `VectorCountError` as an absent
   vector (`TestGeneratedDecoderDropsOversizedVector` pins that today).
3. Fuzz coverage. The client reply-parser fuzzers (`client/parse_fuzz_test.go`) check
   only that nothing panics, cover a handful of reply types, and have no allocation
   oracle. None reaches `GetMappedKeyValuesReply`, and no fuzzer covers the transport
   frame reader.

## Problem

The generated decoders (`pkg/fdbgo/wire/types/*_generated.go`, written by
`cmd/fdb-schema-extract/main.cpp` `emitReads`) allocate `make([]T, 0, count)` for every
vector of tables and decode each element through its own offset. Nothing stops several
offsets from pointing at the same element. In `GetMappedKeyValuesReply`, N `Data`
offsets that all point at one `MappedKeyValueRef` whose range result holds M rows
decode N×M `KeyValueRef`s from O(N+M) bytes: a reply of a few KB allocates hundreds of
MB. `parseGetKeyServerLocationsReply` (`pkg/fdbgo/client/locality.go`) walks the same
shape by hand: N result offsets aliasing one pair with M servers produce N×M
`ServerInfo`s, each with an allocated address string. For a server whose slot 2 holds no
endpoint, it also scans every slot its vtable claims (up to 32K) and allocates an error
for each.

The decoders also swallow structural errors, and none of these cases reports one:

- a failed `ReadVectorCount` (including RFC-271's `VectorCountError`) leaves the vector
  empty;
- an element whose offset does not resolve is skipped;
- a present nested, optional or variant table whose offset does not resolve is left
  zero;
- a present scalar or byte field whose bytes lie outside the buffer reads as zero or nil.
  A present `Optional<Value>` with a corrupt offset, for example, becomes an empty
  value;
- `Parse<T>StringVector` (the `VecSerStrategy::String` rows of `GetKeyValuesReply`)
  returns the rows it decoded before the blob ran out.

`UnmarshalFromReader` has no error result, so a malformed reply decodes as a well-formed
shorter one.

## C++

C++ bounds neither. `ObjectReader` holds only a data pointer, not a length
(`flow/include/flow/ObjectSerializer.h:96-128`); a vector load reserves the stored
count and decodes each element through its offset with no bounds check
(`flow/include/flow/flat_buffers.h:1140-1153`). A malformed or aliasing reply is
undefined behaviour in libfdb_c. A string-strategy vector is the exception: its load
asserts that the elements fill the blob exactly
(`flow/include/flow/Arena.h:1514-1525`). Go needs its own bound, and the bound must
never reject a reply C++ writes.

## Change

1. **Budget.** `wire.DecodeBudget` holds the number of vector-of-table elements a decode
   may still produce, initially `len(buffer)/4`. `Take(n)` fails with a typed
   `*wire.DecodeBudgetError` once a vector would exceed it. A buffer C++ writes never
   reaches the limit. C++ writes every table and vector once (`flat_buffers.h`
   `SaveVisitorLambda`, no sharing except vtables), and every element of a vector of
   tables has its own 4-byte offset slot, so the counts of all its vectors sum to at most
   `len/4`. A sum above that means offsets alias.
2. **Generated decoders fail.** `emitReads` emits an unexported
   `decode(r *wire.Reader, b *wire.DecodeBudget) error` per type. It returns a typed
   `*wire.DecodeError{Type, Field, Err}` when any of these happens:
   - a vector count does not decode, including a `VectorCountError`;
   - a vector element does not resolve;
   - a present nested table, `Optional` table or variant-alternative table does not
     resolve;
   - the budget is exhausted;
   - a present field's bytes lie outside the buffer. The `wire.Reader` leaf reads (scalars,
     byte strings, bare out-of-line scalars) keep returning zero or nil, and now also
     record the first such field. `decode` returns it from `r.Err()` as a
     `*wire.FieldBoundsError` naming the slot.

   The decode passes one budget through the whole tree. A field the vtable does not
   reach, or marks absent, keeps its zero value, as in C++ (`flat_buffers.h:1041-1046`).
   Every nested read checks `FieldPresent` first, so a reply from a schema with fewer
   fields still decodes.

   `UnmarshalFromReader(r) error` and `UnmarshalFDB(data) error` start one budget per
   decode from the whole buffer and call `decode`. `Parse<T>VectorFromReader` returns
   `([]T, error)`. `Parse<T>StringVector` returns `([]T, error)` and fails with a
   `*wire.StringVectorError` unless the elements fill the blob exactly, as C++ asserts.
   An empty blob stays an empty vector. `KeyRangeRef`'s hand-written decoder gets the
   same shape and returns its reader's `Err()`. On error the target holds a partial
   decode, which callers discard.
3. **Hand-written parsers.** `parseGetKeyServerLocationsReply` charges one budget for
   the results vector and for every pair's server vector, and returns its
   `locationsReplyError` when the budget is exhausted. Its fallback endpoint scan stops
   at the slots the `StorageServerInterface` schema defines, not at the vtable length
   the peer claims. `parseClientDBInfoFromReader` walks each of its two vectors
   (grvProxies, commitProxies) once, and `parseSplitRangeReply` walks its splitPoints
   once. No element of these holds a nested vector, so aliasing cannot multiply their
   work. RFC-271's `ReadVectorCount` bound already covers them, pinned by its
   oversized-count tests.

   Every non-generated `UnmarshalFromReader` caller returns the error:
   - client: `parseCommitOutcome` (commitpath.go), `ReadEndpointFromSlot` (endpoint.go),
     `parseGetReadVersionReply` (grv.go), `parseGetKeyServerLocationsReply`
     (locality.go), `parseGetMappedKeyValuesReply` (mappedrange.go),
     `parseWaitMetricsReply` (metrics.go), `parseGetKeyValuesReply` and
     `parseGetValueReply` (readpath.go);
   - `tools/clientbench`: wiregolden and wiretrace count the message as undecodable.

   A malformed reply is then a parse failure, handled where each parser already handles
   one.
4. **DIVERGENCES.md.** A row in the pure-Go client section: Go rejects replies whose
   offsets alias beyond the budget, or whose present fields do not resolve. In libfdb_c
   these are undefined behaviour.

## Fuzzers

- `FuzzUnmarshalFDB` (`wire/types`) feeds raw bytes to every generated type's
  `UnmarshalFDB`, through a type registry the generator emits. Unlike the client parser
  fuzzers, it covers every generated type. Its oracle has two parts:
  - the error is nil, a `NewReader` envelope error, or wraps `*wire.DecodeError`;
  - the bytes allocated (`runtime.MemStats.TotalAlloc`) stay within `1024·len(data) + 1 MiB`.

  The seeds are the ground-truth vectors and the aliasing `GetMappedKeyValuesReply`. On
  the pre-change code the aliasing seed fails the allocation bound.
- `FuzzParseKeyValueRefStringVector` (`wire/types`). Its oracle: a nil error means
  re-packing the rows reproduces the input exactly; otherwise the error is a
  `*wire.DecodeError`.
- `FuzzFrameReader` (`transport`) is new coverage, not a regression for this change. Its
  oracle: `FrameReader.Read` never panics. It returns either an error or a token and body
  whose length matches the declared packet length and whose checksum verified. A frame
  `WriteFrame` built reads back unchanged. Read preallocates the declared length up to
  100 MiB, as C++ does (`fdbrpc/FlowTransport.actor.cpp:1390-1407`, `PACKET_LIMIT`
  `flow/Knobs.cpp:222`).

## Wire compatibility and performance

No bytes change; only malformed input decodes differently. The budget is one stack value
per decode and one subtraction per vector. The fault record costs a store on the
out-of-bounds path and one check per decoded table. The decode paths the allocation
tests pin stay allocation-free.

## Tests

Regressions (each fails on the pre-change code):

- `wire/types`:
  - The aliasing `GetMappedKeyValuesReply` (N offsets to one element with M rows, N×M
    above the budget) fails with `*wire.DecodeBudgetError`.
  - A reply fails with `*wire.DecodeError` naming the type and field in each of these
    cases: an oversized vector count (`TestGeneratedDecoderDropsOversizedVector`
    inverted), an unresolvable vector element, an unresolvable nested table, a present
    `Optional<Error>` whose table does not resolve, and a present variant alternative
    whose table does not resolve.
  - A `GetValueReply` whose present `Optional<Value>` offset lies past the buffer fails
    with a `*wire.FieldBoundsError`.
  - `Parse<T>VectorFromReader` returns the element error. `ParseKeyValueRefStringVector`
    fails on a count the blob cannot hold, on an element that overruns it, and on
    trailing bytes.
- `client`, one table test over every reply parser above: a ground-truth reply with one
  present field moved past the buffer fails with a `*wire.DecodeError`. Further cases:
  - an aliasing `GetKeyServerLocationsReply` fails with a budget error;
  - a server whose vtable claims 32K slots costs the parser a bounded number of
    allocations.
- `tools/clientbench/wiretrace`: a malformed reply counts as undecodable.
- `FuzzUnmarshalFDB`: the aliasing seed fails the allocation oracle.

Non-regression guards (pass before and after):

- Every ground-truth vector decodes with a nil error.
- A dense legitimate reply decodes within the budget: a `GetMappedKeyValuesReply` of
  many range rows holding many empty `KeyValueRef`s, marshaled by the Go encoder that
  the ground-truth vectors pin to C++ layout. The test calls `decode` with its own
  budget and asserts the budget consumed equals the number of decoded elements, so a
  decoder that charges an element twice fails.
- Short vtables: a reply whose nested `MappedKeyValueRef` and top-level vtables are cut
  to fewer slots decodes with a nil error and zero-valued absent fields.
- `DecodeBudget` arms, in `wire`.
- `FuzzFrameReader` seeds.
