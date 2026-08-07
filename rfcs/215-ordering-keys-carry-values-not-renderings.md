# RFC-215: an ordering key carries a Value, not a rendering of one

Status: DRAFT — awaiting Graefe + Torvalds
Branch: `feat/cq-dotted-a`
Instruments: `6eaf85507`, `1d656fe02`, `f2f693d46`

## 1. The finding

`plans/ordering.go:796`, in `RecordQueryInMemorySortPlan.HintOrdering`, builds an
`Ordering`'s key list like this:

```go
keys[i] = &values.FieldValue{Field: sk.Field, Typ: values.UnknownType}
```

`sk.Field` is a **rendered string**. Measured over the sqldriver real-FDB corpus
(command in §7), that site mints **10,778** lazy `FieldValue`s whose `Field`
contains both `.` and `#` — the shape `explainValueOrdinals` produces and nothing
in a schema ever contains. One witness, verbatim from the census:

```
lazy-EXPLAIN-RENDERED :: (q$3728.K#1 + q$3728.K#5)
      pkg/recordlayer/query/plan/plans/ordering.go:796
      pkg/recordlayer/query/plan/cascades/plan_properties.go:326
```

That is not a name. It is an entire `ArithmeticValue` tree rendered to text and
stored in a column-name field. RFC-197's thesis is that `FieldValue.Field` is
display-only and must never decide anything; this is display **output** fed back
in as **identity**.

### 1.1 Why no consumer-side instrument could have found it

The same corpus run records **3** flat-dotted values reaching `AccessorNamePath`,
the one match-domain identity function. 10,778 minted, 3 compared: almost every
one of these is minted, carried on an `Ordering`, and dropped without ever being
looked at.

Any instrument that watches comparisons therefore under-reports this defect by
three orders of magnitude, and the field-decision ratchet — which is entirely
consumer-side — books it as one entry describing three observations.

This generalises beyond this bucket, and it is the reason producer-side
instrumentation is now part of the method: **a small consumption population is
not evidence of a small production population.** The first version of this
investigation inferred exactly that and was wrong by 3,592x.

It has a dual already in the method, and the pair is worth stating together
because they are the same mistake at opposite ends of a pipeline:

- **Reachability is not volume.** A panic probe proves a site is reached; it says
  nothing about how much traffic it takes, and the planner has no `recover()` so
  it dies at the first hit. Use a counter for anything but "is this reachable".
- **Consumption is not production.** A consumer-side counter measures what
  survives to be looked at, not what is made. Use a producer-side counter for
  anything about volume at the source.

Both are true measurements whose authority does not transfer to the corollary
someone wants to draw from them.

### 1.2 `FieldValue.Field` is mutable after construction

Found while building the instrument, and it is a structural constraint rather
than a detail. `Field` is a plain exported string, and four sites assign to it
*after* construction:

| site | assignment |
|---|---|
| `logical_predicate.go:9417` | `fv.Field = ref.col` |
| `cascades_translator.go:4016` | `baked.Field = fv.Field` |
| `cascades_translator.go:7335` | `baked.Field = fv.Field` |
| `unnest_gather.go:399` | `baked.Field = fv.Field` |

Enumerate with `grep -rn '\.Field = ' pkg/ | grep -v '_test\.go'`.

The consequence is not only that a construction-time *hook* cannot be complete.
It is that a construction-time **invariant** cannot be either: RFC-197 cannot
enforce "a `Field` is never a rendering" at any constructor, because a node can
acquire a name its constructor never saw.

**Decision:** `Field` should become unexported with a read accessor, and the four
assignment sites should go through an explicit rename method. Booked as §8
follow-up F1, AFTER the conversion.

The sequencing has two justifications and the obvious one is the weaker one.
Efficiency — converting 57 non-test literals once rather than twice — is real but
not load-bearing. **The load-bearing reason is SAFETY: the mint census hooks all
four post-construction mutation sites, so while it stands, a new
rendering-into-`Field` mint appearing anywhere it can see shows up as a census
delta.** Doing F1 first would remove the very sites the census watches and leave
the conversion window unguarded — the window in which a regression is both most
likely and least visible. The instrument goes away last.

