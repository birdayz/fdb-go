package factory

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/explaindiff"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/conformance/yamsql"
	"fdb.dev/pkg/relational/core/embedded"
)

// secondPlanDisabledRule is the rule the second-plan oracle turns off.
//
// MatchLeafRule is what lets the planner match a predicate against an index;
// without it the same query must be answered by a full scan plus a residual
// filter. That is the strongest plan perturbation available through a
// supported, cross-engine-spelled option (Java keys the same set by
// getSimpleName, so the value is not a Go-only lever), and it changes the
// access path rather than the shape of the answer — which is exactly what a
// rows-must-not-change oracle needs.
const secondPlanDisabledRule = "MatchLeafRule"

// OracleStats counts what each oracle actually DID across a run.
//
// Every field here is reported and floored by the caller. RFC-201 §8.5: an
// oracle whose instrument goes dark is indistinguishable from an oracle that
// passes, and three of the RFC-197 campaign's ledger entries were instrument
// failures rather than engine bugs.
type OracleStats struct {
	// TLPChecked counts candidates whose partition property was evaluated.
	TLPChecked int `json:"tlp_checked"`
	// TLPViolations counts partitions that did not reconstruct — each is a
	// finding, not a skip.
	TLPViolations int `json:"tlp_violations"`
	// SecondPlanKept counts query executions where the second plan genuinely
	// differed and the rows were compared.
	SecondPlanKept int `json:"second_plan_kept"`
	// SecondPlanSkipped counts executions where disabling the rule left the
	// plan unchanged, so there was no second plan to compare against. Counted,
	// never silent — and floored against SecondPlanKept at the run level,
	// because "every case skipped" is exactly what the option being accepted
	// and ignored would look like.
	SecondPlanSkipped int `json:"second_plan_skipped"`
	// SecondPlanViolations counts row disagreements between two plans for the
	// same query — the strongest planner-bug signal this harness has.
	SecondPlanViolations int `json:"second_plan_violations"`
	// CrossEngineKept / CrossEngineViolations count the Java leg.
	CrossEngineKept       int `json:"cross_engine_kept"`
	CrossEngineViolations int `json:"cross_engine_violations"`
	// TLPOnlyBlessed counts candidates blessed on TLP alone because the
	// second-plan oracle was STRUCTURALLY inapplicable to their shape. It is
	// reported so the size of the exemption is visible rather than inferred
	// from the difference between two other counters.
	TLPOnlyBlessed int `json:"tlp_only_blessed"`
}

// Add folds b into s.
func (s *OracleStats) Add(b OracleStats) {
	s.TLPChecked += b.TLPChecked
	s.TLPViolations += b.TLPViolations
	s.SecondPlanKept += b.SecondPlanKept
	s.SecondPlanSkipped += b.SecondPlanSkipped
	s.SecondPlanViolations += b.SecondPlanViolations
	s.CrossEngineKept += b.CrossEngineKept
	s.CrossEngineViolations += b.CrossEngineViolations
	s.TLPOnlyBlessed += b.TLPOnlyBlessed
}

// Finding is an oracle DISAGREEMENT: two things that must agree did not.
//
// A finding is never committed as a test — freezing a disagreement would pin
// whichever side happens to be wrong. It is persisted with everything needed
// to reproduce it and it fails the run's exit code, under the standing rule
// that a red safety net is triaged, not filed.
type Finding struct {
	Oracle    string   `json:"oracle"`
	Seed      uint64   `json:"seed"`
	Candidate string   `json:"candidate"`
	Detail    string   `json:"detail"`
	DDL       string   `json:"ddl"`
	Setup     string   `json:"setup"`
	Queries   []string `json:"queries"`
	Left      string   `json:"left"`
	Right     string   `json:"right"`
}

// Outcome is one candidate's result. Exactly one of Blessed, Skip and Finding
// is meaningful; a candidate that is neither blessed nor skipped nor a finding
// is a bug in this file.
type Outcome struct {
	Candidate Candidate
	Blessed   bool
	Header    factorycorpus.Header
	Scenario  *yamsql.Scenario
	Skip      string
	Findings  []*Finding
	Stats     OracleStats
	// InapplicableFamily names the structural-inapplicability family that
	// permitted a TLP-only blessing, or "" when every applicable oracle ran.
	InapplicableFamily string
}

