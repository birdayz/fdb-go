package sqldriver_test

// Edge cases of the PERMUTED_MIN null repair, beyond the mixed/all-NULL/NULL-
// free groups the main suite pins.
//
// The repair resolves a stored NULL extremum by seeking PAST the group's NULL
// run in the ordinary subspace: the low endpoint is (group..., NULL) EXCLUSIVE,
// which resolves to Strinc of that packed prefix. Everything here turns on that
// boundary being exactly right.
//
// The values that sit CLOSEST to it — the empty string and empty bytes, whose
// encodings start immediately above NULL's — turn out to be unreachable,
// because MIN/MAX are numeric-only in this dialect. That is not a gap to shrug
// at but a precondition to pin, so the first test below asserts the rejection
// and says what to write if it ever lifts. What remains reachable is covered
// here: NULL group KEYS (where the prefix being seeked from contains a NULL of
// its own), NULL runs long enough that seeking and walking differ, and the
// values at the ends of the numeric domain.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_AggregateIndexMin_NonNumericExtremaAreRejected is a NEGATIVE result,
// and it is load-bearing rather than trivia: it is what makes the repair's seek
// boundary unreachable, and it is the condition under which that stays true.
//
// The repair seeks from (group..., NULL) EXCLUSIVE, i.e. Strinc of that packed
// prefix, which lands on the first byte above NULL's 0x00. The values that sit
// closest to that boundary are the ones with the SHORTEST non-NULL encodings —
// empty BYTES (type code 0x01) and the empty STRING (0x02) — and an off-by-one
// in the seek would skip exactly those while leaving every number untouched.
// They are the only values that could reveal such an error.
//
// They are also unreachable: MIN/MAX are numeric-only here, matching Java
// (min_max_string.yaml pins the same rejection), so no aggregate index can ever
// hold a string or bytes extremum and every value the seek can meet is a number
// whose encoding starts far above 0x00.
//
// If this test fails because one of these is now ACCEPTED, the boundary has
// become reachable and the empty-value case has to be written for real — that
// is what the failure message says, and it is the whole point of pinning a
// restriction rather than assuming it.
func TestFDB_AggregateIndexMin_NonNumericExtremaAreRejected(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_min_nonnum")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_min_nonnum")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE minnn "+
		"CREATE TABLE t (id BIGINT, g BIGINT, s STRING, b BYTES, PRIMARY KEY (id)) ")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_min_nonnum/s WITH TEMPLATE minnn")
	dsn := fmt.Sprintf("fdbsql:///testdb_min_nonnum?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, g, s, b) VALUES (1, 1, '', NULL), (2, 1, NULL, NULL)")

	for _, q := range []string{
		"SELECT g, MIN(s) FROM t GROUP BY g",
		"SELECT g, MAX(s) FROM t GROUP BY g",
		"SELECT g, MIN(b) FROM t GROUP BY g",
		"SELECT MIN(s) FROM t",
	} {
		if _, err := mmRows(t, ctx, db, q); err == nil {
			t.Errorf("%q is now ACCEPTED. A non-numeric extremum can hold the EMPTY string or EMPTY "+
				"bytes, whose tuple encodings (0x02, 0x01) sit immediately above NULL's 0x00 — the "+
				"one place the permuted-MIN null repair's seek boundary can be off by one. Write "+
				"the empty-value-versus-NULL case now that it is reachable: a group holding both a "+
				"NULL and an empty value must answer with the EMPTY value, not NULL.", q)
		}
	}
}

// TestFDB_AggregateIndexMin_NullGroupKey puts the NULL in the GROUPING column
// instead of the value. The repair seeks from a prefix built out of the group
// key, so a NULL group key means the prefix it seeks from itself contains a
// NULL — a different position in the same encoding, and one that a repair
// keying off "is this element nil" could confuse with the value being NULL.
func TestFDB_AggregateIndexMin_NullGroupKey(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_min_nullkey", "minnk",
		"CREATE TABLE t (id BIGINT, g BIGINT, h BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_min_v_g AS SELECT MIN(v) FROM t GROUP BY g "+
			"CREATE INDEX t_min_v_gh AS SELECT MIN(v) FROM t GROUP BY g, h ")

	//  g=NULL : a mixed group under a NULL key      -> MIN 4
	//  g=1    : mixed                               -> MIN 6
	//  g=2    : all NULL under a real key           -> MIN NULL
	w.Exec("INSERT INTO t (id, g, h, v) VALUES " +
		"(101, NULL, 1, NULL), (102, NULL, 1, 4), (103, NULL, 2, 9), " +
		"(201, 1, 1, 6), (202, 1, 1, NULL), " +
		"(301, 2, 1, NULL), (302, 2, 1, NULL)")

	w.Want("a NULL grouping key groups like any other",
		"SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g",
		[]string{"NULL|4", "1|6", "2|NULL"})

	// A composite key whose FIRST component is NULL: the seek prefix now has a
	// NULL followed by a real column, so the two NULL positions coexist.
	w.Want("composite key with a NULL leading component",
		"SELECT g, h, MIN(v) FROM t GROUP BY g, h ORDER BY g, h",
		[]string{"NULL|1|4", "NULL|2|9", "1|1|6", "2|1|NULL"})
}

