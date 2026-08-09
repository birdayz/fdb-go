package client

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// getMappedRange — the client half of fdb_transaction_get_mapped_range
// (bindings/c/fdb_c.cpp:1063). A mapped range is an ordinary range read over a
// PRIMARY range (typically index entries) where the storage server additionally
// resolves each row through a `mapper` tuple and returns the resolved
// SECONDARY read alongside it, saving the client a round trip per row.
//
// Every C++ line number cited in this file and its tests is 7.3.77, the version
// the test containers run. Where the reference source read during the port was
// the 7.3.75 tag, the two were checked rather than assumed equivalent: for the
// mapped-range surface — storageserver.actor.cpp, ReadYourWrites.actor.cpp,
// fdb_c.cpp, fdb_c.h, StorageServerInterface.h, FDBTypes.h and
// error_definitions.h — `git diff 7.3.75 7.3.77` is EMPTY, so the line numbers
// and the behaviour are the same text. NativeAPI.actor.cpp is the one cited file
// that differs, and only in waitStorageMetrics trace severity around line 8022,
// below everything referenced here; the mapped-range knobs (QUICK_GET_*_FALLBACK,
// MAX_PARALLEL_QUICK_GET_VALUE, STRICTLY_ENFORCE_BYTE_LIMIT) are unchanged too.
//
// Two facts about 7.3.77 shape everything here, and both were established by
// reading the C++ rather than assumed:
//
//   - The mapper is NOT parsed, validated, or substituted on the client. fdb_c
//     forwards the bytes verbatim (fdb_c.cpp:1088), NativeAPI copies them onto
//     the request (NativeAPI.actor.cpp:4285), and every mapper error —
//     mapper_not_tuple (2043), mapper_bad_index (2030), mapper_no_such_key
//     (2031), mapper_bad_range_decriptor (2032) — is raised by the storage
//     server (fdbserver/storageserver.actor.cpp:4875-4951, :5972). So this
//     client ships the mapper opaquely and its only obligation is to propagate
//     the server's error UNCHANGED. Adding a Go-side mapper parser would be a
//     divergence, not a hardening.
//
//   - Each returned row carries a UNION whose two arms are the two kinds of
//     secondary read the mapper can express: a point lookup
//     (GetValueReqAndResultRef) or a range scan (GetRangeReqAndResultRef, which
//     the `{...}` range descriptor produces). FDBTypes.h:893.
//
// There is deliberately no MATCH_INDEX support: the MATCH_INDEX_* behaviours do
// not exist in 7.3.77. `fdb_transaction_get_mapped_range` has no matchIndex
// parameter (fdb_c.h:587-603) and GetMappedKeyValuesRequest has no matchIndex
// field (StorageServerInterface.h:463-497), so there is no wire slot to carry
// one. It is a later-version feature.

// MappedResultKind names which arm of the per-row union is populated. It mirrors
// the std::variant index in MappedReqAndResultRef, and the underlying values are
// the flatbuffers union tags (1-based, 0 = unset) so a decode is a direct read.
type MappedResultKind uint8

const (
	// MappedResultNone means the row carried no secondary result at all. The
	// storage server does not produce this, but the tag is representable on the
	// wire (tag 0), so it is named rather than silently conflated with a
	// point-lookup miss — those are different facts.
	MappedResultNone MappedResultKind = 0
	// MappedResultGetValue: the mapper resolved to a single key.
	MappedResultGetValue MappedResultKind = 1
	// MappedResultGetRange: the mapper ended in a "{...}" range descriptor and
	// resolved to a key range.
	MappedResultGetRange MappedResultKind = 2
)

func (k MappedResultKind) String() string {
	switch k {
	case MappedResultNone:
		return "none"
	case MappedResultGetValue:
		return "getValue"
	case MappedResultGetRange:
		return "getRange"
	}
	return fmt.Sprintf("MappedResultKind(%d)", uint8(k))
}

