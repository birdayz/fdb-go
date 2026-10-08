package testkit

// Ordering / LIMIT / IN-list oracle battery over real FDB. Every query is
// answered by a Go oracle over the same generated rows (NULLs included) and
// checked for the row SET, the ORDER where the query asks for one (a partial
// order is checked key by key, a LIMIT as a valid top-k prefix, an OFFSET as
// the window after it), and duplicates. The shapes are the ones the
// generative rowdiff corpus does not reach: ORDER BY over a composite index
// with an equality prefix, DESC and NULLS placement, IN-union and IN-join
// under ORDER BY and LIMIT, OR-unions, two-sided ranges, contradictions.
// Written as a Cascades bug hunt's proof that these return correct rows; it
// stays so a regression on any of those axes is caught by the row check, not
// only by the plan pins.

import (
	"fmt"
	"math/rand/v2"
)

type OracleRow struct {
	ID      int64
	A, B, C *int64
	S       *string
	V       *int64
}

func OracleInt(v int64) *int64 { return &v }

func OracleGenRows(n int) []OracleRow {
	rng := rand.New(rand.NewPCG(7, 11))
	out := make([]OracleRow, 0, n)
	pick := func(max int64, nullEvery int) *int64 {
		if rng.IntN(nullEvery) == 0 {
			return nil
		}
		return OracleInt(int64(rng.IntN(int(max))) + 1)
	}
	for i := 1; i <= n; i++ {
		r := OracleRow{ID: int64(i)}
		r.A = pick(5, 9)
		r.B = pick(6, 9)
		r.C = pick(4, 9)
		if rng.IntN(9) != 0 {
			s := []string{"x", "y", "z"}[rng.IntN(3)]
			r.S = &s
		}
		if rng.IntN(9) != 0 {
			r.V = OracleInt(int64(rng.IntN(51)))
		}
		out = append(out, r)
	}
	return out
}

func (r OracleRow) InsertSQL() string {
	lit := func(p *int64) string {
		if p == nil {
			return "NULL"
		}
		return fmt.Sprint(*p)
	}
	s := "NULL"
	if r.S != nil {
		s = "'" + *r.S + "'"
	}
	return fmt.Sprintf("(%d, %s, %s, %s, %s, %s)", r.ID, lit(r.A), lit(r.B), lit(r.C), s, lit(r.V))
}

func OracleVal(p *int64) (int64, bool) {
	if p == nil {
		return 0, false
	}
	return *p, true
}

func OracleEq(p *int64, v int64) bool { x, ok := OracleVal(p); return ok && x == v }
func OracleGt(p *int64, v int64) bool { x, ok := OracleVal(p); return ok && x > v }
func OracleGe(p *int64, v int64) bool { x, ok := OracleVal(p); return ok && x >= v }
func OracleLt(p *int64, v int64) bool { x, ok := OracleVal(p); return ok && x < v }
func OracleLe(p *int64, v int64) bool { x, ok := OracleVal(p); return ok && x <= v }
func OracleIn(p *int64, vs ...int64) bool {
	x, ok := OracleVal(p)
	if !ok {
		return false
	}
	for _, v := range vs {
		if v == x {
			return true
		}
	}
	return false
}
