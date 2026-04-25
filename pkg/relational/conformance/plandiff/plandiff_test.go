package plandiff

import (
	"context"
	"errors"
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
// tolerant filter on Status.
func TestRun_AllJavaUnimplemented(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	report := Run(ctx, SeedCorpus(), NewGoEngine(), NewJavaEngine())
	if report.Summary.Total != len(SeedCorpus()) {
		t.Fatalf("Total: got %d, want %d", report.Summary.Total, len(SeedCorpus()))
	}
	if report.Summary.JavaUnimplemented != report.Summary.Total {
		t.Fatalf("expected every case to be JAVA_UNIMPL, got %d/%d (go-err %d, both-err %d)",
			report.Summary.JavaUnimplemented, report.Summary.Total,
			report.Summary.GoError, report.Summary.BothError)
	}
	if report.Summary.GoError != 0 {
		t.Fatalf("Go engine errored on %d cases — corpus regression", report.Summary.GoError)
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

// TestSeedCorpus_BaselineHash pins the corpus-wide regression key.
// Any change to a query name, query SQL, or the naive planner's
// Explain output for ANY query in the corpus changes this hash. The
// expected value below is the swingshift-50 baseline; update
// deliberately when you add/remove queries or intentionally change
// the planner output.
//
// Maintainer note: when this fails, run
//
//	go test -run=TestSeedCorpus_BaselineHash ./pkg/relational/conformance/plandiff/ -v
//
// and read the diagnostic line "current hash: <new>". If the change
// is intentional (you added a corpus query, or changed the naive
// planner's tree shape on purpose), update the constant below. If
// it's accidental, the diagnostic shows which queries diverge from
// the per-query golden hashes (see TestSeedCorpus_PerQueryGoldens).
func TestSeedCorpus_BaselineHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	report := Run(ctx, SeedCorpus(), NewGoEngine(), NewJavaEngine())
	got := HashCorpus(report)
	// swingshift-50 baseline. Deliberate planner / corpus changes
	// require updating this constant. Run with `-v` to see the
	// current hash diagnostic and copy the new value here.
	const wantBaseline = "7f373f382aa174115e8932c7173f84323ef9738dec0daa0a595d7ccba48d9c42"
	if got != wantBaseline {
		// Per-query report so the user sees WHICH query changed, not
		// just "the corpus changed".
		var msg strings.Builder
		msg.WriteString("plan-equivalence corpus hash drifted.\n")
		msg.WriteString("If this change is intentional (corpus add/remove or planner output change),\n")
		msg.WriteString("update wantBaseline in plandiff_test.go.\n\n")
		msg.WriteString("got:  " + got + "\n")
		msg.WriteString("want: " + wantBaseline + "\n\n")
		msg.WriteString("Per-query trees:\n")
		for _, c := range report.Cases {
			if c.Status == StatusGoError {
				msg.WriteString("  [" + c.Query.Name + "] GO ERROR: " + c.Go.Err.Error() + "\n")
				continue
			}
			msg.WriteString("  [" + c.Query.Name + "] " + c.Go.Hash[:12] + " — " + strings.ReplaceAll(c.Go.Tree, "\n", " | ") + "\n")
		}
		t.Fatal(msg.String())
	}
}
