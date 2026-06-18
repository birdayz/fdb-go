# RFC-117: `Optional<primitive-scalar>` wire codegen (extractor) (`pkg/fdbgo`)

**Status:** Implemented (commit `b5bdbc00` on `rfc/117-optional-primitive-scalar`, through the full
`just test` gate). Regen flipped **only** `ReadOptions.ConsistencyCheckStartVersion` `[]byte`→`int64`
(IPAddress variant byte-identical after the shared-emitter consolidation); proven by pure round-trips
+ `cmd/fdb-diff-oracle` `TestDiffReadOptions` (C++ byte-truth, 5 cases, revert-proven). Implementation
re-review (FDB-C-dev + Torvalds + codex + @claude) gating before merge. RFC review: **FDB C++ maintainer ACK** (serialization byte-identical to
`Optional<UID>`/the #303 variant via `SaveAlternative` non-indirection arm `flat_buffers.h:838-849`;
predicate `is_arithmetic||is_enum||UID` proven safe — it's the exact complement of `use_indirection`
`:402`, and every dynamic `Optional<…>` in the schema has a byte-vector inner; added the `MutationRef`
non-flip note below). **Torvalds ACK** (predicate principled not a hack; per-width helpers fine;
executable spec real; asked to share the bare-OOL-scalar emitter with the variant arm — folded into §3).
Re-review the implementation at HEAD (gating).

**Closes:** The `Optional<primitive-scalar>` codegen follow-on filed in the #304 watchvalue note
(`TODO.md`): the extractor now emits `Optional<UID>` correctly as `[16]byte`, but
`Optional<int64>/<Version>/<bool>` are STILL mis-emitted as `[]byte` (e.g.
`ReadOptions.consistencyCheckStartVersion`, slot 5). A real Go↔C++ wire divergence on a field a
co-located Java/C++ client can set.

**Spec:** C++ `libfdb_c` 7.3.75 (`/home/birdy/projects/foundationdb`). Cites are
`flow/include/flow/flat_buffers.h` and `fdbclient/include/fdbclient/FDBTypes.h` at that tag.

---

## Problem (verified `file:line`)

`ReadOptions` (`FDBTypes.h:1741`) serializes
`serializer(ar, type, cacheResult, debugID, consistencyCheckStartVersion, lockAware)` (`:1759`), where
`consistencyCheckStartVersion` is **`Optional<Version>`** (`:1747`; `Version` = `int64_t`). A
flatbuffers `Optional<T>` is a `union_like` whose single non-empty alternative is `T`; for a scalar
`T` the alternative is serialized **out-of-line, bare** (no length prefix), with a `RelativeOffset` in
the slot — `SaveAlternative` (`flat_buffers.h:838-849`):

```cpp
auto result = save_helper(get<Alternative>(member), ...);     // result = the scalar value
if constexpr (use_indirection<...>) { return result; }        // false for a scalar
writer.write(&result, writer.current_buffer_size + sizeof(result), sizeof(result)); // bare value OOL
return RelativeOffset{ writer.current_buffer_size };           // reloff in the slot
```

Read side `LoadAlternative` (`:867-879`): follow the slot's `uint32` reloff, then `load_helper` the
scalar. This is the **same** shape as `Optional<UID>` (UID is also a non-indirection scalar) and the
`IPAddress` IPv4 `uint32` **variant** alternative fixed in #303 (`SaveAlternative` :848 / `WriteUint32`
at `cbs+4`).

**The Go extractor handles only the UID flavor.** `extract.h` `optionalInnerIsScalar`:

```cpp
template <class T> bool optionalInnerIsScalar() {
    if constexpr (optional_inner_t<T>::is) {
        using Inner = typename optional_inner_t<T>::type;
        return std::is_same_v<Inner, UID>;          // <-- UID ONLY
    }
    return false;
}
```

So `Optional<Version>` returns `false` → falls through to the dynamic-size default → generated as
`ConsistencyCheckStartVersion []byte` (`readoptions_generated.go:29`). A Go client that **sets** it
would write a length-prefixed byte vector where C++ writes a bare 8-byte LE scalar behind the reloff —
a wire divergence; and a Go client **reading** a C++/Java-written value gets the raw 8 bytes as
`[]byte` instead of the decoded `int64`. The diff-oracle currently **skips** this field, so the
divergence is unprobed.

### Why the UID path can't just be widened

