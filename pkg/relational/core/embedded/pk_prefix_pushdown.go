package embedded

import (
	"context"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Composite-PK pure-prefix pushdown.
//
// `WHERE a = 1 AND b = 5` on PK (a, b, c) narrows to the tuple-prefix
// scan [rtk, 1, 5]. Before this branch, such queries fell through to
// the full type scan — PK-equality required all cols equated, and the
// composite-range / composite-IN-list paths require at least one
// non-equality constraint.
//
// Lives in its own file (rather than buried in connection.go) so
// Phase 1c of RFC 021 can relocate it into a future
// pkg/relational/core/plan/physical/scan_pk_prefix.go with minimal
// re-work.

// pkCompositePrefix describes a composite-PK pure-prefix pushdown:
// equalities on the first len(prefixVals) PK cols with no range /
// IN-list / BETWEEN on any col. Scan range is `[rtk, prefixVals...]`
// as a tuple prefix — every record whose leading PK components
// match drops into the cursor. PK cols after the prefix are
// unconstrained here; the scan loop's evalPredicate re-applies
// any residual WHERE on trailing cols as a post-filter.
type pkCompositePrefix struct {
	prefixVals []any
}

// tryPKCompositePrefixPushdown is the SELECT-gated variant of
// tryPKCompositePrefixFromWhere.
func (c *EmbeddedConnection) tryPKCompositePrefixPushdown(
	ctx context.Context,
	sq *selectQuery,
	rt *recordlayer.RecordType,
) (pkCompositePrefix, bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return pkCompositePrefix{}, false
	}
	if sq.havingExpr != nil {
		return pkCompositePrefix{}, false
	}
	return c.tryPKCompositePrefixFromWhere(ctx, sq.whereExpr, rt)
}

// tryPKCompositePrefixFromWhere recognises a composite PK where the
// leading cols are equated but no range / IN-list constraint is
// available to feed the tighter composite-range or composite-IN-list
// paths. `WHERE a = 1 AND b = 5` on PK (a, b, c) narrows to `[rtk,
// 1, 5]` as a tuple prefix — strict win over the fallback full type
// scan.
//
// Call ordering: tried LAST of the PK pushdown branches, after
// equality (all cols), composite IN-list, composite range, and
// single-col forms. If any tighter branch succeeds, this one never
// runs. Non-PK leaves (and leaves on unequated trailing PK cols)
// remain post-filter via evalPredicate — same discipline as the
// composite-range relaxation.
//
// Bail cases:
//   - Single-col PKs (no composite structure — single-col range /
//     equality / IN-list handle these).
//   - No equality on the first PK col (prefix is empty → no narrowing).
//   - Equality on every PK col (full equality path handles it, and
//     composite-prefix would degenerate into a duplicate).
func (c *EmbeddedConnection) tryPKCompositePrefixFromWhere(
	ctx context.Context,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
) (pkCompositePrefix, bool) {
	if whereExpr == nil {
		return pkCompositePrefix{}, false
	}
	pkCols := extractPKUserFields(rt.PrimaryKey)
	if len(pkCols) < 2 {
		return pkCompositePrefix{}, false
	}
	leaves, ok := flattenAndPredicates(whereExpr.Expression())
	if !ok {
		return pkCompositePrefix{}, false
	}
	equalities := make(map[string]any, len(pkCols))
	for _, leaf := range leaves {
		op, col, val, ok := extractColOpLiteral(ctx, c, leaf)
		if !ok || op != "=" {
			continue
		}
		equalities[strings.ToUpper(col)] = val
	}
	// Collect the longest leading prefix of equated PK cols.
	prefixVals := make([]any, 0, len(pkCols))
	for _, col := range pkCols {
		val, has := equalities[strings.ToUpper(col)]
		if !has {
			break
		}
		fd := rt.Descriptor.Fields().ByName(protoreflect.Name(col))
		if fd == nil || !functions.LiteralMatchesPKKind(val, fd.Kind()) {
			return pkCompositePrefix{}, false
		}
		prefixVals = append(prefixVals, val)
	}
	if len(prefixVals) == 0 {
		return pkCompositePrefix{}, false
	}
	if len(prefixVals) == len(pkCols) {
		// Full equality — the equality path already caught this. Bail
		// to keep this branch non-overlapping with tryPKEquality.
		return pkCompositePrefix{}, false
	}
	return pkCompositePrefix{prefixVals: prefixVals}, true
}