// TestFDB_AggregateIndexMin_LongNullRuns puts many NULLs in front of the
// answer. The repair is written to seek past the whole run rather than walk it,
// so the value it returns must not depend on how long the run is — and a
// regression to a linear walk would still return the right value, which is why
// this asserts the ANSWER across run lengths rather than trying to assert the
// read count.
func TestFDB_AggregateIndexMin_LongNullRuns(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_min_longnull", "minln",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (id)) ",
		"CREATE INDEX t_min_v_g AS SELECT MIN(v) FROM t GROUP BY g "+
			"CREATE INDEX t_max_v_g AS SELECT MAX(v) FROM t GROUP BY g ")

	// g=1 gets 400 NULL rows and one real value at the very end of the run;
	// g=2 gets 400 NULL rows and nothing else.
	id := 1
	var rows []string
	for i := 0; i < 400; i++ {
		rows = append(rows, fmt.Sprintf("(%d, 1, NULL)", id))
		id++
	}
	for i := 0; i < 400; i++ {
		rows = append(rows, fmt.Sprintf("(%d, 2, NULL)", id))
		id++
	}
	rows = append(rows, fmt.Sprintf("(%d, 1, 7)", id))
	for start := 0; start < len(rows); start += 100 {
		end := start + 100
		if end > len(rows) {
			end = len(rows)
		}
		w.Exec("INSERT INTO t (id, g, v) VALUES " + strings.Join(rows[start:end], ", "))
	}

	w.Want("a long NULL run does not hide the value behind it",
		"SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|7", "2|NULL"})

	// Add a smaller value AFTER the long run and re-check: the repair reads the
	// current state rather than anything cached alongside the stored extremum.
	w.Exec("INSERT INTO t (id, g, v) VALUES (100001, 1, 3)")
	w.Want("a new minimum behind the same NULL run",
		"SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|3", "2|NULL"})

	// Deleting it falls back to the previous value, still behind the run.
	w.Exec("DELETE FROM t WHERE id = 100001")
	w.Want("removing it falls back through the run",
		"SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|7", "2|NULL"})
}

// TestFDB_AggregateIndexMin_ValueExtremes checks the values at the ends of the
// domain, where a comparison that fell back to something other than tuple order
// would show. The most negative int64 is the one a sign-confused comparison
// mistakes for the largest.
func TestFDB_AggregateIndexMin_ValueExtremes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_min_extremes", "minex",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, d DOUBLE, PRIMARY KEY (id)) ",
		"CREATE INDEX t_min_v AS SELECT MIN(v) FROM t GROUP BY g "+
			"CREATE INDEX t_max_v AS SELECT MAX(v) FROM t GROUP BY g "+
			"CREATE INDEX t_min_d AS SELECT MIN(d) FROM t GROUP BY g "+
			"CREATE INDEX t_max_d AS SELECT MAX(d) FROM t GROUP BY g ")

	//  g=1 : the extreme negative beside a NULL
	//  g=2 : zero beside a NULL — zero must not read as absent
	//  g=3 : the extreme positive beside a NULL
	w.Exec("INSERT INTO t (id, g, v, d) VALUES " +
		"(101, 1, -9223372036854775808, -1.7976931348623157E308), (102, 1, NULL, NULL), " +
		"(201, 2, 0, 0.0), (202, 2, NULL, NULL), " +
		"(301, 3, 9223372036854775807, 1.7976931348623157E308), (302, 3, NULL, NULL)")

	w.Want("MIN at the ends of the integer domain",
		"SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|-9223372036854775808", "2|0", "3|9223372036854775807"})
	w.Want("MAX at the ends of the integer domain",
		"SELECT g, MAX(v) FROM t GROUP BY g ORDER BY g",
		[]string{"1|-9223372036854775808", "2|0", "3|9223372036854775807"})

	// Zero is the value most likely to be confused with absence, so it gets its
	// own assertion rather than only appearing in the list above.
	w.Want("a zero MIN is a value, not an absence",
		"SELECT g FROM t GROUP BY g HAVING MIN(v) = 0", []string{"2"})
	w.Want("no group has a NULL MIN here",
		"SELECT g FROM t GROUP BY g HAVING MIN(v) IS NULL", []string{})
}
