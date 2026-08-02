package embedded

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// Quoted-identifier DDL preserves case in the descriptor ("col1" stays
// col1), while Go's semantic accessors present the FOLDED display name
// (COL1) — the runtime positional namespace. The generator must render every
// key-expression and predicate field name from the DESCRIPTOR by accessor
// ordinal (storageNames), matching Java, whose ResolvedAccessor carries the
// Type.Record.Field itself so getFieldPathNames always yields storage names.
//
// The defect this pins: rendering acc.Field emitted field("COL1") for a
// quoted "col1" column, and metadata validation rejected the template with
// `field "COL1" not found in message "T"` — boolean-ddl.yamsql and
// filter-index.yamsql died there. It was latent through S2-S4 because every
// quoted-column AS-SELECT carrier in the corpus also has a WHERE clause,
// which failed closed until the predicate arm (S5) landed.
func TestAsSelectIndex_QuotedColumnsKeepDescriptorCase(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(`
		CREATE TABLE T("col1" bigint, "name" string, "id" bigint, PRIMARY KEY("id"))
		CREATE INDEX i1 AS SELECT "col1" FROM T ORDER BY "col1"
		CREATE INDEX i2 AS SELECT "name", "id" FROM T WHERE "name" = 'bar' OR "name" = 'foo' ORDER BY "name", "id"
		CREATE INDEX i3 AS SELECT "name", SUM("col1") FROM T GROUP BY "name"`)
	if err != nil {
		t.Fatalf("quoted-column AS-SELECT DDL failed: %v", err)
	}
	md := tmpl.Underlying()

	i1 := md.GetIndex("I1")
	if i1 == nil {
		t.Fatal("I1 not built")
	}
	if want := recordlayer.Field("col1"); !proto.Equal(i1.RootExpression.ToKeyExpression(), want.ToKeyExpression()) {
		t.Errorf("I1 root = %v, want field(col1) — the descriptor's exact case", i1.RootExpression.ToKeyExpression())
	}

	// filter-index.yamsql i1's shape: quoted columns in key AND predicate.
	i2 := md.GetIndex("I2")
	if i2 == nil {
		t.Fatal("I2 not built")
	}
	wantRoot := recordlayer.Concat(recordlayer.Field("name"), recordlayer.Field("id"))
	if !proto.Equal(i2.RootExpression.ToKeyExpression(), wantRoot.ToKeyExpression()) {
		t.Errorf("I2 root = %v, want concat(name, id)", i2.RootExpression.ToKeyExpression())
	}
	pred := i2.GetPredicateProto()
	if pred == nil {
		t.Fatal("I2 predicate missing")
	}
	or := pred.GetOrPredicate()
	if or == nil || len(or.GetChildren()) != 2 {
		t.Fatalf("I2 predicate shape = %v, want OR of two", pred)
	}
	for i, c := range or.GetChildren() {
		vp := c.GetValuePredicate()
		if vp == nil || len(vp.GetValue()) != 1 || vp.GetValue()[0] != "name" {
			t.Errorf("I2 predicate disjunct %d field path = %v, want [name] — the STORAGE name, "+
				"exactly what Java's ValuePredicate stores (FieldValue.getFieldPathNames)", i, vp.GetValue())
		}
	}

	// The aggregate arm renders through the same helpers: grouping and
	// operand names must be storage names too.
	i3 := md.GetIndex("I3")
	if i3 == nil {
		t.Fatal("I3 not built")
	}
	wantAgg := recordlayer.GroupBy(recordlayer.Field("col1"), recordlayer.Field("name"))
	if !proto.Equal(i3.RootExpression.ToKeyExpression(), wantAgg.ToKeyExpression()) {
		t.Errorf("I3 root = %v, want group_by(col1; name)", i3.RootExpression.ToKeyExpression())
	}
}
