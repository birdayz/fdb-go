// Package logical holds the Phase 3 (TODO.md §"Phase 3 — Semantic
// analysis") logical-operator hierarchy. A LogicalOperator describes
// WHAT a SQL query is doing — scan this table, filter on that
// predicate, project these columns, sort by those columns — without
// committing to HOW. Semantic analysis (porting of Java's
// `SemanticAnalyzer`) produces a LogicalOperator tree from the parse
// tree. Phase 4 Cascades consumes the tree and emits physical plans;
// until Cascades lands, the naive Generator can consume LogicalOperator
// directly through a thin translator.
//
// Naming mirrors Java's
// `fdb-relational-core/recordlayer/query/LogicalOperator.java` +
// siblings, trimmed to the core operator set we actually need for
// the current yamsql corpus:
//
//   - LogicalScan        — `FROM tbl` (single-table read)
//   - LogicalFilter      — `WHERE pred`
//   - LogicalProject     — `SELECT a, b, expr AS n`
//   - LogicalSort        — `ORDER BY …`
//   - LogicalLimit       — `LIMIT n OFFSET m`
//   - LogicalAggregate   — `GROUP BY … + agg(…)`
//   - LogicalJoin        — `INNER / LEFT / RIGHT JOIN`
//   - LogicalUnion       — `UNION [ALL]`
//   - LogicalInsert      — `INSERT INTO tbl VALUES / INSERT SELECT`
//   - LogicalUpdate      — `UPDATE tbl SET col = expr WHERE …`
//   - LogicalDelete      — `DELETE FROM tbl WHERE …`
//   - LogicalDDL         — `CREATE / DROP` passthrough (no tree shape)
//   - LogicalCTE         — `WITH name AS (…) SELECT …`
//     (non-recursive + recursive)
//
// **Phase 3 scope:** this package is the TARGET SHAPE only. The
// semantic analyzer that translates parse tree → LogicalOperator
// and the translator that turns LogicalOperator → executable Plan
// are separate Phase 3 deliverables. Committing to the shape here
// lets them land incrementally without churning all of the pkg/
// relational tree at once.
//
// **Java alignment.** Most operators map 1:1 to a Java counterpart:
//
//	LogicalFilter    ↔ LogicalFilter / QueryPredicate-carrying child
//	LogicalProject   ↔ LogicalProjectionExpression
//	LogicalSort      ↔ LogicalSortExpression
//	LogicalAggregate ↔ GroupByExpression
//	LogicalUnion     ↔ LogicalUnionExpression
//	LogicalInsert    ↔ InsertExpression
//	LogicalUpdate    ↔ UpdateExpression
//	LogicalDelete    ↔ DeleteExpression
//
// Two deliberate divergences:
//
//  1. `LogicalScan` does not exist in Java-Cascades; Java represents
//     a FROM-source as a Cascades `FullUnorderedScanExpression`
//     wrapped by a `Quantifier`. RFC-022 argues that Phase 3 should
//     own a pure logical-plan representation distinct from Cascades;
//     LogicalScan is the scan-stand-in at the logical level.
//  2. `LogicalJoin` does not exist in Java-Cascades as a discrete
//     type; Java encodes joins via a `SelectExpression` binding
//     multiple `Quantifier`s. Same rationale: keeping join as an
//     explicit logical operator separates Phase 3 tree-building
//     from Phase 4 Cascades translation.
//
// Both divergences are documented in RFC-023 / TODO Phase 3+4.
//
// Explicit predicate / expression representation is deferred. For
// now LogicalFilter / LogicalProject etc. carry parse-tree handles
// (antlr IExpressionContext). RFC-021 Phase 2 replaces those with
// `Value` nodes from the Cascades Value hierarchy. RFC-023
// committed to non-generic interfaces + `any`; the seed lives in
// `pkg/recordlayer/query/plan/cascades/`. As that package grows
// this one will migrate its text-handle predicates to real Values.
package logical

// LogicalOperator is the root interface every logical operator
// satisfies. A LogicalOperator exposes its children (tree structure)
// and can render an indented text explanation of the subtree.
//
// Operators are value-types (small, immutable once constructed).
// They are NOT identity-comparable — two structurally-identical
// Filter nodes should compare equal under a structural walker
// (implemented on top of this interface, not part of it).
type LogicalOperator interface {
	// Children returns the immediate child operators. Returning an
	// empty slice (not nil) for leaf nodes keeps caller code free
	// of nil checks.
	Children() []LogicalOperator

	// Explain returns an indented textual rendering of this node
	// and its subtree. The indent argument is prefixed to this
	// node's first line; children receive indent + "  ".
	//
	// This is the stable surface Plan.Explain() exposes to
	// frontend callers. Cascades physical plans will Explain()
	// through their own impl; logical plans use this.
	Explain(indent string) string
}
