package sqldriver_test

// Cross-leg same-name-different-type metadata pins (RFC-181 WS-N Phase D).
//
// A join whose two legs each declare a column with the SAME name but a
// DIFFERENT type is the shape where name-keyed metadata derivation goes
// wrong in two independent, separately-observable ways:
//
//  1. TYPE: a bare-name descriptor search over the join's leaf descriptors
//     finds BOTH legs and has to pick one. Picking first-match types the far
//     leg's column with the near leg's type, so the client is told a STRING
//     column is a BIGINT. Java never searches: client metadata is positional
//     over the plan's own flowed record type, so each projected column
//     carries the type of the value that actually flows into that slot.
//
//  2. VALUE: ColumnDef.Name doubles as the by-name datum lookup key, and
//     NewRecordLayerResultSet folds the defs into a name->index map. Two
//     slots sharing a Name collapse to one entry, so a by-name read of the
//     colliding column serves the WRONG leg's value — a wrong-VALUES bug,
//     not merely a wrong-label one.
//
// Both assertions are on the metadata derived from a planned query, which is
// the same derivation the driver hands the client.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
)

// buildCrossLegSameNameMetadata builds two tables that each declare a column
// named VAL, with DIFFERENT types: LEFTT.VAL is a BIGINT, RIGHTT.VAL is a
// STRING. Any name-keyed type derivation must collide on VAL.
func buildCrossLegSameNameMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName("xlegsamename")
	b.AddTable("LEFTT", []metadata.ColumnSpec{
		metadata.NewColumnSpec("LID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("VAL", api.NewLongType(true), 2),
	}, []string{"LID"})
	b.AddTable("RIGHTT", []metadata.ColumnSpec{
		metadata.NewColumnSpec("RID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("VAL", api.NewStringType(true), 2),
	}, []string{"RID"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return tmpl.Underlying()
}

// TestCrossLegSameNameDifferentType_ProjectedTypes pins deliverable (1): an
// explicit projection of both legs' same-named columns must report each
// column's OWN type, positionally. Under a bare-name descriptor search both
// slots report the first matching leg's type.
func TestCrossLegSameNameDifferentType_ProjectedTypes(t *testing.T) {
	t.Parallel()
	md := buildCrossLegSameNameMetadata(t)
	plan, perr := embedded.PlanRecordQueryWithMetadata(
		`SELECT A.VAL, B.VAL FROM LEFTT A, RIGHTT B`, md, nil)
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	defs := embedded.ResultColumnDefsForPlan(plan, md)
	if len(defs) != 2 {
		t.Fatalf("got %d columns, want 2: %+v", len(defs), defs)
	}
	// LEFTT.VAL is declared BIGINT, RIGHTT.VAL is declared STRING. The two
	// slots must NOT report the same type — that is the collision.
	if strings.EqualFold(defs[0].TypeName, defs[1].TypeName) {
		t.Fatalf("both projected columns report type %q — the far leg's VAL was typed by "+
			"the near leg's descriptor (bare-name search picked first-match); "+
			"defs=%+v", defs[0].TypeName, defs)
	}
	if !strings.EqualFold(defs[0].TypeName, "BIGINT") {
		t.Errorf("A.VAL: got type %q, want BIGINT (LEFTT.VAL is declared BIGINT); defs=%+v",
			defs[0].TypeName, defs)
	}
	if !strings.EqualFold(defs[1].TypeName, "STRING") {
		t.Errorf("B.VAL: got type %q, want STRING (RIGHTT.VAL is declared STRING); defs=%+v",
			defs[1].TypeName, defs)
	}
}

// TestCrossLegSameNameDifferentType_DistinctDatumKeys pins deliverable (2):
// the two projected slots must carry DISTINCT ColumnDef.Name values. Name is
// the by-name datum lookup key and NewRecordLayerResultSet folds the defs
// into a name->index map, so equal Names collapse two slots into one and a
// by-name read serves the wrong leg's value.
func TestCrossLegSameNameDifferentType_DistinctDatumKeys(t *testing.T) {
	t.Parallel()
	md := buildCrossLegSameNameMetadata(t)
	plan, perr := embedded.PlanRecordQueryWithMetadata(
		`SELECT A.VAL, B.VAL FROM LEFTT A, RIGHTT B`, md, nil)
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	defs := embedded.ResultColumnDefsForPlan(plan, md)
	if len(defs) != 2 {
		t.Fatalf("got %d columns, want 2: %+v", len(defs), defs)
	}
	if strings.EqualFold(defs[0].Name, defs[1].Name) {
		t.Fatalf("both projected columns share datum key %q — the result set's "+
			"name->index map collapses them and a by-name read serves the wrong "+
			"leg's value; defs=%+v", defs[0].Name, defs)
	}
}

// TestCrossLegSameNameDifferentType_CorrelatedScalarSubquery is the shape that
// actually reproduced the wrong type. A correlated scalar subquery's projected
// slot flows no type of its own at the metadata surface: the projection reads
// it through a LAZY FieldValue, and the ordinal fold that would carry the
// seed's type declines on a lazy node (it composes by ordinal only). The type
// therefore has to come from the type-inheritance chain that reads the inner
// plan's own output.
//
// It did not, because the bare-name descriptor search answered FIRST and
// answered CONFIDENTLY: both legs declare VAL, no descriptor is named for the
// alias "B", so the search first-matched LEFTT and typed the STRING scalar
// BIGINT — and being non-empty, that answer suppressed the inheritance chain
// that would have read STRING off the inner plan. Java cannot reach this state
// at all: metadata is positional over the flowed record type
// (RelationalStructMetaData.getField is List.get(i)), never a name search.
//
// The sibling assertions above pass on both sides of the fix; THIS one is the
// sentinel. If it goes red, a name-keyed derivation has regained precedence
// over the flowed/inherited type.
func TestCrossLegSameNameDifferentType_CorrelatedScalarSubquery(t *testing.T) {
	t.Parallel()
	md := buildCrossLegSameNameMetadata(t)
	plan, perr := embedded.PlanRecordQueryWithMetadata(
		`SELECT A.VAL, (SELECT B.VAL FROM RIGHTT B WHERE B.RID = A.LID) FROM LEFTT A`, md, nil)
	if perr != nil {
		t.Fatalf("plan: %v", perr)
	}
	defs := embedded.ResultColumnDefsForPlan(plan, md)
	if len(defs) != 2 {
		t.Fatalf("got %d columns, want 2: %+v", len(defs), defs)
	}
	if !strings.EqualFold(defs[0].TypeName, "BIGINT") {
		t.Errorf("A.VAL: got type %q, want BIGINT; defs=%+v", defs[0].TypeName, defs)
	}
	// The correlated scalar selects RIGHTT.VAL, declared STRING. Reporting
	// BIGINT is the far leg's type, picked by the bare-name descriptor search.
	if !strings.EqualFold(defs[1].TypeName, "STRING") {
		t.Fatalf("correlated scalar subquery over RIGHTT.VAL: got type %q, want STRING — "+
			"the scalar's column was typed from the OTHER join leg's descriptor by a "+
			"bare-name search (both legs declare VAL; the alias qualifier matches no "+
			"descriptor), and that confident wrong answer preempted the flowed-type "+
			"inheritance; defs=%+v", defs[1].TypeName, defs)
	}
}
