package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

func newTestDatabaseMetaData(t testing.TB) (*CatalogDatabaseMetaData, *InMemoryStoreCatalog, api.Transaction, api.SchemaTemplate) {
	t.Helper()
	c, tx, tmpl := newSeededCatalog(t, "demo")
	md := NewCatalogDatabaseMetaData(CatalogDatabaseMetaDataOptions{
		StoreCatalog: c,
		URL:          "fdbsql:///test",
		UserName:     "testuser",
		DriverName:   "fdbsql",
	})
	return md, c, tx, tmpl
}

func collectStrings(t *testing.T, rs api.ResultSet, ncols int) [][]string {
	t.Helper()
	var out [][]string
	for rs.Next() {
		row := make([]string, ncols)
		for i := 0; i < ncols; i++ {
			v, err := rs.String(i + 1)
			if err != nil {
				t.Fatalf("String(%d): %v", i+1, err)
			}
			row[i] = v
		}
		out = append(out, row)
	}
	return out
}

func TestDatabaseMetaData_ProductIdentification(t *testing.T) {
	t.Parallel()
	md, _, _, _ := newTestDatabaseMetaData(t)
	if md.URL() != "fdbsql:///test" {
		t.Errorf("URL = %q", md.URL())
	}
	if md.UserName() != "testuser" {
		t.Errorf("UserName = %q", md.UserName())
	}
	if md.IsReadOnly() {
		t.Error("IsReadOnly = true, want false")
	}
	if md.DatabaseProductName() != "FoundationDB Relational" {
		t.Errorf("DatabaseProductName = %q", md.DatabaseProductName())
	}
	if md.DriverName() != "fdbsql" {
		t.Errorf("DriverName = %q", md.DriverName())
	}
}

func TestDatabaseMetaData_SchemasEmpty(t *testing.T) {
	t.Parallel()
	md, _, _, _ := newTestDatabaseMetaData(t)
	rs, err := md.Schemas(context.Background())
	if err != nil {
		t.Fatalf("Schemas: %v", err)
	}
	defer rs.Close()
	if rs.Next() {
		t.Error("Schemas() returned rows from an empty catalog")
	}
}

