package client

import (
	"bytes"
	"slices"
	"sort"

	"fdb.dev/pkg/fdbgo/wire"
)

// specialkeys.go is the read side of the special key space, \xff\xff..\xff\xff\xff, for
// what the TRANSACTION module serves: SpecialKeySpace::get / getRange and their selector
// normalization (fdbclient/SpecialKeySpace.actor.cpp:120-493, FoundationDB 7.3.77), the
// module table (:61-80) and ConflictingKeysImpl (:711-729). Line numbers below are that
// file (SKS), ReadYourWrites.actor.cpp (RYW) and NativeAPI.actor.cpp (NAPI).
//
// Of the TRANSACTION module's impls (NAPI:1745-1753) only conflicting_keys/ is served.
// read_conflict_range/ and write_conflict_range/ render the RYW conflict maps
// (RYW:2121-2215), and every other module needs cluster or process state; a read that
// would call any of those impls fails with unsupported_operation (2108) instead of
// returning what libfdb_c would not (DIVERGENCES.md).

// The special key space read errors and illegal_tenant_access (error_definitions.h:251-252,
// 273); unreached impls fail with ErrUnsupportedOperation (2108).
const (
	ErrSpecialKeysCrossModuleRead = 2112
	ErrSpecialKeysNoModuleFound   = 2113
	ErrIllegalTenantAccess        = 2138
)

var (
	specialKeysBegin = []byte("\xff\xff") // specialKeys, SystemData.cpp:36
	specialKeysEnd   = []byte("\xff\xff\xff")

	// conflictingKeysRange, SystemData.cpp:295-298: the map's keys and its two values.
	conflictingKeysBegin = []byte("\xff\xff/transaction/conflicting_keys/")
	conflictingKeysEnd   = []byte("\xff\xff/transaction/conflicting_keys/\xff\xff")
	conflictingKeysTrue  = []byte("1")
	conflictingKeysFalse = []byte("0")
)

// skTooMany is CLIENT_KNOBS->TOO_MANY (ClientKnobs.cpp:38), SKS get's row limit (:467-474).
const skTooMany = 1000000

// skModule is SpecialKeySpace::MODULE (SpecialKeySpace.actor.h:168-185).
type skModule uint8

const (
	skModuleUnknown skModule = iota
	skModuleTransaction
	skModuleOther // a module whose impls are not served here
)

// skModuleBoundaries is moduleToBoundary (SKS:61-80).
var skModuleBoundaries = []struct {
	name       string
	begin, end string
}{
	{"transaction", "\xff\xff/transaction/", "\xff\xff/transaction0"},
	{"worker_interfaces", "\xff\xff/worker_interfaces/", "\xff\xff/worker_interfaces0"},
	{"status_json", "\xff\xff/status/json", "\xff\xff/status/json\x00"},
	{"connection_string", "\xff\xff/connection_string", "\xff\xff/connection_string\x00"},
	{"cluster_file_path", "\xff\xff/cluster_file_path", "\xff\xff/cluster_file_path\x00"},
	{"metrics", "\xff\xff/metrics/", "\xff\xff/metrics0"},
	{"management", "\xff\xff/management/", "\xff\xff/management0"},
	{"error_message", "\xff\xff/error_message", "\xff\xff/error_message\x00"},
	{"configuration", "\xff\xff/configuration/", "\xff\xff/configuration0"},
	{"global_config", "\xff\xff/global_config/", "\xff\xff/global_config0"},
	{"tracing", "\xff\xff/tracing/", "\xff\xff/tracing0"},
	{"actor_lineage", "\xff\xff/actor_lineage/", "\xff\xff/actor_lineage0"},
	{"actor_profiler_conf", "\xff\xff/actor_profiler_conf/", "\xff\xff/actor_profiler_conf0"},
	{"cluster_id", "\xff\xff/cluster_id", "\xff\xff/cluster_id\x00"},
}

// skReadImpl is a SpecialKeyRangeReadImpl. read nil marks a range whose impls are not
// served here; reaching it is unsupported_operation.
type skReadImpl struct {
	name            string
	supportsTenants bool // SpecialKeySpace.actor.h:63, 306, 315, 324
	read            func(tx *Transaction, begin, end []byte) []KeyValue
}

func (impl *skReadImpl) unsupported() error {
	if impl.read == nil {
		return &wire.FDBError{Code: ErrUnsupportedOperation}
	}
	return nil
}

