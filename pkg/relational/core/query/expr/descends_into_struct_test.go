package expr_test

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// nestedScope builds a scope whose single source declares a STRUCT column N
// beside a FLAT column that SHARES the struct member's leaf name.
//
// The shared leaf is the whole point of the fixture, not decoration. A gate
// that answers "does this reference descend into a struct?" by looking at the
// bare leaf alone cannot tell `n.sk` from the flat `sk`, and a table WITHOUT
// the flat column hides that completely — every wrong answer and every right
// answer agree there.
func nestedScope(t *testing.T) (*semantic.Analyzer, *semantic.Scope) {
	t.Helper()
	tbl := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("t2", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("id"), Type: "BIGINT"},
			{Id: semantic.NewUnquoted("sk"), Type: "BIGINT"},
			{Id: semantic.NewUnquoted("n"), Type: "RECORD", StructFields: []semantic.Column{
				{Id: semantic.NewUnquoted("sk"), Type: "BIGINT"},
				{Id: semantic.NewUnquoted("co"), Type: "BIGINT"},
			}},
		},
	}
	s := semantic.NewScope(nil)
	if err := s.AddSource(semantic.ScopeSource{
		Table: tbl, Alias: semantic.NewUnquoted("t2"), CorrelationName: "t2",
	}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	return semantic.NewAnalyzer(semantic.NewInMemoryCatalog(), false), s
}

// TestDescendsIntoStruct drives every arm of the descent predicate that the
// GROUP BY refusal gates on. It exists as a unit because the SQL-level pin
// reaches the arms only through the shapes the fixture happens to contain, and
// an arm that is rare today is exactly the one a later change makes live.
func TestDescendsIntoStruct(t *testing.T) {
	t.Parallel()
	a, s := nestedScope(t)
	r := expr.New(a, s)

	t.Run("a struct member descends", func(t *testing.T) {
		t.Parallel()
		for _, leaf := range []string{"sk", "co"} {
			// CO is driven beside SK deliberately: SK is also a flat column, so
			// a predicate that accidentally answered from the flat column would
			// still be right about SK and wrong about CO.
			got, err := r.DescendsIntoStruct(semantic.NewUnquoted("n"), semantic.NewUnquoted(leaf))
			if err != nil {
				t.Fatalf("n.%s: %v", leaf, err)
			}
			if !got {
				t.Errorf("n.%s reported as NOT descending into a struct.\n"+
					"  N is a struct column and %s is one of its members, so this is "+
					"the descent Java's lookupNestedField mints — INFERRED FROM JAVA "+
					"SOURCE, SemanticAnalyzer.java:578-601. A false here disarms the "+
					"GROUP BY refusal and the key escapes to the executor as a "+
					"malformed plan.", leaf, leaf)
			}
		}
	})

	t.Run("a source-qualified column does not descend", func(t *testing.T) {
		t.Parallel()
		// THE CONTROL that stops the arm above from passing for the wrong
		// reason: if DescendsIntoStruct simply returned true for every
		// qualified reference, `t2.sk` would report true too and legitimate
		// qualified grouping keys would all be refused.
		got, err := r.DescendsIntoStruct(semantic.NewUnquoted("t2"), semantic.NewUnquoted("sk"))
		if err != nil {
			t.Fatalf("t2.sk: %v", err)
		}
		if got {
			t.Error("t2.sk reported as descending into a struct.\n" +
				"  T2 is a FROM source, not a struct column, so this reference " +
				"addresses a source column directly. A true here would refuse " +
				"every table-qualified GROUP BY key.")
		}
	})

	t.Run("a bare reference never descends", func(t *testing.T) {
		t.Parallel()
		got, err := r.DescendsIntoStruct(semantic.Identifier{}, semantic.NewUnquoted("sk"))
		if err != nil {
			t.Fatalf("bare sk: %v", err)
		}
		if got {
			t.Error("a bare `sk` reported as descending into a struct.\n" +
				"  The bare arm returns an empty accessor chain: a descent needs a " +
				"prefix to consume (INFERRED FROM JAVA SOURCE, " +
				"SemanticAnalyzer.java:557-559). " +
				"A true here would refuse ordinary flat GROUP BY keys.")
		}
	})

	t.Run("an unresolvable reference reports the error, not a verdict", func(t *testing.T) {
		t.Parallel()
		got, err := r.DescendsIntoStruct(semantic.NewUnquoted("n"), semantic.NewUnquoted("nope"))
		if err == nil {
			t.Fatalf("n.nope resolved (descends=%v); NOPE is not a member of N and not a column anywhere", got)
		}
		if got {
			t.Error("n.nope reported descends=true alongside an error — a failed " +
				"resolution must not carry a verdict, or the refusal would fire on " +
				"references it never resolved.")
		}
		var notFound *semantic.ColumnNotFoundError
		var srcNotFound *semantic.SourceNotFoundError
		if !errors.As(err, &notFound) && !errors.As(err, &srcNotFound) {
			t.Errorf("n.nope: error is %T (%v), want a semantic not-found error.\n"+
				"  The caller relies on this being the SAME error the existence "+
				"check reports, so that existence stays owned by one gate.", err, err)
		}
	})
}
