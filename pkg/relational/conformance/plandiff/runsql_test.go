package plandiff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestJavaRunner_HappyPath pins the HTTP wire shape for runSql: the runner
// POSTs to /invoke with step="runSql" + params{clusterFile, schemaTemplate,
// sql}, parses {success: true, result: {columns, rows}}, and returns a
// RunResult with Rows populated. Mirrors TestJavaEngine_HappyPath.
func TestJavaRunner_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/invoke" {
			http.Error(w, "wrong path", http.StatusBadRequest)
			return
		}
		var req struct {
			Step   string         `json:"step"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Step != "runSql" {
			http.Error(w, "unexpected step "+req.Step, http.StatusBadRequest)
			return
		}
		for _, k := range []string{"clusterFile", "schemaTemplate", "sql"} {
			if _, ok := req.Params[k]; !ok {
				http.Error(w, "missing param "+k, http.StatusBadRequest)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Result mirrors what conformance/sql_plan_steps.java#runSql
		// produces via gson serialization of the JsonObject.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"columns": []map[string]any{
					{"name": "ID", "type": "BIGINT"},
					{"name": "NAME", "type": "VARCHAR"},
				},
				"rows": [][]any{
					{1, "alice"},
					{2, nil},
				},
			},
		})
	}))
	defer srv.Close()

	runner := NewJavaRunnerHTTP(srv.URL, "fake-cluster-file-content")
	got := runner.Run(context.Background(), Query{
		Name:           "x",
		SQL:            "SELECT id, name FROM Item",
		SchemaTemplate: "CREATE TABLE Item (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))",
	})
	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if len(got.Rows.Columns) != 2 {
		t.Fatalf("Columns: got %d, want 2 (%+v)", len(got.Rows.Columns), got.Rows.Columns)
	}
	if got.Rows.Columns[0].Name != "ID" || got.Rows.Columns[0].Type != "BIGINT" {
		t.Fatalf("Columns[0]: got %+v, want {ID BIGINT}", got.Rows.Columns[0])
	}
	if got.Rows.Columns[1].Name != "NAME" || got.Rows.Columns[1].Type != "VARCHAR" {
		t.Fatalf("Columns[1]: got %+v, want {NAME VARCHAR}", got.Rows.Columns[1])
	}
	if len(got.Rows.Rows) != 2 {
		t.Fatalf("Rows: got %d, want 2", len(got.Rows.Rows))
	}
	// Numbers come back as float64 through encoding/json; that's the
	// universal JSON-numeric semantic — callers that want exact integers
	// downcast at compare time. Pin float64 here so the test reflects
	// reality and a future "switch to json.Number" change is a deliberate
	// API break, not silent.
	if got.Rows.Rows[0][0].(float64) != 1 {
		t.Fatalf("Rows[0][0]: got %v, want 1", got.Rows.Rows[0][0])
	}
	if got.Rows.Rows[0][1].(string) != "alice" {
		t.Fatalf("Rows[0][1]: got %v, want alice", got.Rows.Rows[0][1])
	}
	if got.Rows.Rows[1][1] != nil {
		t.Fatalf("Rows[1][1]: got %v, want nil", got.Rows.Rows[1][1])
	}
}

// TestJavaRunner_JavaError pins the failure shape: Java step returns
// {success: false, exceptionClass, error}, runner surfaces the exception
// class in the error message. Mirrors TestJavaEngine_JavaError.
func TestJavaRunner_JavaError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":            false,
			"error":              "Table not found: nonexistent",
			"exceptionClass":     "RelationalException",
			"exceptionFullClass": "com.apple.foundationdb.relational.api.exceptions.RelationalException",
		})
	}))
	defer srv.Close()

	runner := NewJavaRunnerHTTP(srv.URL, "")
	got := runner.Run(context.Background(), Query{Name: "x", SQL: "SELECT * FROM nonexistent"})
	if got.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(got.Err.Error(), "RelationalException") {
		t.Fatalf("error missing exception class: %v", got.Err)
	}
	if !strings.Contains(got.Err.Error(), "Table not found") {
		t.Fatalf("error missing original message: %v", got.Err)
	}
}

// TestJavaRunner_HTTPNon200 pins that a non-200 server response surfaces
// as a plandiff: HTTP <code> error. Mirrors TestJavaEngine_HTTPNon200.
func TestJavaRunner_HTTPNon200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "step not found", http.StatusNotFound)
	}))
	defer srv.Close()

	runner := NewJavaRunnerHTTP(srv.URL, "")
	got := runner.Run(context.Background(), Query{Name: "x", SQL: "SELECT 1"})
	if got.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(got.Err.Error(), "HTTP 404") {
		t.Fatalf("expected HTTP 404 in error: %v", got.Err)
	}
}

// TestJavaRunner_NilBaseURL pins the Go-only-CI fallback: NewJavaRunner()
// surfaces ErrJavaRunUnimplemented for every call.
func TestJavaRunner_NilBaseURL(t *testing.T) {
	t.Parallel()
	runner := NewJavaRunner()
	got := runner.Run(context.Background(), Query{Name: "x", SQL: "SELECT 1"})
	if !errors.Is(got.Err, ErrJavaRunUnimplemented) {
		t.Fatalf("expected ErrJavaRunUnimplemented, got %v", got.Err)
	}
}

// TestJavaRunner_UnsupportedTypeMarker pins the contract for values
// that SqlPlanSteps#encodeValue can't handle natively. Java sends
// `{"__unsupported__": "<class>"}` as a JSON object; the Go side
// parses it into the row's any slot as a map[string]any. Consumers
// that type-assert to a primitive will fail loudly (good — surfaces
// the gap); consumers that handle the marker gracefully can ignore
// the row or treat it as an unsupported-type sentinel.
//
// This test pins the shape, not consumer behaviour — the harness
// itself must not crash on the marker.
func TestJavaRunner_UnsupportedTypeMarker(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"columns": []map[string]any{
					{"name": "ID", "type": "BIGINT"},
					{"name": "MYSTERY", "type": "OTHER"},
				},
				"rows": [][]any{
					{1, map[string]string{"__unsupported__": "java.sql.Array"}},
				},
			},
		})
	}))
	defer srv.Close()

	runner := NewJavaRunnerHTTP(srv.URL, "")
	got := runner.Run(context.Background(), Query{Name: "x", SQL: "SELECT id, mystery FROM T"})
	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if len(got.Rows.Rows) != 1 || len(got.Rows.Rows[0]) != 2 {
		t.Fatalf("Rows shape: got %+v", got.Rows.Rows)
	}
	// First column passes through as float64 (JSON number); second
	// column arrives as map[string]any with the marker key.
	marker, ok := got.Rows.Rows[0][1].(map[string]any)
	if !ok {
		t.Fatalf("Rows[0][1]: expected map[string]any (unsupported marker), got %T: %v", got.Rows.Rows[0][1], got.Rows.Rows[0][1])
	}
	if marker["__unsupported__"] != "java.sql.Array" {
		t.Fatalf("Rows[0][1] marker: got %v", marker)
	}
}

// TestJavaRunner_EmptyResultSet pins zero-row handling: a SELECT that
// returns no rows produces RowSet with non-nil Columns and an empty
// (or nil) Rows slice. Caller must not crash on len(Rows)==0.
func TestJavaRunner_EmptyResultSet(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"columns": []map[string]any{
					{"name": "ID", "type": "BIGINT"},
				},
				"rows": []any{},
			},
		})
	}))
	defer srv.Close()

	runner := NewJavaRunnerHTTP(srv.URL, "")
	got := runner.Run(context.Background(), Query{Name: "x", SQL: "SELECT id FROM Item WHERE id = -1"})
	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if len(got.Rows.Columns) != 1 {
		t.Fatalf("Columns: got %d, want 1", len(got.Rows.Columns))
	}
	if len(got.Rows.Rows) != 0 {
		t.Fatalf("Rows: got %d, want 0", len(got.Rows.Rows))
	}
}

// TestGoRunner_Unimplemented pins that NewGoRunner() returns
// ErrGoUnimplemented until Track C2 lands the real executor.
func TestGoRunner_Unimplemented(t *testing.T) {
	t.Parallel()
	r := NewGoRunner()
	got := r.Run(context.Background(), Query{Name: "x", SQL: "SELECT 1"})
	if !errors.Is(got.Err, ErrGoUnimplemented) {
		t.Fatalf("expected ErrGoUnimplemented, got %v", got.Err)
	}
}

// TestRunCorpus_BothStubbed pins that RunCorpus with both Go and
// Java stubbed runners classifies every case as GO_UNIMPL — Go is
// the more pressing side today (Track C2's QueryExecutor blocks
// real Go execution), so when both sides are stubbed the report
// surfaces it as Go's responsibility.
func TestRunCorpus_BothStubbed(t *testing.T) {
	t.Parallel()
	queries := []Query{
		{Name: "x", SQL: "SELECT 1"},
		{Name: "y", SQL: "SELECT 2"},
	}
	report := RunCorpus(context.Background(), queries, NewGoRunner(), NewJavaRunner())
	if report.Summary.Total != 2 {
		t.Fatalf("Total: got %d, want 2", report.Summary.Total)
	}
	if report.Summary.GoUnimplemented != 2 {
		t.Fatalf("expected 2 GO_UNIMPL (both sides stubbed → Go's responsibility), got %+v", report.Summary)
	}
}

// TestRunCorpus_NilRunner pins the noop fallback — both nil runners
// produce per-case "nil X runner" errors. Status: BothError (no
// Unimplemented sentinel).
func TestRunCorpus_NilRunner(t *testing.T) {
	t.Parallel()
	report := RunCorpus(context.Background(), []Query{{Name: "x", SQL: "SELECT 1"}}, nil, nil)
	if report.Summary.BothError != 1 {
		t.Fatalf("expected 1 BOTH_ERROR, got %+v", report.Summary)
	}
}

// TestClassifyRun_Agree pins the happy-path branch: identical RowSets
// produce StatusAgree.
func TestClassifyRun_Agree(t *testing.T) {
	t.Parallel()
	rs := RowSet{
		Columns: []Column{{Name: "ID", Type: "BIGINT"}},
		Rows:    [][]any{{float64(1)}, {float64(2)}},
	}
	q := Query{Name: "x"}
	d := classifyRun(q, RunResult{Engine: "go", Rows: rs}, RunResult{Engine: "java", Rows: rs})
	if d.Status != StatusAgree {
		t.Fatalf("Status: got %s, want AGREE", d.Status)
	}
}

// TestClassifyRun_Diverge pins that differing RowSets produce
// StatusDiverge with both sides surfaced in Detail.
func TestClassifyRun_Diverge(t *testing.T) {
	t.Parallel()
	goRS := RowSet{Columns: []Column{{Name: "ID", Type: "BIGINT"}}, Rows: [][]any{{float64(1)}}}
	javaRS := RowSet{Columns: []Column{{Name: "ID", Type: "BIGINT"}}, Rows: [][]any{{float64(2)}}}
	d := classifyRun(Query{Name: "x"}, RunResult{Engine: "go", Rows: goRS}, RunResult{Engine: "java", Rows: javaRS})
	if d.Status != StatusDiverge {
		t.Fatalf("Status: got %s, want DIVERGE", d.Status)
	}
	if !strings.Contains(d.Detail, "--- go") || !strings.Contains(d.Detail, "+++ java") {
		t.Fatalf("Detail missing diff markers: %q", d.Detail)
	}
}

// TestJavaRunner_RunWithSetup_HappyPath pins the wire shape for the
// runWithSetup step: POSTs step="runWithSetup" + params{clusterFile,
// schemaTemplate, setupSqls, querySql}, parses the same RowSet result
// as runSql.
func TestJavaRunner_RunWithSetup_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Step   string         `json:"step"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Step != "runWithSetup" {
			http.Error(w, "unexpected step "+req.Step, http.StatusBadRequest)
			return
		}
		// Verify params include the setup-list shape.
		setups, ok := req.Params["setupSqls"].([]any)
		if !ok || len(setups) != 2 {
			http.Error(w, fmt.Sprintf("setupSqls: got %v", req.Params["setupSqls"]), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"columns": []map[string]any{{"name": "ID", "type": "BIGINT"}},
				"rows":    [][]any{{1}, {2}},
			},
		})
	}))
	defer srv.Close()

	r, ok := NewJavaRunnerHTTP(srv.URL, "fake-cluster").(SetupRunner)
	if !ok {
		t.Fatal("javaRunner does not satisfy SetupRunner")
	}
	got := r.RunWithSetup(
		context.Background(),
		"CREATE TABLE T (id BIGINT, PRIMARY KEY (id))",
		[]string{"INSERT INTO T VALUES (1)", "INSERT INTO T VALUES (2)"},
		"SELECT id FROM T ORDER BY id",
	)
	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if len(got.Rows.Rows) != 2 {
		t.Fatalf("Rows: got %d, want 2", len(got.Rows.Rows))
	}
}

