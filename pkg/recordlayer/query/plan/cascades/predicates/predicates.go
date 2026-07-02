package predicates

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// QueryPredicate hierarchy — seed.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.predicates.
// QueryPredicate` (trimmed to essentials). A QueryPredicate is
// the Cascades-level representation of a boolean expression; the
// SQL WHERE clause becomes a QueryPredicate tree after semantic
// analysis. Rules match on these.
//
// Semantics: SQL Kleene 3-valued logic. Eval returns a tri-state
// (true / false / nil-for-UNKNOWN) — mirrors the embedded
// engine's `triBool`.
//
// Seed types:
//
//   - ConstantPredicate — literal true / false / UNKNOWN.
//   - AndPredicate / OrPredicate — Kleene AND/OR over children.
//   - NotPredicate — Kleene NOT.
//   - ValuePredicate — bare boolean Value used as predicate.
//
// Follow-up work adds: ComparisonRange (see Java's
// `ComparisonRange` aggregator), `Placeholder` (rule-match
// parameter binding), `PredicateWithValueAndRanges`.
// ComparisonPredicate lives in comparisons.go (paired with the
// Comparison / ComparisonType carriers).

// TriBool is the SQL 3-valued logic result. A nil pointer is
// UNKNOWN; otherwise the bool value is true or false. Chose a
// pointer over a dedicated enum so downstream eval code can use
// `if result != nil && *result { ... }` without a custom type.
type TriBool *bool

// TriTrue / TriFalse / TriUnknown are the canonical tri-state
// constants. Matchers compare against these rather than
// constructing fresh pointers.
var (
	triTrueVal  = true
	triFalseVal = false

	TriTrue    TriBool = &triTrueVal
	TriFalse   TriBool = &triFalseVal
	TriUnknown TriBool = nil
)

// WalkPredicate applies visit to every node in p's subtree,
// pre-order. If visit returns false, descent into that node's
// children is skipped (but siblings + ancestors continue). Rule
// authors use this for tree-wide searches (e.g. "does any
// descendant reference the discarded correlation ID?").
//
// Safe on nil: returns immediately. Safe on cyclic trees: predicate
// construction is non-cyclic by contract, so cycle detection is
// unnecessary.
func WalkPredicate(p QueryPredicate, visit func(QueryPredicate) bool) {
	if p == nil {
		return
	}
	if !visit(p) {
		return
	}
	for _, c := range p.Children() {
		WalkPredicate(c, visit)
	}
}

// AsConstant returns (value, true) if p is a ConstantPredicate;
// (_, false) otherwise. Rule bodies use this as the canonical
// "is this a fold-able constant?" check, instead of open-coding
// type assertions.
func AsConstant(p QueryPredicate) (TriBool, bool) {
	if cp, ok := p.(*ConstantPredicate); ok {
		return cp.Value, true
	}
	return nil, false
}

// PredicateSize returns the total node count in p (p + all
// descendants). Rule authors use this to gate expensive
// transformations (e.g. De Morgan expansion that would double the
// size). Constant-time-per-node; cycle-free by construction.
func PredicateSize(p QueryPredicate) int {
	if p == nil {
		return 0
	}
	n := 1
	for _, c := range p.Children() {
		n += PredicateSize(c)
	}
	return n
}

