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

	f.Fuzz(func(t *testing.T, s string) {
		src := logical.NewFilter(logical.NewScan("T", ""), s)
		plangen.Convert(src)
	})
}
