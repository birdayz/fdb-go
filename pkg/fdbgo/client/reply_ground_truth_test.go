package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// Reply-direction ground truth: each vector is the EXACT server->client
// payload (ErrorOr<EnsureTable<T>>, AssumeVersion — no version prefix) that
// FDB's networkSender would emit (networksender.actor.h:38,
// FlowTransport.actor.cpp:1932), serialized by the real C++ ObjectWriter in
// the extractor (cmd/fdb-schema-extract). The assertions drive the
// PRODUCTION parse functions and pin the literal field values the C++
// constructor set — never values re-derived from the Go parse.

type replyVectorEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Size int    `json:"size"`
	Hex  string `json:"hex"`
}

func loadReplyVectors(t *testing.T) []replyVectorEntry {
	t.Helper()
	// Shared with //pkg/fdbgo/wire/types:types_test; declared as a bazel data
	// dep of conn_location_unit_test. Absence is a build bug, never a skip.
	data, err := os.ReadFile("../wire/types/testdata.json")
	if err != nil {
		t.Fatalf("testdata.json not found (missing bazel data dep?): %v", err)
	}
	var vecs []replyVectorEntry
	if err := json.Unmarshal(data, &vecs); err != nil {
		t.Fatalf("parse testdata.json: %v", err)
	}
	var replies []replyVectorEntry
	for _, v := range vecs {
		if v.Kind == "reply" {
			replies = append(replies, v)
		}
	}
	if len(replies) == 0 {
		t.Fatal("no reply vectors in testdata.json")
	}
	return replies
}

