package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"testing"
)

// RFC-048 W2: metamorphic / TLP property testing.
//
// For the query surface there is no cheap reference engine, so instead of an
// oracle we assert relations that must hold between the results of *different*
// queries over the same data. A violation is a loud diff — exactly the signal
// a silent-wrong result otherwise hides. Each relation carries its domain
// restriction (see the comments); we only assert relations that are
// unconditionally true on the generated data.
//
// A small seeded generator emits random single-column predicates over a table
// that deliberately contains NULLs (TLP needs the UNKNOWN partition to be
// non-trivial). The seed is printed on failure so any violation reproduces.
//
// This file also runs under the W1 invariant (armed binary-wide in TestMain),
// so every generated aggregate/HAVING query is additionally checked for
// unresolved references.

// w2Row is one generated row; a/b are nullable bigints, c a nullable string.
type w2Row struct {
	id int64
	a  *int64
	b  *int64
	c  *string
}

// w2DB creates a single-table schema seeded deterministically from `seed`,
// with ~25% NULLs per nullable column.
func w2DB(t *testing.T, seed int64) (*sql.DB, []w2Row) {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := fmt.Sprintf("/w2_%d_%s", seed, t.Name())
	db := openTestDB(t, dbPath)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("W2_TMPL_%d_%s", seed, t.Name())
	ddl := fmt.Sprintf(`CREATE SCHEMA TEMPLATE %s
		CREATE TABLE w2t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c STRING, PRIMARY KEY (id))`, tmpl)
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	sdb, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { sdb.Close() })

	rng := rand.New(rand.NewSource(seed))
	strs := []string{"x", "y", "z"}
	rows := make([]w2Row, 0, 50)
	for i := int64(1); i <= 50; i++ {
		r := w2Row{id: i}
		if rng.Float64() > 0.25 {
			v := int64(rng.Intn(21)) // 0..20
			r.a = &v
		}
		if rng.Float64() > 0.25 {
			v := int64(rng.Intn(21) - 10) // -10..10
			r.b = &v
		}
		if rng.Float64() > 0.25 {
			s := strs[rng.Intn(len(strs))]
			r.c = &s
		}
		rows = append(rows, r)
		ins := fmt.Sprintf("INSERT INTO w2t VALUES (%d, %s, %s, %s)",
			r.id, nullableInt(r.a), nullableInt(r.b), nullableStr(r.c))
		if _, err := sdb.ExecContext(ctx, ins); err != nil {
			t.Fatalf("INSERT: %v (%s)", err, ins)
		}
	}
	return sdb, rows
}

func nullableInt(p *int64) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *p)
}

func nullableStr(p *string) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprintf("'%s'", *p)
}

// scalarInt runs a single-column single-row integer query.
func scalarInt(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	rows := collectRows(t, db, query)
	if len(rows) != 1 {
		t.Fatalf("scalarInt %q: want 1 row, got %d (%v)", query, len(rows), rows)
	}
	switch v := rows[0][0].(type) {
	case int64:
		return v
	case nil:
		return 0
	default:
		t.Fatalf("scalarInt %q: non-int %T (%v)", query, v, v)
		return 0
	}
}

// randPredicate emits a single-column comparison `col OP val`. Single-column is
// the TLP domain restriction: such a predicate is UNKNOWN exactly when col is
// NULL, so `col IS NULL` is precisely its UNKNOWN partition.
func randPredicate(rng *rand.Rand) (pred, col string) {
	intOps := []string{"=", "<>", "<", ">", "<=", ">="}
	switch rng.Intn(3) {
	case 0:
		col = "a"
		op := intOps[rng.Intn(len(intOps))]
		return fmt.Sprintf("a %s %d", op, rng.Intn(21)), col
	case 1:
		col = "b"
		op := intOps[rng.Intn(len(intOps))]
		return fmt.Sprintf("b %s %d", op, rng.Intn(21)-10), col
	default:
		col = "c"
		op := []string{"=", "<>"}[rng.Intn(2)]
		v := []string{"x", "y", "z"}[rng.Intn(3)]
		return fmt.Sprintf("c %s '%s'", op, v), col
	}
}

