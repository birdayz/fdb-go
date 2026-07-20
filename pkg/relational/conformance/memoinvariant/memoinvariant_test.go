package memoinvariant

import (
	"fmt"
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/conformance/rowdiff"
	"fdb.dev/pkg/relational/core/embedded"
)

// ---------------------------------------------------------------------------
// Memo invariants asserted over every extracted plan tree.
//
// These are the plan-side counterparts of what CheckPlanReachability asserts at
// yield time over the memo. They are deliberately structural (typed nodes,
// EqualsPlanWithoutChildren / HashCodeWithoutChildren), never rendered EXPLAIN —
// Explain is lossy and hides the deciding field (RFC-183).
// ---------------------------------------------------------------------------

// planTypeName is the short type label used in violation messages and the arity
// allowlist. Mirrors plan_reachability.go's planTypeName.
func planTypeName(p plans.RecordQueryPlan) string {
	return fmt.Sprintf("%T", p)
}

// arityViolations asserts the plan-side of RFC-183's ReasonNoQuantifier
// invariant: a node reporting K children must report K quantifiers. On a
// fully-linked plan (RFC-183 P5) GetChildren derives from the quantifiers, so
// the two agree by construction; a divergence means a node stored its child
// edge in a second, un-memoized location — exactly the dual-storage class W2
// exists to make unrepresentable.
//
// allow is the set of planTypeName values exempted because they model no
// quantifier for a plan child BY DESIGN (the ReasonNoQuantifier adapter
// population). Anything not in allow with a child/quantifier mismatch is a
// violation.
func arityViolations(root plans.RecordQueryPlan, allow map[string]bool) []string {
	var out []string
	plans.Walk(root, func(n plans.RecordQueryPlan) bool {
		nc := len(n.GetChildren())
		nq := len(n.GetQuantifiers())
		if nc != nq && !allow[planTypeName(n)] {
			out = append(out, fmt.Sprintf("%s reports %d children but %d quantifiers", planTypeName(n), nc, nq))
		}
		return true
	})
	return out
}

