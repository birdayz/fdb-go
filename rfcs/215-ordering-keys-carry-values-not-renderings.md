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
assignment sites should go through an explicit rename method. That is not part of
this RFC's day one — it is a mechanical change across every `&FieldValue{…}`
literal (57 non-test sites, §6) and it should follow the conversion rather than
precede it, so the literals are converted once rather than twice. Booked as
§8 follow-up F1, with its cost stated rather than left to be discovered.

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

And Java keys maps by the `Value` directly:

```java
resultMapBuilder.put(orderingPart.getValue(), orderingPart);   // :129
```

so `Map<Value, O>` (`:127`) is the ordering-lookup type. There is no string
anywhere in the identity path.

This is consistent with the accessor-level rule already cited in the consumer
census: `ResolvedAccessor.equals` compares `getOrdinal()` and nothing else
(`FieldValue.java:684`), `hashCode` is `Objects.hash(getOrdinal())` (`:689`), and
the name survives only for rendering (`:431`, `:695`).

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

- **`DIVERGE-would-now-differ` must be 0.** A non-zero means the conversion
  *loses* a match that exists today, i.e. the rendering was carrying information
  the `Value` does not. That refutes the design and this RFC stops.
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
