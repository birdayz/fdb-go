package embedded

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// RFC-202 S4 goldens: the __ROW_VERSION pseudo-field through the AS-SELECT
// index generator — VERSION index type, version() key expression leaves, the
// covering split with the version inside the key run, and the negative error
// shapes. Each case cites the Java golden it was ported from
// (fdb-relational-core IndexTest.java @ 4.12.11.0).
//
// The templates set store_row_versions=true; versionTemplateDDL's T1 mirrors
// the Java tests' single/compound tables.

const versionGoldenDDL = `CREATE SCHEMA TEMPLATE version_golden_template
	CREATE TABLE t1(col1 bigint, col2 string, col3 bigint, col4 bigint, primary key(col1))
	%s
	WITH OPTIONS(store_row_versions=true)`

// buildVersionTemplateWithIndex compiles the version-enabled template plus
// one CREATE INDEX clause and returns the built index.
func buildVersionTemplateWithIndex(t *testing.T, indexDDL string) (*recordlayer.Index, error) {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(strings.Replace(versionGoldenDDL, "%s", indexDDL, 1))
	if err != nil {
		return nil, err
	}
	for _, idx := range tmpl.Underlying().GetAllIndexes() {
		if idx.Name == "MV1" {
			return idx, nil
		}
	}
	t.Fatalf("index MV1 not found after %q", indexDDL)
	return nil, nil
}

func TestAsSelectVersionIndex_KeyExpressionGoldens(t *testing.T) {
	t.Parallel()

	f := recordlayer.Field
	concat := recordlayer.Concat
	version := recordlayer.VersionKey

	cases := []struct {
		name string
		ddl  string
		want recordlayer.KeyExpression
		typ  string
	}{
		// IndexTest.java:861-868 createSimpleVersionIndex.
		{
			"simple_version",
			`CREATE INDEX mv1 AS SELECT "__ROW_VERSION" FROM t1 ORDER BY "__ROW_VERSION"`,
			version(), recordlayer.IndexTypeVersion,
		},
		// IndexTest.java:870-878 createVersionIndexWithAliasedTable.
		{
			"aliased_table",
			`CREATE INDEX mv1 AS SELECT t."__ROW_VERSION" FROM t1 AS t ORDER BY t."__ROW_VERSION"`,
			version(), recordlayer.IndexTypeVersion,
		},
		// IndexTest.java:888-895 createCompoundVersionIndex: keyWithValue(
		// concat(COL2, version, COL3, COL4), 3) — the version INSIDE the key
		// run, the split at the ORDER BY length.
		{
			"compound_version",
			`CREATE INDEX mv1 AS SELECT col2, "__ROW_VERSION", col3, col4 FROM t1 ORDER BY col2, "__ROW_VERSION", col3`,
			recordlayer.KeyWithValue(concat(f("COL2"), version(), f("COL3"), f("COL4")), 3),
			recordlayer.IndexTypeVersion,
		},
		// IndexTest.java:897-905 createVersionIndexWithVersionInValue: ORDER
		// BY col2 only → split 1, the version in the VALUE portion.
		{
			"version_in_value",
			`CREATE INDEX mv1 AS SELECT col2, "__ROW_VERSION", col3, col4 FROM t1 ORDER BY col2`,
			recordlayer.KeyWithValue(concat(f("COL2"), version(), f("COL3"), f("COL4")), 1),
			recordlayer.IndexTypeVersion,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx, err := buildVersionTemplateWithIndex(t, tc.ddl)
			if err != nil {
				t.Fatalf("DDL failed: %v", err)
			}
			want := tc.want.ToKeyExpression()
			got := idx.RootExpression.ToKeyExpression()
			if !proto.Equal(want, got) {
				t.Errorf("key expression mismatch for %q\nwant: %v\ngot:  %v", tc.ddl, want, got)
			}
			if idx.Type != tc.typ {
				t.Errorf("index type = %q, want %q", idx.Type, tc.typ)
			}
		})
	}
}

// TestAsSelectVersionIndex_RealColumnWins pins real-column-wins at the DDL
// arm (the pseudo-field-clash.yamsql t2 shape): a table declaring a REAL
// "__ROW_VERSION" column gets a plain VALUE index on that column, never a
// VERSION index (Java: Type.Record.addPseudoFields skips when the name is
// present, Type.java:2358-2368, so the resolved FieldValue's type is the
// column's own — the generator's type-keyed version partition stays empty).
func TestAsSelectVersionIndex_RealColumnWins(t *testing.T) {
	t.Parallel()
	ddl := `CREATE SCHEMA TEMPLATE version_clash_template
	CREATE TABLE t2(id bigint, col1 bigint, "__ROW_VERSION" string, primary key(id))
	CREATE INDEX mv1 AS SELECT "__ROW_VERSION" FROM t2
	WITH OPTIONS(store_row_versions=true)`
	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("DDL failed: %v", err)
	}
	var idx *recordlayer.Index
	for _, i := range tmpl.Underlying().GetAllIndexes() {
		if i.Name == "MV1" {
			idx = i
		}
	}
	if idx == nil {
		t.Fatal("index MV1 not found")
	}
	if idx.Type != recordlayer.IndexTypeValue {
		t.Errorf("index type = %q, want %q (real column must win over the pseudo-field)",
			idx.Type, recordlayer.IndexTypeValue)
	}
	want := recordlayer.Field("__ROW_VERSION").ToKeyExpression()
	if got := idx.RootExpression.ToKeyExpression(); !proto.Equal(want, got) {
		t.Errorf("key expression mismatch\nwant: %v\ngot:  %v", want, got)
	}
}

