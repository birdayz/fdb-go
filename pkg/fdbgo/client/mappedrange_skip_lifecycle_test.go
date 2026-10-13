package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// Hold each real FDB read phase before the transport publishes its response.
func TestMappedSkipPendingRetirementAcrossResolutionAndMappedRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, dialer := newSimTestDB(t, ctx)
	db.db.hedgeEnabled.Store(false) // no second replica can bypass a held response

	for _, phase := range []string{"mapped", "selector"} {
		for _, reverse := range []bool{false, true} {
			for _, action := range []string{"Reset", "Cancel"} {
				t.Run(fmt.Sprintf("%s/reverse=%v/%s", phase, reverse, action), func(t *testing.T) {
					fixture := newMappedFixture(t, 7)
					fixture.seed(t, ctx, db, 1, func(i int) []byte { return []byte(fmt.Sprintf("record-%d", i)) })
					begin, end := fixture.indexRange()
					mapper := fixture.getRangeMapper()
					const initialSkip int32 = 2
					wantFirst := int(initialSkip)
					if reverse {
						wantFirst = fixture.n - 1 - int(initialSkip)
					}
					// A successful positive-Skip control proves the populated range and
					// the exact requested selector before fault injection is armed.
					control := db.CreateTransaction()
					rows, _, err := control.GetMappedRangeWithByteTargetAndSkip(ctx, begin, end, mapper, 2, ByteLimitUnlimited, initialSkip, reverse)
					control.Cancel()
					if err != nil || len(rows) == 0 || !bytes.Equal(rows[0].Key, fixture.indexEntryKey(wantFirst)) {
						t.Fatalf("positive-Skip success control: rows=%+v error=%v want first primary %q", rows, err, fixture.indexEntryKey(wantFirst))
					}

					tx := db.CreateTransaction()
					defer tx.Cancel()
					tx.SetSkipGrvCache()
					if rv, err := tx.GetReadVersion(ctx); err != nil || rv <= 0 {
						t.Fatalf("pin real read version: %d, %v", rv, err)
					}
					tx.rpcTimeoutOverride = time.Hour
					oldMarker := []byte(t.Name() + "/discarded")
					newMarker := []byte(t.Name() + "/replacement")
					tx.Set(oldMarker, []byte("old-incarnation"))

					operation, releaseOperation := tx.opContext(ctx)
					defer releaseOperation()
					originalOp := tx.readOperation(operation)
					if originalOp == nil || originalOp.inc == nil {
						t.Fatal("mapped pending read has no captured operation/incarnation")
					}
					originalInc := originalOp.inc
					type heldFrame struct {
						token transport.UID
						body  []byte
					}
					parked := make(chan heldFrame, 1)
					releaseReply, releaseReplyIt := releaseGate(t)
					defer releaseReplyIt()
					var mappedSeen, keySeen atomic.Int32
					var held atomic.Bool
					dialer.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
						if len(body) < 8 {
							return body, false
						}
						fileID := binary.LittleEndian.Uint32(body[4:8])
						switch fileID {
						case 2<<24 | types.GetMappedKeyValuesReplyFileID:
							mappedSeen.Add(1)
							if phase != "mapped" {
								return body, false
							}
						case 2<<24 | types.GetKeyReplyFileID:
							keySeen.Add(1)
							if phase != "selector" {
								return body, false
							}
						default:
							return body, false // GRV/location/commit/other replies stay real
						}
						if held.CompareAndSwap(false, true) {
							parked <- heldFrame{token: token, body: body}
							<-releaseReply
						}
						return body, false
					})
					defer dialer.setIntercept(nil)
					dialer.armAll()

					skip := initialSkip
					pending := tx.StartGetMappedRangeWithSkip(operation, begin, end, mapper, 2, ByteLimitUnlimited, skip, reverse)
					skip = 6 // the started operation owns its signed 32-bit argument
					var frame heldFrame
					select {
					case frame = <-parked:
					case <-ctx.Done():
						t.Fatalf("%s did not reach its actual storage reply: %v", phase, ctx.Err())
					}
					if frame.token == (transport.UID{}) {
						t.Fatal("held frame has no actual reply-promise token")
					}
					if phase == "mapped" {
						heldRows, _, _, err := parseGetMappedKeyValuesReply(frame.body)
						if err != nil || len(heldRows) == 0 || !bytes.Equal(heldRows[0].Key, fixture.indexEntryKey(wantFirst)) || keySeen.Load() != 1 {
							t.Fatalf("held mapped reply: rows=%+v error=%v getKey=%d", heldRows, err, keySeen.Load())
						}
					} else {
						key, orEqual, offset, _, err := parseGetKeyReply(frame.body)
						wantResolved := int(initialSkip)
						if reverse {
							wantResolved = fixture.n - int(initialSkip)
						}
						if err != nil || !orEqual || offset != 0 || !bytes.Equal(key, fixture.indexEntryKey(wantResolved)) || mappedSeen.Load() != 0 || keySeen.Load() != 1 {
							t.Fatalf("held native selector resolution: key=%q orEqual=%v offset=%d error=%v mapped=%d getKey=%d want=%q", key, orEqual, offset, err, mappedSeen.Load(), keySeen.Load(), fixture.indexEntryKey(wantResolved))
						}
					}
					tx.readErrMu.Lock()
					currentInc, issued := tx.readLife, originalInc.issuedReads
					tx.readErrMu.Unlock()
					if currentInc != originalInc || issued != 0 || tx.readOperation(operation) != originalOp {
						t.Fatalf("pending rebound its operation or introduced a Commit barrier: sameInc=%v sameOp=%v issued=%d", currentInc == originalInc, tx.readOperation(operation) == originalOp, issued)
					}

					done := make(chan error, 1)
					go func() {
						rows, more, err := pending.Get()
						if len(rows) != 0 || more {
							done <- fmt.Errorf("retired pending read published held data: rows=%d more=%v error=%v", len(rows), more, err)
							return
						}
						done <- err
					}()
					var resetDone <-chan struct{}
					if action == "Reset" {
						// The explicitly captured operation stays leased until its
						// pending result completes; Reset must cancel it before draining.
						resetDone = retireParkedRead(t, ctx, tx)
					} else {
						tx.Cancel()
					}
					waitForFDB1025(t, done, phase+" positive-Skip "+action)
					if callerErr := ctx.Err(); callerErr != nil {
						t.Fatalf("transaction retirement canceled the caller: %v", callerErr)
					}
					if code := fdbCodeOf(tx.readIncarnationCause(originalInc)); code != 1025 {
						t.Fatalf("retired original operation cause=%d, want literal1025", code)
					}
					rows, more, err := pending.Get()
					if len(rows) != 0 || more || fdbCodeOf(err) != 1025 {
						t.Fatalf("memoized pending changed after retirement: rows=%+v more=%v error=%v", rows, more, err)
					}
					releaseOperation()
					if resetDone != nil {
						waitReadParked(t, ctx, resetDone, "Reset after canceled mapped operation drain")
					} else {
						tx.Reset()
					}
					tx.readErrMu.Lock()
					replacement := tx.readIncarnationLocked()
					tx.readErrMu.Unlock()
					if replacement == originalInc || tx.readOperation(operation).inc != originalInc {
						t.Fatal("Reset failed to separate the replacement from the original operation token")
					}

					// Let the successful obsolete reply arrive only after 1025 and
					// replacement publication. It must not satisfy or poison reuse.
					releaseReplyIt()
					dialer.setIntercept(nil)
					rows, _, err = tx.GetMappedRangeWithByteTargetAndSkip(ctx, begin, end, mapper, 2, ByteLimitUnlimited, initialSkip, reverse)
					if err != nil || len(rows) == 0 || !bytes.Equal(rows[0].Key, fixture.indexEntryKey(wantFirst)) {
						t.Fatalf("replacement positive mapped read: rows=%+v error=%v", rows, err)
					}
					tx.Set(newMarker, []byte("new-incarnation"))
					if err := tx.Commit(ctx); err != nil {
						t.Fatalf("replacement Commit after late %s reply: %v", phase, err)
					}
					assertIndependentRead(t, ctx, db, newMarker, []byte("new-incarnation"))
					assertIndependentRead(t, ctx, db, oldMarker, nil)
				})
			}
		}
	}
}
