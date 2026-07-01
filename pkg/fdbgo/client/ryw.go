package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"sort"
	"sync"

	"fdb.dev/pkg/fdbgo/wire"
)

// rywCache implements a read-your-writes cache that intercepts reads and
// merges them with pending writes from the same transaction. It sits between
// the public Transaction API and the wire-level read functions.
//
// Key invariant: writes entries take precedence over cleared ranges. If a key
// was ClearRange'd then Set, the Set wins.
type rywCache struct {
	mu sync.Mutex

	// writes maps key → written value. Present means Set was called.
	// entry.value == nil means the key was Set to empty bytes, NOT cleared.
	writes map[string]rywEntry

	// sortedKeys is a lazily-maintained sorted copy of write map keys.
	// Set to nil when writes changes (dirty). Rebuilt on demand for getRange
	// to enable O(log N) binary search instead of O(N) linear scan.
	sortedKeys []string

	// cleared is a sorted, non-overlapping list of [begin, end) byte ranges
	// that were ClearRange'd.
	cleared []rywRange

	// unreadableRanges is a sorted, non-overlapping list of [begin, end) candidate
	// stamp ranges made unreadable by a pending SetVersionstampedKey (RFC-098). C++
	// marks the whole getVersionstampKeyRange UNMODIFIED+unreadable via
	// writes.addUnmodifiedAndUnreadableRange (ReadYourWrites.actor.cpp:2271,
	// WriteMap.cpp:205-242): any read REACHING the range throws accessed_unreadable
	// (1036) unless bypassed, and a bypassed read of a range position with no local
	// entry reads through to storage (the range is UNMODIFIED). Clear/ClearRange
	// SUBTRACT the cleared span (cleared = readable — C++ clear() inserts readable
	// entries over the span, WriteMap.cpp:195).
	//
	// Divergence note (deliberate, reviewed): C++ addUnmodifiedAndUnreadableRange
	// REPLACES the write-map span — wiping pending writes/clears inside the candidate
	// range from the RYW view. Go keeps them: under !bypass both models throw 1036 for
	// any read in the range, so the difference is only observable under
	// BYPASS_UNREADABLE for the obscure write-then-SVK-over-it interleaving (Go returns
	// the pending write, C++ reads storage). It is ALSO theoretically visible in
	// committed bytes: C++'s span-wipe (WriteMap.cpp:228-236) drops a prior Set
	// inside the candidate range from the write-map flush
	// (ReadYourWrites.actor.cpp:2041), so C++ never commits that Set while Go
	// does. Pathological (the wiped Set was user-issued and silently lost by
	// C++); keeping it is the saner behavior and stays deliberate.
	unreadableRanges []rywRange

	// unreadableKeys is the sorted index of write-map keys whose entries carry
	// the unreadable flag (pending versionstamped ops — typically 0, rarely 1-2
	// per transaction). Maintained incrementally at the flag transitions
	// (atomic() sets it; clear()/clearRange() delete the entry) so the
	// per-getRange unreadable-cap scan never touches sortedKeys:
	// ensureSortedLocked rebuilds O(N log N) after every write invalidation,
	// and calling it on every read made interleaved write/read transactions
	// quadratic (recordlayer suite timed out at 900s before this index).
	unreadableKeys []string

	// bypassUnreadable mirrors FDB_TR_OPTION_BYPASS_UNREADABLE
	// (ReadYourWrites.actor.cpp:2611-2613, applied per read at :98): reads of
	// unreadable keys return the write-map value with the versionstamp placeholder
	// bytes as written instead of throwing 1036; SVK's unmodified-unreadable range
	// reads through to storage. Set under mu.
	bypassUnreadable bool

	// serverCache caches server-side state at the read version, avoiding
	// redundant server round-trips for repeated reads of the same range.
	// Matches C++'s SnapshotCache. Not invalidated by local writes/clears.
	serverCache snapshotCache

	// byteBuf batch-allocates small byte copies (e.g. atomic mutation params).
	// Reduces per-op allocs for small values.
	byteBuf []byte
}

// rywEntry represents a pending write for a single key.
//
// Op-type preservation (RFC-058) — mirrors the C++ WriteMap segment a key would occupy.
// The eager value-fold (a resolved atomic stored as a plain entry) is correct for VALUES
// but C++ classifies a key by its OPERATION, which two consumers need:
//   - getKey offset walk counts by SEGMENT TYPE (is_kv), not by resolved value.
//   - updateConflictMap records a read-conflict only for DEPENDENT writes + UNMODIFIED
//     ranges, skipping INDEPENDENT writes + cleared ranges.
//
// Two flags capture exactly the C++ predicates without abandoning the value-fold:
type rywEntry struct {
	value []byte
	// absent: the resolved value is "no value" (a matched CompareAndClear) — C++
	// RYWMutation(Optional<ValueRef>(), SetValue). The key is STILL an is_kv slot for
	// getKey (counted in the offset walk, a "phantom" slot), but Get/GetRange skip it.
	// Distinct from value==nil ("Set to empty bytes", a present key).
	absent bool
	// dependent: a DEPENDENT_WRITE — a standalone atomic resolved against a DB base
	// (C++ stack bottom is an atomic, isDependent()==true). Set ONCE at the entry's birth
	// and carried UNCHANGED through later folds (C++ isDependent() reads singletonOperation,
	// the stack bottom, which coalesceOver never mutates — WriteMap.cpp:480). Drives
	// conflict filtering. A plain Set, a fold over a Set, and an atomic over a cleared base
	// are all INDEPENDENT (dependent==false). Unused while hasAtomics (independence of an
	// unresolved chain is derived from whether it contains a versionstamp — see
	// isDependentLocked).
	dependent bool
	// If true, this entry has pending atomic mutations instead of a plain Set.
	hasAtomics bool
	atomics    []rywMutation
	// unreadable: a versionstamped op landed on this key, making it UNREADABLE for
	// the transaction's lifetime — reads reaching it throw accessed_unreadable (1036)
	// unless bypassed. STICKY, mirroring C++ WriteMap.cpp:97
	// (`is_unreadable = it.is_unreadable() || op == SetVersionstampedValue/Key`):
	// set when a versionstamped op lands, PRESERVED by later plain Sets/atomics (the
	// exact-key SetValue stack-replace fast path is gated `!it.is_unreadable()` at
	// :125; on an unreadable entry the Set is pushed and the flag stays at :141), and
	// removed only by clear() (cleared entries are readable — you know they're empty).
	unreadable bool
}

// isDependentLocked reports whether this entry is a C++ DEPENDENT_WRITE (is_independent()
// false) for conflict-map purposes. A resolved entry uses its birth-time dependent flag; an
// unresolved chain is INDEPENDENT iff it contains a versionstamp (unreadable — C++ marks
// SetVersionstamped* independent), else DEPENDENT. Caller holds c.mu.
//
// Invariant (RFC-058 "Consistency"): a dependent==true entry is never cleared-based, because
// an atomic over a cleared range is folded at the independent site (atomic() :!exists &&
// isClearedLocked) with dependent=false. So !isDependentLocked() subsumes C++ is_independent's
// `following_keys_cleared` disjunct for operation keys; pure cleared ranges (no operation key)
// are classified separately via the cleared list.
func (e *rywEntry) isDependentLocked() bool {
	if e.hasAtomics {
		for _, m := range e.atomics {
			if isUnresolvedVersionstamp(m.typ) {
				return false
			}
		}
		return true
	}
	return e.dependent
}

// rywMutation represents a single atomic mutation.
type rywMutation struct {
	typ   MutationType
	param []byte
}

// rywRange represents a cleared range [begin, end).
type rywRange struct {
	begin []byte
	end   []byte
}

// allocBytes returns a slice of n bytes from the cache's shared buffer.
// Must be called with c.mu held. Reduces per-op allocs for small byte copies.
func (c *rywCache) allocBytes(n int) []byte {
	if cap(c.byteBuf)-len(c.byteBuf) < n {
		newCap := max(2*cap(c.byteBuf), len(c.byteBuf)+n)
		if newCap < 2048 {
			newCap = 2048
		}
		newBuf := make([]byte, len(c.byteBuf), newCap)
		copy(newBuf, c.byteBuf)
		c.byteBuf = newBuf
	}
	start := len(c.byteBuf)
	c.byteBuf = c.byteBuf[:start+n]
	return c.byteBuf[start : start+n]
}

// isEmpty reports whether the RYW layer holds no PENDING WRITE state (writes/cleared) and no
// cached reads (serverCache). It is the write-side half of the C++ `writes.empty() &&
// cache.empty()` poison predicate; the READ side is tracked separately by Transaction.hadRead
// (set at every read choke: getValue/getRange/getKey/GetPipelined), because the facade's Get
// uses GetPipelined which does not populate serverCache. Together (`hadRead || !isEmpty()`)
// they are the Go analog of `reading.getFutureCount()>0 || !cache.empty() || !writes.empty()`.
// Used by SetReadYourWritesDisable to decide whether to poison the transaction (RFC-059).
func (c *rywCache) isEmpty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writes) == 0 && len(c.cleared) == 0 && len(c.serverCache.entries) == 0
}