// MappedGetValue is the point-lookup arm (C++ GetValueReqAndResultRef,
// FDBTypes.h:855). Present distinguishes "the mapped key holds an empty value"
// from "the mapped key does not exist" — the C++ field is Optional<ValueRef> and
// collapsing the two would silently invent a record.
type MappedGetValue struct {
	Key     []byte
	Value   []byte
	Present bool
}

// MappedGetRange is the range arm (C++ GetRangeReqAndResultRef,
// FDBTypes.h:873). Begin/End are the selectors the server actually resolved, so
// a range that matched nothing still reports what it probed.
type MappedGetRange struct {
	Begin KeySelector
	End   KeySelector
	Rows  []KeyValue
	More  bool
}

// KeySelector mirrors C++ KeySelectorRef for the secondary range bounds.
type KeySelector struct {
	Key     []byte
	OrEqual bool
	Offset  int32
}

// MappedKeyValue is one row of a mapped range: the primary row (Key/Value —
// the index entry that was scanned) plus the secondary read the mapper resolved
// it to. Mirrors C++ MappedKeyValueRef (FDBTypes.h:895), whose KeyValueRef base
// carries the primary row.
type MappedKeyValue struct {
	Key   []byte
	Value []byte

	Kind     MappedResultKind
	GetValue MappedGetValue
	GetRange MappedGetRange
}

// parseGetMappedKeyValuesReply decodes a GetMappedKeyValuesReply exactly as
// parseGetKeyValuesReply decodes its non-mapped twin, including the inline
// LoadBalancedReply.error arm: the storage server delivers wrong_shard_server
// AND every mapper_* error through that field rather than the ErrorOr root, so
// dropping it here would turn a mapper error into an empty successful read.
func parseGetMappedKeyValuesReply(data []byte) ([]MappedKeyValue, bool, float64, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return nil, false, -1.0, fmt.Errorf("GetMappedKeyValues: %w", err)
	}
	var reply types.GetMappedKeyValuesReply
	reply.UnmarshalFromReader(&r)
	if ferr := wire.ReadInlineReplyError(&r, types.GetMappedKeyValuesReplySlotError); ferr != nil {
		return nil, false, reply.Penalty, ferr
	}

	rows := make([]MappedKeyValue, 0, len(reply.Data))
	for i := range reply.Data {
		rows = append(rows, convertMappedRow(&reply.Data[i]))
	}
	return rows, reply.More, reply.Penalty, nil
}

func convertMappedRow(src *types.MappedKeyValueRef) MappedKeyValue {
	row := MappedKeyValue{
		Key:   src.KeyValue.Key,
		Value: src.KeyValue.Value,
		Kind:  MappedResultKind(src.ReqAndResultTag),
	}
	switch row.Kind {
	case MappedResultGetValue:
		row.GetValue = MappedGetValue{
			Key:     src.ReqAndResultAlt0.Key,
			Value:   src.ReqAndResultAlt0.Result,
			Present: src.ReqAndResultAlt0.HasResult,
		}
	case MappedResultGetRange:
		gr := &src.ReqAndResultAlt1
		row.GetRange = MappedGetRange{
			Begin: KeySelector{Key: gr.Begin.Key, OrEqual: gr.Begin.OrEqual, Offset: gr.Begin.Offset},
			End:   KeySelector{Key: gr.End.Key, OrEqual: gr.End.OrEqual, Offset: gr.End.Offset},
			Rows:  gr.Result.Data,
			More:  gr.Result.More,
		}
	}
	return row
}

// getMappedKeyValuesBufPool is deliberately separate from getKeyValuesBufPool.
// The two request kinds have different size profiles (a mapper tuple rides
// along on every mapped request) and sharing one pool would let mapped traffic
// reshape the buffers the plain read path — the client's hottest path — recycles.
var getMappedKeyValuesBufPool = sync.Pool{
	New: func() any { b := make([]byte, 0, 512); return &b },
}

