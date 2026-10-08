package sqldriver_test

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

// A window's OPTIONS EF_SEARCH is parsed, checked and applied as the target's
// (conformance/window_options_conformance_test.go): the value is
// ParseHelpers.parseDecimal'd, a repeated option is refused, an int-range check
// follows, and the scan searches with the value as given, so an efSearch below
// k returns fewer rows and a huge one returns them all.
func TestFDB_WindowOptionsEfSearch(t *testing.T) {
	t.Parallel()
	db := testkit.SetupPlanShapeDB(t, "winopt", `CREATE TABLE v (id BIGINT, emb VECTOR(3, FLOAT), PRIMARY KEY (id)) `+
		`CREATE VECTOR INDEX vi USING HNSW ON v (emb)`)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO v VALUES (1, CAST([1.0, 0.0, 0.0] AS VECTOR(3, FLOAT))), `+
		`(2, CAST([0.0, 1.0, 0.0] AS VECTOR(3, FLOAT))), (3, CAST([0.0, 0.0, 1.0] AS VECTOR(3, FLOAT))), `+
		`(4, CAST([0.8, 0.2, 0.0] AS VECTOR(3, FLOAT)))`); err != nil {
		t.Fatal(err)
	}
	const (
		duplicate  = "The function is not defined for the given argument types option specified more than once"
		outOfRange = "A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable. option value is out of range for the option's type"
	)
	for _, c := range []struct {
		options string
		want    []int64
		code    api.ErrorCode
		message string
	}{
		{``, []int64{4, 1, 2}, "", ""},
		{` OPTIONS EF_SEARCH = 100`, []int64{4, 1, 2}, "", ""},
		{` OPTIONS EF_SEARCH = 10L`, []int64{4, 1, 2}, "", ""},
		{` OPTIONS EF_SEARCH = 10I`, []int64{4, 1, 2}, "", ""},
		{` OPTIONS EF_SEARCH = 1`, []int64{4}, "", ""},
		{` OPTIONS EF_SEARCH = 0`, []int64{4}, "", ""},
		{` OPTIONS EF_SEARCH = 2147483647`, []int64{4, 1, 2}, "", ""},
		{` OPTIONS EF_SEARCH = 10, EF_SEARCH = 10`, nil, api.ErrCodeInvalidArgumentForFunction, duplicate},
		{` OPTIONS EF_SEARCH = 10, EF_SEARCH = 20`, nil, api.ErrCodeInvalidArgumentForFunction, duplicate},
		{` OPTIONS EF_SEARCH = 3000000000, EF_SEARCH = 3000000000`, nil, api.ErrCodeInvalidArgumentForFunction, duplicate},
		{` OPTIONS EF_SEARCH = 2147483648`, nil, api.ErrCodeCannotConvertType, outOfRange},
		{` OPTIONS EF_SEARCH = 2147483648L`, nil, api.ErrCodeCannotConvertType, outOfRange},
		{` OPTIONS EF_SEARCH = 3000000000I`, nil, api.ErrCodeUnknown, `For input string: "3000000000"`},
		{` OPTIONS EF_SEARCH = 99999999999999999999`, nil, api.ErrCodeUnknown, `For input string: "99999999999999999999"`},
		{` OPTIONS EF_SEARCH = 3000000000I, EF_SEARCH = 1, EF_SEARCH = 1`, nil, api.ErrCodeUnknown, `For input string: "3000000000"`},
	} {
		sql := `SELECT id FROM v QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(emb, CAST([0.9, 0.1, 0.0] AS VECTOR(3, FLOAT)))` +
			c.options + `) <= 3`
		got, err := testkit.QueryInt64s(ctx, db, sql)
		if c.code != "" {
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != c.code || apiErr.Message != c.message {
				t.Fatalf("%s: error %v, want %s %q", c.options, err, c.code, c.message)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.options, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%s: rows %v, want %v", c.options, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: rows %v, want %v", c.options, got, c.want)
			}
		}
	}
}