// PredicateEquals reports structural equality between two
// QueryPredicates. Two predicates are equal when their concrete
// Go types match AND their children + carried data match
// recursively. Constants compare by TriBool identity; ComparisonPredicate
// compares by operand Name + Comparison (Type + Operand literal);
// ValuePredicate compares by wrapped Value Name.
//
// Used by future dedup rules (e.g. `AND(p, p)` → `p`,
// `OR(x, NOT x)` → TRUE). Seed doesn't ship those rules yet, but the
// equality helper belongs with the predicate types themselves so
// rule authors don't roll their own.
func PredicateEquals(a, b QueryPredicate) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch ap := a.(type) {
	case *ConstantPredicate:
		bp, ok := b.(*ConstantPredicate)
		return ok && ap.Value == bp.Value
	case *AndPredicate:
		bp, ok := b.(*AndPredicate)
		return ok && predicateListsEqual(ap.SubPredicates, bp.SubPredicates)
	case *OrPredicate:
		bp, ok := b.(*OrPredicate)
		return ok && predicateListsEqual(ap.SubPredicates, bp.SubPredicates)
	case *NotPredicate:
		bp, ok := b.(*NotPredicate)
		return ok && PredicateEquals(ap.Child, bp.Child)
	case *ValuePredicate:
		bp, ok := b.(*ValuePredicate)
		if !ok {
			return false
		}
		return valueNamesEqual(ap.Value, bp.Value)
	case *ComparisonPredicate:
		bp, ok := b.(*ComparisonPredicate)
		if !ok {
			return false
		}
		// Comparison.Operand is a Value; structural equality goes
		// through valueNamesEqual, which compares via ExplainValue.
		// ConstantValue / NullValue / BooleanValue render their
		// literal content, so equal literals render equal; FieldValue
		// renders its name; IN-lists (ConstantValue over []any)
		// render element-wise. Same surface as the LHS Operand
		// comparison below. Escape rune is part of the
		// Comparison's identity for LIKE — `LIKE 'x' ESCAPE '\'` and
		// `LIKE 'x' ESCAPE '!'` are distinct predicates.
		//
		// Unary types (IS [NOT] NULL) ignore Operand at Eval time, so
		// `IsNull{Operand: nil}` and `IsNull{Operand: LiteralValue(nil)}`
		// are semantically equivalent and must compare equal even
		// though their Operand fields differ structurally.
		if ap.Comparison.Type != bp.Comparison.Type ||
			ap.Comparison.Escape != bp.Comparison.Escape ||
			!valueNamesEqual(ap.Operand, bp.Operand) {
			return false
		}
		if ap.Comparison.Type.IsUnary() {
			return true
		}
		return valueNamesEqual(ap.Comparison.Operand, bp.Comparison.Operand)
	}
	return false
}

func predicateListsEqual(a, b []QueryPredicate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !PredicateEquals(a[i], b[i]) {
			return false
		}
	}
	return true
}

func valueNamesEqual(a, b values.Value) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return values.ValuesStructurallyEqual(a, b)
}

// QueryPredicate is the root of the predicate hierarchy. A
// QueryPredicate is a tree of boolean expressions with 3-valued
// logic semantics.
type QueryPredicate interface {
	// Children returns the immediate sub-predicates. Leaf
	// predicates (ConstantPredicate, ComparisonPredicate, …)
	// return an empty slice.
	Children() []QueryPredicate

	// Eval returns the predicate's truth value given an
	// opaque evaluation context. Concrete eval context is
	// impl-defined; the seed predicates ignore it. A non-nil
	// error signals a runtime evaluation failure (e.g. a
	// type-mismatch comparison or an erroring child Value); the
	// returned TriBool is TriUnknown in that case.
	Eval(evalCtx any) (TriBool, error)

	// Explain renders a parenthesised textual form suitable for
	// debug + plan-diff output.
	Explain() string

	// GetCorrelatedTo returns the transitive set of CorrelationIdentifiers
	// this predicate and all its descendants reference. Each concrete
	// type contributes its own correlations (Values, existential aliases,
	// parameter aliases) plus the union of all children's correlations.
	// Mirrors Java's QueryPredicate.getCorrelatedTo() which is transitive.
	GetCorrelatedTo() map[values.CorrelationIdentifier]struct{}
}

// IsTautology reports whether the predicate always evaluates to TRUE.
// Only ConstantPredicate(TRUE) is a tautology; all other predicates
// return false. Mirrors Java's QueryPredicate.isTautology().
func IsTautology(p QueryPredicate) bool {
	if cp, ok := p.(*ConstantPredicate); ok {
		return cp.Value == TriTrue
	}
	return false
}

// IsContradiction reports whether the predicate always evaluates to FALSE.
func IsContradiction(p QueryPredicate) bool {
	if cp, ok := p.(*ConstantPredicate); ok {
		return cp.Value == TriFalse
	}
	return false
}

// --- ConstantPredicate ---------------------------------------------

// ConstantPredicate is a literal truth value (true / false /
// UNKNOWN). Useful for simplification rules that reduce a subtree
// to a constant.
type ConstantPredicate struct {
	Value TriBool
}

