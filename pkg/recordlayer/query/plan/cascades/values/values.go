// Package values is the Value-tier of the Go Cascades planner port —
// scalar / row-context expressions that compose into predicates,
// projections, and join keys. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values` package.
//
// Contents:
//
//   - Value interface (Children, Type, Name, Evaluate) + concrete
//     subtypes: Constant, Field, Arithmetic, Boolean, Cast, Null,
//     Aggregate, QuantifiedObject, Promote, RecordConstructor,
//     Parameter, ScalarFunction, Not.
//   - ExplainValue — SQL-ish renderer used by plan-cache keying and
//     EXPLAIN output.
//   - SimplifyValue — standalone constant-fold over a Value tree
//     (free function; the rule-driven equivalent lives in cascades's
//     `Simplify`).
//   - LiteralValue / ToInt64 / ToFloat64 — coercion helpers
//     promoted from comparisons.go (RFC-025 Phase 1) so both values/
//     and predicates/ can call them without a layering cycle.
//   - CorrelationIdentifier + Correlated — Quantifier-tracking
//     surface used by Values to declare which upstream Quantifier
//     they depend on; rewrite rules consult this when checking
//     correlation-shape preservation.
//   - ExpressionFolder + DefaultFolder — testable seam for plan-time
//     constant folding (RFC-025 §"Closing the leaks").
//   - The Type hierarchy (`type.go`) — the rich `Type`
//     interface + `TypeCode` enum + concrete impls (`PrimitiveType`,
//     `RecordType`, `ArrayType`, `EnumType`, `RelationType`),
//     canonical singletons for every primitive (incl. UUID, VERSION,
//     None, Any), `TypeRepository`, `WithNullability`, the
//     `IsPromotable` / `MaximumType` / `MaximumTypeOfMany`
//     promotion lattice (with structural recursion through ARRAY /
//     RECORD / ENUM / RELATION), and shape predicates (`IsNull`,
//     `IsArray`, …). Every Value impl's `Type()`
//     returns the rich `Type` directly — the legacy `ValueType`
//     enum + `FromValueType` / `ToValueType` bridges are retired.
//     Once `type.go` exceeds ~1500 LOC it splits into a dedicated
//     `cascades/typing/` sub-package per RFC-025.
//
// Imports: nothing else from `pkg/recordlayer/query/plan/cascades/...`.
// `predicates/`, `matching/`, and root `cascades` all import this
// package; the dependency arrow points inward to keep cycles out.
package values

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer/protoname"
)

// Canonical ISO 8601 layouts for temporal value formatting/parsing.
// Mirrors functions.TimestampLayout / functions.DateLayout — duplicated
// here because values/ must not import functions/ (layering: values is
// the leaf package that predicates/ and cascades/ depend on).
const (
	timestampLayout = "2006-01-02 15:04:05"
	dateLayout      = "2006-01-02"
)

// The legacy `ValueType` enum (TypeUnknown / TypeInt / TypeString / TypeBool /
// TypeFloat) is retired — every Value impl's Type() returns the rich Type
// directly. Only the members whose NAME MATCHES THEIR VALUE remain as bridge
// vars; TypeString is NullableString and TypeBool is NullableBoolean, so
// neither can mislead.
//
// TypeInt and TypeFloat are gone. Both named one type and were another —
// TypeInt was NullableLong, TypeFloat was NullableDouble — and between them
// they produced eight tests that read as coverage for the type in their name
// while asserting the other one's behaviour, plus a live wrong-rows bug where
// the walker routed `CAST(x AS FLOAT)` through the DOUBLE-coded alias so the
// cast never rounded to binary32. Every use of both meant the wider type and
// now says so. Name the type you mean: NullableInt / NullableLong /
// NullableFloat / NullableDouble.
//
// Legacy bridge retirement: RFC-025.
var (
	// TypeUnknown is the placeholder for "type not yet inferred".
	// Maps to the canonical UnknownType singleton.
	TypeUnknown Type = UnknownType
	// TypeString is the legacy name for STRING — bridged to
	// NullableString.
	TypeString Type = NullableString
	// TypeBool is the legacy name for BOOLEAN — bridged to
	// NullableBoolean. Note BooleanValue's Type() returns
	// NotNullBoolean (literals are NOT NULL); compare via
	// `.Code() != TypeCodeBoolean` when nullability is irrelevant.
	TypeBool Type = NullableBoolean
)

// Value is the root of the Value hierarchy.
// Concrete Values implement Children / Type / Name / Evaluate;
// matchers downcast via type switches / type assertions on the
// concrete Go type.
//
// Java equivalent: `Value extends Correlated<Value>, TreeLike<Value>,
// Typed, ...`. The initial port keeps Children + Type + Name + a
// simple Evaluate since those are the surfaces rules touch. The
// `Correlated.GetCorrelatedTo` contract is declared separately (see
// correlation.go) and implemented by those Values that reference a
// Quantifier; leaf values opt out.
type Value interface {
	// Children returns the immediate sub-Values of this node.
	// Leaf Values return an empty slice (never nil — keeps matcher
	// code free of nil checks).
	Children() []Value
	// Type is the rich result Type of evaluating this Value
	// (the legacy ValueType enum is retired; Type() returns the
	// rich Type directly). Never nil —
	// implementations return UnknownType when the type genuinely
	// isn't known yet.
	Type() Type
	// Name is a debug string for error messages + explain output.
	// Not part of the matcher DSL.
	Name() string
	// Evaluate produces the Go-native value this Value represents
	// against an eval context. Leaf ConstantValue ignores the
	// context; FieldValue looks up its column; ArithmeticValue
	// recurses. The context is opaque (`any`) so different
	// subsystems can pass their own row shape — tests use
	// `map[string]any`.
	//
	// Returns (value, nil) on success — (nil, nil) is SQL NULL.
	// (nil, err) signals a data-dependent runtime error (arithmetic
	// overflow, division by zero, invalid cast, type mismatch);
	// callers propagate it instead of recovering a panic.
	Evaluate(evalCtx any) (any, error)
}

// --- Concrete values ------------------------------------------------

// ConstantValue is a literal. Evaluate returns Value verbatim.
//
// Typ carries the literal's rich Type. NULL constants
// (`Value == nil`) keep Typ for the typed-NULL case (e.g.
// `CAST(NULL AS INT)`); the constructor / call sites set
// the canonical singleton appropriate for the literal's Go
// runtime type.
type ConstantValue struct {
	Value any
	Typ   Type
}

func (c *ConstantValue) Children() []Value         { return []Value{} }
func (c *ConstantValue) Name() string              { return "constant" }
func (c *ConstantValue) Evaluate(any) (any, error) { return c.Value, nil }

// Type returns the constant's rich Type. Nullability is derived
// from Value: nil Value → nullable (a typed NULL literal); non-nil
// Value → NOT NULL (the literal carries a concrete value, so by
// definition can't be NULL). Mirrors Java's
// `LiteralValue.computeReturnType` shape.
//
// The Typ field's own nullability is overridden — callers shouldn't
// have to pre-compute the right NotNull / Nullable singleton; the
// presence/absence of Value is the authoritative signal.
func (c *ConstantValue) Type() Type {
	if c.Typ == nil {
		return UnknownType
	}
	return WithNullability(c.Typ, c.Value == nil)
}

// FieldValue references a column by name on a base value. In the full
// Java model, FieldValue always has a child value (typically a
// QuantifiedObjectValue correlated to a quantifier) and a FieldPath
// (multi-step for nested access). In Go, Child is optional for backward
// compatibility: nil Child = leaf field reference (flat model used by
// existing code).
//
// With Child set, FieldValue participates in correlation tracking:
// GetCorrelatedToOfValue walks into Children() and discovers the
// child's correlation. This is essential for push-through rules that
// need to know whether a value is correlated to a specific quantifier.
//
// Field-name contract: callers constructing FieldValue via the SQL
// resolver (expr.ResolveIdentifier) receive the case-folded (upper-
// case) form, matching Identifier.Name(). Downstream row producers
// MUST normalise their map keys to the same form.
type fieldValue struct {
	Field string
	Typ   Type
	Child Value // base value (nil = legacy flat field reference)

	// Resolved is the baked-ordinal path — Java's construction-time
	// FieldPath resolution, where the accessor IS an ordinal and runtime access
	// is positional: resolveOrdinal returns the accessor's ordinal directly, so
	// a positional-row read is row.Get(ordinal) — position-preserving by
	// construction, and therefore sound under DUPLICATE output names, which no
	// name-based resolution can address at all: a by-name lookup DECLINES on an
	// ambiguous name (RecordType.FieldIndexUnique) and a name-keyed map collapses
	// the duplicates. Field is a DISPLAY name for diagnostics and Explain.
	//
	// Every RUNTIME-evaluated FieldValue is BAKED (Resolved non-nil): the
	// runtime name-resolution fallback is deleted, so a LAZY node (nil) that
	// reaches evaluation fails loud (OrdinalResolutionError). Lazy nodes
	// survive only as PLAN-TIME artifacts that are never evaluated against a
	// row — match-candidate columns, ordering-hint name carriers (compared by
	// name, never evaluated), and transient resolver/translator trees that the
	// bake walks (bakeFlatRefsAgainstColumns, bakeGroupByOutputRefs,
	// pullUpSortKeyValue, the seed constructions) bind before the plan
	// finalizes.
	//
	// Two bake KINDS (see FieldPath.FrontierPinned for why the distinction is
	// load-bearing):
	//   - MACHINERY-OWNED (FrontierPinned): built by the join/gather seed
	//     machinery over a typed leg QOV (newFieldValueOfOrdinal) or fused by
	//     the rebase walks; the ordinal is FINAL for the executor's assembled
	//     row / leg window.
	//   - SOURCE-RELATIVE (unpinned single-accessor): the resolver's
	//     construction bind against the reference's OWN source's declared
	//     column order; translator walks rebase it onto composed frontiers,
	//     and at runtime it reads through its leg's window.
	//
	// Identity (Java's FieldValue identity contract): BAKED nodes compare by
	// per-element ORDINAL
	// list equality (FieldPath.Equals); baked vs lazy is UNEQUAL (worst case a
	// missed dedup, never a conflation); lazy vs lazy is name-only.
	// FrontierPinned is EXCLUDED from identity/hash/Explain (an evaluation-
	// contract marker, not a value distinction — like Java excluding name/type
	// from resolvedAccessor equality). Every FieldValue copy/rebuild site MUST
	// preserve Resolved — dropping it silently degrades a baked node to lazy,
	// which is loud at evaluation but conflates duplicate same-named columns
	// in plan-time matching.
	//
	// Field is DISPLAY ONLY, and it names the LAST accessor. Java's fused
	// FieldValue has no root-name accessor at all — the one single-name question
	// askable of it is getLastFieldName (FieldValue.java:134-135 → FieldPath's
	// at :463-466, `getOptionalFieldNames().get(size()-1)`) — so the leaf is the
	// only answer with a spec behind it, and every mint of the fused shape now
	// gives it: the planner's rewrite machinery (compose, the rebase/withChildren
	// fuse arms, select-merge, match-info merge, index expansion, unnest, the
	// left-outer wrappers) and the SQL resolver's fuseNestedAccessors alike. A
	// resolver-produced `n.sk` is fieldValue{Field:"SK", Accessors:[N,SK]}.
	//
	// It was NOT always so. fuseNestedAccessors copied the root node whole and
	// left Field naming the struct ROOT, so `Field` answered differently
	// depending on which mint made the value — which is why nothing downstream
	// may read Field as the column's IDENTITY even now that the two agree.
	// Identity is the resolved path (AccessorNamePath / FieldPath.Equals); Field
	// is a label. Reading it as identity is what collapsed two ORDER BY keys of
	// one struct root (RFC-227), and a value whose Field disagrees with its last
	// accessor is still expressible — nothing validates it at construction, so
	// the naming authorities stay path-based by design rather than by luck.
	Resolved *fieldPath

	// rootType and resultType are exact immutable snapshots populated by the
	// RFC-232 resolver.  They deliberately coexist with the legacy-private
	// fields while the package's internal consumers are migrated; admission via
	// AsFieldValue requires both snapshots and therefore cannot recognize an
	// incompletely initialized node.
	rootType   *exactType
	resultType *exactType
}

// resolvedAccessor is the construction-time-resolved accessor a BAKED
// FieldValue carries — Java's FieldValue.resolvedAccessor (FieldValue.java:~630),
// whose equals/hashCode are ordinal-only.
//
// IMMUTABLE after construction: FieldValue copy sites deliberately SHARE the
// pointer (withChildren, the pullup/pushdown passthrough copies). Any future
// change to the accessor must REPLACE it
// with a new value, never mutate in place, or every shared copy silently
// changes identity.
type resolvedAccessor struct {
	// Field is the PER-STEP display name (Java resolvedAccessor.getField();
	// "" = pure ordinal access, Java's null name). NOT part of the path's
	// identity — Java's element equality is ordinal-only
	// (FieldValue.java:675-689); the name survives only for nested-record
	// descent by per-step name (descendResolvedPath, into a proto.Message or a
	// nested record map) and Explain rendering.
	Field   string
	Ordinal int

	// fieldType is the exact type captured when this ordinal is resolved. It is
	// intentionally absent from accessor identity, but makes the read view and
	// runtime shape checks independent of mutable caller Type graphs.
	fieldType *exactType
}

// FieldPath is the multi-accessor path — Java's FieldValue.FieldPath
// (FieldValue.java:373): ONE FieldValue node holds a whole path, never chained
// nodes. IMMUTABLE after construction (replace-never-mutate):
// FieldValue copy sites share the pointer; WithSuffix
// returns a NEW path (Java :525-534). Single-accessor paths come from the
// resolver's construction binds and the seed machinery;
// multi-accessor paths are produced by the
// baked compose rule (fusing an inner path with an outer suffix).
// INVARIANT: a FieldPath carried by a FieldValue is NON-EMPTY — a zero-step
// path reads nothing and is not a meaningful accessor (Java's FieldPath.EMPTY
// exists only for prefix arithmetic Go doesn't port). Both constructors
// (newFieldPathOfSingle, WithSuffix) uphold it; Root()/Last() panic on a
// hand-built violation rather than tolerating it.
// OrdinalDomain names the LAYOUT an ordinal indexes — the third element of
// column identity (RFC-197: identity is (correlation, domain, ordinal path)).
//
// The same ordinal under the same correlation can address two different
// layouts: a MACHINERY-OWNED bake's ordinal is final for the executor's
// assembled row / leg window, while a SOURCE-RELATIVE bake's ordinal indexes
// the reference's OWN source's declared column order (see
// FieldPath.FrontierPinned). Comparing ordinals across those two layouts is a
// type error that reads as authoritative, which is strictly worse than the
// name conflation this workstream exists to end. So an ordinal is only
// comparable against a STATED layout, and OrdinalIn is the only way to ask.
//
// The token identifies a layout STRUCTURALLY: the ordered list of the layout's
// column names. That is exactly the soundness condition for reusing an ordinal
// — position k of two layouts with the same ordered column list is the same
// slot — and it is derivable independently by the producer (which resolves a
// name against a declared column order) and by the consumer (which holds the
// descriptor-shaped row type). Two structurally identical layouts of different
// provenance are interchangeable FOR POSITIONAL PURPOSES; distinguishing which
// quantifier a reference belongs to is the CORRELATION element's job, checked
// separately by every caller.
//
// The zero value is UNKNOWN: a producer that cannot state which layout its
// ordinal indexes mints no token, and OrdinalIn fails closed on it. Unknown is
// therefore the safe default for every construction site that has not been
// taught its domain — a declined optimization, never a wrong slot.
type OrdinalDomain struct {
	// sig is the layout signature. Unexported so the token cannot be
	// hand-forged from a name at a call site: the only ways to obtain one are
	// the two derivations below, both of which take a whole layout.
	sig string
}

// IsKnown reports whether the token names a layout at all. An unknown token
// (the zero value) never satisfies OrdinalIn, on either side of the check.
func (d OrdinalDomain) IsKnown() bool { return d.sig != "" }

// String renders the token for diagnostics. Not an identity: use ==.
func (d OrdinalDomain) String() string {
	if !d.IsKnown() {
		return "domain(unknown)"
	}
	return "domain(" + d.sig + ")"
}

// OrdinalDomainOfColumnNames derives the token for a layout given as an ordered
// column-name list — the shape the translator's bakers resolve against.
//
// The encoding is length-prefixed so it is INJECTIVE: ["A","B"] and ["A|B"]
// (or ["AB"]) must not collide, or two different layouts would answer to one
// token and the check would be theatre. Names are upper-cased because every
// resolution path in the engine matches case-insensitively.
// An empty list yields the UNKNOWN token: a layout with no columns states
// nothing, and treating "" as a real domain would make every not-yet-taught
// producer's zero token match it.
func OrdinalDomainOfColumnNames(cols []string) OrdinalDomain {
	if len(cols) == 0 {
		return OrdinalDomain{}
	}
	var b strings.Builder
	b.WriteString(strconv.Itoa(len(cols)))
	for _, c := range cols {
		u := strings.ToUpper(c)
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(len(u)))
		b.WriteByte(':')
		b.WriteString(u)
	}
	return OrdinalDomain{sig: b.String()}
}

// OrdinalDomainOfType derives the token for a layout given as a flowed record
// type — the shape match candidates, seeds and the executor hold. Anything but
// a *RecordType (UnknownType, a primitive, a multi-record-type index's
// degraded row type) has no single column order and yields the UNKNOWN token,
// so a caller that cannot name its layout fails closed by construction.
func OrdinalDomainOfType(t Type) OrdinalDomain {
	rt, ok := t.(*RecordType)
	if !ok || len(rt.Fields) == 0 {
		return OrdinalDomain{}
	}
	names := make([]string, len(rt.Fields))
	for i, f := range rt.Fields {
		names[i] = f.Name
	}
	return OrdinalDomainOfColumnNames(names)
}

type fieldPath struct {
	Accessors []resolvedAccessor

	// Domain is the layout the ROOT accessor's ordinal indexes (RFC-197
	// step 0): for a FrontierPinned path the frontier the ordinal is FINAL
	// for, for an unpinned source-relative path the reference's OWN source.
	// Both readings are the same question — "which layout does this ordinal
	// address" — which is why one token and one check (OrdinalIn) serve both
	// bake kinds.
	//
	// Zero (unknown) when the constructing site could not state its layout.
	// That is a fail-closed default, not a gap to paper over at the consumer:
	// an ordinal whose domain is unknown is not comparable to anything.
	//
	// Lives ON THE PATH, once, beside FrontierPinned and for the same reason:
	// it governs the ROOT read context, and accessors beyond the first descend
	// nested records where the question does not arise.
	//
	// EXCLUDED from identity/hash/Explain exactly as FrontierPinned is — an
	// evaluation-contract marker, not a value distinction. Two references to
	// the same column that arrived through different producers must still
	// intern as one memo member.
	//
	// The preservation contract binds COPY, REBUILD and REBASE sites, and only
	// those: a rewrite that takes an existing reference and hands back "the
	// same column, moved" must carry the token across (the preserve-on-copy
	// contract Resolved itself imposes), because dropping it degrades a
	// domain-checkable node into one that fails closed — a lost optimization
	// rather than a wrong answer, but lost SILENTLY. WithChildren, RebaseValue,
	// WithSuffix and the pull-up walk are the enforced ones
	// (ordinal_domain_test.go's preservation cases fail if any of them stops
	// carrying it).
	//
	// PRODUCERS are under no such obligation, and most of them mint UNKNOWN
	// today. That is the designed default, not debt: a site that cannot name
	// the layout its ordinal indexes must say so, and OrdinalIn then declines
	// for it. Teaching a producer its domain is an improvement to be made
	// where the layout is genuinely in hand (newFieldValueOfOrdinal derives it
	// from the typed child it is given, which is the model); inventing one to
	// make the token non-zero would be the ordinal conflation this element
	// exists to prevent, wearing a proof's clothes.
	Domain OrdinalDomain

	// FrontierPinned marks a MACHINERY-OWNED bake: the node was built by the
	// join/gather seed machinery (newFieldValueOfOrdinal over a typed
	// leg QOV, or a rebase-walk fusion), its ordinal FINAL for the executor's
	// assembled row / leg window. Unpinned baked nodes are SOURCE-RELATIVE —
	// the resolver's bind against the reference's own source's declared
	// column order (see FieldValue.SourceRelativeBaked).
	//
	// The distinction is LOAD-BEARING: the seed-shape
	// probes (ordinalJoinSpansOf, IsOrdinalJoinRV, the leg-type harvesters)
	// key on the pin to tell a machinery-built leg-concat SEED from a user
	// projection of resolver-baked references over the same legs, and the
	// translator's rebase/collection walks must rebind source-relative nodes
	// through the composed frontier while leaving machinery-final nodes
	// untouched. Collapsing the two kinds requires rebinding EVERY reference
	// against its chosen physical layout at plan time (Java's
	// translateCorrelations model, where the planner rewrites correlations
	// structurally and no seed-vs-resolver provenance survives) — the
	// remaining architectural gap between Go's two-level join lowering and
	// Java's quantifier rewiring.
	//
	// The contract is a property of the VALUE, invariant under transformation
	// — pullup/pushdown passthrough copies strip Child but share this pointer,
	// so keying machinery-ownership on child presence would let a semantics-
	// preserving rewrite silently demote a node.
	// Lives ON THE PATH, once, not per-accessor: it governs the ROOT read
	// context; accessors beyond the first read nested records, where the bit
	// is meaningless (N copies of a one-meaning bit
	// desynchronize). EXCLUDED from identity/hash/Explain: an evaluation-
	// contract marker, not a value distinction.
	FrontierPinned bool
}

// OrdinalIn is the fail-closed domain accessor (RFC-197 step 0): it reports the
// ordinal this path reads IN the caller's stated layout, and answers ONLY when
// the path provably indexes THAT layout.
//
// It answers for a single-accessor path whose recorded domain IS the given
// frontier — for a FrontierPinned path that means the pin's frontier is the
// given one, for an unpinned path that its own source is. It FAILS CLOSED on
// everything else, and every arm below is a distinct way an ordinal comparison
// has been or could be wrong:
//
//   - a nil path (LAZY node): no ordinal exists to answer with.
//   - a multi-accessor (FUSED) path: its Root() ordinal addresses the OUTER
//     step, so answering with it drops the nested descent — `t.addr.city`
//     would read whatever slot ADDR occupies while claiming to be CITY.
//   - an unknown domain on either side: a producer that could not state its
//     layout, or a caller that cannot state its frontier. Neither can be
//     coerced into a comparison; there is nothing to check.
//   - a domain MISMATCH: the ordinal indexes a different layout. This is the
//     element revision 1 of RFC-197 omitted, and it is the whole reason the
//     domain is a parameter rather than a comment at the call site.
//   - a NEGATIVE ordinal: Java's resolvedAccessor asserts ordinal >= 0 at
//     construction (FieldValue.java:651), which is what makes its ordinal-only
//     equality safe. Go mints `Ordinal: -1` NAME-ONLY accessors at three
//     producer sites — unnest_seed.go, unnest_gather.go, and index_expansion.go
//     (its fan-out collection path, plus any step whose enclosing type is not a
//     record or whose name does not resolve) — where two accessors are ordinal-equal by
//     construction and the NAME is the only identity left. Answering -1 hands
//     a caller an ordinal that matches every other name-only accessor — the
//     wrong-column bind pinned in aggregate_group_key_accessor_name_test.go.
//
// A declined answer is a declined optimization; a wrong answer is wrong rows.
func (p *fieldPath) OrdinalIn(frontier OrdinalDomain) (int, bool) {
	if p == nil || len(p.Accessors) != 1 {
		return 0, false
	}
	if !frontier.IsKnown() || !p.Domain.IsKnown() || p.Domain != frontier {
		return 0, false
	}
	if ord := p.Accessors[0].Ordinal; ord >= 0 {
		return ord, true
	}
	return 0, false
}

// RootOrdinalIn is the ARITY-TOLERANT counterpart to OrdinalIn: it reports the
// ordinal this path's ROOT accessor reads in the caller's layout, and only when
// that root provably indexes THAT layout.
//
// It exists because OrdinalIn refuses on arity BEFORE it reads the domain, so it
// returns false for every multi-accessor path and cannot distinguish "this root
// indexes the layout you asked about" from "it indexes a different one". Those
// are opposite facts for a caller re-anchoring a nested path, and collapsing
// them is what makes OrdinalIn unusable there rather than merely conservative.
//
// It answers about the ROOT only, deliberately: accessors beyond the first
// descend a nested record, where "which layout does this index" does not arise.
// A caller re-stating a root must still DERIVE the new ordinal from the target
// layout — this only says whether the carried one is comparable.
func (p *fieldPath) RootOrdinalIn(frontier OrdinalDomain) (int, bool) {
	if p == nil || len(p.Accessors) == 0 {
		return 0, false
	}
	if !frontier.IsKnown() || !p.Domain.IsKnown() || p.Domain != frontier {
		return 0, false
	}
	if ord := p.Accessors[0].Ordinal; ord >= 0 {
		return ord, true
	}
	return 0, false
}

// WithSuffix returns a NEW path with suffix's accessors appended — Java's
// FieldPath.withSuffix (FieldValue.java:525-534); neither input is mutated.
// The frontier pin comes from the RECEIVER: fusing inner.WithSuffix(outer)
// keeps the INNER path's root read context (the compose rule's shape), and
// the pin governs exactly that root.
func (p *fieldPath) WithSuffix(suffix *fieldPath) *fieldPath {
	if suffix == nil || len(suffix.Accessors) == 0 {
		// A nil/empty SUFFIX argument is a degenerate "append nothing" —
		// tolerated defensively. An empty RECEIVER violates the type's
		// non-empty invariant and gets no arm (don't
		// half-admit empties); empty+empty therefore passes the violating
		// receiver through — acceptable because an empty path is
		// hand-buildable only (both constructors uphold non-empty) and the
		// violation surfaces loudly at
		// the first Root()/Last().
		return p
	}
	merged := make([]resolvedAccessor, 0, len(p.Accessors)+len(suffix.Accessors))
	merged = append(merged, p.Accessors...)
	merged = append(merged, suffix.Accessors...)
	// The domain, like the pin, comes from the RECEIVER: both govern the ROOT
	// read context, and fusing keeps the inner path's root.
	cp := *p
	cp.Accessors = merged
	return &cp
}

