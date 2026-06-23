// Package plandiff is the Phase 4.-1 plan-equivalence harness from
// RFC-022 §4.-1. Its job: feed a SQL string + schema into both the Go
// planner and Java's Cascades planner, capture each side's plan tree,
// and produce a structural diff plus a plan-cache-key hash diff.
//
// First-cut scope: Go-side baseline only.
// The naive Go generator is exercised against an embedded query corpus;
// each query's plan tree is captured via `query.Plan.Explain()` and a
// stable Go-internal hash is computed over the normalised tree text.
// The Java engine is stubbed (`ErrJavaUnimplemented`) — the
// `fdb-relational` Maven deps that would let `conformance_server.java`
// run real planning aren't wired into Bazel yet (TODO.md §CRITICAL
// "Java↔Go SQL conformance harness Phase B").
//
// Even Go-only, the harness is useful as a regression detector: any
// change to the naive planner that alters the plan tree for known
// queries surfaces as a diff against the golden output. Once Java is
// wired the same harness shape will produce real Go-vs-Java diffs.
//
// Output shape: a Report with one Diff per Query. Each Diff records the
// per-engine PlanResult (Tree text + cache-key hash + error), a Status
// (Agree / Diverge / EngineError / JavaUnimplemented), and a Detail
// string carrying the line-by-line tree diff when divergent.
package plandiff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/embedded"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query"
)

// Query is a single harness input: a SQL string with a name for
// reporting and an optional schema hint. SchemaTemplate is forwarded
// to engines that walk the catalog-aware planner path; engines that
// don't (today's text-only Go path) ignore it.
type Query struct {
	// Name identifies the query in reports. Must be unique within a
	// corpus.
	Name string
	// SQL is the statement under test.
	SQL string
	// SchemaTemplate is the CREATE SCHEMA TEMPLATE body (a sequence of
	// DDL statements). Optional; engines that don't consult metadata
	// (text-only Go) ignore it. Engines that DO (catalog-aware Go,
	// Java) use it to populate the catalog before planning.
	SchemaTemplate string
}

// PlanResult is one engine's output for one Query. Either Err is non-
// nil (engine failed) or Tree + Hash are populated.
type PlanResult struct {
	// Engine identifies the producer ("go" / "java").
	Engine string
	// Tree is the rendered plan tree, one line per node, indented to
	// reflect parent/child structure. Empty when Err is non-nil.
	Tree string
	// Hash is a stable Go-internal SHA-256 over the normalised Tree
	// text (whitespace-collapsed, trimmed). Empty when Err is non-nil.
	// Per RFC-024 we do NOT target Java hash compatibility — this is
	// a Go-internal regression key.
	Hash string
	// Err carries any engine error. Non-nil suppresses Tree/Hash.
	Err error
}

// Status classifies a Diff at a glance.
type Status int

const (
	// StatusUnknown is the zero value; uninitialised Diffs report it.
	StatusUnknown Status = iota
	// StatusAgree means both engines produced byte-identical
	// (normalised) Tree output.
	StatusAgree
	// StatusDiverge means both engines succeeded but produced
	// different Tree output.
	StatusDiverge
	// StatusGoError means Go errored — Java may or may not have run.
	StatusGoError
	// StatusJavaError means Java errored while Go succeeded.
	StatusJavaError
	// StatusBothError means both engines errored.
	StatusBothError
	// StatusJavaUnimplemented is the placeholder while
	// fdb-relational maven deps aren't wired into Bazel. Counts as a
	// soft failure — the report shows it but a CI gate that wants to
	// be Go-only-tolerant can ignore it.
	StatusJavaUnimplemented
	// StatusGoUnimplemented is the result-set harness's mirror —
	// Go runner returns ErrGoUnimplemented today (gated on Track C2's
	// QueryExecutor). Distinct from StatusJavaUnimplemented so reports
	// can tell which side is stubbed. Plan-tree harness never produces
	// this status (Go always works on plan-tree).
	StatusGoUnimplemented
)

// String renders Status as a short tag for reports.
func (s Status) String() string {
	switch s {
	case StatusAgree:
		return "AGREE"
	case StatusDiverge:
		return "DIVERGE"
	case StatusGoError:
		return "GO_ERROR"
	case StatusJavaError:
		return "JAVA_ERROR"
	case StatusBothError:
		return "BOTH_ERROR"
	case StatusJavaUnimplemented:
		return "JAVA_UNIMPL"
	case StatusGoUnimplemented:
		return "GO_UNIMPL"
	}
	return "UNKNOWN"
}

// Diff is the per-Query verdict.
type Diff struct {
	// Query is the input that produced this Diff.
	Query Query
	// Go is the Go engine's output.
	Go PlanResult
	// Java is the Java engine's output.
	Java PlanResult
	// Status classifies the pair.
	Status Status
	// Detail is the line-by-line tree diff for StatusDiverge, or the
	// engine error text for *Error statuses. Empty for AGREE.
	Detail string
}

