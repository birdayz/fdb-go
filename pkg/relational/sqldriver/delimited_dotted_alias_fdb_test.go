package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/onsi/gomega"
)

// TestFDB_DelimitedDottedAliasIsVerbatim pins the result-set column LABEL for a
// SELECT-list alias that a user wrote with a dot inside delimited quotes.
//
// Java ground truth: `AS "U.NAME"` is ONE delimited `uid`, so the parser builds
// Identifier{name:"U.NAME", qualifier:[]} (IdentifierVisitor.java:73-81); the
// label is Identifier::toString (Expressions.java:245-256) and getColumnLabel
// IS getColumnName (RelationalStructMetaData.java:81-89). Java's clearQualifier
// (LogicalOperator.java:484-487) strips the structural qualifier LIST, never the
// leaf, so a delimited dotted alias is untouched by construction — pinned in
// Java's own corpus (valid-identifiers.yamsql:287-289, where the alias "__.__."
// comes back verbatim). No path in fdb-relational inspects an alias for a dot.
//
// Go used to inspect it: the label derivation degraded ANY dotted alias whose
// leaf matched the projected reference's leaf, because that string shape was
// how it recognised the qualified datum key the duplicated-bare-leaf dedup
// mints for `SELECT a.k, b.k`. A string comparison cannot tell a machinery key
// from a user alias, so user aliases were degraded too — including in the
// single-table case where no machinery alias can exist. Provenance is now
// CARRIED per slot (LogicalProject.AliasMinted → LogicalProjectionExpression →
// RecordQueryProjectionPlan), so the label site asks who wrote the alias rather
// than what it looks like.
func TestFDB_DelimitedDottedAliasIsVerbatim(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dotalias")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_dotalias")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE dotalias_tmpl "+
			"CREATE TABLE Users (uid BIGINT NOT NULL, name STRING, PRIMARY KEY (uid)) "+
			"CREATE TABLE Orders (oid BIGINT NOT NULL, uid BIGINT, name STRING, PRIMARY KEY (oid))")).
		Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_dotalias/s WITH TEMPLATE dotalias_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_dotalias?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO Users VALUES (1, 'alice'), (2, 'bob')")).
		Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO Orders VALUES (10, 1, 'first'), (11, 2, 'second')")).
		Error().NotTo(gomega.HaveOccurred())

	queryColumns := func(query string) []string {
		rows, err := db.QueryContext(ctx, query)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "query: %s", query)
		defer rows.Close()
		cols, err := rows.Columns()
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return cols
	}

	join := " FROM Users u, Orders o WHERE u.uid = o.uid ORDER BY o.oid"
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		// The alias's leaf matches the projected reference's leaf AND its
		// qualifier names a real table in the query — the exact shape the old
		// string comparison could not distinguish from a machinery key.
		{"join_leaf_match_real_qualifier", `SELECT u.name AS "U.NAME"` + join, []string{"U.NAME"}},
		// Same leaf match, but the qualifier names NO table in the query. Java
		// never looks, so this is verbatim for the same reason.
		{"join_leaf_match_unknown_qualifier", `SELECT u.name AS "Q.NAME"` + join, []string{"Q.NAME"}},
		// Leaf does NOT match: this one survived the old comparison. Kept so the
		// fix is pinned on both sides of the leaf-match branch.
		{"join_leaf_mismatch", `SELECT u.name AS "U.OTHER"` + join, []string{"U.OTHER"}},
		// SINGLE TABLE: no machinery alias can exist here at all (the mint fires
		// only on a DUPLICATED bare leaf that the user did not alias), yet the
		// old string comparison degraded it anyway.
		{"single_table_leaf_match", `SELECT u.name AS "U.NAME" FROM Users u ORDER BY u.uid`, []string{"U.NAME"}},
		// Java's own corpus shape (valid-identifiers.yamsql:287-289).
		{"java_corpus_underscore_dots", `SELECT u.name AS "__.__." FROM Users u ORDER BY u.uid`, []string{"__.__."}},
		// A dotted alias on a COMPUTED column — no projected reference leaf to
		// match at all, so it must pass through untouched.
		{"computed_dotted_alias", `SELECT u.uid + 1 AS "U.NAME" FROM Users u ORDER BY u.uid`, []string{"U.NAME"}},
		// A dotted alias whose leaf matches a column of the OTHER leg.
		{"join_leaf_match_other_leg", `SELECT u.name AS "O.NAME"` + join, []string{"O.NAME"}},
		// Delimited alias containing a space AND a dot.
		{"dotted_alias_with_space", `SELECT u.name AS "U. NAME" FROM Users u ORDER BY u.uid`, []string{"U. NAME"}},
		// A dotted user alias in the SAME query as a machinery-minted qualified
		// datum key: `o.name`/`u.name` duplicate the bare leaf NAME, so slot 1
		// gets the mint while slot 0 carries the user's alias. Both must be
		// reported the way their PROVENANCE says, not the way they are spelled.
		{
			"machinery_mint_and_user_alias_same_query",
			`SELECT u.name AS "U.NAME", o.name` + join,
			[]string{"U.NAME", "NAME"},
		},
		// An alias colliding with a real, unrelated column name of the query.
		{"alias_collides_with_real_column", `SELECT u.name AS "U.UID"` + join, []string{"U.UID"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(queryColumns(tc.query)).To(gomega.Equal(tc.want),
				"a delimited alias is reported VERBATIM (Java: Identifier::toString); query: %s", tc.query)
		})
	}
}

