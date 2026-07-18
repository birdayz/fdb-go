// Package rowdiff is the RFC-182 generative row-soundness differential
// harness: seeded random (schema, data, query) cases executed through the
// full production path (sqldriver → Cascades → executor → real FDB), rows
// diffed against Oracle M — a brute-force in-memory full-scan evaluation
// over the generator's own authoritative row set, reusing the engine's
// predicate evaluation (predicates.Comparison.Eval) so the planner is the
// only component removed. Any mismatch is a plan-soundness finding.
//
// The generator deliberately over-weights the matrix cell that hid the
// pk-intersection residual-drop bug (audit 2026-07-18): two or more indexed
// columns bound PLUS an unindexed residual, executed under every projection
// variant (`SELECT *` vs narrow — projection flips cost winners and masked
// that bug).
package rowdiff

import (
	"fmt"
	"math/rand/v2"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// ColType is the P1 column-type universe. DOUBLE is deliberately absent
// (RFC-182 OQ-3); LIKE/IN/BETWEEN arrive with P2.
type ColType int

const (
	ColBigint ColType = iota
	ColString
	ColBoolean
)

// ColumnDef describes one generated column.
type ColumnDef struct {
	Name    string
	Type    ColType
	NotNull bool
}

// IndexDef is a single- or multi-column value index.
type IndexDef struct {
	Name string
	Cols []string
}

// TableDef is the generated table: fixed pk column "ID" plus value columns.
type TableDef struct {
	Name    string
	Cols    []ColumnDef // excludes ID
	Indexes []IndexDef
}

// Row maps UPPER-CASE column name (incl. "ID") to a driver-typed value:
// int64, string, bool, or nil.
type Row map[string]any

// Pred is one comparison leaf: COL <op> literal, COL IS [NOT] NULL,
// COL IN (…), COL BETWEEN lo AND hi, or COL LIKE pattern.
type Pred struct {
	Col string
	Op  predicates.ComparisonType
	Lit any // nil for IS NULL / IS NOT NULL; the pattern for LIKE; lo for BETWEEN

	// InList holds the membership list for Op == ComparisonIn.
	InList []any
	// BetweenHi holds the inclusive upper bound when IsBetween (rendered as
	// BETWEEN; the oracle evaluates it as >=Lit AND <=BetweenHi with the
	// engine's own comparisons, which is the SQL desugaring).
	BetweenHi any
	IsBetween bool
}

// BoolNode is the AND/OR tree over leaves. Exactly one of Leaf / Kids is set.
type BoolNode struct {
	Leaf *Pred
	And  bool // valid when len(Kids) > 0
	Kids []*BoolNode
}

// NullsPlacement is an ORDER BY key's NULL position. Default follows the
// engine's Java/FDB-parity rule: NULLS FIRST ascending, NULLS LAST
// descending (tuple order). Explicit placements render as NULLS FIRST/LAST.
type NullsPlacement int

const (
	NullsDefault NullsPlacement = iota
	NullsFirst
	NullsLast
)

// OrderKey is one ORDER BY component. Nullable sort keys are allowed; keys
// are always suffixed with ID by the generator so the total order stays
// unique and the ordered comparator exact.
type OrderKey struct {
	Col   string
	Desc  bool
	Nulls NullsPlacement
}

// Query is one generated query body; the runner executes it under every
// projection variant and cross-checks row identity via ID.
type Query struct {
	Where    *BoolNode // nil = no WHERE
	OrderBy  []OrderKey
	Limit    int  // 0 = no LIMIT
	Distinct bool // SELECT DISTINCT (ORDER BY keys ⊆ projection enforced by generator)
}

// Case is everything one seed produces.
type Case struct {
	Seed    uint64
	Table   TableDef
	Rows    []Row
	Queries []Query
}

// Projections returns the projection variants every query runs under:
// star, pk-only, a narrow subset, and the narrow subset reversed.
func (c *Case) Projections() [][]string {
	narrow := []string{"ID"}
	for i, col := range c.Table.Cols {
		if i%2 == 0 {
			narrow = append(narrow, col.Name)
		}
	}
	reversed := make([]string, len(narrow))
	for i, n := range narrow {
		reversed[len(narrow)-1-i] = n
	}
	return [][]string{nil /* SELECT * */, {"ID"}, narrow, reversed}
}

// Generate builds the Case for a seed. Same seed → identical Case.
func Generate(seed uint64) *Case {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))

	table := genTable(rng)
	rows := genRows(rng, table)
	nQueries := 4 + rng.IntN(5)
	queries := make([]Query, 0, nQueries)
	for i := 0; i < nQueries; i++ {
		queries = append(queries, genQuery(rng, table))
	}
	return &Case{Seed: seed, Table: table, Rows: rows, Queries: queries}
}

