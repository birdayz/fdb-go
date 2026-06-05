package values

import (
	"sort"
	"strings"
)

// AnchoredJoinLeg is one source leg of a source-anchored join result (RFC-077): a
// quantifier alias and the columns its result row carries (name + type).
type AnchoredJoinLeg struct {
	Alias   CorrelationIdentifier
	Columns []Field
}

// NewAnchoredJoinRecord builds the source-anchored join result value (RFC-077): a
// RecordConstructorValue whose fields are FieldValue(QuantifiedObjectValue(legAlias), col)
// — one per column of each leg. This replaces the retired opaque, name-keyed merge and
// its after-the-fact field re-anchoring: every projected field names its
// source quantifier directly, exactly as Java's RecordConstructorValue of
// FieldValue(QOV(leg), col) does.
//
// Naming preserves the retired opaque merge's bare+qualified key set so name-based resolution
// (composeFieldOverConstructor: field(RC, name) → the leg FieldValue) keeps working for
// EVERY reference the SARG/derivation can pull up — exactly the keys the executor still
// builds physically (the cursor's mergeRows writes both the bare and the ALIAS.COL form):
//   - EVERY column gets a qualified ALIAS.COL field (upper-cased, matching the merge's ToUpper
//     qualification), so a qualified reference (e.g. A.NAME) ALWAYS resolves — including to a
//     column whose bare name happens to be unique;
//   - EVERY column also gets a bare field, LAST-LEG-WINS on a cross-leg collision — exactly the
//     retired opaque merge's runtime, which wrote every leg's keys bare (later legs overwrite earlier
//     ones for a shared name). A RecordConstructorValue cannot hold two fields of the same name,
//     so for a duplicated bare name only the LAST leg's column gets the bare field (the earlier
//     legs are still reachable by their qualified ALIAS.COL). Emitting bare-only-when-unique
//     instead (an earlier cut) dropped the bare key for duplicated columns, which broke 3+-way
//     joins with 0 rows: by INVARIANT (sourceAlias(LogicalJoin)=sourceAlias(o.Right),
//     cascades_translator.go), a quantifier OVER an inner join is aliased to its right leg, so a
//     qualified predicate FieldValue(QOV(rightLeg), COL) reads the join's merged row by the bare
//     COL key. last-leg-wins = the right leg = QOV(rightLeg)'s alias, so the bare key resolves to
//     exactly the source the predicate means — by construction, not coincidence.
//
// The field VALUE always carries the original (non-upper-cased) column name so the leg's
// QuantifiedObjectValue field access matches the source row's key.
//
// ALREADY-QUALIFIED (DOTTED) leg columns — NESTED joins. A join leg's exposed
// columns are themselves anchored-RC field names: an inner join (A⋈B) exposes
// A.ID, B.ID, etc. When such a DOTTED name is a column of a parent leg, it
// propagates VERBATIM — the field name stays "A.ID" (NOT re-qualified to
// "PARENTLEG.A.ID") and the value is FieldValue(QOV(parentLeg), "A.ID"). This
// mirrors the executor's "preserve already-qualified keys
// verbatim, never re-prefix" (mergeRows): each table contributes a DISTINCT prefix, so a
// dotted key never collides across legs and reaches the right source via the
// parent leg's merged row. A dotted column gets NO extra bare/qualified form
// (it is already the resolvable key), matching the merge.
func NewAnchoredJoinRecord(legs []AnchoredJoinLeg) *RecordConstructorValue {
	// For each BARE (non-dotted) column name, record the LAST leg that carries it —
	// the bare field is emitted only there (last-leg-wins, matching the opaque
	// merge's Evaluate, and avoiding a duplicate RecordConstructorValue field name).
	// Dotted names are already-qualified keys from a nested-join leg and get no bare
	// form.
	lastLeg := make(map[string]int)
	for li, leg := range legs {
		for _, c := range leg.Columns {
			if !strings.Contains(c.Name, ".") {
				lastLeg[strings.ToUpper(c.Name)] = li
			}
		}
	}
	var fields []RecordConstructorField
	for li, leg := range legs {
		qov := NewQuantifiedObjectValue(leg.Alias)
		for _, c := range leg.Columns {
			if strings.Contains(c.Name, ".") {
				// Already-qualified (dotted) column from a nested-join leg —
				// propagate verbatim, no re-qualification, no bare form.
				fields = append(fields, RecordConstructorField{
					Name:  strings.ToUpper(c.Name),
					Value: NewFieldValue(qov, c.Name, c.FieldType),
				})
				continue
			}
			// Qualified ALIAS.COL field — always present, always unambiguous.
			fields = append(fields, RecordConstructorField{
				Name:  strings.ToUpper(leg.Alias.Name()) + "." + strings.ToUpper(c.Name),
				Value: NewFieldValue(qov, c.Name, c.FieldType),
			})
			// Bare field — emitted at the LAST leg carrying this bare name
			// (last-leg-wins), exactly mirroring the executor's mergeRows.
			if lastLeg[strings.ToUpper(c.Name)] == li {
				fields = append(fields, RecordConstructorField{
					Name:  strings.ToUpper(c.Name),
					Value: NewFieldValue(qov, c.Name, c.FieldType),
				})
			}
		}
	}
	rc := NewRecordConstructorValue(fields...)
	rc.AnchoredJoin = true
	return rc
}