// skRangeMap is a RangeMap<Key, V> (fdbrpc/RangeMap.h): boundary entries sorted by key,
// entry i covering [entries[i].key, entries[i+1].key); the last entry is the map end.
type skRangeMap[V comparable] struct {
	entries []skRangeEntry[V]
}

type skRangeEntry[V comparable] struct {
	key   []byte
	value V
}

func newSKRangeMap[V comparable](end []byte) *skRangeMap[V] {
	var zero V
	return &skRangeMap[V]{entries: []skRangeEntry[V]{{key: []byte{}, value: zero}, {key: end, value: zero}}}
}

func (m *skRangeMap[V]) lowerBound(k []byte) int {
	return sort.Search(len(m.entries), func(i int) bool { return bytes.Compare(m.entries[i].key, k) >= 0 })
}

// rangeContaining is RangeMap.h:155-158 (lastLessOrEqual).
func (m *skRangeMap[V]) rangeContaining(k []byte) int {
	return sort.Search(len(m.entries), func(i int) bool { return bytes.Compare(m.entries[i].key, k) > 0 }) - 1
}

// rangeContainingKeyBefore is RangeMap.h:165-171. A key past the map end (an end selector
// keyAfter(\xff\xff\xff), whose C++ lower_bound is end()) gets the last range.
func (m *skRangeMap[V]) rangeContainingKeyBefore(k []byte) int {
	i := min(m.lowerBound(k), len(m.entries)-1)
	if len(m.entries[i].key) == 0 {
		return i
	}
	return i - 1
}

// insert is RangeMap::insert (RangeMap.h:307-324), which does not coalesce.
func (m *skRangeMap[V]) insert(begin, end []byte, v V) {
	if bytes.Equal(begin, end) {
		return
	}
	e := m.lowerBound(end)
	if !bytes.Equal(m.entries[e].key, end) {
		m.entries = slices.Insert(m.entries, e, skRangeEntry[V]{key: end, value: m.entries[e-1].value})
	}
	b := m.lowerBound(begin)
	m.entries = slices.Replace(m.entries, b, e, skRangeEntry[V]{key: begin, value: v})
}

func skContains(begin, end, k []byte) bool {
	return bytes.Compare(begin, k) <= 0 && bytes.Compare(k, end) < 0
}

var (
	// skModules is SpecialKeySpace::modules, skReadImpls its readImpls, as the
	// constructor and modulesBoundaryInit (SKS:267-295) and the registrations at API
	// version >= 630 (NAPI:1745-1753) leave them.
	skModules   = newSKRangeMap[skModule](specialKeysEnd)
	skReadImpls = newSKRangeMap[*skReadImpl](specialKeysEnd)
)

func init() {
	skReadImpls.insert(specialKeysBegin, specialKeysEnd, nil)
	for _, mb := range skModuleBoundaries {
		module := skModuleOther
		if mb.name == "transaction" {
			module = skModuleTransaction
		}
		skModules.insert([]byte(mb.begin), []byte(mb.end), module)
		skReadImpls.insert([]byte(mb.begin), []byte(mb.end), nil)
	}
	skReadImpls.insert(conflictingKeysBegin, conflictingKeysEnd,
		&skReadImpl{name: "conflicting_keys", supportsTenants: true, read: readConflictingKeys})
	skReadImpls.insert([]byte("\xff\xff/transaction/read_conflict_range/"),
		[]byte("\xff\xff/transaction/read_conflict_range/\xff\xff"),
		&skReadImpl{name: "read_conflict_range", supportsTenants: true})
	skReadImpls.insert([]byte("\xff\xff/transaction/write_conflict_range/"),
		[]byte("\xff\xff/transaction/write_conflict_range/\xff\xff"),
		&skReadImpl{name: "write_conflict_range", supportsTenants: true})
	// One unserved impl over each other module: it covers every range C++ registers there,
	// so no read that reaches a C++ impl of that module is answered here.
	for _, mb := range skModuleBoundaries {
		if mb.name != "transaction" {
			skReadImpls.insert([]byte(mb.begin), []byte(mb.end), &skReadImpl{name: mb.name})
		}
	}
}

// conflictingKeyMap is TransactionState::conflictingKeys (NativeAPI.actor.h:307), a
// CoalescedKeyRangeMap<Value>("0", specialKeys.end) holding "1" over each conflicting
// read conflict range. Coalesced inserts of one value over a default leave the sorted
// union of the ranges, so the map is its boundaries: "1" where a merged range begins,
// "0" where it ends (KeyRangeMap.h:206-253). Immutable once built.
type conflictingKeyMap struct {
	entries []KeyValue // boundaries in key order, all inside conflictingKeysRange's prefix
}

