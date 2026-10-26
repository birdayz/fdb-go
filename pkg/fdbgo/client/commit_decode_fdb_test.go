package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// A malformed reply after dispatch is uncertain, even when the real commit applied.
// NativeAPI.actor.cpp:6730-6772 fences uncertainty before returning 1021.
func TestMalformedCommitReplyFencesUnknownResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		corruption string
		conflict   bool
		fenceError bool
		wantCode   int
	}{
		{name: "CommitID version", corruption: "version", wantCode: 1021},
		{name: "ErrorOr envelope", corruption: "envelope", wantCode: 1021},
		{name: "FDB not_committed control", conflict: true, wantCode: 1020},
		{name: "fence failure propagates", corruption: "version", fenceError: true, wantCode: 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			gate := &commitReplyGate{reached: make(chan struct{})}
			rec := &sendRecorder{}
			db := newTestDatabase(t, ctx, sharedClusterFile, rec.wrap(gate.dial))
			t.Cleanup(func() { _ = db.Close() })
			t.Cleanup(func() { _ = gate.release() })

			key := []byte("commit-decode/" + t.Name())
			committed := []byte("original mutation applied")
			tx := db.CreateTransaction()
			if _, err := tx.Get(ctx, key); err != nil {
				t.Fatalf("establish read conflict and version: %v", err)
			}
			wantStored := committed
			if tc.conflict {
				wantStored = []byte("competing mutation applied")
				other := db.CreateTransaction()
				other.Set(key, wantStored)
				if err := other.Commit(ctx); err != nil {
					t.Fatalf("competing commit: %v", err)
				}
			}
			tx.Set(key, committed)
			rec.start()
			gate.mu.Lock()
			gate.armed = true
			originalReached := gate.reached
			gate.mu.Unlock()
			done := startGatedCommit(tx, ctx)
			select {
			case <-originalReached:
			case err := <-done:
				t.Fatalf("commit returned before its real reply was withheld: %v", err)
			case <-ctx.Done():
				t.Fatalf("no real commit reply: %v", ctx.Err())
			}

			gate.mu.Lock()
			if len(gate.held) != 1 {
				gate.mu.Unlock()
				t.Fatal("expected exactly one original commit reply")
			}
			original := gate.held[0]
			gate.held = nil
			fenceReached := make(chan struct{})
			gate.reached = fenceReached
			gate.mu.Unlock()

			outcome, originalErr := parseCommitOutcome(original.body)
			if tc.conflict {
				var r wire.Reader
				var fdbErr *wire.FDBError
				if err := wire.ReadErrorOrInto(original.body, &r); !errors.As(err, &fdbErr) || fdbErr.Code != 1020 {
					t.Fatalf("control must be a real ErrorOr not_committed reply: %v", err)
				}
			} else if originalErr != nil || outcome.version <= 0 {
				t.Fatalf("original must really commit before corruption: version=%d err=%v", outcome.version, originalErr)
			}

			bad := bytes.Clone(original.body)
			switch tc.corruption {
			case "version":
				var r wire.Reader
				if err := wire.ReadErrorOrInto(bad, &r); err != nil {
					t.Fatal(err)
				}
				obj := r.ObjectPos()
				vt := obj - int(int32(binary.LittleEndian.Uint32(bad[obj:])))
				entry := vt + 4 + 2*types.CommitIDSlotVersion
				if r.FieldOffset(types.CommitIDSlotVersion) < 4 || obj+r.FieldOffset(types.CommitIDSlotVersion)+8 > len(bad) {
					t.Fatal("real CommitID must have an in-bounds version field before corruption")
				}
				binary.LittleEndian.PutUint16(bad[entry:], 0xfff0)
				_, err := parseCommitOutcome(bad)
				var decodeErr *wire.DecodeError
				var bounds *wire.FieldBoundsError
				if !errors.As(err, &decodeErr) || decodeErr.Type != "CommitID" ||
					!errors.As(err, &bounds) || bounds.Slot != types.CommitIDSlotVersion {
					t.Fatalf("corruption must produce a CommitID version decode failure: %v", err)
				}
			case "envelope":
				bad = bad[:7]
				_, err := parseCommitOutcome(bad)
				var fdbErr *wire.FDBError
				var decodeErr *wire.DecodeError
				if err == nil || errors.As(err, &fdbErr) || errors.As(err, &decodeErr) {
					t.Fatalf("truncated envelope must produce a non-FDB root parse failure: %v", err)
				}
			}
			if err := original.conn.forward(original.token, bad); err != nil {
				t.Fatalf("deliver original reply: %v", err)
			}

			var commitErr error
			finished := false
			select {
			case commitErr = <-done:
				finished = true
			case <-fenceReached:
				gate.mu.Lock()
				fenceReply := gate.held[0]
				gate.mu.Unlock()
				if fenced, err := parseCommitOutcome(fenceReply.body); err != nil || fenced.version <= 0 {
					t.Fatalf("real fence must commit successfully: version=%d err=%v", fenced.version, err)
				}
				select {
				case commitErr = <-done:
					finished = true
					t.Errorf("public commit returned before the withheld fence reply: %v", commitErr)
				default:
				}
				if tc.fenceError {
					gate.mu.Lock()
					gate.held[0].body = (&types.ErrorOrError{ErrorCode: 2000}).MarshalFDB()
					gate.mu.Unlock()
				}
			case <-ctx.Done():
				t.Fatalf("neither a fence nor a commit result arrived: %v", ctx.Err())
			}
			releaseCommitGate(t, gate)
			if !finished {
				commitErr = awaitGatedCommit(t, ctx, done)
			}

			originalRequests, fenceRequests := 0, 0
			for _, write := range rec.stop().writes {
				if len(write.body) < 8 || binary.LittleEndian.Uint32(write.body[4:8]) != types.CommitTransactionRequestFileID {
					continue
				}
				var req types.CommitTransactionRequest
				if err := req.UnmarshalFDB(write.body); err != nil {
					t.Fatalf("decode recorded commit request: %v", err)
				}
				tr := req.Transaction
				if len(tr.Mutations) != 0 {
					originalRequests++
					if len(tr.Mutations) != 1 || tr.Mutations[0].MutType != 0 ||
						!bytes.Equal(tr.Mutations[0].Param1, key) || !bytes.Equal(tr.Mutations[0].Param2, committed) {
						t.Fatalf("unexpected application mutations: %+v", tr.Mutations)
					}
					continue
				}
				fenceRequests++
				if len(tr.ReadConflictRanges) != 1 || len(tr.WriteConflictRanges) != 1 ||
					!singleKeyRangeIs(tr.ReadConflictRanges[0], key) || !singleKeyRangeIs(tr.WriteConflictRanges[0], key) ||
					req.TenantInfo.TenantId != NoTenantID {
					t.Fatalf("fence must conflict on the original key without mutations: %+v", req)
				}
			}
			if originalRequests != 1 {
				t.Errorf("application commit sent %d times; want exactly once", originalRequests)
			}
			if tc.conflict {
				if fenceRequests != 0 {
					t.Errorf("authoritative not_committed reply sent %d fences; want none", fenceRequests)
				}
			} else if fenceRequests == 0 {
				t.Error("malformed post-send reply returned without a fence request")
			}
			if tc.fenceError && fenceRequests != 1 {
				t.Errorf("nonretryable fence failure sent %d fences; want exactly one", fenceRequests)
			}
			var fdbErr *wire.FDBError
			if !errors.As(commitErr, &fdbErr) || fdbErr.Code != tc.wantCode {
				t.Errorf("commit returned %v with %d fence requests; want FDB error %d", commitErr, fenceRequests, tc.wantCode)
			}
			check := db.CreateTransaction()
			got, err := check.Get(ctx, key)
			if err != nil || !bytes.Equal(got, wantStored) {
				t.Fatalf("durable value=%q err=%v; want %q", got, err, wantStored)
			}
		})
	}
}