// NewScalarSubqueryAnchoredRecord builds the source-anchored result value for a
// correlated-scalar-subquery join seed (RFC-077 7.6), replacing the retired opaque
// merge seed. The outer leg is anchored exactly
// as a binary join leg (bare + qualified + dotted-verbatim, via NewAnchoredJoinRecord),
// so the outer projections resolve both bare and qualified. The inner leg is the
// scalar subquery's SINGLE exposed value, anchored with one field:
//
//   - Name: <innerAlias>.<scalarColKey> (upper-cased) — EXACTLY the field name
//     replaceScalarSubqueryRef reads (it qualifies the scalar reference under the
//     inner quantifier's alias), so composeFieldOverConstructor resolves it by name;
//   - Value: FieldValue(QOV(innerAlias), scalarColKey) — reads the inner row's
//     scalar by the key the inner quantifier's row carries it under.
//
// This re-qualifies scalarColKey under innerAlias even when scalarColKey is itself
// DOTTED (a non-aggregate subquery keeps its source qualifier, e.g. "C.NAME"),
// which NewAnchoredJoinRecord cannot do (it propagates dotted leg columns verbatim).
// The inner field has NO bare form: the projection always reads the scalar via the
// qualified name, and the executor's runtime mergeRows likewise only ever exposes
// the inner scalar prefixed under innerAlias — so a bare inner field would have no
// consumer and could spuriously shadow an outer column of the same bare name.
func NewScalarSubqueryAnchoredRecord(outer AnchoredJoinLeg, innerAlias CorrelationIdentifier, scalarColKey string) *RecordConstructorValue {
	base := NewAnchoredJoinRecord([]AnchoredJoinLeg{outer})
	fields := append([]RecordConstructorField(nil), base.Fields...)
	innerQOV := NewQuantifiedObjectValue(innerAlias)
	fields = append(fields, RecordConstructorField{
		Name:  strings.ToUpper(innerAlias.Name()) + "." + strings.ToUpper(scalarColKey),
		Value: NewFieldValue(innerQOV, scalarColKey, UnknownType),
	})
	rc := NewRecordConstructorValue(fields...)
	rc.AnchoredJoin = true
	return rc
}

// anchoredColumnsByQuantifier groups a parent source-anchored join record's
// DOTTED fields by the QUANTIFIER each field's value is anchored to (RFC-077 7.6
// re-enumeration). Each parent field is FieldValue(QOV(q), "<dottedKey>") (or a
// nested FieldValue chain bottoming out in a QOV); this returns, per anchoring
// quantifier alias, the list of its source-table-qualified column NAMES (the
// dotted field names — "T1.ID", "T2.NEXT_ID", …) the parent exposes for that
// quantifier. It walks the field VALUE for the anchor (not the field NAME),
// because a column carried up through a merge quantifier $m keeps its source-table
// field NAME ("T2.ID") while its anchor QOV is $m — name-prefix grouping would
// mis-attribute it to table T2.
//
// The field value is SIMPLIFIED first: a flattened-nested-join seed anchors a
// buried leg's columns to a SUBSTITUTED inner anchored RC (SelectMergeRule's
// TranslationMap), so the raw value is field(innerRC, "O.X"), NOT a bare
// FieldValue(QOV(O), …). composeFieldOverConstructor folds field(innerRC, "O.X")
// → FieldValue(QOV(O), "X"), exposing the real per-quantifier anchor — without
// this, every buried table groups under nothing and the re-enumeration cannot
// resolve the leg's columns (NewReEnumerationAnchoredRecord returns nil). Only DOTTED parent fields are collected (the
// source-accurate forms); BARE fields are the last-leg-wins resolution-convenience
// duplicates NewAnchoredJoinRecord re-derives. Returns nil for a non-anchored
// input.
func anchoredColumnsByQuantifier(parent *RecordConstructorValue) map[CorrelationIdentifier][]Field {
	if parent == nil || !parent.AnchoredJoin {
		return nil
	}
	out := make(map[CorrelationIdentifier][]Field)
	seen := make(map[CorrelationIdentifier]map[string]struct{})
	for _, f := range parent.Fields {
		if !strings.Contains(f.Name, ".") {
			continue
		}
		anchor, ok := leftmostQOV(SimplifyValue(f.Value))
		if !ok {
			continue
		}
		if seen[anchor] == nil {
			seen[anchor] = make(map[string]struct{})
		}
		if _, dup := seen[anchor][f.Name]; dup {
			continue
		}
		seen[anchor][f.Name] = struct{}{}
		out[anchor] = append(out[anchor], Field{Name: f.Name, FieldType: f.Value.Type(), Ordinal: len(out[anchor])})
	}
	// Canonical per-quantifier column order so the re-enumeration RC is identical
	// regardless of the parent's field order (bipartition-independent interning).
	for q := range out {
		cols := out[q]
		sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })
	}
	return out
}

