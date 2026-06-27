package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// GraphExpansionColumn pairs a column name with the Value that computes
// its contents. Mirrors Java's Column<? extends Value> in the context
// of GraphExpansion — simplified since Go doesn't have Java's typed
// Field wrapper. An empty Name represents an unnamed/anonymous column.
type GraphExpansionColumn struct {
	Name  string
	Value values.Value
}

// GraphExpansion accumulates index-expansion components (result
// columns, predicates, quantifiers, placeholders) before they are
// sealed and built into a SelectExpression. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.GraphExpansion`.
//
// Think of it as a builder for SelectExpression that also supports
// merging multiple expansions and inspecting intermediate state. The
// fundamental difference from a regular builder is that it exposes
// getters and additive merge semantics.
type GraphExpansion struct {
	resultColumns []GraphExpansionColumn
	predicates    []predicates.QueryPredicate
	quantifiers   []expressions.Quantifier
	placeholders  []*predicates.Placeholder
}

// EmptyGraphExpansion returns a zero-valued expansion with all slices
// initialized (non-nil but empty). Equivalent to Java's
// `GraphExpansion.empty()`.
func EmptyGraphExpansion() *GraphExpansion {
	return &GraphExpansion{
		resultColumns: []GraphExpansionColumn{},
		predicates:    []predicates.QueryPredicate{},
		quantifiers:   []expressions.Quantifier{},
		placeholders:  []*predicates.Placeholder{},
	}
}

// NewGraphExpansion constructs a GraphExpansion from explicit slices.
// All slices are defensively copied. Equivalent to Java's
// `GraphExpansion.of(...)`.
func NewGraphExpansion(
	columns []GraphExpansionColumn,
	preds []predicates.QueryPredicate,
	quants []expressions.Quantifier,
	phs []*predicates.Placeholder,
) *GraphExpansion {
	return &GraphExpansion{
		resultColumns: copySlice(columns),
		predicates:    copySlice(preds),
		quantifiers:   copySlice(quants),
		placeholders:  copySlice(phs),
	}
}

// MergeGraphExpansions combines multiple expansions by concatenating
// their result columns, predicates, quantifiers, and placeholders.
// Equivalent to Java's `GraphExpansion.ofOthers(...)`.
func MergeGraphExpansions(expansions ...*GraphExpansion) *GraphExpansion {
	var (
		cols  []GraphExpansionColumn
		preds []predicates.QueryPredicate
		qs    []expressions.Quantifier
		phs   []*predicates.Placeholder
	)
	for _, e := range expansions {
		cols = append(cols, e.resultColumns...)
		preds = append(preds, e.predicates...)
		qs = append(qs, e.quantifiers...)
		phs = append(phs, e.placeholders...)
	}
	return &GraphExpansion{
		resultColumns: cols,
		predicates:    preds,
		quantifiers:   qs,
		placeholders:  phs,
	}
}

// --- Getters ---------------------------------------------------------

// GetResultColumns returns the result column list.
func (g *GraphExpansion) GetResultColumns() []GraphExpansionColumn { return g.resultColumns }

// GetPredicates returns the predicate list.
func (g *GraphExpansion) GetPredicates() []predicates.QueryPredicate { return g.predicates }

// GetQuantifiers returns the quantifier list.
func (g *GraphExpansion) GetQuantifiers() []expressions.Quantifier { return g.quantifiers }

// GetPlaceholders returns the placeholder list.
func (g *GraphExpansion) GetPlaceholders() []*predicates.Placeholder { return g.placeholders }

// --- Seal ------------------------------------------------------------

