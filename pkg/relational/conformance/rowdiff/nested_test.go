package rowdiff_test

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/rowdiff"
)

// TestNestedCaseRendersAStructSchema pins the two renderings that speak
// PHYSICAL schema — DDL and INSERT — because they are the only two places that
// have to know a dotted column name is a struct leaf, and they have to agree
// with each other positionally or every row binds to the wrong column.
func TestNestedCaseRendersAStructSchema(t *testing.T) {
	t.Parallel()
	c := rowdiff.GenerateNested(1)

	ddl := c.DDL()
	for _, want := range []string{
		"CREATE TYPE AS STRUCT nst (a BIGINT, b BIGINT, s STRING)",
		"CREATE TABLE T_RDN (id BIGINT, a BIGINT, n nst, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, PRIMARY KEY (id))",
	} {
		if !strings.Contains(ddl, want) {
			t.Fatalf("DDL is missing %q:\n%s", want, ddl)
		}
	}
	// The struct column must appear ONCE even though three columns are its
	// leaves — a per-leaf emission would produce a table with three `n` columns
	// and a CREATE that fails, taking every candidate of the seed with it.
	if n := strings.Count(ddl, " n nst"); n != 1 {
		t.Fatalf("struct column emitted %d times, want 1:\n%s", n, ddl)
	}
	// A nested column must never be indexed: a nested-path index is refused at
	// DDL, and the refusal kills the whole schema template.
	if strings.Contains(ddl, "CREATE INDEX") && strings.Contains(ddl, "(n.") {
		t.Fatalf("an index reads a nested path, which DDL refuses:\n%s", ddl)
	}

	ins := c.InsertSQL()
	// Positional agreement: id, a, <struct literal>, c, s, f, d — seven values
	// per row, with the struct's three leaves inside ONE parenthesised group.
	first := ins[strings.Index(ins, "VALUES ")+len("VALUES ") : strings.Index(ins, "), (")+1]
	if strings.Count(first, "(") != 2 {
		t.Fatalf("the first row is not (…, (struct), …): %s", first)
	}
}

// TestNestedProjectionsAreAliasedAndNeverStar pins the projection contract.
//
// Both halves are load-bearing and they fail in opposite directions. Without
// the alias, `SELECT a, n.a` returns two columns both labelled A and the
// name-keyed row comparison silently drops one — a weaker assertion that still
// reports green. With a star, the engine returns the struct as one value while
// the oracle holds the leaves separately, and the factory files the disagreement
// as an engine bug that does not exist.
func TestNestedProjectionsAreAliasedAndNeverStar(t *testing.T) {
	t.Parallel()
	c := rowdiff.GenerateNested(7)
	var sawNested, sawSingleTable int
	for _, q := range c.Queries {
		for _, proj := range c.ProjectionsFor(q) {
			if q.Agg == nil && proj == nil {
				t.Fatalf("a nested case offered SELECT * (query %+v)", q)
			}
			if q.Agg != nil || q.Join != nil || q.ThreeWay != nil {
				continue
			}
			sawSingleTable++
			sql := c.SQL(q, proj)
			for _, p := range proj {
				if !strings.Contains(p, ".") {
					continue
				}
				sawNested++
				alias := strings.ToLower(rowdiff.OutputAlias(p))
				if !strings.Contains(sql, strings.ToLower(p)+" AS "+alias) {
					t.Fatalf("nested projection %q is not aliased in:\n%s", p, sql)
				}
			}
		}
	}
	if sawSingleTable == 0 || sawNested == 0 {
		t.Fatalf("this test measured nothing: %d single-table projections, %d nested entries. "+
			"A nested case whose projections are all flat is a flat case paying for a struct schema.",
			sawSingleTable, sawNested)
	}
}

// TestNestedCaseReachesEveryClause is the anti-blind-spot gate.
//
// The generator's whole failure mode was that nested paths reached SELECT and
// nothing else, so the clauses diverged from each other undetected. This asserts
// a dotted column actually lands in the WHERE, the ORDER BY, an aggregate
// argument and a GROUP BY key across a seed range — measured, not assumed.
func TestNestedCaseReachesEveryClause(t *testing.T) {
	t.Parallel()
	clauses := map[string]int{}
	for seed := uint64(1); seed <= 200; seed++ {
		c := rowdiff.GenerateNested(seed)
		for _, q := range c.Queries {
			for _, proj := range c.ProjectionsFor(q) {
				for _, p := range proj {
					if strings.Contains(p, ".") {
						clauses["select"]++
					}
				}
			}
			if q.Where != nil && strings.Contains(rowdiff.PredicateSQL(q.Where), "n.") {
				clauses["where"]++
			}
			for _, k := range q.OrderBy {
				if strings.Contains(k.Col, ".") {
					clauses["order-by"]++
				}
			}
			if q.Agg != nil {
				if strings.Contains(q.Agg.Col, ".") {
					clauses["agg-arg"]++
				}
				for _, g := range q.Agg.GroupBy {
					if strings.Contains(g, ".") {
						clauses["group-by"]++
					}
				}
			}
			if q.Exists != nil && strings.Contains(q.Exists.CorrCol, ".") {
				clauses["exists-corr"]++
			}
			if q.ScalarSub != nil && strings.Contains(q.ScalarSub.OuterCol, ".") {
				clauses["scalar-sub"]++
			}
		}
	}
	for _, want := range []string{"select", "where", "order-by", "agg-arg", "group-by", "exists-corr", "scalar-sub"} {
		if clauses[want] == 0 {
			t.Errorf("no nested path ever reached the %s clause over 200 seeds — that clause is a "+
				"generator blind spot, which is exactly how the nested-path defects shipped. Counts: %v",
				want, clauses)
		}
	}
	t.Logf("nested clause reach over 200 seeds: %v", clauses)
}

