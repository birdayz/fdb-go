package values

// The TranslationMap port (Java
// com.apple.foundationdb.record.query.plan.cascades.values.translation.TranslationMap,
// TranslationMap.java:39-73 / RegularTranslationMap.java:42-228). The planner's
// cross-boundary reference rebase: a map from source quantifier alias to a
// TranslationFunction that rewrites each correlation-bearing LEAF bound to
// that alias. The partition rule's merge case builds
// `.When(lowerAlias).Then(ofOrdinalNumber(QOV(newUpper), i))` per collapsed
// leg (PartitionSelectRule.java:296-303) and applies it to the upper result
// value and predicates.
//
// The composition mechanic that makes it work — a baked enclosing
// FieldValue fusing over the baked replacement during the rebuild — lives in
// the generic withChildren arm (replace.go):
// fuse is a property of the rebuild, not of this map, so every map
// user (matching pull-up, scalar subquery, unnest) composes for free.
//
// Java's rebaseWithAliasMap (RegularTranslationMap.java:100-108, the
// alias→alias identity-rebase form) is deliberately NOT mirrored here: Go
// already has RebaseValue (rebase.go) for that job, and a second spelling of
// the same rebase would be two mechanisms for one concept.

// TranslationFunction rewrites one correlation-bearing LEAF value bound to
// sourceAlias into its replacement — Java's TranslationMap.TranslationFunction
// (TranslationMap.java:79-98). The function owns the replacement's SHAPE
// (plain ofOrdinalNumber in the merge case — composition with enclosing
// references is the rebuild's job, never the function's).
type TranslationFunction func(sourceAlias CorrelationIdentifier, leafValue Value) Value

// TranslationMap is Java's TranslationMap interface: alias membership,
// per-leaf application, and the identity early-out.
type TranslationMap interface {
	ContainsSourceAlias(alias CorrelationIdentifier) bool
	ApplyTranslationFunction(sourceAlias CorrelationIdentifier, leafValue Value) Value
	// DefinesOnlyIdentities reports that applying the map is a no-op —
	// TranslateCorrelations returns its input unchanged (pointer-stable).
	DefinesOnlyIdentities() bool
}

// RegularTranslationMap is the immutable map-backed implementation
// (RegularTranslationMap.java:42-140). Build via NewTranslationMapBuilder.
type RegularTranslationMap struct {
	fns map[CorrelationIdentifier]TranslationFunction
}

func (t *RegularTranslationMap) ContainsSourceAlias(alias CorrelationIdentifier) bool {
	_, ok := t.fns[alias]
	return ok
}

func (t *RegularTranslationMap) ApplyTranslationFunction(sourceAlias CorrelationIdentifier, leafValue Value) Value {
	fn, ok := t.fns[sourceAlias]
	if !ok {
		return leafValue
	}
	return fn(sourceAlias, leafValue)
}

func (t *RegularTranslationMap) DefinesOnlyIdentities() bool { return len(t.fns) == 0 }

// TranslationMapBuilder accumulates alias→function entries —
// RegularTranslationMap.Builder (RegularTranslationMap.java:145-228).
// Usage: NewTranslationMapBuilder().When(a).Then(fn).When(b).Then(fn2).Build().
type TranslationMapBuilder struct {
	fns map[CorrelationIdentifier]TranslationFunction
}

func NewTranslationMapBuilder() *TranslationMapBuilder {
	return &TranslationMapBuilder{fns: map[CorrelationIdentifier]TranslationFunction{}}
}

// TranslationMapWhen is the intermediate of the .When(alias).Then(fn) pair.
type TranslationMapWhen struct {
	b     *TranslationMapBuilder
	alias CorrelationIdentifier
}

func (b *TranslationMapBuilder) When(alias CorrelationIdentifier) *TranslationMapWhen {
	return &TranslationMapWhen{b: b, alias: alias}
}

// Then registers the function for the pending alias. A duplicate alias is a
// caller bug — Java Verifies against it (RegularTranslationMap.java:204);
// loud here too, a silent overwrite would drop a rebase.
func (w *TranslationMapWhen) Then(fn TranslationFunction) *TranslationMapBuilder {
	if _, dup := w.b.fns[w.alias]; dup {
		panic("TranslationMap: duplicate source alias " + w.alias.Name() + " — each alias maps to exactly one translation function")
	}
	w.b.fns[w.alias] = fn
	return w.b
}

