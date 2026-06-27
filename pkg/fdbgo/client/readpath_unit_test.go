package client

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// ============================================================================
// isWrongShardServer / isAllAlternativesFailed / isFutureVersionOrProcessBehind
// — pure error-code predicates that drive the read-path retry decisions.
// ============================================================================

func TestIsWrongShardServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		// Inject literal canonical codes, not the code-under-test's constant
		// (anti-self-confirming, RFC-010 P6): 1001 is wrong_shard_server, 1062
		// is change_feed_cancelled and must NOT be treated as wrong-shard.
		{"FDB 1001 wrong_shard_server", &wire.FDBError{Code: 1001}, true},
		{"wrapped FDB 1001", fmt.Errorf("send: %w", &wire.FDBError{Code: 1001}), true},
		{"FDB 1062 change_feed_cancelled NOT wrong-shard", &wire.FDBError{Code: 1062}, false},
		{"FDB other", &wire.FDBError{Code: 1007}, false},
		{"FDB 1006", &wire.FDBError{Code: ErrAllAlternativesFailed}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWrongShardServer(tt.err); got != tt.want {
				t.Errorf("isWrongShardServer(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestWrongShardServerCode_Canonical pins ErrWrongShardServer to the canonical
// FDB code and proves it is NOT change_feed_cancelled. This is the value-equality
// guard that would have caught the 1062 bug: the constant must equal the wire
// code named "wrong_shard_server", independent of any matcher that consumes it.
func TestWrongShardServerCode_Canonical(t *testing.T) {
	t.Parallel()
	if ErrWrongShardServer != 1001 {
		t.Fatalf("ErrWrongShardServer = %d, want canonical 1001 (wrong_shard_server)", ErrWrongShardServer)
	}
	if desc := (&wire.FDBError{Code: ErrWrongShardServer}).Error(); !strings.Contains(desc, "wrong_shard_server") {
		t.Errorf("FDBError{Code: ErrWrongShardServer}.Error() = %q, want substring %q", desc, "wrong_shard_server")
	}
	// 1062 is change_feed_cancelled — guard against the historical confusion.
	if desc := (&wire.FDBError{Code: 1062}).Error(); !strings.Contains(desc, "change_feed_cancelled") {
		t.Errorf("FDBError{Code: 1062}.Error() = %q, want substring %q", desc, "change_feed_cancelled")
	}
}

func TestIsAllAlternativesFailed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"FDB 1006", &wire.FDBError{Code: ErrAllAlternativesFailed}, true},
		{"wrapped FDB 1006", fmt.Errorf("send: %w", &wire.FDBError{Code: ErrAllAlternativesFailed}), true},
		{"FDB other", &wire.FDBError{Code: 1009}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAllAlternativesFailed(tt.err); got != tt.want {
				t.Errorf("isAllAlternativesFailed(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsFutureVersionOrProcessBehind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"FDB 1009", &wire.FDBError{Code: ErrFutureVersion}, true},
		{"FDB 1037", &wire.FDBError{Code: ErrProcessBehind}, true},
		{"wrapped 1009", fmt.Errorf("send: %w", &wire.FDBError{Code: ErrFutureVersion}), true},
		{"FDB 1001 wrong_shard", &wire.FDBError{Code: 1001}, false},
		{"FDB 1007", &wire.FDBError{Code: 1007}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFutureVersionOrProcessBehind(tt.err); got != tt.want {
				t.Errorf("isFutureVersionOrProcessBehind(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ============================================================================
// sleepCtx — context-aware sleep used by the wrong-shard retry loop.
// ============================================================================

func TestSleepCtx_ElapsesNaturally(t *testing.T) {
	t.Parallel()
	start := time.Now()
	if err := sleepCtx(context.Background(), 30*time.Millisecond); err != nil {
		t.Errorf("expected nil on natural elapse, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("returned too early: %v", elapsed)
	}
}

func TestSleepCtx_ReturnsCtxErrOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- sleepCtx(ctx, time.Hour) // would otherwise block forever
	}()
	// Give the goroutine a moment to enter the select.
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("sleepCtx did not return after ctx cancel")
	}
}

func TestSleepCtx_AlreadyCancelledCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before sleep
	err := sleepCtx(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

// ============================================================================
// buildGetValueRequest — wire-construction round-trip via types.GetValueRequest.
// ============================================================================

func TestBuildGetValueRequest_RoundTrip(t *testing.T) {
	t.Parallel()
	key := []byte("user/42")
	const (
		version  int64 = 0xDEADBEEF
		tenantID int64 = 7
	)
	replyToken := transport.UID{First: 0xAA, Second: 0xBB}
	span := types.SpanContext{TraceID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, SpanID: 0x1234, Flags: 1}

	body, bufp := buildGetValueRequest(key, version, false, tenantID, span, replyToken, transport.UID{})
	defer getValueBufPool.Put(bufp)

	var req types.GetValueRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if string(req.Key) != string(key) {
		t.Errorf("Key: got %q, want %q", req.Key, key)
	}
	// RFC-115 §4: the trace span must round-trip (not the all-zero span we used to send).
	if req.SpanContext.TraceID != span.TraceID || req.SpanContext.SpanID != span.SpanID || req.SpanContext.Flags != span.Flags {
		t.Errorf("SpanContext: got %+v, want %+v", req.SpanContext, span)
	}
	if req.Version != version {
		t.Errorf("Version: got %d, want %d", req.Version, version)
	}
	if req.TenantInfo.TenantId != tenantID {
		t.Errorf("TenantId: got %d, want %d", req.TenantInfo.TenantId, tenantID)
	}
	if req.HasOptions {
		t.Errorf("HasOptions: got true, want false (lockAware was false)")
	}
	first := binary.LittleEndian.Uint64(req.Reply.Token[0:8])
	second := binary.LittleEndian.Uint64(req.Reply.Token[8:16])
	if first != replyToken.First || second != replyToken.Second {
		t.Errorf("reply token: got {%x,%x}, want {%x,%x}",
			first, second, replyToken.First, replyToken.Second)
	}
}

func TestBuildGetValueRequest_LockAwareSetsOptions(t *testing.T) {
	t.Parallel()
	body, bufp := buildGetValueRequest([]byte("k"), 1, true /*lockAware*/, 0,
		types.SpanContext{}, transport.UID{First: 1, Second: 2}, transport.UID{})
	defer getValueBufPool.Put(bufp)

	var req types.GetValueRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if !req.HasOptions {
		t.Fatal("HasOptions: got false, want true (lockAware=true)")
	}
	if !req.Options.LockAware {
		t.Error("Options.LockAware: got false, want true (lock-aware reads must set the real lockAware bool, not the debugID field)")
	}
	// C++ LOCK_AWARE inits a default ReadOptions (type=NORMAL(3), cacheResult=true) then sets
	// lockAware (NativeAPI.actor.cpp:7072; FDBTypes.h:1748). Sending Type=0(EAGER)/CacheResult=false
	// is wire-wrong on every lock-aware read.
	if req.Options.Type != readTypeNormal {
		t.Errorf("Options.Type: got %d, want %d (ReadType::NORMAL)", req.Options.Type, readTypeNormal)
	}
	if !req.Options.CacheResult {
		t.Error("Options.CacheResult: got false, want true (C++ ReadOptions default ctor)")
	}
}

// ============================================================================
// buildGetKeyValuesRequest — same shape but with begin/end selectors + limits.
// ============================================================================

func TestBuildGetKeyValuesRequest_RoundTrip(t *testing.T) {
	t.Parallel()
	begin, end := []byte("a"), []byte("z")
	const (
		version  int64 = 100
		limit    int32 = 25
		tenantID int64 = 0
	)
	replyToken := transport.UID{First: 0x1234, Second: 0x5678}

	body, bufp := buildGetKeyValuesRequest(begin, end, version, limit, false, tenantID,
		types.SpanContext{}, replyToken, transport.UID{})
	defer getKeyValuesBufPool.Put(bufp)

	var req types.GetKeyValuesRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if string(req.Begin.Key) != string(begin) {
		t.Errorf("Begin.Key: got %q, want %q", req.Begin.Key, begin)
	}
	if string(req.End.Key) != string(end) {
		t.Errorf("End.Key: got %q, want %q", req.End.Key, end)
	}
	// firstGreaterOrEqual encoding: orEqual=false, offset=1
	if req.Begin.OrEqual || req.Begin.Offset != 1 {
		t.Errorf("Begin selector: got OrEqual=%v Offset=%d, want OrEqual=false Offset=1",
			req.Begin.OrEqual, req.Begin.Offset)
	}
	if req.End.OrEqual || req.End.Offset != 1 {
		t.Errorf("End selector: got OrEqual=%v Offset=%d, want OrEqual=false Offset=1",
			req.End.OrEqual, req.End.Offset)
	}
	if req.Version != version {
		t.Errorf("Version: got %d, want %d", req.Version, version)
	}
	if req.Limit != limit {
		t.Errorf("Limit: got %d, want %d", req.Limit, limit)
	}
	if req.TenantInfo.TenantId != tenantID {
		t.Errorf("TenantId: got %d, want %d", req.TenantInfo.TenantId, tenantID)
	}
	if req.HasOptions {
		t.Error("HasOptions: got true, want false (lockAware was false)")
	}
}

func TestBuildGetKeyValuesRequest_LockAwareSetsOptions(t *testing.T) {
	t.Parallel()
	body, bufp := buildGetKeyValuesRequest(
		[]byte("a"), []byte("z"), 1, 10, true /*lockAware*/, 0,
		types.SpanContext{}, transport.UID{First: 1, Second: 2}, transport.UID{},
	)
	defer getKeyValuesBufPool.Put(bufp)

	var req types.GetKeyValuesRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if !req.HasOptions || !req.Options.LockAware {
		t.Errorf("lock-aware not set: HasOptions=%v Options.LockAware=%v",
			req.HasOptions, req.Options.LockAware)
	}
	// C++ lock-aware reads send type=NORMAL(3)/cacheResult=true (default ReadOptions ctor), not
	// Type=0(EAGER)/CacheResult=false. (NativeAPI.actor.cpp:7072; FDBTypes.h:1748)
	if req.Options.Type != readTypeNormal {
		t.Errorf("Options.Type: got %d, want %d (ReadType::NORMAL)", req.Options.Type, readTypeNormal)
	}
	if !req.Options.CacheResult {
		t.Error("Options.CacheResult: got false, want true (C++ ReadOptions default ctor)")
	}
}

// buildWatchValueRequest — wire-construction round-trip. `span` is the watchValue
// child (WatchPoll derives it via childSpanContext); the builder stamps it verbatim.
func TestBuildWatchValueRequest_RoundTrip(t *testing.T) {
	t.Parallel()
	key := []byte("watched/key")
	value := []byte("v0")
	const (
		readVersion int64 = 12345
		tenantID    int64 = 7
	)
	replyToken := transport.UID{First: 0xFEED, Second: 0xFACE}
	span := types.SpanContext{
		TraceID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:  0x55,
		Flags:   traceFlagSampled,
	}

	body := buildWatchValueRequest(key, value, readVersion, tenantID, span, replyToken)

	var req types.WatchValueRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if string(req.Key) != string(key) {
		t.Errorf("Key: got %q, want %q", req.Key, key)
	}
	if req.Version != readVersion {
		t.Errorf("Version: got %d, want %d", req.Version, readVersion)
	}
	if !req.HasValue || string(req.Value) != string(value) {
		t.Errorf("Value: HasValue=%v got %q, want %q", req.HasValue, req.Value, value)
	}
	if req.TenantInfo.TenantId != tenantID {
		t.Errorf("TenantId: got %d, want %d", req.TenantInfo.TenantId, tenantID)
	}
	// Revert-prove: drop the SpanContext field in buildWatchValueRequest → zero → red.
	if req.SpanContext != span {
		t.Errorf("SpanContext: got %+v, want %+v", req.SpanContext, span)
	}
	first := binary.LittleEndian.Uint64(req.Reply.Token[0:8])
	second := binary.LittleEndian.Uint64(req.Reply.Token[8:16])
	if first != replyToken.First || second != replyToken.Second {
		t.Errorf("reply token: got {%x,%x}, want {%x,%x}", first, second, replyToken.First, replyToken.Second)
	}
}

// TestBuildWatchValueRequest_NoValue: a nil value leaves HasValue false (the
// watch-on-absent case), matching the original inline construction.
func TestBuildWatchValueRequest_NoValue(t *testing.T) {
	t.Parallel()
	body := buildWatchValueRequest([]byte("k"), nil, 1, 0, types.SpanContext{}, transport.UID{First: 1, Second: 2})
	var req types.WatchValueRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if req.HasValue {
		t.Error("HasValue must be false when value is nil")
	}
}

// TestPendingGet_Resolve_ContextCancelled pins the context-cancel arm of
// PendingGet.Resolve (RFC-010 #3): a cancelled context returns ctx.Err()
// directly — it does NOT re-drive through getValue (no point retrying a
// cancelled read), unlike the wrong-shard/transport/timeout arms. Deterministic:
// flushed=true skips the Flush; the cancel arm never re-drives through conn.
// tx is a zero Transaction: Resolve only touches it for the gen-guarded
// readErr deregistration (mutex zero value, nil-map delete — both no-ops).
func TestPendingGet_Resolve_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &PendingGet{
		tx:          &Transaction{},
		flushed:     true,                          // skip conn.Flush (no conn needed)
		ctx:         ctx,                           // already cancelled
		replyCh:     make(chan transport.Response), // never fires
		replyHandle: &transport.ReplyHandle{},      // zero handle: Cancel/Release are no-ops
		timer:       getTimer(DefaultRPCTimeout),
	}
	_, err := p.Resolve()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve on cancelled context: got %v, want context.Canceled", err)
	}
}

// TestClassifyWatchError pins the watch retry decision (the D4 fix): the SS "poll instead" signals
// (watch_cancelled 1029, process_behind 1037), the SS watch-timeout/future-version (1004/1009), and
// the wrong-shard relocate are all RETRYABLE — only wrong-shard/all-alts invalidates+bounds. A revert
// that makes any poll-signal terminal fails here. Mirrors C++ watchValue catch arms
// (NativeAPI.actor.cpp:3993-4012).
func TestClassifyWatchError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		wantDelay time.Duration
		wantRetry bool
		wantInval bool
	}{
		{"wrong_shard_1001", &wire.FDBError{Code: ErrWrongShardServer}, wrongShardRetryDelay, true, true},
		{"all_alternatives_1006", &wire.FDBError{Code: ErrAllAlternativesFailed}, wrongShardRetryDelay, true, true},
		{"watch_cancelled_1029", &wire.FDBError{Code: ErrWatchCancelled}, watchPollingTime, true, false},
		{"process_behind_1037", &wire.FDBError{Code: ErrProcessBehind}, watchPollingTime, true, false},
		{"timed_out_1004", &wire.FDBError{Code: ErrTimedOut}, futureVersionDelay, true, false},
		{"future_version_1009", &wire.FDBError{Code: ErrFutureVersion}, futureVersionDelay, true, false},
		{"not_committed_terminal", &wire.FDBError{Code: ErrNotCommitted}, 0, false, false},
		{"database_locked_terminal", &wire.FDBError{Code: ErrDatabaseLocked}, 0, false, false},
		{"non_fdb_terminal", errors.New("boom"), 0, false, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			delay, retry, inval := classifyWatchError(tc.err)
			if delay != tc.wantDelay || retry != tc.wantRetry || inval != tc.wantInval {
				t.Fatalf("classifyWatchError = (delay=%v retry=%v inval=%v), want (delay=%v retry=%v inval=%v)",
					delay, retry, inval, tc.wantDelay, tc.wantRetry, tc.wantInval)
			}
		})
	}
}

// TestLockAwareReadOptions pins the shared ReadOptions all three lock-aware read paths (getValue,
// getKey, getKeyValues) send — the single source of truth, so it covers the getKey path that lacks
// its own build*Request round-trip. C++ LOCK_AWARE inits a default ReadOptions (Type=NORMAL(3),
// CacheResult=true) then sets lockAware (NativeAPI.actor.cpp:7072; FDBTypes.h:1748). Revert-proven:
// {LockAware:true} alone (Type=0/EAGER, CacheResult=false) fails here.
func TestLockAwareReadOptions(t *testing.T) {
	t.Parallel()
	ro := lockAwareReadOptions()
	if ro.Type != readTypeNormal {
		t.Errorf("Type: got %d, want %d (ReadType::NORMAL)", ro.Type, readTypeNormal)
	}
	if !ro.CacheResult {
		t.Error("CacheResult: got false, want true")
	}
	if !ro.LockAware {
		t.Error("LockAware: got false, want true")
	}
}
