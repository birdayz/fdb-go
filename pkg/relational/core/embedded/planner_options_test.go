package embedded

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"testing"

	cascades "fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser"
	"fdb.dev/pkg/relational/core/query"
)

// planWithOptions plans one SELECT through the SAME construction site the
// production SELECT path uses (newCascadesPlanner over the connection's
// resolved plannerOptions), returning the winning physical plan and the task
// count. It exists so the option plumbing can be exercised without a live
// store: everything from plannerOptionsFrom onward is the production code.
func planWithOptions(t *testing.T, sql, schemaDDL string, opts *api.Options) (plans.RecordQueryPlan, int, error) {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(schemaDDL)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()

	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse SQL: %v", err)
	}
	q := root.Statements().AllStatement()[0].SelectStatement().Query()
	logicalOp, buildErr := NewPlanVisitor(md).VisitQuery(q)
	if buildErr != nil {
		t.Fatalf("build logical: %v", buildErr)
	}
	if err := resolveQualifiedTableNames(logicalOp, "s"); err != nil {
		t.Fatalf("resolve table names: %v", err)
	}
	ref, _, translateErr := query.TranslateToCascadesWithError(logicalOp, md)
	if translateErr != nil {
		t.Fatalf("translate: %v", translateErr)
	}

	planner := newCascadesPlanner(md, plannerOptionsFrom(opts), cascades.BatchAExpressionRules(), nil)
	best, tasks, planErr := planner.PlanWithContext(context.Background(), ref)
	if planErr != nil {
		return nil, tasks, planErr
	}
	ph, ok := best.(interface {
		GetRecordQueryPlan() plans.RecordQueryPlan
	})
	if !ok {
		t.Fatalf("winner is not a physical plan: %T", best)
	}
	return ph.GetRecordQueryPlan(), tasks, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func explainWithOptions(t *testing.T, sql, schemaDDL string, opts *api.Options) string {
	t.Helper()
	plan, _, err := planWithOptions(t, sql, schemaDDL, opts)
	if err != nil {
		t.Fatalf("planning %q: %v", sql, err)
	}
	return plan.Explain()
}

// indexedTableDDL gives the planner a real access-path choice: without index
// matching, `WHERE a = ?` can only be a full scan plus a residual filter.
const indexedTableDDL = `CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, c STRING, PRIMARY KEY (id))
CREATE INDEX idx_a ON T(a)
CREATE INDEX idx_ab ON T(a, b)`

// starJoinDDL is the SQL spelling of the all-live star the Cascades-level
// budget gate uses (TestOrdinalStarRightDeepBudget): a hub joined to n
// identical spokes, every spoke projected so none can be pruned.
const starJoinDDL = `CREATE TABLE H (id BIGINT, v BIGINT, PRIMARY KEY (id))
CREATE TABLE S1 (id BIGINT, hid BIGINT, PRIMARY KEY (id))
CREATE TABLE S2 (id BIGINT, hid BIGINT, PRIMARY KEY (id))
CREATE TABLE S3 (id BIGINT, hid BIGINT, PRIMARY KEY (id))
CREATE TABLE S4 (id BIGINT, hid BIGINT, PRIMARY KEY (id))
CREATE TABLE S5 (id BIGINT, hid BIGINT, PRIMARY KEY (id))`

// fiveSpokeStarSQL is the hub+5 all-live star. Its Cascades twin
// (buildOrdinalStar(5)) is the narrowest all-live star that exhausts the
// 100,000-task budget at default settings.
const fiveSpokeStarSQL = "SELECT H.id, S1.id, S2.id, S3.id, S4.id, S5.id " +
	"FROM H, S1, S2, S3, S4, S5 " +
	"WHERE H.id = S1.hid AND H.id = S2.hid AND H.id = S3.hid AND H.id = S4.hid AND H.id = S5.hid"

