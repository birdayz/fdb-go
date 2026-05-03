package cascades

// PlannerPhase drives the multi-phase planning lifecycle.
// Java's CascadesPlanner runs REWRITING first (exploration rules,
// RewritingCostModel), then PLANNING (implementation rules,
// PlanningCostModel). Each phase has its own rule set.
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.PlannerPhase.
type PlannerPhase int

const (
	// PhaseRewriting applies exploration/transformation rules that
	// rewrite the logical expression DAG. All expressions produced
	// are exploratory. Uses ExpressionRules.
	PhaseRewriting PlannerPhase = iota

	// PhasePlanning applies implementation rules that convert
	// exploratory expressions to final (physical) expressions.
	// Uses ImplementationCascadesRules. FinalizeExpressionsRule
	// is the catch-all that finalizes any remaining exploratory
	// expression by disentangling its children.
	PhasePlanning
)

func (p PlannerPhase) String() string {
	switch p {
	case PhaseRewriting:
		return "REWRITING"
	case PhasePlanning:
		return "PLANNING"
	default:
		return "UNKNOWN"
	}
}

// NextPhase returns the phase that follows this one.
// PhasePlanning is terminal (panics if called).
func (p PlannerPhase) NextPhase() PlannerPhase {
	switch p {
	case PhaseRewriting:
		return PhasePlanning
	default:
		panic("no phase after PLANNING")
	}
}

// HasNextPhase reports whether there is a subsequent phase.
func (p PlannerPhase) HasNextPhase() bool {
	return p == PhaseRewriting
}