The UID codegen slices the fixed array (`m.X[:]`, `copy(m.X[:], …)`, `readoptions_generated.go:41`,
`main.cpp:279/470`). A primitive (`int64`/`bool`) is **not** an array — it needs per-type
`binary.LittleEndian` encode on write and decode on read. So the `optScalar` codegen must branch
**array (UID) vs primitive (value)**.

---

## Proposed change

### 1. Extractor predicate — accept primitive scalars (type-safe)

Extend `optionalInnerIsScalar` (`extract.h:291`) to also accept arithmetic / enum inners:

```cpp
return std::is_same_v<Inner, UID>
    || std::is_arithmetic_v<Inner>          // int64/Version, bool, int32, double, …
    || std::is_enum_v<Inner>;               // serialized as the underlying integer
```

This is **safe by construction**: every genuinely-dynamic `Optional<…>` in the schema has a
byte-vector inner (`StringRef`/`KeyRef`/`Value`/`Standalone<…>`) — *not* arithmetic, *not* enum, *not*
UID — so it keeps falling through to `[]byte`. A scalar arithmetic/enum inner is **never** a vector
(`use_indirection` is false), so the bare-OOL path is always correct for it. (`Optional<Error>` /
`Optional<ReadOptions>` are struct inners handled by the separate `optionalInnerGoType` whitelist —
untouched.)

### 2. Carry array-vs-primitive in the field descriptor

`optionalInnerScalarInfo<T>()` already returns the right `ScalarInfo` per inner
(`scalarInfoFor<int64_t>` → `{"int64","ReadInt64","WriteInt64"}`, `<UID>` → `{"[16]byte","ReadUID",
"WriteUID"}`). Add a bool `fd.optScalarIsArray` set true iff the inner is `UID` (the only fixed-array
scalar). The codegen branches on it; `fd.scalar.goType`/`fd.size` already differ per type.

### 3. Codegen — branch the four `optScalar` arms (`main.cpp`)

| arm | array (UID) — unchanged | primitive (int64/…) — new |
|-----|--------------------------|----------------------------|
| struct field (`:155`) | `X [16]byte` | `X int64` (already correct via `scalar.goType`) |
| reader (`:279`) | `copy(m.X[:], r.ReadRelOffRaw(slot+1, 16))` | `m.X = int64(r.ReadRelOffUint64(slot+1))` |
| precomputeSize (`:350`) | `ps.Write(cbs + 16)` | `ps.Write(cbs + 8)` (size-driven, already general) |
| writeToBuffer (`:470`) | `wb.Write(m.X[:], cbs+16)` | `wb.WriteUint64(uint64(m.X), cbs+8)` |

Reader/writer chosen by `fd.size` (the bare-OOL width): 8→`uint64`, 4→`uint32` (reuse the existing
variant helper), 1→`uint8`/`bool`. The struct field + precomputeSize arms are already type-general.

**Consolidate with the variant-scalar arm (Torvalds).** The primitive-scalar read/write/precompute
emitted here is **byte-identical** to what the `FieldKind::Variant` scalar arm already emits
(`main.cpp:299-301` reader, `:367-369` precompute, `:487-494` writer) — both are the same bare-OOL
non-indirection scalar (`SaveAlternative` :848), differing only by the present-gate wrapper. Factor a
single `emitBareScalarRead/Write(size, scalarInfo)` pair (or, minimally, a width→helper dispatch
function) used by **both** the variant-scalar case and the new optScalar-primitive case, so a future
width fix lands in one place. The **array (UID)** case stays separate — it is a genuinely different
`copy(slice)` / `Write(slice)`, not a value encode.

### 4. Two new Go wire helpers (mirror the existing `uint32` pair)

`Optional<Version>` is 8 bytes; today only `ReadRelOffUint32` (`reader.go:791`) + positional
`WriteUint32` (`serializer.go:174`) exist. Add the exact mirrors:

- `func (r *Reader) ReadRelOffUint64(vtableSlot int) uint64` — follow the slot reloff, bounds-checked
  (returns 0 on a short buffer, exactly like `ReadRelOffUint32`), decode 8 LE bytes.
- `func (wb *WriteToBuffer) WriteUint64(val uint64, offset int)` — mirror `WriteUint32`.

