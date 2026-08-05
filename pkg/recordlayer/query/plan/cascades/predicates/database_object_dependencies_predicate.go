package predicates

import (
	"fmt"
	"strings"
)

// DatabaseObjectDependenciesPredicate is a leaf predicate carrying the indexes
// a plan depends on. It answers one question: may this plan still be executed
// against the store in front of it?
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.predicates.DatabaseObjectDependenciesPredicate.
// Java attaches it to every planned query as the CONTINUATION plan constraint
// (QueryPlan.java:667,726-735) and re-evaluates it on every execution, so a
// resumed page is gated too, not only a first execution.
//
// A dependency is any index whose availability the plan's CORRECTNESS rests on:
// the indexes it scans, and any index whose metadata property licensed a
// transformation without being scanned. The second kind is why this cannot be
// derived from the plan tree alone at evaluation time.
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

// IndexAvailability answers, for one live record store, the two questions a
// plan's index dependency turns on. Eval's context is opaque by interface
// (predicates.go:229-237), so this is how a caller that HAS a store supplies
// one — the record-store types live in a package this one cannot import.
//
// present=false means the store's metadata does not name the index at all,
// which is Java's `!recordMetaData.hasIndex` leg
// (DatabaseObjectDependenciesPredicate.java:91-93).
type IndexAvailability interface {
	// IndexLastModifiedVersion reports the index's current
	// lastModifiedVersion, and whether the store's metadata names it.
	IndexLastModifiedVersion(name string) (version int, present bool)
	// IsIndexReadable reports whether the index is strictly READABLE —
	// Java's isReadable, which excludes READABLE_UNIQUE_PENDING even though
	// it is scannable.
	IsIndexReadable(name string) bool
}

// Eval reports whether this plan can still be executed against the store
// described by evalCtx. Port of Java's eval
// (DatabaseObjectDependenciesPredicate.java:87-105): every used index must
// still exist, carry the same lastModifiedVersion, and be readable.
//
// evalCtx MUST implement IndexAvailability. Java takes a non-null store and
// dereferences it (`Objects.requireNonNull(store)`), so a context that cannot
// answer is a programming error, not a case to be lenient about: returning
// TriTrue there would silently pass every dependency check ever made through
// the wrong call site. An asserted bridge, never a quiet fallback.
//
// A predicate with no dependencies is vacuously TriTrue and needs no context —
// that is a real absence of dependencies, not an inability to check them.
func (p *DatabaseObjectDependenciesPredicate) Eval(evalCtx any) (TriBool, error) {
	if len(p.UsedIndexes) == 0 {
		return TriTrue, nil
	}
	avail, ok := evalCtx.(IndexAvailability)
	if !ok {
		return TriUnknown, fmt.Errorf(
			"databaseObjectDependencies: evaluation context %T cannot answer index "+
				"availability; it must implement predicates.IndexAvailability", evalCtx)
	}
	for _, used := range p.UsedIndexes {
		version, present := avail.IndexLastModifiedVersion(used.Name)
		if !present || version != used.LastModifiedVersion {
			return TriFalse, nil
		}
		if !avail.IsIndexReadable(used.Name) {
			return TriFalse, nil
		}
	}
	return TriTrue, nil
}

// UnavailableDependency returns the first used index that fails against avail,
// and why — the diagnostic Eval's TriFalse deliberately does not carry, since a
// predicate's job is a truth value. ok=false when every dependency holds.
func (p *DatabaseObjectDependenciesPredicate) UnavailableDependency(
	avail IndexAvailability,
) (name string, reason string, ok bool) {
	if avail == nil {
		return "", "", false
	}
	for _, used := range p.UsedIndexes {
		version, present := avail.IndexLastModifiedVersion(used.Name)
		if !present {
			return used.Name, "it is no longer in the store's metadata", true
		}
		if version != used.LastModifiedVersion {
			return used.Name, "it has been redefined", true
		}
		if !avail.IsIndexReadable(used.Name) {
			return used.Name, "it is no longer readable", true
		}
	}
	return "", "", false
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
