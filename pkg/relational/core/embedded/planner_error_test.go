package embedded

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
)

// TestTranslatePlannerError_SQLSTATEMap pins every classified arm. Before this
// mapping existed the SELECT callsite discarded the planner error outright, so
// a complexity cap, a rule-cycle divergence and a lifecycle misuse all reached
// the user as the single 0AF00 "could not plan query". The unprobed dimension
// was the failure CLASS: every existing test drove the genuinely-unplannable
// case, so the collapse of the other classes onto it was invisible.
func TestTranslatePlannerError_SQLSTATEMap(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		code api.ErrorCode
		// msg is the expected user-facing Message. The budget arm gets a
		// sentence a SQL user can act on; the engine-bug sentinels keep their
		// own curated wording, which names planner concepts rather than Go
		// types.
		msg string
	}{
		// Planning budgets. Java's SQL layer never enables these caps, so there
		// is no shared surface to conform to and no Java code to port: class 54
		// (program limit exceeded) is Go's own answer.
		{
			"maxTasks", cascades.ErrPlannerCapHit,
			api.ErrCodePlanComplexityLimitReached, plannerBudgetExceededMessage,
		},
		{
			"taskQueueSize", cascades.ErrPlannerQueueCapHit,
			api.ErrCodePlanComplexityLimitReached, plannerBudgetExceededMessage,
		},
		{
			"maxMatchesPerRuleCall", cascades.ErrPlannerRuleMatchCapHit,
			api.ErrCodePlanComplexityLimitReached, plannerBudgetExceededMessage,
		},

		// Engine bugs, not user load: never 0AF00. Each gets its own wording so
		// a bug report can tell a rule-cycle divergence from API misuse.
		{
			"explorationRoundCap", cascades.ErrPlannerRoundCapHit,
			api.ErrCodeInternalError, plannerNonConvergenceMessage,
		},
		{
			"plannerReused", cascades.ErrPlannerAlreadyUsed,
			api.ErrCodeInternalError, plannerReusedMessage,
		},
		{
			"nilContext", cascades.ErrPlannerNilContext,
			api.ErrCodeInternalError, plannerMissingContextMessage,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := translatePlannerError(tc.err, plannerUnableToPlanMessage)

			var apiErr *api.Error
			if !errors.As(got, &apiErr) {
				t.Fatalf("want *api.Error, got %T (%v)", got, got)
			}
			if apiErr.Code != tc.code {
				t.Fatalf("code = %q, want %q", apiErr.Code, tc.code)
			}
			// The cause must survive: errors.Is is how a caller tells a budget
			// exhaustion from a rule-cycle divergence once both sit behind a
			// SQLSTATE. It is also what makes the user-facing budget message
			// safe — the sentinel is still there for logs and matching.
			if !errors.Is(got, tc.err) {
				t.Fatalf("cause not preserved: %v does not wrap %v", got, tc.err)
			}
			if apiErr.Message != tc.msg {
				t.Fatalf("message = %q, want %q", apiErr.Message, tc.msg)
			}
			// None of these may masquerade as a verdict on the user's SQL.
			if apiErr.Code == api.ErrCodeUnsupportedQuery {
				t.Fatal("classified failure reported as an unsupported query")
			}
			if strings.Contains(apiErr.Message, plannerUnableToPlanMessage) {
				t.Fatalf("message %q reuses the unable-to-plan wording", apiErr.Message)
			}

			// api.Error.Error() appends the cause unconditionally, so a Message
			// equal to the sentinel's own text renders the identical sentence
			// twice. This is asserted for EVERY sentinel, not just the one arm
			// that was fixed first: pinning it for a single arm is exactly what
			// let the other three carry the defect unnoticed.
			rendered := got.Error()
			if n := strings.Count(rendered, tc.err.Error()); n != 1 {
				t.Fatalf("sentinel text appears %d times in %q, want exactly 1 (in the cause)",
					n, rendered)
			}
			if apiErr.Message == tc.err.Error() {
				t.Fatalf("Message is a copy of the sentinel rather than a curated "+
					"user-facing verdict: %q", apiErr.Message)
			}
		})
	}
}

