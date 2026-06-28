package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"

// PlannerPhase drives the multi-phase planning lifecycle.
// Java's CascadesPlanner runs REWRITING first (exploration rules,
// RewritingCostModel), then PLANNING (implementation rules,
// PlanningCostModel). Each phase has its own rule set and cost model.
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.PlannerPhase.
type PlannerPhase int

const (
	PhaseRewriting PlannerPhase = iota
	PhasePlanning
)

// TargetStage returns the PlannerStage that References should reach
// after this phase completes. Mirrors Java's PlannerPhase.getTargetPlannerStage().
func (p PlannerPhase) TargetStage() expressions.PlannerStage {
	switch p {
	case PhaseRewriting:
		return expressions.StageCanonical
	case PhasePlanning:
		return expressions.StagePlanned
	default:
		return expressions.StageInitial
	}
}

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