// newConflictingKeyMap is the not_committed arm of tryCommit (NAPI:6700-6717): each
// distinct index names a range of the request's read_conflict_ranges, prefixed with
// conflictingKeysRange.begin. reads are the shipped ranges without the tenant prefix,
// which C++ removes before prefixing (:6714-6715). The proxy only sends indices it
// mapped from the request (CommitProxyServer.actor.cpp:2448-2462); one outside it is
// dropped rather than read out of bounds.
func newConflictingKeyMap(indices []int32, reads []KeyRange) *conflictingKeyMap {
	seen := make(map[int32]struct{}, len(indices))
	ranges := make([]KeyRange, 0, len(indices))
	for _, i := range indices {
		if _, dup := seen[i]; dup || i < 0 || int(i) >= len(reads) {
			continue
		}
		seen[i] = struct{}{}
		kr := reads[i]
		if bytes.Compare(kr.Begin, kr.End) >= 0 {
			continue // an empty insert changes nothing (KeyRangeMap.h:209-210)
		}
		ranges = append(ranges, KeyRange{
			Begin: append(bytes.Clone(conflictingKeysBegin), kr.Begin...),
			End:   append(bytes.Clone(conflictingKeysBegin), kr.End...),
		})
	}
	slices.SortFunc(ranges, func(a, b KeyRange) int { return bytes.Compare(a.Begin, b.Begin) })
	m := &conflictingKeyMap{}
	for _, kr := range ranges {
		if n := len(m.entries); n > 0 && bytes.Compare(kr.Begin, m.entries[n-1].Key) <= 0 {
			// Overlaps or touches the previous range: equal neighbours coalesce.
			if bytes.Compare(kr.End, m.entries[n-1].Key) > 0 {
				m.entries[n-1].Key = kr.End
			}
			continue
		}
		m.entries = append(m.entries,
			KeyValue{Key: kr.Begin, Value: conflictingKeysTrue},
			KeyValue{Key: kr.End, Value: conflictingKeysFalse})
	}
	return m
}

// readConflictingKeys is ConflictingKeysImpl::getRange (SKS:711-729): the map's
// boundaries whose key lies in [begin, end), nothing when no conflict was reported.
func readConflictingKeys(tx *Transaction, begin, end []byte) []KeyValue {
	m := tx.conflictingKeys.Load()
	if m == nil {
		return nil
	}
	lo := sort.Search(len(m.entries), func(i int) bool { return bytes.Compare(m.entries[i].Key, begin) >= 0 })
	hi := sort.Search(len(m.entries), func(i int) bool { return bytes.Compare(m.entries[i].Key, end) >= 0 })
	return m.entries[lo:hi:hi]
}

// specialKeysRouted is RYW's apiVersionAtLeast(630) gate (RYW:1634, 1716). Below it C++
// answers a few literal keys instead (:1640-1672, 1722-1729); those stay unrouted here.
func (tx *Transaction) specialKeysRouted() bool {
	return tx.db != nil && tx.db.apiVersion >= 630
}

// routesSpecialKey is RYW::get's route (RYW:1634-1638).
func (tx *Transaction) routesSpecialKey(key []byte) bool {
	return tx.specialKeysRouted() && skContains(specialKeysBegin, specialKeysEnd, key)
}

// routesSpecialKeyRange is RYW::getRange's route on the selectors' keys (RYW:1716-1721).
func (tx *Transaction) routesSpecialKeyRange(beginKey, endKey []byte) bool {
	return tx.specialKeysRouted() && skContains(specialKeysBegin, specialKeysEnd, beginKey) &&
		bytes.Compare(specialKeysBegin, endKey) <= 0 && bytes.Compare(endKey, specialKeysEnd) <= 0
}

// specialKeyEntryError is ThreadSafeTransaction's checkDeferredError ahead of the routed
// call (ThreadSafeTransaction.cpp:431, 476, 491). The routed read runs none of RYW's
// other entry checks, adds no read conflict and is not part of `reading`.
func (tx *Transaction) specialKeyEntryError() error {
	if e := tx.deferredErr.Load(); e != nil {
		return e
	}
	return nil
}

// noopRelease is a routed range read's release: it holds no operation lease.
func noopRelease() {}

