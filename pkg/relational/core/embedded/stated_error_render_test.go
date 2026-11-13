package embedded

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// A DDL refusal whose message is its typed cause's text renders that text once.
func TestDDL_StatedCauseRendersOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, ddl, want string }{
		{"column name", `create table t(id bigint, "A'B" bigint, primary key(id))`, "42602: A'B it not a valid protobuf identifier"},
		{"enum value", `create type as enum mood('O''NEIL') create table t(id bigint, m mood, primary key(id))`, "42602: O'NEIL it not a valid protobuf identifier"},
		{"index predicate", `create table t(id bigint, u uuid, primary key(id)) create index ix as select id from t where u = '5ea7a4c2-54a4-4f7a-9d0e-2d1b0b3f8e10' order by id`, "XXXXX: Unsupported value type class java.util.UUID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildSchemaTemplateFromDDL(tc.ddl)
			var ae *api.Error
			if !errors.As(err, &ae) || ae.Error() != tc.want || errors.Unwrap(ae) == nil {
				t.Fatalf("got %v, want %q with its typed cause", err, tc.want)
			}
		})
	}
}
