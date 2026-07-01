package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestBuildFDBSQLDSN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		clusterFile string
		dbPath      string
		schema      string
		want        string
	}{
		{
			name:   "path only",
			dbPath: "/myapp",
			want:   "fdbsql:///myapp",
		},
		{
			// url.Values.Encode percent-encodes `/` as `%2F`. The
			// driver's url.Parse unpacks it, so the on-wire form stays
			// correct even though it's uglier to read.
			name:        "with cluster file",
			clusterFile: "/etc/fdb/prod.cluster",
			dbPath:      "/myapp",
			want:        "fdbsql:///myapp?cluster_file=%2Fetc%2Ffdb%2Fprod.cluster",
		},
		{
			name:   "with schema",
			dbPath: "/myapp",
			schema: "main",
			want:   "fdbsql:///myapp?schema=main",
		},
		{
			name:        "both options",
			clusterFile: "/c",
			dbPath:      "/myapp",
			schema:      "main",
			want:        "fdbsql:///myapp?cluster_file=%2Fc&schema=main",
		},
		{
			name:   "strips leading slash on path",
			dbPath: "myapp",
			want:   "fdbsql:///myapp",
		},
		{
			// Cluster file paths with spaces / special chars would corrupt
			// the DSN under naive string concatenation. url.Values
			// percent-encodes them so the driver's URL parser recovers
			// the original value. Caught by reviewer round 7.
			name:        "cluster file with space percent-encoded",
			clusterFile: "/home/user/my project/fdb.cluster",
			dbPath:      "/myapp",
			want:        "fdbsql:///myapp?cluster_file=%2Fhome%2Fuser%2Fmy+project%2Ffdb.cluster",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildFDBSQLDSN(tc.clusterFile, tc.dbPath, tc.schema)
			if got != tc.want {
				t.Errorf("buildFDBSQLDSN = %q; want %q", got, tc.want)
			}
		})
	}
}

// Regression: the missing---database error used to read "--database is
// required …", which fang's error banner capitalizes into "--Database is
// required" — garbling the flag name. The message must start with a
// sentence word so banner capitalization can't touch the flag.
func TestSQL_MissingDatabaseFlag_Message(t *testing.T) {
	// Not parallel: writeTestConfig uses t.Setenv.
	writeTestConfig(t, "local")
	c := newSQLCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{})
	err := c.Execute()
	if err == nil {
		t.Fatal("expected error when --database is missing")
	}
	if !strings.Contains(err.Error(), "missing required flag --database") {
		t.Errorf("error = %q; want it to mention `missing required flag --database`", err)
	}
	if strings.HasPrefix(err.Error(), "-") {
		t.Errorf("error = %q; must not start with a flag name (fang banner capitalization garbles it)", err)
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
	// Plain profile: NULL renders as the bare token (no ANSI), so exact
	// matches are safe throughout.
	r := &sqlRunner{st: plainSQLStyles()}
	if got := r.renderCell(nil); got != "NULL" {
		t.Errorf("nil cell = %q; want NULL", got)
	}
	if got := r.renderCell([]byte{0x01, 0x02, 0xff}); got != "0102ff" {
		t.Errorf("[]byte cell = %q; want 0102ff", got)
	}
	if got := r.renderCell("hello"); got != "hello" {
		t.Errorf("string cell = %q; want hello", got)
	}
	if got := r.renderCell(int64(42)); got != "42" {
		t.Errorf("int64 cell = %q; want 42", got)
	}
}

// Regression (RFC-174 bug 2 + codex P2-3): piped/scripted output must be
// pure 7-bit ASCII with zero ANSI escapes. The \x1b check alone would
// pass with Unicode box-drawing (`─┼─`) still present, so assert every
// byte < 0x80 too.
func TestRenderStaticTable_PlainIsASCII(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	r := &sqlRunner{out: &out, st: plainSQLStyles()}
	err := r.renderStaticTable(&out,
		[]string{"NAME", "VALUE"},
		[][]string{{"alpha", "1"}, {"beta", "NULL"}})
	if err != nil {
		t.Fatalf("renderStaticTable: %v", err)
	}
	got := out.Bytes()
	if bytes.ContainsRune(got, 0x1b) {
		t.Errorf("plain table contains ANSI escape:\n%q", got)
	}
	for i, b := range got {
		if b >= 0x80 {
			t.Errorf("plain table contains non-ASCII byte 0x%02x at offset %d:\n%q", b, i, got)
			break
		}
	}
	// Sanity: it still looks like a table (ASCII separators present;
	// exact spacing depends on column widths).
	if !strings.Contains(out.String(), " | ") || !strings.Contains(out.String(), "-+-") {
		t.Errorf("plain table missing ASCII separators:\n%s", out.String())
	}
}

// The TTY profile must keep the box-drawing look — plain mode is a pipe
// concession, not a downgrade for interactive users.
func TestRenderStaticTable_TTYKeepsBoxDrawing(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	r := &sqlRunner{out: &out, st: ttySQLStyles()}
	if err := r.renderStaticTable(&out,
		[]string{"NAME"}, [][]string{{"alpha"}}); err != nil {
		t.Fatalf("renderStaticTable: %v", err)
	}
	if !strings.Contains(out.String(), "─") {
		t.Errorf("tty table lost its box-drawing rule:\n%q", out.String())
	}
}

// isTerminalWriter: a bytes.Buffer (and any non-*os.File) is never a
// terminal — that's what routes tests and pipes to the plain profile.
func TestIsTerminalWriter_BufferIsNotTerminal(t *testing.T) {
	t.Parallel()
	if isTerminalWriter(&bytes.Buffer{}) {
		t.Error("bytes.Buffer reported as terminal")
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