// NewConstantPredicate wraps a TriBool as a ConstantPredicate.
func NewConstantPredicate(v TriBool) *ConstantPredicate {
	return &ConstantPredicate{Value: v}
}

func (*ConstantPredicate) Children() []QueryPredicate  { return []QueryPredicate{} }
func (p *ConstantPredicate) Eval(any) (TriBool, error) { return p.Value, nil }

// GetCorrelatedTo returns the empty set — constants reference no
// quantifier aliases.
func (*ConstantPredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (p *ConstantPredicate) Explain() string {
	switch {
	case p.Value == TriTrue:
		return "TRUE"
	case p.Value == TriFalse:
		return "FALSE"
	default:
		return "UNKNOWN"
	}
}

// --- AndPredicate --------------------------------------------------

// AndPredicate is the Kleene AND of children. Empty children yields
// TRUE (identity). A single FALSE child short-circuits to FALSE.
// An UNKNOWN + no-FALSE yields UNKNOWN.
type AndPredicate struct {
	SubPredicates []QueryPredicate
}

// NewAnd constructs an AndPredicate.
func NewAnd(preds ...QueryPredicate) *AndPredicate {
	return &AndPredicate{SubPredicates: preds}
}

func (p *AndPredicate) Children() []QueryPredicate { return p.SubPredicates }

// GetCorrelatedTo returns the union of all children's correlations.
func (p *AndPredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, child := range p.SubPredicates {
		for k := range child.GetCorrelatedTo() {
			out[k] = struct{}{}
		}
	}
	return out
}

func (p *AndPredicate) Eval(evalCtx any) (TriBool, error) {
	// Kleene AND: TRUE ∧ x = x; FALSE ∧ x = FALSE; UNKNOWN ∧ TRUE
	// = UNKNOWN; UNKNOWN ∧ UNKNOWN = UNKNOWN; UNKNOWN ∧ FALSE =
	// FALSE (short-circuit). Scan once, tracking sawUnknown.
	//
	// Error handling preserves the existing left-to-right short-circuit
	// order: a child's error propagates immediately (it would in Java,
	// where eval throws), exactly as a FALSE short-circuits to FALSE —
	// whichever the scan reaches first in document order wins. A FALSE
	// still beats a later UNKNOWN (sawUnknown is only consulted after a
	// clean full scan), so the three-valued logic is unchanged.
	sawUnknown := false
	for _, sp := range p.SubPredicates {
		v, err := sp.Eval(evalCtx)
		if err != nil {
			return TriUnknown, err
		}
		switch v {
		case TriFalse:
			return TriFalse, nil
		case TriUnknown:
			sawUnknown = true
		}
	}
	if sawUnknown {
		return TriUnknown, nil
	}
	return TriTrue, nil
}

func (p *AndPredicate) Explain() string {
	if len(p.SubPredicates) == 0 {
		return "TRUE"
	}
	parts := make([]string, len(p.SubPredicates))
	for i, sp := range p.SubPredicates {
		parts[i] = sp.Explain()
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// --- OrPredicate ---------------------------------------------------

// OrPredicate is the Kleene OR of children. Empty children yields
// FALSE (identity). A single TRUE child short-circuits to TRUE.
type OrPredicate struct {
	SubPredicates []QueryPredicate
}

// NewOr constructs an OrPredicate.
func NewOr(preds ...QueryPredicate) *OrPredicate {
	return &OrPredicate{SubPredicates: preds}
}

func (p *OrPredicate) Children() []QueryPredicate { return p.SubPredicates }

// GetCorrelatedTo returns the union of all children's correlations.
func (p *OrPredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, child := range p.SubPredicates {
		for k := range child.GetCorrelatedTo() {
			out[k] = struct{}{}
		}
	}
	return out
}

func (p *OrPredicate) Eval(evalCtx any) (TriBool, error) {
	// Kleene OR: FALSE ∨ x = x; TRUE ∨ x = TRUE; UNKNOWN ∨ FALSE
	// = UNKNOWN; UNKNOWN ∨ UNKNOWN = UNKNOWN; UNKNOWN ∨ TRUE = TRUE.
	//
	// Error handling mirrors AndPredicate: a child error propagates
	// immediately in document order, just as a TRUE short-circuits to
	// TRUE; a TRUE still beats a later UNKNOWN (sawUnknown is consulted
	// only after a clean full scan).
	sawUnknown := false
	for _, sp := range p.SubPredicates {
		v, err := sp.Eval(evalCtx)
		if err != nil {
			return TriUnknown, err
		}
		switch v {
		case TriTrue:
			return TriTrue, nil
		case TriUnknown:
			sawUnknown = true
		}
	}
	if sawUnknown {
		return TriUnknown, nil
	}
	return TriFalse, nil
}

func (p *OrPredicate) Explain() string {
	if len(p.SubPredicates) == 0 {
		return "FALSE"
	}
	parts := make([]string, len(p.SubPredicates))
	for i, sp := range p.SubPredicates {
		parts[i] = sp.Explain()
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// --- ValuePredicate ------------------------------------------------

// ValuePredicate wraps a boolean-typed Value as a predicate. The
// Value evaluates to bool (or nil for UNKNOWN); ValuePredicate.Eval
// maps that straight to TriBool. `SELECT ... WHERE is_active` where
// `is_active` is a boolean column or expression goes through this
// node after semantic analysis.
//
// Returns UNKNOWN when the Value evaluates to nil (NULL) or to any
// non-bool type — the latter is a type-checking responsibility the
// semantic analyzer should have already caught; falling through to
// UNKNOWN keeps the runtime safe against analyzer gaps.
type ValuePredicate struct {
	Value values.Value
}

// NewValuePredicate constructs a ValuePredicate.
func NewValuePredicate(v values.Value) *ValuePredicate {
	return &ValuePredicate{Value: v}
}

func (*ValuePredicate) Children() []QueryPredicate { return []QueryPredicate{} }

// GetCorrelatedTo returns the correlations from the carried Value.
func (p *ValuePredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	if p.Value == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	out := values.GetCorrelatedToOfValue(p.Value)
	if out == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	return out
}

func (p *ValuePredicate) Eval(evalCtx any) (TriBool, error) {
	if p.Value == nil {
		return TriUnknown, nil
	}
	v, err := p.Value.Evaluate(evalCtx)
	if err != nil {
		return TriUnknown, err
	}
	if v == nil {
		return TriUnknown, nil
	}
	bv, ok := v.(bool)
	if !ok {
		return TriUnknown, nil
	}
	if bv {
		return TriTrue, nil
	}
	return TriFalse, nil
}

func (p *ValuePredicate) Explain() string {
	if p.Value == nil {
		return "<nil-value>"
	}
	// Use the tree-walking ExplainValue for per-instance rendering.
	// Value.Name() returns the KIND ("field", "constant", …) which
	// isn't useful for explain output — e.g. a FieldValue would
	// render as just `field` instead of the actual column name.
	return values.ExplainValue(p.Value)
}

// --- NotPredicate --------------------------------------------------

// NotPredicate is the Kleene NOT of a single child. NOT UNKNOWN =
// UNKNOWN.
type NotPredicate struct {
	Child QueryPredicate
}

// NewNot constructs a NotPredicate.
func NewNot(child QueryPredicate) *NotPredicate {
	return &NotPredicate{Child: child}
}

func (p *NotPredicate) Children() []QueryPredicate { return []QueryPredicate{p.Child} }

// GetCorrelatedTo returns the child's correlations.
func (p *NotPredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	if p.Child == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	return p.Child.GetCorrelatedTo()
}

func (p *NotPredicate) Eval(evalCtx any) (TriBool, error) {
	v, err := p.Child.Eval(evalCtx)
	if err != nil {
		return TriUnknown, err
	}
	switch v {
	case TriTrue:
		return TriFalse, nil
	case TriFalse:
		return TriTrue, nil
	default:
		return TriUnknown, nil
	}
}

func (p *NotPredicate) Explain() string {
	// Wrap non-connective children so `NOT age >= 18` reads
	// unambiguously as `NOT (age >= 18)`. AndPredicate / OrPredicate
	// already wrap themselves — avoid double-parenthesizing them.
	child := p.Child.Explain()
	switch p.Child.(type) {
	case *AndPredicate, *OrPredicate:
		return "NOT " + child
	}
	return "NOT (" + child + ")"
}
