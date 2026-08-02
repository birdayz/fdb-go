package embedded

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// RFC-202 S6: the ON-source form is a FRONT END over the same generator the
// AS-SELECT form uses (OnSourceIndexGenerator.java:172-229 synthesizes
// projection + ORDER BY and delegates to MaterializedViewIndexGenerator at
// :227-228). These tests pin the front end's own responsibilities: the
// key/INCLUDE projection with the :173-176 dedup filter, per-column order
// clauses, and byte-identity with the AS-SELECT twin.

// TestOnSourceIndex_TwinOfAsSelect pins the D2 property directly: an
// ON-source declaration and its AS-SELECT twin produce byte-identical root
// key expressions, because both run through the one generator.
func TestOnSourceIndex_TwinOfAsSelect(t *testing.T) {
	t.Parallel()
	const table = `CREATE TABLE T (id bigint, a bigint, b string, c bigint, PRIMARY KEY(id))
		`
	cases := []struct{ name, onSource, asSelect string }{
		{
			"plain_multi_column",
			`CREATE INDEX x ON T(a, b)`,
			`CREATE INDEX x AS SELECT a, b FROM T ORDER BY a, b`,
		},
		{
			"desc_column",
			`CREATE INDEX x ON T(a DESC)`,
			`CREATE INDEX x AS SELECT a FROM T ORDER BY a DESC`,
		},
		{
			"include_covering",
			`CREATE INDEX x ON T(a) INCLUDE (b, c)`,
			`CREATE INDEX x AS SELECT a, b, c FROM T ORDER BY a`,
		},
		{
			"desc_key_with_include",
			`CREATE INDEX x ON T(a DESC, b) INCLUDE (c)`,
			`CREATE INDEX x AS SELECT a, b, c FROM T ORDER BY a DESC, b`,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			onTmpl, err := buildSchemaTemplateFromDDL(table + c.onSource)
			if err != nil {
				t.Fatalf("ON-source DDL failed: %v", err)
			}
			asTmpl, err := buildSchemaTemplateFromDDL(table + c.asSelect)
			if err != nil {
				t.Fatalf("AS-SELECT twin DDL failed: %v", err)
			}
			onIdx := onTmpl.Underlying().GetIndex("X")
			asIdx := asTmpl.Underlying().GetIndex("X")
			if onIdx == nil || asIdx == nil {
				t.Fatalf("index not built: on=%v as=%v", onIdx, asIdx)
			}
			got := onIdx.RootExpression.ToKeyExpression()
			want := asIdx.RootExpression.ToKeyExpression()
			if !proto.Equal(got, want) {
				t.Errorf("ON-source root = %v, AS-SELECT twin root = %v — the two DDL forms "+
					"must produce byte-identical metadata (RFC-202 D2: one generator)", got, want)
			}
			if onIdx.Type != asIdx.Type {
				t.Errorf("index type: on=%q as=%q", onIdx.Type, asIdx.Type)
			}
		})
	}
}

