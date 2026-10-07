package constfoldcensus_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/constfoldcensus"
)

// assignment is the census of WS-E design 5.4(b): every site under pkg/ that
// simplifies a value or predicate, or evaluates a constant with no row, and why
// it may. A new site fails TestConstantFoldCensus until it is assigned here;
// the count is per (file, function, callee), so an unrelated edit does not
// move it. Reasons:
//   - simplify: a Java simplification consumer (value or predicate set, no
//     constant evaluation since 5.4(b)).
//   - comparand: the sparse-index stored comparand, evaluated as Java's
//     IndexComparison stores `comparison.getComparand(null, null)`.
//   - analysis: reads a constant for a plan property, EXPLAIN text or a bound,
//     rewrites nothing a plan evaluates, and declines on an evaluation error.
//   - IN (4.1), LIKE/STARTS_WITH (section 1): owned by that section.
//   - coercion: the kept walk-time numeric coercions (DIVERGENCES: a Go
//     read-side extension; the constant is evaluated once and the rewrite
//     declines on error).
//   - walk-time NULL: arithmetic over a typed NULL collapses to NULL when the
//     walker builds it (TODO 5.4(g): stays until the pull-up result simplify
//     is ported).
//   - Go-only: a Go translator fold with its own TODO entry.
//   - harness: a conformance oracle, not the planner.
var assignmentTable = []assigned{
	{"pkg/recordlayer/query/plan/cascades/match_info_merge.go", "collectGroupPullUpCompensations", "values.SimplifyValue", 1, "simplify: GroupByExpression's primitive-value simplification"},
	{"pkg/recordlayer/query/plan/cascades/pullup.go", "PullUp.PullUpValueMaybeWithEquivalence", "values.SimplifyValue", 1, "simplify: Values.simplify in a pull-up"},
	{"pkg/recordlayer/query/plan/cascades/predicates/simplifier_predicate_values.go", "SimplifyPredicateValues", "values.SimplifyPredicateValue", 1, "simplify: the predicate value set over every leaf"},
	{"pkg/recordlayer/query/plan/cascades/rule_constant_folding.go", "ValuePredicateSimplificationRule.OnMatch", "predicates.SimplifyPredicateValues", 1, "simplify: Java's ValuePredicateSimplificationRule"},
	{"pkg/recordlayer/query/plan/cascades/rule_eliminate_null_on_empty.go", "foldPredicateAtNull", "cascades.Simplify", 1, "simplify: Java's foldPredicateAtNull over ConstantFoldingRuleSet"},
	{"pkg/recordlayer/query/plan/cascades/rule_query_predicate_simplification.go", "QueryPredicateSimplificationRule.OnMatch", "cascades.Simplify", 1, "simplify: Java's QueryPredicateSimplificationRule"},
	{"pkg/recordlayer/query/plan/cascades/rule_predicate_to_logical_union.go", "newPredicateUnionLegs", "cascades.Simplify", 1, "simplify: the default predicate rules normalize a union leg's factor"},
	{"pkg/relational/core/query/cascades_translator.go", "cascadesTranslator.foldKnownExists", "cascades.Simplify", 1, "Go-only: the translator's decided-EXISTS fold (TranslatorConstantPredicateRules)"},

	{"pkg/recordlayer/query/plan/cascades/predicates/simplifier_predicate_values.go", "EvaluatePredicateComparands", "values.EvaluateConstantComparand", 1, "comparand: the sparse-index predicate's leaves"},
	{"pkg/relational/core/query/ddl/generator_predicate.go", "generatePredicate", "predicates.EvaluatePredicateComparands", 1, "comparand: the sparse-index predicate stored by DDL"},
	{"pkg/recordlayer/query/plan/cascades/values/simplifier_value.go", "simplifyValue", "values.EvaluateConstant", 1, "comparand: evaluate mode only"},
	{"pkg/recordlayer/query/plan/cascades/values/simplifier_value.go", "simplifyChildrenWith", "Evaluate(nil)", 1, "comparand: a PROMOTE over a constant, evaluate mode only"},
	{"pkg/recordlayer/query/plan/cascades/values/simplifier_value.go", "tryCastConstant", "Evaluate(nil)", 1, "comparand: a CAST over a constant, evaluate mode only"},

	{"pkg/recordlayer/query/plan/cascades/values/values.go", "EvaluateConstant", "Evaluate(nil)", 1, "analysis: the constant evaluator itself"},
	{"pkg/recordlayer/query/plan/cascades/predicates/comparisons.go", "Comparison.Eval", "Evaluate(nil)", 1, "analysis: a comparison's plan-time RHS, for its callers"},
	{"pkg/recordlayer/query/plan/cascades/predicates/comparisons.go", "formatComparisonRHS", "values.EvaluateConstant", 1, "analysis: EXPLAIN text of a literal comparand"},
	{"pkg/recordlayer/query/plan/cascades/predicates/range_enclosure.go", "compileTimeComparand", "Evaluate(nil)", 1, "analysis: range enclosure of compile-time comparands"},
	{"pkg/recordlayer/query/plan/cascades/properties/physical_equality_shape.go", "LogicalEqualityAtMostOnePhysicalKey", "values.EvaluateConstant", 1, "analysis: equality shape and cardinality"},
	{"pkg/recordlayer/query/plan/cascades/properties/physical_equality_shape.go", "PhysicalEqualityShapeForComponent", "values.EvaluateConstant", 1, "analysis: equality shape and cardinality"},
	{"pkg/recordlayer/query/plan/cascades/properties/physical_equality_shape.go", "ProvenFullEqualityMultiplicity", "values.EvaluateConstant", 1, "analysis: equality shape and cardinality"},
	{"pkg/recordlayer/query/plan/cascades/properties/physical_equality_shape.go", "comparisonHasUnsupportedKnownNaN", "values.EvaluateConstant", 1, "analysis: a known NaN comparand"},
	{"pkg/recordlayer/query/plan/plans/ordering.go", "EqualityPinsSinglePhysicalKeyOnColumn", "values.EvaluateConstant", 1, "analysis: constant ordering keys"},
	{"pkg/recordlayer/query/plan/plans/ordering.go", "isZeroFloatEqualityRange", "values.EvaluateConstant", 1, "analysis: constant ordering keys"},
	{"pkg/recordlayer/query/plan/plans/cost.go", "vectorScanCardinality", "Evaluate(nil)", 1, "analysis: a vector scan's literal K, for its cost"},
	{"pkg/recordlayer/query/plan/cascades/rule_sink_limit_into_vector_scan.go", "vectorScanAdjustedLimit", "Evaluate(nil)", 1, "analysis: a vector scan's literal K, folding the limit into the scan"},
	{"pkg/relational/core/embedded/logical_qualify.go", "globalRankVectorLimit", "Evaluate(nil)", 1, "analysis: a distance-rank QUALIFY's literal K (a K that does not evaluate is the runtime cap)"},
	{"pkg/recordlayer/query/plan/cascades/rule_match_intermediate.go", "narrowPromotedFloatScanBound", "values.EvaluateConstant", 1, "analysis: a FLOAT index scan bound; the residual keeps the promoted predicate"},

	{"pkg/relational/core/query/expr/expr.go", "Resolver.resolveInList", "Evaluate(nil)", 1, "IN (4.1(a)): ResolveIn's constant fork"},
	{"pkg/relational/core/query/expr/expr.go", "Resolver.resolveInList", "values.EvaluateConstant", 1, "IN (4.1(a)): ENUM promotion of a literal item"},
	{"pkg/relational/core/query/expr/expr.go", "anyInListItemFoldsToNull", "values.EvaluateConstant", 1, "IN (4.1, 5.4(e)): a NULL-typed IN item refused at plan time"},
	{"pkg/recordlayer/query/plan/cascades/rule_implement_in_join.go", "extractInValues", "Evaluate(nil)", 1, "IN (4.1): the literal values source"},
	{"pkg/recordlayer/query/plan/cascades/rule_implement_in_union.go", "ImplementInUnionRule.OnMatch", "Evaluate(nil)", 1, "IN (4.1): the IN-union's plan-time sources"},
	{"pkg/recordlayer/query/plan/cascades/rule_in_to_explode.go", "explodableIn", "Evaluate(nil)", 1, "IN (4.1(b)): the explode's comparand"},
	{"pkg/relational/core/query/expr/expr.go", "Resolver.ResolveStartsWith", "values.EvaluateConstant", 1, "LIKE/STARTS_WITH (section 1): a STARTS_WITH prefix must be constant"},

	{"pkg/relational/core/query/expr/expr.go", "widenConstAgainstDoubleColumn", "values.EvaluateConstant", 1, "coercion: an INT, LONG or FLOAT constant against a DOUBLE column"},
	{"pkg/relational/core/query/expr/expr.go", "narrowFloatConstAgainstInt", "values.EvaluateConstant", 1, "coercion: a FLOAT or DOUBLE constant against an INT or LONG column"},
	{"pkg/relational/core/query/expr/expr.go", "narrowConstAgainstFloatColumn", "values.EvaluateConstant", 1, "coercion: the FLOAT-column narrowing"},
	{"pkg/relational/core/query/expr/expr.go", "isTypedNullConstant", "values.EvaluateConstant", 1, "walk-time NULL: arithmetic over a typed NULL"},

	{"pkg/relational/conformance/rowdiff/oracle.go", "evalLeaf", "Evaluate(nil)", 2, "harness: the rowdiff oracle evaluates its own leaves"},
}