// Build snapshots the accumulated entries — the returned map is IMMUTABLE
// (Java's ImmutableMap.copyOf): further builder use cannot mutate a map
// already handed out (sharing b.fns let a built empty map stop
// defining only identities when the builder was reused).
func (b *TranslationMapBuilder) Build() TranslationMap {
	fns := make(map[CorrelationIdentifier]TranslationFunction, len(b.fns))
	for k, v := range b.fns {
		fns[k] = v
	}
	return &RegularTranslationMap{fns: fns}
}

// ownCorrelationOfLeaf reports a LEAF value's own correlation — the alias the
// TranslationMap keys on. Mirrors the leaf arms of GetCorrelatedToOfValue
// (Java: leafValue.getCorrelatedTo(), Verify size 1 — Value.java:358-361;
// every Go correlation-bearing leaf carries exactly one alias by
// construction, so the Verify is structural here). Correlation-free leaves
// (constants, parameters) report ok=false and pass through untranslated.
func ownCorrelationOfLeaf(v Value) (CorrelationIdentifier, bool) {
	switch q := v.(type) {
	case *quantifiedObjectValue:
		return q.correlation, true
	case *QuantifiedRecordValue:
		return q.Alias, true
	case *ScalarSubqueryValue:
		return q.Alias, true
	case *ObjectValue:
		return q.Alias, true
	case *UnmatchedAggregateValue:
		return q.UnmatchedID, true
	case *ConstantObjectValue:
		return q.Alias, true
	}
	return CorrelationIdentifier{}, false
}

// TranslateCorrelations applies the map to every correlation-bearing leaf of
// v — Java's Value.translateCorrelations (Value.java:347-368): identity
// early-out, then replaceLeavesMaybe with visitNewLeaves=false (Go's
// ReplaceLeavesOnceMaybe — replacement leaves are never re-translated, the
// load-bearing semantics for self-referential substitutions). Rebuilds go
// through the checked rewrite authority, where an enclosing exact FieldValue
// FUSES over an exact FieldValue replacement (replace.go — Java's
// withNewChild = ofFieldsAndFuseIfPossible). Pointer-stable when nothing
// translates. This Value-only compatibility surface cannot return the typed
// reconstruction error, so it returns nil on failure; callers that need the
// diagnostic use TranslateCorrelationsChecked.
func TranslateCorrelations(v Value, m TranslationMap) Value {
	translated, err := TranslateCorrelationsChecked(v, m)
	if err != nil {
		// The legacy Value-only surface cannot carry the typed reconstruction
		// error. Fail closed: nil is an invalid translated tree and therefore
		// cannot be mistaken for either the original value or a partial rebuild.
		return nil
	}
	return translated
}

// TranslateCorrelationsChecked applies a TranslationMap through the checked
// rewrite spine. In particular, a FieldValue whose QOV leaf changes is rebuilt
// from the replacement's exact root; incompatible replacements return a typed
// error and no original or partially rebuilt Value.
func TranslateCorrelationsChecked(v Value, m TranslationMap) (Value, error) {
	if v == nil || m == nil {
		return v, nil
	}
	// Typed-nil guard: a nil *RegularTranslationMap inside a non-nil interface
	// passes the check above but would deref in DefinesOnlyIdentities.
	if rm, ok := m.(*RegularTranslationMap); ok && rm == nil {
		return v, nil
	}
	if m.DefinesOnlyIdentities() {
		return v, nil
	}
	return replaceLeavesOnceMaybeChecked(v, func(leaf Value) (Value, error) {
		alias, ok := ownCorrelationOfLeaf(leaf)
		if !ok || !m.ContainsSourceAlias(alias) {
			return leaf, nil
		}
		replacement := m.ApplyTranslationFunction(alias, leaf)
		if replacement == nil {
			return nil, resolutionError(RewriteNilReplacement, "translation.replacement", "translation callback returned nil")
		}
		return replacement, nil
	})
}