// TestJavaRunner_RunWithSetup_NilBaseURL pins that the unwired runner
// surfaces ErrJavaRunUnimplemented for RunWithSetup as well as Run.
func TestJavaRunner_RunWithSetup_NilBaseURL(t *testing.T) {
	t.Parallel()
	r, ok := NewJavaRunner().(SetupRunner)
	if !ok {
		t.Fatal("javaRunner does not satisfy SetupRunner")
	}
	got := r.RunWithSetup(context.Background(), "", nil, "SELECT 1")
	if !errors.Is(got.Err, ErrJavaRunUnimplemented) {
		t.Fatalf("expected ErrJavaRunUnimplemented, got %v", got.Err)
	}
}

// TestJavaRunner_RunWithSetup_EmptySetup pins that a nil setup list
// is sent as an empty JSON array (not null) — the Java side iterates
// the list and would NPE on null.
func TestJavaRunner_RunWithSetup_EmptySetup(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		setups, ok := req.Params["setupSqls"].([]any)
		if !ok {
			http.Error(w, "setupSqls missing or not an array", http.StatusBadRequest)
			return
		}
		if len(setups) != 0 {
			http.Error(w, fmt.Sprintf("setupSqls: want empty array, got %v", setups), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"columns": []map[string]any{{"name": "X", "type": "INTEGER"}},
				"rows":    []any{},
			},
		})
	}))
	defer srv.Close()

	r := NewJavaRunnerHTTP(srv.URL, "").(SetupRunner)
	got := r.RunWithSetup(context.Background(), "", nil, "SELECT 1")
	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
}
