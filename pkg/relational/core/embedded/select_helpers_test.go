package embedded

// Direct unit tests for the pure helpers in select_helpers.go.

import (
	"database/sql/driver"
	"testing"

	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// ----- cteRowsToMaps -----------------------------------------------------

// TestCteRowsToMaps_DualKeyForm pins the contract: every output map
// holds each value under BOTH a bare-column key (`col`) and an
// alias-qualified key (`alias.col`). JOIN evaluation reads either
// form depending on whether the SQL reference was qualified.
func TestCteRowsToMaps_DualKeyForm(t *testing.T) {
	t.Parallel()
	cte := &cteData{
		cols: []string{"id", "name"},
		rows: [][]driver.Value{
			{int64(1), "alice"},
			{int64(2), "bob"},
		},
	}
	got := cteRowsToMaps(cte, "u")
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	for i, want := range []struct {
		id   int64
		name string
	}{{1, "alice"}, {2, "bob"}} {
		// Bare-column form.
		if v, ok := got[i]["id"]; !ok || v.(int64) != want.id {
			t.Errorf("row[%d][\"id\"]: got (%v, %v), want (%d, true)", i, v, ok, want.id)
		}
		if v, ok := got[i]["name"]; !ok || v.(string) != want.name {
			t.Errorf("row[%d][\"name\"]: got (%v, %v), want (%s, true)", i, v, ok, want.name)
		}
		// Alias-qualified form.
		if v, ok := got[i]["u.id"]; !ok || v.(int64) != want.id {
			t.Errorf("row[%d][\"u.id\"]: got (%v, %v), want (%d, true)", i, v, ok, want.id)
		}
		if v, ok := got[i]["u.name"]; !ok || v.(string) != want.name {
			t.Errorf("row[%d][\"u.name\"]: got (%v, %v), want (%s, true)", i, v, ok, want.name)
		}
	}
}

// TestCteRowsToMaps_EmptyRows pins the boundary: a CTE with cols but
// zero rows produces an empty slice, not nil and not a slice with
// empty maps.
func TestCteRowsToMaps_EmptyRows(t *testing.T) {
	t.Parallel()
	cte := &cteData{cols: []string{"id"}, rows: nil}
	got := cteRowsToMaps(cte, "x")
	if got == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("len: got %d, want 0", len(got))
	}
}

// TestCteRowsToMaps_NullValuesPreserved pins NULL handling: a nil
// driver.Value in the source row appears under both key forms in
// the output map. Map-key absence vs nil-value semantics matter for
// the JOIN evaluator's "column missing" detection.
func TestCteRowsToMaps_NullValuesPreserved(t *testing.T) {
	t.Parallel()
	cte := &cteData{
		cols: []string{"id", "deleted_at"},
		rows: [][]driver.Value{{int64(1), nil}},
	}
	got := cteRowsToMaps(cte, "u")
	if len(got) != 1 {
		t.Fatalf("len: got %d, want 1", len(got))
	}
	for _, key := range []string{"deleted_at", "u.deleted_at"} {
		v, ok := got[0][key]
		if !ok {
			t.Errorf("key %q: missing from map (want present + nil)", key)
		}
		if v != nil {
			t.Errorf("key %q: got %v, want nil", key, v)
		}
	}
}

// TestCteRowsToMaps_EmptyAlias pins behaviour with an empty alias
// string: the qualified form uses ".col" which is unusual but
// consistent. JOIN evaluation never produces an empty alias in
// practice — this test pins the boundary to guard against future
// callers that might accidentally pass "" and expect graceful
// handling.
func TestCteRowsToMaps_EmptyAlias(t *testing.T) {
	t.Parallel()
	cte := &cteData{
		cols: []string{"id"},
		rows: [][]driver.Value{{int64(1)}},
	}
	got := cteRowsToMaps(cte, "")
	if got[0]["id"] != int64(1) {
		t.Fatalf("bare key missing")
	}
	if _, ok := got[0][".id"]; !ok {
		t.Fatalf("empty-alias qualifier produces .col key; got %v", got[0])
	}
}

