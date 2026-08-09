package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// expressionRecordIdentityMatchesLogicalEquality proves that record-level
// distinctness for this exact physical expression also implies SQL row-value
// distinctness. PropDistinctRecords deliberately answers a different question:
// whether the same stored record can appear twice. Two physically distinct
// records can still be the same SQL row when a FLOAT/DOUBLE primary key differs
// only by raw NaN sign or payload, so callers must require both facts.
func expressionRecordIdentityMatchesLogicalEquality(expr expressions.RelationalExpression) bool {
	ph, ok := expr.(physicalPlanExpression)
	if !ok {
		return false
	}
	return planRecordIdentityMatchesLogicalEquality(ph.GetRecordQueryPlan())
}

func planRecordIdentityMatchesLogicalEquality(plan plans.RecordQueryPlan) bool {
	switch p := plan.(type) {
	case *plans.RecordQueryScanPlan:
		return len(p.GetRecordTypes()) == 1 &&
			properties.TupleKeyUniquenessMatchesLogicalEquality(
				p.GetKeyComponentTypes(), len(p.GetPrimaryKeyValues()),
			)
	case *plans.RecordQueryIndexPlan:
		return len(p.GetRecordTypes()) == 1 &&
			properties.TupleKeyUniquenessMatchesLogicalEquality(
				p.GetPrimaryKeyComponentTypes(), len(p.GetPKColumnNames()),
			)
	case *plans.RecordQueryCoveringIndexPlan:
		// Reconstructing a partial record from an entry neither creates nor
		// removes duplicates, so the record identity is the wrapped scan's.
		// The wrapper is a FIELD, so the fetch arm below cannot reach it, and
		// Fetch(Covering(IndexScan)) is what every index-backed access looks
		// like — without this arm the proof fails closed on all of them.
		return planRecordIdentityMatchesLogicalEquality(p.GetIndexPlan())
	case *plans.RecordQueryDistinctPlan:
		// This operator already applies the SQL row-value dedup key.
		return true
	case *plans.RecordQueryFirstOrDefaultPlan:
		// At most one output row is vacuously distinct.
		return true
	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryFetchFromPartialRecordPlan,
		*plans.RecordQueryDefaultOnEmptyPlan:
		return everyPhysicalMemberOfUnaryChildProves(
			plan, planRecordIdentityMatchesLogicalEquality,
		)
	case *plans.RecordQueryMapPlan:
		qov, ok := p.GetResultValue().(*values.QuantifiedObjectValue)
		if !ok || qov.Correlation != p.GetInnerQuantifier().GetAlias() {
			return false
		}
		return everyPhysicalMemberOfUnaryChildProves(
			plan, planRecordIdentityMatchesLogicalEquality,
		)
	default:
		// PK-distinct, unions/intersections, IN joins, projections, and DML
		// either deduplicate by physical identity or reshape/combine rows. They
		// need their own logical-value proof; record distinctness is not one.
		return false
	}
}

// everyPhysicalMemberOfUnaryChildProves follows the wrapper's actual memo edge,
// not GetChildren's current representative. A live child Reference can contain
// several physical alternatives and extraction may relink the wrapper to any
// winner later. The proof therefore has to hold for every eligible physical
// member; inspecting the first member would make correctness depend on insertion
// order.
func everyPhysicalMemberOfUnaryChildProves(
	plan plans.RecordQueryPlan,
	proof func(plans.RecordQueryPlan) bool,
) bool {
	qs := plan.GetQuantifiers()
	if len(qs) != 1 {
		return false
	}
	ref := qs[0].GetRangesOver()
	if ref == nil {
		return false
	}
	foundPhysical := false
	for _, member := range ref.AllMembers() {
		ph, ok := member.(physicalPlanExpression)
		if !ok {
			continue
		}
		foundPhysical = true
		if !proof(ph.GetRecordQueryPlan()) {
			return false
		}
	}
	return foundPhysical
}

// expressionStorageOrderingIsComplete proves the stronger fact needed before
// record distinctness can make an ordering strict: every coordinate that makes
// the storage key unique is present in the advertised ordering and agrees with
// logical equality.
//
// It READS that fact off the ordering the producer built, and does not compute
// it. An earlier revision re-derived it here from the plan's key component
// types, which made it a second property that had to be kept in agreement with
// the truncation the ordering derivation had actually performed — and the two
// could disagree. They did: a tail dropped wholesale at a signed-zero equality
// left the advertised ordering with no sorted coordinates at all, which this
// function called COMPLETE because the types were fine.
//
// What remains here is the part the ordering cannot answer: a memo group holds
// several physical alternatives and extraction may relink to any of them, so
// the claim has to hold for EVERY eligible member, not for the representative
// this walk happens to see first.
func expressionStorageOrderingIsComplete(expr expressions.RelationalExpression) bool {
	ph, ok := expr.(physicalPlanExpression)
	if !ok {
		return false
	}
	return planStorageOrderingIsComplete(ph.GetRecordQueryPlan())
}

func planStorageOrderingIsComplete(plan plans.RecordQueryPlan) bool {
	switch p := plan.(type) {
	case *plans.RecordQueryScanPlan:
		return p.HintRichOrdering().StorageKeyIsComplete()
	case *plans.RecordQueryIndexPlan:
		return p.HintRichOrdering().StorageKeyIsComplete()
	case *plans.RecordQueryCoveringIndexPlan:
		// HintRichOrdering delegates to the wrapped scan, whose physical range
		// and entry order the wrapper shares. Stated as its own arm rather than
		// left to the fetch arm below, which cannot reach a field.
		return p.HintRichOrdering().StorageKeyIsComplete()
	case *plans.RecordQueryFirstOrDefaultPlan:
		return true
	case *plans.RecordQueryFilterPlan,
		*plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQueryTypeFilterPlan,
		*plans.RecordQueryLimitPlan,
		*plans.RecordQueryFetchFromPartialRecordPlan,
		*plans.RecordQueryDefaultOnEmptyPlan:
		return everyPhysicalMemberOfUnaryChildProves(
			plan, planStorageOrderingIsComplete,
		)
	case *plans.RecordQueryMapPlan:
		qov, ok := p.GetResultValue().(*values.QuantifiedObjectValue)
		if !ok || qov.Correlation != p.GetInnerQuantifier().GetAlias() {
			return false
		}
		return everyPhysicalMemberOfUnaryChildProves(
			plan, planStorageOrderingIsComplete,
		)
	default:
		return false
	}
}