// leftmostQOV descends the leftmost FieldValue chain of v and returns the
// correlation of the QuantifiedObjectValue it bottoms out in. Mirrors
// anchoredJoinFirstLeg's descent: an anchored RC field value is
// FieldValue(QOV(leg), col) — possibly nested when a leg is itself an
// unsimplified join.
func leftmostQOV(v Value) (CorrelationIdentifier, bool) {
	for {
		switch x := v.(type) {
		case *QuantifiedObjectValue:
			return x.Correlation, true
		case *FieldValue:
			if x.Child == nil {
				return CorrelationIdentifier{}, false
			}
			v = x.Child
		default:
			return CorrelationIdentifier{}, false
		}
	}
}

// ReEnumerationLeg names one leg of a re-enumerated merge level (RFC-077 7.6): the
// quantifier the leg's columns are anchored to (Alias), and the parent-quantifier
// aliases whose columns flow into this leg (Sources). For a leg that PASSES
// THROUGH a quantifier the parent already binds (an original table, or a merge
// quantifier from a prior level), Sources is the singleton {Alias} — its columns
// are read straight from the parent. For a NEWLY-CREATED merge quantifier ($m)
// that collapses ≥2 lower quantifiers, Alias is the new merge alias and Sources is
// every collapsed quantifier — $m's row carries the UNION of their columns under
// their source-table-qualified (dotted) names.
type ReEnumerationLeg struct {
	Alias   CorrelationIdentifier
	Sources []CorrelationIdentifier
}

// reEnumColumn is one source-table column a re-enumeration leg flows: its
// SOURCE-TABLE alias (the table the column belongs to), the BARE column name, and
// the KEY the leg quantifier's row carries it under (bare COL for a pass-through
// table leg, dotted SRC.COL for a merge quantifier whose row preserves dotted keys).
type reEnumColumn struct {
	srcTable  string
	bareCol   string
	rowKey    string
	fieldType Type
}

