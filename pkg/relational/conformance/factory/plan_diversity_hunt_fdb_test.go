package factory_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/rowdiff"
	"fdb.dev/pkg/relational/core/embedded"
)

// plandivProgressEvery is how often the plan-diversity sweep reports, in SEEDS.
// Lower than the shape hunts' case interval because a plan-diversity seed is
// far more expensive: it creates a database, pins one connection per
// perturbation, and plans every query 17 times.
const plandivProgressEvery = 50

// perturbation is one planner/executor configuration the hunter runs a query
// under. None of them may change the ANSWER — only the plan or the paging — so
// a row disagreement against the baseline is an engine bug.
//
// alwaysExecute distinguishes the two oracle families here. A PLAN
// perturbation is only interesting when the plan actually moved, so it gates
// on that. An EXECUTION perturbation (forced continuations) leaves the plan
// identical by design; gating it on a plan change would skip it every single
// time and report the resulting silence as agreement.
type perturbation struct {
	name          string
	opts          func(*api.OptionsBuilder) *api.OptionsBuilder
	alwaysExecute bool
}

func disableRules(names ...string) func(*api.OptionsBuilder) *api.OptionsBuilder {
	return func(b *api.OptionsBuilder) *api.OptionsBuilder {
		return b.Set(api.OptDisabledPlannerRules, names)
	}
}

// portfolio is the perturbation set. RFC-201 §4.3 calls plan-diversity
// agreement "the strongest planner-bug detector this repo has"; the committed
// factory implements a ONE-rule slice of it (MatchLeafRule), and §4.2's
// forced-continuation oracle is not implemented at all. This is both.
//
// The rule names are checked against the live registry by
// TestPortfolioRuleNamesAreReal: an unrecognised name is carried through
// verbatim and disables nothing, so its silence would otherwise read as
// agreement.
var portfolio = []perturbation{
	// --- access-path perturbations -------------------------------------
	{name: "MatchLeafRule", opts: disableRules("MatchLeafRule")},
	{name: "MergeFetchIntoCoveringIndexRule", opts: disableRules("MergeFetchIntoCoveringIndexRule")},
	{name: "no-index-access", opts: disableRules("MatchLeafRule", "MatchIntermediateRule", "AggregateDataAccessRule")},
	// --- ordering perturbations ----------------------------------------
	{name: "ImplementSortRule", opts: disableRules("ImplementSortRule")},
	{name: "ImplementInMemorySortRule", opts: disableRules("ImplementInMemorySortRule")},
	{name: "no-ordered-scan", opts: disableRules("OrderedIndexScanRule", "OrderedPrimaryScanRule")},
	// --- predicate-shape perturbations ---------------------------------
	{name: "NormalizePredicatesRule", opts: disableRules("NormalizePredicatesRule")},
	{name: "InComparisonToExplodeRule", opts: disableRules("InComparisonToExplodeRule")},
	{name: "ImplementInJoinRule", opts: disableRules("ImplementInJoinRule")},
	{name: "PredicatePushDownRule", opts: disableRules("PredicatePushDownRule", "PushFilterBelowJoinRule")},
	// --- whole-option levers, all cross-engine spelled ------------------
	{name: "opt:right-deep-joins", opts: func(b *api.OptionsBuilder) *api.OptionsBuilder {
		return b.Set(api.OptPlanRightDeep, true)
	}},
	{name: "opt:no-rewriting", opts: func(b *api.OptionsBuilder) *api.OptionsBuilder {
		return b.Set(api.OptDisablePlannerRewriting, true)
	}},
	{name: "opt:planner-statistics", opts: func(b *api.OptionsBuilder) *api.OptionsBuilder {
		return b.Set(api.OptPlannerStatistics, true)
	}},
	// --- RFC-201 §4.2: forced continuations ----------------------------
	// A scanned-rows limit forces the executor to break and resume through the
	// continuation machinery — which is wire-format critical — while the driver
	// still returns the COMPLETE result. That is what makes it an oracle: the
	// answer must be identical, only the number of page boundaries crossed
	// changes.
	//
	// OptMaxRows is deliberately NOT used here. It TRUNCATES the result to N
	// rows and hands back a continuation for the caller to resume; comparing
	// that against an unpaged baseline reports `base=19 alt=1` on every row
	// set larger than the limit. That is the harness failing to follow a
	// continuation, not the engine losing rows, and it produced 388 false
	// findings in 25 seeds before this comment existed.
	//
	// The plan does not move under these, so they MUST NOT gate on a plan
	// change — hence alwaysExecute.
	{name: "exec:scan-rows-1", alwaysExecute: true, opts: func(b *api.OptionsBuilder) *api.OptionsBuilder {
		return b.Set(api.OptExecutionScannedRowsLimit, 1)
	}},
	{name: "exec:scan-rows-2", alwaysExecute: true, opts: func(b *api.OptionsBuilder) *api.OptionsBuilder {
		return b.Set(api.OptExecutionScannedRowsLimit, 2)
	}},
	{name: "exec:scan-rows-3", alwaysExecute: true, opts: func(b *api.OptionsBuilder) *api.OptionsBuilder {
		return b.Set(api.OptExecutionScannedRowsLimit, 3)
	}},
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// pinWith returns a pinned connection carrying the perturbation's options.
// Pinned because the options live on the connection; handing the query back to
// the pool would run it on a fresh, unconfigured one.
func pinWith(ctx context.Context, db *sql.DB, p perturbation) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if p.opts == nil {
		return conn, nil
	}
	if err := conn.Raw(func(dc any) error {
		ec, ok := dc.(*embedded.EmbeddedConnection)
		if !ok {
			return fmt.Errorf("driver conn is %T, want *embedded.EmbeddedConnection", dc)
		}
		ec.SetOptions(p.opts(api.NewOptionsBuilder()).Build())
		return nil
	}); err != nil {
		conn.Close() //nolint:errcheck
		return nil, err
	}
	return conn, nil
}

