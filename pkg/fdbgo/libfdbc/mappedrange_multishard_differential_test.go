//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	fdbclient "fdb.dev/pkg/fdbgo/client"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

type mdShardEnv struct {
	fixture       mdFixture
	cdb           *libfdbc.CDatabase
	backend       gofdb.BackendDatabase
	goDB          *fdbclient.Database
	facade        gofdb.Database
	metadata      gofdb.Database
	boundaries    [][]byte
	allBoundaries [][]byte
	indices       []int
	middle        int
}

func mdShardReadBoundaries(t *testing.T, backend gofdb.BackendDatabase, f mdFixture, version int64) [][]byte {
	t.Helper()
	begin, end := f.indexRange()
	tr, err := backend.CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cancel()
	for _, err := range []error{tr.Options().SetTimeout(10000), tr.Options().SetReadSystemKeys(), tr.Options().SetLockAware()} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if version != 0 {
		tr.SetReadVersion(version)
	}
	prefix := []byte("\xff/keyServers/")
	rows, err := tr.Snapshot().GetRange(gofdb.KeyRange{
		Begin: gofdb.Key(append(bytes.Clone(prefix), begin...)),
		End:   gofdb.Key(append(bytes.Clone(prefix), end...)),
	}, gofdb.RangeOptions{Mode: gofdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		t.Fatalf("read physical keyServers boundaries: %v", err)
	}
	var boundaries [][]byte
	for _, row := range rows {
		if !bytes.HasPrefix(row.Key, prefix) {
			t.Fatalf("keyServers reply escaped its prefix: %x", row.Key)
		}
		key := row.Key[len(prefix):]
		if bytes.Compare(key, begin) <= 0 || bytes.Compare(key, end) >= 0 {
			continue
		}
		if len(boundaries) > 0 && bytes.Compare(boundaries[len(boundaries)-1], key) >= 0 {
			t.Fatalf("keyServers boundaries are not strictly ordered: %x", rows)
		}
		boundaries = append(boundaries, bytes.Clone(key))
	}
	return boundaries
}

func mdShardQualified(f mdFixture, boundaries [][]byte) [][]byte {
	var qualified [][]byte
	for _, boundary := range boundaries {
		i := sort.Search(f.n, func(i int) bool { return bytes.Compare(f.indexEntryKey(i), boundary) >= 0 })
		if i > 0 && i < f.n {
			qualified = append(qualified, boundary)
		}
	}
	return qualified
}

// Select a genuinely populated middle boundary, with at least two primary
// keys between adjacent boundaries. Knob settings alone never establish this.
func mdShardSelect(f mdFixture, boundaries [][]byte) (int, []int) {
	if len(boundaries) < 3 {
		return -1, nil
	}
	indices := make([]int, len(boundaries))
	for j, boundary := range boundaries {
		i := sort.Search(f.n, func(i int) bool { return bytes.Compare(f.indexEntryKey(i), boundary) >= 0 })
		if i == 0 || i == f.n {
			return -1, nil
		}
		indices[j] = i
	}
	best, bestDistance := -1, len(boundaries)
	for j := 1; j < len(boundaries)-1; j++ {
		if indices[j] < 8 || f.n-indices[j]-1 < 7 || indices[j]-indices[j-1] < 2 || indices[j+1]-indices[j] < 2 {
			continue
		}
		distance := j - len(boundaries)/2
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance {
			best, bestDistance = j, distance
		}
	}
	return best, indices
}

func TestMappedSkipShardBoundaryPopulationGuard(t *testing.T) {
	t.Parallel()
	f := mdFixture{prefix: []byte(t.Name()), n: 100}
	for _, tc := range []struct {
		name    string
		indices []int
		want    int
	}{
		{"empty", nil, -1},
		{"two", []int{20, 80}, -1},
		{"no_left_population", []int{0, 50, 80}, -1},
		{"no_right_population", []int{20, 50, 100}, -1},
		{"too_close", []int{20, 21, 22}, -1},
		{"too_few_left", []int{1, 3, 5}, -1},
		{"too_few_right", []int{94, 96, 98}, -1},
		{"middle", []int{20, 50, 80}, 1},
		{"nearest_middle", []int{10, 20, 40, 60, 80, 90}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var boundaries [][]byte
			for _, i := range tc.indices {
				boundaries = append(boundaries, f.indexEntryKey(i))
			}
			var wantQualified [][]byte
			for _, i := range tc.indices {
				if i > 0 && i < f.n {
					wantQualified = append(wantQualified, f.indexEntryKey(i))
				}
			}
			if got := mdShardQualified(f, boundaries); !reflect.DeepEqual(got, wantQualified) {
				t.Fatalf("qualified boundaries=%x want=%x", got, wantQualified)
			}
			if got, _ := mdShardSelect(f, boundaries); got != tc.want {
				t.Fatalf("population guard middle=%d want=%d", got, tc.want)
			}
		})
	}
}

