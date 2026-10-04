package cascades

import (
	"slices"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// DefaultMaxNumConjuncts bounds fixed-factor subset enumeration. Above it,
// Java considers only the full DNF (the empty fixed subset).
const DefaultMaxNumConjuncts = 9

// PredicateToLogicalUnionRule enumerates primary-key-distinct unions with
// fixed factors made atomic so each leg cannot recursively expand them.
type PredicateToLogicalUnionRule struct {
	matcher matching.BindingMatcher
}

// NewPredicateToLogicalUnionRule constructs the rule.
func NewPredicateToLogicalUnionRule() *PredicateToLogicalUnionRule {
	return &PredicateToLogicalUnionRule{
		matcher: NewExpressionMatcher[*expressions.SelectExpression]("predicate_to_logical_union"),
	}
}

func (r *PredicateToLogicalUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *PredicateToLogicalUnionRule) OnMatch(call *ExpressionRuleCall) {
	sel := matching.Get[*expressions.SelectExpression](call.Bindings, r.matcher)
	explorePredicateUnion(call, sel, partiallyMatchedOrPredicates(call.Reference, sel))
}

func explorePredicateUnion(call *ExpressionRuleCall, sel *expressions.SelectExpression, matchedOrs map[predicates.QueryPredicate]struct{}) {
	preds := sel.GetPredicates()
	if len(preds) == 0 {
		return
	}

	quantifiers := sel.GetQuantifiers()
	// Every DNF leg is rebuilt with a plain ForEach. Duplicating a scalar edge
	// would both erase StrictSingle and change how often its cardinality check
	// runs, so the carrier is an unconditional barrier here.
	if hasStrictSingleQuantifier(quantifiers) {
		return
	}

	// Guard: exactly 1 ForEach quantifier.
	// Mirrors Java's check on ownedForEachAliases.size() != 1.
	var forEachQuantifiers []expressions.Quantifier
	for _, q := range quantifiers {
		switch q.Kind() {
		case expressions.QuantifierForEach:
			forEachQuantifiers = append(forEachQuantifiers, q)
		case expressions.QuantifierExistential:
			// Existentials do not change cardinality by themselves. Each leg
			// retains only those referenced by its predicates, as in Java.
			if _, projected := values.GetCorrelatedToOfValue(sel.GetResultValue())[q.GetAlias()]; projected {
				return // the outer projection cannot read a leg-local binding
			}
		default:
			return
		}
	}
	if len(forEachQuantifiers) != 1 {
		return
	}

	nonTrivial := make(map[predicates.QueryPredicate]int)
	for _, predicate := range preds {
		if !predicates.IsAtomic(predicate) && len(predicate.Children()) > 0 {
			if _, exists := nonTrivial[predicate]; !exists {
				nonTrivial[predicate] = len(nonTrivial)
			}
		}
	}
	if len(nonTrivial) == 0 {
		return
	}
	combinations := 1
	if len(nonTrivial) <= DefaultMaxNumConjuncts {
		combinations = (1 << len(nonTrivial)) - 1
	}
	legs, err := newPredicateUnionLegs(nonTrivial, combinations > 1)
	if err != nil {
		call.Fail(err)
		return
	}
	simplifier := newPredicateUnionSimplifier(preds...)
	for fixedMask := 0; fixedMask < combinations; fixedMask++ {
		if call.CancellationErr() != nil || call.Err() != nil {
			return
		}
		var fixed []int
		var expanded []predicates.QueryPredicate
		for _, predicate := range preds {
			ordinal, nonLeaf := nonTrivial[predicate]
			if nonLeaf && fixedMask&(1<<ordinal) != 0 {
				fixed = append(fixed, ordinal)
			} else {
				expanded = append(expanded, predicate)
			}
		}
		eligible := true
		for _, predicate := range expanded {
			if _, isOr := predicate.(*predicates.OrPredicate); isOr {
				if _, matched := matchedOrs[predicate]; !matched {
					eligible = false
					break
				}
			}
		}
		if !eligible {
			continue
		}
		terms, err := simplifier.terms(expanded)
		if err != nil {
			call.Fail(err)
			return
		}
		if len(terms) == 0 {
			continue
		}
		yieldPredicateUnion(call, sel, quantifiers, forEachQuantifiers[0], legs, fixed, terms)
	}
}

// predicateUnionLegs builds each leg once per rule call. A fixed factor sharing
// a literal with the leg's term is implied by it (the absorption Java's DNF
// simplification already applies to expanded factors), so a leg is its term
// plus only the fixed factors the term does not imply. That leg is the same
// for every fixed subset yielding the term, so all unions share one leg group
// instead of each subset planning its own syntactic variant.
type predicateUnionLegs struct {
	factors []predicateUnionFactor
	built   map[uint64][]predicateUnionLeg
}

type predicateUnionFactor struct {
	predicate predicates.QueryPredicate
	// Disjuncts in the form the DNF simplification gives term literals.
	literals []predicates.QueryPredicate
}

type predicateUnionLeg struct {
	term  predicates.QueryPredicate
	fixed []int
	ref   *expressions.Reference
}

func newPredicateUnionLegs(nonTrivial map[predicates.QueryPredicate]int, fixable bool) (*predicateUnionLegs, error) {
	legs := &predicateUnionLegs{
		factors: make([]predicateUnionFactor, len(nonTrivial)),
		built:   make(map[uint64][]predicateUnionLeg),
	}
	for factor, ordinal := range nonTrivial {
		legs.factors[ordinal].predicate = factor
		if !fixable {
			continue
		}
		normalized, err := Simplify(factor, queryPredicateSimplificationRules())
		if err != nil {
			return nil, err
		}
		legs.factors[ordinal].literals = predicateUnionConnectiveChildren[*predicates.OrPredicate](normalized)
	}
	return legs, nil
}

// predicateUnionConnectiveChildren flattens a non-atomic connective of type T.
func predicateUnionConnectiveChildren[T *predicates.AndPredicate | *predicates.OrPredicate](predicate predicates.QueryPredicate) []predicates.QueryPredicate {
	connective, ok := predicate.(T)
	if !ok || predicates.IsAtomic(predicate) {
		return []predicates.QueryPredicate{predicate}
	}
	var children []predicates.QueryPredicate
	for _, child := range predicates.QueryPredicate(connective).Children() {
		children = append(children, predicateUnionConnectiveChildren[T](child)...)
	}
	return children
}

// unimplied returns the fixed factors a term does not imply, in factor order.
func (l *predicateUnionLegs) unimplied(term predicates.QueryPredicate, fixed []int) []int {
	conjuncts := predicateUnionConnectiveChildren[*predicates.AndPredicate](term)
	var live []int
	for _, ordinal := range fixed {
		implied := false
		for _, literal := range l.factors[ordinal].literals {
			if slices.ContainsFunc(conjuncts, func(conjunct predicates.QueryPredicate) bool {
				return predicates.SemanticEqualsUnderAliasMap(literal, conjunct, nil)
			}) {
				implied = true
				break
			}
		}
		if !implied {
			live = append(live, ordinal)
		}
	}
	return live
}

func (l *predicateUnionLegs) lookup(term predicates.QueryPredicate, fixed []int) *expressions.Reference {
	for _, leg := range l.built[predicates.SemanticHashCode(term)] {
		if slices.Equal(leg.fixed, fixed) && predicates.SemanticEqualsUnderAliasMap(leg.term, term, nil) {
			return leg.ref
		}
	}
	return nil
}

func (l *predicateUnionLegs) remember(term predicates.QueryPredicate, fixed []int, ref *expressions.Reference) {
	hash := predicates.SemanticHashCode(term)
	l.built[hash] = append(l.built[hash], predicateUnionLeg{term: term, fixed: fixed, ref: ref})
}

func yieldPredicateUnion(call *ExpressionRuleCall, sel *expressions.SelectExpression, quantifiers []expressions.Quantifier, onlyForEachQ expressions.Quantifier, legs *predicateUnionLegs, fixed []int, dnfTerms []predicates.QueryPredicate) {
	lowerResultValue, err := onlyForEachQ.RequireFlowedObjectValue()
	if err != nil {
		call.Fail(err)
		return
	}

	// Check if the result value is "simple" — a QuantifiedObjectValue
	// referencing the single ForEach alias. If so, the outer wrapping
	// SelectExpression is unnecessary.
	resultValue := sel.GetResultValue()
	isSimpleResultValue := false
	if qov, ok := values.AsQuantifiedObjectValue(resultValue); ok {
		if qov.Correlation() == onlyForEachQ.GetAlias() {
			isSimpleResultValue = true
		}
	}

	var legRefs []*expressions.Reference
	for _, dnfTerm := range dnfTerms {
		live := legs.unimplied(dnfTerm, fixed)
		if ref := legs.lookup(dnfTerm, live); ref != nil {
			legRefs = append(legRefs, ref)
			continue
		}
		legPreds := make([]predicates.QueryPredicate, 0, len(live)+1)
		for _, ordinal := range live {
			legPreds = append(legPreds, predicates.WithAtomicity(legs.factors[ordinal].predicate, true))
		}
		legPreds = append(legPreds, dnfTerm)

		// Rebuild the ForEach quantifier pointing at the same inner Reference.
		legForEach := expressions.NamedForEachQuantifier(
			onlyForEachQ.GetAlias(),
			onlyForEachQ.GetRangesOver(),
		)

		legQuantifiers := []expressions.Quantifier{legForEach}
		for _, q := range quantifiers {
			if q.Kind() != expressions.QuantifierExistential {
				continue
			}
			for _, predicate := range legPreds {
				if _, needed := predicates.GetCorrelatedToOfPredicate(predicate)[q.GetAlias()]; needed {
					legQuantifiers = append(legQuantifiers, expressions.NamedExistentialQuantifier(q.GetAlias(), q.GetRangesOver()))
					break
				}
			}
		}

		legSelect, err := expressions.NewSelectExpression(
			lowerResultValue,
			legQuantifiers,
			legPreds,
		)
		if err != nil {
			call.Fail(err)
			return
		}
		legSelectRef := call.MemoizeExpression(legSelect)

		legUnique, err := expressions.NewLogicalUniqueExpression(
			expressions.ForEachQuantifier(legSelectRef),
		)
		if err != nil {
			call.Fail(err)
			return
		}
		legUniqueRef := call.MemoizeExpression(legUnique)
		legs.remember(dnfTerm, live, legUniqueRef)

		legRefs = append(legRefs, legUniqueRef)
	}

	// Build the union over all legs.
	unionQuantifiers := make([]expressions.Quantifier, len(legRefs))
	for i, ref := range legRefs {
		unionQuantifiers[i] = expressions.ForEachQuantifier(ref)
	}
	unionExpr, err := expressions.NewLogicalUnionExpression(unionQuantifiers)
	if err != nil {
		call.Fail(err)
		return
	}
	unionRef := call.MemoizeExpression(unionExpr)

	// Dedup across legs, keyed on the PRIMARY KEY.
	//
	// Java writes this as `new LogicalDistinctExpression(...)`, and a port that
	// carried the name across would land on Go's LogicalDistinctExpression —
	// which is the wrong node. The two node sets do not line up:
	//
	//	Java LogicalDistinct = primary-key dedup (ImplementDistinctRule builds
	//	                       RecordQueryUnorderedPrimaryKeyDistinctPlan)
	//	Go   LogicalDistinct = full-ROW dedup, carrying SELECT DISTINCT, which
	//	                       Java's Cascades has no node for at all
	//	Go   LogicalUnique   = primary-key dedup
	//
	// The key has to be the primary key because the legs are separate access
	// paths over one table: a record satisfying two DNF terms is produced by two
	// legs, and when those legs are covering scans of DIFFERENT indexes the two
	// rows for that one record DIFFER — (a, pk) from one, (b, a, pk) from the
	// other. A full-row dedup collapses neither, and the record is returned once
	// per matching leg.
	//
	// REQUIRED mode, because a union of legs is never already distinct and the
	// absorbable mode only elides: ImplementUniqueRule yields no plan at all for
	// an input it cannot prove distinct, which would silently withdraw the
	// access path rather than dedup it.
	distinctExpr, err := expressions.NewRequiredLogicalUniqueExpression(
		expressions.ForEachQuantifier(unionRef),
	)
	if err != nil {
		call.Fail(err)
		return
	}

	if isSimpleResultValue {
		// Simple result value: Distinct is the final expression.
		call.Yield(distinctExpr)
	} else {
		// Non-simple result value: wrap in an outer SelectExpression
		// that projects the original result value.
		distinctRef := call.MemoizeExpression(distinctExpr)

		// Reuse the ForEach alias from the original quantifier so the
		// result value's correlation references still resolve.
		outerQuantifier := expressions.NamedForEachQuantifier(
			onlyForEachQ.GetAlias(),
			distinctRef,
		)
		outerSelect, err := expressions.NewSelectExpression(
			resultValue,
			[]expressions.Quantifier{outerQuantifier},
			nil, // no predicates on the outer select
		)
		if err != nil {
			call.Fail(err)
			return
		}
		call.Yield(outerSelect)
	}
}

// The context-free DNF rules share rewrites of original input subtrees only.
// Root rewrites and generated terms are not reusable across fixed-factor subsets.
type predicateUnionSimplifier struct {
	rules        []CascadesRule
	childResults map[predicates.QueryPredicate]predicates.QueryPredicate
}

func newPredicateUnionSimplifier(factors ...predicates.QueryPredicate) *predicateUnionSimplifier {
	s := &predicateUnionSimplifier{
		rules:        append([]CascadesRule{newPredicateDNFRule()}, queryPredicateSimplificationRules()...),
		childResults: make(map[predicates.QueryPredicate]predicates.QueryPredicate),
	}
	var register func(predicates.QueryPredicate)
	register = func(predicate predicates.QueryPredicate) {
		if predicate == nil {
			return
		}
		if _, known := s.childResults[predicate]; known {
			return
		}
		s.childResults[predicate] = nil
		for _, child := range predicate.Children() {
			register(child)
		}
	}
	for _, factor := range factors {
		register(factor)
	}
	return s
}

func (s *predicateUnionSimplifier) simplify(predicate predicates.QueryPredicate) (predicates.QueryPredicate, error) {
	return simplifyWithReExploration(predicate, s.rules, true, s.childResults)
}

func (s *predicateUnionSimplifier) terms(factors []predicates.QueryPredicate) ([]predicates.QueryPredicate, error) {
	dnf, err := s.simplify(buildAnd(factors))
	if err != nil {
		return nil, err
	}
	if disjunction, ok := dnf.(*predicates.OrPredicate); ok && !predicates.IsAtomic(disjunction) {
		return disjunction.SubPredicates, nil
	}
	return nil, nil
}

var _ ExpressionRule = (*PredicateToLogicalUnionRule)(nil)

func partiallyMatchedOrPredicates(ref *expressions.Reference, expression expressions.RelationalExpression) map[predicates.QueryPredicate]struct{} {
	matched := make(map[predicates.QueryPredicate]struct{})
	for _, partialMatch := range GetPartialMatchesForExpression(ref, expression) {
		info := partialMatch.GetMatchInfo().GetRegularMatchInfo()
		for _, entry := range info.GetPredicateMap().Entries() {
			mapping := entry.Mapping
			if mapping.GetMappingKind() == MappingOrTermImpliesCandidate {
				matched[mapping.GetOriginalQueryPredicate()] = struct{}{}
			}
		}
	}
	return matched
}

func isPredicateUnionRule(rule ExpressionRule) bool {
	switch rule.(type) {
	case *PredicateToLogicalUnionRule, *FilterToLogicalUnionRule:
		return true
	default:
		return false
	}
}
