package plandiff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestJavaExecutor_NilBaseURLReturnsUnimplemented pins the symmetric
// nil-baseURL behavior: NewJavaExecutor() / a zero-value javaExecutor
// surfaces ErrJavaRunSqlUnimplemented for every Run call. Mirrors
// javaEngine's nil-baseURL contract.
func TestJavaExecutor_NilBaseURLReturnsUnimplemented(t *testing.T) {
	t.Parallel()
	exe := NewJavaExecutor()
	got := exe.Run(context.Background(), Query{Name: "x", SQL: "SELECT 1"})
	if got.Err == nil {
		t.Fatal("expected ErrJavaRunSqlUnimplemented, got nil")
	}
	if !errors.Is(got.Err, ErrJavaRunSqlUnimplemented) {
		t.Fatalf("got %v, want ErrJavaRunSqlUnimplemented", got.Err)
	}
	if got.Rows != nil {
		t.Errorf("Rows must be nil on error, got %v", got.Rows)
	}
	if got.UpdateCount != -1 {
		t.Errorf("UpdateCount must be -1 on error, got %d", got.UpdateCount)
	}
}

// TestJavaExecutor_HappyPathSelect pins the HTTP wire shape for SELECT:
// the executor POSTs to /invoke with step=runSql + the right params, and
// the response shape {columns, columnTypes, rows} round-trips into a
// non-nil RowSet with UpdateCount = -1.
func TestJavaExecutor_HappyPathSelect(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"columns":     []string{"ID", "NAME"},
				"columnTypes": []string{"BIGINT", "STRING"},
				"rows": [][]any{
					{1, "alice"},
					{2, "bob"},
					{3, nil}, // SQL NULL
				},
			},
		})
	}))
	defer srv.Close()

	exe := NewJavaExecutorHTTP(srv.URL, "fake-cluster-content")
	got := exe.Run(context.Background(), Query{
		Name:           "x",
		SQL:            "SELECT id, name FROM t",
		SchemaTemplate: "CREATE TABLE t (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))",
	})
	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if got.UpdateCount != -1 {
		t.Errorf("UpdateCount: got %d, want -1 (SELECT path)", got.UpdateCount)
	}
	if got.Rows == nil {
		t.Fatal("Rows must be non-nil for SELECT response")
	}
	rs := got.Rows
	if len(rs.Columns) != 2 || rs.Columns[0] != "ID" || rs.Columns[1] != "NAME" {
		t.Errorf("columns: got %v", rs.Columns)
	}
	if len(rs.ColumnTypes) != 2 || rs.ColumnTypes[0] != "BIGINT" || rs.ColumnTypes[1] != "STRING" {
		t.Errorf("columnTypes: got %v", rs.ColumnTypes)
	}
	if len(rs.Rows) != 3 {
		t.Fatalf("rows count: got %d, want 3", len(rs.Rows))
	}
	// JSON unmarshal turns numbers into float64 — verify shape.
	if v, ok := rs.Rows[0][0].(float64); !ok || v != 1 {
		t.Errorf("Rows[0][0]: got %v (%T), want 1.0 (float64)", rs.Rows[0][0], rs.Rows[0][0])
	}
	if v, ok := rs.Rows[0][1].(string); !ok || v != "alice" {
		t.Errorf("Rows[0][1]: got %v (%T), want \"alice\"", rs.Rows[0][1], rs.Rows[0][1])
	}
	if rs.Rows[2][1] != nil {
		t.Errorf("Rows[2][1]: got %v, want nil (SQL NULL)", rs.Rows[2][1])
	}
}

// TestJavaExecutor_HappyPathDML pins the DML-side response shape
// ({updateCount: n}) — UpdateCount must be n and Rows must be nil.
func TestJavaExecutor_HappyPathDML(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"updateCount": 5,
			},
		})
	}))
	defer srv.Close()

	exe := NewJavaExecutorHTTP(srv.URL, "")
	got := exe.Run(context.Background(), Query{Name: "ins", SQL: "INSERT INTO t VALUES (1)"})
	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if got.Rows != nil {
		t.Errorf("Rows must be nil for DML response, got %v", got.Rows)
	}
	if got.UpdateCount != 5 {
		t.Errorf("UpdateCount: got %d, want 5", got.UpdateCount)
	}
}

// TestJavaExecutor_ZeroUpdateCount pins that updateCount=0 round-trips
// as 0 (not -1, which is the "not a DML response" sentinel). A pointer
// in the parser is the only way to disambiguate "absent field" from
// "explicit zero" — verify that.
func TestJavaExecutor_ZeroUpdateCount(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  map[string]any{"updateCount": 0},
		})
	}))
	defer srv.Close()

	exe := NewJavaExecutorHTTP(srv.URL, "")
	got := exe.Run(context.Background(), Query{Name: "noop_dml", SQL: "DELETE FROM t WHERE 1=0"})
	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if got.UpdateCount != 0 {
		t.Errorf("UpdateCount: got %d, want 0 (explicit zero, not -1 sentinel)", got.UpdateCount)
	}
}

// TestJavaExecutor_JavaError pins error surfacing from the Java side —
// exception class threaded through, original message preserved.
func TestJavaExecutor_JavaError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":            false,
			"error":              "Table not found: NONEXISTENT",
			"exceptionClass":     "RelationalException",
			"exceptionFullClass": "com.apple.foundationdb.relational.api.exceptions.RelationalException",
		})
	}))
	defer srv.Close()

	exe := NewJavaExecutorHTTP(srv.URL, "")
	got := exe.Run(context.Background(), Query{Name: "missing_table", SQL: "SELECT * FROM nonexistent"})
	if got.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(got.Err.Error(), "RelationalException") {
		t.Errorf("error must include exception class: %v", got.Err)
	}
	if !strings.Contains(got.Err.Error(), "Table not found") {
		t.Errorf("error must include original message: %v", got.Err)
	}
	if got.Rows != nil {
		t.Errorf("Rows must be nil on error, got %v", got.Rows)
	}
}

// TestJavaExecutor_HTTPNon200 pins that an HTTP non-200 surfaces as an
// error — an unregistered step or server-side crash should not be
// silently swallowed as a successful empty result.
func TestJavaExecutor_HTTPNon200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "step not registered", http.StatusInternalServerError)
	}))
	defer srv.Close()

	exe := NewJavaExecutorHTTP(srv.URL, "")
	got := exe.Run(context.Background(), Query{Name: "x", SQL: "SELECT 1"})
	if got.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(got.Err.Error(), "HTTP 500") {
		t.Errorf("error must include HTTP status: %v", got.Err)
	}
}

// TestJavaExecutor_AmbiguousResult pins that a result missing both
// columns AND updateCount surfaces as an error rather than a silent
// (Rows=nil, UpdateCount=-1) which the harness would treat as
// "successful empty SELECT".
func TestJavaExecutor_AmbiguousResult(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Result is a JSON object with neither columns nor updateCount —
		// e.g. a Java-side bug that returned an empty {} instead of
		// {updateCount: 0}.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  map[string]any{},
		})
	}))
	defer srv.Close()

	exe := NewJavaExecutorHTTP(srv.URL, "")
	got := exe.Run(context.Background(), Query{Name: "ambig", SQL: "?"})
	if got.Err == nil {
		t.Fatal("expected an error for ambiguous result, got nil")
	}
	if !strings.Contains(got.Err.Error(), "neither columns nor updateCount") {
		t.Errorf("error must name the ambiguity: %v", got.Err)
	}
}
