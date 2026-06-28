package cascades

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// MappingKind
// ---------------------------------------------------------------------------

// MappingKind discriminates the kind of predicate mapping.
// Ports Java's PredicateMultiMap.PredicateMapping.MappingKind.
type MappingKind int

const (
	// MappingRegularImpliesCandidate means the query predicate
	// directly implies the candidate predicate.
	MappingRegularImpliesCandidate MappingKind = iota

	// MappingOrTermImpliesCandidate means the query predicate is
	// one term of an OR that implies the candidate predicate.
	MappingOrTermImpliesCandidate
)

// String returns a human-readable name for the MappingKind.
func (k MappingKind) String() string {
	switch k {
	case MappingRegularImpliesCandidate:
		return "REGULAR_IMPLIES_CANDIDATE"
	case MappingOrTermImpliesCandidate:
		return "OR_TERM_IMPLIES_CANDIDATE"
	default:
		return fmt.Sprintf("MappingKind(%d)", int(k))
	}
}

// ---------------------------------------------------------------------------
// MappingKey
// ---------------------------------------------------------------------------

// MappingKey captures the relationship between query predicate and
// candidate predicate. Ports Java's
// PredicateMultiMap.PredicateMapping.MappingKey.
type MappingKey struct {
	originalQueryPredicate predicates.QueryPredicate
	candidatePredicate     predicates.QueryPredicate
	mappingKind            MappingKind
}

// NewMappingKey constructs a MappingKey.
func NewMappingKey(
	originalQueryPredicate predicates.QueryPredicate,
	candidatePredicate predicates.QueryPredicate,
	mappingKind MappingKind,
) MappingKey {
	return MappingKey{
		originalQueryPredicate: originalQueryPredicate,
		candidatePredicate:     candidatePredicate,
		mappingKind:            mappingKind,
	}
}

// GetOriginalQueryPredicate returns the original query predicate.
func (k MappingKey) GetOriginalQueryPredicate() predicates.QueryPredicate {
	return k.originalQueryPredicate
}

// GetCandidatePredicate returns the candidate predicate.
func (k MappingKey) GetCandidatePredicate() predicates.QueryPredicate {
	return k.candidatePredicate
}

// GetMappingKind returns the mapping kind.
func (k MappingKey) GetMappingKind() MappingKind {
	return k.mappingKind
}

// ---------------------------------------------------------------------------
// PredicateCompensation (functional interface)
// ---------------------------------------------------------------------------

// PredicateCompensation is the functional interface for computing a
// PredicateCompensationFunction from a partial match context.
// Ports Java's PredicateMultiMap.PredicateCompensation.
type PredicateCompensation func(
	partialMatch PartialMatch,
	boundParameterPrefixMap map[values.CorrelationIdentifier]*predicates.ComparisonRange,
	pullUp *PullUp,
) PredicateCompensationFunc

// DefaultPredicateCompensation returns a PredicateCompensation that
// always yields no compensation needed. This is the default used by
// Java's PredicateMapping.Builder.
func DefaultPredicateCompensation() PredicateCompensation {
	return func(_ PartialMatch, _ map[values.CorrelationIdentifier]*predicates.ComparisonRange, _ *PullUp) PredicateCompensationFunc {
		return NoPredicateCompensationNeeded()
	}
}

// PredicateCompensationFunc represents the result of computing
// predicate compensation — whether compensation is needed, impossible,
// or what predicates to apply.
// Ports Java's PredicateMultiMap.PredicateCompensationFunction.
type PredicateCompensationFunc interface {
	IsNeeded() bool
	IsImpossible() bool

	// Amend recreates the compensation function with updated aggregate
	// value mappings. Used during intersection of compensations when
	// aggregates are finalized. Returns a new function (or self if
	// no change needed).
	//
	// unmatchedAggregateMap: BiMap of unmatched aggregate aliases → values
	// amendedMatchedAggregateMap: mapping of old values → new values
	//
	// Ports Java's PredicateCompensationFunction.amend.
	Amend(
		unmatchedAggregateMap *BiMap[values.CorrelationIdentifier, values.Value],
		amendedMatchedAggregateMap map[values.Value]values.Value,
	) PredicateCompensationFunc

	// ApplyCompensationForPredicate applies this compensation by
	// translating correlation references via the translation map and
	// returning the set of predicates that must be injected above the
	// matched candidate scan.
	//
	// Ports Java's PredicateCompensationFunction.applyCompensationForPredicate.
	ApplyCompensationForPredicate(translationMap TranslationMap) []predicates.QueryPredicate
}

