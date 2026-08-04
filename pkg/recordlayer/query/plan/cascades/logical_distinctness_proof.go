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
// the storage key unique is present in the logical ordering topology and agrees
// with logical equality. For an index this includes both the index key and its
// appended primary-key suffix. Merely proving the base PK safe is insufficient:
// a FLOAT/DOUBLE index component can truncate the advertised ordering before a
// later safe PK suffix, leaving duplicate values in the advertised prefix.
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
		return len(p.GetRecordTypes()) == 1 &&
			properties.TupleKeyUniquenessMatchesLogicalEquality(
				p.GetKeyComponentTypes(), len(p.GetPrimaryKeyValues()),
			)
	case *plans.RecordQueryIndexPlan:
		return len(p.GetRecordTypes()) == 1 &&
			properties.TupleKeyUniquenessMatchesLogicalEquality(
				p.GetKeyComponentTypes(), len(p.GetColumnNames()),
			) &&
			properties.TupleKeyUniquenessMatchesLogicalEquality(
				p.GetPrimaryKeyComponentTypes(), len(p.GetPKColumnNames()),
			)
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