// CrossEngine is the Java leg. plandiff.SetupRunner satisfies it; the
// indirection keeps this package free of a hard dependency on a reachable Java
// server, so a run without one degrades to metamorphic blessing rather than
// failing.
type CrossEngine interface {
	Rows(ctx context.Context, schemaTemplate string, setup []string, query string) ([][]any, error)
}

// Runner executes candidates against a live schema.
type Runner struct {
	// DefaultConn plans with the full rule set.
	DefaultConn *sql.Conn
	// SecondPlanConn has secondPlanDisabledRule disabled, so the same query
	// takes a different access path.
	SecondPlanConn *sql.Conn
	// Java, when non-nil, blesses cross-engine.
	Java CrossEngine
	// Date is the batch date stamped into every header. It is an INPUT, never
	// read from the clock: §5.4 forbids wall-clock in the generation path, so
	// a committed file can be regenerated byte-identically.
	Date string
}

// Run evaluates one candidate through every oracle and, on unanimous
// agreement, freezes its four renderings as a scenario.
func (r *Runner) Run(ctx context.Context, cand Candidate) Outcome {
	out := Outcome{Candidate: cand}
	ddl := cand.Case.DDL()
	insert := cand.Case.InsertSQL()
	queries := cand.TLPQueries()

	sqls := make([]string, len(queries))
	for i, q := range queries {
		sqls[i] = cand.Case.SQL(q, cand.Projection)
	}

	// The plan shape is taken from the metadata-only planner over the `p`
	// rendering: it is the candidate's real query, and the shape must not
	// depend on which connection happened to run it.
	plan, err := embedded.PlanPhysicalForTest(sqls[1], ddl, nil)
	if err != nil {
		out.Skip = "plan-error: " + err.Error()
		return out
	}
	shape := explaindiff.ShapeOf(plan)

	results := make([][][]any, len(sqls))
	columns := make([][]string, len(sqls))
	for i, s := range sqls {
		rows, cols, err := queryRows(ctx, r.DefaultConn, s)
		if err != nil {
			// Not a finding: P1 generates only supported shapes, but a
			// generated shape the engine rejects is a gap to report, not a
			// wrong answer to freeze. Blessing a rejection as an error_code
			// expectation would pin a limitation as intended behaviour.
			out.Skip = fmt.Sprintf("engine error on %s: %v", TLPLabels[i], err)
			return out
		}
		if len(rows) > MaxExpectationRows {
			out.Skip = fmt.Sprintf("expectation too large: %s returned %d rows (cap %d)", TLPLabels[i], len(rows), MaxExpectationRows)
			return out
		}
		results[i] = rows
		columns[i] = cols
	}

	// A partition whose rows all land in one branch is a partition the oracle
	// cannot fail on: two of the three comparisons are 0 == 0, and the third
	// restates the unfiltered query. Committing it spends a file and a review
	// on an assertion that would survive almost any predicate bug. Rejected
	// with a counted reason rather than blessed — §5.4's rule that a candidate
	// is committed only if it adds a point, applied to the STRENGTH of the
	// point and not only to its novelty.
	if nonEmptyBranches(results[1], results[2], results[3]) < 2 || len(results[0]) == 0 {
		out.Skip = fmt.Sprintf("degenerate partition: unfiltered=%d, p=%d, not-p=%d, p-is-null=%d",
			len(results[0]), len(results[1]), len(results[2]), len(results[3]))
		return out
	}

	// --- Oracle 1: the ternary-logic partition -----------------------------
	out.Stats.TLPChecked++
	if detail := checkPartition(results[0], results[1], results[2], results[3]); detail != "" {
		out.Stats.TLPViolations++
		out.Findings = append(out.Findings, &Finding{
			Oracle: "tlp", Seed: cand.Seed, Candidate: cand.Name(),
			Detail: detail, DDL: ddl, Setup: insert, Queries: sqls,
			Left:  fmt.Sprintf("unfiltered=%d rows", len(results[0])),
			Right: fmt.Sprintf("p=%d not-p=%d p-is-null=%d", len(results[1]), len(results[2]), len(results[3])),
		})
		return out
	}

	// One value, read once, feeding both the oracle below and the frozen
	// expectation further down. See Candidate.Ordered for why it is a method.
	ordered := cand.Ordered()

	// --- Oracle 2: second-plan agreement -----------------------------------
	for i, s := range sqls {
		kept, detail, err := r.secondPlan(ctx, s, results[i], ordered)
		if err != nil {
			out.Skip = fmt.Sprintf("second-plan infra on %s: %v", TLPLabels[i], err)
			return out
		}
		if detail != "" {
			out.Stats.SecondPlanViolations++
			out.Findings = append(out.Findings, &Finding{
				Oracle: "second-plan", Seed: cand.Seed, Candidate: cand.Name(),
				Detail: detail, DDL: ddl, Setup: insert, Queries: []string{s},
			})
			return out
		}
		if kept {
			out.Stats.SecondPlanKept++
		} else {
			out.Stats.SecondPlanSkipped++
		}
	}

	blessing := factorycorpus.BlessingMetamorphic
	oracles := []string{"tlp"}
	if out.Stats.SecondPlanKept > 0 {
		oracles = append(oracles, "second-plan")
	}

	// --- Oracle 3: the cross-engine differential ---------------------------
	if r.Java != nil {
		for i, s := range sqls {
			javaRows, err := r.Java.Rows(ctx, ddl, []string{insert}, s)
			if err != nil {
				// Java refusing a shape Go accepts is a conformance question of
				// its own, not a blessing. Recorded as a skip so the rate stays
				// visible; the metamorphic path does NOT silently take over,
				// because a batch that quietly degrades its own authority is
				// how a corpus loses its second engine without anyone noticing.
				out.Skip = fmt.Sprintf("java leg failed on %s: %v", TLPLabels[i], err)
				return out
			}
			if detail := crossEngineDiff(results[i], javaRows); detail != "" {
				out.Stats.CrossEngineViolations++
				out.Findings = append(out.Findings, &Finding{
					Oracle: "cross-engine", Seed: cand.Seed, Candidate: cand.Name(),
					Detail: detail, DDL: ddl, Setup: insert, Queries: []string{s},
					Left: fmt.Sprint(results[i]), Right: fmt.Sprint(javaRows),
				})
				return out
			}
			out.Stats.CrossEngineKept++
		}
		blessing = factorycorpus.BlessingCrossEngine
		oracles = append(oracles, "cross-engine")
	}

	// What "blessed" means: cross-engine agreement, OR every APPLICABLE
	// metamorphic oracle agreed.
	//
	// "Applicable" is the load-bearing word, and it is decided STRUCTURALLY —
	// by the shape, against a ledger of families whose inapplicability a
	// committed pin establishes — never by the observation that this run
	// happened to skip. The distinction is the difference between an exemption
	// and an excuse: a per-case skip means the oracle did not apply here, which
	// is usually a property of the data or the plan and says nothing about
	// whether it COULD have; a structural claim means it can never apply to
	// this shape, which is a fact somebody measured and which goes stale
	// loudly when the structure changes.
	//
	// A candidate outside the ledger whose second plan never differed stays
	// UNBLESSABLE. TLP alone is one oracle, and it is blind to a planner
	// returning a consistent partition of the wrong rows.
	if blessing == factorycorpus.BlessingMetamorphic && out.Stats.SecondPlanKept == 0 {
		family := secondPlanInapplicableFor(cand)
		if family == nil {
			out.Skip = "metamorphic blessing needs every applicable oracle; disabling " + secondPlanDisabledRule +
				" left the plan unchanged for all four renderings, and this shape is not a family whose " +
				"second-plan inapplicability is structurally established"
			return out
		}
		// TLP alone, under its OWN label. Never the plain metamorphic one:
		// authority is a census dimension, and a weaker case absorbed into a
		// stronger label makes the measurement lie.
		blessing = factorycorpus.BlessingMetamorphicTLPOnly
		out.Stats.TLPOnlyBlessed++
		out.InapplicableFamily = family.Family
	}

	scenario := &yamsql.Scenario{
		Name:           cand.Name(),
		SchemaTemplate: ddl,
		Setup:          []string{insert},
	}
	for i, s := range sqls {
		scenario.Tests = append(scenario.Tests, yamsql.Test{
			Query: s,
			// The column labels feed the writer's by-name cells; they come from
			// the driver's result-set metadata, which is the same identity
			// Java's by-name matcher resolves against.
			Columns:   columns[i],
			Rows:      results[i],
			Unordered: !ordered,
		})
	}

	shapeDigest := factorycorpus.ShapeDigest(shape)
	out.Blessed = true
	out.Scenario = scenario
	out.Header = factorycorpus.Header{
		Name:          cand.Name(),
		Generator:     GeneratorVersion,
		Seed:          cand.Seed,
		QueryIndex:    cand.QueryIndex,
		Projection:    cand.ProjIndex,
		Date:          r.Date,
		Blessing:      blessing,
		Oracles:       oracles,
		FeatureVector: cand.FeatureVector,
		PlanShape:     shapeDigest,
		DedupKey:      factorycorpus.DedupKeyOf(cand.FeatureVector, shapeDigest),
	}
	return out
}

