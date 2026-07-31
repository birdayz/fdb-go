package javacorpus

import (
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestResultMetadataTypeNameVocabulary pins the type-NAME vocabulary a
// `resultMetadata:` directive can name against Java's.
//
// The directive compares one string, case-insensitively, so the whole
// assertion rests on Go and Java spelling a type the same way. Java derives the
// spelling in two hops — DataType.Code → java.sql.Types via
// `DataType.typeCodeJdbcTypeMap` (DataType.java:69-82), then that code → a name
// via `SqlTypeNamesSupport.getSqlTypeName`. Go has the same two hops
// (`api.JDBCType`, `api.SQLTypeNameFromJDBC`), and this table is the third
// party that says the composition agrees. Without it "identical names" is an
// assertion nobody checks, and a rename on either side would turn every
// metadata expectation red with no indication of which side moved.
//
// `want` is Java's answer, read off SqlTypeNamesSupport, not Go's.
func TestResultMetadataTypeNameVocabulary(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code api.Code
		want string
	}{
		{api.CodeBoolean, "BOOLEAN"},
		{api.CodeLong, "BIGINT"},
		{api.CodeInteger, "INTEGER"},
		{api.CodeFloat, "FLOAT"},
		{api.CodeDouble, "DOUBLE"},
		{api.CodeString, "STRING"},
		{api.CodeBytes, "BINARY"},
		{api.CodeVersion, "BINARY"},
		{api.CodeEnum, "OTHER"},
		{api.CodeUUID, "OTHER"},
		{api.CodeVector, "OTHER"},
		{api.CodeStruct, "STRUCT"},
		{api.CodeArray, "ARRAY"},
		{api.CodeNull, "NULL"},
	}

	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			t.Parallel()
			got := string(api.SQLTypeNameFromJDBC(api.JDBCType(tc.code)))
			if got != tc.want {
				t.Fatalf("%s → %q, Java's SqlTypeNamesSupport says %q — a `resultMetadata:` "+
					"expectation naming %s can no longer match", tc.code, got, tc.want, tc.want)
			}
		})
	}

	// Two deliberate departures, recorded so neither reads as an oversight:
	//
	//   - DATE / TIMESTAMP have no DataType.Code in Java at this tag, so no
	//     corpus expectation can name them and no shared spelling exists to
	//     conform to. Go's names are its own.
	//   - UNKNOWN has a Code in Java but no entry in typeCodeJdbcTypeMap, so
	//     Java's getJdbcSqlCode throws on it. Go answers OTHER rather than
	//     panicking, which cannot break a shared expectation because Java can
	//     never produce one.
	for _, c := range []api.Code{api.CodeDate, api.CodeTimestamp, api.CodeUnknown} {
		if api.SQLTypeNameFromJDBC(api.JDBCType(c)) == "" {
			t.Errorf("%s must still map to SOME name; an empty one would make a metadata "+
				"mismatch report a blank actual type", c)
		}
	}
}

// TestArrayTypeNameIsNotSurfacedByTheDriver pins the measured shape of the
// driver gap SkipResultMetadataNested exists for, from the matcher's side.
//
// Java names an array column "ARRAY(elem)"
// (CheckResultMetadataConfig.buildArrayTypeName). Go's driver reports the
// column's ELEMENT type name instead — `valueTypeName` has no TypeCodeArray arm
// and `protoKindToTypeName` ignores field cardinality — so the two disagree on
// every array column. This test states the consequence in the matcher's own
// terms: the Java spelling and the Go spelling do not match each other, which
// is why an array expectation is declined by name rather than compared.
//
// When the driver starts carrying the array type, this test is the one that
// must be deleted, and deleting it is the signal to stop declining.
func TestArrayTypeNameIsNotSurfacedByTheDriver(t *testing.T) {
	t.Parallel()

	// What Java would report for `x integer array`, and what Go's driver
	// actually reports for the same column.
	const javaName = "ARRAY(INTEGER)"
	goName := string(api.SQLTypeNameFromJDBC(api.JDBCType(api.CodeInteger)))

	if javaName == goName {
		t.Fatal("Go now spells an array column the way Java does; the array half of " +
			"SkipResultMetadataNested is closed and the runner should compare instead of declining")
	}

	// And the matcher agrees: an `{array: INTEGER}` expectation does not match
	// a descriptor carrying the bare element name.
	cfg := metadataConfigFrom(t, "resultMetadata: [{X: {array: INTEGER}}]")
	if err := matchMetadata(cfg.Raw, []columnDescriptor{{Name: "X", TypeName: goName}}); err == nil {
		t.Fatal("an array expectation matched a bare element type name — the descriptor " +
			"gap would then be invisible instead of counted")
	}
}
