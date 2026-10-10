// Portions derived from FoundationDB Record Layer (NotOverComparisonRule.java),
// Copyright 2015-2023 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// Predicate-simplification rules.
//
// Each rule defines a matcher and OnMatch body; Simplify
// (simplifier.go) drives them to fixpoint in production, FireRule
// drives them in tests. These mirror Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.
// simplification.*` predicate simplifications:
//
//   - AndConstantSimplifyRule → AndPredicate with a constant child
//     simplifies. TRUE child drops; FALSE child collapses whole
//     AndPredicate to FALSE.
//   - OrConstantSimplifyRule → OrPredicate mirror: FALSE child
//     drops; TRUE child collapses whole OrPredicate to TRUE.
//
// Seed uses a QueryPredicate-shaped matcher (the existing matcher
// interface is over `any`, so it works directly on QueryPredicate
// trees without any new matcher types).

// AndConstantSimplifyRule matches an AndPredicate and folds constant
// children per Kleene AND identities.
type AndConstantSimplifyRule struct {
	matcher matching.BindingMatcher
}

// NewAndConstantSimplifyRule constructs the rule.
func NewAndConstantSimplifyRule() *AndConstantSimplifyRule {
	m := &AndConstantSimplifyRule{}
	// Match any *AndPredicate via an Instance-like matcher. Seed
	// doesn't have a generic predicate-type matcher, so inline one.
	m.matcher = newAndPredicateMatcher()
	return m
}

func (r *AndConstantSimplifyRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *AndConstantSimplifyRule) OnMatch(call *RuleCall) {
	and := call.Bindings.Get(r.matcher).(*predicates.AndPredicate)
	// Collect non-TRUE children; short-circuit on FALSE.
	var kept []predicates.QueryPredicate
	for i, sp := range and.SubPredicates {
		if cp, ok := sp.(*predicates.ConstantPredicate); ok {
			if cp.Value == predicates.TriFalse {
				// Whole AND collapses to FALSE regardless of siblings.
				call.Yield(predicates.NewConstantPredicate(predicates.TriFalse))
				return
			}
			if cp.Value == predicates.TriTrue {
				// Copy only when an identity is actually removed.
				if kept == nil {
					kept = make([]predicates.QueryPredicate, i, len(and.SubPredicates)-1)
					copy(kept, and.SubPredicates[:i])
				}
				continue
			}
			// UNKNOWN: keep as-is — the AND rule fires again on a
			// rewrite that canonicalises UNKNOWN before the AND.
		}
		if kept != nil {
			kept = append(kept, sp)
		}
	}
	// Only yield when we actually changed something.
	if kept == nil {
		return
	}
	switch len(kept) {
	case 0:
		call.Yield(predicates.NewConstantPredicate(predicates.TriTrue))
	case 1:
		call.Yield(kept[0])
	default:
		call.Yield(&predicates.AndPredicate{SubPredicates: kept})
	}
}

// OrConstantSimplifyRule matches an OrPredicate and folds constant
// children per Kleene OR identities.
type OrConstantSimplifyRule struct {
	matcher matching.BindingMatcher
}

// NewOrConstantSimplifyRule constructs the rule.
func NewOrConstantSimplifyRule() *OrConstantSimplifyRule {
	m := &OrConstantSimplifyRule{}
	m.matcher = newOrPredicateMatcher()
	return m
}

func (r *OrConstantSimplifyRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *OrConstantSimplifyRule) OnMatch(call *RuleCall) {
	or := call.Bindings.Get(r.matcher).(*predicates.OrPredicate)
	var kept []predicates.QueryPredicate
	for i, sp := range or.SubPredicates {
		if cp, ok := sp.(*predicates.ConstantPredicate); ok {
			if cp.Value == predicates.TriTrue {
				call.Yield(predicates.NewConstantPredicate(predicates.TriTrue))
				return
			}
			if cp.Value == predicates.TriFalse {
				if kept == nil {
					kept = make([]predicates.QueryPredicate, i, len(or.SubPredicates)-1)
					copy(kept, or.SubPredicates[:i])
				}
				continue
			}
		}
		if kept != nil {
			kept = append(kept, sp)
		}
	}
	if kept == nil {
		return
	}
	switch len(kept) {
	case 0:
		call.Yield(predicates.NewConstantPredicate(predicates.TriFalse))
	case 1:
		call.Yield(kept[0])
	default:
		call.Yield(&predicates.OrPredicate{SubPredicates: kept})
	}
}

