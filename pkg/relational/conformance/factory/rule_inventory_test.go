package factory_test

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
)

// shortTypeNameLikePlanner reproduces cascades.shortTypeName EXACTLY —
// `fmt.Sprintf("%T", r)` scanned backwards for '.' or '*'.
//
// Reproduced rather than approximated on purpose. This gate's entire job is to
// prove a name string matches what planner.go:829 looks up; an approximation
// that happened to agree for the common case would be a gate validating
// itself, and it would agree right up until the one name that mattered.
func shortTypeNameLikePlanner(r any) string {
	t := fmt.Sprintf("%T", r)
	for i := len(t) - 1; i >= 0; i-- {
		if t[i] == '.' || t[i] == '*' {
			return t[i+1:]
		}
	}
	return t
}

func namesOfRuleSlice(rs any) []string {
	v := reflect.ValueOf(rs)
	out := make([]string, 0, v.Len())
	for i := 0; i < v.Len(); i++ {
		out = append(out, shortTypeNameLikePlanner(v.Index(i).Interface()))
	}
	return out
}

// registryNames is every name Planner.DisabledRules can meaningfully hold.
func registryNames() map[string]string {
	out := map[string]string{}
	add := func(group string, names []string) {
		for _, n := range names {
			if _, dup := out[n]; !dup {
				out[n] = group
			}
		}
	}
	add("expression", namesOfRuleSlice(cascades.DefaultExpressionRules()))
	add("planning-exploration", namesOfRuleSlice(cascades.PlanningExplorationRules()))
	add("batchA", namesOfRuleSlice(cascades.BatchAExpressionRules()))
	add("rewriting", namesOfRuleSlice(cascades.RewritingRules()))
	add("matching", namesOfRuleSlice(cascades.MatchingRules()))
	add("implementation", namesOfRuleSlice(cascades.DefaultImplementationRules()))
	add("go-ext-impl", namesOfRuleSlice(cascades.GoExtensionImplementationRules()))
	add("dml-impl", namesOfRuleSlice(cascades.DMLImplementationRules()))
	return out
}

// portfolioRuleNames extracts the rule names the hunter's portfolio actually
// disables, by building each perturbation's options and reading the value back
// out. It reads the LIVE portfolio rather than a copied list, because a copied
// list drifts and then certifies names nothing uses.
func portfolioRuleNames() map[string][]string {
	out := map[string][]string{}
	for _, p := range portfolio {
		if p.opts == nil {
			continue
		}
		v := p.opts(api.NewOptionsBuilder()).Build().Get(api.OptDisabledPlannerRules)
		names, ok := v.([]string)
		if !ok || len(names) == 0 {
			continue
		}
		out[p.name] = names
	}
	return out
}

// TestPortfolioRuleNamesAreReal is the inert-name gate.
//
// plannerOptionsFrom carries an UNRECOGNISED rule name through verbatim —
// deliberately, because Java's setDisabledTransformationRuleNames never
// resolves names against its rule set either, so rejecting here would fail
// queries Java accepts. The consequence is that a typo disables nothing and
// the perturbation silently becomes a no-op: the hunter then plans the same
// query twice, finds the plans identical, records a skip, and reports the
// silence as agreement.
//
// That failure is invisible in every other signal the hunt produces, which is
// why it gets its own gate rather than a comment.
func TestPortfolioRuleNamesAreReal(t *testing.T) {
	t.Parallel()
	names := registryNames()
	if len(names) == 0 {
		t.Fatal("rule registry resolved ZERO names — the accessors changed and this gate is now vacuous")
	}
	used := portfolioRuleNames()
	if len(used) == 0 {
		t.Fatal("portfolio disables ZERO rules — the gate would pass over an empty set")
	}

	checked := 0
	for pName, rules := range used {
		for _, r := range rules {
			checked++
			group, ok := names[r]
			if !ok {
				t.Errorf("perturbation %q names rule %q, which matches NO rule in the planner registry — "+
					"it disables nothing, so every silence it produces is meaningless", pName, r)
				continue
			}
			t.Logf("RULE-OK %-36s %-38s %s", pName, r, group)
		}
	}
	if checked == 0 {
		t.Fatal("checked zero rule names")
	}
	t.Logf("registry holds %d distinct rule names; portfolio names %d, all resolved", len(names), checked)
}

// TestRuleRegistryInventory prints the full inventory and floors its size, so
// a refactor that empties one of the accessors is visible rather than silently
// shrinking what any rule-name-driven tool can reach.
func TestRuleRegistryInventory(t *testing.T) {
	t.Parallel()
	names := registryNames()
	var keys []string
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-40s %s\n", k, names[k])
	}
	t.Logf("planner rule inventory (%d distinct names):\n%s", len(keys), b.String())

	// Measured at e342883c1: 108 distinct names. Floored, not pinned to the
	// exact value — rules are added routinely and that must not redden the
	// build, while a COLLAPSE means an accessor stopped returning its set and
	// every rule-name-driven perturbation quietly lost its target.
	const floor = 90
	if len(keys) < floor {
		t.Errorf("rule registry resolved %d names, floor %d — an accessor probably stopped "+
			"returning its rule set (108 at e342883c1)", len(keys), floor)
	}
}