type noPredicateCompensationFunc struct{}

func (noPredicateCompensationFunc) IsNeeded() bool     { return false }
func (noPredicateCompensationFunc) IsImpossible() bool { return false }

func (f noPredicateCompensationFunc) Amend(*BiMap[values.CorrelationIdentifier, values.Value], map[values.Value]values.Value) PredicateCompensationFunc {
	return f
}

func (noPredicateCompensationFunc) ApplyCompensationForPredicate(TranslationMap) []predicates.QueryPredicate {
	return nil
}

type impossiblePredicateCompensationFunc struct{}

func (impossiblePredicateCompensationFunc) IsNeeded() bool     { return true }
func (impossiblePredicateCompensationFunc) IsImpossible() bool { return true }

func (f impossiblePredicateCompensationFunc) Amend(*BiMap[values.CorrelationIdentifier, values.Value], map[values.Value]values.Value) PredicateCompensationFunc {
	return f
}

func (impossiblePredicateCompensationFunc) ApplyCompensationForPredicate(TranslationMap) []predicates.QueryPredicate {
	return nil
}

// predicateCompensationOfPredicate wraps a query predicate for
// compensation. Ports Java's PredicateCompensationFunction.ofPredicate.
type predicateCompensationOfPredicate struct {
	predicate            predicates.QueryPredicate
	shouldSimplifyValues bool
	impossible           bool
}

func (f *predicateCompensationOfPredicate) IsNeeded() bool     { return true }
func (f *predicateCompensationOfPredicate) IsImpossible() bool { return f.impossible }

func (f *predicateCompensationOfPredicate) Amend(
	unmatchedAggregateMap *BiMap[values.CorrelationIdentifier, values.Value],
	amendedMatchedAggregateMap map[values.Value]values.Value,
) PredicateCompensationFunc {
	amended := replacePredicateValues(f.predicate, func(v values.Value) values.Value {
		return replaceUnmatchedAggregateValues(unmatchedAggregateMap, amendedMatchedAggregateMap, v)
	})
	return OfPredicateCompensation(amended, true)
}

func (f *predicateCompensationOfPredicate) ApplyCompensationForPredicate(tm TranslationMap) []predicates.QueryPredicate {
	if tm == nil || tm.DefinesOnlyIdentities() {
		return []predicates.QueryPredicate{f.predicate}
	}
	translated := translatePredicateCorrelations(f.predicate, tm)
	return []predicates.QueryPredicate{translated}
}

// OfPredicateCompensation creates a PredicateCompensationFunc that
// wraps a query predicate. When applied, it translates the predicate
// through the translation map and returns it for injection as a
// residual filter. Ports Java's
// PredicateCompensationFunction.ofPredicate.
func OfPredicateCompensation(pred predicates.QueryPredicate, shouldSimplifyValues bool) PredicateCompensationFunc {
	return &predicateCompensationOfPredicate{
		predicate:            pred,
		shouldSimplifyValues: shouldSimplifyValues,
		impossible:           predicateContainsUncompensatableValues(pred),
	}
}

// NoPredicateCompensationNeeded returns a PredicateCompensationFunc
// indicating no compensation is required.
func NoPredicateCompensationNeeded() PredicateCompensationFunc {
	return noPredicateCompensationFunc{}
}

// ImpossiblePredicateCompensation returns a PredicateCompensationFunc
// indicating compensation is needed but impossible.
func ImpossiblePredicateCompensation() PredicateCompensationFunc {
	return impossiblePredicateCompensationFunc{}
}

// ---------------------------------------------------------------------------
// PredicateMapping
// ---------------------------------------------------------------------------

// PredicateMapping maps a query predicate to a candidate predicate
// along with associated compensation, parameter binding, comparison
// range, constraint, and translated predicate.
//
// Ports Java's PredicateMultiMap.PredicateMapping.
type PredicateMapping struct {
	mappingKey               MappingKey
	predicateCompensation    PredicateCompensation
	parameterAlias           *values.CorrelationIdentifier
	comparisonRange          *predicates.ComparisonRange
	constraint               *QueryPlanConstraint
	translatedQueryPredicate predicates.QueryPredicate
}

