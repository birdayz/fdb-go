package embedded

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
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

// TestDDLHonorsOnSourceOrderClause is the RETIRED form of the S0 ON-source
// per-column ASC/DESC/NULLS rejection: the ON-source front end (RFC-202 S6)
// now routes through the generator, which wraps an ordered column in an
// OrderFunctionKeyExpression exactly as Java does — `on t(a desc)` builds
// function("order_desc_nulls_last", field("a")), whose entry bytes are the
// order-inverted encoding (OrderFunctionKeyExpressionFactory.java:44-48).
// Sixteen corpus statements carry the shape; the sub-tests below are verbatim
// from three of them, and each pins the exact root key expression.
func TestDDLHonorsOnSourceOrderClause(t *testing.T) {
	t.Parallel()
	const table = `CREATE TABLE products (pk integer, category string, price integer, rating integer,
		supplier string, stock integer, name string, PRIMARY KEY (pk))
		`
	cases := []struct {
		name, indexName, index string
		want                   recordlayer.KeyExpression
	}{
		// documentation-queries/index-documentation-queries.yamsql:19-23
		{
			"desc_and_asc", "IDX_ON_CATEGORY_DESC",
			`CREATE INDEX idx_on_category_desc ON products(category desc, price asc)`,
			recordlayer.Concat(
				recordlayer.FunctionExpr(recordlayer.OrderFuncDescNullsLast, recordlayer.Field("CATEGORY")),
				recordlayer.Field("PRICE")),
		},
		{
			"asc_nulls_last", "IDX_ON_RATING_NULLS_LAST",
			`CREATE INDEX idx_on_rating_nulls_last ON products(rating asc nulls last)`,
			recordlayer.FunctionExpr(recordlayer.OrderFuncAscNullsLast, recordlayer.Field("RATING")),
		},
		{
			"desc_nulls_first", "IDX_ON_SUPPLIER_DESC_NULLS_FIRST",
			`CREATE INDEX idx_on_supplier_desc_nulls_first ON products(supplier desc nulls first)`,
			recordlayer.FunctionExpr(recordlayer.OrderFuncDescNullsFirst, recordlayer.Field("SUPPLIER")),
		},
		// A bare NULLS clause with no ASC/DESC is still an orderClause: Java
		// maps it to ASCENDING_NULLS_LAST (parseColSpec's DESC() is null, the
		// NULLS token decides) and emits order_asc_nulls_last.
		{
			"bare_nulls_last", "IDX_ON_STOCK_NULLS_LAST",
			`CREATE INDEX idx_on_stock_nulls_last ON products(stock nulls last)`,
			recordlayer.FunctionExpr(recordlayer.OrderFuncAscNullsLast, recordlayer.Field("STOCK")),
		},
		// Explicit ASC NULLS FIRST is the default — no wrapper, exactly like
		// no clause (Java maps ASCENDING to a null ordering function).
		{
			"mixed_three_columns", "IDX_ON_COMPLEX",
			`CREATE INDEX idx_on_complex ON products(category asc nulls first, price desc nulls last, name asc)`,
			recordlayer.Concat(
				recordlayer.Field("CATEGORY"),
				recordlayer.FunctionExpr(recordlayer.OrderFuncDescNullsLast, recordlayer.Field("PRICE")),
				recordlayer.Field("NAME")),
		},
		// index-ddl.yamsql:62 — the order clause on a NON-leading column: a
		// front end that read only the first column's clause would build a
		// plain trailing field here.
		{
			"order_on_trailing_column", "IDX_IOT_ASC_DESC",
			`CREATE INDEX idx_iot_asc_desc ON products(price, category desc)`,
			recordlayer.Concat(
				recordlayer.Field("PRICE"),
				recordlayer.FunctionExpr(recordlayer.OrderFuncDescNullsLast, recordlayer.Field("CATEGORY"))),
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := buildSchemaTemplateFromDDL(table + c.index)
			if err != nil {
				t.Fatalf("an ordered ON-source index must build through the generator: %v", err)
			}
			idx := tmpl.Underlying().GetIndex(c.indexName)
			if idx == nil {
				t.Fatalf("%s was not built", c.indexName)
			}
			if !proto.Equal(idx.RootExpression.ToKeyExpression(), c.want.ToKeyExpression()) {
				t.Errorf("%s root = %v, want %v — the order clause selects the exact "+
					"OrderFunctionKeyExpression wrapper Java stores", c.indexName,
					idx.RootExpression.ToKeyExpression(), c.want.ToKeyExpression())
			}
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

// TestDDLHonorsAsSelectIndexAttributes is the RETIRED form of the S0
// LEGACY_EXTREMUM_EVER rejection: the aggregate arm (RFC-202 S3) now reads
// the attribute and emits MIN_EVER_LONG / MAX_EVER_LONG — the different index
// type with the different maintainer writing a packed long rather than a
// tuple (MaterializedViewIndexGenerator.java:449-465). Four corpus statements
// carry the shape; the sub-tests are verbatim from them, including the
// lower-case spelling.
func TestDDLHonorsAsSelectIndexAttributes(t *testing.T) {
	t.Parallel()
	const table = `CREATE TABLE t1 (pk integer, col1 integer, col2 integer, PRIMARY KEY (pk))
		`
	cases := []struct{ name, indexName, wantType, index string }{
		// aggregate-index-tests.yamsql:39-40
		{"min_ever_upper", "MV12", "min_ever_long", `CREATE INDEX mv12 AS SELECT min_ever(col2) FROM t1 GROUP BY col1 with attributes LEGACY_EXTREMUM_EVER`},
		{"max_ever_upper", "MV13", "max_ever_long", `CREATE INDEX mv13 AS SELECT max_ever(col2) FROM t1 GROUP BY col1 with attributes LEGACY_EXTREMUM_EVER`},
		// index-ddl.yamsql:82-83 spell the attribute in lower case.
		{"min_ever_lower", "IDX_MV_AGE_MIN_EXTREMUM", "min_ever_long", `CREATE INDEX idx_mv_age_min_extremum AS SELECT min_ever(col2) FROM t1 GROUP BY col1 with attributes legacy_extremum_ever`},
		{"max_ever_lower", "IDX_MV_AGE_MAX_EXTREMUM", "max_ever_long", `CREATE INDEX idx_mv_age_max_extremum AS SELECT max_ever(col2) FROM t1 GROUP BY col1 with attributes legacy_extremum_ever`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tmpl, err := buildSchemaTemplateFromDDL(table + c.index)
			if err != nil {
				t.Fatalf("LEGACY_EXTREMUM_EVER must be honored by the aggregate arm: %v", err)
			}
			idx := tmpl.Underlying().GetIndex(c.indexName)
			if idx == nil {
				t.Fatalf("%s was not built", c.indexName)
			}
			if idx.Type != c.wantType {
				t.Errorf("index type = %q, want %q — the attribute selects the LONG-based "+
					"extremum maintainer; the tuple type here means the attribute was silently dropped",
					idx.Type, c.wantType)
			}
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
		// RETIRED rejection: the aggregate arm (RFC-202 S3) routes the
		// definition through the generator, and UNIQUE is the caller's flag
		// exactly as in Java (DdlVisitor.java:215 → generate(…, isUnique, …)).
		t.Parallel()
		tmpl, err := buildSchemaTemplateFromDDL(table +
			`CREATE UNIQUE INDEX mvu AS SELECT min_ever(col2) FROM t1 GROUP BY col1`)
		if err != nil {
			t.Fatalf("UNIQUE on an aggregate AS-SELECT index must be supported by the generator: %v", err)
		}
		idx := tmpl.Underlying().GetIndex("MVU")
		if idx == nil {
			t.Fatal("MVU was not built")
		}
		if !idx.IsUnique() {
			t.Error("UNIQUE dropped — the exact silent drop this test was born to prevent")
		}
		if idx.Type != "min_ever_tuple" {
			t.Errorf("index type = %q, want min_ever_tuple", idx.Type)
		}
	})
	t.Run("where_on_aggregate", func(t *testing.T) {
		// RETIRED rejection: the predicate arm (RFC-202 S5) attaches the
		// WHERE before the value/aggregate split, exactly as Java does
		// (MaterializedViewIndexGenerator.java:169-174) — a sparse aggregate
		// index stores its predicate.
		t.Parallel()
		tmpl, err := buildSchemaTemplateFromDDL(table +
			`CREATE INDEX mvw AS SELECT min_ever(col2) FROM t1 WHERE col1 > 3 GROUP BY col1`)
		if err != nil {
			t.Fatalf("WHERE on an aggregate AS-SELECT index must build through the predicate arm: %v", err)
		}
		idx := tmpl.Underlying().GetIndex("MVW")
		if idx == nil {
			t.Fatal("MVW was not built")
		}
		if idx.GetPredicateProto() == nil {
			t.Error("WHERE dropped from the aggregate index — a FULL aggregate maintained where Java maintains a sparse one")
		}
	})
	t.Run("unique_on_cardinality", func(t *testing.T) {
		// RETIRED rejection: the AS-SELECT value arm (RFC-202 S2) routes a
		// cardinality index through the generator, which carries UNIQUE into
		// the index options exactly as Java does (DdlVisitor.java:215). The
		// pin flips from "fail closed" to "supported and honored".
		t.Parallel()
		tmpl, err := buildSchemaTemplateFromDDL(
			`CREATE TABLE tab (id integer, int_arr integer array null, PRIMARY KEY (id))
			 CREATE UNIQUE INDEX tab_card AS SELECT CARDINALITY(int_arr) AS card FROM tab ORDER BY card`)
		if err != nil {
			t.Fatalf("UNIQUE on a cardinality value index must be supported by the generator: %v", err)
		}
		idx := tmpl.Underlying().GetIndex("TAB_CARD")
		if idx == nil {
			t.Fatal("TAB_CARD was not built")
		}
		if !idx.IsUnique() {
			t.Error("UNIQUE dropped — the exact silent drop this test was born to prevent")
		}
	})
	t.Run("where_on_cardinality", func(t *testing.T) {
		// RETIRED rejection: the predicate arm (RFC-202 S5) serializes the
		// WHERE into the index metadata (MaterializedViewIndexGenerator.java:169-172)
		// — the pin flips from "fail closed" to "supported and stored".
		t.Parallel()
		tmpl, err := buildSchemaTemplateFromDDL(
			`CREATE TABLE tab (id integer, x integer, int_arr integer array null, PRIMARY KEY (id))
			 CREATE INDEX tab_card AS SELECT CARDINALITY(int_arr) AS card FROM tab WHERE x > 3 ORDER BY card`)
		if err != nil {
			t.Fatalf("WHERE on a cardinality value index must build through the predicate arm: %v", err)
		}
		idx := tmpl.Underlying().GetIndex("TAB_CARD")
		if idx == nil {
			t.Fatal("TAB_CARD was not built")
		}
		if idx.GetPredicateProto() == nil {
			t.Error("WHERE dropped — a FULL index built where Java builds a sparse one")
		}
	})
}

// TestDDLValueIndexUniqueAndSparseAfterGenerator pins the S2-era successors
// of the S0 ordering pin. The value-index gap this test used to bucket by is
// CLOSED (RFC-202 S2): a non-aggregate AS-SELECT index now builds through the
// generator, so the two shapes that used to share one rejection diverge —
// UNIQUE is honored (index-ddl.yamsql IDX_MV_UNIQUE_PROFESSION), and WHERE —
// fail-closed until the predicate arm landed (RFC-202 S5) — now builds a
// sparse index whose predicate is stored (sparse-index-tests.yamsql shapes).
func TestDDLValueIndexUniqueAndSparseAfterGenerator(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t1 (pk integer, profession string, PRIMARY KEY (pk))
		 CREATE UNIQUE INDEX idx_mv_unique_profession AS SELECT profession FROM t1 ORDER BY profession`)
	if err != nil {
		t.Fatalf("a UNIQUE value AS-SELECT index must build through the generator: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("IDX_MV_UNIQUE_PROFESSION")
	if idx == nil {
		t.Fatal("IDX_MV_UNIQUE_PROFESSION was not built")
	}
	if !idx.IsUnique() {
		t.Error("UNIQUE dropped from the generated value index")
	}

	// RETIRED rejection (RFC-202 S5): the WHERE now builds a SPARSE index —
	// the predicate is stored in the metadata, never silently dropped.
	tmpl2, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t1 (pk integer, p integer, PRIMARY KEY (pk))
		 CREATE INDEX mv1 AS SELECT p FROM t1 WHERE p > 10 ORDER BY p`)
	if err != nil {
		t.Fatalf("a WHERE (sparse) value index must build through the predicate arm: %v", err)
	}
	sparse := tmpl2.Underlying().GetIndex("MV1")
	if sparse == nil {
		t.Fatal("MV1 was not built")
	}
	if sparse.GetPredicateProto() == nil {
		t.Error("WHERE dropped — a FULL index built holding entries Java's sparse index deliberately omits")
	}
}