// replyAsserters maps vector name -> assertion against the production parser.
// The expected literals mirror the C++ constructor sites in
// cmd/fdb-schema-extract/main.cpp generateTestVectors.
var replyAsserters = map[string]func(t *testing.T, data []byte){
	"GetValueReply_present": func(t *testing.T, data []byte) {
		value, penalty, err := parseGetValueReply(data)
		if err != nil {
			t.Fatalf("parseGetValueReply: %v", err)
		}
		if string(value) != "ground_truth_value" {
			t.Errorf("value = %q, want %q", value, "ground_truth_value")
		}
		if penalty != 1.5 {
			t.Errorf("penalty = %v, want 1.5", penalty)
		}
	},
	"GetValueReply_missing": func(t *testing.T, data []byte) {
		value, penalty, err := parseGetValueReply(data)
		if err != nil {
			t.Fatalf("parseGetValueReply: %v", err)
		}
		if value != nil {
			t.Errorf("value = %q, want nil (absent)", value)
		}
		if penalty != 1.0 {
			t.Errorf("penalty = %v, want 1.0", penalty)
		}
	},
	"GetKeyReply_basic": func(t *testing.T, data []byte) {
		key, orEqual, offset, penalty, err := parseGetKeyReply(data)
		if err != nil {
			t.Fatalf("parseGetKeyReply: %v", err)
		}
		if string(key) != "resolved_key" {
			t.Errorf("key = %q, want %q", key, "resolved_key")
		}
		if !orEqual {
			t.Error("orEqual = false, want true")
		}
		if offset != 3 {
			t.Errorf("offset = %d, want 3", offset)
		}
		if penalty != 2.0 {
			t.Errorf("penalty = %v, want 2.0", penalty)
		}
	},
	"GetKeyValuesReply_two_rows": func(t *testing.T, data []byte) {
		kvs, more, penalty, err := parseGetKeyValuesReply(data)
		if err != nil {
			t.Fatalf("parseGetKeyValuesReply: %v", err)
		}
		want := []struct{ k, v string }{
			{"alpha", "value_a"},
			{"beta", "value_b"},
		}
		if len(kvs) != len(want) {
			t.Fatalf("len(kvs) = %d, want %d", len(kvs), len(want))
		}
		for i, w := range want {
			if string(kvs[i].Key) != w.k || string(kvs[i].Value) != w.v {
				t.Errorf("kvs[%d] = (%q, %q), want (%q, %q)", i, kvs[i].Key, kvs[i].Value, w.k, w.v)
			}
		}
		if !more {
			t.Error("more = false, want true")
		}
		if penalty != 1.25 {
			t.Errorf("penalty = %v, want 1.25", penalty)
		}
	},
	"GetKeyValuesReply_empty": func(t *testing.T, data []byte) {
		kvs, more, penalty, err := parseGetKeyValuesReply(data)
		if err != nil {
			t.Fatalf("parseGetKeyValuesReply: %v", err)
		}
		if len(kvs) != 0 {
			t.Errorf("len(kvs) = %d, want 0", len(kvs))
		}
		if more {
			t.Error("more = true, want false")
		}
		if penalty != 1.0 {
			t.Errorf("penalty = %v, want 1.0", penalty)
		}
	},
	// getMappedRange. These drive the production parser over real C++
	// ObjectWriter bytes for both arms of the per-row union, including the two
	// shapes that decode without error while being wrong: a mapped key that does
	// not exist, and a mapped range that matched nothing.
	"GetMappedKeyValuesReply_getvalue": func(t *testing.T, data []byte) {
		rows, more, penalty, err := parseGetMappedKeyValuesReply(data)
		if err != nil {
			t.Fatalf("parseGetMappedKeyValuesReply: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("len(rows) = %d, want 2", len(rows))
		}
		if string(rows[0].Key) != "idx_a" || string(rows[0].Value) != "pk_a" {
			t.Errorf("rows[0] primary = (%q,%q), want (idx_a,pk_a)", rows[0].Key, rows[0].Value)
		}
		if rows[0].Kind != MappedResultGetValue {
			t.Fatalf("rows[0].Kind = %v, want getValue", rows[0].Kind)
		}
		if string(rows[0].GetValue.Key) != "rec_a" || string(rows[0].GetValue.Value) != "record_a" {
			t.Errorf("rows[0] mapped = (%q,%q), want (rec_a,record_a)",
				rows[0].GetValue.Key, rows[0].GetValue.Value)
		}
		if !rows[0].GetValue.Present {
			t.Error("rows[0].GetValue.Present = false, want true")
		}
		// The mapper resolved to a key that is not in the database. Present must
		// stay false so the caller can tell this apart from an empty record.
		if rows[1].Kind != MappedResultGetValue {
			t.Fatalf("rows[1].Kind = %v, want getValue", rows[1].Kind)
		}
		if string(rows[1].GetValue.Key) != "rec_missing" {
			t.Errorf("rows[1] mapped key = %q, want rec_missing", rows[1].GetValue.Key)
		}
		if rows[1].GetValue.Present {
			t.Errorf("rows[1].GetValue.Present = true (value %q), want false — an "+
				"absent mapped value must not read as an empty one", rows[1].GetValue.Value)
		}
		if more {
			t.Error("more = true, want false")
		}
		if penalty != 1.5 {
			t.Errorf("penalty = %v, want 1.5", penalty)
		}
	},
	"GetMappedKeyValuesReply_getrange": func(t *testing.T, data []byte) {
		rows, more, penalty, err := parseGetMappedKeyValuesReply(data)
		if err != nil {
			t.Fatalf("parseGetMappedKeyValuesReply: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("len(rows) = %d, want 2", len(rows))
		}
		if rows[0].Kind != MappedResultGetRange {
			t.Fatalf("rows[0].Kind = %v, want getRange", rows[0].Kind)
		}
		gr := rows[0].GetRange
		if string(gr.Begin.Key) != "pfx_r\x00" || !gr.Begin.OrEqual || gr.Begin.Offset != 1 {
			t.Errorf("rows[0] begin = (%q,%v,%d), want (pfx_r\\x00,true,1)",
				gr.Begin.Key, gr.Begin.OrEqual, gr.Begin.Offset)
		}
		if string(gr.End.Key) != "pfx_r\xff" {
			t.Errorf("rows[0] end key = %q, want pfx_r\\xff", gr.End.Key)
		}
		if len(gr.Rows) != 2 {
			t.Fatalf("rows[0] nested rows = %d, want 2", len(gr.Rows))
		}
		if string(gr.Rows[0].Key) != "pfx_r_1" || string(gr.Rows[0].Value) != "sub_1" {
			t.Errorf("rows[0] nested[0] = (%q,%q), want (pfx_r_1,sub_1)",
				gr.Rows[0].Key, gr.Rows[0].Value)
		}
		if string(gr.Rows[1].Key) != "pfx_r_2" || string(gr.Rows[1].Value) != "sub_2" {
			t.Errorf("rows[0] nested[1] = (%q,%q), want (pfx_r_2,sub_2)",
				gr.Rows[1].Key, gr.Rows[1].Value)
		}
		// The mapped range matched nothing. The probed selectors must survive so
		// "genuinely empty" stays distinguishable from "truncated".
		if rows[1].Kind != MappedResultGetRange {
			t.Fatalf("rows[1].Kind = %v, want getRange", rows[1].Kind)
		}
		if string(rows[1].GetRange.Begin.Key) != "pfx_e\x00" {
			t.Errorf("rows[1] begin key = %q, want pfx_e\\x00", rows[1].GetRange.Begin.Key)
		}
		if len(rows[1].GetRange.Rows) != 0 {
			t.Errorf("rows[1] nested rows = %d, want 0", len(rows[1].GetRange.Rows))
		}
		if rows[1].GetRange.More {
			t.Error("rows[1] nested more = true, want false")
		}
		if !more {
			t.Error("more = false, want true")
		}
		if penalty != 2.5 {
			t.Errorf("penalty = %v, want 2.5", penalty)
		}
	},
	"GetMappedKeyValuesReply_empty": func(t *testing.T, data []byte) {
		rows, more, penalty, err := parseGetMappedKeyValuesReply(data)
		if err != nil {
			t.Fatalf("parseGetMappedKeyValuesReply: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("len(rows) = %d, want 0", len(rows))
		}
		if more {
			t.Error("more = true, want false")
		}
		if penalty != 1.0 {
			t.Errorf("penalty = %v, want 1.0", penalty)
		}
	},
	"GetReadVersionReply_locked": func(t *testing.T, data []byte) {
		version, locked, rkDefault, rkBatch, _, _, err := parseGetReadVersionReply(data)
		if err != nil {
			t.Fatalf("parseGetReadVersionReply: %v", err)
		}
		if version != 0x123456789a {
			t.Errorf("version = %#x, want 0x123456789a", version)
		}
		// `locked` feeds the per-transaction database_locked enforcement
		// (RFC-096; C++ NativeAPI.actor.cpp:7425-7426).
		if !locked {
			t.Error("locked = false, want true (C++ constructor sets r.locked = true)")
		}
		if rkDefault || rkBatch {
			t.Errorf("rkDefault/rkBatch = %v/%v, want false/false", rkDefault, rkBatch)
		}
	},
	"CommitID_committed": func(t *testing.T, data []byte) {
		var tx Transaction
		if err := tx.parseCommitReply(data); err != nil {
			t.Fatalf("parseCommitReply: %v", err)
		}
		if tx.committedVersion != 0x0abcdef012 {
			t.Errorf("committedVersion = %#x, want 0xabcdef012", tx.committedVersion)
		}
		if tx.txnBatchId != 7 {
			t.Errorf("txnBatchId = %d, want 7", tx.txnBatchId)
		}
	},
	"CommitID_conflict": func(t *testing.T, data []byte) {
		// A conflict delivered IN-BAND (report_conflicting_keys shape:
		// CommitID{version: invalidVersion, conflictingKRIndices}) must map
		// to not_committed, as C++ does (NativeAPI.actor.cpp:6653,6726) —
		// NOT parse as a successful commit at version -1.
		var tx Transaction
		err := tx.parseCommitReply(data)
		if err == nil {
			t.Fatal("parseCommitReply = nil, want not_committed (1020)")
		}
		assertFDBErrorCode(t, err, ErrNotCommitted)
		// conflictingKRIndices is a VectorRef<int> (CommitProxyInterface.h:193) the C++
		// constructor filled with {0, 2}: a count, then the ints, not a byte string.
		out, err := parseCommitOutcome(data)
		assertFDBErrorCode(t, err, ErrNotCommitted)
		if !out.conflictReply || !out.hasConflictIndices || len(out.conflictIndices) != 2 ||
			out.conflictIndices[0] != 0 || out.conflictIndices[1] != 2 {
			t.Fatalf("commit outcome %+v, want an in-band conflict with indices [0 2]", out)
		}
	},
	"WatchValueReply_basic": func(t *testing.T, data []byte) {
		// Production path: a non-error reply means "key changed".
		if err := parseWatchValueReply(data); err != nil {
			t.Fatalf("parseWatchValueReply: %v", err)
		}
		// The production parser discards the fields — pin the wire layer's
		// decode of them too, so the vector proves more than crash-absence.
		var r wire.Reader
		if err := wire.ReadErrorOrInto(data, &r); err != nil {
			t.Fatalf("ReadErrorOrInto: %v", err)
		}
		var reply types.WatchValueReply
		if err := reply.UnmarshalFromReader(&r); err != nil {
			t.Fatalf("UnmarshalFromReader: %v", err)
		}
		if reply.Version != 424242 {
			t.Errorf("Version = %d, want 424242", reply.Version)
		}
		if !reply.Cached {
			t.Error("Cached = false, want true")
		}
	},
	"SplitRangeReply_two_points": func(t *testing.T, data []byte) {
		points, err := parseSplitRangeReply(data)
		if err != nil {
			t.Fatalf("parseSplitRangeReply: %v", err)
		}
		want := [][]byte{[]byte("split_a"), []byte("split_b")}
		if len(points) != len(want) {
			t.Fatalf("len(points) = %d, want %d", len(points), len(want))
		}
		for i := range want {
			if !bytes.Equal(points[i], want[i]) {
				t.Errorf("points[%d] = %q, want %q", i, points[i], want[i])
			}
		}
	},
	"StorageMetrics_basic": func(t *testing.T, data []byte) {
		got, err := parseWaitMetricsReply(data)
		if err != nil {
			t.Fatalf("parseWaitMetricsReply: %v", err)
		}
		if got != 123456789 {
			t.Errorf("bytes = %d, want 123456789", got)
		}
	},
	"GetKeyServerLocationsReply_one_range": func(t *testing.T, data []byte) {
		entries, err := parseGetKeyServerLocationsReply(data)
		if err != nil {
			t.Fatalf("parseGetKeyServerLocationsReply: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		e := entries[0]
		if string(e.begin) != "range_begin" || string(e.end) != "range_end" {
			t.Errorf("range = (%q, %q), want (range_begin, range_end)", e.begin, e.end)
		}
		if len(e.servers) != 1 {
			t.Fatalf("len(servers) = %d, want 1", len(e.servers))
		}
		s := e.servers[0]
		if s.Address != "10.1.2.3:4500" {
			t.Errorf("server address = %q, want 10.1.2.3:4500", s.Address)
		}
		// The extractor pins the SSI getValue endpoint token; the parser
		// derives per-method endpoints from it via getAdjustedEndpoint, so
		// the base token's First word must round-trip exactly.
		wantTok := transport.UID{First: 0x7373737373737373, Second: 0x8484848484848484}
		if s.Token.First != wantTok.First {
			t.Errorf("server token = %#x/%#x, want %#x/%#x",
				s.Token.First, s.Token.Second, wantTok.First, wantTok.Second)
		}
	},
	"GetKeyServerLocationsReply_secondary_address": func(t *testing.T, data []byte) {
		entries, err := parseGetKeyServerLocationsReply(data)
		if err != nil || len(entries) != 1 || len(entries[0].servers) != 1 {
			t.Fatalf("parseGetKeyServerLocationsReply: %+v, %v", entries, err)
		}
		if s := entries[0].servers[0]; s.Address != "10.1.2.3:4500" || s.TLS {
			t.Errorf("server = %+v, want the plaintext primary 10.1.2.3:4500", s)
		}
		// The secondary address decodes as the NetworkAddress table C++ writes.
		_, ss := firstLocation(t, data)
		ep, err := ReadEndpointFromSlot(ss, types.StorageServerInterfaceSlotField_2)
		if err != nil {
			t.Fatal(err)
		}
		if a := ep.Addresses; !a.HasSecondaryAddress || a.SecondaryAddress.Port != 4501 ||
			a.SecondaryAddress.Flags&types.FlagTLS == 0 || networkAddressString(&a.SecondaryAddress) != "10.1.2.3:4501" {
			t.Errorf("secondary address = %+v (has %t), want 10.1.2.3:4501 with TLS", a.SecondaryAddress, a.HasSecondaryAddress)
		}
	},
	"ClientDBInfo_secondary_address": func(t *testing.T, data []byte) {
		info, err := parseErrorOrClientDBInfo(data)
		if err != nil {
			t.Fatalf("parseErrorOrClientDBInfo: %v", err)
		}
		if len(info.GRVProxies) != 1 || len(info.CommitProxies) != 1 ||
			info.GRVProxies[0].Address != "10.1.2.3:4500" || info.CommitProxies[0].Address != "10.1.2.3:4500" {
			t.Fatalf("proxies = %+v / %+v, want one of each at 10.1.2.3:4500", info.GRVProxies, info.CommitProxies)
		}
		if want := (transport.UID{First: 0xd5d5d5d5d5d5d5d5, Second: 0xe6e6e6e6e6e6e6e6}); info.ID != want {
			t.Errorf("id = %+v, want %+v", info.ID, want)
		}
		if got := info.GRVProxies[0].Token.First; got != 0x9191919191919191 {
			t.Errorf("GRV proxy token = %#x, want 0x9191919191919191", got)
		}
	},
	"GetReadVersionReply_version_vector": func(t *testing.T, data []byte) {
		version, _, _, _, _, _, err := parseGetReadVersionReply(data)
		if err != nil || version != 0x2233445566 {
			t.Fatalf("parseGetReadVersionReply: version %#x, %v", version, err)
		}
	},
	"GetReadVersionReply_tag_throttle": func(t *testing.T, data []byte) {
		version, _, _, _, throttles, _, err := parseGetReadVersionReply(data)
		if err != nil || version != 0x3344556677 {
			t.Fatalf("parseGetReadVersionReply: version %#x, %v", version, err)
		}
		// C++ sends NO_EXPIRATION (DBL_MAX) minus now(), which is DBL_MAX.
		if len(throttles) != 1 || string(throttles[0].Tag) != "tag_a" ||
			throttles[0].Limits.TpsRate != 12.5 || throttles[0].Limits.Duration != math.MaxFloat64 {
			t.Fatalf("tagThrottleInfo = %+v, want tag_a at 12.5 tps with no expiration", throttles)
		}
	},
	"GetValueReply_error": func(t *testing.T, data []byte) {
		_, _, err := parseGetValueReply(data)
		assertFDBErrorCode(t, err, 1001)
	},
	"GetKeyReply_error": func(t *testing.T, data []byte) {
		_, _, _, _, err := parseGetKeyReply(data)
		assertFDBErrorCode(t, err, 1001)
	},
	"GetKeyValuesReply_error": func(t *testing.T, data []byte) {
		_, _, _, err := parseGetKeyValuesReply(data)
		assertFDBErrorCode(t, err, 1001)
	},
	"GetMappedKeyValuesReply_error": func(t *testing.T, data []byte) {
		_, _, _, err := parseGetMappedKeyValuesReply(data)
		assertFDBErrorCode(t, err, 1001)
	},
}

// TestReplyGroundTruth feeds C++-ObjectWriter-serialized reply payloads to
// the client's production parse functions and asserts the field values.
func TestReplyGroundTruth(t *testing.T) {
	t.Parallel()
	replies := loadReplyVectors(t)

	seen := make(map[string]bool)
	for _, vec := range replies {
		vec := vec
		seen[vec.Name] = true
		t.Run(vec.Name, func(t *testing.T) {
			t.Parallel()
			assert, ok := replyAsserters[vec.Name]
			if !ok {
				// Every reply vector MUST have an asserter — an unmatched
				// name is a broken net, not a skip (no-skip rule).
				t.Fatalf("no asserter for reply vector %s", vec.Name)
			}
			raw, err := hex.DecodeString(vec.Hex)
			if err != nil {
				t.Fatalf("decode hex: %v", err)
			}
			if len(raw) != vec.Size {
				t.Fatalf("hex decodes to %d bytes, size field says %d", len(raw), vec.Size)
			}
			assert(t, raw)
		})
	}

	// Reverse direction: every asserter must be backed by a vector.
	for name := range replyAsserters {
		if !seen[name] {
			t.Errorf("asserter %s has no vector in testdata.json", name)
		}
	}
}

func requireNativeConflictBounds(t *testing.T, m *conflictingKeyMap, bounds ...string) {
	t.Helper()
	if m == nil || len(m.entries) != len(bounds) {
		t.Fatalf("conflict map=%+v, want %d boundaries", m, len(bounds))
	}
	for i, bound := range bounds {
		wantKey := append(bytes.Clone(conflictingKeysBegin), bound...)
		wantValue := "1"
		if i%2 != 0 {
			wantValue = "0"
		}
		if !bytes.Equal(m.entries[i].Key, wantKey) || string(m.entries[i].Value) != wantValue {
			t.Fatalf("conflict boundary %d=%q/%q, want %q/%q", i, m.entries[i].Key, m.entries[i].Value, wantKey, wantValue)
		}
	}
}

// Gate after decoding: Reset cancels a wire wait before it can test stale publication.
func TestNativeConflictPublicationUsesCommittingIncarnation(t *testing.T) {
	t.Parallel()
	for _, reset := range []bool{false, true} {
		name := "live"
		if reset {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			tx := &Transaction{}
			defer tx.Cancel()
			lease := tx.enterState()
			tx.readErrMu.Lock()
			completion := tx.newVersionstampLocked(lease.inc)
			completion.claimed = true
			tx.readErrMu.Unlock()
			lease.release()
			retained := newConflictingKeyMap([]int32{0}, []KeyRange{{Begin: []byte("q"), End: []byte("r")}})
			tx.conflictingKeys.Store(retained)
			result, err := parseCommitOutcome(replyVector(t, "CommitID_conflict"))
			assertFDBErrorCode(t, err, 1020)
			if !result.conflictReply || !result.hasConflictIndices || len(result.conflictIndices) != 2 || result.conflictIndices[0] != 0 || result.conflictIndices[1] != 2 {
				t.Fatalf("decoded conflict outcome=%+v, want indices [0 2]", result)
			}
			result.conflictingKeys = newConflictingKeyMap(result.conflictIndices, []KeyRange{
				{Begin: []byte("a"), End: []byte("b")},
				{Begin: []byte("c"), End: []byte("d")},
				{Begin: []byte("e"), End: []byte("f")},
			})
			decoded, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				close(decoded)
				select {
				case <-release:
				case <-ctx.Done():
					return
				}
				tx.readErrMu.Lock()
				tx.finishNativeCommitLocked(completion, result, err)
				tx.readErrMu.Unlock()
			}()
			<-decoded
			if reset {
				tx.Reset()
				if tx.conflictingKeys.Load() != retained {
					t.Fatal("Reset dropped the retained conflict map")
				}
				lease := tx.enterState()
				if lease.inc == completion.inc {
					t.Fatal("Reset did not replace the committing incarnation")
				}
				lease.release()
				retained = newConflictingKeyMap([]int32{0}, []KeyRange{{Begin: []byte("r"), End: []byte("s")}})
				tx.conflictingKeys.Store(retained)
			}
			close(release)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("decoded native completion did not finish")
			}
			if reset {
				if tx.conflictingKeys.Load() != retained {
					t.Fatal("retired native conflict replaced the successor's map")
				}
				requireNativeConflictBounds(t, tx.conflictingKeys.Load(), "r", "s")
				assertFDBErrorCode(t, completion.err, ErrTransactionCancelled)
			} else {
				if tx.conflictingKeys.Load() != result.conflictingKeys {
					t.Fatal("live native conflict was not published")
				}
				requireNativeConflictBounds(t, tx.conflictingKeys.Load(), "a", "b", "e", "f")
				assertFDBErrorCode(t, completion.err, 2020)
			}
		})
	}
}

func TestNativeConflictPublicationFromWire(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	peer := newIdentityPeer(t)
	db, _ := identityDatabase(ctx, peer)
	defer closeIdentityDatabase(db)
	db.dbInfo.Store(&DBInfo{CommitProxies: []ProxyInfo{{Address: armAddr, Token: armToken}}})
	db.minAcceptableReadVersion.Store(&minAcceptableStamp{epoch: db.grvCache.epochNow(), version: 1})
	tx := (&Database{db: db}).CreateTransaction()
	defer tx.Cancel()
	tx.SetReadVersion(1)
	tx.SetReportConflictingKeys()
	for _, kr := range []KeyRange{
		{Begin: []byte("a"), End: []byte("b")},
		{Begin: []byte("c"), End: []byte("d")},
		{Begin: []byte("e"), End: []byte("f")},
	} {
		if err := tx.AddReadConflictRange(kr.Begin, kr.End); err != nil {
			t.Fatal(err)
		}
	}
	tx.Set([]byte("a"), []byte("v"))
	done := make(chan error, 1)
	go func() { done <- tx.Commit(ctx) }()
	frame, ok := peer.recv(ctx)
	if !ok {
		t.Fatal("native commit did not reach the proxy")
	}
	var req types.CommitTransactionRequest
	if err := req.UnmarshalFDB(frame.body); err != nil {
		t.Fatal(err)
	}
	if !req.Transaction.Report_conflicting_keys || len(req.Transaction.ReadConflictRanges) != 3 {
		t.Fatalf("commit report flag=%t reads=%+v, want three captured ranges", req.Transaction.Report_conflicting_keys, req.Transaction.ReadConflictRanges)
	}
	for i, bounds := range [][2]string{{"a", "b"}, {"c", "d"}, {"e", "f"}} {
		if got := req.Transaction.ReadConflictRanges[i]; string(got.Begin) != bounds[0] || string(got.End) != bounds[1] {
			t.Fatalf("wire read %d=%+v, want %v", i, got, bounds)
		}
	}
	if err := transport.WriteFrame(peer.server, replyToken(req.Reply.Token), replyVector(t, "CommitID_conflict"), false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		assertFDBErrorCode(t, err, 1020)
	case <-ctx.Done():
		t.Fatal("native conflict commit did not finish")
	}
	requireNativeConflictBounds(t, tx.conflictingKeys.Load(), "a", "b", "e", "f")
}
