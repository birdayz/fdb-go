package embedded

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/relational/api"
)

// The index generator's field-path trie against the root key expressions the
// live 4.14.2.0 JVM STORED for the same DDL (the WS-J oracle's hand-written
// shapes, conformance/ws_j_index_fidelity_conformance_test.go; the protos
// below are its WSJ-JAVA-ROOT lines verbatim). The dimension these pin is a
// run of adjacent field values that MIXES nested and top-level columns:
// FieldValueTrieNode.computeTrieForValues tests the WHOLE path against the
// prefix before descending, and Go tested only its LENGTH, so `s.x, ts` built
// field(S) with its children dropped — a wrong key expression that surfaced as
// XX000 from metadata validation for DDL Java accepts. The nested-only and
// top-then-nested shapes already agreed, which is why no test saw it.
func TestIndexDDLFieldTrieMatchesJavaStoredRoots(t *testing.T) {
	t.Parallel()
	const structT = `create type as struct sc(x bigint, y bigint) create table t(id bigint, s sc, ts bigint, primary key(id)) `
	const deepT = `create type as struct st_in(a bigint, b bigint) create type as struct st_out(i st_in, c bigint) ` +
		`create table t(id bigint, o st_out, primary key(id)) `
	cases := []struct {
		name, ddl, indexType, javaRoot string
	}{
		{
			"nested_then_top", structT + `create index ix as select s.x, ts from t order by s.x, ts`, "value",
			`then:{child:{nesting:{parent:{field_name:"S" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{field:{field_name:"X" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}} child:{field:{field_name:"TS" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}`,
		},
		{
			"top_then_nested", structT + `create index ix as select ts, s.x from t order by ts, s.x`, "value",
			`then:{child:{field:{field_name:"TS" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}} child:{nesting:{parent:{field_name:"S" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{field:{field_name:"X" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}}}`,
		},
		{
			"nested_siblings", structT + `create index ix as select s.x, s.y from t order by s.x, s.y`, "value",
			`nesting:{parent:{field_name:"S" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{then:{child:{field:{field_name:"X" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}} child:{field:{field_name:"Y" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}}}`,
		},
		{
			"nested_siblings_reordered", structT + `create index ix as select s.x, s.y from t order by s.y, s.x`, "value",
			`nesting:{parent:{field_name:"S" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{then:{child:{field:{field_name:"Y" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}} child:{field:{field_name:"X" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}}}`,
		},
		{
			"nested_covering", structT + `create index ix as select s.x, ts from t order by s.x`, "value",
			`key_with_value:{inner_key:{then:{child:{nesting:{parent:{field_name:"S" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{field:{field_name:"X" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}} child:{field:{field_name:"TS" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}} split_point:1}`,
		},
		{
			"nested_desc", structT + `create index ix as select s.x, ts from t order by s.x desc, ts`, "value",
			`then:{child:{nesting:{parent:{field_name:"S" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{function:{name:"order_desc_nulls_last" arguments:{field:{field_name:"X" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}}}} child:{field:{field_name:"TS" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}`,
		},
		{
			"nested_nulls_last", structT + `create index ix as select s.x, s.y from t order by s.x asc nulls last, s.y`, "value",
			`nesting:{parent:{field_name:"S" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{then:{child:{function:{name:"order_asc_nulls_last" arguments:{field:{field_name:"X" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}} child:{field:{field_name:"Y" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}}}`,
		},
		{
			"grouped_by_nested", structT + `create index ix as select s.x, count(*) from t group by s.x`, "count",
			`grouping:{whole_key:{nesting:{parent:{field_name:"S" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{field:{field_name:"X" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}} grouped_count:0}`,
		},
		{
			"deep_nesting_shared", deepT + `create index ix as select o.i.a, o.i.b, o.c from t order by o.i.a, o.i.b, o.c`, "value",
			`nesting:{parent:{field_name:"O" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{then:{child:{nesting:{parent:{field_name:"I" fan_type:SCALAR nullInterpretation:NOT_UNIQUE} child:{then:{child:{field:{field_name:"A" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}} child:{field:{field_name:"B" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}}}} child:{field:{field_name:"C" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}}}}}`,
		},
		{
			"literal_long_arith", `create table t(id bigint, d bigint, primary key(id)) create index ix as select d + 3000000000 from t order by d + 3000000000`, "value",
			`function:{name:"add" arguments:{then:{child:{field:{field_name:"D" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}} child:{value:{long_value:3000000000}}}}}`,
		},
		{
			"literal_double_arith", `create table t(id bigint, c double, primary key(id)) create index ix as select c * 1.5 from t order by c * 1.5`, "value",
			`function:{name:"mul" arguments:{then:{child:{field:{field_name:"C" fan_type:SCALAR nullInterpretation:NOT_UNIQUE}} child:{value:{double_value:1.5}}}}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := BuildSchemaTemplateFromDDL(c.ddl)
			if err != nil {
				t.Fatalf("Java 4.14.2.0 accepts this DDL; Go rejected it: %v", err)
			}
			md, err := tmpl.Underlying().ToProto()
			if err != nil {
				t.Fatalf("ToProto: %v", err)
			}
			var ix *gen.Index
			for _, i := range md.GetIndexes() {
				if i.GetName() == "IX" {
					ix = i
				}
			}
			if ix == nil {
				t.Fatalf("no index IX in %v", md.GetIndexes())
			}
			want := &gen.KeyExpression{}
			if err := prototext.Unmarshal([]byte(c.javaRoot), want); err != nil {
				t.Fatalf("parse Java root: %v", err)
			}
			if !proto.Equal(ix.GetRootExpression(), want) {
				t.Fatalf("root diverges from Java's stored root:\n  go:   %v\n  java: %v", ix.GetRootExpression(), want)
			}
			if ix.GetType() != c.indexType {
				t.Fatalf("type %q, Java stored %q", ix.GetType(), c.indexType)
			}
		})
	}
}

// The two shapes Java REJECTS with 0A000: a parent referenced by two runs that
// are not adjacent, at the top level and one nesting level down. The trie
// fix reorders the prefix and length checks; it must not turn either rejection
// into an accepted index.
func TestIndexDDLFieldTrieRejectsDisconnectedReferences(t *testing.T) {
	t.Parallel()
	for _, ddl := range []string{
		`create type as struct sc(x bigint, y bigint) create table t(id bigint, s sc, ts bigint, primary key(id)) ` +
			`create index ix as select s.x, ts, s.y from t order by s.x, ts, s.y`,
		`create type as struct st_in(a bigint, b bigint) create type as struct st_out(i st_in, c bigint) ` +
			`create table t(id bigint, o st_out, primary key(id)) ` +
			`create index ix as select o.i.a, o.c, o.i.b from t order by o.i.a, o.c, o.i.b`,
	} {
		_, err := BuildSchemaTemplateFromDDL(ddl)
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrCodeUnsupportedOperation {
			t.Fatalf("%s: want 0A000 as Java 4.14.2.0 answers, got %v", ddl, err)
		}
	}
}
