package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// EliminateNullOnEmptyRule drops the null-on-empty flag from a SelectExpression's
// ForEach quantifier when some predicate of the surrounding SELECT PROVABLY
// REJECTS the null tuple that the null-on-empty quantifier would inject at that
// quantifier's alias.
//
// Ports Java's EliminateNullOnEmptyRule (#4186), which REPLACED the buggy
// PullUpNullOnEmptyRule. PullUp's "positional-predicate-equality heuristic"
// (predicates.equals(otherPredicates)) was wrong with predicates that ACCEPT the
// injected null tuple (`… WHERE x IS NULL` over a null-on-empty leg) — it assumed
// the null tuple is always rejected. The correct test is semantic: substitute a
// typed NullValue at the quantifier's alias, constant-fold the predicate, and the
// quantifier is eligible iff the fold is FALSE or NULL (both filter the row out).
//
// Per-alias: a null-rejecting predicate over alias A eliminates A's null-on-empty
// flag while leaving a null-ACCEPTING sibling (`B IS NULL`) over alias B intact.
//
// Reachability note (RFC-144 §1.2): `ForEachNullOnEmptyQuantifier` has no SQL
// producer in Go today (outer joins use the materialized NLJ; FirstOrDefault uses
// a streaming Value, not a null-on-empty quantifier). This rule is Java-parity /
// latent-rule hygiene: IF a null-on-empty producer is wired later (or a synthetic
// path hits it) it is correct, and Go matches Java's rule set.
type EliminateNullOnEmptyRule struct {
	matcher matching.BindingMatcher
}

// NewEliminateNullOnEmptyRule constructs the rule.
func NewEliminateNullOnEmptyRule() *EliminateNullOnEmptyRule {
	return &EliminateNullOnEmptyRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("eliminate_null_on_empty"),
	}
}

func (r *EliminateNullOnEmptyRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *EliminateNullOnEmptyRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	quantifiers := sel.GetQuantifiers()
	preds := sel.GetPredicates()

	// No predicate ⇒ no quantifier qualifies (an empty WHERE is the TRUE
	// predicate, which is null-ACCEPTING). Java returns early in this case.
	if len(preds) == 0 {
		return
	}

	// Determine which null-on-empty aliases are eliminable. Collect ALIASES,
	// not quantifiers: two null-on-empty ForEach quantifiers over structurally
	// identical references compare equal under Quantifier equality and would be
	// conflated. A matched alias qualifies if SOME top-level predicate provably
	// rejects null at it.
	eligible := map[values.CorrelationIdentifier]struct{}{}
	for _, q := range quantifiers {
		if q.Kind() != expressions.QuantifierForEach || !q.IsNullOnEmpty() {
			continue
		}
		alias := q.GetAlias()
		for _, p := range preds {
			if rejectsNull(p, alias) {
				eligible[alias] = struct{}{}
				break
			}
		}
	}
	if len(eligible) == 0 {
		return
	}

	// Rebuild: each eligible null-on-empty quantifier becomes a plain ForEach
	// over the SAME reference, preserving its alias; sibling quantifiers (and
	// null-ACCEPTING null-on-empty quantifiers) are untouched.
	changed := false
	newQuantifiers := make([]expressions.Quantifier, len(quantifiers))
	for i, q := range quantifiers {
		if q.Kind() == expressions.QuantifierForEach && q.IsNullOnEmpty() {
			if _, ok := eligible[q.GetAlias()]; ok {
				newQuantifiers[i] = expressions.NamedForEachQuantifier(q.GetAlias(), q.GetRangesOver())
				changed = true
				continue
			}
		}
		newQuantifiers[i] = q
	}
	if !changed {
		return
	}

	// Yield a new SelectExpression with identical predicates + result value,
	// preserving the source aliases and join type.
	newSelect := expressions.NewSelectExpressionWithJoinType(
		sel.GetResultValue(),
		newQuantifiers,
		preds,
		sel.GetSourceAliases(),
		sel.GetJoinType(),
	)
	call.Yield(newSelect)
}

// rejectsNull reports whether predicate p provably REJECTS the null tuple that a
// null-on-empty ForEach quantifier injects at `alias`. Ports Java's
// ConstantPredicateFoldingUtil.rejectsNull: substitute a typed NullValue at the
// alias, constant-fold, and report true iff the folded outcome is FALSE or NULL
// (both filter the row out of a WHERE/ON/HAVING). A folded TRUE (accepts the
// null tuple) or an UNKNOWN (couldn't fold to a constant) does NOT reject.
func rejectsNull(p predicates.QueryPredicate, alias values.CorrelationIdentifier) bool {
	folded := foldPredicateAtNull(p, alias)
	cp, ok := folded.(*predicates.ConstantPredicate)
	if !ok {
		return false // UNKNOWN — could not fold to a constant.
	}
	// FALSE (contradiction) or NULL (UNKNOWN truth value) both reject the row.
	return cp.Value == predicates.TriFalse || cp.Value == predicates.TriUnknown
}

// foldPredicateAtNull substitutes a typed NullValue for every leaf correlated to
// `alias` in predicate p, then constant-folds the result. Ports Java's
// ConstantPredicateFoldingUtil.foldPredicateAtNull. After substitution, a
// null-strict Value over a NullValue child is collapsed to NullValue (the
// CollapseNullStrictValueOverNullValueRule port) so the comparison's operand
// becomes a NullValue that SimplifyPredicateValues can then fold via 3VL
// (`NULL = x` → UNKNOWN, `NULL IS NULL` → TRUE, …).
//
// Per Java's #4222 limitation, `NULL AND <non-constant>` is NOT folded (the
// simplifier does not assume NULL ≡ FALSE in a filter context). Such a mixed
// predicate stays non-constant → UNKNOWN → does not reject (conservative).
func foldPredicateAtNull(p predicates.QueryPredicate, alias values.CorrelationIdentifier) predicates.QueryPredicate {
	nullified := substituteNullAtAlias(p, alias)
	folded := predicates.SimplifyPredicateValues(nullified)
	return Simplify(folded, DefaultSimplifyRules())
}

