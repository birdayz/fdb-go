package embedded

import (
	"context"
	"database/sql/driver"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// Recursive CTE materialisation.
//
// materializeRecursiveCTE drives the UNION ALL / UNION DISTINCT
// semi-naive evaluation, dispatching to recursiveCTELevelOrder (BFS
// by depth, Java's default TRAVERSAL ORDER) or recursiveCTEDFS
// (pre-order / post-order). recursiveCTEIterationLimit caps UNION
// ALL iterations so cyclic graphs don't hang the scan — UNION
// DISTINCT converges naturally on the seen-row set.
//
// The CTE result set lives as a cteData in c.ctes during the
// outer SELECT's evaluation; execSelectFromCTE consumes it later.

// recursiveCTEIterationLimit caps the number of semi-naive iterations
// (level-order) or the DFS emit count (pre/post-order) for a WITH
// RECURSIVE body. Protects against unbounded recursion on cyclic
// graphs with UNION ALL (UNION DISTINCT converges naturally by
// filtering rows already seen). A well-formed ancestor/descendant
// query over an acyclic hierarchy terminates far below this cap.
const recursiveCTEIterationLimit = 10000

// recursiveTraversal encodes how Java's `TRAVERSAL ORDER …` clause
// selects the recursive-CTE emission order. Level-order = semi-naive
// (BFS per depth); pre-order / post-order = DFS with emission before /
// after the recursive descent into a row's children.
type recursiveTraversal int

const (
	traversalLevelOrder recursiveTraversal = iota
	traversalPreOrder
	traversalPostOrder
)

// materializeRecursiveCTE evaluates a WITH RECURSIVE CTE body. The
// body must be a UNION [ALL] where the left (seed) does not
// self-reference and the right (recursive) references the CTE name.
// The CTE name is bound in c.ctes before the recursive branch is
// evaluated; different strategies choose a different binding.
//
// Level-order (BFS / semi-naive): the binding is the last iteration's
// new rows (the "working set"); iteration terminates when the branch
// produces no new rows. Pre/post-order (DFS): the binding is a single
// row at a time; recursion descends row-by-row, with emission before
// (pre) or after (post) the descent.
//
// For UNION ALL, every row the recursive branch produces is emitted;
// cycles are bounded by the iteration/emit cap. For UNION DISTINCT,
// rows already present in the cumulative result are filtered out,
// which also guarantees termination on cyclic graphs.
func (c *EmbeddedConnection) materializeRecursiveCTE(
	ctx context.Context,
	body antlrgen.IQueryExpressionBodyContext,
	cteName string,
	renameList []string,
	traversal recursiveTraversal,
) ([]string, [][]driver.Value, error) {
	setQ, ok := body.(*antlrgen.SetQueryContext)
	if !ok {
		return nil, nil, api.NewErrorf(api.ErrCodeInvalidRecursion,
			"recursive CTE %q body must be a UNION between a non-recursive seed and a recursive branch", cteName)
	}
	if setQ.UNION() == nil {
		return nil, nil, api.NewErrorf(api.ErrCodeInvalidRecursion,
			"recursive CTE %q requires UNION in the body", cteName)
	}
	distinct := setQ.ALL() == nil

	// Evaluate the seed with cteName unbound. A stray self-reference in
	// the seed surfaces as a normal table-not-found error — standard SQL
	// forbids seed self-reference, and we get that enforcement for free.
	seedCols, _, seedRows, err := c.execQueryBodyRows(ctx, setQ.GetLeft())
	if err != nil {
		return nil, nil, err
	}

	// Apply column rename (`WITH RECURSIVE t(c1, c2, ...) AS ...`) to
	// the seed schema so the recursive branch — which scans this CTE
	// via its name — sees the renamed columns, not the seed's original
	// projection names. When no rename is present, strip projection
	// qualifiers so `SELECT d.id FROM t AS d` produces a CTE column
	// named `id` rather than `d.id` (matches the non-recursive path).
	if renameList != nil {
		if len(renameList) != len(seedCols) {
			return nil, nil, api.NewErrorf(api.ErrCodeInvalidColumnReference,
				"CTE %q column-rename has %d names but inner query has %d columns",
				cteName, len(renameList), len(seedCols))
		}
		seedCols = renameList
	} else {
		seedCols = stripCTEColumnQualifiers(seedCols)
	}

	switch traversal {
	case traversalPreOrder, traversalPostOrder:
		rows, dErr := c.recursiveCTEDFS(ctx, setQ, cteName, seedCols, seedRows, distinct, traversal)
		return seedCols, rows, dErr
	default:
		rows, dErr := c.recursiveCTELevelOrder(ctx, setQ, cteName, seedCols, seedRows, distinct)
		return seedCols, rows, dErr
	}
}

// recursiveCTELevelOrder implements semi-naive BFS: each iteration
// binds the CTE to the previous round's new rows and re-evaluates the
// recursive branch. Termination: branch produces no new rows (UNION
// ALL on acyclic data + UNION DISTINCT in general) or iteration cap
// hit (cyclic UNION ALL).
func (c *EmbeddedConnection) recursiveCTELevelOrder(
	ctx context.Context,
	setQ *antlrgen.SetQueryContext,
	cteName string,
	seedCols []string,
	seedRows [][]driver.Value,
	distinct bool,
) ([][]driver.Value, error) {
	var cumulative [][]driver.Value
	var working [][]driver.Value
	var seen map[string]struct{}
	if !distinct {
		cumulative = append([][]driver.Value(nil), seedRows...)
		working = seedRows
	} else {
		seen = make(map[string]struct{}, len(seedRows))
		dedup := make([][]driver.Value, 0, len(seedRows))
		for _, r := range seedRows {
			k := rowKey(r)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			dedup = append(dedup, r)
		}
		cumulative = dedup
		working = dedup
	}

	// The per-iteration binding c.ctes[cteName] is left in place when
	// the loop exits — the caller (the WITH clause loop in execSelect)
	// overwrites it with the cumulative result immediately after this
	// function returns. On error, execSelect returns early and the
	// enclosing pushCTEScope defer pops the whole scope, so a stale
	// intermediate binding is unreachable either way.

	for iter := 0; len(working) > 0; iter++ {
		if iter >= recursiveCTEIterationLimit {
			return nil, api.NewErrorf(api.ErrCodeExecutionLimitReached,
				"recursive CTE %q exceeded iteration limit of %d — possible cycle or an unbounded result set; use UNION (DISTINCT) or a depth predicate",
				cteName, recursiveCTEIterationLimit)
		}
		c.ctes[cteName] = &cteData{cols: seedCols, rows: working}
		iterCols, _, iterRows, iErr := c.execQueryBodyRows(ctx, setQ.GetRight())
		if iErr != nil {
			return nil, iErr
		}
		if len(iterCols) != len(seedCols) {
			return nil, api.NewErrorf(api.ErrCodeUnionIncorrectColumnCount,
				"recursive CTE %q: seed has %d columns, recursive branch produced %d",
				cteName, len(seedCols), len(iterCols))
		}
		var newRows [][]driver.Value
		if !distinct {
			newRows = iterRows
		} else {
			newRows = make([][]driver.Value, 0, len(iterRows))
			for _, r := range iterRows {
				k := rowKey(r)
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
				newRows = append(newRows, r)
			}
		}
		if len(newRows) == 0 {
			break
		}
		cumulative = append(cumulative, newRows...)
		working = newRows
	}

	return cumulative, nil
}

// recursiveCTEDFS implements DFS pre/post-order: for each seed row,
// emit the row (pre) or descend first (post), then recurse with the
// CTE bound to just that single row so the recursive branch's
// self-reference yields this row's "children". Emission order matches
// Java 4.7.1.0+'s RecursiveUnionCursor DFS modes.
//
// For UNION DISTINCT, a shared `seen` set across the whole traversal
// filters duplicates — both at the seed level and at each descent.
// For UNION ALL there is no dedup; a hard emit cap bounds cycles.
func (c *EmbeddedConnection) recursiveCTEDFS(
	ctx context.Context,
	setQ *antlrgen.SetQueryContext,
	cteName string,
	seedCols []string,
	seedRows [][]driver.Value,
	distinct bool,
	traversal recursiveTraversal,
) ([][]driver.Value, error) {
	var seen map[string]struct{}
	if distinct {
		seen = make(map[string]struct{}, len(seedRows))
	}
	cumulative := make([][]driver.Value, 0, len(seedRows))
	preorder := traversal == traversalPreOrder

	var descend func(row []driver.Value) error
	descend = func(row []driver.Value) error {
		if len(cumulative) >= recursiveCTEIterationLimit {
			return api.NewErrorf(api.ErrCodeExecutionLimitReached,
				"recursive CTE %q exceeded emit limit of %d — possible cycle or an unbounded result set; use UNION (DISTINCT) or a depth predicate",
				cteName, recursiveCTEIterationLimit)
		}
		if preorder {
			cumulative = append(cumulative, row)
		}
		c.ctes[cteName] = &cteData{cols: seedCols, rows: [][]driver.Value{row}}
		iterCols, _, iterRows, iErr := c.execQueryBodyRows(ctx, setQ.GetRight())
		if iErr != nil {
			return iErr
		}
		if len(iterCols) != len(seedCols) {
			return api.NewErrorf(api.ErrCodeUnionIncorrectColumnCount,
				"recursive CTE %q: seed has %d columns, recursive branch produced %d",
				cteName, len(seedCols), len(iterCols))
		}
		for _, child := range iterRows {
			if distinct {
				k := rowKey(child)
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
			}
			if err := descend(child); err != nil {
				return err
			}
		}
		if !preorder {
			if len(cumulative) >= recursiveCTEIterationLimit {
				return api.NewErrorf(api.ErrCodeExecutionLimitReached,
					"recursive CTE %q exceeded emit limit of %d — possible cycle or an unbounded result set; use UNION (DISTINCT) or a depth predicate",
					cteName, recursiveCTEIterationLimit)
			}
			cumulative = append(cumulative, row)
		}
		return nil
	}

	for _, seed := range seedRows {
		if distinct {
			k := rowKey(seed)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
		}
		if err := descend(seed); err != nil {
			return nil, err
		}
	}
	return cumulative, nil
}
