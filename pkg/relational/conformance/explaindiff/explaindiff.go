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

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
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
	return collect(dir, nil)
}

// CollectWithReachability is Collect with RFC-183's yield-time plan-reachability
// accounting routed into the caller's collector. nil = collect nothing.
func CollectWithReachability(dir string, reach *cascades.ReachabilityCollector) ([]Entry, Stats, error) {
	return collect(dir, reach)
}

func collect(dir string, reach *cascades.ReachabilityCollector) ([]Entry, Stats, error) {
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
			e := planOne(base, i, t.Query, t.EffectiveErrorCode(), s.SchemaTemplate, reach)
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
func planOne(file string, idx int, sql, errorPin, schemaTemplate string, reach *cascades.ReachabilityCollector) Entry {
	e := Entry{File: file, Index: idx, ErrorPin: errorPin, SQL: collapse(sql)}
	plan, err := planGuarded(sql, schemaTemplate, reach)
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
func planGuarded(sql, schemaTemplate string, reach *cascades.ReachabilityCollector) (plan plans.RecordQueryPlan, err error) {
	defer func() {
		if r := recover(); r != nil {
			plan, err = nil, &panicErr{value: fmt.Sprint(r)}
		}
	}()
	// Statistics are nil: planner defaults. See the package doc — the
	// baseline is a regression key, not a live-cardinality prediction.
	//
	// reach is threaded rather than switched on globally: the tally belongs to
	// whoever asked for it. A corpus walk under t.Parallel alongside sibling
	// walks is exactly the shape that made the old package-level tally report
	// 3x its true value.
	return embedded.PlanPhysicalForTestWithReachability(sql, schemaTemplate, nil, reach)
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
	fmt.Fprintf(&b, statsTemplate+"\n",
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

// Baseline is a parsed baseline file: its header plus its entries. The header
// is not decoration — the format version decides whether the two files are
// even comparable, and the stats line is the only way to tell a baseline that
// legitimately holds N entries from one whose dump was killed after N.
type Baseline struct {
	// Version is the format tag from the first header line.
	Version string
	// Stats is the reconciliation header the dump wrote.
	Stats Stats
	// Entries are the per-query records, in file order.
	Entries []Entry
	// Path is where the baseline was read from, when it came from disk.
	// ValidateDiffInputs uses it to reject a file diffed against itself.
	Path string
}

// header line prefixes. Kept as constants because Render writes them and
// Parse enforces them; a drift between the two is exactly the bug that let
// the version tag go unchecked.
const (
	statsPrefix   = "# files="
	statsTemplate = "# files=%d queries=%d non_query=%d plan_errors=%d unexpected_errors=%d"
)

// Parse reads back a rendered baseline. Round-tripping through Parse is what
// lets Diff report per-QUERY verdicts instead of per-line text hunks.
//
// The header is PARSED, not skipped:
//
//   - a formatVersion other than the current one is refused outright, because
//     a rendering change makes every entry differ for reasons that have
//     nothing to do with the planner;
//   - the stats line must be present and well-formed;
//   - the stats line's `queries=` must equal the number of entries that
//     follow. A dump interrupted mid-write keeps its header count and loses
//     the tail, which is precisely the truncation this check catches.
func Parse(text string) (*Baseline, error) {
	var (
		b   Baseline
		cur *Entry
	)
	flush := func() {
		if cur != nil {
			b.Entries = append(b.Entries, *cur)
			cur = nil
		}
	}
	sawVersion, sawStats := false, false
	for n, line := range strings.Split(text, "\n") {
		lineNo := n + 1
		switch {
		case !sawVersion && strings.HasPrefix(line, "# ") && !strings.HasPrefix(line, statsPrefix):
			sawVersion = true
			b.Version = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			if b.Version != formatVersion {
				return nil, fmt.Errorf(
					"line %d: baseline format is %q but this build writes %q — regenerate both baselines with `go run ./cmd/explain-differ dump`",
					lineNo, b.Version, formatVersion)
			}
		case !sawStats && strings.HasPrefix(line, statsPrefix):
			if !sawVersion {
				return nil, fmt.Errorf("line %d: stats header before the format-version header", lineNo)
			}
			sawStats = true
			var st Stats
			if _, err := fmt.Sscanf(line, statsTemplate,
				&st.Files, &st.Queries, &st.NonQuery, &st.PlanErrors, &st.UnexpectedErrors); err != nil {
				return nil, fmt.Errorf("line %d: malformed stats header %q: %w", lineNo, line, err)
			}
			b.Stats = st
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

	switch {
	case !sawVersion:
		return nil, fmt.Errorf("baseline has no `# %s` format-version header — not an explain baseline (regenerate with `go run ./cmd/explain-differ dump`)", formatVersion)
	case !sawStats:
		return nil, errors.New("baseline has no `# files=… queries=…` stats header — the dump did not finish writing it")
	case b.Stats.Queries != len(b.Entries):
		return nil, fmt.Errorf(
			"baseline is TRUNCATED: header declares queries=%d but the file holds %d entries — the dump was interrupted; regenerate it",
			b.Stats.Queries, len(b.Entries))
	}
	return &b, nil
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
	// Mispaired counts file#index positions held by a DIFFERENT query on
	// each side — the fingerprint of a corpus insertion, removal, or
	// reorder. Non-zero means the ADDED/REMOVED noise below is corpus
	// churn, not a planner change.
	Mispaired int
}

// Clean reports whether the two baselines are identical.
func (r DiffReport) Clean() bool { return len(r.Deltas) == 0 }

// pairKey is the identity Diff pairs entries on: the corpus position AND the
// query text.
//
// file#index alone is NOT an identity. Inserting a stanza in the middle of a
// corpus file renumbers every following stanza, so position N in the old
// baseline and position N in the new one are different QUERIES — pairing them
// prints one query's SQL beside another query's plans and reports the
// mismatch as a shape flip. Including the SQL makes a shifted entry fall out
// as REMOVED + ADDED, which is what actually happened.
type pairKey struct {
	key string // file#index
	sql string
}

// Diff compares two baselines. Entries pair only when both their corpus
// position AND their SQL match; a corpus insertion, removal, or reorder
// therefore surfaces as ADDED/REMOVED (and is counted in Mispaired) instead
// of being misattributed as a plan change on an unrelated query.
func Diff(oldEntries, newEntries []Entry) DiffReport {
	oldByKey := make(map[pairKey]Entry, len(oldEntries))
	oldSQL := make(map[string]string, len(oldEntries))
	for _, e := range oldEntries {
		oldByKey[pairKey{e.Key(), e.SQL}] = e
		oldSQL[e.Key()] = e.SQL
	}
	newByKey := make(map[pairKey]Entry, len(newEntries))
	newSQL := make(map[string]string, len(newEntries))
	for _, e := range newEntries {
		newByKey[pairKey{e.Key(), e.SQL}] = e
		newSQL[e.Key()] = e.SQL
	}

	rep := DiffReport{TotalOld: len(oldEntries), TotalNew: len(newEntries)}
	// A position occupied on both sides by DIFFERENT queries is the
	// mispairing signal: the corpus moved under the baseline.
	for k, os := range oldSQL {
		if ns, ok := newSQL[k]; ok && ns != os {
			rep.Mispaired++
		}
	}

	// Walk the union in sorted order — never map order — so the report is
	// byte-stable and itself diffable.
	keys := make([]pairKey, 0, len(oldByKey)+len(newByKey))
	for k := range oldByKey {
		keys = append(keys, k)
	}
	for k := range newByKey {
		if _, dup := oldByKey[k]; !dup {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].key != keys[j].key {
			return lessKey(keys[i].key, keys[j].key)
		}
		return keys[i].sql < keys[j].sql
	})

	for _, pk := range keys {
		k := pk.key
		o, inOld := oldByKey[pk]
		n, inNew := newByKey[pk]
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
	fmt.Fprintf(&b, "# shape_flips=%d plan_regressions=%d plan_recoveries=%d mispaired=%d\n",
		r.ShapeFlips, r.Regressions, r.Recoveries, r.Mispaired)
	if r.Clean() {
		b.WriteString("#\n# CLEAN — no plan-shape change across the corpus.\n")
		return b.String()
	}
	if r.Mispaired > 0 {
		fmt.Fprintf(&b, "#\n# WARNING: %d corpus positions hold a different query on each side.\n"+
			"# The corpus itself changed (stanza inserted/removed/reordered), so the\n"+
			"# ADDED/REMOVED deltas below are position shifts, not planner changes.\n"+
			"# Re-run the comparison against baselines from the SAME corpus revision.\n", r.Mispaired)
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
	entries, st, err := collect(dir, reach)
	if err != nil {
		return "", Stats{}, err
	}
	return Render(entries, st), st, nil
}

// LoadBaseline reads and parses a baseline file from disk.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	b, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	b.Path = path
	return b, nil
}

// MinBaselineEntries is the floor below which a baseline is treated as
// broken rather than small. The yamsql corpus plans ~2400 queries; a dump
// holding a double-digit count means the glob, the loader, or the dump run
// itself failed, and letting that compare CLEAN is the single worst failure
// mode this harness has — it reports "no plan changed" because it looked at
// nothing.
const MinBaselineEntries = 100

// MaxEntryCountDrift is the largest relative gap tolerated between the two
// baselines' entry counts, as a fraction of the larger side. A real corpus
// edit moves a handful of stanzas out of thousands; a dump that was killed
// or aborted loses a whole tail. 10% sits far above the former and far below
// the latter.
const MaxEntryCountDrift = 0.10

// ValidateDiffInputs rejects baseline pairs that cannot produce meaningful
// evidence, BEFORE Diff gets a chance to report CLEAN on them. Every check
// here exists because passing it silently is worse than failing loudly: this
// harness's whole job is to be the thing that says "nothing moved", so it
// must refuse to say that when it did not actually look.
func ValidateDiffInputs(oldB, newB *Baseline) error {
	if oldB == nil || newB == nil {
		return errors.New("both baselines must be loaded")
	}
	if err := checkDistinctSources(oldB.Path, newB.Path); err != nil {
		return err
	}
	for _, b := range []*Baseline{oldB, newB} {
		switch {
		case len(b.Entries) == 0:
			return fmt.Errorf("baseline %s holds ZERO entries — it proves nothing; regenerate it with `go run ./cmd/explain-differ dump`", describe(b))
		case len(b.Entries) < MinBaselineEntries:
			return fmt.Errorf("baseline %s holds only %d entries (floor is %d) — the dump did not cover the corpus; regenerate it",
				describe(b), len(b.Entries), MinBaselineEntries)
		}
	}
	lo, hi := len(oldB.Entries), len(newB.Entries)
	if lo > hi {
		lo, hi = hi, lo
	}
	if drift := float64(hi-lo) / float64(hi); drift > MaxEntryCountDrift {
		return fmt.Errorf(
			"baselines differ by %.1f%% in entry count (old=%d new=%d, tolerance %.0f%%) — one dump is truncated or ran against a different corpus; regenerate both",
			drift*100, len(oldB.Entries), len(newB.Entries), MaxEntryCountDrift*100)
	}
	return nil
}

// checkDistinctSources rejects diffing a file against itself. It is always
// operator error (a repeated shell variable, a copy-pasted path) and it
// always reports CLEAN, which reads as "the change moved no plan" — the
// exact conclusion the harness exists to justify.
//
// The check is on the SOURCE, not the content: two dumps of the same tree
// are byte-identical by design, and that comparison is the legitimate
// smoke test that the harness still works.
func checkDistinctSources(oldPath, newPath string) error {
	if oldPath == "" || newPath == "" {
		return nil // in-memory baselines; there is no file to confuse.
	}
	same := oldPath == newPath
	if !same {
		if a, err := filepath.Abs(oldPath); err == nil {
			if bp, err := filepath.Abs(newPath); err == nil {
				same = a == bp
			}
		}
	}
	if !same {
		// Catches the symlink / hard-link spellings of the same file that a
		// string comparison misses.
		if ai, err := os.Stat(oldPath); err == nil {
			if bi, err := os.Stat(newPath); err == nil {
				same = os.SameFile(ai, bi)
			}
		}
	}
	if same {
		return fmt.Errorf("refusing to diff %q against itself: a file always matches itself, so the CLEAN verdict would be meaningless — pass the BEFORE and AFTER baselines", oldPath)
	}
	return nil
}

// describe names a baseline for an error message: its path when it has one.
func describe(b *Baseline) string {
	if b.Path != "" {
		return b.Path
	}
	return "<in-memory>"
}
