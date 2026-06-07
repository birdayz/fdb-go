package predicates

import (
	"fmt"
	"strings"
)

// DatabaseObjectDependenciesPredicate is a leaf predicate that
// captures database object dependencies (index names, record type
// names) that a plan depends on. Used for plan cache invalidation
// when indexes are dropped or rebuilt.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.predicates.DatabaseObjectDependenciesPredicate
//
// At eval time in Java, this checks that all referenced indexes
// still exist, have the same lastModifiedVersion, and are readable.
// In Go, the plan cache is not yet ported, so Eval returns TriTrue
// unconditionally. The type exists for structural conformance.
type DatabaseObjectDependenciesPredicate struct {
	// UsedIndexes is the set of indexes this plan depends on,
	// each carrying its name and the lastModifiedVersion at plan
	// creation time.
	UsedIndexes []UsedIndex
}

// UsedIndex captures the name and lastModifiedVersion of an index
// that a plan depends on. Ports Java's
// DatabaseObjectDependenciesPredicate.UsedIndex.
type UsedIndex struct {
	// Name is the index name.
	Name string
	// LastModifiedVersion is the metadata version at which the
	// index was last modified, captured at plan creation time.
	LastModifiedVersion int
}

// NewDatabaseObjectDependenciesPredicate constructs the predicate
// from the given set of used indexes.
func NewDatabaseObjectDependenciesPredicate(indexes []UsedIndex) *DatabaseObjectDependenciesPredicate {
	// Defensive copy.
	cp := make([]UsedIndex, len(indexes))
	copy(cp, indexes)
	return &DatabaseObjectDependenciesPredicate{UsedIndexes: cp}
}

// Children returns nil -- this is a leaf predicate.
func (*DatabaseObjectDependenciesPredicate) Children() []QueryPredicate {
	return []QueryPredicate{}
}

// GetCorrelatedTo returns the empty set — database object dependency
// predicates reference no quantifier aliases.
func (*DatabaseObjectDependenciesPredicate) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{}
}

// Eval is the error-returning twin (RFC-091). Never fails.
func (*DatabaseObjectDependenciesPredicate) Eval(_ any) (TriBool, error) {
	return TriTrue, nil
}

// Explain renders the predicate in a human-readable form matching
// Java's explain output: databaseObjectDependencies(<index names>).
func (p *DatabaseObjectDependenciesPredicate) Explain() string {
	if len(p.UsedIndexes) == 0 {
		return "databaseObjectDependencies()"
	}
	names := make([]string, 0, len(p.UsedIndexes))
	for _, idx := range p.UsedIndexes {
		names = append(names, fmt.Sprintf("%s@v%d", idx.Name, idx.LastModifiedVersion))
	}
	return fmt.Sprintf("databaseObjectDependencies(%s)", strings.Join(names, ", "))
}