func buildGetMappedKeyValuesRequest(mapper []byte) func(begin, end []byte, version int64, limit int32, lockAware bool, tenantId int64, span types.SpanContext, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
	return func(begin, end []byte, version int64, limit int32, lockAware bool, tenantId int64, span types.SpanContext, replyToken transport.UID, _ transport.UID) ([]byte, *[]byte) {
		req := types.GetMappedKeyValuesRequest{
			Begin:                  types.KeySelectorRef{Key: begin, OrEqual: false, Offset: 1}, // firstGreaterOrEqual(begin)
			End:                    types.KeySelectorRef{Key: end, OrEqual: false, Offset: 1},   // firstGreaterOrEqual(end)
			Mapper:                 mapper,
			Version:                version,
			Limit:                  limit,
			LimitBytes:             replyByteLimit,
			Reply:                  types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
			TenantInfo:             types.TenantInfo{TenantId: tenantId},
			SpanContext:            span,
			SsLatestCommitVersions: emptyVersionVector,
		}
		if lockAware {
			req.HasOptions = true
			req.Options = lockAwareReadOptions()
		}
		bufp := getMappedKeyValuesBufPool.Get().(*[]byte)
		result := req.MarshalFDBPooled(*bufp)
		*bufp = result
		return result, bufp
	}
}

// mappedRangeScanOps mirrors plainRangeScanOps. `key` returns the PRIMARY row's
// key: the shard-walk bounds it feeds are bounds in the primary range, and a
// secondary key from the mapped side would not even be in that keyspace.
var mappedRangeScanOps = rangeScanOps[MappedKeyValue]{
	key: func(m *MappedKeyValue) []byte { return m.Key },
	sizeBytes: func(m *MappedKeyValue) int64 {
		// The ceiling bounds what the CLIENT materializes, so it must count the
		// secondary payload too — that is the bulk of a mapped reply, and
		// charging only the primary row would let a mapped scan blow through a
		// ceiling the plain path respects.
		n := int64(len(m.Key) + len(m.Value))
		switch m.Kind {
		case MappedResultGetValue:
			n += int64(len(m.GetValue.Key) + len(m.GetValue.Value))
		case MappedResultGetRange:
			n += int64(len(m.GetRange.Begin.Key) + len(m.GetRange.End.Key))
			for i := range m.GetRange.Rows {
				n += int64(len(m.GetRange.Rows[i].Key) + len(m.GetRange.Rows[i].Value))
			}
		}
		return n
	},
}

func (tx *Transaction) sendGetMappedRange(mapper []byte) func(ctx context.Context, begin, end []byte, limit int, reverse bool, servers []ServerInfo) ([]MappedKeyValue, bool, error) {
	return func(ctx context.Context, begin, end []byte, limit int, reverse bool, servers []ServerInfo) ([]MappedKeyValue, bool, error) {
		return sendRangeRPC(tx, ctx, begin, end, limit, reverse, servers, rangeRPCOps[MappedKeyValue]{
			// getMappedKeyValues is adjusted endpoint 14, NOT 3. The field is
			// DECLARED immediately after getKeyValues (StorageServerInterface.h:103)
			// but is assigned getAdjustedEndpoint(14) (:184-185) — it was appended
			// after getKeyValuesStream to keep the older indices stable. Reading the
			// declaration order instead of the assignment would silently address
			// getShardState.
			endpoint: EndpointGetMappedKeyValues,
			build:    buildGetMappedKeyValuesRequest(mapper),
			putBuf:   func(bufp *[]byte) { getMappedKeyValuesBufPool.Put(bufp) },
			parse:    parseGetMappedKeyValuesReply,
		})
	}
}

func (tx *Transaction) getMappedRangeImpl(ctx context.Context, begin, end []byte, mapper []byte, limit int, reverse bool) ([]MappedKeyValue, bool, error) {
	ops := mappedRangeScanOps
	ops.send = tx.sendGetMappedRange(mapper)
	return rangeScanImpl(tx, ctx, begin, end, limit, reverse, ops)
}

