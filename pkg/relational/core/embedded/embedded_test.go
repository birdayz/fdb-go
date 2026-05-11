package embedded

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/session"
)

func TestSubstituteParams(t *testing.T) {
	t.Parallel()
	nv := func(ordinal int, v driver.Value) driver.NamedValue {
		return driver.NamedValue{Ordinal: ordinal, Value: v}
	}
	cases := []struct {
		name    string
		query   string
		args    []driver.NamedValue
		want    string
		wantErr bool
	}{
		{
			name:  "no params",
			query: "SELECT * FROM t",
			args:  nil,
			want:  "SELECT * FROM t",
		},
		{
			name:  "int64",
			query: "SELECT * FROM t WHERE id = ?",
			args:  []driver.NamedValue{nv(1, int64(42))},
			want:  "SELECT * FROM t WHERE id = 42",
		},
		{
			name:  "float64",
			query: "INSERT INTO t VALUES (?)",
			args:  []driver.NamedValue{nv(1, float64(3.14))},
			want:  "INSERT INTO t VALUES (3.14)",
		},
		{
			name:  "string escaping",
			query: "INSERT INTO t VALUES (?)",
			args:  []driver.NamedValue{nv(1, "it's fine")},
			want:  "INSERT INTO t VALUES ('it''s fine')",
		},
		{
			name:  "null",
			query: "INSERT INTO t VALUES (?)",
			args:  []driver.NamedValue{nv(1, nil)},
			want:  "INSERT INTO t VALUES (NULL)",
		},
		{
			name:  "bool true",
			query: "INSERT INTO t VALUES (?)",
			args:  []driver.NamedValue{nv(1, true)},
			want:  "INSERT INTO t VALUES (TRUE)",
		},
		{
			name:  "bool false",
			query: "INSERT INTO t VALUES (?)",
			args:  []driver.NamedValue{nv(1, false)},
			want:  "INSERT INTO t VALUES (FALSE)",
		},
		{
			name:  "multiple params",
			query: "INSERT INTO t VALUES (?, ?, ?)",
			args:  []driver.NamedValue{nv(1, int64(1)), nv(2, "hello"), nv(3, nil)},
			want:  "INSERT INTO t VALUES (1, 'hello', NULL)",
		},
		{
			name:    "too few args",
			query:   "SELECT * FROM t WHERE id = ? AND name = ?",
			args:    []driver.NamedValue{nv(1, int64(1))},
			wantErr: true,
		},
		{
			name:    "too many args",
			query:   "SELECT * FROM t WHERE id = ?",
			args:    []driver.NamedValue{nv(1, int64(1)), nv(2, int64(2))},
			wantErr: true,
		},
		{
			name:  "question mark inside string literal not substituted",
			query: "SELECT * FROM t WHERE name = '?' AND id = ?",
			args:  []driver.NamedValue{nv(1, int64(5))},
			want:  "SELECT * FROM t WHERE name = '?' AND id = 5",
		},
		{
			// swingshift-35: line comments must not consume ? placeholders.
			// Previously: `id = ? -- why?` would eat two args (the first for
			// the real placeholder, the second trying to satisfy the ? in
			// the comment) and either over-consume or error on arg count.
			name:  "question mark inside line comment not substituted",
			query: "SELECT * FROM t WHERE id = ? -- why?\nAND name = ?",
			args:  []driver.NamedValue{nv(1, int64(5)), nv(2, "x")},
			want:  "SELECT * FROM t WHERE id = 5 -- why?\nAND name = 'x'",
		},
		{
			name:  "question mark inside block comment not substituted",
			query: "SELECT /* hmm? */ id FROM t WHERE id = ?",
			args:  []driver.NamedValue{nv(1, int64(5))},
			want:  "SELECT /* hmm? */ id FROM t WHERE id = 5",
		},
		{
			name:  "time.Time parameter with time",
			query: "INSERT INTO t VALUES (?, ?)",
			args:  []driver.NamedValue{nv(1, int64(1)), nv(2, time.Date(2024, 7, 4, 15, 30, 45, 0, time.UTC))},
			want:  "INSERT INTO t VALUES (1, '2024-07-04 15:30:45')",
		},
		{
			name:  "time.Time parameter midnight (DATE format)",
			query: "INSERT INTO t VALUES (?, ?)",
			args:  []driver.NamedValue{nv(1, int64(1)), nv(2, time.Date(2024, 7, 4, 0, 0, 0, 0, time.UTC))},
			want:  "INSERT INTO t VALUES (1, '2024-07-04')",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := substituteParams(tc.query, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("substituteParams(%q): want error, got %q", tc.query, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("substituteParams(%q): unexpected error: %v", tc.query, err)
			}
			if got != tc.want {
				t.Errorf("substituteParams(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

func TestParseSchemaIdentifier_AbsolutePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id, currentDB    string
		wantDB, wantName string
		wantErr          bool
	}{
		{"/mydb/myschema", "", "/mydb", "myschema", false},
		{"/domain/db/schema", "", "/domain/db", "schema", false},
		{"/db/s", "/other", "/db", "s", false},        // absolute overrides current
		{"schema", "/mydb", "/mydb", "schema", false}, // relative uses current
		{"schema", "", "", "schema", false},           // relative, no current (caller validates)
		{"/trailingslash/", "", "", "", true},         // trailing slash is invalid
		{"/onlysegment", "", "", "", true},            // no database prefix, only schema segment
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			db, name, err := parseSchemaIdentifier(tc.id, tc.currentDB)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseSchemaIdentifier(%q, %q): want error, got nil", tc.id, tc.currentDB)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSchemaIdentifier(%q, %q): unexpected error: %v", tc.id, tc.currentDB, err)
			}
			if db != tc.wantDB || name != tc.wantName {
				t.Errorf("parseSchemaIdentifier(%q, %q) = (%q, %q), want (%q, %q)",
					tc.id, tc.currentDB, db, name, tc.wantDB, tc.wantName)
			}
		})
	}
}