// secondPlan runs the query again under a plan the planner would not otherwise
// have chosen and compares the rows.
//
// The precondition is ASSERTED, never assumed: the two plans must actually
// DIFFER, or the case is a counted skip. Comparing a plan to itself is a
// tautology that passes for every engine, correct or not, and it looks
// identical in the log to a real check — same counter, same green.
//
// The precondition is plan INEQUALITY and nothing more. An earlier version
// also demanded the second plan be index-free, on the theory that disabling
// index matching must remove every index scan. That is a proxy, and it is
// refuted by the planner itself: MatchLeafRule is the sole seed of PartialMatch
// objects (rule_match_leaf.go:76 is the only unconditional NewPartialMatch
// call; MatchIntermediateRule and AdjustMatchRule both extend matches that
// already exist), so disabling it does starve the whole match/data-access
// pipeline — but the match pipeline is not the only thing that builds an index
// scan. Four paths construct one WITHOUT ever touching a PartialMatch:
// ImplementNestedLoopJoinRule.tryExistsFlatMap
// (rule_implement_nested_loop_join.go:4330), AggregateDataAccessRule,
// OrderedIndexScanRule and StreamingAggFromIndexRule — each reads
// GetMatchCandidates() directly.
//
// The correlated-`NOT EXISTS` counterexample the first factory run hit is the
// first of those: tryExistsFlatMap builds a `RecordQueryIndexPlan` from one
// `ComparisonEquals` range, which renders as `IndexScan(IDX_A, [=])` in the
// FlatMap's inner leg, and it survives MatchLeafRule being off because the
// correlated probe is genuinely not what that rule produces. (The other three
// pass an empty comparison prefix, so they emit only full-range scans and
// cannot produce a `[=]` bound.) TestFDB_SecondPlanKeepsCorrelatedIndexProbe
// pins that shape so the claim stays checkable.
//
// Measuring it turned up more than the counterexample: on a correlated-EXISTS
// query the OUTER leg is a filtered full scan in the baseline too, even with
// its filtered column indexed, so the two plans come out IDENTICAL and this
// oracle skips every such candidate. Combined with the both-oracles blessing
// rule below, that is why no committed scenario carries an `exists=` feature
// vector. TestFDB_SecondPlanIsBlindToCorrelatedExists holds that measurement.
//
// The proxy would therefore have reported a correct engine as a broken option
// on every such query. What actually guards against the option being accepted
// and ignored is the RUN-LEVEL floor on SecondPlanKept — if the option never
// bites, every case skips and the run says so — not a per-case guess about
// which plan shapes the rule owns.
//
// ordered decides how the two plans' rows are compared, and it is the whole
// point of running this oracle on a sorted query. Disabling index matching
// removes the ORDERINGS an index provides, so the second plan reaches its rows
// by a different route and must re-establish the sort; an ordering bug is
// precisely what this perturbation is built to surface, and a multiset compare
// is blind to it. It is safe to demand sequence equality because the generator
// always suffixes ORDER BY with the primary key (rowdiff/gen.go:175-177), so
// every generated sort is a TOTAL order and two correct plans cannot
// legitimately disagree on tie placement.
func (r *Runner) secondPlan(ctx context.Context, sqlText string, baseRows [][]any, ordered bool) (kept bool, detail string, err error) {
	basePlan, err := explainOn(ctx, r.DefaultConn, sqlText)
	if err != nil {
		return false, "", fmt.Errorf("EXPLAIN baseline: %w", err)
	}
	altPlan, err := explainOn(ctx, r.SecondPlanConn, sqlText)
	if err != nil {
		return false, "", fmt.Errorf("EXPLAIN second plan: %w", err)
	}
	if altPlan == basePlan {
		return false, "", nil
	}
	altRows, _, err := queryRows(ctx, r.SecondPlanConn, sqlText)
	if err != nil {
		return false, "", fmt.Errorf("second-plan execution: %w", err)
	}
	if d := rowsDiff(ordered, baseRows, altRows); d != "" {
		how := "as multisets"
		if ordered {
			how = "as sequences (the query carries ORDER BY, and the committed expectation freezes that exact sequence)"
		}
		return true, fmt.Sprintf("two plans for one query returned different rows %s: %s\n  baseline plan: %s\n  second plan:   %s\n  SQL: %s",
			how, d, basePlan, altPlan, sqlText), nil
	}
	return true, "", nil
}

