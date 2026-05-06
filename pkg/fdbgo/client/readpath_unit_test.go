package client

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
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
		{"FDB 1062", &wire.FDBError{Code: ErrWrongShardServer}, true},
		{"wrapped FDB 1062", fmt.Errorf("send: %w", &wire.FDBError{Code: ErrWrongShardServer}), true},
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
		{"FDB 1062", &wire.FDBError{Code: ErrWrongShardServer}, false},
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

	body, bufp := buildGetValueRequest(key, version, false, tenantID, replyToken, transport.UID{})
	defer getValueBufPool.Put(bufp)

	var req types.GetValueRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if string(req.Key) != string(key) {
		t.Errorf("Key: got %q, want %q", req.Key, key)
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
		transport.UID{First: 1, Second: 2}, transport.UID{})
	defer getValueBufPool.Put(bufp)

	var req types.GetValueRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if !req.HasOptions {
		t.Fatal("HasOptions: got false, want true (lockAware=true)")
	}
	if !req.Options.HasLockAware {
		t.Error("Options.HasLockAware: got false, want true")
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
		replyToken, transport.UID{})
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
		transport.UID{First: 1, Second: 2}, transport.UID{},
	)
	defer getKeyValuesBufPool.Put(bufp)

	var req types.GetKeyValuesRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatalf("UnmarshalFDB: %v", err)
	}
	if !req.HasOptions || !req.Options.HasLockAware {
		t.Errorf("lock-aware not set: HasOptions=%v Options.HasLockAware=%v",
			req.HasOptions, req.Options.HasLockAware)
	}
}
