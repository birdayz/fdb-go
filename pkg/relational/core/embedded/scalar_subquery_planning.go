package embedded

import (
	"fdb.dev/pkg/recordlayer"
	cascades "fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query"
)

// PlannedScalarSubquery pairs a scalar subquery's correlation alias with its
// planned physical plan. The executor pre-runs each plan
// (executor.EvaluateScalarSubquery) and binds the scalar result under the
// alias (EvaluationContext.WithScalarSubqueries) before running the outer
// plan — executing the outer plan without the binding fails loudly with
// values.UnboundScalarSubqueryError.
type PlannedScalarSubquery struct {
	Alias values.CorrelationIdentifier
	Plan  plans.RecordQueryPlan
}

// planScalarSubqueryPlans plans each translator-collected scalar subquery
// through its own Cascades pipeline — the ONE planning path for scalar
// subqueries, shared by the production generator (cascades_generator) and the
// plan harness (PlanRecordQueryWithSubqueries) so the two can never diverge.
func planScalarSubqueryPlans(scalarSubqueryPlans []query.ScalarSubqueryPlan, md *recordlayer.RecordMetaData, stats properties.StatisticsProvider) ([]PlannedScalarSubquery, error) {
	if len(scalarSubqueryPlans) == 0 {
		return nil, nil
	}
	rules := cascades.DefaultExpressionRules()
	rules = append(rules, cascades.RewritingRules()...)
	planCtx := buildCascadesPlanContext(md)
	type planExtractor interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	}
	out := make([]PlannedScalarSubquery, 0, len(scalarSubqueryPlans))
	for _, ssq := range scalarSubqueryPlans {
		// Pass md so the scalar subquery's own join legs can anchor (RFC-077 7.6);
		// nested scalar subqueries are not collected here, so any they contain are
		// dropped — matching the previous behavior (TranslateToCascades discards
		// them too).
		subRef, _, subTranslateErr := query.TranslateToCascadesWithError(ssq.Plan, md)
		if subTranslateErr != nil {
			// Surface the subquery's SPECIFIC translation error verbatim (e.g. the
			// numeric-operand gate's 0A000 "type mismatch" for MIN/MAX/SUM/AVG over a
			// non-numeric column) instead of the generic "could not plan scalar
			// subquery" — the bare-nil-ref path below cannot carry that code.
			return nil, subTranslateErr
		}
		if subRef == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Cascades planner could not plan scalar subquery")
		}
		subPlanner := cascades.NewPlanner(rules, planCtx).
			WithImplementationRules(cascades.DefaultImplementationRules()).
			WithPlanningExpressionRules(cascades.BatchAExpressionRules()).
			WithStatistics(stats).
			WithMaxTasks(100_000)
		subBest, _, subErr := subPlanner.Plan(subRef)
		if subErr != nil || subBest == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"Cascades planner could not plan scalar subquery")
		}
		subPh, ok := subBest.(planExtractor)
		if !ok {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"scalar subquery plan extraction failed")
		}
		subPlan := subPh.GetRecordQueryPlan()
		if subPlan == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"scalar subquery physical plan nil")
		}
		if err := cascades.ValidatePlanInvariants(subPlan); err != nil {
			return nil, api.NewError(api.ErrCodeInternalError, "malformed scalar-subquery plan: "+err.Error())
		}
		out = append(out, PlannedScalarSubquery{
			Alias: ssq.Alias,
			Plan:  subPlan,
		})
	}
	return out, nil
}
