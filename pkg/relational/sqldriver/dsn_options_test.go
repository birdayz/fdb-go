package sqldriver

import (
	"errors"
	"math"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// An unknown DSN parameter is refused, naming it and every accepted one, so a
// misspelled option cannot silently leave a connection writable or
// unrestricted (RFC-257 WS-E 6.2).
func TestConnectionOptions_UnknownParameterIsRefused(t *testing.T) {
	t.Parallel()
	dsn, err := ParseDSN("fdbsql:///FRL/db?cluster_file=/f&schema=S&dryrun=true&zz=1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = dsn.ConnectionOptions()
	var apiErr *api.Error
	const want = "unknown DSN parameter dryrun, zz; accepted parameters are cluster_file, " +
		"disable_planner_rewriting, disabled_planner_rules, dry_run, execution_scanned_bytes_limit, " +
		"execution_scanned_rows_limit, execution_time_limit, isolation_level_snapshot, " +
		"max_rows, max_statement_memory_bytes, plan_cache_primary_max_entries, " +
		"plan_cache_primary_time_to_live_millis, plan_cache_secondary_max_entries, " +
		"plan_cache_secondary_time_to_live_millis, plan_cache_tertiary_max_entries, " +
		"plan_cache_tertiary_time_to_live_millis, plan_right_deep, planner_statistics, " +
		"restrict_ddl_to_session_database, schema, transaction_tags, transaction_timeout"
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
		dsn, err := ParseDSN("fdbsql:///FRL/db?" + query)
		if err != nil {
			t.Fatalf("%s: parse: %v", query, err)
		}
		if _, err := dsn.ConnectionOptions(); err != nil {
			t.Errorf("%s refused: %v", query, err)
		}
	}
	dsn, err := ParseDSN("fdbsql:///FRL/db?dry_run=true&isolation_level_snapshot=false")
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
	bad, err := ParseDSN("fdbsql:///FRL/db?dry_run=maybe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.ConnectionOptions(); err == nil {
		t.Fatal("dry_run=maybe accepted")
	}
}

// The engine plan cache's sizes and TTLs are DSN parameters named by the
// lower-cased option, converted and checked by the option's contract.
func TestConnectionOptions_PlanCacheParameters(t *testing.T) {
	t.Parallel()
	dsn, err := ParseDSN("fdbsql:///FRL/db?plan_cache_tertiary_max_entries=4&plan_cache_secondary_time_to_live_millis=60000")
	if err != nil {
		t.Fatal(err)
	}
	opts, err := dsn.ConnectionOptions()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Get(api.OptPlanCacheTertiaryMaxEntries) != 4 || opts.Get(api.OptPlanCacheSecondaryTimeToLiveMillis) != int64(60000) {
		t.Fatalf("options %v", opts.AllEntries())
	}
	for _, bad := range []string{"plan_cache_secondary_max_entries=0", "plan_cache_primary_time_to_live_millis=x"} {
		d, err := ParseDSN("fdbsql:///FRL/db?" + bad)
		if err != nil {
			t.Fatal(err)
		}
		var apiErr *api.Error
		if _, err := d.ConnectionOptions(); !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidParameter {
			t.Errorf("%s: %v, want 22023", bad, err)
		}
	}
}

// Execution limits and planner knobs are DSN parameters named by the
// lower-cased option, converted and range-checked by the option's contract as
// Java's Options.fromProperties does, and refused before FDB is opened.
func TestConnectionOptions_ExecutionLimits(t *testing.T) {
	t.Parallel()
	notInt := []string{"", "false", "1.5", "1ms", "%20", "%2B", "0x10"}
	for _, tc := range []struct {
		param  string
		option api.OptionName
		value  string
		want   any
		bad    []string
	}{
		{"max_rows", api.OptMaxRows, "17", 17, append([]string{"-1", "2147483648"}, notInt...)},
		{"execution_scanned_rows_limit", api.OptExecutionScannedRowsLimit, "3", 3, append([]string{"-1", "2147483648"}, notInt...)},
		{"execution_scanned_bytes_limit", api.OptExecutionScannedBytesLimit, "8192", int64(8192), append([]string{"-1", "9223372036854775808"}, notInt...)},
		{"execution_time_limit", api.OptExecutionTimeLimit, "200", int64(200), append([]string{"-1", "9223372036854775808"}, notInt...)},
		{"max_statement_memory_bytes", api.OptMaxStatementMemoryBytes, "1048576", int64(1048576), append([]string{"-1", "9223372036854775808"}, notInt...)},
		{"transaction_timeout", api.OptTransactionTimeout, "2500", int64(2500), append([]string{"-2", "9223372036854775808"}, notInt...)},
		{"plan_right_deep", api.OptPlanRightDeep, "true", true, []string{"maybe", "2"}},
		{"disable_planner_rewriting", api.OptDisablePlannerRewriting, "off", false, []string{"maybe", "2"}},
	} {
		t.Run(tc.param, func(t *testing.T) {
			t.Parallel()
			connector, err := (&Driver{}).OpenConnector("fdbsql:///FRL/db?" + tc.param + "=" + tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if got := connector.(*Connector).connOpts.Get(tc.option); got != tc.want {
				t.Fatalf("%s=%v (%T), want %v (%T)", tc.param, got, got, tc.want, tc.want)
			}
			for _, bad := range tc.bad {
				_, err := (&Driver{}).OpenConnector("fdbsql:///FRL/db?cluster_file=/does/not/exist&" + tc.param + "=" + bad)
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidParameter || !strings.Contains(err.Error(), tc.param) {
					t.Errorf("%s=%s: got %v, want 22023 naming the parameter", tc.param, bad, err)
				}
			}
		})
	}
	connector, err := (&Driver{}).OpenConnector("fdbsql:///FRL/db?disabled_planner_rules=MatchLeafRule,%20SelectMergeRule")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := connector.(*Connector).connOpts.Get(api.OptDisabledPlannerRules).([]string); strings.Join(got, "|") != "MatchLeafRule|SelectMergeRule" {
		t.Fatalf("disabled_planner_rules = %q", got)
	}
	connector, err = (&Driver{}).OpenConnector("fdbsql:///FRL/db")
	if err != nil {
		t.Fatal(err)
	}
	opts := connector.(*Connector).connOpts
	if len(opts.AllEntries()) != 0 || opts.Get(api.OptMaxRows) != math.MaxInt32 ||
		opts.Get(api.OptExecutionScannedRowsLimit) != math.MaxInt32 ||
		opts.Get(api.OptExecutionScannedBytesLimit) != int64(math.MaxInt64) ||
		opts.Get(api.OptExecutionTimeLimit) != int64(0) || opts.Get(api.OptTransactionTimeout) != nil ||
		opts.Get(api.OptMaxStatementMemoryBytes) != nil {
		t.Fatalf("absent DSN limits changed defaults: %v", opts.AllEntries())
	}
	for _, query := range []string{
		"max_rows=0", "execution_time_limit=0", "execution_scanned_rows_limit=0",
		"execution_scanned_bytes_limit=0", "max_statement_memory_bytes=0",
		"transaction_timeout=0", "transaction_timeout=-1", "max_rows=%2B5",
	} {
		if _, err := (&Driver{}).OpenConnector("fdbsql:///FRL/db?" + query); err != nil {
			t.Errorf("%s: %v", query, err)
		}
	}
}

// A query string url.Query would partly drop is refused, so a malformed limit
// can never leave the connection unbounded; duplicates keep the first value.
func TestParseDSN_MalformedLimitIsNotDropped(t *testing.T) {
	t.Parallel()
	for _, query := range []string{"max_rows=%xx", "max_rows=1;transaction_timeout=2", "transaction_timeout=%"} {
		if _, err := (&Driver{}).OpenConnector("fdbsql:///FRL/db?" + query); err == nil {
			t.Errorf("malformed query %s silently accepted", query)
		}
	}
	c, err := (&Driver{}).OpenConnector("fdbsql:///FRL/db?max_rows=2&max_rows=7")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.(*Connector).connOpts.Get(api.OptMaxRows); got != 2 {
		t.Fatalf("duplicate DSN parameters must keep the first value: %v", got)
	}
	if _, err := (&Driver{}).OpenConnector("fdbsql:///FRL/db?max_row=2"); err == nil || !strings.Contains(err.Error(), "max_rows") {
		t.Fatalf("unknown option did not list max_rows: %v", err)
	}
}