// FDB error codes specific to getMappedRange. Source of truth:
// flow/include/flow/error_definitions.h at the 7.3.77 tag.
const (
	// ErrClientInvalidOperation is raised for a mapped read over the special
	// key space, which getMappedRange does not support.
	ErrClientInvalidOperation = 2000 // client_invalid_operation
	// ErrMapperBadIndex ... ErrMapperNotTuple are raised by the STORAGE SERVER,
	// never by this client — the mapper is not parsed here. They are named so a
	// caller can errors.As them and so the propagate-unchanged contract is
	// testable.
	ErrMapperBadIndex          = 2030 // mapper_bad_index
	ErrMapperNoSuchKey         = 2031 // mapper_no_such_key
	ErrMapperBadRangeDescrptor = 2032 // mapper_bad_range_decriptor (sic: the C++ spelling)
	// ErrKeyNotTuple / ErrValueNotTuple are raised when a {K[n]} / {V[n]}
	// placeholder forces the server to unpack a PRIMARY row that is not a packed
	// tuple (unpackKeyTuple / unpackValueTuple, storageserver.actor.cpp:4820-4844).
	// They are properties of the DATA, not of the mapper, so no amount of mapper
	// checking anywhere would pre-empt them.
	ErrKeyNotTuple   = 2041 // key_not_tuple
	ErrValueNotTuple = 2042 // value_not_tuple
	// ErrGetMappedRangeReadsYourWrites is raised by THIS client when the mapped
	// read — primary range or any resolved secondary read — overlaps a key the
	// transaction has already written.
	ErrGetMappedRangeReadsYourWrites = 2039 // get_mapped_range_reads_your_writes
	ErrMapperNotTuple                = 2043 // mapper_not_tuple
	// ErrUnsupportedOperation is what snapshot=true and read-your-writes-disabled
	// both raise. Neither is a MODE for getMappedRange; both are errors.
	ErrUnsupportedOperation = 2108 // unsupported_operation
)

