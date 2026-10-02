package sqldriver_test

import (
	"context"
	"testing"
)

func TestFDB_DisjunctiveExists(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	const schema = `CREATE TABLE t (id BIGINT, a BIGINT, arr BIGINT ARRAY, PRIMARY KEY(id))
		CREATE TABLE u (id BIGINT, k BIGINT, PRIMARY KEY(id))
		CREATE TABLE v (id BIGINT, PRIMARY KEY(id)) `
	for _, tc := range []struct {
		name, sql string
		want      []string
	}{
		{"correlated", `SELECT id FROM t WHERE a=9 OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id) ORDER BY id`, []string{"1", "2", "3", "4", "5"}},
		{"negated", `SELECT id FROM t WHERE a=9 OR NOT EXISTS (SELECT 1 FROM u WHERE u.k=t.id) ORDER BY id`, []string{"2", "4", "5", "6"}},
		{"independent_true", `SELECT id FROM t WHERE a=9 OR EXISTS (SELECT 1 FROM u WHERE k=1) ORDER BY id`, []string{"1", "2", "3", "4", "5", "6"}},
		{"independent_false", `SELECT id FROM t WHERE a=9 OR EXISTS (SELECT 1 FROM u WHERE k=100) ORDER BY id`, []string{"2", "4", "5"}},
		{"null_or_false", `SELECT id FROM t WHERE a=99 OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id) ORDER BY id`, []string{"1", "3", "4"}},
		{"two_exists", `SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.k=t.id) OR EXISTS (SELECT 1 FROM v WHERE v.id=t.id) ORDER BY id`, []string{"1", "2", "3", "4"}},
		{"three_exists", `SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.k=t.id) OR EXISTS (SELECT 1 FROM v WHERE v.id=t.id) OR EXISTS (SELECT 1 FROM u WHERE u.id=t.id) ORDER BY id`, []string{"1", "2", "3", "4", "5"}},
		{"independent_pair_false", `SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE k=100) OR EXISTS (SELECT 1 FROM v WHERE id=100) ORDER BY id`, nil},
		{"independent_pair_true", `SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE k=100) OR EXISTS (SELECT 1 FROM v) ORDER BY id`, []string{"1", "2", "3", "4", "5", "6"}},
		{"limited", `SELECT id FROM t WHERE a=9 OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id) ORDER BY id LIMIT 2 OFFSET 2`, []string{"3", "4"}},
		{"two_negated", `SELECT id FROM t WHERE NOT EXISTS (SELECT 1 FROM u WHERE u.k=t.id) OR NOT EXISTS (SELECT 1 FROM v WHERE v.id=t.id) ORDER BY id`, []string{"1", "2", "3", "5", "6"}},
		{"nested_boolean", `SELECT id FROM t WHERE (a=9 AND EXISTS (SELECT 1 FROM u WHERE u.k=t.id)) OR (id=6 AND NOT EXISTS (SELECT 1 FROM v WHERE v.id=t.id)) ORDER BY id`, []string{"4", "6"}},
		{"not_or", `SELECT id FROM t WHERE NOT (a=9 OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id)) ORDER BY id`, []string{"6"}},
		{"nested_child", `SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.k=t.id OR EXISTS (SELECT 1 FROM v WHERE v.id=t.id)) ORDER BY id`, []string{"1", "2", "3", "4"}},
		{"not_nested_child", `SELECT id FROM t WHERE NOT EXISTS (SELECT 1 FROM u WHERE u.k=t.id OR EXISTS (SELECT 1 FROM v WHERE v.id=t.id)) ORDER BY id`, []string{"5", "6"}},
		{"known_true", `SELECT id FROM t WHERE a=99 OR EXISTS (SELECT COUNT(*) FROM u WHERE u.k=t.id) ORDER BY id`, []string{"1", "2", "3", "4", "5", "6"}},
		{"known_false", `SELECT id FROM t WHERE a=9 OR EXISTS (SELECT id FROM u WHERE u.k=t.id LIMIT 0) ORDER BY id`, []string{"2", "4", "5"}},
		{"derived", `SELECT x.id FROM (SELECT id FROM t WHERE a=9 OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id)) x ORDER BY x.id`, []string{"1", "2", "3", "4", "5"}},
		{"inner_on", `SELECT t.id, v.id FROM t JOIN v ON t.id=v.id OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id) ORDER BY t.id,v.id`, []string{"1|2", "1|4", "2|2", "3|2", "3|4", "4|2", "4|4"}},
		{"join_where", `SELECT t.id, v.id FROM t,v WHERE t.id=v.id OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id) ORDER BY t.id,v.id`, []string{"1|2", "1|4", "2|2", "3|2", "3|4", "4|2", "4|4"}},
		{"unnest", `SELECT t.id,x FROM t,t.arr x WHERE x=9 OR EXISTS (SELECT 1 FROM u WHERE u.k=x) ORDER BY t.id,x`, []string{"1|1", "1|9", "1|9", "2|3", "2|9"}},
		{"unnest_at", `SELECT t.id,x,p FROM t,t.arr x AT p WHERE x=9 OR NOT EXISTS (SELECT 1 FROM v WHERE v.id=p) ORDER BY t.id,p`, []string{"1|1|1", "1|9|2", "1|9|3", "2|3|1", "2|9|2"}},
		{"first_item_unnest", `SELECT id FROM t WHERE EXISTS (SELECT x FROM t.arr x WHERE x=9 OR EXISTS (SELECT 1 FROM v WHERE v.id=x)) ORDER BY id`, []string{"1", "2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := mmNewTwin(t, context.Background(), "/testdb_disj_exists_"+tc.name, "disj_exists_"+tc.name, schema, "CREATE INDEX ix_a ON t(a) CREATE INDEX ix_k ON u(k) ")
			w.Exec(`INSERT INTO t VALUES (1,0,[1,9,9]),(2,9,[3,9]),(3,NULL,[]),(4,9,NULL),(5,9,[]),(6,0,[])`)
			w.Exec(`INSERT INTO u VALUES (1,99),(2,1),(3,1),(4,3),(5,4)`)
			w.Exec(`INSERT INTO v VALUES (2),(4)`)
			w.Want(tc.name, tc.sql, tc.want)
		})
	}
}

func TestFDB_DisjunctiveExistsDML(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	for _, tc := range []struct {
		name, statement, query string
		want                   []string
	}{
		{"update", `UPDATE t SET a=7 WHERE a=9 OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id)`, `SELECT id,a FROM t ORDER BY id`, []string{"1|7", "2|7", "3|7", "4|7", "5|7", "6|0"}},
		{"delete", `DELETE FROM t WHERE a=9 OR NOT EXISTS (SELECT 1 FROM u WHERE u.k=t.id)`, `SELECT id FROM t ORDER BY id`, []string{"1", "3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := mmNewTwin(t, context.Background(), "/testdb_disj_dml_"+tc.name, "disj_dml_"+tc.name,
				`CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY(id)) CREATE TABLE u (id BIGINT, k BIGINT, PRIMARY KEY(id)) `,
				`CREATE INDEX ix_a ON t(a) CREATE INDEX ix_k ON u(k) `)
			w.Exec(`INSERT INTO t VALUES (1,0),(2,9),(3,NULL),(4,9),(5,9),(6,0)`)
			w.Exec(`INSERT INTO u VALUES (1,99),(2,1),(3,1),(4,3),(5,4)`)
			w.Exec(tc.statement)
			w.Want(tc.name, tc.query, tc.want)
		})
	}
}