// TestFlatGeneratorIsUnchangedByNesting is the regression floor for the
// committed corpus: every one of its scenarios was generated by the FLAT path,
// and its reproduction recipe is a lie the moment that path renders differently.
func TestFlatGeneratorIsUnchangedByNesting(t *testing.T) {
	t.Parallel()
	c := rowdiff.Generate(1)
	if c.Nested || c.Table.IsNested() {
		t.Fatal("the flat generator produced a nested case")
	}
	if !strings.HasPrefix(c.DDL(), "CREATE TABLE T_RD (id BIGINT, a BIGINT,") {
		t.Fatalf("flat DDL changed shape: %s", c.DDL())
	}
	if strings.Contains(c.DDL(), "STRUCT") {
		t.Fatalf("flat DDL grew a struct: %s", c.DDL())
	}
	for _, q := range c.Queries {
		for _, proj := range c.ProjectionsFor(q) {
			if strings.Contains(c.SQL(q, proj), " AS n_") {
				t.Fatalf("flat SQL grew a nested alias: %s", c.SQL(q, proj))
			}
		}
	}
}

// TestFlatColumnPoolsAreUnchanged pins the four table-derived column pools
// against the literal lists they replaced in the leaf generators.
//
// The pools are drawn from with `rng.IntN(len(pool))`, so a pool that changed
// LENGTH or ORDER makes the same seed draw a different column — and every
// committed corpus scenario carries a header promising its seed reproduces it.
// This fails in milliseconds and names the pool; TestFactoryDeterminism finds
// the same break but only after planning the whole corpus.
func TestFlatColumnPoolsAreUnchanged(t *testing.T) {
	t.Parallel()
	flat := rowdiff.Generate(1).Table
	for _, tc := range []struct {
		pool string
		got  []string
		want []string
	}{
		{"bigint", rowdiff.BigintColsForTest(flat), []string{"A", "B", "C"}},
		{"numeric", rowdiff.NumericColsForTest(flat), []string{"A", "B", "C", "D", "E"}},
		{"nullable-sort", rowdiff.NullableSortColsForTest(flat), []string{"A", "C", "S", "D", "E"}},
	} {
		if strings.Join(tc.got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("flat %s pool is %v, want %v — every committed scenario's seed now draws different columns",
				tc.pool, tc.got, tc.want)
		}
	}
	if got := rowdiff.NotNullSortColForTest(flat); got != "B" {
		t.Errorf("flat NOT NULL sort column is %q, want %q", got, "B")
	}
}

// TestNoQueryNamesAColumnTheTableLacks is the gate that caught the generator
// emitting `L.B` over a table whose only B is the struct leaf `N.B`.
//
// That failure mode is the one this whole extension exists to remove, and it is
// nastier than it looks: the query is rejected with 42703, which reads exactly
// like the nested-path resolution gap being measured. A generator whose own
// output is malformed reports the engine's limits wrongly, so the honest
// accounting of what nesting the engine declines depends on this being true.
func TestNoQueryNamesAColumnTheTableLacks(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 60; seed++ {
		c := rowdiff.GenerateNested(seed)
		known := map[string]bool{"ID": true}
		for _, col := range c.Table.Cols {
			known[col.Name] = true
		}
		var checked int
		for _, q := range c.Queries {
			for _, proj := range c.ProjectionsFor(q) {
				for _, p := range proj {
					checked++
					// A join projection is qualified ("L.N.A"); strip the alias.
					name := p
					for _, q := range []string{"L.", "R.", "M."} {
						if strings.HasPrefix(name, q) {
							name = name[len(q):]
							break
						}
					}
					if !known[name] {
						t.Fatalf("seed %d projects %q, which table %s does not have (columns %v)",
							seed, p, c.Table.Name, known)
					}
				}
			}
			for _, k := range q.OrderBy {
				checked++
				if !known[k.Col] {
					t.Fatalf("seed %d orders by %q, which table %s does not have", seed, k.Col, c.Table.Name)
				}
			}
		}
		if checked == 0 {
			t.Fatalf("seed %d yielded nothing to check", seed)
		}
	}
}