// TestPlannerOptions_PlanRightDeep pins PLAN_RIGHT_DEEP end to end from
// api.Options to the join enumeration, on the shape CQ-9 named: an all-live
// star that exhausts the planning budget at DEFAULT settings — in Java too,
// whose PLAN_RIGHT_DEEP also defaults to false — and converges once the
// caller opts in.
func TestPlannerOptions_PlanRightDeep(t *testing.T) {
	t.Parallel()

	_, tasks, err := planWithOptions(t, fiveSpokeStarSQL, starJoinDDL, nil)
	if !errors.Is(err, cascades.ErrPlannerCapHit) {
		t.Fatalf("hub+5 star at default options: err=%v (tasks=%d), want the task cap — "+
			"if the budget now covers this shape, widen the star rather than weakening the test",
			err, tasks)
	}

	rightDeep := api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true).Build()
	plan, rdTasks, rdErr := planWithOptions(t, fiveSpokeStarSQL, starJoinDDL, rightDeep)
	if rdErr != nil {
		t.Fatalf("hub+5 star with PLAN_RIGHT_DEEP: %v (tasks=%d) — the option is not reaching "+
			"PartitionSelectRule", rdErr, rdTasks)
	}
	if plan == nil {
		t.Fatal("PLAN_RIGHT_DEEP converged but produced no plan")
	}
	if rdTasks >= 100_000 {
		t.Fatalf("PLAN_RIGHT_DEEP tasks=%d is not under the 100,000 cap", rdTasks)
	}
	t.Logf("hub+5 star: default CAPS; PLAN_RIGHT_DEEP converges in %d tasks", rdTasks)

	// Explicit false must behave exactly like unset — the default is
	// Java-identical and setting it must not be a way to change it.
	if _, _, offErr := planWithOptions(t, fiveSpokeStarSQL, starJoinDDL,
		api.NewOptionsBuilder().Set(api.OptPlanRightDeep, false).Build()); !errors.Is(offErr, cascades.ErrPlannerCapHit) {
		t.Fatalf("PLAN_RIGHT_DEEP=false: err=%v, want the same cap the unset default hits", offErr)
	}
}

// TestPlannerOptions_DisabledPlannerRules pins DISABLED_PLANNER_RULES against
// a NAMED rule, with a plan-shape difference rather than a "did not crash".
// Disabling MatchLeafRule removes index-candidate matching, so the only plan
// left for an indexed equality is a full scan with a residual filter — the
// access path changes while the answer does not.
//
// The rule is named the way Java names it (`getClass().getSimpleName()`), so
// the same option value is portable between the engines.
func TestPlannerOptions_DisabledPlannerRules(t *testing.T) {
	t.Parallel()
	const sql = "SELECT id, c FROM T WHERE a = 1"

	base := explainWithOptions(t, sql, indexedTableDDL, nil)
	const wantBase = "Project([ID#0, C#3], Fetch(IndexScan(IDX_A, [=])))"
	if base != wantBase {
		t.Fatalf("default plan = %q, want %q — the fixture must start from an INDEX plan for "+
			"the disabled-rule contrast to mean anything", base, wantBase)
	}

	disabled := api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}).Build()
	got := explainWithOptions(t, sql, indexedTableDDL, disabled)
	const wantDisabled = "Project([ID#0, C#3], PredicatesFilter(Scan(T), [1 preds]))"
	if got != wantDisabled {
		t.Fatalf("with MatchLeafRule disabled, plan = %q, want %q — the option is accepted and "+
			"ignored if the plan is unchanged", got, wantDisabled)
	}

	// A name that matches no rule is inert, not an error: Java's
	// setDisabledTransformationRuleNames copies the strings verbatim and never
	// resolves them, so rejecting here would fail queries Java accepts.
	unknown := api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"NoSuchRule"}).Build()
	if got := explainWithOptions(t, sql, indexedTableDDL, unknown); got != base {
		t.Fatalf("an unknown rule name changed the plan: %q", got)
	}

	// The Go %T spelling must NOT work: accepting it would mean two spellings
	// for one concept, and the package-qualified one has no Java counterpart.
	goSpelling := api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"*cascades.MatchLeafRule"}).Build()
	if got := explainWithOptions(t, sql, indexedTableDDL, goSpelling); got != base {
		t.Fatalf("the Go %%T spelling disabled a rule (%q) — rule names are Java simple class "+
			"names in both engines", got)
	}
}

