package client

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// TestRequestTranscriptGate is RFC-262 phase 1's gate: the storage plumbing
// changed (selector-capable builders, rangeLimits, transformRangeLimits, the
// operation's read version passed down) but no request may change. Every case
// runs the same read twice on the same cluster and shard layout, once through
// the pre-change copy (request_transcript_legacy_test.go) and once through the
// current code, and requires the same storage and location requests, field
// for field, and the same result.
func TestRequestTranscriptGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  shardSizeConfig
	}{{"multishard", defaultShardSize}, {"multishard-larger", largerShardSize}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			env := setupMultiShardEnvWithConfig(t, ctx, tc.cfg)
			connStr, err := env.container.ClusterFile(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cf, err := ParseClusterString(connStr)
			if err != nil {
				t.Fatal(err)
			}
			g := newTranscriptGate(t, ctx, cf, plainDial)
			f := mappedFixture{prefix: []byte("tgmapped"), n: 60}
			pad := bytes.Repeat([]byte{'p'}, 8*1024)
			for batch := 0; batch < 6; batch++ {
				g.seed(func(tx *Transaction) {
					for i := batch * 10; i < (batch+1)*10; i++ {
						tx.Set(f.indexEntryKey(i), pad)
						tx.Set(f.recordKey(i), []byte(fmt.Sprintf("record-%08d", i)))
					}
				})
			}
			mb, me := f.indexRange()
			g.requireShards([]byte(env.prefix), append([]byte(env.prefix), 0xff), 2)
			g.requireShards(mb, me, 2)
			g.runAll(transcriptCases([]byte(env.prefix), f, 2))
		})
	}
	t.Run("single-shard-faults", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		connectCF, _ := startProxyFDBContainer(t, ctx)
		sd := newSimDialer()
		g := newTranscriptGate(t, ctx, connectCF, sd.dial)
		prefix := []byte("tg_")
		g.seed(func(tx *Transaction) {
			for i := 0; i < 100; i++ {
				tx.Set([]byte(fmt.Sprintf("%s%04d", prefix, i)), bytes.Repeat([]byte{'v'}, 1000))
			}
		})
		f := mappedFixture{prefix: []byte("tg_mapped"), n: 20}
		g.seed(func(tx *Transaction) {
			for i := 0; i < f.n; i++ {
				tx.Set(f.indexEntryKey(i), []byte{})
				tx.Set(f.recordKey(i), []byte(fmt.Sprintf("record-%08d", i)))
			}
		})
		g.runAll(transcriptCases(prefix, f, 1))

		// Faults hit only the storage connection, so the relocation's location
		// requests pass through and are part of the compared transcript.
		sd.armAddr(storageAddrFor(t, g.db, ctx, prefix))
		for _, c := range transcriptCases(prefix, f, 1) {
			if !c.faultable {
				continue
			}
			for _, fault := range []struct {
				name  string
				reply func(body []byte) ([]byte, bool)
				setup func(tx *Transaction)
			}{
				{"wrong-shard", func([]byte) ([]byte, bool) { return inlineErrorReply(1001, injectPenalty), false }, nil},
				{"reply-timeout", func([]byte) ([]byte, bool) { return nil, true }, func(tx *Transaction) { tx.rpcTimeoutOverride = 300 * time.Millisecond }},
			} {
				fc := c
				fc.name = c.name + "/" + fault.name
				fc.setup = fault.setup
				fc.warmLocations = true
				fc.fault = func() (fired func() int, disarm func()) {
					var n atomic.Int32
					sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
						if n.Add(1) == 1 {
							return fault.reply(body)
						}
						return body, false
					})
					return func() int { return int(n.Load()) }, func() { sd.setIntercept(nil) }
				}
				g.pair(fc)
			}
		}
	})
}

