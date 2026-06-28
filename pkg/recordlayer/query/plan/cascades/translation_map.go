package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

// TranslationMap is a map-like interface used to specify translations
// of correlation references within a Value tree. When a plan rule
// rewrites an expression and correlation identifiers change, a
// TranslationMap tells the rewriter how to map each old (source)
// alias to a new (target) alias and what function to apply to leaf
// values referencing that alias.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.values.translation.TranslationMap.
type TranslationMap interface {
	// ContainsSourceAlias reports whether this map has a translation
	// for the given source alias.
	ContainsSourceAlias(sourceAlias values.CorrelationIdentifier) bool

	// GetTargetAlias returns the target alias for the given source
	// alias. Returns (target, true) if the mapping exists, or
	// (zero, false) if not.
	GetTargetAlias(sourceAlias values.CorrelationIdentifier) (values.CorrelationIdentifier, bool)

	// ApplyTranslationFunction applies the translation function for
	// the given source alias to the leaf value. Panics if no
	// translation exists for sourceAlias (callers must check
	// ContainsSourceAlias first).
	//
	// Ports Java's TranslationMap.applyTranslationFunction.
	ApplyTranslationFunction(sourceAlias values.CorrelationIdentifier, leafValue values.LeafValue) values.Value

	// GetAliasMap returns the underlying AliasMap if this translation
	// map is alias-based. Returns (aliasMap, true) when available, or
	// (nil, false) for translation maps that are not backed by an
	// AliasMap.
	GetAliasMap() (*AliasMap, bool)

	// DefinesOnlyIdentities reports whether all mappings in this
	// translation map are identity mappings (source == target with
	// identity translation function). An empty map is considered
	// identity-only.
	DefinesOnlyIdentities() bool
}

// TranslationFunction is the function type used to specify the
// translation to take place when a leaf value with a particular
// source alias is encountered.
//
// Ports Java's TranslationMap.TranslationFunction functional interface.
type TranslationFunction func(sourceAlias values.CorrelationIdentifier, leafValue values.LeafValue) values.Value

// --- RegularTranslationMap -------------------------------------------

// RegularTranslationMap is the standard implementation of
// TranslationMap backed by a map from source aliases to translation
// functions.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.values.translation.RegularTranslationMap.
type RegularTranslationMap struct {
	aliasToFunctionMap map[values.CorrelationIdentifier]TranslationFunction
	// aliasMap is non-nil only for alias-map-based translation maps
	// (created via RebaseWithAliasMap or the builder). Corresponds to
	// Java's AliasMapBasedTranslationMap subclass.
	aliasMap *AliasMap
}

var emptyTranslationMap = &RegularTranslationMap{
	aliasToFunctionMap: map[values.CorrelationIdentifier]TranslationFunction{},
}

// EmptyTranslationMap returns a shared empty translation map.
// Ports Java's TranslationMap.empty() / RegularTranslationMap.empty().
func EmptyTranslationMap() *RegularTranslationMap {
	return emptyTranslationMap
}

// ContainsSourceAlias reports whether this map has a translation for
// the given source alias.
func (m *RegularTranslationMap) ContainsSourceAlias(sourceAlias values.CorrelationIdentifier) bool {
	_, ok := m.aliasToFunctionMap[sourceAlias]
	return ok
}

// GetTargetAlias returns the target alias for the given source alias
// by consulting the underlying AliasMap. Returns (zero, false) if
// there is no AliasMap or the source is not mapped.
func (m *RegularTranslationMap) GetTargetAlias(sourceAlias values.CorrelationIdentifier) (values.CorrelationIdentifier, bool) {
	if m.aliasMap == nil {
		return values.CorrelationIdentifier{}, false
	}
	return m.aliasMap.GetTargetOrEmpty(sourceAlias)
}

// ApplyTranslationFunction applies the translation function for the
// given source alias to the leaf value. Panics if no translation
// function exists for sourceAlias.
func (m *RegularTranslationMap) ApplyTranslationFunction(sourceAlias values.CorrelationIdentifier, leafValue values.LeafValue) values.Value {
	fn, ok := m.aliasToFunctionMap[sourceAlias]
	if !ok {
		panic("RegularTranslationMap.ApplyTranslationFunction: no translation for alias " + sourceAlias.Name())
	}
	return fn(sourceAlias, leafValue)
}

// GetAliasMap returns the underlying AliasMap if this translation map
// was created from one. Returns (nil, false) for plain translation
// maps without an alias map.
func (m *RegularTranslationMap) GetAliasMap() (*AliasMap, bool) {
	if m.aliasMap == nil {
		return nil, false
	}
	return m.aliasMap, true
}

// DefinesOnlyIdentities reports whether all mappings are identity
// mappings. An empty function map with an identity (or absent) alias
// map is considered identity-only.
//
// Ports Java's RegularTranslationMap.definesOnlyIdentities.
func (m *RegularTranslationMap) DefinesOnlyIdentities() bool {
	if m.aliasMap != nil && !m.aliasMap.IsIdentity() {
		return false
	}
	return len(m.aliasToFunctionMap) == 0
}

// --- Factory functions -----------------------------------------------

// TranslationMapOfAliases creates a RegularTranslationMap that
// translates a single source alias to a target alias by rebasing
// leaf values. Equivalent to RebaseWithAliasMap(AliasMapOfAliases(source, target)).
//
// Ports Java's TranslationMap.ofAliases / RegularTranslationMap.ofAliases.
func TranslationMapOfAliases(source, target values.CorrelationIdentifier) *RegularTranslationMap {
	return RebaseWithAliasMap(AliasMapOfAliases(source, target))
}

