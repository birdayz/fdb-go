package embedded

import (
	"strings"
	"testing"
)

// A PRIMARY KEY part is a LIST of parse-tree uid segments, never a joined
// dotted string that gets re-split — Java carries List<String> from
// Identifier.fullyQualifiedName all the way into
// RecordLayerTable.Builder.toKeyExpression (DdlVisitor.java:183-188,
// RecordLayerTable.java:295, :313-329). A quoted column name may itself
// contain a literal '.', so re-splitting turned the valid single-segment
// key "a.b" into the two-segment nested path a→b: the column lookup then
// failed on the nonexistent head column A with 42703.
func TestDDL_QuotedDotColumnAsPrimaryKey(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t (id BIGINT, "a.b" BIGINT, PRIMARY KEY ("a.b"))`)
	if err != nil {
		t.Fatalf("quoted-dot column as primary key must build: %v", err)
	}
	ke := tmpl.Underlying().GetRecordType("T").PrimaryKey.ToKeyExpression()
	then := ke.GetThen()
	if then == nil || len(then.GetChild()) != 2 {
		t.Fatalf("pk = %v, want then(recordType, field(a__2b))", ke)
	}
	part := then.GetChild()[1]
	if part.GetNesting() != nil {
		t.Fatalf("pk part = %v, want a FLAT field — the quoted dot was split into a nested path", part)
	}
	// ProtoUtils escaping: '.' -> "__2" (protoname.dotEscape), the WIRE
	// storage name of the column.
	if got := part.GetField().GetFieldName(); got != "a__2b" {
		t.Fatalf("pk field name = %q, want %q", got, "a__2b")
	}
}

// The nested primary key the segment list exists for still works: two
// segments really do descend into the struct column.
func TestDDL_NestedPrimaryKeyStillNests(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TYPE AS STRUCT s (a BIGINT)
		 CREATE TABLE t (id s, PRIMARY KEY (id.a))`)
	if err != nil {
		t.Fatalf("nested primary key must build: %v", err)
	}
	ke := tmpl.Underlying().GetRecordType("T").PrimaryKey.ToKeyExpression()
	then := ke.GetThen()
	if then == nil || len(then.GetChild()) != 2 {
		t.Fatalf("pk = %v, want then(recordType, nest(ID.A))", ke)
	}
	nest := then.GetChild()[1].GetNesting()
	if nest == nil || nest.GetParent().GetFieldName() != "ID" ||
		nest.GetChild().GetField().GetFieldName() != "A" {
		t.Fatalf("pk part = %v, want field(ID).nest(field(A))", then.GetChild()[1])
	}
}

// The 42703 guard is not weakened by the segment change: a genuinely
// undefined head column is still rejected before the metadata builder
// leaks an internal error, and the message reports the FULL dotted path.
func TestDDL_PrimaryKeyOverUndefinedColumnStillRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, ddl, want string }{
		{
			"flat", `CREATE TABLE t (id BIGINT, PRIMARY KEY (nope))`,
			`primary key column "NOPE" is not a defined column`,
		},
		{
			"nested head", `CREATE TYPE AS STRUCT s (a BIGINT)
			CREATE TABLE t (id s, PRIMARY KEY (nope.a))`,
			`primary key column "NOPE.A" is not a defined column`,
		},
		{
			"quoted dot not a column", `CREATE TABLE t (id BIGINT, PRIMARY KEY ("a.b"))`,
			`primary key column "a.b" is not a defined column`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildSchemaTemplateFromDDL(tc.ddl)
			if err == nil {
				t.Fatalf("undefined primary key column accepted")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "42703") {
				t.Fatalf("error = %q, want 42703 and %q", err.Error(), tc.want)
			}
		})
	}
}