// reset clears all cached state.
func (c *rywCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = nil
	c.sortedKeys = nil
	c.cleared = nil
	c.unreadableRanges = nil
	c.unreadableKeys = nil
	// BYPASS_UNREADABLE is not a persistent option in C++ (fdb.options has no
	// persistent attribute for it), so resetRyow's options.reset clears it on every
	// retry/reset — match that by clearing here.
	c.bypassUnreadable = false
	c.byteBuf = c.byteBuf[:0]
	c.serverCache.reset()
}

// ensureSortedLocked rebuilds sortedKeys from the writes map if dirty (nil).
// Must be called under c.mu.
func (c *rywCache) ensureSortedLocked() {
	if c.sortedKeys != nil {
		return
	}
	if len(c.writes) == 0 {
		c.sortedKeys = []string{}
		return
	}
	c.sortedKeys = make([]string, 0, len(c.writes))
	for k := range c.writes {
		c.sortedKeys = append(c.sortedKeys, k)
	}
	sort.Strings(c.sortedKeys)
}

// set records a Set operation.
func (c *rywCache) set(key, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writes == nil {
		c.writes = make(map[string]rywEntry)
	}
	// Defensive copy — value must have its own backing array.
	// Cannot share byteBuf: the READABLE_UNIQUE_PENDING test proves
	// that read-back of cached Set values fails when values alias
	// the shared buffer (root cause: byteBuf growth copies data,
	// but sub-slices of the old buffer become stale if the buffer
	// pointer advances during the same transaction).
	copied := make([]byte, len(value))
	copy(copied, value)
	// Preserve the previous entry's unreadable flag: a plain Set over a key with a
	// pending versionstamped op keeps it unreadable (sticky — C++ WriteMap.cpp:125
	// gates the stack-replacing SetValue fast path on !it.is_unreadable(); on an
	// unreadable entry the Set is pushed and :141 keeps the flag).
	c.writes[string(key)] = rywEntry{value: copied, unreadable: c.writes[string(key)].unreadable}
	c.sortedKeys = nil // invalidate sorted index
	// A Set after ClearRange wins — no need to remove from cleared, because
	// get() checks writes before cleared.
}

// clear records a Clear operation (single key).
func (c *rywCache) clear(key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Remove from writes.
	if e, existed := c.writes[string(key)]; existed {
		delete(c.writes, string(key))
		c.sortedKeys = nil // invalidate sorted index
		if e.unreadable {
			c.removeUnreadableKeyLocked(string(key))
		}
	}
	// Add [key, key+\x00) to cleared.
	end := make([]byte, len(key)+1)
	copy(end, key)
	end[len(key)] = 0
	c.addClearedRangeLocked(key, end)
	// Cleared = readable (you know the key is empty): subtract the span from the
	// SVK unreadable ranges. C++ gets this free from the shared PTree — clear()
	// inserts readable cleared entries over the span (WriteMap.cpp:195).
	c.unreadableRanges = subtractRangeList(c.unreadableRanges, key, end)
}

// clearRange records a ClearRange [begin, end).
func (c *rywCache) clearRange(begin, end []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Remove all writes in [begin, end) using sorted keys for O(log N + k).
	if len(c.writes) > 0 {
		c.ensureSortedLocked()
		wStart := sort.SearchStrings(c.sortedKeys, string(begin))
		wEnd := sort.SearchStrings(c.sortedKeys, string(end))
		if wStart < wEnd {
			for i := wStart; i < wEnd; i++ {
				k := c.sortedKeys[i]
				if c.writes[k].unreadable {
					c.removeUnreadableKeyLocked(k)
				}
				delete(c.writes, k)
			}
			c.sortedKeys = nil // invalidate sorted index
		}
	}
	c.addClearedRangeLocked(begin, end)
	// Cleared = readable: subtract the span from the SVK unreadable ranges (see clear).
	c.unreadableRanges = subtractRangeList(c.unreadableRanges, begin, end)
}

// materializeCommit builds the coalesced commit mutation vector from the RYW write map — a port of C++
// ReadYourWritesTransaction::writeRangeToNativeTransaction over the whole key space
// (StringRef()..allKeys.end, ReadYourWrites.actor.cpp:1392 → :1997-2071). Two passes, matching C++:
//   - pass 1: emit every cleared range as a ClearRange mutation. Clears go FIRST ("because of keys that
//     are both cleared and set to a new value") — the clears-then-ops order lets a Set inside a cleared
//     range win, so no cleared range needs splitting around operation keys.
//   - pass 2: per operation key in sorted order emit the coalesced op — a present value → Set; an absent
//     value (a matched CompareAndClear) → single-key Clear (C++ SetValue-absent → tr.clear(key)); a
//     pending atomic chain → one atomicOp per folded stack entry.
//
// The result is [all clears in range order] ++ [all ops in key order]. The fold already happened at insert
// time (coalesceOverAtomics + the site-B/C value-fold), so this walk ships the same bytes libfdb_c does.
// Caller must NOT hold c.mu.
func (c *rywCache) materializeCommit() []Mutation {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Mutation, 0, len(c.cleared)+len(c.writes))
	// Pass 1 — clears (c.cleared is kept sorted + non-overlapping by addClearedRangeLocked).
	for _, r := range c.cleared {
		out = append(out, Mutation{Type: MutClearRange, Key: r.begin, Value: r.end})
	}
	// Pass 2 — per-key operations in sorted key order.
	c.ensureSortedLocked()
	for _, k := range c.sortedKeys {
		e := c.writes[k]
		key := []byte(k)
		switch {
		case e.hasAtomics:
			for _, m := range e.atomics {
				out = append(out, Mutation{Type: m.typ, Key: key, Value: m.param})
			}
		case e.absent:
			// A matched CompareAndClear resolved to "no value" → C++ SetValue(absent) → tr.clear(key),
			// i.e. a single-key ClearRange [key, key+\x00). (An absent entry is an operation key in the
			// write map, emitted in pass 2, NOT a cleared range from pass 1.)
			end := make([]byte, len(key)+1)
			copy(end, key)
			out = append(out, Mutation{Type: MutClearRange, Key: key, Value: end})
		default:
			out = append(out, Mutation{Type: MutSetValue, Key: key, Value: e.value})
		}
	}
	return out
}

// coalesceCommitMutations replays a validated mutation snapshot through a throwaway RYW write map and
// materializes it, yielding the coalesced commit vector libfdb_c ships (RFC-172 / #28). Working from the
// SNAPSHOT rather than the live tx.ryw keeps the shipped set byte-identical to the set Commit just
// validated: the write map is a pure function of the op-log — the site-B/C value-fold and the
// coalesceOverAtomics chain-fold use only LOCAL write state, never DB reads — so the replay reproduces
// tx.ryw exactly, while a Set racing this Commit on another goroutine (appended to tx.mutations beyond the
// snapshot) is simply absent and can never ship unvalidated. Single-key clears are stored in the op-log as
// MutClearRange(k, k+\x00), so clearRange reproduces the original clear().
func coalesceCommitMutations(muts []Mutation) []Mutation {
	var wm rywCache
	for _, m := range muts {
		switch m.Type {
		case MutSetValue:
			wm.set(m.Key, m.Value)
		case MutClearRange:
			wm.clearRange(m.Key, m.Value)
		default:
			wm.atomic(m.Type, m.Key, m.Value)
		}
	}
	return wm.materializeCommit()
}

