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

// TestSeedCorpus_GoEngineSucceedsOnAll asserts the naive Go generator
// produces a non-empty plan tree for every seed query without error.
// A failure here means a corpus entry parses but the planner refuses
// it — the harness loses a regression sentinel.
func TestSeedCorpus_GoEngineSucceedsOnAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eng := NewGoEngine()
	corpus := SeedCorpus()
	for _, q := range corpus {
		q := q
		t.Run(q.Name, func(t *testing.T) {
			t.Parallel()
			r := eng.Plan(ctx, q)
			if r.Err != nil {
				t.Fatalf("Go engine errored on %s: %v\n  sql: %s", q.Name, r.Err, q.SQL)
			}
			if r.Tree == "" {
				t.Fatalf("Go engine produced empty tree for %s\n  sql: %s", q.Name, q.SQL)
			}
			if r.Hash == "" {
				t.Fatalf("Go engine produced empty hash for %s", q.Name)
			}
		})
	}
}

// TestRun_AllJavaUnimplemented pins that Run with the stub Java engine
// classifies every case as StatusJavaUnimplemented (not StatusGoError /
// StatusBothError). This lets a CI gate that wants to be Go-only
// tolerant filter on Status. The single Total==JavaUnimplemented check
// covers both the "all Java unimpl" and "no Go errors" properties —
// if Total matches JavaUnimplemented, no other status (including
// GoError) can be non-zero.
func TestRun_AllJavaUnimplemented(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	report := Run(ctx, SeedCorpus(), NewGoEngine(), NewJavaEngine())
	if report.Summary.Total != len(SeedCorpus()) {
		t.Fatalf("Total: got %d, want %d", report.Summary.Total, len(SeedCorpus()))
	}
	if report.Summary.JavaUnimplemented != report.Summary.Total {
		t.Fatalf("expected every case to be JAVA_UNIMPL, got %d/%d (go-err %d, both-err %d, diverge %d)",
			report.Summary.JavaUnimplemented, report.Summary.Total,
			report.Summary.GoError, report.Summary.BothError, report.Summary.Diverge)
	}
}

// TestRun_NilEngine pins the noop fallback: passing nil engines does
// not panic. Useful for early bring-up where one side isn't wired
// yet.
func TestRun_NilEngine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	report := Run(ctx, []Query{{Name: "x", SQL: "SELECT * FROM t"}}, nil, nil)
	if report.Summary.Total != 1 {
		t.Fatalf("Total: got %d, want 1", report.Summary.Total)
	}
	// Both sides errored with "nil X engine" — classified as
	// StatusBothError.
	if report.Summary.BothError != 1 {
		t.Fatalf("expected 1 BOTH_ERROR, got Summary=%+v", report.Summary)
	}
}

// TestClassify_Agree pins the happy-path branch: when both engines
// produce identical normalised trees, Status=AGREE and Detail empty.
func TestClassify_Agree(t *testing.T) {
	t.Parallel()
	q := Query{Name: "x", SQL: "SELECT 1"}
	gr := PlanResult{Engine: "go", Tree: "Scan(t)", Hash: "g"}
	jr := PlanResult{Engine: "java", Tree: "Scan(t)", Hash: "j"}
	d := classify(q, gr, jr)
	if d.Status != StatusAgree {
		t.Fatalf("Status: got %s, want AGREE", d.Status)
	}
	if d.Detail != "" {
		t.Fatalf("Detail: got %q, want empty", d.Detail)
	}
}

