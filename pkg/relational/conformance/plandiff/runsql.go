package plandiff

// runsql.go is the Phase B SqlSteps wiring (TODO.md Track A1). The
// existing javaEngine.Plan path drives the Java planner via the
// conformance server's `planSql` step and diffs plan trees. To verify
// cross-language SEMANTIC equivalence (not just plan-shape equivalence),
// we also need to drive the Java planner THROUGH execution and capture
// the result rows. That's what `runSql` adds: a sibling step that runs
// the SQL and returns rows as JSON.
//
// First-cut scope: Java executor only. The Go-side executor that would
// run the same SQL through fdb-record-layer-go's embedded engine + diff
// result sets lands in a follow-up — wiring the network round-trip
// first proves the harness shape end-to-end before we invest in the
// row-equivalence diffing machinery.

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

// RowSet is the structural payload from the Java `runSql` step for a
// SELECT result. Mirrors the Java side's Map<String, Object> shape —
// see `executeAndCapture` / `captureRows` in `conformance/sql_plan_steps.java`.
//
// Value typing in Rows[i][j] (after json.Unmarshal into []any):
//   - SQL NULL → nil
//   - BIGINT / INTEGER / SMALLINT / TINYINT → float64 (json.Unmarshal default)
//   - DOUBLE / FLOAT / REAL → float64
//   - BOOLEAN → bool
//   - VARCHAR / CHAR / NVARCHAR → string
//   - VARBINARY / BINARY → string with leading "0x" + lowercase hex
//   - other types → string (Java toString fallback)
//
// Callers comparing rows from two engines should normalise on this side
// (e.g. cast all integers to int64) before comparing — JSON loses the
// int-vs-float distinction. RowSet does NOT do that normalisation
// itself; that's the harness's job because the desired tolerance
// depends on context.
type RowSet struct {
	Columns     []string `json:"columns"`
	ColumnTypes []string `json:"columnTypes"`
	Rows        [][]any  `json:"rows"`
}

// RunResult is the Java executor's per-call output. Either Rows is
// non-nil (SELECT path), UpdateCount >= 0 (DML path), or Err is non-nil.
type RunResult struct {
	// Engine identifies the producer ("java" today; future: "go").
	Engine string
	// Rows holds the captured result set on the SELECT path. Nil for DML.
	Rows *RowSet
	// UpdateCount is the affected-row count on the DML path. -1 means
	// "not a DML response" (i.e. Rows should be consulted instead).
	UpdateCount int
	// Err carries any executor error. Non-nil suppresses Rows / UpdateCount.
	Err error
}

// Executor runs SQL against an engine and returns the result rows.
// Parallel to the Engine interface (which produces plan trees) but
// kept separate so Plan-only and Run-only impls don't have to stub
// each other's methods. A future GoExecutor will sit alongside
// JavaExecutor here.
type Executor interface {
	Run(ctx context.Context, q Query) RunResult
}

// ErrJavaRunSqlUnimplemented is returned by a nil-baseURL JavaExecutor.
// Symmetric with ErrJavaUnimplemented for the Plan path.
var ErrJavaRunSqlUnimplemented = errors.New("plandiff: Java runSql executor not wired (call NewJavaExecutorHTTP with a server URL)")

// javaExecutor talks to the conformance server's `runSql` step. A nil
// baseURL surfaces ErrJavaRunSqlUnimplemented for every call (mirrors
// javaEngine's nil-baseURL behavior).
type javaExecutor struct {
	baseURL     string
	clusterFile string
	httpClient  *http.Client
}

// NewJavaExecutor returns the unwired Java executor. Use when the
// conformance server / FDB testcontainer aren't available.
func NewJavaExecutor() Executor { return javaExecutor{} }

