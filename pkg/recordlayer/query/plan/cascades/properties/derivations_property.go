// Package properties — DerivationsProperty file.
//
// DerivationsProperty computes value derivation information for plan
// nodes — tracking which values flow where through the plan tree.
// A derivation is a Value-tree that is not correlated to anything.
// This is achieved by exhaustively inlining the producer of a
// correlation into the consumer.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.properties.DerivationsProperty`.
//
// In particular, a Value-tree that appears somewhere in a plan as
// part of a QueryPredicate or other expression is guaranteed to be
// part of a derivation which is not correlated.
package properties

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// Derivations captures the result and local value derivations
// collected by the evaluator. Mirrors Java's
// DerivationsProperty.Derivations inner class.
//
//   - ResultValues: the value trees describing what this plan node
//     produces as output rows. After inlining child correlations,
//     these are fully decorrelated.
//   - LocalValues: value trees that appear locally in this node
//     (predicates, comparison keys, etc.) but are not part of the
//     result. Also fully decorrelated after inlining.
type Derivations struct {
	ResultValues []values.Value
	LocalValues  []values.Value
}

// EmptyDerivations returns the singleton empty Derivations.
func EmptyDerivations() *Derivations {
	return &Derivations{}
}

// EvaluateDerivations walks the expression tree bottom-up and
// computes the Derivations for the given expression. Mirrors Java's
// DerivationsProperty.evaluate / DerivationsVisitor.
//
// The evaluator dispatches on the concrete physicalPlanExpression
// wrapper type (via the DerivationsEvaluable interface) or falls
// back to generic single-child / multi-child propagation.
func EvaluateDerivations(expr expressions.RelationalExpression) *Derivations {
	if expr == nil {
		return EmptyDerivations()
	}

	// Fast path: if the expression implements the optional
	// DerivationsEvaluable interface, delegate.
	if de, ok := expr.(DerivationsEvaluable); ok {
		return de.EvaluateDerivations()
	}

	// Generic fallback: single-child expressions propagate child
	// derivations unchanged; multi-child expressions union them.
	qs := expr.GetQuantifiers()
	switch len(qs) {
	case 0:
		return EmptyDerivations()
	case 1:
		return derivationsFromQuantifier(qs[0])
	default:
		return derivationsFromMultipleQuantifiers(qs)
	}
}

// DerivationsEvaluable is the optional interface physical-plan
// expression wrappers implement to compute their derivations.
// Mirrors the per-plan-type visit methods in Java's
// DerivationsVisitor.
type DerivationsEvaluable interface {
	EvaluateDerivations() *Derivations
}

// derivationsFromQuantifier recurses into a Quantifier's Reference
// and evaluates derivations for the first member.
func derivationsFromQuantifier(q expressions.Quantifier) *Derivations {
	ref := q.GetRangesOver()
	if ref == nil {
		return EmptyDerivations()
	}
	return evaluateForReference(ref)
}

// evaluateForReference evaluates derivations for the first member of
// a Reference. Mirrors Java's DerivationsVisitor.evaluateForReference.
func evaluateForReference(ref *expressions.Reference) *Derivations {
	expr := ref.Get()
	if expr == nil {
		return EmptyDerivations()
	}
	return EvaluateDerivations(expr)
}

// derivationsFromMultipleQuantifiers unions derivations from all
// quantifiers. Used for set operations (union, intersection) where
// each child contributes its own derivations.
func derivationsFromMultipleQuantifiers(qs []expressions.Quantifier) *Derivations {
	var resultVals []values.Value
	var localVals []values.Value
	for _, q := range qs {
		child := derivationsFromQuantifier(q)
		resultVals = append(resultVals, child.ResultValues...)
		localVals = append(localVals, child.LocalValues...)
	}
	return &Derivations{ResultValues: resultVals, LocalValues: localVals}
}

// DerivationsFromSingleChild evaluates derivations for a single-child
// expression by recursing into its only quantifier. Exported for use
// by physical plan wrapper implementations. Mirrors Java's
// DerivationsVisitor.derivationsFromSingleChild.
func DerivationsFromSingleChild(expr expressions.RelationalExpression) *Derivations {
	qs := expr.GetQuantifiers()
	if len(qs) != 1 {
		return EmptyDerivations()
	}
	return derivationsFromQuantifier(qs[0])
}

// DerivationsFromQuantifier is the exported version of
// derivationsFromQuantifier for use by wrapper implementations.
func DerivationsFromQuantifier(q expressions.Quantifier) *Derivations {
	return derivationsFromQuantifier(q)
}

// TranslateCorrelation replaces all QuantifiedObjectValue references
// matching `alias` with `replacement` in the given value tree.
// This is Go's equivalent of Java's:
//
//	TranslationMap.regularBuilder()
//	    .when(alias).then((src, leaf) -> replacement)
//	    .build()
//	value.translateCorrelations(translationMap, true)
//
// Returns the transformed value (original if no match).
func TranslateCorrelation(v values.Value, alias values.CorrelationIdentifier, replacement values.Value) values.Value {
	if v == nil {
		return nil
	}
	return values.ReplaceLeavesMaybe(v, func(leaf values.Value) values.Value {
		qov, ok := leaf.(*values.QuantifiedObjectValue)
		if !ok {
			return leaf
		}
		if qov.Correlation == alias {
			return replacement
		}
		return leaf
	})
}

// TranslateCorrelations replaces all QuantifiedObjectValue references
// matching any alias in the map with the corresponding replacement.
// Multiple-alias variant of TranslateCorrelation.
func TranslateCorrelations(v values.Value, aliasMap map[values.CorrelationIdentifier]values.Value) values.Value {
	if v == nil || len(aliasMap) == 0 {
		return v
	}
	return values.ReplaceLeavesMaybe(v, func(leaf values.Value) values.Value {
		qov, ok := leaf.(*values.QuantifiedObjectValue)
		if !ok {
			return leaf
		}
		if replacement, found := aliasMap[qov.Correlation]; found {
			return replacement
		}
		return leaf
	})
}

// IsCorrelatedTo reports whether a value tree references the given
// alias. Go equivalent of Java's Value.isCorrelatedTo(alias).
func IsCorrelatedTo(v values.Value, alias values.CorrelationIdentifier) bool {
	if v == nil {
		return false
	}
	corr := values.GetCorrelatedToOfValue(v)
	_, found := corr[alias]
	return found
}