// readPath is one implementation of the gated entry points.
type readPath struct {
	name          string
	rangeRead     func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error)
	rywOffRange   func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error)
	getKey        func(ctx context.Context, tx *Transaction, key []byte, orEqual bool, offset int32) (string, error)
	mappedScan    func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limit, byteTarget int, reverse bool) (string, error)
	mappedSelect  func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error)
	mappedExactly func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error)
}

func renderKVs(kvs []KeyValue, more bool, readThrough []byte, err error) string {
	keys := make([]string, len(kvs))
	for i := range kvs {
		keys[i] = fmt.Sprintf("%q/%d", kvs[i].Key, len(kvs[i].Value))
	}
	return fmt.Sprintf("rows=%v more=%t readThrough=%q err=%v", keys, more, readThrough, err)
}

func renderMapped(rows []MappedKeyValue, more bool, err error) string {
	keys := make([]string, len(rows))
	for i := range rows {
		keys[i] = fmt.Sprintf("%q->%q", rows[i].Key, rows[i].GetValue.Key)
	}
	return fmt.Sprintf("rows=%v more=%t err=%v", keys, more, err)
}

var legacyReadPath = readPath{
	name: "legacy",
	rangeRead: func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error) {
		kvs, more, rt, err := tx.legacyGetRangeReadThrough(ctx, b, e, limit, byteTarget, reverse)
		return renderKVs(kvs, more, rt, err), err
	},
	// The RYW-disabled route calls getRangeReadThrough directly (range_issue.go:181).
	rywOffRange: func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error) {
		kvs, more, _, err := tx.legacyGetRangeReadThrough(ctx, b, e, limit, byteTarget, reverse)
		return renderKVs(kvs, more, nil, err), err
	},
	getKey: func(ctx context.Context, tx *Transaction, key []byte, orEqual bool, offset int32) (string, error) {
		k, fallback, err := tx.legacyGetKeyForRange(ctx, key, orEqual, offset)
		return fmt.Sprintf("key=%q fallback=%t err=%v", k, fallback, err), err
	},
	mappedScan: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limit, byteTarget int, reverse bool) (string, error) {
		if err := tx.ensureReadVersion(ctx); err != nil {
			return "", err
		}
		rows, more, err := tx.legacyGetMappedRangeImpl(ctx, b, e, mapper, limit, byteTarget, reverse)
		return renderMapped(rows, more, err), err
	},
	mappedSelect: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error) {
		if err := tx.ensureReadVersion(ctx); err != nil {
			return "", err
		}
		res, err := tx.legacyGetMappedRangeSelectors(ctx, mappedSelectorFGE(b), mappedSelectorFGE(e), mapper, limits, reverse)
		return renderMapped(res.rows, res.more, err), err
	},
	mappedExactly: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error) {
		if err := tx.ensureReadVersion(ctx); err != nil {
			return "", err
		}
		res, err := tx.legacyGetMappedRangeSelectorExact(ctx, b, e, mapper, limits, reverse)
		return renderMapped(res.rows, res.more, err), err
	},
}

var currentReadPath = readPath{
	name: "current",
	rangeRead: func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error) {
		kvs, more, rt, err := tx.getRangeReadThrough(ctx, b, e, limit, byteTarget, reverse)
		return renderKVs(kvs, more, rt, err), err
	},
	rywOffRange: func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error) {
		tx.SetReadYourWritesDisable()
		kvs, more, err := tx.GetRangeWithByteTarget(ctx, b, e, limit, byteTarget, reverse)
		return renderKVs(kvs, more, nil, err), err
	},
	getKey: func(ctx context.Context, tx *Transaction, key []byte, orEqual bool, offset int32) (string, error) {
		k, fallback, err := tx.getKeyForRange(ctx, key, orEqual, offset)
		return fmt.Sprintf("key=%q fallback=%t err=%v", k, fallback, err), err
	},
	mappedScan: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limit, byteTarget int, reverse bool) (string, error) {
		rv, err := tx.operationReadVersion(ctx)
		if err != nil {
			return "", err
		}
		rows, more, err := tx.getMappedRangeImpl(ctx, b, e, mapper, limit, byteTarget, rv, reverse)
		return renderMapped(rows, more, err), err
	},
	mappedSelect: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error) {
		rv, err := tx.operationReadVersion(ctx)
		if err != nil {
			return "", err
		}
		res, err := tx.getMappedRangeSelectors(ctx, mappedSelectorFGE(b), mappedSelectorFGE(e), mapper, limits, rv, reverse)
		return renderMapped(res.rows, res.more, err), err
	},
	mappedExactly: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error) {
		rv, err := tx.operationReadVersion(ctx)
		if err != nil {
			return "", err
		}
		res, err := tx.getMappedRangeSelectorExact(ctx, b, e, mapper, limits, rv, reverse)
		return renderMapped(res.rows, res.more, err), err
	},
}