// Summary aggregates Status counts across a Report.
type Summary struct {
	Total             int
	Agree             int
	Diverge           int
	GoError           int
	JavaError         int
	BothError         int
	JavaUnimplemented int
	GoUnimplemented   int
}

// Report is the harness's full output for a corpus run.
type Report struct {
	// Cases holds one Diff per Query, in input order.
	Cases []Diff
	// Summary aggregates Status counts.
	Summary Summary
}

// Engine produces a PlanResult for a Query. Implementations must be
// safe for concurrent use across queries — Engine.Plan is expected
// to be callable from multiple goroutines simultaneously, so a
// future parallelised Run is a drop-in change. Today's Run is
// sequential.
type Engine interface {
	Plan(ctx context.Context, q Query) PlanResult
}

// ErrJavaUnimplemented is returned by JavaEngine until a SqlPlanSteps
// step is added to `conformance_server.java`. See TODO.md §CRITICAL
// "Java↔Go SQL conformance harness Phase B".
//
// Status: fdb-relational-api / fdb-relational-core
// are wired into `conformance/BUILD.bazel` at version 4.12.11.0 (matches
// fdb-record-layer-core's pin). The remaining work is the Java step
// itself: take (sql, schema_template), plan via fdb-relational's
// `EmbeddedRelationalConnection`, return the rendered plan tree.
var ErrJavaUnimplemented = errors.New("plandiff: Java engine not wired (SqlPlanSteps.java step not yet added to conformance_server)")

// Run executes every query in `queries` against `goEng` and `javaEng`,
// produces per-query Diffs, and aggregates the Summary.
func Run(ctx context.Context, queries []Query, goEng, javaEng Engine) Report {
	if goEng == nil {
		goEng = noopEngine{name: "go"}
	}
	if javaEng == nil {
		javaEng = noopEngine{name: "java"}
	}
	out := Report{Cases: make([]Diff, 0, len(queries))}
	for _, q := range queries {
		gr := goEng.Plan(ctx, q)
		jr := javaEng.Plan(ctx, q)
		out.Cases = append(out.Cases, classify(q, gr, jr))
	}
	out.Summary = summarise(out.Cases)
	return out
}

// classify pairs the two PlanResults into a Diff with computed Status
// + Detail.
func classify(q Query, gr, jr PlanResult) Diff {
	d := Diff{Query: q, Go: gr, Java: jr}
	switch {
	case gr.Err != nil && jr.Err != nil:
		// Both errored — distinguish JavaUnimplemented from real Java
		// errors so the harness can opt-in to ignore the stub case.
		if errors.Is(jr.Err, ErrJavaUnimplemented) {
			d.Status = StatusGoError
		} else {
			d.Status = StatusBothError
		}
		d.Detail = fmt.Sprintf("go: %v\njava: %v", gr.Err, jr.Err)
	case gr.Err != nil:
		d.Status = StatusGoError
		d.Detail = fmt.Sprintf("go: %v", gr.Err)
	case jr.Err != nil:
		if errors.Is(jr.Err, ErrJavaUnimplemented) {
			d.Status = StatusJavaUnimplemented
			d.Detail = jr.Err.Error()
		} else {
			d.Status = StatusJavaError
			d.Detail = fmt.Sprintf("java: %v", jr.Err)
		}
	default:
		// Both succeeded — compare normalised trees.
		ng := normaliseTree(gr.Tree)
		nj := normaliseTree(jr.Tree)
		if ng == nj {
			d.Status = StatusAgree
		} else {
			d.Status = StatusDiverge
			d.Detail = lineDiff(ng, nj)
		}
	}
	return d
}

// summarise tallies Status counts across a Diff slice.
func summarise(cases []Diff) Summary {
	s := Summary{Total: len(cases)}
	for _, c := range cases {
		switch c.Status {
		case StatusAgree:
			s.Agree++
		case StatusDiverge:
			s.Diverge++
		case StatusGoError:
			s.GoError++
		case StatusJavaError:
			s.JavaError++
		case StatusBothError:
			s.BothError++
		case StatusJavaUnimplemented:
			s.JavaUnimplemented++
		case StatusGoUnimplemented:
			s.GoUnimplemented++
		}
	}
	return s
}

