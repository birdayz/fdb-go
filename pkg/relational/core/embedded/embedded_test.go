package embedded

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
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

func TestEmbeddedConnection_ResetSession(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{schema: "myschema"}
	if err := conn.ResetSession(context.TODO()); err != nil {
		t.Fatalf("ResetSession: unexpected error: %v", err)
	}
	if conn.schema != "" {
		t.Errorf("ResetSession: schema not cleared, got %q", conn.schema)
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
	conn := &EmbeddedConnection{catalogReady: true}
	if !conn.IsValid() {
		t.Error("IsValid: want true, got false")
	}
	conn2 := &EmbeddedConnection{catalogReady: false}
	if !conn2.IsValid() {
		t.Error("IsValid: uninitialized but open should be valid")
	}
	conn3 := &EmbeddedConnection{catalogReady: true}
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
			got := likeMatch(tc.pattern, tc.s)
			if got != tc.want {
				t.Errorf("likeMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
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