type transcriptCase struct {
	name string
	run  func(ctx context.Context, p readPath, tx *Transaction) (string, error)
	// minStorage is the fewest storage requests the case must send: a
	// cross-shard case that sent one request did not cross a shard.
	minStorage int
	faultable  bool
	setup      func(tx *Transaction)
	fault      func() (fired func() int, disarm func())
	// warmLocations runs the case with every shard located, so a fault aimed at
	// the first reply on the storage connection hits a storage reply rather
	// than a location reply (one process serves both on a single-process cluster).
	warmLocations bool
}

// transcriptCases is the matrix of RFC-262 S2. crossShards is the number of
// storage requests an unlimited read of the whole prefix must at least send.
func transcriptCases(prefix []byte, f mappedFixture, crossShards int) []transcriptCase {
	pb, pe := prefix, append(append([]byte{}, prefix...), 0xff)
	key := func(i int) []byte { return []byte(fmt.Sprintf("%s%04d", prefix, i)) }
	var cases []transcriptCase
	for _, reverse := range []bool{false, true} {
		for _, limit := range []int{1, 5, 0} {
			for _, byteTarget := range []int{ByteLimitUnlimited, 256, 4096, 80000} {
				minStorage := 1
				if limit == 0 && byteTarget == ByteLimitUnlimited {
					minStorage = crossShards
				}
				cases = append(cases, transcriptCase{
					name: fmt.Sprintf("range/reverse=%t/limit=%d/bytes=%d", reverse, limit, byteTarget),
					run: func(ctx context.Context, p readPath, tx *Transaction) (string, error) {
						return p.rangeRead(ctx, tx, pb, pe, limit, byteTarget, reverse)
					},
					minStorage: minStorage,
					faultable:  limit == 0 && (byteTarget == ByteLimitUnlimited || byteTarget == 4096),
				})
			}
			cases = append(cases, transcriptCase{
				name: fmt.Sprintf("ryw-disabled/reverse=%t/limit=%d", reverse, limit),
				run: func(ctx context.Context, p readPath, tx *Transaction) (string, error) {
					return p.rywOffRange(ctx, tx, pb, pe, limit, 4096, reverse)
				},
				minStorage: 1,
			})
		}
	}
	for _, sel := range []struct {
		key     []byte
		orEqual bool
		offset  int32
	}{{key(10), false, 35}, {key(90), false, -40}, {key(50), true, 0}, {key(30), true, 1}, {key(0), false, 1}} {
		cases = append(cases, transcriptCase{
			name: fmt.Sprintf("getkey/%s/%t/%d", sel.key, sel.orEqual, sel.offset),
			run: func(ctx context.Context, p readPath, tx *Transaction) (string, error) {
				return p.getKey(ctx, tx, sel.key, sel.orEqual, sel.offset)
			},
			minStorage: 1,
			faultable:  sel.offset == 35,
		})
	}
	mb, me := f.indexRange()
	mapper := f.getValueMapper()
	for _, reverse := range []bool{false, true} {
		for _, lim := range []struct{ limit, bytes int }{{0, ByteLimitUnlimited}, {5, 4096}} {
			minStorage := 1
			if lim.limit == 0 {
				minStorage = crossShards
			}
			cases = append(cases,
				transcriptCase{
					name: fmt.Sprintf("mapped-scan/reverse=%t/limit=%d", reverse, lim.limit),
					run: func(ctx context.Context, p readPath, tx *Transaction) (string, error) {
						return p.mappedScan(ctx, tx, mb, me, mapper, lim.limit, lim.bytes, reverse)
					},
					minStorage: minStorage,
					faultable:  lim.limit == 0,
				},
				transcriptCase{
					name: fmt.Sprintf("mapped-selectors/reverse=%t/limit=%d", reverse, lim.limit),
					run: func(ctx context.Context, p readPath, tx *Transaction) (string, error) {
						return p.mappedSelect(ctx, tx, mb, me, mapper, newMappedRangeLimits(lim.limit, lim.bytes), reverse)
					},
					minStorage: minStorage,
					// A wrong shard sends the selector loop to the exact fallback.
					faultable: lim.limit == 0,
				},
				transcriptCase{
					name: fmt.Sprintf("mapped-exact/reverse=%t/limit=%d", reverse, lim.limit),
					run: func(ctx context.Context, p readPath, tx *Transaction) (string, error) {
						return p.mappedExactly(ctx, tx, mb, me, mapper, newMappedRangeLimits(lim.limit, lim.bytes), reverse)
					},
					minStorage: minStorage,
				})
		}
	}
	return cases
}

