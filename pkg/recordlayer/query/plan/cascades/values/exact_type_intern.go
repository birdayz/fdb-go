package values

import "sync"

// An exact type is content-addressed and immutable by construction, so two
// snapshots of the same type were always the same VALUE. They were two objects,
// and the cost of that is not the duplicate itself but everything derived from
// it: a fresh node, a fresh canonical encoding built by copying every child's
// canonical bytes, and a private thawCache that every shared reader then has to
// fill again.
//
// Measured over the pure-planner sweep, snapshotExactType ran 243.8 MILLION
// times and produced 170 DISTINCT canonical types. The primitives are handled by
// a static table (see sharedPrimitiveExactTypes); this is the same argument for
// the composite kinds, where the table cannot be precomputed because the
// population depends on the schemas in play.
//
// THE LOOKUP RUNS BEFORE THE NODE IS BUILT, which is the whole point. Interning
// an already-constructed node shrinks only the RETAINED set — the allocation has
// already happened, and on this workload that is 99.9999% of the calls. So the
// probe below describes a candidate out of the SOURCE type plus its
// already-interned children, and a hit returns without allocating anything.
//
// IDENTITY HERE IS FINER THAN CANONICAL IDENTITY, deliberately. The canonical
// encoding excludes the record/enum NAME, because Java's Type.Record equality
// excludes it and the exact channel exists to make that equality checkable. But
// the name is not merely decoration to a snapshot: thaw() rebuilds RecordName
// from it, so collapsing two same-shape different-name records into one object
// would make Type() report the wrong name for one of them. The probe therefore
// carries the name. Two types that are exactTypesEqual may still be two
// objects; nothing depends on the converse.
//
// CHILDREN COMPARE BY POINTER, so what every path must yield is THE shared object
// for its content. There are TWO tables that own that word, and conflating them is
// how the invariant broke once already: composites are owned by the shards below,
// primitives by the static sharedPrimitiveExactTypes map, whose nodes are never
// inserted here. A path that probes the SHARDS for a primitive therefore mints a
// duplicate of a node the static table already owns — same intern hash, canonically
// equal, different pointer — and every composite built over it silently stops
// matching the identical composite built over the shared node.
//
// That is exactly what exactWithNullability did for primitives, which is the hot
// widening case. So the rule is not "route every kind through internedExactType";
// it is route every kind to WHICHEVER table owns it, and never build a node that
// bypasses both. The cost of getting it wrong is correctness of the sharing, never
// of the type, so nothing fails — see TestEveryExactNodeIsInterned, whose witness is
// pointer identity against the owning table for precisely this reason. An
// internHashValue is NOT a witness: both tables stamp it.
//
// The table never evicts. It is bounded by the distinct (shape, NAME) types a
// process has planned against, and each entry is small. The 170 measured above is
// NOT that bound and must not be read as one: it counts distinct CANONICAL types
// over the pure-planner sweep, while this table keys finer than canonical, so the
// entry count over that same sweep is >= 170 — an unmeasured number over a
// different population. TWO axes grow it, not one: the schemas in play, and the
// QUERIES, because a projection result type is a select-list shape and an
// ad-hoc-SQL server mints those per statement. Either axis running unbounded is
// the condition to watch, not the entry count of any workload that exists today.
const exactInternShards = 64

type exactInternShard struct {
	mu      sync.RWMutex
	buckets map[uint64][]*exactType
}

var exactInterned [exactInternShards]exactInternShard

// exactProbe describes a candidate exact type without building one. srcFields
// is the SOURCE record's field list, read for names and ordinals only;
// children is parallel to it and holds the already-interned field types.
type exactProbe struct {
	code       TypeCode
	nullable   bool
	anyRecord  bool
	name       string
	srcFields  []Field
	children   []*exactType
	element    *exactType
	enumValues []EnumValue
}

