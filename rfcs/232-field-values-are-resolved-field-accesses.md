# RFC-232: `FieldValue` means a resolved field access

**Status:** IMPLEMENTATION IN PROGRESS — exact QOV/FV and plan result contracts
have landed; executor/layout integration and full runtime gates are still being
closed.
**Base:** `9a39b5006c19`
**Relates to:** RFC-173, RFC-181, RFC-187, RFC-197, RFC-225, RFC-231
**Wire impact:** none. No key, record, index, continuation, or protobuf plan
encoding changes.

## 1. Decision and scope

After this milestone, every admitted `values.FieldValue` is one immutable,
executable field access:

- it has one non-nil typed child;
- it has a non-empty local path;
- every path step has a non-negative ordinal resolved against the enclosing
  record type;
- its result type and nullability are derived from that child and path;
- names are display metadata, never lookup, equality, hash, or runtime
  authority; and
- construction is atomic: all steps resolve, or no `FieldValue` exists.

There is no `Resolved == nil`, empty-path, childless, ordinal `-1`, or
caller-asserted result type. Unresolved identifiers remain parser,
key-expression, candidate, or ordering metadata until a typed layout resolves
them. A whole-object reference is a typed `QuantifiedObjectValue` (QOV), not a
pathless `FieldValue`.

Making that statement true requires three adjacent corrections. They are part
of this RFC rather than follow-ups because omitting any one leaves a fully
resolved ordinal attached to the wrong row:

1. QOVs become immutable, exactly typed, and explicitly bound. An unbound QOV
   never falls through to an ambient positional row.
2. Go's `FrontierPinned`, `OrdinalDomain`, and `RecordType.Legs` addressing
   state moves out of logical Values and Types into an immutable physical
   `OrdinalLayout`. A source-relative path and a carrier-relative path are two
   different legal forms and can never be mixed.
3. generic Value and predicate rewrites become checked. A rule completes all
   fallible preparation before its first memo/constraint effect, and yields are
   committed only after the rule returns without error.

Exact QOV typing also closes the existing aggregate result-type hole: logical
`GroupBy` and physical streaming aggregation expose the same ordered,
Java-shaped output descriptor. This is the only aggregate work in scope.

This RFC does **not** freeze every Value graph, put physical layout in logical
`Type.Equals` or Reference group identity, redesign Cascades grouping/costing,
change the wire, add per-owner correlation tokens, or serialize layout. It does
make Reference admission fallible so exact result-type disagreement is rejected
before hashing. Physical layout is reattached from the selected immutable plan
after continuation decoding.

Package ownership is fixed up front so the APIs below do not imply import
cycles:

| package | owns |
| --- | --- |
| `cascades/values` | exact types, correlations, the one AliasMap, QOV/FV, immutable non-Value condition trees, aggregate specs/descriptors, OrdinalLayout |
| `cascades/predicates` | predicate ↔ values-owned immutable-condition lowering; imports `values` only |
| `cascades/expressions` | purpose-specific logical owner drafts, Quantifiers, and immutable `Reference` handles over memo-store group IDs; no physical-type registry or public Reference mutator |
| `cascades/internal/memostore` | method-free group/member/constraint/property/epoch/partial-match/task/statistics storage, provisional group handles, the sole commit lock, and exact-private transaction deltas; payloads are opaque and it never imports or invokes an expression/plan method |
| `cascades` | closed all-expression registry, rule invocation drafts, memo/Reference preflight and commit, selector/property matching, phase translation and normalization orchestration |
| `plan/plans` | purpose-specific immutable physical owner constructors, provided/required layout-property views, and buffer-normalization specifications; constructors receive already-admitted child Quantifiers/References and never mint a Reference from an expression |
| `query/executor` | repository-owned record/scalar carriers and exact layout binder over the complete evaluation context |

Interfaces in pseudocode are read views. Admission is always a package-local
exact concrete switch; cross-package callers construct state only through the
named fallible factory. No lower package imports a higher package to inspect an
opaque owner.

## 2. Problem and measured starting point

Go currently uses `FieldValue` for both a resolved ordinal access and an
unresolved/name-only carrier. That sum type lets display spelling make semantic
decisions. `rebaseOuterLegValueOrdinal`, `rebaseOuterLegValue`, `legRef`, and
`classifyLegConjunct` inspect whether a field string contains `.`. A qualified
`A.B`, a nested `addr.city`, and the quoted single identifier `"A.B"` cannot be
distinguished from those bytes. `groupKeyOrdinalByStructure` can decline on a
nil path and its callers then choose a last-wins grouping slot by name.

Three producers append `ResolvedAccessor{Ordinal: -1}`. Accessor identity is
ordinal-only, so all those sentinels compare equal regardless of spelling.
Making `Resolved` merely non-nil preserves the bug.

The uncached RFC-197 debt baseline is:

```text
TOTAL 33 authority/ies (44 escape sites, 44 entries)
boundary       1 authority / 2 escapes
contract       8 authorities / 12 escapes
dotted        12 authorities / 13 escapes
harness        1 authority / 1 escape
name-keyed     4 authorities / 4 escapes
translator     9 authorities / 12 escapes
```

Production construction measurements against the RFC base are:

| population | measured count |
| --- | ---: |
| all `FieldValue` code literals | 61 in 25 files (62 raw hits; one comment) |
| direct nil-path construction expressions | 40: 29 non-factory literals plus 11 `NewFieldValue` calls |
| external `FieldValue` literals | 52 in 24 files: 29 nil and 23 path-bearing |
| external `FieldPath` / `ResolvedAccessor` literals | 10 / 5 |
| constructor calls removed or changed | 76 in 23 files |
| primary construction syntax occurrences | 143 = 52 + 10 + 5 + 76 |
| already-typed construction controls | 27 |
| direct representation assignments / shallow copies / path transforms | 18 / 2 / 15 |
| textual test `&FieldValue{` occurrences | 1,429 in 254 files |

The adjacent measured surfaces are:

| surface | production baseline |
| --- | ---: |
| QOV constructors | 87: 50 untyped, 37 typed |
| QOV concrete references / assertions or switches | 177 / 145 |
| QOV raw literals | 6 |
| `Quantifier.GetFlowedObjectValue` direct call expressions | 38 (40 dotted text minus two comments) |
| `FrontierPinned` / `OrdinalDomain` text | 78 / 96 |
| `SourceRelativeBaked` / `RootIsLegRelativeUnpinned` | 40 / 14 |
| `OrdinalIn` / `LegAwareRootOrdinal` | 79 / 6 |
| leg-layout text including `RecordType.Legs` | 207 in 21 files |
| `PositionalRow` construction surface | 23 external direct literals + 4 factory calls; factory body once (31 raw includes signatures) |
| external Value-evaluation roots (raw upper bound) | 94 `.Evaluate` calls |
| `Replace` / predicate `ReplaceValues` calls | 29 / 21 |
| RC raw concrete mentions / named constructor text / literals | 259 / 24 / 10 (AST manifest separates definitions, reads, calls) |

Raw counts are reproducible checksums, not the final proof. Checked-in
`go/types`/AST gates identify symbols across aliases, dot imports, generics,
method values, embedding, and tests; every zero result has a positive control in
the same invocation.

## 3. Java contract

Reference checkout: tag `4.12.11.0`, commit
`257aa83cae7f90e18ea6595fdf2cf841ca72e802`.

Java `FieldValue` stores an immediate non-null child and a `FieldPath` of
resolved accessors (`FieldValue.java:79-101`). Name and ordinal requests resolve
one step at a time against the child's result type; each step advances the
current type, and construction occurs only after the complete request resolves
(`:271-343`). `ResolvedAccessor` rejects negative ordinals and stores the
resolved field (`:642-673`). Evaluation sends only the ordinal vector to
`MessageHelpers` (`:163-175`), which indexes descriptor declaration order
(`MessageHelpers.java:93-106,170-175`).

The exact result-type rule is normative: the leaf type is overridden nullable
iff the child type or **any** accessor field traversed by the path is nullable
(`FieldValue.java:138-149`). A null or non-Message child evaluates to null
(`FieldValue.java:163-170`); an intermediate null also propagates.

Path identity is ordered ordinal identity. Accessor equality/hash uses only the
ordinal (`FieldValue.java:675-690`), path equality uses list equality
(`:410-425`), and the Value framework accounts for the child while
`FieldValue.equalsWithoutChildren` accounts for the path (`:212-227`). Names
and cached field types do not become accessor identity.

Java may temporarily build `FieldValue(FieldValue(...), suffix)` and later
simplify it (`ValueSimplificationTest.java:169-177,195-204`). Every node's local
path is complete. Go chooses the equivalent canonical fused form at its public
construction boundary; it does not reinterpret “complete” as one accessor.

Java QOV stores a non-null alias and exact result type
(`QuantifiedObjectValue.java:60-79`) and reads that alias's explicit binding
(`:82-94`). Java has one reserved, value-equal `Quantifier.current()` alias
(`Quantifier.java:78-82,846-849`). Current is an output/property namespace;
executable rewrites translate it to concrete aliases where required
(`AggregateDataAccessRule.java:345-361`). Java does not let every unbound QOV
read an ambient row.

Java logical References unify the exact relation result type, not a physical
carrier layout (`Reference.java:498-512`). A relational expression's type is
exactly `RELATION<getResultValue().getResultType()>`
(`RelationalExpression.java:193-196`); a Quantifier unwraps exactly that one
relation layer to obtain its flowed object type (`Quantifier.java:801-810`).
Quantifier `NullOnEmpty` then widens the flowed object at the edge
(`Quantifier.java:193-203`). Logical `GroupBy` reports grouping plus aggregate
output (`GroupByExpression.java:95-153`), and Java 4.12.11.0 declares COUNT as
nullable LONG through `Type.primitiveType` (`CountValue.java:140-142`,
`Type.java:397-406`). Bare streaming aggregation emits no row for empty input;
default-on-empty semantics are a separate owner (`AggregateCursor.java:93-96,
142-145`).

Go conservatively adds the exact QOV type to QOV equality/hash. Java can omit it
because its types and correlation discipline are immutable and consistent; Go
cannot safely equate same-alias QOVs which declare different data shapes.

## 4. Exact types, correlations, QOV, and FieldValue

### 4.1 Private exact type snapshot

QOV, FieldValue, and physical layout use one package-private immutable
`exactType`. It is built by a total checked switch over every supported concrete
`values.Type` and contains:

- type code, nullability, semantic type/record/enum names;
- ordered record fields and recursively exact field types;
- array element, relation inner type, enum members, and primitive details; and
- cached canonical bytes/hash plus read-only structural accessors.

It never contains `RecordType.Legs`; those cease to be Type state. It accepts
duplicate record field names because ordinal access is unambiguous and name
resolution must report ambiguity. A field's stored ordinal must equal its slice
position; mismatch is rejected rather than silently normalized. Construction
does not call `NewRecordType`, which currently panics on duplicate names and
drops leg metadata.

The checked switch rejects nil and typed-nil pointers, cycles, malformed codes,
erased arrays/relations, unresolved `UNKNOWN`/`ANY`/`NONE`, invalid field
ordinals, and invalid nested forms with stable error codes. Concrete SQL NULL is
permitted only where the owning Value kind gives it a declared target type; it
is never a record root.

Public `Type()` methods return fresh ordinary Type graphs. Internal resolution,
identity, layout validation, and evaluation use the cached exact snapshot and
never thaw. Exact private fast paths avoid copy-and-resnapshot when one admitted
QOV/FV supplies another factory's type.

Cross-package memo/type plumbing receives only a sealed read handle; the
implementation remains exact-private and every consumer exact-recognizes it
before calling a method:

```go
type ExactTypeHandle interface {
    Type() Type              // fresh ordinary graph
    CanonicalBytes() []byte  // defensive, collision-free identity encoding
    RelationInner() (ExactTypeHandle, bool)
    isExactTypeHandleView()
}

func SnapshotExactType(Type) (ExactTypeHandle, error)
func ExactRelationOf(object Type) (ExactTypeHandle, error)
func AsExactTypeHandle(any) (ExactTypeHandle, bool)
```

`ExactRelationOf` constructs exactly one `RELATION<object>` layer. A
`memostore.TypeRecord` carries the handle as opaque payload and the identical
canonical bytes as its storage key. `values`—not memostore—recognizes/thaws or
unwraps that payload. Embedded/nil-embedded impostors and a canonical/payload
disagreement are typed errors before a Reference is published.

### 4.2 Unforgeable correlation and exact QOV

`CorrelationIdentifier` gains a private in-memory kind discriminator. Unique,
named, and current identifiers with the same rendered text are unequal. In
particular, `NamedCorrelationIdentifier("_current")` is an ordinary user alias
and cannot forge the reserved current identifier. `CurrentAlias` stops being an
exported assignable variable; `CurrentCorrelation()` returns the global tagged,
value-equal current identifier.

This milestone does not serialize that discriminator. The Go repository has no
production QOV/Value plan serializer or deserializer; the generated
`PQuantifiedObjectValue.alias` string is not an admitted Go ingress or egress.
RFC-232 adds neither one nor a textual envelope hidden inside that string. A
`go/types` gate proves that no code path renders an admitted correlation to
protobuf bytes or reconstructs one from protobuf/display text. Existing proto
definitions and all existing bytes remain untouched. A future Go plan-codec RFC
must add an explicitly versioned kind representation; silently inferring kind
from `_current`, `q$N`, or another rendered spelling is forbidden.

```go
type QuantifiedObjectValue interface {
    Value
    Correlation() CorrelationIdentifier
    FlowedType() Type // fresh copy of the exact stored type
    isQuantifiedObjectValueView()
}

func NewQuantifiedObjectValue(
    correlation CorrelationIdentifier,
    flowed Type,
) (QuantifiedObjectValue, error)

func AsQuantifiedObjectValue(Value) (QuantifiedObjectValue, bool)
func CurrentCorrelation() CorrelationIdentifier

type AliasPair struct {
    Source CorrelationIdentifier
    Target CorrelationIdentifier
}

type AliasMap interface {
    Target(CorrelationIdentifier) (CorrelationIdentifier, bool)
    Source(CorrelationIdentifier) (CorrelationIdentifier, bool)
    isAliasMapView()
}

func NewAliasMap(pairs []AliasPair) (AliasMap, error)
func ExtendAliasMap(
    base AliasMap, pairs []AliasPair,
) (extended AliasMap, compatible bool, err error)

type FrozenPredicateCondition interface {
    isFrozenPredicateConditionView()
}

type OwnerValueDraft interface {
    AddResult(Value) error
    AddPredicateCondition(FrozenPredicateCondition) error
    AddPropertyValue(Value) error
    AddSARG(Value) error
    Current() QuantifiedObjectValue
    isOwnerValueDraft()
}

type AdmittedOwnerValues interface {
    Result() Value
    PredicateConditions() []FrozenPredicateCondition
    PropertyValues() []Value
    SARGs() []Value
    isAdmittedOwnerValuesView()
}

type OwnerValueBuilder func(OwnerValueDraft) error

func BuildOwnerValues(
    ownerResult Type, build OwnerValueBuilder,
) (AdmittedOwnerValues, error)
```

The concrete node is exact-private. The exported interface is only a read view:
another package can embed it and promote its private method, so every admission
or recognition site uses the exact `As...` type switch before invoking methods.
Nil-embedded and hostile embedded views are rejected.

