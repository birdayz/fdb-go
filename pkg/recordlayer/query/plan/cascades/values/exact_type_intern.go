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
// CHILDREN COMPARE BY POINTER, which is exact precisely because every snapshot
// path — primitive table included — yields an interned node. A new kind that
// bypassed interning would silently make equal types compare unequal here,
// costing correctness of the sharing (never of the type), so route every kind
// through internedExactType.
//
// The table never evicts. That is bounded in practice by the distinct
// (shape, name) types a process has planned against — 170 for a whole conformance
// sweep — and each entry is small. TWO axes grow it, not one: the schemas in play,
// and the QUERIES, because a projection result type is a select-list shape and an
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
	shard.mu.Lock()
	defer shard.mu.Unlock()
	// Re-check: another goroutine may have stored an equal type between the
	// read unlock and the write lock.
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
