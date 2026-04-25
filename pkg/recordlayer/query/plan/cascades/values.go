// Package cascades is the Go port of Java's
// `com.apple.foundationdb.record.query.plan.cascades` (the Cascades
// query planner). This is the Phase 4.0 seed — committed shape per
// RFC-023 (non-generic BindingMatcher interface + `any` + generic
// `Get[T]` retrieval helper). Follow-up shifts add the full Value
// and Type hierarchies, the BindingMatcher combinators (AllOf, AnyOf,
// TypedWithDownstream, etc.), PlannerBindings with proper identity
// keying, CascadesRule / CascadesRuleCall, the memo, cost model, and
// the planner driver.
//
// Dayshift-46 seed contents:
//
//   - values.go          — Value interface (Children, Type, Name,
//     Evaluate) + ValueType enum + 10 concrete
//     values (Constant, Field, Arithmetic, Boolean,
//     Cast, Null, Aggregate, QuantifiedObject,
//     Promote, RecordConstructor) + ExplainValue
//     SQL-ish renderer.
//   - matcher.go         — BindingMatcher interface +
//     PlannerBindings + MergedWith + AnyValue /
//     Instance / ArithmeticMatcher + the generic
//     Get[T] retrieval helper.
//   - combinators.go     — AllOf / AnyOf matcher combinators —
//     primary building blocks for real rule
//     patterns.
//   - predicates.go      — QueryPredicate interface + TriBool
//     (Kleene 3VL) + Constant / And / Or / Not /
//     ValuePredicate + WalkPredicate + AsConstant +
//     PredicateSize + PredicateEquals (structural,
//     handles IN-list slice operand and
//     per-instance Value data).
//   - comparisons.go     — 13-operator ComparisonType enum plus
//     ComparisonPredicate, cmpAny with numeric
//     promotion, rune-level LIKE matcher, and the
//     IsEquality / Negate classification helpers. Ops:
//     =, <>, <, <=, >, >=, IS [NOT] NULL, STARTS_WITH,
//     IN, IS [NOT] DISTINCT FROM, LIKE.
//   - correlation.go     — CorrelationIdentifier value-type +
//     Named / Unique factories + Correlated
//     interface signature (Quantifier-tracking
//     surface for rewrite rules).
//   - rule.go            — CascadesRule interface + RuleCall
//     (Yield / Yielded) + FireRule testing driver.
//     Example addConstantFoldRule folds
//     `Const + Const` → `Const`.
//   - rule_simplify.go   — Eleven Phase 4.5 Batch A-style rules:
//     AndFlatten / OrFlatten (associative
//     normalisation), AndConstantSimplify /
//     OrConstantSimplify / NotConstantSimplify /
//     ComparisonConstantSimplify (Kleene folds),
//     AndDedup / OrDedup (structural dedup),
//     AndAbsorbOr / OrAbsorbAnd (absorption),
//     NotComparisonRewrite (NOT-past-comparison).
//   - simplifier.go      — Fixed-point Simplify driver + the
//     DefaultSimplifyRules rule set. Pre-4.6 seed;
//     real planner task-stack replaces this later.
//   - benchmark_test.go  — 7 micro-benchmarks covering Value
//     evaluation, predicate evaluation, and
//     matcher dispatch.
//
// Establishes the shape RFC-023 committed to; real Value
// subtypes (77 in Java), the rest of the matcher combinator
// catalogue (~15 shapes), full QueryPredicate + Comparisons, and
// the CascadesRule / CascadesRuleCall / memo / cost / planner
// driver land in subsequent Phase 4.0 / 4.2-4.6 shifts.
//
// **Zero-size struct gotcha.** Go's spec allows two distinct
// zero-size variables to share an address. `&AnyValue{}` +
// `&AnyValue{}` collapse to the same pointer, which breaks
// PlannerBindings' matcher-identity keying. Every matcher struct
// carries a `uint64` nonce; factory constructors (NewAnyValue,
// NewConstantMatcher, NewFieldMatcher, …) increment an atomic
// counter. Rule authors MUST use the factories, never bare struct
// literals. Java doesn't hit this — `new Object()` always allocates.
//
// Mapping to Java:
//
//	pkg/recordlayer/query/plan/cascades/values.go
//	  ↔ com.apple.foundationdb.record.query.plan.cascades.values.Value
//	     (trimmed — full Java hierarchy has 77 Value subtypes)
//	pkg/recordlayer/query/plan/cascades/matcher.go
//	  ↔ com.apple.foundationdb.record.query.plan.cascades.matching.structure
//	     (trimmed — full Java hierarchy has ~15 matcher shapes)
//
// Future shifts will likely split this into `cascades/values/` +
// `cascades/matching/structure/` subpackages to mirror Java more
// closely, once enough types exist to justify the split.
package cascades