The ordinary QOV constructor rejects `CurrentCorrelation()`; only
`BuildOwnerValues` can mint an admitted current handle. This makes the
owner-pointer validation mechanically enforceable without changing current's
semantic correlation identity.

There is no end-state untyped QOV factory. Every admitted Reference stores the
exact `RELATION<ResultValue.Type()>`; it never stores the bare object type.
`Quantifier.GetFlowedObjectType` changes from `(*RecordType, error)` to
`(Type, error)`, requires exactly one `RELATION` wrapper, unwraps it once, and
then applies edge nullability so scalar explode/unnest objects are legal.
Missing and double relation wrappers are typed errors.
`RequireFlowedObjectValue() (QuantifiedObjectValue, error)` reports
empty/memberless references, unavailable type, and member disagreement. Its 38
direct callers plus indirect result owners are migrated individually. Empty
References are not quantified; no-plan/applicability is represented outside a
QOV.

`NullOnEmpty` widens the exact flowed type once at the quantifier edge. QOV
`Type()` returns that exact type unchanged; it does not blanket-nullify every
object.

QOV identity has three explicit protocols:

- raw equality compares correlation and exact semantic type;
- equality under an `AliasMap` compares the mapped correlation and exact type;
- semantic hash includes the QOV tag and exact type, but excludes correlation
  bytes so alpha-renamed equal Values always hash equally.

Different aliases may collide in the hash. Same alias/different type is not
equal. Two independent operator phases may each use current with different
types; there is no whole-plan current-type uniqueness rule.

A validated exact-private immutable `values.AliasMap` may map named and unique identifiers in any
combination, as ordinary Cascades alpha-equivalence requires. It may map tagged
current only to tagged current. `NewAliasMap` rejects current↔noncurrent with
`CorrelationKindMismatch`; duplicate/non-bijective input is a typed error, while
`ExtendAliasMap` reports a legitimate pairing conflict as `compatible=false`.
Equality APIs exact-recognize only that validated map and remain boolean. The
current `values`, `expressions`, and `cascades` AliasMap implementations collapse
to this one representation; package adapters are type aliases/read views, not
independent raw maps. Raw map literals/conversion and panic-on-compose helpers
are gated to zero. Current-to-
concrete phase translation uses the explicit typed-root API below. Layout
equality follows the same validated map.

Because Java current is also global, same-typed current roots are separated by
non-semantic phase-construction handles rather than new correlation identities.
Every expression/plan purpose constructor declares its named evaluation phases
(for example input, merged-predicate, aggregate-state finalization, and output).
It invokes one `BuildOwnerValues` draft per phase, each with that phase's exact
type/layout and one exact current QOV pointer. Every retained result, predicate,
property Value, and SARG is assigned to exactly one phase. Owner admission
requires each local current occurrence to be the exact allocation for its
declared phase, not merely semantically equal. A newly rebuilt owner gets fresh
phase handles and atomically re-roots every local Value; a getter on the same
owner returns stable admitted graphs.

Current may occur only in an owner's output-relative descriptor/property or in
a Value evaluated by that owner's explicitly current-bound phase. It is never a
window Source. Crossing a quantifier edge uses
`TranslatePhaseRoot(sourceQOV, targetQOV)` after proving the exact target type;
crossing to a new output phase uses the destination owner's callback handle.
Purpose-specific owner validation and a complete Current construction/
translation census reject a same-typed child current retained in a parent
without making the handle part of semantic equality/hash.

`BuildOwnerValues` is that one-shot Value-closure funnel. It freezes the result type, mints
the tagged-current QOV privately (the draft exposes it only during the callback),
and exact-copy/freezes every retained graph through exhaustive Value and
predicate-condition registries. Query predicates first lower through a
purpose-specific `predicates.FreezeCondition` API into a values-owned,
exact-private immutable **non-Value** condition tree whose leaves are admitted
immutable Values; `predicates.ConditionFromFrozen` reconstructs only immutable
read views. Condition tags/connectives have closed purpose factories in
`values`, so that package never imports or switches on predicate concrete
types. This avoids a `values` -> `predicates`
import cycle and lets the values registry recognize the concrete node without
trusting an external interface. It walks the frozen closure to require the exact handle at
every local-current occurrence and publishes nothing on error. The returned
admitted view never exposes the QOV handle separately and returns stable
immutable graphs or defensive slices.

Freezing preserves exact-private immutable QOV/FV/aggregate/condition node
pointers and shared-DAG topology; it copies mutable repository nodes and datum
payloads around them. In particular it never clone-mints the draft's current
QOV. The identity memo and active-stack guard make this preservation explicit,
and a test puts one current QOV under multiple result/predicate/property paths
and requires the one exact handle after admission.

The draft accepts exactly one result, any ordered number of conditions/
properties/SARGs, and no additions after the callback returns. Missing or
duplicate result, a declared `ownerResult` different from the frozen result,
retaining/reusing the draft, or supplying a foreign frozen-condition view is a
typed construction error with no published state.

Purpose-specific owner constructors finish a typed **multi-phase** owner draft
atomically. The draft contains a named map of `AdmittedOwnerValues`, defensive quantifier/reference views,
exact layout requirements, and every type-specific property; there is no public
generic `BuildOwner[T]`. Each constructor validates the complete draft and
publishes the owner once, so no caller can attach quantifiers, predicates,
layouts, or properties after current-handle admission. Its `GetResultValue`,
predicate, property, quantifier, and member getters cannot leak mutable backing
state. The migration deletes mutable predicate wrappers such as
`predicateValue.SetPredicate`; cross-package predicate conditions lower through
a values-owned immutable private composite/rebuilder so the freeze registry has
no import cycle. A per-kind owner-retained closure manifest covers all concrete
Values and predicates, mutable literal payloads, nested children, and metadata.

That manifest gives every concrete Value kind one explicit disposition: reuse
an exact-private immutable node, rebuild it with recursively frozen children and
payload, or reject it at the owner constructor. `FreezeDatum` deep-copies bytes,
arrays, tuples, supported maps in canonical key order, supported protobuf
messages, and typed scalar literals while rejecting cycles and unknown mutable
pointers. A UDF retained by an owner stores an immutable registry identifier/
version and frozen arguments; an arbitrary Go closure or captured mutable
receiver is rejected rather than called later. Descriptor-bearing nodes retain
an immutable repository handle excluded from semantic identity but validated
before execution. There is no catch-all reflection fallback. Mutation tests
cover every admitted payload/child and every rejection row.

Every result producer stores a stable QOV or result Value. A getter never mints
a fresh unique correlation. `PlanExprBase`, the four affected logical result
producers, logical GroupBy, and streaming aggregation are migrated as part of
the 50 untyped-QOV ledger.

Aggregate input and aggregate-state/output phases are separate controls:
different types are expected, and using the input current inside the complete
result or output current inside a grouping operand is rejected. Join
input-edge, merged-predicate, and output phases receive the same treatment.

### 4.3 Exact FieldValue, path, and accessors

```go
type FieldValue interface {
    Value
    ChildValue() Value
    Path() FieldPathView
    DisplayName() string
    ResultType() Type
    isFieldValueView()
}

type FieldPathView interface {
    Len() int
    Ordinals() []int
    Accessor(int) (ResolvedAccessorView, bool)
    isFieldPathView()
}

type ResolvedAccessorView interface {
    Ordinal() int
    DisplayName() (string, bool)
    FieldType() Type
    isResolvedAccessorView()
}

func AsFieldValue(Value) (FieldValue, bool)
```

The implementation contains only private state:

```go
type fieldValue struct {
    child      Value
    rootType   exactType
    path       fieldPath       // copied, non-empty, fully resolved
    resultType exactType       // derived; never supplied
}
```

The child is non-nil and is the sole `Children()` entry, so FieldValue is not a
`LeafValue`. Public getters return scalar data, copied slices, or fresh Type
graphs. No interface assertion is recognition; `AsFieldValue` accepts only the
exact private concrete node.

The public resolver canonicalizes a FieldValue child by concatenating its full
path onto the base child, in inner-to-outer order. Thus no admitted FieldValue
has a FieldValue child, although the API accepts the Java-equivalent chained
input. The root semantic type is part of node information in addition to the
ordinal path; this prevents any QOV identity mistake from conflating
incompatible accesses. Display names and independently
cached accessor field types remain excluded.

Raw and alias-aware equality compare the ordered ordinal path and root semantic
type, then compare the child through the ordinary Value protocol and the same
alias map. Hash folds the node tag, root semantic type, ordered ordinals, and
the child's alias-invariant semantic hash. It never folds display names or raw
correlation bytes. Equal therefore implies equal hash under alpha-renaming.

The admitted canonical child is always one exact QOV. This is a Go
normalization of Java's more general child shape, not a semantic restriction:

- a FieldValue child is fused inner-to-outer until its QOV base is reached;
- an ordinary finalized record constructor is collapsed synchronously: the
  first ordinal selects its child and the remaining suffix is resolved or
  collapsed recursively;
- a record-producing scalar subquery/operator result is first bound to its
  exact quantifier QOV; and
- a parameter or unsupported computed record cannot acquire a FieldValue until
  a typed owner binds it to a QOV.

`ResolveFieldAccess` therefore returns `Value`, because one-step RC collapse
returns the selected child rather than an FV. No admitted FV retains an RC,
arbitrary mutable Value, FieldValue, or open implementation. A checked
child-kind census classifies every existing root and rejects a hostile external
kind.

Ordinary RCs are migrated first to Java's nonnullable-by-default result type.
Direct collapse is allowed only when the selected Value's exact type equals the
field-access result type. An explicitly nullable RC or any root-nullability
difference is bound through a QOV (or represented by a nullability-preserving
checked Value) rather than returning a narrower child. This preserves the
child-or-path nullability OR.

### 4.4 Structured requests and atomic construction

Requests are sealed variants, not a struct with two optional pointers:

```go
type FieldRequest interface { isFieldRequest() }

func FieldByName(semanticName string) (FieldRequest, error)
func FieldByOrdinal(ordinal int) (FieldRequest, error)
func FieldByNameAndOrdinal(
    semanticName string, ordinal int,
) (FieldRequest, error)

func ResolveFieldAccess(child Value, path []FieldRequest) (Value, error)
func TryResolveFieldAccess(
    child Value, path []FieldRequest,
) (value Value, applicable bool, err error)
func RebuildFieldValue(fv FieldValue, child Value) (Value, error)
```

One request is exactly one parsed semantic identifier segment. No function
splits a string on `.`. Quoted `"A.B"` is one `FieldByName("A.B")`; qualified
`A.B` is a QOV for `A` plus one request for `B`; nested `addr.city` is two
requests.

`ResolveFieldAccess` snapshots the complete child type, then resolves all steps
before allocating output. At each step it proves a record, unique name when
requested, non-negative in-range ordinal when requested, name/ordinal
agreement, and record intermediate when more steps remain. It derives each
accessor field snapshot and the Java nullability OR. Empty paths return the
child only through a separate whole-object API; they are invalid here.

`TryResolveFieldAccess` is only for optional optimizer candidates. It maps the
documented unknown/ambiguous/nonrecord/out-of-range/incompatible-layout codes to
`applicable=false, err=nil`. The same miss through SQL semantic resolution is a
typed user error. Malformed metadata, nil, negative ordinals, internal type
contradictions, and hostile Values are never declines.

### 4.5 Runtime FieldValue semantics

FieldValue evaluates the child once. Nil propagates. A nonnil child which is
neither a protobuf message nor an ordinal row returns nil, matching Java's
non-Message rule. Otherwise the evaluator walks the **entire** ordinal path:

- an ordinal row is read only through its ordinal API;
- a protobuf ordinal indexes descriptor declaration order, never a field name;
- an intermediate nil propagates;
- a repeated protobuf leaf returns a list;
- a present singular or an explicit protobuf default returns the value/default;
- an absent singular without a default returns SQL NULL; and
- a short row, out-of-range descriptor, or incompatible nested carrier is a
  typed malformed-runtime error.

Runtime name lookup is absent. The captured exact root/result snapshots are
checked against layout and runtime descriptor shape before data is read.
After ordinal descriptor lookup and presence/default handling, every protobuf
leaf is converted through the one canonical engine conversion currently named
`ProtoFieldToRowValue`; direct FV, positional, covering, and executor paths do
not return raw `protoreflect.Value`. The shared conversion owns repeated,
enum/scalar, tuple/UUID, `NullableArrayWrapper`, and explicit proto2-default
semantics. Bypassing it in any one path is a mutation-test failure.

## 5. Physical ordinal layout and explicit binding

### 5.1 Separation from logical identity

`OrdinalLayout` is a provided/required **physical property**. It is never a
`Type`, Value discriminator, logical-expression discriminator, or Reference
group invariant. `RecordType.Legs`, `FrontierPinned`, and `OrdinalDomain` are
deleted after every reader is migrated.

Each physical evaluation phase has an exact current carrier QOV and one
immutable layout. Parent and child phases can use global current with different
types because their evaluation contexts are distinct. Physical operators own
or derive every input/evaluation layout plus their output layout: a projection
evaluates against its selected child layout and emits another; a join predicate
uses its merged layout; aggregate keys/operands use input layout and aggregate
results use output layout.

### 5.2 Immutable layout representation

```go
type OrdinalTileKind uint8

const (
    OrdinalTileInvalid OrdinalTileKind = iota
    OrdinalTileFlat
    OrdinalTileNested
)

type OrdinalTileSpec struct {
    Parent []int // path to a carrier record; empty means carrier root
    Start  int
    Width  int
    Kind   OrdinalTileKind
}

type OrdinalWindowSpec struct {
    Source        QuantifiedObjectValue
    ObjectPath    []int
    FieldPaths    [][]int
    NullSupplying bool
}

type OrdinalLayout interface {
    Carrier() QuantifiedObjectValue
    CarrierKind() OrdinalCarrierKind
    RawEqual(OrdinalLayout) bool
    EqualUnderAliases(OrdinalLayout, AliasMap) bool
    AliasFreeHash() uint64
    isOrdinalLayoutView()
}

type OrdinalCarrierKind uint8

const (
    OrdinalCarrierInvalid OrdinalCarrierKind = iota
    OrdinalCarrierRecord
    OrdinalCarrierScalar
)

func NewOrdinalLayout(
    carrier QuantifiedObjectValue,
    tiles []OrdinalTileSpec,
    windows []OrdinalWindowSpec,
) (OrdinalLayout, error)
func NewScalarOrdinalLayout(
    carrier QuantifiedObjectValue,
) (OrdinalLayout, error)

func LayoutProvides(
    layout OrdinalLayout, source QuantifiedObjectValue,
) (bool, error)
func LayoutSatisfies(
    layout OrdinalLayout, required RequiredBindings,
) (bool, error)
```

The carrier is an exact QOV for tagged current and an exact type. Record mode
requires a recursively resolved record and the tile/window rules below. Scalar
mode requires a non-record exact type, has zero tiles/windows, and binds current
directly to the scalar; it cannot satisfy a source-window requirement or be a
FieldValue root. The concrete layout and all nested specs are exact-private,
immutable, and cached. No slice, map, Type, or child layout leaks.

At every `Parent`, flat/nested tile ranges form an exact non-overlapping
partition. Starts are non-negative, widths positive, bounds valid; nested width
is one and identifies a nested physical carrier. Parent paths strictly descend
through the finite exact type, so a separate layout-cycle state/test does not
exist. A record-valued ordinary column remains a flat column;
nested carrier structure is explicit, never inferred from a record type.