func TestFDB_W2_Metamorphic_CountEquivalence(t *testing.T) {
	t.Parallel()
	const seed = 0x5eed01
	db, _ := w2DB(t, seed)
	rng := rand.New(rand.NewSource(seed))

	// COUNT(*) ≡ COUNT(1) is *unconditionally* true and is exactly the relation
	// that catches Exhibit-A (the COUNT(*)/COUNT(1) producer/consumer name
	// split). COUNT(*) ≡ COALESCE(SUM(1),0) adds the empty-group domain fix
	// (SUM over zero rows is NULL, COUNT is 0).
	for i := 0; i < 64; i++ {
		p, _ := randPredicate(rng)
		cStar := scalarInt(t, db, fmt.Sprintf("SELECT COUNT(*) FROM w2t WHERE %s", p))
		cOne := scalarInt(t, db, fmt.Sprintf("SELECT COUNT(1) FROM w2t WHERE %s", p))
		if cStar != cOne {
			t.Fatalf("seed=%#x pred=%q: COUNT(*)=%d != COUNT(1)=%d", seed, p, cStar, cOne)
		}
		sOne := scalarInt(t, db, fmt.Sprintf("SELECT COALESCE(SUM(1), 0) FROM w2t WHERE %s", p))
		if cStar != sOne {
			t.Fatalf("seed=%#x pred=%q: COUNT(*)=%d != COALESCE(SUM(1),0)=%d", seed, p, cStar, sOne)
		}
	}
}

func TestFDB_W2_TLP_CountPartition(t *testing.T) {
	t.Parallel()
	const seed = 0x5eed02
	db, _ := w2DB(t, seed)
	rng := rand.New(rand.NewSource(seed))

	total := scalarInt(t, db, "SELECT COUNT(*) FROM w2t")
	// Ternary Logic Partitioning: WHERE p, WHERE NOT p, WHERE p-is-UNKNOWN must
	// partition every row exactly once. For a single-column predicate the
	// UNKNOWN partition is `col IS NULL`. The three counts must sum to the
	// total — if the engine mishandles 3-valued logic (e.g. NOT p keeping NULL
	// rows, or a predicate dropping rows it should keep), the sum diverges.
	for i := 0; i < 64; i++ {
		p, col := randPredicate(rng)
		tt := scalarInt(t, db, fmt.Sprintf("SELECT COUNT(*) FROM w2t WHERE %s", p))
		ff := scalarInt(t, db, fmt.Sprintf("SELECT COUNT(*) FROM w2t WHERE NOT (%s)", p))
		nn := scalarInt(t, db, fmt.Sprintf("SELECT COUNT(*) FROM w2t WHERE %s IS NULL", col))
		if tt+ff+nn != total {
			t.Fatalf("seed=%#x pred=%q col=%s: TLP partition %d+%d+%d=%d != total %d",
				seed, p, col, tt, ff, nn, tt+ff+nn, total)
		}
	}
}

func TestFDB_W2_TLP_RowSetReconstruction(t *testing.T) {
	t.Parallel()
	const seed = 0x5eed03
	db, _ := w2DB(t, seed)
	rng := rand.New(rand.NewSource(seed))

	allPKs := pkSet(t, db, "SELECT id FROM w2t")
	for i := 0; i < 24; i++ {
		p, col := randPredicate(rng)
		part := map[int64]bool{}
		for _, q := range []string{
			fmt.Sprintf("SELECT id FROM w2t WHERE %s", p),
			fmt.Sprintf("SELECT id FROM w2t WHERE NOT (%s)", p),
			fmt.Sprintf("SELECT id FROM w2t WHERE %s IS NULL", col),
		} {
			for pk := range pkSet(t, db, q) {
				if part[pk] {
					t.Fatalf("seed=%#x pred=%q: pk %d in more than one TLP partition", seed, p, pk)
				}
				part[pk] = true
			}
		}
		if !sameSet(allPKs, part) {
			t.Fatalf("seed=%#x pred=%q col=%s: TLP row set reconstruction != all rows (got %d, want %d)",
				seed, p, col, len(part), len(allPKs))
		}
	}
}

func TestFDB_W2_Metamorphic_PredicateRedundancy(t *testing.T) {
	t.Parallel()
	const seed = 0x5eed04
	db, _ := w2DB(t, seed)
	rng := rand.New(rand.NewSource(seed))

	// `p` ≡ `p AND TRUE` ≡ `p AND 1=1`: redundant always-true conjuncts must not
	// change the result. A normaliser that mis-folds them would diverge.
	for i := 0; i < 48; i++ {
		p, _ := randPredicate(rng)
		base := scalarInt(t, db, fmt.Sprintf("SELECT COUNT(*) FROM w2t WHERE %s", p))
		andTrue := scalarInt(t, db, fmt.Sprintf("SELECT COUNT(*) FROM w2t WHERE (%s) AND 1 = 1", p))
		if base != andTrue {
			t.Fatalf("seed=%#x pred=%q: COUNT %d != with AND 1=1 %d", seed, p, base, andTrue)
		}
	}
}

