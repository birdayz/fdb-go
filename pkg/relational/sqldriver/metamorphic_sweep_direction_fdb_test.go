package sqldriver_test

// Twelfth axis: ORDER DIRECTION.
//
// Reverse the sort direction of every key and the result sequence must reverse
// exactly — provided the ordering is TOTAL, which the trailing `id` guarantees
// here. That makes the property an equality between sequences rather than a
// statement about multisets, and equality is what catches an off-by-one.
//
// The axis targets machinery no row-set oracle reaches. A DESC order over an
// indexed column is served by a REVERSE index scan, which is a different code
// path from the forward one at every level — range endpoints, continuation
// encoding, the limit applied to a reversed stream. The twin oracle cannot see
// a defect there: it compares indexed against unindexed, and both sides sort
// DESC, so a reverse-scan bug that produces the same wrong order on both is
// agreement. Comparing ASC against reversed-DESC is what separates them.
//
// NULL ordering rides along for free and is worth naming, because it is the
// part most easily got wrong by hand: NULL sorts FIRST ascending, so it must
// sort LAST descending, and the reversal check asserts that without anyone
// having to write down which end it belongs at.
//
// THE LIMIT ARM IS THE POINT OF THE FILE. `ORDER BY k DESC LIMIT n` must equal
// the LAST n rows of the ascending order, reversed — the shape where a reverse
// scan, a limit and an endpoint meet, and the one most likely to be off by one
// row at either end.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
)

// mmdReverse returns a reversed copy, so a comparison against it reads as an
// assertion about the ORIGINAL rather than about a mutated slice.
func mmdReverse(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

func TestFDB_MetamorphicOrderDirection(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_mhdir", "mhdir",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c DOUBLE, s STRING, f BOOLEAN, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_ab ON t (a, b) "+
			"CREATE INDEX t_c ON t (c) CREATE INDEX t_s ON t (s) ")

	dataRand := rand.New(rand.NewSource(20260820))
	const nRows = 120
	var vals []string
	for i := 1; i <= nRows; i++ {
		vals = append(vals, mhRowLiteral(dataRand, i, false))
	}
	for start := 0; start < len(vals); start += 20 {
		end := start + 20
		if end > len(vals) {
			end = len(vals)
		}
		w.Exec("INSERT INTO t " + mhCols + " VALUES " + strings.Join(vals[start:end], ", "))
	}

	seed := int64(13)
	if s := os.Getenv("MHD_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	iters := 60
	if s := os.Getenv("MHD_ITERS"); s != "" {
		fmt.Sscan(s, &iters)
	}
	g := &mhGen{r: rand.New(rand.NewSource(seed))}

	// Ordering keys, each paired with the index that can serve it. `id` is
	// appended to every one so the order is TOTAL and the reversal is exact.
	keys := []string{"a", "a, b", "c", "s", "b"}

	var compared, nonEmpty, limited, skipped int

	t.Run("full reversal", func(t *testing.T) {
		w := w.Sub(t)
		for i := 0; i < iters; i++ {
			key := g.pick(keys)
			pred := g.pred(1)
			ascKey := strings.ReplaceAll(key, ",", " ASC,") + " ASC, id ASC"
			descKey := strings.ReplaceAll(key, ",", " DESC,") + " DESC, id DESC"

			ascQ := fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY %s", pred, ascKey)
			descQ := fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY %s", pred, descKey)

			asc, err := mmRows(t, ctx, w.idx, ascQ)
			if err != nil {
				// Unsupported together or not at all — a direction that changes
				// whether a query is ACCEPTED is a defect with no rows involved.
				if _, derr := mmRows(t, ctx, w.idx, descQ); (derr == nil) != (err == nil) {
					t.Errorf("reversing the sort direction changed whether the query is "+
						"ACCEPTED\n  asc : %s\n  err: %v\n  desc: %s\n  err: %v",
						ascQ, err, descQ, derr)
				}
				skipped++
				continue
			}
			desc, err := mmRows(t, ctx, w.idx, descQ)
			if err != nil {
				t.Errorf("the ascending form ran and the descending one failed\n"+
					"  asc : %s\n  desc: %s\n  err : %v", ascQ, descQ, err)
				continue
			}
			compared++
			if len(asc) > 0 {
				nonEmpty++
			}
			if !mmEqRows(desc, mmdReverse(asc)) {
				t.Errorf("DESC is not the reverse of ASC\n  asc : %s\n  desc: %s\n"+
					"  asc rows      %v\n  desc rows     %v\n  asc REVERSED  %v\n  %s\n"+
					"  (the trailing id makes the order total, so these must be equal as "+
					"SEQUENCES — a difference is a reverse-scan defect, not a tie)",
					ascQ, descQ, asc, desc, mmdReverse(asc), mmFirstDiff(desc, mmdReverse(asc)))
			}

			// The unindexed side must agree with the indexed one on the
			// descending form too. A reverse INDEX scan is a distinct code path
			// from a reverse in-memory sort, and this is what separates them.
			w.Want("descending answer agrees on both schemas", descQ, desc)
		}
	})

	// `ORDER BY k DESC LIMIT n` must be the LAST n of the ascending order,
	// reversed. Reverse scan + limit + endpoint, which is where an off-by-one
	// row at either end lives.
	t.Run("descending with a limit is the ascending tail", func(t *testing.T) {
		w := w.Sub(t)
		for i := 0; i < iters; i++ {
			key := g.pick(keys)
			pred := g.pred(1)
			ascKey := strings.ReplaceAll(key, ",", " ASC,") + " ASC, id ASC"
			descKey := strings.ReplaceAll(key, ",", " DESC,") + " DESC, id DESC"

			asc, err := mmRows(t, ctx, w.idx,
				fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY %s", pred, ascKey))
			if err != nil {
				skipped++
				continue
			}
			for _, n := range []int{1, 3, 10} {
				descQ := fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY %s LIMIT %d",
					pred, descKey, n)
				got, derr := mmRows(t, ctx, w.idx, descQ)
				if derr != nil {
					t.Errorf("descending with LIMIT failed where the ascending form ran\n"+
						"  q: %s\n  err: %v", descQ, derr)
					continue
				}
				// The last n of the ascending order, reversed.
				tail := asc
				if len(tail) > n {
					tail = tail[len(tail)-n:]
				}
				want := mmdReverse(tail)
				limited++
				if !mmEqRows(got, want) {
					t.Errorf("DESC LIMIT %d is not the ascending tail reversed\n  q: %s\n"+
						"  got  %v\n  want %v\n  full asc %v\n  %s", n, descQ, got, want, asc,
						mmFirstDiff(got, want))
				}
			}
		}
	})

	// Floors. `limited` is counted separately from `compared` because the two
	// arms can fail to exercise independently — a corpus whose predicates all
	// select fewer than one row would still produce limit comparisons, and one
	// whose queries all errored would produce neither.
	if compared < 30 {
		t.Fatalf("only %d direction pairs compared (%d skipped) — below this floor the axis "+
			"is not exercising the engine", compared, skipped)
	}
	if nonEmpty < 20 {
		t.Fatalf("only %d of %d compared pairs had non-empty rows. An empty sequence is its "+
			"own reverse, so below this floor the sweep is comparing absences", nonEmpty, compared)
	}
	if limited < 60 {
		t.Fatalf("only %d limit comparisons ran — the LIMIT arm is the one that exercises "+
			"reverse scan, limit and endpoint together, so its collapse is the one that "+
			"matters most", limited)
	}
	t.Logf("direction sweep: %d reversal pairs (%d non-empty), %d limit comparisons, %d skipped",
		compared, nonEmpty, limited, skipped)
}