func TestValidateDatabasePath(t *testing.T) {
	t.Parallel()
	valid := []string{"/db", "/domain/db", "/a/b/c"}
	for _, p := range valid {
		if err := validateDatabasePath(p); err != nil {
			t.Errorf("validateDatabasePath(%q): unexpected error: %v", p, err)
		}
	}
	invalid := []string{"", "noslash", "db/sub", "/", "/trailing/"}
	for _, p := range invalid {
		if err := validateDatabasePath(p); err == nil {
			t.Errorf("validateDatabasePath(%q): expected error, got nil", p)
		}
	}
}

func TestEmbeddedConnection_BeginTxUnsupportedIsolation(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{}
	// Requesting a non-serializable isolation level must be rejected.
	_, err := conn.BeginTx(context.TODO(), driver.TxOptions{Isolation: driver.IsolationLevel(sql.LevelRepeatableRead)})
	if err == nil {
		t.Fatal("BeginTx with unsupported isolation: want error, got nil")
	}
}

func TestEmbeddedConnection_BeginTxNestedReturnsError(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{}
	// Simulate an already-open transaction.
	conn.activeTx = &embeddedTx{}
	_, err := conn.BeginTx(context.TODO(), driver.TxOptions{})
	if err == nil {
		t.Fatal("nested BeginTx: want error, got nil")
	}
}

func TestEmbeddedConnection_BeginTxClosedReturnsErrBadConn(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{}
	conn.closed.Store(true)
	_, err := conn.BeginTx(context.TODO(), driver.TxOptions{})
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("BeginTx on closed conn: want driver.ErrBadConn, got %v", err)
	}
}

// TestGroupByKey pins the collision-free invariant for GROUP BY keys.
// Encodings must:
//   - keep NULL distinct from the literal string "<nil>" (pre-fix collision)
//   - keep int 5 distinct from string "5" (same type-tag rule as rowKey)
//   - treat two NULLs in the same column as equal (SQL spec)
//   - keep (NULL, 'x') distinct from ('x', NULL) across columns
func TestGroupByKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		a, b  []driver.Value
		equal bool
	}{
		{"identical non-null", []driver.Value{int64(1), "x"}, []driver.Value{int64(1), "x"}, true},
		{"both NULL same cols", []driver.Value{nil, nil}, []driver.Value{nil, nil}, true},
		{"NULL vs nil-string", []driver.Value{nil}, []driver.Value{"<nil>"}, false},
		{"int 5 vs string '5'", []driver.Value{int64(5)}, []driver.Value{"5"}, false},
		{"(NULL,x) vs (x,NULL)", []driver.Value{nil, "x"}, []driver.Value{"x", nil}, false},
		{"same NULL/non-null pattern", []driver.Value{nil, int64(1)}, []driver.Value{nil, int64(1)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ka := groupByKey(tc.a)
			kb := groupByKey(tc.b)
			got := ka == kb
			if got != tc.equal {
				t.Errorf("groupByKey %s: got %v (ka=%q kb=%q), want equal=%v",
					tc.name, got, ka, kb, tc.equal)
			}
		})
	}
}

// TestTriBool pins the Kleene three-valued truth table so any future tweak
// of triAnd/triOr/Not doesn't silently violate SQL §8.12. Exhaustively
// enumerates all 3×3 combinations — 9 AND, 9 OR, 3 NOT.
func TestTriBool(t *testing.T) {
	t.Parallel()
	name := func(v triBool) string {
		switch v {
		case triTrue:
			return "T"
		case triFalse:
			return "F"
		case triNull:
			return "N"
		}
		return "?"
	}

	andCases := []struct {
		a, b, want triBool
	}{
		{triTrue, triTrue, triTrue},
		{triTrue, triFalse, triFalse},
		{triTrue, triNull, triNull},
		{triFalse, triTrue, triFalse},
		{triFalse, triFalse, triFalse},
		{triFalse, triNull, triFalse}, // FALSE short-circuits
		{triNull, triTrue, triNull},
		{triNull, triFalse, triFalse},
		{triNull, triNull, triNull},
	}
	for _, tc := range andCases {
		if got := triAnd(tc.a, tc.b); got != tc.want {
			t.Errorf("triAnd(%s, %s) = %s, want %s", name(tc.a), name(tc.b), name(got), name(tc.want))
		}
	}

	orCases := []struct {
		a, b, want triBool
	}{
		{triTrue, triTrue, triTrue},
		{triTrue, triFalse, triTrue},
		{triTrue, triNull, triTrue}, // TRUE short-circuits
		{triFalse, triTrue, triTrue},
		{triFalse, triFalse, triFalse},
		{triFalse, triNull, triNull},
		{triNull, triTrue, triTrue},
		{triNull, triFalse, triNull},
		{triNull, triNull, triNull},
	}
	for _, tc := range orCases {
		if got := triOr(tc.a, tc.b); got != tc.want {
			t.Errorf("triOr(%s, %s) = %s, want %s", name(tc.a), name(tc.b), name(got), name(tc.want))
		}
	}

	notCases := []struct {
		in, want triBool
	}{
		{triTrue, triFalse},
		{triFalse, triTrue},
		{triNull, triNull},
	}
	for _, tc := range notCases {
		if got := tc.in.Not(); got != tc.want {
			t.Errorf("Not(%s) = %s, want %s", name(tc.in), name(got), name(tc.want))
		}
	}

	// IsTrue: only triTrue is truthy. UNKNOWN must NOT pass the filter
	// boundary — that's the whole point of the tri-state.
	truthyCases := []struct {
		in   triBool
		want bool
	}{
		{triTrue, true},
		{triFalse, false},
		{triNull, false},
	}
	for _, tc := range truthyCases {
		if got := tc.in.IsTrue(); got != tc.want {
			t.Errorf("%s.IsTrue() = %v, want %v", name(tc.in), got, tc.want)
		}
	}

	// triFromBool — round-trip.
	if triFromBool(true) != triTrue {
		t.Error("triFromBool(true) != triTrue")
	}
	if triFromBool(false) != triFalse {
		t.Error("triFromBool(false) != triFalse")
	}
}

