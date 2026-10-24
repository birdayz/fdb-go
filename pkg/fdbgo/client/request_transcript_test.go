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
// current code, and requires:
//
//   - the same requests in the same order, of every kind a read may send
//     (storage reads, location lookups, read versions; sendRecorder), compared
//     field by field without the reply token, span and debug IDs, with
//     versions numbered in order of appearance and body lengths included. This
//     is request-field equality, not literal byte identity;
//   - no frame the recorder could not decode or did not expect, and no partial
//     frame;
//   - the same full result: every row's key and value, more, readThrough, a
//     getKey's key and fallback flag, and every mapped row with its secondary.
//
// Each case runs with the read version preset (both arms at one version) and
// with a fresh transaction, where the arm's own GetReadVersion request is part
// of the compared transcript. Equivalence is claimed only within the C int
// domain, where every limit the cases use lies. newRangeLimits clamps a Go row
// or byte limit above MaxInt32; that leaves the first request unchanged (its
// wire limits are at most REPLY_BYTE_LIMIT), but a scan whose total passes
// MaxInt32 rows or bytes now stops where the old Go-int budget went on, so
// larger totals are excluded.
func TestRequestTranscriptGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  shardSizeConfig
	}{{"multishard", defaultShardSize}, {"multishard-larger", largerShardSize}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
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
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		connectCF, _ := startProxyFDBContainer(t, ctx)
		sd := newSimDialer()
		g := newTranscriptGate(t, ctx, connectCF, sd.dial)
		prefix := []byte("tg_")
		g.seed(func(tx *Transaction) {
			for i := 0; i < 100; i++ {
				tx.Set([]byte(fmt.Sprintf("%s%04d", prefix, i)), bytes.Repeat([]byte{byte('a' + i%26)}, 1000))
			}
		})
		f := mappedFixture{prefix: []byte("tg_mapped"), n: 20}
		g.seed(func(tx *Transaction) {
			for i := 0; i < f.n; i++ {
				tx.Set(f.indexEntryKey(i), []byte{})
				tx.Set(f.recordKey(i), []byte(fmt.Sprintf("record-%08d", i)))
			}
		})
		cases := transcriptCases(prefix, f, 1)
		g.runAll(cases)

		// One process serves storage, proxies and coordination here, so a fault
		// targets a reply by its token: only the reply to a storage request the
		// arm sent is replaced or dropped, once.
		sd.armAddr(storageAddrFor(t, g.db, ctx, prefix))
		for _, c := range cases {
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
				fc.fault = func() (injected func() int, disarm func()) {
					var n atomic.Int32
					sd.setIntercept(func(_ int, token transport.UID, body []byte) ([]byte, bool) {
						if g.rec.isStorageReply(token) && n.CompareAndSwap(0, 1) {
							return fault.reply(body)
						}
						return body, false
					})
					return func() int { return int(n.Load()) }, func() { sd.setIntercept(nil) }
				}
				g.pair(fc)
			}
		}
		g.negativeControls(prefix)
	})
}

// readPath is one implementation of the gated entry points. Each returns the
// full rendering of its result.
type readPath struct {
	name          string
	rangeRead     func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error)
	rywOffRange   func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error)
	getKey        func(ctx context.Context, tx *Transaction, key []byte, orEqual bool, offset int32) (string, error)
	mappedScan    func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limit, byteTarget int, reverse bool) (string, error)
	mappedSelect  func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error)
	mappedExactly func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error)
	mappedSkip    func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limit, byteTarget int, skip int32, reverse bool) (string, error)
}

func renderKVs(kvs []KeyValue, more bool, readThrough []byte) string {
	var b strings.Builder
	for i := range kvs {
		fmt.Fprintf(&b, "%q=%q\n", kvs[i].Key, kvs[i].Value)
	}
	fmt.Fprintf(&b, "rows=%d more=%t readThrough=%q", len(kvs), more, readThrough)
	return b.String()
}

func renderMapped(rows []MappedKeyValue, more bool) string {
	var b strings.Builder
	for i := range rows {
		fmt.Fprintf(&b, "%+v\n", rows[i])
	}
	fmt.Fprintf(&b, "rows=%d more=%t", len(rows), more)
	return b.String()
}

