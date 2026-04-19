package embedded

import (
	"context"
	"database/sql/driver"
	"errors"
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

func TestEmbeddedConnection_BeginTxReturnsUnsupported(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{}
	_, err := conn.BeginTx(context.TODO(), driver.TxOptions{})
	if err == nil {
		t.Fatal("BeginTx: want error, got nil")
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