// TestOnSourceIndex_IncludeBuildsKeyWithValue pins the covering shape Java's
// DdlStatementParsingTest.createIndexOnBasicSyntax asserts
// (DdlStatementParsingTest.java:1385-1392): `on bar(a, b) include (c)` builds
// keyWithValue(concat(A, B, C), 2) — the split point is the KEY column count,
// the INCLUDE columns ride in the value part.
func TestOnSourceIndex_IncludeBuildsKeyWithValue(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE bar (id bigint, a bigint, b bigint, c bigint, PRIMARY KEY(id))
		 CREATE INDEX i1 ON bar(a, b) INCLUDE (c)`)
	if err != nil {
		t.Fatalf("INCLUDE DDL failed — the covering front end must build, not fail closed: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("I1")
	if idx == nil {
		t.Fatal("I1 not built")
	}
	want := recordlayer.KeyWithValue(
		recordlayer.Concat(recordlayer.Field("A"), recordlayer.Field("B"), recordlayer.Field("C")), 2)
	if !proto.Equal(idx.RootExpression.ToKeyExpression(), want.ToKeyExpression()) {
		t.Errorf("I1 root = %v, want keyWithValue(concat(A,B,C), 2)",
			idx.RootExpression.ToKeyExpression())
	}
}

// TestOnSourceIndex_IncludeDedupAgainstKeys pins the :173-176 dedup filter
// (RFC-202 D12): an INCLUDE column that repeats a key column is dropped
// BEFORE the projection forms — `ON t(a) INCLUDE(a, b)` is keyWithValue(
// concat(A, B), 1), not a duplicated column with a shifted split point.
func TestOnSourceIndex_IncludeDedupAgainstKeys(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t (id bigint, a bigint, b bigint, PRIMARY KEY(id))
		 CREATE INDEX i1 ON t(a) INCLUDE (a, b)`)
	if err != nil {
		t.Fatalf("INCLUDE-repeats-key DDL failed: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("I1")
	if idx == nil {
		t.Fatal("I1 not built")
	}
	want := recordlayer.KeyWithValue(
		recordlayer.Concat(recordlayer.Field("A"), recordlayer.Field("B")), 1)
	if !proto.Equal(idx.RootExpression.ToKeyExpression(), want.ToKeyExpression()) {
		t.Errorf("I1 root = %v, want keyWithValue(concat(A,B), 1) — the duplicated INCLUDE "+
			"column must be filtered, or the split point shifts", idx.RootExpression.ToKeyExpression())
	}

	// INCLUDE consisting ONLY of key duplicates leaves no value columns —
	// the index degenerates to a plain (non-covering) one.
	tmpl2, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t (id bigint, a bigint, PRIMARY KEY(id))
		 CREATE INDEX i2 ON t(a) INCLUDE (a)`)
	if err != nil {
		t.Fatalf("INCLUDE-only-duplicates DDL failed: %v", err)
	}
	idx2 := tmpl2.Underlying().GetIndex("I2")
	if idx2 == nil {
		t.Fatal("I2 not built")
	}
	if !proto.Equal(idx2.RootExpression.ToKeyExpression(), recordlayer.Field("A").ToKeyExpression()) {
		t.Errorf("I2 root = %v, want plain field(A)", idx2.RootExpression.ToKeyExpression())
	}
}

// TestOnSourceIndex_UniqueCarried: UNIQUE is the caller's flag, threaded to
// the builder exactly as Java's setUnique (DdlVisitor.java:236-242).
func TestOnSourceIndex_UniqueCarried(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t (id bigint, a bigint, PRIMARY KEY(id))
		 CREATE UNIQUE INDEX i1 ON t(a)`)
	if err != nil {
		t.Fatalf("UNIQUE ON-source DDL failed: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("I1")
	if idx == nil {
		t.Fatal("I1 not built")
	}
	if !idx.IsUnique() {
		t.Error("UNIQUE dropped from the ON-source index")
	}
}

// TestOnSourceIndex_UnknownColumn pins Java's resolution failure:
// Assert.notNullUnchecked(column, ErrorCode.UNDEFINED_COLUMN,
// "could not find " + identifier) — 42703, not the 42F59 template wrapper
// the pre-generator path emitted.
func TestOnSourceIndex_UnknownColumn(t *testing.T) {
	t.Parallel()
	for _, ddl := range []string{
		`CREATE TABLE t (id bigint, a bigint, PRIMARY KEY(id))
		 CREATE INDEX i1 ON t(nonexistent)`,
		`CREATE TABLE t (id bigint, a bigint, PRIMARY KEY(id))
		 CREATE INDEX i1 ON t(a) INCLUDE (nonexistent)`,
	} {
		_, err := buildSchemaTemplateFromDDL(ddl)
		if err == nil {
			t.Fatalf("unknown-column DDL was accepted:\n%s", ddl)
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("error is %T (%v), want *api.Error", err, err)
		}
		if apiErr.Code != api.ErrCodeUndefinedColumn {
			t.Errorf("SQLSTATE = %s, want %s (UNDEFINED_COLUMN, Java's ErrorCode): %v",
				apiErr.Code, api.ErrCodeUndefinedColumn, err)
		}
		if !strings.Contains(err.Error(), "could not find NONEXISTENT") {
			t.Errorf("message = %q, want Java's \"could not find NONEXISTENT\"", err.Error())
		}
	}
}

// TestOnSourceIndex_QuotedColumnsKeepDescriptorCase: same property the
// AS-SELECT path pins (index_asselect_quoted_case_test.go) — a quoted
// lower-case column renders with the DESCRIPTOR's case in the stored key
// expression, and resolution is exact on the normalized identifier
// (quoted preserves case, unquoted folds upper — both engines).
func TestOnSourceIndex_QuotedColumnsKeepDescriptorCase(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE T ("col1" bigint, "id" bigint, PRIMARY KEY("id"))
		 CREATE INDEX i1 ON T("col1" DESC)`)
	if err != nil {
		t.Fatalf("quoted-column ON-source DDL failed: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("I1")
	if idx == nil {
		t.Fatal("I1 not built")
	}
	want := recordlayer.FunctionExpr(recordlayer.OrderFuncDescNullsLast, recordlayer.Field("col1"))
	if !proto.Equal(idx.RootExpression.ToKeyExpression(), want.ToKeyExpression()) {
		t.Errorf("I1 root = %v, want order_desc_nulls_last(field(col1)) — the descriptor's exact case",
			idx.RootExpression.ToKeyExpression())
	}

	// The unquoted spelling folds to COL1 and must NOT resolve against the
	// quoted-lower descriptor field — Java's normalized-identifier lookup
	// misses the same way.
	_, err = buildSchemaTemplateFromDDL(
		`CREATE TABLE T ("col1" bigint, "id" bigint, PRIMARY KEY("id"))
		 CREATE INDEX i1 ON T(col1)`)
	if err == nil {
		t.Fatal("unquoted col1 resolved against quoted \"col1\" — normalized lookup must be exact")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedColumn {
		t.Errorf("want 42703 UNDEFINED_COLUMN, got %v", err)
	}
}

// TestOnSourceIndex_RowVersionPseudoColumn: the __ROW_VERSION pseudo-field
// resolves through the ON-source front end exactly as through the AS-SELECT
// resolver (the source access exposes the appended planner-facing pseudo-field
// when the template stores row versions), and the generator's version arm
// types the index VERSION with the version key expression at the column's slot.
func TestOnSourceIndex_RowVersionPseudoColumn(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t (id bigint, a bigint, PRIMARY KEY(id))
		 CREATE INDEX i1 ON t(a, "__ROW_VERSION")
		 WITH OPTIONS(store_row_versions=true)`)
	if err != nil {
		t.Fatalf("__ROW_VERSION ON-source DDL failed: %v", err)
	}
	idx := tmpl.Underlying().GetIndex("I1")
	if idx == nil {
		t.Fatal("I1 not built")
	}
	if idx.Type != recordlayer.IndexTypeVersion {
		t.Errorf("index type = %q, want %q", idx.Type, recordlayer.IndexTypeVersion)
	}
	want := recordlayer.Concat(recordlayer.Field("A"), recordlayer.VersionKey())
	if !proto.Equal(idx.RootExpression.ToKeyExpression(), want.ToKeyExpression()) {
		t.Errorf("I1 root = %v, want concat(A, version())", idx.RootExpression.ToKeyExpression())
	}

	// Without STORE_ROW_VERSIONS the pseudo-field does not exist — 42703,
	// same as Java's "could not find" (the semantic analyzer has no
	// pseudo-column to resolve).
	_, err = buildSchemaTemplateFromDDL(
		`CREATE TABLE t (id bigint, a bigint, PRIMARY KEY(id))
		 CREATE INDEX i1 ON t("__ROW_VERSION")`)
	if err == nil {
		t.Fatal("__ROW_VERSION resolved without STORE_ROW_VERSIONS")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedColumn {
		t.Errorf("want 42703 UNDEFINED_COLUMN, got %v", err)
	}
}

// TestOnSourceIndex_ArrayColumnRejected: a non-unnested array column cannot
// form a key — the generator's toFieldKeyExpression rejection
// (MaterializedViewIndexGenerator.java:814-819) reaches the ON-source form
// through the same shared tail.
func TestOnSourceIndex_ArrayColumnRejected(t *testing.T) {
	t.Parallel()
	_, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t (id bigint, arr bigint array, PRIMARY KEY(id))
		 CREATE INDEX i1 ON t(arr)`)
	if err == nil {
		t.Fatal("array-column ON-source index was accepted")
	}
	if !strings.Contains(err.Error(), "without unnesting") {
		t.Errorf("message = %q, want the generator's array rejection", err.Error())
	}
}

// TestOnSourceIndex_DuplicateKeyColumn: `ON t(a, a)` reaches the generator's
// trie, whose duplicate-child detection rejects the doubled reference — the
// same emergent path Java takes (FieldValueTrieNode's duplicate key assert).
func TestOnSourceIndex_DuplicateKeyColumn(t *testing.T) {
	t.Parallel()
	_, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t (id bigint, a bigint, PRIMARY KEY(id))
		 CREATE INDEX i1 ON t(a, a)`)
	if err == nil {
		t.Fatal("duplicated key column was accepted")
	}
	if !strings.Contains(err.Error(), "multiple disconnected references") {
		t.Errorf("message = %q, want the trie's duplicate-reference rejection", err.Error())
	}
}

// TestOnSourceIndex_UnknownTable keeps the pre-generator behaviour for a bad
// source: 42F59 naming the unknown table.
func TestOnSourceIndex_UnknownTable(t *testing.T) {
	t.Parallel()
	_, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t (id bigint, a bigint, PRIMARY KEY(id))
		 CREATE INDEX i1 ON nope(a)`)
	if err == nil {
		t.Fatal("unknown-table ON-source index was accepted")
	}
	if !strings.Contains(err.Error(), "unknown table") {
		t.Errorf("message = %q, want it to name the unknown table", err.Error())
	}
}