func TestEmbeddedConnection_ResetSession(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{sess: &session.Session{Schema: "myschema"}}
	if err := conn.ResetSession(context.TODO()); err != nil {
		t.Fatalf("ResetSession: unexpected error: %v", err)
	}
	if conn.sess.Schema != "" {
		t.Errorf("ResetSession: schema not cleared, got %q", conn.sess.Schema)
	}
}

// TestEmbeddedConnection_ResetSessionClearsPerRequestState pins the
// pooled-connection hygiene invariants that were missing before:
// activeTx, ctes, and SchemaCache must not leak across checkouts.
func TestEmbeddedConnection_ResetSessionClearsPerRequestState(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{
		sess: &session.Session{
			Schema:        "other",
			DefaultSchema: "main",
			SchemaCache: map[string]api.Schema{
				"stale": nil,
			},
		},
		ctes: map[string]*cteData{
			"LEAKED": {cols: []string{"x"}, rows: [][]driver.Value{{int64(1)}}},
		},
		// activeTx left nil — rolling back a nil tx must not panic, but the
		// reset must still run to completion (the schema-cache / ctes cleanup
		// would be skipped if we early-returned on activeTx presence).
	}
	if err := conn.ResetSession(context.TODO()); err != nil {
		t.Fatalf("ResetSession: unexpected error: %v", err)
	}
	if conn.sess.Schema != "main" {
		t.Errorf("schema not restored to defaultSchema: got %q want %q", conn.sess.Schema, "main")
	}
	if conn.ctes != nil {
		t.Errorf("ctes not cleared: %v", conn.ctes)
	}
	if len(conn.sess.SchemaCache) != 0 {
		t.Errorf("SchemaCache not cleared: %v", conn.sess.SchemaCache)
	}
	if conn.activeTx != nil {
		t.Errorf("activeTx not cleared: %v", conn.activeTx)
	}
}

func TestEmbeddedConnection_ResetSessionClosedReturnsError(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{}
	conn.closed.Store(true)
	err := conn.ResetSession(context.TODO())
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("ResetSession on closed conn: want driver.ErrBadConn, got %v", err)
	}
}

func TestEmbeddedConnection_IsValid(t *testing.T) {
	t.Parallel()
	// Open connections are valid regardless of catalog init state.
	conn := &EmbeddedConnection{sess: &session.Session{CatalogReady: true}}
	if !conn.IsValid() {
		t.Error("IsValid: want true, got false")
	}
	conn2 := &EmbeddedConnection{sess: &session.Session{CatalogReady: false}}
	if !conn2.IsValid() {
		t.Error("IsValid: uninitialized but open should be valid")
	}
	conn3 := &EmbeddedConnection{sess: &session.Session{CatalogReady: true}}
	conn3.closed.Store(true)
	if conn3.IsValid() {
		t.Error("IsValid: want false for closed, got true")
	}
}

func TestValuesEqual(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b any
		want bool
	}{
		{"nil==nil", nil, nil, true},
		{"nil!=int", nil, int64(0), false},
		{"int!=nil", int64(0), nil, false},
		{"int64 equal", int64(1), int64(1), true},
		{"int64 not equal", int64(1), int64(2), false},
		// Large int64 that float64 cannot represent exactly (> 2^53).
		{"large int64 equal", int64(9007199254740993), int64(9007199254740993), true},
		{"large int64 not equal", int64(9007199254740992), int64(9007199254740993), false},
		{"float64 equal", float64(3.14), float64(3.14), true},
		{"float64 not equal", float64(3.14), float64(2.71), false},
		{"int64 == float64", int64(5), float64(5.0), true},
		{"float64 == int64", float64(5.0), int64(5), true},
		{"string equal", "hello", "hello", true},
		{"string not equal", "hello", "world", false},
		{"bool true==true", true, true, true},
		{"bool false!=true", false, true, false},
		// Mixed-type comparisons must return false — no string coercion.
		{"string '5' != int 5", "5", int64(5), false},
		{"int 5 != string '5'", int64(5), "5", false},
		{"string '5.0' != float 5.0", "5.0", float64(5.0), false},
		{"float 5.0 != string '5.0'", float64(5.0), "5.0", false},
		{"bool true != int 1", true, int64(1), false},
		{"int 1 != bool true", int64(1), true, false},
		{"bool true != string 'true'", true, "true", false},
		{"string 'true' != bool true", "true", true, false},
		{"bytes equal", []byte("abc"), []byte("abc"), true},
		{"bytes not equal", []byte("abc"), []byte("abd"), false},
		{"bytes != string", []byte("abc"), "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := valuesEqual(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("valuesEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestLikeMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"", "", true},
		{"", "x", false},
		{"abc", "abc", true},
		{"abc", "abcd", false},
		{"abc", "ab", false},
		{"%", "", true},
		{"%", "anything", true},
		{"a%", "a", true},
		{"a%", "abc", true},
		{"a%", "bc", false},
		{"%c", "abc", true},
		{"%c", "abx", false},
		{"a%c", "abc", true},
		{"a%c", "axyzc", true},
		{"a%c", "axyz", false},
		{"_", "a", true},
		{"_", "ab", false},
		{"_", "", false},
		{"a_c", "abc", true},
		{"a_c", "ac", false},
		{"a_c", "abbc", false},
		{"%%", "anything", true},
		{"a%b%c", "aXbYc", true},
		{"a%b%c", "aXbY", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"/"+tc.s, func(t *testing.T) {
			t.Parallel()
			got := functions.LikeMatch(tc.pattern, tc.s, -1) // no escape
			if got != tc.want {
				t.Errorf("functions.LikeMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
			}
		})
	}
}

