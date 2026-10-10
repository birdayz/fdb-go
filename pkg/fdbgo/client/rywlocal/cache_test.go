package rywlocal_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// Count real storage requests, not GRVs: a cached read can incorrectly reuse its
// existing read version and still send an unnecessary storage RPC.
type cacheReadCounter struct {
	reads atomic.Int64
	mu    sync.Mutex
	err   error
}

func (c *cacheReadCounter) recordError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

func (c *cacheReadCounter) count(t *testing.T) int64 {
	t.Helper()
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err != nil {
		t.Fatalf("storage request observer: %v", err)
	}
	return c.reads.Load()
}

func (c *cacheReadCounter) dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &cacheCountingConn{Conn: conn, counter: c, handshake: transport.ConnectPacketSize}, nil
}

// The connection remains a real FDB connection. This observer only decodes a
// copy of outgoing plaintext frames, including coalesced and split writes.
type cacheCountingConn struct {
	net.Conn
	counter   *cacheReadCounter
	mu        sync.Mutex
	handshake int
	buffer    []byte
}

func (c *cacheCountingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.observe(p)
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *cacheCountingConn) observe(p []byte) {
	if c.handshake > 0 {
		n := min(c.handshake, len(p))
		c.handshake -= n
		p = p[n:]
	}
	c.buffer = append(c.buffer, p...)
	for len(c.buffer) >= 4 {
		payload := int(binary.LittleEndian.Uint32(c.buffer[:4]))
		if payload < 16 || payload > 100<<20 {
			c.counter.recordError(fmt.Errorf("invalid outgoing payload size %d", payload))
			c.buffer = nil
			return
		}
		frameSize := 12 + payload // length, checksum, endpoint and body
		if len(c.buffer) < frameSize {
			return
		}
		var fr transport.FrameReader
		_, body, err := fr.Read(bytes.NewReader(c.buffer[:frameSize]), false)
		if err != nil {
			c.counter.recordError(err)
		} else {
			r, err := wire.NewReader(body)
			if err != nil {
				c.counter.recordError(err)
			} else {
				// StorageServerInterface.h: GetValue, GetKey, GetKeyValues.
				switch r.FileIdentifier() {
				case 8454530, 10457870, 6795746:
					c.counter.reads.Add(1)
				}
			}
		}
		c.buffer = c.buffer[frameSize:]
	}
}

func cacheDB(t *testing.T, cf *client.ClusterFile) (context.Context, *client.Database, *cacheReadCounter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	counter := &cacheReadCounter{}
	db, err := client.OpenDatabaseFromConfig(ctx, cf, client.WithAPIVersion(730), client.WithDialFunc(counter.dial))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return ctx, db, counter
}