Exactly one of `ObjectPath` and `FieldPaths` is present for a window. Object
mode binds a whole scalar or record Source at that carrier path. Field mode
requires a record Source and exactly one carrier path per Source field. Each
path is nonempty, ordinal-valid, unique within that window, and exact-type
compatible, including inherited nullability. Windows can overlap other windows
and containing tiles, including prefix overlap, but one exact source QOV cannot
have two windows. Current cannot be a Source. Two windows cannot reuse one
correlation with different types. A null-supplying window requires a nullable
Source type and row-level match presence.

Raw equality compares carrier, tiles, and the unordered exact-source window
map, including mode, paths, types, and null-supplying bits. Equality under an
alias map translates carrier/source correlations and compares everything else.
The cached hash includes every non-alias discriminator and hashes windows as a
sorted multiset, but omits correlation bytes. Therefore mapped alpha-renames
hash equally; differently named raw layouts may lawfully collide.

The purpose APIs exact-recognize both layout and QOV before reading private
state. `LayoutProvides` answers exact source coverage; `LayoutSatisfies` applies
it to the `RequiredBindings.WindowSources` collected from a source-bound graph.
Hostile,
typed-nil, current-as-source, and same-correlation/different-type inputs return
stable errors. The executor and physical selector never inspect private window
slices directly.

### 5.3 Binding contract

A record-typed QOV is always bound to its whole typed object, never an
already-consumed field. A scalar QOV binds its scalar directly and is used
without a FieldValue.

```go
type PhysicalOrdinalCarrier interface {
    OrdinalLayout() OrdinalLayout
    isPhysicalOrdinalCarrierView()
}

type RecordOrdinalCarrier interface {
    PhysicalOrdinalCarrier
    OrdinalRow
}

type ScalarOrdinalCarrier interface {
    PhysicalOrdinalCarrier
    Scalar() any
}

type WindowMatchPresence interface {
    MatchState(QuantifiedObjectValue) (matched bool, known bool)
}

type QuantifiedObjectBinder interface {
    GetQuantifiedBinding(
        QuantifiedObjectValue,
    ) (value any, present bool, err error)
}

type TypedEdgeDeclaration interface {
    QOV() QuantifiedObjectValue
    isTypedEdgeDeclarationView()
}

type TypedExternalDeclaration interface {
    QOV() QuantifiedObjectValue
    isTypedExternalDeclarationView()
}

type TypedEdgeBinding interface {
    Declaration() TypedEdgeDeclaration
    isTypedEdgeBindingView()
}

type BindingOrigin uint8

const (
    BindingOriginInvalid BindingOrigin = iota
    BindingOriginCurrent
    BindingOriginEdge
    BindingOriginWindow
    BindingOriginExternal
)

type RequiredBindings interface {
    WindowSources() []QuantifiedObjectValue
    ValidateAgainst(OrdinalLayout) (bool, error)
    isRequiredBindingsView()
}

func CollectRequiredBindings(
    phase AdmittedOwnerValues,
    edges []TypedEdgeDeclaration,
    externals []TypedExternalDeclaration,
) (RequiredBindings, error)

func NewTypedEdgeDeclaration(
    qov QuantifiedObjectValue,
) (TypedEdgeDeclaration, error)
func NewTypedExternalDeclaration(
    qov QuantifiedObjectValue,
) (TypedExternalDeclaration, error)
func BindTypedEdge(
    declaration TypedEdgeDeclaration, wholeObject any,
) (TypedEdgeBinding, error)

func NewOrdinalLayoutBinder(
    layout OrdinalLayout,
    carrier PhysicalOrdinalCarrier,
    presence WindowMatchPresence,
    required RequiredBindings,
    edgeBindings []TypedEdgeBinding,
    externals []TypedExternalDeclaration,
    base *RowEvalContext,
) (*RowEvalContext, error)
```

`PositionalRow` (and every other record carrier) stores an immutable layout
RawEqual to the layout selected by its physical plan. In scalar mode the
carrier object is the scalar itself through a small `ScalarOrdinalCarrier` and
has no ordinal access; the factory requires the carrier subinterface matching
`layout.CarrierKind()`. `NewOrdinalLayoutBinder` exact-recognizes the
layout and uses an executor-owned exhaustive concrete carrier switch
(`PositionalRow`, scalar carrier, protobuf/query-result adapter, and each
buffer decoder listed in the carrier manifest) before invoking a method. An
unknown, embedded, or typed-nil carrier is `LayoutForeignValue`; the public
interface is not admission. The optional presence provider is admitted by the
same executor-owned registry; nil is legal only when the layout contains no
null-supplying window. It overlays local bindings onto a copy of the existing full
`RowEvalContext`, preserving parameters, clock, scalar subqueries, constants,
descriptor/type repository, and declared external correlations.

The binder also exact-recognizes the owner-produced `RequiredBindings` and
requires its current, edge, external, and local-window declarations to agree
exactly with the supplied inputs and layout. Thus selection preflight and
runtime binding cannot classify the same QOV differently or silently attach an
extra/stale origin.

Planning-time edge/external declarations are exact-private QOV-only values; no
row datum exists during property selection. At execution, `BindTypedEdge`
combines one admitted declaration with one whole object (including present nil).
An external declaration delegates value lookup to the base context. All views
are copied/exact-recognized, and hostile embedded views are rejected before use.

The implementation map is keyed by `CorrelationIdentifier`, matching Cascades'
one-variable/one-binding model. Each entry also stores the one admitted exact
QOV/type; lookup receives the QOV view and validates its type and, for local
current, its exact owner handle before returning data. One evaluation phase
rejects two QOVs with the same correlation and different exact types. Separate
parent/child phases may each bind current at different types.

Binding origins are disjoint and validated in this order:

1. the phase's exact owner-current handle;
2. parent quantifier-edge bindings, which bind that parent's alias to the
   selected child's whole output without modifying the child layout;
3. locally materialized source windows from the phase layout; and
4. explicitly declared external correlations delegated to the base context.

A duplicate correlation across origins is an error, not precedence or shadowing.
Only local window sources participate in `LayoutSatisfies`; edge aliases and
externals do not. The same child plan can therefore be ranged over by two parent
aliases, and a correlated inner can read an explicitly declared outer binding,
without changing the child's provided layout.

`CollectRequiredBindings` walks the phase's result, every frozen condition,
property Value, and SARG as one closure. It obtains the exact current handle
from the admitted phase, classifies current/edge/external roots immediately,
and records every remaining noncurrent local root as a provisional window
requirement. Missing declarations, duplicate origins, and type conflicts error.
`RequiredBindings.ValidateAgainst(layout)` proves each provisional root has one
exact window, rejects any undeclared extra/collision, and seals the final
classification consumed by the binder. A single-Value overload does not exist;
two predicates cannot accidentally omit each other's source. The older
`CollectRequiredQOVs` helper is deleted.

For the carrier QOV, the binder returns the whole row. An object window reads
`ObjectPath`. A field window returns an immutable ordinal-row view whose field
`i` reads `FieldPaths[i]`.

Binding distinguishes three states:

- `(present=true, value=nil)` is bound SQL NULL;
- `(present=true, value=non-nil)` is a bound object, even if every field is
  NULL; and
- `present=false` is `UnboundCorrelation`, never SQL NULL.

Present nil is accepted only when that exact QOV type is nullable. A nil
carrier/current/edge/external binding for a nonnullable QOV is
`LayoutNullabilityMismatch` before any FV evaluates. Conversely every
NullOnEmpty/null-supplying edge is widened in its QOV declaration before layout
construction; runtime never fixes a bad declaration by blanket-nullifying it.

For null-supplying windows, every row carries known match presence. Unknown
presence is malformed runtime; unmatched is the present-nil case. Layout
non-equality, short rows, invalid nested carriers, and address/type
contradictions are loud internal errors.

QOV evaluation uses only `GetQuantifiedBinding`. Tagged current is explicitly
bound like any other QOV. Both the bare `OrdinalRow` and generic
`RowEvalContext.Positional` fallbacks are removed. The legacy
`GetCorrelationBinding(CorrelationIdentifier)` surface is migrated through an
18-occurrence producer/reader ledger and cannot serve a QOV in the end state.

### 5.4 Source-bound and carrier-bound addressing

There are exactly two program modes and no double-addressed access:

```go
type OrdinalAddressingMode uint8

const (
    OrdinalAddressingInvalid OrdinalAddressingMode = iota
    OrdinalAddressingSourceBound
    OrdinalAddressingCarrierBound
)

func ReanchorFieldValue(
    fv FieldValue,
    target QuantifiedObjectValue,
    layout OrdinalLayout,
) (FieldValue, error)
```

In source-bound mode, each noncurrent `FV(QOV(A), p)` stays source-relative. The binder
materializes whole `A` from its layout window and FieldValue applies all of
`p`. Direct fields rooted at that phase's exact current QOV are legal in either
mode and apply only their current-relative path; they do not make a source-bound
program a hybrid. In carrier-bound mode, every noncurrent local source access
has been reanchored: reanchor changes the root to layout current and
maps `p` to a complete carrier-relative path; runtime binds only current. A
program must never bind `A` through a window while also applying the mapped
carrier prefix. Thus one program is source-bound iff it retains at least one
local window source; it is carrier-bound iff all local accesses use current.
Edge and external QOVs are orthogonal origins, not addressing modes.

Reanchor exact-recognizes the FieldValue and target, flattens any legacy chain
inner-to-outer, and requires target to be the exact owner handle selected as the
layout carrier. If root and target are the **same exact QOV allocation**, it
returns the original pointer. A semantically RawEqual but allocation-distinct
current is rebuilt onto target; it is never a no-op. Otherwise reanchor finds
the unique exact source window. Object mode maps `ObjectPath + sourcePath`; field mode maps
`FieldPaths[sourcePath[0]] + sourcePath[1:]`. It resolves the mapped path
against target's exact type and requires exact result type/nullability equality.
It never uses display name, type coincidence, or `start + ordinal` arithmetic.

Missing/ambiguous source mapping is physical incompatibility during selection;
wrong target, malformed admitted input, invalid mapped path, or result mismatch
is a fatal planning error. No partial result is returned.

### 5.5 Physical-property propagation and identity

Each physical plan states immutable required input layouts, evaluation layouts,
addressing mode, and provided output layout. A source-bound program requires a
provided layout which binds every exact source root it reads; structurally
different satisfying layouts may compete. A carrier-bound program requires
raw equality with the layout used to reanchor it.

The following property/specification views and their exact-private concrete
implementations live in `plan/plans`; layouts and QOVs remain in `values`.
Root `cascades` consumes the views but does not implement their private markers.

```go
// package plans
type OrdinalPhysicalProperties interface {
    RequiredInputLayouts() []OrdinalLayoutRequirement
    EvaluationPrograms() []OrdinalEvaluationProgram
    ProvidedOutputLayout() OrdinalLayout
    isOrdinalPhysicalPropertiesView()
}

func AsOrdinalPhysicalProperties(any) (OrdinalPhysicalProperties, bool)

type OrdinalEvaluationProgram interface {
    Layout() OrdinalLayout
    RequiredBindings() RequiredBindings
    AddressingMode() OrdinalAddressingMode
    isOrdinalEvaluationProgramView()
}

type OrdinalBufferNormalizationSpec interface {
    isOrdinalBufferNormalizationSpec()
}

type OrdinalLayoutRequirement interface {
    SatisfiedBy(OrdinalLayout) (bool, error)
    isOrdinalLayoutRequirementView()
}

func RequireSources(
    []QuantifiedObjectValue,
) (OrdinalLayoutRequirement, error)
func RequireExactLayout(
    OrdinalLayout,
) (OrdinalLayoutRequirement, error)

// package cascades
func TranslatePhaseRoot(
    value Value,
    source QuantifiedObjectValue,
    target QuantifiedObjectValue,
) (Value, error)
func NormalizeOrdinalCarrier(
    plans.OrdinalBufferNormalizationSpec,
) (complete plans.RecordQueryPlan, error)
```

The returned slices are defensive; evaluation programs and requirements are
private immutable values. Required input requirements are ordered parallel to
the owner's quantifier/input edges. A join, aggregate, or conditional owner may
have several evaluation programs, each pairing exactly one evaluation layout,
binding-origin manifest, and addressing mode; there is no plan-wide mode which
can drift from a particular predicate/result program.
Source requirements call `LayoutSatisfies`; carrier requirements call
`RawEqual`. The selector consumes only this API, never layout internals.

`TranslatePhaseRoot` recursively and atomically replaces only the exact source
handle, requires the target's exact semantic **and ordinal object shape** to
equal the source, rebuilds every affected FV with the unchanged source-relative
path, and returns no graph on error. It does not map a physical carrier prefix;
that is exclusively `ReanchorFieldValue`. Current↔noncurrent translation is
legal only through this API; it never enters an `AliasMap`. `NormalizeOrdinalCarrier`
requires the selected input plan/layout, downstream programs, expected external
result type, and one exact buffer-owner kind in a sealed spec. Only
purpose-specific plan-package factories can build a sort, recursive/temp-table,
aggregate-state, union, or other enumerated spec. The private helper constructs
the pre-buffer current-only adapter, the actual buffer owner, and any required
post-buffer restore projection atomically, returning **one complete plan**.
There are no separately exposable halves, no callback to an unbuilt child, and
no way to mix a pre-buffer adapter with another buffer instance.

Child alternatives are filtered for layout compatibility **before** cost
comparison. Layout-dependent Values are finalized only after selecting the
concrete child, or the parent retains an exact required layout. A cheaper
incompatible child can never be paired with Values prepared for another
alternative.

Every behavior-affecting physical layout and addressing mode joins physical
plan equality **under the final child alias correspondence**. Raw equality is a
fast path; independently built alpha-equivalent plans use
`EqualUnderAliases`. Hashing uses `AliasFreeHash`. Logical Values, logical
expressions, `Type.Equals`, and Reference **group/result-type agreement** never
read a layout. Physical member equality/hash does read the layout, so
`Reference.Insert` retains two otherwise-identical physical members whose
layouts differ while keeping them in one logical group.

Alias-bearing node information is never compared before the aliases on which it
depends exist. `SemanticEquals` and `MemoEqual` follow Java's dependency-aware
protocol (`Reference.java:790-829`, `Quantifiers.java:226-283`). They first
apply alias-invariant hash/type/arity guards. For ordered children, the paired
quantifiers' exact edge attributes -- quantifier kind, `NullOnEmpty`, and
`StrictSingle` -- are compared before recursion. Child `i` is recursively
compared under the map accumulated from
already-proven children `0..i-1`; only after that succeeds is the paired
quantifier alias for `i` added. For children-as-set, matching is
dependency-topological: a candidate pair is eligible only when every
correlation that child requires is already bound and the candidate's exact
kind/`NullOnEmpty`/`StrictSingle` tuple matches; its child is compared under
that map, and its alias is then added before the next dependency. Backtracking
tries another eligible pair on failure. These edge attributes participate in
the deterministic pre-partition and alias-free semantic hash. A future or
unproven sibling alias is never made available to recursive comparison. Once
every child is proven, result Values, predicates, layouts, addressing mode,
and other node information are compared under the completed map. Ordered
physical plans have one positional correspondence. No call site independently
compares alias-bearing node information before this protocol.