func TestFDB_W2_Metamorphic_GroupByCountEqualsSum(t *testing.T) {
	t.Parallel()
	const seed = 0x5eed05
	db, _ := w2DB(t, seed)

	// Per group COUNT(*) ≡ SUM(1): every group is non-empty by construction, so
	// SUM(1) cannot be NULL and equals the row count. This exercises the
	// aggregate-naming path (Exhibit-A's home) through a different aggregate
	// pair than COUNT(1).
	byKeyCount := groupCounts(t, db, "SELECT c, COUNT(*) FROM w2t GROUP BY c")
	byKeySum := groupCounts(t, db, "SELECT c, SUM(1) FROM w2t GROUP BY c")
	if len(byKeyCount) != len(byKeySum) {
		t.Fatalf("seed=%#x: group count %d != group count via SUM(1) %d", seed, len(byKeyCount), len(byKeySum))
	}
	for k, v := range byKeyCount {
		if byKeySum[k] != v {
			t.Fatalf("seed=%#x group %q: COUNT(*)=%d != SUM(1)=%d", seed, k, v, byKeySum[k])
		}
	}
}

func TestFDB_W2_Metamorphic_HavingCountEquivalence(t *testing.T) {
	t.Parallel()
	const seed = 0x5eed07
	db, _ := w2DB(t, seed)
	rng := rand.New(rand.NewSource(seed))

	// The exact Exhibit-A shape, as a generative relation: an aggregate
	// referenced in HAVING under the name COUNT(1) must resolve to the same
	// materialised slot as COUNT(*). If the producer/consumer naming splits,
	// `HAVING COUNT(1) > k` and `HAVING COUNT(*) > k` select different groups.
	// Both must be identical for every threshold and grouping key.
	keys := []string{"c", "a", "b"}
	for i := 0; i < 36; i++ {
		key := keys[rng.Intn(len(keys))]
		k := rng.Intn(6)
		one := groupCounts(t, db, fmt.Sprintf("SELECT %s, COUNT(*) FROM w2t GROUP BY %s HAVING COUNT(1) > %d", key, key, k))
		star := groupCounts(t, db, fmt.Sprintf("SELECT %s, COUNT(*) FROM w2t GROUP BY %s HAVING COUNT(*) > %d", key, key, k))
		if len(one) != len(star) {
			t.Fatalf("seed=%#x key=%s k=%d: HAVING COUNT(1) kept %d groups, HAVING COUNT(*) kept %d",
				seed, key, k, len(one), len(star))
		}
		for g, v := range star {
			if one[g] != v {
				t.Fatalf("seed=%#x key=%s k=%d group %q: HAVING COUNT(*) count=%d, HAVING COUNT(1) count=%d",
					seed, key, k, g, v, one[g])
			}
		}
	}
}

func TestFDB_W2_Metamorphic_ArithmeticIdentity(t *testing.T) {
	t.Parallel()
	const seed = 0x5eed06
	db, _ := w2DB(t, seed)

	// x ≡ x+0 over a bounded integer column (domain: no overflow). Compared as a
	// SUM so we get one scalar per side; over the same non-null rows the totals
	// must be identical.
	lhs := scalarInt(t, db, "SELECT SUM(a) FROM w2t WHERE a IS NOT NULL")
	rhs := scalarInt(t, db, "SELECT SUM(a + 0) FROM w2t WHERE a IS NOT NULL")
	if lhs != rhs {
		t.Fatalf("seed=%#x: SUM(a)=%d != SUM(a+0)=%d", seed, lhs, rhs)
	}
}

// --- small helpers ---

func pkSet(t *testing.T, db *sql.DB, query string) map[int64]bool {
	t.Helper()
	out := map[int64]bool{}
	for _, r := range collectRows(t, db, query) {
		if v, ok := r[0].(int64); ok {
			out[v] = true
		}
	}
	return out
}

func sameSet(a, b map[int64]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// groupCounts reads a (key, count) result into a map keyed by the string form
// of the group key (NULL key folded to the literal "<null>").
func groupCounts(t *testing.T, db *sql.DB, query string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, r := range collectRows(t, db, query) {
		key := "<null>"
		if r[0] != nil {
			key = fmt.Sprintf("%v", r[0])
		}
		switch v := r[1].(type) {
		case int64:
			out[key] = v
		case nil:
			out[key] = 0
		default:
			t.Fatalf("groupCounts %q: non-int count %T (%v)", query, v, v)
		}
	}
	return out
}