type transcriptGate struct {
	t   *testing.T
	ctx context.Context
	db  *Database
	rec *sendRecorder
}

func newTranscriptGate(t *testing.T, ctx context.Context, cf *ClusterFile, dial transport.DialFunc) *transcriptGate {
	t.Helper()
	rec := &sendRecorder{}
	db := newTestDatabase(t, ctx, cf, rec.wrap(dial))
	t.Cleanup(func() { db.Close() })
	// A hedge sends a second request after a load-dependent delay; it is not
	// part of what either path decides.
	db.db.hedgeEnabled.Store(false)
	return &transcriptGate{t: t, ctx: ctx, db: db, rec: rec}
}

func (g *transcriptGate) seed(fn func(tx *Transaction)) {
	g.t.Helper()
	if _, err := g.db.Transact(g.ctx, func(tx *Transaction) (any, error) { fn(tx); return nil, nil }); err != nil {
		g.t.Fatalf("seed: %v", err)
	}
}

// layout is the cluster's shard map, read with a cold location cache. It also
// pins each shard's replica: load balancing picks a replica at random
// (loadbalance.go:242-250), and which replica answers decides how far a getKey
// resolves before it must cross a shard, so the two runs would otherwise send
// different requests for a reason neither path controls. With every storage
// address marked failed until a distinct time, each request goes to the
// replica that is eligible soonest (soonestEligible, loadbalance.go:166-182).
func (g *transcriptGate) layout(t *testing.T) string {
	t.Helper()
	locs := g.locateAll(t)
	g.db.db.locCache.invalidateRange([]byte{}, []byte{0xff, 0xff}, NoTenantID)
	var b strings.Builder
	addrs := map[string]bool{}
	for _, l := range locs {
		fmt.Fprintf(&b, "[%q,%q)", l.ShardBegin, l.ShardEnd)
		for _, s := range l.Servers {
			b.WriteString(" " + s.Address)
			addrs[s.Address] = true
		}
		b.WriteString("\n")
	}
	sorted := make([]string, 0, len(addrs))
	for a := range addrs {
		sorted = append(sorted, a)
	}
	sort.Strings(sorted)
	q := g.db.db.queueModel
	q.mu.Lock()
	for i, a := range sorted {
		q.getOrCreate(a).failedUntil = nowSeconds() + 3600 + float64(i)
	}
	q.mu.Unlock()
	return b.String()
}

