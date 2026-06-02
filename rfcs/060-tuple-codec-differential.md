# RFC-060: Differential proof of tuple-codec byte-identity vs libfdb_c

**Status:** Draft
**Item:** RFC-010 C3 (fresh differential axes). The tuple/key encoding is the wire hard
line (CLAUDE.md: "key encoding … MUST match Java exactly. Divergence there is a bug, full
stop"); it currently has **zero** differential coverage against libfdb_c's codec.

## Problem

`pkg/fdbgo/fdb/tuple` is a near-verbatim port of Apple's Go tuple binding, and an inspection
diff shows the core encode/decode path is byte-for-byte identical (only `interface{}`→`any`,
import paths, and const-block formatting differ). But "identical by inspection of a port" is
not the proof the goal demands ("absolute proof we're identical to the C client"), and two
gaps make the inspection argument insufficient:

1. **Go-only hot-path helpers.** The port *adds* `PackWithPrefix`, `PackConcatWithPrefix`,
   `Pack1WithPrefix`, `Pack1ConcatWithPrefix`, the `Packer`/`AppendInto` API, and a
   `packerPool`. These are hand-written, **not present in libfdb_c's binding**, and they
   build the actual index-entry and record keys written to FDB — i.e. the wire. A subtle
   bug there (a mis-sized buffer, a dropped element, a prefix-placement slip) corrupts keys
   that Java/C clients then misread. Nothing currently byte-compares their output to the
   canonical codec.
2. **No regression sentinel against drift.** Future perf work on `encodeInt`/`encodeBytes`/
   the pool could silently diverge. A differential test freezes the wire contract.

The existing `pkg/fdbgo/fdb/tuple` tests are all **go-internal** (pack→unpack round-trips and
sort-order checks); none import or compare against `github.com/apple/foundationdb/.../tuple`.

## Investigation

- libfdb_c's binding ships `testdata/tuples.golden` (gob `map[string][]byte`) and
  `TestTuplePacking` asserts `cgotuple.Tuple.Pack() == golden` for a canonical case set
  (UUIDs, nil-escaping, large bytes, nested tuples, randomized integers/floats/doubles seeded
  with `rand.NewSource(1)`). So `cgotuple.Pack()` is itself pinned to the cross-language wire
  vectors. Proving `gotuple.Pack() == cgotuple.Pack()` for the same inputs therefore makes
  `gotuple.Pack()` transitively equal to the golden vectors — a clean **live** differential
  needs no gob parsing.
- The `bench` package already imports both `cgotuple` and `gotuple` (used in the
  directory-layer interop test), so the differential compiles there with no new dependency.
- Encoding paths with non-obvious branches worth pinning explicitly (these are where a port
  is most likely to drift): the per-size-class integer encoding at every `sizeLimits[n]`
  boundary; the `*big.Int` `posIntEnd`/`negIntStart` length-prefixed path (>8 bytes) and the
  negative leading-`0xff` zero-fill loop; the float/double sign-bit-flip (`adjustFloatBytes`)
  for negatives vs the sign-bit-only flip for positives; the `0x00`→`0x00 0xFF` escaping in
  bytes/strings and the extra `0xFF` after a nested-tuple `nil`; the `PackWithVersionstamp`
  little-endian offset suffix (2-byte for API <520, 4-byte otherwise).

## Fix

This is **net-new test coverage**, not a code change — the codec is believed correct and the
differential is expected to pass on the first run. (Per CLAUDE.md, an extension/new-axis test
is judged red→green by *introducing a deliberate fault and confirming the test catches it*,
since there is no pre-existing bug to expose. We verify the battery has teeth that way, then
revert the fault.) If the differential surfaces a real divergence, that is a wire bug and gets
fixed in `tuple.go` under this RFC, with the differential as its sentinel.

New file `pkg/fdbgo/bench/differential_tuple_test.go` (no FDB container needed for the pure
codec parts; one end-to-end FDB case for the wire path):

1. **`TestDifferential_TuplePack`** — a table of logical values, each built as both a
   `gotuple.Tuple` and a `cgotuple.Tuple`, asserting `go.Pack()` byte-equals `cgo.Pack()`.
   Covers every type code and the boundary values enumerated above.
2. **`TestDifferential_TupleUnpackCross`** — `gotuple.Unpack(cgo.Pack(x))` and
   `cgotuple.Unpack(go.Pack(x))` both recover `x` (normalized), proving decode parity in both
   directions.
3. **`TestDifferential_TuplePackHelpers`** — the go-only helpers
   (`PackWithPrefix`/`Pack1WithPrefix`/`Pack1ConcatWithPrefix`/`PackConcatWithPrefix`/
   `Packer.AppendInto`) byte-equal the equivalent `cgotuple.Tuple{prefix-as-bytes…}.Pack()`
   construction — pinning the hand-written hot path to the canonical codec.
4. **`TestDifferential_TuplePackVersionstamp`** — `gotuple.PackWithVersionstamp` vs
   `cgotuple.PackWithVersionstamp`: byte-equal output (including the offset suffix) for
   incomplete-versionstamp tuples at several positions and user versions.
5. **`TestDifferential_TupleWireRoundtrip`** (FDB) — write a record/key packed by `gotuple`,
   read it back addressing the key packed by `cgotuple` (and vice versa) against a real FDB
   container, proving the keys are wire-identical end to end.
6. **`FuzzDifferential_TuplePack`** — fuzz a random element stream, build the same logical
   tuple with both codecs, assert `Pack()` byte-equality; 0 mismatches over a long run.

## Performance

Test-only. No production-code change unless a divergence is found.

## Test plan

The tests above ARE the plan. Teeth verified by fault injection (e.g. flip one
`sizeLimits` entry, drop the nested-`nil` `0xFF`, or mis-place the versionstamp offset) and
confirming each named case fails, then reverting. Run under `bazelisk test
//pkg/fdbgo/bench:bench_test` and the fuzz target for ≥60s with 0 mismatches.