type huntStats struct {
	queries, planned, moved, executed, skipEmpty, baseErr, countOnly int
	planErrs, execErrs, movedBy, findingsBy                          map[string]int
	execErrSamples                                                   map[string][]string
	findings                                                         []string
}

func newHuntStats() huntStats {
	return huntStats{
		planErrs: map[string]int{}, execErrs: map[string]int{},
		movedBy: map[string]int{}, findingsBy: map[string]int{},
		execErrSamples: map[string][]string{},
	}
}

func (s *huntStats) fold(o huntStats) {
	s.queries += o.queries
	s.planned += o.planned
	s.moved += o.moved
	s.executed += o.executed
	s.skipEmpty += o.skipEmpty
	s.baseErr += o.baseErr
	s.countOnly += o.countOnly
	s.findings = append(s.findings, o.findings...)
	for k, v := range o.execErrSamples {
		if len(s.execErrSamples[k]) < 3 {
			s.execErrSamples[k] = append(s.execErrSamples[k], v...)
		}
	}
	for _, m := range []struct{ dst, src map[string]int }{
		{s.planErrs, o.planErrs},
		{s.execErrs, o.execErrs},
		{s.movedBy, o.movedBy},
		{s.findingsBy, o.findingsBy},
	} {
		for k, v := range m.src {
			m.dst[k] += v
		}
	}
}