// rowsDiff compares two row sets the way the CANDIDATE will be committed: as
// sequences when its query is ordered, as multisets otherwise.
//
// Tying the comparison to how the scenario freezes its rows is what keeps the
// oracle honest. A blessed ordered scenario asserts an exact row sequence
// (yamsql.Test.Unordered is false), so an oracle that only ever checked the
// multiset would freeze a sequence no oracle ever looked at.
func rowsDiff(ordered bool, a, b [][]any) string {
	if ordered {
		return sequenceDiff(a, b)
	}
	return multisetDiff(a, b)
}

// sequenceDiff reports the first positional difference between two row
// sequences, or "".
func sequenceDiff(a, b [][]any) string {
	ka, kb := rowKeysInOrder(a), rowKeysInOrder(b)
	if len(ka) != len(kb) {
		return fmt.Sprintf("row count %d vs %d", len(ka), len(kb))
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return fmt.Sprintf("at position %d: %s vs %s", i, ka[i], kb[i])
		}
	}
	return ""
}

// checkPartition is the TLP property: the rows of `p`, `NOT p` and `p IS NULL`
// must reconstruct the unfiltered rows exactly, as multisets.
//
// Multisets, not sets: duplicate rows are the interesting case, and set
// equality would pass a plan that drops one of two identical rows from a
// branch. SQL's three-valued logic is what makes the property total — a row
// the predicate says TRUE for lands in the first branch, FALSE in the second,
// UNKNOWN in the third, and no row lands in two of them.
func checkPartition(unfiltered, pos, neg, unknown [][]any) string {
	combined := make([][]any, 0, len(pos)+len(neg)+len(unknown))
	combined = append(combined, pos...)
	combined = append(combined, neg...)
	combined = append(combined, unknown...)
	if len(combined) != len(unfiltered) {
		return fmt.Sprintf("partition size %d (p=%d, not-p=%d, p-is-null=%d) != unfiltered %d",
			len(combined), len(pos), len(neg), len(unknown), len(unfiltered))
	}
	return multisetDiff(unfiltered, combined)
}