The current `MaxPermutationChildren` positional fallback is deleted: it is a
correctness false-negative for a legal permutation. Commutative matching uses
deterministic hash/type/edge partitions plus memoized bipartite backtracking;
it remains complete for every arity. Tests cover separately minted equivalent
physical plans, swapped logical Select children with equal output, swapped
output inequality, and adversarial 9- and 12-child permutations under the
performance budget.

Selection substitutes the chosen child's admitted layout handle before final
reanchor. Runtime accepts that handle or an independently built `RawEqual`
layout; pointer equality is only a fast path. Any behavior-changing difference
is a loud mismatch.

No per-row match-presence bit, hidden slot, or schema field is added to a
continuation. Instead, every owner which serializes/buffers rows inserts a
physical normalization boundary first. `NormalizeOrdinalCarrier` materializes
a canonical current-only carrier, checked-reanchors every downstream source
access to that current, and emits a trivial flat/nested layout with **no source
windows or match-presence state**. Its continuation-visible type, field order,
and ordinary payload values are byte-for-byte the pre-RFC plan contract.

If downstream semantics consume a whole null-supplying source object, that
object must already occupy an ordinary semantic result slot; the normalizer
stores a nonnil nested row for a matched all-NULL object and nil for an
unmatched object in that same slot. If it is not an output slot, all consumers
of that source must be pulled before the buffer and only their existing
semantic results may cross it. A boundary which would need an extra provenance
slot is not a compatible physical alternative. The normalizer never appends a
hidden field, and the one complete plan returned by
`NormalizeOrdinalCarrier` exposes exactly the original result type.

The generated continuation manifest covers sort, recursive/temp-table, join,
flat-map, aggregate partial state, union and every other row buffer. It states
whether a row is normalized or is re-read/recomputed on resume. Existing opaque
payload schemas and legacy tokens therefore remain unchanged. On decode, the
selected plan supplies the authoritative exact expected type; the decoder
checks the token's existing slot/name structure and validates each materialized
datum against that expected type (including nil only for nullable slots) before
reattaching the canonical layout. It does not claim a type fingerprint exists
inside legacy bytes. Because
the normalized form has no source provenance, two equal canonical schemas have
identical binding behavior. Matched-all-NULL and unmatched are distinguished
before normalization in the existing semantic source-object slot or are fully
consumed before the buffer. Golden pre-RFC tokens resume through every buffering
class, and tests round-trip both states and mutate the normalization boundary to
prove the wire-neutral rule.

## 6. Exact aggregate result contract

Exact QOV admission makes the current aggregate placeholder impossible. The
following changes land atomically; none is a general aggregation redesign.

### 6.1 Immutable aggregate specification

`AggregateSpec`, `AggregateValue`, and `AggregateOutputDescriptor` become
exact-private immutable implementations exposed only through read views and
purpose factories. Construction copies labels/spec slices and freezes each
grouping/operand Value through an exhaustive aggregate-local per-kind
copy-or-reject registry. Returned result Values and getters cannot be asserted
back to mutable RC/Aggregate structs. Hostile, mutable-payload, and unsupported
open Values are rejected.

`OperandIntType` is deleted. Preparation
derives a private execution lane from the exact operand type, so accumulator
overflow behavior and declared result type cannot drift.

The operator contract is:

| operator | operand | exact result type |
| --- | --- | --- |
| `COUNT(*)` | absent | nullable LONG |
| `COUNT(expr)` | present, any exact type | nullable LONG |
| `AVG(expr)` | present numeric | nullable DOUBLE |
| `SUM(expr)` | present numeric | nullable operand lane |
| `MIN/MAX(expr)` | present supported numeric lane | nullable operand lane |

```go
type AggregateSpecView interface {
    Function() AggregateFunction
    Operand() (Value, bool)
    ResultType() Type
    isAggregateSpecView()
}

type AggregateOutputDescriptor interface {
    ResultValue() Value
    ResultType() Type
    SlotCount() int
    SlotValue(int) (Value, bool)
    isAggregateOutputDescriptorView()
}

func NewAggregateSpec(
    function AggregateFunction,
    operand Value, // nil only for COUNT(*)
    displayAlias string,
) (AggregateSpecView, error)
func NewAggregateOutputDescriptor(
    grouping []Value,
    aggregates []AggregateSpecView,
) (AggregateOutputDescriptor, error)
func AsAggregateSpec(AggregateSpecView) (AggregateSpecView, bool)
func AsAggregateOutputDescriptor(
    AggregateOutputDescriptor,
) (AggregateOutputDescriptor, bool)
```

`COUNT(1)` is `COUNT(expr)` and counts every row; `COUNT(NULL)` contains one
exact `NullLiteralValue` operand of concrete TypeCode NULL and counts zero. It
is distinct in equality/hash from the absent operand of COUNT star. TypeCode
NULL is accepted only for this/literal-owning context, never as a QOV or FV
record root. Only syntactic COUNT star has no operand. Numeric
and lane validation happens during preparation, including when runtime input is
empty, so string/enum/nonnumeric SUM/AVG/MIN/MAX never hide behind zero rows.

Aggregate Value equality/hash includes function and semantic operand. Display
alias and rendered operand name remain output metadata. The derived execution
lane is validated from the operand and is not a second caller-settable identity.

### 6.2 One output descriptor

One immutable `AggregateOutputDescriptor` is derived from the ordered grouping
Values and aggregate specs:

```text
[ grouping key 0, ..., grouping key n-1,
  aggregate result 0, ..., aggregate result m-1 ]
```

Each slot gets the exact corresponding Value type and optional display label.
No map keyed by rendered name chooses a slot.

Logical `GroupByExpression.GetResultValue()` stores and returns a stable
exact-private descriptor-backed symbolic record Value over that order. It
cannot be downcast to and mutate an ordinary RC. It no longer reports the
input QOV. Repeated structurally equal grouping Values occupy distinct ordered
slots; translating a downstream Value to an output slot requires one unique
semantic match or returns zero/multiple-match error.

Physical streaming aggregation stores the same descriptor plus a stable
`completeResultValue`. Preparation allocates private grouping-state and
aggregate-state aliases and builds the complete result from exact state slots.
During input, the executor explicitly binds the input QOV and evaluates keys and
operands. During finalization, it binds both state aliases and evaluates
`completeResultValue`. There is no ambient current fallback and no fresh result
QOV per getter call.

`OutputRecordType`, `GetResultType`, logical result Value, physical result Value,
and emitted ordinal row all derive from the one descriptor. Returned grouping,
aggregate, field, and name slices are defensive copies.

### 6.3 Empty input

Bare streaming aggregation emits zero rows when it observes no input, grouped
or ungrouped, matching Java. SQL's global-empty one-row behavior is represented
by an explicit default-on-empty owner which produces COUNT zero and nullable
numeric NULLs. It is not an invisible branch inside the accumulator.

The default program is itself typed and evaluated with explicit parameter,
outer-correlation, scalar-subquery, clock, and descriptor context. Continuation
resume preserves whether the child or default arm emitted the row.

## 7. Stable failures and checked rewriting

### 7.1 Error taxonomy

All new construction, resolution, layout, rewrite, and rule errors implement a
stable `Code()`. Factories never panic, return a partial object, substitute
ordinal zero, or return the old node after a failed rebuild.

```go
type ResolutionErrorCode uint16

const (
    TypeNil ResolutionErrorCode = iota + 1
    TypeTypedNil
    TypeCycle
    TypeUnresolved
    TypeErased
    TypeMalformedCode
    TypeMalformedOrdinal
    CorrelationZero
    CorrelationForeignValue
    CorrelationKindMismatch
    CorrelationTypeConflict
    FlowedTypeUnavailable
    FlowedTypeDisagreement
    FieldNilChild
    FieldUnsupportedChild
    FieldEmptyPath
    FieldInvalidRequest
    FieldNegativeOrdinal
    FieldOutOfRange
    FieldNonRecord
    FieldUnknownName
    FieldAmbiguousName
    FieldNameOrdinalMismatch
    FieldIncompatibleRoot
    LayoutForeignValue
    LayoutNonRecordCarrier
    LayoutInvalidTile
    LayoutTileGap
    LayoutTileOverlap
    LayoutInvalidPath
    LayoutInvalidWindow
    LayoutDuplicateSource
    LayoutTypeMismatch
    LayoutNullabilityMismatch
    LayoutCarrierMismatch
    LayoutSourceNotProvided
    LayoutPresenceMissing
    LayoutRuntimeShape
    LayoutNormalizationUnsupported
    LayoutNormalizationTypeMismatch
    ReanchorInvalidValue
    ReanchorTargetMismatch
    ReanchorUnmappedSource
    ReanchorInvalidMappedPath
    ReanchorResultTypeMismatch
    UnboundCorrelation
    AggregateInvalidFunction
    AggregateMissingOperand
    AggregateUnexpectedOperand
    AggregateUnsupportedOperand
    AggregateTypeMismatch
    AggregateLaneMismatch
    AggregateOutputNoMatch
    AggregateOutputAmbiguous
    RewriteNilReplacement
    RewriteValueCycle
    RewriteInvalidCallbackOutput
    RewriteInvalidArity
    RewriteNonComparableNode
    RewriteInvalidTranslation
    UnsupportedValueRebuild
    RuleDirectMutation
    MemoUnsupportedExpression
    MemoResultTypeMismatch
    MemoBatchConflict
    MemoMissingRelationWrapper
    MemoDoubleRelationWrapper
    MemoEmptyReference
    MemoInvalidHandle
    MemoProvisionalEscape
    MemoReferenceCycle
    MemoTransactionClosed
    MemoReentrantTransaction
)
```

| code family | representative conditions | policy |
| --- | --- | --- |
| `TypeInvalid` | nil/typed-nil, cycle, unresolved/erased, malformed ordinal | fatal construction/planning |
| `CorrelationInvalid` | zero/hostile QOV, unavailable/disagreeing flowed type | fatal; optional candidate may decline only before QOV construction |
| `FieldInvalidRequest` | empty step state, nil child, empty path, negative ordinal | fatal |
| `FieldResolution` | unknown/ambiguous name, nonrecord, out-of-range, name/ordinal mismatch | SQL error; documented candidate preflight may decline |
| `FieldIncompatibleRoot` | valid path cannot rebuild on a replacement | optional rewrite no-match or fatal committed rewrite |
| `LayoutInvalid` | bad tile/window/path/type/nullability/duplicate source | fatal construction |
| `LayoutIncompatible` | selected child does not provide required source/layout | decline that physical alternative |
| `ReanchorInvalid` | wrong target, invalid mapped path, result mismatch | fatal planning |
| `UnboundCorrelation` | no exact runtime QOV binding | runtime internal/SQL internal |
| `RuntimeOrdinalShape` | short row, descriptor/layout disagreement, missing presence | runtime internal |
| `Aggregate...` | invalid op/operand/lane/type or mutable unsupported operand | semantic/internal preparation error |
| `Rewrite...` | nil output, cycle, hostile/non-comparable node, arity, translation | fatal planning |
| `UnsupportedValueRebuild` | changed child under an unknown/open Value kind | fatal planning |
| `Memo...` | unknown expression, relation-wrapper/type disagreement, empty/invalid/provisional handle, reference cycle, or transaction lifecycle/reentry | fatal planning, no mutation |
| `RuleDirectMutation` | registered rule bypasses its private invocation draft | static-gate failure and fatal planning backstop |

The concrete enum includes distinct codes for every factory-table mutation:
negative/out-of-range, name miss/ambiguity, tile gap/overlap, duplicate source,
type/nullability mismatch, carrier mismatch, unmapped source, missing presence,
and runtime shape. Tests assert the exact code, not an error substring.

The same field miss has boundary-specific disposition through two APIs:
`ResolveFieldAccess` returns a semantic error; `TryResolveFieldAccess` returns
`applicable=false` only for the documented optional-candidate codes. Callers do
not duplicate resolution or infer policy from text.

The optional list is exactly `FieldUnknownName`, `FieldAmbiguousName`,
`FieldNonRecord`, `FieldOutOfRange`, `LayoutSourceNotProvided`, and
`ReanchorUnmappedSource`, and only at the candidate/physical-selector sites in
the migration ledger. The four optional rewrite boundaries are named exactly:
`FieldIncompatibleRoot` may mean no-match only at
`max_match_map.go:473` and `rule_push_filter_through_fetch.go:336`; the
candidate-ordering preflight at `match_candidate_index.go:209` and fan-out
preflight at `index_expansion.go:600` may decline only the resolution/layout
codes in the preceding list. Every other code or site is fatal. The same
fixtures run each miss once through SQL resolution and once through candidate
preflight to pin the different disposition.

### 7.2 Total Value and predicate rewrites

```go
type RewriteValueFunc func(Value) (Value, error)

type TypedTranslation interface {
    Source() QuantifiedObjectValue
    Target() QuantifiedObjectValue
    isTypedTranslationView()
}

func RewriteValue(Value, RewriteValueFunc) (Value, error)
func RewriteLeavesMaybe(Value, RewriteValueFunc) (Value, error)
func RewriteLeavesOnceMaybe(Value, RewriteValueFunc) (Value, error)
func RewritePredicate(
    predicates.QueryPredicate, RewriteValueFunc,
) (predicates.QueryPredicate, error)
func NewTypedTranslation(
    source QuantifiedObjectValue,
    target QuantifiedObjectValue,
) (TypedTranslation, error)
func RebaseValue(
    Value, []TypedTranslation,
) (Value, error)
```

`RewriteValue` is a post-order copy-on-write walk. It rebuilds children through
the exact private FieldValue/QOV path or a checked per-kind non-field rebuild,
then invokes the callback exactly once on the rebuilt node. A replacement is
not recursively revisited. Nil replacement is an error; deletion uses a
purpose-specific API. First error wins, no callback observes a partially
rebuilt parent, true no-op preserves pointer identity, and cycles are rejected.

The traversal does not use open `Value` interface values as Go map keys. One
exhaustive exact-kind registry returns a stable pointer node identity and
checked rebuilder for every admitted repository Value. A non-pointer,
non-comparable, hostile, or unknown external implementation is rejected before
identity memoization or method invocation. Shared DAG nodes are memoized by that
stable identity; the active-stack bit detects cycles.

If an open/external Value has changed children and no checked rebuilder,
`UnsupportedValueRebuild` is returned. It never silently retains old children.
Callback outputs are exact-recognized again before admission. FieldValue rebuild
re-resolves the complete path, performs checked FV fusion or RC collapse, and
never goes through generic `WithChildren`.

Predicate rebuilding is equally fallible, including existential and external
predicate composites. Both TranslationMap protocols and every callback return
errors. Raw `AliasMap` is not a rewrite authorization. A fallible
`NewTypedTranslation(sourceQOV,targetQOV)` exact-recognizes both QOVs, proves
equal semantic type and permitted noncurrent alpha-rebase, then produces a
sealed token. It rejects zero, hostile, current↔noncurrent, and type-changing
pairs. `RebaseValue` is fallible and accepts only those tokens;
current-to-concrete and layout-changing work uses `TranslatePhaseRoot`/checked
reanchor. The callback/output graph is admitted atomically or not returned.

### 7.3 Prepared, atomic rule commits

The three call families—predicate `RuleCall`, `ExpressionRuleCall`, and
`ImplementationRuleCall`—gain `Fail(error)` and `Err()`. Standalone and
production `FireRule`, `FireExpressionRule`, `FireImplementationRule`, and
`Simplify` return errors to their callers.