## 2. Java is the spec, and it settles the design

`OrderingPart.java` (4.12.11.0):

```java
public class OrderingPart<S extends OrderingPart.SortOrder> {
    @Nonnull private final Value value;                    // :54
    @Nonnull private final S sortOrder;                    // :57

    @Nonnull public Value getValue() { return value; }     // :68

    // equals
    return getValue().equals(keyPart.getValue()) &&        // :94
           getSortOrder() == keyPart.getSortOrder();

    // hashCode
    return Objects.hash(getValue(), getSortOrder().name()); // :104

    // toString
    return getValue() + getSortOrder().getArrowIndicator(); // :109
}
```

Three facts, and together they are the whole design:

1. **An ordering key holds a `Value`** (`:54`), never a string.
2. **Identity is `Value.equals`** (`:94`) and the hash is over the `Value`
   (`:104`).
3. **The rendering exists only in `toString`** (`:109`) — the exact separation Go
   inverted at `ordering.go:796`.

Precise spans, since these are the citations the design rests on: `equals` is
`:85-96` with `:94` the comparison line; the ordering-lookup map is declared
`Map<Value, O>` at `:126` and `:129` is the `put` into it:

```java
resultMapBuilder.put(orderingPart.getValue(), orderingPart);   // :129
```

There is no string anywhere in the identity path. `toString()` (`:109`) is the
ONLY place a `Value` becomes a `String` in `OrderingPart.java`, and neither
`Ordering.java` nor `RequestedOrdering.java` has a String field at all — Java's
ordering identity is `Value`-based end to end.

### 2.1 Java's name-insensitivity is specific to the RESOLVED form

The accessor-level rule is sharper than "Java ignores names", and the sharper
version is the one that supports this RFC.

`ResolvedAccessor.equals` (`FieldValue.java:675-685`, comparison at `:684`)
compares `getOrdinal()` and nothing else; `hashCode` is
`Objects.hash(getOrdinal())` (`:689`); the name survives only for rendering
(`:431`, `:695`). **But the UNRESOLVED `Accessor.equals` (`:633`) DOES compare the
name.**

That is not a complication, it is the principle stated exactly: **before
resolution the name is all the identity there is; after resolution it is
decoration.** Go's lazy `FieldValue` is the unresolved form and its name-based
comparison is legitimate there. The defect at `ordering.go:796` is that a key
which HAS a resolved `Value` in scope is deliberately downgraded to the
unresolved form — and to a rendering of it rather than even a name.

**The RFC is therefore a port, not a design.** Go must carry the `Value`.

## 3. The decision

**`HintOrdering` carries `sk.ValueExpr` — the real key `Value` — instead of
wrapping `sk.Field` in a fresh lazy `FieldValue`.**

The mechanism already exists and is already required. `plans.SortKey`
(`in_memory_sort.go:25`) is:

```go
type SortKey struct {
	Field      string
	Desc       bool
	NullsFirst bool
	ValueExpr  values.Value // REQUIRED: the plan-time-baked key Value, evaluated per row
}
```

`ValueExpr` is the baked `Value` the executor already evaluates per row. It is the
Go spelling of Java's `OrderingPart.value`. `HintOrdering` ignores it, renders
`Field`, and wraps the rendering — so the correct object is in scope at the defect
site and is being discarded in favour of its own `toString`.

**The two are minted on the same line, from the same source, into the same
struct.** Both non-test `SortKey` constructions
(`rule_implement_in_memory_sort.go:135`, `rule_implement_streaming_agg.go:128`)
set `ValueExpr` unconditionally, and directly above each, `Field` is built as
`values.ExplainValue(sk.Value)`. The witness in §1 —
`(q$3728.K#1 + q$3728.K#5)` — is `ExplainValue` applied to the very expression
sitting in `ValueExpr` beside it. There is no availability question to resolve and
no migration to stage: the conversion drops a rendering in favour of the object it
was rendered from.

`Ordering.Keys` is already `[]values.Value`, so the container needs no change.
This is a one-expression change at the defect site plus whatever the change in
comparison behaviour requires (§4, §5).

