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
//   - Phase 4.0 Type hierarchy (`type.go`) — the rich `Type`
//     interface + `TypeCode` enum + concrete impls (`PrimitiveType`,
//     `RecordType`, `ArrayType`, `EnumType`, `RelationType`),
//     canonical singletons for every primitive (incl. UUID, VERSION,
//     None, Any), `TypeRepository`, `WithNullability`, the
//     `IsPromotable` / `MaximumType` / `MaximumTypeOfMany`
//     promotion lattice (with structural recursion through ARRAY /
//     RECORD / ENUM / RELATION), and shape predicates (`IsNull`,
//     `IsArray`, …). Post-swingshift-52, every Value impl's `Type()`
//     returns the rich `Type` directly — the legacy `ValueType`
//     enum + `FromValueType` / `ToValueType` bridges retired.
//     Track G1 in TODO.md. Once `type.go` exceeds ~1500 LOC it
//     splits into a dedicated `cascades/typing/` sub-package per
//     RFC-025.
//
// Imports: nothing else from `pkg/recordlayer/query/plan/cascades/...`.
// `predicates/`, `matching/`, and root `cascades` all import this
// package; the dependency arrow points inward to keep cycles out.
package values

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Canonical ISO 8601 layouts for temporal value formatting/parsing.
// Mirrors functions.TimestampLayout / functions.DateLayout — duplicated
// here because values/ must not import functions/ (layering: values is
// the leaf package that predicates/ and cascades/ depend on).
const (
	timestampLayout = "2006-01-02 15:04:05"
	dateLayout      = "2006-01-02"
)

// Legacy `ValueType` enum (TypeUnknown / TypeInt / TypeString /
// TypeBool / TypeFloat) retired in swingshift-52 — every Value impl's
// Type() now returns the rich Type directly. The names below remain
// as Type-typed vars so existing call sites (`Typ: values.TypeInt`)
// keep working — the value's Go type changes (Type instead of int),
// the constant name doesn't.
//
// Track G1 / RFC-025: legacy bridge retirement.
var (
	// TypeUnknown is the placeholder for "type not yet inferred".
	// Maps to the canonical UnknownType singleton.
	TypeUnknown Type = UnknownType
	// TypeInt is the legacy name for the seed's default integer
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
	// TypeFloat is the legacy name for the seed's default float
	// width — bridged to DOUBLE (matches Java Record Layer's
	// float64 representation).
	TypeFloat Type = NullableDouble
)

// Value is the root of the Phase 4.0 seed Value hierarchy.
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
	// (post-swingshift-52: the legacy ValueType enum retired and
	// Type() now returns the rich Type directly). Never nil —
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
	// subsystems can pass their own row shape — seed uses
	// `map[string]any` in tests.
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
}

func (f *FieldValue) Children() []Value {
	if f.Child == nil {
		return []Value{}
	}
	return []Value{f.Child}
}

func (f *FieldValue) Name() string { return "field" }

// Type returns the field's rich Type. The seed's FieldValue stores
// the column type as-is; callers that know NOT NULL information
// from the catalog set Typ to the non-nullable form.
func (f *FieldValue) Type() Type {
	if f.Typ == nil {
		return UnknownType
	}
	return f.Typ
}

