# RFC-225 — The nested descent reads by ordinal

Takes RFC-197's `boundary` bucket from 2 entries to **0** — but not for the
reason the work started with, and not on one arm. See section 9.2 for the final
position; sections 3.5, 7.9 and 9 record how it changed and why.

## 0. Summary

`FieldValue.descendResolvedPath`'s nested-record step reads a stored proto message
by **field NAME**. Java reads the identical step by **ordinal**, having consumed
the name exactly once at construction. Go's name read has already grown two
compensations — a case-insensitive descriptor scan and an escaped-spelling retry —
each closing a *silent NULL* class, and neither able to close the class of the
other. That is the signature of a read that is asking the wrong question.

This RFC converts the read to Java's form: **the nested path is resolved to
declaration indices once, at construction, and the evaluator indexes
`Descriptor().Fields().Get(ordinal)` with a bounds check that is LOUD.**

The conversion has been blocked, correctly, by a real precondition: on the
producers that reach this arm the ordinal is not the descriptor's declaration
index. This RFC's substance is the audit that establishes what each producer's
ordinal actually is (section 3), and a construction-time resolution that does
not depend on any flowed row type (section 4).

Three refutations are stated up front: two of the inherited framing (section 2)
and one of this RFC's own first draft, which both reviewers NAK'd (section 3.5).

## 1. Java is the spec — verified against `fdb-record-layer/` @ 4.12.11.0

All quotes verified in this session against the checkout, not taken from the
issue text. `CASC = fdb-record-layer/fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/query/plan/cascades`.

**The read is ordinal-only.** `CASC/values/FieldValue.java:163-175`:

```java
164:    public <M extends Message> Object eval(@Nullable final FDBRecordStoreBase<M> store, @Nonnull final EvaluationContext context) {
165:        final var childResult = childValue.eval(store, context);
166:        if (!(childResult instanceof Message)) {
167:            return null;
168:        }
169:        final var fieldValue = MessageHelpers.getFieldValueForFieldOrdinals((Message)childResult, fieldPath.getFieldOrdinals());
```

No name is in play. `getFieldOrdinals()` is an `ImmutableIntArray`
(`FieldValue.java:441-443`).

**Out of range THROWS.** `CASC/values/MessageHelpers.java:169-175`:

```java
170:    public static Descriptors.FieldDescriptor findFieldDescriptorOnMessageByOrdinal(@Nonnull MessageOrBuilder message, int fieldOrdinal) {
171:        if (fieldOrdinal < 0 || fieldOrdinal >= message.getDescriptorForType().getFields().size()) {
172:            throw new Query.InvalidExpressionException("Missing field (#ord=" + fieldOrdinal + ")");
173:        }
174:        return message.getDescriptorForType().getFields().get(fieldOrdinal);
175:    }
```

Both the intermediate hop (`:226-230`) and the leaf hop (`:145-149`) route
through it. Note the negative check is explicit — Java bounds-checks `< 0`
*specifically*, which is exactly the `-1` sentinel case Go mints today.

> **Correction to the inherited description:** the thrown type is
> `Query.InvalidExpressionException`, which is an `IllegalStateException`
> subclass carrying **only a message** (`record/query/expressions/Query.java:273-277`).
> It is *not* a `RecordCoreException` and has **no error code**. Anything that
> planned to port an error code here was porting something that does not exist.

**The name is consumed once, at construction.** `CASC/values/FieldValue.java:271-300`,
`resolveFieldPath`:

```java
284:            if (fieldName != null) {
285:                SemanticException.check(fieldNameFieldMap.containsKey(fieldName), SemanticException.ErrorCode.RECORD_DOES_NOT_CONTAIN_FIELD);
286:                field = fieldNameFieldMap.get(fieldName);
287:                final var fieldOrdinalsMap = Objects.requireNonNull(recordType.getFieldNameToOrdinalMap());
288:                SemanticException.check(fieldOrdinalsMap.containsKey(fieldName), SemanticException.ErrorCode.RECORD_DOES_NOT_CONTAIN_FIELD);
289:                ordinal = fieldOrdinalsMap.get(fieldName);
290:            } else {
...
296:            currentType = field.getFieldType();
297:            accessorPathBuilder.add(ResolvedAccessor.of(field, ordinal));
```

Two properties matter:

- `currentType = field.getFieldType()` (`:296`) — resolution **walks the nested
  type step by step**. Java can resolve step *k* only because it holds the
  stated record type at depth *k*. Go's producers do not hold that type;
  section 4 resolves against the DESCRIPTOR instead, which is the thing Java's
  `Type.Record` is a mirror of anyway.
- A name that is absent is a **hard error** at construction
  (`RECORD_DOES_NOT_CONTAIN_FIELD`, `SemanticException.java:44`), never a NULL.
  Go's current silent-NULL-on-miss for an unpinned path has no counterpart.

**The name-taking overload exists and `eval` is not a caller.** Scoped count —
command run at the Java repo root:

```
grep -rn --include='*.java' 'getFieldOnMessage(' . | grep -v '/src/test/'
```

9 lines. Of these, exactly **3 are call sites of the `String` overload** in main
sources: `MessageHelpers.java:89` (the legacy `getFieldValueForFieldNames` API),
`NullableArrayTypeUtils.java:110`, and `expressions/BaseField.java:58` (the
pre-Cascades `QueryComponent` path). The remainder are declarations
(`:118`, `:124`), `FieldDescriptor`-overload binds (`:120`, `:370`) and javadoc (`:69`).

> **One nuance found that the inherited description did not have, recorded
> because it slightly qualifies "eval never touches a name":** `eval:174` calls
> `NullableArrayTypeUtils.unwrapIfArray`, which at `NullableArrayTypeUtils.java:109-110`
> does a by-name lookup — but of the fixed structural wrapper name `"values"`
> (`:39 REPEATED_FIELD_NAME`), never a user field name. Go's equivalent is
> `values.IsWrappedArrayDescriptor` / `EffectiveListField`, already
> structural. This does not weaken the thesis; it is stated so nobody
> "discovers" it later and reads it as a refutation.

**What the ordinal indexes — the critical one.** It is the position in the
**`Type.Record` field list**, not the proto field number. `CASC/typing/Type.java:2306-2311`:

```java
2306:        private Map<String, Integer> computeFieldNameToOrdinal() {
2307:            return IntStream
2308:                    .range(0, Objects.requireNonNull(fields).size())
```

and `MessageHelpers.java:174` indexes `getFields().get(fieldOrdinal)`, which is
protobuf **declaration order**. So Java's operative invariant is:

> **record-type field-list position `i` == proto descriptor declaration position `i`.**

What enforces it in Java: **nothing but symmetric construction.** Forward,
`Type.java:2339-2353 defineProtoType` emits `for (final var field : fields)`
appending in list order. Reverse, `Type.java:2586-2588 fromDescriptor` ingests
`descriptor.getFields()` through an insertion-ordered Guava `ImmutableMap`.
There is no assertion, no sort, no cross-check. `findFieldDescriptorOnMessageByOrdinal`
bounds-checks the index and nothing else.

That is worth stating plainly because it sets the bar, and because it is what
section 4's revised design turns to Go's advantage: **resolving directly against
the descriptor leaves no second list that could drift**, so Go does not need the
invariant Java leaves unchecked — it has no way to violate it.

**ResolvedAccessor keeps the name, for display only.** `FieldValue.java:684`
`equals` is `getOrdinal() == that.getOrdinal()`; `hashCode` `:689` likewise. The
name feeds `toString`/`explain` (`:694-696`, `:427-433`, `:229-236`) and plan
serialization (`:703`). Go's `ResolvedAccessor` already documents and implements
exactly this. **No change to identity semantics is proposed.**

## 2. Two corrections to the inherited framing

Leading with these because both change the work.

### 2.1 There is a THIRD `-1` producer, not two

The debt entry, the code comment at `values.go:958-966`, and the pin test all
name `unnest_seed.go` and `unnest_gather.go` as the `-1` minters. There is a
third: `index_expansion.go:562-563` mints `Ordinal: -1` as the accessor's
**default**, and keeps it in three distinct cases (`index_expansion.go:566-580`):

- `collectionPath` is true — `if !collectionPath { accessor.Ordinal = ordinal }` (`:568-570`), so a collection path deliberately declines the ordinal it just derived;
- the walked type is not a `*values.RecordType`;
- the name misses in the record type.

Command:

```
grep -rn "ResolvedAccessor{" --include="*.go" pkg/ cmd/ | grep -v "_test.go"
```

A plan that fixes two producers and converts the read would have shipped a
wrong-column read through the third. The audit is the deliverable here, not the
decoration on it.

### 2.2 `fuseNestedAccessors`' ordinal is descriptor-true **by construction**, not "by convention"

The debt entry says `expr.fuseNestedAccessors` "copies a SQL-struct-type position
that matches the emitted descriptor only by convention." Traced to its origin,
the chain is tighter than that:

`expr.go:260` copies `semantic.NestedAccessor.Ordinal`. That field has **exactly
one** production mint —

```
grep -rn "Ordinal:" --include="*.go" pkg/relational/core/query/semantic/ | grep -v _test.go
→ pkg/relational/core/query/semantic/scope.go:355
```

— which takes `ord` from `Column.LookupStructField` (`catalog.go:125-132`), the
index into `Column.StructFields`. `StructFields` in turn has one production
builder, `rlcatalog.go:363-369`:

```go
nested := msg.Fields()
for i := 0; i < nested.Len(); i++ {
    col.StructFields = append(col.StructFields, columnForField(nested.Get(i), enclosing))
}
```

Unconditional append over `protoreflect.FieldDescriptors` in declaration order —
no skip, no filter, no sort. So it **is** the descriptor declaration index, by the
same symmetric-construction argument Java relies on, and it is the *stronger* of
the two directions because it is a single loop over the descriptor itself.

This matters for the design: the one producer everybody assumed was the shaky
one is in fact the **model** the other three should be made to match.

**This is a correction to the debt entry's TEXT, not merely a finding about the
code, and the implementation makes it.** The `EqualFold` entry in
`knownFieldDecisionDebt` asserts that `fuseNestedAccessors` "copies a SQL-struct-type
position that matches the emitted descriptor only by convention." That sentence
is load-bearing — it is one of the two stated reasons the conversion was gated —
and it is wrong. The entry is rewritten to say the ordinal is descriptor-true by
construction through `rlcatalog.go:363-369`, with the trace, so the next reader
does not re-derive it or re-gate on it. The same applies to the identical claim in
`proto_descent_test.go`'s pin doc (`:145-150`).

## 3. The producer audit

Every producer of a **non-root** `ResolvedAccessor` (only non-root accessors
reach `descendResolvedPath`). Commands:

```
grep -rn "WithSuffix\|FuseNestedSuffix" --include="*.go" pkg/ cmd/ | grep -v _test
grep -rn "FieldPath{Accessors" --include="*.go" pkg/ cmd/ | grep -v _test
grep -rn "ResolvedAccessor{" --include="*.go" pkg/ cmd/ | grep -v _test
```

Discarding comment-only hits and the unrelated `formatVersionSaveUnsplitWithSuffix`
identifier in `pkg/recordlayer/store*.go`, **four sites mint fresh non-root
ordinals, three mint `-1`, and the rest carry existing accessors verbatim**:

