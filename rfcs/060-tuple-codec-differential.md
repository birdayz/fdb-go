# RFC-060: Differential proof of tuple-codec byte-identity vs libfdb_c

**Status:** Implemented
**Item:** RFC-010 C3 (fresh differential axes). The tuple/key encoding is the wire hard
line (CLAUDE.md: "key encoding … MUST match Java exactly. Divergence there is a bug, full
stop"); it currently has **zero** differential coverage against libfdb_c's codec.

## Problem

`pkg/fdbgo/fdb/tuple` is a near-verbatim port of Apple's Go tuple binding, and an inspection
diff shows the core encode/decode path is byte-for-byte identical (only `interface{}`→`any`,
import paths, and const-block formatting differ). So a pure-codec differential on the **core
path** is low-yield insurance. The RFC's actual justification is narrower and real:

1. **Go-only hot-path helpers (the reason this RFC exists).** The port *adds*
   `PackWithPrefix`, `PackConcatWithPrefix`, `Pack1WithPrefix`, `Pack1ConcatWithPrefix`, the
   `Packer`/`AppendInto` API, and a `packerPool` — **none of which exist in libfdb_c's
   binding**, and all of which are hand-written and build the actual index-entry and record
   keys written to FDB (the wire). Today they have **zero** cross-codec comparison — only
   Go-internal round-trips. A mis-sized buffer, dropped element, prefix-placement slip, or
   stale pool state corrupts keys that Java/C clients then misread. This gap alone earns the
   RFC.
2. **Drift sentinel.** Future perf work on `encodeInt`/`encodeBytes`/the pool could silently
   diverge from the canonical codec. The pure-codec cases freeze the wire contract cheaply.

The existing `pkg/fdbgo/fdb/tuple` tests are all **go-internal** (pack→unpack round-trips and
sort-order checks); none import or compare against `github.com/apple/foundationdb/.../tuple`.

## Investigation

- **The golden corpus is a sanity anchor, not the proof.** libfdb_c's binding ships
  `testdata/tuples.golden` and `TestTuplePacking` asserts `cgotuple.Tuple.Pack() == golden`.
  But its generators are narrow: `genInt = rand.Int63()` is always positive (never the
  negative size-classes, `MinInt64`, the `negIntStart` leading-`0xff` zero-fill, the >8-byte
  big.Int paths, or the `decodeInt` high-bit→uint64 case), `genFloat/genDouble =
  NormFloat64()` never emits +0/-0/±Inf/NaN/subnormals, and there are **no versionstamp cases
  at all**. So transitivity (`go==cgo`, `cgo==golden` ⟹ `go==golden`) holds **only for
  `Pack()` and only on the subset golden exercises**. The explicit boundary battery below is
  where the proof lives.
- **Shared-latent-bug blind spot (stated honestly).** `go==cgo` proves nothing if both
  inherited the *same* port error. The golden test closes that for `Pack()` — but **only
  `Pack()` is golden-pinned**. `Unpack` and `PackWithVersionstamp` have no golden backstop, so
  those cases are go-vs-cgo-only and could in principle co-fail. We do NOT claim
  golden-transitivity for decode or versionstamp; for `Pack()` we do (on golden's subset). The
  helper cases (#3) are immune to this concern by construction: they are compared to
  `cgotuple.Pack()`, which IS golden-pinned, and the helpers are NOT a shared port (cgo has no
  such code), so any helper bug is independent.
- The `bench` package already imports both `cgotuple` and `gotuple` (directory-layer interop
  test) with both API versions pinned to 730, so the differential compiles there with no new
  dependency.

## Fix

This is **net-new test coverage**, not a code change — the codec is believed correct and the
differential is expected to pass on the first run. Per CLAUDE.md, a new-axis test (no
pre-existing bug to expose) is judged red→green by **introducing a deliberate fault and
confirming a named case catches it, then reverting** — done for each structural branch
(flip a `sizeLimits` entry; drop the nested-`nil` `0xFF`; mis-place the versionstamp offset;
corrupt a helper's prefix splice). If the differential surfaces a real divergence, that is a
wire bug fixed in `tuple.go` under this RFC with the differential as its sentinel.

New file `pkg/fdbgo/bench/differential_tuple_test.go` — **no FDB container needed** (the codec
is pure; an end-to-end FDB round-trip would be tautological — if `Pack()` bytes are equal, the
same bytes address the same byte-keyed FDB key by definition; it would test testcontainers,
not the codec):

1. **`TestDifferential_TuplePack`** — table of logical values, each built as both a
   `gotuple.Tuple` and a `cgotuple.Tuple`, asserting `go.Pack()` byte-equals `cgo.Pack()`.
   Explicit rows (not prose):
   - **Integers, every `sizeLimits[n]` class, n=0..8:** `0`; `±1`; `±(2^(8n)-1)` and the
     adjacent `±2^(8n)` for each n (size-class transitions); `MinInt64`, `MaxInt64`;
     `MaxUint64` (as `uint64` — the `decodeInt` high-bit→only-uint64 path). Negative values at
     every class (not just positive).
   - **`*big.Int`:** >8-byte positive (`posIntEnd`) and negative (`negIntStart`); a
     **large-magnitude** (125-byte) pos/neg pair exercising a length-prefix byte well beyond
     `0x0d` and the `length ^ 0xff` negative-length encoding; an exactly-8-byte negative that
     routes through `decodeBigInt` (the `length=8` fallthrough); `MinInt64` as a `*big.Int`
     (the `minInt64BigInt` round-trip); a negative magnitude whose transformed bytes lead with
     `0x00` (exercising the zero-fill loop).
   - **`float32` and `float64`, each:** `+0`, `-0` (distinct sign bit), `+Inf`, `-Inf`, a
     **fixed-bit-pattern NaN** built via `math.Float32frombits(0x7fc00001)` /
     `Float64frombits(...)` (NOT `math.NaN()`, which is non-canonical) **asserted on Pack
     bytes**, smallest subnormal, `-1.0` (the all-bytes-flip branch) and `+1.0` (sign-bit-only
     branch), `MaxFloat`.
   - **bytes / string:** empty; embedded `0x00` (the `0x00`→`0x00 0xFF` escape); trailing
     `0x00`; embedded `0xFF`; the literal `0x00 0xFF` sequence; a `fdb.KeyConvertible` element
     (cgo lacks the identical interface — supply the equivalent `[]byte` to the cgo side).
   - **nested:** empty nested tuple; nested tuple containing `nil` (the extra-`0xFF`); top-level
     `nil` vs nested `nil`; deep (3-level) nesting; mixed-type nested.
   - **bool** `true`/`false`; **UUID**; **complete Versionstamp** (90-bit transaction version +
     user version).
2. **`TestDifferential_TupleUnpackCross`** — `gotuple.Unpack(cgo.Pack(x))` and
   `cgotuple.Unpack(go.Pack(x))` both recover `x` (normalized to `[]byte`/`int64`/`uint64`),
   for the same battery. (go-vs-cgo only; no golden backstop — stated above.)
3. **`TestDifferential_TuplePackHelpers`** (the core of the RFC) — the go-only helpers
   byte-equal the equivalent canonical `cgotuple.Tuple{…}.Pack()` with the prefix prepended:
   - `PackWithPrefix` / `Pack1WithPrefix` / `Pack1ConcatWithPrefix` / `PackConcatWithPrefix`
     over the battery, with empty / short / long / contains-`0x00` prefixes.
   - `Packer.AppendInto` into a **non-empty buffer whose `cap` forces a realloc** (the
     grow/aliasing path) AND into one with spare cap (the in-place path).
   - **`packerPool` reuse**: pack a tuple, return the packer, then pack a *plain* tuple — assert
     the second result has no residue from the first (Reset correctness, esp. `versionstampPos`).
   - A prefix helper invoked **after** a versionstamp pack on a pooled packer (stale
     `versionstampPos` must not leak).
4. **`TestDifferential_TuplePackVersionstamp`** — `gotuple.PackWithVersionstamp` vs
   `cgotuple.PackWithVersionstamp`: byte-equal output **including the 4-byte little-endian
   offset suffix** for incomplete-versionstamp tuples at several positions and user versions,
   plus the with-prefix form. The **API<520 2-byte-suffix branch is unreachable differentially**
   (both clients are pinned at API 730), so it is explicitly out of scope here; it remains
   covered by the go-internal versionstamp tests. The one-incomplete-versionstamp **panic** is
   asserted on the go side (cgo returns an error for >1; the divergence is go-panic vs
   cgo-error and is documented, not differentially asserted).
5. **`FuzzDifferential_TuplePack`** — fuzz a random element stream, build the same logical
   tuple with both codecs, assert `Pack()` byte-equality; target 0 mismatches over ≥60s.

## Performance

Test-only. No production-code change unless a divergence is found.

## Test plan

The tests above ARE the plan. Teeth verified by fault injection per structural branch (flip a
`sizeLimits` entry; drop the nested-`nil` `0xFF`; mis-place the versionstamp offset; break a
helper's prefix splice / pool Reset) and confirming the corresponding named case fails, then
reverting. Run under `bazelisk test //pkg/fdbgo/bench:bench_test` and the fuzz target for ≥60s
with 0 mismatches.