// ----- poisonAmbiguousBareCols ------------------------------------------

// TestPoisonAmbiguousBareCols_ReplacesBareWithMarker pins the core
// invariant: bare keys in the ambiguous set get replaced with
// ambiguousColumnMarker; qualified keys (alias.col) and unrelated
// bare keys are untouched.
func TestPoisonAmbiguousBareCols_ReplacesBareWithMarker(t *testing.T) {
	t.Parallel()
	row := map[string]driver.Value{
		"id":     int64(1), // ambiguous — present in both a + b
		"a.id":   int64(1),
		"b.id":   int64(99),
		"name":   "alice", // not ambiguous — should stay
		"a.name": "alice",
	}
	ambiguous := map[string]bool{"id": true}
	poisonAmbiguousBareCols(row, ambiguous)

	v, ok := row["id"].(ambiguousColumnMarker)
	if !ok {
		t.Fatalf("row[\"id\"]: got %T (%v), want ambiguousColumnMarker", row["id"], row["id"])
	}
	if v.Col != "id" {
		t.Errorf("marker.Col: got %q, want id", v.Col)
	}
	// Qualified keys unchanged.
	if row["a.id"].(int64) != 1 || row["b.id"].(int64) != 99 {
		t.Errorf("qualified id values changed: a.id=%v b.id=%v", row["a.id"], row["b.id"])
	}
	// Non-ambiguous bare key unchanged.
	if row["name"].(string) != "alice" {
		t.Errorf("non-ambiguous name changed: %v", row["name"])
	}
}

// TestPoisonAmbiguousBareCols_AmbiguousMissingFromRow pins the
// no-op case: an ambiguous-set entry that's NOT a key in the row
// is silently skipped (poisoning a non-existent key would extend
// the row, which would be wrong).
func TestPoisonAmbiguousBareCols_AmbiguousMissingFromRow(t *testing.T) {
	t.Parallel()
	row := map[string]driver.Value{"id": int64(1)}
	ambiguous := map[string]bool{"name": true} // not in row
	poisonAmbiguousBareCols(row, ambiguous)
	if _, ok := row["name"]; ok {
		t.Fatalf("missing-from-row ambiguous key should not be added")
	}
	if row["id"].(int64) != 1 {
		t.Fatalf("untouched key should be preserved")
	}
}

// TestPoisonAmbiguousBareCols_EmptyAmbiguousSet pins another no-op
// boundary: empty/nil ambiguous set leaves the row entirely
// untouched.
func TestPoisonAmbiguousBareCols_EmptyAmbiguousSet(t *testing.T) {
	t.Parallel()
	for _, ambiguous := range []map[string]bool{nil, {}} {
		row := map[string]driver.Value{"id": int64(1), "name": "alice"}
		poisonAmbiguousBareCols(row, ambiguous)
		if row["id"].(int64) != 1 || row["name"].(string) != "alice" {
			t.Fatalf("empty ambiguous set should not touch row, got %v", row)
		}
	}
}

// TestPoisonAmbiguousBareCols_PreservesQualifiedAmbiguousKey pins a
// subtlety: even if an ambiguous KEY happens to be qualified
// (`a.id`), poisoning replaces it. The caller's contract is "the
// ambiguous set names which keys to poison"; it's the caller's job
// to populate the set with bare names only. This test pins the
// helper's literal behaviour so a future caller bug that puts
// qualified names into the ambiguous set surfaces visibly.
func TestPoisonAmbiguousBareCols_PreservesQualifiedAmbiguousKey(t *testing.T) {
	t.Parallel()
	row := map[string]driver.Value{
		"id":   int64(1),
		"a.id": int64(99),
	}
	ambiguous := map[string]bool{"a.id": true} // qualified — caller bug
	poisonAmbiguousBareCols(row, ambiguous)
	if _, ok := row["a.id"].(ambiguousColumnMarker); !ok {
		t.Fatalf("helper poisons whatever's in the ambiguous set; got %T", row["a.id"])
	}
	// Bare "id" untouched.
	if row["id"].(int64) != 1 {
		t.Errorf("bare id should be untouched")
	}
}

