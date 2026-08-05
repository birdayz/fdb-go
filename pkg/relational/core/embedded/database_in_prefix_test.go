package embedded

// databaseInPrefix is the scope predicate shared by SHOW DATABASES and the
// RESTRICT_DDL_TO_SESSION_DATABASE check. Two properties are load-bearing and
// neither is visible from a SQL-level test, so they are pinned here directly.
//
//  1. Segment granularity: /tenant-a confers no authority over /tenant-abc.
//     A plain strings.HasPrefix would merge the two tenants.
//
//  2. An empty prefix matches NOTHING. This is the fail-closed direction. The
//     prefix is trailing-slash trimmed before the HasPrefix test, so an empty
//     prefix would otherwise reduce to HasPrefix(dbID, "/") — true for every
//     database path on the cluster. A scope predicate that widens to the whole
//     cluster exactly when the scope is missing is the disclosure the caller is
//     trying to prevent.
//
// An empty session database is not reachable through the sql.Driver today:
// ParseDSN rejects a DSN with no path (pinned by TestParseDSN_Invalid's
// "missing path embedded" case), and driver.Connector.Connect is the only
// caller of embedded.New. That reachability argument is what makes property 2
// latent rather than shipped — so if ParseDSN ever admits an empty path, this
// test is what keeps the predicate from re-arming the leak.

import "testing"

func TestDatabaseInPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		dbID   string
		prefix string
		want   bool
	}{
		{"exact match", "/tenant-a", "/tenant-a", true},
		{"nested one level", "/tenant-a/sub", "/tenant-a", true},
		{"nested deep", "/tenant-a/sub/deeper", "/tenant-a", true},

		// Segment granularity — character-prefix siblings are OUT of scope.
		{"character prefix sibling", "/tenant-abc", "/tenant-a", false},
		{"underscore sibling", "/tenant-a_extra", "/tenant-a", false},
		{"unrelated", "/tenant-b", "/tenant-a", false},

		// The caller may hand over a prefix with a trailing slash; it must mean
		// the same scope and must not swallow siblings either.
		{"trailing slash prefix exact", "/tenant-a", "/tenant-a/", false},
		{"trailing slash prefix nested", "/tenant-a/sub", "/tenant-a/", true},
		{"trailing slash prefix sibling", "/tenant-abc", "/tenant-a/", false},

		// Property 2: an absent scope selects nothing, never everything.
		{"empty prefix rejects rooted path", "/tenant-a", "", false},
		{"empty prefix rejects system catalog", "/__SYS", "", false},
		{"empty prefix rejects empty id", "", "", false},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := databaseInPrefix(c.dbID, c.prefix); got != c.want {
				t.Fatalf("databaseInPrefix(%q, %q) = %v, want %v",
					c.dbID, c.prefix, got, c.want)
			}
		})
	}
}