// coalesceWriteConflicts sorts and merges write-conflict ranges (overlapping AND adjacent), matching C++'s
// contiguous is_conflict_range segment emission in writeRangeToNativeTransaction pass 2
// (ReadYourWrites.actor.cpp:2022-2033, 2069-2071). The per-op tx.writeConflicts list is uncoalesced, so a
// key hammered N times carries N identical [k,k+\x00) ranges; without this they ship N-fold and trip 2101
// on size just like the unfolded mutations. Adjacent merge ([a,b)+[b,c)→[a,c)) mirrors C++ treating
// touching conflict segments as one range. Returns a fresh slice; input is not mutated.
func coalesceWriteConflicts(in []KeyRange) []KeyRange {
	if len(in) <= 1 {
		return in
	}
	rs := make([]KeyRange, len(in))
	copy(rs, in)
	sort.Slice(rs, func(i, j int) bool {
		if cmp := bytes.Compare(rs[i].Begin, rs[j].Begin); cmp != 0 {
			return cmp < 0
		}
		return bytes.Compare(rs[i].End, rs[j].End) < 0
	})
	out := rs[:1]
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if bytes.Compare(r.Begin, last.End) <= 0 { // overlapping OR adjacent
			if bytes.Compare(r.End, last.End) > 0 {
				last.End = r.End
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// atomic records an atomic mutation.
func (c *rywCache) atomic(op MutationType, key, param []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writes == nil {
		c.writes = make(map[string]rywEntry)
	}
	k := string(key)
	entry, exists := c.writes[k]
	// Versionstamped mutations can NEVER be folded into a plain entry: the 10-byte
	// stamp is assigned at commit, so the resolved key/value is unknown client-side.
	// Eagerly resolving one (the two branches below) would store a plain rywEntry
	// whose value is the unstamped operand — surfacing a phantom present key in
	// pre-commit Get/GetRange, whatever the base state (storage-absent, locally
	// cleared, or a pending plain Set). Always record it as an UNRESOLVED atomic so
	// every read path hits the unreadable gate and throws accessed_unreadable
	// (1036), matching C++ is_unreadable (RFC-098) — unless BYPASS_UNREADABLE
	// resolves the operand as written. The mutation still commits via tx.mutations.
	if !isUnresolvedVersionstamp(op) {
		if exists && !entry.hasAtomics {
			// Site B: fold over an existing resolved entry (plain Set or a prior fold). C++
			// coalesces the op onto the stack TOP; the bottom (dependence) is unchanged, so
			// keep entry.dependent. A matched CompareAndClear → a phantom is_kv slot
			// (absent), NOT a cleared range (the key stays an operation in the write-map).
			val, clr := applyAtomic(op, entry.value, param)
			if !clr && val == nil {
				// A non-clear op that resolved to EMPTY (e.g. doMax(_, "")) is present-empty,
				// NOT absent. Normalize nil→[]byte{} (C++ Optional present) so a LATER fold
				// over this entry reads it as present — matching resolveAtomics. Without this,
				// a subsequent CompareAndClear/V2 op misreads the nil base as absent.
				val = []byte{}
			}
			entry.value = val
			entry.absent = clr
			c.writes[k] = entry
			return
		}
		if !exists && c.isClearedLocked(key) {
			// Site C: atomic over a locally-cleared range. C++ pushes a synthetic
			// SetValue(empty) at the stack bottom (WriteMap.cpp:102-111) → INDEPENDENT
			// (dependent=false). A CompareAndClear over the empty base → phantom (absent);
			// other atomics → a present resolved value.
			val, clr := applyAtomic(op, nil, param)
			if !clr && val == nil {
				val = []byte{} // present-empty, not absent (see site B).
			}
			c.writes[k] = rywEntry{value: val, absent: clr}
			c.sortedKeys = nil
			return
		}
	}
	entry.hasAtomics = true
	// C++ WriteMap.cpp:97: is_unreadable = it.is_unreadable() || op is a
	// versionstamped mutation. Sticky: once set it survives every later mutation on
	// the key (only clear() removes it, by deleting the entry).
	if !entry.unreadable && isUnresolvedVersionstamp(op) {
		entry.unreadable = true
		c.insertUnreadableKeyLocked(k)
	}
	// An entry with atomics carries no plain base — the base comes from storage at
	// read time. Drop any prior plain value (e.g. a pending Set superseded by a
	// versionstamped op) so a reader never mistakes it for a resolved value.
	entry.value = nil
	paramCopy := c.allocBytes(len(param))
	copy(paramCopy, param)
	// Coalesce onto the pending chain (C++ WriteMap::coalesceOver at insert time, WriteMap.cpp:106/144):
	// repeated same-type resolvable atomics fold to one op, so a 150k-`ADD 1` chain on an unread key ships
	// one mutation, not 150k (the RFC-172 / #28 fix). Read-transparent; versionstamps/CompareAndClear push.
	entry.atomics = coalesceOverAtomics(entry.atomics, rywMutation{typ: op, param: paramCopy})
	if !exists {
		c.sortedKeys = nil
	}
	c.writes[k] = entry
}

// get intercepts a single-key read and merges with pending writes.
func (c *rywCache) get(ctx context.Context, key []byte, serverGet func(ctx context.Context, key []byte) ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	k := string(key)
	entry, ok := c.writes[k]
	// Unreadable gate (RFC-098): a read of a key with a pending versionstamped op
	// (sticky entry flag) or inside an SVK candidate stamp range throws
	// accessed_unreadable, before any server read — C++ RYWIterator type()/kv()
	// throw at RYWIterator.cpp:45-46/:75-76 — unless BYPASS_UNREADABLE is set.
	if !c.bypassUnreadable && ((ok && entry.unreadable) || c.isUnreadableLocked(key)) {
		c.mu.Unlock()
		return nil, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	if ok {
		if entry.hasAtomics {
			// Copy atomics list, unlock for server call.
			atomics := make([]rywMutation, len(entry.atomics))
			copy(atomics, entry.atomics)
			c.mu.Unlock()

			if chainHasVersionstamp(atomics) {
				// Reachable only under bypassUnreadable (gated above). Resolve the chain
				// treating versionstamped ops as plain sets of their operand as written —
				// placeholder bytes unfilled (C++ kv() under bypass: coalesceUnder returns
				// the SVV/SVK mutation like an independent SetValue, RYWIterator.cpp:433-449).
				// Transient: do NOT cache — the entry must stay unresolved for commit, and a
				// later non-bypass read must still throw.
				if isUnresolvedVersionstamp(atomics[0].typ) {
					// INDEPENDENT chain: the bottom op is the versionstamped overwrite,
					// so the storage value can never contribute (resolveAtomicsBypass
					// replaces the base at the first versionstamped op). C++ serves this
					// entirely from the write map — an independent unreadable entry is
					// is_kv() under bypass (RYWIterator.cpp:74-84) — with NO storage
					// read: issuing one added latency and let a storage error surface
					// (and poison commit) on a path libfdb_c never reads.
					val, cleared := resolveAtomicsBypass(nil, atomics)
					if cleared {
						return nil, nil
					}
					return val, nil
				}
				// DEPENDENT chain (RMW bottom, e.g. Add before the stamp): C++ reads
				// storage under bypass too — is_kv() is false for a dependent entry,
				// so the read actor falls through to the storage get + op fold.
				base, err := serverGet(ctx, key)
				if err != nil {
					return nil, err
				}
				val, cleared := resolveAtomicsBypass(base, atomics)
				if cleared {
					return nil, nil
				}
				return val, nil
			}

			base, err := serverGet(ctx, key)
			if err != nil {
				return nil, err
			}
			base, cleared, unresolved := resolveAtomics(base, atomics)
			// Re-lock to cache result.
			c.mu.Lock()
			if c.writes == nil {
				c.writes = make(map[string]rywEntry)
			}
			if unresolved {
				// Unreachable: a versionstamped chain is handled above (bypass) or thrown
				// at the gate (!bypass). Defensive: surface the unreadable error rather
				// than the old absent approximation.
				c.mu.Unlock()
				return nil, &wire.FDBError{Code: ErrAccessedUnreadable}
			}
			if cleared {
				// Site E: a standalone CompareAndClear matched its DB base. C++ keeps this
				// as a DEPENDENT_WRITE phantom (is_kv slot, no value), NOT a cleared range.
				// Cache it as absent+dependent (RFC-058) so getKey counts the slot and the
				// conflict map still records the DB read — don't move it to the cleared list.
				c.writes[k] = rywEntry{absent: true, dependent: true}
				c.mu.Unlock()
				return nil, nil
			}
			if base == nil {
				// Defensive: a resolved non-clear chain always normalizes nil→[]byte{}
				// (present-empty) inside resolveAtomics, so this is unreachable unless a
				// future op leaves nil. Treat as absent without caching.
				c.mu.Unlock()
				return nil, nil
			}
			// Site E: a resolved standalone atomic is a DEPENDENT_WRITE (it read the DB
			// base). Preserve that op-type for conflict filtering.
			c.writes[k] = rywEntry{value: base, dependent: true}
			c.mu.Unlock()
			return base, nil
		}
		if entry.absent {
			// Phantom (matched CompareAndClear): an is_kv slot for getKey but ABSENT for a
			// point read — do NOT treat its nil value as present-empty.
			c.mu.Unlock()
			return nil, nil
		}
		val := entry.value
		c.mu.Unlock()
		return val, nil
	}
	isClr := c.isClearedLocked(key)
	if isClr {
		c.mu.Unlock()
		return nil, nil
	}
	// Check snapshot cache for prior server read.
	if val, known := c.serverCache.getKey(key); known {
		c.mu.Unlock()
		return val, nil
	}
	c.mu.Unlock()

	val, err := serverGet(ctx, key)
	if err != nil {
		return nil, err
	}
	// Cache the server result.
	c.mu.Lock()
	keyAfter := append(append([]byte(nil), key...), 0)
	var kvs []KeyValue
	if val != nil {
		kvs = []KeyValue{{Key: append([]byte(nil), key...), Value: val}}
	}
	c.serverCache.insert(key, keyAfter, kvs)
	c.mu.Unlock()
	return val, nil
}

// getRange intercepts a range read and merges with pending writes/clears.
//
// Uses iterative fetching to avoid the silent truncation bug: when the server
// has more data (serverMore=true) but all fetched results are locally cleared,
// we advance the scan range and re-fetch instead of returning more=false.
// This matches the spirit of C++'s RYWIterator which handles unknown ranges
// by issuing server reads and continuing iteration.
func (c *rywCache) getRange(
	ctx context.Context,
	begin, end []byte,
	limit int,
	reverse bool,
	serverGetRange func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error),
) ([]KeyValue, bool, error) {
	c.mu.Lock()
	// Unreadable reach cap (RFC-098): truncate the scan window at the first
	// (forward) / last (reverse) unreadable position; iteration that would
	// CROSS the cap throws accessed_unreadable, results before it emit
	// normally (C++ reach semantics, ReadYourWrites.actor.cpp:685 vs :692).
	// Computed BEFORE the fast-path branch: an SVK candidate range can cover
	// spans with no local write entries at all.
	var unreadableCap []byte
	if !c.bypassUnreadable {
		unreadableCap = c.unreadableScanCapLocked(begin, end, reverse)
	}
	hasWrites := c.hasWritesInRangeLocked(begin, end)
	hasClears := c.hasClearsInRangeLocked(begin, end)
	c.mu.Unlock()

	if unreadableCap != nil {
		if reverse {
			begin = unreadableCap
		} else {
			end = unreadableCap
		}
	}
	// reached reports whether iteration of the capped window would continue
	// INTO the unreadable position: the limit was not filled inside the
	// window (an unlimited scan always reaches).
	reached := func(got int) bool {
		return unreadableCap != nil && (limit <= 0 || got < limit)
	}

	if !hasWrites && !hasClears {
		// Fast path: no local mutations. Check snapshot cache first.
		c.mu.Lock()
		cachedKVs, fullyKnown := c.serverCache.getRangeKVs(begin, end)
		c.mu.Unlock()
		if fullyKnown {
			kvs := applyLimitAndDirection(cachedKVs, limit, reverse)
			if reached(len(kvs)) {
				return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
			}
			return kvs, computeMore(cachedKVs, limit) || limitReached(limit, len(kvs)) || unreadableCap != nil, nil
		}
		kvs, more, err := serverGetRange(ctx, begin, end, limit, reverse)
		if err != nil {
			return nil, false, err
		}
		c.cacheServerResult(begin, end, kvs, more, reverse)
		if !more && reached(len(kvs)) {
			return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
		}
		return kvs, more || unreadableCap != nil, nil
	}

	// Slow path: iterative fetch + merge. Loop until we either fill
	// the limit or the server is exhausted for the remaining range.
	var result []KeyValue
	remaining := limit
	if remaining <= 0 {
		remaining = math.MaxInt // C++ ROW_LIMIT_UNLIMITED: 0 or negative = no limit
	}
	curBegin := begin
	curEnd := end

	for remaining > 0 && bytes.Compare(curBegin, curEnd) < 0 {
		// Fetch from server with headroom to compensate for clears.
		// Cap at 10000 before doubling to avoid overflow when remaining=math.MaxInt.
		fetchLimit := 10000
		if remaining <= 5000 {
			fetchLimit = remaining * 2
			if fetchLimit < 256 {
				fetchLimit = 256
			}
		}

		serverKVs, serverMore, err := c.fetchOrCached(ctx, curBegin, curEnd, fetchLimit, reverse, serverGetRange)
		if err != nil {
			return nil, false, err
		}

		// Knowledge boundary: when serverMore=true, we only know the DB
		// state up to the last returned key. Writes beyond this boundary
		// MUST NOT be included — un-fetched server keys may interleave.
		// When serverMore=false, boundary is nil → all writes in range
		// are safe to include.
		var boundary []byte
		if serverMore && len(serverKVs) > 0 {
			boundary = serverKVs[len(serverKVs)-1].Key
		}

		batch := c.mergeBatch(serverKVs, curBegin, curEnd, boundary, reverse)

		take := len(batch)
		if take > remaining {
			take = remaining
		}
		result = append(result, batch[:take]...)
		remaining -= take

		if remaining <= 0 {
			// Limit reached (remaining hit 0 ⟺ exactly `limit` rows consumed). FDB forces
			// more=true whenever the row limit was the stop reason — limits.isReached()
			// (ReadYourWrites.actor.cpp:799) — even when no further data exists. Previously
			// this returned `take < len(batch) || serverMore || unreadableCap != nil`, which
			// was FALSE at the exactly-limit==total boundary (take==len(batch), no serverMore),
			// diverging from libfdb_c and over-conflicting via rangeConflictExtent (which keys
			// off `more`: a false more=false widens the read-conflict to the full [begin,end)).
			return result, true, nil
		}

		if !serverMore {
			// Server exhausted the (possibly capped) window without filling
			// the limit: iteration would continue INTO the unreadable
			// position — that is the REACH (RFC-098).
			if reached(len(result)) {
				return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
			}
			// All writes included. Done.
			return result, false, nil
		}

		// Server had more data, but we still need results.
		// Advance the scan range past the last fetched server key.
		if len(serverKVs) == 0 {
			break // Shouldn't happen: serverMore=true with 0 results.
		}

		if reverse {
			curEnd = serverKVs[len(serverKVs)-1].Key // [curBegin, lastKey)
		} else {
			// keyAfter(lastKey): append \x00 to step past the last fetched key.
			lastKey := serverKVs[len(serverKVs)-1].Key
			curBegin = append(append([]byte{}, lastKey...), 0)
		}
	}

	if reached(len(result)) {
		// The capped window was exhausted without filling the limit (RFC-098).
		return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	return result, false, nil
}

// fetchOrCached checks the snapshot cache before making a server call.
// If the range is fully cached, returns cached KVs (in scan direction).
// Otherwise fetches from server and caches the result.
func (c *rywCache) fetchOrCached(
	ctx context.Context,
	begin, end []byte,
	limit int,
	reverse bool,
	serverGetRange func(ctx context.Context, begin, end []byte, limit int, reverse bool) ([]KeyValue, bool, error),
) ([]KeyValue, bool, error) {
	c.mu.Lock()
	cachedKVs, fullyKnown := c.serverCache.getRangeKVs(begin, end)
	c.mu.Unlock()

	if fullyKnown {
		kvs := applyLimitAndDirection(cachedKVs, limit, reverse)
		more := computeMore(cachedKVs, limit)
		return kvs, more, nil
	}

	kvs, more, err := serverGetRange(ctx, begin, end, limit, reverse)
	if err != nil {
		return nil, false, err
	}
	c.cacheServerResult(begin, end, kvs, more, reverse)
	return kvs, more, nil
}

// cacheServerResult inserts a server getRange result into the snapshot cache.
func (c *rywCache) cacheServerResult(fetchBegin, fetchEnd []byte, serverKVs []KeyValue, serverMore bool, reverse bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cacheBegin := fetchBegin
	cacheEnd := fetchEnd
	var cacheKVs []KeyValue

	if reverse {
		if serverMore && len(serverKVs) > 0 {
			// Reverse scan: last element is the smallest key returned.
			// Known range is [lastKey, fetchEnd).
			cacheBegin = serverKVs[len(serverKVs)-1].Key
		}
		// Store in forward order.
		cacheKVs = make([]KeyValue, len(serverKVs))
		for i, kv := range serverKVs {
			cacheKVs[len(serverKVs)-1-i] = kv
		}
	} else {
		if serverMore && len(serverKVs) > 0 {
			// Forward scan: known range is [fetchBegin, keyAfter(lastKey)).
			lastKey := serverKVs[len(serverKVs)-1].Key
			cacheEnd = append(append([]byte(nil), lastKey...), 0)
		}
		cacheKVs = make([]KeyValue, len(serverKVs))
		copy(cacheKVs, serverKVs)
	}

	c.serverCache.insert(cacheBegin, cacheEnd, cacheKVs)
}

// applyLimitAndDirection returns KVs with limit and direction applied.
// Input KVs must be in ascending order.
func applyLimitAndDirection(kvs []KeyValue, limit int, reverse bool) []KeyValue {
	if reverse {
		// Reverse the order.
		out := make([]KeyValue, len(kvs))
		for i, kv := range kvs {
			out[len(kvs)-1-i] = kv
		}
		kvs = out
	}
	if limit > 0 && len(kvs) > limit {
		kvs = kvs[:limit]
	}
	return kvs
}

// computeMore returns true if applying the limit would leave remaining KVs.
func computeMore(kvs []KeyValue, limit int) bool {
	return limit > 0 && len(kvs) > limit
}

// limitReached ports C++ GetRangeLimits::isReached() for a row-only limit: the
// requested row limit was fully consumed (rows==0). FDB forces more=true in this
// case — ReadYourWrites.actor.cpp:799 `result.more = result.more || limits.isReached()` —
// the canonical "the limit was the stop reason" contract that continuation-based
// iteration relies on, INDEPENDENT of whether further data actually exists. `returned`
// is the count after the limit was applied, so returned==limit ⟺ the limit was reached.
// (Go's merge already clamps results to [begin,end), so C++'s subsequent
// resize-clears-more branch — which only fires for items BEYOND end — never applies here.)
func limitReached(limit, returned int) bool {
	return limit > 0 && returned >= limit
}

// mergeBatch merges a batch of server results with local writes and clears.
// boundary is the knowledge boundary (last fetched key); nil means the entire
// range is known. Returns sorted key-value pairs.
//
// Uses sorted write keys + two-pointer merge for O(k + S) instead of the
// previous O(W + S log S) where W = total writes, k = writes in range, S = server results.
func (c *rywCache) mergeBatch(
	serverKVs []KeyValue,
	rangeBegin, rangeEnd []byte,
	boundary []byte,
	reverse bool,
) []KeyValue {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ensureSortedLocked()

	// Phase 1: Filter server results — remove cleared keys.
	// Server results are already sorted in scan direction.
	filteredServer := make([]KeyValue, 0, len(serverKVs))
	// Build server key lookup only if needed for atomic resolution.
	// Most Record Layer transactions have no atomics — skip the map allocation.
	var serverValues map[string][]byte
	for _, kv := range serverKVs {
		if !c.isClearedLocked(kv.Key) {
			filteredServer = append(filteredServer, kv)
		}
	}

	// atomicCleared tracks keys where atomic resolution resulted in deletion.
	// These must be excluded from filteredServer during the merge phase,
	// because the atomic was resolved AFTER building filteredServer.
	var atomicCleared map[string]bool

	// Phase 2: Find write keys in the effective range using binary search.
	// For forward scans: include writes in [rangeBegin, boundary] (inclusive).
	// For reverse scans: include writes in [boundary, rangeEnd) (inclusive begin).
	effectiveBegin := string(rangeBegin)
	effectiveEnd := string(rangeEnd)

	if boundary != nil {
		if reverse {
			// Include writes >= boundary.
			if string(boundary) > effectiveBegin {
				effectiveBegin = string(boundary)
			}
		} else {
			// Include writes <= boundary. Use boundary+"\x00" as exclusive end
			// so sort.SearchStrings returns an index that includes boundary itself
			// (the boundary key is the last fetched server key — safe to include).
			boundaryAfter := string(append(append([]byte(nil), boundary...), 0))
			if boundaryAfter < effectiveEnd {
				effectiveEnd = boundaryAfter
			}
		}
	}

	wStart := sort.SearchStrings(c.sortedKeys, effectiveBegin)
	wEnd := sort.SearchStrings(c.sortedKeys, effectiveEnd)

	// Process writes in range: resolve atomics, collect into sorted slice.
	writeKVs := make([]KeyValue, 0, wEnd-wStart)
	for i := wStart; i < wEnd; i++ {
		k := c.sortedKeys[i]
		entry, exists := c.writes[k]
		if !exists {
			continue // phantom key (deleted by prior atomic caching)
		}
		if entry.absent {
			// Phantom slot (a matched CompareAndClear, RFC-058): an is_kv slot for getKey
			// but ABSENT in a range read. Skip it from writeKVs AND shadow any server-present
			// value at this key (the local clear wins) via atomicCleared — else this
			// pre-existing absent entry would fall through to the plain-entry branch below
			// and be emitted as a phantom KV.
			if atomicCleared == nil {
				atomicCleared = make(map[string]bool)
			}
			atomicCleared[k] = true
			continue
		}
		if entry.hasAtomics {
			// Lazily build server values map on first atomic encounter.
			if serverValues == nil {
				serverValues = make(map[string][]byte, len(serverKVs))
				for _, kv := range serverKVs {
					serverValues[string(kv.Key)] = kv.Value
				}
			}
			// Resolve atomics against server base. nil = absent, non-nil = present
			// (incl. present-empty) — mirroring C++ Optional<ValueRef> so V2 ops
			// (MinV2/AndV2) distinguish "missing → operand" from "present-empty → op".
			base, basePresent := serverValues[k]
			if basePresent && base == nil {
				base = []byte{} // present-empty storage value — keep non-nil
			}
			base, cleared, unresolved := resolveAtomics(base, entry.atomics)
			// Cache resolved value.
			if unresolved {
				// Unresolved versionstamped op in the chain: unreadable client-side
				// (the stamp is assigned at commit). Under !bypass this entry is
				// excluded from the scan window by unreadableScanCapLocked (a scan
				// reaching it errors before the merge), so this branch is live only
				// under BYPASS_UNREADABLE: emit the chain resolved with versionstamped
				// ops as plain sets of their operand as written (RFC-098; C++ kv()
				// under bypass, RYWIterator.cpp:433-449). Never cache — the entry
				// must stay unresolved for commit, and a later non-bypass read must
				// still throw. !bypass fallback keeps the old absent-shadowing
				// (defensive; unreachable).
				if c.bypassUnreadable {
					if val, clr := resolveAtomicsBypass(base, entry.atomics); !clr {
						writeKVs = append(writeKVs, KeyValue{Key: []byte(k), Value: val})
					}
				}
				if atomicCleared == nil {
					atomicCleared = make(map[string]bool)
				}
				atomicCleared[k] = true
			} else if cleared {
				// Site E: a standalone CompareAndClear matched. Cache as a DEPENDENT phantom
				// (absent+dependent) — an is_kv slot for getKey that reads ABSENT here — NOT
				// a cleared range (RFC-058). Still exclude the server entry from this merge
				// (filteredServer was built before atomic resolution) via atomicCleared.
				c.writes[k] = rywEntry{absent: true, dependent: true}
				if atomicCleared == nil {
					atomicCleared = make(map[string]bool)
				}
				atomicCleared[k] = true
				// sortedKeys stays valid: the key remains in c.writes (now a phantom),
				// it is not removed.
			} else if base == nil {
				// Defensive: resolveAtomics normalizes a resolved non-clear chain to
				// non-nil (present-empty). Unreachable today; treat as absent.
			} else {
				// Site E: a resolved standalone atomic is a DEPENDENT_WRITE — preserve the
				// op-type for conflict filtering.
				c.writes[k] = rywEntry{value: base, dependent: true}
				writeKVs = append(writeKVs, KeyValue{Key: []byte(k), Value: base})
			}
		} else {
			// A plain (non-atomic) entry is always a PRESENT key: cleared keys are
			// removed from c.writes (never tombstoned), so entry.value == nil means
			// "Set to empty bytes" / an atomic resolved to empty — NOT absent. The
			// previous `entry.value != nil` guard dropped such empty-value keys from
			// the merged range (e.g. after a Get resolved a pending Xor(k,"") into a
			// nil-value entry), so getRange disagreed with libfdb_c. Found by the
			// RFC-055 RYW-read differential.
			writeKVs = append(writeKVs, KeyValue{Key: []byte(k), Value: entry.value})
		}
	}
	// writeKVs is sorted ascending (from sortedKeys iteration).

	// Phase 3: Two-pointer merge.
	// filteredServer: sorted in scan direction (forward=ascending, reverse=descending).
	// writeKVs: sorted ascending. Reverse it for reverse scans.
	if reverse {
		for i, j := 0, len(writeKVs)-1; i < j; i, j = i+1, j-1 {
			writeKVs[i], writeKVs[j] = writeKVs[j], writeKVs[i]
		}
	}

	result := make([]KeyValue, 0, len(filteredServer)+len(writeKVs))
	si, wi := 0, 0

	for si < len(filteredServer) || wi < len(writeKVs) {
		if si >= len(filteredServer) {
			result = append(result, writeKVs[wi:]...)
			break
		}
		if wi >= len(writeKVs) {
			// Append remaining server entries, skipping any cleared by atomics.
			for ; si < len(filteredServer); si++ {
				if !atomicCleared[string(filteredServer[si].Key)] {
					result = append(result, filteredServer[si])
				}
			}
			break
		}

		// Skip server entries cleared by atomic resolution.
		if atomicCleared[string(filteredServer[si].Key)] {
			si++
			continue
		}

		cmp := bytes.Compare(filteredServer[si].Key, writeKVs[wi].Key)
		if cmp == 0 {
			// Write shadows server — take write value, skip server.
			result = append(result, writeKVs[wi])
			si++
			wi++
		} else if (reverse && cmp > 0) || (!reverse && cmp < 0) {
			result = append(result, filteredServer[si])
			si++
		} else {
			result = append(result, writeKVs[wi])
			wi++
		}
	}

	return result
}

// isCleared returns true if key falls within any cleared range (acquires lock).
func (c *rywCache) isCleared(key []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isClearedLocked(key)
}

func (c *rywCache) isClearedLocked(key []byte) bool {
	// Binary search: cleared is sorted by begin, non-overlapping.
	// Find the last range whose begin <= key, then check key < end.
	n := len(c.cleared)
	if n == 0 {
		return false
	}
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(c.cleared[i].begin, key) > 0
	})
	// i is the first range with begin > key. Check i-1.
	if i == 0 {
		return false
	}
	r := c.cleared[i-1]
	return bytes.Compare(key, r.end) < 0
}

// conflictForKeyLocked reports whether a single-key read of `key` must add a read-conflict, the
// single-key analog of C++ updateConflictMap(ryw, key, it) (ReadYourWrites.actor.cpp:322-332):
// conflict iff the key sits in an UNMODIFIED range or a DEPENDENT operation; SKIP for an
// INDEPENDENT write (plain Set / folded atomic / matched-CAC phantom) or a cleared range. It is the
// exact result of conflictRangesLocked(key, keyAfter(key)) — operation→isDependentLocked(),
// cleared→false, gap→true — but reached by ONE map lookup + a cleared binary-search, with NO
// ensureSortedLocked re-sort. C++ positions one WriteMap iterator (it.skip(key)) here; routing the
// hot single-key Get path through the range walk re-sorted the whole growing write map on every
// Get (O(n²·log n) across a write-heavy txn — a 10K-record save hung for 15 min). Caller holds c.mu.
func (c *rywCache) conflictForKeyLocked(key []byte) bool {
	if e, ok := c.writes[string(key)]; ok {
		return e.isDependentLocked() // operation key: conflict iff DEPENDENT_WRITE
	}
	if c.isClearedLocked(key) {
		return false // cleared range: known empty locally, no DB read resolved it
	}
	return true // unmodified gap: a DB read resolved it
}

// conflictRangesLocked walks the write-map over [begin, end) and returns the maximal
// sub-ranges that must be added to the getKey read-conflict map — a faithful port of C++
// updateConflictMap (ReadYourWrites.actor.cpp:335-351), which iterates the WriteMap (NOT the
// merged RYWIterator — the snapshot cache is irrelevant) and records a conflict for a segment
// iff `is_unmodified_range() || (is_operation() && !is_independent())`. I.e.:
//   - UNMODIFIED gap (no write key, not cleared) → conflict (a DB read resolved it).
//   - operation key with isDependentLocked() (DEPENDENT_WRITE — a standalone atomic that read
//     the DB base) → conflict, as the single key [k, keyAfter(k)).
//   - INDEPENDENT write key (plain Set, folded atomic, matched-CAC phantom) → NO conflict.
//   - CLEARED range (no operation key) → NO conflict.
//
// Adjacent qualifying sub-ranges are coalesced. Caller holds c.mu. The over-conflict that the
// old full-range conflict caused (on INDEPENDENT writes + cleared ranges) is thereby removed,
// matching C++ exactly. (RFC-058 sub-edge b.)
func (c *rywCache) conflictRangesLocked(begin, end []byte) [][2][]byte {
	if bytes.Compare(begin, end) >= 0 {
		return nil
	}
	c.ensureSortedLocked()

	// Collect the segment boundaries within [begin, end]: begin, end, every write key in
	// range AND its keyAfter (so an operation occupies its own single-key segment [k,
	// keyAfter(k)) and the following gap is classified separately), and every cleared-range
	// bound clamped into the window.
	bounds := [][]byte{append([]byte(nil), begin...), append([]byte(nil), end...)}
	addBound := func(b []byte) {
		if bytes.Compare(b, begin) > 0 && bytes.Compare(b, end) < 0 {
			bounds = append(bounds, append([]byte(nil), b...))
		}
	}
	wStart := sort.SearchStrings(c.sortedKeys, string(begin))
	for i := wStart; i < len(c.sortedKeys) && c.sortedKeys[i] < string(end); i++ {
		k := []byte(c.sortedKeys[i])
		addBound(k)
		addBound(keyAfterBytes(k))
	}
	for _, r := range c.cleared {
		if bytes.Compare(r.begin, end) >= 0 {
			break
		}
		if bytes.Compare(r.end, begin) <= 0 {
			continue
		}
		addBound(r.begin)
		addBound(r.end)
	}
	sort.Slice(bounds, func(i, j int) bool { return bytes.Compare(bounds[i], bounds[j]) < 0 })

	var out [][2][]byte
	for i := 0; i+1 < len(bounds); i++ {
		lo, hi := bounds[i], bounds[i+1]
		if bytes.Equal(lo, hi) {
			continue // deduped boundary
		}
		conflict := false
		if entry, ok := c.writes[string(lo)]; ok && bytes.Equal(hi, keyAfterBytes(lo)) {
			// Operation key occupying the single-key segment [lo, keyAfter(lo)).
			conflict = entry.isDependentLocked()
		} else if c.isClearedLocked(lo) {
			conflict = false // CLEARED range — no DB read.
		} else {
			conflict = true // UNMODIFIED gap — a DB read resolved it.
		}
		if !conflict {
			continue
		}
		// Coalesce with the previous sub-range if adjacent.
		if n := len(out); n > 0 && bytes.Equal(out[n-1][1], lo) {
			out[n-1][1] = append([]byte(nil), hi...)
		} else {
			out = append(out, [2][]byte{append([]byte(nil), lo...), append([]byte(nil), hi...)})
		}
	}
	return out
}

func (c *rywCache) hasWritesInRangeLocked(begin, end []byte) bool {
	c.ensureSortedLocked()
	if len(c.sortedKeys) == 0 {
		return false
	}
	// Binary search: find the first key >= begin.
	i := sort.SearchStrings(c.sortedKeys, string(begin))
	// If that key is < end, there's a write in range.
	return i < len(c.sortedKeys) && c.sortedKeys[i] < string(end)
}

func (c *rywCache) hasClearsInRangeLocked(begin, end []byte) bool {
	// Binary search: find the last cleared range that could overlap [begin, end).
	// Two ranges [a,b) and [c,d) overlap iff a < d && c < b.
	// Cleared ranges are sorted by begin and non-overlapping.
	n := len(c.cleared)
	if n == 0 {
		return false
	}
	// Find the first range with begin >= end (definitely can't overlap).
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(c.cleared[i].begin, end) >= 0
	})
	// The candidate is the range just before i (largest begin < end).
	// Since ranges are non-overlapping and sorted, if this one doesn't
	// overlap, no earlier range can either (their end <= this one's begin).
	if i > 0 && bytes.Compare(c.cleared[i-1].end, begin) > 0 {
		return true
	}
	return false
}