// Root returns the first accessor — the one the ROOT read context resolves
// (a positional row slot).
func (p *fieldPath) Root() resolvedAccessor { return p.Accessors[0] }

// Last returns the final accessor — the path's display leaf (Java
// getLastFieldAccessor, FieldValue.java:459).
func (p *fieldPath) Last() resolvedAccessor { return p.Accessors[len(p.Accessors)-1] }

// Single returns the path's only accessor when the path is single-step —
// the shape the plain join-seed probes expect; ok=false for multi-accessor
// paths (those probes DECLINE fused shapes).
func (p *fieldPath) Single() (resolvedAccessor, bool) {
	if len(p.Accessors) != 1 {
		return resolvedAccessor{}, false
	}
	return p.Accessors[0], true
}

// Equals is Java FieldPath.equals (FieldValue.java:411-420): element-wise list
// equality over the accessors' ORDINALS (Java's
// resolvedAccessor.equals is getOrdinal()-only, :675-689). The per-step Field
// is NOT compared — display/rendering, not identity. FrontierPinned is
// likewise excluded (evaluation contract, not identity).
func (p *fieldPath) Equals(o *fieldPath) bool {
	if p == o {
		return true
	}
	if p == nil || o == nil || len(p.Accessors) != len(o.Accessors) {
		return false
	}
	for i := range p.Accessors {
		// ORDINAL-ONLY element identity: Java's
		// resolvedAccessor.equals compares getOrdinal() alone
		// (FieldValue.java:675-689 — name and type excluded), and FieldPath
		// equality is the accessor-list equality over it (:411-420). A
		// (Field, Ordinal) pair identity would be a REFINEMENT that
		// could only under-dedup — ordinal-only makes alias-mapped baked
		// references over same-shaped legs intern as one memo member,
		// exactly Java's dedup. The per-step Field survives on the accessor
		// for nested-record descent and Explain rendering only.
		if p.Accessors[i].Ordinal != o.Accessors[i].Ordinal {
			return false
		}
	}
	return true
}

// BakedNameContextError reports a BAKED FieldValue (ordinal authoritative)
// evaluated against a NAME-keyed or unrecognized row context.
// Never a silent name read or silent NULL: the display name is
// diagnostics-only and resolving by it would return the FIRST of duplicate
// same-named columns — the conflation ordinal identity exists to avoid.
// (A nil context stays NULL — that is the appendNullLeg / nil-binding path.)
type BakedNameContextError struct {
	Field   string
	Ordinal int
}

func (e *BakedNameContextError) Error() string {
	return fmt.Sprintf("baked FieldValue %s#%d evaluated against a non-positional row context — the ordinal frontier must supply a positional row (planner/executor bug)", e.Field, e.Ordinal)
}

// UnboundEvalContextError reports a FieldValue whose evaluation resolved to
// NOTHING: an UNRECOGNIZED non-nil context type, or a correlated reference whose
// correlation is UNBOUND and whose context supplies no frontier positional row.
// Production flows only OrdinalRow / *RowEvalContext / CorrelationBinder
// / nil, so reaching one of these tails is a planner/executor bug — LOUD for
// pinned and unpinned alike; a silent NULL would hide it.
// DISTINCT from BakedNameContextError (a PINNED node meeting a name-keyed context
// that DID resolve to a value): here nothing resolved at all. A nil context stays
// NULL (the appendNullLeg / nil-binding path) and never reaches here;
// a correlation that DID match a non-ordinal value (e.g. the executor's
// buildLegBinder raw leg) returns that value and never reaches here either.
type UnboundEvalContextError struct {
	Field       string
	Correlation string
	CtxType     string
}

func (e *UnboundEvalContextError) Error() string {
	if e.Correlation != "" {
		return fmt.Sprintf("correlated FieldValue %q (correlation %q) evaluated against an unbound/unrecognized context (%s) — no frontier row resolved (planner/executor bug)", e.Field, e.Correlation, e.CtxType)
	}
	return fmt.Sprintf("FieldValue %q evaluated against an unrecognized non-nil context (%s) — no frontier row resolved (planner/executor bug)", e.Field, e.CtxType)
}

// ContainsBakedOrdinal reports whether any FieldValue in v's subtree carries
// a MACHINERY-OWNED (FrontierPinned) baked-ordinal marker — the structural
// "is this an ordinal-join value tree" probe the join machinery keys on
// (SelectMergeRule target loop, the executor's ordinal-join construction).
// Deliberately blind to UNPINNED baked nodes (source-relative resolver
// bakes): those carry no join-frontier contract and must not trip join-seed
// machinery.
func ContainsBakedOrdinal(v Value) bool {
	found := false
	WalkValue(v, func(n Value) bool {
		if found {
			return false
		}
		if fv, ok := n.(*fieldValue); ok && fv.Resolved != nil && fv.Resolved.FrontierPinned {
			found = true
			return false
		}
		return true
	})
	return found
}

// IsPositionalMergeRC is the VALUE-level half of the structural
// merge-select recognition (no imperative marker — the exact
// shape PartitionSelectRule.java:284-291 builds and nothing else can): an RC
// whose every field is auto-generated-named ("_i", in position order — Java
// Type.java:2922 isAutoGenerated) and whose value is a BARE QOV of a distinct
// quantifier. The SELECT-level half (the QOVs are the select's own owned
// ForEach quantifiers, covering them) lives at the interning gate; the
// executor checks the QOVs against its two legs. Unconstructible from SQL:
// the generator names all columns, so CTE column-rename selects never match.
// Lives beside ContainsBakedOrdinal — the two value-shape probes ordinal
// join construction triggers on.
func IsPositionalMergeRC(v Value) bool {
	rc, isRC := v.(*RecordConstructorValue)
	if !isRC || len(rc.Fields) < 2 {
		return false
	}
	seen := make(map[CorrelationIdentifier]struct{}, len(rc.Fields))
	for i, f := range rc.Fields {
		if f.Name != OrdinalFieldName(i) {
			return false
		}
		qov, isQOV := f.Value.(*quantifiedObjectValue)
		if !isQOV {
			return false
		}
		if _, dup := seen[qov.correlation]; dup {
			return false
		}
		seen[qov.correlation] = struct{}{}
	}
	return true
}

// IsOrdinalJoinRV reports whether v is an ordinal-model JOIN-SELECT result
// value: a raw (non-anchored) RC whose every field is a FrontierPinned baked
// reference over a quantifier — the flat N-leg seed and its TranslationMap-
// translated upper forms (fused multi-accessor paths included) — spanning at
// least two distinct root quantifiers. This is the ordinal counterpart of the
// AnchoredJoin marker for the interning gate: the shapes whose quantifiers
// have no external identity consumer, where alias-IDENTITY dedup re-explodes
// the join re-enumeration's shared sub-products per bipartition. A lazy field
// anywhere (CTE column renames, computed projections) declines — those
// selects keep the alias-identity dedup that Go's column derivation requires.
func IsOrdinalJoinRV(v Value) bool {
	rc, isRC := v.(*RecordConstructorValue)
	if !isRC || len(rc.Fields) < 2 {
		return false
	}
	roots := make(map[CorrelationIdentifier]struct{}, 2)
	for _, f := range rc.Fields {
		// A bare TYPED
		// QuantifiedObjectValue field is the gathered unnest's whole-object
		// element leg (the mixed no-AT seed) — as position-determined as a
		// FrontierPinned bake (a whole-leg reference; no name resolution can
		// hide in it), so it counts toward the roots. TYPED only: an untyped
		// bare QOV carries no leg contract and keeps declining — the
		// CTE-rename/lazy-field decline rationale is untouched.
		if qov, isQOV := f.Value.(*quantifiedObjectValue); isQOV {
			if qov.Type() == nil || qov.Type().Code() == TypeCodeUnknown {
				return false
			}
			roots[qov.correlation] = struct{}{}
			continue
		}
		fv, isFV := f.Value.(*fieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return false
		}
		qov, isQOV := fv.Child.(*quantifiedObjectValue)
		if !isQOV {
			return false
		}
		roots[qov.correlation] = struct{}{}
	}
	return len(roots) >= 2
}

func (f *fieldValue) Children() []Value {
	if f.Child == nil {
		return []Value{}
	}
	return []Value{f.Child}
}

func (f *fieldValue) Name() string { return "field" }

// Type returns the field's rich Type. FieldValue stores
// the column type as-is; callers that know NOT NULL information
// from the catalog set Typ to the non-nullable form.
func (f *fieldValue) Type() Type {
	if f != nil && f.resultType != nil {
		return f.resultType.thaw()
	}
	if f.Typ == nil {
		return UnknownType
	}
	return f.Typ
}

// OrdinalRow is the ordinal-model runtime row FieldValue.Evaluate
// reads. It is satisfied structurally by executor.PositionalRow, which lives
// in a higher layer — the interface here avoids the import cycle.
//
// Get(ordinal) is the ONLY read: every FieldValue carries a plan-time-baked
// ordinal (Resolved) and reads its slot positionally — Java's
// MessageHelpers.getFieldValueForFieldOrdinals. A miss is a loud error (never
// a silent NULL): column existence is validated at plan time (42703), so a
// runtime miss is a malformed plan. There is no name-keyed read arm — Java's
// runtime never sees a column name (FieldValue.java:164-169).
type OrdinalRow interface {
	Get(ordinal int) (any, bool)
}

// OrdinalResolutionError is the loud internal error raised when
// a FieldValue's column cannot be resolved against the authoritative ordinal
// runtime row. Authority + a silent name-map fallback would mean a
// resolution bug never surfaces, so this is a query error, not a NULL. Ordinal
// is the resolved ordinal, or -1 for a flat-reference (name->ordinal) miss.
// Available carries the row type's column names (when the row exposes them) so
// the failure is diagnosable from the message alone.
type OrdinalResolutionError struct {
	Field     string
	Ordinal   int
	Available []string
}

func (e *OrdinalResolutionError) Error() string {
	return fmt.Sprintf("ordinal resolution: field %q not resolvable in the runtime row (ordinal %d, row columns %v) — malformed plan", e.Field, e.Ordinal, e.Available)
}

// ordinalRowNames extracts the row type's column names for diagnostics, when
// the OrdinalRow implementation exposes them (executor.PositionalRow does).
func ordinalRowNames(row OrdinalRow) []string {
	if tn, ok := row.(interface{ TypeNames() []string }); ok {
		return tn.TypeNames()
	}
	return nil
}

// rowIsMultiLeg reports whether an ordinal row is a MULTI-LEG composed row (a
// merged concat / clustered box row) — consulted via an optional interface the
// executor's PositionalRow/spanAwareRow implement. A multi-leg row cannot
// serve a SOURCE-RELATIVE baked ordinal (the ordinal addresses one leg's own
// window); the correlated fall-through arms go LOUD instead of reading a
// foreign slot (correct-or-loud).
func rowIsMultiLeg(row OrdinalRow) bool {
	ml, ok := row.(interface{ MultiLeg() bool })
	return ok && ml.MultiLeg()
}

// evaluateOrdinal reads f's column from an ordinal-model runtime row: the
// plan-time-baked ordinal (resolveOrdinal) reads its slot positionally — NO
// name resolution of any kind. A miss is loud. For a multi-accessor
// baked path the root read yields the NESTED record, and the remaining
// accessors descend into it (descendResolvedPath).
func (f *fieldValue) evaluateOrdinal(row OrdinalRow) (any, error) {
	if ord, ok := f.resolveOrdinal(); ok {
		if v, inRange := row.Get(ord); inRange {
			return f.descendResolvedPath(v)
		}
		return nil, &OrdinalResolutionError{Field: f.Field, Ordinal: ord, Available: ordinalRowNames(row)}
	}
	// There is NO runtime name-resolution fallback.
	// Every column reference must bind its ordinal at plan time; an
	// UNBAKED reference has no stable ordinal and fails LOUD here (correct-or-loud —
	// never a silent name read that could serve the wrong slot).
	return nil, &OrdinalResolutionError{Field: f.Field, Ordinal: -1, Available: ordinalRowNames(row)}
}

// evaluateDatumBinding resolves a correlation bound to a DATUM rather than an
// OrdinalRow. The leg adapter unwraps a bare-scalar `_0` carrier to its datum
// (executor.isBareScalarRow), so the binding has ALREADY consumed the accessor
// that would have addressed slot 0: the bound value IS the root read, and the
// path's REMAINDER is what is left to apply.
//
// Returning the datum whole dropped that remainder. That is invisible to every
// single-accessor reference — for which the remainder is empty — and so went
// unseen until a DESCENT hit it: a struct-element unnest (`orders.items AS i`)
// binds its element as one raw proto message, and `i.sku` is the two-accessor
// path `/I/SKU`, so the WHOLE element was served where the member was asked
// for. In a predicate comparing to a string that is never equal, which turned a
// wrong read into `WHERE i.sku = 'x'` returning ZERO rows.
//
// descendResolvedPath is the same helper evaluateOrdinal applies after its own
// root read, and it is a no-op on a remainder-free path — so this restores the
// invariant that a binding is always read THROUGH the path, rather than adding
// a case. Java expresses the same thing without a separate arm at all:
// QuantifiedObjectValue hands over the whole bound object
// (QuantifiedObjectValue.java:82-95) and FieldValue applies the entire
// remaining ordinal path to it (FieldValue.java:164-175 →
// MessageHelpers.java:93-106).
//
// A nil binding is the null leg (outer-join no-match) — NULL, not loud. A
// FrontierPinned node bound to a non-ordinal value is a frontier-contract
// violation.
//
// An UNBAKED node (no Resolved path) reads the datum WHOLE, and deliberately so.
// It is tempting to make that loud on the grounds that a node with no remainder
// cannot say which member it wanted — but the whole-datum read is a LIVE and
// CORRECT convention, not an oversight: the sort-key leg fallback mints an
// unbaked `newFieldValue(qov, col, …)` when a leg's layout is not derivable
// (cascades_translator.go), and for a leg bound to a datum the whole datum is
// exactly the right answer. Going loud would break that read. What actually
// keeps a MEMBER reference out of this arm is that a member reference is baked —
// it arrives carrying the path whose remainder is applied below. The
// pinned/unpinned asymmetry is pinned by
// TestFieldValue_UnpinnedNonOrdinalBinding_IsSilent, which exists so that a
// change to this arm is a deliberate red->green edit rather than silent drift.
func (f *fieldValue) evaluateDatumBinding(bound any) (any, error) {
	if bound == nil {
		return nil, nil
	}
	if err := f.frontierContractGuard(); err != nil {
		return nil, err
	}
	return f.descendResolvedPath(bound)
}

// frontierContractGuard enforces the FRONTIER CONTRACT for a
// FrontierPinned baked node: the executor guarantees a positional row, so a
// non-nil context that is NOT a positional/ordinal row is a planner/executor
// bug — reported as a loud *BakedNameContextError rather than a silent NULL that
// would hide it. An unpinned node (and a nil context — the
// appendNullLeg / nil-binding NULL) returns nil (no
// violation). The pinned-node "never silently NULL off the positional
// frontier" invariant lives here, at the non-positional tail of Evaluate /
// evaluateCorrelated.
func (f *fieldValue) frontierContractGuard() error {
	if f.Resolved == nil || !f.Resolved.FrontierPinned {
		return nil
	}
	return &BakedNameContextError{Field: f.Field, Ordinal: f.Resolved.Root().Ordinal}
}

// descendResolvedPath applies a baked path's accessors BEYOND the root to the
// root read's result — Java resolves the whole FieldPath against nested
// Message fields by ordinal (FieldValue.java fieldOrdinals doc); Go's nested
// records surface as a proto.Message or a name-keyed record map, so a nested
// step reads by the accessor's per-step name there (exactly what the chained
// lazy nodes the compose rule fuses did: child evaluates to the record, the
// outer node reads its Field on it) and by ordinal on a positional nested
// row. NULL propagates (a nil nested record yields NULL, matching the
// chained-lazy nil-context arm); an unreadable non-nil nested value is loud
// for a pinned path (a quiet NULL would hide a frontier bug) and NULL for an
// unpinned one (the lazy tail's historical behavior).
func (f *fieldValue) descendResolvedPath(rootVal any) (any, error) {
	// <= 1 (not == 1): a hand-built zero-accessor path violates the type
	// invariant, but a slice-bounds panic in the eval hot path is the wrong
	// place to report it.
	if f.Resolved == nil || len(f.Resolved.Accessors) <= 1 {
		return rootVal, nil
	}
	cur := rootVal
	for _, acc := range f.Resolved.Accessors[1:] {
		if cur == nil {
			return nil, nil
		}
		// No live path flows a name-keyed record map into a fused path's
		// nested step: nested records surface as a proto.Message (the record
		// layer's verbatim struct column, name-addressed like Java's
		// MessageHelpers.getFieldOnMessage) or a positional row (ordinal). A
		// map nested value takes the default arm: loud for a pinned path,
		// NULL for an unpinned one — never a silent name read.
		switch rec := cur.(type) {
		case OrdinalRow:
			v, inRange := rec.Get(acc.Ordinal)
			if !inRange {
				return nil, &OrdinalResolutionError{Field: acc.Field, Ordinal: acc.Ordinal, Available: ordinalRowNames(rec)}
			}
			cur = v
		case proto.Message:
			// A STRUCT column materializes as its raw proto message (the
			// executor's row layer flows nested records verbatim). Go descends it
			// by field NAME. That is the DIVERGENCE this step carries on the
			// `.Field` ratchet (RFC-197's `boundary` bucket) — it is not the port,
			// and this comment used to claim it was.
			//
			// Java descends by ORDINAL. FieldValue.eval calls
			// MessageHelpers.getFieldValueForFieldOrdinals (FieldValue.java:169),
			// which reaches the descriptor through
			// findFieldDescriptorOnMessageByOrdinal — a bounds check and
			// `getFields().get(ordinal)` (MessageHelpers.java:170-175), throwing
			// InvalidExpressionException out of range rather than missing quietly.
			// The NAME is consumed exactly ONCE, at construction: resolveFieldPath
			// maps it through recordType.getFieldNameToOrdinalMap() and raises
			// RECORD_DOES_NOT_CONTAIN_FIELD when it is absent, storing
			// resolvedAccessor.of(field, ordinal) (FieldValue.java:272-300 —
			// the name branch is :283-290, the store :297). The
			// name-taking overloads (getFieldOnMessage(msg, String)) exist in
			// MessageHelpers but FieldValue.eval is not a caller.
			//
			// So this is RFC-197's own thesis, in the reference implementation, at
			// precisely the boundary Go still asks the name — which is why the two
			// debt entries on protoFieldByName's spelling attempts retire together
			// on ONE fix (resolve the nested path to field numbers at the
			// boundary), and why "it may well be correct" no longer holds.
			// Deliberately NOT converted here, and the reason is stronger than an
			// unaudited maybe: on the producers that actually reach this arm the
			// Ordinal is KNOWN NOT to be the descriptor's declaration index.
			// unnest_seed.go and unnest_gather.go mint their struct-descent
			// suffixes as `resolvedAccessor{Field: ..., Ordinal: -1}` on the
			// stated grounds that the ordinal is never consulted here, so an
			// ordinal descent would index at -1; and expr.fuseNestedAccessors
			// copies a position in the SQL struct type's declared field list,
			// which equals the emitted descriptor's declaration index only by
			// convention. Converting the read before the producers is a silent
			// wrong-column read. Pinned by
			// TestFieldValue_DescendProtoMessage_MustNotConsultTheOrdinal, which
			// is what must be updated when the producers become ordinal-true —
			// that edit is the signal these two debt entries are retirable.
			//
			// Unset singular field = NULL (proto3 presence rules ride
			// protoreflect.Has).
			v, found := protoFieldByName(rec.ProtoReflect(), acc.Field)
			if !found {
				if f.Resolved.FrontierPinned {
					return nil, &OrdinalResolutionError{Field: acc.Field, Ordinal: acc.Ordinal}
				}
				return nil, nil
			}
			cur = v
		default:
			if f.Resolved.FrontierPinned {
				return nil, &OrdinalResolutionError{Field: acc.Field, Ordinal: acc.Ordinal}
			}
			return nil, nil
		}
	}
	return cur, nil
}

// uuidProtoMessageName is the fully-qualified tuple_fields.UUID message —
// UUID columns store as this message and surface as a neutral [16]byte (see
// protoScalarToRowValue). Kept equal to the executor's constant of the same
// name (query_result.go); a mismatch is a lockstep break.
const uuidProtoMessageName = "com.apple.foundationdb.record.UUID"

// protoFieldByName reads one field of a proto message by SQL identifier,
// converting to the engine's row-value domain exactly as the executor's
// record→row layer (protoFieldToGo) does. found=false when the descriptor
// has no such field; an unset field with proto2 presence returns (nil, true)
// — SQL NULL.
//
// DIVERGENCE from Java (MessageHelpers.getFieldOnMessage): for a field UNSET
// but carrying an explicit proto2 default, Java returns the declared default;
// Go returns NULL (unset → nil). Unreachable through Go's own metadata
// builder — it never emits explicit field defaults — but a Java-authored
// descriptor read on the Go side would differ here.
func protoFieldByName(m protoreflect.Message, name string) (any, bool) {
	fields := m.Descriptor().Fields()
	// Builder-emitted descriptors carry UPPER field names, Java's stored
	// protos lower/snake — try the SQL identifier verbatim, its lower-case,
	// then a full case-insensitive scan (both real shapes hit within these).
	fd := fields.ByName(protoreflect.Name(name))
	if fd == nil {
		fd = fields.ByName(protoreflect.Name(strings.ToLower(name)))
	}
	if fd == nil {
		for i := 0; i < fields.Len(); i++ {
			if f := fields.Get(i); strings.EqualFold(string(f.Name()), name) {
				fd = f
				break
			}
		}
	}
	if fd == nil {
		// The descriptor's field names are ESCAPED
		// (protoname.ToProtoBufCompliantName, applied wherever a descriptor is
		// emitted from a SQL identifier), and the escaping is not
		// case-insensitivity — it REWRITES characters: `a$b` is stored as
		// `a__1b`, `a.b` as `a__2b`, `a__b` as `a__0b`. None of the three
		// attempts above can reach those, so a field whose identifier the
		// escaper mangles would resolve to "no such field" and, for an
		// unpinned path, read back as a silent NULL.
		//
		// An escaper error means the identifier could never have produced a
		// field name in the first place, so there is nothing to match.
		if escaped, err := protoname.ToProtoBufCompliantName(name); err == nil && escaped != name {
			fd = fields.ByName(protoreflect.Name(escaped))
			if fd == nil {
				for i := 0; i < fields.Len(); i++ {
					if f := fields.Get(i); strings.EqualFold(string(f.Name()), escaped) {
						fd = f
						break
					}
				}
			}
		}
	}
	if fd == nil {
		return nil, false
	}
	if !m.Has(fd) && fd.HasPresence() {
		return nil, true
	}
	return ProtoFieldToRowValue(fd, m.Get(fd)), true
}

// ProtoFieldToRowValue converts one proto FIELD value to the engine's
// row-value domain — the SINGLE conversion both the executor's record→row
// materialization and this package's struct descent use (exported so the two
// cannot drift; the executor's protoFieldToGo delegates here). Repeated
// fields become []any (a downstream Explode's collection); a proto map stays
// its raw Go value; scalars and UUID/message leaves go through
// protoScalarToRowValue.
func ProtoFieldToRowValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	if fd.IsList() {
		list := v.List()
		out := make([]any, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = protoScalarToRowValue(fd, list.Get(i))
		}
		return out
	}
	if fd.IsMap() {
		return v.Interface()
	}
	// A NullableArrayWrapper column reads through the wrapper as the array
	// it stores: a PRESENT wrapper with an empty list is [] (distinct from
	// SQL NULL, which is the ABSENT field — the caller's presence check).
	if inner, wrapped, ok := EffectiveListField(fd); ok && wrapped {
		list := v.Message().Get(inner).List()
		out := make([]any, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = protoScalarToRowValue(inner, list.Get(i))
		}
		return out
	}
	return protoScalarToRowValue(fd, v)
}

// protoScalarToRowValue converts one proto value to the engine's row-value
// domain — the values-layer twin of the executor's fieldProtoToGo
// (query_result.go), kept in lockstep; a divergence surfaces as a
// comparison-type mismatch. Notably a UUID field surfaces as the neutral
// [16]byte (msb‖lsb) the executor uses so a struct-nested UUID compares and
// index-packs identically to a top-level UUID column; nested non-UUID
// messages stay raw for further descent; repeated fields are handled by the
// caller (protoFieldByName), which keeps them as lists for a downstream
// Explode.
func protoScalarToRowValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	if k := fd.Kind(); k == protoreflect.MessageKind || k == protoreflect.GroupKind {
		msg := v.Message()
		if string(msg.Descriptor().FullName()) == uuidProtoMessageName {
			return uuidMessageToRowBytes(msg)
		}
		return msg.Interface()
	}
	return ProtoScalarKindToRowValue(fd.Kind(), v)
}

// ProtoScalarKindToRowValue is the kind-keyed scalar conversion (no UUID/
// message handling — that needs the descriptor, see protoScalarToRowValue).
// Exported as the single source of truth the executor's kind-based conversion
// calls directly, so the record→row and struct-descent conversions cannot drift
// on the scalar arms.
func ProtoScalarKindToRowValue(kind protoreflect.Kind, v protoreflect.Value) any {
	switch kind {
	case protoreflect.BoolKind:
		return v.Bool()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(v.Uint()) //nolint:gosec
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return v.Float()
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BytesKind:
		return v.Bytes()
	case protoreflect.EnumKind:
		return int64(v.Enum())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return v.Message().Interface()
	default:
		return v.Interface()
	}
}

// uuidMessageToRowBytes reads a tuple_fields.UUID message (most/least
// significant bits) into a neutral 16-byte array, msb‖lsb big-endian — the
// same layout the executor's uuidMessageToBytes produces (kept in lockstep).
func uuidMessageToRowBytes(msg protoreflect.Message) [16]byte {
	fields := msg.Descriptor().Fields()
	mostFD := fields.ByName("most_significant_bits")
	leastFD := fields.ByName("least_significant_bits")
	var b [16]byte
	if mostFD == nil || leastFD == nil {
		return b
	}
	binary.BigEndian.PutUint64(b[0:8], uint64(msg.Get(mostFD).Int()))   //nolint:gosec
	binary.BigEndian.PutUint64(b[8:16], uint64(msg.Get(leastFD).Int())) //nolint:gosec
	return b
}

