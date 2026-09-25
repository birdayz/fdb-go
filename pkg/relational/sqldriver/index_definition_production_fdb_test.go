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
	"fdb.dev/pkg/relational/core/embedded"
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

// One DDL front end (RFC-257 WS-J section 3.4): the template a CREATE SCHEMA
// TEMPLATE executed by the driver stores is, byte for byte, the one the tooling
// path (embedded.BuildSchemaTemplateFromDDLNamed, which the planner harness and
// the conformance oracle use) builds from the same text. Every clause kind the
// builder reads is present: WITH OPTIONS, a struct, tables, on-source,
// as-select and aggregate indexes, index options and a vector index.
func TestFDB_ExecutedTemplateIsTheToolingPathsTemplate(t *testing.T) {
	t.Parallel()
	setup := openTestDB(t, "/__SYS")
	ctx := context.Background()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "WSJONE_" + hex.EncodeToString(suffix)
	body := "CREATE TYPE AS STRUCT sc (x BIGINT, y STRING) " +
		"CREATE TABLE t (id BIGINT, s sc, a BIGINT, b STRING, g BIGINT, v BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE u (k STRING, w DOUBLE, e VECTOR(3, FLOAT), PRIMARY KEY (k)) " +
		"CREATE INDEX t_ab ON t (a DESC, b) " +
		"CREATE UNIQUE INDEX t_b AS SELECT b FROM t ORDER BY b " +
		"CREATE INDEX t_sx AS SELECT s.x, a FROM t ORDER BY s.x, a " +
		"CREATE INDEX t_sum AS SELECT sum(v) FROM t GROUP BY g " +
		"CREATE INDEX t_cnt AS SELECT count(*) FROM t GROUP BY g " +
		"CREATE INDEX t_ap1 AS SELECT a + 1 FROM t ORDER BY a + 1 " +
		"CREATE VECTOR INDEX u_e USING HNSW ON u (e) OPTIONS (METRIC = EUCLIDEAN_METRIC)"
	ddl := "CREATE SCHEMA TEMPLATE " + name + " " + body + " WITH OPTIONS (STORE_ROW_VERSIONS = true)"
	built, err := embedded.BuildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("tooling path: %v", err)
	}
	want, err := built.Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	mwjoMustExec(t, setup, ctx, ddl)
	t.Cleanup(func() { _, _ = setup.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+name) })

	h := newEvolHarness(t)
	h.mustRun(t, "load the stored template's bytes", func(txn api.Transaction) error {
		stored, err := h.cat.SchemaTemplateCatalog().LoadTemplateProto(txn, name, built.Version())
		if err != nil {
			return err
		}
		if !proto.Equal(stored, want) {
			t.Errorf("the executed CREATE stored another template than the tooling path builds:\n stored %v\n built  %v",
				prototext.Format(stored), prototext.Format(want))
		}
		if len(stored.GetIndexes()) != 7 {
			t.Errorf("stored %d indexes, want the 7 the DDL declares", len(stored.GetIndexes()))
		}
		return nil
	})
}
