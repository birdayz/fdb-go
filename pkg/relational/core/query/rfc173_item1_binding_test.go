package query

// RFC-173 QP-REF-BIND item 1, commit 1 (dark infrastructure) — white-box pins
// for the binding-keyed seed plumbing. Three design-ruling properties (RFC-173 item 1):
//   - CARRIED, NOT RE-DERIVED: sourceBinding reads the logical leg's Binding
//     field (the parser's single mint authority); it never re-derives binding
//     identity from the SQL alias.
//   - BINDING-KEYED SEED: legTypes / QOV correlations / windows key on the
//     binding, so two duplicate-alias legs stay distinguishable end-to-end
//     (== the UPPER alias for every non-duplicate leg — byte-identical keying
//     for every query that plans today).
//   - C1 IS DARK: the gate's duplicate-alias poison arm stands even when the
//     parser minted a binding — the lift is commit 2's, landing WITH the
//     front-end per-reference resolution (the never-live-separately
//     constraint; a c1 lift would observably flip the predicate-free
//     disjoint class from the name model to the ordinal seed).

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func scanWithBinding(table, alias, binding string) *logical.LogicalScan {
	s := logical.NewScan(table, alias)
	s.Binding = binding
	return s
}

func TestRFC173Item1_SourceBindingCarriedNotRederived(t *testing.T) {
	t.Parallel()

	// Alias-bound legs: binding == sourceAlias's UPPER form.
	if got := sourceBinding(scan("SRC", "s")); got != "S" {
		t.Fatalf("alias-bound scan binding = %q, want S", got)
	}
	// A minted duplicate leg returns the carried id VERBATIM; the display
	// alias is untouched.
	dup := scanWithBinding("AUX", "s", "Q$DUP1")
	if got := sourceBinding(dup); got != "Q$DUP1" {
		t.Fatalf("minted scan binding = %q, want Q$DUP1", got)
	}
	if got := sourceAlias(dup); got != "S" {
		t.Fatalf("sourceAlias must stay the DISPLAY alias, got %q want S", got)
	}
	// Join recursion follows the right leg, like sourceAlias.
	j := inner(scan("SRC", "s"), dup)
	if got := sourceBinding(j); got != "Q$DUP1" {
		t.Fatalf("join binding = %q, want the right leg's Q$DUP1", got)
	}
	// CTE legs carry their own binding.
	cte := logical.NewCTE("d", scan("SRC", "s"), logical.NewScan("d", ""), false)
	cte.Binding = "Q$DUP2"
	if got := sourceBinding(cte); got != "Q$DUP2" {
		t.Fatalf("cte binding = %q, want Q$DUP2", got)
	}
	cte.Binding = ""
	if got := sourceBinding(cte); got != "D" {
		t.Fatalf("alias-bound cte binding = %q, want D", got)
	}
}

