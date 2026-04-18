package embedded

import (
	"testing"
)

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
