package cascades

import (
	"embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestDefaultRules_NotEmpty(t *testing.T) {
	t.Parallel()
	if len(DefaultExpressionRules()) == 0 {
		t.Fatal("DefaultExpressionRules returned empty slice")
	}
}

// productionRuleSets returns every production rule-set constructor's
// members, keyed by constructor name — the runtime side of the RFC-175
// D2 assertions. This replaces the retired `const expected = 42` count
// pin, which carried no information (len == N because we chose N) and
// let real regressions through: a rule type dropped from every set, or
// duplicated within a set, kept the suite green as long as someone
// updated the constant.
//
// DefaultImplementationRules already appends
// GoExtensionImplementationRules, and NormalizationRules already
// prepends DeMorgan onto DefaultSimplifyRules — listing the composites
// covers the parts. FinalizeExpressionsRule is listed explicitly: it is
// instantiated directly by NewPlanner (the REWRITING-phase
// rewritingImplRules), not by a set constructor.
func productionRuleSets() map[string][]any {
	return map[string][]any{
		"DefaultExpressionRules":     anySlice(DefaultExpressionRules()),
		"PlanningExplorationRules":   anySlice(PlanningExplorationRules()),
		"BatchAExpressionRules":      anySlice(BatchAExpressionRules()),
		"DMLImplementationRules":     anySlice(DMLImplementationRules()),
		"RewritingRules":             anySlice(RewritingRules()),
		"MatchingRules":              anySlice(MatchingRules()),
		"DefaultImplementationRules": anySlice(DefaultImplementationRules()),
		"DefaultSimplifyRules":       anySlice(DefaultSimplifyRules()),
		"NormalizationRules":         anySlice(NormalizationRules()),
		"planner rewritingImplRules": {NewFinalizeExpressionsRule()},
	}
}

func anySlice[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// shortNameOf strips Go's package + pointer prefix from %T output for
// any rule shape (ExpressionRule / ImplementationRule / CascadesRule).
func shortNameOf(r any) string {
	t := fmt.Sprintf("%T", r)
	for i := len(t) - 1; i >= 0; i-- {
		if t[i] == '.' || t[i] == '*' {
			return t[i+1:]
		}
	}
	return t
}

// TestRuleSets_NoDuplicateRuleTypes asserts every production rule set
// constructs each rule TYPE at most once (RFC-175 §5 D2: "every rule
// registered exactly once"). A duplicate within a set doubles the
// per-iteration matcher work silently — the failure mode the old count
// pin could not see (a copy-paste duplicate plus a dropped rule keeps
// the count identical).
//
// Cross-SET repetition is deliberate and NOT asserted against:
// NormalizePredicatesRule and friends fire in both REWRITING
// (DefaultExpressionRules) and PLANNING (PlanningExplorationRules) by
// design, mirroring Java's per-phase rule sets.
func TestRuleSets_NoDuplicateRuleTypes(t *testing.T) {
	t.Parallel()
	for setName, rules := range productionRuleSets() {
		seen := map[string]int{}
		for i, r := range rules {
			k := shortNameOf(r)
			if prev, ok := seen[k]; ok {
				t.Errorf("%s constructs %s twice (indices %d and %d)", setName, k, prev, i)
			}
			seen[k] = i
		}
	}
}

// TestRuleRegistry_ResolvesEveryRegisteredSetRule asserts that every
// rule in the four set constructors the package init registers
// (registerDefaultRules / registerBatchARules / registerMatchingRules /
// registerRewritingRules) resolves via LookupRule under its short type
// name. Diagnostic / explain output relies on LookupRule(name) → rule,
// so a registration regression breaks rule-trace logs without a clear
// failure. Generalizes the old hardcoded 11-name spot check to the
// full registered surface.
//
// (No assertion over RegisteredRuleNames() as a whole: tests register
// scratch names concurrently — rule_registry_test.go — so the global
// registry contents are not stable under t.Parallel.)
func TestRuleRegistry_ResolvesEveryRegisteredSetRule(t *testing.T) {
	t.Parallel()
	registered := map[string][]ExpressionRule{
		"DefaultExpressionRules": DefaultExpressionRules(),
		"BatchAExpressionRules":  BatchAExpressionRules(),
		"MatchingRules":          MatchingRules(),
		"RewritingRules":         RewritingRules(),
	}
	for setName, rules := range registered {
		for _, r := range rules {
			name := shortTypeName(r)
			if LookupRule(name) == nil {
				t.Errorf("%s: LookupRule(%q) = nil — package init did not register it", setName, name)
			}
		}
	}
}

// cascadesSourceFS embeds the package's own sources so the coverage
// assertion below can enumerate the DECLARED rule types — the
// enumeration Go reflection cannot provide and an explicit list would
// rot. Test files are filtered out at parse time.
//
//go:embed *.go
var cascadesSourceFS embed.FS

// TestRuleTypes_EveryExportedRuleTypeInAProductionSet is the RFC-175
// §5 D2 coverage assertion: every exported `type XxxRule struct` in
// this package is constructed by at least one production rule set
// (productionRuleSets). This is the axis the retired count pin was
// supposed to guard — "accidental removal during a refactor silently
// shrinks the optimiser's reach" — asserted against the source of
// truth instead of a hand-maintained integer: a rule type that exists
// as code but is fired by no set is either dead machinery (delete it —
// the four orphaned Push/Pull filter rules died with this test's
// introduction) or a missing set entry (a real reach regression).
func TestRuleTypes_EveryExportedRuleTypeInAProductionSet(t *testing.T) {
	t.Parallel()
	entries, err := cascadesSourceFS.ReadDir(".")
	if err != nil {
		t.Fatalf("reading embedded sources: %v", err)
	}
	fset := token.NewFileSet()
	declared := map[string]string{} // rule type name → declaring file
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, readErr := cascadesSourceFS.ReadFile(name)
		if readErr != nil {
			t.Fatalf("reading embedded %s: %v", name, readErr)
		}
		f, parseErr := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", name, parseErr)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				// Struct types only: the rule INTERFACES (ExpressionRule,
				// ImplementationRule, CascadesRule, …) also end in "Rule".
				if _, isStruct := ts.Type.(*ast.StructType); !isStruct {
					continue
				}
				tn := ts.Name.Name
				if !ast.IsExported(tn) || !strings.HasSuffix(tn, "Rule") {
					continue
				}
				declared[tn] = name
			}
		}
	}
	// Anti-vacuity: if the embed/parse pipeline breaks, the test must go
	// red, not silently guard an empty set (the package declares 128
	// rule types at the time of writing).
	if len(declared) < 100 {
		t.Fatalf("parsed only %d declared rule types from embedded sources — embed or parse pipeline broken", len(declared))
	}

	constructed := map[string]struct{}{}
	for _, rules := range productionRuleSets() {
		for _, r := range rules {
			constructed[shortNameOf(r)] = struct{}{}
		}
	}
	for tn, file := range declared {
		if _, ok := constructed[tn]; !ok {
			t.Errorf("exported rule type %s (%s) is not constructed by any production rule set — dead machinery or a missing set entry", tn, file)
		}
	}
}