(Bounds-checked decode is why a primitive uses `ReadRelOffUint64`, not `binary.LittleEndian.Uint64`
of a possibly-nil `ReadRelOffRaw` — which would panic on a truncated/absent field; the UID `copy`
is nil-safe, primitives are not.) Only widths the regen actually flips get a helper; `bool`/`int32`
reuse `ReadRelOffRaw(…,1)[0]!=0` / the existing `uint32` pair if and only if the schema needs them.

### 5. Regenerate + enumerate the flipped fields

`just generate-wire-types && just gazelle` (Docker, cached FDB build). Then **diff the generated
files** to list exactly which `[]byte` fields flipped to a scalar — confirm each is a true
`Optional<arithmetic/enum>` (not a regression on a byte-vector), and that nothing *outside*
`Optional<>` changed. **Expected primary flip: `ReadOptions.ConsistencyCheckStartVersion []byte` →
`int64`, and likely the ONLY flip.**

> **`MutationRef` is NOT a flip (FDB-C-dev).** `MutationRef` declares `Optional<uint32_t> checksum`
> and `Optional<uint16_t> accumulativeChecksumIndex` (`CommitTransaction.h:105-106`) — both arithmetic
> — but they are **not** flatbuffers members: `MutationRef::serialize` (`CommitTransaction.h:316-350`)
> passes only `type/param1/param2` to `serializer(...)` and manually *suffixes* the checksum bytes onto
> `param2` (`:325,:341`). The extractor's `FieldCollector` walks `serialize()`, so it never sees those
> two fields → they do not flip. The §5 diff-enumeration must not be surprised by their absence, and
> any *other* unexpected flip is caught by the "confirm each is a true `Optional<arithmetic/enum>`"
> gate above.

---

## Wire-compat impact

**YES — changes serialized bytes for a Go client that SETS the field, compatibly toward C++.** Today
Go writes a length-prefixed byte vector for `consistencyCheckStartVersion`; after, it writes the bare
8-byte LE scalar behind the reloff that C++ writes and reads. The field is **client-set and currently
unused by the Go client's own call paths** (it's a consistency-checker knob), so no Go caller's
behavior changes; the fix makes the *encoding* match C++ so a co-located Java/C++ client round-trips
it. No other field's bytes change. Full wire review + the oracle differential below gate it.

---

## Executable spec (proof)

The byte-level oracle is `cmd/fdb-diff-oracle` (C++ truth via the FDB serializer). The field is
**skipped** there today; this change **un-skips it** (no fake checkbox):

- **Oracle round-trip** for `ReadOptions.consistencyCheckStartVersion`: set it (and the existing
  `debugID`) in the C++ handler (`cpp/main.cpp`) + the Go `TestDiff`/`Fuzz` fixture; assert the Go
  `MarshalFDB` bytes are **byte-identical** to the C++ serializer, and the Go `UnmarshalFDB` decodes
  the C++-written value back to the same `int64`. Cover present + absent (the `Has…` tag) and the
  zero/`MAX` boundaries. Do the same for every other field the regen flips.
- **Pure round-trip unit test** (`pkg/fdbgo/wire/types`): `ReadOptions{HasConsistencyCheckStartVersion:
  true, ConsistencyCheckStartVersion: <v>}` marshalled-then-parsed returns `<v>`; absent stays absent.
  Mixed with `DebugID` set, to prove the two-Optional layout (slots 3 and 5) is intact.
- **`ReadRelOffUint64` / `WriteUint64` unit tests**: positional write at `cbs+8` then reloff-read
  returns the value; bounds underflow returns 0 (mirror the `uint32` helper tests).
- **Revert-prove**: revert the `optionalInnerIsScalar` extension (back to UID-only) + regen → the
  field returns to `[]byte` → the oracle round-trip + the typed unit test go red.

The per-PR deterministic oracle gate (`-run='TestDiff|^Fuzz'`) replays this; the IPAddress variant
fix in #303 is the precedent that this gate catches a real `Optional/Variant` scalar wire bug.

---

## What this is NOT

- Not a change to `Optional<bytes>`/`<KeyRef>`/`<StringRef>` fields — those are genuinely dynamic and
  stay `[]byte` (the predicate's arithmetic/enum gate excludes them by construction).
- Not a change to `Optional<struct>` (`Optional<Error>`/`<ReadOptions>`) — handled by the separate
  `optionalInnerGoType` whitelist, untouched.
- Not hand-editing generated code — the fix is in the **extractor** (`extract.h` + `main.cpp`),
  regenerated per the wire-types workflow.