```go
func FireRule(CascadesRule, any) ([]any, error)
func Simplify(
    predicates.QueryPredicate, []CascadesRule,
) (predicates.QueryPredicate, error)
func FireExpressionRule(
    ExpressionRule, *expressions.Reference,
) ([]RelationalExpression, error)
func FireExpressionRuleWithMemo(
    ExpressionRule, *expressions.Reference, PlanContext, *Memo,
) ([]RelationalExpression, error)
func FireImplementationRule(
    ImplementationRule, *expressions.Reference, ...*ConstraintMap,
) ([]RelationalExpression, error)
```

The production task-stack counterparts gain the same error result through their
existing planner-run channel. A caller manifest covers standalone, recursive
simplifier, task, and test adapters; no wrapper may discard the error.

Staged `Yield` returns no insertion boolean; the result does not exist until
commit, and no production caller currently branches on it. A source gate proves
zero return-value use before changing the signature. Post-commit insertion
statistics belong to the driver, not the rule body.

Mutation-capable call fields (`Reference`, `Constraints`, Memo) become private;
rules receive read-only accessors. Production planner registries are closed over
the checked repository rule manifest, so an external implementation cannot be
injected into a planning phase. The transitive SSA gate covers every registered
rule and helper and rejects direct `Reference.Insert`, ConstraintMap mutation,
Memo mutation, or mutation of a Reference obtained through bindings. All
production effects must pass through the instrumented RuleCall methods.

All rule-call mutation methods append immutable intents to a private invocation
draft. This includes `Yield`, `MemoizeExpression`, `MemoizeFinalExpression`,
`InsertReExploring`, `PushConstraint`, partial-match insertion, task scheduling,
property mutation, Reference absorption, and memo merge. A staged memoization
returns an invocation-owned `*expressions.Reference` backed by a provisional
memo-store group, not shared state; later staged expressions and existing
Quantifier factories use it unchanged. No rule-body call changes shared planner
state.

```go
type RuleInvocationView interface {
    Err() error
    isRuleInvocationView()
}

type InsertionResult interface {
    Inserted() bool
    Member() RelationalExpression
    isInsertionResultView()
}

type RuleCommitResult interface {
    Insertions() []InsertionResult
    isRuleCommitResultView()
}

func (m *Memo) CommitInvocation(
    RuleInvocationView,
) (RuleCommitResult, error)
```

The invocation/result interfaces are capability views, not open admission.
Root `cascades` exact-switches before use; embedded and nil-embedded impostors
are rejected without method invocation. `CommitInvocation` is the sole publish
entry and returns only after memostore's locked transaction succeeds.

`expressions.Reference` becomes an immutable wrapper around one sealed
`internal/memostore.GroupHandle`; all public mutation methods disappear.
`GetRangesOver()` can therefore remain `*Reference`, so Quantifier structure
remains recognizable, but **every** Reference member/result/topology read is
fallible and returns a defensive view. There is no `(Type, bool)` reader which
can conflate empty, invalidated, provisional, and hostile handles. The wrapper
exposes only fallible reads plus its opaque group reference; it never admits an
expression.
`expressions.NewReferenceHandle(memostore.GroupHandle)` is the sole low-level
wrapper, and a source gate permits production calls only from root `cascades`.
Neither `expressions` nor `memostore` performs a physical-plan type switch.

```go
func (r *Reference) ResultType() (values.Type, error)
func (r *Reference) Members() ([]RelationalExpression, error)
func (r *Reference) FinalMembers() ([]RelationalExpression, error)
func (r *Reference) Canonical() (*Reference, error)
func (r *Reference) Key() (ReferenceKey, error)
func SameReference(*Reference, *Reference) (bool, error)
```

`expressions` keeps one canonical wrapper interner keyed by the canonical
store group reference. `Canonical` follows forwarding and returns that stable
wrapper; `ReferenceKey` is the immutable canonical group identity used by
visited, parent, and index maps. Semantic raw pointer equality and
`map[*Reference]` keys migrate to `SameReference`/`ReferenceKey`; pointer
identity remains only an explicitly named diagnostic identity. A typed
AST/go-types census and gate cover every Reference reader, pointer comparison,
and map or set key.

A staged handle names a provisional group owned by one invocation draft.
`MemoizeExpression` and `MemoizeFinalExpression` become fallible pure-admission
methods: root exact-recognizes the proposed member, freezes/validates its result,
records exact `RELATION<ResultValue.Type()>`, and creates an invocation-local
provisional handle before returning. A later staged parent can therefore build
a typed Quantifier/QOV immediately. Adding another provisional member checks
the same relation type; whole-draft commit still revalidates every member and
cross-group constraint. On successful commit the store atomically resolves the
same immutable provisional group token to either a new or deduplicated committed
group, so every two-level/diamond staged parent already points at the committed
canonical group. A live provisional handle may expose its already-admitted
exact result type and invocation-local members so a staged parent can be
constructed; Quantifier construction is representational and may retain it.
At every staged-parent admission, root recursively verifies that every
provisional child belongs to the **same exact draft capability** as the
RuleCall. Committed admission and every executor boundary reject provisional
children. On abort the draft capability closes; unresolved provisional reads
return `MemoInvalidHandle`, no member is published, and cross-draft use returns
`MemoProvisionalEscape`. The closed-rule escape gate rejects storing one in
globals or rule state and returning it except inside another same-draft staged
intent. A typed census covers every Quantifier factory and every Reference
reader, with same-draft, wrong-draft, read-after-abort, dedup-to-existing,
forwarded-wrapper, and canonical-key tests.

```go
func (c *ExpressionRuleCall) MemoizeExpression(
    RelationalExpression,
) (*expressions.Reference, error)
func (c *ImplementationRuleCall) MemoizeFinalExpression(
    RelationalExpression,
) (*expressions.Reference, error)
```

All overloads/from-other variants follow the same result. Rule bodies call
`Fail(err)` and return; ignored-error gates cover every site. A red test performs
memoize-child → typed parent Quantifier/result construction → staged Yield.

The driver runs the rule to completion, checks `Err()`, freezes every proposed
member through the root registry, and invokes the single memostore transaction
defined below. Its callback resolves pending References, validates the complete
cross-group draft, precomputes equality/dedup decisions against the locked
snapshot, and fills one method-free delta for members, groups, indexes,
constraints, properties, epochs, partial matches, and tasks. The closed-rule
SSA gate rejects direct mutation APIs anywhere in every
registered rule's transitive callees, including References obtained from
bindings. Package boundaries exact-recognize every read view and never invoke
hostile methods. Tests inject failure after each staged intent
and at each whole-draft preflight step, including valid-then-invalid and
invalid-then-valid yields, late invalid absorb/merge, cross-draft duplicates,
and pending-reference cycles. Members, memo topology/indexes, epochs,
constraints, properties, partial matches, tasks, and driver statistics remain
byte-for-byte unchanged on every failure.

### 7.4 Reference admission before hashing

Exact result typing is established before any memo hash/equality or mutation.
Reference construction, exploratory/final insertion, absorption, re-exploring
insertion, and memoization become fallible. The checked insertion protocol is:

1. require a nonnil repository-supported expression and stable admitted result
   Value;
2. recursively reject unresolved QOV/current-owner violations in that result;
3. compute exactly `RELATION<ResultValue.Type()>`;
4. compare that relation type with the Reference's stored exact relation type
   (or set it for the first member); and only then
5. compute member hash/equality and mutate membership/generation/task state.

`RelationalExpression` gains a required non-mutating `ValidateForMemo() error`,
invoked by every Reference/Memo ingress. A compile manifest covers every
repository implementation. Root `cascades`, which can import both logical
expressions and physical plans, first exact-switches on the closed all-expression
registry **without invoking a method**, then performs the generic
admitted-result and exact result-type checks above before hashing. The
`expressions` package does not attempt an exhaustive plan switch. Unknown,
embedded, nil-embedded, or
typed-nil implementations return `MemoUnsupportedExpression`; their validation,
result, equality, and hash methods are never called. `ValidateForMemo` is a
per-known-kind helper, not an open trust boundary. Purpose constructors freeze
all state of manifested implementations, and production planner registries
contain only those implementations.

Root `cascades` converts admitted expressions to method-free immutable member
records. `cascades/internal/memostore` imports no `values`, `expressions`, or
`plans` package and invokes no opaque payload method. Its cross-package API is
storage-only and complete enough for root to read one locked snapshot and build
one delta:

```go
// package cascades/internal/memostore
type GroupID uint64
type MemberID uint64
type IntentID uint32
type MemberSet uint8
type HandleState uint8

const (
    ExploratoryMembers MemberSet = iota + 1
    FinalMembers
)
const (
    HandleDetached HandleState = iota + 1
    HandleProvisional
    HandleCommitted
    HandleInvalid
)

type TypeRecord struct {
    Canonical string // collision-free canonical exact RELATION type bytes
    Payload   any    // opaque root/values-owned immutable exact-type handle
}
type MemberRecord struct {
    ID            MemberID
    Payload       any // opaque root-admitted immutable expression snapshot
    Type          TypeRecord
    AliasFreeHash uint64 // candidate index only, never equality authority
}
type GroupRef interface { isGroupRef() }
type DraftHandle interface { isDraftHandle() }
type GroupHandle interface {
    Ref() GroupRef
    State() HandleState
    TypeRecord() (TypeRecord, bool, error)
    Snapshot() (GroupSnapshot, error)
    Canonical() (GroupHandle, error)
    isGroupHandle()
}

type GroupSnapshot struct {
    Ref, Canonical    GroupRef
    Type              TypeRecord
    HasType           bool
    Generation, Epoch uint64
    Stage, Exploration uint8
    Winner            MemberID
    HasWinner, IsLeaf bool
    Exploratory       []MemberRecord
    Final             []MemberRecord
}
type PayloadKind uint16
type PayloadRecord struct {
    Kind PayloadKind
    Canonical []byte // defensive in and out; complete in-memory encoding
}
type StateRecord struct { Group GroupRef; Key string; Payload PayloadRecord }
type TaskRecord struct { Key string; Payload PayloadRecord }
type IndexRecord struct {
    Lane uint8
    Key string
    Group GroupRef
    Member MemberID
    Payload PayloadRecord
}
type ParentEdgeRecord struct {
    Child, Parent GroupRef
    Member MemberID
    Payload PayloadRecord
}

type TxnSnapshot interface {
    Version() uint64
    Root() (GroupRef, bool)
    Groups() []GroupSnapshot
    Group(GroupRef) (GroupSnapshot, bool, error)
    Resolve(GroupRef) (GroupRef, bool, error)
    MembersByHash(
        GroupRef, MemberSet, uint64,
    ) ([]MemberRecord, error)
    Constraints(GroupRef) []StateRecord
    Properties(GroupRef) []StateRecord
    PartialMatches(GroupRef) []StateRecord
    Statistics(GroupRef) []StateRecord
    ParentEdges(GroupRef) []ParentEdgeRecord
    Index(lane uint8, key string) []IndexRecord
    Tasks() []TaskRecord
    isTxnSnapshot()
}

type StoreSnapshot interface {
    TxnSnapshot
    isStoreSnapshot()
}

type InsertRecord struct {
    Intent IntentID
    Group  GroupRef
    Set    MemberSet
    Member MemberRecord
}
type InsertionOutcome struct {
    Intent   IntentID
    Group    GroupRef
    Inserted bool
    Member   MemberRecord // incoming member or exact existing winner
}
type ResolvedGroup struct { Provisional, Committed GroupRef }

type DeltaBuilder interface {
    NewGroup(source GroupRef, TypeRecord) (GroupRef, error)
    Insert(InsertRecord) error
    Duplicate(InsertionOutcome) error
    Forward(loser, survivor GroupRef) error
    ReplaceMembers(
        GroupRef, MemberSet,
        expected []MemberID, next []MemberRecord,
    ) error
    DeleteGroup(GroupRef, expectedGeneration uint64) error
    PutConstraint(StateRecord) error
    RemoveConstraint(GroupRef, key string) error
    PutProperty(StateRecord) error
    RemoveProperty(GroupRef, key string) error
    AddPartialMatch(StateRecord) error
    RemovePartialMatch(GroupRef, key string) error
    AddStatistic(StateRecord) error
    RemoveStatistic(GroupRef, key string) error
    Enqueue(TaskRecord) error
    Dequeue(expected TaskRecord) error
    PutIndex(IndexRecord) error
    RemoveIndex(lane uint8, key string, group GroupRef, member MemberID) error
    AddParentEdge(ParentEdgeRecord) error
    RemoveParentEdge(ParentEdgeRecord) error
    SetRoot(expected GroupRef, expectedPresent bool, next GroupRef, nextPresent bool) error
    SetWinner(GroupRef, expected MemberID, expectedPresent bool, next MemberID, nextPresent bool) error
    SetLeaf(GroupRef, expected, next bool) error
    SetGeneration(GroupRef, old, next uint64) error
    SetEpoch(GroupRef, old, next uint64) error
    SetStage(GroupRef, old, next uint8) error
    SetExploration(GroupRef, old, next uint8) error
    isDeltaBuilder()
}
type CommitResult interface {
    Version() uint64
    ResolvedGroups() []ResolvedGroup
    Insertions() []InsertionOutcome
    isCommitResult()
}
type PrepareMemoDelta func(TxnSnapshot, DeltaBuilder) error

func NewDraftHandle() DraftHandle
func AbortDraft(DraftHandle) error
func NewProvisionalGroup(
    DraftHandle, local uint32, TypeRecord, MemberRecord,
) (GroupHandle, error)
func NewDetachedGroup(
    TypeRecord, []MemberRecord, []MemberRecord, stage uint8,
) (GroupHandle, error)
func NewDetachedEmptyGroup() GroupHandle
func AsGroupHandle(any) (GroupHandle, bool)
func SameGroup(GroupRef, GroupRef) bool
func NewPayloadRecord(kind PayloadKind, canonical []byte) (PayloadRecord, error)
func (s *Store) Snapshot() (StoreSnapshot, error)
func (s *Store) Transact(
    DraftHandle, PrepareMemoDelta,
) (CommitResult, error)
```

Every returned slice/record is defensive. The root-created exact type payload
is exact-recognized by `values` when `Reference.ResultType` thaws a copy;
memostore never reads it. `GroupHandle.Snapshot` is an ordinary outside-
transaction read of one immutable store-state generation. During preparation,
root memo equality is parameterized by the supplied `TxnSnapshot`: recursive
Quantifier/Reference reads use only `GroupRef` plus `snapshot.Group`/
`MembersByHash`, never ordinary `Reference`/`GroupHandle` getters. This makes
grandchild comparison non-reentrant and keeps one consistent generation.

No state/task/index/parent lane accepts raw `any`. Root's closed per-kind codec
exact-recognizes and freezes the complete lane payload without invoking hostile
methods, then emits a collision-free, self-contained **in-memory** canonical
encoding. This encoding is not a continuation, protobuf, or persisted wire
format. `NewPayloadRecord` rejects zero/unknown kinds and malformed or
noncanonical bytes and copies the byte slice; every snapshot/getter copies it
again. The corresponding root decoder is closed by kind and reconstructs only
immutable read views or stable group/member IDs already present in the same
store version. `DeltaBuilder` validates kind/canonical form before adding an
intent. The manifest covers constraint, property, partial-match, statistic,
task, index, and parent-edge payload kinds; there is no catch-all reflection
arm or pointer-bearing payload.