// GetOriginalQueryPredicate returns the original query predicate.
func (m *PredicateMapping) GetOriginalQueryPredicate() predicates.QueryPredicate {
	return m.mappingKey.GetOriginalQueryPredicate()
}

// GetCandidatePredicate returns the candidate predicate.
func (m *PredicateMapping) GetCandidatePredicate() predicates.QueryPredicate {
	return m.mappingKey.GetCandidatePredicate()
}

// GetMappingKind returns the mapping kind.
func (m *PredicateMapping) GetMappingKind() MappingKind {
	return m.mappingKey.GetMappingKind()
}

// GetMappingKey returns the full mapping key.
func (m *PredicateMapping) GetMappingKey() MappingKey {
	return m.mappingKey
}

// GetPredicateCompensation returns the predicate compensation function.
func (m *PredicateMapping) GetPredicateCompensation() PredicateCompensation {
	return m.predicateCompensation
}

// GetParameterAlias returns the parameter alias, or nil if none.
func (m *PredicateMapping) GetParameterAlias() *values.CorrelationIdentifier {
	return m.parameterAlias
}

// GetComparisonRange returns the comparison range, or nil if none.
func (m *PredicateMapping) GetComparisonRange() *predicates.ComparisonRange {
	return m.comparisonRange
}

// GetConstraint returns the query plan constraint.
func (m *PredicateMapping) GetConstraint() *QueryPlanConstraint {
	return m.constraint
}

// GetTranslatedQueryPredicate returns the translated query predicate.
func (m *PredicateMapping) GetTranslatedQueryPredicate() predicates.QueryPredicate {
	return m.translatedQueryPredicate
}

// WithTranslatedQueryPredicate returns a copy of this mapping with
// the translated query predicate replaced. Mirrors Java's
// PredicateMapping.withTranslatedQueryPredicate().
func (m *PredicateMapping) WithTranslatedQueryPredicate(translated predicates.QueryPredicate) *PredicateMapping {
	return m.ToBuilder().SetTranslatedQueryPredicate(translated).Build()
}

// ToBuilder returns a PredicateMappingBuilder pre-seeded with this
// mapping's values. Mirrors Java's PredicateMapping.toBuilder().
func (m *PredicateMapping) ToBuilder() *PredicateMappingBuilder {
	b := &PredicateMappingBuilder{
		originalQueryPredicate:   m.GetOriginalQueryPredicate(),
		translatedQueryPredicate: m.translatedQueryPredicate,
		candidatePredicate:       m.GetCandidatePredicate(),
		mappingKind:              m.GetMappingKind(),
		predicateCompensation:    m.predicateCompensation,
		parameterAlias:           m.parameterAlias,
		comparisonRange:          m.comparisonRange,
		constraint:               m.constraint,
	}
	return b
}

// ---------------------------------------------------------------------------
// PredicateMappingBuilder
// ---------------------------------------------------------------------------

// PredicateMappingBuilder builds a PredicateMapping.
// Ports Java's PredicateMultiMap.PredicateMapping.Builder.
type PredicateMappingBuilder struct {
	originalQueryPredicate   predicates.QueryPredicate
	translatedQueryPredicate predicates.QueryPredicate
	candidatePredicate       predicates.QueryPredicate
	mappingKind              MappingKind
	predicateCompensation    PredicateCompensation
	parameterAlias           *values.CorrelationIdentifier
	comparisonRange          *predicates.ComparisonRange
	constraint               *QueryPlanConstraint
}

// RegularMappingBuilder creates a PredicateMappingBuilder for a
// regular (non-OR-term) mapping. Mirrors Java's
// PredicateMapping.regularMappingBuilder().
func RegularMappingBuilder(
	originalQueryPredicate predicates.QueryPredicate,
	translatedQueryPredicate predicates.QueryPredicate,
	candidatePredicate predicates.QueryPredicate,
) *PredicateMappingBuilder {
	return &PredicateMappingBuilder{
		originalQueryPredicate:   originalQueryPredicate,
		translatedQueryPredicate: translatedQueryPredicate,
		candidatePredicate:       candidatePredicate,
		mappingKind:              MappingRegularImpliesCandidate,
		predicateCompensation:    DefaultPredicateCompensation(),
		constraint:               &QueryPlanConstraint{},
	}
}

