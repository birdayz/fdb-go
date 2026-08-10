package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/semantic"
)

// TestRejectNestedPathGroupKey drives every arm of the GROUP BY nested-key
// refusal DIRECTLY, because two of them cannot be reached through the SQL
// surface today.
//
// The unreachable pair is the reason this file exists rather than leaning on
// the end-to-end pin. At both call sites resolveColumnRefStructural runs FIRST,
// so an unresolvable key is already a 42703 before this gate sees it — meaning
// the "resolution failed, stay silent" arm is dead on the current ordering and
// a mutation that deleted it stayed green end-to-end (measured). It is kept
// because it is what stops the refusal from DECIDING EXISTENCE: reorder the two
// checks, or add a third call site that forgets to bracket this one, and
// without that arm a misspelled grouping key would be reported as an
// unsupported feature instead of an undefined column.
func TestRejectNestedPathGroupKey(t *testing.T) {
	t.Parallel()

	// The segments are spelled UPPER because that is what the call sites hand
	// over: splitColumnRef captures through StripIdentifierQuotes, which folds
	// unquoted segments and keeps quoted ones verbatim, so the gate receives an
	// ALREADY-normalized pair and does no folding of its own. Feeding it the
	// user's raw lowercase here would test a contract nothing has.
	newResolver := func(t *testing.T) *expr.Resolver {
		t.Helper()
		tbl := &semantic.StaticTable{
			TableName: semantic.ParseQualifiedName("t2", false),
			TableColumns: []semantic.Column{
				{Id: semantic.NewUnquoted("id"), Type: "BIGINT"},
				// The flat SK shares the struct member's leaf name — the shape
				// that used to defeat the refusal entirely.
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
		return expr.New(semantic.NewAnalyzer(semantic.NewInMemoryCatalog(), false), s)
	}

	for _, tc := range []struct {
		name       string
		nilResolv  bool
		bare       string
		qualifier  string
		qualified  bool
		wantRefuse bool
		why        string
	}{
		{
			name: "a struct member is refused", bare: "SK", qualifier: "N", qualified: true,
			wantRefuse: true,
			why:        "N.SK descends into a struct; the aggregate path cannot carry it, so it must be refused at plan time",
		},
		{
			name: "a struct member with no flat twin is refused", bare: "CO", qualifier: "N", qualified: true,
			wantRefuse: true,
			why:        "N.CO descends too — driven beside N.SK because SK also exists as a flat column, so a leaf-based gate is right about one and wrong about the other",
		},
		{
			name: "a source-qualified column is allowed", bare: "SK", qualifier: "T2", qualified: true,
			wantRefuse: false,
			why:        "T2.SK addresses a source column; refusing it would take every table-qualified grouping key with it",
		},
		{
			name: "a bare reference is allowed", bare: "SK", qualifier: "", qualified: false,
			wantRefuse: false,
			why:        "the bare arm cannot descend at all (SemanticAnalyzer.java:557-559), so it is never this gate's business",
		},
		{
			name: "an unresolvable qualifier is left to the existence check", bare: "SK", qualifier: "NOPE", qualified: true,
			wantRefuse: false,
			why: "NOPE names neither a source nor a struct column. Reporting 0AF00 here would " +
				"claim an unsupported FEATURE for what is a misspelled name, and would shadow " +
				"the 42703 the existence check reports",
		},
		{
			name: "an unresolvable member is left to the existence check", bare: "NOPE", qualifier: "N", qualified: true,
			wantRefuse: false,
			why:        "N exists and is a struct, but NOPE is not one of its members — still an existence question, not a capability one",
		},
		{
			name: "a nil resolver decides nothing", nilResolv: true, bare: "SK", qualifier: "N", qualified: true,
			wantRefuse: false,
			why:        "with no scope there is nothing to resolve against; the gate must not guess from the spelling",
		},
		{
			name: "an empty reference decides nothing", bare: "", qualifier: "N", qualified: true,
			wantRefuse: false,
			why:        "an expression key carries no bare segment and is handled by the expression path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var r *expr.Resolver
			if !tc.nilResolv {
				r = newResolver(t)
			}
			err := rejectNestedPathGroupKey(r, tc.bare, tc.qualifier, tc.qualified)
			if tc.wantRefuse {
				if err == nil {
					t.Fatalf("%s.%s was NOT refused.\n  %s", tc.qualifier, tc.bare, tc.why)
				}
				// The CODE is asserted, not merely that an error came back: the
				// whole point of this refusal is that it is not an
				// undefined-column report.
				if !strings.Contains(err.Error(), "0AF00") {
					t.Errorf("%s.%s refused with %v, want 0AF00 (unsupported_query).\n  %s",
						tc.qualifier, tc.bare, err, tc.why)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s.%s was refused: %v\n  %s", tc.qualifier, tc.bare, err, tc.why)
			}
		})
	}
}