func (f *fieldValue) Evaluate(evalCtx any) (any, error) {
	if isAdmittedFieldValue(f) {
		return f.evaluateResolved(evalCtx)
	}
	if f.Child != nil {
		if qov, isQOV := f.Child.(*quantifiedObjectValue); isQOV {
			return f.evaluateCorrelated(qov, evalCtx)
		}
		cv, err := f.Child.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		evalCtx = cv
	}
	if evalCtx == nil {
		return nil, nil
	}
	// The ordinal-model row is the ONLY runtime row — resolve by
	// ordinal, loud on a miss.
	if row, ok := evalCtx.(OrdinalRow); ok {
		return f.evaluateOrdinal(row)
	}
	if rc, ok := evalCtx.(*RowEvalContext); ok {
		if rc.Positional != nil {
			return f.evaluateOrdinal(rc.Positional)
		}
	}
	// A protobuf MESSAGE context is a STRUCT value being descended — the
	// grouping path's primitive leaf accessors
	// (PrimitiveAccessorsForType) chain FieldValue over a struct-typed
	// child whose row value is the raw message. Java's FieldValue.eval
	// resolves exactly this via MessageHelpers.getFieldOnMessage; Go's
	// twin is protoFieldByName, the same struct descent the executor's
	// record→row layer uses (upper/lower name folding included). An
	// ABSENT field is SQL NULL (presence check inside); a field name the
	// descriptor lacks falls through to the loud tail — that is a
	// planner bug, not data.
	if pm, ok := evalCtx.(interface{ ProtoReflect() protoreflect.Message }); ok {
		if v, found := protoFieldByName(pm.ProtoReflect(), f.Field); found {
			return v, nil
		}
	} else if m, ok := evalCtx.(protoreflect.Message); ok {
		if v, found := protoFieldByName(m, f.Field); found {
			return v, nil
		}
	}
	// Unrecognized NON-NIL context: nothing resolved. Production flows an
	// OrdinalRow / *RowEvalContext(+Positional) / CorrelationBinder /
	// proto message (struct descent) / nil — reaching
	// here is a planner/executor bug, LOUD for pinned and unpinned alike;
	// a silent NULL would hide it. (A nil context is the
	// appendNullLeg NULL, handled above.)
	return nil, &UnboundEvalContextError{Field: f.Field, CtxType: fmt.Sprintf("%T", evalCtx)}
}

func (f *fieldValue) evaluateCorrelated(qov *quantifiedObjectValue, evalCtx any) (any, error) {
	// nil context = NULL for baked and lazy alike — the
	// appendNullLeg / nil-binding NULL, mirroring
	// Evaluate's own nil arm. The loud tail guard below is only for
	// unrecognized NON-nil contexts.
	if evalCtx == nil {
		return nil, nil
	}
	// The ordinal-model row is the ONLY runtime row — resolve by
	// ordinal, loud on a miss.
	switch ctx := evalCtx.(type) {
	case OrdinalRow:
		// A bare ordinal-model row IS the single non-join frontier
		// quantifier's row — a correlated reference to that quantifier
		// resolves by ordinal against it, loud on a miss. An UNPINNED
		// leg-relative baked reference over a MULTI-LEG row is the one
		// exception: its ROOT ordinal addresses its source's own window, which
		// this row is not — reading it here would silently serve another leg's
		// slot (correct-or-loud). Keyed on the ROOT's leg-relativity, not the
		// accessor count, so a FUSED (multi-accessor) unpinned twin of the same
		// reference goes loud too rather than reading+descending a foreign slot.
		if f.RootIsLegRelativeUnpinned() && rowIsMultiLeg(ctx) {
			return nil, &UnboundEvalContextError{Field: f.Field, Correlation: qov.correlation.Name(), CtxType: "OrdinalRow (multi-leg row cannot serve a source-relative ordinal)"}
		}
		return f.evaluateOrdinal(ctx)
	case *RowEvalContext:
		if ctx.Correlations != nil {
			if bound, ok := ctx.Correlations.GetCorrelationBinding(qov.correlation); ok {
				// A quantifier bound to an ordinal-model row resolves by
				// ordinal; anything else is a DATUM binding (see
				// evaluateDatumBinding).
				if row, ok := bound.(OrdinalRow); ok {
					return f.evaluateOrdinal(row)
				}
				return f.evaluateDatumBinding(bound)
			}
		}
		// No explicit correlation binding matched, so the reference is to
		// the frontier quantifier itself — resolve by ordinal against the
		// authoritative positional row, loud on a miss. An UNPINNED
		// leg-relative baked reference over a MULTI-LEG row must NOT fall
		// through: its ROOT ordinal addresses its source's own window and the
		// leg binder above already declined the correlation — a whole-row read
		// would silently serve another leg's slot (correct-or-loud). Keyed on
		// the ROOT's leg-relativity, not the accessor count, so a FUSED
		// (multi-accessor) unpinned twin goes loud too.
		if ctx.Positional != nil {
			if f.RootIsLegRelativeUnpinned() && rowIsMultiLeg(ctx.Positional) {
				return nil, &UnboundEvalContextError{Field: f.Field, Correlation: qov.correlation.Name(), CtxType: "*RowEvalContext (multi-leg row cannot serve a source-relative ordinal)"}
			}
			return f.evaluateOrdinal(ctx.Positional)
		}
		// Nothing matched and no frontier row supplied: a dangling correlation.
		// Loud for pinned and unpinned alike — never a silent NULL.
		return nil, &UnboundEvalContextError{Field: f.Field, Correlation: qov.correlation.Name(), CtxType: "*RowEvalContext (correlation unbound, no positional)"}
	case CorrelationBinder:
		if bound, ok := ctx.GetCorrelationBinding(qov.correlation); ok {
			if row, ok := bound.(OrdinalRow); ok {
				return f.evaluateOrdinal(row)
			}
			return f.evaluateDatumBinding(bound)
		}
		// Correlation unbound in this binder: a dangling reference. Loud.
		return nil, &UnboundEvalContextError{Field: f.Field, Correlation: qov.correlation.Name(), CtxType: fmt.Sprintf("%T (unbound)", ctx)}
	}
	// Unrecognized NON-NIL context (no ordinal row supplied): nothing resolved.
	// Loud for pinned and unpinned alike, mirroring Evaluate's
	// own tail; a silent NULL would hide a planner/executor bug.
	return nil, &UnboundEvalContextError{Field: f.Field, Correlation: qov.correlation.Name(), CtxType: fmt.Sprintf("%T", evalCtx)}
}

// resolveOrdinal returns the 0-based ordinal of f.Field within the record type
// f.Child flows, mirroring Java's FieldValue.resolveFieldPath (name -> ordinal
// against the input Type, FieldValue.java:273). Returns (ordinal, true) when
// f.Child flows a RecordType containing f.Field; (0, false) for a nil-Child
// leaf, a non-record child, or an absent/anonymous field.
//
// The ordinal substrate is AUTHORITATIVE for every runtime read.
// evaluateOrdinal resolves
// through it, loud on a miss, no name fallback, for frontier and BAKED
// join-leg references alike (the Resolved fast path below). Side-effect-free,
// so computing it can never perturb planning. A nil-Child leaf or non-record
// child yields false and fails loud at evaluation.
func (f *fieldValue) resolveOrdinal() (int, bool) {
	// A BAKED node's position was resolved at construction
	// (newFieldValueOfOrdinal / newFieldValueWithResolvedOrdinal) — it is
	// authoritative and returned before any lazy child-type derivation:
	// positional by construction, duplicate-name-proof. The display name is
	// diagnostics-only. For a multi-accessor
	// path this is the ROOT ordinal (the slot the root read context resolves;
	// evaluateOrdinal descends the remaining accessors).
	if f.Resolved != nil {
		return f.Resolved.Root().Ordinal, true
	}
	if f.Child == nil {
		return 0, false
	}
	if _, ok := f.Child.Type().(*RecordType); !ok {
		return 0, false
	}
	// There is NO runtime name-derive fallback (no by-name lookup against rt here). A
	// FieldValue with a typed child but no baked Resolved ordinal is an
	// UNBAKED site: its ordinal must be bound at plan time. Return false so
	// evaluateOrdinal fails LOUD rather than re-deriving the ordinal by name at
	// runtime (Java binds every FieldPath ordinal at plan time; runtime never sees
	// a name — FieldValue.java:164-169).
	return 0, false
}

// SourceRelativeBaked reports whether f carries a SINGLE-accessor UNPINNED
// baked path — the construction-time bind against the
// reference's OWN source row (the resolver's declared-column-order ordinal).
// Such a node is ordinal-bound but NOT yet rebased onto any composed frontier
// (join box seed, gathered seed, merged concat, group-by output): the
// translator's rebase/collection walks over composed rows MUST rebind it
// through the walk's own authority (and count it as a leg reference), whereas
// a FrontierPinned (machinery-owned) path is final.
//
// A MULTI-ACCESSOR PATH IS NOT FINAL, and this doc used to say it was — the
// arity clause reads like a second way of being machinery-owned and is not one.
// Machinery-ownership is the FRONTIER PIN alone; arity is orthogonal to it. An
// UNPINNED multi-accessor path (a user-written nested descent, minted as one
// node with a leg-relative root) still addresses its own source row and still
// has to be rebound — but this predicate answers false for it, so a walk that
// selects candidates with SourceRelativeBaked SKIPS it. That is not a
// conservative decline; it is a silent miss, and it shipped as one: an element
// MEMBER reference was skipped, mis-resolved over the composed row, and EXISTS
// dropped every row without an error. Use RootIsLegRelativeUnpinned to ask "is
// this still leg-relative", which is the question every rebase/collection walk
// actually has; use this predicate only where SINGLE-accessor is genuinely the
// requirement. At
// runtime it reads through its source's leg window (legWindowBinder). This is
// the source-vs-machinery half of the Go two-level-lowering bridge Java has no
// analog for (see FieldPath.FrontierPinned).
func (f *fieldValue) SourceRelativeBaked() bool {
	return f.Resolved != nil && !f.Resolved.FrontierPinned && len(f.Resolved.Accessors) == 1
}

// RootIsLegRelativeUnpinned reports whether f's baked path still carries a
// LEG-RELATIVE (source-relative, machinery-UNowned) ROOT ordinal — an UNPINNED
// bake, REGARDLESS of accessor count. The root ordinal addresses the
// reference's OWN source window, so it is NOT yet valid against a composed
// frontier (a MULTI-LEG merged row, or a leg-concatenation seed RC): it must be
// rebased by the reference's leg, and until then a MULTI-LEG merged row cannot
// serve it without a leg binding (its slot N is a foreign leg's).
//
// DELIBERATELY broader than SourceRelativeBaked, which additionally requires a
// SINGLE accessor. A FUSED unpinned path (composeFieldOverField / the
// withChildren rebuild-fuse — WithSuffix inherits the inner node's unpinned
// root) keeps a leg-relative ROOT yet has len(Accessors)>1, so SourceRelativeBaked
// misses it. Two consequences the narrow predicate got WRONG:
//   - EVAL (the multi-leg fall-through guards): the guard would read a foreign
//     leg's root slot and descend into it (silently NULL on
//     descendResolvedPath's unpinned default arm, or a wrong nested value).
//     Keying on the ROOT's leg-relativity makes the fused twin go LOUD too,
//     preserving the property Java gets structurally (FieldValue.eval resolves
//     the ordinal against the CHILD's own flowed Message, never a
//     differently-composed row).
//   - PLAN-TIME leg-concat collapse (values.Replace / the select-merge box
//     collapse): the fused node kept its RAW leg-relative root and fused its
//     suffix onto the WRONG leg's seed field for any non-first leg. Keying the
//     LegAwareRootOrdinal rebase on this predicate lands the fuse on the
//     reference's OWN leg.
//
// Kept DISTINCT from SourceRelativeBaked so the OTHER plan-time single-accessor
// sites (which legitimately want the narrower shape) are untouched; only the
// two guards and the two proven-buggy leg-concat collapse sites use this.
func (f *fieldValue) RootIsLegRelativeUnpinned() bool {
	return f.Resolved != nil && !f.Resolved.FrontierPinned
}

// OrdinalIn is the FieldValue-level fail-closed domain accessor — see
// FieldPath.OrdinalIn. A LAZY node (Resolved == nil) has no ordinal and fails
// closed here; it never falls back to the display name.
func (f *fieldValue) OrdinalIn(frontier OrdinalDomain) (int, bool) {
	if f == nil {
		return 0, false
	}
	return f.Resolved.OrdinalIn(frontier)
}

// WalkValue applies visit to every node in v's subtree, pre-order.
// If visit returns false, descent into that node's children is
// skipped (siblings + ancestors continue). Rule authors use this
// for tree-wide searches — e.g. "does any sub-expression reference
// this correlation?" or "does this Value tree contain an aggregate?".
//
// Safe on nil: returns immediately. Mirrors WalkPredicate over the
// Value side of the hierarchy.
func WalkValue(v Value, visit func(Value) bool) {
	if v == nil {
		return
	}
	if !visit(v) {
		return
	}
	for _, c := range v.Children() {
		WalkValue(c, visit)
	}
}

// ValueSize returns the total node count in v (v + all
// descendants). Counterpart to PredicateSize for the Value tree.
// Rule authors use this to gate expensive rewrites that would
// otherwise explode tree size.
func ValueSize(v Value) int {
	if v == nil {
		return 0
	}
	n := 1
	for _, c := range v.Children() {
		n += ValueSize(c)
	}
	return n
}

// IsConstantValue reports whether v's Evaluate is row-context-
// independent — its value is known at plan time. True for
// ConstantValue, NullValue, BooleanValue, and any composite whose
// children are all constants (`1 + 2`, `CAST(5 AS STRING)`). False
// for FieldValue / QuantifiedObjectValue / AggregateValue and any
// composite containing them.
//
// Used by rule matchers that only fire on fully-foldable operands
// (e.g. ComparisonConstantSimplifyRule's whitelist).
func IsConstantValue(v Value) bool {
	if v == nil {
		return false
	}
	switch v.(type) {
	case *ConstantValue, *NullValue, *BooleanValue:
		return true
	case *fieldValue, *quantifiedObjectValue, *AggregateValue, *ParameterValue,
		*QuantifiedRecordValue, *ExistsValue, *ScalarSubqueryValue,
		*ObjectValue, *UnmatchedAggregateValue, *ConstantObjectValue,
		*IndexEntryObjectValue, *ParameterObjectValue:
		return false
	}
	// Composite: all children must be constant.
	children := v.Children()
	if len(children) == 0 {
		// Unknown leaf — conservatively not constant.
		return false
	}
	for _, c := range children {
		if !IsConstantValue(c) {
			return false
		}
	}
	return true
}

// EvaluateConstant attempts to fold v to a concrete literal at plan
// time. Returns (literal, true) when v is constant (per
// IsConstantValue); (nil, false) otherwise. Safe on nil (returns
// (nil, false)). Useful for rules that want to pre-compute a
// constant sub-expression without writing an `if isConstant { eval
// and wrap }` dance every time.
//
// A data-dependent runtime error from Evaluate (arithmetic overflow,
// division by zero, invalid cast, type mismatch) is reported as "not
// foldable" — (nil, false). This is the plan-time decline-to-fold
// path: the typed runtime-error family now returns via the error
// channel, so the error is swallowed here (leave the node) rather
// than surfacing a query error from the planner.
//
// Genuinely programmer-invariant panics (e.g. an AggregateValue buried
// inside a constant tree that IsConstantValue should have excluded) are
// planner bugs and now surface rather than being silently swallowed —
// the residual recover that masked them has been collapsed.
func EvaluateConstant(v Value) (out any, ok bool) {
	if v == nil || !IsConstantValue(v) {
		return nil, false
	}
	result, err := v.Evaluate(nil)
	if err != nil {
		return nil, false
	}
	return result, true
}

// ContainsAggregate reports whether v has any AggregateValue in its
// subtree. Common gate for rules that only apply to scalar
// expressions — aggregates need the accumulator path, not per-row
// Evaluate.
func ContainsAggregate(v Value) bool {
	found := false
	WalkValue(v, func(n Value) bool {
		if _, ok := n.(*AggregateValue); ok {
			found = true
			return false // stop descent
		}
		return true
	})
	return found
}

// NestedResolvedPath returns the upper-cased dotted PATH a FUSED NESTED field
// reference reads, and reports whether v is one.
//
// THE DEFINITION OF "NESTED" IS THE MULTI-ACCESSOR RESOLVED PATH, and it is one
// function because the predicate is the whole subtlety. The SQL resolver FUSES
// `n.sk` into ONE FieldValue with Resolved=[N,SK] — Java does exactly the same
// fuse (SemanticAnalyzer.lookupNestedField, SemanticAnalyzer.java:598
// `FieldValue.ofFieldsAndFuseIfPossible`) and then names the result by the
// REQUESTED IDENTIFIER `n.sk` rather than by the fused value
// (SemanticAnalyzer.java:599).
//
// `Field` cannot substitute, and the reason SURVIVED the mint fix that made it
// the LEAF name. It used to answer "what struct does this read out of", so
// `n.sk` and `n.co` shared it; it now answers "which member", so `t1.n.sk` and
// a flat `sk` share it instead. Either way it is one segment of a path and the
// question here needs all of them. Every output-naming authority must take the
// path. Reading `Field` there spells two different columns alike, and a
// name-keyed reader then serves one of them where the other was asked for.
//
// THE PATH IS QUALIFIED WHEN THE REFERENCE HAS A CHILD, and that is a decision,
// not a leak. With ≥2 FROM sources the resolver emits the reference through its
// quantifier (`resolveScopedColumn`'s correlated arm), so the FieldValue carries
// `Child = QOV(T1)` and this returns `T1.N.SK`; with one source it returns
// `N.SK`. MEASURED end-to-end: `SELECT n.sk, n.co FROM t1, t2` explains as
// `Project([T1.N#1.SK#0, T1.N#1.CO#1], ...)` against `Project([N#1.SK#0, ...])`
// for the single-source form.
//
// The qualifier is KEPT because dropping it would be the same conflation one
// level up: over `FROM t1, t2` where both declare an `n`, `T1.N.SK` and
// `T2.N.SK` are different columns and a bare `N.SK` collapses them in exactly
// the name-keyed maps this predicate exists to protect. It is also what the two
// neighbouring authorities already do for a childful reference — `sortKeyFieldRef`
// renders `LEG.COL` (cascades_translator.go) and `deriveProjectionColumnDef`'s
// non-nested arm calls `ColumnNameValue` when `Child != nil`
// (cascades_generator.go) — so qualifying here makes the nested arm agree with
// its siblings rather than inventing a third rule. The remaining asymmetry is
// ProjectionColumnName's own non-nested arm, which returns a bare `Field`; that
// arm is deliberately untouched, because changing it moves emitted names for
// every flat qualified projection and is a separate change.
//
// Java agrees, and structurally: the fused nested reference is named by the
// REQUESTED IDENTIFIER (`SemanticAnalyzer.java:599`), an `Identifier` whose
// `fullyQualifiedName()` retains its qualifiers, and the top-level projection
// then strips them for the user-visible label
// (`Identifier.withoutQualifier`, Identifier.java:101). Go does the same: the
// display label for both forms is the bare leaf `SK` — measured, so the
// qualifier is an INTERNAL slot key and never reaches the user.
//
// A path step can never contain the `#` that explainValueOrdinals escapes: a
// struct member name must be a valid protobuf identifier and DDL refuses
// anything else ("field name \"a#1\": a#1 it not a valid protobuf identifier",
// measured). That is now belt AND braces rather than the only argument — the
// escape no longer fires on an ordinal-free rendering at all. See the escape's
// own note in explainValueOrdinals.
func NestedResolvedPath(v Value) (string, bool) {
	fv, ok := v.(*fieldValue)
	if !ok || fv.Resolved == nil || len(fv.Resolved.Accessors) <= 1 {
		return "", false
	}
	return strings.ToUpper(ColumnNameValue(fv)), true
}

// ProjectionColumnName is the projection output-column NAMING CONTRACT: the
// name a projected Value's result is keyed under, alias-absent, in the
// emitted positional row's type (executeProjection's posNames). A NESTED
// FieldValue projects under its resolved PATH ("N.SK"); any other FieldValue
// under its (possibly dotted)
// Field; any other Value under its upper-cased ORDINAL-FREE rendering (a
// computed expression like `n + 1` is keyed "(N + 1)"). Shared here so the
// planner/translator side can READ a projection's output by the exact key the
// executor WRITES — reading by any other rendering (e.g. the logical layer's
// un-parenthesized "N + 1") is a loud
// OrdinalResolutionError on valid SQL.
//
// ORDINAL-FREE IS THE WHOLE POINT OF THE THIRD ARM, and it was ExplainValue
// until the corpus showed what that costs. An ordinal is a PLAN-TIME BINDING of
// a reference; a column's name is not, so a name carrying one changes when the
// same reference is baked — the lockstep ColumnNameValue exists to hold. The
// composite arm was the one route that could mint such a name (the other two
// return schema text), and it did: `SELECT id + 1` inside a CTE keyed its output
// `(C1.ID#0 + 1)`, which the enclosing projection then re-read as a FIELD whose
// text contains a `#`, so the explain escape doubled it and the slot key read
// `(C1.ID##0 + 1)`. One line of the corpus carried it (cte.yaml#25) — the escape
// mechanism is in explainValueOrdinals' fieldValue arm, which now doubles only
// when it is rendering ordinals to disambiguate from.
//
// The nested arm is NOT a special case bolted on: it is the same rule the sort
// side already applies (sortKeyExtraColumnName) and the same rule Java applies
// to every resolved reference. `Field` carries ONE segment — the struct root
// when this was written, the leaf now — so without the path `SELECT n.sk, n.co`
// emitted two slots named `N` (measured, visible to the user as duplicate
// column labels over correct data) and `SELECT t1.n.sk, sk` would emit two
// named `SK`. The path is what separates them.
func ProjectionColumnName(v Value) string {
	if path, nested := NestedResolvedPath(v); nested {
		return path
	}
	if fv, ok := v.(*fieldValue); ok {
		return fv.Field
	}
	return strings.ToUpper(ColumnNameValue(v))
}

// OutputColumnName is the projection OUTPUT-name authority: the name that keys
// the emitted positional row's slot for a projected column (executeProjection's
// posNames) and therefore the name any downstream re-reader must use on the
// ordinal frontier — the upper-cased ALIAS when the column carries one, else
// the ProjectionColumnName rendering. It lives here so every site derives the
// name from ONE rule instead of a hand-synchronized copy — two copies of this
// rule have disagreed before (the
// executor wrote alias-preferring slot names while the recursive-CTE leg wrap
// re-read by ProjectionColumnName alone — a loud OrdinalResolutionError on
// valid SQL, no fallback by design). Both sites delegate here.
func OutputColumnName(v Value, alias string) string {
	if alias != "" {
		return strings.ToUpper(alias)
	}
	return ProjectionColumnName(v)
}

// DisplayColumnName is the USER-VISIBLE label for a projected column: the
// alias when the column carries one, else its own name with the QUALIFIER
// removed — Java's Identifier.withoutQualifier, applied by the top-level
// clearQualifier (Identifier.java:101-106). `SELECT n.sk` is column SK.
//
// This is deliberately NOT OutputColumnName. The qualifier belongs in the
// internal slot key, where it is what keeps two legs' same-named columns
// apart; it does not belong in anything a user names the column by. Wherever a
// projection's name CROSSES into SQL — a result-set label, a CTE's column list
// — the display form is the authority, and disagreeing about that is how one
// recursive CTE came to declare its column N.SK while every reference to it
// resolved SK.
//
// The leaf is taken from the RESOLVED ACCESSORS, never by splitting the
// rendered name: a column may legally be named with a dot in it (`"A.ID"`), and
// a last-dot split would tear that name in half.
func DisplayColumnName(v Value, alias string) string {
	if alias != "" {
		return strings.ToUpper(alias)
	}
	if field, ok := v.(*fieldValue); ok && field.Resolved != nil &&
		len(field.Resolved.Accessors) > 0 {
		leaf := field.Resolved.Accessors[len(field.Resolved.Accessors)-1].Field
		if leaf != "" {
			return strings.ToUpper(leaf)
		}
	}
	return ProjectionColumnName(v)
}

// ProjectionOutputIdentityKey returns an opaque, boundary-safe discriminator
// for the parts of a projection's executor-visible output name that semantic
// Value identity does not already preserve.
//
// A non-empty alias is the entire output-name authority, normalized exactly as
// OutputColumnName normalizes it. Without an alias, the Value's tree shape,
// operators, literals, and correlation structure already participate in
// semantic identity; the missing discriminator is every FieldValue's rendered
// display path. In particular, baked FieldValues compare by ordinal path alone,
// while ProjectionColumnName and ExplainValue still render their Field text
// (and a multi-accessor Explain renders every resolvedAccessor.Field). Walking
// all nested FieldValues therefore distinguishes both A#0 from B#0 and
// arithmetic expressions containing those reads.
//
// CorrelationIdentifier spellings are deliberately excluded. They are
// alpha-renamable planner binders, not SQL output aliases:
// SemanticEqualsUnderAliasMap equates their Values through an AliasMap and
// SemanticHashCode is alias-invariant so hash-first memo lookup can find those
// equal expressions. A stable SQL-visible projection name is carried by an
// explicit projection alias or by FieldValue display paths, both folded here.
func ProjectionOutputIdentityKey(v Value, alias string) string {
	names := make([]string, 0, 1)
	category := byte(0) // Value-derived output name.
	if alias != "" {
		category = 1 // Explicit alias overrides the entire derived name.
		names = append(names, OutputColumnName(v, alias))
	} else {
		WalkValue(v, func(node Value) bool {
			if field, ok := node.(*fieldValue); ok {
				if field.Resolved != nil && len(field.Resolved.Accessors) > 1 {
					for _, accessor := range field.Resolved.Accessors {
						names = append(names, accessor.Field)
					}
				} else {
					names = append(names, field.Field)
				}
			}
			return true
		})
	}

	// The category byte keeps an explicit alias "X" distinct from an unaliased
	// computed expression whose only nested FieldValue is also "X": their name
	// lists match, but their emitted names are X vs the computed rendering.
	encoded := []byte{category}
	encoded = binary.AppendUvarint(encoded, uint64(len(names)))
	for _, name := range names {
		encoded = binary.AppendUvarint(encoded, uint64(len(name)))
		encoded = append(encoded, name...)
	}
	return string(encoded)
}

