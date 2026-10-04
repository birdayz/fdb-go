package values

// AliasPair is one source-to-target alpha-renaming entry.
type AliasPair struct {
	Source CorrelationIdentifier
	Target CorrelationIdentifier
}

// AliasMap is an immutable, validated bijection. The reverse lookup is part of
// the contract because composing or extending an alias map must not silently
// admit two sources for one target.
type AliasMap interface {
	Target(CorrelationIdentifier) (CorrelationIdentifier, bool)
	Source(CorrelationIdentifier) (CorrelationIdentifier, bool)
	isAliasMapView()
}

type ownedAliasMap interface {
	AliasMap
	size() int
	appendPairs([]AliasPair) []AliasPair
}

type singletonAliasMap struct {
	pair AliasPair
}

type aliasMap struct {
	forward map[CorrelationIdentifier]CorrelationIdentifier
	reverse map[CorrelationIdentifier]CorrelationIdentifier
}

var emptyAliasMap AliasMap = &aliasMap{
	forward: map[CorrelationIdentifier]CorrelationIdentifier{},
	reverse: map[CorrelationIdentifier]CorrelationIdentifier{},
}

// EmptyAliasMap returns the immutable validated identity map. It avoids making
// callers handle the impossible NewAliasMap(nil) error path.
func EmptyAliasMap() AliasMap { return emptyAliasMap }

func (*aliasMap) isAliasMapView()          {}
func (*singletonAliasMap) isAliasMapView() {}

func (m *aliasMap) size() int { return len(m.forward) }

func (*singletonAliasMap) size() int { return 1 }

func (m *aliasMap) appendPairs(pairs []AliasPair) []AliasPair {
	for source, target := range m.forward {
		pairs = append(pairs, AliasPair{Source: source, Target: target})
	}
	return pairs
}

func (m *singletonAliasMap) appendPairs(pairs []AliasPair) []AliasPair {
	return append(pairs, m.pair)
}

func (m *singletonAliasMap) Target(source CorrelationIdentifier) (CorrelationIdentifier, bool) {
	if m != nil && source == m.pair.Source {
		return m.pair.Target, true
	}
	return CorrelationIdentifier{}, false
}

func (m *singletonAliasMap) Source(target CorrelationIdentifier) (CorrelationIdentifier, bool) {
	if m != nil && target == m.pair.Target {
		return m.pair.Source, true
	}
	return CorrelationIdentifier{}, false
}

func (m *aliasMap) Target(source CorrelationIdentifier) (CorrelationIdentifier, bool) {
	if m == nil {
		return CorrelationIdentifier{}, false
	}
	target, ok := m.forward[source]
	return target, ok
}

func (m *aliasMap) Source(target CorrelationIdentifier) (CorrelationIdentifier, bool) {
	if m == nil {
		return CorrelationIdentifier{}, false
	}
	source, ok := m.reverse[target]
	return source, ok
}

// NewAliasMap validates and snapshots pairs as a bijection. Current is a
// reserved correlation kind and may map only to itself.
func NewAliasMap(pairs []AliasPair) (AliasMap, error) {
	if len(pairs) == 0 {
		return EmptyAliasMap(), nil
	}
	// Java's ImmutableBiMap.of(source, target) also stores a singleton inline.
	if len(pairs) == 1 {
		if err := validateAliasPair(pairs[0]); err != nil {
			return nil, err
		}
		return &singletonAliasMap{pair: pairs[0]}, nil
	}
	forward := make(map[CorrelationIdentifier]CorrelationIdentifier, len(pairs))
	reverse := make(map[CorrelationIdentifier]CorrelationIdentifier, len(pairs))
	for i, pair := range pairs {
		if err := validateAliasPair(pair); err != nil {
			return nil, err
		}
		if _, duplicate := forward[pair.Source]; duplicate {
			return nil, resolutionError(CorrelationTypeConflict, "alias-map", "duplicate source at pair "+uitoa(uint64(i)))
		}
		if _, duplicate := reverse[pair.Target]; duplicate {
			return nil, resolutionError(CorrelationTypeConflict, "alias-map", "duplicate target at pair "+uitoa(uint64(i)))
		}
		forward[pair.Source] = pair.Target
		reverse[pair.Target] = pair.Source
	}
	return &aliasMap{forward: forward, reverse: reverse}, nil
}

func validateAliasPair(pair AliasPair) error {
	if pair.Source.IsZero() || pair.Target.IsZero() {
		return resolutionError(CorrelationZero, "alias-map", "pair contains a zero correlation")
	}
	if pair.Source.isCurrent() != pair.Target.isCurrent() {
		return resolutionError(CorrelationKindMismatch, "alias-map", "current may map only to current")
	}
	return nil
}

// ExtendAliasMap returns an immutable extension. A legitimate pairing conflict
// reports compatible=false; malformed or foreign input is an error.
func ExtendAliasMap(base AliasMap, pairs []AliasPair) (AliasMap, bool, error) {
	baseMap, ok := asAliasMap(base)
	if !ok {
		return nil, false, resolutionError(CorrelationForeignValue, "alias-map", "base is not a values-owned AliasMap")
	}
	all := baseMap.appendPairs(make([]AliasPair, 0, baseMap.size()+len(pairs)))
	for _, pair := range pairs {
		if target, exists := baseMap.Target(pair.Source); exists && target != pair.Target {
			return base, false, nil
		}
		if source, exists := baseMap.Source(pair.Target); exists && source != pair.Source {
			return base, false, nil
		}
		if target, exists := baseMap.Target(pair.Source); exists && target == pair.Target {
			continue
		}
		all = append(all, pair)
	}
	extended, err := NewAliasMap(all)
	if err != nil {
		return nil, false, err
	}
	return extended, true, nil
}

func asAliasMap(view AliasMap) (ownedAliasMap, bool) {
	switch concrete := view.(type) {
	case nil:
		return emptyAliasMap.(*aliasMap), true
	case *aliasMap:
		return concrete, concrete != nil
	case *singletonAliasMap:
		return concrete, concrete != nil
	default:
		return nil, false
	}
}

func aliasMapEmpty(view AliasMap) bool {
	concrete, ok := asAliasMap(view)
	return ok && concrete.size() == 0
}
