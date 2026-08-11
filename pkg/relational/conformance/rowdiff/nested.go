package rowdiff

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// StructCol is the struct-typed column a nested case's dotted columns live in.
//
// The generator carries the struct as a property of the TABLE rather than as a
// property of each column because that is the direction the information flows:
// a dotted column name (`N.A`) is enough for every consumer downstream of
// generation — the predicate renderer, the ORDER BY renderer, the oracle's row
// map — and only the two places that speak physical schema (DDL and INSERT)
// need to know that `N.A` and `N.B` are two leaves of ONE column. Putting the
// nesting in the column names is what makes this extension cheap: nothing that
// composes a query had to learn about structs, so no query construct is
// silently excluded from the nested axis by having been forgotten in a walker.
type StructCol struct {
	// Name is the physical column, e.g. "N"; empty for an INNER type, which is
	// reached through a member rather than being a column of its own.
	Name string
	// TypeName is the struct type declared by CREATE TYPE AS STRUCT.
	TypeName string
	// Fields are the members in DECLARATION order, which is also the order a
	// positional struct literal in an INSERT must supply them.
	Fields []StructField
}

// StructField is one member of a struct type: either a scalar leaf or, when
// Nested is non-nil, another struct.
//
// The recursion is what makes the DEPTH axis reachable. `walkColumnRef` used to
// refuse a 3-segment reference outright, so a struct inside a struct could not
// be named and there was no point generating one; RFC-204 4.4 landed the
// Identifier model and the arity cap is gone, so `n.dp.a` and the four-segment
// `l.n.dp.a` both resolve. A generator capped at one level would now be capped
// below the engine — testing the shapes that already worked and none of the
// ones the depth fix opened.
type StructField struct {
	Name   string
	Type   ColType
	Nested *StructCol
}

// leafOf returns the path BELOW the struct root of a dotted column, and
// whether the column is nested at all. For "N.DP.A" it returns "DP.A".
func leafOf(col string) (string, bool) {
	_, rest, found := strings.Cut(col, ".")
	if !found {
		return "", false
	}
	return rest, true
}

// IsNested reports whether the case's table carries a struct column.
func (t TableDef) IsNested() bool { return t.Struct != nil }

// physicalCols returns the table's columns in DDL/INSERT order: every flat
// column in Cols order, with the struct column emitted at the position of its
// FIRST leaf.
//
// DDL and InsertSQL must agree on this order or every INSERT binds values to
// the wrong columns, so it is computed once here rather than open-coded twice.
// A positional INSERT is the only form the generated setup uses, which makes
// the agreement load-bearing rather than cosmetic.
func (t TableDef) physicalCols() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range t.Cols {
		if _, nested := leafOf(c.Name); !nested {
			out = append(out, c.Name)
			continue
		}
		if seen[t.Struct.Name] {
			continue
		}
		seen[t.Struct.Name] = true
		out = append(out, t.Struct.Name)
	}
	return out
}

// OutputAlias is the UNIQUE output column name a projected column is given.
//
// A nested projection is always aliased, and that is a correctness requirement
// of the harness rather than a style choice: `SELECT a, n.a FROM t` comes back
// from the engine labelled `[A, A]` — legal SQL, and Java's yaml-tests dialect
// keys result rows BY COLUMN NAME, so the two columns collapse into one and the
// comparison silently stops checking half the projection. The join projections
// alias for exactly this reason; a nested leaf colliding with a flat column of
// the same name is the same hazard reached one level down, and it is the shape
// a wrong-column read has already hidden in.
//
// A flat column is its own alias, so nothing about the flat corpus changes.
func OutputAlias(col string) string {
	return strings.ToUpper(strings.ReplaceAll(col, ".", "_"))
}

// nestedProjectionSQL renders one projection entry of a single-table query,
// aliasing it when the entry is nested.
func nestedProjectionSQL(col string) string {
	if _, nested := leafOf(col); !nested {
		return strings.ToLower(col)
	}
	return strings.ToLower(col) + " AS " + strings.ToLower(OutputAlias(col))
}

