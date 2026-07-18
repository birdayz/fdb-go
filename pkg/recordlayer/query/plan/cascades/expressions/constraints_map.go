package expressions

// ConstraintsMap is the 1:1 port of Java's ConstraintsMap
// (ConstraintsMap.java, tag 4.12.11.0): a Reference-attached map of
// planner constraints with TICK/WATERMARK exploration bookkeeping —
// Java's convergence model. Every constraint push bumps currentTick;
// StartExploration records the tick as the goal watermark;
// CommitExploration promotes the goal to the committed watermark. A
// Reference is EXPLORED when no push has arrived since exploration
// started (goal == current), EXPLORING while goal > committed.
//
// This is RFC-181 WS-P stage (a): the epoch machinery exists and is
// maintained alongside the member-count convergence Go uses today
// (which stages (b)-(d) retire). Keys are opaque comparable values
// (the cascades package's *PlannerConstraint pointers); combine
// semantics arrive via the caller's callback because the expressions
// package cannot see the typed constraint lattice (Java dispatches
// through PlannerConstraint.combine the same way).
type ConstraintsMap struct {
	currentTick            int64
	watermarkGoalTick      int64
	watermarkCommittedTick int64
	entries                map[any]*constraintsMapEntry
	// order preserves insertion order (Java uses a LinkedHashMap).
	order []any
}

type constraintsMapEntry struct {
	lastUpdatedTick int64
	property        any
}

// NewConstraintsMap mirrors the Java constructor: tick 0, both
// watermarks at -1 (never explored).
func NewConstraintsMap() *ConstraintsMap {
	return &ConstraintsMap{
		currentTick:            0,
		watermarkGoalTick:      -1,
		watermarkCommittedTick: -1,
		entries:                map[any]*constraintsMapEntry{},
	}
}

func (m *ConstraintsMap) CurrentTick() int64            { return m.currentTick }
func (m *ConstraintsMap) WatermarkGoalTick() int64      { return m.watermarkGoalTick }
func (m *ConstraintsMap) WatermarkCommittedTick() int64 { return m.watermarkCommittedTick }

// ContainsAttribute reports whether a constraint is present for key.
func (m *ConstraintsMap) ContainsAttribute(key any) bool {
	_, ok := m.entries[key]
	return ok
}

// GetConstraint returns the stored constraint for key.
func (m *ConstraintsMap) GetConstraint(key any) (any, bool) {
	e, ok := m.entries[key]
	if !ok {
		return nil, false
	}
	return e.property, true
}

// PushProperty pushes a constraint (Java pushProperty). When the key is
// absent the constraint is stored and the tick bumps. When present, the
// caller's combine decides: combine returns the combined constraint and
// ok=true to store it (tick bumps), or ok=false when the push is
// subsumed (no change, no tick — Java's empty Optional). Returns the
// stored (possibly combined) constraint and whether the map changed.
func (m *ConstraintsMap) PushProperty(key, constraint any, combine func(existing, pushed any) (any, bool)) (any, bool) {
	if e, ok := m.entries[key]; ok {
		if combine == nil {
			return e.property, false
		}
		combined, changed := combine(e.property, constraint)
		if !changed {
			return e.property, false
		}
		m.bumpTick()
		e.property = combined
		e.lastUpdatedTick = m.currentTick
		return combined, true
	}
	m.bumpTick()
	m.entries[key] = &constraintsMapEntry{lastUpdatedTick: m.currentTick, property: constraint}
	m.order = append(m.order, key)
	return constraint, true
}

// IsExplored: no push has arrived since exploration started.
func (m *ConstraintsMap) IsExplored() bool {
	if m.watermarkGoalTick > m.currentTick {
		panic("ConstraintsMap: goal watermark ahead of the current tick (bookkeeping bug)")
	}
	return m.watermarkGoalTick == m.currentTick
}

// IsExploring: an exploration round is in flight (started, not committed).
func (m *ConstraintsMap) IsExploring() bool {
	return m.watermarkGoalTick > m.watermarkCommittedTick
}

// HasNeverBeenExplored: no exploration has ever committed.
func (m *ConstraintsMap) HasNeverBeenExplored() bool {
	return m.watermarkCommittedTick < 0
}

