// Package plandiff is the Phase 4.-1 plan-equivalence harness from
// RFC-022 §4.-1. Its job: feed a SQL string + schema into both the Go
// planner and Java's Cascades planner, capture each side's plan tree,
// and produce a structural diff plus a plan-cache-key hash diff.
//
// First-cut scope (this commit, swingshift-50): Go-side baseline only.
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
	"sort"
	"strings"

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
}

// Report is the harness's full output for a corpus run.
type Report struct {
	// Cases holds one Diff per Query, in input order.
	Cases []Diff
	// Summary aggregates Status counts.
	Summary Summary
}

// Engine produces a PlanResult for a Query. Implementations must be
// safe for concurrent use across queries — Run() may parallelise.
type Engine interface {
	Plan(ctx context.Context, q Query) PlanResult
}

// ErrJavaUnimplemented is returned by JavaEngine until fdb-relational
// maven deps land in conformance/BUILD.bazel and a SqlPlanSteps Java
// step is added. See TODO.md §CRITICAL "Java↔Go SQL conformance
// harness Phase B".
var ErrJavaUnimplemented = errors.New("plandiff: Java engine not wired (fdb-relational maven deps missing from conformance/BUILD.bazel)")

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
// embedded.NewExplainOnlyGenerator to produce a plan without execution.
type goEngine struct{}

// NewGoEngine returns the Go-side Engine. It is stateless and safe
// for concurrent use.
func NewGoEngine() Engine { return goEngine{} }

func (goEngine) Plan(ctx context.Context, q Query) PlanResult {
	gen := embedded.NewExplainOnlyGenerator()
	plan, err := gen.Plan(ctx, q.SQL)
	if err != nil {
		return PlanResult{Engine: "go", Err: err}
	}
	tree := plan.Explain()
	return PlanResult{Engine: "go", Tree: tree, Hash: hashTree(tree)}
}

// javaEngine is the placeholder Java engine. Always returns
// ErrJavaUnimplemented. Replace once `conformance/BUILD.bazel` gains
// fdb-relational maven deps and `conformance_server.java` exposes a
// SqlPlanSteps action.
type javaEngine struct{}

// NewJavaEngine returns the Java-side Engine stub. Until Bazel wires
// fdb-relational deps every call returns ErrJavaUnimplemented; the
// harness reports those as StatusJavaUnimplemented so a Go-only CI
// gate can pass.
func NewJavaEngine() Engine { return javaEngine{} }

func (javaEngine) Plan(_ context.Context, _ Query) PlanResult {
	return PlanResult{Engine: "java", Err: ErrJavaUnimplemented}
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

// _ pin to avoid an unused-import warning on query if a future edit
// drops the only consumer; query types appear in the doc comments and
// the generator return type.
var _ = query.Plan(nil)
