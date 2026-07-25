package embedded

import (
	"errors"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
)

// plannerUnableToPlanMessage is the wording Java's
// CascadesPlanner.resultOrFail attaches to UnableToPlanException when no final
// expression survives planning. Keeping it verbatim is what lets the
// cross-engine corpus classify a both-engines-reject shape as agreement rather
// than message drift.
const plannerUnableToPlanMessage = "Cascades planner could not plan query"

// unableToPlanScalarSubqueryMessage is the same verdict for a scalar subquery
// planned through its own pipeline. It names the subquery rather than the
// statement so an unplannable `(SELECT ...)` operand is not reported as if the
// outer query were at fault.
const unableToPlanScalarSubqueryMessage = "Cascades planner could not plan scalar subquery"

// plannerInternalErrorMessage is what a planner failure the classifier does not
// recognise tells the user. It claims nothing about their query, because an
// unrecognised failure is evidence about the engine, not about the SQL.
const plannerInternalErrorMessage = "internal error while planning query"

// plannerBudgetExceededMessage is the user-facing verdict for an exhausted
// planning budget. It deliberately does NOT reuse the cap sentinel's own text:
// that names a Go identifier (MaxTasks) rather than a user concept, and
// repeating it as the message would render the same sentence twice, since
// api.Error.Error() appends the cause. The sentinel stays in the cause for
// errors.Is and logs; the numbers ride in Context.
const plannerBudgetExceededMessage = "query is too complex to plan; " +
	"the planner exhausted its search budget before finding a plan"

// Curated messages for the engine-bug sentinels. Like the budget message these
// deliberately do NOT reuse the sentinel's own text: repeating it as the
// message renders the same sentence twice, since api.Error.Error() appends the
// cause. Each states plainly that the engine is at fault, without naming a Go
// identifier, and stays distinct from the others so a bug report can be routed.
// The sentinel itself rides in the cause for errors.Is and logs.
const (
	plannerNonConvergenceMessage = "internal error: the planner did not converge while exploring the query"
	plannerReusedMessage         = "internal error: a planner was reused for a second planning attempt"
	plannerMissingContextMessage = "internal error: the planner was invoked without a context"
)

// Planner failure families. These are the only detail a user or an operator
// gets for an engine-internal planner failure, so they must be specific enough
// to route a bug report and free of anything that names engine internals.
const (
	familyInvariantViolation = "planner invariant violation"
	familyPlanRebuild        = "plan rebuild failure"
	familyUnclassified       = "unclassified planner failure"
)

// plannerFailureFamily names the family of an engine-internal planner failure.
//
// It reads the error's TYPE, tagged at the chokepoint that minted it, rather
// than matching on message text: the messages are exactly what must not be
// trusted or shown, and a text match would silently reclassify itself the first
// time someone rewords an invariant.
func plannerFailureFamily(planErr error) string {
	var invariant *cascades.PlannerInvariantViolationError
	if errors.As(planErr, &invariant) {
		return familyInvariantViolation
	}
	var rebuild *cascades.PlanRebuildError
	if errors.As(planErr, &rebuild) {
		return familyPlanRebuild
	}
	return familyUnclassified
}

// withheldPlannerDetail carries an engine-internal planner failure without
// rendering it.
//
// api.Error.Error() appends its cause unconditionally, so attaching a raw
// invariant violation would print Go internals straight into a database/sql
// caller's error string — the yield-invariant messages format the offending
// expression with %T, which is how "*expressions.LogicalFilterExpression
// quantifier 1 ranges over no reference" ends up in a user's terminal. Wrapping
// the cause in a type whose Error() is a fixed phrase keeps the chain intact
// for errors.Is and errors.As, and for anything that deliberately unwraps to
// log it, while the rendered text stays free of engine internals.
//
// Only the unrecognised default is withheld this way. The classified failures
// carry hand-written messages that name planner concepts rather than Go types,
// and those are worth showing: they are the difference between a user learning
// which budget they exhausted and learning nothing.
//
// What IS rendered is the failure's family. Withholding every message would
// collapse 39-plus distinct engine bugs into one indistinguishable string, so a
// bug report could not even say which subsystem failed. The family carries that
// much and no more.
type withheldPlannerDetail struct {
	family string
	cause  error
}

// Error returns the family and nothing else — never the wrapped cause's text.
func (e *withheldPlannerDetail) Error() string {
	return e.family + " (detail withheld from the SQL surface)"
}

// Unwrap returns the withheld cause so errors.Is and errors.As still reach it.
func (e *withheldPlannerDetail) Unwrap() error { return e.cause }