func cacheSeed(t *testing.T, ctx context.Context, db *client.Database, key, value []byte) {
	t.Helper()
	_, err := db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		if value == nil {
			tx.Clear(key)
		} else {
			tx.Set(key, value)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func cachePointRead(t *testing.T, ctx context.Context, tx *client.Transaction, key []byte, kind string, local bool) ([]byte, error) {
	t.Helper()
	switch kind {
	case "point":
		return tx.Get(ctx, key)
	case "snapshot":
		return tx.Snapshot().Get(ctx, key)
	case "pipelined":
		value, pending, err := tx.GetPipelined(ctx, key)
		if errors.Is(err, client.ErrNeedFullRYW) {
			// The facade uses this fallback for unresolved atomic chains.
			return tx.Get(ctx, key)
		}
		if pending != nil {
			if local {
				t.Error("cached pipelined read returned a remote PendingGet")
			}
			return pending.Resolve()
		}
		return value, err
	case "range_forward", "range_reverse":
		rows, _, err := tx.GetRangeWithByteTarget(ctx, key, append(bytes.Clone(key), 0), 0, client.ByteLimitUnlimited, kind == "range_reverse")
		if err != nil || len(rows) == 0 {
			return nil, err
		}
		if len(rows) != 1 || !bytes.Equal(rows[0].Key, key) {
			t.Fatalf("exact-key range = %v, want only %q", rows, key)
		}
		return rows[0].Value, nil
	default:
		t.Fatalf("unknown point reader %q", kind)
		return nil, nil
	}
}

func cacheRequireValue(t *testing.T, got []byte, err error, want []byte) {
	t.Helper()
	if err != nil || !bytes.Equal(got, want) || (got == nil) != (want == nil) {
		t.Fatalf("value=%x err=%v, want %x (present=%t)", got, err, want, want != nil)
	}
}

func cacheRequireCode(t *testing.T, err error, want int) {
	t.Helper()
	var fe *wire.FDBError
	if !errors.As(err, &fe) || fe.Code != want {
		t.Errorf("error=%v, want FDB%d", err, want)
	}
}

func cacheRequireLocal(t *testing.T, counter *cacheReadCounter, before int64) {
	t.Helper()
	if got := counter.count(t) - before; got != 0 {
		t.Errorf("LOCAL_CACHE_REMOTE_READS: sent %d storage requests, want 0", got)
	}
}

func cacheRequireRemote(t *testing.T, counter *cacheReadCounter, before int64) {
	t.Helper()
	if got := counter.count(t) - before; got <= 0 {
		t.Errorf("remote positive control sent %d storage requests, want >0", got)
	}
}

func cacheRequireRows(t *testing.T, got, want []client.KeyValue, more bool, err error) {
	t.Helper()
	if err != nil || more || len(got) != len(want) {
		t.Fatalf("rows=%v more=%t err=%v, want %v and no more", got, more, err, want)
	}
	for i := range want {
		if !bytes.Equal(got[i].Key, want[i].Key) || !bytes.Equal(got[i].Value, want[i].Value) {
			t.Fatalf("row %d=%q:%x, want %q:%x", i, got[i].Key, got[i].Value, want[i].Key, want[i].Value)
		}
	}
}

func TestRYWCacheAdmissions(t *testing.T) {
	t.Parallel()
	cf := rangeCluster(t)
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *client.ClusterFile)
	}{
		{"cached_point_reads", testCachedReadAdmissions},
		{"snapshot_cache", testSnapshotRYWDisableKeepsStorageCache},
		{"dependent_atomics", testCachedDependentAtomicReadsKeepConflicts},
		{"snapshot_atomics_commit", testSnapshotAtomicReadsPreserveCommitMutations},
		{"ryw_disabled", testRYWDisabledPipelinedReadsRemainRemote},
		{"bypass_point_only", testBypassUnreadableAppliesOnlyToPointReads},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t, cf)
		})
	}
}

func testCachedReadAdmissions(t *testing.T, cf *client.ClusterFile) {
	for _, present := range []bool{false, true} {
		for _, warm := range []string{"point", "snapshot", "pipelined"} {
			for _, read := range []string{"point", "snapshot", "pipelined", "range_forward", "range_reverse"} {
				t.Run(fmt.Sprintf("warm=%s/read=%s/present=%t", warm, read, present), func(t *testing.T) {
					t.Parallel()
					ctx, db, counter := cacheDB(t, cf)
					key := []byte(t.Name() + "/k")
					var want []byte
					if present {
						want = []byte{1}
					}
					cacheSeed(t, ctx, db, key, want)
					tx := db.CreateTransaction()
					defer tx.Cancel()
					before := counter.count(t)
					value, err := cachePointRead(t, ctx, tx, key, warm, false)
					cacheRequireValue(t, value, err, want)
					cacheRequireRemote(t, counter, before)
					before = counter.count(t)
					value, err = cachePointRead(t, ctx, tx, key, read, true)
					cacheRequireValue(t, value, err, want)
					// ReadYourWrites.actor.cpp:101-124 serves cached KV/absence and
					// inserts every completed storage point read into that cache.
					cacheRequireLocal(t, counter, before)
				})
			}
		}
	}
}

