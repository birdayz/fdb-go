package sqldriver_test

// A searched CASE's WHEN condition must be boolean, and a non-boolean one is a
// REJECTED query rather than a query with a surprising answer.
//
// Go had the check and threw it away. walkCaseCondition asks WalkPredicate
// first — the right order, and the repair for a separate defect where a
// parenthesized compound condition resolved as a record. But it fell back to a
// value walk on ANY error, and WalkPredicate's bare-value lift raises
// DATATYPE_MISMATCH (42804) for a definitively-typed non-boolean, mirroring
// Java's Expression.Utils.toUnderlyingPredicate. The fallback swallowed that:
// the value walk succeeded, the integer became the condition, it compared
// unequal to TRUE, and every row took the ELSE branch.
//
//	SELECT CASE WHEN 1 THEN 'p' ELSE 'q' END FROM t
//	  java   "argument of case when must be of boolean type"
//	  go     'q', 'q'   -- silently, for every row
//
// The two failure kinds mean opposite things and the repair separates them: an
// UnsupportedExpressionShapeError says "no predicate reading of this shape
// exists" and the value walk may have one; a DATATYPE_MISMATCH says "this IS a
// condition and its type is wrong" and the value walk has exactly the same
// wrong value. Only the first falls through.
//
// This pin is Go-only and needs no JVM. The cross-engine measurement that found
// it, and that fails if Java moves, is
// conformance/case_condition_typing_java_probe_test.go.
//
// WHAT MUST KEEP WORKING is half of this file, because a repair that rejects
// too much is the obvious way to get the assertions above to pass. A boolean
// column, a comparison, a parenthesized compound condition, an UNKNOWN-typed
// condition and a NULL condition all still resolve — NULL in particular,
// because the lift folds it to an unknown ConstantPredicate BEFORE the type
// switch that raises the mismatch, so `WHEN NULL` remains the SQL-correct
// "not TRUE, take the ELSE branch".

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
	"github.com/onsi/gomega"
)

func TestFDB_CaseConditionMustBeBoolean(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := setupErrorTestDB(t, "/testdb_case_boolcond", "caseboolcond",
		"CREATE TABLE t (id BIGINT, a BIGINT, s STRING, f BOOLEAN, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO t (id, a, s, f, d) VALUES (1, 1, 'x', true, 1.5), (2, 0, 'y', false, 0.0)")

	// ---- rejected: a definitively-typed non-boolean condition ---------------
	t.Run("rejected", func(t *testing.T) {
		rejected := []struct{ name, cond string }{
			{"integer literal", "1"},
			{"zero literal", "0"},
			{"negative literal", "-1"},
			{"integer column", "a"},
			{"string literal", "'x'"},
			{"string column", "s"},
			{"double column", "d"},
			{"arithmetic", "a + 1"},
			{"parenthesized integer", "(1)"},
			{"parenthesized integer column", "(a)"},
			// A non-boolean buried under the ELSE-arm repair's own shape: the
			// condition is still the thing being typed.
			{"nested parentheses", "((a))"},
		}
		for _, c := range rejected {
			q := fmt.Sprintf("SELECT CASE WHEN %s THEN 'p' ELSE 'q' END FROM t ORDER BY id", c.cond)
			t.Run(c.name, func(t *testing.T) {
				g := gomega.NewWithT(t)
				_, err := db.QueryContext(ctx, q)
				g.Expect(err).To(gomega.HaveOccurred(),
					"a non-boolean CASE condition was ACCEPTED. It cannot be true, so every row "+
						"takes the ELSE branch and the answer looks plausible while meaning "+
						"nothing — the silent direction.\n  q: %s", q)
				var apiErr *api.Error
				g.Expect(errors.As(err, &apiErr)).To(gomega.BeTrue(),
					"expected an *api.Error carrying a SQLSTATE, got %T: %v", err, err)
				g.Expect(string(apiErr.Code)).To(gomega.Equal("42804"),
					"the rejection must be DATATYPE_MISMATCH — the code Java's "+
						"\"argument of case when must be of boolean type\" maps to. A different "+
						"code means the query is failing for some other reason and this test is "+
						"no longer measuring the type check.\n  q: %s\n  err: %v", q, err)
			})
		}
	})

	// ---- accepted: everything the repair must NOT have broken ---------------
	//
	// A repair that rejects too much passes every assertion above. These are
	// what stop that.
	t.Run("still_accepted", func(t *testing.T) {
		accepted := []struct {
			name, cond string
			want       []string
		}{
			{"boolean column", "f", []string{"p", "q"}},
			{"negated boolean column", "NOT f", []string{"q", "p"}},
			{"comparison", "a = 1", []string{"p", "q"}},
			{"parenthesized comparison", "(a = 1)", []string{"p", "q"}},
			{"parenthesized compound", "(a = 1 AND s = 'x')", []string{"p", "q"}},
			{"parenthesized boolean column", "(f)", []string{"p", "q"}},
			{"boolean literal TRUE", "TRUE", []string{"p", "p"}},
			{"boolean literal FALSE", "FALSE", []string{"q", "q"}},
			{"IS NULL", "s IS NULL", []string{"q", "q"}},
			{"IN predicate", "a IN (1, 5)", []string{"p", "q"}},
			{"BETWEEN predicate", "a BETWEEN 1 AND 9", []string{"p", "q"}},
			{"LIKE predicate", "s LIKE 'x%'", []string{"p", "q"}},
			// NULL is the deliberate permissive arm. Java rejects it; SQL says a
			// non-TRUE condition takes the ELSE branch, and the lift folds NULL
			// to an unknown ConstantPredicate before the type switch — so this
			// must keep answering, not start raising 42804.
			{"NULL condition", "NULL", []string{"q", "q"}},
		}
		for _, c := range accepted {
			q := fmt.Sprintf("SELECT CASE WHEN %s THEN 'p' ELSE 'q' END FROM t ORDER BY id", c.cond)
			t.Run(c.name, func(t *testing.T) {
				g := gomega.NewWithT(t)
				got, err := mmRows(t, ctx, db, q)
				g.Expect(err).NotTo(gomega.HaveOccurred(),
					"the type check rejected a condition it must accept — the repair narrowed too "+
						"far. Only a DEFINITIVELY-TYPED non-boolean is a mismatch; boolean, "+
						"UNKNOWN and NULL are not.\n  q: %s", q)
				g.Expect(got).To(gomega.Equal(c.want), "q: %s", q)
			})
		}
	})

	// ---- the non-boolean check is on the CONDITION, not on the arms ---------
	//
	// Java asserts BOOLEAN on the condition and passes the consequents through
	// untyped; the arms unify by maximumType. So a mixed-type CASE is legal and
	// a non-boolean ARM must not be dragged into the rejection.
	t.Run("arms_are_not_type_checked_as_conditions", func(t *testing.T) {
		g := gomega.NewWithT(t)
		for _, q := range []string{
			"SELECT CASE WHEN f THEN 1 ELSE 0 END FROM t ORDER BY id",
			"SELECT CASE WHEN f THEN 'p' ELSE 'q' END FROM t ORDER BY id",
			"SELECT CASE WHEN f THEN a > 0 ELSE FALSE END FROM t ORDER BY id",
			"SELECT CASE WHEN f THEN NULL ELSE 1 END FROM t ORDER BY id",
		} {
			_, err := db.QueryContext(ctx, q)
			g.Expect(err).NotTo(gomega.HaveOccurred(),
				"a CASE ARM was type-checked as if it were a condition. The BOOLEAN requirement "+
					"is on the WHEN condition only.\n  q: %s", q)
		}
	})
}
