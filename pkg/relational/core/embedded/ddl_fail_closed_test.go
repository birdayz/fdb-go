package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// The DDL layer silently dropped several index clauses it parses but never
// reads. Each drop built an index that differs from the one Java builds for the
// identical DDL, so a Go app and a Java app sharing a cluster would write
// mutually unreadable entries under one index name. These tests pin the
// fail-closed rejections that replaced the silent drops.
//
// Every case is a shape the vendored Java corpus actually contains, or — for
// the two that the corpus only witnesses in a form the value-index gap claims
// first — the exact aggregate shape under which the drop is reachable.

// requireUnsupported asserts the DDL is refused with ErrCodeUnsupportedOperation
// and a message naming the offending clause.
func requireUnsupported(t *testing.T, ddl, wantSubstr string) {
	t.Helper()
	_, err := buildSchemaTemplateFromDDL(ddl)
	if err == nil {
		t.Fatalf("DDL was ACCEPTED but must be refused; a silently dropped clause here builds "+
			"an index whose entries differ from Java's for the same DDL.\nDDL: %s", ddl)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T (%v), want *api.Error carrying a SQLSTATE", err, err)
	}
	if apiErr.Code != api.ErrCodeUnsupportedOperation {
		t.Errorf("SQLSTATE = %s, want %s (%v)", apiErr.Code, api.ErrCodeUnsupportedOperation, err)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("message = %q, want it to contain %q", err.Error(), wantSubstr)
	}
}

// TestDDLRejectsOnSourceOrderClause pins the ON-source per-column ASC/DESC/NULLS
// rejection.
//
// Java wraps an ordered column in an OrderFunctionKeyExpression —
// `on t(a desc)` becomes function("order_desc_nulls_last", field("a")), whose
// entry bytes are the order-inverted encoding — while this layer read only the
// column name and built a plain ASCENDING index. Sixteen corpus statements
// carry the shape; the sub-tests below are verbatim from three of them.
func TestDDLRejectsOnSourceOrderClause(t *testing.T) {
	t.Parallel()
	const table = `CREATE TABLE products (pk integer, category string, price integer, rating integer,
		supplier string, stock integer, name string, PRIMARY KEY (pk))
		`
	cases := []struct{ name, index string }{
		// documentation-queries/index-documentation-queries.yamsql:19-23
		{"desc_and_asc", `CREATE INDEX idx_on_category_desc ON products(category desc, price asc)`},
		{"asc_nulls_last", `CREATE INDEX idx_on_rating_nulls_last ON products(rating asc nulls last)`},
		{"desc_nulls_first", `CREATE INDEX idx_on_supplier_desc_nulls_first ON products(supplier desc nulls first)`},
		// A bare NULLS clause with no ASC/DESC is still an orderClause: Java maps
		// it to ASCENDING_NULLS_LAST and emits order_asc_nulls_last, so dropping
		// it diverges exactly as DESC does.
		{"bare_nulls_last", `CREATE INDEX idx_on_stock_nulls_last ON products(stock nulls last)`},
		{"mixed_three_columns", `CREATE INDEX idx_on_complex ON products(category asc nulls first, price desc nulls last, name asc)`},
		// index-ddl.yamsql:62 — the order clause on a NON-leading column must be
		// caught too. A guard that inspected only the first column would pass
		// this while still building the wrong index.
		{"order_on_trailing_column", `CREATE INDEX idx_iot_asc_desc ON products(price, category desc)`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			requireUnsupported(t, table+c.index, "per-column ordering")
		})
	}
}