// TestClassify_Diverge_DiffNonEmpty pins that diverging trees produce
// a Detail with the line-by-line diff (— go / + java markers).
func TestClassify_Diverge_DiffNonEmpty(t *testing.T) {
	t.Parallel()
	q := Query{Name: "x", SQL: "SELECT 1"}
	gr := PlanResult{Engine: "go", Tree: "Scan(t)\nFilter(active = TRUE)"}
	jr := PlanResult{Engine: "java", Tree: "Scan(t)\nFilter(active IS TRUE)"}
	d := classify(q, gr, jr)
	if d.Status != StatusDiverge {
		t.Fatalf("Status: got %s, want DIVERGE", d.Status)
	}
	if !strings.Contains(d.Detail, "--- go") || !strings.Contains(d.Detail, "+++ java") {
		t.Fatalf("Detail missing diff markers: %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "active = TRUE") {
		t.Fatalf("Detail missing go-side line: %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "active IS TRUE") {
		t.Fatalf("Detail missing java-side line: %q", d.Detail)
	}
}

// TestClassify_GoError pins that a Go-side error (Java succeeded)
// surfaces as StatusGoError with the go: prefix in Detail.
func TestClassify_GoError(t *testing.T) {
	t.Parallel()
	q := Query{Name: "x", SQL: "BAD SQL"}
	gr := PlanResult{Engine: "go", Err: errors.New("parse: unexpected token")}
	jr := PlanResult{Engine: "java", Tree: "Scan(t)"}
	d := classify(q, gr, jr)
	if d.Status != StatusGoError {
		t.Fatalf("Status: got %s, want GO_ERROR", d.Status)
	}
	if !strings.Contains(d.Detail, "go: parse:") {
		t.Fatalf("Detail missing go: prefix: %q", d.Detail)
	}
}

// TestClassify_GoError_WhenJavaUnimplemented pins the subtle branch
// reviewer flagged: when Go errors AND Java is the stub, Status must
// be StatusGoError (not StatusBothError) so a Go-only CI tolerant
// path doesn't lose the real Go failure behind the stub-Java noise.
// Detail must surface BOTH the Go error and the Java stub message
// so the operator can confirm both sides at a glance.
func TestClassify_GoError_WhenJavaUnimplemented(t *testing.T) {
	t.Parallel()
	q := Query{Name: "x", SQL: "BAD SQL"}
	gr := PlanResult{Engine: "go", Err: errors.New("parse: unexpected token")}
	jr := PlanResult{Engine: "java", Err: ErrJavaUnimplemented}
	d := classify(q, gr, jr)
	if d.Status != StatusGoError {
		t.Fatalf("Status: got %s, want GO_ERROR", d.Status)
	}
	if !strings.Contains(d.Detail, "go: parse:") {
		t.Fatalf("Detail missing go-side error: %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "java:") {
		t.Fatalf("Detail missing java-side stub message: %q", d.Detail)
	}
}

// TestLineDiff_Transposition pins the documented limitation: lines
// present on both sides at different positions are NOT emitted as
// changed — they look like context. Status is still correctly
// StatusDiverge (normaliseTree differs), but Detail loses the
// ordering signal. Documented in lineDiff's doc comment; this test
// makes a future "fix" that switches to LCS land cleanly without
// breaking callers that expect today's behaviour.
func TestLineDiff_Transposition(t *testing.T) {
	t.Parallel()
	q := Query{Name: "x", SQL: "SELECT 1"}
	gr := PlanResult{Engine: "go", Tree: "Scan(t)\nFilter(p)\nProject(id)"}
	jr := PlanResult{Engine: "java", Tree: "Scan(t)\nProject(id)\nFilter(p)"}
	d := classify(q, gr, jr)
	if d.Status != StatusDiverge {
		t.Fatalf("Status: got %s, want DIVERGE (trees differ after normalise)", d.Status)
	}
	// All three lines appear in BOTH sides → multiset-membership
	// guard suppresses every emission. Detail has no `-` or `+`
	// markers; only context. Pin the limitation explicitly.
	if strings.Contains(d.Detail, "- Filter(p)") || strings.Contains(d.Detail, "+ Project(id)") {
		t.Fatalf("transposed lines unexpectedly emitted in diff (docs say they're suppressed): %q", d.Detail)
	}
	if !strings.Contains(d.Detail, "Scan(t)") {
		t.Fatalf("expected at least one context line in diff, got %q", d.Detail)
	}
}

// TestClassify_JavaUnimplemented_StatusOverridesError pins that
// ErrJavaUnimplemented produces StatusJavaUnimplemented (not
// StatusJavaError) so the soft-failure path can ignore the stub.
func TestClassify_JavaUnimplemented_StatusOverridesError(t *testing.T) {
	t.Parallel()
	q := Query{Name: "x", SQL: "SELECT 1"}
	gr := PlanResult{Engine: "go", Tree: "Scan(t)"}
	jr := PlanResult{Engine: "java", Err: ErrJavaUnimplemented}
	d := classify(q, gr, jr)
	if d.Status != StatusJavaUnimplemented {
		t.Fatalf("Status: got %s, want JAVA_UNIMPL", d.Status)
	}
}

// TestNormaliseTree pins the whitespace + empty-line collapse: a
// trailing newline must NOT show as a phantom diff against the same
// tree without one.
func TestNormaliseTree(t *testing.T) {
	t.Parallel()
	a := "Scan(t)\nFilter(p)\n"
	b := "Scan(t)\nFilter(p)"
	c := "  Scan(t)  \n\nFilter(p)\n"
	if normaliseTree(a) != normaliseTree(b) {
		t.Fatalf("trailing-newline diff: %q vs %q", normaliseTree(a), normaliseTree(b))
	}
	if !strings.Contains(normaliseTree(c), "Scan(t)") {
		t.Fatalf("normalise dropped content: %q", normaliseTree(c))
	}
}

// TestHashTree_Stable pins SHA-256 hex output. A known Tree must
// produce the same hash every run; a changed Tree must produce a
// different hash. Used as the corpus-level regression key.
func TestHashTree_Stable(t *testing.T) {
	t.Parallel()
	h1 := hashTree("Scan(t)\nFilter(active = TRUE)")
	h2 := hashTree("Scan(t)\nFilter(active = TRUE)")
	h3 := hashTree("Scan(t)\nFilter(active = FALSE)")
	if h1 != h2 {
		t.Fatalf("hash not stable: %q vs %q", h1, h2)
	}
	if h1 == h3 {
		t.Fatalf("different trees produced same hash: %q", h1)
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64-hex SHA-256, got %d chars: %q", len(h1), h1)
	}
}

// TestFormatReport_AgreeNotExpanded pins the report formatter's
// terseness rule: AGREE cases don't expand, only the count surfaces
// in the header.
func TestFormatReport_AgreeNotExpanded(t *testing.T) {
	t.Parallel()
	r := Report{
		Cases: []Diff{
			{Query: Query{Name: "ok"}, Status: StatusAgree},
			{Query: Query{Name: "bad"}, Status: StatusDiverge, Detail: "diff text"},
		},
		Summary: Summary{Total: 2, Agree: 1, Diverge: 1},
	}
	out := FormatReport(r)
	if !strings.Contains(out, "1 agree") || !strings.Contains(out, "1 diverge") {
		t.Fatalf("summary line missing counts: %q", out)
	}
	if strings.Contains(out, "[AGREE]") {
		t.Fatalf("AGREE case unexpectedly expanded: %q", out)
	}
	if !strings.Contains(out, "[DIVERGE]") {
		t.Fatalf("DIVERGE case missing: %q", out)
	}
	if !strings.Contains(out, "diff text") {
		t.Fatalf("DIVERGE detail missing: %q", out)
	}
}

// TestJavaEngine_HappyPath pins the HTTP wire shape: the engine POSTs
// to /invoke with step=planSql + params{clusterFile, schemaTemplate, sql},
// reads back {success: true, result: "<plan tree>"}, and returns a
// PlanResult with Tree + Hash populated.
func TestJavaEngine_HappyPath(t *testing.T) {
	t.Parallel()
	const wantPlanText = "ISCAN(NAME_IDX <,>)\n  FetchRecords()"
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
		if req.Step != "planSql" {
			http.Error(w, "unexpected step "+req.Step, http.StatusBadRequest)
			return
		}
		// Verify the harness threaded the right params through.
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
			"result":  wantPlanText,
		})
	}))
	defer srv.Close()

	eng := NewJavaEngineHTTP(srv.URL, "fake-cluster-file-content")
	got := eng.Plan(context.Background(), Query{
		Name:           "x",
		SQL:            "SELECT * FROM t",
		SchemaTemplate: "CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))",
	})
	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if got.Tree != wantPlanText {
		t.Fatalf("Tree: got %q, want %q", got.Tree, wantPlanText)
	}
	if got.Hash == "" || len(got.Hash) != 64 {
		t.Fatalf("Hash: got %q (len %d), want 64-hex", got.Hash, len(got.Hash))
	}
}