// ----- isSimpleIdentifier ---------------------------------------------------

func TestIsSimpleIdentifier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"id", true},
		{"TABLE_NAME", true},
		{"_priv", true},
		{"a1b2", true},
		{"", false},
		{"1abc", false},
		{"has space", false},
		{"has-dash", false},
		{"has.dot", false},
		{"COUNT(*)", false},
		{"a+b", false},
	}
	for _, tt := range tests {
		if got := isSimpleIdentifier(tt.input); got != tt.want {
			t.Errorf("isSimpleIdentifier(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// ----- jdbcColumnName -------------------------------------------------------

func TestJdbcColumnName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pos  int
		want string
	}{
		{"id", 0, "ID"},
		{"name", 1, "NAME"},
		{"TABLE_TYPE", 2, "TABLE_TYPE"},
		{"t.id", 0, "ID"},
		{"schema.table.col", 3, "COL"},
		{"COUNT(*)", 0, "_0"},
		{"a + b", 2, "_2"},
		{"UPPER(name)", 1, "_1"},
		{"_priv", 0, "_PRIV"},
	}
	for _, tt := range tests {
		if got := jdbcColumnName(tt.name, tt.pos); got != tt.want {
			t.Errorf("jdbcColumnName(%q, %d) = %q, want %q", tt.name, tt.pos, got, tt.want)
		}
	}
}

// ----- jdbcizeColumnNames ---------------------------------------------------

func TestJdbcizeColumnNames(t *testing.T) {
	t.Parallel()
	in := []string{"id", "t.name", "COUNT(*)", "status"}
	got := jdbcizeColumnNames(in)
	want := []string{"ID", "NAME", "_2", "STATUS"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("col[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestJdbcizeColumnNames_Empty(t *testing.T) {
	t.Parallel()
	got := jdbcizeColumnNames(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

// ----- jdbcTypeMax ----------------------------------------------------------

func TestJdbcTypeMax(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b, want string
	}{
		{"INTEGER", "INTEGER", "INTEGER"},
		{"INTEGER", "BIGINT", "BIGINT"},
		{"BIGINT", "INTEGER", "BIGINT"},
		{"BIGINT", "FLOAT", "FLOAT"},
		{"FLOAT", "DOUBLE", "DOUBLE"},
		{"INTEGER", "DOUBLE", "DOUBLE"},
		{"DOUBLE", "DOUBLE", "DOUBLE"},
		{"STRING", "STRING", "STRING"},
		{"BOOLEAN", "BOOLEAN", "BOOLEAN"},
		{"STRING", "BIGINT", ""},
		{"BIGINT", "BOOLEAN", ""},
		{"", "BIGINT", ""},
		{"BIGINT", "", ""},
		{"", "", ""},
		{"STRING", "BOOLEAN", ""},
	}
	for _, tt := range tests {
		if got := jdbcTypeMax(tt.a, tt.b); got != tt.want {
			t.Errorf("jdbcTypeMax(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
		}
	}
}

// ----- convertedDataTypeToJDBC ----------------------------------------------

func TestConvertedDataTypeToJDBC(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input, want string
	}{
		{"INTEGER", "INTEGER"},
		{"INT", "INTEGER"},
		{"int", "INTEGER"},
		{"BIGINT", "BIGINT"},
		{"LONG", "BIGINT"},
		{"FLOAT", "FLOAT"},
		{"DOUBLE", "DOUBLE"},
		{"DOUBLE PRECISION", "DOUBLE"},
		{"DOUBLEPRECISION", "DOUBLE"},
		{"STRING", "STRING"},
		{"VARCHAR", "STRING"},
		{"CHAR", "STRING"},
		{"TEXT", "STRING"},
		{"BOOLEAN", "BOOLEAN"},
		{"BOOL", "BOOLEAN"},
		{"BYTES", "BINARY"},
		{"UUID", "OTHER"},
		{"unknown", ""},
		{"", ""},
		{"  BIGINT  ", "BIGINT"},
	}
	for _, tt := range tests {
		if got := convertedDataTypeToJDBC(tt.input); got != tt.want {
			t.Errorf("convertedDataTypeToJDBC(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ----- integerLiteralJDBCType -----------------------------------------------

func TestIntegerLiteralJDBCType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text string
		want string
	}{
		{"0", "INTEGER"},
		{"42", "INTEGER"},
		{"-1", "INTEGER"},
		{"2147483647", "INTEGER"},
		{"-2147483648", "INTEGER"},
		{"2147483648", "BIGINT"},
		{"-2147483649", "BIGINT"},
		{"9223372036854775807", "BIGINT"},
	}
	for _, tt := range tests {
		if got := integerLiteralJDBCType(tt.text); got != tt.want {
			t.Errorf("integerLiteralJDBCType(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

// ----- orderByLess ----------------------------------------------------------

func TestOrderByLess_AscendingStrings(t *testing.T) {
	t.Parallel()
	ob := orderByClause{ascending: true}
	less, equal := orderByLess("alice", "bob", ob)
	if !less || equal {
		t.Errorf("alice < bob ASC: less=%v equal=%v", less, equal)
	}
	less, equal = orderByLess("bob", "alice", ob)
	if less || equal {
		t.Errorf("bob < alice ASC: less=%v equal=%v", less, equal)
	}
	less, equal = orderByLess("alice", "alice", ob)
	if less || !equal {
		t.Errorf("alice == alice: less=%v equal=%v", less, equal)
	}
}

func TestOrderByLess_DescendingInts(t *testing.T) {
	t.Parallel()
	ob := orderByClause{ascending: false}
	less, equal := orderByLess(int64(10), int64(5), ob)
	if !less || equal {
		t.Errorf("10 > 5 DESC: less=%v equal=%v", less, equal)
	}
	less, equal = orderByLess(int64(5), int64(10), ob)
	if less || equal {
		t.Errorf("5 > 10 DESC: less=%v equal=%v", less, equal)
	}
}

func TestOrderByLess_NullsDefaultASC(t *testing.T) {
	t.Parallel()
	ob := orderByClause{ascending: true}
	less, equal := orderByLess(nil, "bob", ob)
	if !less || equal {
		t.Errorf("nil < bob ASC (nulls first): less=%v equal=%v", less, equal)
	}
	less, equal = orderByLess("bob", nil, ob)
	if less || equal {
		t.Errorf("bob > nil ASC: less=%v equal=%v", less, equal)
	}
}

func TestOrderByLess_NullsDefaultDESC(t *testing.T) {
	t.Parallel()
	ob := orderByClause{ascending: false}
	less, equal := orderByLess(nil, "bob", ob)
	if less || equal {
		t.Errorf("nil > bob DESC (nulls last): less=%v equal=%v", less, equal)
	}
	less, equal = orderByLess("bob", nil, ob)
	if !less || equal {
		t.Errorf("bob < nil DESC: less=%v equal=%v", less, equal)
	}
}

func TestOrderByLess_NullsBothNil(t *testing.T) {
	t.Parallel()
	ob := orderByClause{ascending: true}
	less, equal := orderByLess(nil, nil, ob)
	if less || !equal {
		t.Errorf("nil == nil: less=%v equal=%v", less, equal)
	}
}

func TestOrderByLess_ExplicitNullsLast(t *testing.T) {
	t.Parallel()
	nf := false
	ob := orderByClause{ascending: true, nullsFirst: &nf}
	less, equal := orderByLess(nil, "bob", ob)
	if less || equal {
		t.Errorf("nil NULLS LAST ASC should sort after bob: less=%v equal=%v", less, equal)
	}
}

func TestOrderByLess_ExplicitNullsFirst(t *testing.T) {
	t.Parallel()
	nf := true
	ob := orderByClause{ascending: false, nullsFirst: &nf}
	less, equal := orderByLess(nil, "bob", ob)
	if !less || equal {
		t.Errorf("nil NULLS FIRST DESC should sort before bob: less=%v equal=%v", less, equal)
	}
}

// ----- projectSystemRows ----------------------------------------------------

func TestProjectSystemRows_SelectStar(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"TABLE_NAME", "TABLE_TYPE"},
		rows: [][]driver.Value{
			{"orders", "TABLE"},
			{"flowers", "TABLE"},
		},
	}
	sq := &selectQuery{tableName: "TABLES", limit: -1}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if len(sr.rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(sr.rows))
	}
}

func TestProjectSystemRows_ProjectSingleColumn(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"TABLE_NAME", "TABLE_TYPE", "TABLE_SCHEMA"},
		rows: [][]driver.Value{
			{"orders", "TABLE", "public"},
			{"flowers", "TABLE", "public"},
		},
	}
	sq := &selectQuery{
		selectClassification: selectClassification{
			projCols:    []string{"TABLE_NAME"},
			projExprs:   []antlrgen.IExpressionContext{nil},
			projAliases: []string{""},
		},
		tableName: "TABLES",
		limit:     -1,
	}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if len(sr.cols) != 1 || sr.cols[0] != "TABLE_NAME" {
		t.Fatalf("cols: got %v, want [TABLE_NAME]", sr.cols)
	}
	if len(sr.rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(sr.rows))
	}
	if sr.rows[0][0] != "orders" || sr.rows[1][0] != "flowers" {
		t.Errorf("unexpected values: %v %v", sr.rows[0], sr.rows[1])
	}
}

func TestProjectSystemRows_ProjectWithAlias(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"TABLE_NAME", "TABLE_TYPE"},
		rows: [][]driver.Value{{"orders", "TABLE"}},
	}
	sq := &selectQuery{
		selectClassification: selectClassification{
			projCols:    []string{"TABLE_NAME"},
			projExprs:   []antlrgen.IExpressionContext{nil},
			projAliases: []string{"tbl"},
		},
		tableName: "TABLES",
		limit:     -1,
	}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if sr.cols[0] != "tbl" {
		t.Errorf("alias not applied: got %q, want %q", sr.cols[0], "tbl")
	}
}

func TestProjectSystemRows_ColumnNotFound(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"TABLE_NAME"},
		rows: [][]driver.Value{{"orders"}},
	}
	sq := &selectQuery{
		selectClassification: selectClassification{
			projCols:    []string{"NONEXISTENT"},
			projExprs:   []antlrgen.IExpressionContext{nil},
			projAliases: []string{""},
		},
		tableName: "TABLES",
	}
	_, err := projectSystemRows(in, sq)
	if err == nil {
		t.Fatal("expected error for missing column")
	}
}

