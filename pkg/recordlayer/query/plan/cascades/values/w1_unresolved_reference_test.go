package values

import (
	"reflect"
	"sort"
	"testing"
)

// RFC-048 W1: the executor "no unresolved reference" invariant. A top-level
// FieldValue whose name is absent from a *complete* row (Strict context) must
// be reported, not silently resolved to NULL — the cardinal silent-wrong.
//
// These tests pin the mechanism in isolation (FieldValue.Evaluate); the
// end-to-end proof that no real query trips it lives in the sqldriver FDB
// suite (TestFDB_W1_*).

// withReportHook installs a recording ReportUnresolvedReference hook for the
// duration of the test and returns the slice that captures missing names.
func withReportHook(t *testing.T) *[]string {
	t.Helper()
	var got []string
	prev := ReportUnresolvedReference
	ReportUnresolvedReference = func(field string, available []string) {
		got = append(got, field)
	}
	t.Cleanup(func() { ReportUnresolvedReference = prev })
	return &got
}

func TestFieldValue_Strict_ReportsMissingLocalReference(t *testing.T) {
	got := withReportHook(t)

	// The original Exhibit-A shape: the aggregate slot was materialised under
	// "COUNT(*)" but the HAVING rewrite referenced "COUNT(1)". Against a
	// complete (Strict) row that only has "COUNT(*)", the "COUNT(1)" lookup is
	// an unresolved reference, not a NULL.
	row := &RowEvalContext{
		Datum:  map[string]any{"COUNT(*)": int64(3), "STATUS": "shipped"},
		Strict: true,
	}
	missing := &FieldValue{Field: "COUNT(1)"}
	if v := missing.Evaluate(row); v != nil {
		t.Fatalf("missing local ref should still evaluate to nil, got %v", v)
	}
	if want := []string{"COUNT(1)"}; !reflect.DeepEqual(*got, want) {
		t.Fatalf("ReportUnresolvedReference: want %v, got %v", want, *got)
	}
}

func TestFieldValue_Strict_PresentReferenceNotReported(t *testing.T) {
	got := withReportHook(t)

	row := &RowEvalContext{
		Datum:  map[string]any{"COUNT(*)": int64(3)},
		Strict: true,
	}
	// A present key whose value is a legitimate SQL NULL must NOT report:
	// the distinction is "name absent" (bug) vs "value nil" (NULL).
	row.Datum["SUM(AMOUNT)"] = nil
	if v := (&FieldValue{Field: "SUM(AMOUNT)"}).Evaluate(row); v != nil {
		t.Fatalf("present nil-valued key: want nil value, got %v", v)
	}
	if v := (&FieldValue{Field: "COUNT(*)"}).Evaluate(row); v != int64(3) {
		t.Fatalf("present key: want 3, got %v", v)
	}
	if len(*got) != 0 {
		t.Fatalf("present references must not report, got %v", *got)
	}
}

func TestFieldValue_NonStrict_DoesNotReport(t *testing.T) {
	got := withReportHook(t)

	// Base-record rows (Strict=false) legitimately omit unset optional fields;
	// a miss is a NULL, never a violation. This is the soundness guarantee
	// that keeps W1 from drowning in false positives.
	row := &RowEvalContext{
		Datum:  map[string]any{"ID": int64(1)},
		Strict: false,
	}
	if v := (&FieldValue{Field: "OPTIONAL_UNSET"}).Evaluate(row); v != nil {
		t.Fatalf("absent optional field: want nil, got %v", v)
	}
	if len(*got) != 0 {
		t.Fatalf("non-strict context must never report, got %v", *got)
	}
}

func TestFieldValue_Strict_NilHookIsNoOp(t *testing.T) {
	// With no hook installed (production default), a Strict miss is a plain nil
	// — zero behaviour change beyond the lookup that already happened.
	prev := ReportUnresolvedReference
	ReportUnresolvedReference = nil
	t.Cleanup(func() { ReportUnresolvedReference = prev })

	row := &RowEvalContext{Datum: map[string]any{"A": 1}, Strict: true}
	if v := (&FieldValue{Field: "B"}).Evaluate(row); v != nil {
		t.Fatalf("nil hook: want nil, got %v", v)
	}
}

func TestFieldValue_Strict_ReportsAvailableKeys(t *testing.T) {
	// The diagnostic carries the row's actual key set so a violation is
	// attributable (what was materialised vs what was asked for).
	var availForB []string
	prev := ReportUnresolvedReference
	ReportUnresolvedReference = func(field string, available []string) {
		if field == "B" {
			availForB = append([]string(nil), available...)
		}
	}
	t.Cleanup(func() { ReportUnresolvedReference = prev })

	row := &RowEvalContext{
		Datum:  map[string]any{"A": 1, "COUNT(*)": 2},
		Strict: true,
	}
	(&FieldValue{Field: "B"}).Evaluate(row)
	sort.Strings(availForB)
	if want := []string{"A", "COUNT(*)"}; !reflect.DeepEqual(availForB, want) {
		t.Fatalf("available keys: want %v, got %v", want, availForB)
	}
}