// TestAsSelectVersionIndex_Rejections pins the negative shapes
// (IndexTest.java:880-886, :907-918, :931-941, :952-971). Error CODES match
// Java's exactly; the 42703 wording is the engine's central undefined-column
// message (mapColumnResolveError), the 42702 wording is Java's verbatim.
func TestAsSelectVersionIndex_Rejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		ddl     string
		code    api.ErrorCode
		message string
	}{
		// IndexTest.java:880-886 failToCreateVersionIndexWithUnknownTable —
		// UNDEFINED_COLUMN (Java: "Attempting to query non existing column
		// T2.__ROW_VERSION").
		{
			"unknown_alias_qualifier",
			`CREATE SCHEMA TEMPLATE vg
			CREATE TABLE t1(col1 bigint, primary key(col1))
			CREATE INDEX mv1 AS SELECT t2."__ROW_VERSION" FROM t1 AS t ORDER BY t2."__ROW_VERSION"
			WITH OPTIONS(store_row_versions=true)`,
			api.ErrCodeUndefinedColumn, "",
		},
		// IndexTest.java:952-960 versionIndexWithoutStoreRowVersions —
		// UNDEFINED_COLUMN: with store_row_versions=false the pseudo-column
		// does not exist (Java: "Attempting to query non existing column
		// __ROW_VERSION").
		{
			"store_row_versions_false",
			`CREATE SCHEMA TEMPLATE vg
			CREATE TABLE t1(col1 bigint, primary key(col1))
			CREATE INDEX mv1 AS SELECT "__ROW_VERSION" FROM t1 ORDER BY "__ROW_VERSION"
			WITH OPTIONS(store_row_versions=false)`,
			api.ErrCodeUndefinedColumn, "",
		},
		// The option-less template is the same negative: no
		// store_row_versions option means no pseudo-column.
		{
			"no_options_clause",
			`CREATE SCHEMA TEMPLATE vg
			CREATE TABLE t1(col1 bigint, primary key(col1))
			CREATE INDEX mv1 AS SELECT "__ROW_VERSION" FROM t1 ORDER BY "__ROW_VERSION"`,
			api.ErrCodeUndefinedColumn, "",
		},
		// IndexTest.java:962-971 failToCreateVersionIndexWithAmbiguousColumn
		// — AMBIGUOUS_COLUMN, Java's verbatim message. (The multi-table FROM
		// would ALSO fail record-type resolution, but the ambiguity check
		// runs during column resolution, before the generator — same order
		// as Java, where SemanticAnalyzer resolves before the generator.)
		{
			"join_ambiguous",
			`CREATE SCHEMA TEMPLATE vg
			CREATE TABLE t1(col1 bigint, primary key(col1))
			CREATE TABLE t2(col2 bigint, primary key(col2))
			CREATE INDEX mv1 AS SELECT "__ROW_VERSION", t1.col1, t2.col2 FROM t1, t2 ORDER BY "__ROW_VERSION", t1.col1, t2.col2
			WITH OPTIONS(store_row_versions=true)`,
			api.ErrCodeAmbiguousColumn, "Ambiguous reference __ROW_VERSION",
		},
		// IndexTest.java:907-918 createVersionIndexWithNestingFields → the
		// disconnected-references 0A000 is pinned by the struct-DDL arm once
		// CREATE TYPE AS STRUCT lands; the double-version rule is pinned
		// here instead (MaterializedViewIndexGenerator.java:185).
		{
			"two_version_columns",
			`CREATE SCHEMA TEMPLATE vg
			CREATE TABLE t1(col1 bigint, primary key(col1))
			CREATE INDEX mv1 AS SELECT "__ROW_VERSION", "__ROW_VERSION" FROM t1 ORDER BY "__ROW_VERSION"
			WITH OPTIONS(store_row_versions=true)`,
			api.ErrCodeUnsupportedOperation, "Cannot have index with more than one version column",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := buildSchemaTemplateFromDDL(tc.ddl)
			if err == nil {
				t.Fatalf("DDL unexpectedly succeeded: %q", tc.ddl)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is not an *api.Error: %v", err)
			}
			if tc.code != "" && apiErr.Code != tc.code {
				t.Errorf("error code = %s, want %s (%v)", apiErr.Code, tc.code, err)
			}
			if tc.message != "" && !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.message)
			}
		})
	}
}