| Site | What its non-root Ordinal is today | What it must become |
|---|---|---|
| `expr/expr.go:260` `fuseNestedAccessors` | Descriptor declaration index (§2.2) | **Unchanged.** Already correct; becomes the asserted case. |
| `index_expansion.go:562` | Position in a proto-derived `RecordType`, **or `-1`** on `collectionPath` / non-record / name miss | Derived index, or a **declined expansion** — never a `-1` accessor |
| `values.go:5237` (`assertSuffixStep` inside `FuseNestedSuffix`) | Re-derived against a stated `RecordType`; refuses on absent/ambiguous/disagreeing | ~~**Unchanged.** This is the boundary the design generalises.~~ **RETRACTED — see 15.2(2) and 16.1: this site is DELETED.** Section 14 showed the `RecordType` it re-derives against can be a whole sub-leg row, so generalising it was generalising the defect. |
| `values.go:5169` `NewFusedFieldValueOfNestedOrdinal` | A **leg-row** ordinal, a different layout | Settled in section 4 Step 4 (two callers, both named) |
| `unnest_seed.go:177` | Literal `-1` | Derived index (§4) |
| `unnest_gather.go:189` | Literal `-1` | Derived index (§4) |
| `cascades_translator.go:7430`, `replace.go:412/418`, `rule_select_merge.go:747`, `simplifier_value.go:295`, `max_match_map.go:919/935`, `match_info_merge.go:803` | Carried verbatim from an already-resolved path | Unchanged — correctness follows from the mints |

## 3.5. Revision 2 — what the first review lap refuted

The first design (widen the flowed row's types, then resolve suffix names
against the widened `RecordType`) was NAK'd by both reviewers on grounds that
verified. Recorded here rather than quietly rewritten, because two of the three
are refutations of the *brief this work started from*, not merely of the draft.

**R1 — the flowed-row widening is another RFC's work, and it is gated.** Draft
§4 quoted `FieldTypeForFD`'s doc as an admission that `TargetTypeForFD` is the
real answer. It never quoted `TargetTypeForFD`'s own doc, which is the rebuttal
(`cascades_translator.go:305-315`):

> *"The two are separate on purpose and the separation is temporary in the
> design, not in intent: FieldTypeForFD types the FLOWED scan row, where
> collapsing structs and arrays to UnknownType is load-bearing today (the
> anchored-leg column types, index-on-source, derived unnest all read that
> collapse). RFC-204 §4.4/§4.5 unify them by making the flowed row carry the
> nested types; until the query surface and metadata pipeline can consume that,
> widening FieldTypeForFD would change plan-time typing for every query rather
> than for DML alone."*

Verified: `rfcs/204-struct-types-relational-layer.md` exists. The widening is
**booked, scoped and gated elsewhere**. Also missed by the draft: `TargetTypeForFD`
returns `ArrayType` for repeated fields, so `:252` would re-type every ARRAY
column too, not only struct columns. Draft §5 Step 1 is withdrawn entirely.

**R2 — name normalization would have made the whole thing a silent no-op.**
Both reviewers found this independently, and it is the CLAUDE.md empty-set false
positive in its purest form. `values.Field` carries only `Name`
(`type.go:310-325`) — no storage-name twin, where Java's `Type.Record.Field`
carries both `fieldName` and `fieldStorageName` (`Type.java:2660-2663`).
`tableColumns` upper-cases (`cascades_translator.go:249`), `TargetElementType`
stamps the raw proto name (`:346`), the producers mint `strings.ToUpper(seg)`,
and `assertSuffixStep` compares `f.Name == want.Field` **exactly**
(`values.go:5341`). Every nested suffix step would have taken the `dupes == 0`
arm and declined the optimization through the producers' existing `nil` returns.
Suite green, feature dead, and the draft's own criterion 6 (test the decline
path) would have passed while proving nothing. Any design here needs a test that
a nested read **succeeds**, not merely that it declines.

**R3 — REFUTES THE PREMISE: the two debt entries do not retire on one change.**
The brief, the debt entry and draft §5 all assert that deleting `protoFieldByName`'s
name-resolution head retires both `boundary` entries at once. It does not,
because `protoFieldByName` has **three** callers and the conversion reaches one:

- `values.go:972` — `descendResolvedPath`'s baked nested step. This RFC's subject.
- `values.go:1190` / `:1194` — `FieldValue.Evaluate`'s **chained** struct descent,
  where a `proto.Message` arrives as the whole evaluation context and the read is
  driven by `f.Field`, not by a path accessor.

The debt entry itself names all three ("protoFieldByName has three callers") and
then states a fix that only addresses one. Delete the name head and the chained
arm breaks; keep it and the `EqualFold` and escaped-spelling comparisons the
ratchet keys on still exist, so the bucket does not reach 0. **The draft's
criteria 1 and 2 were mutually unsatisfiable.**

One correction in the other direction, since a refutation is only worth having
if it is itself checked: the second reviewer's stated reason — that the chained
arm has "no ordinal in existence" — is **wrong**. `PrimitiveAccessorsForType`
builds that chain with `NewFieldValueOfOrdinal(base(), i)`
(`primitive_accessors.go:53-61`), which bakes a single-accessor `Resolved` path
carrying a real ordinal against `rt`. So the chained arm *is* convertible; it
simply reads `f.Field` when `f.Resolved.Root().Ordinal` is sitting right there.
That makes it part of this RFC's scope rather than a blocker — but only because
the fact was checked instead of accepted.

## 4. The design (revision 2)

The draft went looking for a *type* to resolve names against, and the only rich
enough one was owned by RFC-204. That was the wrong search. The thing the read
will ultimately index is the **proto descriptor**, and every producer that mints
a nested accessor is in code that already holds it. So:

> **Resolve the nested path against the DESCRIPTOR, once, at construction.
> Do not widen any flowed row type.**

This is closer to Java than the draft was. Java resolves against `Type.Record`
only because `Type.Record` *is* its descriptor-ordered mirror
(`Type.java:2586-2588`, §1); the semantic content is "resolve against the
declared field order of the thing you will read." Go can consult that order
directly, and gains an assurance Java lacks: there is no second list that could
drift out of order, because there is no second list.

**Step 1 — one construction-time resolver.** A single exported helper in the
`values` package takes a `protoreflect.MessageDescriptor` and a per-step name and
returns a declaration index or an error. Its matching logic is **exactly**
today's `protoFieldByName` head, moved: verbatim, lower-case, `EqualFold` scan,
then the escaped spelling (`protoname.ToProtoBufCompliantName`). Nothing about
name matching is weakened — R2's normalization mismatch is handled by keeping the
folding that already exists, at the one place it now runs. It runs **once per
plan**, not once per row.

This is the migrated shape RFC-197's own bucket header prescribes: *"Fix is to
resolve to ordinals at the boundary, once, rather than at every crossing."* Java
agrees structurally — `resolveFieldPath` (`FieldValue.java:284-289`) is itself a
name lookup, performed once at construction.

The escaped-spelling attempt in particular is not a hedge, it is the **inverse of
a known emission step**: Go's descriptor emitters apply the escape when they
write the field name (`values/proto_type.go:310`, `metadata/builder.go:937`),
which is exactly Java's `getFieldStorageName` (`Type.java:2347`,
`ProtoUtils.toProtoBufCompliantName`). Java can skip the retry at resolution time
only because `Type.Record.Field` carries the storage name alongside the
identifier (`Type.java:2660-2663`) and Go's `values.Field` does not
(`type.go:310-325`). Resolving against the descriptor makes that gap moot — the
storage name is read off the descriptor rather than remembered.

The existing pin `values/proto_field_escaped_name_test.go` (the `a$b` → `a__1b`
case, `:39`) must stay green through the move; it is the regression sentinel for
this exact class and it is re-pointed at the resolver, not rewritten.

**Step 2 — the four minting producers use it.** `unnest_seed.go:177`,
`unnest_gather.go:189` and `index_expansion.go:562` resolve their segments
against the descriptor they are already holding (via `t.resolveRecordType(table).Descriptor`
and the array-element descriptor for the unnest sites — see the array note
below) and mint a true declaration index. A step that will not resolve
**declines the whole optimization** through the `nil` return each site already
has. `expr/expr.go:260` is already descriptor-true (§2.2) and is left alone,
becoming the asserted case.

**Array elements.** Both unnest sites descend an array *element*, and
`FuseNestedSuffix` walks only `*RecordType` (`values.go:5233`), so an array in
the path takes the carry arm today. The descriptor-side resolver has no such
gap: `values.EffectiveListField` already unwraps both the flat-repeated and
wrapped-nullable shapes, and the element's `Message()` descriptor is what the
next step resolves against. This is stated because R2's second half showed that
an unhandled array step is another silent decline.

**Step 3 — convert BOTH reads.** `descendResolvedPath`'s `proto.Message` arm and
`Evaluate`'s chained arm (`values.go:1190`/`:1194`) both become Java's
`findFieldDescriptorOnMessageByOrdinal`: bounds-check `ordinal < 0 || ordinal >=
fields.Len()`, then `fields.Get(ordinal)`. The chained arm reads
`f.Resolved.Root().Ordinal` (§3.5 R3). `protoFieldByName` keeps **only** the
presence check and `ProtoFieldToRowValue`; its name head moves wholesale into
Step 1's resolver and exists nowhere else.

**Step 4 — `NewFusedFieldValueOfNestedOrdinal` (values.go:5169), settled here.**
Both reviewers refused the draft's deferral and both were right; CLAUDE.md is
explicit that "the audit could not establish it" is the work, not a boundary. Its
two callers are `left_outer_existential.go:276` and
`exists_gathered_cluster_wrap.go:172`. The implementation establishes whether
either reaches the proto arm with a leg-row ordinal. **If it does, it joins
Step 2. If the answer is not established, the constructor declines** — because
after Step 3 a leg-row ordinal is *in range* on a struct descriptor and therefore
reads the wrong column silently, which is the one outcome this RFC exists to
prevent.

**Step 5 — re-point the guards.** `TestFieldValue_DescendProtoMessage_MustNotConsultTheOrdinal`
is re-pointed (not deleted) into the positive assertion, per its own stated
retirement condition. `field_name_decision_test.go:680`'s `assertSuffixStep`
entry says the record type "on the production path is not available" — Step 2
does not change that (no widening), so that text stands; it is re-read and
confirmed rather than assumed, since a guard pointing at a stale expectation is
a build break either way.

### Where the debt lands, stated honestly

The `boundary` bucket goes to 0: both entries key on `protoFieldByName`, whose
name head ceases to exist. Step 1's resolver hosts the same `EqualFold` and
escaped-spelling comparisons at a new site, and the ratchet will see them. That
site is **declared, not allowlisted**, and it belongs in the `translator`
bucket — whose definition is name resolution "where a parsed identifier
legitimately arrives as text," which is precisely a SQL segment being resolved
against a descriptor once at plan time. Moving a per-row name decision to a
once-per-plan one is the migration; pretending it vanishes would be the lie.
**RULED (lap 2, both reviewers, independently): this is a genuine migration, not
bucket-shuffling.** A per-row decision becoming once-per-plan is exactly what
RFC-197's bucket header prescribes, and the `translator` bucket's definition fits
a SQL segment resolved against a descriptor at plan time. *"If relocation counted
as shuffling, no entry could ever migrate."* The RFC is **not** amended to
"blocked on RFC-204". The attached conditions are carried into criterion 1.

### Why the alternatives lose

- **Keep the name read, add more spellings.** The status quo's trajectory: two
  compensations already, neither able to cover the other, every miss a silent
  NULL, spelling space open. Rejected.
- **Widen `FieldTypeForFD` → `TargetTypeForFD` (the draft).** Withdrawn on R1 —
  it is RFC-204 §4.4/§4.5's gated work, re-types every array column as well as
  every struct column, and changes plan-time typing for every query. A change
  that big must be argued on its own merits in its own RFC, not as step 1 of
  someone else's.
- **Read by ordinal but verify the name per row.** Rejected: it moves the name
  decision rather than retiring it, costs a descriptor lookup per row on the eval
  hot path, and can only fire where the name is present and correct — protecting
  exactly the cases that were never in danger. Java has no such check
  (`MessageHelpers.java:171` bounds-checks the index and nothing else).
