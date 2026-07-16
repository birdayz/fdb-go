package query

// White-box pins for the binding-keyed seed plumbing and the duplicate-alias
// gate lift. Properties pinned:
//   - CARRIED, NOT RE-DERIVED: sourceBinding reads the logical leg's Binding
//     field (the parser's single mint authority); it never re-derives binding
//     identity from the SQL alias.
//   - BINDING-KEYED SEED: legTypes / QOV correlations / windows key on the
//     binding, so two duplicate-alias legs stay distinguishable end-to-end
//     (== the UPPER alias for every non-duplicate leg — byte-identical keying
//     for every non-duplicate query).
//   - THE LIFT IS BINDING-GUARDED: duplicate aliases gate only when their
//     bindings are distinct; a same-binding pair (unminted duplicate) stays
//     poisoned — correct-or-loud.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

func scanWithBinding(table, alias, binding string) *logical.LogicalScan {
	s := logical.NewScan(table, alias)
	s.Binding = binding
	return s
}

func TestSourceBindingCarriedNotRederived(t *testing.T) {
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

func TestSeedKeyedByBinding(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	// Two legs under the SAME SQL alias, the later one carrying the minted
	// binding (driven directly past the gate — white-box).
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

// TestGatheredUnnestBindingConsistency covers a review catch: the gathered-unnest
// translation consumes the
// seed's binding-keyed legTypes map, so its quantifier correlations,
// sourceAliases and span-offset lookups share the seed's key discipline (an
// alias-keyed lookup would nil-miss a duplicate leg's entry and panic on
// .typ). The gathered fixture's shape with a minted binding on the second
// leg must translate with the binding as that quantifier's correlation; the
// unnest OWNER resolves by SQL alias (first match — the front end owns
// ambiguity) while correlating by the matched leg's binding.
func TestGatheredUnnestBindingConsistency(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	u := &logical.LogicalUnnest{Segments: []string{"s", "ARR"}, Alias: "EL"}
	left := inner(scan("SRC", "s"), scanWithBinding("AUX", "s", "Q$DUP1"))
	j := logical.NewJoin(left, u, logical.JoinInner, "")
	innerCorr := values.NamedCorrelationIdentifier("EL")
	sel := tr.translateGatheredUnnestCluster(j, u, innerCorr, values.NotNullLong, "ARR", unnestTrailing)
	if sel == nil {
		t.Fatal("a binding-distinguished duplicate-alias gathered cluster must translate")
	}
	gathered, isSel := sel.(*expressions.SelectExpression)
	if !isSel {
		t.Fatalf("gathered = %T, want *SelectExpression", sel)
	}
	quants := gathered.GetQuantifiers()
	if len(quants) != 3 {
		t.Fatalf("quantifiers = %d, want 3 (S, Q$DUP1, EL in FROM order)", len(quants))
	}
	want := []string{"S", "Q$DUP1", "EL"}
	for i, q := range quants {
		if q.GetAlias().Name() != want[i] {
			t.Fatalf("quantifier %d correlation = %s, want %s (the dup leg correlates by its BINDING)",
				i, q.GetAlias().Name(), want[i])
		}
	}
}

// TestDuplicateAliasBindingGates pins the rule: duplicate SQL aliases
// with DISTINCT parser-minted bindings GATE (the seed/bake/window keying is
// binding-collision-free, so the cluster is classifiable — Java's model,
// where quantifier ids are never SQL names); two legs binding the SAME
// correlation stay poisoned (an unminted duplicate reaches the gate only
// through a path the mint authority does not cover — correct-or-loud).
func TestDuplicateAliasBindingGates(t *testing.T) {
	t.Parallel()
	tr := newDisjointUnnestTranslator(t)

	// Distinct bindings → the dup-alias cluster gates ordinal (arity 2).
	j := inner(scan("SRC", "s"), scanWithBinding("AUX", "s", "Q$DUP1"))
	d := tr.ordinalWedgeGateDecide(j)
	if !d.Gated || d.Arity != 2 {
		t.Fatalf("binding-distinguished dup cluster decision = %+v, want Gated arity 2", d)
	}

	// Same binding (unminted duplicate) → still poison.
	jSame := inner(scan("SRC", "s"), scan("AUX", "s"))
	dSame := tr.ordinalWedgeGateDecide(jSame)
	if dSame.Gated || dSame.Arity != arityPoison || !strings.Contains(dSame.Reason, "duplicate leg bindings") {
		t.Fatalf("same-binding dup cluster decision = %+v, want the arityPoison duplicate-leg-bindings arm", dSame)
	}
}

func keysOf(m map[string]bakeLegType) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
