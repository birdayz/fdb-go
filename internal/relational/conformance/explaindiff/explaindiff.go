// Package explaindiff renders the PHYSICAL plan shape of every query in the
// yamsql conformance corpus into a stable, diffable text baseline, and diffs
// two such baselines entry-by-entry.
//
// Why it exists (RFC-183 §6): removing the nil-inner "shell" plans from the
// Cascades planner changes plan hashes and costs, which can silently FLIP
// which plan wins. A flipped-but-still-correct plan passes every row-level
// test — the corpus runner, rowdiff, and the 1M stress all stay green while
// plan quality regresses. Diffing the planned shape across the whole corpus
// is the only check that can see it. `plan_contains` (yamsql.go) is a
// per-case substring assert on ONE query; it cannot.
//
// Relationship to the sibling harnesses — this is not a parallel pipeline:
//
//   - `conformance/plandiff` diffs Go against JAVA over its OWN in-code
//     corpus, and its Go side is the explain-only LOGICAL generator
//     (embedded.NewExplainOnlyGeneratorWithSchema → buildLogicalPlanFor*).
//     It answers "do the two engines agree?", never "did Go's physical plan
//     shape move?", and cannot be retargeted at the physical planner without
//     invalidating every golden in its corpus.
//   - `conformance/yamsql` owns the corpus and its loader. This package
//     consumes that loader — it never re-parses the YAML itself.
//   - The planner entry point is `embedded.PlanPhysicalForTest`, the same
//     no-FDB full-Cascades harness the planner's own tests and the RFC-182
//     rowdiff harness plan through. It runs the same Cascades planner the
//     driver runs, but it is NOT the production driver path: the driver
//     enters through the sqldriver/connection stack and supplies live table
//     statistics, while this harness calls the planner directly with
//     planner-default statistics. Two plans that differ only because of that
//     are expected; what the baseline pins is that the SAME harness input
//     keeps producing the SAME plan.
//
// NO FDB REQUIRED. Planning is metadata-only: the corpus scenario's
// `schema_template` is compiled to in-memory RecordMetaData and the query is
// planned against it under the scenario's `connection_options`, as the corpus
// runner's connection is. `setup:` INSERTs are not replayed, so table statistics
// are the planner defaults rather than the live cardinalities the driver
// would fetch from FDB. That is a deliberate trade: the baseline is a
// REGRESSION key (same input → same output → any delta is a real planner
// change), not a prediction of the plan the live driver picks for a
// populated table.
package explaindiff

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"fdb.dev/internal/recordlayer/query/plan/cascades"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/internal/relational/conformance/yamsql"
	"fdb.dev/internal/relational/relapi"
	"fdb.dev/pkg/relational/core/embedded"
)

// formatVersion is stamped into every baseline header. Bump it when the
// rendering changes so a stale baseline is diagnosed rather than silently
// mis-diffed against a new one.
const formatVersion = "explain-baseline/v2"

// maxShapeDepth caps the shape-tree walk. A physical plan is a tree, so the
// cap can only be hit by a cycle introduced by a planner bug — which is
// itself worth surfacing as a stable marker rather than as a stack overflow.
const maxShapeDepth = 200

// Entry is one corpus query's planned shape.
type Entry struct {
	// File is the corpus file's base name (e.g. "aggregate_expr.yaml").
	File string
	// Index is the 0-based position of the test within that file's
	// `tests:` sequence. File+Index is the diff key: it points a reader
	// straight at the exact stanza.
	Index int
	// SQL is the query text, whitespace-collapsed onto one line.
	SQL string
	// ErrorPin is the SQLSTATE the corpus stanza expects the query to fail
	// with (`error_code:` / `error:`), empty when the stanza expects rows.
	// It is what separates the two meanings of a failure marker: a pinned
	// rejection is the corpus working as designed, an UNPINNED one is a
	// query that should plan and doesn't.
	ErrorPin string
	// Plan is the recursive one-line Explain() rendering of the winning
	// physical plan, or a `<PLAN-ERROR: …>` / `<PLAN-PANIC: …>` marker.
	Plan string
	// Shape is the plan's structural skeleton: one line per node, the Go
	// plan type indented by depth. It survives label-format churn, so a
	// pure rendering change shows up in Plan only, while a real structural
	// flip moves Shape too. Empty when the query did not plan.
	Shape []string
}

