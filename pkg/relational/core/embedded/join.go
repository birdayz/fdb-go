package embedded

import (
	"context"
	"database/sql/driver"
	"sort"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
)

// Nested-loop JOIN executor. Handles INNER / LEFT / RIGHT joins on
// AND-chained `a.col = b.col` predicates, feeding the result through
// evalPredicateOnMap for the residual WHERE and
// resolveOuterColumn / outerScopesContainQualifier for correlated
// subqueries. The implementation is intentionally simple — pair-wise
// scans, sorted at the end — because the query shapes the yamsql
// suite exercises are small. Destined for plan/physical/
// nested_loop_join.go per RFC 021 Phase 1c.

func (c *EmbeddedConnection) execSelectJoin(ctx context.Context, sq *selectQuery) (driver.Rows, error) {
	// WHERE bare-paren rejection — fire BEFORE the transaction starts
	// so we don't waste a cross-join scan only to reject. The check is
	// row-independent (purely structural over the parse tree).
	if sq.whereExpr != nil {
		if err := rejectTopLevelParenthesizedWhere(sq.whereExpr.Expression()); err != nil {
			return nil, err
		}
	}

	var cols []string
	var colTypes []string
	var data [][]driver.Value

	_, runErr := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		cols = nil
		colTypes = nil
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()
		// Expand any `<qualifier>.*` slots against the FROM sources now
		// that md is available. No-op when the SELECT list doesn't mix
		// a qualifier-star with named columns.
		if expandErr := c.expandStarSlots(md, sq); expandErr != nil {
			return nil, expandErr
		}
		ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := recordlayer.NewStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		// Scan left table.
		leftRows, leftErr := c.scanTableToMaps(ctx, store, sq.tableName, sq.tableAlias)
		if leftErr != nil {
			return nil, leftErr
		}

		// Bare column names present in more than one FROM-clause source.
		// After every row merge below we poison these bare slots with
		// the ambiguousColumnMarker sentinel so unqualified references
		// surface 42702. Qualified (alias.col) slots remain usable.
		ambiguousBare := c.computeAmbiguousBareColumns(md, sq)

		// Set of valid qualifier aliases (left source + every join source).
		// Used by the SELECT projection to surface 42F01 when a qualified
		// reference names a qualifier that doesn't match any FROM-clause
		// source — the pre-fix behavior silently fell back to the bare
		// column lookup, picking whichever side of the JOIN wrote it last.
		// Also installed on EmbeddedConnection for the lifetime of this
		// execSelectJoin call so evalExprAtomOnMap (WHERE/ON/SELECT
		// expressions) applies the same check — symmetric with the
		// projection path.
		validQualifiers := make(map[string]bool)
		leftQual := sq.tableAlias
		if leftQual == "" {
			leftQual = sq.tableName
		}
		validQualifiers[strings.ToUpper(leftQual)] = true
		for _, jc := range sq.joins {
			a := jc.alias
			if a == "" {
				a = jc.tableName
			}
			validQualifiers[strings.ToUpper(a)] = true
		}
		defer c.pushValidQualifiersScope(validQualifiers)()
		// (leftRows itself is not poisoned here: execSelectJoin only
		// runs when sq.joins is non-empty — see the guard in
		// execSelectQueryFull — so every emitted row flows through a
		// combined/null-pad merge below and gets poisoned there. The
		// no-joins degenerate case goes through the single-table path,
		// which has its own scope and no merging ambiguity.)

		// Track the sources (tableName + alias) merged into `joined` so
		// far. RIGHT JOIN NULL-padding uses this to derive left-side
		// column keys from metadata rather than sampling a runtime row
		// — necessary when the left table is empty (no runtime row
		// exists, so the qualified `a.id` key wouldn't be known without
		// metadata).
		leftSources := []struct{ tableName, alias string }{{sq.tableName, sq.tableAlias}}

		// Scan each joined table; apply nested-loop join.
		joined := leftRows
		for _, jc := range sq.joins {
			rightRows, rightErr := c.scanTableToMaps(ctx, store, jc.tableName, jc.alias)
			if rightErr != nil {
				return nil, rightErr
			}
			var next []map[string]driver.Value
			// For RIGHT JOIN, record during the matching pass which right
			// rows had at least one match so the unmatched-right step
			// doesn't have to re-evaluate the ON predicate a second time.
			var matchedRight []bool
			if jc.joinType == "RIGHT" {
				matchedRight = make([]bool, len(rightRows))
			}
			for _, left := range joined {
				matched := false
				for ri, right := range rightRows {
					// Merge rows into combined map.
					combined := make(map[string]driver.Value, len(left)+len(right))
					for k, v := range left {
						combined[k] = v
					}
					for k, v := range right {
						combined[k] = v
					}
					// Poison bare columns defined on >1 source so
					// unqualified refs error 42702 instead of silently
					// picking the right-hand side (last-write-wins).
					poisonAmbiguousBareCols(combined, ambiguousBare)
					// Evaluate ON condition.
					if jc.onExpr != nil {
						ok, onErr := evalPredicateOnMapExpr(ctx, c, combined, jc.onExpr)
						if onErr != nil {
							return nil, onErr
						}
						if !ok {
							continue
						}
					}
					matched = true
					if matchedRight != nil {
						matchedRight[ri] = true
					}
					next = append(next, combined)
				}
				// LEFT JOIN: emit left row with NULLs if no right match.
				if jc.joinType == "LEFT" && !matched {
					// Build null right side. Populate right-side column keys
					// (both `alias.col` and bare `col`) with NULL, derived
					// from metadata, so downstream evaluators can find
					// `b.label` / `label` and see NULL instead of erroring
					// with 42703. Pre-fix, WHERE / HAVING / aggregate paths
					// on unmatched-left rows failed because those keys were
					// simply absent from the map. Skip any key that also
					// exists on the left (e.g. a shared `id` column) —
					// leaving the left value intact matches the RIGHT JOIN
					// mirror logic.
					rightKeys := c.collectLeftJoinKeys(md, []struct{ tableName, alias string }{{jc.tableName, jc.alias}})
					nullRight := make(map[string]driver.Value, len(left)+len(rightKeys))
					for k, v := range left {
						nullRight[k] = v
					}
					for _, k := range rightKeys {
						if _, exists := left[k]; exists {
							continue
						}
						nullRight[k] = nil
					}
					// LEFT JOIN unmatched row inherits ambiguous bare
					// slots from `left`; re-poison so the unqualified
					// ref still errors at WHERE/SELECT evaluation.
					poisonAmbiguousBareCols(nullRight, ambiguousBare)
					next = append(next, nullRight)
				}
			}
			// RIGHT JOIN: also emit right rows that had no left match (null left side).
			if jc.joinType == "RIGHT" {
				// Derive left-side column keys from metadata (record
				// type descriptor, or CTE column list) for each source
				// that has been merged into `joined` so far. Using
				// metadata rather than sampling a runtime row means the
				// NULL-padding works even when the left side is empty
				// — an unmatched right row still has `a.id` explicitly
				// set to NULL, so `SELECT a.id` doesn't fall through
				// to the unqualified `id` populated from the right.
				leftKeys := c.collectLeftJoinKeys(md, leftSources)
				for ri, right := range rightRows {
					if !matchedRight[ri] {
						combined := make(map[string]driver.Value, len(right)+len(leftKeys))
						for _, k := range leftKeys {
							if _, exists := right[k]; !exists {
								combined[k] = nil
							}
						}
						for k, v := range right {
							combined[k] = v
						}
						// RIGHT JOIN unmatched row: bare slot carries
						// right's value; poison to keep ambiguous refs
						// erroring symmetrically with LEFT JOIN.
						poisonAmbiguousBareCols(combined, ambiguousBare)
						next = append(next, combined)
					}
				}
			}
			joined = next
			leftSources = append(leftSources, struct{ tableName, alias string }{jc.tableName, jc.alias})
		}

		// Apply WHERE filter using map-based evaluation.
		// (rejectTopLevelParenthesizedWhere fires once at execSelectJoin
		// entry, before the transaction — see the function preamble.)
		var filtered []map[string]driver.Value
		for _, row := range joined {
			if sq.whereExpr == nil {
				filtered = append(filtered, row)
				continue
			}
			ok, wErr := evalPredicateOnMapExpr(ctx, c, row, sq.whereExpr.Expression())
			if wErr != nil {
				return nil, wErr
			}
			if ok {
				filtered = append(filtered, row)
			}
		}

		// GROUP BY + aggregate on map rows (for JOIN queries).
		// Aggregated results fall through to ORDER BY/LIMIT/OFFSET below;
		// the normal column-selection and row-building blocks are skipped.
		isAggregate := sq.countStar || len(sq.aggCols) > 0
		if isAggregate {
			aggCols, aggColTypes, aggData, aggErr := c.aggregateMapRows(ctx, sq, filtered)
			if aggErr != nil {
				return nil, aggErr
			}
			cols = aggCols
			colTypes = aggColTypes
			data = aggData
		} else {
			// Determine output columns.
			// For SELECT *, collect all unique unqualified column names in order.
			// For SELECT <qualifier>.*, restrict to the aliased source's columns.
			var qualifierKey string // non-empty when qualified-star; row lookups use "qualifier.col"
			// colSourceAlias maps the column name (index-parallel with `cols`
			// via a parallel slice below) to the source alias that provided
			// it. Used by the SELECT * projection to look up via
			// `alias.col` when the bare key is poisoned by ambiguity —
			// keeping the current SELECT * behavior (first source wins)
			// instead of erroring 42702 for a SELECT that isn't actually
			// referencing the bare name.
			var starColAliases []string
			if sq.projCols == nil {
				if sq.projQualifier != "" {
					qCols, qAlias, qErr := c.resolveQualifierColumns(md, sq, sq.projQualifier)
					if qErr != nil {
						return nil, qErr
					}
					cols = qCols
					qualifierKey = qAlias
				} else {
					seen := make(map[string]bool)
					// Order: left table columns first, then join table columns.
					leftAliasForStar := sq.tableAlias
					if leftAliasForStar == "" {
						leftAliasForStar = sq.tableName
					}
					// collectCols walks a source's column list (record type
					// descriptor or CTE) and appends unseen names + their
					// qualifier to cols/starColAliases. Consolidating the
					// two loops so CTE sources (md.GetRecordType nil) don't
					// silently drop out of SELECT * — the reviewer-flagged
					// CTE starColAliases gap from dayshift-40.
					collectCols := func(tableName, alias string) {
						var names []string
						if c.ctes != nil {
							if cte, ok := c.ctes[strings.ToUpper(tableName)]; ok {
								names = cte.cols
							}
						}
						if names == nil {
							if rt := md.GetRecordType(tableName); rt != nil {
								fields := rt.Descriptor.Fields()
								names = make([]string, 0, fields.Len())
								for i := 0; i < fields.Len(); i++ {
									names = append(names, string(fields.Get(i).Name()))
								}
							}
						}
						for _, name := range names {
							if !seen[name] {
								cols = append(cols, name)
								starColAliases = append(starColAliases, alias)
								seen[name] = true
							}
						}
					}
					collectCols(sq.tableName, leftAliasForStar)
					for _, jc := range sq.joins {
						jAlias := jc.alias
						if jAlias == "" {
							jAlias = jc.tableName
						}
						collectCols(jc.tableName, jAlias)
					}
				}
			} else {
				cols = make([]string, len(sq.projCols))
				colTypes = make([]string, len(sq.projCols))
				// Build a unified bare-name → JDBC-type map across all
				// JOIN sources (left table + every joined CTE / table)
				// so projected columns can carry their result-set type
				// regardless of which source they came from. Same-name
				// across sources first-source-wins, mirroring how the
				// rest of the JOIN executor resolves bare-name lookups.
				typeBySource := make(map[string]string)
				addTypes := func(tableName string) {
					if c.ctes != nil {
						if cte, ok := c.ctes[strings.ToUpper(tableName)]; ok {
							for ci, name := range cte.cols {
								if _, dup := typeBySource[name]; dup {
									continue
								}
								t := ""
								if ci < len(cte.colTypes) {
									t = cte.colTypes[ci]
								}
								typeBySource[name] = t
							}
							return
						}
					}
					if rt := md.GetRecordType(tableName); rt != nil {
						fields := rt.Descriptor.Fields()
						for fi := 0; fi < fields.Len(); fi++ {
							fd := fields.Get(fi)
							name := string(fd.Name())
							if _, dup := typeBySource[name]; dup {
								continue
							}
							typeBySource[name] = jdbcTypeNameForFD(fd)
						}
					}
				}
				addTypes(sq.tableName)
				for _, jc := range sq.joins {
					addTypes(jc.tableName)
				}
				for i, projCol := range sq.projCols {
					out := projCol
					if i < len(sq.projAliases) && sq.projAliases[i] != "" {
						out = sq.projAliases[i]
					}
					cols[i] = out
					if i < len(sq.projExprs) && sq.projExprs[i] != nil {
						// Computed expression — JOIN path doesn't have
						// a single msgDesc to walk against. Leave
						// colTypes[i] = "" until per-source expression
						// type-inference lands.
						continue
					}
					bare := projCol
					if dot := strings.LastIndex(projCol, "."); dot >= 0 {
						bare = projCol[dot+1:]
					}
					colTypes[i] = typeBySource[bare]
				}
			}

			// Build output rows.
			for _, row := range filtered {
				var vals []driver.Value
				if sq.projCols == nil {
					// SELECT * or SELECT <qualifier>.* — use cols order.
					// When qualifierKey is set, look up the source-qualified
					// key first so two sources with overlapping names don't
					// collide into whichever wrote the unqualified key last.
					// Invariant: scanTableToMaps always writes both
					// `alias.col` and `col`, so the qualified lookup
					// succeeds on real rows — the unqualified fallback is
					// a safety net for any future map producer that
					// doesn't populate the qualified form (none today).
					vals = make([]driver.Value, len(cols))
					for i, col := range cols {
						if qualifierKey != "" {
							if v, ok := row[qualifierKey+"."+col]; ok {
								vals[i] = v
								continue
							}
						}
						v := row[col]
						// SELECT * dedupes ambiguous bare names by first
						// source (see cols build above). Fall through the
						// ambiguous-bare sentinel to the qualified lookup
						// so SELECT * on a.(id,name) + b.(id,label) still
						// returns three columns instead of erroring.
						// Referencing the bare name explicitly
						// (`SELECT id FROM a,b`) goes through the projCols
						// branch below and keeps the 42702 behavior.
						if _, isAmb := v.(ambiguousColumnMarker); isAmb && i < len(starColAliases) && starColAliases[i] != "" {
							if qv, ok := row[starColAliases[i]+"."+col]; ok {
								v = qv
							}
						}
						vals[i] = v
					}
				} else {
					vals = make([]driver.Value, len(sq.projCols))
					for i, col := range sq.projCols {
						// Try qualified first, then unqualified.
						if v, ok := row[col]; ok {
							if m, isAmb := v.(ambiguousColumnMarker); isAmb {
								return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
									"column reference %q is ambiguous", m.Col)
							}
							vals[i] = v
						} else if dot := strings.LastIndex(col, "."); dot >= 0 {
							// Qualified reference whose qualified key is NOT
							// in the row. Before falling back to the bare
							// column (which silently returned whichever source
							// wrote it last), check that the qualifier names a
							// valid FROM-clause source. If not → 42F01.
							qual := col[:dot]
							if !validQualifiers[strings.ToUpper(qual)] {
								return nil, api.NewErrorf(api.ErrCodeUndefinedTable,
									"column reference %q names unknown table/alias %q", col, qual)
							}
							// Valid qualifier but the column isn't there —
							// fall through to the bare-name lookup (matches
							// pre-fix behavior for the "safety net" case
							// documented at scanTableToMaps; both keys exist
							// on real rows so this path only fires when a
							// future map producer doesn't populate the
							// qualified form).
							v := row[col[dot+1:]]
							if m, isAmb := v.(ambiguousColumnMarker); isAmb {
								return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
									"column reference %q is ambiguous", m.Col)
							}
							vals[i] = v
						}
					}
				}
				data = append(data, vals)
			}
		}

		// Apply DISTINCT deduplication before sort — the JOIN path
		// was historically missing this, so `SELECT DISTINCT a.cust_id
		// FROM a, b WHERE a.cust_id = b.cust_id` silently returned the
		// cross-join's duplicate rows. Same rowKey encoding as the
		// non-JOIN path so distinct cross-checking matches.
		if sq.distinct && !sq.countStar && !isAggregate {
			seen := make(map[string]struct{}, len(data))
			deduped := data[:0]
			for _, row := range data {
				key := rowKey(row)
				if _, exists := seen[key]; !exists {
					seen[key] = struct{}{}
					deduped = append(deduped, row)
				}
			}
			data = deduped
		}

		// ORDER BY. For aggregate results, `filtered` and `data` diverge — the
		// colName path handles that. For non-aggregate rows, data[i] matches
		// filtered[i] in lockstep, so `ob.expr` can be evaluated against
		// filtered[i] for arbitrary-expression sort keys.
		if len(sq.orderBy) > 0 {
			colIdx := make(map[string]int, len(cols))
			for i, c := range cols {
				colIdx[c] = i
			}
			// Pre-compute sort keys for expression order-by to avoid redundant
			// evaluation inside the comparator.
			hasExpr := false
			for _, ob := range sq.orderBy {
				if ob.expr != nil {
					hasExpr = true
					break
				}
			}
			var keys [][]driver.Value
			if hasExpr {
				// Aggregation or SELECT DISTINCT shrinks rows, breaking the
				// filtered[i]↔data[i] lockstep needed to evaluate ORDER BY
				// expressions. Plain ORDER BY col / ORDER BY SUM(col) still
				// works via the colName path (columnNameFromExpr recognises
				// aggregates) — only expression-based ORDER BY is gated.
				if len(filtered) != len(data) {
					return nil, api.NewError(api.ErrCodeUnsupportedOperation,
						"ORDER BY on an arithmetic / function expression is not supported when the query also aggregates or uses SELECT DISTINCT; use a column reference or a plain aggregate (e.g. ORDER BY SUM(col))")
				}
				keys = make([][]driver.Value, len(data))
				for i := range data {
					keys[i] = make([]driver.Value, len(sq.orderBy))
					for oi, ob := range sq.orderBy {
						if ob.expr != nil {
							v, evalErr := evalExprOnMap(ctx, c, filtered[i], ob.expr)
							if evalErr != nil {
								return nil, evalErr
							}
							keys[i][oi] = v
						}
					}
				}
			}
			indexes := make([]int, len(data))
			for i := range indexes {
				indexes[i] = i
			}
			// Pre-validate ORDER BY column references for non-aggregate
			// JOIN queries (where filtered/data are in lockstep): names
			// not in colIdx must be present in the per-row map; otherwise
			// they're typos that would silently no-op the sort. Skips
			// expression-keyed items (handled by keys[]) and aggregate
			// queries (different lockstep semantics handled below).
			if !isAggregate && len(filtered) == len(data) && len(filtered) > 0 {
				for _, ob := range sq.orderBy {
					if ob.expr != nil {
						continue
					}
					if _, ok := colIdx[ob.colName]; ok {
						continue
					}
					if v, present := filtered[0][ob.colName]; present {
						if m, isAmb := v.(ambiguousColumnMarker); isAmb {
							return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
								"column reference %q is ambiguous", m.Col)
						}
						continue
					}
					// "alias.col" → strip qualifier and re-check (the
					// JOIN row map populates both forms, but only the
					// qualified form for sources that aren't the left).
					if dot := strings.LastIndex(ob.colName, "."); dot >= 0 {
						if _, present := filtered[0][ob.colName[dot+1:]]; present {
							continue
						}
					}
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
						"ORDER BY column %q not found", ob.colName)
				}
			}
			// (No Java-conformance rejection here. Initial nightshift-60
			// attempt added a "left.PK natural order" gate, but Java's
			// Cascades planner can pick which JOIN side runs as outer
			// based on cost — so `FROM Users u, Orders o WHERE u.uid =
			// o.uid ORDER BY o.oid` succeeds in Java by picking Orders
			// as the outer scan (emits in o.oid order) with per-row
			// Users PK lookups. Go's nested-loop is fixed: left source
			// is always outer. A static "left.PK only" gate rejects too
			// many queries Java accepts. Proper fix needs JOIN-side
			// reordering or routing through Cascades — tracked in
			// TODO.md as JOIN sort-site Java-conformance, gated on C2
			// QueryExecutor.)
			sort.SliceStable(indexes, func(ii, jj int) bool {
				i, j := indexes[ii], indexes[jj]
				for oi, ob := range sq.orderBy {
					var a, b driver.Value
					if ob.expr != nil && keys != nil {
						a, b = keys[i][oi], keys[j][oi]
					} else if idx, ok := colIdx[ob.colName]; ok {
						a, b = data[i][idx], data[j][idx]
					} else if !isAggregate && len(filtered) == len(data) {
						// ORDER BY a JOIN-input column not in the
						// projection. The combined map row carries both
						// the bare and alias-qualified forms; pull the
						// value directly. Only valid on the non-aggregate
						// path where filtered[i]↔data[i] is in lockstep.
						a = filtered[i][ob.colName]
						b = filtered[j][ob.colName]
						if a == nil && b == nil {
							if dot := strings.LastIndex(ob.colName, "."); dot >= 0 {
								bare := ob.colName[dot+1:]
								a = filtered[i][bare]
								b = filtered[j][bare]
							}
						}
					} else {
						continue
					}
					less, equal := orderByLess(a, b, ob)
					if !equal {
						return less
					}
				}
				return false
			})
			sorted := make([][]driver.Value, len(data))
			for nn, oldIdx := range indexes {
				sorted[nn] = data[oldIdx]
			}
			data = sorted
		}

		// LIMIT / OFFSET.
		if sq.offset > 0 && int(sq.offset) < len(data) {
			data = data[sq.offset:]
		} else if sq.offset > 0 {
			data = nil
		}
		if sq.limit >= 0 && int(sq.limit) < len(data) {
			data = data[:sq.limit]
		}
		// Drop trailing sort-only aggregate columns now that the sort
		// has consumed them. No-op when the query had no ORDER BY
		// references to hidden aggregates.
		if isAggregate {
			cols, data = stripAggregateSortOnly(sq, cols, data)
		}

		return nil, nil
	})
	if runErr != nil {
		return nil, runErr
	}
	return &staticRows{cols: cols, colTypes: colTypes, rows: data}, nil
}