// --- NotConstantSimplifyRule + DoubleNegationRule ------------------

// NotConstantSimplifyRule folds NOT over a constant child per Kleene
// NOT (NOT TRUE=FALSE, NOT FALSE=TRUE, NOT UNKNOWN=UNKNOWN). Also
// fires on NOT NOT x → x (double-negation elimination).
type NotConstantSimplifyRule struct {
	matcher matching.BindingMatcher
}

// NewNotConstantSimplifyRule constructs the rule.
func NewNotConstantSimplifyRule() *NotConstantSimplifyRule {
	m := &NotConstantSimplifyRule{}
	m.matcher = newNotPredicateMatcher()
	return m
}

func (r *NotConstantSimplifyRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *NotConstantSimplifyRule) OnMatch(call *RuleCall) {
	not := call.Bindings.Get(r.matcher).(*predicates.NotPredicate)
	// NOT NOT x → x (double-negation elimination).
	if inner, ok := not.Child.(*predicates.NotPredicate); ok {
		call.Yield(inner.Child)
		return
	}
	// NOT <constant> → constant with Kleene-negated value.
	cp, ok := not.Child.(*predicates.ConstantPredicate)
	if !ok {
		return
	}
	switch cp.Value {
	case predicates.TriTrue:
		call.Yield(predicates.NewConstantPredicate(predicates.TriFalse))
	case predicates.TriFalse:
		call.Yield(predicates.NewConstantPredicate(predicates.TriTrue))
	default:
		call.Yield(predicates.NewConstantPredicate(predicates.TriUnknown))
	}
}

// predicateMatcher lives in rule.go alongside CascadesRule —
// it's shared infrastructure used by every rule pattern.

func newNotPredicateMatcher() *predicateMatcher[*predicates.NotPredicate] {
	return &predicateMatcher[*predicates.NotPredicate]{rootType: "NotPredicate"}
}

// --- Predicate matchers -------------------------------------------

func newAndPredicateMatcher() *predicateMatcher[*predicates.AndPredicate] {
	return &predicateMatcher[*predicates.AndPredicate]{rootType: "AndPredicate"}
}

func newOrPredicateMatcher() *predicateMatcher[*predicates.OrPredicate] {
	return &predicateMatcher[*predicates.OrPredicate]{rootType: "OrPredicate"}
}

// --- NotComparisonRewriteRule --------------------------------------

// NotComparisonRewriteRule pushes a NOT past a ComparisonPredicate whose
// comparison type ComparisonType.Negate inverts. Ports Java's
// NotOverComparisonRule, which is that same table plus this same decline
// (NotOverComparisonRule.java:71-73 returns on a null inversion).
//
// What it does NOT rewrite is the half worth stating, because each of these
// looks like it should and does not — measured by running every one of them
// through Simplify, and pinned by TestNotComparisonRewrite_CoverageIsJavasTable:
//
//	NOT (x IS NULL)                 stays a NotPredicate
//	NOT (x IS NOT NULL)             stays a NotPredicate
//	NOT (x <> 5)                    stays a NotPredicate
//	NOT (x IS DISTINCT FROM 5)      stays a NotPredicate
//	NOT (x IS NOT DISTINCT FROM 5)  stays a NotPredicate
//	NOT (x IN (...)), NOT (x STARTS_WITH 'p')   stay NotPredicates
//
// The two unary null tests are the trap: each IS the other's negation, and Java
// still refuses them — `invertComparisonType` opens with
// `if (type.isUnary()) return null;`. A reader who assumes otherwise goes
// looking for an `IS NOT NULL` leaf in a plan that has a `NOT` wrapper.
//
// What it DOES rewrite is the five Java inverts: `NOT(x = 5)` -> `x <> 5`, and
// the four ordering comparisons to their complements.
//
// The point of pushing a NOT to a leaf at all is that downstream index-pushdown
// rules then see a canonical leaf comparison rather than a NOT wrapper.
type NotComparisonRewriteRule struct {
	matcher matching.BindingMatcher
}