// translatePlannerError classifies a non-nil error from
// cascades.Planner.PlanWithContext into the SQLSTATE a user sees. Every planner
// callsite that mints a user-facing error routes through here, so the failure
// classes cannot silently converge on one code again.
//
// The default is INTERNAL ERROR, and "this query cannot be planned" is an
// explicit arm. That polarity is deliberate and mirrors Java's
// ExceptionUtil.recordCoreToRelationalException, whose default is a code
// admitting ignorance with UnableToPlanException as the one explicit arm. A
// default of "unsupported query" would make an unproven claim about the user's
// SQL every time a new failure mode appears in the engine; a default of
// "internal error" says only what is actually known.
//
// unableToPlanMessage is the wording for the genuinely-unplannable arm only; it
// never overrides the message a classified failure carries itself.
//
// Returns nil for a nil error so a caller cannot mint a failure out of a
// successful plan.
func translatePlannerError(planErr error, unableToPlanMessage string) error {
	if planErr == nil {
		return nil
	}
	// Cancellation is the caller's own signal, not a verdict on the query.
	// Propagate it untouched so errors.Is(err, context.Canceled) still matches
	// above the SQL layer and a canceled statement is never reported as an
	// unsupported one.
	if isContextCancellation(planErr) {
		return planErr
	}

	// Planning budgets. Java's SQL layer never reaches these: its
	// PlannerConfiguration.buildRecordQueryPlannerConfiguration sets none of the
	// three cap setters, and every guard is gated on a positive bound that
	// defaults to 0 ("unbound"). So this is a Go-only condition, not a shared
	// surface to conform to, and it gets a Go-chosen class-54 code —
	// program-limit-exceeded is exactly "gave up, query too complex, retryable
	// after simplification". Java's own UNKNOWN is documented as "shouldn't be
	// used in general" and would say nothing about what went wrong.
	if errors.Is(planErr, cascades.ErrPlannerCapHit) ||
		errors.Is(planErr, cascades.ErrPlannerQueueCapHit) ||
		errors.Is(planErr, cascades.ErrPlannerRuleMatchCapHit) {
		return withBudgetContext(
			api.WrapError(api.ErrCodePlanComplexityLimitReached, plannerBudgetExceededMessage, planErr), planErr)
	}

	// The one genuinely user-facing planner verdict: the query asks for a
	// predicate only an index can evaluate, and no index serves it. Its payload
	// is derived from the user's own query text, so it is safe to surface, and
	// 0AF00 is the honest answer — this shape is unsupported.
	var indexOnly *cascades.UnplannableIndexOnlyResidualError
	if errors.As(planErr, &indexOnly) {
		return api.WrapError(api.ErrCodeUnsupportedQuery, unableToPlanMessage, planErr)
	}

	// Engine bugs with curated messages. The exploration round cap is a
	// rule-cycle detector (RFC-180 I2) and the two lifecycle sentinels are
	// misuse of the planning API; all three mean the engine is broken, never
	// that the user's query is unsupported. Each gets its own wording so the
	// three stay distinguishable in a report, and none repeats its sentinel.
	switch {
	case errors.Is(planErr, cascades.ErrPlannerRoundCapHit):
		return api.WrapError(api.ErrCodeInternalError, plannerNonConvergenceMessage, planErr)
	case errors.Is(planErr, cascades.ErrPlannerAlreadyUsed):
		return api.WrapError(api.ErrCodeInternalError, plannerReusedMessage, planErr)
	case errors.Is(planErr, cascades.ErrPlannerNilContext):
		return api.WrapError(api.ErrCodeInternalError, plannerMissingContextMessage, planErr)
	}

	// Everything else: memo yield-invariant violations, structural extraction
	// failures, and any failure mode added later. All are evidence of engine
	// state that should not exist, and none of them is a statement about the
	// user's SQL. The detail is withheld from the rendered message — only the
	// family is named — but stays reachable through Unwrap.
	return api.WrapError(api.ErrCodeInternalError, plannerInternalErrorMessage,
		&withheldPlannerDetail{family: plannerFailureFamily(planErr), cause: planErr})
}

// withBudgetContext attaches which budget was exhausted and how far the run
// got, under the same keys Java's addLogInfo uses on
// RecordQueryPlanComplexityException. A user should learn which limit they hit
// and by how much, not merely that some limit existed.
func withBudgetContext(out *api.Error, planErr error) *api.Error {
	var budget *cascades.PlannerBudgetExceededError
	if !errors.As(planErr, &budget) {
		return out
	}
	limitKey, observedKey := "max_task_count", "task_count"
	switch {
	case errors.Is(planErr, cascades.ErrPlannerQueueCapHit):
		limitKey, observedKey = "max_task_queue_size", "task_queue_size"
	case errors.Is(planErr, cascades.ErrPlannerRuleMatchCapHit):
		limitKey, observedKey = "max_rule_matches_count", "rule_matches_count"
	}
	return out.WithContext(limitKey, budget.Limit).WithContext(observedKey, budget.Observed)
}
