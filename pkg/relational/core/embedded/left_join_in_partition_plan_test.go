package embedded_test

import (
	"testing"

	"fdb.dev/pkg/relational/core/embedded"
)

// TestLeftJoinLegUnderInPlans pins the planning failure of factory scenarios
// fc_0000000534_q3 and fc_0000000556_q5: a LEFT JOIN leg whose WHERE IN rejects
// the null tuple loses its null-on-empty flag (EliminateNullOnEmptyRule), and
// the result value keeps the leg's nullable row. PartitionSelectRule's
// positional merge then typed the leg's slot NOT NULL, and the memo refused
// the member (resolution error 64), which failed the statement. The slot now
// keeps the stated nullable row (positionalMergeCase), and the executor admits
// the leg's NOT NULL rows under it (TestAttachOrdinalLayout_NotNullRowTakesNullableCarrier;
// the factory full corpus executes both scenarios).
func TestLeftJoinLegUnderInPlans(t *testing.T) {
	t.Parallel()
	const ddl = "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) " +
		"CREATE INDEX idx_s ON T_RD (s) CREATE INDEX idx_e ON T_RD (e) CREATE INDEX idx_b ON T_RD (b)"
	for _, sql := range []string{
		"SELECT l.id, m.id, r.id FROM t_rd AS l JOIN t_rd AS m ON l.id = m.id LEFT JOIN t_rd AS r ON m.a = r.id WHERE ((l.e IN (3, 6)) AND (r.b IN (5, 10, 7)))",
		"SELECT l.*, m.*, r.* FROM t_rd AS l JOIN t_rd AS m ON l.id = m.id LEFT JOIN t_rd AS r ON m.a = r.id WHERE ((l.e IN (3, 6)) AND (r.b IN (5, 10, 7)))",
	} {
		if _, err := embedded.PlanQueryForTest(sql, ddl, nil); err != nil {
			t.Errorf("%s: %v", sql, err)
		}
	}
}