- **Resolve by proto field NUMBER.** Wire-durable, and rejected anyway: Java's
  ordinal *is* the declaration index (`Type.java:2306-2311`) and `ResolvedAccessor`
  equality is ordinal-only, so a number-keyed Go accessor would intern
  differently in the memo from its Java counterpart. No stored bytes change
  either way; the plan-identity divergence is the cost.
- **Fix only the two producers the debt entry names.** Ships a wrong-column read
  through `index_expansion.go` (§2.1). Rejected on the audit.

### Wire format

**Nothing here writes bytes.** It changes which descriptor field a read selects,
so the hard line applies in full — which is why Step 3 comes after Step 2, why
Step 4 declines rather than assumes, and why criterion 3 below pins the
wrong-column direction specifically.

## 5. Acceptance criteria (SUPERSEDED by section 8)

> These were written when the RFC still claimed the bucket reaches 0. Section 3.5
> R3 refuted that and section 8 restates them. Kept, not deleted, because the
> delta between the two lists is the record of what the refutation cost — a
> replaced criteria list with no trace is how a scope change becomes invisible.


Falsifiable, and each names what makes it fail:

1. `boundary` bucket is 0, both `protoFieldByName` entries removed from
   `knownFieldDecisionDebt`, `TestFieldDebtBucketsArePartition` green — **and**
   the resolver's new decision site is declared in `translator` with its count,
   not allowlisted — stating both its COUNT and its once-per-plan property. Not
   conditional: the migration question was ruled in lap 2 (section 4).
   Accompanied by a test pinning that **no per-row name decision remains**.
2. `protoFieldByName` contains no name lookup: `ByName`, `EqualFold` and
   `ToProtoBufCompliantName` do not appear in it. Verified by grep, output pasted.
3. **A nested read SUCCEEDS.** An end-to-end SQL query descending a struct column
   returns the right value and, per CLAUDE.md, an `EXPLAIN`/plan-shape assertion
   proves the baked nested path was taken rather than a decline to a fallback.
   This criterion exists because R2's failure mode passes every other one.
4. A test pins that the descent **consults the ordinal** — the exact inverse of
   the current pin's second assertion, on the same fixture, so both cannot be
   green at once.
5. A test pins that `-1` and out-of-range are **LOUD for pinned and unpinned
   alike**. The unpinned arm is the behaviour change; a test driving only the
   pinned arm passes with the change half-applied.
6. A test drives each converted producer's **decline** path explicitly (census
   rule: an arm the corpus happens not to reach is an untested arm).
7. A test pins the escaped-spelling case (`a$b` → `a__1b`) resolving correctly
   through the new resolver — the exact shape that broke, not a simpler cousin.
8. Step 4's outcome is stated with the measurement that established it, and if
   unestablished, the decline is pinned by a test.
9. Every pin mutation-checked separately per direction — wrong ordinal,
   tolerated `-1`, silent-instead-of-loud miss, and unresolved-name decline —
   with the RED line quoted verbatim.
10. `just test` green; `//pkg/docscheck`, `//pkg/recordlayer/query/plan/cascades/values`
    and the relational targets green under Bazel with `--nocache_test_results`,
    with the executed-test count read from the output rather than assumed.

## 6. Review record

### Lap 1 (revision 1) — NAK / NAK

Both reviewers independently found the name-normalization hole (R2) and both
rejected the `FieldTypeForFD` → `TargetTypeForFD` widening. Revision 2 withdrew
the widening and redesigned around descriptor-side resolution. Both reviewers
verified sections 2 and 3 (the producer audit) as accurate.

### Lap 2 (revision 2) — Cascades reviewer: NAK, with one ruling

**RULED — the debt migration is genuine, not bucket-shuffling.** The open
question at the end of section 4 is answered: moving a per-row name decision to
a once-per-plan resolver **is** the migration RFC-197's bucket header
prescribes, and the `translator` bucket's definition fits a SQL segment resolved
against a descriptor at plan time. *"If relocation counted as shuffling, no
entry could ever migrate."* The RFC is **not** to be amended to "blocked on
RFC-204". Conditions attached: the `translator` entry states its count and its
once-per-plan property, and a test pins that no per-row name decision remains.

**Open blocking conditions carried into revision 3:**

1. **`FuseNestedSuffix`'s one production caller is unaudited.**
   `left_outer_existential.go:51` (via `descendOrFail`) is the sole production
   caller. Its suffix ordinals come from `assertSuffixStep` against a
   `*RecordType`, and its **carry arm** (`values.go:5260-5263`) passes
   `want.Ordinal` through verbatim when the walk is unstated. Those ordinals
   reach the proto arm after Step 3. Section 3 marks this site "Unchanged" and
   calls it "the boundary the design generalises" **without applying section
   2.2's own standard to it** — the RecordType it resolves against was never
   shown to be descriptor-ordered. Audit it or route it through Step 1's
   resolver.

2. **Converting `values.go:1190`/`:1194` pulls an unaudited path into scope —
   the one respect in which revision 2 is worse than revision 1.** Section 3.5
   R3's fact is correct (`primitive_accessors.go:59` bakes a real ordinal via
   `NewFieldValueOfOrdinal`), but that ordinal indexes `rt`, and `rt`'s sole
   production feeder is `gk.Type()` (`grouping_primitive_expansion.go:27`),
   which section 3 never audited. If any such `rt` is a `tableColumns` product
   it is upper-cased and carries `__ROW_VERSION` at index `Len()`
   (`cascades_translator.go:249-260`) — after Step 3 that is an **out-of-range
   read on a live path**. Extend the section 3 audit to `gk.Type()`'s
   provenance and pin the `__ROW_VERSION`/upper-case shape before converting
   this arm.

   Partial progress: `expandGroupingKeysToPrimitives`
   (`grouping_primitive_expansion.go:23-33`) passes each grouping key's own
   `Type()`. A base-table struct column collapses to `UnknownType` under
   `FieldTypeForFD`, and `PrimitiveAccessorsForType`'s non-record arm
   (`primitive_accessors.go:43-50`) then returns the base value unchanged — no
   chained `FieldValue`, so no proto descent. The record arm is therefore
   reached only for keys carrying a *stated* `RecordType`. **Which producers
   those are is not yet established, and the audit is not complete.**

3. **Criterion 8 is a deferral and must be removed.** "Establish it during
   implementation, decline if unestablished" is not acceptable with both callers
   already named (`left_outer_existential.go:276`,
   `exists_gathered_cluster_wrap.go:172` — confirmed by grep). Establish
   whether either reaches the proto arm with a leg-row ordinal. The decline
   stays as the runtime guard; it is not the answer.

**Status: NOT APPROVED. Implementation has not started and must not start until
these are discharged and both reviewers ACK.**

### Lap 2 (revision 2) — code reviewer: NAK

Discharged: (b) the widening withdrawal, including the array-widening catch
(`TargetTypeForFD` returns `ArrayType` for repeated fields, `:253-255`);
(c) case normalization and the `EffectiveListField` array unwrap, with
criterion 3 pinning success rather than decline. Ruled with the other reviewer
that the debt move is migration, not laundering.

Withdrawn by the reviewer: his lap-1 claim that `values.go:1190` has "no ordinal
in existence". `primitive_accessors.go:53-61` bakes one and `values.go:1526-1536`
bounds-checks it. Section 3.5 R3 stands on the fact.

**Additional blocking conditions carried into revision 3:**

4. **Step 2 ARMS A DEAD ARM, and revision 2 never mentions it.**
   `assertSuffixStep` declines when `want.Ordinal >= 0 && want.Ordinal != derived`
   (`values.go:5351`). Every producer-minted `-1` is exempt from that arm today,
   so it has never fired on a production path. Descriptor-derived ordinals are
   **not** exempt. Section 4 must state this and carry a unit pin that drives the
   arm — per CLAUDE.md's census rule, an arm whose first real firing would be
   read as a "finding" rather than as an untested branch.

5. **The `:680` ratchet text is invalidated on a clause section 5 does not
   address.** `field_name_decision_test.go:680` reads *"the suffix ordinals are
   CARRIED, because a suffix indexes a struct's own declared field order which no
   merge restates."* Step 2 makes them **derived**. Step 5 argues from "no
   widening", which reconciles the `into` half of that entry and not the
   carried-ordinal half. Fix the text on the clause Step 2 actually falsifies.

6. **Criterion 3's success assertion is the load-bearing one** and must not be
   satisfiable by a decline — restated here because both laps found the
   silent-decline failure mode independently, in two different mechanisms
   (revision 1's name mismatch, revision 2's newly-live disagreement arm).

### Convergence note

Across two laps the reviewers agreed on every factual finding and disagreed with
each other on none. The design has moved from "widen the flowed row type" to
"resolve against the descriptor", which is both smaller and closer to Java. What
remains blocking is **entirely audit work on three named sites** —
`left_outer_existential.go:51`, `gk.Type()`'s provenance at `values.go:1190`, and
`NewFusedFieldValueOfNestedOrdinal`'s two callers — plus the two guard texts
above. None of it is a missing capability.

## 7. Revision 3 — the audits

Lap 2's blocking conditions were audit tasks. They are discharged here. Two of
them turned out to rest on premises that do not hold, and one guard text asserts
a count the code no longer has.

### 7.1 `left_outer_existential.go` — `into` is NIL on the production path

Commands:

```
grep -n "suffixInto" pkg/recordlayer/query/plan/cascades/left_outer_existential.go
→ 154 (declaration), 193 (assignment), 285, 301 (uses)
```

The assignment is the whole answer (`left_outer_existential.go:193`):

```go
suffixInto, _ = w.Typ.Fields[legOrdinal].FieldType.(*values.RecordType)
```

`w` is an `OrdinalSeedLegWindow`, whose `Typ` is "the leg's own record type"
(`values/ordinal_seed_layout.go:28-33`). A leg column's `FieldType` comes from
the flowed layout, and the flowed layout collapses struct columns to
`UnknownType` (`FieldTypeForFD`, `cascades_translator.go:368-372`) — the measured
fact `FuseNestedSuffix` already records at `values.go:5195-5199`. **So the type
assertion fails and `suffixInto` is `nil`**, which means `FuseNestedSuffix` takes
its CARRY arm and `assertSuffixStep` is never called from this site.

That answers lap 2's condition 1: the site does not need its `RecordType` shown
descriptor-ordered, because it does not have one. What it does instead is carry
the producer's ordinal verbatim — so its correctness reduces entirely to the
producers, which is what section 3 audits.

