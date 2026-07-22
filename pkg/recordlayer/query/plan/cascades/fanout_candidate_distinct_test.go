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
// plan-context builder must thread the IndexDef's fan-out signal onto the match
// candidate. Before the fix ValueIndexScanMatchCandidate.createsDuplicates was
// never populated (constant false), so every index scan — including fan-out
// indexes — reported DistinctRecords=true (the unsafe over-report, a dropped
// dedup → duplicate rows).
func TestPlanContext_ThreadsCreatesDuplicates(t *testing.T) {
	t.Parallel()

	creates := func(def IndexDef) bool {
		ctx := NewPlanContextFromIndexDefs([]IndexDef{def})
		cands := ctx.GetMatchCandidates()
		if len(cands) != 1 {
			t.Fatalf("expected 1 candidate, got %d", len(cands))
		}
		dup, ok := cands[0].(interface{ CreatesDuplicates() bool })
		if !ok {
			t.Fatal("candidate does not expose CreatesDuplicates()")
		}
		return dup.CreatesDuplicates()
	}

	if !creates(fanoutTestIndexDef{name: "idx_tags", cols: []string{"TAGS"}, createsDup: true}) {
		t.Fatal("fan-out IndexDef must produce a candidate with CreatesDuplicates()=true (signal not threaded)")
	}
	if creates(fanoutTestIndexDef{name: "idx_v", cols: []string{"V"}, createsDup: false}) {
		t.Fatal("non-fan-out IndexDef must produce a candidate with CreatesDuplicates()=false")
	}
	if creates(plainTestIndexDef{name: "idx_plain"}) {
		t.Fatal("IndexDef without the fan-out signal must default to non-fan-out (safe under-report)")
	}
}
