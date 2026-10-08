package sqltest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

// Vector index DDL options as Java 4.14's DdlVisitor.parseVectorOptions.
func TestFDB_VectorIndexDDLOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.OpenDB(t, "/FRL/testdb_vecopt")
	for i, tc := range []struct {
		using, options string
		code           api.ErrorCode
	}{
		{"HNSW", "", ""},
		{"HNSW", "OPTIONS (connectivity = 8, METRIC = COSINE_METRIC, use_rabitq = TRUE, maintain_stats_probability = 0.50)", ""},
		{"GUARDIANN", "OPTIONS (primary_cluster_min = 4, metric = EUCLIDEAN_METRIC)", ""},
		{"HNSW", "OPTIONS (primary_cluster_min = 4)", api.ErrCodeUnsupportedOperation},
		{"GUARDIANN", "OPTIONS (connectivity = 8)", api.ErrCodeUnsupportedOperation},
		{"HNSW", "OPTIONS (bogus = 1)", api.ErrCodeUnsupportedOperation},
		{"HNSW", "OPTIONS (connectivity = 8, CONNECTIVITY = 9)", api.ErrCodeSyntaxError},
		{"HNSW", "OPTIONS (connectivity = 1.5)", api.ErrCodeSyntaxError},
		{"HNSW", "OPTIONS (metric = 3)", api.ErrCodeSyntaxError},
	} {
		tmpl := fmt.Sprintf("vecopt_tmpl_%d", i)
		_, err := db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA TEMPLATE %s "+
			"CREATE TABLE T (id BIGINT, v VECTOR(3, FLOAT), PRIMARY KEY (id)) "+
			"CREATE VECTOR INDEX vi USING %s ON T (v) %s", tmpl, tc.using, tc.options))
		var apiErr *api.Error
		switch {
		case tc.code == "" && err != nil:
			t.Errorf("%s %s: %v", tc.using, tc.options, err)
		case tc.code != "" && (!errors.As(err, &apiErr) || apiErr.Code != tc.code):
			t.Errorf("%s %s: want %s, got %v", tc.using, tc.options, tc.code, err)
		}
	}
}