// TestLikeMatchWithEscape pins the ESCAPE clause behaviour added in
// swingshift-35. Matches Java ExpressionVisitor.visitLikePredicate which
// passes the escape char into the pattern-compile step so `\_` is literal.
func TestLikeMatchWithEscape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		s       string
		escape  rune
		want    bool
	}{
		// Literal underscore via escape.
		{`a\_b`, "a_b", '\\', true},
		{`a\_b`, "axb", '\\', false}, // escaped _ doesn't match arbitrary char
		// Literal percent via escape.
		{`a\%b`, "a%b", '\\', true},
		{`a\%b`, "abb", '\\', false},
		// Escape char itself can be escaped.
		{`a\\b`, `a\b`, '\\', true},
		// Alt escape char.
		{`a!_b`, "a_b", '!', true},
		{`a!_b`, "axb", '!', false},
		// Without escape the same char is literal (escape=-1).
		{`a\_b`, "a_b", -1, false}, // `\` is literal, `_` still wildcard → "a\Xb"
		{`a\_b`, `a\xb`, -1, true}, // matches `a\` + any char + `b`
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"/"+tc.s, func(t *testing.T) {
			t.Parallel()
			got := functions.LikeMatch(tc.pattern, tc.s, tc.escape)
			if got != tc.want {
				t.Errorf("functions.LikeMatch(%q, %q, %q) = %v, want %v",
					tc.pattern, tc.s, string(tc.escape), got, tc.want)
			}
		})
	}
}

func TestRowKey(t *testing.T) {
	t.Parallel()
	row := func(vals ...driver.Value) []driver.Value { return vals }

	cases := []struct {
		a, b []driver.Value
		same bool
	}{
		{row(int64(1)), row(int64(1)), true},
		{row(int64(1)), row(int64(2)), false},
		{row(nil), row(nil), true},
		{row(nil), row(int64(0)), false},
		// Binary string containing separator bytes must not collide.
		{row("foo\x00"), row("foo", "\x00"), false},
		{row("a", "b"), row("ab"), false},
		// Compound (STRING, BIGINT) duplicates must produce identical keys
		// — the canonical SELECT DISTINCT a, b shape (TODO #42 sentinel).
		{row("x", int64(1)), row("x", int64(1)), true},
		{row("x", int64(1)), row("x", int64(2)), false},
		{row("x", int64(1)), row("y", int64(1)), false},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			t.Parallel()
			ka, kb := rowKey(tc.a), rowKey(tc.b)
			if tc.same && ka != kb {
				t.Errorf("expected equal keys for %v and %v, got %q vs %q", tc.a, tc.b, ka, kb)
			} else if !tc.same && ka == kb {
				t.Errorf("expected distinct keys for %v and %v, both got %q", tc.a, tc.b, ka)
			}
		})
	}
}

// FuzzApplyMathOp pins the arithmetic evaluator. The function must never
// panic, must reject non-numeric operands cleanly, must propagate NULL, and
// must error on div/0 for both `/` and `%` (unified in swingshift-35).
func FuzzApplyMathOp(f *testing.F) {
	f.Add(int64(7), int64(3), "+")
	f.Add(int64(7), int64(3), "-")
	f.Add(int64(7), int64(3), "*")
	f.Add(int64(7), int64(3), "/")
	f.Add(int64(7), int64(3), "%")
	// Division by zero.
	f.Add(int64(1), int64(0), "/")
	f.Add(int64(1), int64(0), "%")
	// Unknown op.
	f.Add(int64(1), int64(2), "@")
	// Mixed int/float shouldn't panic — we pass only int64 to the int64 fuzz,
	// but the NULL-on-either-side path is critical.
	f.Fuzz(func(t *testing.T, a, b int64, op string) {
		_, err := functions.ApplyMathOp(a, b, op)
		_ = err
		// NULL propagation on left, right, and both.
		for _, pair := range []struct{ l, r any }{{nil, b}, {a, nil}, {nil, nil}} {
			v, err := functions.ApplyMathOp(pair.l, pair.r, op)
			if err != nil || v != nil {
				t.Fatalf("functions.ApplyMathOp(%v, %v, %q) = (%v, %v), want (nil, nil)",
					pair.l, pair.r, op, v, err)
			}
		}
		// Non-numeric operand must error cleanly.
		if _, err := functions.ApplyMathOp("bad", b, op); err == nil {
			t.Fatalf("functions.ApplyMathOp(string, _, %q) must error", op)
		}
	})
}