// TestFDB_DuplicateBareLeafKeepsTwoColumns is the HAZARD case for the fix
// above. The duplicated-bare-leaf dedup mints the projected reference's
// QUALIFIED spelling as an internal alias so the two same-named datum keys do
// not collapse into one row-map entry. Deleting that mint was measured to red
// seven FDB suites with WRONG ROWS — `SELECT a.k, b.k` came back as a SINGLE
// column. The mint is therefore load-bearing and the label fix must leave it
// running: only its user-visible REPORTING changes.
//
// Java reaches the same result differently: duplicates make the INTERNAL column
// anonymous (Expressions.java:268-287) while the label is untouched, and where
// two names for one column are genuinely needed it uses a separate
// construction-time field (Type.Record.Field.fieldStorageNameOptional,
// Type.java:2826-2827,2897-2899) — never an in-band marker recovered later by
// string inspection.
func TestFDB_DuplicateBareLeafKeepsTwoColumns(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_dupleaf")
	g.Expect(setup.ExecContext(ctx, "CREATE DATABASE /testdb_dupleaf")).Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE dupleaf_tmpl "+
			"CREATE TABLE A (id BIGINT NOT NULL, k STRING, PRIMARY KEY (id)) "+
			"CREATE TABLE B (id BIGINT NOT NULL, k STRING, PRIMARY KEY (id))")).
		Error().NotTo(gomega.HaveOccurred())
	g.Expect(setup.ExecContext(ctx,
		"CREATE SCHEMA /testdb_dupleaf/s WITH TEMPLATE dupleaf_tmpl")).Error().NotTo(gomega.HaveOccurred())

	dsn := fmt.Sprintf("fdbsql:///testdb_dupleaf?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer db.Close()

	g.Expect(db.ExecContext(ctx, "INSERT INTO A VALUES (1, 'a-one'), (2, 'a-two')")).
		Error().NotTo(gomega.HaveOccurred())
	g.Expect(db.ExecContext(ctx, "INSERT INTO B VALUES (1, 'b-one'), (2, 'b-two')")).
		Error().NotTo(gomega.HaveOccurred())

	const q = `SELECT A."K", B."K" FROM A, B WHERE A.id = B.id ORDER BY A.id`
	rows, err := db.QueryContext(ctx, q)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	defer rows.Close()

	cols, err := rows.Columns()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	// TWO columns, both labelled with the bare leaf: Java reports the bare
	// column for `SELECT a.k, b.k` and JDBC permits duplicate labels.
	g.Expect(cols).To(gomega.Equal([]string{"K", "K"}),
		"the duplicated bare leaf yields TWO columns; collapsing to one is the wrong-rows hazard")

	var got [][2]string
	for rows.Next() {
		var l, r string
		g.Expect(rows.Scan(&l, &r)).To(gomega.Succeed())
		got = append(got, [2]string{l, r})
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	g.Expect(got).To(gomega.Equal([][2]string{{"a-one", "b-one"}, {"a-two", "b-two"}}),
		"each leg's own K must reach its own slot — the qualified datum key is what keeps them apart")
}
