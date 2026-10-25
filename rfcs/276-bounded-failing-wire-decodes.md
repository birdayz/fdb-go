# RFC-276: Bounded, failing wire decodes

**Status:** Implemented

**Base:** RFC-271 (PR #997, merged), which makes `ReadVectorCount` fail with
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
   `decode(r *wire.Reader, b *wire.DecodeBudget) error` per type. It fails when any of
   these happens:
   - a vector count does not decode, including a `VectorCountError`;
   - a vector element does not resolve;
   - a present nested table, `Optional` table or variant-alternative table does not
     resolve;
   - the budget is exhausted;
   - a present field's bytes lie outside the buffer, or the vtable claims more entries
     than the buffer holds. The `wire.Reader` leaf reads (scalars, byte strings, bare
     out-of-line scalars) keep returning zero or nil, and now also record the first such
     slot. `decode` returns it from `r.Err()` as a `*wire.FieldBoundsError`.

   Every failure reaches callers as a `*wire.DecodeError{Type, Field, Err}` wrapping the
   specific cause (`VectorCountError`, `DecodeBudgetError`, `FieldBoundsError`,
   `StringVectorError`, or the reader's error). `Field` is empty when the cause is a leaf
   read or a whole vector. The innermost type that failed names the error.

   Absent fields follow C++ (`flat_buffers.h:1022-1046`). A field the vtable does not
   reach, or marks absent, keeps its zero value, and every nested read checks
   `FieldPresent` first, so a reply from a schema with fewer fields still decodes. An
   `Optional` is present only with tag 1 and its value slot present. A variant decodes
   only a known tag with its value slot present (`LoadAlternative`,
   `flat_buffers.h:863-886`). Anything else leaves the zero value, as C++ leaves the
   member empty. `wire.ReadInlineReplyError`, `ReadOptionalInt32` and
   `ReadOptionalString` use the same tag rule.

   An `Optional` of a table decodes as that table: the extractor now gives every
   `Optional<T>` whose `T` has a Go type a nested decode, which turns
   `NetworkAddressList.SecondaryAddress` from a byte string into the `NetworkAddress` C++
   writes. An `Optional` of a table with no Go type (`EncryptKeyProxyInterface`,
   `ClientTrCommitCostEstimation`, `WipedString`) records only its presence. These
   fields used to be read as byte strings, which the stricter reads would refuse.

   A present field whose RelativeOffset is 0 keeps reading as empty: C++ then reads the
   offset word itself, a zero length or count, so the value is the same. A nested table
   with a zero offset still fails, as before.

   `UnmarshalFromReader(r) error` and `UnmarshalFDB(data) error` start one budget per
   decode from the whole buffer and call `decode`. `Parse<T>VectorFromReader` returns
   `([]T, error)`. `Parse<T>StringVector` returns `([]T, error)` and fails unless the
   elements fill the blob exactly, as C++ asserts. An empty blob stays an empty vector.
   `KeyRangeRef`'s hand-written decoder gets the same shape. On error the target holds a
   partial decode, which callers discard.
3. **Hand-written parsers.**
   - `parseGetKeyServerLocationsReply` charges one budget for the results vector and
     for every pair's server vector. A server whose slot-2 endpoint is present but does
     not decode fails the reply with its `locationsReplyError`. Only a slot-2 endpoint
     that is absent (`endpointAbsentError`) or has a zero token falls back to the scan.
     The scan probes slots that may hold no endpoint at all, so it skips their errors.
     It stops at the slots the `StorageServerInterface` schema defines, not at the
     vtable length the peer claims. Its range-key check from RFC-271 stays: it also
     rejects a zero offset, which `ReadBytes` reads as an empty key, as C++ does.
   - `parseClientDBInfoFromReader` walks each of its two vectors (grvProxies,
     commitProxies) once, and `parseSplitRangeReply` walks its splitPoints once. No
     element of these holds a nested vector, so aliasing cannot multiply their work.
     RFC-271's `ReadVectorCount` bound covers them. `parseClientDBInfoFromReader`'s own
     leaf reads (id, forward) return `r.Err()`.
   - `parseGetKeyReply` decodes the reply through the generated `GetKeyReply` and its
     selector through the generated `KeySelectorRef`, replacing its own leaf reads.
   - `wire.ReadErrorOrInto` fails with a `DecodeError` when an error alternative's code
     lies outside the buffer, instead of returning `FDBError{Code: 0}`.

   Hand-written parsers keep their own error types (`locationsReplyError`, the
   `fmt` errors of the proxy-list and split-point walks); only the generated decoders
   and the paths above produce a `DecodeError`.

   Every non-generated `UnmarshalFromReader` caller returns the error:
   - client: `parseCommitOutcome` (commitpath.go), `ReadEndpointFromSlot` (endpoint.go),
     `parseGetReadVersionReply` (grv.go), `parseGetKeyServerLocationsReply`
     (locality.go), `parseGetMappedKeyValuesReply` (mappedrange.go),
     `parseWaitMetricsReply` (metrics.go), `parseGetKeyReply`, `parseGetKeyValuesReply`
     and `parseGetValueReply` (readpath.go);
   - `tools/clientbench`: wiregolden and wiretrace count the message as undecodable;
   - the `ReadErrorOrInto` doc example.

   A malformed reply is then a parse failure, handled where each parser already handles
   one.
4. **DIVERGENCES.md.** A row in the pure-Go client section: Go rejects replies whose
   offsets alias beyond the budget, or whose present fields do not resolve. In libfdb_c
   these are undefined behaviour. **CHANGELOG.md** records the `wire/types` signature
   changes as breaking.

## Fuzzers

- `FuzzUnmarshalFDB` (`wire/types`) feeds raw bytes to every generated type's
  `UnmarshalFDB`, through a type registry the generator emits. Unlike the client parser
  fuzzers, it covers every generated type. Its oracle has two parts:
  - for input `wire.NewReader` accepts, the error is nil or a `*wire.DecodeError`;
  - the bytes allocated (`runtime.MemStats.TotalAlloc`) stay within `1024·len(data) + 1 MiB`.

  The seeds are the ground-truth vectors and the aliasing `GetMappedKeyValuesReply`. On
  the pre-change code the aliasing seed fails the allocation bound.
- `FuzzParseKeyValueRefStringVector` (`wire/types`). Its oracle: an error is a
  `*wire.StringVectorError` with no rows. Otherwise the input is empty with no rows, or
  re-packing the rows reproduces the input exactly.
- `FuzzFrameReader` (`transport`) is new coverage, not a regression for this change. Its
  oracle: `FrameReader.Read` never panics. It returns either an error or a token and body
  whose length matches the declared packet length and whose checksum verified. A frame
  `WriteFrame` built reads back unchanged. Read preallocates the declared length up to
  100 MiB, as C++ does (`fdbrpc/FlowTransport.actor.cpp:1390-1407`, `PACKET_LIMIT`
  `flow/Knobs.cpp:222`).

## Wire compatibility and performance

No bytes the client sends change; only malformed input decodes differently. The generated
encoders of the two retyped Optional tables (`NetworkAddressList.SecondaryAddress`,
`CommitTransactionRef` `spanContext`) now write a nested table, as C++ does, where they
wrote a byte string; the client sets neither. The budget is one stack value
per decode and one subtraction per vector. The fault record costs a store on the
out-of-bounds path and one check per decoded table. The decode paths the allocation
tests pin stay allocation-free.

## Tests

Regressions (each fails when its part of the change is reverted; the PR records each
revert):

- `wire`: a present scalar, byte string or bare out-of-line scalar outside the buffer,
  and a vtable longer than the buffer, give `Err() = *FieldBoundsError`, and a later
  fault does not replace the first. A vector field past the buffer is a
  `ReadVectorCount` error.
- `wire/types`:
  - The aliasing `GetMappedKeyValuesReply` (2000 offsets to one row of 1000 empty rows,
    24 KB) fails with `*wire.DecodeBudgetError`.
  - A `DecodeError` naming the type and field for an oversized vector count
    (RFC-271's `TestGeneratedDecoderDropsOversizedVector`, inverted as
    `TestGeneratedDecoderRejectsOversizedVector`), an unresolvable vector
    element (`GetMappedKeyValuesReply.Data`), nested table (`MappedKeyValueRef.KeyValue`),
    `Optional<Error>` (`GetKeyValuesReply.Error`) and variant alternative
    (`MappedKeyValueRef.ReqAndResult`).
  - A `GetValueReply` whose present value lies past the buffer fails with a
    `FieldBoundsError`, and so does a `KeyRangeRef` conflict range whose begin does.
  - Tags: a `GetValueReply` value with tag 2, or with tag 1 and no value slot, an inline
    error with tag 2, and a variant with tag 3 all read as absent.
  - An ErrorOr error whose code lies past the buffer fails with a `DecodeError`.
  - `Parse<T>VectorFromReader` returns the element error. `ParseKeyValueRefStringVector`
    fails with a `StringVectorError` on a short blob, a count the blob cannot hold, an
    element that overruns it, and trailing bytes.
- `client`: one table test over the reply parsers. Each moves one present field past
  the buffer and expects a `*wire.DecodeError`. The replies are ground-truth vectors,
  except the `ClientDBInfo` id case, which uses a minimal Go-marshaled reply:
  - `GetValueReply` value, `GetKeyReply` penalty, `GetKeyValuesReply` rows,
    `GetMappedKeyValuesReply` rows;
  - `GetReadVersionReply` version, `CommitID` version, `StorageMetrics` bytes;
  - `ClientDBInfo` id, and the locations reply's slot-2 endpoint address.

  Further cases:
  - `parseGetKeyValuesReply` fails with a `StringVectorError` on a row blob with a
    trailing byte or a count above its rows;
  - `ClientDBInfo.forward` with tag 2 reads as no forward;
  - an aliasing `GetKeyServerLocationsReply` (300 results × 300 servers) fails with a
    budget error;
  - a server with a zero token whose vtable claims 32K slots costs at most 500
    allocations.
- `tools/clientbench`: in wiretrace, a GRV, commit or range request with a field past
  the buffer is dropped as undecodable, and such a reply marks its request malformed.
  In wiregolden, a send whose key lies past the buffer is undecodable.
- The send recorder's transcript gate reports the decode failure for a malformed
  request, and still reports "does not re-encode" for a request with a trailing byte.
- `FuzzUnmarshalFDB`: the aliasing seed fails the allocation oracle.

- `client`: the new ground-truth vectors `GetKeyServerLocationsReply_secondary_address`
  and `ClientDBInfo_secondary_address` (endpoints with a TLS secondary address) parse,
  with the primary address; a selector key past the buffer fails `parseGetKeyReply`;
  a range key with a zero offset still fails the locations reply. A location pair, a
  server or a split-point reply whose vtable runs past the buffer fails its parse, and a
  server whose slot-2 endpoint or its wrapper field is absent is still found by the scan.
- `wire`: `ReadOptionalInt32`/`ReadOptionalString` treat tag 2 as absent and record a
  value past the buffer.
- `TestReaderReuseClearsFault`: a faulted reader is reused through `InitReader`,
  root-object initialization, nested initialization and `ReadErrorOrInto`; each clears
  the old fault and reads the next message.
- `TestGroundTruthOptionalTables`: C++ fixtures pin both span contexts on a commit
  request, an endpoint's secondary address, and a presence-only commit-cost table.
  The registered Optional tables also re-encode byte-exactly in `TestGroundTruthMarshal`.

Non-regression guards (pass before and after):

- Every ground-truth vector decodes with a nil error, and `TestReplyGroundTruth` (now in
  the fast lane) asserts each reply vector through its production parser. New C++
  vectors cover the fields the stricter decoders now check: the inline error on
  `GetValueReply`, `GetKeyReply`, `GetKeyValuesReply` and `GetMappedKeyValuesReply`, and a
  `GetReadVersionReply` with an `ssVersionVectorDelta`, and one with a `tagThrottleInfo`
  entry. `ClientTagThrottleLimits` writes `expiration - now()`, so that vector uses
  `NO_EXPIRATION` (DBL_MAX), the only regen-stable expiration.
- A dense legitimate reply decodes within the budget: a `GetMappedKeyValuesReply` of
  many range rows holding many empty `KeyValueRef`s, marshaled by the Go encoder that
  the ground-truth vectors pin to C++ layout. The test calls `decode` with its own
  budget and asserts the budget consumed equals the number of decoded elements, so a
  decoder that charges an element twice fails.
- Short vtables: a reply whose nested `MappedKeyValueRef` and top-level vtables are cut
  to fewer slots decodes with a nil error and zero-valued absent fields. That includes
  a variant whose tag slot survives the cut and whose value slot does not.
- An empty string-vector blob and a zero count both give no rows and no error.
- `DecodeBudget` arms, in `wire`.
- `FuzzFrameReader` and `FuzzParseKeyValueRefStringVector` seeds.