import (
	"strconv"
	"strings"
)

// ValueType is a stand-in for the full Cascades Type hierarchy. The
// production port adds `Type` / `TypeRepository` / `Typed`, at which
// point this enum goes away. See TODO.md Phase 4.0.
type ValueType int

const (
	TypeUnknown ValueType = iota
	TypeInt
	TypeString
	TypeBool
	TypeFloat
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
	// Type is the result type of evaluating this Value.
	Type() ValueType
	// Name is a debug string for error messages + explain output.
	// Not part of the matcher DSL.
	Name() string
	// Evaluate produces the Go-native value this Value represents
	// against an eval context. Leaf ConstantValue ignores the
	// context; FieldValue looks up its column; ArithmeticValue
	// recurses. The context is opaque (`any`) so different
	// subsystems can pass their own row shape — seed uses
	// `map[string]any` in tests.
	Evaluate(evalCtx any) any
}

// --- Concrete values ------------------------------------------------

// ConstantValue is a literal. Evaluate returns Value verbatim.
type ConstantValue struct {
	Value any
	Typ   ValueType
}

func (c *ConstantValue) Children() []Value { return []Value{} }
func (c *ConstantValue) Type() ValueType   { return c.Typ }
func (c *ConstantValue) Name() string      { return "constant" }
func (c *ConstantValue) Evaluate(any) any  { return c.Value }

// FieldValue references a column by name. Evaluate expects a
// `map[string]any` eval context and returns the field's value
// (nil if absent — SQL NULL semantics).
//
// Field-name contract: callers constructing FieldValue via the SQL
// resolver (expr.ResolveIdentifier) receive the case-folded (upper-
// case) form, matching Identifier.Name(). Downstream row producers
// MUST normalise their map keys to the same form — a row with
// lowercase keys against an UPPER-case FieldValue silently returns
// nil for every lookup. This is intentional: SQL identifier
// resolution is case-insensitive by default, so there has to be a
// single canonical casing at the evaluation boundary.
type FieldValue struct {
	Field string
	Typ   ValueType
}

func (f *FieldValue) Children() []Value { return []Value{} }
func (f *FieldValue) Type() ValueType   { return f.Typ }
func (f *FieldValue) Name() string      { return "field" }

func (f *FieldValue) Evaluate(evalCtx any) any {
	if evalCtx == nil {
		return nil
	}
	row, ok := evalCtx.(map[string]any)
	if !ok {
		return nil
	}
	return row[f.Field]
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
	case *FieldValue, *QuantifiedObjectValue, *AggregateValue:
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
// Panics during Evaluate are caught and translated to (nil, false)
// — a constant-looking tree that panics (e.g. an AggregateValue
// buried inside a Cast — IsConstantValue should exclude it, but
// defence-in-depth) is better reported as "not foldable" than
// bubbling up.
func EvaluateConstant(v Value) (out any, ok bool) {
	if v == nil || !IsConstantValue(v) {
		return nil, false
	}
	defer func() {
		if r := recover(); r != nil {
			out = nil
			ok = false
		}
	}()
	return v.Evaluate(nil), true
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
		return cv.Field
	case *ArithmeticValue:
		return "(" + ExplainValue(cv.Left) + " " + cv.Op.symbol() + " " + ExplainValue(cv.Right) + ")"
	case *BooleanValue:
		if cv.Value == nil {
			return "NULL"
		}
		if *cv.Value {
			return "TRUE"
		}
		return "FALSE"
	case *CastValue:
		return "CAST(" + ExplainValue(cv.Child) + " AS " + cv.Target.String() + ")"
	case *PromoteValue:
		return "PROMOTE(" + ExplainValue(cv.Child) + " TO " + cv.Target.String() + ")"
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
	}
	return v.Name()
}

