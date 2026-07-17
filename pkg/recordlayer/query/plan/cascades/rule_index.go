package cascades

import (
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
)

// matcherHaver is the sliver of ExpressionRule / ImplementationRule the
// rule index needs: every rule exposes its root pattern.
type matcherHaver interface {
	Matcher() matching.BindingMatcher
}

// ruleIndex buckets rules by their matcher's typed root operator so an
// expression is only offered rules that can possibly match it. Rules whose
// matcher declares no root (the any-matcher, custom multi-type matchers)
// go to the always bucket and are offered to every expression. Ports
// Java's AbstractRuleSet: ruleIndex Multimap keyed by getRootOperator,
// alwaysRules for Optional.empty(), and the per-class rules cache whose
// lists are indexed-rules-then-always in registration order.
//
// The key is reflect.Type — computed from the matcher's type parameter
// (matching.RootOperatorMatcher), never a hand-typed name string, so a
// typo cannot produce a rule that silently never fires.
type ruleIndex[R matcherHaver] struct {
	byRoot map[reflect.Type][]R
	always []R
	cache  map[reflect.Type][]R
}

func newRuleIndex[R matcherHaver](rules []R) *ruleIndex[R] {
	ix := &ruleIndex[R]{
		byRoot: make(map[reflect.Type][]R),
		cache:  make(map[reflect.Type][]R),
	}
	for _, r := range rules {
		if rm, ok := r.Matcher().(matching.RootOperatorMatcher); ok {
			if root := rm.RootOperator(); root != nil {
				ix.byRoot[root] = append(ix.byRoot[root], r)
				continue
			}
		}
		ix.always = append(ix.always, r)
	}
	return ix
}

// rulesFor returns the rules applicable to expr's concrete type: the
// root-indexed rules first, then the always rules, each in registration
// order (Java's rulesCache list shape). The returned slice is cached and
// must not be mutated.
func (ix *ruleIndex[R]) rulesFor(expr expressions.RelationalExpression) []R {
	t := reflect.TypeOf(expr)
	if cached, ok := ix.cache[t]; ok {
		return cached
	}
	rooted := ix.byRoot[t]
	var out []R
	if len(rooted) == 0 {
		out = ix.always
	} else {
		out = make([]R, 0, len(rooted)+len(ix.always))
		out = append(out, rooted...)
		out = append(out, ix.always...)
	}
	ix.cache[t] = out
	return out
}