// TestDefaultRules_NoNil verifies every rule in the default set is
// non-nil and has a non-nil Matcher. Caught a bug class where a rule
// constructor accidentally returns nil under some conditions.
func TestDefaultRules_NoNil(t *testing.T) {
	t.Parallel()
	for i, r := range DefaultExpressionRules() {
		if r == nil {
			t.Fatalf("default rule at index %d is nil", i)
		}
		if r.Matcher() == nil {
			t.Fatalf("default rule at index %d (%T) has nil Matcher", i, r)
		}
	}
}

// typeName returns the rule's concrete type name (Go's %T format).
// Used for order comparison in the StableOrder test — works on
// any rule type, including future ones, without per-rule
// maintenance.
func typeName(r ExpressionRule) string {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", r)
}

// TestDefaultRules_StableOrder pins that DefaultExpressionRules
// returns the rules in the same order on every call. The exploration
// driver iterates rules in this order, so a rule reordering would
// change which equivalent expressions land first in the Reference's
// member list — fine but worth pinning so nothing accidentally
// shuffles them.
func TestDefaultRules_StableOrder(t *testing.T) {
	t.Parallel()
	first := DefaultExpressionRules()
	for trial := 0; trial < 5; trial++ {
		next := DefaultExpressionRules()
		if len(first) != len(next) {
			t.Fatalf("trial %d: length differs (first=%d, next=%d)", trial, len(first), len(next))
		}
		for i := range first {
			if typeName(first[i]) != typeName(next[i]) {
				t.Fatalf("trial %d: index %d differs (first=%s, next=%s)", trial, i, typeName(first[i]), typeName(next[i]))
			}
		}
	}
}

// TestDefaultRules_EndToEndOptimisation drives a multi-rule rewrite
// chain through the default rule set:
//
//	Filter(TRUE) over Filter(TRUE) over Distinct over Distinct over Scan
//
// Each rule fires in turn and each yield grows the Reference because
// Reference.Insert's children-aware dedup distinguishes shapes that
// share EqualsWithoutChildren but range over different inner
// References (the dedup contract documented on Reference.Insert).
//
// Expected fires (over 2-3 iterations):
//   - FilterMerge on outer Filter — yields Filter([T,T]) over outerD's Q.
//   - NoOpFilter on outer Filter — yields innerF.
//   - NoOpFilter on the merged Filter([T,T]) — yields outerD.
//   - DistinctMerge on outerD — yields Distinct over scanQ.
//
// Test pins that the optimisation chain produces a Distinct(Scan)
// member somewhere in the resulting Reference.
func TestDefaultRules_EndToEndOptimisation(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerD := expressions.NewLogicalDistinctExpression(scanQ)
	innerDQ := expressions.ForEachQuantifier(expressions.InitialOf(innerD))
	outerD := expressions.NewLogicalDistinctExpression(innerDQ)
	outerDQ := expressions.ForEachQuantifier(expressions.InitialOf(outerD))
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	innerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, outerDQ)
	innerFQ := expressions.ForEachQuantifier(expressions.InitialOf(innerF))
	outerF := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, innerFQ)
	ref := expressions.InitialOf(outerF)

	tasks, converged := exploreRewriting(NewPlanner(DefaultExpressionRules(), nil), ref)
	if !converged {
		t.Fatalf("did not converge — tasks=%d", tasks)
	}

	// Find the most-optimised member: Distinct directly over Scan.
	// Reaching it requires the full FilterMerge + 2× NoOpFilter +
	// DistinctMerge chain, so finding the shape pins the composition.
	foundShape := false
	for _, m := range ref.Members() {
		d, ok := m.(*expressions.LogicalDistinctExpression)
		if !ok {
			continue
		}
		inner := d.GetInner().GetRangesOver().Get()
		if _, ok := inner.(*expressions.FullUnorderedScanExpression); ok {
			foundShape = true
			break
		}
	}
	if !foundShape {
		t.Fatalf("after exploration, Reference has no Distinct(Scan) member — members=%d", len(ref.Members()))
	}
}
