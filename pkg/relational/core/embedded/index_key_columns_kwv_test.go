package embedded

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
)

// RFC-202 D10(a): the flat candidate bridge's column list must respect the
// KeyWithValue split. The negative pin is built from JAVA-AUTHORED metadata
// BYTES (a KeyWithValue proto deserialized through KeyExpressionFromProto —
// key_expression_proto.go's keyWithValueFromProto), not from a hand-built Go
// expression: a Go process reading a Java-written covering index reaches
// IndexColumnNames through exactly this path, and before the fix BOTH arms —
// the structured walk AND the FieldNames fallback — reported every VALUE
// column as a key column. Sargable aliases and scan-prefix positions past the
// split would then address entry columns living in the FDB VALUE.
func javaAuthoredKWVIndex(t *testing.T, split int32) *recordlayer.Index {
	t.Helper()
	field := func(name string) *gen.KeyExpression {
		fan := gen.Field_SCALAR
		return &gen.KeyExpression{Field: &gen.Field{FieldName: proto.String(name), FanType: &fan}}
	}
	kwvProto := &gen.KeyExpression{KeyWithValue: &gen.KeyWithValue{
		InnerKey: &gen.KeyExpression{Then: &gen.Then{
			Child: []*gen.KeyExpression{field("A"), field("B"), field("C")},
		}},
		SplitPoint: proto.Int32(split),
	}}
	root, err := recordlayer.KeyExpressionFromProto(kwvProto)
	if err != nil {
		t.Fatalf("KeyExpressionFromProto: %v", err)
	}
	return recordlayer.NewIndex("KWV_IDX", root)
}

// kwvTestMetadata builds a metadata the KWV index's record type would live in;
// the index itself is deliberately NOT registered (the def under test carries
// it directly, the way NewPlanContextFromIndexDefs receives Java-read
// metadata defs).
func kwvTestMetadata(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE T (id bigint, a bigint, b bigint, c bigint, PRIMARY KEY(id))`)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	return tmpl.Underlying()
}

func TestJavaAuthoredKeyWithValue_IndexColumnNamesRespectSplit(t *testing.T) {
	t.Parallel()
	def := &metadataIndexDef{idx: javaAuthoredKWVIndex(t, 1), md: kwvTestMetadata(t)}

	cols := def.IndexColumnNames()
	if len(cols) != 1 || cols[0] != "A" {
		t.Errorf("IndexColumnNames = %v, want [A] — the split point is the key/value "+
			"boundary; value columns reported as key columns is the D10(a) wrong-column-set defect", cols)
	}
	// The STRUCTURED arm independently: the guarded path and the FieldNames
	// fallback back each other up inside IndexColumnNames, so a mutation of
	// one arm alone is compensated there — this direct assertion is what
	// turns a structured-arm regression red on its own.
	structNames, ok := indexKeyColumnNames(def.idx.RootExpression.ToKeyExpression())
	if !ok || len(structNames) != 1 || structNames[0] != "A" {
		t.Errorf("indexKeyColumnNames = %v (ok=%v), want [A] — the structured walk must truncate at the split", structNames, ok)
	}
	valueCols := def.IndexValueColumnNames()
	if len(valueCols) != 2 || valueCols[0] != "B" || valueCols[1] != "C" {
		t.Errorf("IndexValueColumnNames = %v, want [B C]", valueCols)
	}

	// The parallel-list contract: names length == ColumnSize (the split).
	if got, want := len(cols), def.idx.RootExpression.ColumnSize(); got != want {
		t.Errorf("len(IndexColumnNames) = %d, ColumnSize = %d — must stay parallel", got, want)
	}

	// Split at the full width degenerates to a plain index: all columns are
	// key columns, no value part.
	full := &metadataIndexDef{idx: javaAuthoredKWVIndex(t, 3), md: kwvTestMetadata(t)}
	if cols := full.IndexColumnNames(); len(cols) != 3 {
		t.Errorf("split=3 IndexColumnNames = %v, want all three", cols)
	}
	if valueCols := full.IndexValueColumnNames(); len(valueCols) != 0 {
		t.Errorf("split=3 IndexValueColumnNames = %v, want empty", valueCols)
	}
}

// TestJavaAuthoredKeyWithValue_CandidatePlansWithSplit: the same Java-authored
// root produces a candidate whose sargable surface is the key part and whose
// covering surface includes the value part — the candidate ADMITS (it used to
// decline: keyExpressionFlatColumnDescriptors had no KeyWithValue arm, "safe
// but not planned").
func TestJavaAuthoredKeyWithValue_CandidatePlansWithSplit(t *testing.T) {
	t.Parallel()
	def := &metadataIndexDef{idx: javaAuthoredKWVIndex(t, 1), md: kwvTestMetadata(t)}
	ctx := cascades.NewPlanContextFromIndexDefs([]cascades.IndexDef{def})
	cands := ctx.GetMatchCandidates()
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	cand, ok := cands[0].(*cascades.ValueIndexScanMatchCandidate)
	if !ok {
		t.Fatalf("candidate is %T", cands[0])
	}
	if got := cand.GetColumnNames(); len(got) != 1 || got[0] != "A" {
		t.Errorf("candidate key columns = %v, want [A]", got)
	}
	if got := cand.GetValueColumnNames(); len(got) != 2 || got[0] != "B" || got[1] != "C" {
		t.Errorf("candidate value columns = %v, want [B C]", got)
	}
	if cand.GetTraversal() == nil {
		t.Error("candidate declined to expand — a Java-authored covering index must be PLANNABLE (RFC-202 D10(a))")
	}
}