func TestRFC173Item1_SeedKeyedByBinding(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	// Two legs under the SAME SQL alias, the later one carrying the minted
	// binding (the c2 state, driven directly past the gate — white-box).
	j := inner(scan("SRC", "s"), scanWithBinding("AUX", "s", "Q$DUP1"))
	legs := tr.legsOfGatedJoin(j)
	if len(legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(legs))
	}
	if legs[0].binding != "S" || legs[1].binding != "Q$DUP1" {
		t.Fatalf("leg bindings = [%s %s], want [S Q$DUP1]", legs[0].binding, legs[1].binding)
	}
	if legs[0].alias != "S" || legs[1].alias != "S" {
		t.Fatalf("leg DISPLAY aliases = [%s %s], want [S S]", legs[0].alias, legs[1].alias)
	}

	fields, legTypes := tr.ordinalJoinSeedFields(legs)
	if fields == nil {
		t.Fatal("seed fields must build for binding-distinguished duplicate legs")
	}
	if _, ok := legTypes["S"]; !ok {
		t.Fatalf("legTypes must key the first leg by its alias binding S; keys=%v", keysOf(legTypes))
	}
	if _, ok := legTypes["Q$DUP1"]; !ok {
		t.Fatalf("legTypes must key the duplicate leg by its minted binding; keys=%v", keysOf(legTypes))
	}
	// QOV correlations carry the binding — the two legs are distinguishable
	// (the alias-keyed collision this plumbing exists to dissolve).
	corrs := map[string]bool{}
	for _, f := range fields {
		fv := f.Value.(*values.FieldValue)
		qov := fv.Child.(*values.QuantifiedObjectValue)
		corrs[qov.Correlation.Name()] = true
	}
	if !corrs["S"] || !corrs["Q$DUP1"] || len(corrs) != 2 {
		t.Fatalf("seed QOV correlations = %v, want exactly {S, Q$DUP1}", corrs)
	}

	// The executor-side window derivation stays run-distinct: two windows,
	// keyed by the (fold-stable) binding names.
	rv, _ := tr.buildOrdinalJoinResultValue(legs)
	if rv == nil {
		t.Fatal("ordinal RV must build")
	}
	windows, _ := values.OrdinalSeedLegWindows(rv.(*values.RecordConstructorValue))
	if len(windows) != 2 {
		t.Fatalf("windows = %v, want 2 distinct binding-keyed windows", windows)
	}
	if _, ok := windows["S"]; !ok {
		t.Fatalf("missing window for S; windows=%v", windows)
	}
	if _, ok := windows[strings.ToUpper("Q$DUP1")]; !ok {
		t.Fatalf("missing window for Q$DUP1; windows=%v", windows)
	}
}

// TestRFC173Item1_W5GatherBindingConsistency covers the review catch: the W5
// gathered-unnest translation consumes the seed's legTypes map, so its
// quantifier correlations, sourceAliases and span-offset lookups now share the
// seed's BINDING key discipline (an alias-keyed lookup against the
// binding-keyed map would nil-miss a duplicate leg's entry and panic on
// .typ). In c1 the converted path is DARK the same way the seed keying is —
// the W5 gather consults the ONE gate authority, whose dup poison arm
// declines a duplicate-alias cluster even with a minted binding present
// (pinned here, the W5 twin of GatePoisonIntactC1); the c2 lift's red-first
// suite activates the binding-keyed quantifier/span asserts e2e. The non-dup
// path (binding == alias) is byte-identical and stays covered by the W5
// gather suite.
func TestRFC173Item1_W5GatherBindingConsistency(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	left := inner(scan("SRC", "s"), scanWithBinding("AUX", "s", "Q$DUP1"))
	j := logical.NewJoin(left, u, logical.JoinInner, "")
	innerCorr := values.NamedCorrelationIdentifier("EL")
	if got := tr.translateGatheredUnnestCluster(j, u, innerCorr, values.NotNullLong, "ARR", unnestTrailing); got != nil {
		t.Fatalf("c1: a duplicate-alias gathered cluster must DECLINE at the gate even with a minted binding (got %T) — the lift is commit 2's", got)
	}
}

// TestRFC173Item1_GatePoisonIntactC1 pins commit 1's DARKNESS: the gate's
// duplicate-alias arm still poisons a dup cluster even when the parser minted
// a binding id for it. The lift lands in commit 2 WITH the per-reference
// front end — never separately (a c1 lift would flip the predicate-free
// disjoint class's plan shape, and a front-end-less ordinal dup cluster has
// no resolver emitting binding-correlated references to bake).
func TestRFC173Item1_GatePoisonIntactC1(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	j := inner(scan("SRC", "s"), scanWithBinding("AUX", "s", "Q$DUP1"))
	d := tr.ordinalWedgeGateDecide(j)
	if d.Gated {
		t.Fatal("c1 must keep the duplicate-alias cluster POISONED (the lift is commit 2's, with the front end)")
	}
	if d.Arity != arityPoison || !strings.Contains(d.Reason, "duplicate leg aliases") {
		t.Fatalf("dup cluster decision = %+v, want the arityPoison duplicate-leg-aliases arm", d)
	}
}

func keysOf(m map[string]bakeLegType) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
