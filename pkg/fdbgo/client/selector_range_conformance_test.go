package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tcfdb "fdb.dev/pkg/testcontainers/foundationdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Selector range reads against libfdb_c 7.3.77 (RFC-262 D10). Both clients read
// the same static data at the same version, with the same pending writes and
// options, and must return the same rows and register the same read-conflict
// ranges. libfdb_c reports its ranges under \xff\xff/transaction/read_conflict_range/
// (ReadYourWrites.actor.cpp:2121-2160, the coalesced map in both RYW modes); the
// Go ranges are the transaction's, coalesced the same way.
//
// The cluster has small shards (as libfdbc's range_shard_pair test) and data
// distribution is frozen, so selectors anchored next to shard boundaries
// resolve across them: storage answers wrong_shard_server and the read takes
// getRangeFallback (NativeAPI.actor.cpp:4457-4500, 4864-4870;
// storageserver.actor.cpp:4602-4611, 4622-4630). The test counts those fallbacks on the Go
// side and requires some.
//
// The reads follow Apple's binding: GetRange reads batch 1 in ITERATOR mode, then
// GetSliceWithError reads EXACT with a limit and WANT_ALL without, continuing
// with FirstGreaterThan(last) /
// FirstGreaterOrEqual(last) and the other selector kept
// (bindings/go/src/fdb/range.go:135-153, 214-233); the iterator form reads in
// ITERATOR mode with the byte progression of fdb_c.cpp:1006-1029.

const selConfPrefix = "selconf/"

// selConfKey is data key i; only even indices are stored, so odd ones are
// selector anchors between keys.
func selConfKey(i int) string { return fmt.Sprintf("%sk%03d", selConfPrefix, i) }

const selConfKeys = 240

type selConfEnv struct {
	cdb        cgofdb.Database
	goDB       *Database
	boundaries []string // shard boundaries strictly inside the data
}