// OrTermMappingBuilder creates a PredicateMappingBuilder for an
// OR-term mapping. Mirrors Java's
// PredicateMapping.orTermMappingBuilder().
func OrTermMappingBuilder(
	originalQueryPredicate predicates.QueryPredicate,
	translatedQueryPredicate predicates.QueryPredicate,
	candidatePredicate predicates.QueryPredicate,
) *PredicateMappingBuilder {
	return &PredicateMappingBuilder{
		originalQueryPredicate:   originalQueryPredicate,
		translatedQueryPredicate: translatedQueryPredicate,
		candidatePredicate:       candidatePredicate,
		mappingKind:              MappingOrTermImpliesCandidate,
		predicateCompensation:    DefaultPredicateCompensation(),
		constraint:               &QueryPlanConstraint{},
	}
}

// SetPredicateCompensation sets the predicate compensation function.
func (b *PredicateMappingBuilder) SetPredicateCompensation(c PredicateCompensation) *PredicateMappingBuilder {
	b.predicateCompensation = c
	return b
}

// SetParameterAlias sets the parameter alias.
func (b *PredicateMappingBuilder) SetParameterAlias(alias values.CorrelationIdentifier) *PredicateMappingBuilder {
	b.parameterAlias = &alias
	return b
}

// SetComparisonRange sets the comparison range.
func (b *PredicateMappingBuilder) SetComparisonRange(cr *predicates.ComparisonRange) *PredicateMappingBuilder {
	b.comparisonRange = cr
	return b
}

// SetSargable sets both the parameter alias and comparison range.
// Mirrors Java's Builder.setSargable().
func (b *PredicateMappingBuilder) SetSargable(
	alias values.CorrelationIdentifier,
	cr *predicates.ComparisonRange,
) *PredicateMappingBuilder {
	return b.SetParameterAlias(alias).SetComparisonRange(cr)
}

// SetConstraint sets the query plan constraint.
func (b *PredicateMappingBuilder) SetConstraint(c *QueryPlanConstraint) *PredicateMappingBuilder {
	b.constraint = c
	return b
}

// SetTranslatedQueryPredicate sets the translated query predicate.
func (b *PredicateMappingBuilder) SetTranslatedQueryPredicate(p predicates.QueryPredicate) *PredicateMappingBuilder {
	b.translatedQueryPredicate = p
	return b
}

// Build constructs the PredicateMapping.
func (b *PredicateMappingBuilder) Build() *PredicateMapping {
	return &PredicateMapping{
		mappingKey: NewMappingKey(
			b.originalQueryPredicate,
			b.candidatePredicate,
			b.mappingKind,
		),
		predicateCompensation:    b.predicateCompensation,
		parameterAlias:           b.parameterAlias,
		comparisonRange:          b.comparisonRange,
		constraint:               b.constraint,
		translatedQueryPredicate: b.translatedQueryPredicate,
	}
}

// ---------------------------------------------------------------------------
// PredicateMultiMap
// ---------------------------------------------------------------------------

// predicateEntry pairs a query predicate with its mappings, preserving
// insertion order.
type predicateEntry struct {
	predicate predicates.QueryPredicate
	mappings  []*PredicateMapping
}

// PredicateMultiMap maps query predicates to sets of candidate
// predicate mappings. Uses identity-based keying (pointer equality)
// for predicates, matching Java's LinkedIdentityMap semantics.
//
// The zero value is a valid, empty PredicateMultiMap.
//
// Ports Java's com.apple.foundationdb.record.query.plan.cascades.PredicateMultiMap.
type PredicateMultiMap struct {
	// entries preserves insertion order (like Java's
	// LinkedIdentityMap). Each entry holds a distinct query
	// predicate (by pointer identity) and its associated mappings.
	entries []*predicateEntry

	// index maps predicate pointer to index in entries for O(1)
	// lookup. Uses fmt.Sprintf("%p", pred) as key to achieve
	// identity semantics without requiring predicates to be
	// comparable by value.
	index map[string]int
}