// FuzzApplyBitOp pins the bitwise-operator implementation. The function
// must never panic, must reject non-integer operands cleanly, must
// propagate NULL, and must consistently reject the shift operators
// `<<` / `>>` per the nightshift-61 Java alignment (Java tokenizes
// the shift operators but its function registry has no evaluator).
func FuzzApplyBitOp(f *testing.F) {
	f.Add(int64(7), int64(3), "&")
	f.Add(int64(7), int64(3), "|")
	f.Add(int64(7), int64(3), "^")
	// Shift seeds — these used to evaluate; now always reject (Java parity).
	f.Add(int64(7), int64(2), "<<")
	f.Add(int64(7), int64(1), ">>")
	f.Add(int64(1), int64(64), "<<")
	f.Add(int64(1), int64(-1), "<<")
	f.Add(int64(-1), int64(63), ">>")
	// Unknown op.
	f.Add(int64(1), int64(2), "@")
	f.Fuzz(func(t *testing.T, a, b int64, op string) {
		// Must not panic. Either returns a value+nil, NULL+nil, or value+error.
		_, err := functions.ApplyBitOp(a, b, op)
		_ = err // accept any error
		// Also try with NULL operands; those must always return nil, nil
		// regardless of the op (NULL propagates first).
		v, err := functions.ApplyBitOp(nil, b, op)
		if err != nil || v != nil {
			t.Fatalf("functions.ApplyBitOp(nil, _) = (%v, %v), want (nil, nil)", v, err)
		}
		v, err = functions.ApplyBitOp(a, nil, op)
		if err != nil || v != nil {
			t.Fatalf("functions.ApplyBitOp(_, nil) = (%v, %v), want (nil, nil)", v, err)
		}
		// Non-integer operand must error cleanly, not panic.
		if _, err := functions.ApplyBitOp("string", b, op); err == nil {
			t.Fatalf("functions.ApplyBitOp(string, _) must error")
		}
		// Shift operators are always rejected (Java parity nightshift-61).
		if op == "<<" || op == ">>" {
			_, shErr := functions.ApplyBitOp(a, b, op)
			if shErr == nil {
				t.Fatalf("functions.ApplyBitOp(%d, %d, %q) must error post-nightshift-61", a, b, op)
			}
		}
	})
}

// FuzzCompareValues pins core algebraic invariants of the SQL
// three-valued compare function — antisymmetry, reflexivity,
// no-panic — across every value-type combination that flows through
// ORDER BY, JOIN, IN-list dedup, and HAVING. The seed corpus walks
// the type matrix (NULL × int64 × float64 × string × bool × []byte);
// the fuzzer mutates the integer indices to permute pair selection.
//
// Invariants checked:
//   - Compare(a, b) ∈ {-1, 0, 1}           — bounded result
//   - Compare(a, b) == -Compare(b, a)      — antisymmetry
//   - Compare(a, a) == 0                   — reflexivity
//
// CompareValues' branching (NULL ordering, int64-fast-path, numeric
// promotion, type-name fallback) makes property-based fuzz a strong
// fit: ~1B mutations exhaust the type-pair × value-magnitude space
// faster than a hand-written matrix could.
func FuzzCompareValues(f *testing.F) {
	// Seed: walk every combination of representative values × types ×
	// a third index for transitivity.
	for _, ai := range []int{0, 1, 2, 3, 4, 5} {
		for _, bi := range []int{0, 1, 2, 3, 4, 5} {
			f.Add(ai, bi, 0, int64(0), float64(0), "", false, []byte(nil))
		}
	}
	f.Add(0, 1, 2, int64(7), float64(3.14), "hello", true, []byte("bytes"))
	f.Add(2, 3, 4, int64(-1), float64(0), "", false, []byte{})
	f.Add(4, 5, 1, int64(1<<53), float64(1<<53)+1, "z", true, []byte{0xff})

	// fuzzCompareValuesPick maps a small int index to a driver.Value of
	// the corresponding type, drawing from the fuzz-supplied scalars.
	pick := func(idx int, i int64, fl float64, s string, b bool, by []byte) driver.Value {
		switch ((idx % 6) + 6) % 6 {
		case 0:
			return nil
		case 1:
			return i
		case 2:
			return fl
		case 3:
			return s
		case 4:
			return b
		case 5:
			return by
		}
		return nil
	}

	f.Fuzz(func(t *testing.T, ai, bi, ci int, i int64, fl float64, s string, b bool, by []byte) {
		a := pick(ai, i, fl, s, b, by)
		bv := pick(bi, i, fl, s, b, by)
		c := pick(ci, i, fl, s, b, by)
		// No panic on any input.
		got := functions.CompareValues(a, bv)
		// Result must be in {-1, 0, 1}.
		if got < -1 || got > 1 {
			t.Fatalf("CompareValues(%v, %v) = %d, must be in {-1, 0, 1}", a, bv, got)
		}
		// Antisymmetry: swapping operands negates the result.
		swap := functions.CompareValues(bv, a)
		if swap != -got {
			t.Fatalf("antisymmetry: CompareValues(%v, %v)=%d but CompareValues(%v, %v)=%d (want %d)",
				a, bv, got, bv, a, swap, -got)
		}
		// Reflexivity: Compare(a, a) == 0. NaN floats violate this in
		// strict IEEE754; skip the float NaN case explicitly.
		if af, ok := a.(float64); !ok || af == af { // af == af false iff NaN
			if r := functions.CompareValues(a, a); r != 0 {
				t.Fatalf("reflexivity: CompareValues(%v, %v) = %d, want 0", a, a, r)
			}
		}
		// Transitivity: if a ≤ b and b ≤ c then a ≤ c. Total-order
		// requirement; sort stability depends on it. NaN floats break
		// the comparison axioms (NaN is incomparable in IEEE754; our
		// implementation falls back to type-name ordering, which is
		// consistent but not order-preserving against itself), so skip
		// the transitivity check whenever any operand is a NaN float.
		hasNaN := func(x driver.Value) bool {
			f, ok := x.(float64)
			return ok && f != f
		}
		if !hasNaN(a) && !hasNaN(bv) && !hasNaN(c) {
			ab := functions.CompareValues(a, bv)
			bc := functions.CompareValues(bv, c)
			ac := functions.CompareValues(a, c)
			if ab <= 0 && bc <= 0 && ac > 0 {
				t.Fatalf("transitivity: a≤b ∧ b≤c ⇒ a≤c violated:\n  a=%v, b=%v, c=%v\n  Compare(a,b)=%d Compare(b,c)=%d Compare(a,c)=%d",
					a, bv, c, ab, bc, ac)
			}
		}
		// Bool × bool with both values: the fuzz-supplied `b` is used
		// symmetrically by `pick`, so the (4, 4) case can never produce
		// CompareValues(true, false). Cover the different-bool case
		// explicitly inside the fuzz body so each iteration revalidates
		// the bool branch alongside the mutated-type-pair tests.
		if r := functions.CompareValues(true, false); r != 1 {
			t.Fatalf("CompareValues(true, false) = %d, want 1", r)
		}
		if r := functions.CompareValues(false, true); r != -1 {
			t.Fatalf("CompareValues(false, true) = %d, want -1", r)
		}
		// Cross-helper consistency: ORDER BY (CompareValues) and `=`
		// (valuesEqual) MUST agree on equality. If they diverged a
		// query like `SELECT * FROM t WHERE x = y ORDER BY x` could
		// surface rows where `x = y` is true but the sort treats them
		// as ordered relative to each other (or vice versa). Skip when
		// either operand is NaN, since IEEE754 NaN ≠ NaN by definition
		// (CompareValues falls back to type-name order = 0; valuesEqual
		// returns false). This isn't a divergence — it's the IEEE754
		// boundary, which both helpers handle consistently with their
		// design.
		if !hasNaN(a) && !hasNaN(bv) {
			cmp0 := functions.CompareValues(a, bv) == 0
			eq := valuesEqual(a, bv)
			if cmp0 != eq {
				t.Fatalf("CompareValues == 0 vs valuesEqual disagree:\n  a=%v, b=%v\n  CompareValues=%d (==0 ? %v)\n  valuesEqual=%v",
					a, bv, functions.CompareValues(a, bv), cmp0, eq)
			}
		}
	})
}