// TestFDB_PlanDiversityHunt runs every generated query under every
// perturbation. Same query, different plan or different paging, different
// rows == an engine bug.
func TestFDB_PlanDiversityHunt(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	start := envInt("HUNT_SEED_START", 1)
	count := envInt("HUNT_SEEDS", 40)
	workers := envInt("HUNT_WORKERS", 4)

	// Budget and progress reporting, for the same reasons runShapeHuntGen has
	// them — and this hunt is the reason to say "the same reasons" out loud.
	//
	// Both fixes were made there first and NOT carried here, so this harness
	// kept the seed-count bound that had already been shown to end in a panic.
	// It then did exactly that: 60 minutes, killed by the Go test timeout, with
	// no summary line and no progress trail, so the run yielded nothing at all
	// about the range it walked. The Oracle-M sweeps that hit their budgets the
	// same hour each ended PASS with their coverage stated.
	//
	// One defect, two copies, one of them fixed. Carrying the fix is the whole
	// lesson.
	began := time.Now()
	budget := huntBudget()
	deadline := began.Add(budget)

	var mu sync.Mutex
	total := newHuntStats()
	var walked, seen int

	seeds := make(chan uint64)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for seed := range seeds {
				r := huntSeed(t, seed, w)
				mu.Lock()
				total.fold(r)
				seen++
				if seen%plandivProgressEvery == 0 {
					el := time.Since(began)
					fmt.Printf("HUNT progress seeds=%d queries=%d moved=%d executed=%d findings=%d "+
						"elapsed=%s rate=%.2f seeds/s\n",
						seen, total.queries, total.moved, total.executed, len(total.findings),
						el.Round(time.Second), float64(seen)/el.Seconds())
				}
				mu.Unlock()
			}
		}(w)
	}
	for s := start; s < start+count; s++ {
		if time.Now().After(deadline) {
			fmt.Printf("HUNT BUDGET EXHAUSTED after %s: walked %d of %d seeds. NORMAL end — the seed "+
				"count is an upper bound and the clock sizes the run.\n", budget, walked, count)
			break
		}
		seeds <- uint64(s)
		walked++
	}
	close(seeds)
	wg.Wait()
	fmt.Printf("HUNT walked=%d of %d seeds in %s\n", walked, count, time.Since(began).Round(time.Second))

	fmt.Printf("HUNT seeds=%d..%d queries=%d planned=%d moved=%d executed=%d empty-skips=%d base-errs=%d count-only=%d findings=%d\n",
		start, start+count-1, total.queries, total.planned, total.moved,
		total.executed, total.skipEmpty, total.baseErr, total.countOnly, len(total.findings))
	fmt.Println("HUNT --- plan moved / always-executed, by perturbation ---")
	dumpCounts("HUNT", total.movedBy)
	fmt.Println("HUNT --- planning errors by perturbation ---")
	dumpCounts("HUNT", total.planErrs)
	fmt.Println("HUNT --- execution errors by perturbation ---")
	dumpCounts("HUNT", total.execErrs)
	for k, v := range total.execErrSamples {
		for _, s := range v {
			fmt.Printf("HUNT EXECERR %s: %s\n", k, s)
		}
	}
	fmt.Println("HUNT --- findings by perturbation ---")
	dumpCounts("HUNT", total.findingsBy)

	for _, f := range total.findings {
		fmt.Println("HUNT FINDING " + f)
	}

	// Vacuity guards. A hunt that planned nothing, moved no plan, or executed
	// no perturbed query is an instrument that went dark, and a dark
	// instrument reports the same green as a clean engine.
	if total.queries == 0 {
		t.Fatal("HUNT VACUOUS: zero queries generated")
	}
	if total.moved == 0 {
		t.Fatal("HUNT VACUOUS: no perturbation ever changed a plan — the option is not reaching the planner")
	}
	if total.executed == 0 {
		t.Fatal("HUNT VACUOUS: no perturbed plan was ever executed")
	}
	// The continuation oracle is alwaysExecute, so its silence can only mean
	// "never ran". Floor it independently of the plan perturbations.
	if total.movedBy["exec:scan-rows-1"] == 0 {
		t.Fatal("HUNT VACUOUS: the forced-continuation oracle executed zero queries")
	}
	if len(total.findings) > 0 {
		t.Fatalf("HUNT: %d findings", len(total.findings))
	}
}

