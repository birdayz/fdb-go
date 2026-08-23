package embedded

import (
	"strings"
	"testing"
)

// CREATE INDEX resolves its ON-source table through GetRecordType, which retries
// a miss through the protobuf escaping -- that is what lets an index be created
// on a table whose name escapes. The retry is gated on the source being a SINGLE
// identifier, and that gate is the whole point of this file.
//
// `tableName` is built from GetText(), which concatenates a multi-segment FullId
// into `S.T` with no separator information left. That string is
// indistinguishable from the quoted identifier `"S.T"`, whose storage name is
// `S__2T`. Ungated, a SCHEMA-QUALIFIED `ON S.T` would escape to `S__2T`, find a
// table declared `"S.T"`, and attach the index -- and any UNIQUE constraint with
// it -- to a record type the statement never named. The gate reads the parse
// tree (len(AllUid()) > 1) rather than looking for a dot in the text, because a
// dot in the text is exactly the ambiguity being resolved.
func TestDDL_IndexOnSourceResolvesEscapedTableButNotAQualifier(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		ddl     string
		wantErr string // empty means the template must build
	}{
		{
			// THE FIX. Before it, a raw map lookup by the SQL identifier
			// missed the stored MY__1TABLE and an escaped table could not be
			// given a secondary index at all.
			name: "escaped table name resolves",
			ddl: `CREATE TABLE "MY$TABLE" (id BIGINT, name STRING, PRIMARY KEY (id))
			      CREATE INDEX idx_name ON "MY$TABLE" (name)`,
		},
		{
			// THE GATE. `S.T` here is a schema qualifier, and the table
			// declared as the quoted identifier "S.T" is a different thing
			// that happens to escape to the same storage name. Attaching to
			// it silently is the hazard; failing is correct.
			name: "qualifier is not escaped onto a quoted dotted table",
			ddl: `CREATE TABLE "S.T" (id BIGINT, PRIMARY KEY (id))
			      CREATE TABLE t (id BIGINT, PRIMARY KEY (id))
			      CREATE INDEX idx_q ON S.T (id)`,
			wantErr: `references unknown table "S.T"`,
		},
		{
			// The same qualifier with NO quoted dotted table present. It has
			// always failed and must keep failing for the same reason --
			// otherwise the arm above passes because nothing could match,
			// rather than because the gate held.
			name: "qualifier fails with no dotted table to land on",
			ddl: `CREATE TABLE t (id BIGINT, PRIMARY KEY (id))
			      CREATE INDEX idx_q ON S.T (id)`,
			wantErr: `references unknown table "S.T"`,
		},
		{
			// The ordinary single-segment case, so a regression that broke
			// the gate open in the other direction -- refusing every source
			// -- is caught here rather than only in the corpus.
			name: "plain table name still resolves",
			ddl: `CREATE TABLE t (id BIGINT, v BIGINT, PRIMARY KEY (id))
			      CREATE INDEX idx_v ON t (v)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := buildSchemaTemplateFromDDL(tc.ddl)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("template must build: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("qualified source accepted, template built: %v.\n"+
					"A schema-qualified ON-source must not be flattened to `S.T` and then\n"+
					"escaped: it resolves onto a table declared as the quoted identifier\n"+
					"\"S.T\" and attaches the index to the wrong record type.", tmpl)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