// addClearedRange adds [begin, end) to the cleared list, merging overlapping
// and adjacent ranges to keep the list sorted and non-overlapping.
func (c *rywCache) addClearedRange(begin, end []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addClearedRangeLocked(begin, end)
}

func (c *rywCache) addClearedRangeLocked(begin, end []byte) {
	n := len(c.cleared)
	if n == 0 {
		c.cleared = []rywRange{{
			begin: append([]byte(nil), begin...),
			end:   append([]byte(nil), end...),
		}}
		return
	}

	// Binary search to find the overlap window.
	// Ranges are sorted by begin, non-overlapping.
	// A range r overlaps/is-adjacent to [begin, end) if r.begin <= end && begin <= r.end.

	// First overlapping range: last range with begin <= end.
	// We find the first range with begin > end, then look back.
	hiIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(c.cleared[i].begin, end) > 0
	})
	// Last overlapping range: first range with end >= begin.
	// We find the first range whose end > begin, starting from 0.
	// Since ranges are sorted and non-overlapping, end values are also sorted.
	loIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(c.cleared[i].end, begin) >= 0
	})

	// [loIdx, hiIdx) are the ranges that overlap or are adjacent.
	newBegin := append([]byte(nil), begin...)
	newEnd := append([]byte(nil), end...)

	for i := loIdx; i < hiIdx; i++ {
		if bytes.Compare(c.cleared[i].begin, newBegin) < 0 {
			newBegin = c.cleared[i].begin
		}
		if bytes.Compare(c.cleared[i].end, newEnd) > 0 {
			newEnd = c.cleared[i].end
		}
	}

	// Replace [loIdx, hiIdx) with the merged range.
	merged := rywRange{begin: newBegin, end: newEnd}
	overlapCount := hiIdx - loIdx
	if overlapCount == 0 {
		// No overlaps — insert at loIdx.
		c.cleared = append(c.cleared, rywRange{})
		copy(c.cleared[loIdx+1:], c.cleared[loIdx:])
		c.cleared[loIdx] = merged
	} else if overlapCount == 1 {
		// Replace single overlapping range in-place.
		c.cleared[loIdx] = merged
	} else {
		// Replace multiple overlapping ranges with one.
		c.cleared[loIdx] = merged
		c.cleared = append(c.cleared[:loIdx+1], c.cleared[hiIdx:]...)
	}
}