// predicateKey returns the identity key for a predicate pointer.
func predicateKey(p predicates.QueryPredicate) string {
	return fmt.Sprintf("%p", p)
}

// Get returns the mappings for the given query predicate, or nil if
// the predicate is not in the map. Uses identity (pointer) equality.
func (m *PredicateMultiMap) Get(pred predicates.QueryPredicate) []*PredicateMapping {
	if m == nil || m.index == nil {
		return nil
	}
	idx, ok := m.index[predicateKey(pred)]
	if !ok {
		return nil
	}
	return m.entries[idx].mappings
}

// Entries returns all (predicate, mapping) pairs in insertion order.
// Each predicate may appear once; each entry may have multiple mappings.
func (m *PredicateMultiMap) Entries() []struct {
	Predicate predicates.QueryPredicate
	Mapping   *PredicateMapping
} {
	if m == nil {
		return nil
	}
	var result []struct {
		Predicate predicates.QueryPredicate
		Mapping   *PredicateMapping
	}
	for _, entry := range m.entries {
		for _, mapping := range entry.mappings {
			result = append(result, struct {
				Predicate predicates.QueryPredicate
				Mapping   *PredicateMapping
			}{
				Predicate: entry.predicate,
				Mapping:   mapping,
			})
		}
	}
	return result
}

// KeySet returns the distinct query predicates in insertion order.
func (m *PredicateMultiMap) KeySet() []predicates.QueryPredicate {
	if m == nil || len(m.entries) == 0 {
		return nil
	}
	result := make([]predicates.QueryPredicate, len(m.entries))
	for i, entry := range m.entries {
		result[i] = entry.predicate
	}
	return result
}

// Values returns all mappings across all predicates, in insertion order.
func (m *PredicateMultiMap) Values() []*PredicateMapping {
	if m == nil {
		return nil
	}
	var result []*PredicateMapping
	for _, entry := range m.entries {
		result = append(result, entry.mappings...)
	}
	return result
}

// Size returns the total number of mappings across all predicates.
func (m *PredicateMultiMap) Size() int {
	if m == nil {
		return 0
	}
	n := 0
	for _, entry := range m.entries {
		n += len(entry.mappings)
	}
	return n
}

// PredicateCount returns the number of distinct predicates in the map.
func (m *PredicateMultiMap) PredicateCount() int {
	if m == nil {
		return 0
	}
	return len(m.entries)
}

// ---------------------------------------------------------------------------
// PredicateMultiMapBuilder
// ---------------------------------------------------------------------------

// PredicateMultiMapBuilder builds a PredicateMultiMap.
// Ports Java's PredicateMultiMap.Builder.
type PredicateMultiMapBuilder struct {
	entries []*predicateEntry
	index   map[string]int
}

// NewPredicateMultiMapBuilder creates a new empty builder.
func NewPredicateMultiMapBuilder() *PredicateMultiMapBuilder {
	return &PredicateMultiMapBuilder{
		index: make(map[string]int),
	}
}

// Put adds a mapping for the given query predicate. Returns true if
// the mapping was actually added (not a duplicate).
func (b *PredicateMultiMapBuilder) Put(
	queryPredicate predicates.QueryPredicate,
	mapping *PredicateMapping,
) bool {
	key := predicateKey(queryPredicate)
	if idx, ok := b.index[key]; ok {
		// Check for duplicate mapping (identity).
		for _, existing := range b.entries[idx].mappings {
			if existing == mapping {
				return false
			}
		}
		b.entries[idx].mappings = append(b.entries[idx].mappings, mapping)
		return true
	}
	b.index[key] = len(b.entries)
	b.entries = append(b.entries, &predicateEntry{
		predicate: queryPredicate,
		mappings:  []*PredicateMapping{mapping},
	})
	return true
}

// PutAll adds all entries from an existing PredicateMultiMap. Returns
// true if any mapping was added.
func (b *PredicateMultiMapBuilder) PutAll(other *PredicateMultiMap) bool {
	if other == nil {
		return false
	}
	modified := false
	for _, entry := range other.entries {
		for _, mapping := range entry.mappings {
			if b.Put(entry.predicate, mapping) {
				modified = true
			}
		}
	}
	return modified
}

