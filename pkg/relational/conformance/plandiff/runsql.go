package plandiff

// Phase B / Track A1 of TODO.md: drive a SQL statement through both engines'
// EXECUTION paths (not just planning) and capture the result set.
//
// The plan-tree diff (Engine + PlanResult in plandiff.go) tells us "did the
// planners agree on the plan shape". The result-set diff (Runner + RunResult
// here) tells us "do the engines compute the same answer". They're parallel
// abstractions; a corpus query can be run through one or both.
//
// Java side: SqlPlanSteps#runSql / runWithSetup in conformance/sql_plan_steps.java.
// Go side: NewJavaRunnerHTTP for the wired form, NewGoRunner stubbed until
// Track C2 (QueryExecutor) lands an in-process executor — mirrors the
// NewJavaEngine() unwired form's contract.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Column is one column's metadata in a RowSet: its name and JDBC type-name
// (e.g. "BIGINT", "VARCHAR"). Type names come from the source engine's
// ResultSetMetaData; comparison across engines is at the diff harness's
// discretion (a cross-engine type-name mapping table will land with the
// result-set diff harness — A3).
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// RowSet is the result of running a SQL statement: column metadata plus
// row values. Each row has len(Columns) entries; SQL NULLs come through
// as Go nil. byte-array columns arrive base64-encoded as strings (the
// Java side encodes them; consumers wanting raw bytes decode at the
// comparison layer).
type RowSet struct {
	Columns []Column `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// RunResult is one engine's output for one Query. Either Err is non-nil
// (engine failed) or Rows is populated. Engine identifies the producer
// ("go" / "java"). Mirrors PlanResult's shape so callers can write
// uniform per-engine handlers.
type RunResult struct {
	Engine string
	Rows   RowSet
	Err    error
}

// Runner produces a RunResult for a Query. Implementations must be safe
// for concurrent use across queries, the same as Engine.
type Runner interface {
	Run(ctx context.Context, q Query) RunResult
}

// SetupRunner extends Runner with a setup-then-query mode that keeps a
// single ephemeral schema alive across multiple SQL statements. Used by
// round-trip type-coverage tests where a SELECT depends on prior INSERTs.
//
// `setupSqls` are run via JDBC executeUpdate (DML); `querySql` is run
// via executeQuery and its RowSet is returned. All statements run on
// the same JDBC connection, so the ephemeral schema persists for the
// whole sequence.
type SetupRunner interface {
	Runner
	RunWithSetup(ctx context.Context, schemaTemplate string, setupSqls []string, querySql string) RunResult
}

// ErrJavaRunUnimplemented is returned by NewJavaRunner() (the no-baseURL
// form) so a Go-only CI gate can run plandiff infrastructure without a
// Java conformance server.
var ErrJavaRunUnimplemented = errors.New("plandiff: Java runner not wired (use NewJavaRunnerHTTP)")

// ErrGoUnimplemented is returned by NewGoRunner() until the in-process
// Go executor lands (Track C2 — QueryExecutor). Symmetric with
// ErrJavaRunUnimplemented so the diff harness can report which side is
// stubbed.
var ErrGoUnimplemented = errors.New("plandiff: Go runner not yet implemented (waits on Track C2 QueryExecutor)")

// javaRunner POSTs to the conformance server's `runSql` step (see
// conformance/sql_plan_steps.java#runSql). Mirrors javaEngine's wire shape
// but parses the result as a RowSet rather than a plan-tree string.
type javaRunner struct {
	baseURL     string
	httpClient  *http.Client
	clusterFile string
}

// NewJavaRunner returns the unwired form. Every Run() call surfaces
// ErrJavaRunUnimplemented. Useful for local-only test runs without a
// Java conformance server.
func NewJavaRunner() Runner { return javaRunner{} }

// NewJavaRunnerHTTP returns a Runner that drives the Java conformance
// server at baseURL against the FDB cluster whose cluster-file content
// is clusterFileContent (matches NewJavaEngineHTTP's parameter contract).
//
// 120s response timeout (not 30s) — race-instrumented harness +
// memory-pressured JVM under CI pushes per-call latency past 30s on
// the first few cold invocations.
func NewJavaRunnerHTTP(baseURL, clusterFileContent string) Runner {
	return javaRunner{
		baseURL:     baseURL,
		clusterFile: clusterFileContent,
		httpClient:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (r javaRunner) Run(ctx context.Context, q Query) RunResult {
	if r.baseURL == "" {
		return RunResult{Engine: "java", Err: ErrJavaRunUnimplemented}
	}
	rows, err := r.invokeRunSql(ctx, q)
	if err != nil {
		return RunResult{Engine: "java", Err: err}
	}
	return RunResult{Engine: "java", Rows: rows}
}

// RunWithSetup posts to the conformance server's `runWithSetup` step
// (see conformance/sql_plan_steps.java#runWithSetup). All `setupSqls`
// run via executeUpdate; `querySql` runs via executeQuery and its
// RowSet is returned.
func (r javaRunner) RunWithSetup(ctx context.Context, schemaTemplate string, setupSqls []string, querySql string) RunResult {
	if r.baseURL == "" {
		return RunResult{Engine: "java", Err: ErrJavaRunUnimplemented}
	}
	rows, err := r.invokeRunWithSetup(ctx, schemaTemplate, setupSqls, querySql)
	if err != nil {
		return RunResult{Engine: "java", Err: err}
	}
	return RunResult{Engine: "java", Rows: rows}
}

func (r javaRunner) invokeRunSql(ctx context.Context, q Query) (RowSet, error) {
	var rows RowSet
	err := invokeStep(ctx, r.httpClient, r.baseURL, "runSql", map[string]any{
		"clusterFile":    r.clusterFile,
		"schemaTemplate": q.SchemaTemplate,
		"sql":            q.SQL,
	}, &rows)
	return rows, err
}

func (r javaRunner) invokeRunWithSetup(ctx context.Context, schemaTemplate string, setupSqls []string, querySql string) (RowSet, error) {
	if setupSqls == nil {
		// Java's deserializeArgs treats missing param as null; the
		// List<String> param would arrive as null and NPE inside the
		// for-each. Send an empty array instead.
		setupSqls = []string{}
	}
	var rows RowSet
	err := invokeStep(ctx, r.httpClient, r.baseURL, "runWithSetup", map[string]any{
		"clusterFile":    r.clusterFile,
		"schemaTemplate": schemaTemplate,
		"setupSqls":      setupSqls,
		"querySql":       querySql,
	}, &rows)
	return rows, err
}

// RunDiff is the per-Query verdict for the runSql harness — parallel
// to the plan-tree Diff. Either Go or Java may have errored; the
// Status classifies the pair.
type RunDiff struct {
	Query  Query
	Go     RunResult
	Java   RunResult
	Status Status
	// Detail carries the row-by-row delta on Diverge, or the engine
	// error text on *Error statuses. Empty for AGREE.
	Detail string
}

// RunReport aggregates RunDiffs from a corpus run.
type RunReport struct {
	Cases   []RunDiff
	Summary Summary
}

// RunCorpus drives every query through both runners and produces a
// RunReport. Today's primary use: pin Java-side outputs via the
// JavaRunner against a corpus, regression-detecting fdb-relational
// changes. When Track C2 lands the Go-side runner, this function
// upgrades to cross-engine result-set comparison without any caller
// changes.
//
// `goR` and `javaR` may be nil — the noop fallback returns
// "nil runner" errors so the report stays deterministic.
func RunCorpus(ctx context.Context, queries []Query, goR, javaR Runner) RunReport {
	if goR == nil {
		goR = noopRunner{name: "go"}
	}
	if javaR == nil {
		javaR = noopRunner{name: "java"}
	}
	out := RunReport{Cases: make([]RunDiff, 0, len(queries))}
	for _, q := range queries {
		gr := goR.Run(ctx, q)
		jr := javaR.Run(ctx, q)
		out.Cases = append(out.Cases, classifyRun(q, gr, jr))
	}
	out.Summary = summariseRun(out.Cases)
	return out
}

// classifyRun pairs two RunResults into a RunDiff with computed
// Status + Detail. Mirrors the plan-tree classify shape.
func classifyRun(q Query, gr, jr RunResult) RunDiff {
	d := RunDiff{Query: q, Go: gr, Java: jr}
	switch {
	case gr.Err != nil && jr.Err != nil:
		// Both errored. Each side has its own stub sentinel
		// (ErrGoUnimplemented / ErrJavaRunUnimplemented) — track
		// them with distinct Status values so reports are clear
		// about which side is stubbed.
		goStub := errors.Is(gr.Err, ErrGoUnimplemented)
		javaStub := errors.Is(jr.Err, ErrJavaRunUnimplemented)
		switch {
		case goStub && javaStub:
			// Both stubbed — pick GoUnimplemented (the more pressing
			// side today; flip when Java becomes the lagging side).
			d.Status = StatusGoUnimplemented
		case goStub:
			d.Status = StatusGoUnimplemented
		case javaStub:
			d.Status = StatusJavaUnimplemented
		default:
			d.Status = StatusBothError
		}
		d.Detail = fmt.Sprintf("go: %v\njava: %v", gr.Err, jr.Err)
	case gr.Err != nil:
		if errors.Is(gr.Err, ErrGoUnimplemented) {
			// Go-side stub today. Don't fail the whole report — the
			// Java baseline is what we want to capture.
			d.Status = StatusGoUnimplemented
			d.Detail = "go: " + gr.Err.Error()
		} else {
			d.Status = StatusGoError
			d.Detail = fmt.Sprintf("go: %v", gr.Err)
		}
	case jr.Err != nil:
		if errors.Is(jr.Err, ErrJavaRunUnimplemented) {
			d.Status = StatusJavaUnimplemented
			d.Detail = jr.Err.Error()
		} else {
			d.Status = StatusJavaError
			d.Detail = fmt.Sprintf("java: %v", jr.Err)
		}
	default:
		// Both succeeded — compare normalised RowSets.
		ng := normaliseRows(gr.Rows)
		nj := normaliseRows(jr.Rows)
		if ng == nj {
			d.Status = StatusAgree
		} else {
			d.Status = StatusDiverge
			d.Detail = fmt.Sprintf("--- go\n%s\n+++ java\n%s", ng, nj)
		}
	}
	return d
}

// summariseRun tallies Status counts across the RunDiff slice.
func summariseRun(cases []RunDiff) Summary {
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

// normaliseRows produces a stable string representation of a RowSet
// suitable for byte-equality comparison + hashing. Format: column
// metadata as "name|type" pairs, one per line, then a separator,
// then row values one per line (values JSON-encoded for unambiguous
// type preservation across Go's any).
func normaliseRows(r RowSet) string {
	var b strings.Builder
	for _, c := range r.Columns {
		b.WriteString(c.Name)
		b.WriteString("|")
		b.WriteString(c.Type)
		b.WriteString("\n")
	}
	b.WriteString("---\n")
	for _, row := range r.Rows {
		bs, _ := json.Marshal(row)
		b.Write(bs)
		b.WriteString("\n")
	}
	return b.String()
}

// RunCorpusWithSetup is RunCorpus's sibling for SeedRunCorpus-style
// queries that need INSERT before SELECT. Calls Java's runWithSetup
// step. The Go side returns ErrGoUnimplemented today (Track C2).
//
// Returns a RunReport in the same shape as RunCorpus so reporting
// utilities can be shared.
func RunCorpusWithSetup(ctx context.Context, queries []RunQuery, goR, javaR SetupRunner) RunReport {
	if goR == nil {
		goR = noopSetupRunner{name: "go"}
	}
	if javaR == nil {
		javaR = noopSetupRunner{name: "java"}
	}
	out := RunReport{Cases: make([]RunDiff, 0, len(queries))}
	for _, rq := range queries {
		// Use Query as the keyed-name carrier in the RunDiff so
		// reports stay homogeneous with RunCorpus output.
		q := Query{Name: rq.Name, SQL: rq.Query, SchemaTemplate: rq.SchemaTemplate}
		gr := goR.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
		jr := javaR.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
		out.Cases = append(out.Cases, classifyRun(q, gr, jr))
	}
	out.Summary = summariseRun(out.Cases)
	return out
}

// noopSetupRunner is the fallback when RunCorpusWithSetup is called
// with a nil SetupRunner.
type noopSetupRunner struct{ name string }

func (n noopSetupRunner) Run(_ context.Context, _ Query) RunResult {
	return RunResult{Engine: n.name, Err: fmt.Errorf("plandiff: nil %s runner", n.name)}
}

func (n noopSetupRunner) RunWithSetup(_ context.Context, _ string, _ []string, _ string) RunResult {
	return RunResult{Engine: n.name, Err: fmt.Errorf("plandiff: nil %s runner", n.name)}
}

// noopRunner is the fallback when RunCorpus is called with a nil
// Runner. Mirrors plandiff's noopEngine for the plan-tree harness.
type noopRunner struct{ name string }

func (n noopRunner) Run(_ context.Context, _ Query) RunResult {
	return RunResult{Engine: n.name, Err: fmt.Errorf("plandiff: nil %s runner", n.name)}
}

// goRunner is the unwired Go runner. Returns ErrGoUnimplemented for
// every call. Track C2 (QueryExecutor) replaces this with a real
// in-process executor.
type goRunner struct{}

// NewGoRunner returns the unwired Go runner. Stubbed until Track C2
// lands. Counts as a real engine error (not "unimplemented") in any
// future RunReport — there's no soft-failure tier for the Go side.
func NewGoRunner() Runner { return goRunner{} }

func (goRunner) Run(_ context.Context, _ Query) RunResult {
	return RunResult{Engine: "go", Err: ErrGoUnimplemented}
}

func (goRunner) RunWithSetup(_ context.Context, _ string, _ []string, _ string) RunResult {
	return RunResult{Engine: "go", Err: ErrGoUnimplemented}
}

// Compile-time assertion that javaRunner / goRunner satisfy SetupRunner.
// Catches a future RunWithSetup signature drift at build time.
var (
	_ SetupRunner = javaRunner{}
	_ SetupRunner = goRunner{}
)