// NewJavaExecutorHTTP returns a Java executor that drives the
// conformance server at `baseURL` against the FDB cluster whose
// cluster-file content is `clusterFileContent` (config text, not path
// — the Java step writes it to a file before opening the database).
//
// Each Run() call POSTs to `baseURL/invoke` with step="runSql" and
// params={clusterFile, schemaTemplate, sql}. The Java step creates a
// uniquely-named schema/database for the call, executes the SQL,
// returns either {columns, columnTypes, rows} (SELECT) or
// {updateCount} (INSERT/UPDATE/DELETE), and tears down the schema —
// see `conformance/sql_plan_steps.java#runSql`.
func NewJavaExecutorHTTP(baseURL, clusterFileContent string) Executor {
	return javaExecutor{
		baseURL:     baseURL,
		clusterFile: clusterFileContent,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (e javaExecutor) Run(ctx context.Context, q Query) RunResult {
	if e.baseURL == "" {
		return RunResult{Engine: "java", Err: ErrJavaRunSqlUnimplemented, UpdateCount: -1}
	}
	rs, n, err := e.invokeRunSql(ctx, q)
	if err != nil {
		return RunResult{Engine: "java", Err: err, UpdateCount: -1}
	}
	return RunResult{Engine: "java", Rows: rs, UpdateCount: n}
}

// invokeRunSql is the per-call HTTP dance. Returns:
//   - (*RowSet, -1, nil) for SELECT-shape responses
//   - (nil,    n, nil) for DML responses (n = updateCount)
//   - (nil,   -1, err) on transport / Java error
//
// Body shape mirrors the existing javaEngine.invokePlanSql for the
// outer envelope; the inner result is the executeAndCapture Map.
func (e javaExecutor) invokeRunSql(ctx context.Context, q Query) (*RowSet, int, error) {
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
			"clusterFile":    e.clusterFile,
			"schemaTemplate": q.SchemaTemplate,
			"sql":            q.SQL,
		},
	})
	if err != nil {
		return nil, -1, fmt.Errorf("plandiff/runsql: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", e.baseURL+"/invoke", bytes.NewReader(reqBody))
	if err != nil {
		return nil, -1, fmt.Errorf("plandiff/runsql: build HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := e.httpClient.Do(httpReq)
	if err != nil {
		return nil, -1, fmt.Errorf("plandiff/runsql: HTTP POST: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, -1, fmt.Errorf("plandiff/runsql: read body: %w", err)
	}
	if httpResp.StatusCode != 200 {
		return nil, -1, fmt.Errorf("plandiff/runsql: HTTP %d: %s", httpResp.StatusCode, string(body))
	}

	var resp response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, -1, fmt.Errorf("plandiff/runsql: unmarshal response: %w (body=%q)", err, string(body))
	}
	if !resp.Success {
		if resp.ExceptionClass != "" {
			return nil, -1, fmt.Errorf("plandiff/runsql: java %s: %s", resp.ExceptionClass, resp.Error)
		}
		return nil, -1, fmt.Errorf("plandiff/runsql: java error: %s", resp.Error)
	}

	// The Java step returns either:
	//   {"columns": [...], "columnTypes": [...], "rows": [[...]]}  — SELECT
	//   {"updateCount": <n>}                                         — DML
	//
	// Decode opportunistically: try SELECT shape first; if columns is
	// absent, fall back to the DML shape.
	type dual struct {
		Columns     []string `json:"columns"`
		ColumnTypes []string `json:"columnTypes"`
		Rows        [][]any  `json:"rows"`
		UpdateCount *int     `json:"updateCount"`
	}
	var d dual
	if err := json.Unmarshal(resp.Result, &d); err != nil {
		return nil, -1, fmt.Errorf("plandiff/runsql: parse runSql result: %w (result=%q)", err, string(resp.Result))
	}
	if d.Columns != nil {
		return &RowSet{
			Columns:     d.Columns,
			ColumnTypes: d.ColumnTypes,
			Rows:        d.Rows,
		}, -1, nil
	}
	if d.UpdateCount != nil {
		return nil, *d.UpdateCount, nil
	}
	return nil, -1, fmt.Errorf("plandiff/runsql: result has neither columns nor updateCount: %s", string(resp.Result))
}
