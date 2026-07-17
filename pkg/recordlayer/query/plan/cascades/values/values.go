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
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Canonical ISO 8601 layouts for temporal value formatting/parsing.
// Mirrors functions.TimestampLayout / functions.DateLayout — duplicated
// here because values/ must not import functions/ (layering: values is
// the leaf package that predicates/ and cascades/ depend on).
const (
	timestampLayout = "2006-01-02 15:04:05"
	dateLayout      = "2006-01-02"
)

// The legacy `ValueType` enum (TypeUnknown / TypeInt / TypeString /
// TypeBool / TypeFloat) is retired — every Value impl's
// Type() returns the rich Type directly. The names below remain
// as Type-typed vars so existing call sites (`Typ: values.TypeInt`)
// keep working — the value's Go type changes (Type instead of int),
// the constant name doesn't.
//
// Legacy bridge retirement: RFC-025.
var (
	// TypeUnknown is the placeholder for "type not yet inferred".
	// Maps to the canonical UnknownType singleton.
	TypeUnknown Type = UnknownType
	// TypeInt is the legacy name for the package's default integer
	// width — bridged to LONG (BIGINT default; matches Java Record
	// Layer's int64 representation).
	TypeInt Type = NullableLong
	// TypeString is the legacy name for STRING — bridged to
	// NullableString.
	TypeString Type = NullableString
	// TypeBool is the legacy name for BOOLEAN — bridged to
	// NullableBoolean. Note BooleanValue's Type() returns
	// NotNullBoolean (literals are NOT NULL); compare via
	// `.Code() != TypeCodeBoolean` when nullability is irrelevant.
	TypeBool Type = NullableBoolean
	// TypeFloat is the legacy name for the package's default float
	// width — bridged to DOUBLE (matches Java Record Layer's
	// float64 representation).
	TypeFloat Type = NullableDouble
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
type FieldValue struct {
	Field string
	Typ   Type
	Child Value // base value (nil = legacy flat field reference)

	// Resolved is the baked-ordinal path — Java's construction-time
	// FieldPath resolution, where the accessor IS an ordinal and runtime access
	// is positional: resolveOrdinal returns the accessor's ordinal directly, so
	// a positional-row read is row.Get(ordinal) — position-preserving by
	// construction, and therefore sound under DUPLICATE output names, which
	// every name-based resolution collapses (RecordType.FieldIndex is
	// first-match). Field is a DISPLAY name for diagnostics and Explain.
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
	//     machinery over a typed leg QOV (NewFieldValueOfOrdinal) or fused by
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
	// from ResolvedAccessor equality). Every FieldValue copy/rebuild site MUST
	// preserve Resolved — dropping it silently degrades a baked node to lazy,
	// which is loud at evaluation but conflates duplicate same-named columns
	// in plan-time matching. For baked nodes Field equals the LAST accessor's
	// name (Java getLastFieldName) — display only.
	Resolved *FieldPath
}

// ResolvedAccessor is the construction-time-resolved accessor a BAKED
// FieldValue carries — Java's FieldValue.ResolvedAccessor (FieldValue.java:~630),
// whose equals/hashCode are ordinal-only.
//
// IMMUTABLE after construction: FieldValue copy sites deliberately SHARE the
// pointer (withChildren, the pullup/pushdown passthrough copies). Any future
// change to the accessor must REPLACE it
// with a new value, never mutate in place, or every shared copy silently
// changes identity.
type ResolvedAccessor struct {
	// Field is the PER-STEP display name (Java ResolvedAccessor.getField();
	// "" = pure ordinal access, Java's null name). NOT part of the path's
	// identity — Java's element equality is ordinal-only
	// (FieldValue.java:675-689); the name survives only for nested-record
	// descent by per-step name (descendResolvedPath, into a proto.Message or a
	// nested record map) and Explain rendering.
	Field   string
	Ordinal int
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
// (NewFieldPathOfSingle, WithSuffix) uphold it; Root()/Last() panic on a
// hand-built violation rather than tolerating it.
type FieldPath struct {
	Accessors []ResolvedAccessor

	// FrontierPinned marks a MACHINERY-OWNED bake: the node was built by the
	// join/gather seed machinery (NewFieldValueOfOrdinal over a typed
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

// NewFieldPathOfSingle is Java's FieldPath.ofSingle (FieldValue.java:563) —
// the constructor every single-accessor bake goes through.
func NewFieldPathOfSingle(field string, ordinal int, frontierPinned bool) *FieldPath {
	return &FieldPath{
		Accessors:      []ResolvedAccessor{{Field: field, Ordinal: ordinal}},
		FrontierPinned: frontierPinned,
	}
}

// WithSuffix returns a NEW path with suffix's accessors appended — Java's
// FieldPath.withSuffix (FieldValue.java:525-534); neither input is mutated.
// The frontier pin comes from the RECEIVER: fusing inner.WithSuffix(outer)
// keeps the INNER path's root read context (the compose rule's shape), and
// the pin governs exactly that root.
func (p *FieldPath) WithSuffix(suffix *FieldPath) *FieldPath {
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
	merged := make([]ResolvedAccessor, 0, len(p.Accessors)+len(suffix.Accessors))
	merged = append(merged, p.Accessors...)
	merged = append(merged, suffix.Accessors...)
	return &FieldPath{Accessors: merged, FrontierPinned: p.FrontierPinned}
}

// Root returns the first accessor — the one the ROOT read context resolves
// (a positional row slot).
func (p *FieldPath) Root() ResolvedAccessor { return p.Accessors[0] }

// Last returns the final accessor — the path's display leaf (Java
// getLastFieldAccessor, FieldValue.java:459).
func (p *FieldPath) Last() ResolvedAccessor { return p.Accessors[len(p.Accessors)-1] }

// Single returns the path's only accessor when the path is single-step —
// the shape the plain join-seed probes expect; ok=false for multi-accessor
// paths (those probes DECLINE fused shapes).
func (p *FieldPath) Single() (ResolvedAccessor, bool) {
	if len(p.Accessors) != 1 {
		return ResolvedAccessor{}, false
	}
	return p.Accessors[0], true
}

// Equals is Java FieldPath.equals (FieldValue.java:411-420): element-wise list
// equality over the accessors' ORDINALS (Java's
// ResolvedAccessor.equals is getOrdinal()-only, :675-689). The per-step Field
// is NOT compared — display/rendering, not identity. FrontierPinned is
// likewise excluded (evaluation contract, not identity).
func (p *FieldPath) Equals(o *FieldPath) bool {
	if p == o {
		return true
	}
	if p == nil || o == nil || len(p.Accessors) != len(o.Accessors) {
		return false
	}
	for i := range p.Accessors {
		// ORDINAL-ONLY element identity: Java's
		// ResolvedAccessor.equals compares getOrdinal() alone
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
		if fv, ok := n.(*FieldValue); ok && fv.Resolved != nil && fv.Resolved.FrontierPinned {
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
		qov, isQOV := f.Value.(*QuantifiedObjectValue)
		if !isQOV {
			return false
		}
		if _, dup := seen[qov.Correlation]; dup {
			return false
		}
		seen[qov.Correlation] = struct{}{}
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
		if qov, isQOV := f.Value.(*QuantifiedObjectValue); isQOV {
			if qov.Type() == nil || qov.Type().Code() == TypeCodeUnknown {
				return false
			}
			roots[qov.Correlation] = struct{}{}
			continue
		}
		fv, isFV := f.Value.(*FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			return false
		}
		qov, isQOV := fv.Child.(*QuantifiedObjectValue)
		if !isQOV {
			return false
		}
		roots[qov.Correlation] = struct{}{}
	}
	return len(roots) >= 2
}

func (f *FieldValue) Children() []Value {
	if f.Child == nil {
		return []Value{}
	}
	return []Value{f.Child}
}

func (f *FieldValue) Name() string { return "field" }

// Type returns the field's rich Type. FieldValue stores
// the column type as-is; callers that know NOT NULL information
// from the catalog set Typ to the non-nullable form.
func (f *FieldValue) Type() Type {
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
func (f *FieldValue) evaluateOrdinal(row OrdinalRow) (any, error) {
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

// frontierContractGuard enforces the FRONTIER CONTRACT for a
// FrontierPinned baked node: the executor guarantees a positional row, so a
// non-nil context that is NOT a positional/ordinal row is a planner/executor
// bug — reported as a loud *BakedNameContextError rather than a silent NULL that
// would hide it. An unpinned node (and a nil context — the
// appendNullLeg / nil-binding NULL) returns nil (no
// violation). The pinned-node "never silently NULL off the positional
// frontier" invariant lives here, at the non-positional tail of Evaluate /
// evaluateCorrelated.
func (f *FieldValue) frontierContractGuard() error {
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
func (f *FieldValue) descendResolvedPath(rootVal any) (any, error) {
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
			// executor's row layer flows nested records verbatim) — descend
			// by field NAME, Java's FieldValue.eval →
			// MessageHelpers.getFieldOnMessage. Unset singular field = NULL
			// (proto3 presence rules ride protoreflect.Has).
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
// Exported as the single source of truth the executor's kind-based
// scalarProtoToGo delegates to, so the record→row and struct-descent
// conversions cannot drift on the scalar arms.
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

func (f *FieldValue) Evaluate(evalCtx any) (any, error) {
	if f.Child != nil {
		if qov, isQOV := f.Child.(*QuantifiedObjectValue); isQOV {
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
	// Unrecognized NON-NIL context: nothing resolved. Production flows an
	// OrdinalRow / *RowEvalContext(+Positional) / CorrelationBinder / nil — reaching
	// here is a planner/executor bug, LOUD for pinned and unpinned alike;
	// a silent NULL would hide it. (A nil context is the
	// appendNullLeg NULL, handled above.)
	return nil, &UnboundEvalContextError{Field: f.Field, CtxType: fmt.Sprintf("%T", evalCtx)}
}

func (f *FieldValue) evaluateCorrelated(qov *QuantifiedObjectValue, evalCtx any) (any, error) {
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
			return nil, &UnboundEvalContextError{Field: f.Field, Correlation: qov.Correlation.Name(), CtxType: "OrdinalRow (multi-leg row cannot serve a source-relative ordinal)"}
		}
		return f.evaluateOrdinal(ctx)
	case *RowEvalContext:
		if ctx.Correlations != nil {
			if bound, ok := ctx.Correlations.GetCorrelationBinding(qov.Correlation); ok {
				// A quantifier bound to an ordinal-model row resolves by
				// ordinal. A nil binding is the null leg (outer-join
				// no-match) — NULL, not loud. A FrontierPinned node bound to any
				// other non-ordinal value (a name-keyed map, a stray struct) is a
				// frontier-contract violation — loud.
				if row, ok := bound.(OrdinalRow); ok {
					return f.evaluateOrdinal(row)
				}
				if bound != nil {
					if err := f.frontierContractGuard(); err != nil {
						return nil, err
					}
				}
				return bound, nil
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
				return nil, &UnboundEvalContextError{Field: f.Field, Correlation: qov.Correlation.Name(), CtxType: "*RowEvalContext (multi-leg row cannot serve a source-relative ordinal)"}
			}
			return f.evaluateOrdinal(ctx.Positional)
		}
		// Nothing matched and no frontier row supplied: a dangling correlation.
		// Loud for pinned and unpinned alike — never a silent NULL.
		return nil, &UnboundEvalContextError{Field: f.Field, Correlation: qov.Correlation.Name(), CtxType: "*RowEvalContext (correlation unbound, no positional)"}
	case CorrelationBinder:
		if bound, ok := ctx.GetCorrelationBinding(qov.Correlation); ok {
			if row, ok := bound.(OrdinalRow); ok {
				return f.evaluateOrdinal(row)
			}
			// nil binding = null leg (NULL); any other non-ordinal
			// binding is a frontier-contract violation for a pinned node.
			if bound != nil {
				if err := f.frontierContractGuard(); err != nil {
					return nil, err
				}
			}
			return bound, nil
		}
		// Correlation unbound in this binder: a dangling reference. Loud.
		return nil, &UnboundEvalContextError{Field: f.Field, Correlation: qov.Correlation.Name(), CtxType: fmt.Sprintf("%T (unbound)", ctx)}
	}
	// Unrecognized NON-NIL context (no ordinal row supplied): nothing resolved.
	// Loud for pinned and unpinned alike, mirroring Evaluate's
	// own tail; a silent NULL would hide a planner/executor bug.
	return nil, &UnboundEvalContextError{Field: f.Field, Correlation: qov.Correlation.Name(), CtxType: fmt.Sprintf("%T", evalCtx)}
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
func (f *FieldValue) resolveOrdinal() (int, bool) {
	// A BAKED node's position was resolved at construction
	// (NewFieldValueOfOrdinal / NewFieldValueWithResolvedOrdinal) — it is
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
	// There is NO runtime name-derive fallback (no rt.FieldIndex here). A
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
// a FrontierPinned (machinery-owned) or multi-accessor path is final. At
// runtime it reads through its source's leg window (legWindowBinder). This is
// the source-vs-machinery half of the Go two-level-lowering bridge Java has no
// analog for (see FieldPath.FrontierPinned).
func (f *FieldValue) SourceRelativeBaked() bool {
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
func (f *FieldValue) RootIsLegRelativeUnpinned() bool {
	return f.Resolved != nil && !f.Resolved.FrontierPinned
}

// NewFieldValue constructs a FieldValue with a child (base) value.
// Mirrors Java's FieldValue(childValue, FieldPath).
func NewFieldValue(child Value, field string, typ Type) *FieldValue {
	return &FieldValue{Field: field, Typ: typ, Child: child}
}

// NewFlatFieldValue constructs a childless LAZY FieldValue (no baked ordinal).
// PLAN-TIME ONLY: such a node fails loud if it ever reaches runtime evaluation
// (the name-resolution fallback is deleted). Callers are match-candidate /
// ordering-hint carriers (compared by name, never evaluated) and resolver/
// translator trees the bake walks rewrite before the plan finalizes.
func NewFlatFieldValue(field string, typ Type) *FieldValue {
	return &FieldValue{Field: field, Typ: typ}
}

// NewFieldValueWithResolvedOrdinal constructs a flat FieldValue carrying a
// plan-time-resolved ordinal accessor — Java's FieldValue.ofOrdinalNumber. The
// read is row.Get(ordinal) (positional by construction, duplicate-name-proof).
// The accessor is UNPINNED (a source-relative bake, not machinery-owned — the
// caller resolves a column against a source's declared column order).
// Distinct from NewOrdinalFieldValue, which bakes the anonymous `_<ordinal>`
// WITH-ORDINALITY element/ordinal columns.
func NewFieldValueWithResolvedOrdinal(field string, ordinal int, typ Type) *FieldValue {
	return &FieldValue{Field: field, Typ: typ, Resolved: NewFieldPathOfSingle(field, ordinal, false)}
}

// NewCorrelatedFieldValueWithResolvedOrdinal constructs a QOV-child FieldValue
// carrying a plan-time-resolved SOURCE-RELATIVE ordinal accessor — the
// construction bind for the CORRELATED resolver arm (Java's
// FieldValue.ofFieldName over a QuantifiedObjectValue, FieldValue.java:273-299:
// the FieldPath ordinal is fixed against the referent's result type at
// construction). The accessor is UNPINNED and single-accessor, so the node is
// SourceRelativeBaked: ordinal-bound against the reference's OWN source row
// (the resolver's declared-column-order ordinal), but NOT rebased onto any
// composed frontier — every translator/executor walk that rebinds lazy
// references treats it identically (the SourceRelativeBaked widenings). At
// runtime the correlation binds a source-shaped row (the source's own row or
// its leg window — both flow the source's declared column order), so the
// source-relative ordinal reads the right slot.
func NewCorrelatedFieldValueWithResolvedOrdinal(child Value, field string, ordinal int, typ Type) *FieldValue {
	return &FieldValue{Field: field, Typ: typ, Child: child, Resolved: NewFieldPathOfSingle(field, ordinal, false)}
}

// NewOrdinalFieldValue accesses a record field by ORDINAL position,
// mirroring Java's `FieldValue.ofOrdinalNumber(child, ordinal)`. The field
// DISPLAY name is the ordinal name `_0`/`_1` (see OrdinalFieldName — the
// WITH-ORDINALITY Explode's anonymous element/ordinal columns), and the
// ordinal is BAKED at construction: the Explode's positional
// row serves slot `ordinal` directly. Used by the lateral-unnest lowering to
// bind the AS alias to field 0 (element) and the AT alias to field 1 (the INT
// NOT NULL ordinal).
func NewOrdinalFieldValue(child Value, ordinal int, typ Type) *FieldValue {
	return &FieldValue{Field: OrdinalFieldName(ordinal), Typ: typ, Child: child, Resolved: NewFieldPathOfSingle(OrdinalFieldName(ordinal), ordinal, false)}
}

// OrdinalBakeError is the loud construction-time error NewFieldValueOfOrdinal
// returns when the requested ordinal cannot be resolved against the child's
// flowed type — the Go analog of Java's resolveFieldPath raising
// SemanticException(FIELD_ACCESS_INPUT_NON_RECORD_TYPE) for a non-record child
// and IndexOutOfBoundsException for an out-of-range ordinal
// (FieldValue.java:273-296). Never a silent fallback: a bake failure is a
// planner bug, not a NULL.
type OrdinalBakeError struct {
	Ordinal   int
	ChildType Type   // the child's flowed type (nil for a nil child)
	Reason    string // which precondition failed: nil child / non-record child / out of range
}

func (e *OrdinalBakeError) Error() string {
	return fmt.Sprintf("ordinal bake: cannot resolve ordinal %d: %s (child type %v)", e.Ordinal, e.Reason, e.ChildType)
}

// NewFieldValueOfOrdinal constructs a BAKED FieldValue accessing the child's
// record field by ORDINAL position — Java's
// `FieldValue.ofOrdinalNumber(childValue, ordinalNumber)` (FieldValue.java:335):
// the position is resolved ONCE, here, and carried on the node (Resolved);
// resolveOrdinal returns it without re-deriving from the child type. The
// DISPLAY name (Field) and Typ are read from the child's RecordType at
// `ordinal` — the name serves diagnostics/Explain; the ordinal is
// authoritative (it survives even when a runtime row's type names disagree
// with the display name). The bake is MACHINERY-OWNED (FrontierPinned): this
// is the join / gather seed constructor.
//
// Errors loudly (Java raises; no silent fallback) when the child does not
// flow a *RecordType or the ordinal is out of range.
func NewFieldValueOfOrdinal(child Value, ordinal int) (*FieldValue, error) {
	if child == nil {
		return nil, &OrdinalBakeError{Ordinal: ordinal, Reason: "nil child value"}
	}
	rt, ok := child.Type().(*RecordType)
	if !ok {
		return nil, &OrdinalBakeError{Ordinal: ordinal, ChildType: child.Type(), Reason: "child does not flow a record type"}
	}
	if ordinal < 0 || ordinal >= len(rt.Fields) {
		return nil, &OrdinalBakeError{Ordinal: ordinal, ChildType: rt, Reason: fmt.Sprintf("ordinal out of range for a %d-field record type", len(rt.Fields))}
	}
	fld := rt.Fields[ordinal]
	typ := fld.FieldType
	// Java FieldValue.computeResultType: the accessed field's type is
	// overridden NULLABLE when the child's record type is nullable — the
	// LEFT-outer null-supplying leg's record-level wrap
	// makes every column read through it nullable, because the padded row
	// serves NULL in every slot (how Java reports LEFT JOIN metadata
	// nullable without a per-column seed wrap). Keyed on the STORED Typ for
	// a QOV child: QOV.Type() blanket-wraps nullable (the pre-existing
	// pass-through rule), so the record-level marker is only observable on
	// q.Typ — the same authority every seed consumer reads.
	childNullable := rt.Nullable
	if qov, isQOV := child.(*QuantifiedObjectValue); isQOV {
		if srt, isRT := qov.Typ.(*RecordType); isRT {
			childNullable = srt.Nullable
		}
	}
	if childNullable && typ != nil && !typ.IsNullable() {
		typ = WithNullability(typ, true)
	}
	// FrontierPinned: this constructor is the join seed's — the executor
	// supplies positional rows for every context these nodes evaluate in, so
	// these nodes only ever resolve by ordinal.
	return &FieldValue{
		Field:    fld.Name,
		Typ:      typ,
		Child:    child,
		Resolved: NewFieldPathOfSingle(fld.Name, ordinal, true),
	}, nil
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
	case *FieldValue, *QuantifiedObjectValue, *AggregateValue, *ParameterValue,
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

// ProjectionColumnName is the projection output-column NAMING CONTRACT: the
// name a projected Value's result is keyed under, alias-absent, in the
// emitted positional row's type (executeProjection's posNames). A FieldValue
// projects under its (possibly dotted)
// Field; any other Value under its upper-cased explain rendering (a computed
// expression like `n + 1` is keyed "(N + 1)"). Shared here so the
// planner/translator side can READ a projection's output by the exact key the
// executor WRITES — reading by any other rendering (e.g. the logical layer's
// un-parenthesized "N + 1") is a loud
// OrdinalResolutionError on valid SQL.
func ProjectionColumnName(v Value) string {
	if fv, ok := v.(*FieldValue); ok {
		return fv.Field
	}
	return strings.ToUpper(ExplainValue(v))
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
func ColumnNameValue(v Value) string { return explainValueOrdinals(v, false) }

func explainValueOrdinals(v Value, withOrdinals bool) string {
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
	case *FieldValue:
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
		// discriminator). Display only: ProjectionColumnName's FieldValue arm
		// returns Field verbatim, so plain-field Datum keys and positional
		// slot names never change (a COMPUTED composite over a #-named field
		// shifts its derived key spelling consistently on writer and reader,
		// both sides of the shared contract).
		name := strings.ReplaceAll(cv.Field, "#", "##")
		// A multi-accessor baked path renders EVERY step as name#ordinal,
		// dot-joined (Java FieldPath.toString, FieldValue.java:428-433) — the
		// single-accessor rendering below is its one-step special case (the
		// step name IS cv.Field by construction).
		if cv.Resolved != nil && len(cv.Resolved.Accessors) > 1 {
			steps := make([]string, len(cv.Resolved.Accessors))
			for i, acc := range cv.Resolved.Accessors {
				steps[i] = strings.ReplaceAll(acc.Field, "#", "##")
				if withOrdinals {
					steps[i] += "#" + strconv.Itoa(acc.Ordinal)
				}
			}
			path := strings.Join(steps, ".")
			if cv.Child != nil {
				return explainValueOrdinals(cv.Child, withOrdinals) + "." + path
			}
			return path
		}
		if cv.Child != nil {
			name = explainValueOrdinals(cv.Child, withOrdinals) + "." + name
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
		return "(" + explainValueOrdinals(cv.Left, withOrdinals) + " " + cv.Op.symbol() + " " + explainValueOrdinals(cv.Right, withOrdinals) + ")"
	case *StrictRankLimitValue:
		// Renders as the strict adjustment it computes (max(0, K-1)); matches the
		// prior ArithmeticValue "(K - 1)" form so plan output is unchanged.
		return "(" + explainValueOrdinals(cv.K, withOrdinals) + " - 1)"
	case *BooleanValue:
		if cv.Value == nil {
			return "NULL"
		}
		if *cv.Value {
			return "TRUE"
		}
		return "FALSE"
	case *CastValue:
		return "CAST(" + explainValueOrdinals(cv.Child, withOrdinals) + " AS " + explainTypeName(cv.Target) + ")"
	case *PromoteValue:
		return "PROMOTE(" + explainValueOrdinals(cv.Child, withOrdinals) + " TO " + explainTypeName(cv.Target) + ")"
	case *RecordConstructorValue:
		parts := make([]string, 0, len(cv.Fields))
		for _, f := range cv.Fields {
			parts = append(parts, f.Name+": "+explainValueOrdinals(f.Value, withOrdinals))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *NullValue:
		return "NULL"
	case *AggregateValue:
		if cv.Op == AggCountStar {
			return "COUNT(*)"
		}
		return cv.Op.Symbol() + "(" + explainValueOrdinals(cv.Operand, withOrdinals) + ")"
	case *QuantifiedObjectValue:
		return cv.Correlation.Name()
	case *ScalarFunctionValue:
		parts := make([]string, len(cv.Args))
		for i, a := range cv.Args {
			parts[i] = explainValueOrdinals(a, withOrdinals)
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
			parts[i] = explainValueOrdinals(a, withOrdinals)
		}
		sel := explainValueOrdinals(cv.Selector, withOrdinals)
		return "CASE(" + sel + ", [" + strings.Join(parts, ", ") + "])"
	case *ConditionSelectorValue:
		conds := make([]string, len(cv.Implications))
		for i, c := range cv.Implications {
			conds[i] = explainValueOrdinals(c, withOrdinals)
		}
		return "WHEN(" + strings.Join(conds, ", ") + ")"
	case *CardinalityValue:
		// Java: ExplainTokens.addFunctionCall(FunctionNames.CARDINALITY, ...).
		// Renders `cardinality(<child>)`, e.g. `cardinality(_.int_arr)`.
		return "cardinality(" + explainValueOrdinals(cv.Child, withOrdinals) + ")"
	case *ScalarSubqueryValue:
		return "(SCALAR_SUBQUERY " + cv.Alias.Name() + ")"
	case *UnmatchedAggregateValue:
		return "unmatched(" + cv.UnmatchedID.Name() + ")"
	case *ParameterObjectValue:
		return "$" + cv.ParameterName
	}
	return v.Name()
}

// explainTypeName renders a Type as a short SQL-ish name for the
// CAST / PROMOTE rendering in ExplainValue. Mirrors the legacy
// ValueType.String() output (`INT` / `STRING` / `BOOL` / `FLOAT` /
// `UNKNOWN`) — LONG/INT deliberately conflate to INT and DOUBLE/FLOAT
// to FLOAT so the rendered output, and the plan-cache keys derived via
// ExplainValue, stay byte-stable with the pre-retirement rendering.
func explainTypeName(t Type) string {
	if t == nil {
		return "UNKNOWN"
	}
	switch t.Code() {
	case TypeCodeInt, TypeCodeLong:
		return "INT"
	case TypeCodeString:
		return "STRING"
	case TypeCodeBoolean:
		return "BOOL"
	case TypeCodeFloat, TypeCodeDouble:
		return "FLOAT"
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
	Positional       OrdinalRow
	Binder           ParameterBinder
	Correlations     CorrelationBinder
	ScalarSubqueries map[CorrelationIdentifier]any // pre-evaluated scalar subquery results
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
// The supported family is the one gated by IsCascadesSafeScalarFunction
// (string, math, date-part, bit, and null/comparison helpers); the
// runtime semantics live in evalScalarFunction, the single dispatch seam
// a future production registry can replace without touching the Value
// contract.
type ScalarFunctionValue struct {
	FuncName string
	Args     []Value
	Typ      Type
}

// IsCascadesSafeScalarFunction reports whether the named scalar function
// is supported by the Cascades planner. Single authoritative list — all
// callers (translator, predicate upgrade, unsupported-function detection)
// must use this.
func IsCascadesSafeScalarFunction(name string) bool {
	switch name {
	// String functions.
	case "UPPER", "LOWER",
		"LENGTH", "LEN", "CHAR_LENGTH", "CHARACTER_LENGTH", "OCTET_LENGTH",
		"SUBSTRING", "SUBSTR",
		"TRIM", "LTRIM", "RTRIM",
		"CONCAT", "CONCAT_WS",
		"REPLACE",
		"LEFT", "RIGHT",
		"POSITION", "REVERSE":
		return true
	// Math functions.
	case "ABS", "MOD",
		"FLOOR", "CEIL", "CEILING", "ROUND",
		"SQRT", "POWER", "POW",
		"SIGN", "PI",
		"EXP", "LN", "LOG":
		return true
	// Null/comparison helpers, bit ops, and date-part extraction.
	case "COALESCE", "IFNULL",
		"GREATEST", "LEAST",
		"BITAND", "BITOR", "BITXOR",
		"YEAR", "MONTH", "DAY", "DAYOFMONTH",
		"HOUR", "MINUTE", "SECOND",
		"DAYOFWEEK", "DAYOFYEAR",
		"CURRENT_DATE", "CURRENT_TIMESTAMP", "CURRENT_TIME", "LOCALTIME":
		return true
	}
	return false
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
	return evalScalarFunction(s.FuncName, args)
}

// evalScalarFunction dispatches the gated scalar function family
// (IsCascadesSafeScalarFunction). NULL argument propagates to NULL result
// (SQL standard), returned as (nil, nil). Genuine decline edges — unknown
// function, wrong arity, a non-coercible arg type, or an out-of-domain math
// input that SQL degrades to NULL — also return (nil, nil): the value
// becomes SQL NULL rather than erroring. The data-dependent error edges
// return a typed error so the executor maps it to a SQLSTATE:
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

func evalScalarFunction(name string, args []any) (any, error) {
	switch name {
	case "UPPER":
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.ToUpper(s), nil
	case "LOWER":
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.ToLower(s), nil
	case "LENGTH", "LEN", "CHAR_LENGTH", "CHARACTER_LENGTH":
		// Rune count — matches embedded.scalar_functions.go's LENGTH
		// (utf8.RuneCountInString) so plan-time fold and runtime eval
		// agree. Go coerces []byte the same way for symmetry
		// with OCTET_LENGTH (byte count there, rune count here).
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
	case "OCTET_LENGTH":
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
	case "ABS":
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
	case "FLOOR", "CEIL", "CEILING", "ROUND":
		if len(args) < 1 || args[0] == nil {
			return nil, nil
		}
		var f float64
		switch n := args[0].(type) {
		case int64:
			// Already an integer — short-circuit to mirror embedded.
			return n, nil
		case float64:
			f = n
		default:
			return nil, nil
		}
		var result float64
		switch name {
		case "FLOOR":
			result = math.Floor(f)
		case "CEIL", "CEILING":
			result = math.Ceil(f)
		case "ROUND":
			decimals := int64(0)
			if len(args) >= 2 {
				if args[1] == nil {
					return nil, nil
				}
				d, ok := scalarFnInt64Arg(args[1])
				if !ok {
					return nil, nil
				}
				decimals = d
			}
			if decimals == 0 {
				result = math.Round(f)
			} else {
				factor := math.Pow(10, float64(decimals))
				result = math.Round(f*factor) / factor
			}
		}
		// Match embedded's "return int64 if no fractional part" rule.
		if result == math.Trunc(result) && float64FitsInt64(result) {
			return int64(result), nil
		}
		return result, nil
	case "PI":
		// Zero-arg constant. Mirrors embedded.scalar_functions.go's PI.
		if len(args) != 0 {
			return nil, nil
		}
		return math.Pi, nil
	case "SQRT":
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		f, _, ok := ToFloat64(args[0])
		if !ok {
			return nil, nil
		}
		if f < 0 {
			// SQL §6.27: SQRT of a negative argument raises 22023
			// INVALID_PARAMETER_VALUE (Go-only divergence from the old
			// embedded path, which returned NULL — RFC-087 step 3).
			return nil, &InvalidArgumentError{
				Message: fmt.Sprintf("SQRT of negative number: %v", f),
			}
		}
		return math.Sqrt(f), nil
	case "POWER", "POW":
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
	case "COALESCE":
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
	case "NULLIF":
		// NULLIF(a, b) → NULL when a == b; otherwise a. Compare via
		// nullifEqual so int/float promotion mirrors embedded.
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
	case "TRIM":
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.TrimSpace(s), nil
	case "LTRIM":
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.TrimLeft(s, " \t\n\r"), nil
	case "RTRIM":
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			return nil, nil
		}
		return strings.TrimRight(s, " \t\n\r"), nil
	case "CONCAT":
		// Postgres CONCAT semantics — NULL skips, doesn't poison (unlike
		// MySQL CONCAT, which returns NULL if any arg is NULL).
		// Pinned by trim_concat.yaml; the embedded path uses the
		// same rule.
		var b strings.Builder
		for _, a := range args {
			if a == nil {
				continue
			}
			b.WriteString(scalarArgString(a))
		}
		return b.String(), nil
	case "CONCAT_WS":
		// CONCAT With Separator — MySQL semantics: first arg is the
		// separator (NULL → result is NULL); remaining args are
		// concatenated with the separator between non-NULL values.
		// NULL elements are skipped (different from CONCAT in
		// Postgres, which poisons; matches embedded.scalar_functions.go).
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
	case "SUBSTRING", "SUBSTR":
		// SUBSTRING(s, pos[, len]) — 1-based position per SQL standard.
		// pos < 1 normalises to 1 (matches embedded, MySQL).
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
	case "REPLACE":
		// REPLACE(s, from, to). NULL `to` is treated as empty (matches
		// embedded). Pure-string semantics — non-string args coerce
		// via fmt.Sprintf("%v", v) for parity with the embedded path.
		if len(args) != 3 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		toStr := ""
		if args[2] != nil {
			toStr = scalarArgString(args[2])
		}
		return strings.ReplaceAll(scalarArgString(args[0]), scalarArgString(args[1]), toStr), nil
	case "SIGN":
		// SIGN(numeric) — -1 / 0 / 1 in the input's numeric type. Mirrors
		// embedded.scalar_functions.go's SIGN: int64 input → int64 sign,
		// float64 input → float64 sign. Non-numeric input declines so
		// the runtime evaluator surfaces 22018.
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
	case "MOD":
		// MOD(a, b) — int64%int64 stays int64, mixed promotes to float64
		// via math.Mod. Division-by-zero errors with 22012
		// DIVISION_BY_ZERO. Mirrors embedded's MOD semantics.
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
	case "IFNULL":
		// IFNULL(a, b) — `a` if non-null, else `b`. 2-arg COALESCE alias
		// (MySQL/SQLite spelling). Type-uniform like embedded.
		if len(args) != 2 {
			return nil, nil
		}
		if args[0] != nil {
			return args[0], nil
		}
		return args[1], nil
	case "IF", "IIF":
		// IF(cond, then, else) — evaluates condition first; returns
		// `then` if truthy, `else` otherwise. Truthy: non-zero numeric,
		// non-empty string, true bool. Mirrors embedded's IF.
		if len(args) != 3 {
			return nil, nil
		}
		switch v := args[0].(type) {
		case bool:
			if v {
				return args[1], nil
			}
			return args[2], nil
		case int64:
			if v != 0 {
				return args[1], nil
			}
			return args[2], nil
		case float64:
			if v != 0 {
				return args[1], nil
			}
			return args[2], nil
		case string:
			if v != "" {
				return args[1], nil
			}
			return args[2], nil
		case nil:
			// SQL §6.30: IF(NULL, …) returns the else branch (NULL is
			// not truthy). embedded matches this.
			return args[2], nil
		}
		// Unsupported condition type — decline so runtime can error.
		return nil, nil
	case "GREATEST", "LEAST":
		// GREATEST/LEAST — Java conformance: any NULL arg → NULL result
		// (Postgres skips, Oracle propagates; Java propagates). Mirror
		// Java per embedded's behaviour. Cross-type comparisons error
		// with 22000 CANNOT_CONVERT_TYPE.
		if len(args) == 0 {
			return nil, nil
		}
		isGreatest := name == "GREATEST"
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
	case "EXP":
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		f, _, ok := ToFloat64(args[0])
		if !ok {
			return nil, nil
		}
		result := math.Exp(f)
		// Overflow (e.g. EXP(1000) → +Inf) and NaN degrade to SQL NULL,
		// matching the POWER/SQRT out-of-domain convention above and the
		// pre-RFC embedded EXP semantics this ports verbatim.
		if math.IsInf(result, 0) || math.IsNaN(result) {
			return nil, nil
		}
		return result, nil
	case "LN":
		// Natural log. Domain: x > 0. Out-of-domain (≤ 0) declines to
		// SQL NULL (matches the old embedded path; SQRT<0 is the only
		// math-domain edge RFC-087 promotes to a runtime error).
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		f, _, ok := ToFloat64(args[0])
		if !ok || f <= 0 {
			return nil, nil
		}
		return math.Log(f), nil
	case "LOG":
		// 1-arg LOG(x) = log10(x). 2-arg LOG(base, x) = ln(x)/ln(base).
		// Mirrors embedded; out-of-domain declines to SQL NULL.
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
	case "REVERSE":
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
	case "POSITION":
		// POSITION(substr, str) — 1-based rune index of first match,
		// 0 if not found. Mirrors embedded POSITION (note: not the
		// `POSITION(substr IN str)` SQL-standard grammar shape).
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
	case "LEFT":
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
	case "RIGHT":
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
	case "BITAND":
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		a, aok := args[0].(int64)
		b, bok := args[1].(int64)
		if !aok || !bok {
			return nil, nil
		}
		return a & b, nil
	case "BITOR":
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		a, aok := args[0].(int64)
		b, bok := args[1].(int64)
		if !aok || !bok {
			return nil, nil
		}
		return a | b, nil
	case "BITXOR":
		if len(args) != 2 || args[0] == nil || args[1] == nil {
			return nil, nil
		}
		a, aok := args[0].(int64)
		b, bok := args[1].(int64)
		if !aok || !bok {
			return nil, nil
		}
		return a ^ b, nil
	case "YEAR", "MONTH", "DAY", "DAYOFMONTH",
		"HOUR", "MINUTE", "SECOND",
		"DAYOFWEEK", "DAYOFYEAR":
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		s, ok := args[0].(string)
		if !ok {
			// Also handle time.Time if the argument was already parsed.
			if t, tok := args[0].(time.Time); tok {
				return datePartFromTime(name, t), nil
			}
			return nil, nil
		}
		var t time.Time
		var err error
		for _, layout := range []string{
			timestampLayout,
			dateLayout,
			"15:04:05",
		} {
			t, err = time.Parse(layout, s)
			if err == nil {
				break
			}
		}
		if err != nil {
			return nil, nil
		}
		return datePartFromTime(name, t), nil
	case "CURRENT_TIMESTAMP", "CURRENT_TIME", "LOCALTIME":
		return time.Now().UTC().Format(timestampLayout), nil
	case "CURRENT_DATE":
		return time.Now().UTC().Format(dateLayout), nil
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

// scalarFnInt64Arg coerces a numeric scalar-fn argument to int64.
// Float coercion only succeeds for whole-valued floats — non-integer
// floats decline so the fold path returns nil and the runtime
// evaluator (which can surface 22018 INVALID_CHARACTER_VALUE) handles
// the conversion error. Mirrors the strictness of
// embedded.functions.ToIntegerArg.
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

// nullifEqual is the equality test used by NULLIF's plan-time fold.
// Mirrors embedded.functions.CompareValues for the int/float promotion
// case while staying conservative (declines on mixed-type comparisons
// the Type hierarchy can't model).
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
// FLOAT, else LONG (the conservative integer default, also used when an
// operand type is unknown). Mirrors Java's ArithmeticValue result typing and
// the float promotion Evaluate already performs. NULL propagates through
// Evaluate, so the result is nullable.
func (a *ArithmeticValue) Type() Type {
	lc, rc := arithOperandCode(a.Left), arithOperandCode(a.Right)
	if lc == TypeCodeDouble || rc == TypeCodeDouble {
		return NullableDouble
	}
	if lc == TypeCodeFloat || rc == TypeCodeFloat {
		return NullableFloat
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
		if out, err, handled := a.evalInt32(l, r); handled {
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
	lf64, _, lok := ToFloat64(l)
	rf64, _, rok := ToFloat64(r)
	if !lok || !rok {
		return nil
	}
	lf, rf := float32(lf64), float32(rf64)
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

// evalInt32 is the INT lane (Java ADD_II/SUB_II/MUL_II via
// Math.*Exact(int,int)): both operands statically INT, arithmetic bounds
// checked at the int32 boundary — `int_col + int_col` crossing 2^31 errors
// with the overflow class where the LONG lane would silently return the
// wide value. DIV_II/MOD_II mirror the long lane's zero/MinInt semantics at
// 32 bits (Java (int) division wraps MinInt/-1). handled=false when an
// operand isn't an admitted integer (caller falls through).
func (a *ArithmeticValue) evalInt32(l, r any) (any, error, bool) {
	li, lok := toInt64ForArith(l)
	ri, rok := toInt64ForArith(r)
	if !lok || !rok {
		return nil, nil, false
	}
	// Inputs outside the int32 range mean the STATIC type lied about the
	// runtime value (an INT column cannot hold them; Java's (int) cast
	// would silently truncate, which is unreachable there because typing
	// guarantees the range). Fall through to the LONG lane rather than
	// emulate a truncation no valid execution produces.
	if li > math.MaxInt32 || li < math.MinInt32 || ri > math.MaxInt32 || ri < math.MinInt32 {
		return nil, nil, false
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
			return nil, &ArithmeticOverflowError{}, true
		}
		return out, nil, true
	case OpDiv:
		if ri == 0 {
			return nil, &ArithmeticDivisionByZeroError{}, true
		}
		if li == math.MinInt32 && ri == -1 {
			// Java DIV_II is `(int)l / (int)r` — wraps to MinInt.
			return int64(math.MinInt32), nil, true
		}
		return li / ri, nil, true
	case OpMod:
		if ri == 0 {
			return nil, &ArithmeticDivisionByZeroError{}, true
		}
		return li % ri, nil, true
	}
	return nil, nil, false
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
	if v == nil {
		return nil, nil
	}
	if c.Target == nil {
		return nil, nil
	}
	switch c.Target.Code() {
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
			n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 32)
			if err != nil {
				return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast string '%s' to INT: %s", val, err)}
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
			n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			if err != nil {
				return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast string '%s' to LONG: %s", val, err)}
			}
			return n, nil
		}
	case TypeCodeBoolean:
		switch val := v.(type) {
		case bool:
			return val, nil
		case int64:
			return val != 0, nil
		case float64:
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
			s := strconv.FormatFloat(f, 'g', -1, 64)
			if !strings.ContainsAny(s, ".eE") && s != "NaN" && s != "+Inf" && s != "-Inf" {
				s += ".0"
			}
			return s, nil
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
			for _, layout := range []string{timestampLayout, "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05", dateLayout} {
				if t, err := time.Parse(layout, s); err == nil {
					return t.UTC().Format(timestampLayout), nil
				}
			}
			return nil, &InvalidCastError{Message: fmt.Sprintf("Cannot cast string '%s' to TIMESTAMP", val)}
		case int64:
			return time.UnixMilli(val).UTC().Format(timestampLayout), nil
		}
	case TypeCodeFloat, TypeCodeDouble:
		// CAST … AS FLOAT — accept float64/float32 verbatim; promote
		// integral types to float64. Without this case, the walker's
		// shiny new CastValue{TypeFloat} path silently returns nil
		// from Evaluate and constant-fold of `CAST(5 AS FLOAT) = 3.14`
		// gets UNKNOWN instead of FALSE.
		switch val := v.(type) {
		case float64:
			return val, nil
		case float32:
			return float64(val), nil
		case int64:
			return float64(val), nil
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
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

// NewRawRecordConstructorValue constructs a RecordConstructorValue keeping
// every field name VERBATIM — duplicate names allowed. It exists for
// ordinal-join seeds:
// a join's ordinal RC concatenates the legs' columns, each field a
// BAKED FieldValue over its leg's QOV, and duplicate names across legs
// (`SELECT * FROM a JOIN b` with same-named columns) MUST survive verbatim —
// positional access is by ordinal, so duplicates are unambiguous, and the
// duplicate-name identity pins are unconstructible without them.
//
// NEVER use this for a projection RC: NewRecordConstructorValue (above)
// appends _2/_3 suffixes, which is correct there (SQL projection column
// naming) — a raw duplicate under name-keyed plan-time lookup (FieldIndex
// first-match) silently
// resolves to the first match, the exact conflation ordinal identity exists to avoid.
func NewRawRecordConstructorValue(fields ...RecordConstructorField) *RecordConstructorValue {
	out := make([]RecordConstructorField, len(fields))
	copy(out, fields)
	return &RecordConstructorValue{Fields: out}
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
		fields[i] = Field{
			Name:      f.Name,
			FieldType: ft,
			Ordinal:   i,
		}
	}
	return &RecordType{Nullable: true, Fields: fields}
}

// Name returns the debug-print kind.
func (*RecordConstructorValue) Name() string { return "record" }

// Evaluate produces a map[string]any with each field evaluated.
// Downstream consumers (projections, field-access) index into this
// map by field name.
func (r *RecordConstructorValue) Evaluate(evalCtx any) (any, error) {
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
// Evaluate currently delegates to Child.Evaluate —
// cmpAny already promotes numerics at runtime, so an explicit
// Promote in the tree is a no-op evaluation-wise. The value is in
// having the coercion visible at plan time so rule matchers can
// simplify `Promote(x, x.Type)` → `x`.
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

// Evaluate delegates to the child for the numeric/cross-width case —
// Go treats Promote as a no-op there since cmpAny already
// handles cross-width promotion, and plan-time inspection (explain,
// rewrite rules) is where those Promotes earn their keep.
//
// The ONE runtime-active arm is STRING → UUID (Java's
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
		return childResult, nil
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

// QuantifiedObjectValue represents "the current row of the
// quantifier identified by Correlation". Emitted by the analyzer
// for references like `t` in `SELECT t.col FROM tbl AS t` — the
// parent expression (`t.col`) then projects a FieldValue with
// operand = QuantifiedObjectValue{Correlation: t}.
//
// Mirrors Java's `QuantifiedObjectValue`. Evaluate resolves this quantifier's
// row from the eval context: a correlation binding for Correlation (an outer
// quantifier), else the frontier Positional row (the sole runtime row), else
// nil. Downstream FieldValue lookups then read a column off that row by ordinal.
type QuantifiedObjectValue struct {
	Correlation CorrelationIdentifier
	// Typ is the row type (struct shape) this quantifier produces —
	// a *RecordType on the typed frontier (the translator
	// stamps it from the inner expression's result type), or
	// UnknownType where inference hasn't reached.
	Typ Type
}

// NewQuantifiedObjectValue constructs a QuantifiedObjectValue. Zero
// correlation is rejected — a quantifier without an identifier is a
// design error, not something the analyzer should allow.
func NewQuantifiedObjectValue(corr CorrelationIdentifier) *QuantifiedObjectValue {
	if corr.IsZero() {
		panic("NewQuantifiedObjectValue: correlation is zero-value; use NamedCorrelationIdentifier or UniqueCorrelationIdentifier")
	}
	return &QuantifiedObjectValue{Correlation: corr, Typ: UnknownType}
}

// NewQuantifiedObjectValueOfType constructs a QuantifiedObjectValue whose
// flowed value carries a known type. Used where the quantifier flows a SCALAR
// of a known type — e.g. a lateral array unnest's element quantifier, whose
// flowed value is one array element (the array's elementType), not an
// UnknownType row. Carrying the real type lets result-set column metadata
// report it (a STRING array's element is STRING, not the UnknownType→BIGINT
// fallback). A nil typ degrades to UnknownType, matching NewQuantifiedObjectValue.
func NewQuantifiedObjectValueOfType(corr CorrelationIdentifier, typ Type) *QuantifiedObjectValue {
	if corr.IsZero() {
		panic("NewQuantifiedObjectValueOfType: correlation is zero-value; use NamedCorrelationIdentifier or UniqueCorrelationIdentifier")
	}
	if typ == nil {
		typ = UnknownType
	}
	return &QuantifiedObjectValue{Correlation: corr, Typ: typ}
}

// Children returns an empty slice — the quantifier is a leaf in
// the Value tree, with its correlation link being external metadata
// (not a child Value).
func (*QuantifiedObjectValue) Children() []Value { return []Value{} }

// Type returns the row reference Type. Always nullable — rows pass
// through as nullable (e.g. LEFT JOIN's right side).
func (q *QuantifiedObjectValue) Type() Type {
	if q.Typ == nil {
		return UnknownType
	}
	return WithNullability(q.Typ, true)
}

// Name returns the debug-print kind.
func (*QuantifiedObjectValue) Name() string { return "quantifier" }

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
func (q *QuantifiedObjectValue) Evaluate(evalCtx any) (any, error) {
	if evalCtx == nil {
		return nil, nil
	}
	switch ctx := evalCtx.(type) {
	case *RowEvalContext:
		if ctx.Correlations != nil {
			if val, ok := ctx.Correlations.GetCorrelationBinding(q.Correlation); ok {
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
	case CorrelationBinder:
		val, ok := ctx.GetCorrelationBinding(q.Correlation)
		if !ok {
			return nil, nil
		}
		return val, nil
	}
	return nil, nil
}

// GetCorrelatedTo implements the Correlated interface — returns
// a set containing this quantifier's correlation.
func (q *QuantifiedObjectValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{q.Correlation: {}}
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
//   - COUNT / COUNT(*): NotNullLong (zero on empty groups).
//   - AVG: NullableDouble — AVG is real division, always DOUBLE
//     regardless of operand type (Java AVG_{I,L,F,D} → DOUBLE). NOT
//     operand-derived: AVG(BIGINT) is DOUBLE, not LONG.
//   - SUM / MIN / MAX: nullable; Type derived from the operand when
//     available, else NullableLong (Java SUM_L→LONG, MIN/MAX→operand).
func (a *AggregateValue) Type() Type {
	switch a.Op {
	case AggCount, AggCountStar:
		return NotNullLong
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