func (p *exactProbe) internHash() uint64 {
	h := newSemanticHasher()
	var scratch [8]byte
	writeUint64 := func(v uint64) {
		for i := 0; i < 8; i++ {
			scratch[i] = byte(v >> (8 * i))
		}
		_, _ = h.Write(scratch[:])
	}
	writeUint64(uint64(p.code))
	if p.nullable {
		writeUint64(1)
	}
	if p.anyRecord {
		writeUint64(2)
	}
	_, _ = h.WriteString(p.name)
	writeUint64(uint64(len(p.srcFields)))
	for i := range p.srcFields {
		_, _ = h.WriteString(p.srcFields[i].Name)
		writeUint64(uint64(p.srcFields[i].Ordinal))
		writeUint64(p.children[i].internHashValue)
	}
	if p.element != nil {
		writeUint64(p.element.internHashValue)
	}
	writeUint64(uint64(len(p.enumValues)))
	for i := range p.enumValues {
		_, _ = h.WriteString(p.enumValues[i].Name)
		writeUint64(uint64(p.enumValues[i].Number))
	}
	return h.Sum64()
}

func (p *exactProbe) matches(existing *exactType) bool {
	if existing.code != p.code || existing.nullable != p.nullable ||
		existing.anyRecord != p.anyRecord || existing.name != p.name ||
		existing.element != p.element ||
		len(existing.fields) != len(p.srcFields) ||
		len(existing.enumValues) != len(p.enumValues) {
		return false
	}
	for i := range existing.fields {
		if existing.fields[i].name != p.srcFields[i].Name ||
			existing.fields[i].ordinal != p.srcFields[i].Ordinal ||
			existing.fields[i].typ != p.children[i] {
			return false
		}
	}
	for i := range existing.enumValues {
		if existing.enumValues[i] != p.enumValues[i] {
			return false
		}
	}
	return true
}

// internedExactType returns the shared exact type the probe describes, calling
// build only when there is not one yet. build must return a node matching the
// probe; it is called at most once per distinct type per process, and under the
// shard's write lock, so it must not itself snapshot.
func internedExactType(probe *exactProbe, build func() *exactType) *exactType {
	hash := probe.internHash()
	shard := &exactInterned[hash%exactInternShards]
	shard.mu.RLock()
	existing := lookupInternedLocked(shard, hash, probe)
	shard.mu.RUnlock()
	if existing != nil {
		return existing
	}
	return storeInternedExactType(shard, hash, probe, build)
}

// storeInternedExactType is internedExactType's miss path: it RE-CHECKS under the
// write lock before building, because another goroutine may have stored an equal
// type between the read unlock above and this lock.
//
// Skipping the re-check would not corrupt the table and would not produce a wrong
// TYPE — it would append a SECOND equal node, and the loser's caller would walk
// away holding an object no later lookup ever returns again. Since children
// compare BY POINTER (see the file doc), that silently defeats the sharing of
// every composite built over it, which is the whole reason this table exists.
//
// It is split out so the re-check can be driven directly rather than raced at:
// calling it twice with equal probes reaches the second lookup deterministically,
// which is what TestInterningRechecksUnderTheWriteLock does.
func storeInternedExactType(
	shard *exactInternShard,
	hash uint64,
	probe *exactProbe,
	build func() *exactType,
) *exactType {
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if existing := lookupInternedLocked(shard, hash, probe); existing != nil {
		return existing
	}
	fresh := build()
	fresh.internHashValue = hash
	fresh.finishCanonical()
	if shard.buckets == nil {
		shard.buckets = make(map[uint64][]*exactType, 16)
	}
	shard.buckets[hash] = append(shard.buckets[hash], fresh)
	return fresh
}

func lookupInternedLocked(shard *exactInternShard, hash uint64, probe *exactProbe) *exactType {
	for _, existing := range shard.buckets[hash] {
		if probe.matches(existing) {
			return existing
		}
	}
	return nil
}