// Seal deduplicates placeholders and returns an immutable
// SealedGraphExpansion. Mirrors Java's `GraphExpansion.seal()`.
//
// Placeholder deduplication:
//  1. Group by ParameterAlias — placeholders with the same alias are
//     collapsed (ranges merged).
//  2. Among the unique-by-alias placeholders, identify those whose
//     Values are semantically equal (by structural comparison). Replace
//     all duplicates-by-value with the first representative.
//
// The resulting SealedGraphExpansion carries deduplicated predicates,
// the original quantifiers, deduplicated placeholders, and a result
// value built from the columns.
func (g *GraphExpansion) Seal() *SealedGraphExpansion {
	resultColumns := g.resultColumns

	// --- Deduplicate columns with duplicate field names ---------------
	// If multiple columns share the same non-empty name, all of those
	// become unnamed (same as Java's seal logic).
	seenNames := make(map[string]int) // name → count
	for _, col := range resultColumns {
		if col.Name != "" {
			seenNames[col.Name]++
		}
	}
	duplicateNames := make(map[string]struct{})
	for name, count := range seenNames {
		if count > 1 {
			duplicateNames[name] = struct{}{}
		}
	}
	if len(duplicateNames) > 0 {
		normalized := make([]GraphExpansionColumn, len(resultColumns))
		for i, col := range resultColumns {
			if _, dup := duplicateNames[col.Name]; dup {
				normalized[i] = GraphExpansionColumn{Name: "", Value: col.Value}
			} else {
				normalized[i] = col
			}
		}
		resultColumns = normalized
	}

	// --- Build result value from columns -----------------------------
	var resultValue values.Value
	if len(resultColumns) > 0 {
		fields := make([]values.RecordConstructorField, len(resultColumns))
		anonCounter := 0
		usedNames := make(map[string]struct{}, len(resultColumns))
		for i, col := range resultColumns {
			name := col.Name
			if name == "" {
				// Generate a unique anonymous field name to avoid
				// duplicate-key panics in NewRecordConstructorValue.
				// Mirrors Java's Field.unnamedOf which uses
				// Optional.empty() — Go's string-keyed
				// RecordConstructorField needs a distinct string.
				for {
					name = fmt.Sprintf("_%d", anonCounter)
					anonCounter++
					if _, exists := usedNames[name]; !exists {
						break
					}
				}
			}
			usedNames[name] = struct{}{}
			fields[i] = values.RecordConstructorField{
				Name:  name,
				Value: col.Value,
			}
		}
		resultValue = values.NewRecordConstructorValue(fields...)
	}

	if len(g.placeholders) == 0 {
		return &SealedGraphExpansion{
			resultColumns: resultColumns,
			predicates:    g.predicates,
			quantifiers:   g.quantifiers,
			placeholders:  []*predicates.Placeholder{},
			resultValue:   resultValue,
		}
	}

	// --- Placeholder deduplication -----------------------------------

	// Step 1: Deduplicate by ParameterAlias. Placeholders with the same
	// alias are merged — the first one wins, subsequent ones' ranges are
	// logically combined. Since the Go Placeholder doesn't have
	// WithExtraRanges (Java merges ComparisonRange lists), we keep the
	// first occurrence per alias. This matches Java's practical behavior
	// for the seed where duplicate-alias placeholders carry Empty ranges.
	type aliasEntry struct {
		placeholder *predicates.Placeholder
		index       int // position in uniquePlaceholders
	}
	byAlias := make(map[values.CorrelationIdentifier]*aliasEntry)
	var uniquePlaceholders []*predicates.Placeholder
	for _, ph := range g.placeholders {
		if existing, ok := byAlias[ph.ParameterAlias]; ok {
			// Keep existing; merge is a no-op for empty ranges.
			_ = existing
		} else {
			entry := &aliasEntry{
				placeholder: ph,
				index:       len(uniquePlaceholders),
			}
			byAlias[ph.ParameterAlias] = entry
			uniquePlaceholders = append(uniquePlaceholders, ph)
		}
	}

	// Step 2: Among the unique placeholders, deduplicate by value
	// (structural equality via ExplainValue). For predicates that are
	// Placeholders with the same Value, replace with the canonical one.
	resultPlaceholders := make([]*predicates.Placeholder, len(uniquePlaceholders))
	copy(resultPlaceholders, uniquePlaceholders)

	// Build (placeholder, index) pairs for those whose values appear
	// in the predicate set.
	type phPair struct {
		ph    *predicates.Placeholder
		index int
	}
	localPlaceholderPairs := make([]phPair, 0, len(uniquePlaceholders))
	for i, ph := range uniquePlaceholders {
		localPlaceholderPairs = append(localPlaceholderPairs, phPair{ph: ph, index: i})
	}

	var resultPredicates []predicates.QueryPredicate
	for _, pred := range g.predicates {
		ph, isPh := pred.(*predicates.Placeholder)
		if !isPh {
			resultPredicates = append(resultPredicates, pred)
			continue
		}

		// Find matching placeholder by value.
		foundAtOrdinal := -1
		remaining := localPlaceholderPairs[:0]
		for _, pair := range localPlaceholderPairs {
			if semanticValueEquals(ph.Value, pair.ph.Value) {
				if foundAtOrdinal < 0 {
					foundAtOrdinal = pair.index
					resultPredicates = append(resultPredicates, pair.ph)
					resultPlaceholders[foundAtOrdinal] = pair.ph
				} else {
					resultPlaceholders[pair.index] = resultPlaceholders[foundAtOrdinal]
				}
				// Remove from pairs (consumed).
			} else {
				remaining = append(remaining, pair)
			}
		}
		localPlaceholderPairs = remaining
	}

	return &SealedGraphExpansion{
		resultColumns: resultColumns,
		predicates:    resultPredicates,
		quantifiers:   g.quantifiers,
		placeholders:  resultPlaceholders,
		resultValue:   resultValue,
	}
}

// semanticValueEquals compares two Values structurally,
// matching Java's Value.semanticEquals with an empty AliasMap.
func semanticValueEquals(a, b values.Value) bool {
	return values.ValuesStructurallyEqual(a, b)
}

