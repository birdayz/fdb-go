package embedded

import (
	"strings"
	"testing"
)

// A schema template may not declare a table and an auxiliary type under the
// same name: Java's Builder.addTable and Builder.addAuxiliaryType BOTH call
// verifyNameIsNotUsed (RecordLayerSchemaTemplate.java:465, :553, :712-717),
// so the collision is rejected whichever clause is seen first. Without the
// reciprocal check on the table side the template built two descriptors
// named alike, one silently shadowing the other.
func TestDDL_TableTypeNameCollision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ddl  string
		want string
	}{
		{
			// TYPE first: the table pass must see the registered type.
			// This is the direction the struct pass ordering exposed —
			// struct clauses are always registered before tables, so only
			// the table-side check can catch it.
			name: "type declared first",
			ddl: `CREATE TYPE AS STRUCT s (a BIGINT)
			      CREATE TABLE s (id BIGINT, PRIMARY KEY (id))`,
			want: "type with name 'S' already exists",
		},
		{
			// TABLE first in DDL TEXT — the struct pass still runs first,
			// so this also lands on the table-side check.
			name: "table declared first",
			ddl: `CREATE TABLE s (id BIGINT, PRIMARY KEY (id))
			      CREATE TYPE AS STRUCT s (a BIGINT)`,
			want: "type with name 'S' already exists",
		},
		{
			// Two tables of the same name: the table-side check catches
			// its own kind too (Java's verifyNameIsNotUsed scans tables
			// before auxiliary types).
			name: "duplicate table",
			ddl: `CREATE TABLE t (id BIGINT, PRIMARY KEY (id))
			      CREATE TABLE t (id BIGINT, PRIMARY KEY (id))`,
			want: "table with name 'T' already exists",
		},
		{
			// Two structs of the same name: the pre-existing
			// AddAuxiliaryType direction, pinned alongside so a
			// refactor cannot drop one half of the pair.
			name: "duplicate struct",
			ddl: `CREATE TYPE AS STRUCT s (a BIGINT)
			      CREATE TYPE AS STRUCT s (b BIGINT)
			      CREATE TABLE t (id BIGINT, PRIMARY KEY (id))`,
			want: "type with name 'S' already exists",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := buildSchemaTemplateFromDDL(tc.ddl)
			if err == nil {
				t.Fatalf("collision accepted, template built: %v", tmpl)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
			// INVALID_SCHEMA_TEMPLATE, Java's ErrorCode on
			// verifyNameIsNotUsed's Assert.thatUnchecked.
			if !strings.Contains(err.Error(), "42F59") {
				t.Fatalf("error = %q, want SQLSTATE 42F59", err.Error())
			}
		})
	}
}

// A name shared between a table and a type is only rejected when they
// actually collide — a struct and a table with DISTINCT names still build,
// and a table's own name remains usable as a column type (Builder.findType
// checks tables first).
func TestDDL_TableTypeNamesDoNotFalselyCollide(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TYPE AS STRUCT s (a BIGINT)
		 CREATE TABLE t (id BIGINT, v s, PRIMARY KEY (id))
		 CREATE TABLE u (id BIGINT, w t, PRIMARY KEY (id))`)
	if err != nil {
		t.Fatalf("distinct names must build: %v", err)
	}
	if tmpl.Underlying().GetRecordType("T") == nil || tmpl.Underlying().GetRecordType("U") == nil {
		t.Fatalf("both tables must exist")
	}
}
