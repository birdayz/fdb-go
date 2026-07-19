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
//     rowdiff harness plan through. There is one planning path.
//
// NO FDB REQUIRED. Planning is metadata-only: the corpus scenario's
// `schema_template` is compiled to in-memory RecordMetaData and the query is
// planned against it. `setup:` INSERTs are not replayed, so table statistics
// are the planner defaults rather than the live cardinalities the driver
// would fetch from FDB. That is a deliberate trade: the baseline is a
// REGRESSION key (same input → same output → any delta is a real planner
// change), not a prediction of the plan the live driver picks for a
// populated table.
package explaindiff

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/conformance/yamsql"
	"fdb.dev/pkg/relational/core/embedded"
)

// formatVersion is stamped into every baseline header. Bump it when the
// rendering changes so a stale baseline is diagnosed rather than silently
// mis-diffed against a new one.
const formatVersion = "explain-baseline/v1"

// maxShapeDepth caps the shape-tree walk. A physical plan is a tree, so the
// cap can only be hit by a cycle introduced by a planner bug — which is
// itself worth surfacing as a stable marker rather than as a stack overflow.
const maxShapeDepth = 200

// Entry is one corpus query's planned shape.
type Entry struct {
	// File is the corpus file's base name (e.g. "aggregate_expr.yaml").
	File string
	// Index is the 0-based position of the test within that file's
	// `tests:` sequence. File+Index is the diff key: it points a reviewer
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
// and plans every query test in it.
//
// Only tests routed through the Query path (SELECT / WITH / VALUES, per
// yamsql.IsQuery) are planned: `exec:` DML stanzas are sequencing steps, and
// the planner harness takes a SELECT statement. Their count is reported in
// Stats so the corpus total still reconciles.
//
// Scenarios whose schema_template does not compile yield one
// `<PLAN-ERROR: …>` entry per query in the file rather than vanishing — a
// schema regression must not silently shrink the baseline.
//
// The returned slice is sorted by (file, index) and is byte-stable across
// runs: no map iteration, no timestamps, no addresses.
func Collect(dir string) ([]Entry, Stats, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, Stats{}, fmt.Errorf("glob %s: %w", dir, err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return nil, Stats{}, fmt.Errorf("no *.yaml scenarios under %s", dir)
	}

	var entries []Entry
	st := Stats{Files: len(matches)}
	for _, path := range matches {
		base := filepath.Base(path)
		s, loadErr := yamsql.Load(path)
		if loadErr != nil {
			// A corpus file that no longer loads is a hard error: silently
			// dropping it would shrink the baseline and read as "no change".
			return nil, Stats{}, fmt.Errorf("load %s: %w", base, loadErr)
		}
		for i, t := range s.Tests {
			if !yamsql.IsQuery(t.Query) {
				st.NonQuery++
				continue
			}
			st.Queries++
			e := planOne(base, i, t.Query, t.EffectiveErrorCode(), s.SchemaTemplate)
			if e.Failed() {
				st.PlanErrors++
			}
			if e.UnexpectedlyFailed() {
				st.UnexpectedErrors++
			}
			entries = append(entries, e)
		}
	}
	return entries, st, nil
}

// Stats are the reconciliation counts for a Collect run. They are derived,
// deterministic, and printed in the baseline header so a diff of two headers
// immediately says whether the corpus itself moved.
type Stats struct {
	// Files is the number of *.yaml scenarios walked.
	Files int
	// Queries is the number of planned query tests (the entry count).
	Queries int
	// NonQuery is the number of `exec:` DML stanzas skipped.
	NonQuery int
	// PlanErrors is how many entries are failure markers.
	PlanErrors int
	// UnexpectedErrors is how many of those failures have no corpus error
	// pin — queries that are supposed to plan and don't.
	UnexpectedErrors int
}

// planOne plans a single query against the scenario's schema template,
// converting any error OR panic into a stable marker entry.
func planOne(file string, idx int, sql, errorPin, schemaTemplate string) Entry {
	e := Entry{File: file, Index: idx, ErrorPin: errorPin, SQL: collapse(sql)}
	plan, err := planGuarded(sql, schemaTemplate)
	var pe *panicErr
	switch {
	case errors.As(err, &pe):
		e.Plan = "<PLAN-PANIC: " + normalizeErr(pe.value) + ">"
	case err != nil:
		e.Plan = "<PLAN-ERROR: " + normalizeErr(err.Error()) + ">"
	case plan == nil:
		// Defensive: a nil plan with a nil error would otherwise render as
		// an empty line and diff as "no change".
		e.Plan = "<PLAN-ERROR: planner returned a nil plan with no error>"
	default:
		e.Plan = normalizeAliases(collapse(plan.Explain()))
		e.Shape = shapeOf(plan)
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

// normalizeAliases renumbers generated correlation identifiers densely from
// 0, in order of first appearance WITHIN this plan. Distinctness is
// preserved — two different correlations stay different, and the same
// correlation referenced twice stays the same — so the rendering still
// carries the plan's aliasing structure; only the global counter's value is
// erased. The prefix's case is preserved because `q$` and `Q$` come from
// different rendering paths and a swap between them is a real change.
func normalizeAliases(s string) string {
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

// planGuarded runs the full Cascades pipeline and converts a panic into an
// error. A panic must not abort the corpus walk: the whole point is that
// every query gets a line, including the ones that blow up.
func planGuarded(sql, schemaTemplate string) (plan plans.RecordQueryPlan, err error) {
	defer func() {
		if r := recover(); r != nil {
			plan, err = nil, &panicErr{value: fmt.Sprint(r)}
		}
	}()
	// Statistics are nil: planner defaults. See the package doc — the
	// baseline is a regression key, not a live-cardinality prediction.
	return embedded.PlanPhysicalForTest(sql, schemaTemplate, nil)
}

// shapeOf renders the plan's structural skeleton: the Go type of each node,
// indented two spaces per level, in GetChildren() order (documented stable).
// A nil child renders as `<nil>` — RFC-183's shells are exactly that state,
// so the baseline must be able to say it out loud.
func shapeOf(p plans.RecordQueryPlan) []string {
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
//	# explain-baseline/v1
//	# files=334 queries=2407 non_query=252 plan_errors=255 unexpected_errors=4
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
	fmt.Fprintf(&b, "# files=%d queries=%d non_query=%d plan_errors=%d unexpected_errors=%d\n",
		st.Files, st.Queries, st.NonQuery, st.PlanErrors, st.UnexpectedErrors)
	b.WriteString("#\n")
	b.WriteString("# Source: pkg/relational/conformance/yamsql/testdata/*.yaml, planned through\n")
	b.WriteString("# embedded.PlanPhysicalForTest (full Cascades, no FDB, default statistics).\n")
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

// Parse reads back a rendered baseline. Round-tripping through Parse is what
// lets Diff report per-QUERY verdicts instead of per-line text hunks.
func Parse(text string) ([]Entry, error) {
	var (
		entries []Entry
		cur     *Entry
	)
	flush := func() {
		if cur != nil {
			entries = append(entries, *cur)
			cur = nil
		}
	}
	for n, line := range strings.Split(text, "\n") {
		lineNo := n + 1
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "=== "):
			flush()
			key := strings.TrimPrefix(line, "=== ")
			pin := ""
			if k, tail, ok := strings.Cut(key, " expect-error="); ok {
				key, pin = k, tail
			}
			file, idx, err := splitKey(key)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			cur = &Entry{File: file, Index: idx, ErrorPin: pin}
		case cur == nil:
			return nil, fmt.Errorf("line %d: payload before any === entry header", lineNo)
		case strings.HasPrefix(line, "sql:   "):
			cur.SQL = strings.TrimPrefix(line, "sql:   ")
		case strings.HasPrefix(line, "plan:  "):
			cur.Plan = strings.TrimPrefix(line, "plan:  ")
		case strings.HasPrefix(line, "shape: "):
			cur.Shape = append(cur.Shape, strings.TrimPrefix(line, "shape: "))
		default:
			return nil, fmt.Errorf("line %d: unrecognized line %q", lineNo, line)
		}
	}
	flush()
	return entries, nil
}

// splitKey parses a "file.yaml#12" diff key.
func splitKey(key string) (string, int, error) {
	i := strings.LastIndex(key, "#")
	if i < 0 {
		return "", 0, fmt.Errorf("malformed entry key %q: want <file>#<index>", key)
	}
	idx, err := strconv.Atoi(key[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("malformed entry key %q: %w", key, err)
	}
	return key[:i], idx, nil
}

// Change classifies one key's verdict between two baselines.
type Change int

const (
	// Changed means the key exists on both sides with a different plan or shape.
	Changed Change = iota
	// Added means the key exists only in the new baseline.
	Added
	// Removed means the key exists only in the old baseline.
	Removed
)

// String renders a Change as its report tag.
func (c Change) String() string {
	switch c {
	case Added:
		return "ADDED"
	case Removed:
		return "REMOVED"
	}
	return "CHANGED"
}

// Delta is one differing key.
type Delta struct {
	// Key is the file#index the delta belongs to.
	Key string
	// Kind classifies the delta.
	Kind Change
	// SQL is the query text (new side's, falling back to old).
	SQL string
	// OldPlan / NewPlan are the two renderings; the missing side of an
	// ADDED / REMOVED delta is empty.
	OldPlan, NewPlan string
	// OldShape / NewShape are the two structural skeletons.
	OldShape, NewShape []string
	// ShapeChanged distinguishes a STRUCTURAL flip (different operators or
	// tree) from a rendering-only delta (same skeleton, different labels).
	// RFC-183's risk is the former; separating them keeps a formatting
	// churn from drowning a real flip. Only set when BOTH sides planned —
	// a planned↔error transition is reported by RegressedToError /
	// RecoveredFromError, and double-tagging it as a shape flip would
	// inflate the count the exit gate reads.
	ShapeChanged bool
	// RegressedToError marks old-planned → new-fails. This is the single
	// most important signal in the report: a query that stopped planning.
	RegressedToError bool
	// RecoveredFromError marks old-failed → new-plans.
	RecoveredFromError bool
}

// DiffReport is the full verdict between two baselines.
type DiffReport struct {
	// Deltas are the differing keys, sorted by (file, index).
	Deltas []Delta
	// Same is the count of byte-identical entries.
	Same int
	// TotalOld / TotalNew are the two baselines' entry counts.
	TotalOld, TotalNew int
	// ShapeFlips counts deltas whose structural skeleton moved.
	ShapeFlips int
	// Regressions counts planned → error transitions.
	Regressions int
	// Recoveries counts error → planned transitions.
	Recoveries int
}

// Clean reports whether the two baselines are identical.
func (r DiffReport) Clean() bool { return len(r.Deltas) == 0 }

// Diff compares two baselines by key. Both sides are indexed by file#index,
// so a query inserted in the middle of a corpus file shifts the following
// keys and shows as ADDED/REMOVED rather than as a phantom plan change —
// the reason the report distinguishes those kinds.
func Diff(oldEntries, newEntries []Entry) DiffReport {
	oldByKey := make(map[string]Entry, len(oldEntries))
	for _, e := range oldEntries {
		oldByKey[e.Key()] = e
	}
	newByKey := make(map[string]Entry, len(newEntries))
	for _, e := range newEntries {
		newByKey[e.Key()] = e
	}

	rep := DiffReport{TotalOld: len(oldEntries), TotalNew: len(newEntries)}
	// Walk the union in sorted order — never map order — so the report is
	// byte-stable and itself diffable.
	keys := make([]string, 0, len(oldByKey)+len(newByKey))
	for k := range oldByKey {
		keys = append(keys, k)
	}
	for k := range newByKey {
		if _, dup := oldByKey[k]; !dup {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return lessKey(keys[i], keys[j]) })

	for _, k := range keys {
		o, inOld := oldByKey[k]
		n, inNew := newByKey[k]
		switch {
		case inOld && !inNew:
			rep.Deltas = append(rep.Deltas, Delta{
				Key: k, Kind: Removed, SQL: o.SQL, OldPlan: o.Plan, OldShape: o.Shape,
			})
		case !inOld && inNew:
			rep.Deltas = append(rep.Deltas, Delta{
				Key: k, Kind: Added, SQL: n.SQL, NewPlan: n.Plan, NewShape: n.Shape,
			})
		default:
			bothPlanned := !o.Failed() && !n.Failed()
			shapeMoved := bothPlanned && !equalLines(o.Shape, n.Shape)
			if o.Plan == n.Plan && equalLines(o.Shape, n.Shape) {
				rep.Same++
				continue
			}
			d := Delta{
				Key: k, Kind: Changed, SQL: n.SQL,
				OldPlan: o.Plan, NewPlan: n.Plan,
				OldShape: o.Shape, NewShape: n.Shape,
				ShapeChanged:       shapeMoved,
				RegressedToError:   !o.Failed() && n.Failed(),
				RecoveredFromError: o.Failed() && !n.Failed(),
			}
			if d.ShapeChanged {
				rep.ShapeFlips++
			}
			if d.RegressedToError {
				rep.Regressions++
			}
			if d.RecoveredFromError {
				rep.Recoveries++
			}
			rep.Deltas = append(rep.Deltas, d)
		}
	}
	return rep
}

// lessKey orders keys by file name then numeric index, so file#2 sorts
// before file#10 (a lexical sort would not).
func lessKey(a, b string) bool {
	af, ai, aerr := splitKey(a)
	bf, bi, berr := splitKey(b)
	if aerr != nil || berr != nil {
		return a < b
	}
	if af != bf {
		return af < bf
	}
	return ai < bi
}

func equalLines(a, b []string) bool {
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

// RenderDiff formats a DiffReport for a human. The summary leads with the
// counts that decide RFC-183's P0 exit criteria: structural flips and
// plan-time regressions.
func RenderDiff(r DiffReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s diff\n", formatVersion)
	fmt.Fprintf(&b, "# entries: old=%d new=%d identical=%d differing=%d\n",
		r.TotalOld, r.TotalNew, r.Same, len(r.Deltas))
	fmt.Fprintf(&b, "# shape_flips=%d plan_regressions=%d plan_recoveries=%d\n",
		r.ShapeFlips, r.Regressions, r.Recoveries)
	if r.Clean() {
		b.WriteString("#\n# CLEAN — no plan-shape change across the corpus.\n")
		return b.String()
	}
	b.WriteString("#\n")
	for _, d := range r.Deltas {
		tags := d.Kind.String()
		if d.ShapeChanged {
			tags += " SHAPE"
		}
		if d.RegressedToError {
			tags += " STOPPED-PLANNING"
		}
		if d.RecoveredFromError {
			tags += " NOW-PLANS"
		}
		fmt.Fprintf(&b, "=== %s [%s]\n", d.Key, tags)
		fmt.Fprintf(&b, "sql:   %s\n", d.SQL)
		if d.Kind != Added {
			fmt.Fprintf(&b, "-plan: %s\n", d.OldPlan)
		}
		if d.Kind != Removed {
			fmt.Fprintf(&b, "+plan: %s\n", d.NewPlan)
		}
		if d.ShapeChanged {
			for _, l := range d.OldShape {
				fmt.Fprintf(&b, "-shape: %s\n", l)
			}
			for _, l := range d.NewShape {
				fmt.Fprintf(&b, "+shape: %s\n", l)
			}
		}
	}
	return b.String()
}

// GenerateBaseline is the one-call entry point: walk dir, plan everything,
// render. Used by cmd/explain-differ and by the package's own tests, so the
// tool and the tests can never drift apart.
func GenerateBaseline(dir string) (string, Stats, error) {
	entries, st, err := Collect(dir)
	if err != nil {
		return "", Stats{}, err
	}
	return Render(entries, st), st, nil
}

// LoadBaseline reads and parses a baseline file from disk.
func LoadBaseline(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	entries, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return entries, nil
}