// NewNotComparisonRewriteRule constructs the rule.
func NewNotComparisonRewriteRule() *NotComparisonRewriteRule {
	return &NotComparisonRewriteRule{matcher: newNotPredicateMatcher()}
}

func (r *NotComparisonRewriteRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *NotComparisonRewriteRule) OnMatch(call *RuleCall) {
	not := call.Bindings.Get(r.matcher).(*predicates.NotPredicate)
	cp, ok := not.Child.(*predicates.ComparisonPredicate)
	if !ok {
		return
	}
	negated, ok := cp.Comparison.Type.Negate()
	if !ok {
		return
	}
	call.Yield(&predicates.ComparisonPredicate{
		Operand:    cp.Operand,
		Comparison: predicates.Comparison{Type: negated, Operand: cp.Comparison.Operand},
	})
}

// --- AbsorptionRule: AND-absorbs-OR and OR-absorbs-AND -------------
//
// Classical boolean absorption:
//
//	p AND (p OR q) ≡ p         (AndAbsorbOrRule)
//	p OR  (p AND q) ≡ p        (OrAbsorbAndRule)
//
// Rewrites the enclosing AND/OR by dropping the redundant OR/AND
// child when any of that child's operands is structurally equal to
// any sibling in the enclosing connective. Mirrors Java's
// `ValueSimplificationRuleSet` absorption pass.

// AndAbsorbOrRule: inside an AND, any OR child that contains a
// sibling is redundant — drop it. `AND(p, OR(p, q))` → `AND(p)` → `p`
// once the constant-fold rules collapse the unary AND.
type AndAbsorbOrRule struct {
	matcher matching.BindingMatcher
}

// NewAndAbsorbOrRule constructs the rule.
func NewAndAbsorbOrRule() *AndAbsorbOrRule {
	r := &AndAbsorbOrRule{}
	r.matcher = newAndPredicateMatcher()
	return r
}

func (r *AndAbsorbOrRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *AndAbsorbOrRule) OnMatch(call *RuleCall) {
	and := call.Bindings.Get(r.matcher).(*predicates.AndPredicate)
	kept := absorbMinorTerms(and.SubPredicates, normalFormCNF)
	if len(kept) < len(and.SubPredicates) {
		call.Yield(buildAnd(kept))
	}
}

// OrAbsorbAndRule: mirror. Inside an OR, any AND child that contains
// a sibling is redundant — drop it. `OR(p, AND(p, q))` → `OR(p)` → `p`.
type OrAbsorbAndRule struct {
	matcher matching.BindingMatcher
}

// NewOrAbsorbAndRule constructs the rule.
func NewOrAbsorbAndRule() *OrAbsorbAndRule {
	r := &OrAbsorbAndRule{}
	r.matcher = newOrPredicateMatcher()
	return r
}

func (r *OrAbsorbAndRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *OrAbsorbAndRule) OnMatch(call *RuleCall) {
	or := call.Bindings.Get(r.matcher).(*predicates.OrPredicate)
	kept := absorbMinorTerms(or.SubPredicates, normalFormDNF)
	if len(kept) < len(or.SubPredicates) {
		call.Yield(buildOr(kept))
	}
}

// Java rebuilds surviving minor sets only when absorption removes a major term.
// Rebuilding intentionally drops minor atomicity, permitting later normalization.
func absorbMinorTerms(terms []predicates.QueryPredicate, mode normalFormMode) []predicates.QueryPredicate {
	var clauseBuffer [16][]predicates.QueryPredicate
	clauses := clauseBuffer[:]
	if len(terms) > len(clauseBuffer) {
		clauses = make([][]predicates.QueryPredicate, len(terms))
	} else {
		clauses = clauses[:len(terms)]
	}
	for i, term := range terms {
		if mode.isMinor(term) {
			clauses[i] = dedupPredicateSlice(term.Children())
		} else {
			clauses[i] = terms[i : i+1]
		}
	}
	var survivorBuffer [16]int
	survivors := absorptionSurvivors(clauses, survivorBuffer[:0])
	if len(survivors) == len(terms) {
		return terms
	}
	kept := make([]predicates.QueryPredicate, 0, len(survivors))
	for _, i := range survivors {
		kept = append(kept, mode.minorWithChildren(clauses[i]))
	}
	return kept
}