`Store` is the sole owner of group members, forwarding/topology, indexes,
constraints, properties, generations, epochs, partial matches, task queue, and
planner stage/exploration state, and insertion statistics. Its state is one
immutable aggregate pointer.
`ReplaceMembers` is the only member-set destructive primitive: advance-stage
promotion, prune-to-set, clearing exploratory/final members, and designation
changes are precomputed as expected-ID/next-record replacements. Winner
selection, root/leaf classification, parent indexes, global/hash indexes,
property and partial-match removal, statistics removal, and task dequeue use
the explicit delta lanes above. No mutable winner, queue, parent map, root/leaf
set, or global index remains outside `Store`. `Store.Snapshot` returns one
defensive, generation-consistent read view for diagnostics and extraction; it
never exposes the aggregate pointer or a mutable record.
`Transact` consumes one draft exactly once, serializes writers with the sole
lock, supplies one exact-private
read-only snapshot and single-use builder, calls root's callback, privately
validates every reference/type/version/cross-record relationship, builds a new
aggregate state, and publishes it with one infallible pointer swap before
unlock. `NewGroup` receives the already-existing detached/provisional group
reference, so the result's `ResolvedGroup` maps the exact token already stored
inside staged parent Quantifiers. Success resolves and closes the draft;
callback/validation failure discards the delta and invalidates its provisional
tokens. A pre-transaction rule failure calls `AbortDraft`. No separately locked
Reference, Memo, ConstraintMap, planner stack, epoch, or statistics state may
participate in a rule effect.

Builder calls only append to the private delta. After the callback returns,
every builder method returns `MemoTransactionClosed`. `GroupHandle.Snapshot`
is a lock-free load of the immutable aggregate state pointer; while a
transaction callback runs, it can only see the same pre-commit generation for
that Store and cannot deadlock. Root comparison nevertheless must use the
supplied `TxnSnapshot` so cross-store/provisional reads cannot bypass the
transaction overlay. A source gate enforces that rule. A guarded second
`Transact` while a callback is active returns `MemoReentrantTransaction`
instead of blocking; the ordinary concurrent-driver path retries after the
active writer finishes, while the source gate forbids intentional nesting. No
prepared/apply token exits `Transact`.
Root wraps `memostore.CommitResult` as `RuleCommitResult`; the two names are not
the same interface.

Snapshot lookup by group, canonical group, hash bucket, parent edge, index key,
or task key is indexed and does not scan `Groups()`. Transaction preparation is
linear in the changed records plus semantic comparisons selected from indexed
candidate buckets; apply path-copies only affected immutable maps and slices.
The complete-store snapshot and every lane participate in the same version.
Tests exercise advance-stage promotion, prune, both member clears, winner
set/clear, root/leaf replacement, parent/index add/remove, every state-record
add/remove, task enqueue/dequeue, group forwarding/deletion, and a late failure
after each operation; failure leaves every lane and the store version
byte-for-byte unchanged.

Preparation checks the complete batch against the locked snapshot and every
other batch member before mutation. Root equality/dedup owns canonical semantic
comparison; hashes are candidate indexes only. A whole-batch preflight handles
valid-then-invalid and invalid-then-valid records before the builder can publish
anything. Root-level `Memo.Insert`/`Memo.InsertFinal` return
`(inserted bool, err error)` through one-member transactions. Root-level
`Absorb`, `InsertReExploring`, Memo construction/register/merge, and all rule
drivers use corresponding complete-batch transactions and propagate errors.

Fallible root constructors `cascades.InitialOf`, `FinalOf`, and
`FinalOfAtStage` replace all expression-taking constructors in `expressions`.
They validate the whole singleton and mint a detached immutable handle. The
`expressions.NewMatchableSortExpressionFromExpr` convenience constructor is
deleted; root provides a fallible equivalent, while the remaining expressions
constructor accepts an already-admitted Quantifier. Likewise
`plans.QuantifierOverPlan(s)` is deleted: every purpose-specific plan
constructor/rebuilder accepts already-admitted physical Quantifier(s), and root
orchestration mints them through checked final-Reference factories before
calling `plans`. This is the only package-buildable direction because
`expressions` and `plans` cannot import their parent root package.

```go
// package cascades
func InitialOf(
    expressions.RelationalExpression,
) (*expressions.Reference, error)
func FinalOf(
    expressions.RelationalExpression,
) (*expressions.Reference, error)
func FinalOfAtStage(
    expressions.RelationalExpression, expressions.PlannerStage,
) (*expressions.Reference, error)
func QuantifierOverPlan(
    plans.RecordQueryPlan,
) (expressions.Quantifier, error)
func QuantifiersOverPlans(
    []plans.RecordQueryPlan,
) ([]expressions.Quantifier, error)
func NewMatchableSortExpressionFromExpr(
    []values.CorrelationIdentifier, bool,
    expressions.RelationalExpression,
) (*expressions.MatchableSortExpression, error)
```

The generic root Quantifier helpers are orchestration conveniences only; every
`plans` constructor's actual input is the admitted Quantifier/Reference, so no
infallible child-plan overload survives below root.

The authoritative Go AST/go-types census is 67 production call expressions in
28 files: `InitialOf=54`, `FinalOf=7`, and `FinalOfAtStage=6`. Its package
partition is 44 root `cascades`, 21 SQL/query, one `expressions`, and one
`plans`. A raw textual search reports 74 only because it also sees three
definitions and four comment-only occurrences; comments and strings are
negative controls, never call sites. The six `FinalOfAtStage` calls are
`expression_rule_call.go:232,286`, `implementation_rule.go:156`,
`intersector_primary_key.go:709,717`, and `plans/plan_expression.go:118`.
The ledger separately records the production `QuantifierOverPlan(s)` call
population from the same typed AST pass. All calls migrate through the fallible
root boundary; none of the six staged calls is left behind. Same-package
expressions/plans tests either become external test packages or use test-only
exact memostore fixture handles which cannot compile into production. A
package/source gate requires zero expression-taking Reference factory or call
below root and proves that comment/string lookalikes do not affect the count.

`Reference.ResultType() (Type, error)` returns `MemoEmptyReference` for a
detached empty read view and the stable handle error for an invalidated or
hostile view. It never maps either case to absence. Every Quantifier factory
rejects nil, zero, typed-nil, empty, untyped, missing/double-`RELATION`,
wrong-draft, and invalidated handles; same-draft provisional handles are
representationally valid until staged-parent admission rechecks their draft.
The three empty References in plan extraction become explicit no-plan/decline
before Quantifier construction; the empty restricted-final Reference becomes a
stable error/no Reference. The gate requires zero production
`&expressions.Reference{}` and zero quantified empty handle.

Preparation uses non-compressing canonical traversal. Path compression,
forwarding cleanup, lazy cache writes, member sorting, generation/epoch changes,
and task/statistic changes are delta/apply-only. Tests cover: late failure after
every builder lane with byte-for-byte state/version equality; builder use after
return; nested transaction/access; recursive grandchild equality; two-group
and diamond provisional graphs; dedup to an existing group; pending cycles;
hash collisions; forwarded-chain late failure; concurrent writers; and
success/failure across member, constraint, property, epoch, partial-match, task,
and statistic state. On success every retained provisional token resolves to
the expected committed canonical group; on failure every token is closed and
unresolvable.

The raw base also has 17 direct Insert/InsertFinal call expressions and two
InsertReExploring calls. `ValidateForMemo` is added to 65 production and 16
compiled test RelationalExpression implementations (excluding docscheck string
fixtures). A hostile hash-spy with a disagreeing result proves closed-registry
and exact relation-type validation happen before hashing or transaction state.

### 7.5 Fallible relational reconstruction

Rebinding quantifiers can change the exact flowed type, physical child layout,
and owner-current handle. The existing infallible interface cannot return an
invalid copy or silently keep the old owner. It changes uniformly:

```go
type RelationalExpression interface {
    // existing methods...
    WithQuantifiers([]Quantifier) (RelationalExpression, error)
    ValidateForMemo() error
}
```

Every implementation rebuilds through its fallible purpose constructor,
allocates a fresh owner-current handle, re-roots local current Values, validates
result type/layout requirements, and returns no object on failure. Returning the
same pointer is allowed only for an exact no-op on the identical quantifier
slice. Extraction, rule construction, memo adapters, and all generic callers
propagate the error before insertion.

The raw base contains 65 production implementations, 16 compiled test implementations,
and 31 production call expressions. The AST ledger classifies all of them plus
method values and interface adapters. Static fixtures reject an old one-result
implementation or ignored error. A wrong-type/wrong-layout replacement test is
run through logical Select, every physical plan family, extraction, and a rule;
it must produce no owner and no memo mutation.

## 8. Production migration ledger

### 8.1 All 40 nil-path producers

The checked AST inventory and this table must reconcile exactly. Every row has
one valid control and one mutation at that exact site; a grouped test counts only
if changing each listed site makes it fail.

Rows performed during SQL/logical translation resolve semantic paths only
against exact logical source type and correlation scope. They do not inspect an
`OrdinalLayout`. The later physical implementation/selector chooses a layout,
binds source-rooted Values, or checked-reanchors them. Table shorthand such as
“input/seed layout” means that physical step, not a logical identity input.

| current site | disposition and authority | proof focus |
| --- | --- | --- |
| `recordlayer/primary_key_translation.go:59,75` | keep complete raw `KeyExpression` as non-Value metadata; resolve against candidate flowed type | flat/nested/type-prefix/reorder/invalid suffix |
| `cascades/fk_chain_cardinality.go:405` | resolve witnessed output slot against projection/map/flat-map result type and layout | rename/direct/computed/missing/cross-leg |
| `cascades/intersector_primary_key.go:1199` | delete PlanContext name fallback; decline unless every candidate exposes resolved PK Values | missing-leg decline; unanimous structural PK |
| `cascades/primary_scan_match_candidate.go:222` | resolve full PK expression against `c.baseType` | descriptor reorder; nested/type-prefix |
| `cascades/vector_index_match_candidate.go:218` | resolve against real single-record flowed type, never `UnknownType` | single/multi/unknown layout |
| `cascades/windowed_index_match_candidate.go:212` | resolve against constructor flowed type | path/parameter alignment |
| `cascades/rule_primary_scan.go:54` | resolve full PK expression against scan flowed type | flat/nested/reordered property |
| `cascades/rule_ordered_primary_scan.go:110` | same for ordered scan | ASC/DESC; unresolved declines |
| `cascades/rule_ordered_index_scan.go:193` | compare candidate typed structured column, never reconstruct `colName` | nested/top-level/quoted-dot/wrapper |
| `cascades/max_match_map.go:971` | resolve ordinal `i` from `v.Type()` | every slot; wrong index |
| `cascades/values/pullup.go:77` | resolve RC output slot then checked collapse/reanchor | duplicate names; original unchanged |
| `cascades/values/values_helpers.go:53` | deconstruct by declared ordinal | reconstruct/deconstruct rows |
| `embedded/logical_predicate.go:4444` | group-key loop position is output ordinal | same-leaf keys select distinct slots |
| `embedded/logical_predicate.go:7382` | use derived `gkOrdinal` on aggregate output current | ASC/DESC rows; no name map |
| `cascades_translator.go:4919` | resolve structured projection reference against inner/source layout | qualified/quoted/nested/duplicate/miss |
| `cascades_translator.go:5705` | pull sort ref to unique folded-RC output slot | hidden/computed/duplicate; safe decline |
| `cascades_translator.go:7194` | resolve structured sort ref against input layout | qualified/quoted/nested/ambiguous |
| `cascades_translator.go:7502` | resolve projection `ColumnRef` against input layout | duplicate-leaf projection rows |
| `cascades_translator.go:7822` | resolve against correlated-scalar seed layout | colliding outer/inner names |
| `cascades_translator.go:8319` | resolve group key against aggregate input layout | SELECT/HAVING/ORDER BY agreement |
| `cascades_translator.go:8401` | resolve aggregate operand against input layout | SUM/AVG/COUNT source |
| `query/clustered_outer_scalar.go:873` | resolve projection ref against cluster seed layout | scalar/NULL-padding rows |
| `cascades_translator.go:3929` | resolve and retain the predicate as a source-QOV-rooted logical FV using only exact logical type/provenance; the later physical purpose constructor binds or reanchors it after selecting a concrete merged layout | real-FDB rows for callers `:3796,:3824,:4014` under two competing physical layouts |
| `cascades/index_expansion.go:120` | resolve placeholder against candidate base type or decline candidate | typed/unknown/ambiguous candidate |
| `cascades/index_predicate_to_query.go:111` | resolve complete metadata path against candidate base | sparse nested/top-level predicate |
| `cascades/vector_index_expansion.go:36,48` | resolve partition/vector fields against flowed type | partition/vector slots |
| `cascades/match_candidate_index.go:569` | resolve before CARDINALITY/ordered-bytes wrapper | wrapper identity and rows |
| `plans/ordering.go:916` | failed name lookup returns no ordering claim | missing/ambiguous materializes sort |
| `embedded/logical_predicate.go:5528` | remain pre-resolution or return typed planning error | no canonical-name Value fallback |
| `cascades_translator.go:5380,5392` | unattributable/missing sort source declines fold | safe plan and row order |
| `cascades_translator.go:7873` | empty scalar seed is typed planning error | no late NULL/ordinal failure |
| `cascades_translator.go:10359` | require structured provenance+typed layout or reject recursive shape | qualified versus quoted-dot |
| `cascades/index_expansion.go:600` | failed fan-out resolution excludes candidate | valid nested / invalid decline |
| `cascades/primary_scan_match_candidate.go:248` | layout-less ordering key is absent | sort not falsely elided |
| `cascades/match_candidate_index.go:209` | unknown/non-unique layout returns absence | candidate ordering controls |
| `cascades_translator.go:5375` | leg spelling without leg layout declines fold | no accidental whole-datum read |
| `cascades/match_info_merge.go:773` | unresolved step becomes impossible; preserve/revalidate valid path | invalid input; fused rows |

The partition is 26 resolve, two pre-layout metadata, 11 decline/error, one
preserve/invariant, and zero whole-object conversions.

### 8.2 Remaining FieldValue construction surface

The three ordinal `-1` producers are independent blockers:

- `query/unnest_seed.go` resolves every suffix against the classified element
  type/layout or declines before Value construction;
- `query/unnest_gather.go` does the same for the gathered element; and
- `cascades/index_expansion.go` resolves the candidate nested layout or retains
  non-Value metadata and declines.

The complete sealed-construction ledger is:

| group | sites/count | required migration |
| --- | ---: | --- |
| external FV/path/accessor literals | 52 / 10 / 5 | exact factories; zero raw construction |
| removed/changed constructors | 76 | 11 lazy; 19 ordinal; 23 domain; 5 pinned; 8 correlated; 2 ordinal; 8 raw path/fusion |
| typed controls | 27 | retain typed authority; propagate errors |
| direct assignments / shallow copies / path transforms | 18 / 2 / 15 | checked immutable rebuild |

The 76 calls reconcile exactly:

```text
NewFieldValue                                      11 (7 resolve, 4 decline)
NewFieldValueWithResolvedOrdinal                  19
NewFieldValueWithResolvedOrdinalInDomain          23
NewFieldValueWithPinnedOrdinalInDomain             5
NewCorrelatedFieldValueWithResolvedOrdinal         4
NewCorrelatedFieldValueWithResolvedOrdinalInDomain 4
NewOrdinalFieldValue                               2
NewFieldPathOfSingle                               4
NewFieldPathOfSingleInDomain                       2
FuseNestedSuffix                                   2
```

The 23 existing path-bearing literals are descriptor/DDL (3), sentinel paths
(3), translator reanchors (5), generic transforms (8), max-match splits (2),
and group-path rebuilds (2). All raw path/accessor literals are in those groups.
The checked manifest records each file:line, authority, factory, error policy,
and test; the end-state gate requires zero legacy constructors.

Because safe RC collapse changes ordinary RC nullability to Java's nonnullable
default, a separate AST ledger classifies the 259 raw concrete mentions, 24
constructor-name hits, and ten literals into construction, Type/nullability
consumer, mutation, recognition, or diagnostic use. Explicit nullable RC
construction remains purpose-named. Every call-site mutation test proves either
unchanged rows or the intended nullability-preserving QOV boundary.

### 8.3 QOV and result typing

All 87 production QOV construction sites state an exact type. The 50 untyped
sites and 38 direct `GetFlowedObjectValue` calls are separate AST ledgers;
indirect result-owner uses are additionally tracked through `go/types`. Each is
classified as:

- derive exact type from quantifier/reference/descriptor/array element;
- construct stable output current from the owner's exact result descriptor; or
- return a typed error/decline before creating a QOV.

There is no “reporting-only Unknown QOV” in an admitted Value graph. GroupBy,
streaming aggregate, `PlanExprBase`, memberless-Reference sentinels, and every
other formerly untyped result producer get a concrete disposition. Infallible
`GetResultValue()` implementations do not resolve on demand: their fallible
owner constructors validate and store the exact stable result once. The owner
manifest names every constructor signature/caller and tests memberless and
disagreeing References plus repeated getters. All 145
assertions/switches use exact recognition, six literals disappear, and direct
field reads use immutable views.

The executable-root gate classifies all 94 raw `.Evaluate` calls into recursive
Value-internal calls and named evaluation chokepoints. Every chokepoint supplies
the full existing environment plus exact current/source bindings. New direct
root calls fail the gate. The eight CurrentAlias mentions reduce to the private
tagged definition, rendering helpers, and `CurrentCorrelation()`.

Because the correlation kind tag changes in-memory map-key identity, a separate
complete manifest covers every correlation constructor, text/parser
reconstruction, alias-map operation, equality/hash, explain conversion, and map
key. Named machine-shaped and quoted `_current`/`q$5` identifiers remain named
in memory and unequal to tagged current/unique. A wire gate proves there is no
Go QOV serialization/deserialization path; no semantic identity is
reconstructed from display text.

### 8.4 Rewrite and rule surface

| API | production surface | end state |
| --- | ---: | --- |
| `Replace` | 29 calls in 11 files | 2 observers become walk; 27 checked rewrites |
| `ReplaceLeavesMaybe` / once | 6 / 1 external + 1 internal | checked fallible leaf rewrite |
| predicate `ReplaceValues` | 21 callers | `RewritePredicate` with error propagation |
| generic `WithChildren` | 5 external + 1 internal | per-kind checked rebuild; zero FV-capable generic calls |
| `RebaseValue` | 16 external | exact typed alias-only specialization |
| `TranslateCorrelations` | 5 external | checked typed translation |
| `MapFieldValues` | 1 external | fallible |

`functions/flatten` and NLJ propagate checked errors; max-match and fetch map
only documented incompatibility to decline; null-on-empty constructs a typed
NullValue; constant folding declines an FV it cannot prove. Both TranslationMap
protocols, both predicate translation sites, `SelfWithChildren`, and every
second-order callback are in the manifest.

The three rule-call drivers and simplifier gain error results. The rule-effect
analyzer enumerates every `Yield`, memoize, re-explore, constraint/property,
partial-match, and task-scheduling method. Existing rule implementations with
no fallible preparation compile unchanged except for driver signature updates;
affected rules split into pure `Prepare...` and infallible build/commit halves.

### 8.5 Pin/domain/leg-layout replacement

An authoritative generated manifest covers the 207 leg-layout occurrences in
21 files and the 78/96 pin/domain populations, with these dispositions:

| current responsibility | end-state authority |
| --- | --- |
| Value resolution and ordinal proof | exact child QOV + complete FieldValue path |
| source versus merged root | source-bound QOV window or checked carrier reanchor |
| seed recognition/build enablement | explicit physical tile/layout kind |
| top-level runs | immutable layout tiles |
| buried/overlapping leg address | immutable exact-source windows |
| null-supplied leg | nullable Source QOV + per-row match presence |
| downstream leg propagation | physical plan provided output layout |
| executor row adaptation | layout-owning positional carrier + exact binder |
| planning/cost/layout compatibility | provided/required physical property |
| diagnostics/census | read-only views; excluded from authority counts |

The affected production files are grouped and checked as follows:

- representation/resolution: `values/{type,values,column_identity,pullup,
  replace,map_field_values,semantic_hash,ordinal_join_seed,
  ordinal_seed_layout}.go`;
- optimizer: quantifier/logical projection, covered ordinals, max-match,
  match-info/candidates, select merge, group-by/filter/distinct/in-union/NLJ
  rules, cost and ordering;
- translator: `cascades_translator`, ordinal/unnest/gather/cluster/exists,
  `expr`, embedded generator/predicate/visitor; and
- runtime: evaluation context, positional row, ordinal join, flat-map,
  streaming cursors, and executor roots.

All 23 direct external literals and four factory calls receive the exact
plan-owned layout or a validated trivial flat layout; the factory body is
migrated once. Zero gates cover not just fields but the former
mechanism names: `SourceRelativeBaked`, `RootIsLegRelativeUnpinned`,
`OrdinalIn`, `LegAwareRootOrdinal`, `OrdinalSeedLeg*`, pin, domain, and Legs.

### 8.6 Physical plan/property owners

A generated manifest enumerates **every** concrete `RecordQueryPlan` type (42
at the RFC base), rather than a hand-selected Value-owner list. That includes
leaf scans, aggregate-index, explode, table function, temp-table/load-by-keys,
all joins/unions/recursive/selectors, DML/text/vector/index plans, row-shaping
operators, and pass-through wrappers.

Each row records required input/evaluation layout, parent edge bindings,
provided output layout, addressing mode, physical-member key participation,
row constructor, executor binder, continuation/normalization disposition, and
a layout-differential mutation test. A new concrete physical plan without a
manifest row fails a `go/types` gate. Logical Select's old
pin-derived merge marker becomes a one-shot pre-memo construction state or an
idempotent structural derivation; no admitted logical node reads layout.

### 8.7 Aggregate surface

The compile surface is one `AggregateSpec` mint, two logical GroupBy
constructors, five streaming-plan constructors, aggregate-index translation,
and the grouping/streaming cursor paths. The migration deletes
`OperandIntType`, fixes `IsCountStar`, makes construction/preparation fallible,
and replaces every UNKNOWN aggregate output field with the shared exact
descriptor. Every getter returns stable state and defensive slices.

## 9. RFC-197 debt retired by this milestone

No allowlist entry is deleted because a field became private. Each removal has
a mutation test which fails if the old mechanism returns.

### 9.1 Three dotted decliners and independent box probe

`rebaseOuterLegValueOrdinal`, `rebaseOuterLegValue`, and `legRef` dispatch only
on exact QOV root, complete path, and selected source window. A direct leg field
may have a literal-dot display name. A nested field is known from path arity,
not spelling. A merged/current root is known from its QOV and addressing mode,
not pin or domain.

`classifyLegConjunct` owns a separate direct dot probe and is not retired merely
because `legRef` changes. It is replaced only when one exact source QOV maps to
one window; merged/current/foreign roots remain conservative. Its own
source/nested/literal-dot/foreign tests are required.

For every site, the control matrix contains:

- one-accessor source field named `A.B`;
- qualified `A.B` represented as source QOV A plus accessor B;
- two-accessor nested `addr.city`;
- source-bound flat/nested/overlapping window;
- carrier-bound checked reanchor; and
- null-supplied, merged, and foreign negative controls.

A mutation restoring `strings.Contains`, arity-as-qualification, pin/domain, or
offset-only mapping must fail rows or the exact decline verdict.

### 9.2 Group output binding

Implemented. The aggregate construction records each key/call output in the
ordered exact result descriptor, and SELECT, HAVING, and ORDER BY carry those
resolved Values rather than rebuilding an output slot from a rendered label.
The former `groupByOutputOrdinals`, `groupByOutputBaker`, and
`groupKeyOrdinalByStructure` compatibility island is deleted; there is no name
map left to consult or to fall back to.

The live controls are the exact construction/boundary proofs in
`group_key_pull_up_guard_test.go`: distinct quantifier owners remain distinct,
an ambiguous exact boundary declines, and a unique match still binds. Those
tests exercise the post-name-map contract directly rather than keeping a dead
compatibility algorithm alive solely for its tests.

### 9.3 Expected debt delta

The pre-implementation projection of 39 entries / 29 authorities is historical,
not the current ledger. RFC-232 retired a broader set of compatibility readers
and dead twins. The current uncached authoritative result is **18 entries over
11 distinct authorities**: boundary 2, contract 4, dotted 4, name-keyed 4,
translator 4, with escape and harness at zero. The exact bucket/authority table
and its concentration rows live in RFC-197 and are checked by
`TestFieldDebtBucketsArePartition`; this paragraph records the RFC-232 outcome,
not a second independently editable inventory.

`rebaseUnnestOuterLegPredicate` is not one of the five entries, but its lazy
mint violates the representation invariant. Logical translation resolves and
retains a source-QOV-rooted FV from exact logical type/provenance; it does not
read a selected physical layout. After implementation selection, the physical
purpose constructor binds that source QOV or checked-reanchors it against the
selected merged layout. Real-FDB tests cover all guarded caller classes around
`:3796`, `:3824`, and `:4014`, and execute the same logical predicate under two
compatible physical layouts. Deleting, passing through, or declining that
predicate is forbidden because it can silently drop EXISTS rows. A package
gate forbids SQL/logical translation from importing or inspecting
`OrdinalLayout`.

## 10. Test and proof plan

Every bullet below is a mutation-sensitive deliverable. A row-affecting path
asserts actual rows and values; explain shape alone is never the proof. All new
tests call `t.Parallel()` and execution tests use real FDB, not mocks.

### 10.1 Type, QOV, and construction

- exact-type table: nil, typed nil, cycle, UNKNOWN/ANY/NONE, erased array or
  relation, malformed primitive, divergent field ordinal, nested malformed
  type, duplicate names, and every valid type form; each invalid mutation gets
  its exact code and no panic;
- mutate every caller Type slice/pointer after snapshot and every Type returned
  by a getter; QOV/FV/layout type, hash, equality, resolution, and evaluation
  remain unchanged;
- exact recognizers accept private nodes and reject direct, embedded,
  nil-embedded, and hostile views without invoking their methods;
- zero, named, unique, current, and forged textual `_current` correlations;
- alias-map tests reject current↔noncurrent, permit named↔unique
  alpha-equivalence in both directions, and keep raw named/unique identifiers
  unequal; checked phase-root translation is the only current-to-concrete path;
- raw and alias-mapped QOV equality plus alias-free type-sensitive hash;
  mapped equal implies equal hash, while same alias/different type is unequal;
- owner/evaluation-phase admission rejects two same-correlation QOVs with
  different types and two current handles; independent parent/child current
  phases with different types remain valid;
- one aggregate owner has different input and state/output phase types, and one
  join has input-edge, merged-predicate, and output phases; swapping only two
  same-correlation phase handles is rejected before memo admission;
- every Reference stores exactly `RELATION<ResultValue.Type()>`; record and
  scalar Explode Quantifiers unwrap once, while missing/double wrappers, member
  disagreement, and memberless References fail before hash; stable repeated
  result getters and NullOnEmpty widening-once are positive controls;
- nested parent/child phases with equal output types: a mutation retaining the
  child current root across the edge is rejected before evaluation, while the
  checked quantifier rebase succeeds;
- nil/nonrecord child, empty path, negative/out-of-range at every depth, scalar
  intermediate, unknown/ambiguous name, name/ordinal mismatch, and hostile
  request/value return exact errors with no object;
- ordinal zero and arbitrary-depth paths succeed; literal-dot, qualified, and
  nested requests remain structurally distinct;
- duplicate-name fields are selectable by distinct ordinals and ambiguous by
  name; changing only display metadata does not change identity;
- nullable child, each nullable intermediate, nullable leaf, and all-nonnull
  matrix pins the Java OR rule; and
- chained FV input canonicalizes to the same fused path/hash/result; RC collapse
  selects the right duplicate-name child, retains every suffix, and never
  leaves a partial FV; ordinary nonnullable RC collapses exactly, while an
  explicitly nullable RC preserves nullable result or is bound through QOV.

### 10.2 Layout factory and identity

Factory table tests mutate one field at a time: carrier kind/type, tile kind,
parent/start/width, gap, overlap, nested-on-scalar, negative/out-of-range
path, both/neither window modes, field-path count, duplicate exact source,
duplicate field address, type mismatch, and nullability mismatch. Each returns
its exact code and no layout. Valid recursive tiles and overlapping/prefix
windows are positive controls.

Snapshot tests mutate every supplied spec/slice/type and every getter result;
raw equality, hash, binding, and reanchor stay unchanged. Independently built
equal layouts hash equally; window order is irrelevant; a consistent alias
rename is raw-unequal, alias-map-equal, and hash-equal. Every non-alias
behavioral mutation is unequal. Fuzzing asserts `equal => equal hash` for raw
and alias-aware equality. `AsOrdinalPhysicalProperties` accepts only the
plans-owned private concrete view and rejects direct, embedded, nil-embedded,
and hostile implementations without invoking a method.

### 10.3 Binding and runtime

- object and field windows; scalar, flat, nested, and reordered storage; two
  source aliases over overlapping slots; and one record whose fields are all
  NULL;
- matched all-NULL versus unmatched null-supplied versus absent binding;
  present nil is SQL NULL, absent is `UnboundCorrelation`;
- explicit current binding, explicit source binding, foreign alias, wrong
  non-equal layout, short row, invalid nested carrier, and missing match
  presence; independently built RawEqual layout is accepted;
- one selected child reused under two distinct parent edge aliases, a correlated
  inner with one declared external, missing external, and collision among
  current/edge/window/external origins;
- one admitted phase closure contains result, two conditions, a property and a
  SARG which jointly require two windows; omitting any graph, colliding origins,
  or validating against a one-window layout fails selection;
- property selection uses QOV-only edge/external declarations before any row
  exists; execution must present one datum-bearing edge binding for each edge
  declaration and cannot add or reclassify a source;
- one QOV bound to a whole record, scalar unnest QOV used directly, and struct
  member access through FV; no QOV is bound to an already-consumed field;
- two same-shaped rows bound to source and current produce distinct values; a
  mutation restoring positional/bare-row fallback fails;
- protobuf declaration reorder, misleading display names, explicit proto2
  default, absent singular, present zero, repeated leaf, enum/scalar,
  tuple/UUID/nullable-array, nil intermediate, and out-of-range descriptor;
  direct FV, positional, covering, continuation-resume, nested Value,
  executor, and SQL-driver paths return the same canonical row-carrier values;
  a mutant bypassing `ProtoFieldToRowValue` in any one path fails; and
