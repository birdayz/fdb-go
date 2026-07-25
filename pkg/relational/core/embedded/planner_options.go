package embedded

import (
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer"
	cascades "fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/relational/api"
)

// plannerOptions is the planner-facing projection of api.Options, resolved
// once into the two things the Cascades planner consumes: a disabled rule-name
// set and a PlannerConfiguration.
//
// Java's relational `PlannerConfiguration.of(readableIndexes, options)` reads
// FOUR option names. Three are handled here — DISABLED_PLANNER_RULES,
// DISABLE_PLANNER_REWRITING and PLAN_RIGHT_DEEP. The fourth,
// INDEX_FETCH_METHOD (via OptionsUtils.getIndexFetchMethod), selects FDB's
// remote-fetch index scan, which Go has no implementation of at any layer:
// neither FDB client Go uses exposes getMappedRange, and Go's index-scan plan
// has no fetch-method field to carry it. api.OptIndexFetchMethod is therefore
// accepted and NOT honored; that gap is tracked in TODO.md rather than papered
// over with a config field nothing reads.
//
// plannerOptionsFrom is the only place that reads the option names, and
// newCascadesPlanner is the only place that turns the result into a planner.
// Both take the whole plannerOptions value, so no caller of the funnel can
// honor one option and drop another. That is a single-funnel convention, not a
// type-system guarantee: cascades.NewPlanner is exported, so a new file in this
// package could still bypass newCascadesPlanner and get the defaults silently.
// Route new planner construction through the funnel.
type plannerOptions struct {
	// disabledRules is the union of the user's DISABLED_PLANNER_RULES and,
	// when DISABLE_PLANNER_REWRITING is set, the optional rewriting rules.
	// Java merges the two into the same disabledTransformationRules set
	// (setDisabledTransformationRuleNames followed by disableRewritingRules,
	// which itself calls disableTransformationRule per rule), so they are one
	// field here too rather than two flags the planner would have to
	// re-combine. Names use Java's simple-class-name spelling.
	disabledRules map[string]struct{}

	// config is the Cascades PlannerConfiguration these options imply. Only
	// ShouldJoinRightDeep is option-driven today; the rest are the Java
	// defaults, exactly as Java's buildRecordQueryPlannerConfiguration leaves
	// everything it does not set at RecordQueryPlannerConfiguration's default.
	config cascades.PlannerConfiguration
}

// plannerOptionsFrom resolves the connection's api.Options into the planner's
// view of them. Ports the option-reading half of Java's
// `PlannerConfiguration.of` + `buildRecordQueryPlannerConfiguration`
// (fdb-relational-core .../query/PlannerConfiguration.java) — the three of its
// four option names Go can act on; see the plannerOptions doc for the fourth,
// INDEX_FETCH_METHOD, which Go has no implementation for.
//
// A nil/empty Options yields the Java defaults (no disabled rules, rewriting
// on, right-deep off), so the no-options path plans exactly as before.
//
// Unrecognized rule names are carried through rather than rejected: Java's
// setDisabledTransformationRuleNames copies the strings verbatim and never
// resolves them against the rule set, so a name that matches nothing is inert
// in both engines. Rejecting here would fail queries Java accepts.
func plannerOptionsFrom(o *api.Options) plannerOptions {
	po := plannerOptions{config: cascades.DefaultPlannerConfiguration()}
	if o == nil {
		return po
	}

	names := optStringSet(o, api.OptDisabledPlannerRules)
	if optBool(o, api.OptDisablePlannerRewriting, false) {
		// Java's disableRewritingRules(): fold RewritingRuleSet.OPTIONAL_RULES
		// into the same disabled set. FinalizeExpressionsRule is deliberately
		// NOT in that set — it is what stamps an expression FINAL, and
		// planning cannot proceed without it.
		//
		// The option is near-total in Java and partial in Go, and that is not a
		// divergence. Java's RewritingRuleSet has six rules, so disabling five
		// of them empties the phase; Go runs the default expression rules in
		// REWRITING as well, so the same five are five out of roughly forty.
		// Both engines disable BY NAME through the same isRuleEnabled filter and
		// the named set is identical, so the option means the same thing in
		// both: "turn off the optional rewrites". Go's extra REWRITING rules are
		// normalization rules Java also runs, just from its PlanningRuleSet
		// rather than its RewritingRuleSet — a phase-placement difference that
		// predates this option and is not something the option should paper over
		// by disabling rules Java leaves enabled.
		for _, n := range cascades.OptionalRewritingRuleNames() {
			names[n] = struct{}{}
		}
	}
	if len(names) > 0 {
		po.disabledRules = names
	}

	po.config.ShouldJoinRightDeep = optBool(o, api.OptPlanRightDeep, false)
	return po
}