// applyAtomic applies an atomic mutation to a base value, mirroring the C++
// implementations in fdbclient/include/fdbclient/Atomic.h exactly.
//
// Convention: base==nil means "key absent" (C++ Optional<ValueRef> not present).
// isUnresolvedVersionstamp reports whether op is a versionstamped mutation, which
// cannot be resolved client-side: the 10-byte stamp is assigned at commit, so the
// resolved key (SetVersionstampedKey) or value (SetVersionstampedValue) is unknown
// pre-commit. C++ marks such a key accessed_unreadable; the RYW read paths must read
// it as ABSENT (our approximation) rather than surface a phantom value.
func isUnresolvedVersionstamp(op MutationType) bool {
	return op == MutSetVersionstampedKey || op == MutSetVersionstampedValue
}

// isNonAssociativeOp mirrors C++ isNonAssociativeOp / NON_ASSOCIATIVE_MASK (CommitTransaction.h:576-578):
// ops that obey the associative law ONLY when all operands have equal length. coalesceOverAtomics folds
// two same-type non-associative ops solely when their operand lengths match; the associative ops
// (And, AndV2, ByteMin, ByteMax, AppendIfFits) always fold on same type.
func isNonAssociativeOp(op MutationType) bool {
	switch op {
	case MutAddValue, MutOr, MutXor, MutMax, MutMin, MutMinV2,
		MutSetVersionstampedKey, MutSetVersionstampedValue, MutCompareAndClear:
		return true
	default:
		return false
	}
}

