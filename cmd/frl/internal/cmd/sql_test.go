package cmd

import (
	"strings"
	"testing"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
)

func TestBuildFDBSQLDSN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		ctx    *configv1.Context
		dbPath string
		schema string
		want   string
	}{
		{
			name:   "path only",
			ctx:    &configv1.Context{},
			dbPath: "/myapp",
			want:   "fdbsql:///myapp",
		},
		{
			// url.Values.Encode percent-encodes `/` as `%2F`. The
			// driver's url.Parse unpacks it, so the on-wire form stays
			// correct even though it's uglier to read.
			name:   "with cluster file",
			ctx:    &configv1.Context{ClusterFile: "/etc/fdb/prod.cluster"},
			dbPath: "/myapp",
			want:   "fdbsql:///myapp?cluster_file=%2Fetc%2Ffdb%2Fprod.cluster",
		},
		{
			name:   "with schema",
			ctx:    &configv1.Context{},
			dbPath: "/myapp",
			schema: "main",
			want:   "fdbsql:///myapp?schema=main",
		},
		{
			name:   "both options",
			ctx:    &configv1.Context{ClusterFile: "/c"},
			dbPath: "/myapp",
			schema: "main",
			want:   "fdbsql:///myapp?cluster_file=%2Fc&schema=main",
		},
		{
			name:   "strips leading slash on path",
			ctx:    &configv1.Context{},
			dbPath: "myapp",
			want:   "fdbsql:///myapp",
		},
		{
			// Cluster file paths with spaces / special chars would corrupt
			// the DSN under naive string concatenation. url.Values
			// percent-encodes them so the driver's URL parser recovers
			// the original value. Caught by reviewer round 7.
			name:   "cluster file with space percent-encoded",
			ctx:    &configv1.Context{ClusterFile: "/home/user/my project/fdb.cluster"},
			dbPath: "/myapp",
			want:   "fdbsql:///myapp?cluster_file=%2Fhome%2Fuser%2Fmy+project%2Ffdb.cluster",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildFDBSQLDSN(tc.ctx, tc.dbPath, tc.schema)
			if got != tc.want {
				t.Errorf("buildFDBSQLDSN = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestIsQuery(t *testing.T) {
	t.Parallel()
	queries := []string{
		"SELECT 1",
		"select 1",
		"  SELECT * FROM foo",
		// Multi-line — leading keyword followed by newline must still
		// match. Earlier HasPrefix("SELECT ") version broke here and
		// routed the statement to ExecContext (reported by reviewer).
		"SELECT\n  *\nFROM orders",
		"select\n  count(*)\nfrom orders",
		"WITH cte AS (SELECT 1) SELECT * FROM cte",
		"EXPLAIN SELECT 1",
		"SHOW TABLES",
		"VALUES (1), (2)",
		"DESCRIBE foo",
	}
	non := []string{
		"INSERT INTO foo VALUES (1)",
		"UPDATE foo SET x = 1",
		"DELETE FROM foo",
		"CREATE TABLE foo (x INT)",
		"DROP TABLE foo",
		"",
	}
	for _, q := range queries {
		if !isQuery(q) {
			t.Errorf("isQuery(%q) = false; want true", q)
		}
	}
	for _, q := range non {
		if isQuery(q) {
			t.Errorf("isQuery(%q) = true; want false", q)
		}
	}
}

func TestEndsStatement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"SELECT 1;", true},
		{"SELECT 1", false},
		{"  SELECT 1 ;  ", true},
		{"SELECT 1; -- trailing comment", true},
		{"-- just a comment", false},
		{"INSERT INTO foo VALUES (1);", true},
		{"", false},
	}
	for _, tc := range cases {
		if got := endsStatement(tc.in); got != tc.want {
			t.Errorf("endsStatement(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestSplitStatements(t *testing.T) {
	t.Parallel()
	input := `-- seed data
INSERT INTO orders VALUES (1, 'a');
INSERT INTO orders VALUES (2, 'b');

-- trailing without terminator should still be kept
SELECT count(*) FROM orders`
	got := splitStatements(input)
	if len(got) != 3 {
		t.Fatalf("got %d statements, want 3:\n%v", len(got), got)
	}
	if !strings.Contains(got[0], "INSERT") || !strings.Contains(got[0], "(1, 'a')") {
		t.Errorf("stmt[0] = %q", got[0])
	}
	if !strings.Contains(got[2], "SELECT count(*)") {
		t.Errorf("stmt[2] = %q; want the trailing SELECT", got[2])
	}
}

func TestSplitStatements_Empty(t *testing.T) {
	t.Parallel()
	cases := []string{"", "   ", "-- just comments\n-- nothing else"}
	for _, in := range cases {
		got := splitStatements(in)
		if len(got) != 0 {
			t.Errorf("splitStatements(%q) = %v; want empty", in, got)
		}
	}
}

func TestPlural(t *testing.T) {
	t.Parallel()
	if plural(1) != "" {
		t.Errorf("plural(1) = %q; want empty", plural(1))
	}
	for _, n := range []int{0, 2, 42} {
		if plural(n) != "s" {
			t.Errorf("plural(%d) = %q; want 's'", n, plural(n))
		}
	}
}

func TestRenderCell_TypeDispatch(t *testing.T) {
	t.Parallel()
	// NULL is special — renders with ANSI-styled NULL text, so we
	// match on the literal "NULL" token rather than the whole string.
	got := renderCell(nil)
	if !strings.Contains(got, "NULL") {
		t.Errorf("nil cell = %q; want to contain NULL", got)
	}
	if got := renderCell([]byte{0x01, 0x02, 0xff}); got != "0102ff" {
		t.Errorf("[]byte cell = %q; want 0102ff", got)
	}
	if got := renderCell("hello"); got != "hello" {
		t.Errorf("string cell = %q; want hello", got)
	}
	if got := renderCell(int64(42)); got != "42" {
		t.Errorf("int64 cell = %q; want 42", got)
	}
}

func TestTxCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want txCommandKind
	}{
		{"BEGIN", txBegin},
		{"begin", txBegin},
		{"  BEGIN   ", txBegin},
		{"BEGIN WORK", txBegin},
		{"START TRANSACTION", txBegin},
		{"start transaction", txBegin},
		{"START TRANSACTION READ WRITE", txBegin},

		{"COMMIT", txCommit},
		{"commit", txCommit},
		{"COMMIT ", txCommit},
		{"COMMIT WORK", txCommit},

		{"ROLLBACK", txRollback},
		{"rollback", txRollback},
		{"ROLLBACK WORK", txRollback},

		// Not tx commands — watchwords that shouldn't trip the matcher.
		{"SELECT 1", txNone},
		{"INSERT INTO begin VALUES (1)", txNone}, // begin as ident, not keyword
		{"", txNone},
		{"BEGINNER", txNone}, // prefix-only match must require space
	}
	for _, tc := range cases {
		if got := txCommand(tc.in); got != tc.want {
			t.Errorf("txCommand(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestPadCell(t *testing.T) {
	t.Parallel()
	if got := padCell("hi", 5); got != "hi   " {
		t.Errorf("padCell(hi, 5) = %q; want %q", got, "hi   ")
	}
	if got := padCell("hello", 5); got != "hello" {
		t.Errorf("padCell(hello, 5) = %q; want no padding", got)
	}
	// Over-sized input is returned verbatim — never truncates.
	if got := padCell("overlong", 3); got != "overlong" {
		t.Errorf("padCell truncated: %q", got)
	}
}
