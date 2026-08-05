package parser

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestDecimalAndNumericAreNotColumnTypes pins the absence of an exact-decimal
// type, and pins WHICH error a user gets for it.
//
// This matters beyond "the feature is missing": the operator guide
// (docs/mt-saas.md) tells application authors to represent money as scaled
// integers in a BIGINT, and that instruction rests entirely on DECIMAL/NUMERIC
// being unavailable. Java has no exact-decimal type either
// (conformance/yamsql/ansi_roster.go, E011-03), so this is a shared gap, not a
// divergence — but nothing pinned it, so a grammar change adding DECIMAL to
// primitiveType would silently make the guide's advice wrong.
//
// The error CODE is half the point. DECIMAL and NUMERIC are lexer tokens that
// appear in no parser rule and are absent from keywordsCanBeId, so they are
// rejected by the PARSER as a syntax error (42601), not by the DDL visitor's
// unsupported-column-type arm (0A000, ddl.go). A reader who expects a
// feature-unsupported message goes looking in the wrong layer; the assertion
// records which layer actually answers.
func TestDecimalAndNumericAreNotColumnTypes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			name: "decimal with precision and scale",
			sql:  "CREATE SCHEMA TEMPLATE s CREATE TABLE t (id BIGINT, amount DECIMAL(10,2), PRIMARY KEY (id))",
		},
		{
			name: "bare decimal",
			sql:  "CREATE SCHEMA TEMPLATE s CREATE TABLE t (id BIGINT, amount DECIMAL, PRIMARY KEY (id))",
		},
		{
			name: "numeric",
			sql:  "CREATE SCHEMA TEMPLATE s CREATE TABLE t (id BIGINT, amount NUMERIC(10,2), PRIMARY KEY (id))",
		},
		{
			name: "cast to decimal",
			sql:  "SELECT CAST(amount AS DECIMAL) FROM t",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tc.sql)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded; an exact-decimal type must stay unavailable, "+
					"because docs/mt-saas.md's scaled-integer money guidance depends on it", tc.sql)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("Parse(%q) returned %T (%v), want *api.Error", tc.sql, err, err)
			}
			if apiErr.Code != api.ErrCodeSyntaxError {
				t.Fatalf("Parse(%q) → SQLSTATE %s, want %s: DECIMAL/NUMERIC are rejected by the "+
					"grammar (absent from primitiveType and from keywordsCanBeId), not by the "+
					"DDL visitor's unsupported-column-type arm",
					tc.sql, apiErr.Code, api.ErrCodeSyntaxError)
			}
		})
	}
}

// TestSupportedNumericColumnTypesStillParse is the control: the assertion above
// must fail because DECIMAL is absent, not because this CREATE TABLE shape is
// unparseable for some unrelated reason. Without it, a grammar break elsewhere
// would leave the decimal test green for the wrong reason.
func TestSupportedNumericColumnTypesStillParse(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"CREATE SCHEMA TEMPLATE s CREATE TABLE t (id BIGINT, amount BIGINT, PRIMARY KEY (id))",
		"CREATE SCHEMA TEMPLATE s CREATE TABLE t (id BIGINT, amount INTEGER, PRIMARY KEY (id))",
		"CREATE SCHEMA TEMPLATE s CREATE TABLE t (id BIGINT, amount DOUBLE, PRIMARY KEY (id))",
		"CREATE SCHEMA TEMPLATE s CREATE TABLE t (id BIGINT, amount FLOAT, PRIMARY KEY (id))",
		"SELECT CAST(amount AS DOUBLE) FROM t",
	} {
		if _, err := Parse(sql); err != nil {
			t.Errorf("Parse(%q) failed: %v", sql, err)
		}
	}
}