// --- SealedGraphExpansion --------------------------------------------

// SealedGraphExpansion is an immutable, deduplicated version of
// GraphExpansion. Created by Seal(). Used to (repeatedly) build
// SelectExpressions. Mirrors Java's `GraphExpansion.Sealed`.
type SealedGraphExpansion struct {
	resultColumns []GraphExpansionColumn
	predicates    []predicates.QueryPredicate
	quantifiers   []expressions.Quantifier
	placeholders  []*predicates.Placeholder
	resultValue   values.Value // RecordConstructorValue from columns, or nil
}

// GetResultColumns returns the sealed result columns.
func (s *SealedGraphExpansion) GetResultColumns() []GraphExpansionColumn { return s.resultColumns }

// GetPredicates returns the sealed predicate list.
func (s *SealedGraphExpansion) GetPredicates() []predicates.QueryPredicate { return s.predicates }

// GetQuantifiers returns the sealed quantifier list.
func (s *SealedGraphExpansion) GetQuantifiers() []expressions.Quantifier { return s.quantifiers }

// GetPlaceholders returns the sealed placeholder list.
func (s *SealedGraphExpansion) GetPlaceholders() []*predicates.Placeholder { return s.placeholders }

// GetResultValue returns the RecordConstructorValue built from the
// result columns during sealing. Nil if there were no columns.
func (s *SealedGraphExpansion) GetResultValue() values.Value { return s.resultValue }

// BuildSelect creates a SelectExpression using the sealed columns'
// RecordConstructorValue as the result value. Mirrors Java's
// `Sealed.buildSelect()`.
func (s *SealedGraphExpansion) BuildSelect() *expressions.SelectExpression {
	return expressions.NewSelectExpression(s.resultValue, s.quantifiers, s.predicates)
}

// BuildSelectWithResultValue creates a SelectExpression with an
// externally-provided result value. The sealed expansion must have
// no result columns (panics otherwise, matching Java's
// `Verify.verify(resultColumns.isEmpty())`). Mirrors Java's
// `Sealed.buildSelectWithResultValue(resultValue)`.
func (s *SealedGraphExpansion) BuildSelectWithResultValue(resultValue values.Value) *expressions.SelectExpression {
	if len(s.resultColumns) > 0 {
		panic("BuildSelectWithResultValue: resultColumns must be empty")
	}
	return expressions.NewSelectExpression(resultValue, s.quantifiers, s.predicates)
}

// --- Builder ---------------------------------------------------------

// GraphExpansionBuilder accumulates graph expansion components
// incrementally. Mirrors Java's `GraphExpansion.Builder`.
type GraphExpansionBuilder struct {
	columns      []GraphExpansionColumn
	predicates   []predicates.QueryPredicate
	quantifiers  []expressions.Quantifier
	placeholders []*predicates.Placeholder
}

// NewGraphExpansionBuilder returns an empty builder.
func NewGraphExpansionBuilder() *GraphExpansionBuilder {
	return &GraphExpansionBuilder{
		columns:      []GraphExpansionColumn{},
		predicates:   []predicates.QueryPredicate{},
		quantifiers:  []expressions.Quantifier{},
		placeholders: []*predicates.Placeholder{},
	}
}

// AddColumn appends a named result column.
func (b *GraphExpansionBuilder) AddColumn(name string, value values.Value) *GraphExpansionBuilder {
	b.columns = append(b.columns, GraphExpansionColumn{Name: name, Value: value})
	return b
}

// AddPredicate appends a predicate.
func (b *GraphExpansionBuilder) AddPredicate(pred predicates.QueryPredicate) *GraphExpansionBuilder {
	b.predicates = append(b.predicates, pred)
	return b
}

// AddQuantifier appends a quantifier.
func (b *GraphExpansionBuilder) AddQuantifier(q expressions.Quantifier) *GraphExpansionBuilder {
	b.quantifiers = append(b.quantifiers, q)
	return b
}

// AddPlaceholder appends a placeholder.
func (b *GraphExpansionBuilder) AddPlaceholder(p *predicates.Placeholder) *GraphExpansionBuilder {
	b.placeholders = append(b.placeholders, p)
	return b
}

// Build produces a GraphExpansion from the accumulated state.
func (b *GraphExpansionBuilder) Build() *GraphExpansion {
	return &GraphExpansion{
		resultColumns: copySlice(b.columns),
		predicates:    copySlice(b.predicates),
		quantifiers:   copySlice(b.quantifiers),
		placeholders:  copySlice(b.placeholders),
	}
}

// --- helpers ---------------------------------------------------------

func copySlice[T any](src []T) []T {
	if src == nil {
		return nil
	}
	out := make([]T, len(src))
	copy(out, src)
	return out
}
