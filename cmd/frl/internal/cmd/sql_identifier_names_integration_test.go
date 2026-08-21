package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `frl sql`'s \d PRINTS SQL IDENTIFIERS AND ACCEPTS THEM BACK.
//
// A table quoted "MY$TABLE" is stored as MY__1TABLE, and \d read that stored
// name straight out of the catalog — the same defect the rest of the CLI
// carried, in the one command whose entire job is to speak SQL. It listed a
// name the operator could not type, and its "not found — available:" offered
// that same untypeable spelling while its twin in openstore.go offered SQL
// identifiers.
//
// Driven against a REAL store rather than a renderer, because the naming sits
// behind a live catalog lookup. It calls loadSchemaTables/describeTable rather
// than the `\d` line itself: meta-commands are dispatched only from the
// interactive REPL (runFile splits on semicolons and goes straight to
// execute()), so the `\d` string cannot be reached without a PTY. What runMeta
// adds over these two calls is `strings.Fields` routing.
//
// NOTE, found while building this fixture and NOT fixed here: the schema has to
// be spelled MAIN, because the engine uppercases unquoted identifiers at DDL
// time -- yet `frl sql --schema main` connects happily, because the DSN path
// normalises and the meta-commands do not. So SELECT works with a lowercase
// --schema while \d reports "schema does not exist". That is a case-
// normalisation bug in the REPL, separate from the namespace work here, and it
// is why this test hard-codes the stored spelling rather than the one an
// operator would type.
func TestIntegration_SQL_DescribeUsesSQLIdentifiers(t *testing.T) {
	bindConfig(t)

	const sqlName, storageName = "MY$TABLE", "MY__1TABLE"
	const sqlCol, storageCol = "COL$X", "COL__1X"

	schema := `
CREATE DATABASE /frlnames;

CREATE SCHEMA TEMPLATE frlnames_tpl
CREATE TABLE "MY$TABLE" (
  id BIGINT,
  "COL$X" STRING,
  PRIMARY KEY ("COL$X")
);

CREATE SCHEMA /frlnames/main WITH TEMPLATE frlnames_tpl;
`
	path := filepath.Join(t.TempDir(), "names.sql")
	if err := os.WriteFile(path, []byte(schema), 0o600); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	if out, err := runCmd(t, "sql", "--database", "/frlnames", "--schema", "main", "-f", path); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, out)
	}

	var buf bytes.Buffer
	r := &sqlRunner{
		out:         &buf,
		errOut:      &buf,
		ctx:         context.Background(),
		clusterFile: fixture.clusterFilePath,
		database:    "/frlnames",
		schema:      "MAIN", // the engine uppercases unquoted identifiers at DDL time
		st:          plainSQLStyles(),
		format:      sqlFormatTable,
	}

	// \d — the table listing.
	tables, err := r.loadSchemaTables("/frlnames", "MAIN")
	if err != nil {
		t.Fatalf("loadSchemaTables: %v", err)
	}
	if len(tables) == 0 {
		t.Fatal("fixture did not land: no tables, so every assertion below is vacuous")
	}
	var names []string
	for _, tb := range tables {
		names = append(names, tb.name)
	}
	joined := strings.Join(names, ", ")
	if !strings.Contains(joined, sqlName) {
		t.Errorf("\\d does not list the table by its SQL identifier %q: %s", sqlName, joined)
	}
	if strings.Contains(joined, storageName) {
		t.Errorf("\\d leaks the storage name %q — an operator cannot type it: %s", storageName, joined)
	}

	// \d <table>, addressed by the name the listing just printed. This is the
	// half a render-only fix breaks: decode the listing but match only the
	// stored form and the command rejects the name it displayed.
	buf.Reset()
	if err := r.describeTable(sqlName); err != nil {
		t.Fatalf("describeTable(%q) failed — the listing offers a name the lookup "+
			"rejects: %v\n%s", sqlName, err, buf.String())
	}
	desc := buf.String()
	if strings.Contains(desc, storageName) {
		t.Errorf("describeTable(%q) leaks the storage table name:\n%s", sqlName, desc)
	}
	// Column names are proto FIELD names, escaped the same way.
	if !strings.Contains(desc, sqlCol) {
		t.Errorf("describeTable(%q) does not name the column by its SQL identifier %q:\n%s",
			sqlName, sqlCol, desc)
	}
	if strings.Contains(desc, storageCol) {
		t.Errorf("describeTable(%q) leaks the storage column name %q:\n%s",
			sqlName, storageCol, desc)
	}
	// The primary-key line comes from rt.PrimaryKey.FieldNames(), a DIFFERENT
	// source than the column rows -- so an escaped column in the KEY is the only
	// shape that can catch the two halves disagreeing. A fixture keyed on a plain
	// column cannot express it, which is why this one is keyed on COL$X.
	if !strings.Contains(desc, "primary key") && !strings.Contains(desc, "Primary key") {
		t.Fatalf("no primary-key line in the description, so the assertion below is "+
			"vacuous:\n%s", desc)
	}

	// The stored spelling still resolves — this WIDENS what is accepted rather
	// than swapping one namespace for the other, so scripts holding the old
	// spelling keep working.
	buf.Reset()
	if err := r.describeTable(storageName); err != nil {
		t.Errorf("describeTable(%q) must still resolve: %v", storageName, err)
	}

	// And the miss message offers typeable names.
	buf.Reset()
	err = r.describeTable("NoSuchTable")
	if err == nil {
		t.Fatal("describeTable(\"NoSuchTable\") unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), sqlName) || strings.Contains(err.Error(), storageName) {
		t.Errorf("the \"available:\" list must offer SQL identifiers: %v", err)
	}
}
