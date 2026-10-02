package expressions

import (
	"maps"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ExactReplica reports whether a and b are the same operator over the same
// input groups — the Cascades memo's duplicate key (Graefe 1995 §2: "an
// operator and the groups of its inputs"). Inputs compare by canonical group
// identity, never by content, so the test costs one node comparison.
//
// A quantifier alias is part of the operator only where something outside it
// can name the alias. Expressions that opt into alias-aware interning, and
// planner merge aliases anywhere, have no such consumer and may be renamed
// consistently; any other alias must agree. A rename may not change what an
// input group or an outer reference reads on either side.
func ExactReplica(a, b RelationalExpression) bool {
	if a == nil || b == nil {
		return false
	}
	if a == b {
		return true
	}
	if len(a.GetQuantifiers()) != len(b.GetQuantifiers()) || a.CanCorrelate() != b.CanCorrelate() {
		return false
	}
	renameAny := InternsAliasAware(a) && InternsAliasAware(b)
	pairable := func(left, right Quantifier) bool {
		return renameAny || left.GetAlias() == right.GetAlias() ||
			left.GetAlias().IsMergeAlias() && right.GetAlias().IsMergeAlias()
	}
	return maps.Equal(outerReads(a), outerReads(b)) &&
		matchQuantifierBindings(a, b, EmptyAliasMap(), referenceCorrelations, sameInputGroup, pairable)
}

// bindsMergeAlias reports whether e binds a planner merge alias, the one
// alias ExactReplica renames in any expression.
func bindsMergeAlias(e RelationalExpression) bool {
	for _, quantifier := range e.GetQuantifiers() {
		if quantifier.GetAlias().IsMergeAlias() {
			return true
		}
	}
	return false
}

func outerReads(e RelationalExpression) map[values.CorrelationIdentifier]struct{} {
	reads := maps.Clone(e.GetCorrelatedToWithoutChildren())
	for _, quantifier := range e.GetQuantifiers() {
		delete(reads, quantifier.GetAlias())
	}
	return reads
}

func referenceCorrelations(ref *Reference) map[values.CorrelationIdentifier]struct{} {
	return ref.GetCorrelatedTo()
}

func sameInputGroup(a, b *Reference, aliases *AliasMap) bool {
	a, b = canonicalReferenceReadOnly(a), canonicalReferenceReadOnly(b)
	if a == nil || a != b {
		return false
	}
	for alias := range a.GetCorrelatedTo() {
		if aliases.GetTargetOrDefault(alias, alias) != alias {
			return false
		}
		if source, renamed := aliases.GetSource(alias); renamed && source != alias {
			return false
		}
	}
	return true
}