// TestJavaEngine_JavaError pins the failure shape: when the Java
// step returns {success: false, exceptionClass: "...", error: "..."},
// the engine surfaces the exception class in the error message so
// classify() can attribute the failure correctly.
func TestJavaEngine_JavaError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":            false,
			"error":              "Unsupported feature: window function",
			"exceptionClass":     "RelationalException",
			"exceptionFullClass": "com.apple.foundationdb.relational.api.exceptions.RelationalException",
		})
	}))
	defer srv.Close()

	eng := NewJavaEngineHTTP(srv.URL, "")
	got := eng.Plan(context.Background(), Query{Name: "x", SQL: "SELECT ROW_NUMBER() OVER ()"})
	if got.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(got.Err.Error(), "RelationalException") {
		t.Fatalf("error message missing exception class: %v", got.Err)
	}
	if !strings.Contains(got.Err.Error(), "window function") {
		t.Fatalf("error message missing original message: %v", got.Err)
	}
}

// TestJavaError_SQLStateExtraction pins that the JavaError struct
// carries the SQLSTATE field through from the conformance server's
// structured error response. Wired nightshift-57 so the cross-engine
// yamsql harness's `assertCrossEngineErrorCode` can match SQLSTATEs
// without parsing the message text.
func TestJavaError_SQLStateExtraction(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":            false,
			"error":              "Type mismatch in IN list",
			"exceptionClass":     "RelationalException",
			"exceptionFullClass": "com.apple.foundationdb.relational.api.exceptions.RelationalException",
			"sqlState":           "42804",
		})
	}))
	defer srv.Close()

	eng := NewJavaEngineHTTP(srv.URL, "")
	got := eng.Plan(context.Background(), Query{Name: "x", SQL: "SELECT id FROM t WHERE v IN (1, 'a')"})
	if got.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	var je *JavaError
	if !errors.As(got.Err, &je) {
		t.Fatalf("expected *JavaError, got %T: %v", got.Err, got.Err)
	}
	if je.SQLState != "42804" {
		t.Fatalf("SQLState: got %q, want %q", je.SQLState, "42804")
	}
	if je.ExceptionClass != "RelationalException" {
		t.Fatalf("ExceptionClass: got %q", je.ExceptionClass)
	}
	if je.Message != "Type mismatch in IN list" {
		t.Fatalf("Message: got %q", je.Message)
	}
}

