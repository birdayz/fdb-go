package cascades

import (
	"embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

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
// The assertion is an exact set equality, not a subset: no test may
// register into the package-level registry (TestMain fails the binary
// if one does), so the registry's contents are exactly what init put
// there and an unexpected EXTRA name is as much a regression as a
// missing one.
func TestRuleRegistry_ResolvesEveryRegisteredSetRule(t *testing.T) {
	t.Parallel()
	registered := map[string][]ExpressionRule{
		"DefaultExpressionRules": DefaultExpressionRules(),
		"BatchAExpressionRules":  BatchAExpressionRules(),
		"MatchingRules":          MatchingRules(),
		"RewritingRules":         RewritingRules(),
	}
	fromSets := map[string]struct{}{}
	for setName, rules := range registered {
		for _, r := range rules {
			name := shortTypeName(r)
			fromSets[name] = struct{}{}
			if LookupRule(name) == nil {
				t.Errorf("%s: LookupRule(%q) = nil — package init did not register it", setName, name)
			}
		}
	}
	for _, name := range RegisteredRuleNames() {
		if _, ok := fromSets[name]; !ok {
			t.Errorf("registry holds %q, which no production rule set constructs", name)
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

// TestRuleTypes_EveryProductionRuleHasDirectBehavioralTest closes RFC-190.10's
// other half of rule completeness. Membership tests above prove that every
// exported rule is reachable; this test proves that every reachable rule's
// canonical constructor is called directly inside a runnable Test function. A
// rule reached only because an e2e planner test installs the entire default set
// does not count.
//
// The source scan deliberately ignores comments, identifier-only mentions,
// locally shadowed constructor names, and non-Test helpers. That keeps an
// inventory entry such as productionRuleSets() or a prose mention from
// satisfying the gate. The named test remains responsible for seeding the
// matching expression and asserting the rule's transformation; this mechanical
// check makes a newly registered rule fail until such a test is added.
func TestRuleTypes_EveryProductionRuleHasDirectBehavioralTest(t *testing.T) {
	t.Parallel()

	productionTypes := map[string]struct{}{}
	for _, rules := range productionRuleSets() {
		for _, rule := range rules {
			productionTypes[shortNameOf(rule)] = struct{}{}
		}
	}
	testConstructors := make(map[string]string, len(productionTypes))
	for ruleType := range productionTypes {
		testConstructors["New"+ruleType] = ruleType
	}

	covered := map[string][]string{}
	entries, err := cascadesSourceFS.ReadDir(".")
	if err != nil {
		t.Fatalf("reading embedded sources: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, readErr := cascadesSourceFS.ReadFile(name)
		if readErr != nil {
			t.Fatalf("reading embedded %s: %v", name, readErr)
		}
		// Keep object resolution enabled: unresolved package-level constructor
		// calls have Obj == nil, while a local variable shadowing NewFooRule has
		// a non-nil Obj and must not satisfy the coverage gate.
		file, parseErr := parser.ParseFile(fset, name, src, 0)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", name, parseErr)
		}
		testingAliases := map[string]struct{}{}
		cascadesAliases := map[string]struct{}{}
		dotTestingImport := false
		for _, imported := range file.Imports {
			path, unquoteErr := strconv.Unquote(imported.Path.Value)
			if unquoteErr != nil {
				continue
			}
			switch path {
			case "testing":
				if imported.Name == nil {
					testingAliases["testing"] = struct{}{}
					continue
				}
				if imported.Name.Name == "." {
					dotTestingImport = true
					continue
				}
				testingAliases[imported.Name.Name] = struct{}{}
			case "fdb.dev/pkg/recordlayer/query/plan/cascades":
				if imported.Name == nil {
					cascadesAliases["cascades"] = struct{}{}
				} else if imported.Name.Name != "." && imported.Name.Name != "_" {
					cascadesAliases[imported.Name.Name] = struct{}{}
				}
			}
		}
		for _, decl := range file.Decls {
			testFn, ok := decl.(*ast.FuncDecl)
			if !ok || !isRunnableGoTest(testFn, testingAliases, dotTestingImport) {
				continue
			}
			for ruleType := range directlyCalledRuleConstructors(
				testFn,
				testConstructors,
				cascadesAliases,
			) {
				covered[ruleType] = append(
					covered[ruleType],
					name+":"+testFn.Name.Name,
				)
			}
		}
	}

	var missing []string
	for ruleType := range productionTypes {
		if len(covered[ruleType]) == 0 {
			missing = append(missing, ruleType)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Fatalf(
			"%d production rules have no direct behavioral test reference: %s",
			len(missing),
			strings.Join(missing, ", "),
		)
	}
}

func isRunnableGoTest(
	fn *ast.FuncDecl,
	testingAliases map[string]struct{},
	dotTestingImport bool,
) bool {
	if fn == nil || fn.Body == nil || fn.Recv != nil || !isGoTestName(fn.Name.Name) {
		return false
	}
	if fn.Type.TypeParams != nil && len(fn.Type.TypeParams.List) != 0 {
		return false
	}
	if fn.Type.Results != nil && len(fn.Type.Results.List) != 0 {
		return false
	}
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	parameter := fn.Type.Params.List[0]
	if len(parameter.Names) > 1 {
		return false
	}
	pointer, ok := parameter.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	if selector, ok := pointer.X.(*ast.SelectorExpr); ok {
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || selector.Sel.Name != "T" {
			return false
		}
		_, ok = testingAliases[pkg.Name]
		return ok
	}
	identifier, ok := pointer.X.(*ast.Ident)
	return ok && dotTestingImport && identifier.Name == "T"
}

func directlyCalledRuleConstructors(
	fn *ast.FuncDecl,
	constructors map[string]string,
	cascadesAliases map[string]struct{},
) map[string]struct{} {
	called := map[string]struct{}{}
	if fn == nil || fn.Body == nil {
		return called
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch constructor := call.Fun.(type) {
		case *ast.Ident:
			if constructor.Obj != nil {
				return true
			}
			if ruleType, found := constructors[constructor.Name]; found {
				called[ruleType] = struct{}{}
			}
		case *ast.SelectorExpr:
			qualifier, ok := constructor.X.(*ast.Ident)
			if !ok || qualifier.Obj != nil {
				return true
			}
			if _, imported := cascadesAliases[qualifier.Name]; !imported {
				return true
			}
			if ruleType, found := constructors[constructor.Sel.Name]; found {
				called[ruleType] = struct{}{}
			}
		}
		return true
	})
	return called
}

func isGoTestName(name string) bool {
	const prefix = "Test"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	first, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(first)
}

func TestIsRunnableGoTest_MatchesGoDiscovery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		decl string
		want bool
	}{
		{name: "ordinary test", decl: "func TestRule(t *testing.T) {}", want: true},
		{name: "bare prefix", decl: "func Test(t *testing.T) {}", want: true},
		{name: "underscore suffix", decl: "func Test_rule(t *testing.T) {}", want: true},
		{name: "lowercase suffix", decl: "func Testhelper(t *testing.T) {}", want: false},
		{name: "method", decl: "func (fixture) TestRule(t *testing.T) {}", want: false},
		{name: "wrong argument", decl: "func TestRule(t int) {}", want: false},
		{name: "two arguments", decl: "func TestRule(t, other *testing.T) {}", want: false},
		{name: "result", decl: "func TestRule(t *testing.T) error { return nil }", want: false},
		{name: "generic", decl: "func TestRule[T any](t *testing.T) {}", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := "package coverage\nimport \"testing\"\ntype fixture struct{}\n" + tc.decl
			file, err := parser.ParseFile(
				token.NewFileSet(),
				tc.name+".go",
				source,
				parser.SkipObjectResolution,
			)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var fn *ast.FuncDecl
			for _, decl := range file.Decls {
				candidate, ok := decl.(*ast.FuncDecl)
				if ok {
					fn = candidate
				}
			}
			if fn == nil {
				t.Fatal("synthetic source has no function declaration")
			}
			got := isRunnableGoTest(
				fn,
				map[string]struct{}{"testing": {}},
				false,
			)
			if got != tc.want {
				t.Fatalf("isRunnableGoTest(%q) = %v, want %v", tc.decl, got, tc.want)
			}
		})
	}
}

func TestDirectlyCalledRuleConstructors_RejectsMentionsAndShadows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		aliases map[string]struct{}
		want    bool
	}{
		{name: "canonical call", body: "NewExampleRule()", want: true},
		{
			name:    "external package selector",
			body:    "cascades.NewExampleRule()",
			aliases: map[string]struct{}{"cascades": {}},
			want:    true,
		},
		{
			name: "unrelated selector",
			body: "other.NewExampleRule()",
			want: false,
		},
		{
			name:    "shadowed package selector",
			body:    "cascades := struct{ NewExampleRule func() }{func() {}}; cascades.NewExampleRule()",
			aliases: map[string]struct{}{"cascades": {}},
			want:    false,
		},
		{name: "identifier mention", body: "_ = NewExampleRule", want: false},
		{name: "type mention", body: "var _ *ExampleRule", want: false},
		{
			name: "local shadow call",
			body: "NewExampleRule := func() {}; NewExampleRule()",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := "package coverage\nfunc TestRule() {\n" + tc.body + "\n}"
			file, err := parser.ParseFile(token.NewFileSet(), tc.name+".go", source, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			fn := file.Decls[0].(*ast.FuncDecl)
			called := directlyCalledRuleConstructors(
				fn,
				map[string]string{"NewExampleRule": "ExampleRule"},
				tc.aliases,
			)
			_, got := called["ExampleRule"]
			if got != tc.want {
				t.Fatalf("constructor call detection for %q = %v, want %v", tc.body, got, tc.want)
			}
		})
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