func TestDatabaseMetaData_SchemasAllListed(t *testing.T) {
	t.Parallel()
	md, c, tx, tmpl := newTestDatabaseMetaData(t)
	for _, p := range [][2]string{{"/a", "s1"}, {"/a", "s2"}, {"/b", "s1"}} {
		if err := c.SaveSchema(tx, tmpl.GenerateSchema(p[0], p[1]), true); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := md.Schemas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	rows := collectStrings(t, rs, 2)
	want := [][]string{
		{"s1", "/a"}, {"s2", "/a"}, {"s1", "/b"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d: %v", len(rows), len(want), rows)
	}
	for i, r := range rows {
		if r[0] != want[i][0] || r[1] != want[i][1] {
			t.Errorf("row %d: got %v, want %v", i, r, want[i])
		}
	}
}

func TestDatabaseMetaData_SchemasFilteredPatterns(t *testing.T) {
	t.Parallel()
	md, c, tx, tmpl := newTestDatabaseMetaData(t)
	for _, p := range [][2]string{
		{"/prod", "public"},
		{"/prod", "staging"},
		{"/dev", "public"},
		{"/dev", "private"},
	} {
		if err := c.SaveSchema(tx, tmpl.GenerateSchema(p[0], p[1]), true); err != nil {
			t.Fatal(err)
		}
	}

	// Catalog LIKE '/prod': only rows with db == /prod.
	rs, err := md.SchemasFiltered(context.Background(), "/prod", "")
	if err != nil {
		t.Fatal(err)
	}
	rows := collectStrings(t, rs, 2)
	rs.Close()
	if len(rows) != 2 || rows[0][1] != "/prod" || rows[1][1] != "/prod" {
		t.Errorf("filter by /prod: got %v", rows)
	}

	// Schema LIKE 'p%': public, private.
	rs, err = md.SchemasFiltered(context.Background(), "", "p%")
	if err != nil {
		t.Fatal(err)
	}
	rows = collectStrings(t, rs, 2)
	rs.Close()
	wantSchemas := map[string]int{"public": 2, "private": 1}
	gotSchemas := map[string]int{}
	for _, r := range rows {
		gotSchemas[r[0]]++
	}
	for s, c := range wantSchemas {
		if gotSchemas[s] != c {
			t.Errorf("filter p%%: schema %q count = %d, want %d", s, gotSchemas[s], c)
		}
	}

	// `_taging` (single wildcard): matches "staging" exactly.
	rs, err = md.SchemasFiltered(context.Background(), "", "_taging")
	if err != nil {
		t.Fatal(err)
	}
	rows = collectStrings(t, rs, 2)
	rs.Close()
	if len(rows) != 1 || rows[0][0] != "staging" {
		t.Errorf("filter _taging: got %v", rows)
	}
}

func TestDatabaseMetaData_Tables(t *testing.T) {
	t.Parallel()
	md, c, tx, tmpl := newTestDatabaseMetaData(t)
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s"), true); err != nil {
		t.Fatal(err)
	}
	rs, err := md.Tables(context.Background(), "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()

	// Demo proto has Order / Customer / TypedRecord.
	var names []string
	for rs.Next() {
		name, _ := rs.String(3)
		typ, _ := rs.String(4)
		if typ != "TABLE" {
			t.Errorf("table type = %q, want TABLE", typ)
		}
		names = append(names, name)
	}
	expectContainsName(t, names, "Order", "Customer", "TypedRecord")
}

func TestDatabaseMetaData_TablesFiltered(t *testing.T) {
	t.Parallel()
	md, c, tx, tmpl := newTestDatabaseMetaData(t)
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s"), true); err != nil {
		t.Fatal(err)
	}
	rs, err := md.Tables(context.Background(), "", "", "Or%", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var got []string
	for rs.Next() {
		name, _ := rs.String(3)
		got = append(got, name)
	}
	if len(got) != 1 || got[0] != "Order" {
		t.Errorf("tables filter 'Or%%': got %v, want [Order]", got)
	}
}

func TestDatabaseMetaData_TablesTypeFilterExcludesNonTable(t *testing.T) {
	t.Parallel()
	md, c, tx, tmpl := newTestDatabaseMetaData(t)
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s"), true); err != nil {
		t.Fatal(err)
	}
	// Asking for only VIEW rows → empty.
	rs, err := md.Tables(context.Background(), "", "", "", []string{"VIEW"})
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if rs.Next() {
		t.Error("Tables with types=[VIEW] returned rows; our catalog has no views")
	}
}

func TestDatabaseMetaData_PrimaryKeys(t *testing.T) {
	t.Parallel()
	md, c, tx, tmpl := newTestDatabaseMetaData(t)
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s"), true); err != nil {
		t.Fatal(err)
	}
	rs, err := md.PrimaryKeys(context.Background(), "/db", "s", "Order")
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()

	if !rs.Next() {
		t.Fatal("PrimaryKeys(Order) returned no rows")
	}
	// 6 columns: TABLE_CAT, TABLE_SCHEM, TABLE_NAME, COLUMN_NAME,
	// KEY_SEQ, PK_NAME.
	cat, _ := rs.String(1)
	schema, _ := rs.String(2)
	tableName, _ := rs.String(3)
	col, _ := rs.String(4)
	keySeq, _ := rs.Long(5)
	pkName, _ := rs.String(6)

	if cat != "/db" || schema != "s" || tableName != "Order" {
		t.Errorf("got (cat, schema, table) = (%q, %q, %q)", cat, schema, tableName)
	}
	if col == "" || keySeq != 1 || pkName == "" {
		t.Errorf("got (col, keySeq, pkName) = (%q, %d, %q)", col, keySeq, pkName)
	}
}

func TestDatabaseMetaData_PrimaryKeysMissingTable(t *testing.T) {
	t.Parallel()
	md, c, tx, tmpl := newTestDatabaseMetaData(t)
	if err := c.SaveSchema(tx, tmpl.GenerateSchema("/db", "s"), true); err != nil {
		t.Fatal(err)
	}
	_, err := md.PrimaryKeys(context.Background(), "/db", "s", "NotATable")
	if err == nil {
		t.Fatal("PrimaryKeys(missing) should error")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedTable {
		t.Errorf("Code = %q, want %q", apiErr.Code, api.ErrCodeUndefinedTable)
	}
}

func TestDatabaseMetaData_NilCatalogPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewCatalogDatabaseMetaData with nil StoreCatalog did not panic")
		}
	}()
	_ = NewCatalogDatabaseMetaData(CatalogDatabaseMetaDataOptions{})
}

func TestCompileLikePattern(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		input   string
		match   bool
	}{
		{"", "anything", true}, // empty pattern → nil regex handled by caller
		{"abc", "abc", true},
		{"abc", "abcd", false},
		{"a%", "abc", true},
		{"a%", "bac", false},
		{"_bc", "abc", true},
		{"_bc", "aabc", false},
		{"a.c", "a.c", true}, // metacharacter '.'  must be literal
		{"a.c", "aXc", false},
		{"a_c_", "abcd", true},
	}
	for _, tc := range cases {
		got := false
		if tc.pattern == "" {
			got = true
		} else {
			re := compileLikePattern(tc.pattern)
			got = re.MatchString(tc.input)
		}
		if got != tc.match {
			t.Errorf("pattern=%q input=%q: got %v, want %v", tc.pattern, tc.input, got, tc.match)
		}
	}
}

// expectContainsName is a small string-slice helper reused across
// catalog tests.
func expectContainsName(t *testing.T, haystack []string, needles ...string) {
	t.Helper()
	set := make(map[string]struct{}, len(haystack))
	for _, s := range haystack {
		set[s] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; !ok {
			t.Errorf("expected %q in %v", n, haystack)
		}
	}
}