// NewReEnumerationAnchoredRecord builds the source-anchored result value for a
// PartitionSelectRule re-enumeration level (RFC-077 7.6), replacing the retired
// opaque merge. Each leg's columns are read from the parent
// anchored RC (grouped by anchoring quantifier) and re-anchored to the leg's
// quantifier. It emits EXACTLY the retired opaque merge's bare+qualified key set so name
// resolution is preserved (Graefe condition 2):
//
//   - a SOURCE-TABLE-qualified field SRC.COL for EVERY column, anchored to
//     QOV(legAlias). For a pass-through original-table leg the row carries the
//     column BARE (FieldValue(QOV(table), "COL")); for a merge quantifier ($m,
//     collapsing ≥2 tables) the row preserves the DOTTED key
//     (FieldValue(QOV($m), "SRC.COL")) — mergeRows keeps dotted keys verbatim,
//     runtime untouched;
//   - a BARE field COL, LAST-LEG-WINS on a cross-leg collision, anchored to
//     QOV(legAlias) reading the BARE key — both a leaf table row and a merge row
//     carry bare keys (mergeRows writes them), so an UNQUALIFIED projection of a
//     buried column resolves, exactly as the executor's mergeRows still writes bare
//     keys (the buried-column-bare-projection 0-row regression otherwise).
//
// legs must already be in the rule's canonical (alias-name-sorted) order so two
// bipartitions producing the same leg-set intern to one Reference (the anchored
// RC's structural identity is order-sensitive). Returns nil if the parent is not
// anchored or a leg's source columns are unavailable; in practice every parent
// reaching the rule is anchored and every leg's source is a parent quantifier, so
// resolution always succeeds — the rule's callers panic on a nil (fail-loud on a
// proven-unreachable invariant rather than store a nil result value silently).
func NewReEnumerationAnchoredRecord(parent *RecordConstructorValue, legs []ReEnumerationLeg) *RecordConstructorValue {
	byQ := anchoredColumnsByQuantifier(parent)
	if byQ == nil {
		return nil
	}
	// Resolve each leg's columns; collect (legAlias → its reEnumColumns) in leg order.
	type legCols struct {
		alias CorrelationIdentifier
		cols  []reEnumColumn
	}
	resolved := make([]legCols, 0, len(legs))
	for _, leg := range legs {
		passThroughTable := len(leg.Sources) == 1 && leg.Sources[0] == leg.Alias &&
			allPrefixedBy(byQ[leg.Alias], leg.Alias.Name())
		// Canonical source order: a merge leg collapsing the SAME tables reached
		// from different bipartitions must produce the SAME column sequence so the
		// anchored RCs intern (structural identity is order-sensitive).
		sources := append([]CorrelationIdentifier(nil), leg.Sources...)
		sort.Slice(sources, func(i, j int) bool { return sources[i].Name() < sources[j].Name() })
		var cols []reEnumColumn
		for _, src := range sources {
			srcCols, ok := byQ[src]
			if !ok {
				return nil
			}
			for _, c := range srcCols {
				// c.Name is the parent's dotted SRC.COL (anchoredColumnsByQuantifier
				// only collects dotted forms). Split into source table + bare column.
				srcTable, bareCol := c.Name, c.Name
				if dot := strings.IndexByte(c.Name, '.'); dot >= 0 {
					srcTable, bareCol = c.Name[:dot], c.Name[dot+1:]
				}
				rowKey := bareCol // pass-through table: leg row carries the bare key
				if !passThroughTable {
					rowKey = c.Name // merge quantifier: leg row carries the dotted key
				}
				cols = append(cols, reEnumColumn{srcTable: srcTable, bareCol: bareCol, rowKey: rowKey, fieldType: c.FieldType})
			}
		}
		resolved = append(resolved, legCols{alias: leg.Alias, cols: cols})
	}

	// Last OCCURRENCE (leg index, column index within the leg) of each bare column
	// name across ALL legs. The bare field is emitted exactly once, at that
	// occurrence — last-occurrence-wins, matching the retired merge's last-table-wins
	// Evaluate. Keying on the leg index alone (an earlier cut) emitted the bare field
	// once PER duplicate within a single merge leg: a merge quantifier collapsing two
	// tables that share a bare name (A.ID + B.ID) produced two "ID" fields, which
	// NewRecordConstructorValue renamed to ID + ID_2 — a spurious key the merge never
	// produced.
	type bareOcc struct{ li, ci int }
	lastBareOcc := make(map[string]bareOcc)
	for li, lc := range resolved {
		for ci, c := range lc.cols {
			lastBareOcc[strings.ToUpper(c.bareCol)] = bareOcc{li, ci}
		}
	}

	var fields []RecordConstructorField
	for li, lc := range resolved {
		qov := NewQuantifiedObjectValue(lc.alias)
		for ci, c := range lc.cols {
			// Source-table-qualified field — always present, always unambiguous.
			fields = append(fields, RecordConstructorField{
				Name:  strings.ToUpper(c.srcTable) + "." + strings.ToUpper(c.bareCol),
				Value: NewFieldValue(qov, c.rowKey, c.fieldType),
			})
			// Bare field — emitted exactly once, at the LAST occurrence of this bare
			// name across all legs (last-occurrence-wins), reading the leg row's BARE key.
			if o := lastBareOcc[strings.ToUpper(c.bareCol)]; o.li == li && o.ci == ci {
				fields = append(fields, RecordConstructorField{
					Name:  strings.ToUpper(c.bareCol),
					Value: NewFieldValue(qov, c.bareCol, c.fieldType),
				})
			}
		}
	}
	rc := NewRecordConstructorValue(fields...)
	rc.AnchoredJoin = true
	return rc
}

// allPrefixedBy reports whether every column's dotted name is prefixed by
// "<alias>." (case-insensitive) — i.e. the columns are an original single table's
// own columns, not a merge carrying multiple tables.
func allPrefixedBy(cols []Field, alias string) bool {
	if len(cols) == 0 {
		return false
	}
	pfx := strings.ToUpper(alias) + "."
	for _, c := range cols {
		if !strings.HasPrefix(strings.ToUpper(c.Name), pfx) {
			return false
		}
	}
	return true
}