func TestProjectSystemRows_CaseInsensitiveMatch(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"TABLE_NAME"},
		rows: [][]driver.Value{{"orders"}},
	}
	sq := &selectQuery{
		selectClassification: selectClassification{
			projCols:    []string{"table_name"},
			projExprs:   []antlrgen.IExpressionContext{nil},
			projAliases: []string{""},
		},
		tableName: "TABLES",
		limit:     -1,
	}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if sr.rows[0][0] != "orders" {
		t.Errorf("case-insensitive match failed: got %v", sr.rows[0][0])
	}
}

func TestProjectSystemRows_OrderByASC(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"NAME"},
		rows: [][]driver.Value{{"charlie"}, {"alice"}, {"bob"}},
	}
	sq := &selectQuery{
		selectClassification: selectClassification{
			orderBy: []orderByClause{{colName: "NAME", ascending: true}},
		},
		tableName: "T",
		limit:     -1,
	}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	want := []string{"alice", "bob", "charlie"}
	for i, w := range want {
		if sr.rows[i][0] != w {
			t.Errorf("row[%d]: got %v, want %v", i, sr.rows[i][0], w)
		}
	}
}

func TestProjectSystemRows_OrderByDESC(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"NAME"},
		rows: [][]driver.Value{{"charlie"}, {"alice"}, {"bob"}},
	}
	sq := &selectQuery{
		selectClassification: selectClassification{
			orderBy: []orderByClause{{colName: "NAME", ascending: false}},
		},
		tableName: "T",
		limit:     -1,
	}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	want := []string{"charlie", "bob", "alice"}
	for i, w := range want {
		if sr.rows[i][0] != w {
			t.Errorf("row[%d]: got %v, want %v", i, sr.rows[i][0], w)
		}
	}
}