// substituteNullAtAlias replaces every leaf QuantifiedObjectValue correlated to
// `alias` with a typed NullValue, then collapses any resulting null-strict Value
// over a NullValue child to NullValue (bottom-up). It is applied to every Value
// operand of the predicate tree. Returns a new predicate tree (CoW where
// unchanged).
func substituteNullAtAlias(p predicates.QueryPredicate, alias values.CorrelationIdentifier) predicates.QueryPredicate {
	subst := func(v values.Value) values.Value {
		return substituteAndCollapse(v, alias)
	}
	return mapPredicateValues(p, subst)
}

// substituteAndCollapse rewrites v bottom-up: a QuantifiedObjectValue over
// `alias` becomes a typed NullValue, and a null-strict Value that ends up with a
// NullValue child collapses to NullValue. Ports the combination of Java's
// translateCorrelations(alias → NullValue) + CollapseNullStrictValueOverNullValueRule.
func substituteAndCollapse(v values.Value, alias values.CorrelationIdentifier) values.Value {
	if v == nil {
		return nil
	}
	// Leaf QOV over the target alias → typed NullValue.
	if qov, ok := v.(*values.QuantifiedObjectValue); ok {
		if qov.Correlation == alias {
			return values.NewNullValue(qov.Type())
		}
		return v
	}
	children := v.Children()
	if len(children) == 0 {
		return v
	}
	// Recurse bottom-up.
	newChildren := make([]values.Value, len(children))
	changed := false
	for i, c := range children {
		nc := substituteAndCollapse(c, alias)
		newChildren[i] = nc
		if nc != c {
			changed = true
		}
	}
	rebuilt := v
	if changed {
		rebuilt = values.WithChildren(v, newChildren)
	}
	// CollapseNullStrictValueOverNullValueRule: a null-strict value with a
	// NullValue child yields NULL (preserving the result type).
	if isNullStrictValue(rebuilt) && hasNullValueChild(rebuilt) {
		return values.NewNullValue(rebuilt.Type())
	}
	return rebuilt
}

// mapPredicateValues applies the Value transform `fn` to every Value operand
// reachable inside ComparisonPredicate / ValuePredicate / ExistentialValuePredicate
// leaves, recursing through AND/OR/NOT connectives. Returns a new predicate tree.
// Mirrors the traversal shape of predicates.SimplifyPredicateValues.
func mapPredicateValues(p predicates.QueryPredicate, fn func(values.Value) values.Value) predicates.QueryPredicate {
	if p == nil {
		return nil
	}
	switch q := p.(type) {
	case *predicates.ComparisonPredicate:
		cmp := q.Comparison
		cmp.Operand = fn(q.Comparison.Operand)
		return &predicates.ComparisonPredicate{Operand: fn(q.Operand), Comparison: cmp}
	case *predicates.ValuePredicate:
		return predicates.NewValuePredicate(fn(q.Value))
	case *predicates.ExistentialValuePredicate:
		return predicates.MustNewExistentialValuePredicate(fn(q.Value), q.Comparison)
	case *predicates.AndPredicate:
		subs := make([]predicates.QueryPredicate, len(q.SubPredicates))
		for i, sp := range q.SubPredicates {
			subs[i] = mapPredicateValues(sp, fn)
		}
		return &predicates.AndPredicate{SubPredicates: subs}
	case *predicates.OrPredicate:
		subs := make([]predicates.QueryPredicate, len(q.SubPredicates))
		for i, sp := range q.SubPredicates {
			subs[i] = mapPredicateValues(sp, fn)
		}
		return &predicates.OrPredicate{SubPredicates: subs}
	case *predicates.NotPredicate:
		return &predicates.NotPredicate{Child: mapPredicateValues(q.Child, fn)}
	default:
		// Conservative passthrough: a predicate type not enumerated above is
		// returned unmapped. This is correct for the leaf/atom predicates that
		// carry no mappable Value children. WHEN ADDING A NEW PREDICATE TYPE
		// THAT CARRIES Value CHILDREN (a new compound or a value-bearing leaf),
		// add a case here so the null-substitution reaches it — otherwise the
		// rebase/null-substitution silently skips it.
		return p
	}
}

// isNullStrictValue reports whether v is one of the strictly-null-propagating
// Value classes (yields NULL if any child is NULL). Mirrors Java's
// CollapseNullStrictValueOverNullValueRule.VALUE_CLASSES — KEEP IN SYNC: when a
// new null-strict Value type is ported (Java adds one to that allowlist), add it
// here too, else this rule's null-folding silently under-approximates. As of
// Java 4.12 the set is ArithmeticValue, CastValue, FieldValue, NotValue,
// PromoteValue, SubscriptValue (enumerated below).
func isNullStrictValue(v values.Value) bool {
	switch v.(type) {
	case *values.ArithmeticValue, *values.CastValue, *values.FieldValue,
		*values.NotValue, *values.PromoteValue, *values.SubscriptValue:
		return true
	default:
		return false
	}
}

// hasNullValueChild reports whether any immediate child of v is a NullValue.
func hasNullValueChild(v values.Value) bool {
	for _, c := range v.Children() {
		if _, ok := c.(*values.NullValue); ok {
			return true
		}
	}
	return false
}

var _ ExpressionRule = (*EliminateNullOnEmptyRule)(nil)