// locateAll fills the location cache with every shard.
func (g *transcriptGate) locateAll(t *testing.T) []LocationResult {
	t.Helper()
	g.db.db.locCache.invalidateRange([]byte{}, []byte{0xff, 0xff}, NoTenantID)
	locs, err := g.db.db.locCache.locateRange(g.db.db, g.ctx, []byte{}, []byte{0xff}, 10000, false, NoTenantID, types.SpanContext{})
	if err != nil {
		t.Fatalf("locate all: %v", err)
	}
	return locs
}

// requireShards waits until [begin, end) spans at least n shards: a gate that
// ran on one shard would only have compared the single-shard path.
func (g *transcriptGate) requireShards(begin, end []byte, n int) {
	g.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		locs, err := g.db.db.locCache.locateRange(g.db.db, g.ctx, begin, end, 1000, false, NoTenantID, types.SpanContext{})
		g.db.db.locCache.invalidateRange(begin, end, NoTenantID)
		if err == nil && len(locs) >= n {
			return
		}
		if time.Now().After(deadline) {
			g.t.Fatalf("[%q, %q) spans %d shards (err %v), want >= %d", begin, end, len(locs), err, n)
		}
		time.Sleep(2 * time.Second)
	}
}

func (g *transcriptGate) runAll(cases []transcriptCase) {
	for _, c := range cases {
		g.pair(c)
	}
}

// pair runs c through both paths at one read version with a cold location
// cache each time. Data distribution can move a shard between the two runs;
// a mismatch counts only when the shard map was the same before and after.
func (g *transcriptGate) pair(c transcriptCase) {
	g.t.Run(c.name, func(t *testing.T) {
		for attempt := 1; ; attempt++ {
			before := g.layout(t)
			rvTx := g.db.CreateTransaction()
			rv, err := rvTx.GetReadVersion(g.ctx)
			rvTx.Cancel()
			if err != nil {
				t.Fatalf("read version: %v", err)
			}
			legacyReqs, legacyRes := g.run(t, c, legacyReadPath, rv)
			currentReqs, currentRes := g.run(t, c, currentReadPath, rv)
			after := g.layout(t)
			same := legacyRes == currentRes && strings.Join(legacyReqs, "\n") == strings.Join(currentReqs, "\n")
			if !same && before != after && attempt < 5 {
				t.Logf("attempt %d: shard map moved during the pair; retrying", attempt)
				continue
			}
			if !same {
				t.Fatalf("transcripts differ\nlegacy result:  %s\ncurrent result: %s\nlegacy requests:\n%s\ncurrent requests:\n%s",
					legacyRes, currentRes, strings.Join(legacyReqs, "\n"), strings.Join(currentReqs, "\n"))
			}
			storage := 0
			for _, r := range legacyReqs {
				if !strings.HasPrefix(r, "GetKeyServerLocations") {
					storage++
				}
			}
			if storage < c.minStorage {
				t.Fatalf("%d storage requests, want >= %d:\n%s", storage, c.minStorage, strings.Join(legacyReqs, "\n"))
			}
			return
		}
	})
}

func (g *transcriptGate) run(t *testing.T, c transcriptCase, p readPath, rv int64) ([]string, string) {
	t.Helper()
	if c.warmLocations {
		g.locateAll(t)
	} else {
		g.db.db.locCache.invalidateRange([]byte{}, []byte{0xff, 0xff}, NoTenantID)
	}
	tx := g.db.CreateTransaction()
	defer tx.Cancel()
	tx.SetReadVersion(rv)
	if c.setup != nil {
		c.setup(tx)
	}
	var fired func() int
	if c.fault != nil {
		var disarm func()
		fired, disarm = c.fault()
		defer disarm()
	}
	g.rec.start()
	res, err := c.run(g.ctx, p, tx)
	reqs := g.rec.stop()
	if err != nil {
		t.Fatalf("%s: %v", p.name, err)
	}
	if fired != nil && fired() == 0 {
		t.Fatalf("%s: the fault never fired", p.name)
	}
	return reqs, res
}
