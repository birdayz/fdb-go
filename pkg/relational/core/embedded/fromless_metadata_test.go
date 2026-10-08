package embedded

import (
	"context"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// A FROM-less SELECT's result metadata takes each expression's own type and
// nullability, as a SELECT over a table does. Each expectation is the Java
// 4.14.2.0 outcome the WS-E oracle measured for the same expression over a
// table (conformance/ws_e_probe_conformance_test.go, the row named beside it);
// FromlessSelectJavaProbe compares the FROM-less forms between the engines.
func TestFromlessSelect_ResultMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		sql      string
		typeName string
		nullable int
		measured string
	}{
		{"SELECT 7", "INTEGER", api.ColumnNoNulls, "nn_literal"},
		{"SELECT 1 + 2", "INTEGER", api.ColumnNullable, "nn_add_literals"},
		{"SELECT 5 % 2", "INTEGER", api.ColumnNullable, "nn_mod_literals"},
		{"SELECT COALESCE(1, 2)", "INTEGER", api.ColumnNoNulls, "nn_coalesce_literals"},
		{"SELECT GREATEST(1, 5)", "INTEGER", api.ColumnNoNulls, "nn_greatest_literals"},
		{"SELECT CAST(1 AS BIGINT)", "BIGINT", api.ColumnNoNulls, "cast_literal_nullability_select"},
		{"SELECT NOT FALSE", "BOOLEAN", api.ColumnNullable, "not_false_select"},
		{"SELECT TRUE AND TRUE", "BOOLEAN", api.ColumnNullable, "true_and_true_select"},
		{"SELECT 'a' 'b'", "STRING", api.ColumnNoNulls, "literal_adjacent"},
	}
	for _, test := range tests {
		t.Run(test.measured, func(t *testing.T) {
			t.Parallel()
			g, md := newLoggingGenerator(t, "CREATE TABLE t (id BIGINT, PRIMARY KEY (id))", &captureLogger{})
			planned, err := g.planSelectCascades(context.Background(), parseQuery(t, test.sql), md, true, statementOptions{})
			if err != nil {
				t.Fatalf("plan %q: %v", test.sql, err)
			}
			columns := resultColumns(planned.(*cascadesPlan).physicalPlan)
			if len(columns) != 1 || columns[0].TypeName != test.typeName || columns[0].Nullable != test.nullable {
				t.Fatalf("%s: columns = %+v, want one %s with nullability %v", test.sql, columns, test.typeName, test.nullable)
			}
		})
	}
}