// cacheKeyPart renders the resolved planner options into a plan-cache key
// component. Java's QueryCacheKey carries the whole PlannerConfiguration for
// this reason: without it, changing a planner option on a connection would
// keep returning the plan built under the previous options — a wrong-plan bug,
// not merely a stale-cost one.
//
// The all-defaults case renders the empty string, which planCacheScope treats
// as "emit no component at all" — so a caller who sets no options gets exactly
// the scope it got before planner options were keyed.
func (p plannerOptions) cacheKeyPart() string {
	if len(p.disabledRules) == 0 && !p.config.ShouldJoinRightDeep {
		return ""
	}
	names := make([]string, 0, len(p.disabledRules))
	for n := range p.disabledRules {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	if p.config.ShouldJoinRightDeep {
		b.WriteString("rd")
	}
	for _, n := range names {
		b.WriteString(",")
		b.WriteString(n)
	}
	return b.String()
}

// newCascadesPlanner builds the Cascades planner for a user query. It is the
// ONLY planner-construction site in the embedded engine, so the planner
// options cannot be honored on one query path and silently dropped on
// another: the rule set, the disabled-rule filter and the PlannerConfiguration
// all come from the same plannerOptions value.
//
// planningRules are the phase-PLANNING expression rules the callsite needs
// (BatchA for SELECT, BatchA + DML wrappers for DML).
func newCascadesPlanner(
	md *recordlayer.RecordMetaData,
	popts plannerOptions,
	planningRules []cascades.ExpressionRule,
	stats properties.StatisticsProvider,
) *cascades.Planner {
	rules := cascades.DefaultExpressionRules()
	rules = append(rules, cascades.RewritingRules()...)
	planCtx := buildCascadesPlanContext(md, popts.config)
	planner := cascades.NewPlanner(rules, planCtx).
		WithImplementationRules(cascades.DefaultImplementationRules()).
		WithPlanningExpressionRules(planningRules).
		WithStatistics(stats).
		WithMaxTasks(100_000)
	planner.DisabledRules = popts.disabledRules
	return planner
}

// optBool reads an option as a bool, returning fallback when the option is
// absent or of an unexpected type. Companion to optInt64.
func optBool(opts *api.Options, name api.OptionName, fallback bool) bool {
	if v, ok := opts.Get(name).(bool); ok {
		return v
	}
	return fallback
}

// optStringSet reads a collection-typed option into a set. Java's
// DISABLED_PLANNER_RULES contract is CollectionContract<String>; Go stores it
// as []string, which is the only slice-typed option value the api package
// produces (see api.defaultOptionValues). Any other dynamic type is a caller
// bug, and it is handled the same way optBool handles a non-bool: fall back to
// the default rather than guess. There is no properties/DSN decoder in the
// repo today that could hand us a []any, so no speculative arm exists for one;
// if one is added it must set the declared type, not a lookalike.
//
// Two classes of entry are dropped, both inert by construction:
//
//   - blank names, which can never match a rule; and
//   - names containing planCacheScopeDelim. A Go rule's simple type name is a
//     Go identifier and can hold no control character, so such a name never
//     matches a rule either — but these names ARE user-controlled and they flow
//     into the plan-cache scope through cacheKeyPart. Letting one through would
//     make the scope non-injective: a delimiter inside the options component is
//     indistinguishable from the component boundary, so
//     (schema "S", DISABLED_PLANNER_RULES ["\x010"]) and
//     (schema "S\x010\x01,", no options) render the same scope and collide in
//     the plan cache. The pre-option scope was unconditionally injective —
//     the version is always digits, so a parse from the right always resolves —
//     and this user-controlled component is what introduced the corner, so it
//     is closed here rather than documented.
func optStringSet(opts *api.Options, name api.OptionName) map[string]struct{} {
	out := map[string]struct{}{}
	names, ok := opts.Get(name).([]string)
	if !ok {
		return out
	}
	for _, s := range names {
		if s = strings.TrimSpace(s); s == "" || strings.Contains(s, planCacheScopeDelim) {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}
