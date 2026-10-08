package javacorpus

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// The descending branches are exercised here directly, against the semantics
// CheckResultMetadataConfig.matchesExpected defines, besides the corpus's
// check-result-metadata files, which reach them through the driver's metadata.

// metadataConfigFrom parses a real `.yamsql` document and returns its
// `resultMetadata:` config, so the tests run the same parse→match path the
// corpus does. The trailing `result:` is required — QueryConfig.validateConfigs
// rejects a metadata directive with no result-consuming config.
func metadataConfigFrom(t *testing.T, metadataLine string) *javayamsql.Config {
	t.Helper()
	src := "---\n" +
		"schema_template: create table t(a bigint, primary key(a))\n" +
		"---\n" +
		"test_block:\n" +
		"  tests:\n" +
		"    -\n" +
		"      - query: select a from t\n" +
		"      - " + metadataLine + "\n" +
		"      - result: []\n"
	f, err := javayamsql.Parse("metadata_test.yamsql", []byte(src))
	if err != nil {
		t.Fatalf("parse %q: %v", metadataLine, err)
	}
	for _, b := range f.Blocks {
		if b.Kind != javayamsql.BlockTest {
			continue
		}
		for _, c := range b.Test.Tests[0].Command.Configs {
			if c.Kind == javayamsql.ConfigResultMetadata {
				return c
			}
		}
	}
	t.Fatalf("no resultMetadata config parsed from %q", metadataLine)
	return nil
}

func scalarCols(pairs ...string) []columnDescriptor {
	out := make([]columnDescriptor, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, columnDescriptor{Name: pairs[i], TypeName: pairs[i+1]})
	}
	return out
}

