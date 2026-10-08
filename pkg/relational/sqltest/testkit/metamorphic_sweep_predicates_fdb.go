package testkit

// Metamorphic bug hunter: runs the SAME query text against two schemas that
// hold IDENTICAL data and differ ONLY in which secondary indexes exist. Any
// row-set difference is a planner/index-matching defect, because the answer to
// a query may not depend on the presence of an index.
//
// Second oracle (TLP): for any predicate P, the id sets of `WHERE P`,
// `WHERE NOT P` and `WHERE P IS NULL` must partition the table exactly.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
)

func MhScanStrings(ctx context.Context, db *sql.DB, q string) ([]string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			return nil, err
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			v := c.(*sql.NullString)
			if v.Valid {
				parts[i] = v.String
			} else {
				parts[i] = "NULL"
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- random predicate generation -------------------------------------------

// mhGen generates random predicates. The column names are parameters so the
// same generator can drive fixtures with different schemas; a generator that
// emits a column the table does not have produces a 42703 on BOTH sides, which
// is an EMPTY comparison, not a passing one.
type MhGen struct {
	R *rand.Rand
	// intCols/dblCols/strCols/boolCols default (when nil) to the hunt-1 fixture.
	IntCols, DblCols, StrCols, BoolCols []string
}

func (g *MhGen) Ints() []string {
	if g.IntCols == nil {
		return []string{"a", "b", "id"}
	}
	return g.IntCols
}

func (g *MhGen) Dbls() []string {
	if g.DblCols == nil {
		return []string{"c"}
	}
	return g.DblCols
}

func (g *MhGen) Strs() []string {
	if g.StrCols == nil {
		return []string{"s"}
	}
	return g.StrCols
}

func (g *MhGen) bools() []string {
	if g.BoolCols == nil {
		return []string{"f"}
	}
	return g.BoolCols
}

var (
	MhIntLits = []string{"-2", "-1", "0", "1", "2", "3", "10"}
	MhDblLits = []string{"-1.5", "-0.0", "0.0", "1.5", "2.25", "3.0"}
	MhStrLits = []string{"''", "'a'", "'ab'", "'b'", "'B'", "'abc'", "'z'"}
)

func (g *MhGen) Pick(ss []string) string { return ss[g.R.Intn(len(ss))] }

func (g *MhGen) intExpr(depth int) string {
	if depth <= 0 || g.R.Intn(3) == 0 {
		if g.R.Intn(2) == 0 {
			return g.Pick(g.Ints())
		}
		return g.Pick(MhIntLits)
	}
	op := g.Pick([]string{"+", "-", "*"})
	return "(" + g.intExpr(depth-1) + " " + op + " " + g.intExpr(depth-1) + ")"
}

func (g *MhGen) CmpOp() string {
	return g.Pick([]string{"=", "<>", "<", "<=", ">", ">="})
}

func (g *MhGen) Atom(depth int) string {
	all := append(append(append(append([]string{}, g.Ints()...), g.Dbls()...), g.Strs()...), g.bools()...)
	switch g.R.Intn(10) {
	case 0, 1:
		return g.Pick(g.Ints()) + " " + g.CmpOp() + " " + g.Pick(MhIntLits)
	case 2:
		if len(g.Dbls()) == 0 {
			return g.Pick(g.Ints()) + " " + g.CmpOp() + " " + g.Pick(MhIntLits)
		}
		return g.Pick(g.Dbls()) + " " + g.CmpOp() + " " + g.Pick(MhDblLits)
	case 3:
		return g.Pick(g.Strs()) + " " + g.CmpOp() + " " + g.Pick(MhStrLits)
	case 4:
		return g.Pick(all) + " IS " + g.Pick([]string{"", "NOT "}) + "NULL"
	case 5:
		return g.Pick(g.Ints()) + " IN (" + g.Pick(MhIntLits) + ", " + g.Pick(MhIntLits) + ", " + g.Pick(MhIntLits) + ")"
	case 6:
		lo, hi := g.Pick(MhIntLits), g.Pick(MhIntLits)
		return g.Pick(g.Ints()) + " BETWEEN " + lo + " AND " + hi
	case 7:
		return g.Pick(g.Strs()) + " LIKE " + g.Pick([]string{"'a%'", "'%b'", "'%a%'", "'_b'", "'a_'", "''"})
	case 8:
		return g.intExpr(2) + " " + g.CmpOp() + " " + g.intExpr(2)
	default:
		if len(g.bools()) == 0 {
			return g.Pick(g.Ints()) + " " + g.CmpOp() + " " + g.Pick(MhIntLits)
		}
		if g.R.Intn(2) == 0 {
			return g.Pick(g.bools())
		}
		return "NOT " + g.Pick(g.bools())
	}
}

func (g *MhGen) Pred(depth int) string {
	if depth <= 0 {
		return g.Atom(2)
	}
	switch g.R.Intn(5) {
	case 0:
		return "(NOT " + g.Pred(depth-1) + ")"
	case 1, 2:
		return "(" + g.Pred(depth-1) + " AND " + g.Pred(depth-1) + ")"
	case 3:
		return "(" + g.Pred(depth-1) + " OR " + g.Pred(depth-1) + ")"
	default:
		return g.Atom(2)
	}
}

// ---- fixture ----------------------------------------------------------------

const MhCols = "(id, a, b, c, s, f)"

// mhRowLiteral builds one fixture row.
func MhRowLiteral(r *rand.Rand, id int) string {
	nul := func(p int, gen func() string) string {
		if r.Intn(100) < p {
			return "NULL"
		}
		return gen()
	}
	a := nul(18, func() string { return fmt.Sprintf("%d", r.Intn(7)-2) })
	b := nul(18, func() string { return fmt.Sprintf("%d", r.Intn(4)) })
	c := nul(18, func() string {
		return []string{"-1.5", "-0.0", "0.0", "1.5", "2.25", "3.0"}[r.Intn(6)]
	})
	s := nul(18, func() string {
		return []string{"''", "'a'", "'ab'", "'b'", "'B'", "'abc'", "'z'", "'aa'"}[r.Intn(8)]
	})
	f := nul(18, func() string {
		if r.Intn(2) == 0 {
			return "true"
		}
		return "false"
	})
	return fmt.Sprintf("(%d, %s, %s, %s, %s, %s)", id, a, b, c, s, f)
}