// ExplainValue renders a Value as a readable expression string.
// Free function rather than a Value-interface method so existing
// third-party Value impls (once the port grows) don't have to
// track another method. Walks children recursively for composite
// values like ArithmeticValue / CastValue.
//
// Output style matches SQL-ish expression rendering:
//
//	ConstantValue     → the literal as %v
//	FieldValue        → the field name
//	ArithmeticValue   → (left OP right)
//	BooleanValue      → TRUE / FALSE / NULL
//	CastValue         → CAST(child AS TypeX)
//	NullValue         → NULL
func ExplainValue(v Value) string { return explainValueOrdinals(v, true) }

// ExplainPlanValues renders one plan node's retained Value program in a stable
// local correlation namespace. Unique correlations are allocation identities,
// so their process-global numeric suffixes must not leak into plan text: doing
// so makes two structurally identical plans explain differently solely because
// another query happened to allocate aliases first.
//
// A program with one unique correlation and no bare QOV omits that root, which
// preserves the familiar `ID#0` spelling for a projection over one input. With
// multiple unique roots (or a bare scalar QOV) the roots are numbered q$0,
// q$1, ... by first structural occurrence. Named correlations — including a
// quoted user alias whose text looks like q$7 — are never rewritten.
func ExplainPlanValues(vs []Value) []string {
	unique := make([]CorrelationIdentifier, 0, 2)
	seen := make(map[CorrelationIdentifier]struct{})
	hasBareUniqueQOV := false
	add := func(correlation CorrelationIdentifier) {
		if correlation.kind != correlationKindUnique {
			return
		}
		if _, ok := seen[correlation]; ok {
			return
		}
		seen[correlation] = struct{}{}
		unique = append(unique, correlation)
	}
	for _, v := range vs {
		if qov, ok := v.(*quantifiedObjectValue); ok && qov != nil && qov.correlation.kind == correlationKindUnique {
			hasBareUniqueQOV = true
		}
		WalkValue(v, func(node Value) bool {
			switch value := node.(type) {
			case *quantifiedObjectValue:
				if value != nil {
					add(value.correlation)
				}
			case *ScalarSubqueryValue:
				if value != nil {
					add(value.Alias)
				}
			case *UnmatchedAggregateValue:
				if value != nil {
					add(value.UnmatchedID)
				}
			}
			return true
		})
	}
	aliases := make(map[CorrelationIdentifier]string, len(unique))
	if len(unique) == 1 && !hasBareUniqueQOV {
		aliases[unique[0]] = ""
	} else {
		for i, correlation := range unique {
			aliases[correlation] = "q$" + intToDec(int64(i))
		}
	}
	result := make([]string, len(vs))
	for i, v := range vs {
		result[i] = explainValueOrdinalsWithAliases(v, true, aliases)
	}
	return result
}

// ColumnNameValue renders v exactly like ExplainValue but WITHOUT the baked
// `#<ordinal>` accessor discriminators — the NAME-derivation rendering. Every
// place that derives an OUTPUT COLUMN NAME from a Value (aggregate result
// columns, group-key output columns, sort-key field refs, projection column
// defs) must use this form, never ExplainValue: a reference's column NAME
// must not change when the reference is bound to its ordinal at plan time,
// or the naming lockstep between the layers that render the
// name from DIFFERENT instances of the same reference (one baked, one lazy)
// silently breaks. ExplainValue keeps the ordinal discriminators for
// EXPLAIN/debug output, where collapsing two different reads is itself a bug.
func ColumnNameValue(v Value) string {
	// The tagged current correlation is the private owner of a physical row,
	// not a user-visible qualifier. Exact ordinal integration roots provider
	// keys on that carrier so they remain evaluable, but carrying `_current`
	// into a derived column name would turn SORT_KEY into
	// `_current.SORT_KEY`. Suppress only the tagged identifier here; an ordinary
	// user alias whose text is `_current` has a different correlation kind and
	// remains visible.
	return explainValueOrdinalsWithAliases(
		v,
		false,
		map[CorrelationIdentifier]string{CurrentCorrelation(): ""},
	)
}

// CanBridgeOrderingFieldValues reports whether two non-structurally-equal
// ordering Values may safely be reconciled by their ordinal-free column name.
//
// The bridge is deliberately narrow. SQL requested orderings are often
// plan-time-baked (COL#ordinal), while candidate orderings are rebuilt as lazy
// flat fields (COL); those two representations need to meet. Two baked fields
// never meet through this helper: different ordinals are different reads, even
// when their display names match. Childful/nested paths are also excluded
// because a leaf name does not identify their source slot.
//
// Callers must test structural equality first. This helper handles only the
// representation bridge, including the harmless case-only difference between
// two lazy flat field names.
func CanBridgeOrderingFieldValues(left, right Value) bool {
	leftField, leftOK := left.(*fieldValue)
	rightField, rightOK := right.(*fieldValue)
	if !leftOK || !rightOK ||
		leftField.Child != nil || rightField.Child != nil {
		return false
	}
	if leftField.Resolved != nil && rightField.Resolved != nil {
		return false
	}
	if leftField.Resolved != nil && len(leftField.Resolved.Accessors) != 1 {
		return false
	}
	if rightField.Resolved != nil && len(rightField.Resolved.Accessors) != 1 {
		return false
	}
	leftName := ColumnNameValue(left)
	rightName := ColumnNameValue(right)
	answer := leftName != "" && strings.EqualFold(leftName, rightName)
	// Census: this is the ANSWERING comparison a flat-dotted name can reach.
	NoteOrderingBridgeDotted(leftName, rightName, answer)
	return answer
}

func explainValueOrdinals(v Value, withOrdinals bool) string {
	return explainValueOrdinalsWithAliases(v, withOrdinals, nil)
}

func explainValueOrdinalsWithAliases(v Value, withOrdinals bool, aliases map[CorrelationIdentifier]string) string {
	if v == nil {
		return ""
	}
	switch cv := v.(type) {
	case *ConstantValue:
		if cv.Value == nil {
			return "NULL"
		}
		if s, ok := cv.Value.(string); ok {
			return "'" + s + "'"
		}
		return valueLiteralString(cv.Value)
	case *fieldValue:
		// The raw field text has '#' DOUBLED so the '#<ordinal>' suffix below is
		// unambiguous BY CONSTRUCTION: a quoted identifier may legally contain
		// '#' (the lexer's DOUBLE_QUOTE_ID accepts any non-quote character), so
		// without the escape a plain name-read of a field literally named "X#0"
		// renders identically to an ordinal read of X at slot 0. With doubling,
		// a rendering ends in an UNPAIRED '#' + digits iff it is an ordinal
		// read — the rendering is injective over (field text, ordinal).
		//
		// EXPLAIN-FORMAT pin, not identity (RFC-176 P3): plan identity was
		// once keyed on these renderings (the escape's origin, PR #446 round
		// 3) but is semantic since RFC-176 P2 — the escape stays because
		// debugging output that collapses two DIFFERENT reads is still a bug
		// (writeSemanticHash's FieldValue arm keeps the same injective
		// discriminator).
		//
		// THE ESCAPE IS SCOPED TO THE ORDINAL RENDERING, because that is the
		// only thing it disambiguates FROM. Without ordinals there is no
		// `#<ordinal>` suffix for a literal `#` to be confused with, so doubling
		// would corrupt the text while separating nothing — and this renderer's
		// ordinal-free form is the NAME form (ColumnNameValue), where the text
		// IS the contract: it keys a positional slot and a Datum map. Doubling
		// there disagreed with ProjectionColumnName's plain-field arm, which
		// returns Field verbatim, so one name-derivation route escaped a `#`
		// and its sibling did not.
		//
		// THE NAME PATH IS THEREFORE SAFE BY CONSTRUCTION, not by the schema.
		// It used to rest on DDL: a struct member name must be a valid protobuf
		// identifier, so `CREATE TYPE AS STRUCT hst ("a#1" BIGINT, ...)` is
		// rejected 42F59 `field name "a#1": a#1 it not a valid protobuf
		// identifier` (measured, pinned in the sqldriver suite). That argument
		// was load-bearing for NestedResolvedPath and it was ALSO INCOMPLETE:
		// a derived name is minted from this renderer too, and a derived name
		// is not a schema field, so DDL never constrained it. It carried an
		// ordinal — the very `#` the escape then doubled — and reached a slot
		// key as `(C1.ID##0 + 1)`. Ordinal-free name derivation removed the
		// source; the gate removes the mechanism.
		name := cv.Field
		if withOrdinals {
			name = strings.ReplaceAll(name, "#", "##")
		}
		// A multi-accessor baked path renders EVERY step as name#ordinal,
		// dot-joined (Java FieldPath.toString, FieldValue.java:428-433) — the
		// single-accessor rendering below is its one-step special case (the
		// step name IS cv.Field by construction).
		if cv.Resolved != nil && len(cv.Resolved.Accessors) > 1 {
			steps := make([]string, len(cv.Resolved.Accessors))
			for i, acc := range cv.Resolved.Accessors {
				steps[i] = acc.Field
				if withOrdinals {
					steps[i] = strings.ReplaceAll(steps[i], "#", "##") +
						"#" + strconv.Itoa(acc.Ordinal)
				}
			}
			path := strings.Join(steps, ".")
			if cv.Child != nil {
				if child := explainValueOrdinalsWithAliases(cv.Child, withOrdinals, aliases); child != "" {
					return child + "." + path
				}
			}
			return path
		}
		if cv.Child != nil {
			if child := explainValueOrdinalsWithAliases(cv.Child, withOrdinals, aliases); child != "" {
				name = child + "." + name
			}
		}
		// A baked ordinal accessor renders its ordinal (Java's FieldPath
		// `#ordinal` syntax) alongside the name: two reads of DUPLICATE-named
		// slots differ only by ordinal, and explain output rendering both as
		// the bare name would make different plans read identically (PR #446
		// round 2). FrontierPinned deliberately does NOT render: it is an
		// evaluation-contract marker, not part of the value's identity.
		// ColumnNameValue drops the discriminator: a column's NAME is the
		// same whether the reference is lazy or baked.
		if cv.Resolved != nil && withOrdinals {
			return name + "#" + strconv.Itoa(cv.Resolved.Root().Ordinal)
		}
		return name
	case *ArithmeticValue:
		return "(" + explainValueOrdinalsWithAliases(cv.Left, withOrdinals, aliases) + " " + cv.Op.symbol() + " " + explainValueOrdinalsWithAliases(cv.Right, withOrdinals, aliases) + ")"
	case *StrictRankLimitValue:
		// Renders as the strict adjustment it computes (max(0, K-1)); matches the
		// prior ArithmeticValue "(K - 1)" form so plan output is unchanged.
		return "(" + explainValueOrdinalsWithAliases(cv.K, withOrdinals, aliases) + " - 1)"
	case *BooleanValue:
		if cv.Value == nil {
			return "NULL"
		}
		if *cv.Value {
			return "TRUE"
		}
		return "FALSE"
	case *CastValue:
		return "CAST(" + explainValueOrdinalsWithAliases(cv.Child, withOrdinals, aliases) + " AS " + explainTypeName(cv.Target) + ")"
	case *PromoteValue:
		return "PROMOTE(" + explainValueOrdinalsWithAliases(cv.Child, withOrdinals, aliases) + " TO " + explainTypeName(cv.Target) + ")"
	case *RecordConstructorValue:
		parts := make([]string, 0, len(cv.Fields))
		for _, f := range cv.Fields {
			parts = append(parts, f.Name+": "+explainValueOrdinalsWithAliases(f.Value, withOrdinals, aliases))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *NullValue:
		return "NULL"
	case *AggregateValue:
		if cv.Op == AggCountStar {
			return "COUNT(*)"
		}
		return cv.Op.Symbol() + "(" + explainValueOrdinalsWithAliases(cv.Operand, withOrdinals, aliases) + ")"
	case *quantifiedObjectValue:
		if alias, ok := aliases[cv.correlation]; ok {
			return alias
		}
		return cv.correlation.Name()
	case *ScalarFunctionValue:
		parts := make([]string, len(cv.Args))
		for i, a := range cv.Args {
			parts[i] = explainValueOrdinalsWithAliases(a, withOrdinals, aliases)
		}
		return cv.FuncName + "(" + strings.Join(parts, ", ") + ")"
	case *ParameterValue:
		// Render with the same `?` sigil the grammar accepts:
		// `?` for plain positional, `?N` once an ordinal is assigned,
		// `?name` for the lexer's NAMED_PARAMETER form. Keeps Explain
		// round-trippable to recognisable SQL.
		switch {
		case cv.Ordinal > 0:
			return "?" + intToDec(int64(cv.Ordinal))
		case cv.ParamName != "":
			return "?" + cv.ParamName
		default:
			// Unnumbered positional `?` — the per-statement ordinal
			// counter isn't wired yet, so render the surface form.
			return "?"
		}
	case *PickValue:
		parts := make([]string, len(cv.Alternatives))
		for i, a := range cv.Alternatives {
			parts[i] = explainValueOrdinalsWithAliases(a, withOrdinals, aliases)
		}
		sel := explainValueOrdinalsWithAliases(cv.Selector, withOrdinals, aliases)
		return "CASE(" + sel + ", [" + strings.Join(parts, ", ") + "])"
	case *ConditionSelectorValue:
		conds := make([]string, len(cv.Implications))
		for i, c := range cv.Implications {
			conds[i] = explainValueOrdinalsWithAliases(c, withOrdinals, aliases)
		}
		return "WHEN(" + strings.Join(conds, ", ") + ")"
	case *CardinalityValue:
		// Java: ExplainTokens.addFunctionCall(FunctionNames.CARDINALITY, ...).
		// Renders `cardinality(<child>)`, e.g. `cardinality(_.int_arr)`.
		return "cardinality(" + explainValueOrdinalsWithAliases(cv.Child, withOrdinals, aliases) + ")"
	case *ScalarSubqueryValue:
		alias := cv.Alias.Name()
		if mapped, ok := aliases[cv.Alias]; ok {
			alias = mapped
		}
		// The separator belongs to the alias, not to the keyword: a program
		// whose SOLE unique root is this subquery has that root mapped to the
		// empty string by ExplainPlanValues' collapse, and the alias is then
		// absent rather than blank. `(SCALAR_SUBQUERY )` renders the collapse
		// as a missing operand.
		if alias == "" {
			return "(SCALAR_SUBQUERY)"
		}
		return "(SCALAR_SUBQUERY " + alias + ")"
	case *UnmatchedAggregateValue:
		alias := cv.UnmatchedID.Name()
		if mapped, ok := aliases[cv.UnmatchedID]; ok {
			alias = mapped
		}
		return "unmatched(" + alias + ")"
	case *ParameterObjectValue:
		return "$" + cv.ParameterName
	}
	return v.Name()
}

// explainTypeName renders a Type as a short SQL-ish name for the
// CAST / PROMOTE rendering in ExplainValue.
//
// This rendering is INJECTIVE over the types it names, and must stay that way.
// It is not merely human-facing text: it is consumed as a SEMANTIC IDENTITY KEY
// by dedupSortKeys (sort-key equality), rule_intersection_merge (comparison-key
// equality) and max_match_map (query-value → candidate-value matching during
// index selection). Two distinct types rendering to one string is an identity
// collision in all three.
//
// Both collapses that used to live here were justified by byte-stability with
// the legacy ValueType.String() output, and both were wrong:
//
//   - DOUBLE/FLOAT produce DIFFERENT VALUES (binary32 rounding vs none). This
//     one was a live defect — the walker routed CAST(x AS FLOAT) through a
//     DOUBLE-coded type so the cast never rounded, and the rendering hid it.
//   - INT/LONG produce the same int64 whenever both succeed, so no sort or
//     comparison can diverge; but they are NOT interchangeable, because
//     CAST(v AS INTEGER) raises 22F3H above 2^31 where CAST(v AS BIGINT)
//     succeeds. "Nothing downstream can tell them apart" was too strong: the
//     error path tells them apart, and an identity map has no business
//     deciding that two operations differing in whether they raise are the
//     same operation.
//
// Byte-stability with a rendering that cannot express a real difference is not
// worth keeping. Splitting them moved 8 and 20 plan-shape golden lines
// respectively, each one a cast that had been claiming to be another type.
func explainTypeName(t Type) string {
	if t == nil {
		return "UNKNOWN"
	}
	switch t.Code() {
	case TypeCodeInt:
		return "INT"
	case TypeCodeLong:
		return "BIGINT"
	case TypeCodeString:
		return "STRING"
	case TypeCodeBoolean:
		return "BOOL"
	case TypeCodeFloat:
		return "FLOAT"
	case TypeCodeDouble:
		// FLOAT and DOUBLE render DISTINCTLY. Collapsing them was harmless
		// only while the walker mapped both CAST targets onto one type, so the
		// two really were the same operation; they are now genuinely different
		// (binary32 rounding vs none) and a rendering that hides that is a name
		// that lies. It also feeds a semantic decision: dedupSortKeys keys
		// sort-key identity on this very text, so two ORDER BY keys that differ
		// only in width would read as duplicates. INT/LONG stay collapsed above
		// because they sort identically — both are int64 in the row domain —
		// so nothing downstream can tell them apart.
		return "DOUBLE"
	case TypeCodeDate:
		return "DATE"
	case TypeCodeTimestamp:
		return "TIMESTAMP"
	case TypeCodeUuid:
		return "UUID"
	}
	return "UNKNOWN"
}

// Symbol returns the SQL-text form of the arithmetic operator.
// Exposed for callers that want to render the op without going
// through ExplainValue (e.g. error messages, plan diagnostics).
// Lower-case `symbol` continues to be the package-internal alias.
func (o ArithmeticOp) Symbol() string { return o.symbol() }

func (o ArithmeticOp) symbol() string {
	switch o {
	case OpAdd:
		return "+"
	case OpSub:
		return "-"
	case OpMul:
		return "*"
	case OpDiv:
		return "/"
	case OpMod:
		return "%"
	}
	return "?"
}

func valueLiteralString(v any) string {
	switch x := v.(type) {
	case int64:
		return intToDec(x)
	case int:
		return intToDec(int64(x))
	case int32:
		return intToDec(int64(x))
	case int16:
		return intToDec(int64(x))
	case int8:
		return intToDec(int64(x))
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case string:
		return "'" + x + "'"
	case [16]byte:
		// A UUID flows through the value layer as a neutral [16]byte (RFC-162).
		// Render the canonical 36-char form so EXPLAIN reads sensibly and
		// ExplainValue-based structural equality stays injective over distinct
		// UUID constants (two UUIDs must not both collapse to "?").
		return "'" + uuid.UUID(x).String() + "'"
	case []byte:
		// SQL hex-literal form — matches formatCompareOperand, so
		// the Explain and the RHS renderer agree. Also makes
		// ExplainValue-based equality injective over byte slices:
		// `X'0102'` ≠ `X'0103'`.
		const hex = "0123456789abcdef"
		buf := make([]byte, 0, 3+2*len(x))
		buf = append(buf, 'X', '\'')
		for _, b := range x {
			buf = append(buf, hex[b>>4], hex[b&0xf])
		}
		buf = append(buf, '\'')
		return string(buf)
	case []any:
		// Paren list so different element-counts / elements render
		// differently — required for structural equality via
		// ExplainValue. Matches formatCompareOperand's IN-list form.
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil {
				parts[i] = "NULL"
				continue
			}
			if s, ok := e.(string); ok {
				parts[i] = "'" + s + "'"
				continue
			}
			parts[i] = valueLiteralString(e)
		}
		return "(" + strings.Join(parts, ", ") + ")"
	}
	return "?"
}

func intToDec(n int64) string {
	// Defer to strconv.FormatInt — the previous hand-rolled
	// implementation negated `n` before walking the digits, which
	// overflows for n == math.MinInt64 (|MinInt64| > MaxInt64) and
	// produced "-" instead of "-9223372036854775808". valueLiteralString
	// feeds into ExplainValue, and ExplainValue is the plan-cache key
	// seam — a wrong literal rendering would collide cache keys
	// across distinct queries.
	return strconv.FormatInt(n, 10)
}

// NullValue is the SQL NULL literal — evaluates to nil regardless
// of context. Not collapsed into ConstantValue{Value: nil} because
// having a dedicated type lets rule matchers check for NULL
// specifically (without also matching `Value: nil` ConstantValues
// that happen to represent a NULL literal in a non-type-annotated
// way).
type NullValue struct {
	Typ Type // type NULL was cast to; UnknownType when unconstrained
}

// NewNullValue constructs a NullValue of the given type.
func NewNullValue(typ Type) *NullValue {
	return &NullValue{Typ: typ}
}

func (*NullValue) Children() []Value         { return []Value{} }
func (*NullValue) Name() string              { return "null" }
func (*NullValue) Evaluate(any) (any, error) { return nil, nil }

// Type returns the typed-NULL annotation (UnknownType when
// unannotated). SQL NULL is always nullable so the result is forced
// to nullable regardless of how the caller stored Typ.
func (n *NullValue) Type() Type {
	if n.Typ == nil {
		return UnknownType
	}
	return WithNullability(n.Typ, true)
}

// ParameterValue is a placeholder for a prepared-statement parameter
// — `?` (positional, Ordinal>=1) or `:name` (named, Ordinal=0).
// Its concrete value is unknown at plan time, so Evaluate returns
// nil unless the eval context implements ParameterBinder. Treated
// as non-constant by IsConstantValue, so constant-fold rules
// decline to fire on `x = ?` / `x = :foo`.
//
// Plan-cache keying: ExplainValue renders a parameter as `?N` /
// `:name`, which means `WHERE x = ?` and `WHERE x = ?` for two
// different bind-values share the same Explain string — the seam a
// future plan cache will key on.
//
// Runtime evaluation goes through the ParameterBinder interface: an
// evalCtx that implements it (RowEvalContext.Binder) resolves the
// binding by ordinal/name; without a binder the value degrades to
// NULL — acceptable only for plan-time / explain-time evaluation.
type ParameterValue struct {
	Ordinal   int    // 1-based positional index; 0 ⇒ named parameter
	ParamName string // populated when Ordinal == 0
	Typ       Type   // UnknownType until upstream type inference fills it
}

// NewParameterValue constructs a positional `?` parameter (1-based).
func NewParameterValue(ordinal int) *ParameterValue {
	return &ParameterValue{Ordinal: ordinal, Typ: UnknownType}
}

// NewNamedParameterValue constructs a named `:name` parameter.
func NewNamedParameterValue(name string) *ParameterValue {
	return &ParameterValue{ParamName: name, Typ: UnknownType}
}

// ParameterBinder is an optional eval-context capability: when
// ParameterValue.Evaluate is called with a context that implements
// this interface, the parameter is resolved to its bound value.
// Otherwise Evaluate returns nil (SQL UNKNOWN), which is the safe
// default for plan-time evaluation where no bindings exist.
type ParameterBinder interface {
	BindParameter(ordinal int, name string) (any, bool)
}

// CorrelationBinder is an optional eval-context capability for
// resolving correlation bindings. When QuantifiedObjectValue.Evaluate
// is called with a context implementing this interface, it resolves the
// correlated row. Mirrors Java's EvaluationContext.getBinding(CORRELATION, alias).
type CorrelationBinder interface {
	GetCorrelationBinding(id CorrelationIdentifier) (any, bool)
}

// RowEvalContext is a composite evaluation context for Value.Evaluate that
// carries the ordinal frontier row plus prepared-statement parameters
// (ParameterBinder), correlation bindings (CorrelationBinder), and pre-evaluated
// scalar subqueries. Pass this when evaluating expressions that mix field
// references, parameters, and correlation bindings (e.g. InJoin explode aliases).
type RowEvalContext struct {
	// Positional is the authoritative ordinal-model row for the non-join
	// frontier — the SOLE runtime row. When non-nil, FieldValue resolution goes
	// through the ordinal path (the plan-time-baked ordinal, resolveOrdinal), a
	// loud OrdinalResolutionError on a miss, NO name resolution. It is the
	// single frontier quantifier's row: an outer correlation still resolves via
	// Correlations first, and only an unbound (frontier) quantifier reference
	// falls through to this row.
	Positional OrdinalRow
	// Objects is the exact-QOV binding authority for an admitted physical
	// evaluation layout. When present, QOV evaluation never falls through to
	// Positional or the alias-only legacy correlation binder.
	Objects          QuantifiedObjectBinder
	Binder           ParameterBinder
	Correlations     CorrelationBinder
	ScalarSubqueries map[CorrelationIdentifier]any // pre-evaluated scalar subquery results
	// Clock supplies the statement-stable CURRENT_TIMESTAMP-family instant
	// (StatementClock). Set from the executor's EvaluationContext so every
	// row of one statement observes the same time; nil falls back to
	// time.Now() per evaluation (the pre-statement-clock behavior).
	Clock StatementClock
}

// StatementNow implements StatementClock by delegating to the carried
// Clock; without one it degrades to the wall clock, which is the
// per-evaluation drift the statement clock exists to prevent — callers
// that need statement stability must set Clock.
func (r *RowEvalContext) StatementNow() time.Time {
	if r.Clock != nil {
		return r.Clock.StatementNow()
	}
	return time.Now().UTC()
}

func (r *RowEvalContext) BindParameter(ordinal int, name string) (any, bool) {
	if r.Binder == nil {
		return nil, false
	}
	return r.Binder.BindParameter(ordinal, name)
}

func (r *RowEvalContext) GetCorrelationBinding(id CorrelationIdentifier) (any, bool) {
	if r.Correlations == nil {
		return nil, false
	}
	return r.Correlations.GetCorrelationBinding(id)
}

func (*ParameterValue) Children() []Value { return []Value{} }
func (*ParameterValue) Name() string      { return "param" }

// Type returns the parameter's rich Type. Parameter bindings can be
// NULL so the result is forced to nullable regardless of how the
// caller stored Typ.
func (p *ParameterValue) Type() Type {
	if p.Typ == nil {
		return UnknownType
	}
	return WithNullability(p.Typ, true)
}

func (p *ParameterValue) Evaluate(evalCtx any) (any, error) {
	if evalCtx == nil {
		return nil, nil
	}
	if b, ok := evalCtx.(ParameterBinder); ok {
		v, _ := b.BindParameter(p.Ordinal, p.ParamName)
		return v, nil
	}
	return nil, nil
}