func TestMatchMetadataScalar(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		metadata string
		actual   []columnDescriptor
		wantErr  string
	}{
		{
			name:     "single column matches",
			metadata: "resultMetadata: [{ID: BIGINT}]",
			actual:   scalarCols("ID", "BIGINT"),
		},
		{
			name:     "multiple columns match",
			metadata: "resultMetadata: [{ID: BIGINT}, {COL1: BIGINT}]",
			actual:   scalarCols("ID", "BIGINT", "COL1", "BIGINT"),
		},
		{
			// CheckResultMetadataConfig: "Column names are compared
			// case-insensitively." Both directions, since the corpus writes the
			// expectation in lower case and the driver answers in upper.
			name:     "column name is case-insensitive",
			metadata: "resultMetadata: [{id: BIGINT}, {col1: BIGINT}]",
			actual:   scalarCols("ID", "BIGINT", "COL1", "BIGINT"),
		},
		{
			name:     "type name is case-insensitive",
			metadata: "resultMetadata: [{ID: bigint}]",
			actual:   scalarCols("ID", "BIGINT"),
		},
		{
			name:     "wrong type name",
			metadata: "resultMetadata: [{ID: INTEGER}]",
			actual:   scalarCols("ID", "BIGINT"),
			wantErr:  "result metadata mismatch",
		},
		{
			name:     "wrong column name",
			metadata: "resultMetadata: [{WRONG: BIGINT}, {COL1: BIGINT}]",
			actual:   scalarCols("ID", "BIGINT", "COL1", "BIGINT"),
			wantErr:  "result metadata mismatch",
		},
		{
			// Order is positional in Java (indexed walk), so a permutation of
			// the same names is a mismatch, not a match.
			name:     "wrong column order",
			metadata: "resultMetadata: [{COL1: BIGINT}, {ID: BIGINT}]",
			actual:   scalarCols("ID", "BIGINT", "COL1", "BIGINT"),
			wantErr:  "result metadata mismatch",
		},
		{
			name:     "extra expected column",
			metadata: "resultMetadata: [{ID: BIGINT}, {COL1: BIGINT}]",
			actual:   scalarCols("ID", "BIGINT"),
			wantErr:  "result metadata mismatch",
		},
		{
			name:     "missing expected column",
			metadata: "resultMetadata: [{ID: BIGINT}]",
			actual:   scalarCols("ID", "BIGINT", "COL1", "BIGINT"),
			wantErr:  "result metadata mismatch",
		},
		{
			// An empty result SET still carries column metadata; the check is
			// about columns, not rows.
			name:     "empty actual with empty expectation",
			metadata: "resultMetadata: []",
			actual:   nil,
		},
		{
			// Go is not an older server: no column descriptors for a query that
			// expects some is a derivation gap, and Java's warn-and-skip
			// workaround must not hide it here.
			name:     "no actual columns is a derivation gap, not a mismatch",
			metadata: "resultMetadata: [{ID: BIGINT}]",
			actual:   nil,
			wantErr:  "column-derivation gap",
		},
		{
			// Java: `entry.getValue() instanceof String` — a YAML integer is
			// not a type name.
			name:     "non-string scalar expectation",
			metadata: "resultMetadata: [{ID: 5}]",
			actual:   scalarCols("ID", "BIGINT"),
			wantErr:  "result metadata mismatch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := metadataConfigFrom(t, tc.metadata)
			err := matchMetadata(cfg.Raw, tc.actual)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want match, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want mismatch containing %q, got a match", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// structCol builds the descriptor Java's extractDescriptors produces for a
// plain struct column.
func structCol(name, typeName string, fields ...columnDescriptor) columnDescriptor {
	return columnDescriptor{
		Name: name, TypeName: "STRUCT",
		StructTypeName: typeName, HasStructTypeName: true,
		Fields: fields, HasFields: true,
	}
}

// arrayOfStructCol is the array-of-struct form: typeName "ARRAY(STRUCT)", the
// ELEMENT struct's fields, and isArray true.
func arrayOfStructCol(name, typeName string, fields ...columnDescriptor) columnDescriptor {
	c := structCol(name, typeName, fields...)
	c.TypeName = "ARRAY(STRUCT)"
	c.IsArray = true
	return c
}

func TestMatchMetadataNested(t *testing.T) {
	t.Parallel()

	pt := structCol("PT", "point",
		columnDescriptor{Name: "X", TypeName: "BIGINT"},
		columnDescriptor{Name: "Y", TypeName: "BIGINT"})
	pts := arrayOfStructCol("PTS", "point",
		columnDescriptor{Name: "X", TypeName: "BIGINT"},
		columnDescriptor{Name: "Y", TypeName: "BIGINT"})
	id := columnDescriptor{Name: "ID", TypeName: "BIGINT"}

	cases := []struct {
		name     string
		metadata string
		actual   []columnDescriptor
		want     bool
	}{
		{
			name:     "struct fields match",
			metadata: "resultMetadata: [{ID: BIGINT}, {PT: [{X: BIGINT}, {Y: BIGINT}]}]",
			actual:   []columnDescriptor{id, pt},
			want:     true,
		},
		{
			name:     "struct type name matches",
			metadata: "resultMetadata: [{ID: BIGINT}, {PT: [point, {X: BIGINT}, {Y: BIGINT}]}]",
			actual:   []columnDescriptor{id, pt},
			want:     true,
		},
		{
			name:     "struct type name is case-insensitive",
			metadata: "resultMetadata: [{ID: BIGINT}, {PT: [POINT, {X: BIGINT}, {Y: BIGINT}]}]",
			actual:   []columnDescriptor{id, pt},
			want:     true,
		},
		{
			name:     "wrong struct type name",
			metadata: "resultMetadata: [{ID: BIGINT}, {PT: [line, {X: BIGINT}, {Y: BIGINT}]}]",
			actual:   []columnDescriptor{id, pt},
			want:     false,
		},
		{
			name:     "wrong struct field name",
			metadata: "resultMetadata: [{ID: BIGINT}, {PT: [{X: BIGINT}, {Z: BIGINT}]}]",
			actual:   []columnDescriptor{id, pt},
			want:     false,
		},
		{
			name:     "wrong struct field type",
			metadata: "resultMetadata: [{ID: BIGINT}, {PT: [{X: BIGINT}, {Y: INTEGER}]}]",
			actual:   []columnDescriptor{id, pt},
			want:     false,
		},
		{
			// The whole point of the isArray flag: a plain field list must NOT
			// match an array-of-struct column, and the `{array: [...]}` form
			// must NOT match a plain struct column. Without the flag both shapes
			// would agree on the field list and the distinction would vanish.
			name:     "plain list does not match an array-of-struct column",
			metadata: "resultMetadata: [{ID: BIGINT}, {PTS: [{X: BIGINT}, {Y: BIGINT}]}]",
			actual:   []columnDescriptor{id, pts},
			want:     false,
		},
		{
			name:     "array form does not match a plain struct column",
			metadata: "resultMetadata: [{ID: BIGINT}, {PT: {array: [{X: BIGINT}, {Y: BIGINT}]}}]",
			actual:   []columnDescriptor{id, pt},
			want:     false,
		},
		{
			name:     "array of struct matches",
			metadata: "resultMetadata: [{ID: BIGINT}, {PTS: {array: [{X: BIGINT}, {Y: BIGINT}]}}]",
			actual:   []columnDescriptor{id, pts},
			want:     true,
		},
		{
			name:     "array of struct with type name",
			metadata: "resultMetadata: [{ID: BIGINT}, {PTS: {array: [point, {X: BIGINT}, {Y: BIGINT}]}}]",
			actual:   []columnDescriptor{id, pts},
			want:     true,
		},
		{
			name:     "array of scalar matches by built type name",
			metadata: "resultMetadata: [{PK: INTEGER}, {X: {array: INTEGER}}]",
			actual: []columnDescriptor{
				{Name: "PK", TypeName: "INTEGER"},
				{Name: "X", TypeName: "ARRAY(INTEGER)"},
			},
			want: true,
		},
		{
			name:     "array of scalar with wrong element type",
			metadata: "resultMetadata: [{PK: INTEGER}, {X: {array: BIGINT}}]",
			actual: []columnDescriptor{
				{Name: "PK", TypeName: "INTEGER"},
				{Name: "X", TypeName: "ARRAY(INTEGER)"},
			},
			want: false,
		},
		{
			name:     "nested array builds a nested type name",
			metadata: "resultMetadata: [{X: {array: {array: INTEGER}}}]",
			actual:   scalarCols("X", "ARRAY(ARRAY(INTEGER))"),
			want:     true,
		},
		{
			// A map with no `array` key yields Java's "ARRAY(null)" sentinel,
			// which equals no real type name.
			name:     "map without an array key never matches",
			metadata: "resultMetadata: [{X: {notarray: INTEGER}}]",
			actual:   scalarCols("X", "ARRAY(INTEGER)"),
			want:     false,
		},
		{
			// `field-named-array.yamsql`: a STRUCT field that happens to be
			// called `array` must be read as a field, not as the array marker.
			// The two are told apart by position — the array marker is the
			// mapping VALUE of a column entry, a field named array is an
			// element of a field LIST.
			name:     "a struct field named array is a field",
			metadata: "resultMetadata: [{COL: [{ARRAY: BIGINT}]}]",
			actual: []columnDescriptor{
				structCol("COL", "s", columnDescriptor{Name: "ARRAY", TypeName: "BIGINT"}),
			},
			want: true,
		},
		{
			// `type-named-array.yamsql`: the array key is matched
			// case-insensitively, so `ARRAY:` is the marker too.
			name:     "the array key is case-insensitive",
			metadata: "resultMetadata: [{X: {ARRAY: INTEGER}}]",
			actual:   scalarCols("X", "ARRAY(INTEGER)"),
			want:     true,
		},
		{
			// The driver truncation, expressed as a matcher fact: a struct
			// expectation against a descriptor with no field list is a
			// mismatch. This is why the runner declines these by NAME instead
			// of comparing — otherwise every one would read as a divergence.
			name:     "struct expectation against a flat descriptor",
			metadata: "resultMetadata: [{PT: [{X: BIGINT}]}]",
			actual:   scalarCols("PT", "STRUCT"),
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := metadataConfigFrom(t, tc.metadata)
			err := matchMetadata(cfg.Raw, tc.actual)
			if got := err == nil; got != tc.want {
				t.Fatalf("match = %v, want %v (err = %v)", got, tc.want, err)
			}
		})
	}
}

// TestMetadataDescends pins the gate that routes a directive to the counted
// skip instead of to the comparison. It is the load-bearing half of the split:
// a descending expectation wrongly classified as scalar would be compared
// against a descriptor the driver never filled in and reported as a Go defect.
func TestMetadataDescends(t *testing.T) {
	t.Parallel()

	cases := []struct {
		metadata string
		want     bool
	}{
		{"resultMetadata: [{ID: BIGINT}]", false},
		{"resultMetadata: [{ID: BIGINT}, {COL1: STRING}]", false},
		{"resultMetadata: []", false},
		{"resultMetadata: [{PT: [{X: BIGINT}]}]", true},
		{"resultMetadata: [{PT: [point, {X: BIGINT}]}]", true},
		{"resultMetadata: [{X: {array: INTEGER}}]", true},
		{"resultMetadata: [{X: {array: [{A: BIGINT}]}}]", true},
		// Mixed: one scalar column does not make the directive assertable.
		{"resultMetadata: [{ID: BIGINT}, {X: {array: INTEGER}}]", true},
		// A malformed expectation is NOT a driver gap. Java rejects a
		// non-String scalar as a plain mismatch, so declining it here would
		// book a broken corpus line as a capability Go lacks — and would hide
		// the very mismatch the file is supposed to surface.
		{"resultMetadata: [{ID: 5}]", false},
		{"resultMetadata: [{ID: true}]", false},
		{"resultMetadata: [{ID: null}]", false},
	}

	for _, tc := range cases {
		t.Run(tc.metadata, func(t *testing.T) {
			t.Parallel()
			cfg := metadataConfigFrom(t, tc.metadata)
			got, why := metadataDescends(cfg.Raw)
			if got != tc.want {
				t.Fatalf("metadataDescends = %v (%q), want %v", got, why, tc.want)
			}
			if got && why == "" {
				t.Fatal("a descending directive must name the column that descends")
			}
		})
	}
}

// TestExtractDescriptorsFromDriverSurface pins what the runner reads back.
// Without result set metadata only `sql.ColumnType.DatabaseTypeName` is known,
// one flat type name per column. With it, the descriptors are Java's
// extractDescriptors over the column DataTypes: STRUCT with its type name and
// fields, ARRAY(STRUCT) likewise, ARRAY(elem) for a scalar or nested array.
func TestExtractDescriptorsFromDriverSurface(t *testing.T) {
	t.Parallel()

	got := extractDescriptors(rs([]string{"ID", "PT"}, []string{"BIGINT", "STRUCT"}))
	if len(got) != 2 {
		t.Fatalf("got %d descriptors, want 2", len(got))
	}
	if got[0].Name != "ID" || got[0].TypeName != "BIGINT" {
		t.Errorf("scalar descriptor = %+v", got[0])
	}
	if got[1].HasFields || got[1].HasStructTypeName || got[1].IsArray {
		t.Errorf("without metadata a STRUCT column has no nested descriptor; got %+v", got[1])
	}
	if extractDescriptors(nil) != nil {
		t.Error("a nil result set has no descriptors")
	}

	point := api.NewStructType("POINT", []api.StructField{
		api.NewStructField("X", api.NewLongType(true), 0),
		api.NewStructField("TAGS", api.NewArrayType(api.NewStringType(false), true), 1),
	}, true)
	cols := []executor.ColumnDef{
		{Name: "ID", TypeName: "BIGINT", DataType: api.NewLongType(false)},
		{Name: "PT", TypeName: "STRUCT", DataType: point},
		{Name: "PTS", TypeName: "ARRAY", DataType: api.NewArrayType(point.WithNullable(false), true)},
		{Name: "M", TypeName: "ARRAY", DataType: api.NewArrayType(api.NewArrayType(api.NewIntegerType(false), false), true)},
	}
	withMeta := rs([]string{"ID", "PT", "PTS", "M"}, []string{"BIGINT", "STRUCT", "ARRAY", "ARRAY"})
	withMeta.Meta = executor.NewResultSetMetaData(cols)
	var b strings.Builder
	for _, d := range extractDescriptors(withMeta) {
		appendDescriptor(&b, d, "")
	}
	want := "ID: BIGINT\n" +
		"PT: STRUCT(POINT)\n" +
		"    X: BIGINT\n" +
		"    TAGS: ARRAY(STRING)\n" +
		"PTS: ARRAY(STRUCT)(POINT)\n" +
		"    X: BIGINT\n" +
		"    TAGS: ARRAY(STRING)\n" +
		"M: ARRAY(ARRAY(INTEGER))\n"
	if b.String() != want {
		t.Errorf("descriptors with metadata:\n%s\nwant:\n%s", b.String(), want)
	}
}
