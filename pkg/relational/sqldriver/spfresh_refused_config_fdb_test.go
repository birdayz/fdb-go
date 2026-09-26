package sqldriver_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// An SPFresh index whose configuration the maintainer refuses is refused over
// SQL with SQLSTATE 42000. The DDL passes RABITQ_NUM_EX_BITS through unchecked
// (embedded/ddl.go, parseVectorOptions), the maintainer's ValidateSPFreshConfig
// refuses 0 extra bits as a MetaDataError, and translateFDBError maps a
// MetaDataError to 42000 (embedded/connection.go). SPFresh is a Go-only index,
// so the code is Go's own; this pins it. SQL can spell only Java's four metric
// names, so the maintainer's other refusal class (an IllegalArgumentError for
// a metric that does not parse) is not reachable from SQL.
func TestFDB_SPFreshRefusedConfigurationIsA42000(t *testing.T) {
	t.Parallel()
	h := newFleetHarness(t)
	suffix := strings.ReplaceAll(t.Name(), "_", "")
	dbPath := "/SPFREFUSED" + strings.ToUpper(suffix)
	name := "SPFREFUSED_T"
	body := "CREATE TABLE DOCS(ID BIGINT, EMBEDDING VECTOR(3, HALF), PRIMARY KEY(ID)) " +
		"CREATE VECTOR INDEX V USING SPFRESH ON DOCS(EMBEDDING) OPTIONS (RABITQ_NUM_EX_BITS = 0)"
	carrySetup(t, h, dbPath, name, body, []string{"S"}, func(string) []string { return nil })

	// A k-NN query plans the SPFresh index (its metric reads, the count is not
	// the planner's) and its scan reads the configuration through the
	// maintainer's reader. The query reaches the refusal without a row: Go's
	// INSERT does not take a vector value from SQL yet (TODO.md, "Go's INSERT
	// VALUES does not take a vector").
	db := fleetOpen(t, dbPath, "S")
	rows, err := db.QueryContext(context.Background(), `SELECT ID FROM DOCS
		QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(EMBEDDING, [1.0, 0.0, 0.0])) <= 3`)
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		_ = rows.Close()
	}
	var ae *api.Error
	if !errors.As(err, &ae) {
		t.Fatalf("k-NN query: %v, want an api.Error", err)
	}
	t.Logf("SPFREFUSED %s %s", ae.Code, ae.Message)
	if ae.Code != api.ErrCodeSyntaxOrAccessViolation || !strings.Contains(fmt.Sprint(err), "raBitQNumExBits must be in [1, 8], got 0") {
		t.Fatalf("k-NN query: %s %q (%v), want 42000 naming the refused count", ae.Code, ae.Message, err)
	}
}
