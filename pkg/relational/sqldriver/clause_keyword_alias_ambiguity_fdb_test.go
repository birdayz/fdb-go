package sqldriver_test

// A keyword that both INTRODUCES A CLAUSE and is usable as a bare identifier is
// ambiguous after a table source, and the alias reading can win.
//
// That is not hypothetical. FULL sat in the grammar's `keywordsCanBeId` list
// while `(LEFT | RIGHT | FULL) OUTER? JOIN` needed it as a keyword, so
// `FROM a FULL JOIN b` parsed as `FROM a AS FULL JOIN b` — an INNER join — and
// since SQL makes OUTER optional the short spelling silently answered a
// different query. Measured at the time: 13 rows with OUTER, 10 without.
//
// LEFT and RIGHT were already excluded for exactly that reason, with a comment
// in the grammar saying so. FULL had been overlooked. This file closes the
// CLASS rather than the one instance: it enumerates every clause-introducing
// keyword that `keywordsCanBeId` still admits and asserts that each one is
// parsed as its CLAUSE.
//
// The census that produced the list is worth repeating rather than trusting,
// because it is what makes this file's coverage checkable:
//
//	awk '/^keywordsCanBeId/,/^    ;/' RelationalParser.g4 |
//	  grep -ow 'INNER|CROSS|NATURAL|JOIN|USING|WHERE|GROUP|HAVING|ORDER|LIMIT|OFFSET|UNION|LEFT|RIGHT|FULL'
//
// which today reports exactly GROUP, OFFSET and ORDER — LEFT, RIGHT and FULL
// having been excluded, and the rest never having been admitted.
//
// WHY THESE THREE ARE EXPECTED TO BE SAFE, and why that is asserted rather than
// assumed: consuming GROUP or ORDER as an alias leaves `BY <expr>` dangling and
// consuming OFFSET leaves a bare number, none of which completes a valid parse.
// The alias reading therefore cannot win — the parser has to backtrack to the
// clause reading. FULL was dangerous precisely because `FULL JOIN b ON p` IS a
// complete valid parse under the alias reading, so nothing forced backtracking.
//
// That reasoning is exactly the kind this session has repeatedly found to be
// wrong when checked, so it is checked.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func openClauseKeywordDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_clausekw")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_clausekw")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE clausekw_t "+
		"CREATE TABLE t (id BIGINT, g BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_clausekw/s WITH TEMPLATE clausekw_t")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///testdb_clausekw?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFDB_ClauseKeywordsAreNotSwallowedAsAliases(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := openClauseKeywordDB(t)
	// g groups {1,1,1} and {2,2}: any GROUP BY that actually grouped gives two
	// rows with counts 3 and 2, and one that did not gives something else.
	mwjoMustExec(t, db, ctx,
		"INSERT INTO t (id, g) VALUES (1,1), (2,1), (3,1), (4,2), (5,2)")

	cases := []struct {
		keyword string
		sql     string
		want    []string
		why     string
	}{
		{
			keyword: "GROUP",
			sql:     "SELECT g, COUNT(*) FROM t GROUP BY g ORDER BY g",
			want:    []string{"1|3", "2|2"},
			why: "grouping actually happened — an alias reading would leave `BY g` " +
				"dangling, and a non-grouping fallback would return five rows",
		},
		{
			keyword: "ORDER",
			sql:     "SELECT id FROM t ORDER BY id DESC",
			want:    []string{"5", "4", "3", "2", "1"},
			why: "the ordering was applied and applied DESCENDING — an alias reading " +
				"would leave `BY id DESC` dangling, and a dropped ORDER BY would most " +
				"likely return ascending order, which is the reverse",
		},
		{
			keyword: "OFFSET",
			sql:     "SELECT id FROM t ORDER BY id LIMIT 2 OFFSET 2",
			want:    []string{"3", "4"},
			why: "the offset was applied — an alias reading would leave a bare `2` " +
				"dangling, and a dropped OFFSET would return the first two ids",
		},
	}

	for _, c := range cases {
		t.Run(c.keyword, func(t *testing.T) {
			got, err := mmRows(t, ctx, db, c.sql)
			if err != nil {
				t.Fatalf("%s is no longer parsed as a clause: %v\n  q: %s\n"+
					"  If %s has become ambiguous with a table alias, it needs excluding from "+
					"keywordsCanBeId the way LEFT, RIGHT and FULL are", c.keyword, err, c.sql,
					c.keyword)
			}
			if !mmEqRows(got, c.want) {
				t.Errorf("%s was not applied as a clause\n  q: %s\n  got  %v\n  want %v\n  %s",
					c.keyword, c.sql, got, c.want, c.why)
			}
		})
	}

	// OUTER IS OPTIONAL FOR ALL THREE, and all three are checked rather than
	// only the one that broke.
	//
	// The grammar spells them together — `(LEFT | RIGHT | FULL) OUTER? JOIN` —
	// so the optionality is one production and the property is one property.
	// FULL was the member that failed it, because FULL alone was still in
	// keywordsCanBeId; LEFT and RIGHT were already excluded. Checking only FULL
	// would pin the instance and leave the property resting on the fact that
	// nobody has since added LEFT or RIGHT back to that list.
	//
	// The predicate is chosen so the three joins give DIFFERENT answers from
	// each other over this fixture. Two rows on the left, five on the right, and
	// `u.g = 1` matches three of them: LEFT preserves the unmatched left rows,
	// RIGHT the unmatched right ones, FULL both. A test whose three joins all
	// returned the same count would pass against an engine that had collapsed
	// them into one.
	t.Run("OUTER is optional for every join type", func(t *testing.T) {
		type result struct {
			outer, short []string
		}
		got := map[string]result{}

		for _, kind := range []string{"LEFT", "RIGHT", "FULL"} {
			outerQ := fmt.Sprintf(
				"SELECT COUNT(*) FROM t %s OUTER JOIN t AS u ON u.g = 1 AND t.g = 2", kind)
			shortQ := fmt.Sprintf(
				"SELECT COUNT(*) FROM t %s JOIN t AS u ON u.g = 1 AND t.g = 2", kind)

			withOuter, err := mmRows(t, ctx, db, outerQ)
			if err != nil {
				t.Errorf("%s OUTER JOIN failed: %v", kind, err)
				continue
			}
			short, err := mmRows(t, ctx, db, shortQ)
			if err != nil {
				t.Errorf("`%s JOIN` (no OUTER) is not accepted: %v\n"+
					"  SQL makes OUTER optional and the grammar agrees — "+
					"`(LEFT | RIGHT | FULL) OUTER? JOIN` — so the short spelling must parse as "+
					"the same join", kind, err)
				continue
			}
			if !mmEqRows(short, withOuter) {
				t.Errorf("`%s JOIN` and `%s OUTER JOIN` disagree\n  OUTER -> %v\n  short -> %v\n"+
					"  (%s is being consumed as a table ALIAS — check whether it has returned to "+
					"keywordsCanBeId, which is what made FULL do exactly this)",
					kind, kind, withOuter, short, kind)
			}
			got[kind] = result{outer: withOuter, short: short}
		}

		// The vacuity guard, and it is the one that matters here: three joins
		// answering identically would satisfy every assertion above while
		// proving nothing about which join ran. Over this fixture they must
		// differ — LEFT and RIGHT preserve different sides, and FULL preserves
		// both, so FULL is strictly the largest.
		if len(got) != 3 {
			t.Fatalf("only %d join kinds were measured, not 3 — the comparisons above are "+
				"about a shrunken set", len(got))
		}
		left, right, full := got["LEFT"].outer, got["RIGHT"].outer, got["FULL"].outer
		if mmEqRows(left, right) && mmEqRows(right, full) {
			t.Errorf("LEFT (%v), RIGHT (%v) and FULL (%v) all answered the SAME over a fixture "+
				"built to separate them. Each assertion above then compares a join with itself, "+
				"and an engine that collapsed all three into one shape would pass",
				left, right, full)
		}
	})
}