// PutAllMappings adds all mappings for a single query predicate.
// Returns true if any mapping was added.
func (b *PredicateMultiMapBuilder) PutAllMappings(
	queryPredicate predicates.QueryPredicate,
	mappings []*PredicateMapping,
) bool {
	modified := false
	for _, mapping := range mappings {
		if b.Put(queryPredicate, mapping) {
			modified = true
		}
	}
	return modified
}

// checkConflicts verifies that no candidate predicate appears in
// mappings for more than one query predicate. Returns false if a
// conflict is detected. Uses identity (pointer) equality for
// candidate predicates, matching Java's Sets.newIdentityHashSet().
func (b *PredicateMultiMapBuilder) checkConflicts() bool {
	seen := make(map[string]struct{})
	for _, entry := range b.entries {
		for _, mapping := range entry.mappings {
			key := predicateKey(mapping.GetCandidatePredicate())
			if _, exists := seen[key]; exists {
				return false
			}
			seen[key] = struct{}{}
		}
	}
	return true
}

// Build constructs the PredicateMultiMap. Panics if there are
// conflicts (a candidate predicate mapped from multiple query
// predicates). Mirrors Java's Builder.build().
func (b *PredicateMultiMapBuilder) Build() *PredicateMultiMap {
	if !b.checkConflicts() {
		panic("PredicateMultiMapBuilder.Build: conflicts in mapping")
	}
	return b.buildUnchecked()
}

// BuildMaybe constructs the PredicateMultiMap, returning nil if there
// are conflicts. Mirrors Java's Builder.buildMaybe().
func (b *PredicateMultiMapBuilder) BuildMaybe() *PredicateMultiMap {
	if !b.checkConflicts() {
		return nil
	}
	return b.buildUnchecked()
}

func (b *PredicateMultiMapBuilder) buildUnchecked() *PredicateMultiMap {
	// Defensive copy of entries.
	entries := make([]*predicateEntry, len(b.entries))
	idx := make(map[string]int, len(b.entries))
	for i, entry := range b.entries {
		mappings := make([]*PredicateMapping, len(entry.mappings))
		copy(mappings, entry.mappings)
		entries[i] = &predicateEntry{
			predicate: entry.predicate,
			mappings:  mappings,
		}
		idx[predicateKey(entry.predicate)] = i
	}
	return &PredicateMultiMap{
		entries: entries,
		index:   idx,
	}
}

// ---------------------------------------------------------------------------
// PredicateMap — single-mapping variant
// ---------------------------------------------------------------------------

// PredicateMap is a PredicateMultiMap that enforces the constraint
// that each query predicate maps to at most one candidate predicate
// mapping. Ports Java's PredicateMap.
type PredicateMap struct {
	PredicateMultiMap
}

// GetMappingOptional returns the single mapping for the given
// predicate, or nil/false if the predicate has no mapping or has more
// than one. Mirrors Java's PredicateMap.getMappingOptional().
func (m *PredicateMap) GetMappingOptional(pred predicates.QueryPredicate) (*PredicateMapping, bool) {
	mappings := m.Get(pred)
	if len(mappings) != 1 {
		return nil, false
	}
	return mappings[0], true
}

// EmptyPredicateMap returns an empty PredicateMap.
func EmptyPredicateMap() *PredicateMap {
	return &PredicateMap{}
}

// ---------------------------------------------------------------------------
// PredicateMapBuilder
// ---------------------------------------------------------------------------

// PredicateMapBuilder builds a PredicateMap, enforcing uniqueness.
// Ports Java's PredicateMap.Builder.
type PredicateMapBuilder struct {
	PredicateMultiMapBuilder
}

// NewPredicateMapBuilder creates a new empty PredicateMapBuilder.
func NewPredicateMapBuilder() *PredicateMapBuilder {
	return &PredicateMapBuilder{
		PredicateMultiMapBuilder: *NewPredicateMultiMapBuilder(),
	}
}