// String renders ValueType as a human-readable name (used by
// ExplainValue for CAST rendering).
func (t ValueType) String() string {
	switch t {
	case TypeInt:
		return "INT"
	case TypeString:
		return "STRING"
	case TypeBool:
		return "BOOL"
	case TypeFloat:
		return "FLOAT"
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
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// NullValue is the SQL NULL literal — evaluates to nil regardless
// of context. Not collapsed into ConstantValue{Value: nil} because
// having a dedicated type lets rule matchers check for NULL
// specifically (without also matching `Value: nil` ConstantValues
// that happen to represent a NULL literal in a non-type-annotated
// way).
type NullValue struct {
	Typ ValueType // type NULL was cast to; TypeUnknown when unconstrained
}

// NewNullValue constructs a NullValue of the given type.
func NewNullValue(typ ValueType) *NullValue {
	return &NullValue{Typ: typ}
}

func (*NullValue) Children() []Value { return []Value{} }
func (n *NullValue) Type() ValueType { return n.Typ }
func (*NullValue) Name() string      { return "null" }
func (*NullValue) Evaluate(any) any  { return nil }

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
func (a *ArithmeticValue) Type() ValueType   { return TypeInt }
func (a *ArithmeticValue) Name() string      { return "arith" }

func (a *ArithmeticValue) Evaluate(evalCtx any) any {
	l := a.Left.Evaluate(evalCtx)
	r := a.Right.Evaluate(evalCtx)
	if l == nil || r == nil {
		return nil
	}
	li, lok := l.(int64)
	ri, rok := r.(int64)
	if !lok || !rok {
		return nil
	}
	switch a.Op {
	case OpAdd:
		return li + ri
	case OpSub:
		return li - ri
	case OpMul:
		return li * ri
	case OpDiv:
		if ri == 0 {
			return nil
		}
		return li / ri
	case OpMod:
		// SQL: `a MOD 0` is undefined / NULL. Match Div's nil-on-zero
		// guard. Sign of result matches Go's `%` (truncated toward
		// zero) — matches MySQL / PostgreSQL semantics.
		if ri == 0 {
			return nil
		}
		return li % ri
	}
	return nil
}

// --- BooleanValue + CastValue -------------------------------------

// BooleanValue is a literal true / false (and NULL when Value is
// nil — SQL UNKNOWN at the Value layer).
type BooleanValue struct {
	Value *bool // nil = UNKNOWN
}

// NewBooleanValue wraps a Go bool.
func NewBooleanValue(v bool) *BooleanValue {
	b := v
	return &BooleanValue{Value: &b}
}

func (*BooleanValue) Children() []Value { return []Value{} }
func (*BooleanValue) Type() ValueType   { return TypeBool }
func (*BooleanValue) Name() string      { return "bool" }

func (b *BooleanValue) Evaluate(any) any {
	if b.Value == nil {
		return nil
	}
	return *b.Value
}

// CastValue converts a child Value's result to a target ValueType.
// Seed handles the trivial conversions our existing corpus needs:
// int ↔ string (via strconv-free formatting for the seed), bool ↔
// int (false=0, true=1). Unknown conversions return nil (UNKNOWN).
// Full type tower lands with the Type hierarchy.
type CastValue struct {
	Child  Value
	Target ValueType
}

// NewCastValue constructs a CastValue.
func NewCastValue(child Value, target ValueType) *CastValue {
	return &CastValue{Child: child, Target: target}
}

func (c *CastValue) Children() []Value { return []Value{c.Child} }
func (c *CastValue) Type() ValueType   { return c.Target }
func (c *CastValue) Name() string      { return "cast" }

func (c *CastValue) Evaluate(evalCtx any) any {
	v := c.Child.Evaluate(evalCtx)
	if v == nil {
		return nil
	}
	switch c.Target {
	case TypeInt:
		switch val := v.(type) {
		case int64:
			return val
		case bool:
			if val {
				return int64(1)
			}
			return int64(0)
		}
	case TypeBool:
		switch val := v.(type) {
		case bool:
			return val
		case int64:
			return val != 0
		}
	case TypeString:
		if s, ok := v.(string); ok {
			return s
		}
		if i, ok := v.(int64); ok {
			return uitoa(uint64(i))
		}
	case TypeFloat:
		// CAST … AS FLOAT — accept float64/float32 verbatim; promote
		// integral types to float64. Without this case, the walker's
		// shiny new CastValue{TypeFloat} path silently returns nil
		// from Evaluate and constant-fold of `CAST(5 AS FLOAT) = 3.14`
		// gets UNKNOWN instead of FALSE.
		switch val := v.(type) {
		case float64:
			return val
		case float32:
			return float64(val)
		case int64:
			return float64(val)
		}
	}
	return nil
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
// Panics on duplicate field names — callers should rename via AS
// before constructing.
func NewRecordConstructorValue(fields ...RecordConstructorField) *RecordConstructorValue {
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if _, dup := seen[f.Name]; dup {
			panic("NewRecordConstructorValue: duplicate field name: " + f.Name)
		}
		seen[f.Name] = struct{}{}
	}
	// Defensive copy so the caller can't mutate.
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

// Type returns TypeUnknown — record type is a struct shape which
// the seed's flat ValueType enum can't express. The Type hierarchy
// port replaces this with a real StructType.
func (*RecordConstructorValue) Type() ValueType { return TypeUnknown }

// Name returns the debug-print kind.
func (*RecordConstructorValue) Name() string { return "record" }

// Evaluate produces a map[string]any with each field evaluated.
// Downstream consumers (projections, field-access) index into this
// map by field name.
func (r *RecordConstructorValue) Evaluate(evalCtx any) any {
	out := make(map[string]any, len(r.Fields))
	for _, f := range r.Fields {
		out[f.Name] = f.Value.Evaluate(evalCtx)
	}
	return out
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
	Target ValueType
}

// NewPromoteValue constructs a PromoteValue. Rejects nil child and
// zero-value Target — both are programmer errors.
func NewPromoteValue(child Value, target ValueType) *PromoteValue {
	if child == nil {
		panic("NewPromoteValue: child is nil")
	}
	if target == TypeUnknown {
		panic("NewPromoteValue: target is TypeUnknown; use CastValue if target is genuinely unknown")
	}
	return &PromoteValue{Child: child, Target: target}
}

// Children returns the single child as a one-element slice.
func (p *PromoteValue) Children() []Value { return []Value{p.Child} }

// Type returns the promotion target.
func (p *PromoteValue) Type() ValueType { return p.Target }

// Name returns the debug-print kind.
func (*PromoteValue) Name() string { return "promote" }

// Evaluate delegates to the child — the seed treats Promote as a
// no-op at runtime since cmpAny already handles cross-width
// promotion. Plan-time inspection (explain, rewrite rules) is where
// Promote earns its keep.
func (p *PromoteValue) Evaluate(evalCtx any) any {
	return p.Child.Evaluate(evalCtx)
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
	// Seed keeps it as TypeUnknown until the Type hierarchy port
	// lands — the test surface doesn't need real struct types yet.
	Typ ValueType
}

// NewQuantifiedObjectValue constructs a QuantifiedObjectValue. Zero
// correlation is rejected — a quantifier without an identifier is a
// design error, not something the analyzer should allow.
func NewQuantifiedObjectValue(corr CorrelationIdentifier) *QuantifiedObjectValue {
	if corr.IsZero() {
		panic("NewQuantifiedObjectValue: correlation is zero-value; use NamedCorrelationIdentifier or UniqueCorrelationIdentifier")
	}
	return &QuantifiedObjectValue{Correlation: corr, Typ: TypeUnknown}
}

// Children returns an empty slice — the quantifier is a leaf in
// the Value tree, with its correlation link being external metadata
// (not a child Value).
func (*QuantifiedObjectValue) Children() []Value { return []Value{} }

// Type returns the seed's placeholder TypeUnknown for the row type.
func (q *QuantifiedObjectValue) Type() ValueType { return q.Typ }

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
func (q *QuantifiedObjectValue) Evaluate(evalCtx any) any {
	if evalCtx == nil {
		return nil
	}
	switch ctx := evalCtx.(type) {
	case map[CorrelationIdentifier]map[string]any:
		return ctx[q.Correlation]
	case map[string]any:
		return ctx
	}
	return nil
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

// Type returns the SQL type the aggregate produces. COUNT / COUNT(*)
// always produce int; SUM / MIN / MAX inherit from the operand
// (seed assumes int — grows with the Type hierarchy port); AVG
// stays int in the seed even though true SQL AVG returns a rational.
func (a *AggregateValue) Type() ValueType {
	switch a.Op {
	case AggCount, AggCountStar:
		return TypeInt
	case AggSum, AggMin, AggMax, AggAvg:
		if a.Operand != nil {
			return a.Operand.Type()
		}
		return TypeInt
	}
	return TypeUnknown
}

// Name returns the debug-print kind.
func (*AggregateValue) Name() string { return "agg" }

// Evaluate panics — aggregates are multi-row and don't have a
// single-row Evaluate semantics. Rule / plan code type-asserts
// AggregateValue and routes it to an accumulator instead of calling
// Evaluate. The panic message is loud so nobody silently returns nil
// and debugs for an hour.
func (a *AggregateValue) Evaluate(any) any {
	panic("AggregateValue.Evaluate: aggregate must be evaluated over rows by the aggregator, not per-row")
}