var legacyReadPath = readPath{
	name: "legacy",
	rangeRead: func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error) {
		kvs, more, rt, err := tx.legacyGetRangeReadThrough(ctx, b, e, limit, byteTarget, reverse)
		return renderKVs(kvs, more, rt), err
	},
	// The RYW-disabled route calls getRangeReadThrough directly (range_issue.go:181).
	rywOffRange: func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error) {
		kvs, more, _, err := tx.legacyGetRangeReadThrough(ctx, b, e, limit, byteTarget, reverse)
		return renderKVs(kvs, more, nil), err
	},
	getKey: func(ctx context.Context, tx *Transaction, key []byte, orEqual bool, offset int32) (string, error) {
		k, fallback, err := tx.legacyGetKeyForRange(ctx, key, orEqual, offset)
		return fmt.Sprintf("key=%q fallback=%t", k, fallback), err
	},
	mappedScan: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limit, byteTarget int, reverse bool) (string, error) {
		if err := tx.ensureReadVersion(ctx); err != nil {
			return "", err
		}
		rows, more, err := tx.legacyGetMappedRangeImpl(ctx, b, e, mapper, limit, byteTarget, reverse)
		return renderMapped(rows, more), err
	},
	mappedSelect: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error) {
		if err := tx.ensureReadVersion(ctx); err != nil {
			return "", err
		}
		res, err := tx.legacyGetMappedRangeSelectors(ctx, mappedSelectorFGE(b), mappedSelectorFGE(e), mapper, limits, reverse)
		return renderMapped(res.rows, res.more), err
	},
	mappedExactly: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error) {
		if err := tx.ensureReadVersion(ctx); err != nil {
			return "", err
		}
		res, err := tx.legacyGetMappedRangeSelectorExact(ctx, b, e, mapper, limits, reverse)
		return renderMapped(res.rows, res.more), err
	},
	mappedSkip: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limit, byteTarget int, skip int32, reverse bool) (string, error) {
		rows, more, err := tx.legacyGetMappedRangeWithByteTargetAndSkip(ctx, b, e, mapper, limit, byteTarget, skip, reverse)
		return renderMapped(rows, more), err
	},
}

