package sqldriver_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"maps"
	"slices"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// The index-definition fixes of RFC-257 WS-J measured end to end through the
// PRODUCTION DDL path: a CREATE SCHEMA TEMPLATE statement executed by the SQL
// driver (execCreateSchemaTemplate), the template it stores read back from the
// driver's own catalog. The unit goldens and the WS-J oracle build templates
// through the plan-harness front end; this is the path a user's DDL takes.
//
// Each expected root is the target's, as the WS-J oracle measured it
// (WSJ-JAVA-ROOT lines of shapes nested_then_top, literal_int_arith and
// bitmap_bucket_offset_value, Java 4.14.2.0):
//   - the field-path trie keeps a nested leaf when a top-level column follows it;
//   - an INT literal is stored as int_value, and the bitmap entry size as the
//     INT 10000;
//   - a permuted aggregate index stores its options as [unique, permutedSize].
func TestFDB_IndexDefinitionProductionPathStoresTargetShapes(t *testing.T) {
	t.Parallel()
	setup := openTestDB(t, "/__SYS")
	ctx := context.Background()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "WSJPROD_" + hex.EncodeToString(suffix)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE "+name+" "+
		"CREATE TYPE AS STRUCT sc (x BIGINT, y BIGINT) "+
		"CREATE TABLE t (id BIGINT, s sc, ts BIGINT, d BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE INDEX nested_then_top AS SELECT s.x, ts FROM t ORDER BY s.x, ts "+
		"CREATE INDEX dplus AS SELECT d + 1 FROM t ORDER BY d + 1 "+
		"CREATE INDEX bucket AS SELECT bitmap_bucket_offset(id) FROM t ORDER BY bitmap_bucket_offset(id) "+
		"CREATE INDEX mx AS SELECT max(v) FROM t GROUP BY g")
	t.Cleanup(func() { _, _ = setup.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+name) })

	h := newEvolHarness(t)
	var md *gen.MetaData
	h.mustRun(t, "load the stored template", func(txn api.Transaction) error {
		tmpl, err := h.cat.SchemaTemplateCatalog().LoadSchemaTemplate(txn, name)
		if err != nil {
			return err
		}
		md, err = tmpl.(*metadata.RecordLayerSchemaTemplate).Underlying().ToProto()
		return err
	})
	byName := map[string]*gen.Index{}
	for _, ix := range md.GetIndexes() {
		byName[ix.GetName()] = ix
	}
	roots := map[string]string{
		"NESTED_THEN_TOP": `then:{child:{nesting:{parent:{field_name:"S" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{field:{field_name:"X" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}} child:{field:{field_name:"TS" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}`,
		"DPLUS":           `function:{name:"add" arguments:{then:{child:{field:{field_name:"D" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}} child:{value:{int_value:1}}}}}`,
		"BUCKET":          `function:{name:"bitmap_bucket_offset" arguments:{then:{child:{field:{field_name:"ID" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}} child:{value:{int_value:10000}}}}}`,
	}
	for ixName, text := range roots {
		ix, ok := byName[ixName]
		if !ok {
			t.Fatalf("index %s not stored; stored: %v", ixName, slices.Sorted(maps.Keys(byName)))
		}
		want := &gen.KeyExpression{}
		if err := prototext.Unmarshal([]byte(text), want); err != nil {
			t.Fatalf("%s: bad expected root: %v", ixName, err)
		}
		if !proto.Equal(ix.GetRootExpression(), want) {
			t.Errorf("%s stores root\n  %v\nwant the target's\n  %v", ixName, ix.GetRootExpression(), want)
		}
	}
	mx, ok := byName["MX"]
	if !ok {
		t.Fatal("index MX not stored")
	}
	var keys []string
	for _, o := range mx.GetOptions() {
		keys = append(keys, o.GetKey())
	}
	if !slices.Equal(keys, []string{"unique", "permutedSize"}) {
		t.Errorf("MX stores options %v, want the target's [unique permutedSize]", keys)
	}
}