// structDDL renders the CREATE TYPE AS STRUCT declarations a nested case's
// schema template needs, ahead of its CREATE TABLE.
func (t TableDef) structDDL() string {
	if t.Struct == nil {
		return ""
	}
	var b strings.Builder
	// POST-ORDER: an inner type must be declared before the type that names it,
	// or CREATE TYPE refers to something the catalog does not have yet.
	var emit func(s *StructCol)
	emit = func(s *StructCol) {
		for _, f := range s.Fields {
			if f.Nested != nil {
				emit(f.Nested)
			}
		}
		fmt.Fprintf(&b, "CREATE TYPE AS STRUCT %s (", strings.ToLower(s.TypeName))
		for i, f := range s.Fields {
			if i > 0 {
				b.WriteString(", ")
			}
			if f.Nested != nil {
				fmt.Fprintf(&b, "%s %s", strings.ToLower(f.Name), strings.ToLower(f.Nested.TypeName))
				continue
			}
			fmt.Fprintf(&b, "%s %s", strings.ToLower(f.Name), sqlTypeName(f.Type))
		}
		b.WriteString(") ")
	}
	emit(t.Struct)
	return b.String()
}

// structLiteral renders one row's struct column as the positional literal an
// INSERT binds, in declaration order.
func (t TableDef) structLiteral(r Row) string {
	var b strings.Builder
	var emit func(s *StructCol, path string)
	emit = func(s *StructCol, path string) {
		b.WriteString("(")
		for i, f := range s.Fields {
			if i > 0 {
				b.WriteString(", ")
			}
			if f.Nested != nil {
				emit(f.Nested, path+"."+f.Name)
				continue
			}
			b.WriteString(renderLiteral(r[path+"."+f.Name]))
		}
		b.WriteString(")")
	}
	emit(t.Struct, t.Struct.Name)
	return b.String()
}

func sqlTypeName(t ColType) string {
	switch t {
	case ColBigint:
		return "BIGINT"
	case ColString:
		return "STRING"
	case ColBoolean:
		return "BOOLEAN"
	case ColDouble:
		return "DOUBLE"
	case ColFloat:
		return "FLOAT"
	}
	return "BIGINT"
}

// NestedGeneratorVersion identifies the nested-case derivation rules, and is
// separate from the flat generator's version on purpose: the two case families
// are generated by different entry points into different seeds, so a change to
// one must not invalidate the other's committed reproduction recipes.
const NestedGeneratorVersion = "rowdiff-nested/2"

// GenerateNested builds a NESTED Case for a seed: the same query mix as
// Generate, over a table whose columns are partly leaves of a struct column.
//
// The query generators are shared verbatim. They read `ColumnDef.Name` and
// never parse it, so a dotted name flows through predicate leaves, ORDER BY
// keys, aggregate arguments, join keys, EXISTS correlations and scalar
// subqueries with no change — which is the point. Deciding per construct which
// ones "support" nesting is precisely the blind spot that let nested defects
// ship: the clauses diverged from each other, so the generator must reach them
// all and let the ENGINE decide what it can answer.
func GenerateNested(seed uint64) *Case {
	// A distinct stream constant keeps a nested seed from deriving the same
	// query mix as the flat seed of the same number; two corpora that differ
	// only in schema would double the file count for one shape's worth of
	// coverage.
	rng := rand.New(rand.NewPCG(seed, seed^0x2545f4914f6cdd1d))

	table := genNestedTable(rng)
	rows := genRows(rng, table)
	nQueries := 4 + rng.IntN(5)
	queries := make([]Query, 0, nQueries)
	for i := 0; i < nQueries; i++ {
		switch {
		case rng.IntN(10) == 0:
			queries = append(queries, genThreeWayJoin(rng, table))
			continue
		case rng.IntN(4) == 0:
			queries = append(queries, genJoinQuery(rng, table))
			continue
		case rng.IntN(4) == 0:
			queries = append(queries, genAggQuery(rng, table))
			continue
		case rng.IntN(6) == 0:
			queries = append(queries, genUnionQuery(rng, table))
			continue
		case rng.IntN(6) == 0:
			queries = append(queries, genDerivedQuery(rng, table))
			continue
		}
		queries = append(queries, genQuery(rng, table))
	}
	return &Case{Seed: seed, Table: table, Rows: rows, Queries: queries, Nested: true}
}