// TestPlannerOptions_DisablePlannerRewriting pins DISABLE_PLANNER_REWRITING
// two ways: the rule SET it turns off is exactly Java's
// RewritingRuleSet.OPTIONAL_RULES, and turning it off visibly changes a plan.
//
// The visible case is a LEFT OUTER join. RewriteOuterJoinRule canonicalizes it
// into a correlated null-supplying form the data-access path can drive off an
// index; with rewriting disabled the planner is left with a plain nested-loop
// outer join over two full scans. Same rows, different plan — which is the
// point: this option is a planning stop-gap, not a semantic switch.
func TestPlannerOptions_DisablePlannerRewriting(t *testing.T) {
	t.Parallel()

	t.Run("disables_exactly_javas_optional_rewriting_rules", func(t *testing.T) {
		t.Parallel()

		// A LITERAL transcription of Java's RewritingRuleSet.OPTIONAL_RULES, not
		// a re-derivation from the Go function under test. OPTIONAL_RULES is
		// ALL_EXPRESSION_RULES minus FinalizeExpressionsRule, where
		// ALL_EXPRESSION_RULES = PREORDER{} + EXPLORATION{
		// QueryPredicateSimplificationRule, PredicatePushDownRule,
		// DecorrelateValuesRule, RewriteOuterJoinRule} + IMPLEMENTATION{
		// SelectMergeRule, FinalizeExpressionsRule} (RewritingRuleSet.java).
		//
		// Comparing against cascades.OptionalRewritingRuleNames() would be
		// circular — that function is derived from Go's RewritingRules(), which
		// is exactly where a future Go-only rewriting rule would land, so the
		// comparison would keep passing while the two engines drifted apart.
		// With the literal here, adding a rule to Go's REWRITING set reds this
		// test and forces a deliberate decision about whether Java disables it
		// too.
		want := []string{
			"DecorrelateValuesRule",
			"PredicatePushDownRule",
			"QueryPredicateSimplificationRule",
			"RewriteOuterJoinRule",
			"SelectMergeRule",
		}

		po := plannerOptionsFrom(api.NewOptionsBuilder().
			Set(api.OptDisablePlannerRewriting, true).Build())
		got := make([]string, 0, len(po.disabledRules))
		for n := range po.disabledRules {
			got = append(got, n)
		}
		sort.Strings(got)
		if !equalStrings(got, want) {
			t.Fatalf("DISABLE_PLANNER_REWRITING disabled %v, want Java's OPTIONAL_RULES %v", got, want)
		}

		// The helper the production path uses must agree with that literal too,
		// so a Go-side rule-set change cannot slip past by only touching the
		// helper.
		derived := append([]string(nil), cascades.OptionalRewritingRuleNames()...)
		sort.Strings(derived)
		if !equalStrings(derived, want) {
			t.Fatalf("cascades.OptionalRewritingRuleNames() = %v, want Java's OPTIONAL_RULES %v — "+
				"a rule was added to Go's REWRITING set; confirm against Java's RewritingRuleSet "+
				"before widening the literal", derived, want)
		}

		// FinalizeExpressionsRule stays ON — it is what stamps an expression
		// FINAL, and Java's OPTIONAL_RULES excludes it for that reason.
		if _, off := po.disabledRules["FinalizeExpressionsRule"]; off {
			t.Fatal("FinalizeExpressionsRule must never be disabled; planning cannot proceed without it")
		}
	})

	t.Run("changes_the_plan", func(t *testing.T) {
		t.Parallel()
		const sql = "SELECT T.id FROM T LEFT JOIN T AS U ON T.a = U.a"

		base := explainWithOptions(t, sql, indexedTableDDL, nil)
		const wantBase = "Project([T.ID#0], FlatMap(outer=Scan(T), inner=DefaultOnEmpty(Fetch(IndexScan(IDX_AB, [=, *])))))"
		if base != wantBase {
			t.Fatalf("default plan = %q, want %q — the fixture must start from the REWRITTEN "+
				"outer join for the contrast to mean anything", base, wantBase)
		}

		off := api.NewOptionsBuilder().Set(api.OptDisablePlannerRewriting, true).Build()
		got := explainWithOptions(t, sql, indexedTableDDL, off)
		const wantOff = "Project([T.ID#0], NestedLoopJoin(LEFT OUTER, [1 preds], Scan(T), Scan(T)))"
		if got != wantOff {
			t.Fatalf("with rewriting disabled, plan = %q, want %q — the option is accepted and "+
				"ignored if the plan is unchanged", got, wantOff)
		}
	})

	t.Run("unions_with_explicitly_disabled_rules", func(t *testing.T) {
		t.Parallel()
		// Java feeds both DISABLED_PLANNER_RULES and disableRewritingRules()
		// into ONE disabledTransformationRules set; neither must clobber the
		// other.
		po := plannerOptionsFrom(api.NewOptionsBuilder().
			Set(api.OptDisablePlannerRewriting, true).
			Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}).Build())
		if _, off := po.disabledRules["MatchLeafRule"]; !off {
			t.Fatal("an explicitly disabled rule was lost when DISABLE_PLANNER_REWRITING was also set")
		}
		for _, n := range cascades.OptionalRewritingRuleNames() {
			if _, off := po.disabledRules[n]; !off {
				t.Fatalf("rewriting rule %s was lost when DISABLED_PLANNER_RULES was also set", n)
			}
		}
	})
}