func mdShardPause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func mdShardSetup(t *testing.T, ctx context.Context) mdShardEnv {
	t.Helper()
	startup, cancelStartup := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelStartup()
	container, err := foundationdbtc.Run(startup, "",
		foundationdbtc.WithStorageEngine("ssd"), foundationdbtc.WithDirectIP(),
		foundationdbtc.WithProcessCount(3), foundationdbtc.WithRedundancyMode("double"),
		foundationdbtc.WithKnob("min_shard_bytes", "10000"),
		foundationdbtc.WithKnob("max_shard_bytes", "50000"),
		foundationdbtc.WithKnob("shard_bytes_ratio", "2"),
		foundationdbtc.WithKnob("storage_metrics_polling_delay", "1"))
	if err != nil {
		t.Fatalf("three-process cross-shard cluster startup: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		foundationdbtc.TerminateT(t, stop, container)
	})
	content, err := container.ClusterFile(startup)
	if err != nil {
		t.Fatal(err)
	}
	clusterFile := t.TempDir() + "/fdb.cluster"
	if err := os.WriteFile(clusterFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cdb.Close)
	backend, err := libfdbc.Open(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	goDB, err := fdbclient.OpenDatabase(ctx, clusterFile, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { goDB.Close() })
	primeMappedSnapshotClient(t, ctx, goDB)
	// Current keyServers checks must not ratchet the query client's minimum
	// acceptable version past an active, explicitly pinned reader.
	metadataDB, err := fdbclient.OpenDatabase(ctx, clusterFile, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { metadataDB.Close() })
	env := mdShardEnv{fixture: mdFixture{prefix: []byte(t.Name()), n: 100}, cdb: cdb, backend: backend, goDB: goDB, facade: gofdb.WrapDatabase(goDB), metadata: gofdb.WrapDatabase(metadataDB)}
	// The INDEX, not just its secondary records, holds 1MB. The key-only mapper
	// does not parse these non-tuple primary values (storage server :4911-4940).
	for batch := 0; batch < 10; batch++ {
		tr, err := mdCreateTransaction(t, cdb)
		if err != nil {
			t.Fatal(err)
		}
		for i := batch * 10; i < (batch+1)*10; i++ {
			tr.Set(env.fixture.indexEntryKey(i), bytes.Repeat([]byte{byte('A' + i%26)}, 10000))
			for s := 0; s < 3; s++ {
				tr.Set(env.fixture.splitRecordKey(i, s), bytes.Repeat([]byte{byte('a' + (i+s)%26)}, 400))
			}
		}
		if err := tr.Commit(); err != nil {
			tr.Close()
			t.Fatalf("seed primary shard population, batch %d: %v", batch, err)
		}
		tr.Close()
	}
	splitCtx, cancelSplit := context.WithTimeout(ctx, 60*time.Second)
	defer cancelSplit()
	for {
		allBoundaries := mdShardReadBoundaries(t, backend, env.fixture, 0)
		boundaries := mdShardQualified(env.fixture, allBoundaries)
		middle, indices := mdShardSelect(env.fixture, boundaries)
		if middle >= 0 {
			env.allBoundaries, env.boundaries, env.middle, env.indices = allBoundaries, boundaries, middle, indices
			break
		}
		if err := mdShardPause(splitCtx, time.Second); err != nil {
			t.Fatalf("no populated three-boundary primary range after split wait: boundaries=%x: %v", boundaries, err)
		}
	}
	output, err := container.FDBCLIExec(ctx, "datadistribution off")
	if err != nil {
		t.Fatalf("freeze data distribution: %v, output=%s", err, output)
	}
	stableCtx, cancelStable := context.WithTimeout(ctx, 20*time.Second)
	defer cancelStable()
	stable := 0
	for stable < 3 {
		boundaries := mdShardReadBoundaries(t, backend, env.fixture, 0)
		if reflect.DeepEqual(boundaries, env.allBoundaries) {
			stable++
		} else {
			env.allBoundaries = boundaries
			stable = 0
		}
		if stable < 3 {
			if err := mdShardPause(stableCtx, time.Second); err != nil {
				t.Fatalf("keyServers boundaries did not stabilize after DD off: %x: %v", env.allBoundaries, err)
			}
		}
	}
	env.boundaries = mdShardQualified(env.fixture, env.allBoundaries)
	env.middle, env.indices = mdShardSelect(env.fixture, env.boundaries)
	if env.middle < 0 {
		t.Fatalf("frozen keyServers vector lost populated cross-shard precondition: %x", env.boundaries)
	}
	for j, boundary := range env.boundaries {
		i := env.indices[j]
		if bytes.Compare(env.fixture.indexEntryKey(i-1), boundary) >= 0 || bytes.Compare(env.fixture.indexEntryKey(i), boundary) < 0 {
			t.Fatalf("boundary %x lacks seeded primary keys on both sides", boundary)
		}
	}
	t.Logf("seeded %d fat primary keys; observed %d strict interior physical boundaries; middle=%x index=%d; DD disabled", env.fixture.n, len(env.boundaries), env.boundaries[env.middle], env.indices[env.middle])
	return env
}

func mdShardAssertFrozen(t *testing.T, env mdShardEnv) {
	t.Helper()
	cKeys := mdShardReadBoundaries(t, env.backend, env.fixture, 0)
	begin, end := env.fixture.indexRange()
	gKeys, err := env.metadata.LocalityGetBoundaryKeys(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var goKeys [][]byte
	for _, key := range gKeys {
		if bytes.Compare(key, begin) > 0 && bytes.Compare(key, end) < 0 {
			goKeys = append(goKeys, bytes.Clone(key))
		}
	}
	if !reflect.DeepEqual(cKeys, env.allBoundaries) || !reflect.DeepEqual(goKeys, env.allBoundaries) {
		t.Fatalf("physical primary boundaries drifted: frozen=%x C=%x Go=%x", env.allBoundaries, cKeys, goKeys)
	}
}

// The raw C mapped API may resolve an offset beyond its client-clipped shard
// end. Resolve through ordinary reads and use bounded, plain mapped requests.
func mdShardPrimaryRows(t *testing.T, tr libfdbc.CTxnHandle, f mdFixture, begin, end []byte, snapshot bool) []libfdbc.CKeyValue {
	t.Helper()
	var rows []libfdbc.CKeyValue
	for iteration := 1; iteration <= f.n+1; iteration++ {
		batch, more, err := libfdbc.CGetRangeBatch(tr, begin, end, 0, 80000, libfdbc.CStreamingModeWantAll, iteration, false, snapshot)
		if err != nil {
			t.Fatalf("ordinary C primary batch %d: %v", iteration, err)
		}
		rows = append(rows, batch...)
		if len(rows) > f.n {
			t.Fatal("ordinary C primary scan repeated rows")
		}
		if !more {
			return rows
		}
		if len(batch) == 0 {
			t.Fatal("ordinary C primary scan made no progress")
		}
		begin = append(bytes.Clone(batch[len(batch)-1].Key), 0)
	}
	t.Fatal("ordinary C primary scan did not terminate")
	return nil
}

func mdShardDrainC(t *testing.T, tr libfdbc.CTxnHandle, f mdFixture, limit, mode int, skip int32, reverse bool) ([]libfdbc.CMappedRow, []mdBatch) {
	t.Helper()
	begin, end := f.indexRange()
	primary := mdShardPrimaryRows(t, tr, f, begin, end, true)
	if len(primary) != f.n || skip <= 0 || int(skip) >= len(primary) {
		t.Fatalf("cross-shard C resolution population=%d fixture=%d skip=%d", len(primary), f.n, skip)
	}
	for i, row := range primary {
		if !bytes.Equal(row.Key, f.indexEntryKey(i)) {
			t.Fatalf("ordinary C primary %d differs from seeded key", i)
		}
	}
	skippedBegin, skippedEnd := begin, primary[int(skip)].Key
	if reverse {
		skippedBegin, skippedEnd = primary[len(primary)-int(skip)].Key, end
		end = skippedBegin
	} else {
		begin = skippedEnd
	}
	// Ordinary non-snapshot reads retain the skipped PRIMARY interval without
	// fetching or conflicting any skipped secondary. The C conflict map is checked below.
	if skipped := mdShardPrimaryRows(t, tr, f, skippedBegin, skippedEnd, false); len(skipped) != int(skip) {
		t.Fatalf("ordinary C skipped-primary population=%d want=%d", len(skipped), skip)
	}
	remaining := gofdb.EffectiveRowLimit(limit)
	var rows []libfdbc.CMappedRow
	var batches []mdBatch
	for iteration := 1; iteration <= f.n+1; iteration++ {
		requested := remaining
		var batch []libfdbc.CMappedRow
		more := false
		for calls := 0; calls <= f.n; calls++ {
			target := 0
			if mode == libfdbc.CStreamingModeExact {
				// NativeAPI:4850's unlimited-byte FGT continuation can cross the
				// clipped shard end. Absorb bounded calls with public FGE bounds instead.
				target = 80000
			}
			part, partMore, err := libfdbc.CGetMappedRangeBatch(tr, begin, end, f.getRangeMapper(), requested-len(batch), target, mode, iteration, reverse, false)
			if err != nil {
				t.Fatalf("plain C mapped batch %d call %d: %v", iteration, calls+1, err)
			}
			batch = append(batch, part...)
			more = partMore
			if len(part) > 0 {
				last := part[len(part)-1].Key
				if reverse {
					end = bytes.Clone(last)
				} else {
					begin = append(bytes.Clone(last), 0)
				}
			}
			if mode != libfdbc.CStreamingModeExact || len(batch) >= requested || !more || len(part) == 0 {
				break
			}
			if calls == f.n {
				t.Fatal("bounded C exact page did not terminate")
			}
		}
		batches = append(batches, mdBatch{iteration, requested, len(batch), more})
		rows = append(rows, batch...)
		remaining -= len(batch)
		if !more || len(batch) == 0 || remaining <= 0 {
			return rows, batches
		}
	}
	t.Fatal("plain C mapped iteration did not terminate")
	return nil, nil
}

func mdShardDrainGo(t *testing.T, ctx context.Context, tr *fdbclient.Transaction, f mdFixture, limit int, mode gofdb.StreamingMode, skip int32, reverse bool) ([]gofdb.MappedKeyValue, []mdBatch) {
	t.Helper()
	begin, end := f.indexRange()
	remaining := gofdb.EffectiveRowLimit(limit)
	var rows []gofdb.MappedKeyValue
	var batches []mdBatch
	for iteration := 1; iteration <= f.n+1; iteration++ {
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		target, err := gofdb.ModeTargetBytes(mode, iteration)
		if err != nil {
			t.Fatal(err)
		}
		firstSkip := int32(0)
		if iteration == 1 {
			firstSkip = skip
		}
		batch, more, err := tr.GetMappedRangeWithByteTargetAndSkip(ctx, begin, end, f.getRangeMapper(), remaining, target, firstSkip, reverse)
		if err != nil {
			t.Fatalf("direct Go cross-shard batch %d: %v", iteration, err)
		}
		batches = append(batches, mdBatch{iteration, remaining, len(batch), more})
		rows = append(rows, batch...)
		remaining -= len(batch)
		if !more || len(batch) == 0 || remaining <= 0 {
			return rows, batches
		}
		last := batch[len(batch)-1].Key
		if reverse {
			end = bytes.Clone(last)
		} else {
			begin = append(bytes.Clone(last), 0)
		}
	}
	t.Fatal("direct Go cross-shard iteration did not terminate")
	return nil, nil
}

func mdShardExpectedBounds(f mdFixture, rows []libfdbc.CMappedRow, limit int, reverse bool) []libfdbc.CKeyValue {
	begin, end := f.indexRange()
	if limit > 0 && len(rows) == limit {
		last := rows[len(rows)-1].Key
		if reverse {
			begin = last
		} else {
			end = append(bytes.Clone(last), 0)
		}
	}
	bounds := []libfdbc.CKeyValue{{Key: begin, Value: []byte("1")}, {Key: end, Value: []byte("0")}}
	for _, row := range rows {
		bounds = append(bounds, libfdbc.CKeyValue{Key: row.RangeBeginKey, Value: []byte("1")}, libfdbc.CKeyValue{Key: row.RangeEndKey, Value: []byte("0")})
	}
	sort.Slice(bounds, func(i, j int) bool { return bytes.Compare(bounds[i].Key, bounds[j].Key) < 0 })
	return bounds
}

// Go has no read-conflict inspection API. Check every C interval endpoint and
// every seeded primary/secondary key through deterministic commit-order probes.
// The bounded page itself spans another physical boundary beyond the Skip.
func mdShardConflictMembership(t *testing.T, ctx context.Context, env mdShardEnv, skip int32, reverse bool, first, skipped int) {
	t.Helper()
	f := env.fixture
	limit := env.indices[env.middle+1] - first + 1
	if reverse {
		limit = first - env.indices[env.middle-1] + 2
	}
	ct, err := mdCreateTransaction(t, env.cdb)
	if err != nil {
		t.Fatal(err)
	}
	defer ct.Close()
	if _, err := ct.GetReadVersion(); err != nil {
		t.Fatal(err)
	}
	mdShardAssertFrozen(t, env)
	cRows, _ := mdShardDrainC(t, ct, f, limit, libfdbc.CStreamingModeExact, skip, reverse)
	if len(cRows) != limit || limit < 2 {
		t.Fatalf("bounded conflict page has %d rows, want %d>=2", len(cRows), limit)
	}
	bounds, err := libfdbc.CReadConflictRangeBoundaries(ct)
	if err != nil {
		t.Fatal(err)
	}
	if expected := mdShardExpectedBounds(f, cRows, limit, reverse); !reflect.DeepEqual(bounds, expected) {
		t.Fatalf("membership oracle C=%+v want=%+v", bounds, expected)
	}
	contains := func(key []byte) bool {
		for i := 0; i < len(bounds); i += 2 {
			if bytes.Compare(key, bounds[i].Key) >= 0 && bytes.Compare(key, bounds[i+1].Key) < 0 {
				return true
			}
		}
		return false
	}
	if !contains(f.indexEntryKey(skipped)) || contains(f.splitRecordKey(skipped, 0)) {
		t.Fatal("cross-shard membership oracle lost Skip primary/secondary distinction")
	}
	probes := make(map[string][]byte)
	add := func(key []byte) { probes[string(key)] = bytes.Clone(key) }
	for _, boundary := range bounds {
		add(boundary.Key)
		add(append(bytes.Clone(boundary.Key), 0))
	}
	for i := 0; i < f.n; i++ {
		add(f.indexEntryKey(i))
		add(append(f.indexEntryKey(i), 0))
		for s := 0; s < 3; s++ {
			add(f.splitRecordKey(i, s))
		}
	}
	keys := make([]string, 0, len(probes))
	for key := range probes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	positive, negative := 0, 0
	for _, name := range keys {
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		key := probes[name]
		want := 0
		if contains(key) {
			want = 1020
			positive++
		} else {
			negative++
		}
		reader, err := env.facade.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		// Fresh C versions per probe, never the old oracle/setup version. Keep
		// fresh metadata GRVs off the query client's version floor entirely.
		versionReader, err := mdCreateTransaction(t, env.cdb)
		if err != nil {
			reader.Cancel()
			t.Fatal(err)
		}
		version, err := versionReader.GetReadVersion()
		versionReader.Close()
		if err != nil {
			reader.Cancel()
			t.Fatal(err)
		}
		reader.SetReadVersion(version)
		mdShardAssertFrozen(t, env)
		rows, err := mdSkipFacadeResult(t, reader, f, f.getRangeMapper(), gofdb.RangeOptions{Limit: limit, Mode: gofdb.StreamingModeExact, Reverse: reverse}, skip).GetSliceWithError()
		if err != nil || len(rows) != limit {
			reader.Cancel()
			t.Fatalf("cross-shard membership read rows=%d want=%d err=%v", len(rows), limit, err)
		}
		for i := range cRows {
			mdCompareRangeRow(t, cRows[i], rows[i])
		}
		reader.Set(gofdb.Key(mdPack(f.prefix, []byte("MEMBERSHIP-READER"))), []byte("r"))
		writer, err := env.facade.CreateTransaction()
		if err != nil {
			reader.Cancel()
			t.Fatal(err)
		}
		writer.SetReadVersion(version)
		writer.Set(gofdb.Key(mdPack(f.prefix, []byte("MEMBERSHIP-WRITER"))), []byte("w"))
		if err := writer.AddWriteConflictKey(gofdb.Key(key)); err != nil {
			writer.Cancel()
			reader.Cancel()
			t.Fatal(err)
		}
		if err := writer.Commit().Get(); err != nil {
			writer.Cancel()
			reader.Cancel()
			t.Fatal(err)
		}
		writer.Cancel()
		code := mdMappedErrorCode(t, reader.Commit().Get())
		reader.Cancel()
		if code != want {
			t.Fatalf("cross-shard conflict membership probe %q: Go=%d C interval requires=%d", key, code, want)
		}
		mdShardAssertFrozen(t, env)
	}
	if positive == 0 || negative == 0 {
		t.Fatalf("vacuous cross-shard membership population: positive=%d negative=%d", positive, negative)
	}
	t.Logf("checked all %d C intervals and all seeded keys: %d conflicting/%d non-conflicting probes", len(bounds)/2, positive, negative)
}

func TestLibFDBC_MappedRangeSkipAcrossShards(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	env := mdShardSetup(t, ctx)
	f := env.fixture
	for _, reverse := range []bool{false, true} {
		i := env.indices[env.middle]
		skip, first, skipped := int32(i+1), i+1, 0
		secondBoundary := env.boundaries[env.middle+1]
		if reverse {
			skip, first, skipped = int32(f.n-i+1), i-2, f.n-1
			secondBoundary = env.boundaries[env.middle-1]
		}
		boundary := env.boundaries[env.middle]
		if (!reverse && !(bytes.Compare(f.indexEntryKey(skipped), boundary) < 0 && bytes.Compare(f.indexEntryKey(first), boundary) >= 0 && bytes.Compare(f.indexEntryKey(first), secondBoundary) < 0)) ||
			(reverse && !(bytes.Compare(f.indexEntryKey(skipped), boundary) >= 0 && bytes.Compare(f.indexEntryKey(first), boundary) < 0 && bytes.Compare(f.indexEntryKey(first), secondBoundary) >= 0)) {
			t.Fatalf("chosen Skip does not straddle physical boundaries: reverse=%v skip=%d first=%d", reverse, skip, first)
		}
		population := f.n - int(skip)
		for _, mode := range []struct {
			name   string
			goMode gofdb.StreamingMode
			cMode  int
		}{
			{"small", gofdb.StreamingModeSmall, libfdbc.CStreamingModeSmall},
			{"iterator", gofdb.StreamingModeIterator, libfdbc.CStreamingModeIterator},
			{"exact", gofdb.StreamingModeExact, libfdbc.CStreamingModeExact},
		} {
			limits := []int{0, 7, population}
			if mode.goMode == gofdb.StreamingModeExact {
				limits = []int{7, population}
			}
			for _, limit := range limits {
				t.Run(fmt.Sprintf("data/%s/reverse=%v/skip=%d/limit=%d", mode.name, reverse, skip, limit), func(t *testing.T) {
					ct, err := mdCreateTransaction(t, env.cdb)
					if err != nil {
						t.Fatal(err)
					}
					defer ct.Close()
					version, err := ct.GetReadVersion()
					if err != nil {
						t.Fatal(err)
					}
					mdShardAssertFrozen(t, env)
					cRows, cBatches := mdShardDrainC(t, ct, f, limit, mode.cMode, skip, reverse)
					gt := env.goDB.CreateTransaction()
					defer gt.Cancel()
					gt.SetReadVersion(version)
					gRows, gBatches := mdShardDrainGo(t, ctx, gt, f, limit, mode.goMode, skip, reverse)
					mdShardAssertFrozen(t, env)
					ft, err := env.facade.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer ft.Cancel()
					ft.SetReadVersion(version)
					it := mdSkipFacadeResult(t, ft, f, f.getRangeMapper(), gofdb.RangeOptions{Limit: limit, Mode: mode.goMode, Reverse: reverse}, skip).Iterator()
					var fRows []gofdb.MappedKeyValue
					var fBatches []mdBatch
					it.SetTraceLog(func(iteration, requested, returned int, more bool, err error) {
						if err != nil {
							t.Fatal(err)
						}
						fBatches = append(fBatches, mdBatch{iteration, requested, returned, more})
					})
					for it.Advance() {
						row, err := it.Get()
						if err != nil {
							t.Fatal(err)
						}
						fRows = append(fRows, row)
						if len(fRows) > f.n {
							t.Fatal("cross-shard facade repeated rows")
						}
					}
					if _, err := it.Get(); err != nil {
						t.Fatal(err)
					}
					mdShardAssertFrozen(t, env)
					want := population
					if limit > 0 && limit < want {
						want = limit
					}
					if len(cRows) != want || len(gRows) != want || len(fRows) != want {
						t.Fatalf("cross-shard population C=%d directGo=%d facade=%d want=%d", len(cRows), len(gRows), len(fRows), want)
					}
					if !reflect.DeepEqual(cBatches, gBatches) || !reflect.DeepEqual(cBatches, fBatches) {
						t.Fatalf("cross-shard batch metadata C=%+v directGo=%+v facade=%+v", cBatches, gBatches, fBatches)
					}
					if mode.goMode != gofdb.StreamingModeExact && len(cBatches) < 2 {
						t.Fatal("cross-shard streaming comparison lacks multiple fetches")
					}
					for j := range cRows {
						mdCompareRangeRow(t, cRows[j], gRows[j])
						mdCompareRangeRow(t, cRows[j], fRows[j])
						index := first + j
						if reverse {
							index = first - j
						}
						if !bytes.Equal(cRows[j].Key, f.indexEntryKey(index)) || !bytes.Equal(cRows[j].Value, bytes.Repeat([]byte{byte('A' + index%26)}, 10000)) {
							t.Fatalf("cross-shard primary %d does not match seeded data", j)
						}
					}
					if limit == 0 || limit == population {
						last := cRows[len(cRows)-1].Key
						if (!reverse && !(bytes.Compare(cRows[0].Key, secondBoundary) < 0 && bytes.Compare(last, secondBoundary) >= 0)) ||
							(reverse && !(bytes.Compare(cRows[0].Key, secondBoundary) >= 0 && bytes.Compare(last, secondBoundary) < 0)) {
							t.Fatalf("returned population never crossed second physical boundary %x", secondBoundary)
						}
					}
					bounds, err := libfdbc.CReadConflictRangeBoundaries(ct)
					if err != nil {
						t.Fatal(err)
					}
					if expected := mdShardExpectedBounds(f, cRows, limit, reverse); !reflect.DeepEqual(bounds, expected) {
						t.Fatalf("cross-shard full C conflicts=%+v want=%+v", bounds, expected)
					}
					// No skipped secondary range may appear in the full conflict set.
					probe := f.splitRecordKey(skipped, 0)
					for j := 0; j < len(bounds); j += 2 {
						if bytes.Compare(probe, bounds[j].Key) >= 0 && bytes.Compare(probe, bounds[j+1].Key) < 0 {
							t.Fatal("skipped secondary was mapped/conflicted")
						}
					}
					ft.Set(gofdb.Key(mdPack(f.prefix, []byte("SHARD-READER"))), []byte("r"))
					writer, err := env.facade.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer writer.Cancel()
					writer.SetReadVersion(version)
					writer.Set(gofdb.Key(mdPack(f.prefix, []byte("SHARD-WRITER"))), []byte("w"))
					if err := writer.AddWriteConflictKey(gofdb.Key(f.indexEntryKey(skipped))); err != nil {
						t.Fatal(err)
					}
					if err := writer.Commit().Get(); err != nil {
						t.Fatal(err)
					}
					if code := mdMappedErrorCode(t, ft.Commit().Get()); code != 1020 {
						t.Fatalf("cross-shard skipped-primary conflict=%d want1020", code)
					}
					mdShardAssertFrozen(t, env)
					t.Logf("compared %d primary/%d secondary rows, %d batches; original Skip crosses %x", want, want*3, len(cBatches), boundary)
				})
			}
		}
		t.Run(fmt.Sprintf("full_conflict_membership/reverse=%v", reverse), func(t *testing.T) {
			mdShardConflictMembership(t, ctx, env, skip, reverse, first, skipped)
		})
		for _, primary := range []bool{false, true} {
			t.Run(fmt.Sprintf("own_write/reverse=%v/primary=%v", reverse, primary), func(t *testing.T) {
				ct, err := mdCreateTransaction(t, env.cdb)
				if err != nil {
					t.Fatal(err)
				}
				defer ct.Close()
				version, err := ct.GetReadVersion()
				if err != nil {
					t.Fatal(err)
				}
				mdShardAssertFrozen(t, env)
				gt := env.goDB.CreateTransaction()
				defer gt.Cancel()
				gt.SetReadVersion(version)
				ft, err := env.facade.CreateTransaction()
				if err != nil {
					t.Fatal(err)
				}
				defer ft.Cancel()
				ft.SetReadVersion(version)
				key := f.splitRecordKey(skipped, 0)
				if primary {
					key = f.indexEntryKey(skipped)
				}
				ct.Set(key, []byte("pending"))
				gt.Set(key, []byte("pending"))
				ft.Set(gofdb.Key(key), []byte("pending"))
				begin, end := f.indexRange()
				var cRows []libfdbc.CMappedRow
				var cMore bool
				var cErr error
				if primary {
					// A plain read of the original first primary independently proves
					// that the pending skipped-primary write is not mapped-readable.
					cRows, cMore, cErr = libfdbc.CGetMappedRangeBatch(ct, begin, end, f.getRangeMapper(), 1, 256, libfdbc.CStreamingModeSmall, 1, reverse, false)
				} else {
					var batches []mdBatch
					cRows, batches = mdShardDrainC(t, ct, f, 7, libfdbc.CStreamingModeExact, skip, reverse)
					if len(batches) != 1 {
						t.Fatalf("exact C control returned %d pages", len(batches))
					}
					cMore = batches[0].more
				}
				gRows, gMore, gErr := gt.GetMappedRangeWithByteTargetAndSkip(ctx, begin, end, f.getRangeMapper(), 7, fdbclient.ByteLimitUnlimited, skip, reverse)
				fRows, fErr := mdSkipFacadeResult(t, ft, f, f.getRangeMapper(), gofdb.RangeOptions{Limit: 7, Mode: gofdb.StreamingModeExact, Reverse: reverse}, skip).GetSliceWithError()
				want := 0
				if primary {
					want = 2039
				}
				for _, err := range []error{cErr, gErr, fErr} {
					if code := mdMappedErrorCode(t, err); code != want {
						t.Fatalf("cross-shard own-write code=%d want=%d: %v", code, want, err)
					}
				}
				if primary {
					if len(cRows) != 0 || len(gRows) != 0 || len(fRows) != 0 || cMore || gMore {
						t.Fatal("failed cross-shard mapped read returned rows/more")
					}
				} else {
					if len(cRows) != 7 || len(gRows) != 7 || len(fRows) != 7 || !cMore || gMore != cMore {
						t.Fatalf("skipped-secondary write changed population: C=%d/%v Go=%d/%v facade=%d", len(cRows), cMore, len(gRows), gMore, len(fRows))
					}
					for j := range cRows {
						mdCompareRangeRow(t, cRows[j], gRows[j])
						mdCompareRangeRow(t, cRows[j], fRows[j])
					}
				}
				mdShardAssertFrozen(t, env)
			})
		}
	}
	t.Run("reverse_skip_fallback_at_begin", func(t *testing.T) {
		mdShardReverseSkipFallbackAtBegin(t, ctx, env)
	})
}

// Native reverse Skip that crosses the original end shard uses exact fallback,
// which clears storage More when its final row equals the resolved begin.
func mdShardReverseSkipFallbackAtBegin(t *testing.T, ctx context.Context, env mdShardEnv) {
	t.Helper()
	f := env.fixture
	index := env.indices[env.middle] - 2
	begin, resolvedEnd := f.indexEntryKey(index), f.indexEntryKey(index+1)
	_, end := f.indexRange()
	skip := int32(f.n - index - 1)
	boundary := env.boundaries[env.middle]
	if index < 0 || skip <= 0 || bytes.Compare(begin, env.boundaries[env.middle-1]) < 0 ||
		bytes.Compare(resolvedEnd, boundary) >= 0 || bytes.Compare(boundary, end) >= 0 ||
		bytes.Compare(f.indexEntryKey(f.n-1), env.boundaries[env.middle+1]) < 0 {
		t.Fatalf("reverse Skip does not leave one row before a crossed physical boundary: index=%d skip=%d begin=%x resolvedEnd=%x boundary=%x end=%x", index, skip, begin, resolvedEnd, boundary, end)
	}
	value := bytes.Repeat([]byte("v"), 32)
	primaryBytes := len(begin) + len(value)
	if primaryBytes <= 51 || primaryBytes+8 >= 256 {
		t.Fatalf("fallback fixture misses SMALL prefetch window: primary=%d charge=%d", primaryBytes, primaryBytes+8)
	}
	mdShardAssertFrozen(t, env)
	seed, err := mdCreateTransaction(t, env.cdb)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	seed.Set(begin, value)
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	seed.Close()
	mdShardAssertFrozen(t, env)
	ct, err := mdCreateTransaction(t, env.cdb)
	if err != nil {
		t.Fatal(err)
	}
	defer ct.Close()
	version, err := ct.GetReadVersion()
	if err != nil {
		t.Fatal(err)
	}
	native, err := env.backend.CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer native.Cancel()
	if err := native.Options().SetTimeout(10000); err != nil {
		t.Fatal(err)
	}
	native.SetReadVersion(version)
	primary, err := native.Snapshot().GetRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, gofdb.RangeOptions{Mode: gofdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil || len(primary) != int(skip)+1 {
		t.Fatalf("ordinary C reverse-Skip population=%d want=%d err=%v", len(primary), int(skip)+1, err)
	}
	for j, row := range primary {
		wantValue := bytes.Repeat([]byte{byte('A' + (index+j)%26)}, 10000)
		if j == 0 {
			wantValue = value
		}
		if !bytes.Equal(row.Key, f.indexEntryKey(index+j)) || !bytes.Equal(row.Value, wantValue) {
			t.Fatalf("ordinary C primary %d differs from frozen fixture", j)
		}
	}
	resolved, err := native.Snapshot().GetKey(gofdb.KeySelector{Key: gofdb.Key(end), Offset: 1 - int(skip)}).Get()
	if err != nil || !bytes.Equal(resolved, resolvedEnd) {
		t.Fatalf("ordinary C selector resolved=%x want=%x err=%v", resolved, resolvedEnd, err)
	}
	// Plain selectors in the resolved shard retain storage More. NativeAPI's
	// exact fallback clears it when the final key equals begin (:4366-4368).
	witness, witnessMore, err := libfdbc.CGetMappedRangeBatch(ct, begin, resolvedEnd, f.getRangeMapper(), 2, 256, libfdbc.CStreamingModeSmall, 1, true, false)
	if err != nil || len(witness) != 1 || !witnessMore || !bytes.Equal(witness[0].Key, begin) || !bytes.Equal(witness[0].Value, value) {
		t.Fatalf("same-shard direct More witness: C rows=%d more=%v err=%v", len(witness), witnessMore, err)
	}
	mdShardAssertFrozen(t, env)
	gt := env.goDB.CreateTransaction()
	defer gt.Cancel()
	gt.SetReadVersion(version)
	gRows, gMore, err := gt.GetMappedRangeWithByteTargetAndSkip(ctx, begin, end, f.getRangeMapper(), 2, 256, skip, true)
	if err != nil || len(gRows) != 1 || gMore {
		t.Fatalf("cross-shard exact fallback: Go rows=%d more=%v err=%v; plain C witness=%d/%v", len(gRows), gMore, err, len(witness), witnessMore)
	}
	mdCompareRangeRow(t, witness[0], gRows[0])
	ft, err := env.facade.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer ft.Cancel()
	ft.SetReadVersion(version)
	reader, ok := any(ft).(gofdb.MappedRangeSkipReader)
	if !ok {
		t.Fatal("pure-Go facade lacks mapped Skip capability")
	}
	it := reader.GetMappedRangeWithSkip(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, f.getRangeMapper(),
		gofdb.RangeOptions{Limit: 2, Mode: gofdb.StreamingModeSmall, Reverse: true}, skip).Iterator()
	var batches []mdBatch
	it.SetTraceLog(func(iteration, requested, returned int, more bool, err error) {
		if err != nil {
			t.Fatal(err)
		}
		batches = append(batches, mdBatch{iteration, requested, returned, more})
	})
	seen := 0
	for it.Advance() {
		row, err := it.Get()
		if err != nil || seen != 0 {
			t.Fatalf("fallback facade row %d: %v", seen, err)
		}
		mdCompareRangeRow(t, witness[0], row)
		seen++
	}
	if _, err := it.Get(); err != nil {
		t.Fatal(err)
	}
	if seen != 1 || !reflect.DeepEqual(batches, []mdBatch{{1, 2, 1, false}}) {
		t.Fatalf("fallback facade must finish in one fetch: rows=%d batches=%+v", seen, batches)
	}
	mdShardAssertFrozen(t, env)
	t.Logf("reverse Skip=%d crossed %x; ordinary C population=%d; primary+8=%d<256; plain C storage More=true, Go exact fallback More=false", skip, boundary, len(primary), primaryBytes+8)
}