// TestTranslatePlannerError_CuratedMessagesAreDistinct pins the property the
// per-sentinel messages exist for. Non-duplication is asserted for every arm in
// SQLSTATEMap above; what that cannot catch is all six arms sharing ONE curated
// message, which would satisfy every check there while collapsing six failure
// modes back into one indistinguishable diagnosis.
//
// The budget arm additionally has to be actionable: a SQL user who is told
// their query is too complex can simplify it, whereas the internal wording
// named a planner config field and offered no remedy.
func TestTranslatePlannerError_CuratedMessagesAreDistinct(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		cascades.ErrPlannerCapHit,
		cascades.ErrPlannerQueueCapHit,
		cascades.ErrPlannerRuleMatchCapHit,
		cascades.ErrPlannerRoundCapHit,
		cascades.ErrPlannerAlreadyUsed,
		cascades.ErrPlannerNilContext,
	}

	// The three budget sentinels deliberately share one verdict (the user's
	// remedy is identical), so four distinct messages is the correct count.
	messages := map[string][]string{}
	for _, s := range sentinels {
		var apiErr *api.Error
		if !errors.As(translatePlannerError(s, plannerUnableToPlanMessage), &apiErr) {
			t.Fatalf("%v did not classify to an *api.Error", s)
		}
		messages[apiErr.Message] = append(messages[apiErr.Message], s.Error())
	}
	if len(messages) != 4 {
		t.Fatalf("expected 4 distinct curated messages (one per budget, one per "+
			"engine-bug sentinel), got %d: %v", len(messages), messages)
	}

	if !strings.Contains(plannerBudgetExceededMessage, "too complex") {
		t.Fatalf("budget message gives the user nothing to act on: %q",
			plannerBudgetExceededMessage)
	}
}

// TestTranslatePlannerError_UnableToPlan pins the arm that keeps 0AF00. It is
// now the EXPLICIT arm rather than the default, so it must name the condition:
// UnplannableIndexOnlyResidualError is the only error escaping the planner that
// is a genuine verdict on the query rather than evidence about the engine.
func TestTranslatePlannerError_UnableToPlan(t *testing.T) {
	t.Parallel()

	cause := &cascades.UnplannableIndexOnlyResidualError{Predicate: "distance_rank(v, 5)"}

	for _, tc := range []struct {
		name string
		msg  string
	}{
		{"statement", plannerUnableToPlanMessage},
		// The scalar-subquery pipeline keeps its own verdict wording so an
		// unplannable `(SELECT ...)` operand is not reported as if the outer
		// statement were at fault.
		{"scalarSubquery", unableToPlanScalarSubqueryMessage},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := translatePlannerError(cause, tc.msg)

			var apiErr *api.Error
			if !errors.As(got, &apiErr) {
				t.Fatalf("want *api.Error, got %T (%v)", got, got)
			}
			if apiErr.Code != api.ErrCodeUnsupportedQuery {
				t.Fatalf("code = %q, want %q", apiErr.Code, api.ErrCodeUnsupportedQuery)
			}
			if apiErr.Message != tc.msg {
				t.Fatalf("message = %q, want %q", apiErr.Message, tc.msg)
			}
			if !errors.Is(got, cause) {
				t.Fatalf("cause discarded: %v does not wrap %v", got, cause)
			}
			// The predicate text is derived from the user's own query, so it is
			// safe to surface — and it is the only actionable detail.
			if !strings.Contains(got.Error(), "distance_rank(v, 5)") {
				t.Fatalf("rendered error dropped the offending predicate: %v", got)
			}
		})
	}
}

// TestTranslatePlannerError_DefaultsToInternal pins the classifier's polarity.
// An unrecognised planner failure is evidence about the ENGINE, so the default
// must admit ignorance rather than assert that the user's SQL is unsupported —
// the same polarity as Java's recordCoreToRelationalException, whose default is
// a code meaning "we don't know" with UnableToPlanException as the explicit
// arm. Inverting this makes every future planner failure mode silently blame
// the query.
func TestTranslatePlannerError_DefaultsToInternal(t *testing.T) {
	t.Parallel()

	// A structural extraction failure, verbatim in shape from
	// plan_extraction.go's rebuild arms — a bare fmt.Errorf with no sentinel and
	// no type, exactly what the default arm has to catch.
	cause := fmt.Errorf("LogicalProjectionExpression: expected 1 child, got %d", 3)
	got := translatePlannerError(cause, plannerUnableToPlanMessage)

	var apiErr *api.Error
	if !errors.As(got, &apiErr) {
		t.Fatalf("want *api.Error, got %T (%v)", got, got)
	}
	if apiErr.Code != api.ErrCodeInternalError {
		t.Fatalf("code = %q, want %q — an unrecognised planner failure must not "+
			"claim the query is unsupported", apiErr.Code, api.ErrCodeInternalError)
	}
	if apiErr.Message != plannerInternalErrorMessage {
		t.Fatalf("message = %q, want %q", apiErr.Message, plannerInternalErrorMessage)
	}
	// Withheld from the rendered text, but still reachable programmatically.
	if !errors.Is(got, cause) {
		t.Fatalf("cause unreachable: %v does not wrap %v", got, cause)
	}
}