func genTable(rng *rand.Rand) TableDef {
	// Fixed column pool: A,B,C BIGINT (B NOT NULL so it is sortable in P1),
	// S STRING, F BOOLEAN. Small and stable so index/predicate interplay —
	// not schema exotica — is what varies.
	cols := []ColumnDef{
		{Name: "A", Type: ColBigint},
		{Name: "B", Type: ColBigint, NotNull: true},
		{Name: "C", Type: ColBigint},
		{Name: "S", Type: ColString},
		{Name: "F", Type: ColBoolean},
	}

	pool := []IndexDef{
		{Name: "IDX_A", Cols: []string{"A"}},
		{Name: "IDX_B", Cols: []string{"B"}},
		{Name: "IDX_C", Cols: []string{"C"}},
		{Name: "IDX_S", Cols: []string{"S"}},
		{Name: "IDX_AB", Cols: []string{"A", "B"}},
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	// ≥2 indexes with high probability: that is where intersections and
	// cost ties live. 0-1 indexes still occur so full scans stay covered.
	nIdx := 2 + rng.IntN(3)
	if rng.IntN(10) == 0 {
		nIdx = rng.IntN(2)
	}
	idxs := make([]IndexDef, nIdx)
	copy(idxs, pool[:nIdx])

	return TableDef{Name: "T_RD", Cols: cols, Indexes: idxs}
}

func genRows(rng *rand.Rand, table TableDef) []Row {
	n := 20 + rng.IntN(101)
	rows := make([]Row, 0, n)
	stringDomain := []string{"", "alpha", "beta", "gamma", "delta", "epsilon"}
	for i := 0; i < n; i++ {
		r := Row{"ID": int64(i + 1)}
		for _, col := range table.Cols {
			if !col.NotNull && rng.IntN(6) == 0 {
				r[col.Name] = nil
				continue
			}
			switch col.Type {
			case ColBigint:
				// Domain 0..9 → heavy duplicates → intersections do real work,
				// plus boundary values at low probability.
				switch rng.IntN(20) {
				case 0:
					r[col.Name] = int64(-1)
				case 1:
					r[col.Name] = int64(1) << 62
				default:
					r[col.Name] = int64(rng.IntN(10))
				}
			case ColString:
				r[col.Name] = stringDomain[rng.IntN(len(stringDomain))]
			case ColBoolean:
				r[col.Name] = rng.IntN(2) == 0
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func genQuery(rng *rand.Rand, table TableDef) Query {
	indexed := map[string]bool{}
	for _, idx := range table.Indexes {
		for _, c := range idx.Cols {
			indexed[c] = true
		}
	}
	var indexedCols, unindexedCols []ColumnDef
	for _, c := range table.Cols {
		if indexed[c.Name] {
			indexedCols = append(indexedCols, c)
		} else {
			unindexedCols = append(unindexedCols, c)
		}
	}

	// The killer-cell bias: with ≥2 indexed columns available, half of all
	// queries take ≥2 indexed equality/range leaves PLUS one unindexed
	// residual leaf.
	var leaves []*Pred
	if len(indexedCols) >= 2 && rng.IntN(2) == 0 {
		perm := rng.Perm(len(indexedCols))
		nIdxLeaves := 2 + rng.IntN(len(indexedCols)-1)
		if nIdxLeaves > len(indexedCols) {
			nIdxLeaves = len(indexedCols)
		}
		for _, pi := range perm[:nIdxLeaves] {
			leaves = append(leaves, genPred(rng, indexedCols[pi], true))
		}
		if len(unindexedCols) > 0 && rng.IntN(4) != 0 {
			leaves = append(leaves, genPred(rng, unindexedCols[rng.IntN(len(unindexedCols))], false))
		}
	} else {
		nLeaves := 1 + rng.IntN(3)
		for i := 0; i < nLeaves; i++ {
			col := table.Cols[rng.IntN(len(table.Cols))]
			leaves = append(leaves, genPred(rng, col, indexed[col.Name]))
		}
	}

	var where *BoolNode
	if len(leaves) == 1 {
		where = &BoolNode{Leaf: leaves[0]}
	} else if rng.IntN(4) != 0 {
		// AND of all leaves (the majority — conjunctive index planning).
		kids := make([]*BoolNode, len(leaves))
		for i, l := range leaves {
			kids[i] = &BoolNode{Leaf: l}
		}
		where = &BoolNode{And: true, Kids: kids}
	} else {
		// One OR splitting the leaves into two conjuncts.
		split := 1 + rng.IntN(len(leaves)-1)
		left := make([]*BoolNode, 0, split)
		for _, l := range leaves[:split] {
			left = append(left, &BoolNode{Leaf: l})
		}
		right := make([]*BoolNode, 0, len(leaves)-split)
		for _, l := range leaves[split:] {
			right = append(right, &BoolNode{Leaf: l})
		}
		wrap := func(kids []*BoolNode) *BoolNode {
			if len(kids) == 1 {
				return kids[0]
			}
			return &BoolNode{And: true, Kids: kids}
		}
		where = &BoolNode{And: false, Kids: []*BoolNode{wrap(left), wrap(right)}}
	}

	var orderBy []OrderKey
	switch rng.IntN(4) {
	case 0:
		orderBy = []OrderKey{{Col: "ID", Desc: rng.IntN(2) == 0}}
	case 1:
		// B is the NOT NULL sortable value column; append ID for a total
		// order so the oracle's expected sequence is unique.
		orderBy = []OrderKey{{Col: "B", Desc: rng.IntN(2) == 0}, {Col: "ID"}}
	case 2:
		// NULLABLE sort key (A, C, or S) with a NULLS placement drawn from
		// {default, FIRST, LAST}; ID suffix keeps the total order unique.
		col := []string{"A", "C", "S"}[rng.IntN(3)]
		orderBy = []OrderKey{
			{Col: col, Desc: rng.IntN(2) == 0, Nulls: NullsPlacement(rng.IntN(3))},
			{Col: "ID"},
		}
	}

	q := Query{Where: where, OrderBy: orderBy}

	// DISTINCT on ~1/6 of queries. ORDER BY is dropped for DISTINCT — the
	// projection variants do not all contain every sort key, and SQL
	// requires DISTINCT's ORDER BY ⊆ projection; the unordered multiset
	// comparison stays exact.
	if rng.IntN(6) == 0 {
		q.Distinct = true
		q.OrderBy = nil
	}

	// LIMIT on ~1/4 of queries: small values exercise the boundary, large
	// values exercise the |M| < k clamp.
	if rng.IntN(4) == 0 {
		q.Limit = 1 + rng.IntN(30)
	}

	return q
}

func genPred(rng *rand.Rand, col ColumnDef, indexBiased bool) *Pred {
	// NULL-comparison leaves at low probability on nullable columns.
	if !col.NotNull && rng.IntN(10) == 0 {
		op := predicates.ComparisonIsNull
		if rng.IntN(2) == 0 {
			op = predicates.ComparisonIsNotNull
		}
		return &Pred{Col: col.Name, Op: op}
	}

	// P2 leaf kinds: IN (~1/8), BETWEEN (~1/10, BIGINT), LIKE (~1/6, STRING).
	switch {
	case col.Type != ColBoolean && rng.IntN(8) == 0:
		n := 2 + rng.IntN(3)
		list := make([]any, 0, n)
		for i := 0; i < n; i++ {
			if col.Type == ColBigint {
				list = append(list, int64(rng.IntN(12)))
			} else {
				list = append(list, []string{"", "alpha", "beta", "gamma", "delta", "zeta"}[rng.IntN(6)])
			}
		}
		return &Pred{Col: col.Name, Op: predicates.ComparisonIn, InList: list}
	case col.Type == ColBigint && rng.IntN(10) == 0:
		lo := int64(rng.IntN(10)) - 1
		hi := lo + int64(rng.IntN(6))
		return &Pred{Col: col.Name, Op: predicates.ComparisonGreaterThanEq, Lit: lo, BetweenHi: hi, IsBetween: true}
	case col.Type == ColString && rng.IntN(6) == 0:
		pat := []string{"%a%", "al%", "%ta", "_eta", "%e%a%", "alpha", "%"}[rng.IntN(7)]
		return &Pred{Col: col.Name, Op: predicates.ComparisonLike, Lit: pat}
	}

	var ops []predicates.ComparisonType
	if indexBiased && rng.IntN(3) != 0 {
		// Equality dominates on indexed columns — the sargable sweet spot.
		ops = []predicates.ComparisonType{predicates.ComparisonEquals}
	} else {
		ops = []predicates.ComparisonType{
			predicates.ComparisonEquals,
			predicates.ComparisonNotEquals,
			predicates.ComparisonLessThan,
			predicates.ComparisonLessThanOrEq,
			predicates.ComparisonGreaterThan,
			predicates.ComparisonGreaterThanEq,
		}
	}
	op := ops[rng.IntN(len(ops))]

	var lit any
	switch col.Type {
	case ColBigint:
		lit = int64(rng.IntN(10))
	case ColString:
		lit = []string{"", "alpha", "beta", "gamma", "delta"}[rng.IntN(5)]
	case ColBoolean:
		// Only =/<> make sense; force equality.
		op = predicates.ComparisonEquals
		lit = rng.IntN(2) == 0
	}
	return &Pred{Col: col.Name, Op: op, Lit: lit}
}

// --- SQL rendering ---------------------------------------------------------

// DDL renders the schema-template body (CREATE TABLE + CREATE INDEX lines).
func (c *Case) DDL() string {
	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	b.WriteString(c.Table.Name)
	b.WriteString(" (id BIGINT NOT NULL")
	for _, col := range c.Table.Cols {
		b.WriteString(", ")
		b.WriteString(strings.ToLower(col.Name))
		switch col.Type {
		case ColBigint:
			b.WriteString(" BIGINT")
		case ColString:
			b.WriteString(" STRING")
		case ColBoolean:
			b.WriteString(" BOOLEAN")
		}
	}
	b.WriteString(", PRIMARY KEY (id))")
	for _, idx := range c.Table.Indexes {
		fmt.Fprintf(&b, " CREATE INDEX %s ON %s (%s)",
			strings.ToLower(idx.Name), c.Table.Name, strings.ToLower(strings.Join(idx.Cols, ", ")))
	}
	return b.String()
}

// InsertSQL renders one multi-row INSERT for the dataset.
func (c *Case) InsertSQL() string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(c.Table.Name)
	b.WriteString(" VALUES ")
	for i, r := range c.Rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(")
		b.WriteString(renderLiteral(r["ID"]))
		for _, col := range c.Table.Cols {
			b.WriteString(", ")
			b.WriteString(renderLiteral(r[col.Name]))
		}
		b.WriteString(")")
	}
	return b.String()
}

// SQL renders one query under a projection (nil = SELECT *).
func (c *Case) SQL(q Query, projection []string) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	if q.Distinct {
		b.WriteString("DISTINCT ")
	}
	if projection == nil {
		b.WriteString("*")
	} else {
		lower := make([]string, len(projection))
		for i, p := range projection {
			lower[i] = strings.ToLower(p)
		}
		b.WriteString(strings.Join(lower, ", "))
	}
	b.WriteString(" FROM ")
	b.WriteString(strings.ToLower(c.Table.Name))
	if q.Where != nil {
		b.WriteString(" WHERE ")
		renderBool(&b, q.Where)
	}
	if len(q.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, k := range q.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strings.ToLower(k.Col))
			if k.Desc {
				b.WriteString(" DESC")
			}
			switch k.Nulls {
			case NullsFirst:
				b.WriteString(" NULLS FIRST")
			case NullsLast:
				b.WriteString(" NULLS LAST")
			}
		}
	}
	if q.Limit > 0 {
		fmt.Fprintf(&b, " LIMIT %d", q.Limit)
	}
	return b.String()
}

func renderBool(b *strings.Builder, n *BoolNode) {
	if n.Leaf != nil {
		p := n.Leaf
		col := strings.ToLower(p.Col)
		switch {
		case p.IsBetween:
			fmt.Fprintf(b, "%s BETWEEN %s AND %s", col, renderLiteral(p.Lit), renderLiteral(p.BetweenHi))
		case p.Op == predicates.ComparisonIsNull:
			fmt.Fprintf(b, "%s IS NULL", col)
		case p.Op == predicates.ComparisonIsNotNull:
			fmt.Fprintf(b, "%s IS NOT NULL", col)
		case p.Op == predicates.ComparisonIn:
			lits := make([]string, len(p.InList))
			for i, v := range p.InList {
				lits[i] = renderLiteral(v)
			}
			fmt.Fprintf(b, "%s IN (%s)", col, strings.Join(lits, ", "))
		case p.Op == predicates.ComparisonLike:
			fmt.Fprintf(b, "%s LIKE %s", col, renderLiteral(p.Lit))
		default:
			fmt.Fprintf(b, "%s %s %s", col, opSQL(p.Op), renderLiteral(p.Lit))
		}
		return
	}
	sep := " AND "
	if !n.And {
		sep = " OR "
	}
	for i, kid := range n.Kids {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString("(")
		renderBool(b, kid)
		b.WriteString(")")
	}
}

func opSQL(op predicates.ComparisonType) string {
	switch op {
	case predicates.ComparisonEquals:
		return "="
	case predicates.ComparisonNotEquals:
		return "<>"
	case predicates.ComparisonLessThan:
		return "<"
	case predicates.ComparisonLessThanOrEq:
		return "<="
	case predicates.ComparisonGreaterThan:
		return ">"
	case predicates.ComparisonGreaterThanEq:
		return ">="
	}
	panic(fmt.Sprintf("rowdiff: unrenderable comparison op %d", op))
}

func renderLiteral(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return fmt.Sprintf("%d", t)
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'"
	case bool:
		if t {
			return "TRUE"
		}
		return "FALSE"
	}
	panic(fmt.Sprintf("rowdiff: unrenderable literal %T", v))
}