// getSpecialKey is a routed get: SpecialKeySpace::getActor (SKS:474-489), a getRange of
// [key, keyAfter(key)).
func (tx *Transaction) getSpecialKey(key []byte) ([]byte, error) {
	if err := tx.specialKeyEntryError(); err != nil {
		return nil, err
	}
	limits := rangeLimits{rows: skTooMany, minRows: 1, bytes: byteLimitUnlimited}
	kvs, _, err := tx.skGetRange(firstGreaterOrEqual(key), firstGreaterOrEqual(keyAfterSelectorKey(key)), limits, false)
	if err != nil || len(kvs) == 0 {
		return nil, err
	}
	return bytes.Clone(kvs[0].Value), nil
}

// specialKeyRange is a routed getRange with the C binding's limits (fdb_c.cpp:989-994:
// 0 is unlimited). The rows are the caller's.
func (tx *Transaction) specialKeyRange(begin, end KeySelector, limit, byteTarget int, reverse bool) ([]KeyValue, bool, error) {
	if err := tx.specialKeyEntryError(); err != nil {
		return nil, false, err
	}
	kvs, more, err := tx.skGetRange(begin, end, newRangeLimits(limit, byteTarget), reverse)
	return cloneReadKVs(kvs), more, err
}

// skGetRange is SpecialKeySpace::getRange (SKS:450-472).
func (tx *Transaction) skGetRange(begin, end KeySelector, limits rangeLimits, reverse bool) ([]KeyValue, bool, error) {
	if !limits.isValid() {
		return nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid}
	}
	if limits.isReached() {
		return nil, false, nil
	}
	begin, end = begin.removeOrEqual(), end.removeOrEqual()
	if begin.Offset >= end.Offset && bytes.Compare(begin.Key, end.Key) >= 0 {
		return nil, false, nil
	}
	return tx.skAggregate(begin, end, limits, reverse)
}

// skAggregate is getRangeAggregationActor (SKS:313-448).
func (tx *Transaction) skAggregate(begin, end KeySelector, limits rangeLimits, reverse bool) ([]KeyValue, bool, error) {
	boundBegin, boundEnd := specialKeysBegin, specialKeysEnd
	if !tx.specialKeySpaceRelaxed {
		i := skModules.rangeContaining(begin.Key)
		mb, me := skModules.entries[i].key, skModules.entries[i+1].key
		if bytes.Compare(mb, end.Key) > 0 || bytes.Compare(end.Key, me) > 0 {
			return nil, false, &wire.FDBError{Code: ErrSpecialKeysCrossModuleRead}
		}
		if skModules.entries[i].value == skModuleUnknown {
			return nil, false, &wire.FDBError{Code: ErrSpecialKeysNoModuleFound}
		}
		boundBegin, boundEnd = mb, me
	}
	beginOffset, err := tx.skNormalize(&begin, boundBegin, boundEnd)
	if err != nil {
		return nil, false, err
	}
	endOffset, err := tx.skNormalize(&end, boundBegin, boundEnd)
	if err != nil {
		return nil, false, err
	}
	if beginOffset >= endOffset && bytes.Compare(begin.Key, end.Key) >= 0 { // :353-357
		return nil, false, nil
	}
	if bytes.Equal(begin.Key, boundEnd) || bytes.Equal(end.Key, boundBegin) { // :358-362
		return nil, false, nil
	}
	if bytes.Compare(begin.Key, end.Key) >= 0 {
		return nil, false, nil // an empty [begin, end) reads nothing from any impl
	}
	lo, hi := skReadImpls.rangeContaining(begin.Key), skReadImpls.lowerBound(end.Key) // :363-364
	if tx.tenantId != NoTenantID {                                                    // :366-380
		for i := lo; i < hi; i++ {
			impl := skReadImpls.entries[i].value
			if impl == nil {
				continue
			}
			if err := impl.unsupported(); err != nil {
				return nil, false, err
			}
			if !impl.supportsTenants {
				return nil, false, &wire.FDBError{Code: ErrIllegalTenantAccess}
			}
		}
	}
	var result []KeyValue
	visit := func(i int) (bool, error) {
		impl := skReadImpls.entries[i].value
		if impl == nil {
			return false, nil
		}
		if err := impl.unsupported(); err != nil {
			return false, err
		}
		kb, ke := skReadImpls.entries[i].key, skReadImpls.entries[i+1].key
		keyStart, keyEnd := kb, ke
		if skContains(kb, ke, begin.Key) {
			keyStart = begin.Key
		}
		if skContains(kb, ke, end.Key) {
			keyEnd = end.Key
		}
		pairs := impl.read(tx, keyStart, keyEnd)
		for j := range pairs {
			if reverse {
				j = len(pairs) - 1 - j
			}
			result = append(result, pairs[j])
			limits.decrement(1, 8+len(pairs[j].Key)+len(pairs[j].Value)) // NAPI:2823-2829
			if limits.isReached() {
				return true, nil
			}
		}
		return false, nil
	}
	for k := 0; k < hi-lo; k++ { // :384-447, iterating the intersecting ranges
		i := lo + k
		if reverse {
			i = hi - 1 - k
		}
		reached, err := visit(i)
		if err != nil {
			return nil, false, err
		}
		if reached {
			return result, true, nil
		}
	}
	return result, false, nil
}