func selConfSetup(t *testing.T, ctx context.Context) *selConfEnv {
	t.Helper()
	startup, cancelStartup := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelStartup()
	container, err := tcfdb.Run(startup, "",
		tcfdb.WithStorageEngine("ssd"), tcfdb.WithDirectIP(),
		tcfdb.WithProcessCount(3), tcfdb.WithRedundancyMode("double"),
		tcfdb.WithKnob("min_shard_bytes", "10000"),
		tcfdb.WithKnob("max_shard_bytes", "50000"),
		tcfdb.WithKnob("shard_bytes_ratio", "2"),
		tcfdb.WithKnob("storage_metrics_polling_delay", "1"))
	if err != nil {
		t.Fatalf("cluster startup: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), tcfdb.TerminateTimeout)
		defer cancel()
		tcfdb.TerminateT(t, stop, container)
	})
	content, err := container.ClusterFile(startup)
	if err != nil {
		t.Fatal(err)
	}
	clusterFile := t.TempDir() + "/fdb.cluster"
	if err := os.WriteFile(clusterFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cgofdb.APIVersion(730); err != nil {
		t.Fatal(err)
	}
	cdb, err := cgofdb.OpenDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	goDB, err := OpenDatabase(ctx, clusterFile, WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { goDB.Close() })
	env := &selConfEnv{cdb: cdb, goDB: goDB}

	value := bytes.Repeat([]byte{'v'}, 6000)
	for batch := 0; batch < selConfKeys; batch += 20 {
		if _, err := cdb.Transact(func(tr cgofdb.Transaction) (any, error) {
			for i := batch; i < batch+20; i += 2 {
				tr.Set(cgofdb.Key(selConfKey(i)), append([]byte(selConfKey(i)), value...))
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for split := time.Now(); len(env.boundaries) < 6; {
		if time.Since(split) > 2*time.Minute {
			t.Fatalf("data split into only %d shards", len(env.boundaries)+1)
		}
		time.Sleep(time.Second)
		env.boundaries = selConfBoundaries(t, ctx, goDB)
	}
	if output, err := container.FDBCLIExec(ctx, "datadistribution off"); err != nil {
		t.Fatalf("freeze data distribution: %v, output=%s", err, output)
	}
	for stable, settle := 0, time.Now(); stable < 3; {
		if time.Since(settle) > 30*time.Second {
			t.Fatal("keyServers boundaries did not settle after data distribution off")
		}
		time.Sleep(time.Second)
		if b := selConfBoundaries(t, ctx, goDB); reflect.DeepEqual(b, env.boundaries) {
			stable++
		} else {
			env.boundaries, stable = b, 0
		}
	}
	return env
}

func selConfBoundaries(t *testing.T, ctx context.Context, db *Database) []string {
	t.Helper()
	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.SetReadSystemKeys()
	tx.SetLockAware()
	prefix := "\xff/keyServers/"
	rows, _, err := tx.Snapshot().GetRange(ctx, []byte(prefix+selConfKey(0)), []byte(prefix+selConfKey(selConfKeys)), 0)
	if err != nil {
		t.Fatalf("read keyServers: %v", err)
	}
	var out []string
	for _, row := range rows {
		if k := strings.TrimPrefix(string(row.Key), prefix); k > selConfKey(0) {
			out = append(out, k)
		}
	}
	return out
}

type selConfOp struct {
	kind     string // set, clear, clearRange, add
	key, end string
}

type selConfQuery struct {
	begin, end       KeySelector
	limit            int
	reverse          bool
	snapshot         bool
	rywDisabled      bool
	snapshotRYWOff   bool
	iterator         bool // ITERATOR mode drain instead of GetSliceWithError's
	ops              []selConfOp
	warmBegin, warmE string // a snapshot read of [warmBegin, warmE) first, when set
}

func (q selConfQuery) String() string {
	return fmt.Sprintf("[%s,%s) limit=%d reverse=%t snapshot=%t rywDisabled=%t snapshotRYWOff=%t iterator=%t ops=%v warm=[%q,%q)",
		selString(q.begin), selString(q.end), q.limit, q.reverse, q.snapshot, q.rywDisabled, q.snapshotRYWOff, q.iterator, q.ops, q.warmBegin, q.warmE)
}

// anchorIndex picks a key index, half the time next to a shard boundary.
func (env *selConfEnv) anchorIndex(rng *rand.Rand) int {
	if rng.Intn(2) == 0 {
		return rng.Intn(selConfKeys)
	}
	b := env.boundaries[rng.Intn(len(env.boundaries))]
	i := sort.Search(selConfKeys, func(i int) bool { return selConfKey(i) >= b })
	return max(0, min(selConfKeys-1, i+rng.Intn(7)-3))
}

func (env *selConfEnv) randomSelector(rng *rand.Rand, anchor int) KeySelector {
	forms := []struct {
		orEqual bool
		offset  int32
	}{{false, 1}, {true, 1}, {true, 0}, {false, 0}}
	f := forms[rng.Intn(len(forms))]
	return KeySelector{Key: []byte(selConfKey(anchor)), OrEqual: f.orEqual, Offset: f.offset + int32(rng.Intn(7)-3)}
}

func (env *selConfEnv) randomQuery(rng *rand.Rand) selConfQuery {
	a := env.anchorIndex(rng)
	b := a + rng.Intn(12) - 2
	if rng.Intn(4) == 0 {
		b = env.anchorIndex(rng)
	}
	b = max(0, min(selConfKeys-1, b))
	q := selConfQuery{
		begin:   env.randomSelector(rng, a),
		end:     env.randomSelector(rng, b),
		limit:   []int{0, 0, 1, 2, 3, 7}[rng.Intn(6)],
		reverse: rng.Intn(2) == 0,
	}
	switch rng.Intn(5) {
	case 0:
		q.rywDisabled = true
		q.snapshot = rng.Intn(2) == 0
	case 1:
		q.snapshot, q.snapshotRYWOff = true, true
	case 2:
		q.snapshot = true
	}
	q.iterator = rng.Intn(4) == 0
	lo, hi := min(a, b), max(a, b)
	near := func() int { return max(0, min(selConfKeys-1, lo+rng.Intn(hi-lo+5)-2)) }
	for n := rng.Intn(5); n > 0; n-- {
		k := near()
		switch rng.Intn(4) {
		case 0:
			q.ops = append(q.ops, selConfOp{kind: "set", key: selConfKey(k)})
		case 1:
			q.ops = append(q.ops, selConfOp{kind: "clear", key: selConfKey(k)})
		case 2:
			q.ops = append(q.ops, selConfOp{kind: "clearRange", key: selConfKey(k), end: selConfKey(k + 1 + rng.Intn(4))})
		case 3:
			q.ops = append(q.ops, selConfOp{kind: "add", key: selConfKey(k)})
		}
	}
	if rng.Intn(3) == 0 {
		w := near()
		q.warmBegin, q.warmE = selConfKey(w), selConfKey(w+1+rng.Intn(6))
	}
	return q
}

var addOperand = []byte{1, 0, 0, 0, 0, 0, 0, 0}

// selConfIterationBytes is fdb_c.cpp's iteration_progression (:1006-1007).
var selConfIterationBytes = []int{4096, 6144, 9216, 13824, 20736, 31104, 46656, 69984, 80000, 120000}

type selConfResult struct {
	rows      []KeyValue
	code      int // FDB error code, 0 for none
	conflicts [][2]string
}

func errorCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if code, ok := fdbErrorCode(err); ok {
		return code
	}
	var ce cgofdb.Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return -1
}

func (env *selConfEnv) runC(t *testing.T, q selConfQuery, version int64) selConfResult {
	t.Helper()
	tr, err := env.cdb.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cancel()
	for _, err := range []error{tr.Options().SetTimeout(20000), tr.Options().SetSpecialKeySpaceRelaxed()} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if q.rywDisabled {
		if err := tr.Options().SetReadYourWritesDisable(); err != nil {
			t.Fatal(err)
		}
	}
	if q.snapshotRYWOff {
		if err := tr.Options().SetSnapshotRywDisable(); err != nil {
			t.Fatal(err)
		}
	}
	tr.SetReadVersion(version)
	for _, op := range q.ops {
		switch op.kind {
		case "set":
			tr.Set(cgofdb.Key(op.key), []byte("w"))
		case "clear":
			tr.Clear(cgofdb.Key(op.key))
		case "clearRange":
			tr.ClearRange(cgofdb.KeyRange{Begin: cgofdb.Key(op.key), End: cgofdb.Key(op.end)})
		case "add":
			tr.Add(cgofdb.Key(op.key), addOperand)
		}
	}
	if q.warmBegin != "" {
		if _, err := tr.Snapshot().GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(q.warmBegin), End: cgofdb.Key(q.warmE)}, cgofdb.RangeOptions{}).GetSliceWithError(); err != nil {
			if code := errorCodeOf(err); code == ErrTransactionTooOld {
				return selConfResult{code: code}
			}
			t.Fatalf("C warm read: %v", err)
		}
	}
	sel := func(s KeySelector) cgofdb.KeySelector {
		return cgofdb.KeySelector{Key: cgofdb.Key(s.Key), OrEqual: s.OrEqual, Offset: int(s.Offset)}
	}
	r := cgofdb.SelectorRange{Begin: sel(q.begin), End: sel(q.end)}
	opts := cgofdb.RangeOptions{Limit: q.limit, Reverse: q.reverse}
	var reader cgofdb.ReadTransaction = tr
	if q.snapshot {
		reader = tr.Snapshot()
	}
	var res selConfResult
	var rows []cgofdb.KeyValue
	if q.iterator {
		opts.Mode = cgofdb.StreamingModeIterator
		it := reader.GetRange(r, opts).Iterator()
		for it.Advance() {
			kv, err := it.Get()
			if err != nil {
				res.code = errorCodeOf(err)
				break
			}
			rows = append(rows, kv)
		}
	} else {
		rows, err = reader.GetRange(r, opts).GetSliceWithError()
		res.code = errorCodeOf(err)
	}
	for _, kv := range rows {
		res.rows = append(res.rows, KeyValue{Key: bytes.Clone(kv.Key), Value: bytes.Clone(kv.Value)})
	}
	module := "\xff\xff/transaction/read_conflict_range/"
	kvs, err := tr.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(module), End: cgofdb.Key(module + "\xff\xff")}, cgofdb.RangeOptions{}).GetSliceWithError()
	if err != nil {
		if code := errorCodeOf(err); code == ErrTransactionTooOld {
			return selConfResult{code: code}
		}
		t.Fatalf("read %s: %v", module, err)
	}
	// The module lists the map's boundaries: "1" opens a conflict range, "0" closes it.
	for i, kv := range kvs {
		if string(kv.Value) != "1" {
			continue
		}
		end := "\xff\xff"
		if i+1 < len(kvs) {
			end = strings.TrimPrefix(string(kvs[i+1].Key), module)
		}
		res.conflicts = append(res.conflicts, [2]string{strings.TrimPrefix(string(kv.Key), module), end})
	}
	return res
}