// IsFullyExploring: the first-ever exploration round is in flight.
func (m *ConstraintsMap) IsFullyExploring() bool {
	return m.HasNeverBeenExplored() && m.IsExploring()
}

// IsExploredForAttributes: explored at least once, and none of the given
// keys has been pushed past the committed watermark.
func (m *ConstraintsMap) IsExploredForAttributes(keys []any) bool {
	if m.HasNeverBeenExplored() {
		return false
	}
	for _, k := range keys {
		if e, ok := m.entries[k]; ok && e.lastUpdatedTick > m.watermarkCommittedTick {
			return false
		}
	}
	return true
}

// NeedsExploration is Java Reference.needsExploration over this map:
// not mid-round and not converged.
func (m *ConstraintsMap) NeedsExploration() bool {
	return !m.IsExploring() && !m.IsExplored()
}

// StartExploration begins a round: converge to the state as of now.
func (m *ConstraintsMap) StartExploration() {
	m.watermarkGoalTick = m.currentTick
}

// CommitExploration ends the round at its goal.
func (m *ConstraintsMap) CommitExploration() {
	m.watermarkCommittedTick = m.watermarkGoalTick
}

// SetExplored force-marks convergence at the current tick.
func (m *ConstraintsMap) SetExplored() {
	m.watermarkGoalTick = m.currentTick
	m.watermarkCommittedTick = m.currentTick
}

// AdvancePlannerStage drops constraints not refreshed since the last
// committed watermark and resets both watermarks to never-explored —
// the stage-boundary reset (Java advancePlannerStage).
func (m *ConstraintsMap) AdvancePlannerStage() {
	kept := m.order[:0]
	for _, k := range m.order {
		e := m.entries[k]
		if e.lastUpdatedTick < m.watermarkCommittedTick {
			delete(m.entries, k)
			continue
		}
		kept = append(kept, k)
	}
	m.order = kept
	m.watermarkGoalTick = -1
	m.watermarkCommittedTick = -1
}

// InheritFromOther copies the tick/watermark state (Java inheritFromOther
// — used when a Reference adopts another's exploration progress).
func (m *ConstraintsMap) InheritFromOther(other *ConstraintsMap) {
	if other == nil {
		return
	}
	m.currentTick = other.currentTick
	m.watermarkGoalTick = other.watermarkGoalTick
	m.watermarkCommittedTick = other.watermarkCommittedTick
	m.entries = make(map[any]*constraintsMapEntry, len(other.entries))
	m.order = append(m.order[:0:0], other.order...)
	for k, e := range other.entries {
		copied := *e
		m.entries[k] = &copied
	}
}

func (m *ConstraintsMap) bumpTick() int64 {
	m.currentTick++
	return m.currentTick
}

// constraintCombineProvider resolves the per-key lattice combine for
// constraint folds performed INSIDE the expressions package (the Memo
// Absorb path). The cascades package registers its typed dispatch at
// init — the expressions package cannot see the constraint lattice
// (same layering as Java, where PlannerConstraint.combine lives with
// the constraint definitions, not the Reference).
var constraintCombineProvider func(key any) func(existing, pushed any) (any, bool)

// SetConstraintCombineProvider registers the per-key combine dispatch.
// Called once from the cascades package's init.
func SetConstraintCombineProvider(p func(key any) func(existing, pushed any) (any, bool)) {
	constraintCombineProvider = p
}

func combineFor(key any) func(existing, pushed any) (any, bool) {
	if constraintCombineProvider != nil {
		return constraintCombineProvider(key)
	}
	// No registry (standalone expressions tests): conservative
	// always-changed — over-ticking errs toward re-exploration.
	return func(_, pushed any) (any, bool) { return pushed, true }
}

// ReArm bumps the tick so NeedsExploration reports true — the epoch
// expression of the Absorb member-fold re-arm: members folded from a
// merged-away Reference were explored under the LOSER's identity
// (rule bindings and partial matches are (group, expression)-scoped),
// so the survivor must re-explore them under its own.
func (m *ConstraintsMap) ReArm() {
	m.bumpTick()
}