// ScalarFunctionValue is a row-scalar function call — `UPPER(name)`,
// `LENGTH(str)`, etc. Args carries the evaluated sub-Values; Name is
// the canonical (UPPER-CASE) function identifier as it appears in the
// catalog. Children returns Args so IsConstantValue / WalkValue
// recurse normally — `UPPER('foo')` is a constant composite and folds
// via EvaluateConstant; `UPPER(name)` is non-constant because the
// FieldValue arg is non-constant.
//
// The supported family is registered in scalarFunctionCatalog (string,
// math, date-part, bit, and null/comparison helpers). The same definition
// selects evaluator dispatch, result typing, and Cascades admission.
type ScalarFunctionValue struct {
	FuncName string
	Args     []Value
	Typ      Type
}

// NewScalarFunctionValue builds a ScalarFunctionValue. The function
// name is upper-cased so callers can pass case-insensitive identifiers.
func NewScalarFunctionValue(name string, typ Type, args ...Value) *ScalarFunctionValue {
	return &ScalarFunctionValue{FuncName: strings.ToUpper(name), Args: args, Typ: typ}
}

func (s *ScalarFunctionValue) Children() []Value {
	if len(s.Args) == 0 {
		return []Value{}
	}
	return s.Args
}
func (*ScalarFunctionValue) Name() string { return "scalarfn" }

// Type returns the scalar function's rich result Type. Most scalar
// functions can return NULL on NULL input — the result is forced to
// nullable regardless of how the caller stored Typ.
func (s *ScalarFunctionValue) Type() Type {
	if s.Typ == nil {
		return UnknownType
	}
	return WithNullability(s.Typ, true)
}

func (s *ScalarFunctionValue) Evaluate(evalCtx any) (any, error) {
	result, err := s.evaluateUncoerced(evalCtx)
	if err != nil || result == nil {
		return result, err
	}
	return coerceNumericResult(result, s.Type()), nil
}

// evaluateUncoerced applies the function's semantic operator. Evaluate wraps
// its result with the declared-type carrier conversion so a statically DOUBLE
// scalar cannot feed an int64 into downstream arithmetic.
func (s *ScalarFunctionValue) evaluateUncoerced(evalCtx any) (any, error) {
	// SHORT-CIRCUITING forms evaluate arguments lazily — SQL requires
	// that COALESCE stop at the first non-NULL argument and that IF
	// evaluate only the taken branch, so `COALESCE(1, 1/0)` is 1 and
	// `IF(true, x, 1/0)` is x, never a 22012. The eager loop below
	// evaluated every argument first and turned these legal
	// expressions into runtime errors.
	definition, knownFunction := scalarFunctionDefinitionFor(s.FuncName)
	if knownFunction {
		switch definition.operator {
		case scalarFunctionCoalesce, scalarFunctionIfNull:
			// IFNULL is the strictly 2-arg COALESCE spelling — the lazy arm
			// must keep the strict arm's arity decline (nil, nil), not
			// degrade IFNULL(1) / IFNULL(a,b,c) into variadic COALESCE.
			if definition.operator == scalarFunctionIfNull && len(s.Args) != 2 {
				return nil, nil
			}
			for _, a := range s.Args {
				if a == nil {
					return nil, nil
				}
				av, err := a.Evaluate(evalCtx)
				if err != nil {
					return nil, err
				}
				if av != nil {
					return av, nil
				}
			}
			return nil, nil
		case scalarFunctionIf:
			if len(s.Args) != 3 || s.Args[0] == nil {
				return nil, nil
			}
			cond, err := s.Args[0].Evaluate(evalCtx)
			if err != nil {
				return nil, err
			}
			branch, ok := scalarIfBranch(cond)
			if !ok {
				// Unsupported condition type — decline like the strict arm.
				return nil, nil
			}
			if s.Args[branch] == nil {
				return nil, nil
			}
			return s.Args[branch].Evaluate(evalCtx)
		}
	}
	args := make([]any, len(s.Args))
	for i, a := range s.Args {
		if a == nil {
			return nil, nil
		}
		av, err := a.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		args[i] = av
	}
	if knownFunction && definition.argumentStrategy == scalarFunctionCommonNumericArguments {
		for i := range args {
			args[i] = coerceNumericResult(args[i], s.Type())
		}
	}
	return evalScalarFunctionCtx(s.FuncName, args, evalCtx)
}

// coerceNumericResult keeps a Go carrier aligned with its static numeric type.
// FLOAT computes at float32 precision but uses the row domain's float64
// carrier; DOUBLE widens every integral carrier.
func coerceNumericResult(result any, resultType Type) any {
	if resultType == nil {
		return result
	}
	switch resultType.Code() {
	case TypeCodeDouble:
		if numeric, _, ok := ToFloat64(result); ok {
			return numeric
		}
	case TypeCodeFloat:
		if numeric, ok := toFloat32Operand(result); ok {
			return float64(numeric)
		}
	}
	return result
}

// scalarIfBranch maps an evaluated IF/IIF condition to the argument
// index of the branch to take (1 = then, 2 = else). Truthy: non-zero
// numeric, non-empty string, true bool; NULL takes the else branch
// (SQL §6.30 — NULL is not truthy). Single authority shared with the
// strict evalScalarFunction arm so the two can never diverge.
func scalarIfBranch(cond any) (int, bool) {
	switch v := cond.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 2, true
	case int64:
		if v != 0 {
			return 1, true
		}
		return 2, true
	case float64:
		if v != 0 {
			return 1, true
		}
		return 2, true
	case string:
		if v != "" {
			return 1, true
		}
		return 2, true
	case nil:
		return 2, true
	}
	return 0, false
}

// StatementClock is the optional evalCtx capability supplying the
// statement-stable timestamp: SQL fixes CURRENT_TIMESTAMP / CURRENT_DATE
// / CURRENT_TIME per STATEMENT, so every reference inside one statement
// must observe the same instant. Evaluation contexts that carry a
// statement time (the executor's EvaluationContext, the INSERT-VALUES
// fold) implement this; without it the arms fall back to time.Now().
type StatementClock interface {
	StatementNow() time.Time
}

// DependsOnStatementClock reports whether the value tree contains a
// CURRENT_TIMESTAMP-family function — one whose result is defined by
// the statement clock rather than by its (zero) arguments. The executor
// computes this ONCE per operator to decide whether a bare frontier row
// must be wrapped in a clock-bearing RowEvalContext: evaluating such a
// value against a bare OrdinalRow falls back to per-row time.Now() and
// drifts across the rows of one statement, which SQL forbids.
func DependsOnStatementClock(v Value) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(*ScalarFunctionValue); ok {
		if def, known := scalarFunctionDefinitionFor(s.FuncName); known {
			switch def.operator {
			case scalarFunctionStatementTimestamp, scalarFunctionStatementDate:
				return true
			}
		}
	}
	for _, c := range v.Children() {
		if DependsOnStatementClock(c) {
			return true
		}
	}
	return false
}

// evalScalarFunction dispatches catalogued scalar operators, including the
// internal IF/NULLIF forms that are deliberately excluded from Cascades SQL
// admission. NULL argument propagates to NULL result (SQL standard), returned
// as (nil, nil). Genuine decline edges — unknown function, wrong arity, a
// non-coercible arg type, or an out-of-domain math input that SQL degrades to
// NULL — also return (nil, nil): the value becomes SQL NULL rather than
// erroring. The data-dependent error edges return a typed error so the
// executor maps it to a SQLSTATE:
//
//   - ABS(MinInt64)             → *ArithmeticOverflowError       (22003)
//   - MOD(x, 0)                 → *ArithmeticDivisionByZeroError (22012)
//   - SQRT(negative)            → *InvalidArgumentError          (22023)
//   - GREATEST/LEAST mixed type → *ScalarTypeMismatchError       (22000)
//
// (nil, nil) is SQL NULL; (nil, err) is a runtime error — the two are now
// unambiguous, which is the whole point of the error channel.
// scalarArgString renders a scalar-function argument as a string. A UUID flows
// through the value layer as a neutral [16]byte (RFC-162); a bare fmt.Sprintf
// "%v" would print it as a Go array literal ("[85 14 …]"), so string functions
// (CONCAT, SUBSTRING, REPLACE, …) over a UUID column would emit garbage. Render
// it as the canonical 36-char form, matching Java (where a UUID arg is a
// java.util.UUID whose toString() is canonical).
func scalarArgString(a any) string {
	if b, ok := a.([16]byte); ok {
		return uuid.UUID(b).String()
	}
	return fmt.Sprintf("%v", a)
}

// timestampParseLayouts is the SINGLE authority for the string forms the
// engine accepts as timestamps — the TIMESTAMP cast and the date-part
// functions parse the SAME set, so a string castable to TIMESTAMP can
// never be "not a date/time value" to YEAR() (the date-part arm appends
// the bare time-only layout for HOUR/MINUTE/SECOND over "15:04:05").
var timestampParseLayouts = []string{timestampLayout, "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05", dateLayout}

// evalScalarFunctionCtx is evalScalarFunction with the evalCtx threaded
// for the statement-clock arms (CURRENT_TIMESTAMP family); every other
// function ignores the context.
func evalScalarFunctionCtx(name string, args []any, evalCtx any) (any, error) {
	definition, ok := scalarFunctionDefinitionFor(name)
	if !ok {
		return nil, nil
	}
	switch definition.operator {
	case scalarFunctionStatementTimestamp:
		return statementTime(evalCtx).Format(timestampLayout), nil
	case scalarFunctionStatementDate:
		return statementTime(evalCtx).Format(dateLayout), nil
	}
	return evalScalarFunction(name, args)
}

// statementTime resolves the statement-stable instant from the evalCtx
// (StatementClock) — every CURRENT_* reference in one statement observes
// the same time, per SQL — falling back to time.Now() for contexts that
// carry no clock.
func statementTime(evalCtx any) time.Time {
	if c, ok := evalCtx.(StatementClock); ok {
		return c.StatementNow().UTC()
	}
	return time.Now().UTC()
}

func evalScalarFunction(name string, args []any) (any, error) {
	definition, ok := scalarFunctionDefinitionFor(name)
	if !ok {
		return nil, nil
	}

	switch definition.operator {
	case scalarFunctionUpper:
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.ToUpper(s), nil
	case scalarFunctionLower:
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.ToLower(s), nil
	case scalarFunctionLength:
		// Rune count. Go accepts []byte for symmetry with OCTET_LENGTH
		// (byte count there, rune count here).
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		switch v := args[0].(type) {
		case string:
			return int64(utf8.RuneCountInString(v)), nil
		case []byte:
			return int64(utf8.RuneCount(v)), nil
		}
		return nil, nil
	case scalarFunctionOctetLength:
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		switch v := args[0].(type) {
		case string:
			return int64(len(v)), nil
		case []byte:
			return int64(len(v)), nil
		}
		return nil, nil
	case scalarFunctionAbs:
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		switch n := args[0].(type) {
		case int64:
			// MinInt64 abs overflows (two's-complement: -MinInt64 wraps
			// back to MinInt64). Surface 22003 NUMERIC_VALUE_OUT_OF_RANGE.
			if n == math.MinInt64 {
				return nil, &ArithmeticOverflowError{}
			}
			if n < 0 {
				return -n, nil
			}
			return n, nil
		case float64:
			return math.Abs(n), nil
		}
		return nil, nil
	case scalarFunctionFloor, scalarFunctionCeil, scalarFunctionRound:
		if len(args) < 1 || args[0] == nil {
			return nil, nil
		}
		if definition.operator == scalarFunctionRound {
			if len(args) > 2 {
				return nil, nil
			}
		} else if len(args) != 1 {
			return nil, nil
		}
		decimals := int64(0)
		if len(args) == 2 {
			if args[1] == nil {
				return nil, nil
			}
			d, ok := scalarFnInt64Arg(args[1])
			if !ok {
				return nil, nil
			}
			decimals = d
		}
		var f float64
		switch n := args[0].(type) {
		case int64:
			if definition.operator != scalarFunctionRound || decimals >= 0 {
				// FLOOR, CEIL, and non-negative-precision ROUND are identities
				// on integers.
				return n, nil
			}
			rounded, ok := roundInt64DecimalPlaces(n, decimals)
			if !ok {
				return nil, &ArithmeticOverflowError{}
			}
			return rounded, nil
		case float64:
			f = n
		default:
			return nil, nil
		}
		var result float64
		switch definition.operator {
		case scalarFunctionFloor:
			result = math.Floor(f)
		case scalarFunctionCeil:
			result = math.Ceil(f)
		case scalarFunctionRound:
			if decimals == 0 {
				result = math.Round(f)
			} else {
				result = roundFloat64DecimalPlaces(f, decimals)
			}
		}
		// Preserve the compact direct-evaluator carrier. ScalarFunctionValue
		// converts it to the declared FLOAT/DOUBLE carrier at its boundary.
		if result == math.Trunc(result) && float64FitsInt64(result) {
			return int64(result), nil
		}
		return result, nil
	case scalarFunctionPi:
		// Zero-argument constant.
		if len(args) != 0 {
			return nil, nil
		}
		return math.Pi, nil
	case scalarFunctionSqrt:
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		f, _, ok := ToFloat64(args[0])
		if !ok {
			return nil, nil
		}
		if f < 0 {
			// SQL §6.27: SQRT of a negative argument raises 22023
			// INVALID_PARAMETER_VALUE.
			return nil, &InvalidArgumentError{
				Message: fmt.Sprintf("SQRT of negative number: %v", f),
			}
		}
		return math.Sqrt(f), nil
	case scalarFunctionPower:
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		base, _, bok := ToFloat64(args[0])
		exp, _, eok := ToFloat64(args[1])
		if !bok || !eok {
			return nil, nil
		}
		result := math.Pow(base, exp)
		if math.IsNaN(result) || math.IsInf(result, 0) {
			return nil, nil
		}
		if result == math.Trunc(result) && float64FitsInt64(result) {
			return int64(result), nil
		}
		return result, nil
	case scalarFunctionCoalesce:
		// First non-nil argument wins; all nil → nil. Empty argument
		// list also folds to nil so a degenerate `COALESCE()` doesn't
		// error at plan time (the parser rejects zero-arg COALESCE
		// anyway, so this is just a defensive default).
		for _, a := range args {
			if a != nil {
				return a, nil
			}
		}
		return nil, nil
	case scalarFunctionNullIf:
		// NULLIF(a, b) → NULL when a == b; otherwise a. Compare via
		// nullifEqual so int/float values compare after numeric promotion.
		if len(args) != 2 {
			return nil, nil
		}
		if args[0] == nil {
			return nil, nil
		}
		if args[1] != nil && nullifEqual(args[0], args[1]) {
			return nil, nil
		}
		return args[0], nil
	case scalarFunctionTrim:
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.TrimSpace(s), nil
	case scalarFunctionLTrim:
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.TrimLeft(s, " \t\n\r"), nil
	case scalarFunctionRTrim:
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.TrimRight(s, " \t\n\r"), nil
	case scalarFunctionConcat:
		// Postgres CONCAT semantics — NULL skips, doesn't poison (unlike
		// MySQL CONCAT, which returns NULL if any arg is NULL).
		// Pinned by trim_concat.yaml.
		var b strings.Builder
		for _, a := range args {
			if a == nil {
				continue
			}
			b.WriteString(scalarArgString(a))
		}
		return b.String(), nil
	case scalarFunctionConcatWS:
		// CONCAT With Separator — MySQL semantics: first arg is the
		// separator (NULL → result is NULL); remaining args are
		// concatenated with the separator between non-NULL values.
		// NULL elements are skipped.
		if len(args) < 1 || args[0] == nil {
			return nil, nil
		}
		sep, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		var b strings.Builder
		first := true
		for _, a := range args[1:] {
			if a == nil {
				continue
			}
			if !first {
				b.WriteString(sep)
			}
			b.WriteString(scalarArgString(a))
			first = false
		}
		return b.String(), nil
	case scalarFunctionSubstring:
		// SUBSTRING(s, pos[, len]) — 1-based position per SQL standard.
		// pos < 1 normalises to 1.
		if len(args) < 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		s := scalarArgString(args[0])
		pos, ok := scalarFnInt64Arg(args[1])
		if !ok {
			return nil, nil
		}
		if pos < 1 {
			pos = 1
		}
		runes := []rune(s)
		start := int(pos) - 1
		if start >= len(runes) {
			return "", nil
		}
		if len(args) >= 3 {
			if args[2] == nil {
				return nil, nil
			}
			n, ok := scalarFnInt64Arg(args[2])
			if !ok {
				return nil, nil
			}
			end := start + int(n)
			if end > len(runes) {
				end = len(runes)
			}
			if end < start {
				return "", nil
			}
			return string(runes[start:end]), nil
		}
		return string(runes[start:]), nil
	case scalarFunctionReplace:
		// REPLACE(s, from, to). NULL `to` is treated as empty.
		// Non-string args coerce via fmt.Sprintf("%v", v).
		if len(args) != 3 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		toStr := ""
		if args[2] != nil {
			toStr = scalarArgString(args[2])
		}
		return strings.ReplaceAll(scalarArgString(args[0]), scalarArgString(args[1]), toStr), nil
	case scalarFunctionSign:
		// SIGN(numeric) — -1 / 0 / 1 in the input's numeric type.
		// Non-numeric input declines so the runtime evaluator surfaces 22018.
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		switch n := args[0].(type) {
		case int64:
			switch {
			case n > 0:
				return int64(1), nil
			case n < 0:
				return int64(-1), nil
			}
			return int64(0), nil
		case float64:
			switch {
			case n > 0:
				return float64(1), nil
			case n < 0:
				return float64(-1), nil
			}
			return float64(0), nil
		}
		return nil, nil
	case scalarFunctionMod:
		// MOD(a, b) — int64%int64 stays int64, mixed promotes to float64
		// via math.Mod. Division-by-zero errors with 22012
		// DIVISION_BY_ZERO.
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		ai, aIsInt := args[0].(int64)
		bi, bIsInt := args[1].(int64)
		if aIsInt && bIsInt {
			if bi == 0 {
				return nil, &ArithmeticDivisionByZeroError{}
			}
			return ai % bi, nil
		}
		af, _, aok := ToFloat64(args[0])
		bf, _, bok := ToFloat64(args[1])
		if !aok || !bok {
			return nil, nil
		}
		if bf == 0 {
			return nil, &ArithmeticDivisionByZeroError{}
		}
		return math.Mod(af, bf), nil
	case scalarFunctionIfNull:
		// IFNULL(a, b) — `a` if non-null, else `b`. 2-arg COALESCE alias
		// (MySQL/SQLite spelling).
		if len(args) != 2 {
			return nil, nil
		}
		if args[0] != nil {
			return args[0], nil
		}
		return args[1], nil
	case scalarFunctionIf:
		// IF(cond, then, else) — evaluates condition first; returns
		// `then` if truthy, `else` otherwise. Truthy: non-zero numeric,
		// non-empty string, true bool.
		if len(args) != 3 {
			return nil, nil
		}
		if branch, ok := scalarIfBranch(args[0]); ok {
			return args[branch], nil
		}
		// Unsupported condition type — decline so runtime can error.
		return nil, nil
	case scalarFunctionGreatest, scalarFunctionLeast:
		// GREATEST/LEAST — Java conformance: any NULL arg → NULL result
		// (Postgres skips, Oracle and Java propagate). Cross-type comparisons
		// error with 22000 CANNOT_CONVERT_TYPE.
		if len(args) == 0 {
			return nil, nil
		}
		isGreatest := definition.operator == scalarFunctionGreatest
		best := args[0]
		if best == nil {
			return nil, nil
		}
		for _, a := range args[1:] {
			if a == nil {
				return nil, nil
			}
			cmp, ok := compareScalar(best, a)
			if !ok {
				return nil, &ScalarTypeMismatchError{
					Message: fmt.Sprintf("incompatible types for %s: %T vs %T", name, best, a),
				}
			}
			if (isGreatest && cmp < 0) || (!isGreatest && cmp > 0) {
				best = a
			}
		}
		return best, nil
	case scalarFunctionExp:
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		f, _, ok := ToFloat64(args[0])
		if !ok {
			return nil, nil
		}
		result := math.Exp(f)
		// Overflow (e.g. EXP(1000) → +Inf) and NaN degrade to SQL NULL,
		// matching the POWER/SQRT out-of-domain convention above.
		if math.IsInf(result, 0) || math.IsNaN(result) {
			return nil, nil
		}
		return result, nil
	case scalarFunctionLn:
		// Natural log. Domain: x > 0. Out-of-domain (≤ 0) declines to
		// SQL NULL; SQRT<0 is the typed-error math-domain edge.
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		f, _, ok := ToFloat64(args[0])
		if !ok || f <= 0 {
			return nil, nil
		}
		return math.Log(f), nil
	case scalarFunctionLog:
		// 1-arg LOG(x) = log10(x). 2-arg LOG(base, x) = ln(x)/ln(base).
		// Out-of-domain inputs decline to SQL NULL.
		switch len(args) {
		case 1:
			if args[0] == nil {
				return nil, nil
			}
			f, _, ok := ToFloat64(args[0])
			if !ok || f <= 0 {
				return nil, nil
			}
			return math.Log10(f), nil
		case 2:
			if args[0] == nil || args[1] == nil {
				return nil, nil
			}
			base, _, baseOK := ToFloat64(args[0])
			x, _, xOK := ToFloat64(args[1])
			if !baseOK || !xOK || base <= 0 || base == 1 || x <= 0 {
				return nil, nil
			}
			return math.Log(x) / math.Log(base), nil
		}
		return nil, nil
	case scalarFunctionReverse:
		// String reverse — rune-aware so multibyte UTF-8 stays valid.
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s := scalarArgString(args[0])
		runes := []rune(s)
		for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
			runes[i], runes[j] = runes[j], runes[i]
		}
		return string(runes), nil
	case scalarFunctionPosition:
		// POSITION(substr, str) — 1-based rune index of first match,
		// 0 if not found (not the `POSITION(substr IN str)` SQL-standard
		// grammar shape).
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		needle := scalarArgString(args[0])
		haystack := scalarArgString(args[1])
		byteIdx := strings.Index(haystack, needle)
		if byteIdx < 0 {
			return int64(0), nil
		}
		return int64(utf8.RuneCountInString(haystack[:byteIdx]) + 1), nil
	case scalarFunctionLeft:
		// LEFT(str, n) — first n runes; whole string if n ≥ length.
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		s := scalarArgString(args[0])
		n, ok := scalarFnInt64Arg(args[1])
		if !ok {
			return nil, nil
		}
		if n < 0 {
			n = 0
		}
		runes := []rune(s)
		if int(n) >= len(runes) {
			return s, nil
		}
		return string(runes[:n]), nil
	case scalarFunctionRight:
		// RIGHT(str, n) — last n runes; whole string if n ≥ length.
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		s := scalarArgString(args[0])
		n, ok := scalarFnInt64Arg(args[1])
		if !ok {
			return nil, nil
		}
		if n < 0 {
			n = 0
		}
		runes := []rune(s)
		if int(n) >= len(runes) {
			return s, nil
		}
		return string(runes[len(runes)-int(n):]), nil
	case scalarFunctionBitAnd:
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		a, aok := args[0].(int64)
		b, bok := args[1].(int64)
		if !aok || !bok {
			return nil, nil
		}
		return a & b, nil
	case scalarFunctionBitOr:
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		a, aok := args[0].(int64)
		b, bok := args[1].(int64)
		if !aok || !bok {
			return nil, nil
		}
		return a | b, nil
	case scalarFunctionBitXor:
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		a, aok := args[0].(int64)
		b, bok := args[1].(int64)
		if !aok || !bok {
			return nil, nil
		}
		return a ^ b, nil
	case scalarFunctionBitmapBucketOffset:
		// Java ArithmeticValue BITMAP_BUCKET_OFFSET_* (:513-514):
		// floorDiv(l, r) * r.
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		a, aok := scalarFnInt64Arg(args[0])
		b, bok := scalarFnInt64Arg(args[1])
		if !aok || !bok || b == 0 {
			return nil, nil
		}
		// Math.multiplyExact, not `*`: for the minimum BIGINT with the default
		// entry size 10000 the floor-divided quotient times 10000 falls below
		// MinInt64, and an unchecked multiply wraps it to a bogus POSITIVE
		// bucket offset. Java raises ArithmeticException here.
		prod, ok := mulInt64Checked(floorDivInt64(a, b), b)
		if !ok {
			return nil, &ArithmeticOverflowError{}
		}
		return prod, nil
	case scalarFunctionBitmapBitPosition:
		// Java ArithmeticValue BITMAP_BIT_POSITION_* (:519-520):
		// l - floorDiv(l, r) * r (a floor-mod).
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		a, aok := scalarFnInt64Arg(args[0])
		b, bok := scalarFnInt64Arg(args[1])
		if !aok || !bok || b == 0 {
			return nil, nil
		}
		// Java composes subtractExact over multiplyExact
		// (ArithmeticValue.java:519-520); the INTERMEDIATE product overflows
		// first, so both steps are checked.
		prod, ok := mulInt64Checked(floorDivInt64(a, b), b)
		if !ok {
			return nil, &ArithmeticOverflowError{}
		}
		diff, ok := subInt64Checked(a, prod)
		if !ok {
			return nil, &ArithmeticOverflowError{}
		}
		return diff, nil
	case scalarFunctionDatePart:
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			// Also handle time.Time if the argument was already parsed —
			// normalized to UTC like the string path, so the parts never
			// depend on the carrier's zone representation.
			if t, tok := args[0].(time.Time); tok {
				return datePartFromTime(name, t.UTC()), nil
			}
			return nil, nil
		}
		var t time.Time
		var err error
		for _, layout := range append(timestampParseLayouts[:len(timestampParseLayouts):len(timestampParseLayouts)], "15:04:05") {
			t, err = time.Parse(layout, s)
			if err == nil {
				break
			}
		}
		if err != nil {
			// A non-NULL string that parses as NO supported date/time
			// layout is DATA-DEPENDENT garbage input — a typed 22023,
			// like SQRT(negative), never a silent NULL. The NULL-degrade
			// contract covers TYPE declines (non-string, wrong arity),
			// not malformed data: folding a typo to NULL silently
			// corrupts an INSERT.
			return nil, &InvalidArgumentError{Message: fmt.Sprintf("cannot extract %s from %q: not a date/time value", name, s)}
		}
		// Normalize to UTC before extraction — the TIMESTAMP cast
		// canonicalizes zoned forms to UTC, and the date parts must
		// agree with it (HOUR('…T03:04:05+02:00') is 1, not 3).
		return datePartFromTime(name, t.UTC()), nil
		// CURRENT_TIMESTAMP / CURRENT_DATE / CURRENT_TIME / LOCALTIME
		// live in evalScalarFunctionCtx — they need the evalCtx's
		// StatementClock; a single authority so the strict path can
		// never fork the timestamp semantics.
	}
	return nil, nil
}

