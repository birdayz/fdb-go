package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// coveredOrdinalSet is one record type's covering answer: the layout token of
// that type's row (RFC-197's ordinal DOMAIN) plus the ordinals of the columns
// the index can serve without a fetch.
//
// The index definition names its columns — that is its right, it is a metadata
// artifact whose currency is names. The name dies HERE, at the boundary: it is
// resolved against the record descriptor ONCE when the translate function is
// built, and match time compares domain-checked ordinals only (RFC-197 item 1).
type coveredOrdinalSet struct {
	domain   values.OrdinalDomain
	ordinals map[int]struct{}
	rowType  values.ExactTypeHandle
}

// buildCoveredOrdinalSets resolves a covered-column NAME set against each row
// layout the index serves, yielding one (domain, ordinal set) pair per layout.
//
// One set per RECORD TYPE, not one set for the index: a multi-record-type
// index has no single row layout, so a single ordinal set would be a claim
// about a layout that does not exist — ordinal 2 of type A and ordinal 2 of
// type B are different columns. Keying each set by its layout's domain token
// makes the match self-selecting: a value states which layout its ordinal
// indexes, and only the set for THAT layout can answer it. A layout that
// cannot be typed (UnknownType — what a multi-type index degrades to when
// per-type descriptors are unavailable) contributes nothing, so the site
// declines rather than guessing. A declined push is recoverable; a wrong
// ordinal is not.
//
// A covered name that does not resolve in a given layout is simply absent from
// that layout's set: it cannot be a covering read of a row that has no such
// column.
func buildCoveredOrdinalSets(rowTypes []values.Type, coveredColumns map[string]struct{}) []coveredOrdinalSet {
	if len(coveredColumns) == 0 {
		return nil
	}
	sets := make([]coveredOrdinalSet, 0, len(rowTypes))
	for _, rowType := range rowTypes {
		domain := values.OrdinalDomainOfType(rowType)
		if !domain.IsKnown() {
			continue
		}
		rt, ok := rowType.(*values.RecordType)
		if !ok {
			continue
		}
		exactRowType, err := values.SnapshotExactType(rt)
		if err != nil {
			continue
		}
		ordinals := make(map[int]struct{}, len(coveredColumns))
		for i, f := range rt.Fields {
			if _, covered := coveredColumns[strings.ToUpper(f.Name)]; covered {
				ordinals[i] = struct{}{}
			}
		}
		if len(ordinals) == 0 {
			continue
		}
		sets = append(sets, coveredOrdinalSet{domain: domain, ordinals: ordinals, rowType: exactRowType})
	}
	return sets
}

func pushCoveredOrdinalWithType(
	sets []coveredOrdinalSet,
	fv values.FieldValue,
) (int, values.OrdinalDomain, values.Type, bool) {
	for _, set := range sets {
		identity, ok := values.CorrelatedFieldIdentityIn(fv, set.domain)
		if !ok {
			continue
		}
		ord := identity.Ordinal
		if _, covered := set.ordinals[ord]; covered {
			return ord, set.domain, set.rowType.Type(), true
		}
		// The value's layout IS this set's layout and the ordinal is not
		// covered: a definite NO, not a reason to try another layout.
		return 0, values.OrdinalDomain{}, nil, false
	}
	return 0, values.OrdinalDomain{}, nil, false
}