// RebaseWithAliasMap creates a RegularTranslationMap from an AliasMap
// where each source-to-target mapping generates a translation
// function that calls RebaseLeaf on the leaf value with the target
// alias.
//
// Ports Java's TranslationMap.rebaseWithAliasMap /
// RegularTranslationMap.rebaseWithAliasMap.
func RebaseWithAliasMap(am *AliasMap) *RegularTranslationMap {
	fnMap := make(map[values.CorrelationIdentifier]TranslationFunction, am.Size())
	for source, target := range am.forward {
		targetAlias := target // capture for closure
		fnMap[source] = func(_ values.CorrelationIdentifier, leafValue values.LeafValue) values.Value {
			return leafValue.RebaseLeaf(targetAlias)
		}
	}
	return &RegularTranslationMap{
		aliasToFunctionMap: fnMap,
		aliasMap:           am,
	}
}

// ComposeTranslationMaps creates a new RegularTranslationMap by
// composing multiple translation maps. Each source alias must be
// unique across all maps; panics on conflict.
//
// Ports Java's RegularTranslationMap.compose.
func ComposeTranslationMaps(maps ...*RegularTranslationMap) *RegularTranslationMap {
	b := NewTranslationMapBuilder()
	for _, tm := range maps {
		b.Compose(tm)
	}
	return b.Build()
}

// --- Builder ---------------------------------------------------------

// TranslationMapBuilder constructs a RegularTranslationMap
// incrementally using a fluent When/Then API.
//
// Ports Java's RegularTranslationMap.Builder.
type TranslationMapBuilder struct {
	entries         map[values.CorrelationIdentifier]TranslationFunction
	aliasMapBuilder *AliasMapBuilder
}

// NewTranslationMapBuilder creates an empty builder.
func NewTranslationMapBuilder() *TranslationMapBuilder {
	return &TranslationMapBuilder{
		entries:         make(map[values.CorrelationIdentifier]TranslationFunction),
		aliasMapBuilder: NewAliasMapBuilder(),
	}
}

// When starts a When/Then chain for the given source alias. Returns
// a TranslationMapWhen that must be completed with Then.
//
// Ports Java's RegularTranslationMap.Builder.when.
func (b *TranslationMapBuilder) When(sourceAlias values.CorrelationIdentifier) *TranslationMapWhen {
	return &TranslationMapWhen{
		builder:     b,
		sourceAlias: sourceAlias,
	}
}

// WhenAny starts a WhenAny/Then chain for multiple source aliases.
// The translation function will be applied to all of them.
//
// Ports Java's RegularTranslationMap.Builder.whenAny.
func (b *TranslationMapBuilder) WhenAny(sourceAliases []values.CorrelationIdentifier) *TranslationMapWhenAny {
	return &TranslationMapWhenAny{
		builder:       b,
		sourceAliases: sourceAliases,
	}
}

// Compose merges another RegularTranslationMap into this builder.
// Panics if any source alias in other already exists in the builder.
//
// Ports Java's RegularTranslationMap.Builder.compose.
func (b *TranslationMapBuilder) Compose(other *RegularTranslationMap) *TranslationMapBuilder {
	for key, fn := range other.aliasToFunctionMap {
		if _, exists := b.entries[key]; exists {
			panic("TranslationMapBuilder.Compose: duplicate source alias " + key.Name())
		}
		b.entries[key] = fn
	}
	if am, ok := other.GetAliasMap(); ok {
		b.aliasMapBuilder.PutAll(am)
	}
	return b
}

// Build creates an immutable RegularTranslationMap from the
// builder's current state. If the function map is empty, returns
// the shared empty translation map.
//
// Ports Java's RegularTranslationMap.Builder.build.
func (b *TranslationMapBuilder) Build() *RegularTranslationMap {
	if len(b.entries) == 0 {
		return EmptyTranslationMap()
	}
	fnMap := make(map[values.CorrelationIdentifier]TranslationFunction, len(b.entries))
	for k, v := range b.entries {
		fnMap[k] = v
	}
	am := b.aliasMapBuilder.Build()
	return &RegularTranslationMap{
		aliasToFunctionMap: fnMap,
		aliasMap:           am,
	}
}

// --- When/Then fluent helpers ----------------------------------------

// TranslationMapWhen is an intermediate builder step that captures
// the source alias for a When/Then chain.
//
// Ports Java's RegularTranslationMap.Builder.When inner class.
type TranslationMapWhen struct {
	builder     *TranslationMapBuilder
	sourceAlias values.CorrelationIdentifier
}

// Then completes the When/Then chain by specifying the translation
// function for the source alias.
func (w *TranslationMapWhen) Then(fn TranslationFunction) *TranslationMapBuilder {
	w.builder.entries[w.sourceAlias] = fn
	return w.builder
}

// TranslationMapWhenAny is an intermediate builder step that
// captures multiple source aliases for a WhenAny/Then chain.
//
// Ports Java's RegularTranslationMap.Builder.WhenAny inner class.
type TranslationMapWhenAny struct {
	builder       *TranslationMapBuilder
	sourceAliases []values.CorrelationIdentifier
}

// Then completes the WhenAny/Then chain by applying the same
// translation function to all source aliases.
func (w *TranslationMapWhenAny) Then(fn TranslationFunction) *TranslationMapBuilder {
	for _, alias := range w.sourceAliases {
		w.builder.entries[alias] = fn
	}
	return w.builder
}

// Compile-time interface satisfaction.
var _ TranslationMap = (*RegularTranslationMap)(nil)
