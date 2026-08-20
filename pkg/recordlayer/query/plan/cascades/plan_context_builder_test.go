package cascades

import (
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
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
func (d testIndexDef) IndexCreatesDuplicates() bool     { return false }

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
	// The def's own spelling, verbatim: an index column name is PHYSICAL, and
	// the candidate resolves it by exact name against a base type named from
	// the same descriptor.
	cols := cands[0].GetColumnNames()
	if len(cols) != 1 || cols[0] != "status" {
		t.Fatalf("cand[0] columns=%v, want [status]", cols)
	}
	cols = cands[1].GetColumnNames()
	if len(cols) != 2 || cols[0] != "status" || cols[1] != "date" {
		t.Fatalf("cand[1] columns=%v, want [status date]", cols)
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

// TestNewPlanContextFromIndexDefs_KeepsSargableColumnCase pins that an index
// column name reaches the candidate with its case INTACT.
//
// The builder used to upper-fold it. That was invisible for a DDL-created
// metadata, whose descriptor names are already upper, and silently fatal for
// any other: the candidate's expansion resolves each column by EXACT name
// against a base type whose slots come from values.FieldNameForProtoField, so a
// folded name misses, expandValueIndex returns nil, the candidate declines and
// the planner full-scans. Rows stay correct, nothing goes red, and the index is
// simply never used — which is why this is pinned on the NAME rather than left
// to an end-to-end plan assertion.
func TestNewPlanContextFromIndexDefs_KeepsSargableColumnCase(t *testing.T) {
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
	if len(cols) != 2 || cols[0] != "myCol" || cols[1] != "Another_Col" {
		t.Fatalf("columns=%v, want [myCol Another_Col] verbatim", cols)
	}
}

// TestNewPlanContextFromIndexDefs_KeepsPrimaryKeyColumnCase is the same pin on
// the appended primary-key suffix, which was folded by the same loop.
func TestNewPlanContextFromIndexDefs_KeepsPrimaryKeyColumnCase(t *testing.T) {
	t.Parallel()
	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		pkTestIndexDef{
			testIndexDef: testIndexDef{
				name:        "idx",
				columns:     []string{"myCol"},
				recordTypes: []string{"T"},
			},
			pk: []string{"rec_id"},
		},
	})
	candidate, ok := ctx.GetMatchCandidates()[0].(*ValueIndexScanMatchCandidate)
	if !ok {
		t.Fatalf("candidate type = %T, want *ValueIndexScanMatchCandidate", ctx.GetMatchCandidates()[0])
	}
	pk := candidate.GetPKColumnNames()
	if len(pk) != 1 || pk[0] != "rec_id" {
		t.Fatalf("primary key columns=%v, want [rec_id] verbatim", pk)
	}
}

type pkTestIndexDef struct {
	testIndexDef
	pk []string
}

func (d pkTestIndexDef) IndexPrimaryKeyColumns() []string { return d.pk }

type rootTestIndexDef struct {
	testIndexDef
	root *gen.KeyExpression
}

func (d rootTestIndexDef) IndexRootKeyExpression() *gen.KeyExpression {
	return d.root
}

func TestNewPlanContextFromIndexDefs_ScalarNestingFailsClosed(t *testing.T) {
	t.Parallel()

	scalar := gen.Field_SCALAR
	fanOut := gen.Field_FAN_OUT
	nested := func(parentFanType gen.Field_FanType) *gen.KeyExpression {
		return &gen.KeyExpression{Nesting: &gen.Nesting{
			Parent: &gen.Field{
				FieldName: proto.String("ADDR"),
				FanType:   &parentFanType,
			},
			Child: &gen.KeyExpression{Field: &gen.Field{
				FieldName: proto.String("CITY"),
				FanType:   &scalar,
			}},
		}}
	}
	nestedChildFanOut := &gen.KeyExpression{Nesting: &gen.Nesting{
		Parent: &gen.Field{
			FieldName: proto.String("ADDR"),
			FanType:   &scalar,
		},
		Child: &gen.KeyExpression{Field: &gen.Field{
			FieldName: proto.String("CITY"),
			FanType:   &fanOut,
		}},
	}}
	mixedScalarNestingAndFanOut := &gen.KeyExpression{
		Then: &gen.Then{Child: []*gen.KeyExpression{
			nested(scalar),
			{Field: &gen.Field{
				FieldName: proto.String("TAGS"),
				FanType:   &fanOut,
			}},
		}},
	}
	ctx := NewPlanContextFromIndexDefs([]IndexDef{
		rootTestIndexDef{
			testIndexDef: testIndexDef{
				name:        "nested_scalar_city",
				columns:     []string{"CITY"},
				recordTypes: []string{"T"},
			},
			root: nested(scalar),
		},
		rootTestIndexDef{
			testIndexDef: testIndexDef{
				name:        "nested_fanout_city",
				columns:     []string{"CITY"},
				recordTypes: []string{"T"},
			},
			root: nested(fanOut),
		},
		rootTestIndexDef{
			testIndexDef: testIndexDef{
				name:        "nested_child_fanout_city",
				columns:     []string{"CITY"},
				recordTypes: []string{"T"},
			},
			root: nestedChildFanOut,
		},
		rootTestIndexDef{
			testIndexDef: testIndexDef{
				name:        "mixed_scalar_nesting_and_fanout",
				columns:     []string{"CITY", "TAGS"},
				recordTypes: []string{"T"},
			},
			root: mixedScalarNestingAndFanOut,
		},
	})
	candidates := ctx.GetMatchCandidates()
	if len(candidates) != 2 ||
		candidates[0].CandidateName() != "nested_fanout_city" ||
		candidates[1].CandidateName() != "nested_child_fanout_city" {
		t.Fatalf(
			"nested candidates = %#v, want only structurally expanded fanout; scalar ADDR.CITY must never bind top-level CITY, even beside fanout TAGS",
			candidates,
		)
	}
}
