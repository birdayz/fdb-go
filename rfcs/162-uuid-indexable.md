# RFC-162: Make UUID columns indexable (Java parity) — design + verified wire format (item 1021)

**Status:** Design — read side REVISED per Graefe r1 NAK (typed-coercion, no masks); re-requesting ACK.
**Gate:** Graefe (query-engine + wire) + Torvalds + codex + @claude

## Why this is an RFC, not a PR

`CREATE INDEX … ON t (uuid_col)` fails today with a leaky `XX000` ("field is a message type; use
Nest()") because Go stores a UUID column as the `tuple_fields.UUID` proto MESSAGE and the key-expression
validator rejects message-typed fields. Java treats UUID as a first-class indexable primitive
(`DataType.Primitives.UUID`), so this is a real Go divergence.

I prototyped the fix end-to-end and **proved the approach works** (the index probe returns the right
row) but then reverted to seek a design review — and Graefe's r1 review **vindicated that**: it found a
latent bug in the prototype's positional-mask read side (it silently mis-packs an index-nested-loop join
key whose comparand is already a `tuple.UUID`). The change is **2 clean write/validate sites + a
read-side redesign**, across 3 packages including a sensitive Cascades `values` file, and it touches the
**wire format** (index-key bytes shared with Java). Per the query-engine rule, a multi-site planner+wire
change needs a **Graefe design ACK first**. A partial landing is *unsafe*: enabling the index without the
read side makes `SELECT v WHERE v = '…'` or a covering `SELECT v` silently return wrong/no rows — worse
than the current clean failure. Hence this RFC, not a PR.

## Root cause: UUID's binary-vs-string split

- **Storage / index:** a UUID is the `tuple_fields.UUID` message (`sfixed64 most_significant_bits=1`,
  `least_significant_bits=2`) and must encode into a tuple as a **`tuple.UUID` (0x30 + 16 bytes)** —
  byte-identical to Java.
- **SQL value layer:** carries a UUID as the **canonical 36-char string** (`cast.go` STRING_TO_UUID;
  `query_result.go uuidMessageToString` on read). PromoteValue (RFC-083) that would carry it as a typed
  UUID is **not implemented**, so a `uuid_col = '…'` comparand stays STRING-typed.

Every boundary between these two representations needs an explicit conversion. The verified wire fact
that ties them together: Java `TupleUtil.encode(UUID)` = `0x30 + msb(8B BE) + lsb(8B BE)`
(`TupleUtil.java:402`, `UUID_CODE=0x30`), Go `tuple.encodeUUID` = `0x30 + u[16]` verbatim, and
`tuple.UUID = msb‖lsb big-endian` matches `uuidMessageToString`'s own `binary.BigEndian` packing
(`query_result.go:122-130`) — so a Go-written entry round-trips and is Java-compatible.

## The design — 2 write/validate sites (land now) + a redesigned read side

1. **Validation** — `key_expression_validate.go:isTupleField` is a `return false` STUB. Implement:
   ```go
   const uuidProtoFullName = "com.apple.foundationdb.record.UUID"
   func isTupleField(fd protoreflect.FieldDescriptor) bool {
       return fd.Kind() == protoreflect.MessageKind && fd.Message().FullName() == uuidProtoFullName
   }
   ```
   (Java's `TupleFieldsHelper.isTupleField` accepts UUID + 7 `Nullable*`; Go's DDL only emits UUID — the
   Nullable* cases index as native proto primitives — so UUID is the only one needed.)

2. **Index-entry write** — `key_expression.go:scalarToInterface` `default`-rejects MessageKind. Add:
   ```go
   case protoreflect.MessageKind:
       if msg := value.Message(); msg.Descriptor().FullName() == uuidProtoFullName {
           return uuidMessageToTuple(msg), nil   // tuple.UUID, msb‖lsb BE
       }
       return nil, &KeyExpressionError{…unsupported…}
   ```
   with `uuidMessageToTuple` reading fields by number (1=msb, 2=lsb), `binary.BigEndian.PutUint64`.
   Both eval paths funnel messages here (interpreted `EvaluateScalar`; compiled evaluator's `default` →
   `scalarToInterface`); `PackDirect` returns false for messages → falls back. ✅ verified: CREATE INDEX
   + INSERT succeed, entry written.