// Key returns the stable diff key for an entry.
func (e Entry) Key() string { return e.File + "#" + strconv.Itoa(e.Index) }

// Failed reports whether the entry is a plan failure marker rather than a
// plan. A query that STOPS planning is exactly the regression this harness
// exists to catch, so failures are recorded as entries — never skipped.
func (e Entry) Failed() bool {
	return strings.HasPrefix(e.Plan, "<PLAN-ERROR:") || strings.HasPrefix(e.Plan, "<PLAN-PANIC:")
}

// Panicked reports whether planning this query panicked. Always a bug: the
// planner's contract is an error, never a panic (design principle 4).
func (e Entry) Panicked() bool { return strings.HasPrefix(e.Plan, "<PLAN-PANIC:") }

// UnexpectedlyFailed reports a query that failed to plan although its corpus
// stanza expects rows. This is the signal RFC-183 P0 must not regress: a
// query that stops planning.
func (e Entry) UnexpectedlyFailed() bool { return e.Failed() && e.ErrorPin == "" }

// Collect walks dir/*.yaml, loads every scenario through the yamsql loader,
// and plans every query AND DELETE/UPDATE test in it.
//
// Two plannable classes are pinned:
//
//   - SELECT / WITH / VALUES (yamsql.IsQuery) → the SELECT harness
//     (embedded.PlanPhysicalForTest). Counted in Stats.Queries.
//   - DELETE / UPDATE (isDML) → the DML harness
//     (embedded.PlanPhysicalDMLForTest). Counted in Stats.DML. This closes the
//     DML-blind gap: a planner change that reads clean on every SELECT while
//     corrupting the DELETE-WHERE-EXISTS path (RFC-184 W2) is invisible to a
//     SELECT-only dump.
//
// Everything else (`exec:` INSERT and other sequencing steps) has no
// interesting winning plan and is counted in Stats.NonQuery so the corpus total
// still reconciles (Queries + DML + NonQuery == tests).
//
// Scenarios whose schema_template does not compile yield one
// `<PLAN-ERROR: …>` entry per query in the file rather than vanishing — a
// schema regression must not silently shrink the baseline.
//
// The returned slice is sorted by (file, index) and is byte-stable across
// runs: no map iteration, no timestamps, no addresses.
func Collect(dir string) ([]Entry, Stats, error) {
	return collect(dir, nil, false)
}

// CollectBoundLiterals is Collect with SELECT literals planned as production
// plans them: statement-pool references, the form the plan cache reuses.
func CollectBoundLiterals(dir string) ([]Entry, Stats, error) {
	return collect(dir, nil, true)
}

// CollectWithReachability is Collect with RFC-183's yield-time plan-reachability
// accounting routed into the caller's collector. nil = collect nothing.
func CollectWithReachability(dir string, reach *cascades.ReachabilityCollector) ([]Entry, Stats, error) {
	return collect(dir, reach, false)
}

func collect(dir string, reach *cascades.ReachabilityCollector, bound bool) ([]Entry, Stats, error) {
	jobs, st, err := corpusJobs(dir, bound)
	if err != nil {
		return nil, Stats{}, err
	}
	entries := planJobs(jobs, reach)
	for _, e := range entries {
		if e.Failed() {
			st.PlanErrors++
		}
		if e.UnexpectedlyFailed() {
			st.UnexpectedErrors++
		}
	}
	return entries, st, nil
}

// job is one plannable corpus statement.
type job struct {
	base, sql, errorPin, schemaTemplate string
	idx                                 int
	plan                                planFn
}

