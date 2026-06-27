package properties

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// ReferencesAndDependencies captures the set of References in an
// expression subtree and the dependency edges between them. Matches
// Java's ReferencesAndDependenciesProperty result type
// (PartiallyOrderedSet<Reference>) in a simplified form.
type ReferencesAndDependencies struct {
	// Refs is the set of all References found in the subtree.
	Refs map[*expressions.Reference]struct{}
	// Dependencies maps a Reference to the set of References it
	// depends on (i.e. ref -> {child refs it quantifies over}).
	Dependencies map[*expressions.Reference]map[*expressions.Reference]struct{}
}

// EvaluateReferencesAndDependencies walks the expression tree and
// collects all References and their dependency relationships.
// Matches Java's ReferencesAndDependenciesProperty.evaluate.
//
// At each Reference node, the visitor adds the Reference itself to
// the set and records dependency edges from that Reference to each
// child Reference it quantifies over.
func EvaluateReferencesAndDependencies(expr expressions.RelationalExpression) ReferencesAndDependencies {
	result := ReferencesAndDependencies{
		Refs:         make(map[*expressions.Reference]struct{}),
		Dependencies: make(map[*expressions.Reference]map[*expressions.Reference]struct{}),
	}
	if expr == nil {
		return result
	}
	evaluateRefsRec(expr, &result)
	return result
}

// EvaluateReferencesAndDependenciesForRef evaluates starting from
// a Reference. Matches Java's evaluate(Reference) overload.
func EvaluateReferencesAndDependenciesForRef(ref *expressions.Reference) ReferencesAndDependencies {
	result := ReferencesAndDependencies{
		Refs:         make(map[*expressions.Reference]struct{}),
		Dependencies: make(map[*expressions.Reference]map[*expressions.Reference]struct{}),
	}
	if ref == nil {
		return result
	}
	evaluateRefsAtRef(ref, &result)
	return result
}

func evaluateRefsRec(expr expressions.RelationalExpression, result *ReferencesAndDependencies) {
	if expr == nil {
		return
	}
	for _, q := range expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		evaluateRefsAtRef(ref, result)
	}
}

func evaluateRefsAtRef(ref *expressions.Reference, result *ReferencesAndDependencies) {
	// Process all members first (recurse into children).
	for _, m := range ref.Members() {
		evaluateRefsRec(m, result)
	}

	// Add this ref to the set.
	result.Refs[ref] = struct{}{}

	// Record dependencies: this ref depends on each child ref
	// it quantifies over.
	for _, m := range ref.AllMembers() {
		for _, q := range m.GetQuantifiers() {
			childRef := q.GetRangesOver()
			if childRef == nil {
				continue
			}
			deps, ok := result.Dependencies[ref]
			if !ok {
				deps = make(map[*expressions.Reference]struct{})
				result.Dependencies[ref] = deps
			}
			deps[childRef] = struct{}{}
		}
	}
}