// coalesceOverAtomics folds newMut into the pending atomic chain, a port of C++ WriteMap::coalesceOver
// (WriteMap.cpp:480-494) + coalesce (:357) restricted to Go's representation: the SetValue base is stored
// separately in rywEntry.value and resolved via the eager value-fold at atomic() sites B/C, so this stack
// is a PURE atomic chain (its top is always an atomic op, never a SetValue). Within that restriction the
// two C++ branches reduce to:
//   - same op type, associative OR equal operand length → FOLD (combine operands via applyAtomic, keep the
//     atomic type) — 150k `ADD 1` (all 8-byte) → one `ADD 150000`;
//   - same op type, non-associative, DIFFERENT operand length → PUSH (keep both);
//   - a DIFFERENT op type → PUSH (two atomics of different type never collapse — C++ else-branch
//     `isAtomicOp(new) && isAtomicOp(existing)` → push).
//
// Versionstamped ops and CompareAndClear are always PUSHED (never folded): CompareAndClear is excluded from
// C++'s same-type fold branch (`newEntry.type != CompareAndClear`) and over another atomic pushes; a
// versionstamped op is kept intact so the entry's unreadable/resolve semantics are byte-for-byte unchanged
// (its 10-byte stamp is assigned at commit, so folding cannot combine operands anyway). The fold is
// read-transparent: resolveAtomics over the folded chain yields the identical value.
func coalesceOverAtomics(stack []rywMutation, newMut rywMutation) []rywMutation {
	if len(stack) == 0 || isUnresolvedVersionstamp(newMut.typ) || newMut.typ == MutCompareAndClear {
		return append(stack, newMut)
	}
	top := &stack[len(stack)-1]
	if top.typ != newMut.typ || isUnresolvedVersionstamp(top.typ) {
		return append(stack, newMut) // different type (or the top is an unfoldable versionstamp) → keep both
	}
	if isNonAssociativeOp(top.typ) && len(top.param) != len(newMut.param) {
		return append(stack, newMut) // non-associative + operand-size mismatch → keep both
	}
	// Fold: combine the two operands with the op's own primitive, keeping the atomic type
	// (C++ coalesce same-type branch, e.g. AddValue → doAdd(existing, new) → AddValue).
	combined, _ := applyAtomic(top.typ, top.param, newMut.param)
	top.param = combined
	return stack
}