// skNormalize is normalizeKeySelectorActor (SKS:216-265): it moves ks to the standard
// form over the impls inside the boundary, clamping to the boundary when it runs out.
// It returns the offset left before the clamp.
func (tx *Transaction) skNormalize(ks *KeySelector, boundBegin, boundEnd []byte) (int32, error) {
	var i int
	if ks.Offset < 1 {
		i = skReadImpls.rangeContainingKeyBefore(ks.Key)
	} else {
		i = skReadImpls.rangeContaining(ks.Key)
	}
	for (ks.Offset < 1 && bytes.Compare(skReadImpls.entries[i].key, boundBegin) >= 0) ||
		(ks.Offset > 1 && bytes.Compare(skReadImpls.entries[i].key, boundEnd) < 0) {
		if impl := skReadImpls.entries[i].value; impl != nil {
			if err := tx.skMoveOver(impl, skReadImpls.entries[i].key, skReadImpls.entries[i+1].key, ks); err != nil {
				return 0, err
			}
		}
		if ks.Offset < 1 {
			if i == 0 {
				break
			}
			i--
		} else if ks.Offset > 1 {
			i++
		}
	}
	actual := ks.Offset
	if !ks.isFirstGreaterOrEqual() {
		if ks.Offset < 1 {
			ks.Key = boundBegin
		} else {
			ks.Key = boundEnd
		}
		ks.Offset = 1
	}
	return actual, nil
}

// skMoveOver is moveKeySelectorOverRangeActor (SKS:128-206) over the impl on [kb, ke).
func (tx *Transaction) skMoveOver(impl *skReadImpl, kb, ke []byte, ks *KeySelector) error {
	if err := impl.unsupported(); err != nil {
		return err
	}
	if tx.tenantId != NoTenantID && !impl.supportsTenants {
		return &wire.FDBError{Code: ErrIllegalTenantAccess}
	}
	start, end := kb, ke
	if skContains(kb, ke, ks.Key) {
		if ks.Offset < 1 {
			end = ks.Key
		} else {
			start = ks.Key
		}
	}
	pairs := impl.read(tx, start, end)
	n := int64(len(pairs))
	if n == 0 {
		return nil
	}
	off := int64(ks.Offset)
	if off < 1 {
		if n >= 1-off {
			ks.Key, ks.Offset = pairs[n-(1-off)].Key, 1
		} else {
			ks.Key, ks.Offset = pairs[0].Key, int32(off+n)
		}
	} else {
		if n >= off {
			ks.Key, ks.Offset = pairs[off-1].Key, 1
		} else {
			ks.Key, ks.Offset = keyAfterSelectorKey(pairs[n-1].Key), int32(off-n)
		}
	}
	return nil
}

// SetReportConflictingKeys is REPORT_CONFLICTING_KEYS (712): the next commit asks the
// proxy for the read conflict ranges that conflicted (NAPI:7135-7137, 6887-6889), which
// a not_committed then leaves readable under \xff\xff/transaction/conflicting_keys/.
// Not persistent (fdb.options:327): reset and OnError restore the database default.
func (tx *Transaction) SetReportConflictingKeys() {
	lease := tx.enterState()
	defer lease.release()
	tx.reportConflictingKeys = true
}

// SetSpecialKeySpaceRelaxed is SPECIAL_KEY_SPACE_RELAXED (RYW:2603-2605): a special key
// range read may span modules or none (SKS:331-352). Not persistent.
func (tx *Transaction) SetSpecialKeySpaceRelaxed() {
	lease := tx.enterState()
	defer lease.release()
	tx.specialKeySpaceRelaxed = true
}

// SetTransactionReportConflictingKeys is the database option
// TRANSACTION_REPORT_CONFLICTING_KEYS (702, defaultFor 712): every transaction, and
// every reset or retry of one, starts with SetReportConflictingKeys
// (ReadYourWrites.actor.cpp:1553-1556, 2722-2723, 2751-2754).
func (d *Database) SetTransactionReportConflictingKeys() {
	d.db.mutateDefaults(func(td *TransactionDefaults) { td.ReportConflictingKeys = true })
}
