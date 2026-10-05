package sqldriver

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// An unknown DSN parameter is refused, naming it and every accepted one, so a
// misspelled option cannot silently leave a connection writable or
// unrestricted (RFC-257 WS-E 6.2).
func TestConnectionOptions_UnknownParameterIsRefused(t *testing.T) {
	t.Parallel()
	dsn, err := ParseDSN("fdbsql:///db?cluster_file=/f&schema=S&dryrun=true&zz=1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = dsn.ConnectionOptions()
	var apiErr *api.Error
	const want = "unknown DSN parameter dryrun, zz; accepted parameters are cluster_file, dry_run, " +
		"isolation_level_snapshot, planner_statistics, restrict_ddl_to_session_database, schema, transaction_tags"
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidParameter || apiErr.Message != want {
		t.Fatalf("ConnectionOptions = %v; want 22023 %q", err, want)
	}
}

// Every accepted parameter opens, and the two connection-scope booleans set
// their options.
func TestConnectionOptions_AcceptedParameters(t *testing.T) {
	t.Parallel()
	for _, query := range []string{
		"cluster_file=/f", "schema=S", "dry_run=true", "isolation_level_snapshot=true",
		"planner_statistics=true", "restrict_ddl_to_session_database=true", "transaction_tags=a",
	} {
		dsn, err := ParseDSN("fdbsql:///db?" + query)
		if err != nil {
			t.Fatalf("%s: parse: %v", query, err)
		}
		if _, err := dsn.ConnectionOptions(); err != nil {
			t.Errorf("%s refused: %v", query, err)
		}
	}
	dsn, err := ParseDSN("fdbsql:///db?dry_run=true&isolation_level_snapshot=false")
	if err != nil {
		t.Fatal(err)
	}
	opts, err := dsn.ConnectionOptions()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Get(api.OptDryRun) != true || opts.Get(api.OptIsolationLevelSnapshot) != false {
		t.Fatalf("options %v; want DRY_RUN true, ISOLATION_LEVEL_SNAPSHOT false", opts.AllEntries())
	}
	bad, err := ParseDSN("fdbsql:///db?dry_run=maybe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.ConnectionOptions(); err == nil {
		t.Fatal("dry_run=maybe accepted")
	}
}