func testSnapshotRYWDisableKeepsStorageCache(t *testing.T, cf *client.ClusterFile) {
	for _, disableBeforeWarm := range []bool{false, true} {
		for _, present := range []bool{false, true} {
			for _, read := range []string{"point", "range_forward", "range_reverse"} {
				t.Run(fmt.Sprintf("disable_before_warm=%t/present=%t/%s", disableBeforeWarm, present, read), func(t *testing.T) {
					t.Parallel()
					ctx, db, counter := cacheDB(t, cf)
					prefix := []byte(t.Name() + "/")
					k1, k2 := append(bytes.Clone(prefix), 'a'), append(bytes.Clone(prefix), 'z')
					end := append(bytes.Clone(k2), 0)
					var v1, v2 []byte
					if present {
						v1, v2 = []byte{1}, []byte{2}
					}
					cacheSeed(t, ctx, db, k1, v1)
					cacheSeed(t, ctx, db, k2, v2)
					tx := db.CreateTransaction()
					defer tx.Cancel()
					if disableBeforeWarm {
						tx.SetSnapshotRYWDisable()
					}
					var want []client.KeyValue
					if present {
						want = []client.KeyValue{{Key: k1, Value: v1}, {Key: k2, Value: v2}}
						if read == "range_reverse" {
							want[0], want[1] = want[1], want[0]
						}
					}
					readSnapshot := func() {
						if read == "point" {
							value, err := tx.Snapshot().Get(ctx, k1)
							cacheRequireValue(t, value, err, v1)
							return
						}
						rows, more, err := tx.Snapshot().GetRangeWithByteTarget(ctx, k1, end, 0, client.ByteLimitUnlimited, read == "range_reverse")
						cacheRequireRows(t, rows, want, more, err)
					}
					before := counter.count(t)
					readSnapshot()
					cacheRequireRemote(t, counter, before)
					if !disableBeforeWarm {
						tx.SetSnapshotRYWDisable()
					}
					tx.Set(k1, []byte{9})
					tx.Clear(k2)
					tx.Set(append(bytes.Clone(prefix), 'm'), []byte{8})
					before = counter.count(t)
					readSnapshot()
					// C++ :366-376,400-403 chooses SnapshotCache, not readThrough.
					cacheRequireLocal(t, counter, before)
					value, err := tx.Get(ctx, k1)
					cacheRequireValue(t, value, err, []byte{9})
					cacheRequireLocal(t, counter, before)
				})
			}
		}
	}
}

func testCachedDependentAtomicReadsKeepConflicts(t *testing.T, cf *client.ClusterFile) {
	for _, present := range []bool{false, true} {
		for _, read := range []string{"point", "snapshot", "pipelined", "range_forward", "range_reverse"} {
			t.Run(fmt.Sprintf("present=%t/%s", present, read), func(t *testing.T) {
				t.Parallel()
				ctx, db, counter := cacheDB(t, cf)
				key := []byte(t.Name() + "/k")
				var base []byte
				want := []byte{1}
				if present {
					base, want = []byte{1}, []byte{2}
				}
				cacheSeed(t, ctx, db, key, base)
				tx := db.CreateTransaction()
				defer tx.Cancel()
				before := counter.count(t)
				value, err := tx.Snapshot().Get(ctx, key)
				cacheRequireValue(t, value, err, base)
				cacheRequireRemote(t, counter, before)
				tx.Atomic(client.MutAddValue, key, []byte{1})
				before = counter.count(t)
				value, err = cachePointRead(t, ctx, tx, key, read, true)
				cacheRequireValue(t, value, err, want)
				// RYWIterator.cpp:38-41,82-84: dependent + known base is KV.
				cacheRequireLocal(t, counter, before)
				if read == "snapshot" {
					// Snapshot itself must add no conflict; a later ordinary read
					// must still see the entry as dependent on the storage base.
					value, err = tx.Get(ctx, key)
					cacheRequireValue(t, value, err, want)
					cacheRequireLocal(t, counter, before)
				}
				cacheSeed(t, ctx, db, key, []byte{9})
				// ReadYourWrites.actor.cpp:328-330 records dependent read conflicts.
				cacheRequireCode(t, tx.Commit(ctx), 1020)
			})
		}
	}
}

// Snapshot reads may evaluate an atomic, but must not turn it into a SET/CLEAR
// at commit: C++ RYWIterator.cpp:82-91 and ReadYourWrites.actor.cpp:2035-2060.
func testSnapshotAtomicReadsPreserveCommitMutations(t *testing.T, cf *client.ClusterFile) {
	for _, warm := range []bool{false, true} {
		for _, op := range []client.MutationType{client.MutAddValue, client.MutCompareAndClear} {
			for _, read := range []string{"point", "range_forward", "range_reverse"} {
				t.Run(fmt.Sprintf("warm=%t/op=%d/%s", warm, op, read), func(t *testing.T) {
					t.Parallel()
					ctx, db, counter := cacheDB(t, cf)
					key := []byte(t.Name() + "/k")
					cacheSeed(t, ctx, db, key, []byte{1})
					tx := db.CreateTransaction()
					defer tx.Cancel()
					if warm {
						before := counter.count(t)
						value, err := tx.Snapshot().Get(ctx, key)
						cacheRequireValue(t, value, err, []byte{1})
						cacheRequireRemote(t, counter, before)
					}
					tx.Atomic(op, key, []byte{1})
					var wantRead []byte
					wantCommit := []byte{9}
					if op == client.MutAddValue {
						wantRead, wantCommit = []byte{2}, []byte{10}
					}
					if read == "point" {
						value, err := tx.Snapshot().Get(ctx, key)
						cacheRequireValue(t, value, err, wantRead)
					} else {
						rows, more, err := tx.Snapshot().GetRangeWithByteTarget(ctx, key, append(bytes.Clone(key), 0), 0, client.ByteLimitUnlimited, read == "range_reverse")
						var wantRows []client.KeyValue
						if wantRead != nil {
							wantRows = []client.KeyValue{{Key: key, Value: wantRead}}
						}
						cacheRequireRows(t, rows, wantRows, more, err)
					}
					cacheSeed(t, ctx, db, key, []byte{9})
					if err := tx.Commit(ctx); err != nil {
						t.Fatalf("snapshot-only atomic commit must not conflict: %v", err)
					}
					verify := db.CreateTransaction()
					defer verify.Cancel()
					value, err := verify.Get(ctx, key)
					cacheRequireValue(t, value, err, wantCommit)
				})
			}
		}
	}
}