// genNestedTable builds the nested case's table.
//
// The column set is chosen from the shapes that produced real defects, not
// from what is easy to generate:
//
//   - `A` flat and `N.A` nested — a nested leaf whose last segment EQUALS a
//     flat column's name. A wrong-column read lived exactly here, and it is
//     invisible to any schema whose leaf names are all distinct from the flat
//     ones.
//
//   - `N.A` and `N.B` and `N.S` — three leaves of ONE struct root, so a
//     projection can name two of them at once. A nested projection that
//     labelled two distinct columns identically is the second shipped defect,
//     and it needs two leaves of the same root to express.
//
//   - Both a BIGINT and a STRING leaf, so nesting is exercised on a sortable
//     numeric domain AND on the string domain the LIKE/TRIM predicates read.
//
//   - a struct INSIDE the struct, so `n.dp.a` is three segments and a join
//     alias makes `l.n.dp.a` four. RFC-204 4.4 removed the arity cap that made
//     these unreachable: splitColumnRef now hands a SEGMENT LIST to the scope
//     and Scope.ResolvePathNested walks it, so depth is bounded by the schema
//     rather than by the resolver. A generator capped at one level would now be
//     capped BELOW the engine, covering only the shapes that already worked.
//
//   - `A`, `N.A` and `N.DP.A` all share a last segment, so the leaf-collision
//     axis exists at every depth rather than only between a leaf and a flat
//     column.
//
// Nested columns stay OUT of the index pool: a nested-path index is refused at
// DDL, and a refused CREATE takes every candidate of the seed with it.
func genNestedTable(rng *rand.Rand) TableDef {
	structCol := &StructCol{
		Name:     "N",
		TypeName: "NST",
		Fields: []StructField{
			{Name: "A", Type: ColBigint},
			{Name: "B", Type: ColBigint},
			{Name: "S", Type: ColString},
			{Name: "DP", Nested: &StructCol{
				TypeName: "INR",
				Fields: []StructField{
					{Name: "A", Type: ColBigint},
					{Name: "S", Type: ColString},
				},
			}},
		},
	}
	cols := []ColumnDef{
		{Name: "A", Type: ColBigint},
		{Name: "N.A", Type: ColBigint},
		{Name: "N.B", Type: ColBigint, NotNull: true},
		{Name: "N.DP.A", Type: ColBigint},
		{Name: "C", Type: ColBigint},
		{Name: "S", Type: ColString},
		{Name: "N.S", Type: ColString},
		{Name: "N.DP.S", Type: ColString},
		{Name: "F", Type: ColBoolean},
		{Name: "D", Type: ColDouble},
	}

	// Only FLAT columns are indexable: a nested-path index is refused at DDL
	// (`unsupported-DDL:struct-index` in the vendored corpus), so an index on
	// `N.A` would fail the whole schema template and take every candidate of
	// the seed with it. The index axis stays live on the flat columns, which is
	// what makes a nested predicate's residual-filter plan distinguishable from
	// an indexed one.
	pool := []IndexDef{
		{Name: "IDX_A", Cols: []string{"A"}},
		{Name: "IDX_C", Cols: []string{"C"}},
		{Name: "IDX_S", Cols: []string{"S"}},
		{Name: "IDX_AC", Cols: []string{"A", "C"}},
		{Name: "IDX_D", Cols: []string{"D"}},
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	nIdx := 2 + rng.IntN(3)
	if rng.IntN(10) == 0 {
		nIdx = rng.IntN(2)
	}
	idxs := make([]IndexDef, nIdx)
	copy(idxs, pool[:nIdx])

	return TableDef{Name: "T_RDN", Cols: cols, Indexes: idxs, Struct: structCol}
}

// nestedProjections are the projection variants a nested single-table query
// runs under.
//
// `SELECT *` is absent, and that is the one shape deliberately withheld. A
// star over a struct column returns the STRUCT as one value, while the oracle
// holds the leaves as separate row entries — so the two would disagree on
// every row for a reason that is entirely a harness artifact, and the factory
// would file it as an oracle disagreement, i.e. a fabricated engine bug. What
// `SELECT *` over a struct returns is worth testing and is tested, by
// nested-tests.yamsql in the vendored corpus, where the expectation is written
// as a nested map rather than derived from a flat oracle.
//
// Every variant names at least one nested leaf, because a nested case whose
// projections were all flat is a flat case paying for a struct schema.
func (c *Case) nestedProjections() [][]string {
	var nested, flat []string
	for _, col := range c.Table.Cols {
		if _, isNested := leafOf(col.Name); isNested {
			nested = append(nested, col.Name)
		} else {
			flat = append(flat, col.Name)
		}
	}
	// Two leaves of one struct root, adjacent — the duplicate-label shape.
	twoLeaves := append([]string{"ID"}, nested[:min(2, len(nested))]...)
	// The SAME last segment at three depths beside the flat column that shares
	// it. This is the wrong-column read's shape, and it only exists once the
	// struct nests: with one level there is no third `A` to confuse.
	collision := []string{"ID", "A", "N.A", "N.DP.A"}
	// A depth-3 path on its own, the shape the arity cap used to refuse.
	deep := []string{"ID", "N.DP.A", "N.DP.S"}
	// Every leaf plus every flat column: the widest shape, and the one where a
	// mis-ordered output list shows up as a row-wide mismatch.
	wide := append([]string{"ID"}, flat...)
	wide = append(wide, nested...)
	// Nested first, flat after — the same columns in the opposite order, which
	// is what catches a projection built from the TABLE's order rather than
	// from the query's.
	reversed := append([]string{"ID"}, nested...)
	reversed = append(reversed, flat...)
	return [][]string{twoLeaves, collision, deep, wide, reversed}
}

// --- table-derived column pools --------------------------------------------
//
// The leaf generators used to carry LITERAL column lists (`{"A","B","C"}`)
// duplicating genTable's declaration. That duplication is what made the
// generator structurally unable to grow a new schema: a second table shape
// gets predicates over columns it does not have, and the SQL is rejected for a
// reason that has nothing to do with the feature under test. Deriving the pool
// from the TableDef makes a schema change reach every clause automatically,
// which is the property a nested axis needs and the flat generator never had.
//
// For the flat table these return exactly the lists they replaced, in the same
// order and with the same length — so the same seed draws the same columns and
// every committed scenario's reproduction recipe still holds. That equivalence
// is not asserted by inspection: TestFactoryDeterminism regenerates all of the
// committed corpus from its seeds, and TestFlatColumnPoolsAreUnchanged pins the
// four lists directly.

// bigintCols is the BIGINT column pool, in declaration order.
func bigintCols(t TableDef) []string {
	var out []string
	for _, c := range t.Cols {
		if c.Type == ColBigint {
			out = append(out, c.Name)
		}
	}
	return out
}

// numericColsOf is the BIGINT/DOUBLE/FLOAT pool: every pair has a common
// maximum type, so a mixed-width column-vs-column comparison promotes rather
// than erroring.
func numericColsOf(t TableDef) []string {
	var out []string
	for _, c := range t.Cols {
		if isNumericColType(c.Type) {
			out = append(out, c.Name)
		}
	}
	return out
}

// notNullSortCol is the NOT NULL value column an ORDER BY uses when it wants a
// key with no NULLs in it.
func notNullSortCol(t TableDef) string {
	for _, c := range t.Cols {
		if c.NotNull {
			return c.Name
		}
	}
	return "ID"
}

// nullableSortCols is the NULLABLE, ORDERABLE column pool: everything except
// the NOT NULL column and the BOOLEAN, whose two-value domain makes a sort key
// that distinguishes almost nothing.
func nullableSortCols(t TableDef) []string {
	var out []string
	for _, c := range t.Cols {
		if !c.NotNull && c.Type != ColBoolean {
			out = append(out, c.Name)
		}
	}
	return out
}

// The pool accessors below exist for the flat-equivalence pin in
// nested_test.go, which is an external test package: the pins have to read the
// same functions the generator draws from, or they pin a copy.
func BigintColsForTest(t TableDef) []string       { return bigintCols(t) }
func NumericColsForTest(t TableDef) []string      { return numericColsOf(t) }
func NullableSortColsForTest(t TableDef) []string { return nullableSortCols(t) }
func NotNullSortColForTest(t TableDef) string     { return notNullSortCol(t) }
