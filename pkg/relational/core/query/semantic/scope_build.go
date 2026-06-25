package semantic

import (
	"fmt"

	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// BuildScopeFromFromClause walks a parsed FROM clause and produces
// a Scope populated with one ScopeSource per TableSource. The
// analyzer resolves each table via its catalog; missing tables
// return the first TableNotFoundError encountered.
//
// Supports the simple shape (comma-separated AtomTableItem entries
// + optional alias); subquery-in-FROM and JOIN clauses are
// deferred until the join/derived-table resolution passes land.
// Unsupported shapes return an UnsupportedFromShapeError so callers
// can fall back to the existing logical-builder path cleanly.
//
// Pass parent=nil for a top-level query; pass the enclosing scope
// for correlated subqueries.
func (a *Analyzer) BuildScopeFromFromClause(parent *Scope, fromCtx antlrgen.IFromClauseContext) (*Scope, error) {
	scope := NewScope(parent)
	if fromCtx == nil {
		return scope, nil
	}
	sources := fromCtx.TableSources()
	if sources == nil {
		return scope, nil
	}
	for _, ts := range sources.AllTableSource() {
		srcBase, ok := ts.(*antlrgen.TableSourceBaseContext)
		if !ok {
			return nil, &UnsupportedFromShapeError{Shape: fmt.Sprintf("%T", ts)}
		}
		atom, ok := srcBase.TableSourceItem().(*antlrgen.AtomTableItemContext)
		if !ok {
			// Subquery in FROM / other shapes — not wired yet.
			return nil, &UnsupportedFromShapeError{Shape: fmt.Sprintf("%T", srcBase.TableSourceItem())}
		}
		// RFC-140 / R3: the `AT atAlias` unnest-with-ordinality clause parses (Java 4.12
		// #4112) but is not bound until R5; reject it rather than ignore the alias, which
		// could otherwise mis-resolve a reference to a same-named real column.
		if atom.GetAtAlias() != nil {
			return nil, &UnsupportedFromShapeError{Shape: "AT ordinality"}
		}
		if len(srcBase.AllJoinPart()) > 0 {
			return nil, &UnsupportedFromShapeError{Shape: "JOIN"}
		}
		tblName := atom.TableName()
		if tblName == nil {
			return nil, &UnsupportedFromShapeError{Shape: "missing TableName"}
		}
		tbl, err := a.ResolveTableRef(tblName.FullId())
		if err != nil {
			return nil, err
		}
		// Alias: either the AS-bound alias or the table's last-segment
		// name when omitted. Case-fold via the analyzer's setting.
		alias := tbl.Name().LeafIdentifier()
		if alias.IsZero() {
			// LeafIdentifier loses the quoting bit only when the name
			// is zero; construct a fallback from the table string.
			alias = New(tbl.Name().Name(), a.caseSensitive)
		}
		// Grammar is `tableName (AS? alias=uid)?` — AS is optional.
		// `atom.AS()` being nil does NOT mean no alias; check only
		// `GetAlias() != nil`. Earlier version gated on both and
		// silently dropped implicit aliases like `FROM t u`.
		if atom.GetAlias() != nil {
			alias = FromUidContext(atom.GetAlias(), a.caseSensitive)
		}
		if err := scope.AddSource(ScopeSource{
			Table:           tbl,
			Alias:           alias,
			CorrelationName: alias.Name(),
		}); err != nil {
			return nil, err
		}
	}
	return scope, nil
}

// UnsupportedFromShapeError signals a FROM-clause shape the seed
// analyzer doesn't handle yet. Carried up so callers can fall back
// to the existing logical-builder path rather than erroring out at
// the SQL level.
type UnsupportedFromShapeError struct {
	Shape string
}

func (e *UnsupportedFromShapeError) Error() string {
	return fmt.Sprintf("FROM-clause shape not yet supported: %s", e.Shape)
}