// corpusJobs gathers dir/*.yaml's plannable statements in corpus order, with
// the Stats counts that do not depend on planning.
func corpusJobs(dir string, bound bool) ([]job, Stats, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, Stats{}, fmt.Errorf("glob %s: %w", dir, err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return nil, Stats{}, fmt.Errorf("no *.yaml scenarios under %s", dir)
	}

	var jobs []job
	st := Stats{Files: len(matches)}
	for _, path := range matches {
		base := filepath.Base(path)
		s, loadErr := yamsql.Load(path)
		if loadErr != nil {
			// A corpus file that no longer loads is a hard error: silently
			// dropping it would shrink the baseline and read as "no change".
			return nil, Stats{}, fmt.Errorf("load %s: %w", base, loadErr)
		}
		opts, optsErr := s.Options()
		if optsErr != nil {
			return nil, Stats{}, fmt.Errorf("%s: %w", base, optsErr)
		}
		for i, t := range s.Tests {
			args, argErr := StatementArgs(&s.Tests[i])
			if argErr != nil {
				return nil, Stats{}, fmt.Errorf("%s#%d: %w", base, i, argErr)
			}
			// Pick the harness by statement class. SELECT/WITH/VALUES plan
			// through the SELECT harness; DELETE/UPDATE through the DML harness;
			// INSERT and other sequencing steps have no interesting plan and are
			// counted (not planned) so the corpus total reconciles.
			var plan planFn
			switch {
			case isDML(t.Query):
				st.DML++
				plan = planDML(args, opts)
			case yamsql.IsQuery(t.Query):
				st.Queries++
				plan = planSelect(args, opts)
				if bound {
					plan = planSelectBound(args, opts)
				}
			default:
				st.NonQuery++
				continue
			}
			jobs = append(jobs, job{base, t.Query, t.EffectiveErrorCode(), s.SchemaTemplate, i, plan})
		}
	}
	return jobs, st, nil
}

// planJobs plans jobs concurrently and returns the serial entries in job order: aliases are
// renumbered per plan and the reachability collector is mutex-guarded.
func planJobs(jobs []job, reach *cascades.ReachabilityCollector) []Entry {
	entries := make([]Entry, len(jobs))
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), max(len(jobs), 1)) {
		wg.Go(func() {
			for k := range work {
				j := jobs[k]
				entries[k] = planOne(j.base, j.idx, j.sql, j.errorPin, j.schemaTemplate, reach, j.plan)
			}
		})
	}
	for k := range jobs {
		work <- k
	}
	close(work)
	wg.Wait()
	return entries
}

// Instability is a corpus statement that planned to more than one rendering.
type Instability struct {
	Key   string
	Plans []string // the distinct renderings, in the order first seen
}

// Replan plans the corpus, replans each statement keep accepts runs more times, and reports
// those whose rendering changed; kept counts the replanned statements.
func Replan(dir string, runs int, keep func(Entry) bool) (kept int, unstable []Instability, err error) {
	jobs, _, err := corpusJobs(dir, false)
	if err != nil {
		return 0, nil, err
	}
	kept, unstable = replan(jobs, runs, keep, func(js []job) []Entry { return planJobs(js, nil) })
	return kept, unstable, nil
}

func replan(jobs []job, runs int, keep func(Entry) bool, plan func([]job) []Entry) (int, []Instability) {
	first := plan(jobs)
	var keptJobs []job
	var keptFirst []Entry
	for k, e := range first {
		if keep(e) {
			keptJobs = append(keptJobs, jobs[k])
			keptFirst = append(keptFirst, e)
		}
	}
	seen := make([][]string, len(keptJobs))
	for k, e := range keptFirst {
		seen[k] = []string{e.Plan}
	}
	for range runs {
		for k, e := range plan(keptJobs) {
			if !slices.Contains(seen[k], e.Plan) {
				seen[k] = append(seen[k], e.Plan)
			}
		}
	}
	var unstable []Instability
	for k, plans := range seen {
		if len(plans) > 1 {
			unstable = append(unstable, Instability{Key: keptFirst[k].Key(), Plans: plans})
		}
	}
	return len(keptJobs), unstable
}

// Stats are the reconciliation counts for a Collect run. They are derived,
// deterministic, and printed in the baseline header so a diff of two headers
// immediately says whether the corpus itself moved.
type Stats struct {
	// Files is the number of *.yaml scenarios walked.
	Files int
	// Queries is the number of planned SELECT/WITH/VALUES tests.
	Queries int
	// DML is the number of planned DELETE/UPDATE tests. Together with Queries
	// it accounts for every entry: Queries + DML == len(entries).
	DML int
	// NonQuery is the number of non-plannable stanzas skipped (INSERT and
	// other `exec:` sequencing steps).
	NonQuery int
	// PlanErrors is how many entries are failure markers.
	PlanErrors int
	// UnexpectedErrors is how many of those failures have no corpus error
	// pin — statements that are supposed to plan and don't.
	UnexpectedErrors int
}