### 3.1 Rejected alternatives

**Parse the rendering back into a Value.** Rejected outright. It is the string
hack RFC-197 exists to remove, it cannot be made correct (`(q$3728.K#1 +
q$3728.K#5)` requires a full expression parser, and `explainValueOrdinals` escapes
`#` to `##` so the encoding is not even injective in the direction that matters),
and it would make the rendering a wire format between two parts of the planner.

**Keep the rendering and teach consumers to tolerate it.** This is the status quo
plus effort. `AccessorNamePath` already tolerates it, by declining — which costs a
residual filter or a redundant sort on every one of these keys. The tolerance is
what hid the defect for as long as it hid: a decline is silent.

**Give `FieldValue` a `Rendered bool` so consumers can detect the shape.** Loses
because it makes the rendering legitimate. The `#` discriminator this
investigation used is a *diagnostic*, not a contract, and promoting it to a field
would mean the planner permanently carries two kinds of name and every consumer
grows a branch.

**Make `Ordering` hold `OrderingPart` structs mirroring Java's class.** Not
rejected on merit — it is where Go should end up, and Java's `sortOrder` bundles
`Desc`/`NullsFirst` more coherently than Go's three parallel slices. It is
rejected *for day one* only, because it is a container refactor across every
producer and consumer of `Ordering` and it is separable from the defect. Booked as
F2.

## 4. Why this is RFC-first rather than a one-line fix

The change is one expression. Its consequence is not.

Today an ordering key is a lazy `FieldValue` holding a rendering, so
`ColumnNamePathsEqual` **declines** on it (`AccessorNamePath` refuses to split a
flat-dotted name). After the change the key is a baked `Value`, so the same
comparisons start **succeeding** — which is the point, and also the risk: every
ordering claim that currently fails to match will begin matching.

That changes **plan selection**. `orderingSatisfiesGroupingKeys`
(`rule_implement_streaming_agg.go:225`) is the measured consumer; a grouping key
that now matches an available ordering selects a streaming aggregate where the
planner previously took a sort. That is the correct plan and it is a different
plan, and "the planner picks better plans now" is exactly the class of change this
project requires a Graefe lap and a stress comparison for.

Fixing it inline would have been the third "small and safe" of this session. The
first two each cost a full cycle.

### 4.1 What gates the merge, and what does not

**The goldens gate the merge. The stress comparison is a secondary control and
waits for disk headroom.**

Stated explicitly because the opposite is the natural assumption for a change that
alters plan selection. The repo's stress-comparison workflow requires a baseline
worktree and this branch on the SAME filesystem, and records that ext4
point-lookup latency degrades sharply above ~95% utilisation and reports as a
planner regression. `/home` has been at 99% throughout this investigation,
oscillating between 5G and 16G free as concurrent agents build and caches are
reclaimed. A stress run taken there measures the disk — and it measures it in the
direction that manufactures a false regression.

The goldens are the better primary control regardless: they are STRUCTURAL rather
than timed, §5 already requires one per distinct changed shape, and a changed plan
is exactly what a golden is for. The stress comparison adds a latency check no
golden gives, so it is not dropped — it is sequenced after headroom exists. Left
implicit, this invites someone to run it at 99% and read noise as a regression.

### 4.2 A plan-time nil the executor guard cannot catch

`HintOrdering` runs at PLAN TIME, ahead of the executor. `SortKey.ValueExpr` is
documented REQUIRED and `TestSortCursor_UnbakedKeyIsLoud` pins a loud rejection
for a nil one — but that guard lives in the CURSOR, so after the conversion a nil
`ValueExpr` would yield a nil ordering key and a silently degraded ordering claim
long before the cursor is reached.

Production cannot reach it: both non-test `SortKey` constructions
(`rule_implement_in_memory_sort.go:135`, `rule_implement_streaming_agg.go:128`)
set `ValueExpr` unconditionally. So this is ONE ASSERTION at the conversion site,
not a redesign — a nil `ValueExpr` in `HintOrdering` is a planner bug and must say
so where it happens, rather than becoming a missing ordering key that costs a sort
and explains nothing.

