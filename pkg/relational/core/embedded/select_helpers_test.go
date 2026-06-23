package embedded

// Direct unit tests for the pure helpers in select_helpers.go.

import (
	"database/sql/driver"
	"testing"

	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

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

// ----- classifyPrimitiveType ------------------------------------------------

func TestClassifyPrimitiveType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		castType, wantType string
	}{
		{"INTEGER", "INTEGER"},
		{"BIGINT", "BIGINT"},
		{"FLOAT", "FLOAT"},
		{"DOUBLE", "DOUBLE"},
		{"STRING", "STRING"},
		{"BOOLEAN", "BOOLEAN"},
		{"BYTES", "BYTES"},
		{"UUID", "UUID"},
		{"DATE", "DATE"},
		{"TIMESTAMP", "TIMESTAMP"},
	}
	for _, tt := range tests {
		t.Run(tt.castType, func(t *testing.T) {
			t.Parallel()
			cdt := parseCastType(t, "SELECT CAST(1 AS "+tt.castType+") FROM t")
			got := classifyPrimitiveType(cdt)
			if got != tt.wantType {
				t.Errorf("classifyPrimitiveType(%s) = %q, want %q", tt.castType, got, tt.wantType)
			}
		})
	}
}

// parseCastType parses a SELECT containing CAST(... AS T) and returns
// the ConvertedDataType node.
func parseCastType(t *testing.T, sql string) antlrgen.IConvertedDataTypeContext {
	t.Helper()
	sq := parseSelect(t, sql)
	if len(sq.projExprs) == 0 || sq.projExprs[0] == nil {
		t.Fatalf("no projections in %q", sql)
	}
	expr := sq.projExprs[0]
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		t.Fatalf("expected PredicatedExpressionContext, got %T", expr)
	}
	fcea, ok := pred.ExpressionAtom().(*antlrgen.FunctionCallExpressionAtomContext)
	if !ok {
		t.Fatalf("expected FunctionCallExpressionAtomContext, got %T", pred.ExpressionAtom())
	}
	sfc, ok := fcea.FunctionCall().(*antlrgen.SpecificFunctionCallContext)
	if !ok {
		t.Fatalf("expected SpecificFunctionCallContext, got %T", fcea.FunctionCall())
	}
	dtfc, ok := sfc.SpecificFunction().(*antlrgen.DataTypeFunctionCallContext)
	if !ok {
		t.Fatalf("expected DataTypeFunctionCallContext, got %T", sfc.SpecificFunction())
	}
	cdt := dtfc.ConvertedDataType()
	if cdt == nil {
		t.Fatalf("no ConvertedDataType in %q", sql)
	}
	return cdt
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
