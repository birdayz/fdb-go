package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_QuotedHashIdentifier pins that the dialect accepts quoted
// identifiers containing '#' end-to-end (DOUBLE_QUOTE_ID lexes any non-quote
// character) — the REACHABILITY premise of the review round-3 finding on PR
// #446: because `AS "X#0"` is legal and an outer scope can reference it, a
// plain name-read of a field literally named X#0 exists in real plans and
// must never render identically to an ordinal read of X at slot 0 in the
// ExplainValue-keyed plan identity (values.ExplainValue doubles '#' in raw
// field text; the identity is injective over (field text, ordinal)).
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
			"CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE hash_ident_tmpl", dbPath))).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO t VALUES (7)")).Error().NotTo(gomega.HaveOccurred())

	// The quoted '#' alias is accepted and surfaces verbatim as the column label.
	rows, err := db.QueryContext(ctx, `SELECT id AS "X#0" FROM t`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()
	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(cols).To(gomega.Equal([]string{"X#0"}))
	var v int64
	g.Expect(rows.Next()).To(gomega.BeTrue())
	g.Expect(rows.Scan(&v)).To(gomega.Succeed())
	g.Expect(v).To(gomega.Equal(int64(7)))
	g.Expect(rows.Next()).To(gomega.BeFalse())
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())

	// An OUTER scope can reference the '#' alias — i.e. a FieldValue whose
	// Field is literally "X#0" (a plain NAME read) occurs in real plans.
	var vv int64
	g.Expect(db.QueryRowContext(ctx,
		`SELECT "X#0" FROM (SELECT id AS "X#0" FROM t) AS d`).Scan(&vv)).To(gomega.Succeed())
	g.Expect(vv).To(gomega.Equal(int64(7)))
}