// GetMappedRange reads the primary range [begin, end) and, for each row, the
// secondary read the `mapper` tuple resolves it to — C++
// ReadYourWritesTransaction::getMappedRange (ReadYourWrites.actor.cpp:1779).
//
// The guard sequence below is C++'s, in C++'s order, and the order is load-bearing:
// the special-key, legal-range and limit checks all run BEFORE the snapshot /
// RYW-disabled guard, so a mapped read that is invalid for two reasons reports
// the FIRST one C++ would report.
//
// `limit` follows the C API, not the internal GetRangeLimits: fdb_c's
// validate_and_update_parameters maps 0 to ROW_LIMIT_UNLIMITED before RYW ever
// sees it (fdb_c.cpp:990-992), and fdb_transaction_get_mapped_range runs that
// same validator (:1080). So 0 here means UNLIMITED, and GetRangeLimits::isReached()'s
// "limit 0 returns an empty result" branch (NativeAPI.actor.cpp:2856) is
// unreachable from any C-API caller. A limit < -1 is range_limits_invalid,
// matching GetRangeLimits::isValid (FDBTypes.h:753).
//
// Isolation: this ALWAYS issues at Snapshot::True and then re-adds read conflict
// ranges by hand — for the primary range AND for every secondary read the server
// resolved (ReadYourWrites.actor.cpp:1163-1192). That is not an optimization; it
// is what makes a serializable mapped read correct, because the secondary keys
// are not known until the reply comes back and so cannot be conflict-ranged up
// front. Any caller building a remote-fetch cursor on top of this inherits the
// same obligation.
func (tx *Transaction) GetMappedRange(ctx context.Context, begin, end, mapper []byte, limit int, reverse bool) ([]MappedKeyValue, bool, error) {
	// C++ :1786 — the special key space is not mapped-readable. Checked first,
	// before even the read version is taken.
	if isSpecialKey(begin) && isSpecialKey(end) {
		return nil, false, &wire.FDBError{Code: ErrClientInvalidOperation}
	}
	if err := tx.ensureReadVersion(ctx); err != nil {
		return nil, false, tx.trackReadError(err)
	}
	// C++ :1801 — key_outside_legal_range.
	maxKey := tx.maxReadKey()
	if bytes.Compare(begin, maxKey) > 0 || bytes.Compare(end, maxKey) > 0 {
		return nil, false, &wire.FDBError{Code: 2004}
	}
	// C++ :1817 — range_limits_invalid.
	if limit < -1 {
		return nil, false, &wire.FDBError{Code: ErrRangeLimitsInvalid}
	}
	// C++ :1823 — an inverted range is an empty result, not an error.
	if bytes.Compare(begin, end) >= 0 {
		return nil, false, nil
	}
	// C++ :1219-1233 makes snapshot isolation and RYW-disabled both
	// unsupported_operation. Only ONE of those is reachable here, and the
	// difference matters: RYW-disabled is a runtime state, so it is guarded
	// below; snapshot is unreachable BY CONSTRUCTION because *Snapshot exposes
	// no GetMappedRange at all, so there is no way to ask for the unsupported
	// thing. That is a stronger guarantee than returning 2108, not a weaker one
	// — but it holds only while *Snapshot lacks the method, which is why
	// TestGetMappedRange_SnapshotCannotRequestIt pins the absence rather than
	// leaving it as an unstated property of the API surface.
	if tx.rywDisabled {
		return nil, false, &wire.FDBError{Code: ErrUnsupportedOperation}
	}

	rows, more, err := tx.getMappedRangeImpl(ctx, begin, end, mapper, limit, reverse)
	if err != nil {
		return nil, false, tx.trackReadError(err)
	}

	// C++ addConflictRangeAndMustUnmodified (:1163-1192): the read went out at
	// Snapshot::True, so every conflict range it needs is added here, and each
	// one is first checked against the write map — an overlap means the mapped
	// read observed a key this transaction wrote, which RYW cannot honour for
	// mapped reads and which C++ reports rather than silently serving stale data.
	if err := tx.addMappedRangeConflicts(begin, end, reverse, more, rows); err != nil {
		return nil, false, err
	}
	return rows, more, nil
}