// assigned is one census entry: a site key, its count and its reason.
type assigned struct {
	file, fn, callee string
	count            int
	reason           string
}

// assignment indexes assignmentTable by site, failing on a duplicate key.
func assignment(t *testing.T) map[constfoldcensus.Site]assigned {
	t.Helper()
	out := map[constfoldcensus.Site]assigned{}
	for _, a := range assignmentTable {
		k := constfoldcensus.Site{File: a.file, Func: a.fn, Callee: a.callee}
		if _, dup := out[k]; dup {
			t.Fatalf("duplicate assignment: %s", k)
		}
		if a.count < 1 || a.reason == "" {
			t.Fatalf("assignment %s needs a count and a reason", k)
		}
		out[k] = a
	}
	return out
}

// minFiles is the vacuity floor: a census that parsed fewer files did not
// read the real tree (measured 1113 at introduction).
const minFiles = 800

// repoRoot finds the module root. Under Bazel the test stages //:MODULE.bazel
// and follows its runfiles symlink to the real tree, because the census must
// see every package, not only declared inputs; the target is tagged
// `external` so a cached pass never hides a new site. Under `go test` it walks
// up from the package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	if sd, ws := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); sd != "" && ws != "" {
		if real, err := filepath.EvalSymlinks(filepath.Join(sd, ws, "MODULE.bazel")); err == nil {
			return filepath.Dir(real)
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "MODULE.bazel")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("MODULE.bazel not found: the census cannot read the tree")
		}
		dir = parent
	}
}

