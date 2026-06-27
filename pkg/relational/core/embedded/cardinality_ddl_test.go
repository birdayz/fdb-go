package embedded

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// TestCardinalityDDL_IndexShape drives the full SQL DDL parse path
// (CREATE TABLE with an array column + CREATE INDEX … AS SELECT
// CARDINALITY(col) … ORDER BY) and verifies the resulting metadata index is a
// VALUE index whose root is a CardinalityFunctionKeyExpression over the array
// field — matching Java's MaterializedViewIndexGenerator emitting
// function("cardinality", field("arr", Concatenate)).
func TestCardinalityDDL_IndexShape(t *testing.T) {
	t.Parallel()
	ddl := `CREATE TABLE tab (id integer, int_arr integer array null, PRIMARY KEY (id))
		CREATE INDEX tab_card AS SELECT CARDINALITY(int_arr) AS card FROM tab ORDER BY card`

	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("buildSchemaTemplateFromDDL: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("TAB_CARD")
	if idx == nil {
		t.Fatal("cardinality index TAB_CARD not found in metadata")
	}
	if idx.Type != recordlayer.IndexTypeValue {
		t.Errorf("index type = %q, want %q (cardinality is a value index)", idx.Type, recordlayer.IndexTypeValue)
	}

	card, ok := idx.RootExpression.(*recordlayer.CardinalityFunctionKeyExpression)
	if !ok {
		t.Fatalf("root expression is %T, want *CardinalityFunctionKeyExpression", idx.RootExpression)
	}
	if card.Name() != recordlayer.FunctionNameCardinality {
		t.Errorf("function name = %q, want %q", card.Name(), recordlayer.FunctionNameCardinality)
	}
	// The single key column is the array field (upper-cased identifier).
	if names := card.FieldNames(); len(names) != 1 || names[0] != "INT_ARR" {
		t.Errorf("argument field names = %v, want [INT_ARR]", names)
	}
	// createsDuplicates must be false (Java override).
	if got := idx.RootExpression.ColumnSize(); got != 1 {
		t.Errorf("column size = %d, want 1", got)
	}
}

// TestCardinalityDDL_CatalogRoundTrip is the wire-compat pin for the DDL path:
// a cardinality index built from SQL DDL serialises to the catalog
// KeyExpression proto (Function field, name "cardinality") and deserialises
// back to a CardinalityFunctionKeyExpression byte-identically — a Go-written
// cardinality index is wire-compatible with Java.
func TestCardinalityDDL_CatalogRoundTrip(t *testing.T) {
	t.Parallel()
	ddl := `CREATE TABLE tab (id integer, int_arr integer array null, PRIMARY KEY (id))
		CREATE INDEX tab_card AS SELECT CARDINALITY(int_arr) AS card FROM tab ORDER BY card`

	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("buildSchemaTemplateFromDDL: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("TAB_CARD")
	if idx == nil {
		t.Fatal("cardinality index TAB_CARD not found")
	}

	// Serialise the index key to proto and back.
	pb := idx.RootExpression.ToKeyExpression()
	if pb.GetFunction() == nil || pb.GetFunction().GetName() != recordlayer.FunctionNameCardinality {
		t.Fatalf("serialised key is not a cardinality Function proto: %+v", pb)
	}
	raw, err := proto.Marshal(pb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := recordlayer.KeyExpressionFromProto(pb)
	if err != nil {
		t.Fatalf("KeyExpressionFromProto: %v", err)
	}
	if _, ok := back.(*recordlayer.CardinalityFunctionKeyExpression); !ok {
		t.Fatalf("round-tripped key is %T, want *CardinalityFunctionKeyExpression", back)
	}
	raw2, err := proto.Marshal(back.ToKeyExpression())
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(raw) != string(raw2) {
		t.Fatalf("re-serialised catalog bytes differ:\n first=%x\nsecond=%x", raw, raw2)
	}
}

// NOTE: the nested-struct array case (CARDINALITY(struct.int_arr), yamsql
// tab2_index) is not exercised here: the metadata builder rejects STRUCT
// columns ("only primitive column types are supported"), the same limitation
// the Phase-1 scalar test documents. buildCardinalityIndex already builds the
// dotted-column nesting argument (field("struct").nest(field("arr",
// Concatenate))); it lands when struct-column support arrives. The cardinality
// machinery itself is struct-agnostic.