// normaliseTree collapses runs of whitespace + trims so structurally
// equivalent trees with different whitespace compare equal. Empty
// lines drop out so a trailing newline doesn't show as a phantom diff.
func normaliseTree(t string) string {
	lines := strings.Split(t, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// hashTree returns a hex SHA-256 of the normalised tree. Stable across
// runs, sensitive to any structural change. Used as a regression
// fingerprint and as the corpus-level golden key.
func hashTree(t string) string {
	h := sha256.Sum256([]byte(normaliseTree(t)))
	return hex.EncodeToString(h[:])
}

// lineDiff produces a unified-style diff between two trees, line by
// line. Symmetric (`-` for left-only, `+` for right-only). Cheap
// O(n+m) walk — good enough for harness reports; full LCS-style diff
// is overkill for plan trees that diverge by a few lines at most.
//
// Known limitation: transposed lines (same content present on both
// sides at different positions) are NOT emitted — both sides report
// the line as " " (context). The Status (Diverge) is correct because
// normaliseTree differs, but the Detail will look as if no lines
// diverged. This is unlikely to fire on today's flat-ish naive
// planner output; a Cascades-pipeline reorder (e.g. Filter pushed
// before Project on one engine, not the other) would surface the
// gap. Swap to a real LCS diff (e.g. github.com/sergi/go-diff or
// hand-rolled Hunt-McIlroy) when transpositions become common
// enough to matter for triage.
func lineDiff(a, b string) string {
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	// Build a multiset for O(1) "line in other side?" lookup.
	bset := make(map[string]int, len(lb))
	for _, l := range lb {
		bset[l]++
	}
	aset := make(map[string]int, len(la))
	for _, l := range la {
		aset[l]++
	}
	var out []string
	out = append(out, "--- go")
	out = append(out, "+++ java")
	// Walk both in parallel for context; emit divergent lines per
	// side. Lines present in both at any position render as " ".
	max := len(la)
	if len(lb) > max {
		max = len(lb)
	}
	for i := 0; i < max; i++ {
		var aline, bline string
		if i < len(la) {
			aline = la[i]
		}
		if i < len(lb) {
			bline = lb[i]
		}
		if aline == bline {
			out = append(out, "  "+aline)
			continue
		}
		if i < len(la) && bset[aline] == 0 {
			out = append(out, "- "+aline)
		}
		if i < len(lb) && aset[bline] == 0 {
			out = append(out, "+ "+bline)
		}
	}
	return strings.Join(out, "\n")
}

// FormatReport renders a human-readable summary + per-case status. The
// report's first line is the Summary tag string, followed by one block
// per non-AGREE case (AGREE cases are summarised by count, not
// expanded — they're not interesting). Useful for `go test -v` output
// and CI logs.
func FormatReport(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "plandiff: %d total / %d agree / %d diverge / %d go-err / %d java-err / %d both-err / %d java-unimpl\n",
		r.Summary.Total, r.Summary.Agree, r.Summary.Diverge,
		r.Summary.GoError, r.Summary.JavaError, r.Summary.BothError,
		r.Summary.JavaUnimplemented)
	// Stable order by Query.Name so report output is deterministic.
	cases := append([]Diff(nil), r.Cases...)
	sort.SliceStable(cases, func(i, j int) bool { return cases[i].Query.Name < cases[j].Query.Name })
	for _, c := range cases {
		if c.Status == StatusAgree {
			continue
		}
		fmt.Fprintf(&b, "\n[%s] %s\n  sql: %s\n", c.Status, c.Query.Name, c.Query.SQL)
		if c.Detail != "" {
			// Indent detail two spaces.
			for _, l := range strings.Split(c.Detail, "\n") {
				fmt.Fprintf(&b, "  %s\n", l)
			}
		}
	}
	return b.String()
}

// goEngine is the Go-side implementation. Uses
// embedded.NewExplainOnlyGenerator (or the catalog-aware variant
// NewExplainOnlyGeneratorWithSchema when Query.SchemaTemplate is non-
// empty) to produce a plan without execution. RFC-022 §4.-1 Phase 3.
type goEngine struct{}

// NewGoEngine returns the Go-side Engine. It is stateless and safe
// for concurrent use.
func NewGoEngine() Engine { return goEngine{} }

func (goEngine) Plan(ctx context.Context, q Query) PlanResult {
	gen, err := buildGoGenerator(q)
	if err != nil {
		return PlanResult{Engine: "go", Err: err}
	}
	plan, err := gen.Plan(ctx, q.SQL)
	if err != nil {
		return PlanResult{Engine: "go", Err: err}
	}
	tree := plan.Explain()
	return PlanResult{Engine: "go", Tree: tree, Hash: hashTree(tree)}
}

// buildGoGenerator picks the catalog-aware constructor when the
// Query carries a SchemaTemplate (so WHERE / DELETE / UPDATE shapes
// route through buildLogicalPlanFor*WithCatalog and produce real
// cascades.predicates.QueryPredicate trees in Explain output) and falls back
// to the text-only generator otherwise.
func buildGoGenerator(q Query) (query.Generator, error) {
	if q.SchemaTemplate == "" {
		return embedded.NewExplainOnlyGenerator(), nil
	}
	return embedded.NewExplainOnlyGeneratorWithSchema(q.SchemaTemplate)
}

// javaEngine talks to the conformance Java server's `planSql` step
// (see `conformance/sql_plan_steps.java`). The harness POSTs a JSON
// payload to `baseURL/invoke` and reads the EXPLAIN PLAN column that
// fdb-relational's planner produces.
//
// A nil baseURL surfaces ErrJavaUnimplemented for every call so a CI
// gate that wants to be Go-only-tolerant can pass without a running
// conformance server. NewJavaEngine() with no args returns this
// nil-baseURL form; NewJavaEngineHTTP(url) returns the wired form.
type javaEngine struct {
	baseURL    string
	httpClient *http.Client
	// clusterFile is the FDB cluster-file content (NOT path) the Java
	// step needs to open a database. Plumbed in at construction so
	// each Plan() call doesn't have to thread it through Query.
	clusterFile string
}

// NewJavaEngine returns the unwired Java-side Engine. Every Plan()
// call returns ErrJavaUnimplemented. Use this when running the Go
// harness in isolation (no conformance server / FDB testcontainer
// available). Reports as StatusJavaUnimplemented in the diff.
func NewJavaEngine() Engine { return javaEngine{} }

// NewJavaEngineHTTP returns a Java-side Engine that drives the
// conformance server at `baseURL` (e.g. `http://127.0.0.1:35451`)
// against the FDB cluster whose cluster-file content is
// `clusterFileContent` (the actual config text, not a path — the
// Java step writes it to a file before opening the database, matching
// the existing ConformanceBase pattern).
//
// Each Plan() call POSTs to `baseURL/invoke` with step="planSql" and
// params={clusterFile, schemaTemplate, sql}. The Java step creates a
// uniquely-named schema template + database + schema for the call,
// runs `EXPLAIN <sql>`, returns the PLAN column, and tears the
// schema down — see `conformance/sql_plan_steps.java`.
func NewJavaEngineHTTP(baseURL, clusterFileContent string) Engine {
	return javaEngine{
		baseURL:     baseURL,
		clusterFile: clusterFileContent,
		// 120s, not 30s. Race-instrumented Go runs ~3× slower under
		// `--@rules_go//go/config:race`, and the harness drives the
		// Java conformance server in the same process tree as the
		// race-instrumented test code; under runner memory pressure
		// the JVM also contends for CPU. 30s flaked when the race
		// build's first `planSql` calls (cold per-call schema
		// template + database + schema setup) exceeded the budget.
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

func (e javaEngine) Plan(ctx context.Context, q Query) PlanResult {
	if e.baseURL == "" {
		return PlanResult{Engine: "java", Err: ErrJavaUnimplemented}
	}
	tree, err := e.invokePlanSql(ctx, q)
	if err != nil {
		return PlanResult{Engine: "java", Err: err}
	}
	return PlanResult{Engine: "java", Tree: tree, Hash: hashTree(tree)}
}

// invokePlanSql posts to the conformance server's `planSql` step
// (see conformance/sql_plan_steps.java#planSql) and decodes the bare
// JSON-string result (the PLAN column text).
func (e javaEngine) invokePlanSql(ctx context.Context, q Query) (string, error) {
	var plan string
	err := invokeStep(ctx, e.httpClient, e.baseURL, "planSql", map[string]any{
		"clusterFile":    e.clusterFile,
		"schemaTemplate": q.SchemaTemplate,
		"sql":            q.SQL,
	}, &plan)
	return plan, err
}

// noopEngine is the fallback when Run is called with a nil Engine. It
// keeps Run's behaviour deterministic without crashing the test
// harness.
type noopEngine struct{ name string }

func (n noopEngine) Plan(_ context.Context, _ Query) PlanResult {
	return PlanResult{Engine: n.name, Err: fmt.Errorf("plandiff: nil %s engine", n.name)}
}

// HashCorpus returns a stable hex hash over (Query.Name, Go.Tree)
// pairs, in name-sorted order. Useful for "did anything in the corpus
// change?" CI gates: a single hash diff means a real plan-tree
// regression somewhere.
func HashCorpus(r Report) string {
	cases := append([]Diff(nil), r.Cases...)
	sort.SliceStable(cases, func(i, j int) bool { return cases[i].Query.Name < cases[j].Query.Name })
	h := sha256.New()
	for _, c := range cases {
		fmt.Fprintf(h, "%s\n%s\n---\n", c.Query.Name, normaliseTree(c.Go.Tree))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// _ static assertion that the embedded-backed Generator satisfies
// query.Generator. Catches a future Generator-method-rename at
// compile time. The query import is structurally needed by
// embedded.NewExplainOnlyGenerator's return type, so this is
// belt-and-suspenders, not the primary anchor.
var _ query.Generator = embedded.NewExplainOnlyGenerator()
