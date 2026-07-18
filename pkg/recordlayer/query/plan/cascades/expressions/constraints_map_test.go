package expressions

import "testing"

// TestConstraintsMap_TickWatermarkSemantics pins the Java ConstraintsMap
// contract (RFC-181 WS-P stage (a)): pushes bump ticks; exploration
// converges exactly when no push arrived since the round started; a
// post-commit push re-arms; the stage boundary drops stale constraints
// and resets to never-explored.
func TestConstraintsMap_TickWatermarkSemantics(t *testing.T) {
	t.Parallel()
	m := NewConstraintsMap()
	type key struct{ name string }
	k1, k2 := &key{"c1"}, &key{"c2"}

	if !m.HasNeverBeenExplored() || m.IsExploring() {
		t.Fatal("fresh map: never explored, not exploring")
	}
	// Java: fresh map has goal==-1 != tick==0 → needs exploration.
	if !m.NeedsExploration() {
		t.Fatal("fresh map needs exploration")
	}

	// First push stores and ticks.
	if _, changed := m.PushProperty(k1, "a", nil); !changed || m.CurrentTick() != 1 {
		t.Fatalf("first push: changed=%v tick=%d", changed, m.CurrentTick())
	}
	// Same-key push with nil combine is subsumed: no tick.
	if _, changed := m.PushProperty(k1, "b", nil); changed || m.CurrentTick() != 1 {
		t.Fatalf("subsumed push must not tick: changed=%v tick=%d", changed, m.CurrentTick())
	}
	// Combine that reports change ticks and stores.
	got, changed := m.PushProperty(k1, "b", func(existing, pushed any) (any, bool) {
		return existing.(string) + pushed.(string), true
	})
	if !changed || got != "ab" || m.CurrentTick() != 2 {
		t.Fatalf("combined push: got=%v changed=%v tick=%d", got, changed, m.CurrentTick())
	}

	// Round 1: start → exploring; commit with no new pushes → explored.
	m.StartExploration()
	if !m.IsExploring() || m.NeedsExploration() {
		t.Fatal("started round: exploring, not needing")
	}
	m.CommitExploration()
	if !m.IsExplored() || m.IsExploring() || m.NeedsExploration() {
		t.Fatal("committed with no pushes: explored")
	}
	if !m.IsExploredForAttributes([]any{k1}) {
		t.Fatal("k1 not pushed past the committed watermark")
	}

	// A post-commit push re-arms convergence.
	m.PushProperty(k2, "x", nil)
	if m.IsExplored() || !m.NeedsExploration() {
		t.Fatal("post-commit push must re-arm")
	}
	if m.IsExploredForAttributes([]any{k2}) {
		t.Fatal("k2 pushed past the watermark — not explored for it")
	}

	// Round 2 with a MID-ROUND push: commit leaves it unconverged.
	m.StartExploration()
	m.PushProperty(k1, "c", func(_, pushed any) (any, bool) { return pushed, true })
	m.CommitExploration()
	if m.IsExplored() || !m.NeedsExploration() {
		t.Fatal("mid-round push: still needs exploration after commit")
	}
	// Round 3 converges.
	m.StartExploration()
	m.CommitExploration()
	if !m.IsExplored() {
		t.Fatal("quiet round converges")
	}

	// Stage boundary: k2 was last updated BEFORE the final committed
	// watermark → dropped; k1 (refreshed in round 2) survives; both
	// watermarks reset to never-explored.
	m.AdvancePlannerStage()
	if !m.HasNeverBeenExplored() || !m.NeedsExploration() {
		t.Fatal("stage boundary resets to never-explored")
	}
	if !m.ContainsAttribute(k1) {
		t.Fatal("recently-refreshed constraint survives the boundary")
	}
	if m.ContainsAttribute(k2) {
		t.Fatal("stale constraint is dropped at the boundary")
	}

	// InheritFromOther copies state deep (mutating the copy leaves the
	// source untouched).
	m2 := NewConstraintsMap()
	m2.InheritFromOther(m)
	if m2.CurrentTick() != m.CurrentTick() || !m2.ContainsAttribute(k1) {
		t.Fatal("inherit copies ticks + entries")
	}
	m2.PushProperty(k1, "z", func(_, pushed any) (any, bool) { return pushed, true })
	if v, _ := m.GetConstraint(k1); v == "z" {
		t.Fatal("inherit must deep-copy entries, not alias them")
	}
}