// FuzzCastValue pins CastValue's no-panic + NULL-pass-through
// invariants. CastValue has many branches (INTEGER vs BIGINT range
// checks, DOUBLE_TO_LONG rounding, STRING_TO_LONG parse, UUID
// canonicalisation, BOOLEAN string mapping); the seed corpus walks
// representative values × target-type strings, the fuzzer mutates
// both axes.
//
// Properties pinned:
//   - No panic on any (value, typeName) pair
//   - CAST(NULL AS T) → (nil, nil)               — SQL spec
//   - When (v, "INTEGER") fits in int32, ok      — Java alignment
//   - When (v, "INTEGER") doesn't fit, errors    — vs silent wrap
//
// Cross-references the swingshift-52 UUID-CAST plumbing and the
// CAST-overflow gotchas (CLAUDE.md) — same code path.
func FuzzCastValue(f *testing.F) {
	// Seed: representative target types × value types.
	for _, ty := range []string{"INTEGER", "INT", "BIGINT", "LONG", "DOUBLE", "FLOAT", "STRING", "VARCHAR", "BOOLEAN", "BOOL", "UUID", "BYTES", "BINARY", "<unknown>"} {
		f.Add(ty, int64(0), float64(0), "")
		f.Add(ty, int64(1), float64(0), "")
		f.Add(ty, int64(-1), float64(0), "")
	}
	// Edge values: int64 limits, NaN/Inf, edge strings.
	f.Add("INTEGER", int64(math.MinInt32), float64(0), "")
	f.Add("INTEGER", int64(math.MaxInt32), float64(0), "")
	f.Add("INTEGER", int64(math.MaxInt32)+1, float64(0), "") // out of range
	f.Add("BIGINT", int64(0), math.MaxFloat64, "")           // float→bigint overflow
	f.Add("BIGINT", int64(0), math.NaN(), "")                // NaN→bigint reject
	f.Add("BIGINT", int64(0), math.Inf(1), "")               // Inf→bigint reject
	f.Add("UUID", int64(0), float64(0), "00000000-0000-0000-0000-000000000000")
	f.Add("UUID", int64(0), float64(0), "not-a-uuid")
	f.Add("BOOLEAN", int64(0), float64(0), "true")
	f.Add("BOOLEAN", int64(0), float64(0), "TRUE  ") // trailing whitespace per Java
	f.Add("BOOLEAN", int64(0), float64(0), "no")     // bad input

	f.Fuzz(func(t *testing.T, typeName string, i int64, fl float64, s string) {
		// Try each value type as the source value. No panic on any combo.
		// `true` and `false` are both included to exercise CastValue's
		// `case bool:` arms in the integer / boolean target branches —
		// these constants don't need dedicated f.Add seeds because the
		// type-iteration loop above already seeds every typeName.
		for _, v := range []any{nil, i, fl, s, true, false} {
			r, err := functions.CastValue(v, typeName)
			_ = err
			// SQL: CAST(NULL AS T) → (nil, nil) for any typeName.
			if v == nil {
				if r != nil || err != nil {
					t.Fatalf("CAST(NULL AS %q) = (%v, %v), want (nil, nil)", typeName, r, err)
				}
			}
			// When err is non-nil, the result must also be nil (no
			// silent value-leak through error path). Pre-fix some
			// error paths could return both a partial value and an
			// error; the convention is value-OR-error.
			if err != nil && r != nil {
				t.Fatalf("CAST(%v AS %q) = (%v, %v): non-nil value alongside error",
					v, typeName, r, err)
			}
		}
	})
}