// TestTranslatePlannerError_InternalDetailNotLeaked pins the "safe user-facing
// error classes" half of this item. Memo yield-invariant violations format the
// offending expression with %T, and api.Error.Error() appends its cause
// unconditionally, so a database/sql caller was one classification away from
// reading Go type names out of their terminal. The detail stays reachable
// through the error chain for logging and errors.As; only the rendering is
// withheld.
func TestTranslatePlannerError_InternalDetailNotLeaked(t *testing.T) {
	t.Parallel()

	// Shaped exactly like yield_invariants.go's %T-formatted message.
	const leak = "*expressions.LogicalFilterExpression"
	cause := fmt.Errorf("yield-invariant: %s quantifier 1 ranges over no reference "+
		"— its child was never memoized", leak)

	got := translatePlannerError(cause, plannerUnableToPlanMessage)

	var apiErr *api.Error
	if !errors.As(got, &apiErr) {
		t.Fatalf("want *api.Error, got %T (%v)", got, got)
	}
	if strings.Contains(apiErr.Message, leak) {
		t.Fatalf("Message leaks an engine type name: %q", apiErr.Message)
	}
	// Error() is what database/sql hands the caller — the real user surface.
	if strings.Contains(got.Error(), leak) {
		t.Fatalf("rendered error leaks an engine type name: %q", got.Error())
	}
	if strings.Contains(got.Error(), "yield-invariant") {
		t.Fatalf("rendered error leaks the invariant's internal text: %q", got.Error())
	}
	// Withholding must not mean discarding: the detail is still there for a
	// logger or a test that deliberately unwraps.
	if !errors.Is(got, cause) {
		t.Fatalf("withheld detail was discarded, not withheld: %v", got)
	}
	var withheld *withheldPlannerDetail
	if !errors.As(got, &withheld) {
		t.Fatal("cause was not routed through the withholding wrapper")
	}
	if !strings.Contains(errors.Unwrap(withheld).Error(), leak) {
		t.Fatal("unwrapping the wrapper did not recover the full detail")
	}
}

// TestTranslatePlannerError_FamilyIsTriageable pins that withholding does not
// flatten every engine bug into one indistinguishable string. There are 39-plus
// distinct internal planner messages across three families, and nothing in the
// repo logs the withheld text — there is no production PlanGenerationLogger, so
// the rendered error IS the whole record. If it named no family, an operator
// holding a bug report could not tell which subsystem failed.
//
// The families are read from the error TYPE, tagged where each is minted, not
// from the message text — the text is exactly what must not be trusted.
func TestTranslatePlannerError_FamilyIsTriageable(t *testing.T) {
	t.Parallel()

	// The wrapped text is verbatim in shape from each family's real minting
	// site: a quantifier over a nil reference from verifyChildrenMemoized, an
	// arity mismatch from rebuildWithFreshChildren.
	invariant := &cascades.PlannerInvariantViolationError{
		Cause: errors.New("yield-invariant: *expressions.LogicalFilterExpression " +
			"quantifier 1 ranges over no reference"),
	}
	rebuild := &cascades.PlanRebuildError{
		Cause: errors.New("LogicalProjectionExpression: expected 1 child, got 3"),
	}

	cases := []struct {
		name   string
		err    error
		family string
	}{
		{"invariant", invariant, familyInvariantViolation},
		{"rebuild", rebuild, familyPlanRebuild},
		{"unclassified", errors.New("some future planner failure"), familyUnclassified},
	}

	// Distinctness is checked over what plannerFailureFamily actually RETURNS,
	// not over the expected values above — keying it on the test's own literals
	// would make it pass no matter what the function did.
	returned := map[string][]string{}
	for _, tc := range cases {
		returned[plannerFailureFamily(tc.err)] = append(returned[plannerFailureFamily(tc.err)], tc.name)
	}
	if len(returned) != len(cases) {
		t.Fatalf("plannerFailureFamily collapsed %d inputs onto %d families — a "+
			"discriminator that answers the same for different inputs discriminates "+
			"nothing: %v", len(cases), len(returned), returned)
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := translatePlannerError(tc.err, plannerUnableToPlanMessage)
			var apiErr *api.Error
			if !errors.As(got, &apiErr) {
				t.Fatalf("want *api.Error, got %T (%v)", got, got)
			}
			if apiErr.Code != api.ErrCodeInternalError {
				t.Fatalf("code = %q, want %q", apiErr.Code, api.ErrCodeInternalError)
			}
			if !strings.Contains(got.Error(), tc.family) {
				t.Fatalf("rendered error does not name the family %q: %q", tc.family, got.Error())
			}
			// Naming the family must not smuggle the detail back in.
			if strings.Contains(got.Error(), "expressions.") ||
				strings.Contains(got.Error(), "yield-invariant") {
				t.Fatalf("family line leaked engine internals: %q", got.Error())
			}
		})
	}
}