func huntSeed(t *testing.T, seed uint64, worker int) huntStats {
	res := newHuntStats()
	c := rowdiff.Generate(seed)
	if len(c.Rows) > 24 {
		c.Rows = c.Rows[:24]
	}

	ctx := context.Background()
	dbPath := fmt.Sprintf("/HUNT_%d_%d", worker, seed)
	schema := fmt.Sprintf("hs_%d_%d", worker, seed)
	tmpl := schema + "t"

	setupDB, err := sql.Open("fdbsql", "fdbsql:///__SYS?cluster_file="+clusterFilePath+"&schema=CATALOG")
	if err != nil {
		t.Errorf("seed %d: open sys: %v", seed, err)
		return res
	}
	defer setupDB.Close()
	if _, err := setupDB.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Errorf("seed %d: create database: %v", seed, err)
		return res
	}
	defer setupDB.ExecContext(ctx, "DROP DATABASE "+dbPath) //nolint:errcheck

	ddl := c.DDL()
	for _, stmt := range []string{
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", tmpl, ddl),
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schema, tmpl),
	} {
		if _, err := setupDB.ExecContext(ctx, stmt); err != nil {
			t.Errorf("seed %d: setup %q: %v", seed, stmt, err)
			return res
		}
	}
	defer setupDB.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s/%s", dbPath, schema)) //nolint:errcheck
	defer setupDB.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA TEMPLATE %s", tmpl))     //nolint:errcheck

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schema))
	if err != nil {
		t.Errorf("seed %d: open: %v", seed, err)
		return res
	}
	defer db.Close()

	base, err := pinWith(ctx, db, perturbation{name: "base"})
	if err != nil {
		t.Errorf("seed %d: pin base: %v", seed, err)
		return res
	}
	defer base.Close() //nolint:errcheck
	if _, err := base.ExecContext(ctx, c.InsertSQL()); err != nil {
		t.Errorf("seed %d: insert: %v", seed, err)
		return res
	}

	conns := make([]*sql.Conn, len(portfolio))
	for i, p := range portfolio {
		pc, err := pinWith(ctx, db, p)
		if err != nil {
			t.Errorf("seed %d: pin %s: %v", seed, p.name, err)
			return res
		}
		conns[i] = pc
		defer pc.Close() //nolint:errcheck
	}

	for _, q := range c.Queries {
		for _, proj := range c.ProjectionsFor(q) {
			sqlText := c.SQL(q, proj)
			res.queries++
			basePlan, err := explainConn(ctx, base, sqlText)
			if err != nil {
				res.baseErr++
				continue
			}
			baseRows, err := rowsOf(ctx, base, sqlText)
			if err != nil {
				res.baseErr++
				continue
			}
			if len(baseRows) == 0 {
				// An empty baseline cannot distinguish a correct plan from one
				// that returns nothing at all.
				res.skipEmpty++
				continue
			}
			ordered := len(q.OrderBy) > 0
			unstable := unstableSubset(q)
			if unstable {
				res.countOnly++
			}
			for i, p := range portfolio {
				res.planned++
				if !p.alwaysExecute {
					altPlan, err := explainConn(ctx, conns[i], sqlText)
					if err != nil {
						res.planErrs[p.name]++
						continue
					}
					if altPlan == basePlan {
						continue
					}
					res.moved++
					res.movedBy[p.name]++
					runAndCompare(ctx, conns[i], p, sqlText, ddl, basePlan, altPlan, baseRows, ordered, unstable, seed, &res)
					continue
				}
				res.moved++
				res.movedBy[p.name]++
				runAndCompare(ctx, conns[i], p, sqlText, ddl, basePlan, basePlan, baseRows, ordered, unstable, seed, &res)
			}
		}
	}
	return res
}

func runAndCompare(ctx context.Context, conn *sql.Conn, p perturbation, sqlText, ddl, basePlan, altPlan string,
	baseRows [][]any, ordered, unstable bool, seed uint64, res *huntStats,
) {
	altRows, err := rowsOf(ctx, conn, sqlText)
	if err != nil {
		res.execErrs[p.name]++
		if len(res.execErrSamples[p.name]) < 3 {
			res.execErrSamples[p.name] = append(res.execErrSamples[p.name],
				fmt.Sprintf("seed=%d %v | SQL: %s", seed, err, sqlText))
		}
		// An error under a perturbation, on a query the BASELINE answered fine,
		// is a finding unless it is one of the resource limits the perturbation
		// legitimately provokes. Counting all of them and reporting none is the
		// same defect the factory has: `engine error` there is a skip class
		// nothing triages, which is why 25 batches could report findings 0.
		//
		// The QOV-binding defect fixed in InComparisonToExplodeRule was exactly
		// this shape — an execution failure, not a wrong row — so a hunt that
		// files execution errors in a counter is blind to the one real engine
		// bug this session has found.
		if benignPerturbationError(err) {
			return
		}
		res.findingsBy[p.name]++
		res.findings = append(res.findings, fmt.Sprintf(
			"seed=%d perturbation=%s EXECUTION ERROR on a query the baseline answered\n"+
				"    err:  %v\n    SQL:  %s\n    DDL:  %s\n    base plan: %s\n    alt  plan: %s",
			seed, p.name, err, sqlText, ddl, basePlan, altPlan))
		return
	}
	res.executed++
	d := diffRows(ordered, baseRows, altRows)
	if unstable {
		d = countOnlyDiff(baseRows, altRows)
	}
	if d == "" {
		return
	}
	res.findingsBy[p.name]++
	res.findings = append(res.findings, fmt.Sprintf(
		"seed=%d perturbation=%s ordered=%v\n    diff: %s\n    SQL:  %s\n    DDL:  %s\n    base plan: %s\n    alt  plan: %s",
		seed, p.name, ordered, d, sqlText, ddl, basePlan, altPlan))
}

