package cascades

import "testing"

// fanoutTestIndexDef is an IndexDef that also states its fan-out signal
// (IndexDefWithCreatesDuplicates), used to prove the plan-context builder
// threads createsDuplicates onto the candidate rather than hardcoding false.
type fanoutTestIndexDef struct {
	name       string
	cols       []string
	createsDup bool
}

func (d fanoutTestIndexDef) IndexName() string                { return d.name }
func (d fanoutTestIndexDef) IndexColumnNames() []string       { return d.cols }
func (d fanoutTestIndexDef) IndexRecordTypes() []string       { return []string{"T"} }
func (d fanoutTestIndexDef) IndexIsUnique() bool              { return false }
func (d fanoutTestIndexDef) IndexPrimaryKeyColumns() []string { return []string{"ID"} }
func (d fanoutTestIndexDef) IndexCreatesDuplicates() bool     { return d.createsDup }

// plainTestIndexDef does NOT implement IndexDefWithCreatesDuplicates — it must
// default to non-fan-out (the safe under-report).
type plainTestIndexDef struct{ name string }

func (d plainTestIndexDef) IndexName() string                { return d.name }
func (d plainTestIndexDef) IndexColumnNames() []string       { return []string{"V"} }
func (d plainTestIndexDef) IndexRecordTypes() []string       { return []string{"T"} }
func (d plainTestIndexDef) IndexIsUnique() bool              { return false }
func (d plainTestIndexDef) IndexPrimaryKeyColumns() []string { return []string{"ID"} }

// TestPlanContext_ThreadsCreatesDuplicates pins RFC-188 finding 10 M4: the
// plan-context builder threads the IndexDef's fan-out signal onto the candidate
// as a KNOWN/UNKNOWN tri-state, never a hardcoded false. Before M4 the signal
// was constant false, over-reporting every index (incl. fan-out) as distinct. A
// def that supplies no signal yields an UNKNOWN candidate
// (DistinctRecordsSignal()==nil → property abstains to distinct=false), so a
// fan-out index whose def omits the signal is never mis-stamped as distinct.
func TestPlanContext_ThreadsCreatesDuplicates(t *testing.T) {
	t.Parallel()

	signal := func(def IndexDef) *bool {
		ctx := NewPlanContextFromIndexDefs([]IndexDef{def})
		cands := ctx.GetMatchCandidates()
		if len(cands) != 1 {
			t.Fatalf("expected 1 candidate, got %d", len(cands))
		}
		sig, ok := cands[0].(interface{ DistinctRecordsSignal() *bool })
		if !ok {
			t.Fatal("candidate does not expose DistinctRecordsSignal()")
		}
		return sig.DistinctRecordsSignal()
	}

	// Fan-out def → known true (distinct will be false).
	if s := signal(fanoutTestIndexDef{name: "idx_tags", cols: []string{"TAGS"}, createsDup: true}); s == nil || !*s {
		t.Fatalf("fan-out IndexDef must thread a known createsDuplicates=true signal, got %v", s)
	}
	// Non-fan-out def → known false (distinct will be true).
	if s := signal(fanoutTestIndexDef{name: "idx_v", cols: []string{"V"}, createsDup: false}); s == nil || *s {
		t.Fatalf("non-fan-out IndexDef must thread a known createsDuplicates=false signal, got %v", s)
	}
	// Def WITHOUT the fan-out interface → UNKNOWN (nil) → property abstains
	// (distinct=false). Absence must NOT be read as "known non-fan-out", or a
	// fan-out index missing the signal would elide a legitimate DISTINCT.
	if s := signal(plainTestIndexDef{name: "idx_plain"}); s != nil {
		t.Fatalf("IndexDef without the fan-out signal must yield an UNKNOWN signal (nil), got %v (=%v)", s, *s)
	}
}