// TestTranslatePlannerError_CancellationPassesThrough pins that cancellation
// keeps its identity. A canceled statement is the caller's own signal, so
// wrapping it in any SQLSTATE would tell the user something about their query
// when they aborted it themselves.
func TestTranslatePlannerError_CancellationPassesThrough(t *testing.T) {
	t.Parallel()

	cause := errors.New("statement abandoned by caller")
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"canceled", context.Canceled, context.Canceled},
		{"deadline", context.DeadlineExceeded, context.DeadlineExceeded},
		// The planner's own wrapping shape: context error joined with the
		// WithCancelCause cause.
		{"withCause", fmt.Errorf("%w: %w", context.Canceled, cause), context.Canceled},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := translatePlannerError(tc.err, plannerUnableToPlanMessage)
			if got != tc.err {
				t.Fatalf("cancellation was rewritten: got %v, want the original %v", got, tc.err)
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("errors.Is(%v, %v) = false", got, tc.want)
			}
			var apiErr *api.Error
			if errors.As(got, &apiErr) {
				t.Fatalf("cancellation acquired SQLSTATE %s", apiErr.Code)
			}
		})
	}

	// Guard the classifier's precondition: a nil planner error is a successful
	// plan and must never be turned into a failure.
	if got := translatePlannerError(nil, plannerUnableToPlanMessage); got != nil {
		t.Fatalf("translatePlannerError(nil) = %v, want nil", got)
	}
}

// TestPlannerCapHit_ProductionSelectPathSQLSTATE drives the real production
// SELECT planning path (planSelectCascades — the callsite that discarded
// planErr) with a join wide enough to genuinely exhaust the 100_000-task
// budget. It proves the WIRING, not just the classifier: no cap is injected and
// no seam is used, so the query must actually trip cascades.ErrPlannerCapHit
// inside the planner and come back out as a class-54 program-limit error
// carrying which budget was exhausted and how far the run got.
func TestPlannerCapHit_ProductionSelectPathSQLSTATE(t *testing.T) {
	t.Parallel()

	g, md := newLoggingGenerator(t, ordersSchema, nil)
	// Six-way self-join: join enumeration exceeds 100_000 tasks well before the
	// memo converges. Four legs plan fine, so this is the cap tripping and not
	// an unplannable shape.
	q := parseQuery(t, "SELECT a.id FROM orders a, orders b, orders c, orders d, orders e, orders f "+
		"WHERE a.id = b.id AND b.id = c.id AND c.id = d.id AND d.id = e.id AND e.id = f.id")

	plan, err := g.planSelectCascades(context.Background(), q, md, false)
	if err == nil {
		t.Fatalf("planning converged within the task cap; the query no longer exercises the cap "+
			"(plan = %v) — widen the join rather than deleting this test", plan)
	}
	if !errors.Is(err, cascades.ErrPlannerCapHit) {
		t.Fatalf("planning failed for a different reason than the task cap: %v", err)
	}

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *api.Error, got %T (%v)", err, err)
	}
	if apiErr.Code != api.ErrCodePlanComplexityLimitReached {
		t.Fatalf("code = %q, want %q", apiErr.Code, api.ErrCodePlanComplexityLimitReached)
	}
	if apiErr.Message != plannerBudgetExceededMessage {
		t.Fatalf("message = %q, want the user-facing budget verdict %q",
			apiErr.Message, plannerBudgetExceededMessage)
	}

	// Java attaches MAX_TASK_COUNT and TASK_COUNT to its complexity exception
	// via addLogInfo and the SQL layer propagates them as context. A user should
	// learn which limit they hit and how far past it they got, not merely that
	// some limit existed.
	limit, ok := apiErr.Context["max_task_count"].(int)
	if !ok {
		t.Fatalf("no max_task_count in context %v", apiErr.Context)
	}
	if limit != 100_000 {
		t.Fatalf("max_task_count = %d, want the configured 100000", limit)
	}
	observed, ok := apiErr.Context["task_count"].(int)
	if !ok {
		t.Fatalf("no task_count in context %v", apiErr.Context)
	}
	if observed < limit {
		t.Fatalf("task_count = %d, want at least the limit %d", observed, limit)
	}

	// The typed budget error is what carries those numbers; pin that it reaches
	// the SQL boundary intact rather than being flattened into a string.
	var budget *cascades.PlannerBudgetExceededError
	if !errors.As(err, &budget) {
		t.Fatal("budget error type did not survive to the SQL boundary")
	}
	if budget.Budget != "MaxTasks" {
		t.Fatalf("Budget = %q, want %q", budget.Budget, "MaxTasks")
	}
}