var currentReadPath = readPath{
	name: "current",
	rangeRead: func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error) {
		kvs, more, rt, err := tx.getRangeReadThrough(ctx, b, e, limit, byteTarget, reverse)
		return renderKVs(kvs, more, rt), err
	},
	rywOffRange: func(ctx context.Context, tx *Transaction, b, e []byte, limit, byteTarget int, reverse bool) (string, error) {
		tx.SetReadYourWritesDisable()
		kvs, more, err := tx.GetRangeWithByteTarget(ctx, b, e, limit, byteTarget, reverse)
		return renderKVs(kvs, more, nil), err
	},
	getKey: func(ctx context.Context, tx *Transaction, key []byte, orEqual bool, offset int32) (string, error) {
		k, fallback, err := tx.getKeyForRange(ctx, key, orEqual, offset)
		return fmt.Sprintf("key=%q fallback=%t", k, fallback), err
	},
	mappedScan: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limit, byteTarget int, reverse bool) (string, error) {
		rv, err := tx.operationReadVersion(ctx)
		if err != nil {
			return "", err
		}
		rows, more, err := tx.getMappedRangeImpl(ctx, b, e, mapper, limit, byteTarget, rv, reverse)
		return renderMapped(rows, more), err
	},
	mappedSelect: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error) {
		rv, err := tx.operationReadVersion(ctx)
		if err != nil {
			return "", err
		}
		res, err := tx.getMappedRangeSelectors(ctx, mappedSelectorFGE(b), mappedSelectorFGE(e), mapper, limits, rv, reverse)
		return renderMapped(res.rows, res.more), err
	},
	mappedExactly: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limits mappedRangeLimits, reverse bool) (string, error) {
		rv, err := tx.operationReadVersion(ctx)
		if err != nil {
			return "", err
		}
		res, err := tx.getMappedRangeSelectorExact(ctx, b, e, mapper, limits, rv, reverse)
		return renderMapped(res.rows, res.more), err
	},
	mappedSkip: func(ctx context.Context, tx *Transaction, b, e, mapper []byte, limit, byteTarget int, skip int32, reverse bool) (string, error) {
		rows, more, err := tx.GetMappedRangeWithByteTargetAndSkip(ctx, b, e, mapper, limit, byteTarget, skip, reverse)
		return renderMapped(rows, more), err
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
	// fault arms a fault for one run and reports how many storage replies it
	// changed; each faulted arm must change exactly one.
	fault func() (injected func() int, disarm func())
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
			// The public mapped entry: Skip 0 takes the exact scan, a positive Skip
			// resolves its offset with native getKeys and then reads through the
			// selector loop or, after a partial resolution, the exact fallback
			// (mappedrange.go:323, mappedrange_skip.go:15). A Skip past the data
			// resolves past the range and reads nothing.
			for _, skip := range []int32{0, 1, 7, 1000} {
				minSkipStorage := 1
				if skip == 0 && lim.limit == 0 {
					minSkipStorage = crossShards
				}
				cases = append(cases, transcriptCase{
					name: fmt.Sprintf("mapped-skip/reverse=%t/limit=%d/skip=%d", reverse, lim.limit, skip),
					run: func(ctx context.Context, p readPath, tx *Transaction) (string, error) {
						return p.mappedSkip(ctx, tx, mb, me, mapper, lim.limit, lim.bytes, skip, reverse)
					},
					minStorage: minSkipStorage,
					faultable:  lim.limit == 0 && skip == 7,
				})
			}
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
	// Never sample read tags: the legacy arm cannot send them, so a sampled
	// request would differ from it by chance.
	db, err := OpenDatabaseFromConfig(ctx, cf, WithDialFunc(rec.wrap(dial)), WithAPIVersion(730), withReadTagSampling(0, nil))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
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
	all := []byte{0xff, 0xff}
	g.db.db.locCache.invalidateRange([]byte{}, all, NoTenantID)
	locs, err := g.db.db.locCache.locateRange(g.db.db, g.ctx, []byte{}, []byte{0xff}, 10000, false, NoTenantID, types.SpanContext{})
	g.db.db.locCache.invalidateRange([]byte{}, all, NoTenantID)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
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

// armResult is one run of a case.
type armResult struct {
	path     string
	rec      recording
	result   string
	err      error
	injected int
}

// pair runs c through both paths, with a preset read version and with a fresh
// one. Data distribution can move a shard between the two runs; a mismatch
// counts only when the shard map was the same before and after.
func (g *transcriptGate) pair(c transcriptCase) {
	for _, fresh := range []bool{false, true} {
		name := c.name + "/preset-version"
		if fresh {
			name = c.name + "/fresh-version"
		}
		g.t.Run(name, func(t *testing.T) {
			for attempt := 1; ; attempt++ {
				before := g.layout(t)
				rv := int64(0)
				if !fresh {
					rv = g.readVersion(t)
				}
				legacy := g.run(c, legacyReadPath, rv, nil)
				current := g.run(c, currentReadPath, rv, nil)
				err := compareArms(c, legacy, current)
				if err != nil && attempt < 5 && g.layout(t) != before {
					t.Logf("attempt %d: shard map moved during the pair; retrying", attempt)
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				return
			}
		})
	}
}

func (g *transcriptGate) readVersion(t *testing.T) int64 {
	t.Helper()
	tx := g.db.CreateTransaction()
	defer tx.Cancel()
	rv, err := tx.GetReadVersion(g.ctx)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	return rv
}

// run executes c through p with a cold location cache, at rv when rv > 0 and
// otherwise at the read version the transaction acquires itself. extra, when
// set, runs after the read inside the recorded window (negative controls).
func (g *transcriptGate) run(c transcriptCase, p readPath, rv int64, extra func(tx *Transaction)) armResult {
	g.db.db.locCache.invalidateRange([]byte{}, []byte{0xff, 0xff}, NoTenantID)
	tx := g.db.CreateTransaction()
	defer tx.Cancel()
	if rv > 0 {
		tx.SetReadVersion(rv)
	}
	if c.setup != nil {
		c.setup(tx)
	}
	var injected func() int
	if c.fault != nil {
		var disarm func()
		injected, disarm = c.fault()
		defer disarm()
	}
	g.rec.start()
	res, err := c.run(g.ctx, p, tx)
	if extra != nil {
		extra(tx)
	}
	arm := armResult{path: p.name, rec: g.rec.stop(), result: res, err: err, injected: -1}
	if injected != nil {
		arm.injected = injected()
	}
	return arm
}

// compareArms is the gate's verdict on one pair.
func compareArms(c transcriptCase, legacy, current armResult) error {
	for _, a := range []armResult{legacy, current} {
		if len(a.rec.problems) > 0 {
			return fmt.Errorf("%s: recorder problems (fails closed):\n%s", a.path, strings.Join(a.rec.problems, "\n"))
		}
		if a.err != nil {
			return fmt.Errorf("%s: %v", a.path, a.err)
		}
		if a.injected != -1 && a.injected != 1 {
			return fmt.Errorf("%s: the fault changed %d storage replies, want exactly 1", a.path, a.injected)
		}
	}
	if legacy.result != current.result {
		return fmt.Errorf("results differ at %s", firstDifference(legacy.result, current.result))
	}
	lr, cr := strings.Join(legacy.rec.requests, "\n"), strings.Join(current.rec.requests, "\n")
	if lr != cr {
		return fmt.Errorf("requests differ\nlegacy:\n%s\ncurrent:\n%s", lr, cr)
	}
	storage := 0
	for _, r := range legacy.rec.requests {
		if !strings.HasPrefix(r, "GetKeyServerLocationsRequest") && !strings.HasPrefix(r, "GetReadVersionRequest") {
			storage++
		}
	}
	if storage < c.minStorage {
		return fmt.Errorf("%d storage requests, want >= %d:\n%s", storage, c.minStorage, lr)
	}
	return nil
}

func firstDifference(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	clip := func(s string) string {
		lo, hi := max(0, i-80), min(len(s), i+80)
		return fmt.Sprintf("%q", s[lo:hi])
	}
	return fmt.Sprintf("byte %d (lengths %d, %d)\nlegacy:  %s\ncurrent: %s", i, len(a), len(b), clip(a), clip(b))
}

// negativeControls run pairs the gate must reject, each through compareArms:
// an extra GetValue, an extra GetReadVersion, a frame the recorder cannot
// decode, a known request kind with a malformed field, a frame of an
// unexpected kind, a malformed length, and a partial frame left at the end of
// the window. The frame controls feed a socketless parser registered for the
// one window (sendRecorder.parser): such bytes on a real socket would end the
// connection, and no real write can interleave with them.
func (g *transcriptGate) negativeControls(prefix []byte) {
	pb, pe := prefix, append(append([]byte{}, prefix...), 0xff)
	c := transcriptCase{
		run: func(ctx context.Context, p readPath, tx *Transaction) (string, error) {
			return p.rangeRead(ctx, tx, pb, pe, 5, ByteLimitUnlimited, false)
		},
		minStorage: 1,
	}
	commit := (&types.CommitTransactionRequest{}).MarshalFDB()
	for _, nc := range []struct {
		name  string
		extra func(tx *Transaction, parser *recordingConn)
		want  string
	}{
		{"extra-getvalue", func(tx *Transaction, _ *recordingConn) { _, _ = tx.getValue(g.ctx, pb) }, "requests differ"},
		{"extra-read-version", func(*Transaction, *recordingConn) {
			tx := g.db.CreateTransaction()
			defer tx.Cancel()
			_, _ = tx.GetReadVersion(g.ctx)
		}, "requests differ"},
		{"undecodable-frame", func(_ *Transaction, rc *recordingConn) {
			rc.observe(recorderFrame(transport.UID{First: 7}, []byte{1, 2, 3}))
		}, "undecodable frame"},
		{"malformed-field", func(_ *Transaction, rc *recordingConn) {
			rc.observe(recorderFrame(transport.UID{First: 7}, malformedGetKeyValues()))
		}, "decode GetKeyValuesRequest.Begin"},
		{"noncanonical-body", func(_ *Transaction, rc *recordingConn) {
			rc.observe(recorderFrame(transport.UID{First: 7}, nonCanonicalGetKeyValues()))
		}, "does not re-encode"},
		{"unexpected-kind", func(_ *Transaction, rc *recordingConn) {
			rc.observe(recorderFrame(transport.UID{First: 7}, commit))
		}, "unexpected request kind"},
		{"malformed-length", func(_ *Transaction, rc *recordingConn) {
			rc.observe([]byte{4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		}, "malformed frame"},
		{"partial-frame", func(_ *Transaction, rc *recordingConn) {
			rc.observe(recorderFrame(transport.UID{First: 7}, commit)[:20])
		}, "partial frame"},
	} {
		g.t.Run("negative-control/"+nc.name, func(t *testing.T) {
			for _, fresh := range []bool{false, true} {
				rv := int64(0)
				if !fresh {
					rv = g.readVersion(t)
				}
				legacy := g.run(c, legacyReadPath, rv, nil)
				parser, remove := g.rec.parser()
				current := g.run(c, currentReadPath, rv, func(tx *Transaction) { nc.extra(tx, parser) })
				remove()
				if err := compareArms(c, legacy, legacy); err != nil {
					t.Fatalf("control arm alone fails: %v", err)
				}
				err := compareArms(c, legacy, current)
				if err == nil || !strings.Contains(err.Error(), nc.want) {
					t.Fatalf("fresh=%t: gate verdict %v, want an error containing %q", fresh, err, nc.want)
				}
			}
		})
	}
}