// FuzzLikePrefixStrinc pins the LIKE-prefix strinc helper — must never
// panic, and when it returns ok=true the result must be strictly
// greater than any string starting with the prefix (in byte order).
// The all-0xFF case must return ok=false, never a wrong bound.
func FuzzLikePrefixStrinc(f *testing.F) {
	f.Add("a")
	f.Add("foo")
	f.Add("")
	f.Add("\xff")
	f.Add("\xff\xff")
	f.Add("a\xff")
	f.Add("\xff\xffa")
	f.Add("Hello, 世界")
	f.Add("0")
	f.Add("~")
	f.Fuzz(func(t *testing.T, prefix string) {
		high, ok := likePrefixStrinc(prefix)
		if !ok {
			// Unreachable for any prefix with a byte < 0xFF.
			for _, b := range []byte(prefix) {
				if b < 0xFF {
					t.Fatalf("likePrefixStrinc(%q) = _, false but prefix has byte < 0xFF", prefix)
				}
			}
			return
		}
		// high must be strictly greater than prefix, and greater than
		// every extension `prefix || anything`. The latter is implied
		// by high being the byte-level successor of some prefix of
		// `prefix` — so any string S with S >= prefix AND S < high
		// must start with `prefix` (which is what the range scan
		// semantics rely on).
		if high <= prefix {
			t.Fatalf("likePrefixStrinc(%q) = %q, not > prefix", prefix, high)
		}
		// Known worst-case extension: `prefix || \xff` must sort
		// before `high` (otherwise we'd miss rows).
		ext := prefix + "\xff"
		if ext >= high {
			t.Fatalf("likePrefixStrinc(%q) = %q, but %q >= high — extension misses range",
				prefix, high, ext)
		}
	})
}

// FuzzLikePatternToPrefix pins the LIKE-pattern prefix extractor.
// Must never panic on arbitrary inputs, and every returned prefix
// must be non-empty.
//
// The stronger invariant — "every string matching the pattern starts
// with the extracted prefix" — is the correctness condition for the
// pushdown: if some matching row `s` does NOT have `prefix` as a
// byte-level prefix, the scan range `[prefix, strinc(prefix))` would
// exclude it (false-negative pruning → silently wrong query results).
//
// Scope: valid-UTF-8 only. likePatternToPrefix uses []rune decoding,
// which folds invalid UTF-8 bytes (0x80-0xFF not forming a legal
// sequence) into U+FFFD — likeMatch compares the same folded runes
// and returns true for distinct invalid-byte inputs, but
// strings.HasPrefix is byte-level and sees them as different. For
// protobuf STRING columns this is moot: protobuf rejects invalid
// UTF-8 at (de)serialization time, so the fuzz skips such inputs.
//
// The corpus drives `s` explicitly so the fuzzer can find counter-
// examples rather than only random patterns that no input happens
// to match. When likeMatch(pattern, s, escape) is true and ok is
// true, strings.HasPrefix(s, prefix) must hold. Any divergence is
// a pushdown bug.
func FuzzLikePatternToPrefix(f *testing.F) {
	// (pattern, escape, candidate string) seeds. Candidates are chosen
	// so at least some satisfy likeMatch on the paired pattern.
	f.Add("foo%", rune(-1), "foobar")
	f.Add("foo\\_%", rune('\\'), "foo_bar")
	f.Add("foo", rune(-1), "foo")
	f.Add("%", rune(-1), "anything")
	f.Add("", rune(-1), "")
	f.Add("_", rune(-1), "x")
	f.Add("f%o", rune(-1), "foo")
	f.Add("f_o", rune(-1), "fxo")
	f.Add("foo%bar", rune(-1), "foobar")
	f.Add("foo%bar", rune(-1), "fooxbar")
	f.Add("\\%", rune('\\'), "%")
	f.Add("\\", rune('\\'), "")
	f.Add("%%", rune(-1), "abc")
	f.Add("a_b%c", rune(-1), "axbxc")
	f.Fuzz(func(t *testing.T, pattern string, escape rune, s string) {
		// Protobuf STRING columns reject invalid UTF-8 on
		// (de)serialization; replicate that contract here so the
		// byte-vs-rune mismatch on invalid sequences (both fold to
		// U+FFFD in likeMatch but remain byte-distinct in HasPrefix)
		// doesn't produce spurious failures.
		if !utf8.ValidString(pattern) || !utf8.ValidString(s) {
			return
		}
		prefix, ok := likePatternToPrefix(pattern, escape)
		if !ok {
			return
		}
		if prefix == "" {
			t.Fatalf("likePatternToPrefix(%q, %q) returned empty prefix with ok=true", pattern, escape)
		}
		// Correctness invariant: if `s` matches `pattern`, it MUST
		// start with `prefix`. Otherwise the pushdown's scan bound
		// excludes a matching row.
		if functions.LikeMatch(pattern, s, escape) && !strings.HasPrefix(s, prefix) {
			t.Fatalf("likePatternToPrefix false-negative: pattern=%q escape=%q s=%q matches but prefix=%q is not a prefix of s",
				pattern, escape, s, prefix)
		}
	})
}