// datePartFromTime extracts an integer date-part from a time.Time value.
// DAYOFWEEK uses MySQL convention: Sunday=1 .. Saturday=7.
func datePartFromTime(name string, t time.Time) int64 {
	switch name {
	case "YEAR":
		return int64(t.Year())
	case "MONTH":
		return int64(t.Month())
	case "DAY", "DAYOFMONTH":
		return int64(t.Day())
	case "HOUR":
		return int64(t.Hour())
	case "MINUTE":
		return int64(t.Minute())
	case "SECOND":
		return int64(t.Second())
	case "DAYOFWEEK":
		return int64(t.Weekday()) + 1
	case "DAYOFYEAR":
		return int64(t.YearDay())
	}
	return 0
}

// compareScalar returns -1 / 0 / 1 for a < b / a == b / a > b under the
// package's numeric/string/bool comparison rules. Returns ok=false on
// cross-type pairs it can't compare (the runtime reports the
// CANNOT_CONVERT_TYPE error per Java alignment).
func compareScalar(a, b any) (int, bool) {
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			switch {
			case av < bv:
				return -1, true
			case av > bv:
				return 1, true
			}
			return 0, true
		case float64:
			af := float64(av)
			switch {
			case af < bv:
				return -1, true
			case af > bv:
				return 1, true
			}
			return 0, true
		}
	case float64:
		switch bv := b.(type) {
		case int64:
			bf := float64(bv)
			switch {
			case av < bf:
				return -1, true
			case av > bf:
				return 1, true
			}
			return 0, true
		case float64:
			switch {
			case av < bv:
				return -1, true
			case av > bv:
				return 1, true
			}
			return 0, true
		}
	case string:
		bv, ok := b.(string)
		if !ok {
			return 0, false
		}
		return strings.Compare(av, bv), true
	case bool:
		bv, ok := b.(bool)
		if !ok {
			return 0, false
		}
		switch {
		case !av && bv:
			return -1, true
		case av && !bv:
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// roundInt64DecimalPlaces rounds an integer at a negative decimal precision
// without converting through float64. Ties round away from zero, matching the
// exact-numeric SQL/MySQL ROUND contract. ok=false means the rounded magnitude
// no longer fits int64.
func roundInt64DecimalPlaces(value, decimals int64) (int64, bool) {
	if decimals >= 0 || value == 0 {
		return value, true
	}
	if decimals < -19 {
		return 0, true
	}

	places := -decimals // safe after the -19 bound above
	factor := uint64(1)
	for range places {
		factor *= 10
	}

	negative := value < 0
	magnitude := uint64(value)
	if negative {
		magnitude = -magnitude
	}
	quotient, remainder := magnitude/factor, magnitude%factor
	if remainder >= factor/2 {
		quotient++
	}
	roundedMagnitude := quotient * factor
	if negative {
		const minInt64Magnitude = uint64(1) << 63
		if roundedMagnitude > minInt64Magnitude {
			return 0, false
		}
		if roundedMagnitude == minInt64Magnitude {
			return math.MinInt64, true
		}
		return -int64(roundedMagnitude), true
	}
	if roundedMagnitude > math.MaxInt64 {
		return 0, false
	}
	return int64(roundedMagnitude), true
}

// roundFloat64DecimalPlaces applies MySQL's supported precision window and
// avoids turning a finite value into NaN/Inf solely because the temporary
// decimal scaling overflowed. A scale finer than the value's representable ULP
// is an identity at float64 precision.
func roundFloat64DecimalPlaces(value float64, decimals int64) float64 {
	const maxDecimalPlaces int64 = 30
	if decimals > maxDecimalPlaces {
		decimals = maxDecimalPlaces
	} else if decimals < -maxDecimalPlaces {
		decimals = -maxDecimalPlaces
	}

	if decimals > 0 {
		factor := math.Pow10(int(decimals))
		scaled := value * factor
		if math.IsInf(scaled, 0) && !math.IsInf(value, 0) {
			return value
		}
		return math.Round(scaled) / factor
	}

	factor := math.Pow10(int(-decimals))
	rounded := math.Round(value/factor) * factor
	if math.IsInf(rounded, 0) && !math.IsInf(value, 0) {
		return value
	}
	return rounded
}

// scalarFnInt64Arg coerces a numeric scalar-fn argument to int64.
// Float coercion only succeeds for whole-valued floats — non-integer
// floats decline so the fold path returns nil and the runtime
// evaluator (which can surface 22018 INVALID_CHARACTER_VALUE) handles
// the conversion error.
// floorDivInt64 matches Java's Math.floorDiv: the largest int64 less than or
// equal to the algebraic quotient (truncating division adjusted toward
// negative infinity when the signs differ and there is a remainder).
func floorDivInt64(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func scalarFnInt64Arg(v any) (int64, bool) {
	if i, ok := ToInt64(v); ok {
		return i, true
	}
	if f, _, ok := ToFloat64(v); ok && f == math.Trunc(f) && float64FitsInt64(f) {
		return int64(f), true
	}
	return 0, false
}

// twoPow63 is 2^63 — the smallest float64 strictly greater than math.MaxInt64.
// math.MaxInt64 (2^63-1) has no exact float64 representation and rounds UP to
// this value, so it cannot be used as an inclusive upper bound in a float guard.
const twoPow63 = 9223372036854775808.0

// float64FitsInt64 reports whether a float64 is safely convertible to int64
// (i.e. int64(f) does not overflow). The upper bound is EXCLUSIVE at 2^63: a
// `f <= math.MaxInt64` guard rounds the constant up to 2^63 and wrongly admits
// 2^63 itself, which overflows int64 (RFC-087). The lower bound
// math.MinInt64 (-2^63) IS exactly representable as float64, so it is inclusive.
func float64FitsInt64(f float64) bool {
	return f >= math.MinInt64 && f < twoPow63
}

// nullifEqual is the equality test used by NULLIF's plan-time fold. It handles
// int/float promotion while staying conservative on mixed-type comparisons
// the Type hierarchy cannot model.
func nullifEqual(a, b any) bool {
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			return av == bv
		case float64:
			return float64(av) == bv
		}
	case float64:
		switch bv := b.(type) {
		case int64:
			return av == float64(bv)
		case float64:
			return av == bv
		}
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}
	return false
}

// ArithmeticOp is a subset of SQL arithmetic — enough to build a
// non-trivial matcher.
type ArithmeticOp int

const (
	OpAdd ArithmeticOp = iota
	OpSub
	OpMul
	OpDiv
	OpMod
)

// ArithmeticValue is a binary arithmetic over two child Values.
// Evaluate recurses left + right and applies the op with numeric
// promotion (float arithmetic when either operand is float64, else
// int64; mixed non-numeric operands are a ScalarTypeMismatchError).
// NULL on either side propagates (SQL semantics). Division by zero
// returns nil (UNKNOWN).
type ArithmeticValue struct {
	Op    ArithmeticOp
	Left  Value
	Right Value
}

func (a *ArithmeticValue) Children() []Value { return []Value{a.Left, a.Right} }
func (a *ArithmeticValue) Name() string      { return "arith" }

// Type returns the arithmetic result Type by numeric promotion of the
// operand types: DOUBLE if either operand is DOUBLE, else FLOAT if either is
// FLOAT, else INT when BOTH are INT (Java's ADD_II/DIV_II/MOD_II declare
// result INT — without this the static property re-erases the width the
// lane dispatch keys on, and `(a+b)+c` over INT columns escapes the int32
// bounds at the outer op), else LONG (the conservative integer default,
// also used when an operand type is unknown). NULL propagates through
// Evaluate, so the result is nullable.
func (a *ArithmeticValue) Type() Type {
	lc, rc := arithOperandCode(a.Left), arithOperandCode(a.Right)
	if a.Op == OpAdd && (lc == TypeCodeString || rc == TypeCodeString) {
		// Java's ADD_*S/S* operators CONCATENATE: any + with a STRING
		// operand (against INT/LONG/FLOAT/DOUBLE/STRING) yields STRING
		// (ArithmeticValue.java ADD_IS..ADD_SS) — string wins over the
		// numeric promotions for ADD.
		return NullableString
	}
	if lc == TypeCodeDouble || rc == TypeCodeDouble {
		return NullableDouble
	}
	if lc == TypeCodeFloat || rc == TypeCodeFloat {
		return NullableFloat
	}
	if lc == TypeCodeInt && rc == TypeCodeInt {
		return NullableInt
	}
	return NullableLong
}

func arithOperandCode(v Value) TypeCode {
	if v == nil {
		return TypeCodeUnknown
	}
	if t := v.Type(); t != nil {
		return t.Code()
	}
	return TypeCodeUnknown
}

func (a *ArithmeticValue) Evaluate(evalCtx any) (any, error) {
	l, err := a.Left.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	r, err := a.Right.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if l == nil || r == nil {
		return nil, nil
	}
	// STATIC-TYPE lane dispatch (Java ArithmeticValue's per-TypeCode
	// physical operators, keyed on the operands' STATIC types like
	// ADD_II/ADD_FF — never on the widened runtime representation):
	//   both INT            → int lane: exact int32 bounds (Math.addExact(int,int));
	//   max FLOAT (no DOUBLE) → float lane: float32 computation (ADD_FF/IF/LF —
	//                          overflow to ±Inf at the float32 boundary where
	//                          the double lane would return a finite value);
	//   otherwise            → the existing LONG/DOUBLE lanes below.
	// Operands with UNKNOWN static types keep the runtime-typed fallback —
	// the dispatch grows more precise as the resolver types more values
	// (WS-N Phase D).
	lc, rc := arithOperandCode(a.Left), arithOperandCode(a.Right)
	if a.Op == OpAdd && (lc == TypeCodeString || rc == TypeCodeString) {
		if out, handled := a.evalStringConcat(l, r, lc, rc); handled {
			return out, nil
		}
	}
	if lc == TypeCodeDouble || rc == TypeCodeDouble {
		otherOK := func(code TypeCode) bool {
			return code == TypeCodeDouble || code == TypeCodeFloat ||
				code == TypeCodeInt || code == TypeCodeLong
		}
		if otherOK(lc) && otherOK(rc) {
			// Java chooses its arithmetic physical operator from the static
			// TypeCodes. A common-typed CASE/COALESCE or a PromoteValue can
			// therefore declare DOUBLE while its selected Go carrier is still
			// int64; force the DOUBLE lane from the type, not the carrier.
			if out := a.evalFloat(l, r); out != nil {
				return out, nil
			}
		}
	}
	if lc == TypeCodeFloat || rc == TypeCodeFloat {
		otherOK := func(c TypeCode) bool {
			return c == TypeCodeFloat || c == TypeCodeInt || c == TypeCodeLong
		}
		if otherOK(lc) && otherOK(rc) {
			if out := a.evalFloat32(l, r); out != nil {
				return out, nil
			}
		}
	}
	if lc == TypeCodeInt && rc == TypeCodeInt {
		if out, handled, err := a.evalInt32(l, r); handled {
			return out, err
		}
	}
	// Float promotion: if either operand is float64 AND the other is numeric, use float arithmetic.
	_, lf := l.(float64)
	_, rf := r.(float64)
	if lf || rf {
		_, _, lNum := ToFloat64(l)
		_, _, rNum := ToFloat64(r)
		if lNum && rNum {
			return a.evalFloat(l, r), nil
		}
		return nil, &ScalarTypeMismatchError{
			Message: fmt.Sprintf("arithmetic type mismatch: %T %s %T", l, a.Op.Symbol(), r),
		}
	}
	li, lok := toInt64ForArith(l)
	ri, rok := toInt64ForArith(r)
	if !lok || !rok {
		return nil, &ScalarTypeMismatchError{
			Message: fmt.Sprintf("arithmetic type mismatch: %T %s %T", l, a.Op.Symbol(), r),
		}
	}
	switch a.Op {
	case OpAdd:
		out, ok := addInt64Checked(li, ri)
		if !ok {
			return nil, &ArithmeticOverflowError{}
		}
		return out, nil
	case OpSub:
		out, ok := subInt64Checked(li, ri)
		if !ok {
			return nil, &ArithmeticOverflowError{}
		}
		return out, nil
	case OpMul:
		out, ok := mulInt64Checked(li, ri)
		if !ok {
			return nil, &ArithmeticOverflowError{}
		}
		return out, nil
	case OpDiv:
		if ri == 0 {
			return nil, &ArithmeticDivisionByZeroError{}
		}
		if li == math.MinInt64 && ri == -1 {
			// Java DIV_LL is UNCHECKED `(long)l / (long)r` — the JVM wraps
			// MinLong / -1 back to MinLong with no exception (unlike
			// ADD/SUB/MUL, which use Math.*Exact). Go's `/` would panic on
			// exactly this pair, so the wrap is explicit. Parity, not taste.
			return int64(math.MinInt64), nil
		}
		return li / ri, nil
	case OpMod:
		if ri == 0 {
			return nil, &ArithmeticDivisionByZeroError{}
		}
		if li == math.MinInt64 && ri == -1 {
			return int64(0), nil
		}
		return li % ri, nil
	}
	return nil, nil
}

func (a *ArithmeticValue) evalFloat(l, r any) any {
	lf, _, lok := ToFloat64(l)
	rf, _, rok := ToFloat64(r)
	if !lok || !rok {
		return nil
	}
	switch a.Op {
	case OpAdd:
		return lf + rf
	case OpSub:
		return lf - rf
	case OpMul:
		return lf * rf
	case OpDiv:
		// IEEE-754 floating division: x/0.0 -> ±Inf, 0.0/0.0 -> NaN.
		// Java (and SQL for approximate-numeric types) returns these
		// rather than raising; only INTEGER division by zero errors.
		return lf / rf
	case OpMod:
		return math.Mod(lf, rf)
	}
	return nil
}

// evalFloat32 is the FLOAT lane (Java ADD_FF/IF/LF …): computation in
// float32, so overflow saturates to ±Inf at the float32 boundary and
// low-bit rounding matches Java's per-operation float math. Returns the
// float32 result widened to the row-domain float64 carrier; nil when an
// operand isn't numeric (caller falls through to the generic lanes).
func (a *ArithmeticValue) evalFloat32(l, r any) any {
	lf, lok := toFloat32Operand(l)
	rf, rok := toFloat32Operand(r)
	if !lok || !rok {
		return nil
	}
	var out float32
	switch a.Op {
	case OpAdd:
		out = lf + rf
	case OpSub:
		out = lf - rf
	case OpMul:
		out = lf * rf
	case OpDiv:
		out = lf / rf
	case OpMod:
		out = float32(math.Mod(float64(lf), float64(rf)))
	default:
		return nil
	}
	return float64(out)
}

// evalStringConcat is Java's ADD string family
// (ArithmeticValue.java ADD_IS/LS/FS/DS/SI/SL/SF/SD/SS): `+` with a
// STRING operand CONCATENATES, rendering the numeric side exactly as
// Java's string coercion would (Integer/Long decimal, Float/Double via
// their toString — ".0" on whole values, upper-case E exponents,
// Infinity/-Infinity/NaN spellings). handled=false when the other
// operand's static code is outside the Java table (the caller's
// generic arms then error as before).
func (a *ArithmeticValue) evalStringConcat(l, r any, lc, rc TypeCode) (any, bool) {
	render := func(v any, code TypeCode) (string, bool) {
		switch code {
		case TypeCodeString:
			s, ok := v.(string)
			return s, ok
		case TypeCodeInt, TypeCodeLong:
			iv, ok := toInt64ForArith(v)
			if !ok {
				return "", false
			}
			return strconv.FormatInt(iv, 10), true
		case TypeCodeFloat:
			f64, _, ok := ToFloat64(v)
			if !ok {
				return "", false
			}
			return javaFloatString(float64(float32(f64)), 32), true
		case TypeCodeDouble:
			f64, _, ok := ToFloat64(v)
			if !ok {
				return "", false
			}
			return javaFloatString(f64, 64), true
		default:
			return "", false
		}
	}
	ls, lok := render(l, lc)
	rs, rok := render(r, rc)
	if !lok || !rok {
		return nil, false
	}
	return ls + rs, true
}

// javaFloatString renders a float exactly the way Java's
// Float.toString / Double.toString does (the Java SE contract):
//   - 1e-3 ≤ |v| < 1e7 (and zero) render in DECIMAL form, whole values
//     completed with ".0" (2e6 → "2000000.0", 0.001 → "0.001");
//   - everything else renders in "computerized scientific notation":
//     one digit before the point, ".0"-completed mantissa, upper-case E,
//     no plus sign, no zero-padded exponent (1e7 → "1.0E7",
//     1e-4 → "1.0E-4");
//   - the special values spell Infinity/-Infinity/NaN.
//
// The DIGITS come from the Schubfach engine (schubfach.go) — the JDK
// 19+ algorithm — not Go's shortest-decimal strconv: the two agree on
// normal values but diverge on subnormals (Double.MIN_VALUE: Java
// "4.9E-324", Go "5e-324"). The layout is forced here because Go's 'g'
// format picks its own form boundary and zero-pads exponents.
func javaFloatString(f float64, bits int) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	neg := math.Signbit(f)
	var digits uint64
	var e int
	if bits == 32 {
		digits, e = javaFloatDigits(float32(math.Abs(f)))
	} else {
		digits, e = javaDoubleDigits(math.Abs(f))
	}
	return javaRenderDigits(neg, digits, e)
}

// JavaDoubleToString is Java's Double.toString(double).
func JavaDoubleToString(f float64) string { return javaFloatString(f, 64) }

// JavaFloatToString is Java's Float.toString(float).
func JavaFloatToString(f float32) string { return javaFloatString(float64(f), 32) }

// toFloat32Operand converts a FLOAT-lane operand to float32. Integer
// operands convert DIRECTLY int64→float32 (Java's ADD_LF does `(float)l`
// in one rounding step; routing through float64 first would double-round
// longs above 2^53 near float32 ties).
func toFloat32Operand(v any) (float32, bool) {
	if li, ok := toInt64ForArith(v); ok {
		return float32(li), true
	}
	f64, _, ok := ToFloat64(v)
	if !ok {
		return 0, false
	}
	return float32(f64), true
}

// evalInt32 is the INT lane (Java ADD_II/SUB_II/MUL_II via
// Math.*Exact(int,int)): both operands statically INT, arithmetic bounds
// checked at the int32 boundary — `int_col + int_col` crossing 2^31 errors
// with the overflow class where the LONG lane would silently return the
// wide value. DIV_II/MOD_II mirror the long lane's zero/MinInt semantics at
// 32 bits (Java (int) division wraps MinInt/-1). handled=false when an
// operand isn't an admitted integer (caller falls through).
func (a *ArithmeticValue) evalInt32(l, r any) (out any, handled bool, err error) {
	li, lok := toInt64ForArith(l)
	ri, rok := toInt64ForArith(r)
	if !lok || !rok {
		return nil, false, nil
	}
	// Inputs outside the int32 range mean the STATIC type lied about the
	// runtime value (an INT column cannot hold them; Java's (int) cast
	// would silently truncate, which is unreachable there because typing
	// guarantees the range). Fall through to the LONG lane rather than
	// emulate a truncation no valid execution produces.
	if li > math.MaxInt32 || li < math.MinInt32 || ri > math.MaxInt32 || ri < math.MinInt32 {
		return nil, false, nil
	}
	switch a.Op {
	case OpAdd, OpSub, OpMul:
		var out int64
		switch a.Op {
		case OpAdd:
			out = li + ri
		case OpSub:
			out = li - ri
		case OpMul:
			out = li * ri
		}
		if out > math.MaxInt32 || out < math.MinInt32 {
			return nil, true, &ArithmeticOverflowError{}
		}
		return out, true, nil
	case OpDiv:
		if ri == 0 {
			return nil, true, &ArithmeticDivisionByZeroError{}
		}
		if li == math.MinInt32 && ri == -1 {
			// Java DIV_II is `(int)l / (int)r` — wraps to MinInt.
			return int64(math.MinInt32), true, nil
		}
		return li / ri, true, nil
	case OpMod:
		if ri == 0 {
			return nil, true, &ArithmeticDivisionByZeroError{}
		}
		return li % ri, true, nil
	}
	return nil, false, nil
}

func toInt64ForArith(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case uint64:
		// The tuple layer decodes positive integers above math.MaxInt64 as
		// uint64; the LOSSLESS half joins integer arithmetic (the
		// comparators already admit the whole domain — CompareExactInts).
		// Values above MaxInt64 stay declined: int64 arithmetic cannot
		// represent them and Java's equivalent is BigInteger territory.
		if n <= math.MaxInt64 {
			return int64(n), true
		}
	case uint:
		if uint64(n) <= math.MaxInt64 {
			return int64(n), true
		}
	}
	return 0, false
}

// ArithmeticDivisionByZeroError is returned by ArithmeticValue.Evaluate
// when division or modulo by zero is attempted. Callers (the executor)
// convert this to the appropriate SQL error.
type ArithmeticDivisionByZeroError struct{}

func (*ArithmeticDivisionByZeroError) Error() string {
	return "division by zero"
}

// ArithmeticOverflowError is returned by ArithmeticValue.Evaluate
// when integer arithmetic overflows. Callers (the executor) convert
// this to SQLSTATE 22003 NUMERIC_VALUE_OUT_OF_RANGE.
type ArithmeticOverflowError struct{}

func (*ArithmeticOverflowError) Error() string {
	return "integer overflow"
}

// ScalarTypeMismatchError is returned by scalar functions (GREATEST,
// LEAST) when arguments have incompatible types. The executor
// converts this to SQLSTATE 22000 DATA_EXCEPTION.
type ScalarTypeMismatchError struct {
	Message string
}

func (e *ScalarTypeMismatchError) Error() string {
	return e.Message
}

// InvalidCastError is returned by CastValue.Evaluate when a cast
// is out of range or structurally invalid (NaN→INT, overflow, etc.).
// The executor converts this to SQLSTATE 22F3H INVALID_CAST.
type InvalidCastError struct {
	Message string
}

func (e *InvalidCastError) Error() string {
	return e.Message
}

// InvalidArgumentError is returned by a scalar function when an argument
// is outside the function's mathematical domain — currently SQRT of a
// negative number. The executor converts this to SQLSTATE 22023
// INVALID_PARAMETER_VALUE. Distinct from ScalarTypeMismatchError (wrong
// argument *type*); this is a wrong argument *value* of the right type.
type InvalidArgumentError struct {
	Message string
}

func (e *InvalidArgumentError) Error() string {
	return e.Message
}

// AggregateEvalError is returned by AggregateValue.Evaluate when an
// aggregate node is reached on the per-row scalar evaluation path —
// e.g. an aggregate used in WHERE (`WHERE COUNT(*) > 0`). Java rejects
// this shape at plan time ("unable to eval an aggregation function with
// eval()"); Go's planner does not yet (TODO: plan-time rejection of
// aggregate-in-scalar-context), so the misuse reaches row eval. It is
// genuinely reachable from user query data, so it must return an error
// rather than panic (RFC-087 residual-panic audit, gate #1). The
// executor maps this to SQLSTATE 42803 (grouping error).
type AggregateEvalError struct {
	Message string
}

func (e *AggregateEvalError) Error() string {
	return e.Message
}

// addInt64Checked / subInt64Checked / mulInt64Checked mirror
// embedded.functions.{Add,Sub,Mul}Int64Checked. Re-implemented in
// cascades to keep the value-layer arithmetic free of cross-package
// imports (the package-structure goal in RFC-025).
//
// Add/Sub overflow: signed-overflow detection via the standard
// "different sign" check (well-defined under int64 wrap semantics).
// Mul: defer to math/bits to avoid the full multiword arithmetic
// inline.
func addInt64Checked(a, b int64) (int64, bool) {
	r := a + b
	if (a > 0 && b > 0 && r < a) || (a < 0 && b < 0 && r > a) {
		return 0, false
	}
	return r, true
}

func subInt64Checked(a, b int64) (int64, bool) {
	r := a - b
	if (b > 0 && r > a) || (b < 0 && r < a) {
		return 0, false
	}
	return r, true
}

func mulInt64Checked(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	r := a * b
	// Reverse-divide to detect overflow. The MinInt64 * -1 case is
	// the one a/b == 1 wouldn't catch — handle explicitly.
	if a == math.MinInt64 && b == -1 || b == math.MinInt64 && a == -1 {
		return 0, false
	}
	if r/b != a {
		return 0, false
	}
	return r, true
}

// --- BooleanValue + CastValue -------------------------------------

// BooleanValue is a literal true / false (and NULL when Value is
// nil — SQL UNKNOWN at the Value layer).
//
// NAMING CAVEAT: Java has a `BooleanValue` of the same name but
// it's an INTERFACE (Value→QueryPredicate translation shim), not a
// concrete type. The Go-side concrete is closer to Java's
// `LiteralValue<Boolean>`. The name collision is regrettable but
// the Go code references this concrete type explicitly; rule code
// should not pattern-match on `*BooleanValue` thinking it has
// Java's interface semantics.
type BooleanValue struct {
	Value *bool // nil = UNKNOWN
}

// NewBooleanValue wraps a Go bool.
func NewBooleanValue(v bool) *BooleanValue {
	b := v
	return &BooleanValue{Value: &b}
}

func (*BooleanValue) Children() []Value { return []Value{} }
func (*BooleanValue) Name() string      { return "bool" }

// Type returns the boolean literal's Type — NotNullBoolean for
// concrete TRUE/FALSE; NullableBoolean when Value is nil (the
// SQL UNKNOWN-at-Value-layer case).
func (b *BooleanValue) Type() Type {
	if b.Value == nil {
		return NullableBoolean
	}
	return NotNullBoolean
}

func (b *BooleanValue) Evaluate(any) (any, error) {
	if b.Value == nil {
		return nil, nil
	}
	return *b.Value, nil
}

// CastValue converts a child Value's result to a target Type.
// Go handles the trivial conversions our existing corpus needs:
// int ↔ string (via strconv-free formatting), bool ↔
// int (false=0, true=1). Unknown conversions return nil (UNKNOWN) —
// extend the Evaluate switch when a corpus query needs a new pair.
type CastValue struct {
	Child  Value
	Target Type
}