// checkUniqueness verifies that each query predicate maps to at most
// one candidate predicate mapping. If multiple mappings exist for a
// predicate, they must all be equivalent (same kind, alias, range,
// constraint, and semantically equal candidate predicates). Non-
// equivalent duplicates cause the check to fail.
//
// Mirrors Java's PredicateMap.checkUniqueness().
func (b *PredicateMapBuilder) checkUniqueness() bool {
	for _, entry := range b.entries {
		if len(entry.mappings) <= 1 {
			continue
		}
		first := entry.mappings[0]
		for _, other := range entry.mappings[1:] {
			if !predicateMappingsEquivalent(first, other) {
				return false
			}
		}
		// Deduplicate to first.
		entry.mappings = entry.mappings[:1]
	}
	return true
}

// predicateMappingsEquivalent checks whether two predicate mappings
// are equivalent. Mirrors Java's PredicateMap.mappingsAreEquivalent().
func predicateMappingsEquivalent(a, b *PredicateMapping) bool {
	if a.GetMappingKind() != b.GetMappingKind() {
		return false
	}
	// Compare parameter aliases.
	if (a.parameterAlias == nil) != (b.parameterAlias == nil) {
		return false
	}
	if a.parameterAlias != nil && *a.parameterAlias != *b.parameterAlias {
		return false
	}
	// Compare comparison ranges by pointer — nil means absent.
	if (a.comparisonRange == nil) != (b.comparisonRange == nil) {
		return false
	}
	// Compare constraints by pointer — nil means absent.
	if (a.constraint == nil) != (b.constraint == nil) {
		return false
	}
	cp1 := a.GetCandidatePredicate()
	cp2 := b.GetCandidatePredicate()
	if cp1 == nil || cp2 == nil {
		return cp1 == cp2
	}
	return predicates.StructurallyEqual(cp1, cp2)
}

// Build constructs the PredicateMap. Panics if there are conflicts or
// non-unique mappings.
func (b *PredicateMapBuilder) Build() *PredicateMap {
	if !b.checkConflicts() {
		panic("PredicateMapBuilder.Build: conflicts in mapping")
	}
	if !b.checkUniqueness() {
		panic("PredicateMapBuilder.Build: non-unique mappings")
	}
	return &PredicateMap{
		PredicateMultiMap: *b.buildUnchecked(),
	}
}

// BuildMaybe constructs the PredicateMap, returning nil if there are
// conflicts or non-unique mappings.
func (b *PredicateMapBuilder) BuildMaybe() *PredicateMap {
	if !b.checkConflicts() {
		return nil
	}
	if !b.checkUniqueness() {
		return nil
	}
	return &PredicateMap{
		PredicateMultiMap: *b.buildUnchecked(),
	}
}

// ---------------------------------------------------------------------------
// Amend helpers — ports Java's replaceNewlyMatchedValues and checks
// ---------------------------------------------------------------------------

// replaceUnmatchedAggregateValues walks a Value tree and replaces
// UnmatchedAggregateValue markers with the corresponding matched
// aggregate value. Ports Java's
// PredicateMultiMap.replaceNewlyMatchedValues.
func replaceUnmatchedAggregateValues(
	unmatchedAggregateMap *BiMap[values.CorrelationIdentifier, values.Value],
	amendedMatchedAggregateMap map[values.Value]values.Value,
	rootValue values.Value,
) values.Value {
	if rootValue == nil {
		return nil
	}
	return values.Replace(rootValue, func(v values.Value) values.Value {
		uav, ok := v.(*values.UnmatchedAggregateValue)
		if !ok {
			return v
		}
		queryValue, found := unmatchedAggregateMap.Get(uav.UnmatchedID)
		if !found {
			return v
		}
		if translated, ok := amendedMatchedAggregateMap[queryValue]; ok {
			return translated
		}
		return v
	})
}