// TestPlannerOptions_Defaults pins that the no-options path is byte-identical
// to the Java defaults. A default that drifted would change every plan in the
// engine, so it gets its own assertion rather than riding on the tests above.
func TestPlannerOptions_Defaults(t *testing.T) {
	t.Parallel()

	for name, po := range map[string]plannerOptions{
		"nil":         plannerOptionsFrom(nil),
		"no options":  plannerOptionsFrom(api.NoOptions()),
		"empty build": plannerOptionsFrom(api.NewOptionsBuilder().Build()),
	} {
		if len(po.disabledRules) != 0 {
			t.Errorf("%s: disabled rules = %v, want none", name, po.disabledRules)
		}
		if po.config != cascades.DefaultPlannerConfiguration() {
			t.Errorf("%s: config = %+v, want the Cascades default", name, po.config)
		}
		if po.config.ShouldJoinRightDeep {
			t.Errorf("%s: right-deep must default OFF (Java's JOIN_RIGHT_DEEP_MASK is unset)", name)
		}
		if !po.config.ShouldDeferCrossProducts {
			t.Errorf("%s: cross-product deferral must default ON (Java inverts DONT_DEFER_CROSS_PRODUCTS_MASK)", name)
		}
		if part := po.cacheKeyPart(); part != "" {
			t.Errorf("%s: default cache key part = %q, want empty so the default key is unchanged", name, part)
		}
	}
}

// TestPlanCacheScope_DefaultKeyUnchanged pins the claim that keying the plan
// cache by planner options changed NOTHING for a caller who sets none. The
// assertion is on the RENDERED scope against the literal pre-change form, not
// on cacheKeyPart() being empty: an empty part still appended a trailing
// delimiter in the first cut of this change, which the part-is-empty check
// could not see. A user who sets no options must get byte-for-byte the key
// they got before.
func TestPlanCacheScope_DefaultKeyUnchanged(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		schema  string
		version int
	}{
		{"S", 0},
		{"SCHEMA_A", 17},
		{"", 0},
	} {
		// The pre-change rendering, spelled out rather than computed, so a
		// change to planCacheScope cannot silently redefine "unchanged".
		want := tc.schema + planCacheScopeDelim + strconv.Itoa(tc.version)
		got := planCacheScope(tc.schema, tc.version, plannerOptionsFrom(nil).cacheKeyPart())
		if got != want {
			t.Errorf("default scope for (%q, %d) = %q (len %d), want %q (len %d) — keying planner "+
				"options must not change the key for callers who set none",
				tc.schema, tc.version, got, len(got), want, len(want))
		}
	}

	// And a scope WITH options must differ from the default one, or the
	// identity above would be bought by dropping the component entirely.
	rightDeep := plannerOptionsFrom(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true).Build())
	withOpts := planCacheScope("S", 0, rightDeep.cacheKeyPart())
	if withOpts == planCacheScope("S", 0, plannerOptionsFrom(nil).cacheKeyPart()) {
		t.Fatalf("a scope with PLAN_RIGHT_DEEP set (%q) collides with the default scope", withOpts)
	}
}

// TestPlanCacheScope_InjectiveWithOptions pins that adding a USER-CONTROLLED
// component to the plan-cache scope did not cost it injectivity.
//
// The scope was unconditionally injective before planner options were keyed:
// its only variable parts were the schema and a metadata version that is always
// digits, so a parse from the right always resolves. DISABLED_PLANNER_RULES is
// the first component whose content a caller chooses freely, and a delimiter
// byte inside a rule name would be indistinguishable from the component
// boundary. optStringSet therefore drops such names, and this is the exact
// collision pair that would otherwise result — two DIFFERENT (schema, options)
// inputs rendering one scope, which is a plan-cache collision.
//
// Dropping them is free: a rule name is matched against a Go simple type name,
// which is an identifier and can hold no control character, so a name
// containing the delimiter could never have disabled anything.
func TestPlanCacheScope_InjectiveWithOptions(t *testing.T) {
	t.Parallel()

	poisoned := plannerOptionsFrom(api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{planCacheScopeDelim + "0"}).Build())
	// Left side: schema "S", version 0, a rule name carrying the delimiter.
	left := planCacheScope("S", 0, poisoned.cacheKeyPart())
	// Right side: a schema that spells out the left side's own rendering, with
	// no options at all.
	right := planCacheScope("S"+planCacheScopeDelim+"0"+planCacheScopeDelim+",", 0,
		plannerOptionsFrom(nil).cacheKeyPart())

	if left == right {
		t.Fatalf("plan-cache scope is not injective: a delimiter-bearing rule name and a "+
			"delimiter-bearing schema render the same scope %q — two different queries would "+
			"share one cache entry", left)
	}

	// The mechanism, asserted directly so the property above cannot be
	// satisfied by accident: the poisoned name is dropped, leaving the
	// all-defaults scope.
	if _, kept := poisoned.disabledRules[planCacheScopeDelim+"0"]; kept {
		t.Fatal("a rule name containing the scope delimiter was kept; it can never match a rule " +
			"and it breaks scope injectivity")
	}
	if got := planCacheScope("S", 0, poisoned.cacheKeyPart()); got != planCacheScope("S", 0, "") {
		t.Fatalf("scope with only a dropped rule name = %q, want the all-defaults scope %q",
			got, planCacheScope("S", 0, ""))
	}

	// A legitimate rule name alongside a poisoned one must survive — the drop
	// must be surgical, not "reject the whole option".
	mixed := plannerOptionsFrom(api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{planCacheScopeDelim + "x", "MatchLeafRule"}).Build())
	if _, kept := mixed.disabledRules["MatchLeafRule"]; !kept {
		t.Fatal("a valid rule name was dropped alongside a delimiter-bearing one")
	}
	if len(mixed.disabledRules) != 1 {
		t.Fatalf("disabled set = %v, want only MatchLeafRule", mixed.disabledRules)
	}
}

