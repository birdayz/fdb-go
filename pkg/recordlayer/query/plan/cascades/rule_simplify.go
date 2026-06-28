package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Predicate-simplification rules — seed.
//
// Examples of the rule pattern for Phase 4.5 Batch A. Each rule
// defines a matcher and OnMatch body; FireRule drives them from
// tests. These mirror Java's
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
	kept := make([]predicates.QueryPredicate, 0, len(and.SubPredicates))
	for _, sp := range and.SubPredicates {
		if cp, ok := sp.(*predicates.ConstantPredicate); ok {
			if cp.Value == predicates.TriFalse {
				// Whole AND collapses to FALSE regardless of siblings.
				call.Yield(predicates.NewConstantPredicate(predicates.TriFalse))
				return
			}
			if cp.Value == predicates.TriTrue {
				// TRUE is AND-identity; drop.
				continue
			}
			// UNKNOWN: keep as-is — the AND rule fires again on a
			// rewrite that canonicalises UNKNOWN before the AND.
		}
		kept = append(kept, sp)
	}
	// Only yield when we actually changed something.
	if len(kept) == len(and.SubPredicates) {
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
	kept := make([]predicates.QueryPredicate, 0, len(or.SubPredicates))
	for _, sp := range or.SubPredicates {
		if cp, ok := sp.(*predicates.ConstantPredicate); ok {
			if cp.Value == predicates.TriTrue {
				call.Yield(predicates.NewConstantPredicate(predicates.TriTrue))
				return
			}
			if cp.Value == predicates.TriFalse {
				// FALSE is OR-identity; drop.
				continue
			}
		}
		kept = append(kept, sp)
	}
	if len(kept) == len(or.SubPredicates) {
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

// --- AndFlattenRule / OrFlattenRule --------------------------------

// AndFlattenRule normalises nested AndPredicates: `AND(AND(a, b), c)`
// → `AND(a, b, c)`. Mirrors Java's associative-flatten in
// `ValueSimplificationRuleSet`. Runs before the constant-simplify
// pass so the simplifier sees a flat list of operands.
type AndFlattenRule struct {
	matcher matching.BindingMatcher
}

// NewAndFlattenRule constructs the rule.
func NewAndFlattenRule() *AndFlattenRule {
	r := &AndFlattenRule{}
	r.matcher = newAndPredicateMatcher()
	return r
}

func (r *AndFlattenRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *AndFlattenRule) OnMatch(call *RuleCall) {
	and := call.Bindings.Get(r.matcher).(*predicates.AndPredicate)
	// Check for any child that is itself an AndPredicate.
	hasNested := false
	for _, sp := range and.SubPredicates {
		if _, ok := sp.(*predicates.AndPredicate); ok {
			hasNested = true
			break
		}
	}
	if !hasNested {
		return
	}
	flat := make([]predicates.QueryPredicate, 0, len(and.SubPredicates))
	for _, sp := range and.SubPredicates {
		if inner, ok := sp.(*predicates.AndPredicate); ok {
			flat = append(flat, inner.SubPredicates...)
		} else {
			flat = append(flat, sp)
		}
	}
	call.Yield(&predicates.AndPredicate{SubPredicates: flat})
}

// OrFlattenRule: mirror of AndFlattenRule for OR.
type OrFlattenRule struct {
	matcher matching.BindingMatcher
}

// NewOrFlattenRule constructs the rule.
func NewOrFlattenRule() *OrFlattenRule {
	r := &OrFlattenRule{}
	r.matcher = newOrPredicateMatcher()
	return r
}

func (r *OrFlattenRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *OrFlattenRule) OnMatch(call *RuleCall) {
	or := call.Bindings.Get(r.matcher).(*predicates.OrPredicate)
	hasNested := false
	for _, sp := range or.SubPredicates {
		if _, ok := sp.(*predicates.OrPredicate); ok {
			hasNested = true
			break
		}
	}
	if !hasNested {
		return
	}
	flat := make([]predicates.QueryPredicate, 0, len(or.SubPredicates))
	for _, sp := range or.SubPredicates {
		if inner, ok := sp.(*predicates.OrPredicate); ok {
			flat = append(flat, inner.SubPredicates...)
		} else {
			flat = append(flat, sp)
		}
	}
	call.Yield(&predicates.OrPredicate{SubPredicates: flat})
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

// --- AndDedupRule / OrDedupRule ------------------------------------

// AndDedupRule removes structurally-equal duplicate children from
// an AndPredicate. `AND(p, p, q, p)` → `AND(p, q)`. Mirrors Java
// `PredicateSimplification`'s dedup pass.
type AndDedupRule struct {
	matcher matching.BindingMatcher
}

// NewAndDedupRule constructs the rule.
func NewAndDedupRule() *AndDedupRule {
	r := &AndDedupRule{}
	r.matcher = newAndPredicateMatcher()
	return r
}

func (r *AndDedupRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *AndDedupRule) OnMatch(call *RuleCall) {
	and := call.Bindings.Get(r.matcher).(*predicates.AndPredicate)
	deduped := dedupPredicates(and.SubPredicates)
	if len(deduped) == len(and.SubPredicates) {
		return
	}
	switch len(deduped) {
	case 0:
		call.Yield(predicates.NewConstantPredicate(predicates.TriTrue))
	case 1:
		call.Yield(deduped[0])
	default:
		call.Yield(&predicates.AndPredicate{SubPredicates: deduped})
	}
}

// OrDedupRule: mirror of AndDedupRule.
type OrDedupRule struct {
	matcher matching.BindingMatcher
}

// NewOrDedupRule constructs the rule.
func NewOrDedupRule() *OrDedupRule {
	r := &OrDedupRule{}
	r.matcher = newOrPredicateMatcher()
	return r
}

func (r *OrDedupRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *OrDedupRule) OnMatch(call *RuleCall) {
	or := call.Bindings.Get(r.matcher).(*predicates.OrPredicate)
	deduped := dedupPredicates(or.SubPredicates)
	if len(deduped) == len(or.SubPredicates) {
		return
	}
	switch len(deduped) {
	case 0:
		call.Yield(predicates.NewConstantPredicate(predicates.TriFalse))
	case 1:
		call.Yield(deduped[0])
	default:
		call.Yield(&predicates.OrPredicate{SubPredicates: deduped})
	}
}

// dedupPredicates returns a new slice with duplicates (by
// PredicateEquals) removed, preserving first-occurrence order.
// O(n²) is fine for AND/OR operand counts the corpus exercises
// (typically < 10 children).
func dedupPredicates(in []predicates.QueryPredicate) []predicates.QueryPredicate {
	out := make([]predicates.QueryPredicate, 0, len(in))
	for _, p := range in {
		dup := false
		for _, o := range out {
			if predicates.PredicateEquals(p, o) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// --- ComparisonConstantSimplifyRule --------------------------------

// ComparisonConstantSimplifyRule folds a ComparisonPredicate whose
// operand is a ConstantValue — both sides of the comparison are
// known at plan time, so the predicate evaluates to a constant.
// Mirrors Java's `ValueSimplificationRuleSet` constant-predicate
// short-circuits.
//
// Example: `5 = 3` → `FALSE`, `7 > 2` → `TRUE`, `NULL = 1` →
// `UNKNOWN`. Only fires when the operand's Evaluate(nil) returns
// non-context-dependent data — ConstantValue.Evaluate returns its
// literal regardless of context, which is the only current seed
// Value whose result is reproducible without an eval context.
type ComparisonConstantSimplifyRule struct {
	matcher matching.BindingMatcher
}

// NewComparisonConstantSimplifyRule constructs the rule.
func NewComparisonConstantSimplifyRule() *ComparisonConstantSimplifyRule {
	m := &ComparisonConstantSimplifyRule{}
	m.matcher = newComparisonPredicateMatcher()
	return m
}

func (r *ComparisonConstantSimplifyRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ComparisonConstantSimplifyRule) OnMatch(call *RuleCall) {
	cp := call.Bindings.Get(r.matcher).(*predicates.ComparisonPredicate)
	// Only fold when BOTH sides are deterministic without a row
	// context. Whitelist known-constant Value types rather than
	// calling Evaluate(nil) — FieldValue.Evaluate(nil) also returns
	// nil and would produce a false positive.
	lhs, lok := constantLiteral(cp.Operand)
	if !lok {
		return
	}
	// Binary comparisons require a known-constant RHS too. Unary
	// (IS [NOT] NULL) ignores the RHS entirely and is always
	// foldable once the LHS is known-constant.
	if !cp.Comparison.Type.IsUnary() {
		if cp.Comparison.Operand == nil {
			return
		}
		if !values.IsConstantValue(cp.Comparison.Operand) {
			return
		}
	}
	// Plan-time constant folding: a type-mismatch (e.g. WHERE 5 = 'abc')
	// declines to fold — leave the predicate node untouched rather than
	// propagating a runtime error from the planner.
	result, err := cp.Comparison.Eval(lhs)
	if err != nil {
		return
	}
	call.Yield(predicates.NewConstantPredicate(result))
}

// constantLiteral unwraps a known-constant Value to its Go-native
// literal for plan-time folding. Reports ok=false for any Value
// whose Evaluate depends on a row context (FieldValue, an
// ArithmeticValue over row columns, …).
//
// Leaf constants (ConstantValue / NullValue / BooleanValue) hit
// the fast path. Composites whose children are all constant —
// `CAST(5 AS STRING)`, `1 + 2`, `CAST(1+2 AS BOOL)` — fold via
// EvaluateConstant. The composite path is what lets
// ComparisonConstantSimplifyRule fire on `CAST(5 AS STRING) = 'X'`
// rather than leaving the whole predicate unsimplified.
func constantLiteral(v values.Value) (any, bool) {
	switch x := v.(type) {
	case *values.ConstantValue:
		return x.Value, true
	case *values.NullValue:
		return nil, true
	case *values.BooleanValue:
		// Unwrap *bool so the typed-nil doesn't masquerade as a
		// non-NULL bool when downstream Eval NULL-guards on
		// `left == nil`. A BooleanValue with Value==nil is SQL NULL.
		if x.Value == nil {
			return nil, true
		}
		return *x.Value, true
	}
	return values.EvaluateConstant(v)
}

func newComparisonPredicateMatcher() *predicateMatcher[*predicates.ComparisonPredicate] {
	return &predicateMatcher[*predicates.ComparisonPredicate]{rootType: "ComparisonPredicate"}
}

// --- Predicate matchers -------------------------------------------

func newAndPredicateMatcher() *predicateMatcher[*predicates.AndPredicate] {
	return &predicateMatcher[*predicates.AndPredicate]{rootType: "AndPredicate"}
}

func newOrPredicateMatcher() *predicateMatcher[*predicates.OrPredicate] {
	return &predicateMatcher[*predicates.OrPredicate]{rootType: "OrPredicate"}
}

// --- NotComparisonRewriteRule --------------------------------------

// NotComparisonRewriteRule pushes a NOT past a ComparisonPredicate
// whose comparison type has a direct negation: `NOT(x = 5)` →
// `x <> 5`, `NOT(x IS NULL)` → `x IS NOT NULL`. Leaves
// `NOT(x IN (...))` and `NOT(x STARTS_WITH 'pre')` alone — those
// have no direct-negation comparison type.
//
// Mirrors Java's predicate-simplification passes that push NOT down
// to leaves so downstream index-pushdown rules see a canonical
// leaf-level predicate and don't have to also handle NOT wrappers.
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
	// Preserve Escape across the negation. Today no Negate()-supporting
	// type carries a non-zero Escape (only ComparisonLike does, and
	// Negate declines on it), so this is defensive: if a future
	// ComparisonType grows both Negate-support and Escape-meaning, the
	// rewrite stays correct without an explicit fix.
	call.Yield(&predicates.ComparisonPredicate{
		Operand:    cp.Operand,
		Comparison: predicates.Comparison{Type: negated, Operand: cp.Comparison.Operand, Escape: cp.Comparison.Escape},
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
	kept := make([]predicates.QueryPredicate, 0, len(and.SubPredicates))
	changed := false
	for _, sp := range and.SubPredicates {
		or, ok := sp.(*predicates.OrPredicate)
		if !ok {
			kept = append(kept, sp)
			continue
		}
		// Drop the OR if any of its operands matches a sibling.
		if anyMatchesAnother(or.SubPredicates, and.SubPredicates, sp) {
			changed = true
			continue
		}
		kept = append(kept, sp)
	}
	if !changed {
		return
	}
	switch len(kept) {
	case 0:
		// Shouldn't happen — the matching OR still leaves its sibling.
		call.Yield(predicates.NewConstantPredicate(predicates.TriTrue))
	case 1:
		call.Yield(kept[0])
	default:
		call.Yield(&predicates.AndPredicate{SubPredicates: kept})
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
	kept := make([]predicates.QueryPredicate, 0, len(or.SubPredicates))
	changed := false
	for _, sp := range or.SubPredicates {
		and, ok := sp.(*predicates.AndPredicate)
		if !ok {
			kept = append(kept, sp)
			continue
		}
		if anyMatchesAnother(and.SubPredicates, or.SubPredicates, sp) {
			changed = true
			continue
		}
		kept = append(kept, sp)
	}
	if !changed {
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

// anyMatchesAnother reports whether any element of `candidates`
// structurally equals any element of `siblings` other than `self`.
// Used by the absorption rules to decide whether a child is made
// redundant by a sibling.
func anyMatchesAnother(candidates, siblings []predicates.QueryPredicate, self predicates.QueryPredicate) bool {
	for _, c := range candidates {
		for _, s := range siblings {
			if s == self {
				continue
			}
			if predicates.PredicateEquals(c, s) {
				return true
			}
		}
	}
	return false
}