func (f *FieldValue) Evaluate(evalCtx any) (any, error) {
	if f.Child != nil {
		if qov, isQOV := f.Child.(*QuantifiedObjectValue); isQOV {
			return f.evaluateCorrelated(qov, evalCtx), nil
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
	if row, ok := evalCtx.(map[string]any); ok {
		return row[f.Field], nil
	}
	if rc, ok := evalCtx.(*RowEvalContext); ok && rc.Datum != nil {
		v, present := rc.Datum[f.Field]
		if !present && rc.Strict && ReportUnresolvedReference != nil {
			ReportUnresolvedReference(f.Field, mapKeys(rc.Datum))
		}
		return v, nil
	}
	return nil, nil
}

// mapKeys returns the keys of a datum map, for unresolved-reference
// diagnostics. Only called on the W1 violation path (never in prod).
func mapKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func (f *FieldValue) evaluateCorrelated(qov *QuantifiedObjectValue, evalCtx any) any {
	qualKey := strings.ToUpper(qov.Correlation.String()) + "." + strings.ToUpper(f.Field)
	switch ctx := evalCtx.(type) {
	case *RowEvalContext:
		if ctx.Correlations != nil {
			if bound, ok := ctx.Correlations.GetCorrelationBinding(qov.Correlation); ok {
				if bm, ok := bound.(map[string]any); ok {
					if v, ok := bm[f.Field]; ok {
						return v
					}
					lower := strings.ToLower(f.Field)
					if v, ok := bm[lower]; ok {
						return v
					}
					return nil
				}
				return bound
			}
		}
		if ctx.Datum != nil {
			if v, ok := ctx.Datum[qualKey]; ok {
				return v
			}
			// Already-qualified field (e.g. "T3.ID") accessed through a merge
			// quantifier: a re-enumerated N-way join collapses a buried table
			// into a merge quantifier whose row flows that table's columns under
			// their own qualified ALIAS.COL keys (the executor's mergeRows
			// preserves dotted keys verbatim — they are NOT re-prefixed with the
			// merge alias). Prepending the merge alias above would invent a key
			// (e.g. "$M.T3.ID") that was never written. Mirror the binding path
			// (bm[f.Field]) by resolving the qualified field directly. (RFC-043.)
			if strings.Contains(f.Field, ".") {
				if v, ok := ctx.Datum[strings.ToUpper(f.Field)]; ok {
					return v
				}
				if v, ok := ctx.Datum[f.Field]; ok {
					return v
				}
			}
		}
		return nil
	case CorrelationBinder:
		if bound, ok := ctx.GetCorrelationBinding(qov.Correlation); ok {
			if bm, ok := bound.(map[string]any); ok {
				if v, ok := bm[f.Field]; ok {
					return v
				}
				lower := strings.ToLower(f.Field)
				if v, ok := bm[lower]; ok {
					return v
				}
				return nil
			}
			return bound
		}
		return nil
	case map[CorrelationIdentifier]map[string]any:
		if sub, ok := ctx[qov.Correlation]; ok {
			return sub[f.Field]
		}
		return nil
	case map[string]any:
		if v, ok := ctx[qualKey]; ok {
			return v
		}
		// Already-qualified field accessed through a merge quantifier — see the
		// *RowEvalContext branch above for the rationale. (RFC-043.)
		if strings.Contains(f.Field, ".") {
			if v, ok := ctx[strings.ToUpper(f.Field)]; ok {
				return v
			}
			if v, ok := ctx[f.Field]; ok {
				return v
			}
		}
		return nil
	}
	return nil
}

// resolveOrdinal returns the 0-based ordinal of f.Field within the record type
// f.Child flows, mirroring Java's FieldValue.resolveFieldPath (name -> ordinal
// against the input Type, FieldValue.java:273). Returns (ordinal, true) when
// f.Child flows a RecordType containing f.Field; (0, false) for a nil-Child
// leaf, a non-record child, or an absent/anonymous field.
//
// RFC-173 P1: the ordinal substrate for the name -> ordinal column-resolution
// migration (retiring the name-based AnchoredJoin model). It is DARK — computed
// but NOT authoritative; the name-lookup path in Evaluate stays the source of
// truth until P2 provides a positional runtime row and P1's dual-mode assert has
// proven the ordinal path agrees. Side-effect-free, so computing it can never
// perturb planning. The nil-Child leaf (legacy flat field, no child type to
// resolve against) is the case that stays on the name path — see RFC-173 §4 P1.
func (f *FieldValue) resolveOrdinal() (int, bool) {
	if f.Child == nil {
		return 0, false
	}
	rt, ok := f.Child.Type().(*RecordType)
	if !ok {
		return 0, false
	}
	// Return the field's SLICE POSITION (FieldIndex), not a stored Field.Ordinal —
	// position IS the Java ordinal (Type.Record.computeFieldNameToOrdinal is list
	// position), and it is sound even for a raw RecordType that bypassed
	// NewRecordType's normalization (RFC-173 P1 review: Torvalds/Graefe converged).
	return rt.FieldIndex(f.Field)
}

// NewFieldValue constructs a FieldValue with a child (base) value.
// Mirrors Java's FieldValue(childValue, FieldPath).
func NewFieldValue(child Value, field string, typ Type) *FieldValue {
	return &FieldValue{Field: field, Typ: typ, Child: child}
}

// NewFlatFieldValue constructs a FieldValue without a child (legacy
// flat model).
func NewFlatFieldValue(field string, typ Type) *FieldValue {
	return &FieldValue{Field: field, Typ: typ}
}

// NewOrdinalFieldValue accesses a record field by ORDINAL position,
// mirroring Java's `FieldValue.ofOrdinalNumber(child, ordinal)`. Go's
// runtime Datum is a name-keyed map, and anonymous record fields (the
// element/ordinal of a WITH ORDINALITY Explode) are keyed by their
// ordinal name `_0`/`_1` (see OrdinalFieldName) — so ordinal access is
// name access on the `_<ordinal>` key. Used by the lateral-unnest lowering
// to bind the AS alias to field 0 (element) and the AT alias to field 1
// (the INT NOT NULL ordinal).
func NewOrdinalFieldValue(child Value, ordinal int, typ Type) *FieldValue {
	return &FieldValue{Field: OrdinalFieldName(ordinal), Typ: typ, Child: child}
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
func ExplainValue(v Value) string {
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
		if cv.Child != nil {
			return ExplainValue(cv.Child) + "." + cv.Field
		}
		return cv.Field
	case *ArithmeticValue:
		return "(" + ExplainValue(cv.Left) + " " + cv.Op.symbol() + " " + ExplainValue(cv.Right) + ")"
	case *StrictRankLimitValue:
		// Renders as the strict adjustment it computes (max(0, K-1)); matches the
		// prior ArithmeticValue "(K - 1)" form so plan output is unchanged.
		return "(" + ExplainValue(cv.K) + " - 1)"
	case *BooleanValue:
		if cv.Value == nil {
			return "NULL"
		}
		if *cv.Value {
			return "TRUE"
		}
		return "FALSE"
	case *CastValue:
		return "CAST(" + ExplainValue(cv.Child) + " AS " + explainTypeName(cv.Target) + ")"
	case *PromoteValue:
		return "PROMOTE(" + ExplainValue(cv.Child) + " TO " + explainTypeName(cv.Target) + ")"
	case *RecordConstructorValue:
		parts := make([]string, 0, len(cv.Fields))
		for _, f := range cv.Fields {
			parts = append(parts, f.Name+": "+ExplainValue(f.Value))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *NullValue:
		return "NULL"
	case *AggregateValue:
		if cv.Op == AggCountStar {
			return "COUNT(*)"
		}
		return cv.Op.Symbol() + "(" + ExplainValue(cv.Operand) + ")"
	case *QuantifiedObjectValue:
		return cv.Correlation.Name()
	case *ScalarFunctionValue:
		parts := make([]string, len(cv.Args))
		for i, a := range cv.Args {
			parts[i] = ExplainValue(a)
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
			parts[i] = ExplainValue(a)
		}
		sel := ExplainValue(cv.Selector)
		return "CASE(" + sel + ", [" + strings.Join(parts, ", ") + "])"
	case *ConditionSelectorValue:
		conds := make([]string, len(cv.Implications))
		for i, c := range cv.Implications {
			conds[i] = ExplainValue(c)
		}
		return "WHEN(" + strings.Join(conds, ", ") + ")"
	case *CardinalityValue:
		// Java: ExplainTokens.addFunctionCall(FunctionNames.CARDINALITY, ...).
		// Renders `cardinality(<child>)`, e.g. `cardinality(_.int_arr)`.
		return "cardinality(" + ExplainValue(cv.Child) + ")"
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
// `UNKNOWN`) — the seed conflates LONG/INT into INT and DOUBLE/FLOAT
// into FLOAT here so the rendered output stays stable across the
// ValueType retirement (Track G1, swingshift-52). Plan-cache keys
// derived via ExplainValue stay byte-stable across the migration.
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
// Seed runtime evaluation is intentionally minimal: a richer
// EvalContext that threads parameter bindings through every
// Value.Evaluate is the next step. Until then ParameterValue
// degrades to NULL at exec time, which is harmless for the
// plan-time / explain-time work this type unblocks.
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

// RowEvalContext is a composite evaluation context for Value.Evaluate
// that satisfies FieldValue (datum map), ParameterValue
// (ParameterBinder), and CorrelationBinder. Pass this when evaluating
// expressions that mix field references, prepared-statement parameters,
// and correlation bindings (e.g. InJoin explode aliases).
type RowEvalContext struct {
	Datum            map[string]any
	Binder           ParameterBinder
	Correlations     CorrelationBinder
	ScalarSubqueries map[CorrelationIdentifier]any // pre-evaluated scalar subquery results
	// Strict turns a local field-reference miss (a top-level FieldValue whose
	// name is absent from Datum) from a silent nil into a reported violation
	// via ReportUnresolvedReference. It is set ONLY when evaluating against a
	// row whose key set is complete — i.e. a computed/synthetic row (aggregate
	// output, projection, join-merge) that has no proto-style optional-field
	// omissions, where an absent name is unambiguously an unresolved reference
	// rather than a legitimate SQL NULL. Base-record rows (which legitimately
	// omit unset optional fields) leave this false. See RFC-048 W1.
	Strict bool
}

// ReportUnresolvedReference, when non-nil, is invoked by FieldValue.Evaluate
// whenever a Strict RowEvalContext is asked for a local field name that is not
// present in its (complete) row. It is the RFC-048 W1 "no unresolved
// reference" invariant: a silent name->NULL — the cardinal silent-wrong — is
// turned into a loud, attributable signal. It is nil by default (zero
// production overhead and behaviour beyond the map lookup that already
// happens); test/debug builds install a hook that fails the test. `field` is
// the missing name; `available` is the row's actual key set (for diagnostics).
var ReportUnresolvedReference func(field string, available []string)

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
		// agree. The seed coerces []byte the same way for symmetry
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
// seed's numeric/string/bool comparison rules. Returns ok=false on
// cross-type pairs the seed can't compare (the runtime reports the
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
// 2^63 itself, which overflows int64 (codex finding, RFC-087). The lower bound
// math.MinInt64 (-2^63) IS exactly representable as float64, so it is inclusive.
func float64FitsInt64(f float64) bool {
	return f >= math.MinInt64 && f < twoPow63
}

// nullifEqual is the equality test used by NULLIF's plan-time fold.
// Mirrors embedded.functions.CompareValues for the int/float promotion
// case while staying conservative (declines on mixed-type comparisons
// the seed Type hierarchy can't model).
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
// Evaluate recurses left + right, coerces to int64, and applies
// the op. NULL on either side propagates (SQL semantics). Division
// by zero returns nil (UNKNOWN).
type ArithmeticValue struct {
	Op    ArithmeticOp
	Left  Value
	Right Value
	// Result type: int for the seed impls; full type inference lands
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
			return nil, &ArithmeticOverflowError{}
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

func toInt64ForArith(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
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
// Seed handles the trivial conversions our existing corpus needs:
// int ↔ string (via strconv-free formatting for the seed), bool ↔
// int (false=0, true=1). Unknown conversions return nil (UNKNOWN).
// Full type tower lands with the Type hierarchy.
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
// results are always nullable in the seed.
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

	// AnchoredJoin marks a source-anchored join RESULT value (RFC-077 7.6):
	// the RecordConstructorValue NewAnchoredJoinRecord builds, whose fields are
	// each FieldValue(QuantifiedObjectValue(leg), col) over the enclosing
	// select's OWN immediate join quantifiers. It is the structural successor of
	// the retired opaque merge's Seed provenance bit, carrying the SAME
	// dual-purpose semantics (RFC-077 F2):
	//   - exploration-time HIDING: GetCorrelatedToOfValue does NOT descend into an
	//     anchored-join RC, so its self-bound leg QOVs are excluded from the
	//     value's reported external correlation set (mirroring the retired seed bit's
	//     "report nothing"). Reporting them inflates every enclosing select's
	//     correlation order and tips the ≥4-way STAR past the task budget.
	//   - partition-time RE-EXPOSURE: PartitionSelectRule keeps ALL lower aliases
	//     live for an anchored-join result (the seed never names the real
	//     projection), and AddMergeSeedAliases re-collects the buried leg QOVs by
	//     walking INTO the RC's fields — so a predicate reading a buried column is
	//     classified as spanning, not pushed below the merge (the 0-row bug).
	//
	// An ORDINARY RecordConstructorValue (a SELECT projection) leaves this false:
	// its correlations are real and must be reported. The flag is the honest
	// structural marker that "this RC is a join result, not a user projection",
	// NOT a downstream-observable heuristic — and it is PRESERVED through every
	// Value reconstruction (WithChildren, Replace, RebaseValue, and the value
	// simplifier's RecordConstructor/liftConstructor rebuilds) so the hiding
	// survives SelectMergeRule's flatten-time substitution of nested join legs.
	AnchoredJoin bool
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
// Seed Evaluate currently delegates to Child.Evaluate — the seed's
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
// the seed treats Promote as a no-op there since cmpAny already
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
// Mirrors Java's `QuantifiedObjectValue`. The seed Evaluate reads
// the row directly out of the eval context when it's a
// `map[CorrelationIdentifier]map[string]any` (the multi-source
// shape); for the single-source `map[string]any` shape it returns
// the map verbatim so downstream FieldValue lookups can index into
// it.
type QuantifiedObjectValue struct {
	Correlation CorrelationIdentifier
	// Typ is the row type (struct shape) this quantifier produces.
	// Seed keeps it as UnknownType until proper struct-type
	// inference lands — the test surface doesn't need real struct
	// types yet.
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

// Evaluate extracts the row bound to this quantifier's correlation.
// Eval context shapes this impl handles:
//
//   - map[CorrelationIdentifier]map[string]any — multi-source shape,
//     returns the nested map for this correlation (nil if missing).
//   - map[string]any — single-source compat shim: IGNORES q.Correlation
//     and returns the whole map. Safe only when there's one
//     QuantifiedObjectValue in play; multi-source callers MUST use
//     the per-correlation shape or two quantifiers with different
//     correlations silently evaluate to the same row.
//   - anything else — nil.
//
// The single-source shim exists so existing single-table tests /
// callers that feed a bare row map keep working while the eval
// path migrates. New callers MUST NOT rely on it — thread the
// per-correlation shape end-to-end. The shim is scheduled for
// removal once no caller needs it.
//
// Downstream FieldValue / nested-field resolvers then index into the
// returned map to pick a specific column.
func (q *QuantifiedObjectValue) Evaluate(evalCtx any) (any, error) {
	if evalCtx == nil {
		return nil, nil
	}
	switch ctx := evalCtx.(type) {
	case map[CorrelationIdentifier]map[string]any:
		return ctx[q.Correlation], nil
	case *RowEvalContext:
		if ctx.Correlations != nil {
			if val, ok := ctx.Correlations.GetCorrelationBinding(q.Correlation); ok {
				return val, nil
			}
		}
		return ctx.Datum, nil
	case map[string]any:
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

// Enum of aggregate operators the seed supports. Ordered to match
// Java's bi-map so serialised plans round-trip.
const (
	AggInvalid   AggregateOp = iota // unassigned — rejects if ever evaluated
	AggCount                        // COUNT(expr)
	AggCountStar                    // COUNT(*)
	AggSum                          // SUM(expr)
	AggMin                          // MIN(expr)
	AggMax                          // MAX(expr)
	AggAvg                          // AVG(expr) — seed: rejects at Evaluate, no streaming impl
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
//	AggMin       → MIN_EVER_LONG   (or MIN_EVER_TUPLE for non-numeric)
//	AggMax       → MAX_EVER_LONG   (or MAX_EVER_TUPLE)
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
		return "MIN_EVER_LONG"
	case AggMax:
		return "MAX_EVER_LONG"
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