func explainConn(ctx context.Context, c *sql.Conn, q string) (string, error) {
	rows, err := c.QueryContext(ctx, "EXPLAIN "+q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		out = append(out, fmt.Sprint(vals[0]))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(out, "\n"), nil
}

func rowsOf(ctx context.Context, c *sql.Conn, q string) ([][]any, error) {
	rows, err := c.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

// rowKey renders a row so two rows compare equal only when every scalar does.
// Floats are rendered at full precision and with their sign preserved, because
// -0.0 and 0.0 are distinct answers a dedup or sort bug can confuse.
func rowKey(r []any) string {
	parts := make([]string, len(r))
	for i, v := range r {
		switch x := v.(type) {
		case nil:
			parts[i] = "NULL"
		case []byte:
			parts[i] = "b:" + string(x)
		case float64:
			parts[i] = "f:" + strconv.FormatFloat(x, 'b', -1, 64)
		case float32:
			parts[i] = "f32:" + strconv.FormatFloat(float64(x), 'b', -1, 32)
		default:
			parts[i] = fmt.Sprintf("%T:%v", v, v)
		}
	}
	return strings.Join(parts, "|")
}

// benignPerturbationError reports whether an execution failure under a
// perturbation is an expected consequence of the perturbation rather than a
// defect.
//
// The list is deliberately an ALLOWLIST of specific resource limits, not a
// denylist of known-bad classes. A denylist fails open: the first unfamiliar
// error class would be silently tolerated, which is the failure this whole
// change exists to remove.
//
// Both entries are earned, not guessed. `exec:scan-rows-*` pins a scanned-rows
// limit of 1-3, and some plans genuinely cannot make progress inside that
// budget and surface 54F01 instead of resuming — measured at 26 occurrences per
// arm over seeds 1..25, all on RIGHT JOIN shapes. Disabling an implementation
// rule can leave the planner with no way to build a plan at all, which is a
// statement about the rule set and not about correctness.
func benignPerturbationError(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	for _, benign := range []string{
		// Scanned-rows / scanned-bytes budget exhausted under a forced-paging arm.
		"scan limit reached",
		"scanned-records limit exceeded",
		// The rule set left after a disable cannot produce any plan.
		"Cascades planner could not plan query",
	} {
		if strings.Contains(msg, benign) {
			return true
		}
	}
	return false
}

// unstableSubset reports whether the query's RESULT SET — not merely its row
// order — is left unspecified by SQL, so two correct plans may legitimately
// disagree about WHICH rows come back.
//
// `LIMIT n` with no ORDER BY is exactly that: the engine may return any n of
// the qualifying rows, and a different access path picks a different n. This
// is not a hypothetical. Comparing such queries row-for-row produced 70
// findings over seeds 500001..501200, and all 70 were this shape — 70 of 70,
// classified by grep, not by judgement. Every one was a correct engine.
//
// A total ORDER BY removes the freedom, and the generator always suffixes its
// sort keys with the primary key (rowdiff/gen.go:175-177), so an ordered query
// here has a TOTAL order and its first n rows are uniquely determined.
//
// What survives is still an oracle, and deliberately so rather than skipping
// the shape: the row COUNT is fully determined at min(n, |qualifying|), so a
// plan returning the wrong NUMBER of rows is still caught. That is the
// wrong-window class this repo has already shipped once.
func unstableSubset(q rowdiff.Query) bool {
	return (q.Limit > 0 || q.Offset > 0) && len(q.OrderBy) == 0
}

// countOnlyDiff compares just the cardinality, for queries whose row identity
// SQL leaves to the engine.
func countOnlyDiff(a, b [][]any) string {
	if len(a) != len(b) {
		return fmt.Sprintf("row count base=%d alt=%d (LIMIT without ORDER BY: which rows is "+
			"unspecified, but HOW MANY is not)", len(a), len(b))
	}
	return ""
}

func diffRows(ordered bool, a, b [][]any) string {
	ka := make([]string, len(a))
	for i, r := range a {
		ka[i] = rowKey(r)
	}
	kb := make([]string, len(b))
	for i, r := range b {
		kb[i] = rowKey(r)
	}
	if !ordered {
		sort.Strings(ka)
		sort.Strings(kb)
	}
	if len(ka) != len(kb) {
		return fmt.Sprintf("row count base=%d alt=%d", len(ka), len(kb))
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return fmt.Sprintf("at position %d: base=%q alt=%q", i, ka[i], kb[i])
		}
	}
	return ""
}

func dumpCounts(tag string, m map[string]int) {
	type kv struct {
		k string
		v int
	}
	var xs []kv
	for k, v := range m {
		xs = append(xs, kv{k, v})
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].v > xs[j].v })
	for _, e := range xs {
		fmt.Printf("%s   %-36s %6d\n", tag, e.k, e.v)
	}
}