// TestJavaError_NoSQLState pins that JavaError handles the case where
// Java's exception didn't expose a SQLSTATE — bare RuntimeException
// (NullPointerException, ArithmeticException) without
// RelationalException wrapping. SQLState is left empty; the Error()
// format still includes the exception class.
func TestJavaError_NoSQLState(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":            false,
			"error":              "/ by zero",
			"exceptionClass":     "ArithmeticException",
			"exceptionFullClass": "java.lang.ArithmeticException",
			// no sqlState field — Java's bare RuntimeException
		})
	}))
	defer srv.Close()

	eng := NewJavaEngineHTTP(srv.URL, "")
	got := eng.Plan(context.Background(), Query{Name: "x", SQL: "SELECT 1/0"})
	if got.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	var je *JavaError
	if !errors.As(got.Err, &je) {
		t.Fatalf("expected *JavaError, got %T: %v", got.Err, got.Err)
	}
	if je.SQLState != "" {
		t.Fatalf("SQLState should be empty for bare RuntimeException, got %q", je.SQLState)
	}
	if je.ExceptionClass != "ArithmeticException" {
		t.Fatalf("ExceptionClass: got %q", je.ExceptionClass)
	}
}

// TestJavaEngine_HTTPNon200 pins that a non-200 response from the
// server (e.g. step not registered, JSON parse error on Java side)
// surfaces as a plandiff: HTTP <code> error.
func TestJavaEngine_HTTPNon200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "step not found", http.StatusNotFound)
	}))
	defer srv.Close()

	eng := NewJavaEngineHTTP(srv.URL, "")
	got := eng.Plan(context.Background(), Query{Name: "x", SQL: "SELECT 1"})
	if got.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(got.Err.Error(), "HTTP 404") {
		t.Fatalf("expected HTTP 404 in error: %v", got.Err)
	}
}

// TestJavaEngine_NilBaseURL pins the Go-only-CI fallback: the no-arg
// constructor surfaces ErrJavaUnimplemented for every call so the
// harness reports JAVA_UNIMPL rather than crashing.
func TestJavaEngine_NilBaseURL(t *testing.T) {
	t.Parallel()
	eng := NewJavaEngine()
	got := eng.Plan(context.Background(), Query{Name: "x", SQL: "SELECT 1"})
	if !errors.Is(got.Err, ErrJavaUnimplemented) {
		t.Fatalf("expected ErrJavaUnimplemented, got %v", got.Err)
	}
}

// FuzzNormaliseTree pins the no-panic contract: any byte sequence
// (including invalid UTF-8, embedded NULs, unbalanced whitespace)
// fed as a tree must normalise without crashing the harness.
func FuzzNormaliseTree(f *testing.F) {
	for _, seed := range []string{
		"",
		"   ",
		"a\nb",
		"\n\n\n",
		"\xff\xfe\xfd",    // invalid UTF-8
		"  Scan(t)  \n\n", // leading/trailing whitespace
		"a\r\nb",          // CRLF
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		// Should never panic regardless of input.
		out := normaliseTree(in)
		// Idempotency: normalise(normalise(x)) == normalise(x).
		if normaliseTree(out) != out {
			t.Fatalf("normaliseTree not idempotent on %q: got %q vs %q", in, out, normaliseTree(out))
		}
	})
}

// FuzzHashTree pins the same no-panic contract for hashTree, plus
// the determinism invariant: the same input always produces the
// same hash.
func FuzzHashTree(f *testing.F) {
	for _, seed := range []string{"", " ", "Scan(t)", "\xff\xfe", "a\nb"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		h1 := hashTree(in)
		h2 := hashTree(in)
		if h1 != h2 {
			t.Fatalf("hashTree non-deterministic on %q: %q vs %q", in, h1, h2)
		}
		if len(h1) != 64 {
			t.Fatalf("hashTree did not produce 64-hex on %q: got %d chars", in, len(h1))
		}
	})
}

// TestSeedCorpus_BaselineHash was removed swingshift-52: the
// hand-pinned hash had to be updated every time the corpus grew, and
// the per-query goldens (TestSeedCorpus_PerQueryGoldens, when wired)
// already cover the regression-detection use case at finer
// granularity. Adding a corpus entry no longer requires touching a
// hash constant.