// resolveAtomics folds an atomic-mutation chain onto a storage base for the RYW read
// paths (single-key get + range merge). It returns the resolved value and two
// terminal flags:
//
//   - cleared: a CompareAndClear matched → the key is removed from the merged view.
//   - unresolved: the chain contains a versionstamped op → the key is UNREADABLE
//     client-side (C++ accessed_unreadable) and reads as absent. This is terminal and
//     DOMINANT: once a versionstamp appears, a later resolvable op layered on top is
//     still unreadable (an op over an unknown value/key stays unknown), so the rest of
//     the chain is skipped and unresolved wins over cleared.
//
// Convention: base==nil means "key absent" (C++ Optional<ValueRef> not present);
// base==[]byte{} means "present with empty value". A resolved non-clear op normalizes
// a nil result to []byte{} so a later V2 op (MinV2/AndV2) sees "present" and does the
// op rather than the absent→operand path — matching C++ Optional.present() (Atomic.h).
func resolveAtomics(base []byte, atomics []rywMutation) (result []byte, cleared, unresolved bool) {
	for _, m := range atomics {
		if isUnresolvedVersionstamp(m.typ) {
			return nil, false, true
		}
		base, cleared = applyAtomic(m.typ, base, m.param)
		if cleared {
			base = nil
		} else if base == nil {
			base = []byte{}
		}
	}
	return base, cleared, false
}

// base==[]byte{} means "key present with empty value".
// Returns (result, cleared). cleared=true means the key should be removed
// (only happens for CompareAndClear).
func applyAtomic(op MutationType, base, param []byte) (result []byte, cleared bool) {
	switch op {
	case MutSetValue:
		return append([]byte(nil), param...), false
	case MutAddValue:
		return doAdd(base, param), false
	case MutAnd:
		return doAnd(base, param), false
	case MutAndV2:
		return doAndV2(base, param), false
	case MutOr:
		return doOr(base, param), false
	case MutXor:
		return doXor(base, param), false
	case MutMax:
		return doMax(base, param), false
	case MutMin:
		return doMin(base, param), false
	case MutMinV2:
		return doMinV2(base, param), false
	case MutByteMax:
		return doByteMax(base, param), false
	case MutByteMin:
		return doByteMin(base, param), false
	case MutAppendIfFits:
		return doAppendIfFits(base, param), false
	case MutCompareAndClear:
		return doCompareAndClear(base, param)
	default:
		// Versionstamped mutations can't be resolved client-side.
		if base != nil {
			return append([]byte(nil), base...), false
		}
		return nil, false
	}
}

// existing returns the "present" value for C++ Optional<ValueRef> semantics.
// nil → empty StringRef (for operations that treat absent as empty).
func existing(base []byte) []byte {
	if base == nil {
		return []byte{}
	}
	return base
}

// doAdd — C++ doAdd in Atomic.h. Little-endian addition.
// Result length = len(param), matching C++ which allocates otherOperand.size().
// Base bytes beyond len(param) are silently dropped (carry discarded).
func doAdd(base, param []byte) []byte {
	e := existing(base)
	size := len(param)
	if size == 0 {
		return []byte{}
	}
	a := make([]byte, size)
	copy(a, e) // zero-pads if len(e) < size; truncates if len(e) > size
	b := make([]byte, size)
	copy(b, param)

	if size == 8 {
		result := make([]byte, 8)
		binary.LittleEndian.PutUint64(result, binary.LittleEndian.Uint64(a)+binary.LittleEndian.Uint64(b))
		return result
	}
	result := make([]byte, size)
	var carry uint16
	for i := 0; i < size; i++ {
		sum := carry + uint16(a[i]) + uint16(b[i])
		result[i] = byte(sum)
		carry = sum >> 8
	}
	return result
}

// doAnd — C++ doAnd in Atomic.h. Result length = param length. Missing base → 0x00.
func doAnd(base, param []byte) []byte {
	e := existing(base)
	if len(param) == 0 {
		return []byte{}
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] & param[i]
	}
	// Remaining positions: 0x00 (base bytes beyond existing are 0, 0 & anything = 0)
	return result
}

// doAndV2 — C++ doAndV2. If absent → return param. Otherwise → doAnd.
func doAndV2(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	return doAnd(base, param)
}

// doOr — C++ doOr. Result length = param length. Missing base → 0x00.
func doOr(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] | param[i]
	}
	for i := minLen; i < len(param); i++ {
		result[i] = param[i]
	}
	return result
}

// doXor — C++ doXor. Result length = param length. Missing base → 0x00.
func doXor(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] ^ param[i]
	}
	for i := minLen; i < len(param); i++ {
		result[i] = param[i]
	}
	return result
}

// doMax — C++ doMax. Little-endian unsigned compare. Result length = param length.
func doMax(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	// Compare from MSB of param down. Extra param bytes beyond existing treated as > 0.
	for i := len(param) - 1; i >= len(e); i-- {
		if param[i] != 0 {
			return append([]byte(nil), param...)
		}
	}
	for i := min(len(e), len(param)) - 1; i >= 0; i-- {
		if param[i] > e[i] {
			return append([]byte(nil), param...)
		} else if param[i] < e[i] {
			// Return existing truncated/zero-padded to param length.
			result := make([]byte, len(param))
			copy(result, e)
			return result
		}
	}
	return append([]byte(nil), param...)
}

// doMin — C++ doMin. Little-endian unsigned compare. Result length = param length.
func doMin(base, param []byte) []byte {
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	e := existing(base)
	// Compare from MSB of param down.
	for i := len(param) - 1; i >= len(e); i-- {
		if param[i] != 0 {
			result := make([]byte, len(param))
			copy(result, e)
			return result
		}
	}
	for i := min(len(e), len(param)) - 1; i >= 0; i-- {
		if param[i] > e[i] {
			result := make([]byte, len(param))
			copy(result, e)
			return result
		} else if param[i] < e[i] {
			return append([]byte(nil), param...)
		}
	}
	return append([]byte(nil), param...)
}

// doMinV2 — C++ doMinV2. If absent → return param. Otherwise → doMin.
func doMinV2(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	return doMin(base, param)
}

// doByteMax — C++ doByteMax. Lexicographic (big-endian byte) comparison.
func doByteMax(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(base, param) > 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// doByteMin — C++ doByteMin. Lexicographic comparison.
func doByteMin(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(base, param) < 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// doAppendIfFits — C++ doAppendIfFits. Concatenates if within 100KB limit.
func doAppendIfFits(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), e...)
	}
	if len(e)+len(param) > valueSizeLimit { // CLIENT_KNOBS->VALUE_SIZE_LIMIT (pkg const)
		return append([]byte(nil), e...)
	}
	result := make([]byte, len(e)+len(param))
	copy(result, e)
	copy(result[len(e):], param)
	return result
}

// doCompareAndClear — C++ doCompareAndClear. If absent or equal → clear.
func doCompareAndClear(base, param []byte) ([]byte, bool) {
	if base == nil || bytes.Equal(base, param) {
		return nil, true // Clear the value.
	}
	return append([]byte(nil), base...), false // No change.
}