func (env *selConfEnv) runGo(t *testing.T, ctx context.Context, q selConfQuery, version int64, fallbacks *atomic.Int64) selConfResult {
	t.Helper()
	tx := env.goDB.CreateTransaction()
	defer tx.Cancel()
	tx.SetTimeout(20000)
	tx.resolveKeyWorkerObserver = func(KeySelector) { fallbacks.Add(1) }
	if q.rywDisabled {
		tx.SetReadYourWritesDisable()
	}
	if q.snapshotRYWOff {
		tx.SetSnapshotRYWDisable()
	}
	tx.SetReadVersion(version)
	for _, op := range q.ops {
		switch op.kind {
		case "set":
			tx.Set([]byte(op.key), []byte("w"))
		case "clear":
			tx.Clear([]byte(op.key))
		case "clearRange":
			if err := tx.ClearRange([]byte(op.key), []byte(op.end)); err != nil {
				t.Fatal(err)
			}
		case "add":
			tx.Atomic(MutAddValue, []byte(op.key), addOperand)
		}
	}
	if q.warmBegin != "" {
		// Apple's GetSliceWithError of a KeyRange: batch 1 in ITERATOR mode, the rest
		// WANT_ALL, each a getRange of firstGreaterOrEqual selectors, which is the
		// walk C++ runs (ReadYourWrites.actor.cpp:1711-1770).
		b, e := firstGreaterOrEqual([]byte(q.warmBegin)), firstGreaterOrEqual([]byte(q.warmE))
		for byteTarget := 4096; ; byteTarget = 80000 {
			kvs, more, err := tx.Snapshot().GetRangeSelectors(ctx, b, e, 0, byteTarget, false)
			if err != nil {
				if code := errorCodeOf(err); code == ErrTransactionTooOld {
					return selConfResult{code: code}
				}
				t.Fatalf("Go warm read: %v", err)
			}
			if len(kvs) == 0 || !more {
				break
			}
			b = firstGreaterThan(bytes.Clone(kvs[len(kvs)-1].Key))
		}
	}
	read := tx.GetRangeSelectors
	if q.snapshot {
		read = tx.Snapshot().GetRangeSelectors
	}
	var res selConfResult
	begin, end, limit := q.begin, q.end, q.limit
	for iteration := 1; ; iteration++ {
		// Batch 1 is GetRange's own, in the caller's mode: ITERATOR, the default
		// (bindings/go/src/fdb/transaction.go:295-296). GetSliceWithError reads the
		// rest EXACT with a limit, else WANT_ALL, which is SERIAL (fdb_c.cpp:1009-1024).
		byteTarget := selConfIterationBytes[min(iteration, len(selConfIterationBytes))-1]
		if !q.iterator && iteration > 1 {
			byteTarget = 80000
			if q.limit != 0 {
				byteTarget = 0 // EXACT: no byte limit
			}
		}
		kvs, more, err := read(ctx, begin, end, limit, byteTarget, q.reverse)
		if err != nil {
			res.code = errorCodeOf(err)
			break
		}
		if len(kvs) == 0 { // Apple's Advance ends on an empty batch (range.go:194-210)
			break
		}
		res.rows = append(res.rows, kvs...)
		if !more || len(kvs) == limit { // fetchNextBatch (range.go:214-217)
			break
		}
		if limit > 0 {
			limit -= len(kvs)
		}
		last := bytes.Clone(kvs[len(kvs)-1].Key)
		if q.reverse {
			end = firstGreaterOrEqual(last)
		} else {
			begin = firstGreaterThan(last)
		}
	}
	// The C module coalesces the native list too when RYW is disabled.
	for _, r := range readConflictMap(tx.readConflictRanges()) {
		res.conflicts = append(res.conflicts, [2]string{string(r.Begin), string(r.End)})
	}
	return res
}