## 5. Deliverable 1 — the gate, before the conversion

**Booked first, gate-before-conversion, following the CQ-95 erratum pattern: an
instrument that can refute the target for the price of one corpus run, run before
any behaviour changes.**

Add a **shadow-comparison census** at the ordering comparison sites
(`ColumnNamePathsEqual` via `orderingSatisfiesGroupingKeys`, and
`CanBridgeOrderingFieldValues`). At each comparison, evaluate the answer **twice**
— once as today (rendered key) and once as it would be with `sk.ValueExpr` — and
record the pair. The conversion is not applied; only the counterfactual is
measured.

The classes partition every comparison, with an independent denominator:

| class | meaning |
|---|---|
| `AGREE-true` | both say same column |
| `AGREE-false` | both say different |
| `DIVERGE-would-now-match` | today declines, `ValueExpr` matches — **the population whose plans change** |
| `DIVERGE-would-now-differ` | today matches, `ValueExpr` says different — **must be zero** |

Readings and what each licenses:

- **`DIVERGE-would-now-differ` must be 0** — and a non-zero must be DIAGNOSABLE,
  not merely counted. This class has two causes with opposite implications:

  1. **The rendering carried information the `Value` does not.** Refutes the
     design; this RFC stops.
  2. **Today's match is a name-based conflation** — two different columns whose
     renderings collide — **and the baked `Value`s correctly refuse it.** That is
     RFC-197 working exactly as intended, and stopping on it would be stopping on
     success.

  A bare count cannot separate them, and an undiagnosable kill condition is one
  that gets argued away the first time it fires: someone proposes cause 2, nobody
  can refute it, and the gate quietly becomes advisory. That is the precise
  failure the gate exists to prevent, so it must not be reachable.

  **Every `would-now-differ` therefore records a witness**: both renderings, both
  `Value`s, and both ordinals with their `OrdinalDomain`s. Cause 2 is then visible
  directly — two different ordinals under one rendering is a conflation the
  conversion fixes; the same ordinal under two renderings is cause 1 and stops the
  work. The witness set is capped PER CLASS (§6.2).

### 5.1 The shadow evaluation must not perturb what it measures

Argued rather than assumed, because §1.2 establishes that `Field` is mutable at
four sites — so "a second comparison is obviously harmless" is not available.

**Both evaluations must be pure, and that is a constraint on the instrument
rather than an observation about it.** `ColumnNamePathsEqual`, `AccessorNamePath`
and `CanBridgeOrderingFieldValues` read `Field`, `Child` and `Resolved` and assign
to none of them; the four mutation sites of §1.2 all sit in translator/gather
paths that no comparison calls. The shadow arm may call only those three, and must
not route through any rebase, bake or pull-up walk — every one of which can
rename.

This is checkable rather than asserted: the mint census already counts every
`Field` write it can see, so **the shadow census's own acceptance criterion is
that enabling it moves no mint-census class.** If it does, the shadow arm is
mutating the tree it is measuring, and it is withdrawn rather than explained.
- **`DIVERGE-would-now-match` = 0** would mean the conversion is inert on this
  corpus: no plan changes, and the change is a pure hygiene fix that can land with
  goldens unchanged.
- **`DIVERGE-would-now-match` > 0** is the expected case. Those are the plans that
  change, and each distinct shape gets a golden reviewed before conversion.

This is the deliverable that can kill the RFC cheaply, which is why it is first.

## 6. Scope, against the measurement

Day one covers **the Explain-rendered class only**: 10,778 mints from the one
site, `plans/ordering.go:796`.

Day one does **not** cover the ordinary dotted class — 599 mints of the
`C.AK` / `T1.ID` shape (a qualifier composed onto a leaf) from three sites:
`cascades_translator.go:6940`, `cascades_translator.go:6628`, `pullup.go:76`.
Those are ordinary missing-identity debt, not a layering violation, and they have
no `ValueExpr` sitting in scope to switch to. They are a different fix and they
are booked as F3.

### 6.1 Coverage limitation, carried forward from the census