func TestProjectSystemRows_Limit(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"ID"},
		rows: [][]driver.Value{{int64(1)}, {int64(2)}, {int64(3)}, {int64(4)}},
	}
	sq := &selectQuery{tableName: "T", limit: 2}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if len(sr.rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(sr.rows))
	}
	if sr.rows[0][0] != int64(1) || sr.rows[1][0] != int64(2) {
		t.Errorf("wrong rows: %v", sr.rows)
	}
}

func TestProjectSystemRows_Offset(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"ID"},
		rows: [][]driver.Value{{int64(1)}, {int64(2)}, {int64(3)}},
	}
	sq := &selectQuery{tableName: "T", limit: -1, offset: 1}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if len(sr.rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(sr.rows))
	}
	if sr.rows[0][0] != int64(2) {
		t.Errorf("offset not applied: first row = %v", sr.rows[0][0])
	}
}

func TestProjectSystemRows_OffsetBeyondRows(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"ID"},
		rows: [][]driver.Value{{int64(1)}},
	}
	sq := &selectQuery{tableName: "T", limit: -1, offset: 10}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if len(sr.rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(sr.rows))
	}
}

func TestProjectSystemRows_LimitAndOffset(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"ID"},
		rows: [][]driver.Value{{int64(1)}, {int64(2)}, {int64(3)}, {int64(4)}, {int64(5)}},
	}
	sq := &selectQuery{tableName: "T", limit: 2, offset: 2}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if len(sr.rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(sr.rows))
	}
	if sr.rows[0][0] != int64(3) || sr.rows[1][0] != int64(4) {
		t.Errorf("LIMIT 2 OFFSET 2: got %v %v", sr.rows[0][0], sr.rows[1][0])
	}
}