**But the audit found a consequence in the opposite direction from the one the
review flagged, and it is the more important one.** Today every carried `-1`
hits `values.go:5266-5271` ("states no ordinal and no layout to derive one
from"), which errors, so `descendOrFail` sets `failed = true`
(`left_outer_existential.go:49-52`) and **the whole rebase declines**. After
Step 2 the carried ordinal is descriptor-derived and non-negative, so this path
stops declining and **starts producing plans it has never produced**. That is a
plan-shape change, not merely a correctness fix, and it needs the
before/after stress comparison CLAUDE.md requires of planner changes. It is
added to the acceptance criteria.

### 7.2 The `assertSuffixStep` disagreement arm — the premise does not hold

Lap 2 condition 4 says Step 2 arms a never-fired arm (`values.go:5351`). It does
not, for the reason in 7.1: with `into == nil` the carry arm runs and
`assertSuffixStep` is not reached. Step 2 supplies ORDINALS, not a `RecordType`.

The arm is also **not** an untested instrument. `TestFuseNestedSuffix_Declines`
(`values/fuse_nested_suffix_test.go:101-183`) is a table driving **all six**
decline arms explicitly, including the disagreement arm at `:133-142` ("the
carried ordinal CONTRADICTS the record it descends into") and the negative-ordinal
arm at `:150-155`. The census rule is satisfied today.

MEASURED, not read off the source — `--nocache_test_results`, 7 `=== RUN` lines
(1 parent + 6 subtests), non-empty population confirmed before reading the
verdict:

```
=== RUN   TestFuseNestedSuffix_Declines
=== RUN   TestFuseNestedSuffix_Declines/the_suffix_names_a_field_the_record_does_not_have
=== RUN   TestFuseNestedSuffix_Declines/the_suffix_name_is_DUPLICATED_in_the_record
=== RUN   TestFuseNestedSuffix_Declines/the_carried_ordinal_CONTRADICTS_the_record_it_descends_into
=== RUN   TestFuseNestedSuffix_Declines/an_UNNAMED_suffix_step_out_of_range
=== RUN   TestFuseNestedSuffix_Declines/an_unnamed_step_with_NO_ordinal_and_no_layout_to_derive_one
=== RUN   TestFuseNestedSuffix_Declines/the_suffix_descends_past_a_STATED_leaf_type
--- PASS: TestFuseNestedSuffix_Declines (0.00s)
```

What genuinely goes live is different and narrower: the `step.Ordinal < 0` guard
(`values.go:5266-5271`) stops firing. That is 7.1's finding, and it is pinned by
the existing `:150-155` arm — which must be **re-pointed**, since after Step 2 no
production producer mints `-1` and the arm's stated justification ("Go mints
Ordinal:-1 name-only accessors at four producer sites") ceases to be true.

### 7.3 The `:680` ratchet text — REVERSED by sections 14 and 15; see criterion 10

> **This subsection's ruling no longer holds.** It concluded the entry needed no
> edit, on two grounds that later work falsified: that the assert arm stays dead
> (section 15.2(2) deletes it) and that a record type is unavailable on the
> production path (section 14 found one). Kept as the record of a call that was
> right against the design of the time. The live instruction is criterion 10.


`field_name_decision_test.go:680` (the `assertSuffixStep` contract entry) reads:

> *"this arm only fires when a record type happens to be available, and on the
> production path it is not (the positional merge states UNKNOWN for a struct
> column). Retires with the merged-layout struct-typing work booked in TODO.md,
> at which point the assert arm becomes the live one"*

7.1 confirms this verbatim and Step 2 does not change it — the widening is
withdrawn, so the merged layout still states UNKNOWN and the assert arm stays
dead. **The entry needs no edit.** Lap 2 condition 5 asked to fix its
carried-ordinal clause; the clause is correct as written (the ordinals ARE
carried), and what changes is only their PROVENANCE, which the entry does not
assert. Editing it would have been a change for the worse.

### 7.4 A stale count in a guard text — `values.go:478-482`

```
grep -rn "ResolvedAccessor{" --include="*.go" pkg/ cmd/ | grep -v _test.go
grep -rn "NewFieldPathOfSingle(\|NewFieldPathOfSingleInDomain(" --include="*.go" pkg/ | grep -v _test.go
```

`OrdinalIn`'s doc claims Go mints `-1` name-only accessors "at four producer
sites (the unnest/gather/index-expansion seeds and **the translator's array-path
model**)". There are **three**: `unnest_seed.go:177`, `unnest_gather.go:189`,
`index_expansion.go:564`. No `NewFieldPathOfSingle*` caller passes a negative
ordinal, and the translator's array-path model mints none. The count is stale —
exactly the unscoped-enumeration defect CLAUDE.md names. Corrected as part of
this work, since Step 2 takes the number to zero anyway and a guard left
asserting "four" would be doubly wrong.

## 8. Scope, first restatement: 2 → 1 (SUPERSEDED by 7.9 and 9.2 — final answer is 2 → 0)

> Kept as the record of a scoping call that was correct while 7.6 was open and
> was reversed by measurement, not by argument. The error-type decision below is
> still live; the 2 → 1 split is not. See 10.3 for why the hazard that motivated
> it does not exist.


Section 3.5 R3 established that the two `boundary` entries do not retire
together. The acceptance criteria are corrected to match rather than stretched to
force a zero, because stretching is precisely how a migration acquires a silent
wrong-column read.

**This RFC's scope — step A — closes ONE entry.** The `EqualFold` entry names
`descendResolvedPath`'s baked nested step (`values.go:972`); converting that arm
and routing its producers through the construction-time resolver retires it.

**The `!=` entry — the escaped-spelling attempt — does NOT retire with it.** Both
attempts live in the same helper, so the helper keeps its name head as long as
ANY caller needs it, and `Evaluate`'s chained arm (`values.go:1190`/`:1194`) does.
The bucket therefore goes **2 → 1** on step A, and the surviving entry's text is
rewritten to say exactly which caller keeps it alive — replacing the current text,
which prescribes a fix for a caller it does not name.

**Step B — converting the chained arm — is named as the NEXT piece, not done
here.** The reason is a scoping judgement and it is stated rather than assumed:
step B's producer audit is a *different* audit (the `gk.Type()` provenance
question of section 7.5), its hazard is different (an out-of-range read from
`__ROW_VERSION` at index `Len()`, not a wrong-column read), and its blast radius
is the aggregate/grouping path rather than the nested-reference path. Doing both
in one change would mean one mutation check covering two independent failure
directions, which is the shape CLAUDE.md warns produces fixes that satisfy only
one. Step B gets its own section in this RFC, its own audit, and its own review
lap — in this RFC, not a TODO entry.

### The out-of-range error type — decided

Go **does** error out of range, matching Java's throw
(`MessageHelpers.java:171-172`). The type is the existing
**`*values.OrdinalResolutionError`**, not a new one and never a bare sentinel:

- it is already what this exact arm raises for a pinned path
  (`values.go:975-977`) and what the `OrdinalRow` arm raises (`:924-928`), so the
  conversion narrows the behaviour to one error type instead of adding a second;
- it carries `Field`, `Ordinal` and `Available`, which is a strict superset of
  Java's context — `Query.InvalidExpressionException` is a message-only
  `IllegalStateException` (`Query.java:273-277`) whose text is just
  `"Missing field (#ord=" + fieldOrdinal + ")"`. CLAUDE.md's rule is to carry the
  Java exception's context fields; there is nothing there to lose;
- `errors.As` already works on it at existing call sites.

Pinned by criterion 5, which asserts the concrete type via `errors.As`, not a
string match.

### Corrected acceptance criteria (SUPERSEDED — section 12 is the live list)

> These encode the abandoned 2 → 1 scope: criterion 1 says "2 → 1 … point at
> step B", which 9.2 reversed, and criterion 8 makes the stress comparison the
> acceptance evidence that 10.2 demotes below the e2e + golden delta. Nothing
> here carries 10.1's atomic-flip pin or 11's growth pin.
> **Do not implement against this list.**

1. `boundary` bucket goes **2 → 1**. The `EqualFold` entry is removed; the `!=`
   entry's text is rewritten to name `Evaluate`'s chained arm as what keeps it
   alive and to point at step B. `TestFieldDebtBucketsArePartition` green.
   The resolver's new decision site is declared in `translator` with its count
   and its once-per-plan property (ruled in section 4), not allowlisted.
2. A test pins that **no per-row name decision remains on the baked path** —
   `descendResolvedPath` reaches no name lookup.
3. **A nested read SUCCEEDS**, end-to-end through SQL, with a plan-shape
   assertion proving the baked nested path was taken and not a decline to a
   fallback. Both laps found the silent-decline failure mode independently; this
   is the criterion that cannot be satisfied by a decline.
4. The existing escaped-spelling pin (`values/proto_field_escaped_name_test.go:39`,
   `a$b` → `a__1b`) stays green through the move, re-pointed at the resolver.
5. Out-of-range and negative ordinals raise `*values.OrdinalResolutionError`, for
   **pinned and unpinned paths alike**, asserted with `errors.As`. The unpinned
   arm is the behaviour change; a test driving only the pinned arm passes with the
   change half-applied.
6. Each converted producer's decline path is driven by an explicit test.
7. `fuse_nested_suffix_test.go:150-155` (the negative-ordinal decline) is
   re-pointed, and `values.go:478-482`'s stale "four producer sites" count is
   corrected (section 7.4).
8. **A before/after stress comparison**, per CLAUDE.md's planner-change rule —
   section 7.1 showed this change makes `left_outer_existential.go`'s rebase stop
   declining, so plans appear that did not exist before. Row counts and durations
   recorded.
9. Every pin mutation-checked per direction — wrong ordinal, tolerated `-1`,
   silent-instead-of-loud miss, unresolved-name decline — with the RED line quoted.
10. `just test` green; the affected Bazel targets green with
    `--nocache_test_results`, with the executed-test count read from the output.

### 7.5 `index_expansion.go`'s three `-1` cases — audited

The `-1` at `index_expansion.go:564` is the accessor's default; `:568-570`
overwrites it only when `!collectionPath`. The three retaining cases:

**(a) `collectionPath == true` — deliberate, and for the same reason as the
unnest producers.** `collectionPath` is `true` at exactly one call site, the
`Field_FAN_OUT` arm (`:436-443`), which builds the collection value fed to an
`Explode`. `fanOutFieldPathValue`'s own doc states the rule (`:521-526`):

> *"For a collection path, nested proto-message suffixes remain name-addressed
> (ordinal -1); ordinary scalar paths resolve every available nested ordinal."*

So this is the third instance of the identical decision `unnest_seed.go:172-176`
and `unnest_gather.go:182-190` document — a struct materializes as a proto
message, so the ordinal was never going to be consulted. It converts with them,
by the same Step 2 resolution.

**(b) non-record walk and (c) name miss** are joint: both set the accessor to
`-1` *and* collapse `currentType`/`leafType` to `UnknownType` (`:572-579`). They
are the "we lost the type, carry the name" arm. Under Step 2 they become an
explicit decline — the caller already has a decline path
(`lazyFanOutFieldPathValue`, `:539/:545/:550`).

**The ordinal on the non-`-1` path is descriptor-true**, by the same argument as
section 2.2: `recordTypeField` (`:609-622`) returns the position in
`recordType.Fields`, and `baseType` is the proto-derived `RecordType`. Note it
matches with `strings.EqualFold` (`:617`) — **the resolver in Step 1 must preserve
that case-insensitivity**, which is the third independent confirmation of R2's
normalization requirement.

### 7.6 `gk.Type()` provenance — PARTIAL, and this is why step B is separate

Established. Callers of `expandGroupingKeysToPrimitives` (scoped, non-test):
`aggregate_index_candidate.go:290`, `:328`,
`rule_push_requested_ordering_through_groupby.go:72`,
`rule_implement_streaming_agg.go:166`. Each passes `gb.GetGroupingKeys()`, and
`grouping_primitive_expansion.go:27` passes each key's own `Type()`.

Established: a base-table struct column collapses to `UnknownType`
(`FieldTypeForFD`), and `PrimitiveAccessorsForType`'s non-record arm
(`primitive_accessors.go:43-50`) then returns the base value **unchanged** — no
chained `FieldValue`, so no proto descent. The record arm therefore requires a
key carrying a *stated* `RecordType`.

**NOT established: which producers can state one, and specifically whether a
`tableColumns` product can reach it.** That is the live hazard — a `tableColumns`
row type is upper-cased and carries `__ROW_VERSION` at index `protoFields.Len()`
(`cascades_translator.go:249-269`), so converting `Evaluate`'s chained arm before
answering it risks an out-of-range read on a live path.

**This is the reason step B is scoped separately (section 8) rather than folded
in.** It is not a deferral: the question is named, the hazard is named, and it is
the first item of step B's own audit.

**The two arms are provably disjoint, which is what makes the split sound rather
than convenient.** Scoped:

```
grep -n "descendResolvedPath" pkg/recordlayer/query/plan/cascades/values/values.go
→ 252, 862, 1360 (comments); 893, 904 (doc + definition); 866 (the ONLY call)
```

The sole call is `evaluateOrdinal:866`, reached only when the evaluation context
is an `OrdinalRow` (or a `*RowEvalContext` with `Positional` set) — i.e. after a
plan-time-baked ordinal has already read the root slot. `Evaluate`'s chained
proto-message arm (`:1190`/`:1194`) is a *sibling* branch of the same dispatch
and never reaches `descendResolvedPath`. So step A cannot perturb the path whose
audit is incomplete, and step B cannot be smuggled in by accident.

### 7.7 CORRECTION to 7.5 — case (a) is a MEMO-IDENTITY constraint, and it makes the change ATOMIC

The completed `index_expansion.go` audit overturns 7.5's reading of case (a).
7.5 said the `collectionPath` `-1` is "the same decision as the unnest producers,
converts with them." That is right about the *shape* and wrong about the *reason*,
and the real reason imposes a constraint the RFC did not have.

**Provenance.** `git log -S collectionPath --oneline -- .../index_expansion.go`
returns exactly one commit (`bde66debe`), and `git show` confirms the parameter,
the `-1` mint and the `if !collectionPath` guard arrived in one hunk. There is no
"we tried the ordinal and it broke X" history; the doc comment is the whole
argument.

**The reason.** The value built here is the **candidate** side of the memo. It is
compared against the **query** side (`unnest_seed.go` / `unnest_gather.go`)
through `ExplodeExpression.EqualsWithoutChildren` (`expressions/explode.go:134-144`)
→ `FieldPath.Equals`, which is **ordinal-only** (`values.go:634-644`), and hashed
by `semantic_hash.go:125-129`, which folds **only** the per-step ordinals:

```go
for _, acc := range t.Resolved.Accessors { _, _ = fmt.Fprintf(h, "#%d", acc.Ordinal) }
```

So the candidate mints `-1` to stay bit-identical to a query side that also mints
`-1`. **If the candidate baked a real ordinal while the query side still minted
`-1`, the two Explodes would hash into different memo buckets and compare
unequal, and the fanout index match would be SILENTLY LOST** — not wrong rows, a
lost optimization, which no correctness test detects.

**Consequences for this RFC, all new:**

1. **The change is ATOMIC across three producers.** `index_expansion.go:564`,
   `unnest_seed.go:177` and `unnest_gather.go:189` must flip in ONE commit.
   Converting any subset breaks fanout index matching. Section 4 Step 2 is
   amended to say so; it previously read as three independent edits.
2. **The candidate-side `-1` is never evaluated.** Its consumers are structural
   only (`Equals`, `HashCodeWithoutChildren`); `descendResolvedPath` is reached
   solely from `evaluateOrdinal` on the *query*-side plan. So this site costs
   match opportunities, never wrong rows — which correctly re-ranks it: the two
   query-side producers are the wrong-column risk, this one is a match risk.
3. **`recordTypeField` returns a `RecordType.Fields` slice index**
   (`index_expansion.go:609-622`), matched with `strings.EqualFold`. That the
   slice index equals the descriptor declaration index still has to be
   established for this site — it is the same equivalence section 2.2 established
   for `rlcatalog`, not a free inheritance of it.
4. **Reachability of case (a) is Java-metadata-only.** `builder.go:333`
   `AddFanOutIndex` accepts only a direct top-level array column, and the SQL DDL
   path never emits the `nesting(field(A,SCALAR), field(B,FAN_OUT))` shape. So
   case (a) is reachable from Java-authored metadata loaded out of FDB, and I
   could not construct a Go-only path. Stated rather than guessed.
5. **Nothing requires `-1` to remain `-1`.** Every consumer treats a negative
   ordinal as "decline" (`values.go:494-497` `OrdinalIn`,
   `column_identity.go:370-382` `statesColumnPath`, `:412-425` `SameColumnPath`).
   Flipping all three producers *upgrades* those from decline to answer — a
   second, unlooked-for benefit, and also a second behaviour change to measure.
6. **No test pins any suffix accessor from this site.** The only ordinal
   assertion is on `Root()` (`index_expansion_test.go:502`). Cases (b) and (c)
   have no producer and no test at all.

### 7.8 Step B — scoped as a follow-up section of this RFC

Named here so section 8's promise and the document agree.

**Step B converts `Evaluate`'s chained proto-message arm** (`values.go:1190`/`:1194`)
to read `f.Resolved.Root().Ordinal`, retiring the second `boundary` entry and
deleting `protoFieldByName`'s name head outright.

**Its gating audit is 7.6's open question**, restated as step B's entry
condition: enumerate every producer whose `RecordType` can reach
`PrimitiveAccessorsForType`'s record arm, and establish whether a `tableColumns`
product — upper-cased, with `__ROW_VERSION` at index `protoFields.Len()`
(`cascades_translator.go:249-269`) — can be among them. If it can, step B needs a
pseudo-field-aware bound before it converts anything.

**It is a section of this RFC, not a TODO entry and not a separate RFC**, and it
does not start until step A has both ACKs and has landed.

### 7.9 CORRECTION to 7.6 and 8 — the `gk.Type()` hazard is REFUTED; step B is unblocked

The completed `gk.Type()` audit answers the question 7.6 left open, and the
answer removes the reason step B was scoped separately. Recorded as a correction
rather than folded silently, because it reverses a scoping decision this RFC
already argued for.

**The record arm IS reachable in production**, on `rule_implement_streaming_agg.go:166`,
whose leaves become `plans.SortKey.ValueExpr` (`:198`) and ARE evaluated at
runtime. The other three callers
(`rule_push_requested_ordering_through_groupby.go:72`,
`aggregate_index_candidate.go:290`, `:328`) are plan-time only.

**Exactly two producers can state a `RecordType`, and BOTH are descriptor-ordered:**

| Producer | Reached by | Descriptor-ordered? |
|---|---|---|
| `structColumnType` (`expr/expr.go:1796-1820`, via `columnCascadesType:1831-1835`) | `GROUP BY <struct column>` | **Yes** — `rlcatalog.go:365-368` appends one entry per proto field in descriptor index order; `structColumnType` stamps `Ordinal: i` 1:1 |
| `RecordConstructorValue.Type()` (`values.go:4596-4608`) | `GROUP BY (a,b)`, and `GROUP BY (x)` — a one-element constructor is NOT unwrapped (`walk.go:1479-1482`) | **Yes** — the descriptor is stamped from the constructor's own `Type()` (`plan_finalize.go:161`) and `buildRecordMessage` binds positionally with a loud length-drift guard (`record_constructor_message.go:34-47`) |

**The `__ROW_VERSION` hazard does NOT materialise.** A `tableColumns` product
cannot reach `rt`: the expression resolver never returns a bare
`QuantifiedObjectValue` (all three mints at `expr.go:362`, `:553`, `:620` pass it
as a *child*), a `FieldValue` baked against a `tableColumns` `RecordType` can
never be record-typed (`NewFieldValueOfOrdinal` takes
`rt.Fields[ordinal].FieldType`, and every such field is `FieldTypeForFD` or the
primitive `NullableVersion`), and `GROUP BY (t.*)` is refused outright
(`walk.go:1516`). The auditor states this as "no producer found" rather than an
exhaustive proof of a negative, which is the correct strength of claim.

**Consequences:**

1. **Step B is unblocked and the bucket CAN go 2 → 0** in this RFC. Section 8's
   2 → 1 split was correct *given an unanswered question*; the question is now
   answered by measurement, so keeping the split would be caution theatre.
   Sections 8 and 7.8 are amended: step B proceeds, in this RFC, with its own
   producer audit **done** rather than pending.
2. **The two arms still get SEPARATE mutation checks.** They fail in different
   directions (wrong column vs out-of-range) and CLAUDE.md is explicit that a fix
   with several independent directions is mutated per direction. The split
   survives as a *verification* boundary, not a scope boundary.
3. **Add an explicit loud bound at the `Evaluate` arm**, not a reliance on
   `Fields().Get()`. That is where a *future* `rt` producer — a `tableColumns`
   type, or a `structColumnType` whose catalog and stored descriptor drift —
   would surface, and a panic is the wrong way to learn it.
4. **A name-path test proves NOTHING about the ordinal path here, and this is a
   trap worth naming.** `structColumnType` names its fields with
   `recordlayer.ToUserIdentifier` (`rlcatalog.go:335`) — the USER identifier,
   which differs from the storage name for any DDL name containing `$`, `.` or
   `__`. Today's read only works because `protoFieldByName` folds and unescapes;
   the ordinal read bypasses that mismatch class entirely. So the conversion is
   an *improvement* on exactly the escaped-identifier class, and the existing
   name-path assertions cannot detect a regression in it. Criterion 4's
   escaped-spelling pin must therefore be driven through the ORDINAL path too,
   not merely kept green on the name path.
5. `struct_grouping_fdb_test.go:137,151,164` (`GROUP BY home` over a nested
   `STRUCT ADDR`, asserting a two-level descent) is the existing end-to-end
   sentinel for this arm and is what criterion 3 extends.

## 9. Revision 4 — reconciliation, and the ONE audit still open

Lap 3 verified 7.4 (the stale count, independently re-swept), 7.5, 7.3, the
step A/B disjointness, and 7.2's census leg. Three things need fixing and one is
a genuine hole.

### 9.1 OPEN — 7.1's `into == nil` is asserted, not proven. This is the blocker.

`left_outer_existential.go:193` reads `w.Typ.Fields[legOrdinal].FieldType`, and
`w.Typ` is `qov.Typ.(*RecordType)` (`ordinal_seed_layout.go:256`, `:347`, `:358`)
— **whose producers this RFC never audited**. 7.1 established the collapse only
for `FieldTypeForFD` base-table legs and then generalised, which is exactly the
move 7.6 was honest enough not to make.

If ANY leg states a struct field type, `suffixInto` is non-nil,
`assertSuffixStep` runs on the production path with Step 2's derived ordinals,
and `values.go:5243` takes `ord` from that `rt` — so a non-descriptor-ordered
`rt` is a wrong-column read on the one production caller. Lap-2 condition 1
returns in full.

**This is the last open item and it gates implementation.** Two acceptable
resolutions, to be decided by the audit rather than in advance: audit every
producer of `qov.Typ` reaching `OrdinalSeedLegWindows`, or route the suffix
through Step 1's descriptor resolver so the site never depends on `rt` at all.
The second is strictly safer and is the likely answer, since it makes the
question moot rather than answered — but it is not asserted here without the
measurement.

### 9.2 Section 4 and section 8 reconciled — section 4 was right

Lap 3 flagged section 4's "the `boundary` bucket goes to 0" and "convert BOTH
reads" as contradicting section 8's 2 → 1. Both were flagged correctly *at the
time* and both are now **restored by 7.9**: the `gk.Type()` audit removed the
reason for the split, so the bucket does go to 0 and both reads do convert.

**Section 8's 2 → 1 split is SUPERSEDED by 7.9**, and 7.8 with it. The document's
final position, in one place so no reader has to reconcile four revisions:

- **Bucket: 2 → 0.** Both entries retire, because both `protoFieldByName`
  callers convert.
- **Both arms convert in this RFC**, with SEPARATE mutation checks (they fail in
  different directions: wrong-column vs out-of-range).
- **The three `-1` producers flip ATOMICALLY** in one commit (7.7), or fanout
  index matching silently breaks.
- The header line's "2 entries to 1" is corrected to 2 → 0.

The intermediate 2 → 1 position is kept in the document rather than erased: it
was the correct call while 7.6 was open, and the record of *why* it was correct
then and is not now is the useful artifact.

### 9.3 `NewFusedFieldValueOfNestedOrdinal` — settled, deferral text removed

Section 4 Step 4's "if the answer is not established, the constructor declines"
is struck. The answer is established: the constructor's fused path
(`slot.Resolved.WithSuffix(leaf.Resolved)`, `values.go:5169`) is read by
`evaluateOrdinal` → `descendResolvedPath` like any other, so **it does reach the
proto arm and it joins Step 2**. Its two callers are `left_outer_existential.go:276`
and `exists_gathered_cluster_wrap.go:172`. Its `legOrdinal` indexes the LEG's row
type, not a struct descriptor, so it is a genuine domain conflation and Step 2
must give it a descriptor-derived suffix ordinal or decline at construction —
the runtime decline stays as a guard, never as the answer.

### 9.4 Status

**NOT APPROVED — three laps, still NAK.** One substantive audit open (9.1). The
convergence is real: lap 1 killed a design, lap 2 killed a scope, lap 3 killed
one asserted premise and confirmed everything else. Implementation does not start
until 9.1 is discharged and both reviewers ACK.

## 10. Binding implementation constraints

These three are not notes. Each was found by an audit that overturned a stated
assumption in an earlier revision, and each changes the shape of the commit.

### 10.1 The three `-1` producers flip ATOMICALLY, in one commit

`index_expansion.go:564`, `unnest_seed.go:177`, `unnest_gather.go:189`. The
reason is memo identity, not evaluation (7.7): the candidate side is compared to
the query side by ordinal-only `FieldPath.Equals` (`values.go:634-644`) and
hashed by ordinal-only `semantic_hash.go:125-129`. Flip a subset and the two
Explodes land in different memo buckets — **fanout index matching is silently
lost, and every correctness test still passes**, because the rows are right and
only the plan is worse.

**Required pin, and it is the one this RFC would most easily have shipped
without:** a test asserting the candidate-side and query-side Explode values
still compare equal AND hash equal. **Mutation-checked by flipping ONE producer
alone** — not by reverting the whole change. A mutation that reverts all three
leaves the pin green and proves nothing; the failure direction being guarded is
*divergence between* producers, so the mutation has to create exactly that.

The gap is confirmed, not assumed: `expressions/explode_test.go` has hash tests,
but they cover determinism (`:148-151`) and plain-vs-ordinality discrimination
(`:184`) — **nothing compares a candidate-side Explode against a query-side one.**
No existing test would go red on a partial flip.

### 10.2 The rebase stops declining — plan-shape evidence, not the stress table

Today every carried `-1` errors at `values.go:5266-5271`, `descendOrFail` sets
`failed = true` (`left_outer_existential.go:49-52`), and the rebase declines.
After the change it succeeds.

**Plan shapes that become reachable**, to be confirmed against goldens rather
than asserted: a LEFT-OUTER/existential correlation whose inner predicate
references a **nested struct field of an outer leg** — the
`WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.k = t1.n.sk)` family named at
`left_outer_existential.go:167-169` — currently falls back to the unrebased form
and after this change rebases to the fused two-step ordinal address. Same for the
gathered-cluster wrap (`exists_gathered_cluster_wrap.go:172`).

**Acceptance evidence is a correctness e2e against real FDB (testcontainers, no
mocks) plus the golden delta**, with every changed golden read and justified. The
stress comparison stays, but it measures latency and cannot see a wrong plan that
returns right rows.

### 10.3 Why the scope was 2 → 1 before it was 2 → 0

Recorded so the next reader does not re-derive the hazard. The chained-`Evaluate`
arm was scoped out because `gk.Type()`'s provenance was unaudited and a
`tableColumns` product would have been upper-cased with `__ROW_VERSION` at index
`protoFields.Len()` — an out-of-range read on a live path. **The audit (7.9)
found that product cannot reach `rt`**: the resolver never returns a bare QOV, a
`FieldValue` baked against `tableColumns` cannot be record-typed, and
`GROUP BY (t.*)` is refused (`walk.go:1516`). Both reachable producers are
descriptor-ordered. The hazard is refuted, not tolerated — do not re-open it
without new evidence, and do not treat the 2 → 1 revision as the cautious answer
it looked like.

### 10.4 Dropped

The earlier instruction to add a test arming `assertSuffixStep`'s disagreement
arm is **withdrawn**: `TestFuseNestedSuffix_Declines` already drives all six
arms, measured (7.2). Adding another would be redundant coverage sold as rigour.

## 11. §9.1 CLOSED — no producer found; the generalisation is now measured

The blocker was that 7.1's "`suffixInto` is nil on the production path" was
established for `FieldTypeForFD` base-table legs and then generalised. Here is
the enumeration that replaces the generalisation.

**The reduction.** `left_outer_existential.go:193` asserts on
`w.Typ.Fields[legOrdinal].FieldType`. `w.Typ` has three mint sites in
`values/ordinal_seed_layout.go`:

- `:256` and `:358` — `Typ: legType`, where `legType, isRT := qov.Typ.(*RecordType)`;
- `:347` — a synthesized 1-field window, `FieldType: qov.Type()`.

`:347` is **structurally safe**: it is the `!isRT` branch, so `qov.Typ` is not a
`*RecordType`, and `qov.Type()` is `WithNullability(q.Typ, true)`
(`values.go:4801-4806`) which cannot turn a non-record into a record. So the
question reduces to the two `legType` branches: **can a merge-leg QOV's own
`RecordType` have a `*RecordType`-typed FIELD?**

**The enumeration.** 39 non-test call sites of `NewQuantifiedObjectValueOfType`:

```
grep -rn "NewQuantifiedObjectValueOfType(" --include="*.go" pkg/ | grep -v _test.go | wc -l
→ 39
```

Narrowed to the types that can be a merge-RC leg, every one collapses struct
columns before they can become a field type:

| Leg type source | Field types from | Struct column becomes |
|---|---|---|
| `ordinal_seed.go` `ordinalLegType` (scan / gated-box legs) | `ordinalLegColumns` → `FieldTypeForFD` (`cascades_translator.go:368-372`) | `UnknownType` |
| `derivedOutputColumns` (derived table / CTE, `cascades_translator.go:577-600`) | set to `values.UnknownType` **unconditionally** at `:598` | `UnknownType` |
| `aggregateOutputColumns` (`:819-841`) | inherits the input leg's `c.FieldType`, and falls to `UnknownType` on any code mismatch (`:841`) | `UnknownType` |
| `unnest_seed.go:254` inner element leg | `arrayFieldElementType` (`cascades_translator.go:1579-1603`) | **`UnknownType`** — the `MessageKind` arm returns `values.UnknownType` for every non-UUID message (`:1598-1600`), so an ARRAY OF STRUCTS states no record type |

The last row is the one that looked most dangerous and is the most decisive: an
unnest over an array of structs — the exact shape this whole descent exists for —
still yields `UnknownType`, because the only element-typing function on this path
refuses message kinds. The recursive builder that *would* produce a nested
`RecordType`, `TargetElementType` (`:337-355`), has exactly ONE ENTRY POINT
outside its own recursion, and it is `insert_cascades.go:371` — the DML target
side, never a merge leg. Scoped, because an earlier draft of this sentence said
"one non-test caller" and that is the very unscoped-enumeration defect 7.4
exists to correct:

```
grep -rn "TargetElementType(\|TargetTypeForFD(" --include="*.go" pkg/ cmd/ | grep -v _test.go
→ insert_cascades.go:371            TargetElementType  (ENTRY POINT)
→ cascades_translator.go:326, :328  TargetElementType  (internal recursion)
→ cascades_translator.go:347        TargetTypeForFD    (internal recursion)
→ cascades_translator.go:316, :337  declarations
```

Three call sites of `TargetElementType`, not one; two are the pair recursing into
itself, and `TargetTypeForFD`'s only non-test call site is likewise internal.
What the argument needs is the entry-point count, and that is one.

**Answer: no producer found.** `suffixInto` is nil on every enumerated path, so
`assertSuffixStep` is not reached from `left_outer_existential.go`, and the
Step 2 conversion cannot become a wrong-column read there. 7.1's claim stands, now
on an enumeration rather than on a generalisation from one case.

**Stated at its real strength, because the failure this closes was overreach:**
this is "no producer found over the enumerated space", not a proof of a negative.
The space enumerated is the four leg-type sources above, reached from 39
constructor sites. What would re-open it is a new leg-type source that states a
struct field type — which is exactly what **RFC-204 §4.4/§4.5 will do** when it
makes the flowed row carry nested types. So this closure has a stated shelf life
and a named expiry, and the pin below is written to fail when it expires rather
than to pass quietly.

**Required pin:** a test asserting that for every merge-leg window derived by
`OrdinalSeedLegWindows`, no `w.Typ.Fields[k].FieldType` is a `*RecordType` —
failing with a message saying that RFC-204 has landed and
`left_outer_existential.go:193` now needs the descriptor-resolver route (section
9.1's second resolution), not that the test is wrong. This is the guard-direction
rule: the population is watched for GROWTH, and growth is the alarm.

**Status: SUPERSEDED — see section 13. This closure does not hold.**

### 11.1 The narrowing, completed — the five sources the table did not name

The four-row table above lists leg-type *sources*; five call sites reach
`NewQuantifiedObjectValueOfType` with types the table does not name explicitly.
Closed here so the enumeration is checkable rather than trusted:

- `clustered_outer_scalar.go:108` — `concatType` is `t.ordinalLegType(j)`
  (`:104`). Row 1.
- `scalar_subquery_seed.go:51` — `outerType` is `t.ordinalLegType(outerOp)`. Row 1.
- `positional_merge.go`, `rule_implement_nested_loop_join.go` (`mergedRowType`,
  `leg.alias`/`rt`), `exists_gathered_cluster_wrap.go:486` (`mergedType`) — all
  MERGED types assembled from leg types already in the table, or returned by
  `OrdinalSeedLegWindows` itself. They inherit the collapse; they do not
  introduce a field type.
- The two hand-built one-field inner types are the only genuinely new shapes, and
  both are scalars by construction:
  - `scalar_subquery_seed.go:108-110` — `{Name: innerTitle, FieldType: scalarType}`,
    the scalar subquery's result type;
  - `clustered_outer_scalar.go:501-503` — `{Name: scalarCol, FieldType:
    values.WithNullability(values.UnknownType, true)}`, explicitly Unknown.

So the enumeration covers every non-test constructor site that can produce a
merge-leg QOV type, and none of them yields a `*RecordType`-typed FIELD.

## 12. Acceptance criteria — THE LIVE LIST

Every earlier criteria list in this document (section 5, section 8) is
superseded. This is the only one to implement against. Falsifiable, each naming
what makes it fail.

**Scope pinned here so it cannot drift again:** the `boundary` bucket goes
**2 → 0**; BOTH `protoFieldByName` callers convert (`values.go:972`
`descendResolvedPath`, and `values.go:1190`/`:1194` `Evaluate`'s chained arm);
the three `-1` producers flip ATOMICALLY.

1. **Bucket 2 → 0.** Both `protoFieldByName` entries removed from
   `knownFieldDecisionDebt`; `TestFieldDebtBucketsArePartition` green. The new
   construction-time resolver's decision site is DECLARED in the `translator`
   bucket with its count and its once-per-plan property (ruled in section 4) —
   never allowlisted.
2. **`protoFieldByName` contains no name lookup.** `ByName`, `EqualFold` and
   `ToProtoBufCompliantName` do not appear in it. Verified by grep, output pasted.
3. **A nested read SUCCEEDS**, end-to-end through SQL against real FDB
   (testcontainers, no mocks), with a plan-shape assertion proving the baked
   nested path was taken and not a decline to a fallback. Both review laps found
   the silent-decline failure mode independently, in two different mechanisms;
   this is the criterion a decline cannot satisfy.
4. **The escaped-spelling class is pinned through the ORDINAL path.**
   `values/proto_field_escaped_name_test.go` (`a$b` → `a__1b`, `:39`) stays green,
   re-pointed at the resolver. Per 7.9(4) a name-path assertion proves nothing
   about the ordinal path — `structColumnType` names fields with
   `ToUserIdentifier` (`rlcatalog.go:335`), so the ordinal read bypasses the
   user-vs-storage mismatch entirely and needs its own coverage.
5. **Out-of-range and negative ordinals raise `*values.OrdinalResolutionError`**
   — asserted with `errors.As`, never a string match — for **pinned and unpinned
   paths alike**. The unpinned arm is the behaviour change; a test driving only
   the pinned arm passes with the change half-applied.
6. **ATOMICITY PIN (10.1).** A test asserting the candidate-side and query-side
   Explode values still compare equal AND hash equal. **Mutated by flipping ONE
   producer alone** — a mutation reverting all three restores bit-identity and
   stays green, proving nothing. No existing test covers this
   (`explode_test.go:148-151`, `:184` cover determinism and
   plain-vs-ordinality only).
7. **GROWTH PIN (11).** A test asserting that no `w.Typ.Fields[k].FieldType` from
   `OrdinalSeedLegWindows` is a `*RecordType`, failing with a message saying
   RFC-204 has landed and `left_outer_existential.go:193` now needs the
   descriptor-resolver route — not that the test is wrong. Watched for GROWTH:
   growth is the alarm.
8. **PLAN-SHAPE EVIDENCE (10.2).** The `left_outer_existential` rebase stops
   declining, so plans appear that never existed. Acceptance evidence is the
   correctness e2e plus the **golden delta**, every changed golden read and
   justified. The stress comparison is run but is NOT the evidence — it measures
   latency and cannot see a wrong plan that returns right rows.
9. **Each converted producer's decline path** driven by an explicit test (census
   rule: an arm the corpus happens not to reach is an untested arm).
10. **Guard texts corrected at source:** `values.go:478-482`'s `-1` producer
    count (done, and it must go to zero with this change);
    `fuse_nested_suffix_test.go:150-155`'s negative-ordinal justification
    re-pointed; the `EqualFold` debt entry's "only by convention" claim about
    `fuseNestedAccessors` rewritten per 2.2, in both the ratchet and
    `proto_descent_test.go`'s pin doc.

    **`field_name_decision_test.go:680` IS edited — 7.3's ruling is REVERSED.**
    7.3 said the entry needed no edit, and that was correct against the
    then-current design. Section 15 falsifies it twice over:
    - the entry says the arm *"Retires with the merged-layout struct-typing work
      booked in TODO.md, at which point the assert arm becomes the live one"* —
      15.2(2) DELETES the arm, so it never becomes live and retires here instead;
    - it says *"on the production path it is not [available]"* — section 14
      proved a record-typed leg field IS reachable, so the stated reason was
      false even before the design changed.

    **The entry is DELETED, not rewritten — those were mutually exclusive and
    the draft asserted both.** The entry is keyed on
    `values.go # assertSuffixStep # a == comparison # 1`, and 15.2(2) deletes
    that comparison. A *rewritten* entry would point at a site with zero
    occurrences, and the census fails count-1/actual-0 — the ratchet is a count,
    not a boolean, which is exactly what makes it catch this. Acceptance: the
    `//pkg/docscheck` run showing no orphaned entry, output pasted. A guard left
    pointing at a dead expectation is the shelf-life failure, and this one was
    pointing at two.
11. **Mutation-checked per direction, separately** — wrong ordinal, tolerated
    `-1`, silent-instead-of-loud miss, unresolved-name decline, and producer
    divergence (criterion 6). Each RED line quoted verbatim. The two converted
    arms fail in different directions and get separate checks.
12. `just test` green; affected Bazel targets green with `--nocache_test_results`,
    executed-test counts read from the output rather than assumed.

## 13. §11 RE-OPENED — the enumeration axis was wrong (again)

Section 11 claimed closure. It does not hold, and the way it fails is the same
way 7.1 failed: an exhaustive sweep along an axis that does not cover the
population.

**The hole.** §11 enumerated *SQL-translator column builders*. The population is
*QOV types that land in a merge RC*, and at least one feeder is a **physical
plan**, not a translator:

`rule_implement_nested_loop_join.go:2229` builds `rt := &values.RecordType{Fields:
concatFields}`, mints the leg QOV over it (`:2235`), and makes each `FieldValue` a
merge-RC field (`:2244`) — exactly `OrdinalSeedLegWindows`' flat-run shape
(`ordinal_seed_layout.go:226-256`). So `w.Typ.Fields[k].FieldType` **is**
`concatFields[k].FieldType`, and `concatFields` comes from
`planBuriedLegConcat:2072-2074`:

```go
rt, isRT := inner.GetResultType().(*values.RecordType)
...
return rt.Fields, ...
```

— the result type of a scan / index / covering **plan**. Nothing in §11 touches
that. `positional_merge.go:100`'s `legTypes` scavenge is a second unenumerated
feeder.

**What remains to audit**, precisely, so the next pass does not re-derive it:
whether any physical scan/index/covering plan's `GetResultType()` `*RecordType`
can carry a struct-typed FIELD. Named feeders to trace:
`cascades_translator.go:2594`, `index_expansion.go:95` and `:221`,
`rule_ordered_index_scan.go:273`, `plan_extraction.go:632`, plus
`positional_merge.go:100`. The likely answer is that they too route through
`FieldTypeForFD` and collapse — but **that is the guess §11 already got burned
making, and it is not recorded as a result until measured.**

**Status: §9.1 is OPEN.** The `left_outer_existential.go:193` question is not
settled, so Step 2 is not cleared to implement. Two resolutions remain as stated
in 9.1, and the second — routing the suffix through Step 1's descriptor resolver
so the site never consults `rt` at all — is now clearly the better one, because
it makes this entire enumeration unnecessary rather than merely complete. **A
design that cannot be undermined by a missed feeder beats an enumeration that has
now been wrong twice.** The next revision should adopt it and keep the growth pin
as the backstop.

### 13.1 Corrections folded

- **`derivedOutputColumns` row (11).** "set to `values.UnknownType`
  unconditionally at `:598`" is FALSE as written: `:598` is the `LogicalProject`
  arm only. The function has eleven arms — `LogicalScan`/`LogicalJoin`
  (`:613-616`) delegate to `legColumns`, `LogicalAggregate` to
  `aggregateOutputColumns`, `LogicalUnion` to `unionOutputColumns`, four recurse.
  The conclusion survives via `legColumns` → `FieldTypeForFD`, but the stated
  reason covered 1 arm of 11.
- **Line-number inconsistency.** Section 3's table says `index_expansion.go:562`
  (the `ResolvedAccessor{` literal); 7.4 and 10.1 say `:564` (the `Ordinal: -1`
  line). Both are the same mint. Standardised on **`:564`**, the line the grep
  matches.
- **Section 9.4 is superseded** by section 12 (live criteria) and by this
  section: the status is NOT "nothing open" — it is one open audit, named above.

## 14. REFUTED, with the producer in hand — the design change is now forced

Section 13 said §11's closure "does not hold" and named an unaudited axis. The
completed audit goes further: **the premise is false, and here is the producer.**

### 14.1 `w.Typ.Fields[k].FieldType` IS a `*RecordType`

`rule_implement_nested_loop_join.go:2126-2155`, `planBuriedLegConcat`'s
`*plans.RecordQueryFlatMapPlan` arm, states it in its own comment
(`:2133-2136`):

> *"A positional merge's concat is its N MERGE SLOTS, not its legs' columns.
> **Each field is typed with that sub-leg's own `*RecordType` (the whole row
> lives in the slot)**"*

```go
fields[i] = values.Field{Name: f.Name, FieldType: qov.Type(), Ordinal: base + i}
if rt, isRT := qov.Typ.(*values.RecordType); isRT && rt != nil { /* nested leg */ }
```

The field type IS a record, and it is INTRODUCED here — not inherited from any
row of §11's table. It becomes the leg QOV's type at `:2229-2235`
(`rt := &values.RecordType{Fields: concatFields, ...}`), and
`ordinal_seed_layout.go:256` files it as `w.Typ`.

### 14.2 It reaches the NARROW entry too, through a width test

`:2224-2228` records `.Legs` **only when `len(buriedLegs) > 1`**. The narrow
walk's fail-closed decline keys on `.Legs` (`ordinal_seed_layout.go:402`):

```go
if leg.Kind == LegKindNested && !acceptNested { return nil, nil, nil }
```

So a leg plan that is a positional merge with **exactly one** record-typed slot
records no legs at all, the decline has nothing to fire on, and the narrow entry
returns a window with a record-typed field. That shape is documented as real —
`ordinal_seed_layout.go:159-168` records 750 of 18246 merge slots stating no
type, every witness an unnest ELEMENT alias, i.e. exactly the
`{_0: typed leg, _1: untyped element}` merge that `FROM t, t.arr AS x` collapses to.

§11 also reduced `w.Typ` to three mint sites and never mentioned
`positionalMergeWindows` (`:340-363`) or the nested sub-windows (`:506`, `:533`),
which exist only under `OrdinalSeedLegWindowsAcceptingNested` — and
`left_outer_existential.go:193` is reached from that entry too, via
`rule_implement_nested_loop_join.go:4055` → `:4219`. The reduction was incomplete
on its own axis as well as on the population.

### 14.3 Why this is the wrong-column read, exactly

The record a merge-slot field points at is a **whole sub-leg ROW**, not a struct
column. Under Step 2, a suffix accessor carrying a descriptor-derived ordinal
would be resolved against that row's field list — an ordinal from one layout
indexing a different one. **That is precisely the conflation `OrdinalIn` exists to
refuse**, and it is the failure §9.1 was opened to prevent. The audit found it
before it shipped, which is the entire value of having held the gate.

### 14.4 The design is no longer a choice

§9.1's second resolution — **route the suffix through Step 1's descriptor
resolver so `left_outer_existential.go:193` never consults `rt` at all** — is now
REQUIRED, not the safer of two. An enumeration that has been wrong twice is not
the instrument to bet a wrong-column read on. Section 4 Step 2 is amended: the
site takes the resolver route, `suffixInto` stops being consulted, and the
question of what `w.Typ` can carry becomes irrelevant rather than answered.

### 14.5 Criterion 7 is inverted — it must go RED today

§12's growth pin asserts no `w.Typ.Fields[k].FieldType` is a `*RecordType`. That
is now known FALSE. It is rewritten as its own mutation check: a fixture built
from `reconstructFoldStep1Seed` over a `RecordQueryFlatMapPlan` positional merge
**must produce a record-typed leg field today**, and the pin asserts the
RESOLVER ROUTE is taken there rather than that the population is empty. If a
version of that fixture passes an emptiness assertion, that is a statement about
fixture coverage, not about the population — the exact false green CLAUDE.md
names.

### 14.6 Two more counts of mine, corrected

- "39 non-test call sites of `NewQuantifiedObjectValueOfType`" counts the
  DEFINITION line. Call sites: **38**.
- §11 cited `derivedOutputColumns:598` as the collapse. The site that actually
  feeds `ordinalLegType` for a project leg is `legColumns`'
  `*logical.LogicalProject` arm (`cascades_translator.go:487-506`), which takes
  `o.ProjectedValues[i].Type()` and **can state a `*RecordType`**. Whether a bare
  `LogicalProject` reaches it in a gated-join leg position is UNAUDITED — flagged,
  not claimed, and it does not matter once 14.4's route is taken.

**Status: premise refuted, design forced, criteria 7 inverted. Next revision
implements the resolver route; no further enumeration is warranted.**

## 15. Step 2, AMENDED — the descriptor resolver route

This supersedes section 4's Step 2 wherever the two differ. It is written as the
design, not as an option, because section 14 removed the alternative.

### 15.1 The property being bought

The earlier Step 2 gave producers descriptor-derived ordinals and left the
consuming sites resolving those ordinals against whatever `*RecordType` happened
to be in scope. That is sound only while every such `RecordType` is
descriptor-ordered — a property established by ENUMERATION, and the enumeration
was wrong twice (11 missed a physical-plan feeder; 13/14 found the producer).

The resolver route buys a different property:

> **A nested suffix step is resolved against the DESCRIPTOR OF THE MESSAGE IT
> WILL ACTUALLY READ, and against nothing else. No `*RecordType` is consulted on
> the descent path at all.**

That is not a better enumeration; it removes the question. A missed feeder can no
longer produce a wrong-column read, because no feeder's type participates in the
decision. **Prefer a shape that makes a failure impossible over one that detects
it** — and the failure here is a silent read of the wrong stored column.

### 15.2 What changes

1. **`left_outer_existential.go:193` stops computing `suffixInto`.** The
   `w.Typ.Fields[legOrdinal].FieldType.(*values.RecordType)` assertion is deleted,
   not fixed.

   **Precisely where the resolver runs, because "descendOrFail takes the
   resolver" would be the wrong reading:** the resolver runs at the PRODUCER
   (`unnest_seed`, `unnest_gather`, `index_expansion`), which is the only place
   that holds the descriptor of the message the suffix will descend. By the time
   a suffix reaches the rebase site its ordinals are ALREADY descriptor-true, so
   the rebase site's correct behaviour is to **carry them verbatim and consult
   nothing** — `FuseNestedSuffix` with `into == nil`, the carry arm that already
   exists (`values.go:5261-5264`, *"No type to descend: the carried accessor
   stands alone"*).

   So this is a DELETION, not a new dependency: the site needs no descriptor,
   because it needs to make no decision. That is what makes 15.1's property
   cheap — the site with no type available is also the site with nothing to
   decide. The one guard that must stay is the carry arm's `step.Ordinal < 0`
   check (`values.go:5266-5271`), which after Step 2 is unreachable from production and
   becomes the tripwire for a producer that forgot to resolve.
2. **`FuseNestedSuffix`'s `into` parameter is retired — and there is no other
   caller to keep it for.** An earlier draft of this clause said the
   `assertSuffixStep` arm "stays for callers that genuinely hold a schema-stated
   struct type." **That population is EMPTY**, and asserting it was this
   document's fourth unchecked-population error:

   ```
   grep -rn "FuseNestedSuffix(" --include="*.go" pkg/ cmd/ | grep -v _test.go
   → left_outer_existential.go:51   (the ONE production caller)
   → values.go:5205                 (the definition)
   ```

   `exists_gathered_cluster_wrap.go:172` goes through
   `NewFusedFieldValueOfNestedOrdinal`, not this function. So retiring `into` at
   `:51` leaves `assertSuffixStep` **test-only**, reached solely by
   `TestFuseNestedSuffix_Declines`. That is a real consequence and it is stated
   rather than softened: either the parameter and its assert arm are deleted with
   the route, or they are kept as deliberately-unreached defensive code with a
   comment saying so. **Decision: DELETE.** Unreached code whose only exerciser
   is its own test is the dead-twin shape this repo keeps finding; keeping it
   would preserve exactly the `rt`-consulting path 15.1 exists to remove, ready
   for a future caller to re-arm.

   **The reasoning in those decline arms is NOT discarded — it RELOCATES to the
   resolver**, which is the site that holds the right input to make it. Each arm
   maps, and the mapping is the argument that DELETE is not a loss:

   | `assertSuffixStep` arm | Under the resolver route |
   |---|---|
   | name absent from the record | name absent from the DESCRIPTOR — the resolver declines, and against the authoritative list rather than a mirror of it |
   | name ambiguous (duplicate) | proto descriptors cannot carry duplicate field names, so the case ceases to exist rather than needing a guard |
   | carried ordinal DISAGREES | **no analogue, and none is needed**: the producer MINTS the ordinal, so there is no second opinion to contradict. This arm exists only because the old design had two sources for one fact |
   | unnamed step out of range | resolver bounds-checks against `Fields().Len()`, and the read bounds-checks again (Java's `MessageHelpers.java:171`) |
   | negative ordinal, no layout | cannot arise — a resolved step is non-negative by construction; the read's `< 0` check stays as the tripwire (15.2(1)) |
   | descends past a STATED leaf | the descriptor says it: a non-message field has no sub-descriptor to descend, so the resolver declines |

   Every arm either relocates to a better-informed site or becomes
   unconstructible. That is what distinguishes deleting a mechanism from
   deleting the thinking inside it.
3. **The three `-1` producers** (`unnest_seed.go:177`, `unnest_gather.go:189`,
   `index_expansion.go:564`) resolve their segments through the same resolver,
   against the descriptor they already hold. Unchanged from Step 2 as written —
   this half was never the problem.
4. **`NewFusedFieldValueOfNestedOrdinal` (9.3)** takes the resolver for its
   suffix step. Its `legOrdinal` indexes the LEG's row and must never be reused
   as a struct ordinal; that is the same domain conflation section 14.3 names,
   and the resolver route is what prevents it rather than detecting it.

### 15.3 Criterion 7, rewritten — and its own vacuity guarded

The old pin asserted the population is empty. Section 14 proved it is not, so
that pin either reds or — worse — passes on a fixture that never builds the
shape. Replaced by a pin with **two assertions, in this order**:

1. **The fixture DOES produce the shape.** Build a seed via
   `reconstructFoldStep1Seed` over a `RecordQueryFlatMapPlan` positional merge and
   assert that some `w.Typ.Fields[k].FieldType` **IS** a `*RecordType`. If this
   assertion fails, the fixture stopped exercising the case and the second
   assertion below is vacuous — so it fails LOUD, naming that, rather than
   letting the pin pass on an empty population. This is the guard the old
   criterion 7 lacked and is the reason it would have shipped green.
2. **The descent takes the resolver route** on that fixture — the suffix resolves
   against the message descriptor, and `w.Typ` is not consulted.

**Failure message states the direction:** the alarm is no longer "a record-typed
leg field appeared" (it is there today and that is expected); it is "the descent
consulted `w.Typ`", i.e. someone reintroduced the `into` route and re-armed the
wrong-column read. A guard whose expected value inverted must say which direction
is now the alarm.

### 15.4 Carried unchanged

- **Atomic flip (10.1) still binds.** Three producers, one commit, pinned by the
  candidate-vs-query Explode equality/hash test, **mutated by flipping ONE
  producer alone**. The memo-bucket split it prevents is invisible to every
  correctness test — rows stay right, only the plan gets worse.
- **`legColumns`' `LogicalProject` arm (`cascades_translator.go:487-506`) stays
  FLAGGED-UNAUDITED.** It takes `o.ProjectedValues[i].Type()` and can state a
  `*RecordType`. It is moot under the resolver route and it is deliberately NOT
  dropped: if a future change moves off that route, it becomes live again, and a
  reader must know it was never audited rather than assume it was.
- **The enumeration errors stay visible** (7.4, 11, 13.1, 14.6 — three wrong
  counts, two wrong axes). They are not tidied away, because "the enumeration was
  wrong twice" IS the argument for 15.1's property. A reader who sees only the
  conclusion would reasonably ask why the simpler route was not taken.

## 16. Revision 7 — §15.1 delivered UNCHECKED, not IMPOSSIBLE

### 16.1 The hole, and the fix that closes it for real

§15.1 claimed the missed-feeder class becomes impossible. It does not. Dropping
`into` deletes `assertSuffixStep` (`values.go:5322-5358`), **the only fail-closed
check on this path**, and §15 never enumerates who mints the suffix accessors
arriving at `left_outer_existential.go:285`/`:301`. Correctness then rests on
"every such accessor is resolver-minted" — an unenumerated producer population.

That is §11's hole wearing new clothes, and I walked into it while writing the
section whose whole point was to escape it. **Unchecked is not impossible.**

**The fix: make resolver-provenance a CHECKABLE PROPERTY carried on the value,
and assert it LOUD at fuse time.** `FieldPath.FrontierPinned` is the precedent
(`values.go:228` — a bool on the path, excluded from identity/hash/Explain
because it is an evaluation contract, not identity). A
`ResolvedAccessor.DescriptorResolved` bit follows it exactly:

- the resolver sets it, and nothing else may;
- `FuseNestedSuffix` asserts it on every suffix step and **errors** when unset,
  replacing `assertSuffixStep` as the fail-closed check — same guard position,
  but keyed on a property that is *true by construction at the mint* rather than
  re-derived from a layout that may not describe the struct;
- it is excluded from identity/hash for the same reason `FrontierPinned` is.

This restores the "impossible" claim honestly: a non-resolver-minted accessor
cannot reach the descent, because the fuse refuses it — **an asserted bridge,
never a silent fallback**. And unlike an enumeration, it cannot be invalidated
by a feeder nobody found.

### 16.2 §9.3 and §15.2(4) are WRONG — struck

`NewFusedFieldValueOfNestedOrdinal` (`values.go:5135`) has **no struct-descent
suffix step**. It fuses `slot.Resolved.WithSuffix(leaf.Resolved)` (`:5169`) where
both are ordinal-model reads — the slot in the merged row, the leaf in the leg
row — and `legOrdinal` indexes `legType`, its own domain, correctly. The nested
suffix is fused afterwards, by `descendOrFail`. So §9.3's "genuine domain
conflation" and §15.2(4)'s "must give it a descriptor-derived suffix ordinal"
are false on their face. **Struck.** The constructor needs no change; only the
suffix `descendOrFail` appends does, and 16.1 covers it.

### 16.3 Criterion 7's second assertion was unfalsifiable — fixed

Once `suffixInto` is deleted, "the descent did not consult `w.Typ`" is not
observable: there is no code left to observe. Replaced with an assertion that can
actually fail:

**Build the fixture so the leg-row field list would yield a DIFFERENT ordinal
than the descriptor does** for the same step. Then a `w.Typ`-consulting descent
returns the wrong column and the test reds on the VALUE. Assertion 1's vacuity
guard (the fixture really does produce a record-typed leg field) stays as written
— it was right.

This is the general lesson and it is worth stating: a pin that asserts an absence
of code is not a pin. Make the two candidate behaviours produce *different
answers*, then assert the answer.

### 16.4 Criterion 7 corrected in place

§12 calls itself the only list to implement against while its criterion 7 was
known-false and inverted three sections later. Fixed at the source below, not by
another forward reference — a live list that needs a later section to be correct
is not a live list.

### 16.5 Nits

`unnest_seed.go:177`, `unnest_gather.go:189` and
`exists_gathered_cluster_wrap.go:172` live in `pkg/relational/core/query/`, and
are cited bare beside `pkg/recordlayer/.../cascades` files throughout; qualified
on the implementation pass. `values.go:5169` is the `WithSuffix` line — the
constructor is `:5135`.

### 16.6 The producer resolver INHERITS all six decline arms — criterion 9 amended

15.2(2)'s table showed each `assertSuffixStep` arm relocating or becoming
unconstructible. That was written as an argument that DELETE loses nothing; it
must also be written as a **requirement**, because criterion 9 said "decline
path", singular, and a singular decline pins one arm out of six.

**The resolver must refuse on the same set**, and criterion 9 is amended to pin
each arm that survives the mapping, per producer:

1. **name absent from the descriptor** — decline (the arm that replaces
   "absent from the record", now against the authoritative list);
2. **descends past a non-message field** — decline (replaces "stated leaf");
3. **step index out of `Fields().Len()`** — decline at resolve time, and the READ
   bounds-checks again (Java's `MessageHelpers.java:171`);
4. **unresolvable name on a `collectionPath` / non-record / name-miss step** —
   decline the whole optimization, which is the `nil` return each producer
   already has (7.5(b)/(c));
5. **duplicate name** — *unconstructible*: proto descriptors cannot carry
   duplicate field names. Pinned as a NEGATIVE with a comment saying why, not
   silently dropped;
6. **carried ordinal disagrees** — *unconstructible*: the producer mints the
   ordinal, so there is no second source. Likewise pinned as a negative.

Arms 5 and 6 get negative pins rather than nothing, because "this case cannot
arise" is a claim with a shelf life — a future producer that carries an ordinal
from elsewhere re-arms arm 6, and the pin is what says so.
