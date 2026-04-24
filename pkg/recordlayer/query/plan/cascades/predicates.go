package cascades

import (
	"fmt"
	"strings"
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
// Seed types (dayshift-46):
//
//   - ConstantPredicate — literal true / false / UNKNOWN.
//   - AndPredicate / OrPredicate — Kleene AND/OR over children.
//   - NotPredicate — Kleene NOT.
//
// Follow-up shifts add: ComparisonPredicate (wraps a Value +
// Comparisons.Type + Value), ValuePredicate (bare boolean Value),
// ComparisonRange, Placeholder.

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
	// impl-defined; the seed predicates ignore it.
	Eval(evalCtx any) TriBool

	// Explain renders a parenthesised textual form suitable for
	// debug + plan-diff output.
	Explain() string
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

func (*ConstantPredicate) Children() []QueryPredicate { return nil }
func (p *ConstantPredicate) Eval(any) TriBool         { return p.Value }
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

func (p *AndPredicate) Eval(evalCtx any) TriBool {
	// Kleene AND: TRUE ∧ x = x; FALSE ∧ x = FALSE; UNKNOWN ∧ TRUE
	// = UNKNOWN; UNKNOWN ∧ UNKNOWN = UNKNOWN; UNKNOWN ∧ FALSE =
	// FALSE (short-circuit). Scan once, tracking sawUnknown.
	sawUnknown := false
	for _, sp := range p.SubPredicates {
		v := sp.Eval(evalCtx)
		switch v {
		case TriFalse:
			return TriFalse
		case TriUnknown:
			sawUnknown = true
		}
	}
	if sawUnknown {
		return TriUnknown
	}
	return TriTrue
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

func (p *OrPredicate) Eval(evalCtx any) TriBool {
	// Kleene OR: FALSE ∨ x = x; TRUE ∨ x = TRUE; UNKNOWN ∨ FALSE
	// = UNKNOWN; UNKNOWN ∨ UNKNOWN = UNKNOWN; UNKNOWN ∨ TRUE = TRUE.
	sawUnknown := false
	for _, sp := range p.SubPredicates {
		v := sp.Eval(evalCtx)
		switch v {
		case TriTrue:
			return TriTrue
		case TriUnknown:
			sawUnknown = true
		}
	}
	if sawUnknown {
		return TriUnknown
	}
	return TriFalse
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

func (p *NotPredicate) Eval(evalCtx any) TriBool {
	switch p.Child.Eval(evalCtx) {
	case TriTrue:
		return TriFalse
	case TriFalse:
		return TriTrue
	default:
		return TriUnknown
	}
}

func (p *NotPredicate) Explain() string {
	return fmt.Sprintf("NOT %s", p.Child.Explain())
}