// TestPlannerOptions_CacheKeyPart pins that the plan-cache key separates plans
// built under different planner options. Without this, changing an option on a
// connection would keep serving the plan built under the previous options — a
// wrong-plan bug, which is why Java's QueryCacheKey carries the whole
// PlannerConfiguration.
func TestPlannerOptions_CacheKeyPart(t *testing.T) {
	t.Parallel()

	part := func(b *api.OptionsBuilder) string {
		return plannerOptionsFrom(b.Build()).cacheKeyPart()
	}
	base := part(api.NewOptionsBuilder())
	rightDeep := part(api.NewOptionsBuilder().Set(api.OptPlanRightDeep, true))
	oneRule := part(api.NewOptionsBuilder().Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}))
	noRewrite := part(api.NewOptionsBuilder().Set(api.OptDisablePlannerRewriting, true))
	both := part(api.NewOptionsBuilder().
		Set(api.OptPlanRightDeep, true).
		Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule"}))

	seen := map[string]string{}
	for label, got := range map[string]string{
		"default":    base,
		"right-deep": rightDeep,
		"one rule":   oneRule,
		"no rewrite": noRewrite,
		"both":       both,
	} {
		if prev, dup := seen[got]; dup {
			t.Fatalf("%q and %q share cache key part %q — one would serve the other's plan", label, prev, got)
		}
		seen[got] = label
	}

	// Stable across orderings: a set rendered from a Go map must not make the
	// same options miss the cache on the next statement.
	a := part(api.NewOptionsBuilder().Set(api.OptDisabledPlannerRules, []string{"MatchLeafRule", "SelectMergeRule"}))
	b := part(api.NewOptionsBuilder().Set(api.OptDisabledPlannerRules, []string{"SelectMergeRule", "MatchLeafRule"}))
	if a != b {
		t.Fatalf("cache key part is order-dependent: %q vs %q", a, b)
	}
}

// TestPlannerOptions_CacheKeyPart_Injective pins that cacheKeyPart cannot
// collide two DIFFERENT disabled-rule sets into the same string. Before the
// rendering was length-prefixed, names were joined with a bare comma, so two
// real rule names given as separate entries and one inert (unrecognized, and
// therefore never rejected — see optStringSet) name that merely CONTAINS a
// comma rendered identically: {"PredicatePushDownRule", "SelectMergeRule"}
// and {"PredicatePushDownRule,SelectMergeRule"} both produced
// ",PredicatePushDownRule,SelectMergeRule". A caller who set the latter would
// then get served a plan built with BOTH real rules disabled — a wrong-plan
// bug from the plan cache, not merely a missed optimization.
func TestPlannerOptions_CacheKeyPart_Injective(t *testing.T) {
	t.Parallel()

	twoRules := plannerOptionsFrom(api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"PredicatePushDownRule", "SelectMergeRule"}).Build()).
		cacheKeyPart()
	oneCommaJoinedName := plannerOptionsFrom(api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"PredicatePushDownRule,SelectMergeRule"}).Build()).
		cacheKeyPart()

	if twoRules == oneCommaJoinedName {
		t.Fatalf("cache key part collides two different disabled-rule sets: "+
			"{PredicatePushDownRule, SelectMergeRule} and {PredicatePushDownRule,SelectMergeRule} "+
			"both render %q", twoRules)
	}
}
