package embedded

import (
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"github.com/antlr4-go/antlr/v4"
)

// checkDecimalConstants parses every decimal constant of a statement as Java's
// AstNormalizer does before anything is planned (its literalNodes run
// ParseHelpers.parseDecimal), so a literal Java refuses fails the statement
// wherever it stands. A LIMIT clause is not descended: Java refuses the clause
// itself (visitLimitClause), and Go's LIMIT reads its own literal.
func checkDecimalConstants(tree antlr.Tree) error {
	switch n := tree.(type) {
	case *antlrgen.DecimalConstantContext:
		_, err := expr.ParseDecimal(n.GetText())
		return err
	case *antlrgen.NegativeDecimalConstantContext:
		_, err := expr.ParseDecimal(n.GetText())
		return err
	case *antlrgen.LimitClauseContext:
		return nil
	}
	for i := 0; i < tree.GetChildCount(); i++ {
		if err := checkDecimalConstants(tree.GetChild(i)); err != nil {
			return err
		}
	}
	return nil
}