The mint census hooks **33 sites**: 6 constructors, 23 struct literals, and the 4
post-construction `Field` mutations of §1.2. **28 struct literals remain unhooked
and out of scope** — `replace.go` (3), `max_match_map.go` (3),
`match_info_merge.go` (3), `index_onsource.go` (2),
`primary_key_translation.go` (2), and singletons elsewhere. Enumerate with:

```
grep -rn '&FieldValue{\|values\.FieldValue{' pkg/ | grep -v '_test\.go' | wc -l
```

**A zero class in that census means "no mint of this shape among the 33 sites this
census can see", never "no mint of this shape."** Every number in this RFC
inherits that bound, including the 10,778 and the 599: they are lower bounds.

### 6.2 An instrument bug worth recording

The census's first version capped captured origin stacks at 64 **shared across
classes**. 599 ordinary dotted mints can fill that before a rare class is seen —
and the rare class is what the census exists to find. Counts were never capped, so
the readings were true, but the *diagnosis* was at risk. Fixed to a per-class cap
in `f2f693d46`.

**A witness cap shared across classes is a trap the census family should not
repeat.** The general form: any bounded diagnostic sample must be bounded per
class, or the common case evicts the finding.

## 7. Reproduction

```
bazelisk test //pkg/relational/sqldriver:sqldriver_test \
  --test_output=streamed --nocache_test_results
```
exit 0, uncached. Reports:

```
field-value MINT census (33 hook sites: 6 constructors + 23 literals + 4
post-construction Field mutations; 28 literals elsewhere OUT OF SCOPE and
uncounted): total 1895411
  lazy-bare                486626
  lazy-DOTTED              599
  lazy-EXPLAIN-RENDERED    10778
  lazy-empty               0
  baked                    1397408
```

and, from the consumer census in the same run:

```
accessor-name-path census (per call to AccessorNamePath): total 269979
  OK-all-baked           203796
  OK-has-lazy            64271
  DECLINE-lazy-dotted    3
```

The partition holds against an independent denominator in both.

## 8. Retirement condition, as a measurable

**When this lands, `lazy-EXPLAIN-RENDERED` reads 0 on the corpus run in §7**, with
the total unchanged in order of magnitude (so the zero is absence of a shape, not
absence of counting). That is the acceptance criterion; nothing else is claimed.

### 8.1 Which ratchet entries retire — deliberately unanswered

The `dotted` bucket has 15 entries. This RFC retires **none of them on landing**,
and the honest reason is that nobody has measured which consumers are downstream
of this producer.

`accessor_name_path.go:74` in particular does **not** retire: it is a correct
conservative guard, and its mutation test shows that removing it reclassifies a
dotted value as an ordinary lazy name and **matches** it — the conflation the
ratchet exists to prevent. It stops *firing* when the producer is fixed; that is
not the same as it stopping being *needed*.

**What would have to be instrumented to answer it:** a per-consumer attribution
census — for each of the 15 dotted entries, whether the values reaching it carry
an Explain rendering, an ordinary qualifier, or neither. Only that reading
partitions the bucket into "retires with this producer" and "independent". That is
booked as F4, and it is the successor's deliverable 1 in exactly the way §5 is
this one's.

This is the third time this bucket has declined to batch consumers on measurement
grounds. The line holds: an entry retired on a plausible grouping is how three
inverted entries got written in the first place.

### 8.2 Follow-ups booked

- **F1** — unexport `FieldValue.Field`, route the four assignment sites through an
  explicit rename. Cost: touches all 57 literal sites; do it *after* the
  conversion so they are converted once.
- **F2** — `Ordering` holds `OrderingPart` structs mirroring Java's class rather
  than three parallel slices.
- **F3** — the 599 ordinary dotted mints at `cascades_translator.go:6940`, `:6628`,
  `pullup.go:76`.
- **F4** — per-consumer attribution census; prerequisite for retiring any `dotted`
  entry.

## 9. Review

Query-engine change (planner ordering properties, plan selection). Requires a
Graefe ACK on this RFC and on the implementation, plus Torvalds. The
implementation lap must include a stress comparison, since §4 predicts plan
changes.