func TestSelectorRangeConformance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	env := selConfSetup(t, ctx)
	t.Logf("shard boundaries inside the data: %d", len(env.boundaries))

	rng := rand.New(rand.NewSource(26210))
	var fallbacks atomic.Int64
	boundedOvershoots := []selConfQuery{
		{begin: KeySelector{Key: []byte(selConfKey(selConfKeys - 2)), Offset: 3}, end: firstGreaterOrEqual([]byte(selConfPrefix + "z"))},
		{begin: KeySelector{Key: []byte(selConfKey(2)), Offset: 0}, end: KeySelector{Key: []byte(selConfKey(4)), Offset: -4}, reverse: true},
	}
	queries := 900 + len(boundedOvershoots)
	var nonEmpty, withConflicts, withOps, crossShard, rywDisabled, snapshots, tooOldRetries int
	shardOf := func(k []byte) int {
		return sort.Search(len(env.boundaries), func(i int) bool { return env.boundaries[i] > string(k) })
	}
	for i := 0; i < queries; i++ {
		var q selConfQuery
		if i < len(boundedOvershoots) {
			q = boundedOvershoots[i]
		} else {
			q = env.randomQuery(rng)
		}
		var c, g selConfResult
		for attempt := 1; ; attempt++ {
			ctr, err := env.cdb.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			version, err := ctr.GetReadVersion().Get()
			ctr.Cancel()
			if err != nil {
				t.Fatalf("read version: %v", err)
			}
			c = env.runC(t, q, version)
			g = env.runGo(t, ctx, q, version, &fallbacks)
			// transaction_too_old on either side is the clock (a stall past the MVCC window), not a
			// client difference: rerun both at a new read version.
			if c.code != ErrTransactionTooOld && g.code != ErrTransactionTooOld {
				break
			}
			if attempt == 5 {
				t.Fatalf("query %d %s: transaction_too_old on %d read versions (C %d, Go %d)", i, q, attempt, c.code, g.code)
			}
			tooOldRetries++
		}
		if c.code != g.code {
			t.Fatalf("query %d %s: error code C %d, Go %d", i, q, c.code, g.code)
		}
		if keysOf(c.rows) != keysOf(g.rows) {
			t.Fatalf("query %d %s:\n  C rows  %s\n  Go rows %s", i, q, keysOf(c.rows), keysOf(g.rows))
		}
		for j := range c.rows {
			if !bytes.Equal(c.rows[j].Value, g.rows[j].Value) {
				t.Fatalf("query %d %s: value of %q differs", i, q, c.rows[j].Key)
			}
		}
		if !slices.Equal(c.conflicts, g.conflicts) {
			t.Fatalf("query %d %s: rows %s\n  C read conflicts  %q\n  Go read conflicts %q", i, q, keysOf(c.rows), c.conflicts, g.conflicts)
		}
		if i < len(boundedOvershoots) {
			want := [][2]string{{string(q.begin.Key), string(q.end.Key)}}
			if len(c.rows) != 0 || !slices.Equal(c.conflicts, want) {
				t.Fatalf("bounded overshoot %d: C rows %s conflicts %q, want empty rows and %q", i, keysOf(c.rows), c.conflicts, want)
			}
			t.Logf("bounded overshoot reverse=%t: C/Go empty rows and exact conflicts %q", q.reverse, want)
		}
		if len(c.rows) > 0 {
			nonEmpty++
			if shardOf(c.rows[0].Key) != shardOf(c.rows[len(c.rows)-1].Key) {
				crossShard++
			}
		}
		if len(c.conflicts) > 0 {
			withConflicts++
		}
		if len(q.ops) > 0 {
			withOps++
		}
		if q.rywDisabled {
			rywDisabled++
		}
		if q.snapshot {
			snapshots++
		}
	}
	t.Logf("queries=%d nonEmpty=%d withConflicts=%d withOps=%d crossShard=%d rywDisabled=%d snapshot=%d goFallbackResolutions=%d tooOldRetries=%d",
		queries, nonEmpty, withConflicts, withOps, crossShard, rywDisabled, snapshots, fallbacks.Load(), tooOldRetries)
	// The comparisons must have had something to compare.
	if nonEmpty < queries/3 || withConflicts < queries/5 || withOps < queries/3 || crossShard == 0 || rywDisabled < queries/10 || snapshots < queries/5 {
		t.Fatal("population too thin; see the counts above")
	}
	if fallbacks.Load() == 0 {
		t.Fatal("no selector resolution crossed a shard: getRangeFallback was never exercised")
	}
}
