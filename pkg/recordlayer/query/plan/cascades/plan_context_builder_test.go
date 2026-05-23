package cascades

import (
	"testing"
)

type testIndexDef struct {
	name        string
	columns     []string
	recordTypes []string
	unique      bool
}

func (d testIndexDef) IndexName() string                { return d.name }
func (d testIndexDef) IndexColumnNames() []string       { return d.columns }
func (d testIndexDef) IndexRecordTypes() []string       { return d.recordTypes }
func (d testIndexDef) IndexIsUnique() bool              { return d.unique }
func (d testIndexDef) IndexPrimaryKeyColumns() []string { return nil }

func TestNewPlanContextFromIndexDefs_Basic(t *testing.T) {
	t.Parallel()
	defs := []IndexDef{
		testIndexDef{
			name:        "Order$status",
			columns:     []string{"status"},
			recordTypes: []string{"Order"},
			unique:      false,
		},
		testIndexDef{
			name:        "Order$status_date",
			columns:     []string{"status", "date"},
			recordTypes: []string{"Order"},
			unique:      true,
		},
	}
	ctx := NewPlanContextFromIndexDefs(defs)
	cands := ctx.GetMatchCandidates()
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(cands))
	}
	if cands[0].CandidateName() != "Order$status" {
		t.Fatalf("cand[0] name=%q", cands[0].CandidateName())
	}
	if cands[1].CandidateName() != "Order$status_date" {
		t.Fatalf("cand[1] name=%q", cands[1].CandidateName())
	}
	cols := cands[0].GetColumnNames()
	if len(cols) != 1 || cols[0] != "STATUS" {
		t.Fatalf("cand[0] columns=%v, want [STATUS]", cols)
	}
	cols = cands[1].GetColumnNames()
	if len(cols) != 2 || cols[0] != "STATUS" || cols[1] != "DATE" {
		t.Fatalf("cand[1] columns=%v, want [STATUS DATE]", cols)
	}
	if !cands[1].IsUnique() {
		t.Fatal("cand[1] should be unique")
	}
	if cands[0].IsUnique() {
		t.Fatal("cand[0] should not be unique")
	}
}

func TestNewPlanContextFromIndexDefs_SkipsEmptyColumns(t *testing.T) {
	t.Parallel()
	defs := []IndexDef{
		testIndexDef{
			name:        "bad_idx",
			columns:     nil,
			recordTypes: []string{"X"},
		},
		testIndexDef{
			name:        "good_idx",
			columns:     []string{"a"},
			recordTypes: []string{"X"},
		},
	}
	ctx := NewPlanContextFromIndexDefs(defs)
	if len(ctx.GetMatchCandidates()) != 1 {
		t.Fatalf("expected 1 candidate (skip empty), got %d", len(ctx.GetMatchCandidates()))
	}
}

func TestNewPlanContextFromIndexDefs_UpperCasesSargable(t *testing.T) {
	t.Parallel()
	defs := []IndexDef{
		testIndexDef{
			name:        "idx",
			columns:     []string{"myCol", "Another_Col"},
			recordTypes: []string{"T"},
		},
	}
	ctx := NewPlanContextFromIndexDefs(defs)
	cols := ctx.GetMatchCandidates()[0].GetColumnNames()
	if cols[0] != "MYCOL" || cols[1] != "ANOTHER_COL" {
		t.Fatalf("columns not uppercased: %v", cols)
	}
}