func TestConstantFoldCensus(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	var all []constfoldcensus.Site
	files := 0
	err := filepath.WalkDir(filepath.Join(root, "pkg"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !constfoldcensus.InPopulation(rel) {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sites, err := constfoldcensus.ScanSource(rel, src)
		if err != nil {
			return err
		}
		files++
		all = append(all, sites...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < minFiles {
		t.Fatalf("parsed %d files under %s/pkg, want at least %d: not the real tree", files, root, minFiles)
	}
	t.Logf("census population: %d files, %d sites", files, len(all))
	found := constfoldcensus.Count(all)
	assignment := assignment(t)
	for _, s := range constfoldcensus.Sorted(found) {
		a, ok := assignment[s]
		if !ok {
			t.Errorf("unassigned constant-fold site (%d): %s -- assign it a reason in census_test.go (WS-E design 5.4(b))", found[s], s)
			continue
		}
		if a.count != found[s] {
			t.Errorf("%s: %d sites, assigned %d", s, found[s], a.count)
		}
	}
	for s := range assignment {
		if found[s] == 0 {
			t.Errorf("assigned site not found (positive control): %s", s)
		}
	}
}

// TestCensusMatcher drives the matcher over synthetic sources: what is a site
// and what is not.
func TestCensusMatcher(t *testing.T) {
	t.Parallel()
	const src = `package p
func f(x X, ctx any) {
	values.SimplifyValue(v)        // site
	g(values.EvaluateConstantComparand) // site: a function value
	x.Evaluate(nil)                // site
	x.Eval(nil)                    // site
	x.Eval(ctx)                    // not a site: a row
	// x.Eval(nil) in a comment is not a site
	SimplifyValue(v)               // not a site outside the values package
	r.Simplify(p)                  // not a site: a method, not cascades.Simplify
	cascades.Simplify(p, rules)    // site
}`
	sites, err := constfoldcensus.ScanSource("pkg/other/p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, s := range sites {
		got[s.Callee]++
	}
	want := map[string]int{
		"values.SimplifyValue": 1, "values.EvaluateConstantComparand": 1,
		"Evaluate(nil)": 1, "Eval(nil)": 1, "cascades.Simplify": 1,
	}
	if len(got) != len(want) {
		t.Fatalf("sites %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: %d sites, want %d (all: %v)", k, got[k], n, got)
		}
	}
	inValues, err := constfoldcensus.ScanSource("pkg/recordlayer/query/plan/cascades/values/v.go",
		[]byte("package values\nfunc f() { SimplifyValue(v) }"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inValues) != 1 || inValues[0].Callee != "values.SimplifyValue" || inValues[0].Func != "f" {
		t.Errorf("an unqualified call in the defining package: %v", inValues)
	}
	for file, in := range map[string]bool{
		"pkg/a/b.go": true, "pkg/a/b_test.go": false, "pkg/a/b.pb.go": false,
		"pkg/a/testdata/c.go": false, "cmd/x/main.go": false,
	} {
		if constfoldcensus.InPopulation(file) != in {
			t.Errorf("InPopulation(%s) = %v, want %v", file, !in, in)
		}
	}
}