// replacePredicateValues applies a Value replacement function to all
// Value trees embedded in a predicate. Ports Java's
// QueryPredicate.replaceValuesMaybe.
func replacePredicateValues(p predicates.QueryPredicate, fn func(values.Value) values.Value) predicates.QueryPredicate {
	if p == nil {
		return nil
	}
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		newOperand := values.Replace(pred.Operand, fn)
		newCompOperand := values.Replace(pred.Comparison.Operand, fn)
		if newOperand == pred.Operand && newCompOperand == pred.Comparison.Operand {
			return p
		}
		// Copy the whole Comparison and replace ONLY the new RHS operand,
		// preserving Escape AND every other Comparison subclass field
		// (ParameterName, the Text* fields, the DistanceRank vector fields).
		// A partial {Type, Operand, Escape} reconstruction would drop the rest
		// and change the comparison's semantics.
		cmp := pred.Comparison
		cmp.Operand = newCompOperand
		return &predicates.ComparisonPredicate{
			Operand:    newOperand,
			Comparison: cmp,
		}
	case *predicates.ValuePredicate:
		newVal := values.Replace(pred.Value, fn)
		if newVal == pred.Value {
			return p
		}
		return predicates.NewValuePredicate(newVal)
	case *predicates.AndPredicate:
		changed := false
		newSubs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			newSubs[i] = replacePredicateValues(s, fn)
			if newSubs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return predicates.NewAnd(newSubs...)
	case *predicates.OrPredicate:
		changed := false
		newSubs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		for i, s := range pred.SubPredicates {
			newSubs[i] = replacePredicateValues(s, fn)
			if newSubs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return predicates.NewOr(newSubs...)
	case *predicates.NotPredicate:
		newChild := replacePredicateValues(pred.Child, fn)
		if newChild == pred.Child {
			return p
		}
		return predicates.NewNot(newChild)
	case *predicates.Placeholder:
		newVal := values.Replace(pred.Value, fn)
		if newVal == pred.Value {
			return p
		}
		return &predicates.Placeholder{
			ParameterAlias: pred.ParameterAlias,
			Value:          newVal,
			CompRange:      pred.CompRange,
		}
	default:
		return p
	}
}

// predicateContainsUncompensatableValues reports whether a predicate
// contains UnmatchedAggregateValue or IndexOnlyValue markers in its
// value trees. Ports Java's predicateContainsUncompensatableValues.
//
// Java dispatches on the PredicateWithValue / PredicateWithComparisons
// INTERFACES; Go switches on the two concrete query-predicate types that carry a
// value or comparison — ComparisonPredicate (Java ValuePredicate's value +
// comparison) and ValuePredicate — descending And/Or/Not via WalkPredicate. This
// is exhaustive for the predicates that actually reach this gate: it is only ever
// called on QUERY predicates (residual query preds in rule_match_intermediate /
// the amended query pred), which the SQL translator builds exclusively from
// Comparison/Value/And/Or/Not/Constant nodes. Java's other PredicateWithValue
// implementors — PredicateWithValueAndRanges and Placeholder — are CANDIDATE-side
// / matching constructs (PredicateWithValueAndRanges is never even constructed in
// the Go port) and never flow in; ExistentialValuePredicate's value is always a
// QuantifiedObjectValue and its comparison a NullComparison, so it carries no
// uncompensatable value and the false default matches Java. (Verified during the
// RFC-153 bug-hunt: this is not the fail-open gate it superficially resembles.)
func predicateContainsUncompensatableValues(p predicates.QueryPredicate) bool {
	found := false
	predicates.WalkPredicate(p, func(node predicates.QueryPredicate) bool {
		if found {
			return false
		}
		switch pred := node.(type) {
		case *predicates.ComparisonPredicate:
			if valueContainsUncompensatable(pred.Operand) {
				found = true
				return false
			}
			if pred.Comparison.Operand != nil && valueContainsUncompensatable(pred.Comparison.Operand) {
				found = true
				return false
			}
		case *predicates.ValuePredicate:
			if valueContainsUncompensatable(pred.Value) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// valueContainsUncompensatable reports whether a Value tree contains an
// index-only value (Java's Value.IndexOnlyValue marker) or an
// UnmatchedAggregateValue — values that can only be produced by an index scan
// and so cannot be evaluated as a residual filter on a base-record scan.
// Mirrors Java's predicateContainsUncompensatableValues: any IsIndexOnly value
// (IndexOnlyAggregateValue, RowNumberValue, the metric-specific
// DistanceRowNumberValues) makes the predicate uncompensatable, so a candidate
// that would leave it as a residual forms an impossible match and is discarded.
func valueContainsUncompensatable(v values.Value) bool {
	found := false
	values.WalkValue(v, func(node values.Value) bool {
		if found {
			return false
		}
		if values.IsIndexOnly(node) {
			found = true
			return false
		}
		if _, ok := node.(*values.UnmatchedAggregateValue); ok {
			found = true
			return false
		}
		return true
	})
	return found
}