// TestDDLAcceptsOnSourceIndexWithoutOrderClause is the other direction of the
// same fix, and it is what stops the rejection from being over-broad.
//
// A column carrying NO order clause is ascending in both engines — Java maps
// ASCENDING to a null ordering function and emits the bare field — so the plain
// index this layer already built was correct and must keep building.
func TestDDLAcceptsOnSourceIndexWithoutOrderClause(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE products (pk integer, category string, price integer, PRIMARY KEY (pk))
		 CREATE INDEX idx_plain ON products(category, price)`)
	if err != nil {
		t.Fatalf("an ON-source index with no order clause must still be accepted "+
			"(Java emits the bare field for ASCENDING); got: %v", err)
	}
	if idx := tmpl.Underlying().GetIndex("IDX_PLAIN"); idx == nil {
		t.Fatal("IDX_PLAIN was not built")
	}
}

// TestDDLRejectsVectorIndexOrderClause covers the other two readers of the same
// indexColumnSpec node. Java parses the vector index's value columns and its
// PARTITION BY columns through the identical colSpec parser, so DESC is honored
// there too and dropping it is the same divergence. The corpus does not witness
// this shape; the drop is reachable all the same.
func TestDDLRejectsVectorIndexOrderClause(t *testing.T) {
	t.Parallel()
	const table = `CREATE TABLE docs (pk integer, region string, embedding vector(3, half), PRIMARY KEY (pk))
		`
	t.Run("value_column", func(t *testing.T) {
		t.Parallel()
		requireUnsupported(t, table+
			`CREATE VECTOR INDEX idx_vec USING HNSW ON docs(embedding desc)`,
			"per-column ordering")
	})
	t.Run("partition_by_column", func(t *testing.T) {
		t.Parallel()
		requireUnsupported(t, table+
			`CREATE VECTOR INDEX idx_vec USING HNSW ON docs(embedding) PARTITION BY (region desc)`,
			"per-column ordering")
	})
}

// TestDDLRejectsAsSelectIndexAttributes pins the LEGACY_EXTREMUM_EVER rejection.
//
// Java reads the attribute and emits MIN_EVER_LONG / MAX_EVER_LONG — a
// different index type with a different maintainer writing a packed long rather
// than a tuple — while this layer mapped MIN_EVER/MAX_EVER unconditionally to
// the TUPLE variants, so the attribute changed nothing. Four corpus statements
// carry the shape and all four were accepted and mis-built; the sub-tests are
// verbatim from them, including the lower-case spelling.
func TestDDLRejectsAsSelectIndexAttributes(t *testing.T) {
	t.Parallel()
	const table = `CREATE TABLE t1 (pk integer, col1 integer, col2 integer, PRIMARY KEY (pk))
		`
	cases := []struct{ name, index string }{
		// aggregate-index-tests.yamsql:39-40
		{"min_ever_upper", `CREATE INDEX mv12 AS SELECT min_ever(col2) FROM t1 GROUP BY col1 with attributes LEGACY_EXTREMUM_EVER`},
		{"max_ever_upper", `CREATE INDEX mv13 AS SELECT max_ever(col2) FROM t1 GROUP BY col1 with attributes LEGACY_EXTREMUM_EVER`},
		// index-ddl.yamsql:82-83 spell the attribute in lower case.
		{"min_ever_lower", `CREATE INDEX idx_mv_age_min_extremum AS SELECT min_ever(col2) FROM t1 GROUP BY col1 with attributes legacy_extremum_ever`},
		{"max_ever_lower", `CREATE INDEX idx_mv_age_max_extremum AS SELECT max_ever(col2) FROM t1 GROUP BY col1 with attributes legacy_extremum_ever`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			requireUnsupported(t, table+c.index, "WITH ATTRIBUTES")
		})
	}
}

// TestDDLAcceptsAsSelectExtremumWithoutAttributes is the paired direction: with
// NO attribute Java also emits the TUPLE variant, so the unattributed extremum
// index this layer built was correct and must keep building. Without this the
// attribute rejection could be "satisfied" by refusing every MIN_EVER/MAX_EVER.
func TestDDLAcceptsAsSelectExtremumWithoutAttributes(t *testing.T) {
	t.Parallel()
	// aggregate-index-tests.yamsql:41 — the same statement minus the attribute.
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t4 (pk integer, col1 integer, col2 integer, PRIMARY KEY (pk))
		 CREATE INDEX mv14 AS SELECT min_ever(col1) FROM t4 GROUP BY col2`)
	if err != nil {
		t.Fatalf("an AS-SELECT extremum index with no attributes must still be accepted "+
			"(Java emits the TUPLE variant too); got: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("MV14")
	if idx == nil {
		t.Fatal("MV14 was not built")
	}
	if idx.Type != "min_ever_tuple" {
		t.Errorf("index type = %q, want %q — the unattributed form is the TUPLE variant in both engines",
			idx.Type, "min_ever_tuple")
	}
}

// TestDDLRejectsAsSelectUniqueAndWhere pins the two drops that are reachable
// only through an AGGREGATE select element.
//
// The corpus witnesses both clauses only on NON-aggregate AS-SELECT indexes
// (`create unique index … as select profession …`,
// `… as select p from t where p > 10`), and those are refused earlier by the
// value-index gap, which is why the drops read as unreachable. They are not:
// paired with an aggregate the definition reaches the builder, and the builder
// reads neither clause.
//
// Java honors both — UNIQUE goes into the index's options map, and WHERE makes
// the index SPARSE by storing a predicate and maintaining entries only for
// matching records. Dropping WHERE in particular builds a FULL index, holding
// entries Java's index deliberately omits.
func TestDDLRejectsAsSelectUniqueAndWhere(t *testing.T) {
	t.Parallel()
	const table = `CREATE TABLE t1 (pk integer, col1 integer, col2 integer, PRIMARY KEY (pk))
		`
	t.Run("unique_on_aggregate", func(t *testing.T) {
		t.Parallel()
		requireUnsupported(t, table+
			`CREATE UNIQUE INDEX mvu AS SELECT min_ever(col2) FROM t1 GROUP BY col1`,
			"UNIQUE")
	})
	t.Run("where_on_aggregate", func(t *testing.T) {
		t.Parallel()
		requireUnsupported(t, table+
			`CREATE INDEX mvw AS SELECT min_ever(col2) FROM t1 WHERE col1 > 3 GROUP BY col1`,
			"WHERE clause")
	})
	t.Run("unique_on_cardinality", func(t *testing.T) {
		t.Parallel()
		requireUnsupported(t,
			`CREATE TABLE tab (id integer, int_arr integer array null, PRIMARY KEY (id))
			 CREATE UNIQUE INDEX tab_card AS SELECT CARDINALITY(int_arr) AS card FROM tab ORDER BY card`,
			"UNIQUE")
	})
	t.Run("where_on_cardinality", func(t *testing.T) {
		t.Parallel()
		requireUnsupported(t,
			`CREATE TABLE tab (id integer, x integer, int_arr integer array null, PRIMARY KEY (id))
			 CREATE INDEX tab_card AS SELECT CARDINALITY(int_arr) AS card FROM tab WHERE x > 3 ORDER BY card`,
			"WHERE clause")
	})
}

// TestDDLValueIndexGapPrecedesDroppedClauseGuard pins the ORDER the two
// rejections fire in, which is what keeps the corpus ledger meaningful.
//
// A value index written in AS-SELECT form that ALSO carries UNIQUE or WHERE has
// two reasons to be refused. It must report the value-index gap, because that
// is the capability whose absence blocks the file — the corpus books ~42 files
// under that class, and several carry a WHERE. Were the dropped-clause guard to
// run first, those files would silently re-bucket and the gap's measured size
// would shrink for no reason anyone had changed.
func TestDDLValueIndexGapPrecedesDroppedClauseGuard(t *testing.T) {
	t.Parallel()
	// index-ddl.yamsql:70 and sparse-index-tests.yamsql shapes: non-aggregate
	// AS-SELECT carrying UNIQUE / WHERE respectively.
	for _, ddl := range []string{
		`CREATE TABLE t1 (pk integer, profession string, PRIMARY KEY (pk))
		 CREATE UNIQUE INDEX idx_mv_unique_profession AS SELECT profession FROM t1 ORDER BY profession`,
		`CREATE TABLE t1 (pk integer, p integer, PRIMARY KEY (pk))
		 CREATE INDEX mv1 AS SELECT p FROM t1 WHERE p > 10 ORDER BY p`,
	} {
		_, err := buildSchemaTemplateFromDDL(ddl)
		if err == nil {
			t.Fatalf("expected a rejection\nDDL: %s", ddl)
		}
		const want = "select element must contain an aggregate function"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection must name the value-index gap, not the dropped clause — the corpus "+
				"ledger buckets these files by this message and re-bucketing them would silently "+
				"shrink unsupported-DDL:value-index-as-select.\n got: %v\nwant substring: %q", err, want)
		}
	}
}