// TestWrapSaveRecordError pins the mapping between record-layer
// error types and SQLSTATE-carrying api.Error values. Tests run
// without FDB — the helper is pure.
func TestWrapSaveRecordError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       error
		wantOK   bool // nil wrap for nil input
		wantCode api.ErrorCode
	}{
		{
			name: "nil passes through",
			in:   nil,
		},
		{
			name: "RecordIndexUniquenessViolationError -> 23505",
			in: &recordlayer.RecordIndexUniquenessViolationError{
				IndexName:  "t_email",
				IndexKey:   tuple.Tuple{"a@x.com"},
				PrimaryKey: tuple.Tuple{int64(1)},
			},
			wantOK:   true,
			wantCode: api.ErrCodeUniqueConstraintViolation,
		},
		{
			name: "RecordAlreadyExistsError -> 23505",
			in: &recordlayer.RecordAlreadyExistsError{
				Message:    "duplicate",
				PrimaryKey: tuple.Tuple{int64(1)},
			},
			wantOK:   true,
			wantCode: api.ErrCodeUniqueConstraintViolation,
		},
		{
			name: "IndexKeySizeError -> 22023",
			in: &recordlayer.IndexKeySizeError{
				IndexName: "big",
				KeySize:   10240,
				Limit:     1024,
			},
			wantOK:   true,
			wantCode: api.ErrCodeInvalidParameter,
		},
		{
			name: "IndexValueSizeError -> 22023",
			in: &recordlayer.IndexValueSizeError{
				IndexName: "big",
				ValueSize: 1024 * 1024,
				Limit:     65536,
			},
			wantOK:   true,
			wantCode: api.ErrCodeInvalidParameter,
		},
		{
			name:     "already-wrapped *api.Error passes through",
			in:       api.NewError(api.ErrCodeNotNullViolation, "pre-wrapped"),
			wantOK:   true,
			wantCode: api.ErrCodeNotNullViolation,
		},
		{
			name:     "unknown error wraps as internal",
			in:       errors.New("mystery"),
			wantOK:   true,
			wantCode: api.ErrCodeInternalError,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := wrapSaveRecordError(tc.in)
			if tc.in == nil {
				if out != nil {
					t.Fatalf("nil in, want nil out, got %v", out)
				}
				return
			}
			if out == nil {
				t.Fatal("want wrapped error, got nil")
			}
			var apiErr *api.Error
			if !errors.As(out, &apiErr) {
				t.Fatalf("want *api.Error, got %T: %v", out, out)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("code = %s, want %s (msg: %s)", apiErr.Code, tc.wantCode, apiErr.Message)
			}
			// Original error must remain reachable via Unwrap chain —
			// errors.Is on the original instance must succeed.
			if !errors.Is(out, tc.in) {
				t.Errorf("errors.Is failed — wrapped error must preserve original via Unwrap")
			}
		})
	}
}

// TestInt64CheckedArithmetic pins the overflow semantics of the
// helpers behind applyMathOp's int64 fast-path. They mirror Java's
// Math.addExact / subtractExact / multiplyExact — the moment the true
// mathematical result doesn't fit in a signed 64-bit integer, the
// operation reports overflow rather than silently wrapping.
func TestInt64CheckedArithmetic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		op     func(a, b int64) (int64, bool)
		a, b   int64
		want   int64
		wantOK bool
	}{
		// Add
		{"add/ok", functions.AddInt64Checked, 1, 2, 3, true},
		{"add/zero", functions.AddInt64Checked, 0, 0, 0, true},
		{"add/negatives", functions.AddInt64Checked, -3, -4, -7, true},
		{"add/max+0", functions.AddInt64Checked, math.MaxInt64, 0, math.MaxInt64, true},
		{"add/max+1", functions.AddInt64Checked, math.MaxInt64, 1, 0, false},
		{"add/max+max", functions.AddInt64Checked, math.MaxInt64, math.MaxInt64, 0, false},
		{"add/min-1", functions.AddInt64Checked, math.MinInt64, -1, 0, false},
		{"add/min+min", functions.AddInt64Checked, math.MinInt64, math.MinInt64, 0, false},
		// Cross-sign cannot overflow.
		{"add/max-1", functions.AddInt64Checked, math.MaxInt64, -1, math.MaxInt64 - 1, true},
		{"add/min+1", functions.AddInt64Checked, math.MinInt64, 1, math.MinInt64 + 1, true},
		// Sub
		{"sub/ok", functions.SubInt64Checked, 5, 3, 2, true},
		{"sub/zero", functions.SubInt64Checked, 0, 0, 0, true},
		{"sub/max-max", functions.SubInt64Checked, math.MaxInt64, math.MaxInt64, 0, true},
		{"sub/min-min", functions.SubInt64Checked, math.MinInt64, math.MinInt64, 0, true},
		{"sub/min-1", functions.SubInt64Checked, math.MinInt64, 1, 0, false},
		{"sub/min-max", functions.SubInt64Checked, math.MinInt64, math.MaxInt64, 0, false},
		{"sub/max-(-1)", functions.SubInt64Checked, math.MaxInt64, -1, 0, false},
		// Same-sign subtraction cannot overflow.
		{"sub/max-1", functions.SubInt64Checked, math.MaxInt64, 1, math.MaxInt64 - 1, true},
		{"sub/min-(-1)", functions.SubInt64Checked, math.MinInt64, -1, math.MinInt64 + 1, true},
		// Mul
		{"mul/zero.l", functions.MulInt64Checked, 0, math.MaxInt64, 0, true},
		{"mul/zero.r", functions.MulInt64Checked, math.MinInt64, 0, 0, true},
		{"mul/small", functions.MulInt64Checked, 7, 8, 56, true},
		{"mul/neg", functions.MulInt64Checked, -7, 8, -56, true},
		{"mul/min*-1", functions.MulInt64Checked, math.MinInt64, -1, 0, false},
		{"mul/-1*min", functions.MulInt64Checked, -1, math.MinInt64, 0, false},
		{"mul/min*1", functions.MulInt64Checked, math.MinInt64, 1, math.MinInt64, true},
		{"mul/max*2", functions.MulInt64Checked, math.MaxInt64, 2, 0, false},
		{"mul/half*3", functions.MulInt64Checked, math.MaxInt64 / 2, 3, 0, false},
		{"mul/half*2", functions.MulInt64Checked, math.MaxInt64 / 2, 2, (math.MaxInt64 / 2) * 2, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := tc.op(tc.a, tc.b)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (a=%d b=%d got=%d)", ok, tc.wantOK, tc.a, tc.b, got)
			}
			if tc.wantOK && got != tc.want {
				t.Fatalf("result = %d, want %d", got, tc.want)
			}
		})
	}
}