// NewCastValue constructs a CastValue.
func NewCastValue(child Value, target Type) *CastValue {
	return &CastValue{Child: child, Target: target}
}

func (c *CastValue) Children() []Value { return []Value{c.Child} }
func (c *CastValue) Name() string      { return "cast" }

// Type returns the cast's target Type. CAST may produce NULL on
// out-of-range / unsupported source (Evaluate returns nil), so cast
// results are always nullable in Go.
func (c *CastValue) Type() Type {
	if c.Target == nil {
		return UnknownType
	}
	return WithNullability(c.Target, true)
}

// javaMathRound mirrors java.lang.Math.round(double) on Java 7+ (post
// JDK-6430675): round to nearest, ties toward positive infinity. It corrects
// the pre-Java-7 floor(x+0.5) algorithm at the boundary where x+0.5 rounds up
// purely due to floating-point error — e.g. the largest double below 0.5
// (0.49999999999999994) must round to 0, not 1. Go's math.Round differs (it
// rounds half AWAY from zero, so -0.5 → -1 vs Java's 0), so it cannot be used.
// trimJavaWhitespace strips leading/trailing characters the way Java's
// String.trim() does — ONLY code points <= U+0020 — not the full Unicode
// whitespace set strings.TrimSpace removes. Numeric CAST(string AS
// INT/LONG/DOUBLE) delegates to Integer/Long/Double.parseXxx(s.trim()) in Java
// (CastValue.java:187,195,211), so e.g. CAST(NBSP+"5" AS INT) must THROW (NBSP,
// U+00A0, is not stripped) rather than yield 5 as strings.TrimSpace would.
func trimJavaWhitespace(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
}

func javaMathRound(a float64) float64 {
	// Integer bit-ops on the IEEE-754 representation, exactly as Java does —
	// floor(a+0.5) can't be patched up in float (the correcting subtraction
	// rounds too).
	const (
		significandWidth = 53
		expBias          = 1023
		expBitMask       = int64(0x7FF0000000000000)
		signifBitMask    = int64(0x000FFFFFFFFFFFFF)
	)
	longBits := int64(math.Float64bits(a))
	biasedExp := (longBits & expBitMask) >> (significandWidth - 1)
	shift := (significandWidth - 2 + expBias) - biasedExp
	if (shift & -64) == 0 { // 0 <= shift < 64
		r := (longBits & signifBitMask) | (signifBitMask + 1)
		if longBits < 0 {
			r = -r
		}
		return float64(((r >> uint(shift)) + 1) >> 1)
	}
	if shift < 0 {
		// |a| >= 2^52 — already an exact integer; rounding is the identity.
		// Return a unchanged so a caller's overflow range-check sees the true
		// magnitude. (Java's Math.round saturates to Long.MAX/MIN here, but that
		// would mask CAST overflow detection, e.g. CAST(1e20 AS BIGINT).)
		return a
	}
	// shift >= 64 — |a| < 2^-12, far below 0.5, so it rounds to 0.
	return 0
}

func (c *CastValue) Evaluate(evalCtx any) (any, error) {
	v, err := c.Child.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	return c.castEvaluated(v, c.Child.Type())
}

// castEvaluated applies the cast to an already-evaluated runtime value. The
// array arm re-enters it per element with the declared source element type: an
// element is ALREADY the evaluated value, so it needs neither a throwaway Value
// node nor an eval context, but its Go carrier alone cannot distinguish
// FLOAT/DOUBLE or INT/LONG.
func (c *CastValue) castEvaluated(v any, source Type) (any, error) {
	if v == nil {
		return nil, nil
	}
	if c.Target == nil {
		return nil, nil
	}
	sourceCode := TypeCodeUnknown
	if source != nil {
		sourceCode = source.Code()
	}
	switch c.Target.Code() {
	case TypeCodeArray:
		// Java CastValue.castArrayToArray: cast element-wise, keeping NULL
		// elements as NULL and an empty array empty. Each element goes
		// through the same scalar cast machinery (Java re-injects a cast on
		// a per-element LiteralValue).
		at, ok := c.Target.(*ArrayType)
		if !ok || at.ElementType == nil {
			return nil, &InvalidCastError{Message: "Target array element type cannot be null"}
		}
		list, ok := v.([]any)
		if !ok {
			// A non-list carrier under an ARRAY target — degrade to
			// UNKNOWN rather than corrupt (the plan-time pair gate rejects
			// non-array sources; this arm only sees array-typed children).
			return nil, nil
		}
		// An EMPTY array casts to an empty array of the target type without
		// consulting the source element type at all — Java returns here
		// (CastValue.java:586-589) before anything reads it. It has to: the
		// only type an empty array literal can carry is an unknown element
		// type, so demanding a source element type up front would reject
		// `CAST([] AS INTEGER ARRAY)`, which is legal in both engines.
		if len(list) == 0 {
			return []any{}, nil
		}
		var sourceElement Type
		if sourceArray, isArray := source.(*ArrayType); isArray {
			sourceElement = sourceArray.ElementType
		}
		out := make([]any, 0, len(list))
		for _, e := range list {
			if e == nil {
				// A NULL element stays NULL and is never cast, so it never
				// needs a source element type either.
				out = append(out, nil)
				continue
			}
			if sourceElement != nil && sourceElement.Equals(at.ElementType) {
				// Identical element types: Java passes the element through
				// rather than re-entering the cast machinery.
				out = append(out, e)
				continue
			}
			if sourceElement == nil {
				// Only a REAL element cast needs the source type, which is
				// where Java raises (CastValue.java:599-602).
				return nil, &InvalidCastError{Message: "Source array element type cannot be null"}
			}
			ev, eerr := (&CastValue{Target: at.ElementType}).castEvaluated(e, sourceElement)
			if eerr != nil {
				return nil, eerr
			}
			out = append(out, ev)
		}
		return out, nil
	case TypeCodeInt:
		switch val := v.(type) {
		case int64:
			if val < math.MinInt32 || val > math.MaxInt32 {
				return nil, &InvalidCastError{Message: fmt.Sprintf("Value out of range for INT: %d", val)}
			}
			return val, nil
		case bool:
			if val {
				return int64(1), nil
			}
			return int64(0), nil
		case float64:
			if val != val || math.IsInf(val, 0) {
				return nil, &InvalidCastError{Message: "Cannot cast NaN or Infinite to INT"}
			}
			rounded := javaMathRound(val)
			if rounded > math.MaxInt32 || rounded < math.MinInt32 {
				return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast %v to INT: out of range", val)}
			}
			return int64(int32(rounded)), nil
		case string:
			n, err := strconv.ParseInt(trimJavaWhitespace(val), 10, 32)
			if err != nil {
				// Java STRING_TO_INT wraps Integer.parseInt's
				// NumberFormatException — its message is the stock
				// `For input string: "X"` for syntax AND range failures
				// alike (CastValue.java:185-192), never Go's strconv text.
				return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast string '%s' to INT: For input string: \"%s\"", val, trimJavaWhitespace(val))}
			}
			return n, nil
		}
	case TypeCodeLong:
		switch val := v.(type) {
		case int64:
			return val, nil
		case bool:
			if val {
				return int64(1), nil
			}
			return int64(0), nil
		case float64:
			if val != val || math.IsInf(val, 0) {
				return nil, &InvalidCastError{Message: "Cannot cast NaN or Infinite to LONG"}
			}
			rounded := javaMathRound(val)
			if !float64FitsInt64(rounded) {
				return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast %v to LONG: out of range", val)}
			}
			return int64(rounded), nil
		case string:
			n, err := strconv.ParseInt(trimJavaWhitespace(val), 10, 64)
			if err != nil {
				// Java STRING_TO_LONG: Long.parseLong's stock message.
				return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast string '%s' to LONG: For input string: \"%s\"", val, trimJavaWhitespace(val))}
			}
			return n, nil
		}
	case TypeCodeBoolean:
		switch val := v.(type) {
		case bool:
			return val, nil
		case int64:
			// Java's cast table has INT_TO_BOOLEAN but NO
			// LONG/FLOAT/DOUBLE_TO_BOOLEAN (CastValue.java:230; missing
			// pairs fail resolution with "No cast defined from X to
			// BOOLEAN"). Dispatch on the CHILD's STATIC code: a genuine
			// INT converts (!= 0); LONG rejects like Java. An
			// UNKNOWN-typed child keeps the conversion — the static
			// widths now flow from the catalog, so unknown means an
			// internal untyped expression, not a BIGINT column.
			if sourceCode == TypeCodeLong {
				return nil, &InvalidCastError{Message: "No cast defined from LONG to BOOLEAN"}
			}
			return val != 0, nil
		case float64:
			if sourceCode == TypeCodeDouble || sourceCode == TypeCodeFloat {
				return nil, &InvalidCastError{Message: fmt.Sprintf("No cast defined from %v to BOOLEAN", sourceCode)}
			}
			return val != 0, nil
		case string:
			switch strings.ToLower(strings.TrimSpace(val)) {
			case "true", "1":
				return true, nil
			case "false", "0":
				return false, nil
			}
			return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast string '%s' to BOOLEAN", val)}
		}
	case TypeCodeString:
		if s, ok := v.(string); ok {
			return s, nil
		}
		if i, ok := v.(int64); ok {
			// strconv.FormatInt handles signed values correctly —
			// uitoa(uint64(i)) would reinterpret negative int64 as
			// the corresponding huge positive number (CAST(-5 AS
			// STRING) → "18446744073709551611").
			return strconv.FormatInt(i, 10), nil
		}
		if f, ok := v.(float64); ok {
			// Dispatch on the child's STATIC type code, like Java's
			// FLOAT_TO_STRING vs DOUBLE_TO_STRING operator rows:
			// FLOAT renders through the Float.toString contract
			// (32-bit shortest-repr), DOUBLE through Double.toString.
			if sourceCode == TypeCodeFloat {
				return javaFloatString(float64(float32(f)), 32), nil
			}
			return javaFloatString(f, 64), nil
		}
		if f32, ok := v.(float32); ok {
			return javaFloatString(float64(f32), 32), nil
		}
		if b, ok := v.(bool); ok {
			// Match runtime functions.CastValue: lowercase
			// "true"/"false" (Java's CastValue.BOOLEAN_TO_STRING).
			// Without this arm, fold-time `CAST(TRUE AS STRING)`
			// returned nil while the runtime returned "true" — fold
			// vs runtime mismatch on a constant input.
			if b {
				return "true", nil
			}
			return "false", nil
		}
		if b, ok := v.([16]byte); ok {
			// A UUID flows through the engine as a neutral [16]byte (RFC-162);
			// CAST(uuid AS STRING) renders the canonical 36-char form, matching
			// Java's UUID.toString(). uuid.String() gives the lowercase 8-4-4-4-12
			// layout with zero-padding preserved.
			return uuid.UUID(b).String(), nil
		}
	case TypeCodeDate:
		switch val := v.(type) {
		case time.Time:
			return val.UTC().Format(dateLayout), nil
		case string:
			s := strings.TrimSpace(val)
			t, err := time.Parse(dateLayout, s)
			if err != nil {
				if t2, err2 := time.Parse(timestampLayout, s); err2 == nil {
					return t2.UTC().Format(dateLayout), nil
				}
				return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast string '%s' to DATE: %s", val, err)}
			}
			return t.UTC().Format(dateLayout), nil
		}
	case TypeCodeTimestamp:
		switch val := v.(type) {
		case time.Time:
			return val.UTC().Format(timestampLayout), nil
		case string:
			s := strings.TrimSpace(val)
			for _, layout := range timestampParseLayouts {
				if t, err := time.Parse(layout, s); err == nil {
					return t.UTC().Format(timestampLayout), nil
				}
			}
			return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast string '%s' to TIMESTAMP", val)}
		case int64:
			return time.UnixMilli(val).UTC().Format(timestampLayout), nil
		}
	case TypeCodeFloat:
		// FLOAT is genuinely binary32 here — a FLOAT column's index entries
		// pack as tuple code 0x20 (single) — so a cast TO it must ROUND to
		// binary32, exactly like Java's DOUBLE_TO_FLOAT (`value.floatValue()`),
		// INT/LONG_TO_FLOAT (`Float.valueOf`) and STRING_TO_FLOAT
		// (`Float.parseFloat`) — CastValue.java:126-205. Sharing this arm with
		// DOUBLE returned the operand at binary64 precision and made the cast a
		// no-op, which is wrong in BOTH directions on a value binary32 cannot
		// hold: `d = CAST(0.1 AS FLOAT)` matched a DOUBLE 0.1 that the rounded
		// constant is not equal to, and `f = CAST(0.1 AS FLOAT)` missed the
		// FLOAT row that it IS equal to.
		//
		// The result stays a float64 CARRYING binary32 precision, the row
		// domain every FLOAT-typed value lives in (see tupleElementToRowValue);
		// coerceTupleElementForKey narrows it to a real float32 at the tuple-probe
		// boundary, where the wire type code is what matters.
		// A FLOAT-typed child makes this an IDENTITY cast, which Java short
		// circuits before any operator runs ("If the types are the same, no
		// cast is needed" — CastValue.inject, CastValue.java:441-446). It must
		// NOT re-apply the DOUBLE_TO_FLOAT checks: an already-FLOAT ±Infinity
		// (reachable via CAST('Infinity' AS FLOAT), which Float.parseFloat
		// accepts) would otherwise be rejected by casting it to its own type.
		// Dispatch on the child's STATIC type, since every FLOAT value is
		// carried as a float64 and the runtime type cannot tell the two apart.
		if sourceCode == TypeCodeFloat {
			return v, nil
		}
		switch val := v.(type) {
		case float64:
			if math.IsNaN(val) || math.IsInf(val, 0) {
				return nil, &InvalidCastError{Message: "Cannot cast NaN or Infinite to FLOAT"}
			}
			if val > math.MaxFloat32 || val < -math.MaxFloat32 {
				return nil, &InvalidCastError{
					Message: fmt.Sprintf("Value out of range for FLOAT: %s", javaFloatString(val, 64)),
				}
			}
			return float64(float32(val)), nil
		case float32:
			return float64(val), nil
		case int64:
			return float64(float32(val)), nil
		case string:
			// bitSize 32 rounds to binary32. ErrRange is NOT a failure here:
			// Go reports binary32 overflow by returning ±Inf WITH an error,
			// while Java's Float.parseFloat returns Infinity and throws only on
			// malformed text (CastValue.java:201-205). Treating ErrRange as an
			// invalid cast would reject `CAST('1e39' AS FLOAT)`, which Java
			// accepts. The returned value is already the ±Inf Java produces.
			f, err := strconv.ParseFloat(trimJavaWhitespace(val), 32)
			if err != nil && !errors.Is(err, strconv.ErrRange) {
				return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast string '%s' to FLOAT: %s", val, err)}
			}
			return f, nil
		case bool:
			if val {
				return float64(1), nil
			}
			return float64(0), nil
		}
	case TypeCodeDouble:
		// CAST … AS DOUBLE — accept float64/float32 verbatim; promote
		// integral types to float64. Without this case, the walker's
		// CastValue{TypeDouble} path silently returns nil from Evaluate and
		// constant-fold of `CAST(5 AS DOUBLE) = 3.14` gets UNKNOWN instead of
		// FALSE. Every conversion here WIDENS, so unlike FLOAT above there is
		// no rounding step and no range that can overflow.
		switch val := v.(type) {
		case float64:
			return val, nil
		case float32:
			return float64(val), nil
		case int64:
			return float64(val), nil
		case string:
			f, err := strconv.ParseFloat(trimJavaWhitespace(val), 64)
			if err != nil {
				return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast string '%s' to DOUBLE: %s", val, err)}
			}
			return f, nil
		case bool:
			if val {
				return float64(1), nil
			}
			return float64(0), nil
		}
	case TypeCodeUuid:
		// Java CastValue.STRING_TO_UUID → PromoteValue.stringToUuidValue
		// (UUID.fromString; invalid → SemanticException INVALID_UUID_VALUE).
		// Same routine and wording as Go's PromoteValue UUID arm — Java
		// routes CAST and promotion through the one helper, so must we.
		// Every failure is a TYPED InvalidCastError (→ 22F3H): the codebase's
		// generic fall-through-NULL for unknown sources is wrong here — Java
		// has no non-string coercion to UUID at all, so CAST(bigint AS UUID)
		// silently NULLing every row was a silent-wrong.
		switch val := v.(type) {
		case string:
			u, perr := uuid.Parse(val)
			if perr != nil {
				return nil, &InvalidCastError{Message: fmt.Sprintf("Invalid UUID value for the UUID type %s", val)}
			}
			return [16]byte(u), nil
		case [16]byte:
			// Already the neutral UUID representation (RFC-162).
			return val, nil
		default:
			return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast %T to UUID", v)}
		}
	case TypeCodeBytes:
		// Java defines no cast operators TO bytes — only the identity
		// cast passes CastPairDefined. Anything non-[]byte here means an
		// unknown-typed child bypassed the plan-time pair gate; reject
		// like Java's construction-time "No cast defined" rather than
		// fall through to the silent-NULL tail below.
		switch val := v.(type) {
		case []byte:
			return val, nil
		default:
			return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast %T to BYTES", v)}
		}
	}
	return nil, nil
}

// --- RecordConstructorValue ----------------------------------------

// RecordConstructorField pairs a field name with the Value that
// computes its contents. Named so the output has a struct shape
// downstream consumers (projections, aggregations) can address by
// name.
type RecordConstructorField struct {
	Name  string
	Value Value
}

// RecordConstructorValue constructs a record (struct) from named
// children. Used by the analyzer for SELECT projection output
// (`SELECT a, b+1 AS c` → Record{a: a, c: b+1}) and anywhere a
// tuple-of-values is needed (ORDER BY key groups, aggregate keys).
//
// Mirrors Java's `RecordConstructorValue`.
type RecordConstructorValue struct {
	Fields []RecordConstructorField

	// desc is the message descriptor STAMPED at plan time, from the single
	// per-plan type repository (FinalizePlan). Java reads the equivalent off
	// the EvaluationContext instead (RecordConstructorValue.java:113-114); Go
	// has no uniform context to read it from — Evaluate was measured
	// receiving four unrelated concrete types, the most frequent being a bare
	// positional row with no binding surface at all — so the descriptor rides
	// on the value. RFC-204 §4.5.1 records the decision and
	// TestFrontierContextIsNotAUniformCarrier pins the premise, so that this
	// reverts to Java's shape if the contexts are ever unified.
	//
	// Written once, on the plan-cache MISS path before PlanCache.Put, and
	// never afterwards: the cache hands the SAME plan pointer to every later
	// execution and each page rebuilds its cursors from it concurrently, so a
	// later write would be a data race.
	desc protoreflect.MessageDescriptor

	// typeName is the DECLARED type name of a named struct literal
	// (`STRUCT GEO (1 AS lat, 2 AS lon)`); empty for the anonymous case.
	// Java models this as a separate factory —
	// RecordConstructorValue.ofColumnsAndName (RecordConstructorValue.java
	// :485-487) builds the value as `computeResultType(columns,
	// false).withName(name)` — so the name belongs to the resulting Record
	// TYPE, not to the value. Go computes the type on demand in Type(), so
	// the name is carried here and applied there.
	//
	// This is the USER name. Java's Type.Record.withName also derives the
	// storage name via ProtoUtils.toProtoBufCompliantName (Type.java
	// :2221-2223); Go's RecordType has no storage-name field, and the escape
	// is deterministic, so the wire spelling is recomputed where needed
	// rather than stored as a second copy that could drift.
	typeName string
}

// SetTypeName records the declared name of a named struct literal. Plan-time
// only, for the same reason SetMessageDescriptor is.
func (r *RecordConstructorValue) SetTypeName(name string) {
	r.typeName = name
}

// TypeName returns the declared name of a named struct literal, or "" when the
// record is anonymous.
func (r *RecordConstructorValue) TypeName() string {
	return r.typeName
}

// SetMessageDescriptor stamps the plan-time descriptor. Plan-time only — see
// the field comment for why a later write races.
func (r *RecordConstructorValue) SetMessageDescriptor(md protoreflect.MessageDescriptor) {
	r.desc = md
}

// MessageDescriptor returns the stamped descriptor, or nil if this constructor
// was never walked by FinalizePlan.
func (r *RecordConstructorValue) MessageDescriptor() protoreflect.MessageDescriptor {
	return r.desc
}

// NewRecordConstructorValue constructs a RecordConstructorValue.
// Duplicate field names are deduplicated by appending a numeric
// suffix (_2, _3, ...) to later occurrences, matching SQL semantics
// where `SELECT a, a FROM T` produces columns a, a_2.
func NewRecordConstructorValue(fields ...RecordConstructorField) *RecordConstructorValue {
	seen := make(map[string]int, len(fields))
	out := make([]RecordConstructorField, len(fields))
	for i, f := range fields {
		count := seen[f.Name]
		seen[f.Name] = count + 1
		if count > 0 {
			out[i] = RecordConstructorField{
				Name:  fmt.Sprintf("%s_%d", f.Name, count+1),
				Value: f.Value,
			}
		} else {
			out[i] = f
		}
	}
	return &RecordConstructorValue{Fields: out}
}

// NewRawRecordConstructorValue constructs a machinery-owned positional
// RecordConstructorValue keeping every field name VERBATIM — duplicate names
// allowed. It exists for ordinal-join seeds and private aggregate output rows:
// a join's ordinal RC concatenates the legs' columns, each field a
// BAKED FieldValue over its leg's QOV, and duplicate names across legs
// (`SELECT * FROM a JOIN b` with same-named columns) MUST survive verbatim —
// positional access is by ordinal, so duplicates are unambiguous, and the
// duplicate-name identity pins are unconstructible without them.
//
// NEVER use this for a user-facing projection RC: NewRecordConstructorValue (above)
// appends _2/_3 suffixes, which is correct there (SQL projection column
// naming) — a raw duplicate is not addressable by name at all: the plan-time
// lookup declines on the ambiguity, so a projection built this way would lose the
// columns rather than name them, the exact conflation ordinal identity exists to avoid.
func NewRawRecordConstructorValue(fields ...RecordConstructorField) *RecordConstructorValue {
	out := make([]RecordConstructorField, len(fields))
	copy(out, fields)
	return &RecordConstructorValue{Fields: out}
}

// ErrWholeRowProjection is returned by ProjectionResultValue when the
// projection list is a single bare RECORD-typed QuantifiedObjectValue — a
// "one-slot whole-row projection". A scalar QOV is an ordinary scalar slot
// (not a wrapped row) and is admitted.
//
// THIS IS A DERIVATION REFUSING TO SYNTHESISE A ROW. It is NOT a constructor
// guard and the shape is NOT unbuildable — say so here, in the file that owns
// the error, because earlier text in three files claimed the opposite and this
// is where a reader looks first.
//
// What actually holds: every LogicalProjectionExpression constructor is a plain
// struct fill that validates nothing, so the one-slot whole-row projection can
// be built and IS built (expressions/flowed_value_typing_test.go's
// TestLogicalProjectionFallsBackToUntypedQOV). What this error does is stop the
// projection CLAIMING a row it cannot name; GetResultValue then falls back to an
// untyped QOV, which is the pre-RFC-226 decline kept deliberately for the one
// shape that cannot answer. The fallback is a LIVE arm, not dead code.
//
// WHY THE SHAPE CANNOT ANSWER. The executor emits one positional slot PER
// PROJECTION, so this projection produces a 1-slot row WRAPPING its inner's row,
// and the wrapper has no name for its single field. Java never has the shape at
// all — GraphExpansion expands SELECT * into per-field columns, and Go's
// SELECT * builds no projection node either.
//
// WHAT IS STILL OWED, and is RFC-226 §4.4(c)'s follow-on rather than something
// this error delivers: the two rules that yield an inner's N-field row into the
// projection's OWN memo reference make two differently-shaped plans co-members
// of one equivalence class. Refusing the derivation does not stop that; only
// deleting the rules does. Do not read this guard as having closed it.
var ErrWholeRowProjection = errors.New(
	"projection list is a single bare QuantifiedObjectValue (one-slot whole-row projection): " +
		"the executor emits one slot per projection, so this wraps the inner row instead of " +
		"passing it through; project the inner's columns per-field instead")

// projectionWrapsItsInputRow reports the one shape ErrWholeRowProjection names:
// a lone projected value that is a MACHINERY row, so the emitted one-slot row
// wraps it and has no name for its single field.
//
// The discriminator is the correlation's KIND, not the value's type. A
// record-typed bare QOV over a NAMED correlation is a column of that source —
// `SELECT x FROM t, t.items AS x` projects the whole struct element, and x is
// its name — while a `_current` carrier or a machinery-minted `q$N` names a
// row the SQL never gave a name to, which is the wrap with nothing to call its
// field.
//
// Deciding it on the TYPE instead split one shape in half: the same SQL planned
// for a scalar element and was refused for a STRUCT one, because only the
// struct made the projected QOV record-typed. The type never was the question.
func projectionWrapsItsInputRow(projections []Value) bool {
	if len(projections) != 1 {
		return false
	}
	qov, bare := projections[0].(*quantifiedObjectValue)
	if !bare || qov == nil || qov.flowed == nil || qov.flowed.code != TypeCodeRecord {
		return false
	}
	return qov.correlation.kind == correlationKindCurrent ||
		qov.correlation.kind == correlationKindUnique
}

// ProjectionResultValue builds the row a projection PRODUCES, as a record
// constructor over its projected values and output aliases. It is the single
// authority for that derivation, shared by the logical projection expression
// and its physical twin so the two cannot drift.
//
// Slot names come from OutputColumnName — the same authority the executor uses
// to name the emitted positional row — so the type a projection STATES matches
// the row it EMITS. A slot that still resolves to no name takes Java's
// ordinal spelling, "_"+i (Type.java normalizeFields).
//
// Duplicate names go through NewRecordConstructorValue's _2/_3 dedup, never the
// raw constructor: a raw duplicate under name-keyed lookup resolves to the
// first match, the exact conflation ordinal identity exists to prevent.
func ProjectionResultValue(projections []Value, aliases []string) (*RecordConstructorValue, error) {
	return ProjectionResultValueForOutputSchema(projections, aliases, nil)
}

