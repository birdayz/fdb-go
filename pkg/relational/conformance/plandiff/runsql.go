package plandiff

// Phase B / Track A1 of TODO.md: drive a SQL statement through both engines'
// EXECUTION paths (not just planning) and capture the result set.
//
// The plan-tree diff (Engine + PlanResult above) tells us "did the planners
// agree on the plan shape". The result-set diff (Runner + RunResult below)
// tells us "do the engines compute the same answer". They're parallel
// abstractions; a corpus query can be run through one or both.
//
// First-cut scope (this commit, swingshift-52): Java-side `runSql` step lands
// in conformance/sql_plan_steps.java and the Go side gets the HTTP plumbing
// to call it. The Go runner is intentionally NOT shipped here — the embedded
// engine's read path is exercised by other tests, and a dedicated diff
// runner that brings up an in-process FDBRecordStore behind a SchemaTemplate
// is its own multi-shift workstream (Track C2 — QueryExecutor). Until that
// lands, NewGoRunner() returns ErrGoUnimplemented for symmetry with
// NewJavaEngine()'s unwired form.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
func NewJavaRunnerHTTP(baseURL, clusterFileContent string) Runner {
	return javaRunner{
		baseURL:     baseURL,
		clusterFile: clusterFileContent,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
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

func (r javaRunner) invokeRunSql(ctx context.Context, q Query) (RowSet, error) {
	type request struct {
		Step   string         `json:"step"`
		Params map[string]any `json:"params"`
	}
	type response struct {
		Success            bool            `json:"success"`
		Result             json.RawMessage `json:"result"`
		Error              string          `json:"error"`
		ExceptionClass     string          `json:"exceptionClass"`
		ExceptionFullClass string          `json:"exceptionFullClass"`
	}

	reqBody, err := json.Marshal(request{
		Step: "runSql",
		Params: map[string]any{
			"clusterFile":    r.clusterFile,
			"schemaTemplate": q.SchemaTemplate,
			"sql":            q.SQL,
		},
	})
	if err != nil {
		return RowSet{}, fmt.Errorf("plandiff: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", r.baseURL+"/invoke", bytes.NewReader(reqBody))
	if err != nil {
		return RowSet{}, fmt.Errorf("plandiff: build HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := r.httpClient.Do(httpReq)
	if err != nil {
		return RowSet{}, fmt.Errorf("plandiff: HTTP POST: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return RowSet{}, fmt.Errorf("plandiff: read body: %w", err)
	}
	if httpResp.StatusCode != 200 {
		return RowSet{}, fmt.Errorf("plandiff: HTTP %d: %s", httpResp.StatusCode, string(body))
	}

	var resp response
	if err := json.Unmarshal(body, &resp); err != nil {
		return RowSet{}, fmt.Errorf("plandiff: unmarshal response: %w (body=%q)", err, string(body))
	}
	if !resp.Success {
		if resp.ExceptionClass != "" {
			return RowSet{}, fmt.Errorf("plandiff: java %s: %s", resp.ExceptionClass, resp.Error)
		}
		return RowSet{}, fmt.Errorf("plandiff: java error: %s", resp.Error)
	}

	var rows RowSet
	if err := json.Unmarshal(resp.Result, &rows); err != nil {
		return RowSet{}, fmt.Errorf("plandiff: parse rows from result: %w (result=%q)", err, string(resp.Result))
	}
	return rows, nil
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