// addUnreadableRangeLocked merges [begin, end) into the SVK candidate-stamp
// unreadable ranges (sorted, non-overlapping; same shape as `cleared`).
// C++ writes.addUnmodifiedAndUnreadableRange (ReadYourWrites.actor.cpp:2271).
func (c *rywCache) addUnreadableRangeLocked(begin, end []byte) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	n := len(c.unreadableRanges)
	hiIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(c.unreadableRanges[i].begin, end) > 0
	})
	loIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(c.unreadableRanges[i].end, begin) >= 0
	})
	newBegin := append([]byte(nil), begin...)
	newEnd := append([]byte(nil), end...)
	for i := loIdx; i < hiIdx; i++ {
		if bytes.Compare(c.unreadableRanges[i].begin, newBegin) < 0 {
			newBegin = c.unreadableRanges[i].begin
		}
		if bytes.Compare(c.unreadableRanges[i].end, newEnd) > 0 {
			newEnd = c.unreadableRanges[i].end
		}
	}
	merged := append([]rywRange(nil), c.unreadableRanges[:loIdx]...)
	merged = append(merged, rywRange{begin: newBegin, end: newEnd})
	merged = append(merged, c.unreadableRanges[hiIdx:]...)
	c.unreadableRanges = merged
}

// subtractRangeList removes [begin, end) from a sorted, non-overlapping range
// list, splitting ranges that straddle the span. Used to make cleared spans
// readable again (C++ gets this free from the shared PTree: clear() inserts
// readable entries over the span, WriteMap.cpp:195).
func subtractRangeList(ranges []rywRange, begin, end []byte) []rywRange {
	if len(ranges) == 0 || bytes.Compare(begin, end) >= 0 {
		return ranges
	}
	out := make([]rywRange, 0, len(ranges)+1)
	for _, r := range ranges {
		// No overlap: keep as-is.
		if bytes.Compare(r.end, begin) <= 0 || bytes.Compare(r.begin, end) >= 0 {
			out = append(out, r)
			continue
		}
		// Left remainder.
		if bytes.Compare(r.begin, begin) < 0 {
			out = append(out, rywRange{begin: r.begin, end: append([]byte(nil), begin...)})
		}
		// Right remainder.
		if bytes.Compare(r.end, end) > 0 {
			out = append(out, rywRange{begin: append([]byte(nil), end...), end: r.end})
		}
	}
	return out
}

// isUnreadableLocked reports whether key falls inside an SVK candidate-stamp
// unreadable range. Caller holds c.mu.
func (c *rywCache) isUnreadableLocked(key []byte) bool {
	// First range with end > key; key is inside iff that range's begin <= key.
	i := sort.Search(len(c.unreadableRanges), func(i int) bool {
		return bytes.Compare(c.unreadableRanges[i].end, key) > 0
	})
	return i < len(c.unreadableRanges) && bytes.Compare(c.unreadableRanges[i].begin, key) <= 0
}

// firstUnreadableInLocked returns the begin of the first unreadable range
// intersecting [begin, end), or nil. Drives GetRange REACH semantics: a scan
// throws only when iteration reaches the unreadable segment
// (ReadYourWrites.actor.cpp:685 limit-break precedes the :692 throw).
// Caller holds c.mu.
func (c *rywCache) firstUnreadableInLocked(begin, end []byte) []byte {
	i := sort.Search(len(c.unreadableRanges), func(i int) bool {
		return bytes.Compare(c.unreadableRanges[i].end, begin) > 0
	})
	if i < len(c.unreadableRanges) && bytes.Compare(c.unreadableRanges[i].begin, end) < 0 {
		// The intersection starts at max(range.begin, begin).
		if bytes.Compare(c.unreadableRanges[i].begin, begin) > 0 {
			return c.unreadableRanges[i].begin
		}
		return begin
	}
	return nil
}

// lastUnreadableInLocked returns the (exclusive) end of the last unreadable
// range intersecting [begin, end), or nil — the reverse-scan counterpart of
// firstUnreadableInLocked, using the same binary search (ranges are sorted
// and non-overlapping, so only the last range with r.begin < end can
// intersect). Caller holds c.mu.
func (c *rywCache) lastUnreadableInLocked(begin, end []byte) []byte {
	i := sort.Search(len(c.unreadableRanges), func(i int) bool {
		return bytes.Compare(c.unreadableRanges[i].begin, end) >= 0
	}) - 1
	if i < 0 || bytes.Compare(c.unreadableRanges[i].end, begin) <= 0 {
		return nil
	}
	if bytes.Compare(c.unreadableRanges[i].end, end) < 0 {
		return c.unreadableRanges[i].end
	}
	return end
}

// chainHasVersionstamp reports whether an unresolved atomic chain contains a
// versionstamped op (the condition that makes the entry unreadable).
func chainHasVersionstamp(atomics []rywMutation) bool {
	for _, m := range atomics {
		if isUnresolvedVersionstamp(m.typ) {
			return true
		}
	}
	return false
}

// resolveAtomicsBypass resolves a chain CONTAINING versionstamped ops for a
// BYPASS_UNREADABLE read: each versionstamped op applies as a plain Set of its
// operand exactly as written — placeholder bytes unfilled, trailing 4-byte
// offset suffix INCLUDED (C++ kv() under bypass returns the write-map value;
// the RYWIterator.cpp:433-449 unit pins kv->value == metadataVersionRequiredValue,
// all 14 bytes). Mirrors resolveAtomics for everything else. Never cached.
func resolveAtomicsBypass(base []byte, atomics []rywMutation) (result []byte, cleared bool) {
	for _, m := range atomics {
		if isUnresolvedVersionstamp(m.typ) {
			base = m.param
			cleared = false
			continue
		}
		val, clr := applyAtomic(m.typ, base, m.param)
		if !clr && val == nil {
			val = []byte{} // present-empty, matching resolveAtomics normalization
		}
		base = val
		cleared = clr
	}
	return base, cleared
}

// unreadableScanCapLocked computes how far a scan over [begin, end) may
// iterate before REACHING unreadable state (an SVK candidate range or an
// entry with a pending versionstamped op / sticky unreadable flag): the cap
// is an exclusive end bound for forward scans, an inclusive begin bound for
// reverse scans, or nil when nothing unreadable intersects. Reach semantics:
// C++ throws only when the iterator lands on the segment
// (ReadYourWrites.actor.cpp:685 limit-break precedes the :692 throw), so the
// caller emits results inside the capped window and errors only if iteration
// would cross the cap. Caller holds c.mu.
func (c *rywCache) unreadableScanCapLocked(begin, end []byte, reverse bool) []byte {
	// Fast path: no unreadable state at all — the overwhelmingly common case.
	// This runs on EVERY getRange, so it must not touch sortedKeys
	// (ensureSortedLocked rebuilds O(N log N) after every write invalidation;
	// per-read rebuilding made interleaved write/read transactions quadratic).
	// Unreadable ENTRIES are found via the dedicated sorted unreadableKeys
	// index, maintained at the flag transitions.
	if len(c.unreadableKeys) == 0 && len(c.unreadableRanges) == 0 {
		return nil
	}
	var cap_ []byte
	if !reverse {
		cap_ = c.firstUnreadableInLocked(begin, end)
		// Smallest unreadable entry key in [begin, end).
		if i := sort.SearchStrings(c.unreadableKeys, string(begin)); i < len(c.unreadableKeys) && c.unreadableKeys[i] < string(end) {
			if k := c.unreadableKeys[i]; cap_ == nil || k < string(cap_) {
				cap_ = []byte(k)
			}
		}
		return cap_
	}
	cap_ = c.lastUnreadableInLocked(begin, end)
	// Largest unreadable entry key in [begin, end); its exclusive upper bound
	// (key+\x00) becomes the reverse scan's begin cap. Only the LARGEST key
	// matters: a reverse scan iterates downward from end, so the highest
	// unreadable key is the first one it can reach — any lower entries sit
	// behind it (the forward path symmetrically needs only the smallest).
	if i := sort.SearchStrings(c.unreadableKeys, string(end)) - 1; i >= 0 && c.unreadableKeys[i] >= string(begin) {
		kb := append([]byte(c.unreadableKeys[i]), 0)
		if cap_ == nil || string(kb) > string(cap_) {
			cap_ = kb
		}
	}
	return cap_
}

// insertUnreadableKeyLocked adds k to the sorted unreadableKeys index (no-op
// if present). Called when an entry's unreadable flag transitions on.
func (c *rywCache) insertUnreadableKeyLocked(k string) {
	i := sort.SearchStrings(c.unreadableKeys, k)
	if i < len(c.unreadableKeys) && c.unreadableKeys[i] == k {
		return
	}
	c.unreadableKeys = append(c.unreadableKeys, "")
	copy(c.unreadableKeys[i+1:], c.unreadableKeys[i:])
	c.unreadableKeys[i] = k
}

// removeUnreadableKeyLocked drops k from the unreadableKeys index. Called when
// an unreadable entry is deleted (clear/clearRange — the only flag-off paths).
func (c *rywCache) removeUnreadableKeyLocked(k string) {
	i := sort.SearchStrings(c.unreadableKeys, k)
	if i < len(c.unreadableKeys) && c.unreadableKeys[i] == k {
		c.unreadableKeys = append(c.unreadableKeys[:i], c.unreadableKeys[i+1:]...)
	}
}

// addUnreadableRange marks [begin, end) unreadable (SVK candidate stamp
// range, RFC-098).
func (c *rywCache) addUnreadableRange(begin, end []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addUnreadableRangeLocked(begin, end)
}

// setBypassUnreadable mirrors FDB_TR_OPTION_BYPASS_UNREADABLE.
func (c *rywCache) setBypassUnreadable(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bypassUnreadable = v
}