- every buffering owner normalizes to current-only layout; matched-all-NULL and
  unmatched round-trip distinctly through sort and recursive/temp-table buffers,
  while deleting normalization or attaching a non-equal plan layout fails.

### 10.4 Reanchor and physical selection

- flat reorder, nested object prefix, chained path order, non-contiguous field
  window, overlapping windows, duplicate field names, and null-supplied leg;
- evaluate source-bound and carrier-bound forms over real rows and require
  identical result/type/nullability;
- mutations using offset-only math, names, dropped/reversed suffix, wrong
  source/target, or double window application fail with the exact code;
- no-op preserves pointer; failure leaves every input untouched and returns no
  partial output; semantically equal but allocation-distinct current roots
  rebuild onto the exact target handle rather than taking the no-op;
- one logical group has a cheaper incompatible child and a costlier compatible
  child; selection must choose the compatible plan;
- source-bound accepts distinct satisfying layouts, carrier-bound requires raw
  equality, and layout additions do not change logical Reference membership;
  and
- two layout-different physical plans remain distinct members with equal
  logical result type and identical SQL rows; and
- a reordered commutative Select with correlated siblings matches only in a
  dependency-valid order: the dependency is proven and added before the
  dependent child is compared; a mutant exposing a future sibling alias or
  omitting an already-proven dependency fails; 9- and 12-child permutations
  prove there is no positional cap; and
- otherwise-identical quantifier edges mutated one field at a time across
  concrete kind, `NullOnEmpty`, and `StrictSingle` are unequal through fast
  Insert, `SemanticEquals`, `MemoEqual`, and the hash candidate path; the
  unchanged control remains equal/hash-equal.

### 10.5 Rewrite and rule atomicity

- post-order, replacement-not-revisited, nil replacement, cycle, first error,
  no-op pointer, FV/QOV hostile callback output, and unknown external composite
  with changed children;
- every Value and predicate family preserves all suffix ordinals and returns
  the original graph unchanged on failure;
- typed alias-only rebase succeeds only for exact equal target type; current to
  concrete, different type, or different physical layout uses checked
  translation/reanchor;
- optional matcher converts only documented codes to decline; SQL and committed
  translator paths propagate;
- staged multiple yields followed by `Fail` commit nothing; successful batch
  deduplicates and commits once; valid-then-invalid and invalid-then-valid
  commit preflights, late invalid absorb/merge, pending-reference cycles, and
  cross-batch duplicates leave every shared structure unchanged;
- a late error after every `DeltaBuilder` lane leaves the complete memostore
  state/version byte-identical; retained-builder calls return
  `MemoTransactionClosed`, a nested transaction returns the exact reentry code
  without deadlock, concurrent writers retry safely, lock-free readers observe
  either the old or atomically published aggregate state, and recursive
  grandchild comparison succeeds through the transaction snapshot; and
- for constraint, property, partial-match, statistic, task, index, and
  parent-edge lanes, mutate every caller-owned map/slice/pointer before and
  after canonical record construction and every byte/slice/read view returned
  by `Store.Snapshot`; stored equality, canonical bytes, version, later
  removal/dequeue behavior, and root materialization remain unchanged. Unknown
  kind, malformed/noncanonical bytes, direct raw objects, and pointer-bearing
  payloads are rejected before a builder intent;
- staged memoize → pending-handle quantifier → yield resolves atomically to an
  existing or new group; pending-handle escape, wrong draft, member cycle, and
  failed pure admission all return no usable handle and cause no memo mutation,
  while valid admission exposes its exact flowed type immediately;
- forwarded and deduplicated References canonicalize to one stable wrapper and
  `ReferenceKey`; visited sets, parent indexes, and extraction traversal neither
  revisit nor miss the group. A mutant using raw pointer equality or
  `map[*Reference]` fails on allocation-distinct wrappers for the same group;
- analyzer fixtures reject direct Reference/Memo/constraint/property/task
  mutation from every registered rule or transitive helper, with valid staged
  Yield/failure and prepare-then-infallible-commit controls for all three call
  kinds; and
- a gate proves zero rule branches on/assigns the old Yield boolean before the
  void staged signature lands; post-commit driver statistics are tested
  separately; and
- constructing a flowed QOV then attempting a disagreeing single or batched
  Reference insert
  invokes no hash method and changes no member, group type, epoch, task, memo
  topology, property, constraint, or staged yield.
- a forwarded Reference chain plus late failure leaves every forwarding pointer,
  canonical target, lazy cache, generation, and member order unchanged; and
- advance-stage, promote, prune, clear exploratory/final, winner set/clear,
  root/leaf replacement, parent/index add/remove, every state-record removal,
  task dequeue, and group deletion each commit atomically in the positive
  control and leave the complete outside `Store.Snapshot` byte-identical when a
  later intent fails.

### 10.6 Aggregate contract

- COUNT(*), COUNT(1), COUNT(NULL), COUNT(nullable string/enum/number), AVG,
  numeric SUM/MIN/MAX lanes, and string/enum/nonnumeric rejection before empty
  execution;
- COUNT and all numeric output nullability/types match the table; INT versus
  LONG overflow proves the lane is derived from operand type;
- logical GroupBy, physical complete result, output record type, ordinal
  layout, and emitted row agree slot-for-slot;
- duplicate leaf names, repeated equal grouping Values, and zero/one/multiple
  semantic output matches across SELECT/HAVING/ORDER BY;
- bare grouped and ungrouped empty aggregate emit zero rows; explicit default
  wrapper emits exactly one global row; and
- repeated getters, input slice mutation, continuation branch, and current/input
  binding mutations cannot alter identity or rows; and
- mutate the caller spec, grouping/operand node and nested payload, returned
  result Value, output labels, and every getter slice after construction;
  descriptor/type/hash/layout/rows remain fixed or the unsupported input is
  rejected at construction.

### 10.7 Static gates and old-test polarity

End-state `go/types` gates require zero raw FV/path/accessor/QOV construction or
field mutation; zero lazy/raw/domain/pin factory; zero nil/negative path branch;
zero untyped QOV or mutable CurrentAlias; zero public-view assertion used as
admission; zero name lookup in FV evaluation; zero generic unchecked rewrite;
zero ambient QOV fallback; zero `RecordType.Legs`, pin, domain, or former helper
mechanism; zero unchecked Reference/Memo ingress; zero registered-rule direct
mutation outside RuleCall; and zero concrete physical plan or buffered-row owner
lacking a layout/normalization disposition.

Each gate has alias, dot-import, generic, method-value, embedding, and unrelated
symbol fixtures plus a positive hit. The generated before/after manifest names
every migrated site and refuses unexplained additions or disappearances.

A dedicated package-dependency/type gate forbids logical expressions and SQL
semantic translation from reading `OrdinalLayout` or physical requirements;
one logical expression implemented under two layouts must retain identical
logical equality/hash and Reference result type.

Tests which pin the invalid state are flipped or removed, including the
layout-less `FuseNestedSuffix` carry tests, name-based proto descent,
childless-fused reference, silent unpinned/nonordinal binding, duplicate-name
leg name-arm, flat-dotted `legRef`, dotted-frontier `box_conjunct`, and group-key
“name map in charge” fixture.

## 11. Performance contract

Correctness is non-negotiable, but the new checks must not accidentally turn a
shared Cascades DAG into quadratic copying.

- exact type and layout construction are linear in input size; cycles are
  detected by identity during one traversal;
- layout window canonicalization is `O(w log w)` and exact-source lookup is
  expected `O(1)`;
- QOV/FV recognizers, equality, and hash allocate zero; hashes and exact type
  keys are cached;
- FV evaluation is `O(path depth)` and performs no layout-sized copy;
- binding and reanchor are `O(address depth)` and
  `O(FV depth + mapped path)` respectively;
- no public `Type()` thaw occurs on internal equality, hash, resolution,
  selection, or evaluation hot paths;
- checked rewrites preserve shared no-change nodes and visit each reachable DAG
  node once through an identity memo; and
- already-validated layout/value handles are not recursively revalidated at
  every memo lookup.

Before/after measurements use the same box, at least ten runs, `benchstat`, and
`-benchmem`. They include existing FV Evaluate, semantic hash, memo hit,
realistic/full planner, deep/wide extraction, streaming aggregate, and ordinal
row-binding benchmarks, plus new exact-type snapshot, FV construction,
recognizer, QOV binding, layout construction/hash, reanchor, and no-op/change
rewrite cases at fixed depth and width.

Acceptance:

- recognizer/equality/hash and ordinal evaluation allocations do not increase;
- FV ordinal evaluation median `ns/op` does not regress more than 5%;
- planner/memo median `ns/op` and bytes/op do not regress more than 10%/5%;
- layout lookup/evaluation allocates zero after construction; and
- construction cost remains linear. Any breach blocks completion until fixed or
  explicitly re-reviewed as a correctness-required tradeoff; budgets are not
  raised to make a regression green.

## 12. Verification and review protocol

Before production implementation:

1. this RFC receives Graefe ACK for Java/Cascades alignment and Torvalds ACK for
   API, migration, performance, and proof completeness on one SHA-256;
2. the first implementation checkpoint adds red constructor, identity, layout,
   binding, rewrite, aggregate, debt, and static-gate tests before the
   corresponding production change; and
3. every measured inventory is reproduced with a positive control.

Implementation proceeds in test-first slices:

1. propagation-only plumbing: fallible rule drivers and `WithQuantifiers`,
   invocation-local effect staging, error/static gates, and one source-gated
   package-private apply adapter over the **existing infallible** Reference
   mutations. No new semantic constructor or Reference-admission failure lands
   in this slice; all fallible work completes before the adapter applies a
   successful invocation, and the adapter has a deletion gate in slice 3;
2. exact type/correlation/QOV/FV/request/resolver, checked Value/predicate
   rewrite, the closed owner/member snapshot registry, owner-closure freezing,
   and ordinal-only FV runtime semantics. These failures travel through the
   slice-1 channel and occur before any staged effect is applied;
3. atomic memostore-backed memo/Reference commits, immutable canonical
   `*expressions.Reference` handles and keys, fallible checked root Reference
   factories over the slice-2 exact `RELATION` snapshot/member registry, plan
   constructors accepting admitted Quantifiers, and deletion of the slice-1
   apply adapter;
4. immutable layout, record/scalar carriers, origin declarations, full-context
   explicit binding, and provided/required property propagation;
5. checked reanchor plus complete buffer normalization built on the slice-2 FV
   and slice-4 layout APIs;
6. immutable aggregate spec/output descriptor and logical/physical aggregate
   owner migration, now that its grouping/operand FV graphs can be frozen and
   explicitly bound;
7. all remaining producer/consumer/rule migrations and RFC-197 retirement.

Each slice is independently buildable, runs its affected tests uncached, and
does not use an unsafe compatibility constructor. Transitional adapters are
package-private, counted, reject invalid state, and have a deletion gate in the
next slice.

Slice 1 lands the error channel and staging protocol before any constructor can
return a new failure to a void rule or infallible relational rebuild. Its first
checkpoint is signature/gate red tests, followed by a compiling
propagation-only change. Its apply adapter is safe only because it admits no
new fallible Reference invariant and the existing serialized mutations are
infallible after full successful rule preparation; an AST gate prevents calls
from anywhere except the one driver. Exact-type/QOV/FV failures begin in slice
2 and are staged before apply. Slice 3 replaces that adapter with the fully
fallible, atomic memostore transaction after exact type/member snapshots exist.
Layout normalization cannot call reanchor before slice 5; aggregate freezing
waits for slice 6.
No later slice is permitted to add a fallible call behind an unpropagated
driver, memo, or `WithQuantifiers` boundary.

Final verification:

1. run every touched Bazel target uncached with exact `=== RUN` counts,
   including values, predicates, expressions, cascades, plans, executor,
   sqldriver, and docscheck;
2. run real-FDB layout/binding/row suites plus ten-run determinism checks for
   affected planner tests;
3. run `TestFieldNameNeverDecides` and
   `TestFieldDebtBucketsArePartition` uncached and report the exact delta;
4. run all type-aware zero gates and reconcile every before/after population;
5. run `just gazelle`, `bazel mod tidy`, and `just test`;
6. because `core/embedded` changes, run
   `//pkg/relational/conformance/factorycorpus/full:full_test` uncached against a
   same-box baseline; on the known parallel contention signature, rerun the same
   SHA rather than increasing timeout;
7. run the performance protocol; and
8. obtain parallel Graefe and Torvalds implementation ACKs, fold findings, then
   obtain one final delta reconfirmation from both on final HEAD.

No failure is skipped or deferred. No debt entry is removed without its
mechanism-removal test.

## 13. Rejected alternatives

**Make `Resolved` non-nil while keeping exported state.** Rejected: zero,
empty, negative, partial, mutable, and inconsistent states remain constructible.

**Use an exported concrete struct with private state.** Rejected: its zero value
still implements Value. Exact-private implementations plus exact recognizers
reject embedded-interface impostors.

**Use ordinal `-1`, empty path, or ordinal zero as migration scaffolding.**
Rejected: each erases or asserts the wrong field identity.

**Add an unresolved FieldValue/QOV which participates in Values or memo.**
Rejected: resolution happens before Value construction; inability to state a
result type is an error or non-Value applicability state.

**Split identifiers on `.`.** Rejected: quoted `"A.B"`, qualified `A.B`, and a
nested path are different syntax and semantics.

**Give current a fresh per-owner token.** Rejected as unnecessary and unlike
Java. One tagged global current is safe because each evaluation phase binds it
explicitly and Values crossing a phase are translated.

**Keep ambient QOV positional fallback.** Rejected: it makes the stated child
alias irrelevant and lets a valid ordinal read the wrong carrier.

**Keep pin/domain/Legs and merely add them to logical identity.** Rejected:
physical storage provenance is not logical result identity, can split/dedup the
wrong Cascades alternatives, and leaves mutable duplicate layout authorities.

**Put ordinal layout in Type or Reference identity.** Rejected: Java groups by
logical result type; physical layouts are provided/required properties of
members and chosen alternatives.

**Use `start + ordinal` for reanchor.** Rejected: non-contiguous, nested, and
overlapping windows require a complete structural path map.

**Bind a QOV to a scalar or already-consumed field.** Rejected: a record QOV is
the whole typed object. Scalar aliases are QOVs used directly.

**Return `(Value, bool)` from generic rebuild.** Rejected: it cannot distinguish
unsupported kind, invalid arity/root, deliberate no-match, and success, and it
permits silent retention of stale children.

**Panic from generic `WithChildren`.** Rejected: checked rebuild and stable
errors are the only construction path.

**Apply staged memo/constraint effects before whole-draft validation.** Rejected:
the prepared rule delta makes every fallible decision before one infallible,
locked publish; mixed-validity batches otherwise leak partial planner state.

**Keep aggregate output UNKNOWN as unrelated work.** Rejected: exact QOV and
FV construction cannot be proven while a core logical and physical result
producer lies about its output type.

**Serialize `OrdinalLayout`.** Rejected: no wire change. The selected immutable
plan is the authority and reattaches layout on continuation resume.

**Require only feature tests.** Rejected: every retired mechanism, factory
invariant, error policy, layout discriminator, binding state, and migration site
has a control plus a mutation which makes the test fail.