func TestProjectSystemRows_OrderByThenLimit(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"NAME"},
		rows: [][]driver.Value{{"charlie"}, {"alice"}, {"bob"}, {"dave"}},
	}
	sq := &selectQuery{
		selectClassification: selectClassification{
			orderBy: []orderByClause{{colName: "NAME", ascending: true}},
		},
		tableName: "T",
		limit:     2,
	}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if len(sr.rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(sr.rows))
	}
	if sr.rows[0][0] != "alice" || sr.rows[1][0] != "bob" {
		t.Errorf("top-2 ASC: got %v %v", sr.rows[0][0], sr.rows[1][0])
	}
}

func TestProjectSystemRows_ProjectionThenOrderBy(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"NAME", "TYPE"},
		rows: [][]driver.Value{
			{"charlie", "A"},
			{"alice", "B"},
			{"bob", "A"},
		},
	}
	sq := &selectQuery{
		selectClassification: selectClassification{
			projCols:    []string{"NAME"},
			projExprs:   []antlrgen.IExpressionContext{nil},
			projAliases: []string{""},
			orderBy:     []orderByClause{{colName: "NAME", ascending: true}},
		},
		tableName: "T",
		limit:     -1,
	}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if len(sr.cols) != 1 {
		t.Fatalf("expected 1 column, got %d", len(sr.cols))
	}
	want := []string{"alice", "bob", "charlie"}
	for i, w := range want {
		if sr.rows[i][0] != w {
			t.Errorf("row[%d]: got %v, want %v", i, sr.rows[i][0], w)
		}
	}
}

func TestProjectSystemRows_NoLimitMeansUnbounded(t *testing.T) {
	t.Parallel()
	in := &staticRows{
		cols: []string{"ID"},
		rows: [][]driver.Value{{int64(1)}, {int64(2)}, {int64(3)}},
	}
	sq := &selectQuery{tableName: "T", limit: -1}
	got, err := projectSystemRows(in, sq)
	if err != nil {
		t.Fatal(err)
	}
	sr := got.(*staticRows)
	if len(sr.rows) != 3 {
		t.Fatalf("limit -1 should be unbounded, got %d rows", len(sr.rows))
	}
}
