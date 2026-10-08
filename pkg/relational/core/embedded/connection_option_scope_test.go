package embedded

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/session"
)

// SetOption merges one checked option into the connection's set, keeping the
// connector's (the DSN's restrict_ddl_to_session_database among them), and
// ResetSession restores the connector's set when the pooled connection is
// returned, so a DRY_RUN set through Conn.Raw never reaches the next borrower
// (RFC-257 WS-E 6.2).
func TestSetOption_MergesAndLastsOneBorrow(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{sess: &session.Session{Schema: "S", DefaultSchema: "S"}}
	c.SetOptions(api.NoOptions().With(api.OptRestrictDDLToSessionDatabase, true))

	if err := c.SetOption(api.OptDryRun, true); err != nil {
		t.Fatal(err)
	}
	if !optBool(c.Options(), api.OptDryRun, false) || !optBool(c.Options(), api.OptRestrictDDLToSessionDatabase, false) {
		t.Fatalf("after SetOption: %v; want DRY_RUN and the connector's restriction", c.Options().AllEntries())
	}
	// A value outside the option's contract is refused and changes nothing.
	err := c.SetOption(api.OptExecutionScannedRowsLimit, -1)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidParameter {
		t.Fatalf("SetOption(EXECUTION_SCANNED_ROWS_LIMIT, -1) = %v, want 22023", err)
	}
	if _, set := c.Options().AllEntries()[api.OptExecutionScannedRowsLimit]; set {
		t.Fatal("a refused option was stored")
	}

	if err := c.ResetSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if optBool(c.Options(), api.OptDryRun, false) {
		t.Fatal("DRY_RUN outlived the borrow: the next borrower's DML would store nothing")
	}
	if !optBool(c.Options(), api.OptRestrictDDLToSessionDatabase, false) {
		t.Fatal("ResetSession dropped the connector's option")
	}
}

// A statement reads the options it executed with on every page: a SetOption
// between two pages does not change the rest of the result.
func TestPaginatingRows_OptionsCapturedAtExecution(t *testing.T) {
	t.Parallel()
	c := &EmbeddedConnection{sess: &session.Session{}}
	if err := c.SetOption(api.OptExecutionScannedRowsLimit, 7); err != nil {
		t.Fatal(err)
	}
	rows := &paginatingRows{conn: c, opts: c.Options()}
	if err := c.SetOption(api.OptExecutionScannedRowsLimit, 9); err != nil {
		t.Fatal(err)
	}
	if got := rows.executeProps().ScannedRecordsLimit; got != 7 {
		t.Fatalf("page scan limit %d, want the executed statement's 7", got)
	}
}
