package sqldriver_test

// The null-born hole in descriptorForColumn's agreement gate, pinned as the
// exact shape that demonstrated it (see the agreement-gate comment in
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
// The hole is currently UNREACHABLE through the driver metadata surface, and
// this test is the negative-result pin of that fact: the NoNulls derivation
// keys on proto REQUIRED cardinality, scalar NOT NULL is unexpressible (Java
// parity: NOT NULL is only allowed for ARRAY column type, rejected at CREATE),
// and ARRAY NOT NULL columns are flat REPEATED, not REQUIRED — so no
// DDL-expressible column ever derives ColumnNoNulls, the null-born upgrade
// (NoNulls → Nullable) is vacuous, and first-match cannot produce an
// observable wrong answer on this axis. That emitter property is PINNED, not
// merely asserted here: TestDDLEmitterNeverEmitsRequired
// (pkg/relational/core/metadata) fails if any column shape starts emitting
// REQUIRED, and names this test as one of the things it re-arms. Both slots below report nullable,
// which is also Java's (#4274) answer for the null-supplying slot. If either
// assertion goes red because a column reports NoNulls again, some shape
// re-armed the agreement-gate hole — re-read the file-top comment before
// touching the assertions; positional (D3) metadata is the real per-slot fix.
//
// No choice function INSIDE descriptorForColumn can repair the hole: both
// result slots hand it the SAME candidate list, while the correct answer
// differs per slot — (NoNulls, Nullable). The repair therefore had to stop
// asking it. A QUANTIFIER-ADDRESSED read now resolves its leg structurally
// (leg plan + leg-relative ordinal), which answers per slot. Not "never a
// name", as this comment first put it: the leg is found by correlation ALIAS
// and an unbaked read still falls back to a leaf-name match. What it no longer
// does is search a COLUMN name across the legs, which is the step that gave
// both slots the same answer;
// TestCrossLegNullBorn_RequiredColumnOnNullSupplyingLeg pins that on
// record-layer metadata, the only input that can carry the REQUIRED field this
// path needs. The FLAT (childless) read has no correlation to resolve a leg
// from and still goes through the name lookup, so for that form positional
// metadata flowed from the plan's own result type (the D3 deliverable) remains
// the answer.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// setupCrossLegNullBornDB builds the minimal counterexample schema: two tables
// that AGREE on their shared column VAL — both plain BIGINT, the only scalar
// nullability DDL can express — so the agreement gate keeps first-match, while
// a LEFT JOIN puts only one of them on the null-supplying side.
func setupCrossLegNullBornDB(t *testing.T, g *gomega.WithT) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbPath := "/testdb_xleg_nullborn"
	setup := openTestDB(t, dbPath)
	_, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, `CREATE SCHEMA TEMPLATE xleg_nullborn_tmpl
		CREATE TABLE leftt (lid BIGINT, val BIGINT, PRIMARY KEY (lid))
		CREATE TABLE rightt (rid BIGINT, val BIGINT, PRIMARY KEY (rid))`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, fmt.Sprintf(
		"CREATE SCHEMA %s/main WITH TEMPLATE xleg_nullborn_tmpl", dbPath))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { db.Close() })

	// One matched left row and one unmatched, so the join genuinely NULL-pads
	// RIGHTT's leg at runtime.
	_, err = db.ExecContext(ctx, `INSERT INTO leftt (lid, val) VALUES (1, 10), (2, 20)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO rightt (rid, val) VALUES (1, 100)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return db
}

// TestFDB_CrossLegAgreementGate_NullBornNotCovered pins the UNREACHABILITY of
// the agreement gate's null-born first-match hole: with no DDL-expressible
// column deriving NoNulls, both slots report nullable and the identity-keyed
// upgrade has nothing to flip. See the file-top comment for what re-arms it.
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

	// A.VAL: preserved leg. Its source column is proto OPTIONAL (the only
	// scalar shape), so it reports nullable.
	if nullable, ok := cts[0].Nullable(); ok {
		g.Expect(nullable).To(gomega.BeTrue(),
			"A.VAL reported NoNulls: some shape derives NoNulls again, which re-arms "+
				"the agreement gate's null-born first-match hole — see the file-top comment")
	}
	// B.VAL: null-supplying leg. Nullable is Java's (#4274) answer; here it
	// arrives from source nullability, not from the null-born upgrade (which
	// is vacuous while nothing derives NoNulls).
	if nullable, ok := cts[1].Nullable(); ok {
		g.Expect(nullable).To(gomega.BeTrue(),
			"B.VAL reported NoNulls: either the null-born hole re-armed (a shape derives "+
				"NoNulls and first-match answered the preserved leg) or nullability "+
				"derivation changed — see the file-top comment before touching this")
	}

	// The runtime rows: the unmatched left row (lid=2) NULL-pads B.VAL — the
	// genuine SQL NULL the metadata must never contradict with NoNulls.
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
		"unmatched row: B.VAL is a genuine runtime NULL")
}