// identityHashViolations asserts the memo dedup invariant over one plan tree:
// any two nodes that are EqualsPlanWithoutChildren-equal MUST share
// HashCodeWithoutChildren (else the memo would bucket equal expressions into
// different groups, or fail to dedup them), and HashCodeWithoutChildren must be
// deterministic (same node hashed twice yields the same value — a hash that
// drifts corrupts grouping across a single planning run).
//
// Pairwise within one tree is bounded (plan trees have tens of nodes) and is
// where the invariant bites: a set operation legitimately holds two structurally
// identical legs, a self-join two identical scans.
func identityHashViolations(root plans.RecordQueryPlan) []string {
	var nodes []plans.RecordQueryPlan
	plans.Walk(root, func(n plans.RecordQueryPlan) bool {
		nodes = append(nodes, n)
		return true
	})
	var out []string
	for _, n := range nodes {
		if h1, h2 := n.HashCodeWithoutChildren(), n.HashCodeWithoutChildren(); h1 != h2 {
			out = append(out, fmt.Sprintf("%s: HashCodeWithoutChildren nondeterministic (%d != %d)", planTypeName(n), h1, h2))
		}
	}
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			if nodes[i].EqualsPlanWithoutChildren(nodes[j]) &&
				nodes[i].HashCodeWithoutChildren() != nodes[j].HashCodeWithoutChildren() {
				out = append(out, fmt.Sprintf(
					"%s / %s are EqualsPlanWithoutChildren-equal but hash differently (%d != %d)",
					planTypeName(nodes[i]), planTypeName(nodes[j]),
					nodes[i].HashCodeWithoutChildren(), nodes[j].HashCodeWithoutChildren()))
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Compensating-rule family coverage.
//
// RFC-184 §7 exit criterion: W4 must exercise every compensating-rule site
// (FlatMap, RecursiveDfsJoin, InJoin, UnorderedUnion, PredicatesFilter,
// Projection) with generated shapes — that coverage is what licenses W2. Family
// membership is read from the typed plan node, never from EXPLAIN text
// (CLAUDE.md: NO TEXT MATCHING ON PLAN TREES).
// ---------------------------------------------------------------------------

var requiredFamilies = []string{
	"FlatMap", "RecursiveDfsJoin", "InJoin",
	"UnorderedUnion", "PredicatesFilter", "Projection",
}

// planFamilies returns the compensating-rule families present in a plan tree,
// with an occurrence count per family.
func planFamilies(root plans.RecordQueryPlan) map[string]int {
	fams := map[string]int{}
	plans.Walk(root, func(n plans.RecordQueryPlan) bool {
		switch n.(type) {
		case *plans.RecordQueryFlatMapPlan:
			fams["FlatMap"]++
		case *plans.RecordQueryRecursiveDfsJoinPlan:
			fams["RecursiveDfsJoin"]++
		case *plans.RecordQueryInJoinPlan:
			fams["InJoin"]++
		case *plans.RecordQueryUnorderedUnionPlan:
			fams["UnorderedUnion"]++
		case *plans.RecordQueryPredicatesFilterPlan:
			fams["PredicatesFilter"]++
		case *plans.RecordQueryProjectionPlan:
			fams["Projection"]++
		}
		return true
	})
	return fams
}

// familyProbe is a hand-written (schema, query) pair that deterministically
// drives one compensating family. The generator reaches most families by luck
// over enough seeds, but recursive CTEs (RecursiveDfsJoin) it never emits, and
// the rare ones (InJoin, UnorderedUnion) are seed-count-fragile. The probes make
// the exit-criterion coverage DETERMINISTIC rather than probabilistic, and each
// runs the SAME memo invariants as the generated sweep — they are not a coverage
// fiction, they are real planned shapes exercising the site.
type familyProbe struct {
	name   string
	schema string
	sql    string
}

const probeOrdersSchema = `
CREATE TABLE ORDERS (
  id BIGINT NOT NULL,
  customer_id BIGINT,
  status STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX idx_customer ON ORDERS(customer_id)
CREATE INDEX idx_status ON ORDERS(status)
CREATE INDEX idx_amount ON ORDERS(amount)
`

const probeJoinSchema = `
CREATE TABLE ORDERS (id BIGINT NOT NULL, customer_id BIGINT, PRIMARY KEY (id))
CREATE TABLE CUSTOMERS (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))
CREATE INDEX idx_customer ON ORDERS(customer_id)
`

const probeTreeSchema = `CREATE TABLE tree (id BIGINT NOT NULL, parent_id BIGINT, name STRING, PRIMARY KEY (id))`

var familyProbes = []familyProbe{
	{
		name:   "InJoin",
		schema: probeOrdersSchema,
		sql:    "SELECT id, customer_id FROM orders WHERE customer_id IN (0, 1, 2, 3, 4) ORDER BY id",
	},
	{
		name:   "FlatMap",
		schema: probeJoinSchema,
		sql:    "SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 10 ORDER BY o.id",
	},
	{
		name:   "PredicatesFilter",
		schema: probeOrdersSchema,
		sql:    "SELECT id FROM orders WHERE amount > 5 AND status = 'x'",
	},
	{
		name:   "Projection",
		schema: probeOrdersSchema,
		sql:    "SELECT status, amount FROM orders WHERE customer_id = 3",
	},
	{
		name:   "UnorderedUnion",
		schema: probeOrdersSchema,
		sql:    "SELECT id FROM orders WHERE amount > 5 UNION ALL SELECT id FROM orders WHERE customer_id < 3",
	},
	{
		name:   "RecursiveDfsJoin",
		schema: probeTreeSchema,
		sql: `WITH RECURSIVE descendants AS (
			SELECT id, name FROM tree WHERE parent_id = 1
			UNION ALL
			SELECT t.id, t.name FROM tree t, descendants d WHERE t.parent_id = d.id
		) SELECT name FROM descendants ORDER BY name`,
	},
}

// ---------------------------------------------------------------------------
// The sweep.
// ---------------------------------------------------------------------------

// noQuantifierBaseline is the aggregate ReasonNoQuantifier edge count MEASURED
// over the non-race generated sweep (seedCount=160) plus the family probes: it
// is 0.
//
// This is NOT the corpus figure. TestCorpusPlanReachability records 32–38
// no-quantifier edges — the scanPlanExpression adapter wrapping a
// TypeFilter(Scan), which the memo needs only for MULTI-record-type stores. The
// rowdiff generator emits single-table, single-type schemas, so that adapter is
// never constructed and this population is genuinely 0 for the generated set. It
// is a distinct measurement over a distinct input, not a claim that the corpus
// figure is already 0.
//
// RFC-184 drives the class to 0, so it can only fall. Asserted with <=, so the
// smaller -race/-short sweeps (fewer edges) can never spuriously exceed it, and
// any rule that starts constructing plan children the memo does not model pushes
// this above 0 and fires the assertion. The live figure is printed on every run
// (the "BASELINE …" log) so drift stays visible even when green.
const noQuantifierBaseline = 0

// blindnessFloorNumerator/Denominator require the collector to have actually
// COMPARED most of the edges it walked (compared >= edges * num/den), the
// anti-blindness guard from TestCorpusPlanReachability: a checker that silently
// stopped comparing would still count edges but not compare them. The gap is the
// no-quantifier adapter population, so the ratio is high but not 1.
const (
	blindnessFloorNumerator   = 7
	blindnessFloorDenominator = 10
)

// TestMemoInvariants_GeneratedShapes plans seeded-random shapes (and the family
// probes) and asserts the memo invariants over each — reachability and arity via
// the yield-time collector, identity/hash and arity again over each extracted
// plan tree — then asserts every compensating-rule family was exercised.
func TestMemoInvariants_GeneratedShapes(t *testing.T) {
	t.Parallel()

	// ONE collector owned by this test, threaded through every planning call, so
	// the tally is this measurement's alone (RFC-183: a shared collector summed
	// three concurrent tests into one number). It aggregates across the whole
	// sweep, which is exactly the population the reachability assertion wants.
	reach := cascades.NewReachabilityCollector()

	// familyBy[fam][source] counts occurrences; source is "generator" or "probe".
	familyBy := map[string]map[string]int{}
	tallyFamilies := func(plan plans.RecordQueryPlan, source string) {
		for fam := range planFamilies(plan) {
			if familyBy[fam] == nil {
				familyBy[fam] = map[string]int{}
			}
			familyBy[fam][source]++
		}
	}

	// The extracted-tree arity allowlist. Empty by design on a fully-linked-plan
	// branch: every leaf reports 0 children and 0 quantifiers, and every internal
	// node derives GetChildren from its quantifiers. A non-empty allowlist here
	// would be a finding — it means a plan type carries a child the memo does not
	// model — so we start empty and let the assertion catch any such node.
	arityAllow := map[string]bool{}

	var planned, planErrs int
	var arityViol, idhashViol int
	reportArity := func(sql string, vs []string) {
		for _, v := range vs {
			arityViol++
			if arityViol <= 10 {
				t.Errorf("ARITY violation: %s\n  query: %s", v, sql)
			}
		}
	}
	reportIDHash := func(sql string, vs []string) {
		for _, v := range vs {
			idhashViol++
			if idhashViol <= 10 {
				t.Errorf("IDENTITY/HASH violation: %s\n  query: %s", v, sql)
			}
		}
	}

	n := seedCount
	if testing.Short() {
		n = seedCount / 4
		if n < 4 {
			n = 4
		}
	}

	for s := 0; s < n; s++ {
		c := rowdiff.Generate(uint64(s))
		ddl := c.DDL()
		for _, q := range c.Queries {
			for _, proj := range c.ProjectionsFor(q) {
				sql := c.SQL(q, proj)
				plan, err := embedded.PlanPhysicalForTestWithReachability(sql, ddl, nil, reach)
				if err != nil {
					// The generator emits some shapes the engine legitimately
					// rejects (unsupported query, etc.); those are not soundness
					// findings. Count them so a sweep that planned NOTHING cannot
					// pass vacuously.
					planErrs++
					continue
				}
				planned++
				reportArity(sql, arityViolations(plan, arityAllow))
				reportIDHash(sql, identityHashViolations(plan))
				tallyFamilies(plan, "generator")
			}
		}
	}

	// Targeted probes — deterministic coverage of every required family, run
	// through the identical invariant checks and the same collector.
	for _, p := range familyProbes {
		plan, err := embedded.PlanPhysicalForTestWithReachability(p.sql, p.schema, nil, reach)
		if err != nil {
			t.Errorf("family probe %q failed to plan (a probe must plan): %v\n  query: %s", p.name, err, p.sql)
			continue
		}
		planned++
		reportArity(p.sql, arityViolations(plan, arityAllow))
		reportIDHash(p.sql, identityHashViolations(plan))
		tallyFamilies(plan, "probe")
	}

	// --- Sample-size guard: a zero that means "planned nothing" is
	// indistinguishable from a clean zero unless the sample is asserted too. ---
	if planned == 0 {
		t.Fatalf("planned 0 queries over %d seeds — the invariant tally would be vacuously clean", n)
	}

	edges, compared := reach.Edges(), reach.ComparedEdges()
	noQuant := reach.NoQuantifierCount()
	unreach := reach.Count()

	// Print the live baselines UNCONDITIONALLY (green or red). A ratchet that
	// only speaks when it fails cannot be checked for drift.
	t.Logf("BASELINE seeds=%d planned=%d planErrs=%d | edges=%d compared=%d unreachable=%d no-quantifier=%d | arityViol=%d idhashViol=%d",
		n, planned, planErrs, edges, compared, unreach, noQuant, arityViol, idhashViol)
	t.Logf("BASELINE family coverage: %s", formatFamilyCoverage(familyBy))

	// --- REACHABILITY: no plan may execute a child its quantifier's group
	// cannot produce (ReasonAbsent), and no quantifier may range over an empty
	// reference (ReasonEmptyGroup). Both are genuine defects; reach.Count()
	// counts exactly those two. ---
	if unreach != 0 {
		t.Errorf("REACHABILITY: %d unreachable plan edges across %d planned queries — "+
			"the memo is costing an expression that will never run\n%s",
			unreach, planned, reach.Report(20))
	}

	// --- Anti-blindness: the collector must have actually COMPARED most edges,
	// else a silently-blind checker would report a clean zero. ---
	if compared < edges*blindnessFloorNumerator/blindnessFloorDenominator {
		t.Errorf("collector compared only %d of %d edges — this assertion is blind, not clean\n%s",
			compared, edges, reach.Report(20))
	}

	// --- ARITY (memo edges): the ReasonNoQuantifier population. RFC-184 drives
	// this to 0; it may only fall. ---
	if noQuant > noQuantifierBaseline {
		t.Errorf("ARITY: no-quantifier memo edges rose to %d (baseline %d) — a rule built a "+
			"plan whose children the memo does not model\n%s",
			noQuant, noQuantifierBaseline, reach.Report(20))
	}

	// --- COVERAGE: every compensating-rule family must have been exercised. ---
	for _, fam := range requiredFamilies {
		if len(familyBy[fam]) == 0 {
			t.Errorf("COVERAGE: family %q was never exercised by the generated sweep OR a probe — "+
				"W4 cannot license W2 without covering every compensation site\n%s",
				fam, formatFamilyCoverage(familyBy))
		}
	}
}

// formatFamilyCoverage renders required-family coverage deterministically:
// which families were hit, by which source (generator vs. probe), and which
// were missed. Honest about the split — a family reached ONLY by a probe is
// reported as such (the generator did not reach it), never disguised as
// generator coverage.
func formatFamilyCoverage(familyBy map[string]map[string]int) string {
	parts := make([]string, 0, len(requiredFamilies))
	for _, fam := range requiredFamilies {
		src := familyBy[fam]
		if len(src) == 0 {
			parts = append(parts, fmt.Sprintf("%s=MISSING", fam))
			continue
		}
		sources := make([]string, 0, len(src))
		for k := range src {
			sources = append(sources, k)
		}
		sort.Strings(sources)
		detail := make([]string, 0, len(sources))
		for _, k := range sources {
			detail = append(detail, fmt.Sprintf("%s:%d", k, src[k]))
		}
		parts = append(parts, fmt.Sprintf("%s=[%v]", fam, detail))
	}
	return fmt.Sprintf("%v", parts)
}