func nonEmptyBranches(branches ...[][]any) int {
	n := 0
	for _, b := range branches {
		if len(b) > 0 {
			n++
		}
	}
	return n
}

// multisetDiff reports the first difference between two row multisets, or "".
func multisetDiff(a, b [][]any) string {
	ka, kb := rowKeys(a), rowKeys(b)
	if len(ka) != len(kb) {
		return fmt.Sprintf("row count %d vs %d", len(ka), len(kb))
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return fmt.Sprintf("at sorted position %d: %s vs %s", i, ka[i], kb[i])
		}
	}
	return ""
}

func rowKeys(rows [][]any) []string {
	out := rowKeysInOrder(rows)
	sort.Strings(out)
	return out
}

// rowKeysInOrder renders each row to a comparable key, PRESERVING the order the
// engine returned them in. rowKeys is this plus a sort; the two must render a
// row identically or an ordered and an unordered comparison of the same data
// would disagree about what a row IS.
func rowKeysInOrder(rows [][]any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		parts := make([]string, len(r))
		for i, v := range r {
			parts[i] = fmt.Sprintf("%T:%v", v, v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out
}

// crossEngineDiff compares Go rows against Java's.
//
// Java's JSON transport turns every number into a float64 and base64-encodes
// bytes, so the comparison is numeric rather than typed — comparing types
// across a JSON boundary would report the transport as a divergence.
//
// It compares MULTISETS even for a query carrying ORDER BY, where sequence
// equality would be the stronger check. That is a deliberate, conservative
// choice: a multiset comparison can miss an ordering divergence but can never
// manufacture one, and this whole leg is currently unexercised (no environment
// has yet run the Java server against the factory's container). An aggressive
// unverified check that reports the engine as wrong is a worse failure than a
// conservative one that misses a class. Tighten it to a sequence comparison
// for ordered queries in the same change that first gets the leg running and
// can measure the result.
func crossEngineDiff(goRows, javaRows [][]any) string {
	if len(goRows) != len(javaRows) {
		return fmt.Sprintf("row count: go %d, java %d", len(goRows), len(javaRows))
	}
	g, j := crossKeys(goRows), crossKeys(javaRows)
	for i := range g {
		if g[i] != j[i] {
			return fmt.Sprintf("at sorted position %d: go %s, java %s", i, g[i], j[i])
		}
	}
	return ""
}

func crossKeys(rows [][]any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		parts := make([]string, len(r))
		for i, v := range r {
			parts[i] = crossScalar(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	sort.Strings(out)
	return out
}

func crossScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		return fmt.Sprintf("b:%t", x)
	case string:
		return "s:" + x
	case int64:
		// EXACT, never through float64. An earlier version rendered every
		// number with %.9g, which has nine significant digits and therefore
		// cannot tell two large int64s apart: 2^62 and 2^62+1 — and 2^62 IS in
		// the committed corpus, it is one of rowdiff's boundary values — both
		// collapse to "4.61168602e+18". A differential oracle that reports two
		// different values as equal is worse than no oracle: it passes.
		return "n:" + strconv.FormatInt(x, 10)
	case float32:
		// FLOAT columns are captured at float32 width (queryRows); Java's JSON
		// leg widens them to float64 the same lossless way this conversion does.
		return "n:" + crossFloat(float64(x))
	case float64:
		return "n:" + crossFloat(x)
	default:
		return fmt.Sprintf("?:%v", x)
	}
}

// crossFloat renders a float64 so that an INTEGRAL value compares equal to the
// int64 that Go returned for the same column.
//
// That equality is the whole reason this is not just %v. Java's JSON transport
// has one number type, so a BIGINT arrives here as a float64 while Go's driver
// hands back an int64, and rendering the two differently would report the
// TRANSPORT as an engine divergence on every integer column. Non-integral
// values fall through to a round-trippable rendering ('g' with 17 significant
// digits, enough to distinguish any two distinct float64s), and signed zero is
// kept distinct because +0.0 and -0.0 are genuinely different values under the
// FDB tuple ordering the sort keys use.
func crossFloat(x float64) string {
	if x == 0 {
		if math.Signbit(x) {
			return "-0"
		}
		return "0"
	}
	// The bounds are the exactly-representable float64 neighbours of the int64
	// range; outside them the int64 conversion is undefined.
	if !math.IsNaN(x) && x == math.Trunc(x) && x >= -9223372036854775808.0 && x < 9223372036854775808.0 {
		return strconv.FormatInt(int64(x), 10)
	}
	return strconv.FormatFloat(x, 'g', 17, 64)
}

// queryRows executes and materializes rows in the yamsql expectation form:
// int64 / float64 / string / bool / nil, with byte slices decoded to strings.
// Anything else reaches valueScalar in the writer and is rejected there rather
// than being frozen in a form the loader cannot read back.
func queryRows(ctx context.Context, conn *sql.Conn, sqlText string) ([][]any, []string, error) {
	rows, err := conn.QueryContext(ctx, sqlText)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	// A FLOAT column's cell must be captured AT FLOAT WIDTH, because the
	// committed expectation's spelling is class-sensitive: the yamsql writer
	// freezes a float32 under `!f` (a Java Float expectation) and a float64 as
	// a plain YAML float (a Double expectation), and Java's matcher — which
	// javacorpus mirrors — never lets a Double expectation satisfy a Float
	// actual. The driver widens FLOAT to float64 on Scan, so the width is
	// recovered from the column's declared type; the widening is exact in both
	// directions (every float32 is a float64), so no value is disturbed.
	isFloat32 := make([]bool, len(cols))
	if types, err := rows.ColumnTypes(); err == nil {
		for i, ct := range types {
			if i < len(isFloat32) && strings.EqualFold(ct.DatabaseTypeName(), "FLOAT") {
				isFloat32[i] = true
			}
		}
	}
	out := [][]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		for i, v := range vals {
			switch x := v.(type) {
			case []byte:
				vals[i] = string(x)
			case float64:
				if isFloat32[i] {
					vals[i] = float32(x)
				}
			}
		}
		out = append(out, vals)
	}
	return out, cols, rows.Err()
}

// explainOn returns the query's plan text with generated correlation
// identifiers renumbered per-plan.
//
// The normalization is load-bearing, not cosmetic, and the failure it prevents
// is measured rather than argued. `q$6381` carries a PROCESS-GLOBAL counter, so
// the same plan rendered on the baseline connection and on the second-plan
// connection gets different numbers: `SELECT (SELECT MIN(b) FROM t) FROM t`
// comes back as `Project([(SCALAR_SUBQUERY q$11)], Scan(T))` on one and
// `...q$38...` on the other, for a query MatchLeafRule cannot touch at all.
//
// The oracle's entire precondition is the string comparison
// `altPlan == basePlan`. Left raw, those two IDENTICAL plans compare unequal,
// the case counts as SecondPlanKept, and a tautological "the engine agrees with
// itself" comparison is banked as a real check. That also disarms the
// run-level went-dark floor, which fires only when kept == 0 — so the harness
// would report a well-exercised oracle built entirely out of counter drift.
// A scalar subquery is drawn on roughly a fifth of the generator's plain
// queries, so this is a routine shape, not a corner.
//
// It reuses explaindiff's normalizer rather than replicating it: that one is
// already pinned as stable and injective by
// TestNormalizeAliasesIsStableAndInjective, and two copies of an
// erase-this-but-not-that rule drift.
func explainOn(ctx context.Context, conn *sql.Conn, sqlText string) (string, error) {
	var plan string
	if err := conn.QueryRowContext(ctx, "EXPLAIN "+sqlText).Scan(&plan); err != nil {
		return "", err
	}
	return explaindiff.NormalizeAliases(plan), nil
}

// PinConn returns a pinned connection with the given planner options applied.
// Pinned because the option lives on the connection: handing the query back to
// the pool would run it on a fresh, unconfigured one.
func PinConn(ctx context.Context, db *sql.DB, disabledRules []string) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if len(disabledRules) == 0 {
		return conn, nil
	}
	if err := conn.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver conn is %T, want *embedded.EmbeddedConnection", dc)
		}
		ec.SetOptions(api.NewOptionsBuilder().Set(api.OptDisabledPlannerRules, disabledRules).Build())
		return nil
	}); err != nil {
		conn.Close() //nolint:errcheck
		return nil, err
	}
	return conn, nil
}

// SecondPlanRules is the disabled-rule set the second-plan oracle pins.
func SecondPlanRules() []string { return []string{secondPlanDisabledRule} }
