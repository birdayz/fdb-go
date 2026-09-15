package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/protoname"
	"fdb.dev/pkg/relational/api"
	"github.com/onsi/gomega"
)

// TestFDB_QuotedHashIdentifier pins the materialization boundary established by
// RFC-256: although the lexer accepts '#' inside a delimited identifier, an
// authored SELECT alias becomes a protobuf result field and Java rejects X#0
// with 42602. The typed InvalidNameError cause must survive the driver wrapper.
func TestFDB_QuotedHashIdentifier(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	dbPath := "/hash_ident_probe"
	setup := openTestDB(t, dbPath)
	g.Expect(setup.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath))).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE hash_ident_tmpl "+
			"CREATE TABLE t (id BIGINT, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE hash_ident_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO t VALUES (7)")).Error().NotTo(gomega.HaveOccurred())

	for _, query := range []string{
		`SELECT id AS "X#0" FROM t`,
		`SELECT "X#0" FROM (SELECT id AS "X#0" FROM t) AS d`,
	} {
		rows, queryErr := db.QueryContext(ctx, query)
		if rows != nil {
			_ = rows.Close()
		}
		var apiErr *api.Error
		g.Expect(errors.As(queryErr, &apiErr)).To(gomega.BeTrue(), "query: %s; error: %v", query, queryErr)
		g.Expect(apiErr.Code).To(gomega.Equal(api.ErrCodeInvalidName))
		var cause *protoname.InvalidNameError
		g.Expect(errors.As(queryErr, &cause)).To(gomega.BeTrue(), "invalid-name cause was lost: %v", queryErr)
	}
}