Sites 1–2 are type-driven off the descriptor and clean — **land them as written**.

The read side is **redesigned per Graefe's review** (the original draft proposed out-of-band positional
masks; he NAK'd that — see below). The prototype confirmed the *symptom* (string `0x02` probe never
matches the `0x30` UUID entry; covering scan leaks a raw `tuple.UUID`) and the *wire format*, but the
mask approach is wrong:

> **Why the positional mask is rejected (Graefe).** Carrying UUID-ness out of band (`keyIsUUID []bool`,
> rederived per call site) means every future consumer must remember the mask or silently break —
> exactly the "partial landing = silent wrong rows" hazard, generalized; it violates principle #10
> (emergent behaviour over special-case checks). Concretely it has a latent bug: at an **index-nested-loop
> join key** the outer comparand is ALREADY a `tuple.UUID` (sourced from an index entry), so a
> `val.(string)` assert fails and the raw value is appended → silent wrong rows. A mask can't see that.

3. **Read-side typing (replaces probe-mask sites 3 + 4 + IN-lists + INL keys).** Extract the SINGLE UUID
   arm of `PromoteValue.Coercion` (string → UUID) — scoped, NOT gated on all of RFC-083 — and have the
   planner type the comparand as UUID where it compares against a UUID column. Typing the comparand
   **once at the predicate** makes the equality probe, range scan, `IN`-list, AND the INL join key all
   pack a `tuple.UUID` regardless of dataflow source — no per-site mask, the type travels with the value.
   This is Java's model (the comparand is a `java.util.UUID`, not a string).

4. **Covering projection + materialization (collapses the old sites 5 + 6 into ONE boundary).**
   `IndexEntryObjectValue.Evaluate` stays a **pure ordinal extractor** (1:1 with Java's leaf, which never
   renders type) — do NOT convert there (it would lie to any consumer that wants the tuple). Instead
   convert `tuple.UUID → canonical string` at the **result-materialization boundary** (or via a
   planner-inserted `PromoteValue` when `ResultType == UUID`), the same boundary the record-read path
   already crosses (`query_result.go uuidMessageToString`). One conversion point covers covering scans,
   `ORDER BY`, and any other read path.

5. **Edge cases Graefe flagged.** (a) **MIN/MAX-ever-over-UUID:** confirm it orders correctly in tuple
   space or is rejected exactly as Java does — don't leave it undefined. (b) **Verify DDL only ever emits
   the UUID wrapper, never the `Nullable*` messages** — otherwise `isTupleField` (UUID-only) would reject
   a nullable scalar that should index. If DDL can emit `Nullable*`, widen `isTupleField` to Java's full
   set with the matching `scalarToInterface` arms.

## Design decision (Graefe, resolved)

The open question (targeted conversions vs. PromoteValue) is **answered: the scoped typed path.** Not all
of RFC-083 — just the one UUID coercion arm — but type the comparand at the predicate rather than
threading masks. This makes the read-side correctness *emergent* from the value's type (principle #10),
not a checklist every call site must honor, and it eliminates the INL-join-key bug by construction.

## Test plan

- `indexable_types_probe_test.go`: flip the UUID sentinel to `uuid_indexable_and_roundtrips` — CREATE
  INDEX succeeds; INSERT two UUIDs; `SELECT id, v WHERE v = <uuid>` returns exactly the matching row
  (round-trips the index encoding); the second UUID resolves to its own row (no collision).
- **Covering** `SELECT v WHERE v = <uuid>` returns the canonical string (not a raw `tuple.UUID`) — pins
  the materialization-boundary conversion.
- UUID-**PRIMARY-KEY** probe (the PK-scan path).
- **INL join on a UUID key** (the case Graefe flagged): a join `a.v = b.v` where the outer side is
  index-sourced (comparand already `tuple.UUID`) must return the right rows — the regression that the
  rejected positional-mask design would have silently broken.
- **MIN/MAX-ever over a UUID column**: either correct tuple-space ordering or the same rejection Java gives.
- Cross-engine corpus entry: a Go-written UUID index entry is byte-identical to Java's (wire).
- Full `just test`.