// planFn produces the winning physical plan for one statement against a schema
// template. It is the ONLY thing that differs between the SELECT path and the
// DML path — everything downstream (Explain, shapeOf, marker rendering) is
// shared through planOne.
type planFn func(sql, schemaTemplate string, reach *cascades.ReachabilityCollector) (plans.RecordQueryPlan, error)

// StatementArgs are a test's `args:`, numbered as database/sql numbers the
// runner's positional arguments. A harness binds them, so a `?` plans with
// its driver value's type, exactly as the runner executes it.
func StatementArgs(t *yamsql.Test) ([]driver.NamedValue, error) {
	values, err := t.ArgValues()
	if err != nil {
		return nil, err
	}
	named := make([]driver.NamedValue, len(values))
	for i, v := range values {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return named, nil
}

// planSelect routes a SELECT/WITH/VALUES statement through the SELECT harness.
func planSelect(args []driver.NamedValue, opts *api.Options) planFn {
	return func(sql, schemaTemplate string, reach *cascades.ReachabilityCollector) (plans.RecordQueryPlan, error) {
		// Statistics are nil: planner defaults (see the package doc — the
		// baseline is a regression key, not a live-cardinality prediction).
		return embedded.PlanPhysicalForTestWithArgs(sql, schemaTemplate, args, opts, nil, reach)
	}
}

// planDML routes a DELETE/UPDATE statement through the DML harness — the same
// no-FDB Cascades pipeline, entered via the DML logical builders and the DML
// planning rule set (see embedded.PlanPhysicalDMLForTest).
func planSelectBound(args []driver.NamedValue, opts *api.Options) planFn {
	return func(sql, schemaTemplate string, _ *cascades.ReachabilityCollector) (plans.RecordQueryPlan, error) {
		return embedded.PlanPhysicalForTestWithBoundLiterals(sql, schemaTemplate, args, opts, nil)
	}
}

func planDML(args []driver.NamedValue, opts *api.Options) planFn {
	return func(sql, schemaTemplate string, reach *cascades.ReachabilityCollector) (plans.RecordQueryPlan, error) {
		return embedded.PlanPhysicalDMLForTestWithArgs(sql, schemaTemplate, args, opts, nil, reach)
	}
}

// isDML reports whether stmt is a DELETE or UPDATE — the two DML shapes with a
// scanned/filtered tree worth pinning. It mirrors yamsql.IsQuery's leading-
// keyword tokenization (strip leading whitespace/paren, take the first word).
// INSERT is deliberately excluded: INSERT … VALUES is literal rows and
// INSERT … SELECT is planned as its SELECT body by the Query path already.
func isDML(stmt string) bool {
	switch leadingKeyword(stmt) {
	case "DELETE", "UPDATE":
		return true
	}
	return false
}

// leadingKeyword returns the upper-cased first token of a statement, skipping
// leading whitespace and open parens — the same tokenization yamsql.IsQuery
// applies so the two classifiers agree on where a statement's keyword starts.
func leadingKeyword(stmt string) string {
	s := strings.TrimLeft(stmt, " \t\r\n(")
	for i, r := range s {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '(' {
			s = s[:i]
			break
		}
	}
	return strings.ToUpper(s)
}

// planOne plans a single statement against the scenario's schema template
// using the supplied harness, converting any error OR panic into a stable
// marker entry.
func planOne(file string, idx int, sql, errorPin, schemaTemplate string, reach *cascades.ReachabilityCollector, plan planFn) Entry {
	e := Entry{File: file, Index: idx, ErrorPin: errorPin, SQL: collapse(sql)}
	physPlan, err := planGuarded(sql, schemaTemplate, reach, plan)
	var pe *panicErr
	switch {
	case errors.As(err, &pe):
		e.Plan = "<PLAN-PANIC: " + normalizeErr(pe.value) + ">"
	case err != nil:
		e.Plan = "<PLAN-ERROR: " + normalizeErr(err.Error()) + ">"
	case physPlan == nil:
		// Defensive: a nil plan with a nil error would otherwise render as
		// an empty line and diff as "no change".
		e.Plan = "<PLAN-ERROR: planner returned a nil plan with no error>"
	default:
		e.Plan = NormalizeAliases(collapse(physPlan.Explain()))
		e.Shape = ShapeOf(physPlan)
	}
	return e
}

// generatedAlias matches a planner-generated correlation identifier —
// `q$6381`, `Q$407245`. The numeric suffix comes from a PROCESS-GLOBAL
// counter, so the same query planned as the 5th statement and as the 5000th
// renders different text for an identical plan. Left raw, that alone makes
// every baseline comparison meaningless (the corpus dump would not even
// reproduce itself when run twice in one process).
var generatedAlias = regexp.MustCompile(`([A-Za-z_]*)\$([0-9]+)`)

// NormalizeAliases renumbers generated correlation identifiers densely from
// 0, in order of first appearance WITHIN this plan. Distinctness is
// preserved — two different correlations stay different, and the same
// correlation referenced twice stays the same — so the rendering still
// carries the plan's aliasing structure; only the global counter's value is
// erased. The prefix's case is preserved because `q$` and `Q$` come from
// different rendering paths and a swap between them is a real change.
//
// It is exported because any comparison of two EXPLAIN texts needs it, not
// just this package's baseline dump: the factory's second-plan oracle decides
// whether the disabled rule produced a DIFFERENT plan by comparing two EXPLAIN
// strings, and raw counter values would make two identical plans look
// different and count a tautological row comparison as a real one.
func NormalizeAliases(s string) string {
	seen := map[string]int{}
	return generatedAlias.ReplaceAllStringFunc(s, func(m string) string {
		parts := generatedAlias.FindStringSubmatch(m)
		prefix, id := parts[1], parts[2]
		n, ok := seen[id]
		if !ok {
			n = len(seen)
			seen[id] = n
		}
		return fmt.Sprintf("%s$%d", prefix, n)
	})
}

// panicErr carries a recovered planner panic as an error so the caller's
// single error path handles it.
type panicErr struct{ value string }

func (p *panicErr) Error() string { return "PANIC " + p.value }

// planGuarded runs the supplied harness and converts a panic into an error. A
// panic must not abort the corpus walk: the whole point is that every
// statement gets a line, including the ones that blow up.
//
// reach is threaded through the harness rather than switched on globally: the
// tally belongs to whoever asked for it. A corpus walk under t.Parallel
// alongside sibling walks is exactly the shape that made the old package-level
// tally report 3x its true value.
func planGuarded(sql, schemaTemplate string, reach *cascades.ReachabilityCollector, plan planFn) (physPlan plans.RecordQueryPlan, err error) {
	defer func() {
		if r := recover(); r != nil {
			physPlan, err = nil, &panicErr{value: fmt.Sprint(r)}
		}
	}()
	return plan(sql, schemaTemplate, reach)
}

// ShapeOf renders the plan's structural skeleton: the Go type of each node,
// indented two spaces per level, in GetChildren() order (documented stable).
// A nil child renders as `<nil>` — RFC-183's shells are exactly that state,
// so the baseline must be able to say it out loud.
//
// Exported because it is the repo's one STABLE-BY-CONSTRUCTION plan
// fingerprint: it carries no aliases, no counters and no version-dependent
// cost numbers, so the same plan renders identically across runs and across
// releases. plans.PlanHash is explicitly NOT that (plan_hash.go documents its
// value as free to change across releases), which is why the RFC-201 factory
// dedups on this rendering instead. Type identity alone is COARSE — two plans
// differing only in which index or predicate they use collapse together — so a
// dedup key must pair it with a description of the query, never use it alone.
func ShapeOf(p plans.RecordQueryPlan) []string {
	var out []string
	var walk func(n plans.RecordQueryPlan, depth int)
	walk = func(n plans.RecordQueryPlan, depth int) {
		indent := strings.Repeat("  ", depth)
		if n == nil {
			out = append(out, indent+"<nil>")
			return
		}
		if depth >= maxShapeDepth {
			out = append(out, indent+"<truncated: depth limit>")
			return
		}
		out = append(out, indent+planTypeName(n))
		for _, c := range n.GetChildren() {
			walk(c, depth+1)
		}
	}
	walk(p, 0)
	return out
}

// planTypeName is the node's Go type name without its package qualifier or
// pointer star — "RecordQueryIndexPlan", not "*plans.RecordQueryIndexPlan".
// Type identity is the stable structural signal; the package path is noise.
func planTypeName(p plans.RecordQueryPlan) string {
	name := fmt.Sprintf("%T", p)
	name = strings.TrimPrefix(name, "*")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// hexAddr matches anything that looks like a pointer address so a marker
// line stays byte-identical across runs. An address in an error message is
// the classic source of a baseline that diffs against itself.
var hexAddr = regexp.MustCompile(`0x[0-9a-fA-F]+`)

// normalizeErr makes an error message deterministic and single-line:
// addresses redacted, whitespace collapsed, length capped, and the marker's
// own delimiters neutralised so a message containing ">" cannot forge the
// end of the marker or break the parser.
func normalizeErr(msg string) string {
	msg = hexAddr.ReplaceAllString(msg, "0xADDR")
	msg = collapse(msg)
	msg = strings.ReplaceAll(msg, ">", "›")
	const cap = 300
	if len(msg) > cap {
		msg = msg[:cap] + "…"
	}
	return msg
}

// collapse squeezes all runs of whitespace (including newlines, which the
// corpus's block-scalar SQL is full of) into single spaces and trims. This
// is what keeps every rendering exactly one line, so a diff hunk maps to
// exactly one query.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// Render writes entries in the baseline text format.
//
// The format is line-oriented and entry-keyed so both `diff -u` and Parse
// work on it:
//
//	# explain-baseline/v2
//	# files=334 queries=2407 dml=276 non_query=45 plan_errors=255 unexpected_errors=4
//	#
//	=== aggregate_expr.yaml#3
//	sql:   SELECT COUNT(*) FROM T
//	plan:  StreamingAgg(keys=[], Scan(T))
//	shape: RecordQueryStreamingAggregationPlan
//	shape:   RecordQueryScanPlan
//	=== bad_column.yaml#0 expect-error=42703
//	sql:   SELECT nope FROM T
//	plan:  <PLAN-ERROR: 42703: Unknown column NOPE›
//
// Every payload line carries its own prefix, so no field can be confused
// with a structural line and a multi-node shape stays greppable.
func Render(entries []Entry, st Stats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", formatVersion)
	fmt.Fprintf(&b, statsTemplate+"\n",
		st.Files, st.Queries, st.DML, st.NonQuery, st.PlanErrors, st.UnexpectedErrors)
	b.WriteString("#\n")
	b.WriteString("# Source: internal/relational/conformance/yamsql/testdata/*.yaml, planned through\n")
	b.WriteString("# embedded.PlanPhysicalForTest (SELECT) and PlanPhysicalDMLForTest\n")
	b.WriteString("# (DELETE/UPDATE) — full Cascades, no FDB, default statistics.\n")
	b.WriteString("# Regenerate: go run ./cmd/explain-differ dump -out <file>\n")
	b.WriteString("#\n")
	for _, e := range entries {
		if e.ErrorPin != "" {
			fmt.Fprintf(&b, "=== %s expect-error=%s\n", e.Key(), e.ErrorPin)
		} else {
			fmt.Fprintf(&b, "=== %s\n", e.Key())
		}
		fmt.Fprintf(&b, "sql:   %s\n", e.SQL)
		fmt.Fprintf(&b, "plan:  %s\n", e.Plan)
		for _, line := range e.Shape {
			fmt.Fprintf(&b, "shape: %s\n", line)
		}
	}
	return b.String()
}

// GenerateBaseline is the one-call entry point: walk dir, plan everything,
// render. Used by cmd/explain-differ and by the package's own tests, so the
// tool and the tests can never drift apart.
func GenerateBaseline(dir string) (string, Stats, error) {
	return GenerateBaselineWithReachability(dir, nil)
}

// GenerateBaselineWithReachability is GenerateBaseline with RFC-183's
// yield-time plan-reachability accounting routed into the caller's collector.
//
// The collector is a PARAMETER, so a corpus walk's tally is the corpus walk's
// alone. The reachability ratchet and several sibling tests in this package all
// plan this same corpus under t.Parallel; when the tally was package state in
// cascades they summed into one number and the ratchet read edges=53748 for a
// true 17916. nil = collect nothing.
func GenerateBaselineWithReachability(dir string, reach *cascades.ReachabilityCollector) (string, Stats, error) {
	entries, st, err := collect(dir, reach, false)
	if err != nil {
		return "", Stats{}, err
	}
	return Render(entries, st), st, nil
}
