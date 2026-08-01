package sqldriver_test

// The null-born hole in descriptorForColumn's agreement gate, pinned as the
// exact shape that demonstrates it (see the agreement-gate comment in
// cascades_generator.go).
//
// The gate keeps first-match when every candidate descriptor agrees on the SQL
// type name and the cardinality, on the argument that the choice among agreeing
// candidates is unobservable. That argument covers only the consumers that read
// type+cardinality; the null-born upgrade in deriveColumnsFromProjection reads
// the returned descriptor's IDENTITY — nullBorn[d.FullName()], membership of
// the leaf in an outer join's null-supplying legs — and two legs can agree on
// type+cardinality while differing on that membership. First-match then answers
// with the preserved leg's descriptor and the null-supplying column's upgrade
// never fires.
//
// No choice function inside descriptorForColumn can repair this: both result
// slots hand it the SAME candidate list, while the correct answer differs per
// slot — (NoNulls, Nullable). Only positional metadata flowed from the plan's
// own result type (the D3 deliverable) can answer per slot.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// setupCrossLegNullBornDB builds the minimal counterexample schema: two tables
// that AGREE on their shared column VAL — both declare it BIGINT NOT NULL — so
// the agreement gate keeps first-match, while a LEFT JOIN puts only one of them
// on the null-supplying side.
func setupCrossLegNullBornDB(t *testing.T, g *gomega.WithT) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbPath := "/testdb_xleg_nullborn"
	setup := openTestDB(t, dbPath)
	_, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE xleg_nullborn_tmpl
		CREATE TABLE leftt (lid BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (lid))
		CREATE TABLE rightt (rid BIGINT NOT NULL, val BIGINT NOT NULL, PRIMARY KEY (rid))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA %s/main WITH TEMPLATE xleg_nullborn_tmpl", dbPath))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { db.Close() })

	// One matched left row and one unmatched, so the join genuinely NULL-pads
	// RIGHTT's leg at runtime while the metadata under test claims it cannot.
	_, err = db.ExecContext(ctx, `INSERT INTO leftt (lid, val) VALUES (1, 10), (2, 20)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO rightt (rid, val) VALUES (1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return db
}

// TestFDB_CrossLegAgreementGate_NullBornNotCovered pins the KNOWN-WRONG answer
// the agreement gate's first-match produces on the null-born axis.
//
// Both legs declare VAL as BIGINT NOT NULL, so the candidates AGREE on
// everything the gate compares and first-match stands. B.VAL sits on the
// null-supplying leg of the LEFT JOIN, so the correct metadata — Java's, per
// #4274 — is (A.VAL NoNulls, B.VAL Nullable). First-match answers LEFTT for
// both lookups, RIGHTT's null-born upgrade never fires, and BOTH columns
// report NoNulls — while the runtime rows, asserted below, genuinely carry a
// NULL in B.VAL for the unmatched left row.
//
// This test asserts the CURRENT, WRONG behavior on purpose: it is the proof
// that the agreement gate does not make first-match safe, kept as the
// regression sentinel for that fact. When positional metadata (D3) lands, the
// expected values here MUST flip to (NoNulls, Nullable).
func TestFDB_CrossLegAgreementGate_NullBornNotCovered(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	db := setupCrossLegNullBornDB(t, g)
	ctx := context.Background()

	rows, err := db.QueryContext(ctx,
		"SELECT A.VAL, B.VAL FROM LEFTT A LEFT JOIN RIGHTT B ON B.RID = A.LID")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cts, err := rows.ColumnTypes()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(len(cts)).To(gomega.Equal(2))

	// A.VAL: preserved leg, declared NOT NULL — NoNulls is correct and must
	// stay NoNulls on both sides of the D3 flip.
	if nullable, ok := cts[0].Nullable(); ok {
		g.Expect(nullable).To(gomega.BeFalse(),
			"A.VAL is on the PRESERVED leg and declared NOT NULL; it must report "+
				"not-nullable both before and after positional (D3) metadata")
	}
	// B.VAL: null-supplying leg. Java (#4274) reports it NULLABLE; Go currently
	// reports NoNulls because descriptorForColumn's agreement gate first-matches
	// LEFTT (both legs agree on BIGINT NOT NULL for VAL) and the null-born
	// upgrade keyed on the returned descriptor's identity never fires.
	if nullable, ok := cts[1].Nullable(); ok {
		g.Expect(nullable).To(gomega.BeFalse(),
			"B.VAL reported NULLABLE. If this went red because positional (D3) "+
				"metadata now flows nullability per slot: the defect this test pins is "+
				"FIXED — flip this assertion to expect (A.VAL NoNulls, B.VAL Nullable), "+
				"Java's #4274 answer, and rewrite the comments that call the answer "+
				"known-wrong (this file and descriptorForColumn's agreement-gate "+
				"comment). If it went red for any other reason, nullability moved "+
				"without the per-slot fix — investigate before touching the assertion")
	}

	// The runtime rows are what make the NoNulls claim on B.VAL wrong rather
	// than merely un-Java: the unmatched left row (lid=2) NULL-pads B.VAL.
	got := map[int64]sql.NullInt64{}
	for rows.Next() {
		var a int64
		var b sql.NullInt64
		g.Expect(rows.Scan(&a, &b)).To(gomega.Succeed())
		got[a] = b
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.HaveLen(2))
	g.Expect(got[10].Valid).To(gomega.BeTrue(), "matched row: B.VAL = 100")
	g.Expect(got[10].Int64).To(gomega.Equal(int64(100)))
	g.Expect(got[20].Valid).To(gomega.BeFalse(),
		"unmatched row: B.VAL is a genuine runtime NULL in a column whose metadata claims NoNulls")
}