// primaryConflictRange is C++ addConflictRange for the mapped read's PRIMARY
// range: the forward overload at ReadYourWrites.actor.cpp:245-281 and the
// reverse one at :284-319, which differ in which END of the range a truncated
// result narrows.
//
// The point of the narrowing is that a read cut short by a row limit did not
// observe the whole requested range, so it must not conflict on the part it
// never looked at. Forward: everything past the last row returned is unread, so
// the range ends at keyAfter(lastRow). Reverse: everything below the last row
// returned is unread, so the range begins there. Recording [begin, end) instead
// is safe for serializability — a superset never MISSES a conflict — but it is
// not safe for the mustUnmodified check that shares this range, where a
// superset turns a write C++ tolerates into get_mapped_range_reads_your_writes.
//
// C++'s readToBegin / readThroughEnd clamps (:263-265, :302-304) are omitted
// because they are unreachable through this API, not because they are
// unavailable. Both are guarded on the selector offsets — `begin.offset <= 0`
// and `end.offset > 0` — and GetMappedRange resolves its plain key arguments as
// firstGreaterOrEqual, i.e. offset 1 for both ends. That kills the readToBegin
// arm outright. readThroughEnd survives the offset guard but is only ever set
// by getRangeFallback (NativeAPI.actor.cpp:4483-4491) when the RESOLVED end key
// is allKeys.end AND the result is not truncated, in which case it rewrites
// rangeEnd from end.getKey() to getMaxReadKey() — the same value, since
// getMaxReadKey() is what bounds `end` at the legal-range check above. So the
// clamp is a no-op for every input this API accepts.
func primaryConflictRange(begin, end []byte, reverse, more bool, rows []MappedKeyValue) [2][]byte {
	// The begin >= end case C++ handles by swapping is unreachable: GetMappedRange
	// returns an empty result before issuing the read.
	rangeBegin, rangeEnd := begin, end
	if reverse {
		// C++ :295 — `rangeBegin = read.begin.offset <= 1 && result.more ? end : begin`.
		if more {
			rangeBegin = end
		}
		if n := len(rows); n > 0 {
			// result.end()[-1] is the SMALLEST key a reverse scan returned.
			if last := rows[n-1].Key; bytes.Compare(last, rangeBegin) < 0 {
				rangeBegin = last
			}
			// read.end.offset > 0 always holds here (firstGreaterOrEqual).
			if bytes.Compare(rangeEnd, rows[0].Key) <= 0 {
				rangeEnd = keyAfterBytes(rows[0].Key)
			}
		}
		return [2][]byte{rangeBegin, rangeEnd}
	}
	// C++ :257 — `rangeEnd = read.end.offset > 0 && result.more ? begin : end`.
	if more {
		rangeEnd = begin
	}
	if n := len(rows); n > 0 {
		// The `read.begin.offset <= 0` arm that lowers rangeBegin to result[0].key
		// is dead here for the same offset reason as readToBegin.
		if last := rows[n-1].Key; bytes.Compare(rangeEnd, last) <= 0 {
			rangeEnd = keyAfterBytes(last)
		}
	}
	return [2][]byte{rangeBegin, rangeEnd}
}

// addMappedRangeConflicts implements C++ addConflictRangeAndMustUnmodified.
func (tx *Transaction) addMappedRangeConflicts(begin, end []byte, reverse, more bool, rows []MappedKeyValue) error {
	// Primary range first, exactly as C++ does — and NARROWED, see
	// primaryConflictRange. The narrowing is not an optimization: the same range
	// feeds the mustUnmodified check, where recording a superset would raise
	// get_mapped_range_reads_your_writes on a write C++ lets through.
	ranges := [][2][]byte{primaryConflictRange(begin, end, reverse, more, rows)}
	for i := range rows {
		switch rows[i].Kind {
		case MappedResultGetValue:
			k := rows[i].GetValue.Key
			// A single key is the half-open range [k, k\x00) — C++
			// singleKeyRange. The point lookup gets a conflict range whether or
			// not the key EXISTED: a miss is a real observation, and a later
			// insert at that key must conflict.
			ranges = append(ranges, [2][]byte{k, keyAfterBytes(k)})
		case MappedResultGetRange:
			gr := &rows[i].GetRange
			// The selectors the server actually resolved, not the mapper's
			// intent — C++ passes getRange.begin/getRange.end straight through.
			ranges = append(ranges, [2][]byte{gr.Begin.Key, gr.End.Key})
		}
	}
	// C++ interleaves the check and the insert: updateConflictMap<true>
	// (ReadYourWrites.actor.cpp:335-351) walks ONE range's write-map segments,
	// throwing on the first modified one and inserting the rest as it goes, and
	// addConflictRangeAndMustUnmodified calls it once per range. So a throw on
	// range N leaves ranges 0..N-1 recorded. Checking every range up front and
	// inserting only on success would look tidier and would diverge: a caller
	// that catches 2039 and commits anyway would ship a transaction missing the
	// conflict ranges C++ had already banked.
	for _, r := range ranges {
		if bytes.Compare(r[0], r[1]) >= 0 {
			continue // empty range contributes no conflict and cannot overlap a write
		}
		if tx.ryw.hasModificationsInRange(r[0], r[1]) {
			return &wire.FDBError{Code: ErrGetMappedRangeReadsYourWrites}
		}
		tx.addReadConflicts([][2][]byte{r})
	}
	return nil
}
