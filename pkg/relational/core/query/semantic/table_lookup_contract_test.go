package semantic_test

// THE CONTRACT EVERY semantic.Table MUST KEEP: what `Columns()` presents,
// `LookupColumn` finds.
//
// It sounds too obvious to test, and it was broken. `recordTypeTable` presents
// FOLDED identifiers in its ordered view while indexing the exact descriptor
// spelling for lookup — deliberately, so plan-time and runtime share one
// namespace — and a caller that built a map from `Columns()` and then asked
// `LookupColumn` for the user's spelling silently missed the column. That
// caller was the USING resolver, and the symptom was Go refusing
// `q1 JOIN q4 USING ("id") JOIN q2 USING ("k")`, which Java answers.
//
// The repair was to resolve through `LookupColumn` and key on the identifier it
// returns, which makes any single implementation self-consistent by
// construction. This test is the other half: it stops a THIRD implementation
// from arriving with the same internal split, which is the thing the repair
// cannot prevent on its own.
//
// The property is deliberately weak enough to be true of every reasonable
// table: a column the table lists must be findable under the identifier the
// table itself listed it with, and must come back as the same column. It says
// nothing about which spellings a table chooses to ALSO accept — that is each
// implementation's business, and rlcatalog accepting both the storage and user
// spellings is exactly why the split was invisible.

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/semantic"
)

// contractTable is one implementation under test, with a name for the failure
// message and a constructed instance carrying at least one column.
type contractTable struct {
	name  string
	table semantic.Table
}

func TestTableLookupAgreesWithColumns(t *testing.T) {
	t.Parallel()

	cases := []contractTable{
		{
			name: "StaticTable, plain lower-case names",
			table: &semantic.StaticTable{
				TableName: semantic.FromSegments([]string{"S", "T"}, false),
				TableColumns: []semantic.Column{
					{Id: semantic.NewUnquoted("id"), Type: "LONG"},
					{Id: semantic.NewUnquoted("k"), Type: "LONG"},
				},
			},
		},
		{
			name: "StaticTable, QUOTED lower-case names",
			table: &semantic.StaticTable{
				TableName: semantic.FromSegments([]string{"S", "T"}, false),
				TableColumns: []semantic.Column{
					{Id: semantic.New(`"id"`, false), Type: "LONG"},
					{Id: semantic.New(`"k"`, false), Type: "LONG"},
				},
			},
		},
		{
			name: "StaticTable, MIXED quoted and unquoted",
			table: &semantic.StaticTable{
				TableName: semantic.FromSegments([]string{"S", "T"}, false),
				TableColumns: []semantic.Column{
					{Id: semantic.New(`"k"`, false), Type: "LONG"},
					{Id: semantic.NewUnquoted("K2"), Type: "LONG"},
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cols := c.table.Columns()
			if len(cols) == 0 {
				t.Fatalf("%s exposes no columns, so this case asserts nothing", c.name)
			}
			for _, col := range cols {
				got, ok := c.table.LookupColumn(col.Id)
				if !ok {
					t.Errorf("%s: Columns() lists %q but LookupColumn(%q) does not find it.\n"+
						"A caller that builds a map from Columns() and then resolves through "+
						"LookupColumn — which is what the USING resolver does — silently loses "+
						"this column, and the query is refused as if it named something that "+
						"does not exist.",
						c.name, col.Id.Name(), col.Id.Name())
					continue
				}
				if got.Id.Name() != col.Id.Name() {
					t.Errorf("%s: LookupColumn(%q) returned the column named %q.\n"+
						"The identifier a table LISTS and the identifier it RETURNS for that "+
						"same column must agree, or a count keyed on one cannot be read with "+
						"the other.",
						c.name, col.Id.Name(), got.Id.Name())
				}
			}
		})
	}
}