// ProjectionOutputSchemaIdentityOverrides returns only the externally frozen
// portion of a projection schema: each slot whose authoritative output name
// differs from the name the Value program and aliases naturally derive. The
// resulting sparse vector is suitable for memo identity.
//
// This distinction is load-bearing. Internal Value names can contain
// alpha-renamable correlation identifiers (a scalar-subquery QOV is the
// canonical example), so folding every derived result field name into a hash
// breaks alias invariance. Conversely, an SQL boundary may deliberately freeze
// a different positional key (`S.ID` over a Value naturally named `ID`); that
// difference is executable schema and must keep the projections apart. nil is
// returned when no frozen name adds information beyond the natural schema.
func ProjectionOutputSchemaIdentityOverrides(
	projections []Value,
	aliases []string,
	outputNames []string,
) ([]string, error) {
	if outputNames == nil {
		return nil, nil
	}
	natural, err := ProjectionResultValue(projections, aliases)
	if err != nil {
		return nil, err
	}
	frozen, err := ProjectionResultValueForOutputSchema(projections, aliases, outputNames)
	if err != nil {
		return nil, err
	}
	if len(natural.Fields) != len(frozen.Fields) {
		return nil, fmt.Errorf("projection natural schema has %d slots, frozen schema has %d", len(natural.Fields), len(frozen.Fields))
	}
	overrides := make([]string, len(frozen.Fields))
	anyOverride := false
	for i := range frozen.Fields {
		if natural.Fields[i].Name == frozen.Fields[i].Name {
			continue
		}
		overrides[i] = frozen.Fields[i].Name
		anyOverride = true
	}
	if !anyOverride {
		return nil, nil
	}
	return overrides, nil
}

// ProjectionResultValueForOutputSchema builds the exact row a projection
// produces while optionally preserving a schema that was already established
// by the logical SQL boundary. A nil outputNames derives names from the Value
// program and aliases exactly like ProjectionResultValue. A non-nil slice is
// authoritative: alpha-rebasing a Value onto a physical child must not rename
// the SQL column it computes.
//
// A one-slot whole-record QOV is still rejected even with an explicit name:
// the executor emits one slot per projection, so naming that slot does not make
// it equivalent to the inner record's N top-level fields.
func ProjectionResultValueForOutputSchema(
	projections []Value,
	aliases []string,
	outputNames []string,
) (*RecordConstructorValue, error) {
	if outputNames != nil {
		if len(outputNames) != len(projections) {
			return nil, fmt.Errorf("projection output schema has %d names for %d slots", len(outputNames), len(projections))
		}
		if projectionWrapsItsInputRow(projections) {
			return nil, ErrWholeRowProjection
		}
		fields := make([]RecordConstructorField, len(projections))
		for i := range projections {
			if outputNames[i] == "" {
				return nil, fmt.Errorf("projection output schema slot %d has an empty name", i)
			}
			fields[i] = RecordConstructorField{Name: outputNames[i], Value: projections[i]}
		}
		return NewRecordConstructorValue(fields...), nil
	}

	if projectionWrapsItsInputRow(projections) {
		return nil, ErrWholeRowProjection
	}
	fields := make([]RecordConstructorField, len(projections))
	for i, v := range projections {
		alias := ""
		if i < len(aliases) {
			alias = aliases[i]
		}
		name := OutputColumnName(v, alias)
		if name == "" {
			name = OrdinalFieldName(i)
		}
		fields[i] = RecordConstructorField{Name: name, Value: v}
	}
	return NewRecordConstructorValue(fields...), nil
}

// Children returns each field's Value as a flat list, in field
// declaration order. Lets WalkValue traverse the whole tree.
func (r *RecordConstructorValue) Children() []Value {
	out := make([]Value, len(r.Fields))
	for i, f := range r.Fields {
		out[i] = f.Value
	}
	return out
}

// Type synthesises a RecordType from the constructor's fields. The
// outer record is anonymous + nullable (we can't prove an inferred
// record is NOT NULL).
func (r *RecordConstructorValue) Type() Type {
	fields := make([]Field, len(r.Fields))
	for i, f := range r.Fields {
		var ft Type = UnknownType
		if f.Value != nil {
			ft = f.Value.Type()
		}
		// Java normalizes names centrally, in Type.Record.fromFields ->
		// normalizeFields (Type.java:2617-2682), not at each caller: "no field
		// is unnamed" is an invariant of the record TYPE. An unnamed field
		// would otherwise reach the name-keyed readers as "", and
		// computeFieldNameToOrdinal's analogue has no entry to return.
		name := f.Name
		if name == "" {
			name = OrdinalFieldName(i)
		}
		fields[i] = Field{
			Name:      name,
			FieldType: ft,
			Ordinal:   i,
		}
	}
	// Measured here rather than reasoned about at the call sites: this is the
	// GENERIC derivation path, and whether it ever derives a DOTTED `LEG.COL`
	// row decides whether it is a second producer of the row a leg-table
	// population would target. The live member-agreement guard adopts a
	// populated leg table over an empty one, so a second UNPOPULATED producer is
	// harmless; a second producer that states DIFFERENT boundaries is the
	// plan-level conflict.
	if LegIdentityCensusEnabled() {
		RecordDottedRowTypeDerivation(fields)
	}
	// A named struct literal carries its declared name into the result type,
	// exactly as Java's ofColumnsAndName resolves the type through
	// Type.Record.withName (RecordConstructorValue.java:485-487).
	// A successful record constructor always produces a record object. Its
	// fields may independently be nullable, but the container itself is not.
	// This is Java's computeResultType(columns, false) contract; nullable record
	// results require an explicit nullable owner/boundary rather than an
	// accidental default on every RC.
	return &RecordType{RecordName: r.typeName, Nullable: false, Fields: fields}
}

// Name returns the debug-print kind.
func (*RecordConstructorValue) Name() string { return "record" }

// Evaluate produces the constructed record.
//
// STAMPED (the plan path): a dynamicpb message of the baked descriptor, which
// is what Java always produces (RecordConstructorValue.eval builds a
// DynamicMessage from the per-plan TypeRepository). This is the only form that
// can reach the driver as an api.Struct, because a bare map carries no
// declared field ORDER and no type identity.
//
// UNSTAMPED: the name-keyed map. This is not a fallback for plan values — every
// constructor in a plan is stamped by FinalizePlan before the plan is cached.
// It is the representation for constructors that never went through a plan
// walk at all: constant folding evaluates a constructor at build time (before
// any plan exists to walk), and unit tests hand-build constructors directly.
// Neither has a repository to bake against, and neither reaches the driver.
// A type with no message form (MessageDescriptorFor returns *ProtoTypeError)
// also stays here rather than failing the query.
func (r *RecordConstructorValue) Evaluate(evalCtx any) (any, error) {
	if r.desc != nil {
		return buildRecordMessage(r.desc, r.Fields, evalCtx)
	}
	out := make(map[string]any, len(r.Fields))
	for _, f := range r.Fields {
		fv, err := f.Value.Evaluate(evalCtx)
		if err != nil {
			return nil, err
		}
		out[f.Name] = fv
	}
	return out, nil
}

// --- PromoteValue --------------------------------------------------

// PromoteValue wraps a child Value to coerce it to a target SQL type
// when the analyzer inserts an implicit conversion. E.g.
// `int_col = 5.0` rewrites to `PromoteValue(int_col, FLOAT) = 5.0`
// so the comparison sees two FLOATs.
//
// Distinct from CastValue: Cast is an explicit `CAST(x AS T)` that
// the user wrote; Promote is machine-inserted and cost-modelled
// separately. Mirrors Java's `PromoteValue`.
//
// Evaluate converts numeric carriers to the target width and handles the
// STRING→UUID representation change. Other promotion families remain
// representation-preserving.
type PromoteValue struct {
	Child  Value
	Target Type
}

// NewPromoteValue constructs a PromoteValue. Rejects nil child and
// nil / Unknown Target — both are programmer errors.
func NewPromoteValue(child Value, target Type) *PromoteValue {
	if child == nil {
		panic("NewPromoteValue: child is nil")
	}
	if target == nil || target.Code() == TypeCodeUnknown {
		panic("NewPromoteValue: target is UnknownType; use CastValue if target is genuinely unknown")
	}
	return &PromoteValue{Child: child, Target: target}
}

// Children returns the single child as a one-element slice.
func (p *PromoteValue) Children() []Value { return []Value{p.Child} }

// Type returns the promotion target. Nullability is inherited from
// the child — promoting a NOT NULL value preserves NOT NULL.
func (p *PromoteValue) Type() Type {
	if p.Target == nil {
		return UnknownType
	}
	childNullable := true
	if p.Child != nil {
		if ct := p.Child.Type(); ct != nil {
			childNullable = ct.IsNullable()
		}
	}
	return WithNullability(p.Target, childNullable)
}

// Name returns the debug-print kind.
func (*PromoteValue) Name() string { return "promote" }

// Evaluate applies numeric width conversion and STRING → UUID (Java's
// PromoteValue.STRING_TO_UUID, `UUID.fromString`): a UUID column has
// no native proto/SQL primitive, so `uuid_col = '<uuid>'` arrives as
// a STRING comparand. Promoting it to UUID here parses the canonical
// string into a neutral 16-byte value ([16]byte, matching Java's
// java.util.UUID — no `tuple` import so `values` stays wire-agnostic).
// The scan-range packer turns that [16]byte into a `tuple.UUID` at the
// FDB wire boundary, so the equality probe hits the 0x30 index entry
// instead of packing a 0x02 string that never matches.
func (p *PromoteValue) Evaluate(evalCtx any) (any, error) {
	childResult, err := p.Child.Evaluate(evalCtx)
	if err != nil {
		return nil, err
	}
	if !IsUuid(p.Target) {
		return coerceNumericResult(childResult, p.Target), nil
	}
	switch v := childResult.(type) {
	case nil:
		// NULL promotes to NULL (SQL NULL propagation).
		return nil, nil
	case string:
		u, perr := uuid.Parse(v)
		if perr != nil {
			// Java verbatim wording (SemanticException INVALID_UUID_VALUE).
			return nil, fmt.Errorf("Invalid UUID value for the UUID type %s", v)
		}
		return [16]byte(u), nil
	case [16]byte:
		// Already a neutral UUID (e.g. an index-sourced INL join key);
		// pass through unchanged — nothing to parse.
		return v, nil
	default:
		return childResult, nil
	}
}

// --- QuantifiedObjectValue -----------------------------------------

// QuantifiedObjectValue is the sealed read view of one correlation-bearing
// whole object. Its flowed type is an immutable exact snapshot, not a mutable
// ordinary Type graph retained from the caller.
type QuantifiedObjectValue interface {
	Value
	Correlation() CorrelationIdentifier
	FlowedType() Type
	isQuantifiedObjectValueView()
}

type quantifiedObjectValue struct {
	correlation CorrelationIdentifier
	flowed      *exactType
	// sourceLayout is physical provenance captured defensively from the
	// constructor input. It is excluded from semantic equality/hash and from
	// FlowedType; OrdinalLayout factories are its only execution consumer.
	sourceLayout *qovRecordLayout
}

// NewQuantifiedObjectValue snapshots flowed and returns an exact QOV. The
// ordinary constructor cannot mint the reserved current correlation; current
// handles belong to the checked owner-value builder.
func NewQuantifiedObjectValue(
	correlation CorrelationIdentifier,
	flowed Type,
) (QuantifiedObjectValue, error) {
	if correlation.IsZero() {
		return nil, resolutionError(CorrelationZero, "qov.correlation", "correlation is zero")
	}
	if correlation.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, "qov.correlation", "current is owner-scoped")
	}
	handle, err := SnapshotExactType(flowed)
	if err != nil {
		return nil, err
	}
	exact := handle.(*exactType)
	if exact.code == TypeCodeNull || exact.code == TypeCodeRelation {
		return nil, resolutionError(TypeMalformedCode, "qov.flowed", "QOV root must be an object or scalar exact type")
	}
	return &quantifiedObjectValue{
		correlation:  correlation,
		flowed:       exact,
		sourceLayout: snapshotQOVRecordLayout(flowed),
	}, nil
}

// AsQuantifiedObjectValue exact-recognizes the package-owned concrete node.
func AsQuantifiedObjectValue(value Value) (QuantifiedObjectValue, bool) {
	qov, ok := value.(*quantifiedObjectValue)
	if !ok || qov == nil || qov.flowed == nil || qov.correlation.IsZero() {
		return nil, false
	}
	return qov, true
}

func (*quantifiedObjectValue) isQuantifiedObjectValueView() {}

func (q *quantifiedObjectValue) Correlation() CorrelationIdentifier {
	return q.correlation
}

func (q *quantifiedObjectValue) FlowedType() Type {
	if q == nil || q.flowed == nil {
		return nil
	}
	return q.flowed.thaw()
}

// SharedFlowedType is FlowedType WITHOUT the defensive copy, for readers that
// only ask the graph a question and never retain or mutate it.
//
// THE DEFENSIVE COPY IS DELIBERATE AND PINNED (a getter that leaks a mutable
// graph lets a caller rename the carrier's own fields under it), so it stays
// the default and this is the deliberate opt-out — named so a reader has to
// mean it. The opt-out exists because the executor asks the same question once
// per ROW: does this row's shape equal the carrier's. Answering it by
// rebuilding the whole graph made a 20k-row scan allocate ~4M objects
// reconstructing a value that is a pure function of an immutable handle.
//
// Callers must treat the result as READ-ONLY. Use FlowedType anywhere the graph
// is stored, handed onward, or modified.
func SharedFlowedType(value QuantifiedObjectValue) Type {
	exact, ok := value.(*quantifiedObjectValue)
	if !ok || exact == nil || exact.flowed == nil {
		return nil
	}
	return exact.flowed.thawShared()
}

// Children returns an empty slice — the quantifier is a leaf in
// the Value tree, with its correlation link being external metadata
// (not a child Value).
func (*quantifiedObjectValue) Children() []Value { return []Value{} }

// Type returns a fresh copy of the exact flowed type. Nullability widening is
// performed once at the quantifier edge, not unconditionally here.
//
// The fresh copy is DELIBERATE and is pinned by
// TestRFC232QOVSnapshotsAndDefensivelyThawsItsType, which mutates one Type()
// result and requires the next to be unaffected. It was tried as a shared
// read-only graph — Type() is the generic accessor the planner calls while
// hashing and comparing, and it accounted for 78% of the allocations under
// FlowedType — and the pin correctly rejected it: unlike the constant-valued
// implementations that return package singletons, this graph is derived per
// call from a handle whose identity a QOV and every memo boundary depend on.
//
// SharedFlowedType is the named opt-out for readers that only ask the graph a
// question, and it is where that saving belongs.
func (q *quantifiedObjectValue) Type() Type { return q.FlowedType() }

// Name returns the debug-print kind.
func (*quantifiedObjectValue) Name() string { return "quantifier" }

// Evaluate extracts the row bound to this quantifier's correlation. The
// ordinal row is the sole runtime row; the eval-context shapes this handles:
//
//   - *RowEvalContext — a correlation binding for q.Correlation (an outer
//     quantifier), else the frontier Positional row; nil if neither.
//   - OrdinalRow — a bare frontier PositionalRow IS this quantifier's object.
//   - CorrelationBinder — the row bound to q.Correlation (nil if unbound).
//   - anything else — nil.
//
// Downstream FieldValue / nested-field resolvers then read a specific column off
// the returned row by ordinal.
func (q *quantifiedObjectValue) Evaluate(evalCtx any) (any, error) {
	if evalCtx == nil {
		return nil, nil
	}
	switch ctx := evalCtx.(type) {
	case *RowEvalContext:
		if ctx.Objects != nil {
			value, present, err := ctx.Objects.GetQuantifiedBinding(q)
			if err != nil {
				return nil, err
			}
			if !present {
				return nil, resolutionError(UnboundCorrelation, "qov.binding",
					fmt.Sprintf("exact QOV %q (%s) has no declared runtime binding", q.correlation.Name(), q.FlowedType()))
			}
			return value, nil
		}
		if ctx.Correlations != nil {
			if val, ok := ctx.Correlations.GetCorrelationBinding(q.correlation); ok {
				return val, nil
			}
		}
		// A bare QOV whole-row read resolves to the ordinal Positional row
		// on the frontier (downstream FieldValue reads it by ordinal). No
		// name-keyed fallback exists.
		if ctx.Positional != nil {
			return ctx.Positional, nil
		}
		return nil, nil
	case OrdinalRow:
		// A bare frontier PositionalRow (the posNeedsCtx-false fast path in
		// frontierRowContext flows the ordinal row directly, not wrapped in a
		// RowEvalContext) IS this quantifier's object — the same resolution the
		// *RowEvalContext Positional fallback gives, so a QOV-child FieldValue
		// (`c2.name`) reads its column by ordinal against it.
		return ctx, nil
	case QuantifiedObjectBinder:
		value, present, err := ctx.GetQuantifiedBinding(q)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, resolutionError(UnboundCorrelation, "qov.binding",
				fmt.Sprintf("exact QOV %q (%s) has no declared runtime binding", q.correlation.Name(), q.FlowedType()))
		}
		return value, nil
	case CorrelationBinder:
		val, ok := ctx.GetCorrelationBinding(q.correlation)
		if !ok {
			return nil, nil
		}
		return val, nil
	}
	return nil, nil
}

// GetCorrelatedTo implements the Correlated interface — returns
// a set containing this quantifier's correlation.
func (q *quantifiedObjectValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{q.correlation: {}}
}

// --- AggregateValue -----------------------------------------------

// AggregateOp identifies an aggregate function. Mirrors the subset
// of Java's `AggregateValue` that the embedded engine currently
// lowers to a Record Layer aggregate-index query.
type AggregateOp int

// Enum of aggregate operators Go supports. Ordered to match
// Java's bi-map so serialised plans round-trip.
const (
	AggInvalid   AggregateOp = iota // unassigned — rejects if ever evaluated
	AggCount                        // COUNT(expr)
	AggCountStar                    // COUNT(*)
	AggSum                          // SUM(expr)
	AggMin                          // MIN(expr)
	AggMax                          // MAX(expr)
	AggAvg                          // AVG(expr) — rejects at Evaluate, no streaming impl
)

// Symbol returns the canonical SQL function name.
func (op AggregateOp) Symbol() string {
	switch op {
	case AggCount:
		return "COUNT"
	case AggCountStar:
		return "COUNT(*)"
	case AggSum:
		return "SUM"
	case AggMin:
		return "MIN"
	case AggMax:
		return "MAX"
	case AggAvg:
		return "AVG"
	default:
		return "?AGG?"
	}
}

// AggregateValue represents an aggregate function application —
// `COUNT(*)`, `SUM(col)`, `MIN(expr)`, etc. The Operand is the
// argument (nil for COUNT(*)); the Op identifies which aggregate.
//
// AggregateValue does NOT implement per-row Evaluate — aggregates
// span rows and need an accumulator. Evaluate returns nil to make
// the ignore-of-row-context explicit; rule code identifies
// AggregateValues by type-assertion and routes them to the aggregate
// operator (hash-agg, streaming-agg, index-backed agg) at build
// time.
type AggregateValue struct {
	Op      AggregateOp
	Operand Value // nil iff Op == AggCountStar
}

// NewAggregateValue constructs an AggregateValue. Panics on
// inconsistent op/operand combos (AggCountStar with operand,
// non-CountStar without operand) — these are static programmer
// errors, not runtime data problems.
func NewAggregateValue(op AggregateOp, operand Value) *AggregateValue {
	if op == AggCountStar && operand != nil {
		panic("NewAggregateValue: COUNT(*) takes no operand")
	}
	if op != AggCountStar && op != AggInvalid && operand == nil {
		panic("NewAggregateValue: aggregate requires an operand (use COUNT(*) for star)")
	}
	return &AggregateValue{Op: op, Operand: operand}
}

// Children returns the operand as a single child (empty for
// COUNT(*)). Lets WalkValue traverse aggregate arguments.
func (a *AggregateValue) Children() []Value {
	if a.Operand == nil {
		return []Value{}
	}
	return []Value{a.Operand}
}

// Type returns the rich Type the aggregate produces, matching Java's
// per-operator resultTypeCode (NumericAggregationValue.PhysicalOperator):
//   - COUNT / COUNT(*): NullableLong. COUNT is non-null inside its own group,
//     but a GroupBy row can itself be null-supplied by an outer relational
//     edge; the exact flowed aggregate-row contract therefore carries the
//     widened nullable type.
//   - AVG: NullableDouble — AVG is real division, always DOUBLE
//     regardless of operand type (Java AVG_{I,L,F,D} → DOUBLE). NOT
//     operand-derived: AVG(BIGINT) is DOUBLE, not LONG.
//   - SUM / MIN / MAX: nullable; Type derived from the operand when
//     available, else NullableLong (Java SUM_L→LONG, MIN/MAX→operand).
func (a *AggregateValue) Type() Type {
	switch a.Op {
	case AggCount, AggCountStar:
		return NullableLong
	case AggAvg:
		return NullableDouble
	case AggSum, AggMin, AggMax:
		if a.Operand != nil {
			ot := a.Operand.Type()
			if ot == nil {
				return NullableLong
			}
			return WithNullability(ot, true)
		}
		return NullableLong
	}
	return UnknownType
}

// Name returns the debug-print kind.
func (*AggregateValue) Name() string { return "agg" }

// Evaluate returns AggregateEvalError — aggregates are multi-row and have
// no single-row Evaluate semantics. Rule / plan code type-asserts
// AggregateValue and routes it to an accumulator instead of calling
// Evaluate. The misuse path (an aggregate in a per-row scalar position,
// e.g. WHERE COUNT(*) > 0) is reachable from user data, so it returns a
// typed error rather than panicking (RFC-087 residual-panic audit).
func (a *AggregateValue) Evaluate(any) (any, error) {
	// Reachable from user data: an aggregate misused on the per-row scalar
	// path (e.g. `WHERE COUNT(*) > 0`). Java rejects this at plan time; Go's
	// planner doesn't yet, so it reaches here — return an error (not a panic)
	// per the RFC-087 residual-panic audit. The correct aggregate path goes
	// through the aggregator, which never calls Evaluate.
	return nil, &AggregateEvalError{Message: "aggregate function is not allowed here (e.g. in WHERE); use HAVING or a subquery"}
}

// GetIndexTypeName returns the FDB index-type name that backs this
// aggregate when an aggregate index is available. Mirrors Java's
// `IndexableAggregateValue.getIndexTypeName()` (Java's interface
// marker; Go uses an accessor on AggregateValue itself).
//
// The mapping:
//
//	AggCount     → COUNT_NOT_NULL  (counts non-null values)
//	AggCountStar → COUNT           (counts all rows incl. NULL)
//	AggSum       → SUM
//	AggMin       → permuted_min    (current-extremum index, tracks deletes)
//	AggMax       → permuted_max    (current-extremum index, tracks deletes)
//	AggAvg       → ""              (no direct index — computed from
//	                                 SUM/COUNT pair instead)
//	AggInvalid   → ""
//
// Returns the empty string when no FDB index type backs this
// aggregate. The planner consults this to decide whether to lower
// to an index-aggregate scan (constant-cost lookup) or fall back
// to a streaming aggregator (linear-time row scan).
func (a *AggregateValue) GetIndexTypeName() string {
	switch a.Op {
	case AggCount:
		return "COUNT_NOT_NULL"
	case AggCountStar:
		return "COUNT"
	case AggSum:
		return "SUM"
	case AggMin:
		// permuted_min (the recordlayer IndexTypePermutedMin value; a string
		// literal here because the values package cannot import recordlayer).
		// Java NumericAggregationValue.Min.getIndexTypeName() = PERMUTED_MIN — a
		// current-extremum index that tracks deletes, NOT the monotone min_ever.
		return "permuted_min"
	case AggMax:
		return "permuted_max"
	case AggAvg, AggInvalid:
		return ""
	}
	return ""
}

// IndexableAggregate is the Go-side counterpart to Java's
// IndexableAggregateValue interface. Any Value that has an index-
// backed aggregate form can implement this — currently only
// AggregateValue (when its Op has a non-empty index-type name).
//
// Planner / matcher code can type-assert against this interface to
// pick aggregates eligible for index-scan lowering:
//
//	if iav, ok := v.(IndexableAggregate); ok && iav.GetIndexTypeName() != "" {
//	    // can lower to index-aggregate scan
//	}
type IndexableAggregate interface {
	Value
	GetIndexTypeName() string
}

var _ IndexableAggregate = (*AggregateValue)(nil)

// NonEvaluable is the Go-side counterpart to Java's
// `Value.NonEvaluableValue` interface marker. Any Value that
// can't be evaluated at runtime (plan-time-only placeholders like
// AggregateValue, IndexOnlyAggregateValue) implements this marker.
//
// Planner / matcher code can type-assert against this to refuse to
// pass non-evaluable Values to runtime evaluators.
//
// Java's NonEvaluableValue is a true marker interface (no methods);
// the Go equivalent uses one method whose presence (and the implied
// `true` return) IS the marker.
type NonEvaluable interface {
	Value
	IsNonEvaluable() bool
}

// IsNonEvaluable is a helper that any Value can call to check
// whether v is plan-time-only. Avoids type-assertion boilerplate
// in callers.
func IsNonEvaluable(v Value) bool {
	if ne, ok := v.(NonEvaluable); ok {
		return ne.IsNonEvaluable()
	}
	return false
}

// IsNonEvaluable on AggregateValue returns true — aggregates are
// multi-row and can't be evaluated per-row by the standard
// Evaluate path. Implements NonEvaluable.
func (*AggregateValue) IsNonEvaluable() bool { return true }

var _ NonEvaluable = (*AggregateValue)(nil)

// IndexOnly is the Go-side counterpart to Java's
// `Value.IndexOnlyValue` interface marker. Any Value whose result
// can ONLY be produced by an index scan (vs a streaming
// aggregator over the base records) implements this marker.
//
// Used by: RowNumberValue, DistanceRowNumberValue, IndexOnlyAggregateValue.
//
// Planner / matcher code can type-assert against this to refuse to
// optimise paths that would require running the value over a base-
// record scan — they MUST be matched against an index, otherwise
// the plan fails to compile.
type IndexOnly interface {
	Value
	IsIndexOnly() bool
}

// IsIndexOnly is a helper that any Value can call to check whether
// v requires an index scan to produce its result.
func IsIndexOnly(v Value) bool {
	if io, ok := v.(IndexOnly); ok {
		return io.IsIndexOnly()
	}
	return false
}