func testRYWDisabledPipelinedReadsRemainRemote(t *testing.T, cf *client.ClusterFile) {
	for _, read := range []string{"point", "snapshot", "pipelined", "snapshot_key", "range_forward", "range_reverse"} {
		t.Run(read, func(t *testing.T) {
			t.Parallel()
			ctx, db, counter := cacheDB(t, cf)
			key := []byte(t.Name() + "/k")
			cacheSeed(t, ctx, db, key, []byte{1})
			tx := db.CreateTransaction()
			defer tx.Cancel()
			tx.SetReadYourWritesDisable()
			tx.Set(key, []byte{9})
			before := counter.count(t)
			gotKey, err := tx.Snapshot().GetKey(ctx, key, false, 1)
			cacheRequireValue(t, gotKey, err, key)
			cacheRequireRemote(t, counter, before)
			for range 2 {
				before = counter.count(t)
				if read == "snapshot_key" {
					value, err := tx.Snapshot().GetKey(ctx, key, false, 1)
					cacheRequireValue(t, value, err, key)
				} else {
					value, err := cachePointRead(t, ctx, tx, key, read, false)
					cacheRequireValue(t, value, err, []byte{1})
				}
				// C++ :400-401 selects readThrough even after earlier reads.
				cacheRequireRemote(t, counter, before)
			}
		})
	}
}

func testBypassUnreadableAppliesOnlyToPointReads(t *testing.T, cf *client.ClusterFile) {
	for _, overwrite := range []bool{false, true} {
		for _, read := range []string{"point", "snapshot", "pipelined", "get_key", "snapshot_get_key", "range_forward", "range_reverse", "snapshot_range_forward", "snapshot_range_reverse"} {
			t.Run(fmt.Sprintf("overwrite=%t/%s", overwrite, read), func(t *testing.T) {
				t.Parallel()
				ctx, db, counter := cacheDB(t, cf)
				tx := db.CreateTransaction()
				defer tx.Cancel()
				key := []byte(t.Name() + "/k")
				operand := append(bytes.Repeat([]byte{0xff}, 10), 0, 0, 0, 0)
				tx.SetBypassUnreadable(true)
				tx.Atomic(client.MutSetVersionstampedValue, key, operand)
				want := operand
				if overwrite {
					want = []byte("plain")
					tx.Set(key, want)
				}
				before := counter.count(t)
				var err error
				switch read {
				case "point", "snapshot", "pipelined":
					var value []byte
					value, err = cachePointRead(t, ctx, tx, key, read, true)
					cacheRequireValue(t, value, err, want)
				case "get_key":
					_, err = tx.GetKey(ctx, key, false, 1)
				case "snapshot_get_key":
					_, err = tx.Snapshot().GetKey(ctx, key, false, 1)
				case "range_forward", "range_reverse":
					_, _, err = tx.GetRangeWithByteTarget(ctx, key, append(bytes.Clone(key), 0), 0, client.ByteLimitUnlimited, read == "range_reverse")
				case "snapshot_range_forward", "snapshot_range_reverse":
					_, _, err = tx.Snapshot().GetRangeWithByteTarget(ctx, key, append(bytes.Clone(key), 0), 0, client.ByteLimitUnlimited, read == "snapshot_range_reverse")
				}
				// C++ ReadYourWrites.actor.cpp:98-99 enables bypass only in
				// GetValueReq; GetKey/GetRange (:140-169) retain unreadability.
				if read != "point" && read != "snapshot" && read != "pipelined" {
					cacheRequireCode(t, err, 1036)
				}
				cacheRequireLocal(t, counter, before)
				if _, ok := tx.ReadVersionInstant(); ok {
					t.Error("local bypass/unreadable admission acquired a read version")
				}
			})
		}
	}
}
