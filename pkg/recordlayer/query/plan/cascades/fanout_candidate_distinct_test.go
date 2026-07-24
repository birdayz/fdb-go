package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

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

// plainTestIndexDef does NOT implement IndexDefWithCreatesDuplicates. Its
// candidate remains UNKNOWN and must not acquire a usable traversal.
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

	candidate := func(def IndexDef) MatchCandidate {
		ctx := NewPlanContextFromIndexDefs([]IndexDef{def})
		cands := ctx.GetMatchCandidates()
		if len(cands) != 1 {
			t.Fatalf("expected 1 candidate, got %d", len(cands))
		}
		return cands[0]
	}
	signal := func(cand MatchCandidate) *bool {
		sig, ok := cand.(interface{ DistinctRecordsSignal() *bool })
		if !ok {
			t.Fatal("candidate does not expose DistinctRecordsSignal()")
		}
		return sig.DistinctRecordsSignal()
	}

	// A true signal without the FAN_OUT AST retains the tri-state property but
	// cannot be structurally expanded, so it is unusable for data access.
	fanOutCandidate := candidate(fanoutTestIndexDef{
		name: "idx_tags", cols: []string{"TAGS"}, createsDup: true,
	})
	if s := signal(fanOutCandidate); s == nil || !*s {
		t.Fatalf("fan-out IndexDef must thread a known createsDuplicates=true signal, got %v", s)
	}
	if fanOutCandidate.GetTraversal() != nil {
		t.Fatal("fan-out signal without a structural root produced a flat traversal")
	}

	// A known false signal affirmatively permits the flat scalar path.
	scalarCandidate := candidate(fanoutTestIndexDef{
		name: "idx_v", cols: []string{"V"}, createsDup: false,
	})
	if s := signal(scalarCandidate); s == nil || *s {
		t.Fatalf("non-fan-out IndexDef must thread a known createsDuplicates=false signal, got %v", s)
	}
	if scalarCandidate.GetTraversal() == nil {
		t.Fatal("known scalar candidate did not retain its flat traversal")
	}

	// No signal and no root is UNKNOWN: both the property and data-access
	// traversal abstain. Merely withholding DISTINCT elimination is not enough;
	// a flat scan could still omit empty fan-out fields and multiply rows.
	unknownCandidate := candidate(plainTestIndexDef{name: "idx_plain"})
	if s := signal(unknownCandidate); s != nil {
		t.Fatalf("IndexDef without the fan-out signal must yield an UNKNOWN signal (nil), got %v (=%v)", s, *s)
	}
	if unknownCandidate.GetTraversal() != nil {
		t.Fatal("unknown index metadata produced a flat traversal")
	}
}

func TestCandidatePreservesBaseRecordCardinalityRequiresAffirmativeSignal(t *testing.T) {
	t.Parallel()

	alias := []values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}
	unknown := NewValueIndexScanMatchCandidate(
		"unknown",
		[]string{"T"},
		[]string{"V"},
		alias,
		values.UnknownType,
		false,
		nil,
	)
	if candidatePreservesBaseRecordCardinality(unknown) {
		t.Fatal("missing duplicate signal was treated as cardinality-preserving")
	}

	duplicates := true
	fanout := NewValueIndexScanMatchCandidateWithFunctions(
		"fanout",
		[]string{"T"},
		[]string{"V"},
		nil,
		alias,
		values.UnknownType,
		false,
		nil,
		&duplicates,
	)
	if candidatePreservesBaseRecordCardinality(fanout) {
		t.Fatal("known fanout candidate was treated as cardinality-preserving")
	}

	distinct := false
	scalar := NewValueIndexScanMatchCandidateWithFunctions(
		"scalar",
		[]string{"T"},
		[]string{"V"},
		nil,
		alias,
		values.UnknownType,
		false,
		nil,
		&distinct,
	)
	if !candidatePreservesBaseRecordCardinality(scalar) {
		t.Fatal("known scalar candidate did not preserve base-record cardinality")
	}
}
