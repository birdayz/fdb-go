package plangen_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/plangen"
)

func FuzzPredicateTextParser(f *testing.F) {
	f.Add("a = 1")
	f.Add("status = 'active'")
	f.Add("x > 10 AND y < 20")
	f.Add("a = 1 OR b = 2")
	f.Add("a = 1 AND b = 2 OR c = 3")
	f.Add("col BETWEEN 1 AND 100")
	f.Add("id IN (1, 2, 3)")
	f.Add("name LIKE '%test%'")
	f.Add("name NOT LIKE 'foo' ESCAPE '\\'")
	f.Add("col IS NULL")
	f.Add("col IS NOT NULL")
	f.Add("status NOT IN ('deleted', 'archived')")
	f.Add("")
	f.Add("   ")
	f.Add("((()))")
	f.Add("AND AND AND")
	f.Add("OR OR OR")
	f.Add("BETWEEN")
	f.Add("IN ()")
	f.Add("LIKE")
	f.Add("x = 'unterminated")
	f.Add("a = 1 AND")
	f.Add("OR b = 2")
	f.Add("UPPER(name) = 'FOO'")
	f.Add("LENGTH(x) > 5")
	f.Add("ABS(a - b) < 10")
	f.Add("COALESCE(a, b, c) IS NOT NULL")
	f.Add("x + 1 = y * 2")
	f.Add("(a + b) * (c - d) / e > 0")
	f.Add("UPPER(LOWER(name)) = 'test'")
	f.Add("x IS DISTINCT FROM y + 1")
	f.Add("STARTS_WITH(name, UPPER('prefix'))")
	f.Add("a % 2 = 0 AND b / 3 > 1")
	f.Add("NOW() = NOW()")
	f.Add("FUNC(")
	f.Add("FUNC(a,")
	f.Add("FUNC(a, b,)")

	f.Fuzz(func(t *testing.T, s string) {
		src := logical.NewFilter(logical.NewScan("T", ""), s)
		plangen.Convert(src)
	})
}
