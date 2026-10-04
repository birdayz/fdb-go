package predicates

import (
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PartitionPredicates ports SelectExpression.partitionPredicates: opaque
// predicates precede stable, value-equivalent groups of residuals and ranges.
func PartitionPredicates(predicates []QueryPredicate) []QueryPredicate {
	type group struct {
		value      values.Value
		predicates []QueryPredicate
	}
	var result []QueryPredicate
	var groups []group
	buckets := make(map[uint64][]int)
	var flattened []QueryPredicate
	for _, predicate := range predicates {
		flattened = append(flattened, flattenPartitionPredicate(predicate, true)...)
	}
	for _, predicate := range flattened {
		value := partitionPredicateValue(predicate)
		if !partitionValuePresent(value) || !partitionComparisonValuesPresent(predicate) {
			result = append(result, predicate)
			continue
		}
		hash := values.SemanticHashCode(value)
		index := -1
		for _, candidate := range buckets[hash] {
			if values.SemanticEqualsUnderAliasMap(groups[candidate].value, value, nil) {
				index = candidate
				break
			}
		}
		if index < 0 {
			index = len(groups)
			buckets[hash] = append(buckets[hash], index)
			groups = append(groups, group{value: value})
		}
		duplicate := false
		for _, previous := range groups[index].predicates {
			if SemanticEqualsUnderAliasMap(previous, predicate, nil) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			groups[index].predicates = append(groups[index].predicates, predicate)
		}
	}
	for _, group := range groups {
		builder := NewRangeConstraintsBuilder()
		for _, predicate := range group.predicates {
			switch p := predicate.(type) {
			case *ComparisonPredicate:
				if !builder.AddComparisonMaybe(p.Comparison) {
					result = append(result, predicate)
				}
			case *PredicateWithValueAndRanges:
				if len(p.ranges) == 1 {
					builder.Add(p.ranges[0])
				} else {
					result = append(result, predicate)
				}
			default:
				result = append(result, predicate)
			}
		}
		if bounds := builder.Build(); bounds.IsConstraining() {
			result = append(result, NewPredicateWithValueAndRanges(group.value, []*RangeConstraints{bounds}))
		}
	}
	return result
}

// Malformed payloads must reach admission unchanged, not panic during hashing
// or disappear when an empty range is coalesced.
func partitionValuePresent(value values.Value) bool {
	if value == nil {
		return false
	}
	if reflected := reflect.ValueOf(value); reflected.Kind() == reflect.Pointer && reflected.IsNil() {
		return false
	}
	for _, child := range value.Children() {
		if !partitionValuePresent(child) {
			return false
		}
	}
	return true
}

func partitionComparisonValuesPresent(predicate QueryPredicate) bool {
	present := func(comparison Comparison) bool {
		return (comparison.Operand == nil || partitionValuePresent(comparison.Operand)) &&
			(comparison.QueryVector == nil || partitionValuePresent(comparison.QueryVector))
	}
	switch p := predicate.(type) {
	case *ComparisonPredicate:
		return present(p.Comparison)
	case *ExistentialValuePredicate:
		return present(p.Comparison)
	case *Placeholder:
		for _, comparison := range p.CompRange.GetComparisons() {
			if comparison == nil || !present(*comparison) {
				return false
			}
		}
	case *PredicateWithValueAndRanges:
		for _, bounds := range p.ranges {
			if bounds == nil {
				return false
			}
			for _, comparison := range bounds.GetComparisons() {
				if !present(comparison) {
					return false
				}
			}
		}
	}
	return true
}

func flattenPartitionPredicate(predicate QueryPredicate, conjunction bool) []QueryPredicate {
	if IsAtomic(predicate) {
		return []QueryPredicate{predicate}
	}
	var children []QueryPredicate
	var isAnd bool
	switch p := predicate.(type) {
	case *AndPredicate:
		if p == nil {
			return []QueryPredicate{predicate}
		}
		children, isAnd = p.SubPredicates, true
	case *OrPredicate:
		if p == nil {
			return []QueryPredicate{predicate}
		}
		children = p.SubPredicates
	default:
		return []QueryPredicate{predicate}
	}
	var flattened []QueryPredicate
	changed := false
	for _, child := range children {
		parts := flattenPartitionPredicate(child, isAnd)
		changed = changed || len(parts) != 1 || parts[0] != child
		flattened = append(flattened, parts...)
	}
	if isAnd == conjunction {
		return flattened
	}
	if len(flattened) == 1 {
		return flattened
	}
	if changed {
		if isAnd {
			predicate = NewAnd(flattened...)
		} else {
			predicate = NewOr(flattened...)
		}
	}
	return []QueryPredicate{predicate}
}

func partitionPredicateValue(predicate QueryPredicate) values.Value {
	switch p := predicate.(type) {
	case *ComparisonPredicate:
		if p != nil {
			return p.Operand
		}
	case *PredicateWithValueAndRanges:
		if p != nil {
			return p.value
		}
	case *Placeholder:
		if p != nil {
			return p.Value
		}
	case *ValuePredicate:
		if p != nil {
			return p.Value
		}
	case *ExistentialValuePredicate:
		if p != nil {
			return p.Value
		}
	}
	return nil
}
