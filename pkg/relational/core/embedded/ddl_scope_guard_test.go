package embedded

// Unit-level pins for the two RESTRICT_DDL_TO_SESSION_DATABASE guards. Neither
// property is visible from a SQL-level test:
//
//   - checkDDLDatabaseScope must fail CLOSED on a session path that scopes
//     nothing. The bare root "/" is the dangerous spelling: it trims to "" and
//     every database path starts with "/", so a scope predicate that admitted
//     it would grant authority over the entire cluster — the inverse of what
//     the restriction is for. Not reachable through the sql.Driver (ParseDSN
//     rejects a DSN whose path is "/" as missing), so it is pinned here.
//
//   - checkSchemaTemplateDDLAllowed must refuse ALL schema-template DDL on a
//     restricted connection, because templates are cluster-global and have no
//     owning database to scope against.

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/session"
)

// scopeTestConn builds the minimum connection state both guards read: the
// session database path and the per-connection options.
func scopeTestConn(dbPath string, restrict bool) *EmbeddedConnection {
	c := &EmbeddedConnection{sess: &session.Session{DBPath: dbPath}}
	if restrict {
		c.SetOptions(api.NoOptions().With(api.OptRestrictDDLToSessionDatabase, true))
	}
	return c
}

func TestScopePrefixOrEmpty(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want string
	}{
		{"/tenant-a", "/tenant-a"},
		{"/tenant-a/sub", "/tenant-a/sub"},
		{"/tenant-a/", "/tenant-a/"},
		// The two spellings that scope nothing.
		{"", ""},
		{"/", ""},
	}
	for _, c := range cases {
		if got := scopePrefixOrEmpty(c.in); got != c.want {
			t.Errorf("scopePrefixOrEmpty(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCheckDDLDatabaseScope(t *testing.T) {
	t.Parallel()

	t.Run("unrestricted allows everything", func(t *testing.T) {
		t.Parallel()
		// The Java-parity default: no option, no check, any target allowed —
		// including from a session with an unscopable path.
		for _, sess := range []string{"/tenant-a", "", "/"} {
			c := scopeTestConn(sess, false)
			if err := c.checkDDLDatabaseScope("DROP DATABASE", "/tenant-b"); err != nil {
				t.Errorf("session %q unrestricted: got %v, want nil", sess, err)
			}
		}
	})

	t.Run("restricted allows own and nested", func(t *testing.T) {
		t.Parallel()
		c := scopeTestConn("/tenant-a", true)
		for _, target := range []string{"/tenant-a", "/tenant-a/sub"} {
			if err := c.checkDDLDatabaseScope("CREATE SCHEMA", target); err != nil {
				t.Errorf("target %q: got %v, want nil", target, err)
			}
		}
	})

	t.Run("restricted rejects foreign and lookalike", func(t *testing.T) {
		t.Parallel()
		c := scopeTestConn("/tenant-a", true)
		for _, target := range []string{"/tenant-b", "/tenant-abc", "/tenant-a_extra"} {
			err := c.checkDDLDatabaseScope("DROP DATABASE", target)
			assertScopeRejected(t, err, "/tenant-a", target)
		}
	})

	// The fail-closed direction. A session that scopes nothing must confer no
	// authority — NOT authority over everything.
	t.Run("restricted unscopable session rejects everything", func(t *testing.T) {
		t.Parallel()
		for _, sess := range []string{"", "/"} {
			c := scopeTestConn(sess, true)
			for _, target := range []string{"/tenant-a", "/tenant-a/sub", "/__SYS", "/"} {
				err := c.checkDDLDatabaseScope("DROP DATABASE", target)
				if err == nil {
					t.Fatalf("session %q, target %q: got nil, want rejection — an "+
						"unscopable session path must not grant cluster-wide DDL", sess, target)
				}
				assertScopeRejected(t, err, sess, target)
			}
		}
	})
}

func assertScopeRejected(t *testing.T, err error, wantSession, wantTarget string) {
	t.Helper()
	if err == nil {
		t.Fatalf("target %q: expected rejection, got nil", wantTarget)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("target %q: not an *api.Error: %v", wantTarget, err)
	}
	if apiErr.Code != api.ErrCodeInsufficientPrivilege {
		t.Errorf("target %q: SQLSTATE = %q, want %q", wantTarget, apiErr.Code, api.ErrCodeInsufficientPrivilege)
	}
	var scopeErr *api.CrossDatabaseDDLError
	if !errors.As(err, &scopeErr) {
		t.Fatalf("target %q: no *api.CrossDatabaseDDLError in chain: %v", wantTarget, err)
	}
	if scopeErr.SessionDatabase != wantSession {
		t.Errorf("target %q: SessionDatabase = %q, want %q", wantTarget, scopeErr.SessionDatabase, wantSession)
	}
	if scopeErr.TargetDatabase != wantTarget {
		t.Errorf("target %q: TargetDatabase = %q, want %q", wantTarget, scopeErr.TargetDatabase, wantTarget)
	}
}

func TestCheckSchemaTemplateDDLAllowed(t *testing.T) {
	t.Parallel()

	// Java-parity default: templates are freely creatable and droppable.
	t.Run("unrestricted allows template ddl", func(t *testing.T) {
		t.Parallel()
		c := scopeTestConn("/tenant-a", false)
		for _, op := range []string{"CREATE SCHEMA TEMPLATE", "DROP SCHEMA TEMPLATE"} {
			if err := c.checkSchemaTemplateDDLAllowed(op); err != nil {
				t.Errorf("%s unrestricted: got %v, want nil", op, err)
			}
		}
	})

	// Refusal, not scoping: there is no owning database to compare against, and
	// the shared namespace makes template DDL reach other tenants' schemas.
	t.Run("restricted refuses all template ddl", func(t *testing.T) {
		t.Parallel()
		c := scopeTestConn("/tenant-a", true)
		for _, op := range []string{"CREATE SCHEMA TEMPLATE", "DROP SCHEMA TEMPLATE"} {
			err := c.checkSchemaTemplateDDLAllowed(op)
			if err == nil {
				t.Fatalf("%s restricted: got nil, want rejection", op)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("%s: not an *api.Error: %v", op, err)
			}
			if apiErr.Code != api.ErrCodeInsufficientPrivilege {
				t.Errorf("%s: SQLSTATE = %q, want %q", op, apiErr.Code, api.ErrCodeInsufficientPrivilege)
			}
			var tmplErr *api.SchemaTemplateDDLRestrictedError
			if !errors.As(err, &tmplErr) {
				t.Fatalf("%s: no *api.SchemaTemplateDDLRestrictedError in chain: %v", op, err)
			}
			if tmplErr.Operation != op {
				t.Errorf("Operation = %q, want %q", tmplErr.Operation, op)
			}
			if tmplErr.SessionDatabase != "/tenant-a" {
				t.Errorf("SessionDatabase = %q, want %q", tmplErr.SessionDatabase, "/tenant-a")
			}
		}
	})
}
