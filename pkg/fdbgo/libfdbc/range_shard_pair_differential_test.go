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

type rspEnv struct {
	f          mdFixture
	cdb        *libfdbc.CDatabase
	backend    gofdb.BackendDatabase
	goDB       *fdbclient.Database
	boundaries [][]byte // physical boundaries with fixture keys on both sides
}

// Raw C futures do not observe Go contexts; bound every transaction natively.
func rspCreateTransaction(t *testing.T, cdb *libfdbc.CDatabase) libfdbc.CTxnHandle {
	t.Helper()
	tr, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.SetTimeout(10000); err != nil {
		tr.Close()
		t.Fatal(err)
	}
	return tr
}

func rspBoundaries(t *testing.T, backend gofdb.BackendDatabase, f mdFixture) [][]byte {
	t.Helper()
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
	begin, end := f.indexRange()
	prefix := []byte("\xff/keyServers/")
	rows, err := tr.Snapshot().GetRange(gofdb.KeyRange{
		Begin: gofdb.Key(append(bytes.Clone(prefix), begin...)),
		End:   gofdb.Key(append(bytes.Clone(prefix), end...)),
	}, gofdb.RangeOptions{Mode: gofdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		t.Fatalf("read keyServers: %v", err)
	}
	var boundaries [][]byte
	for _, row := range rows {
		key := bytes.TrimPrefix(row.Key, prefix)
		i := sort.Search(f.n, func(i int) bool { return bytes.Compare(f.indexEntryKey(i), key) >= 0 })
		if i > 0 && i < f.n {
			boundaries = append(boundaries, bytes.Clone(key))
		}
	}
	return boundaries
}

func rspSetup(t *testing.T, ctx context.Context) rspEnv {
	t.Helper()
	startup, cancelStartup := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelStartup()
	container, err := foundationdbtc.Run(startup, "",
		foundationdbtc.WithStorageEngine("ssd"), foundationdbtc.WithDirectIP(),
		foundationdbtc.WithProcessCount(3), foundationdbtc.WithRedundancyMode("double"),
		foundationdbtc.WithKnob("min_shard_bytes", "10000"),
		foundationdbtc.WithKnob("max_shard_bytes", "50000"),
		foundationdbtc.WithKnob("shard_bytes_ratio", "2"),
		foundationdbtc.WithKnob("storage_metrics_polling_delay", "1"),
		// Give the mapped index read the whole request budget, so a mapped shard
		// reply ends with More=false wherever a plain one does.
		foundationdbtc.WithKnob("fraction_index_bytelimit_prefetch", "1"))
	if err != nil {
		t.Fatalf("three-process cluster startup: %v", err)
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
	env := rspEnv{f: mdFixture{prefix: []byte(t.Name()), n: 100}, cdb: cdb, backend: backend, goDB: goDB}
	for batch := 0; batch < 10; batch++ {
		tr := rspCreateTransaction(t, cdb)
		for i := batch * 10; i < (batch+1)*10; i++ {
			tr.Set(env.f.indexEntryKey(i), bytes.Repeat([]byte{byte('A' + i%26)}, 10000))
			for s := 0; s < 3; s++ {
				tr.Set(env.f.splitRecordKey(i, s), bytes.Repeat([]byte{byte('a' + (i+s)%26)}, 400))
			}
		}
		if err := tr.Commit(); err != nil {
			tr.Close()
			t.Fatalf("seed batch %d: %v", batch, err)
		}
		tr.Close()
	}
	for split := time.Now(); len(env.boundaries) < 8; {
		if time.Since(split) > time.Minute {
			t.Fatalf("index range split into only %d populated shards", len(env.boundaries)+1)
		}
		time.Sleep(time.Second)
		env.boundaries = rspBoundaries(t, backend, env.f)
	}
	if output, err := container.FDBCLIExec(ctx, "datadistribution off"); err != nil {
		t.Fatalf("freeze data distribution: %v, output=%s", err, output)
	}
	for stable, settle := 0, time.Now(); stable < 3; {
		if time.Since(settle) > 20*time.Second {
			t.Fatal("keyServers boundaries did not settle after data distribution off")
		}
		time.Sleep(time.Second)
		if boundaries := rspBoundaries(t, backend, env.f); reflect.DeepEqual(boundaries, env.boundaries) {
			stable++
		} else {
			env.boundaries, stable = boundaries, 0
		}
	}
	return env
}

// TestLibFDBC_RangeShardPairSoftLimit pins where a byte-limited read ends when it
// starts in a shard with nothing left to read. libfdb_c then falls back to
// getExactRange (NativeAPI.actor.cpp:4828-4836), which locates shards in pairs and
// relocates after the second without its soft byte-limit return (:4392-4417), so
// the call returns rows from two shards, the second fetched with the byte budget
// the first left over. Plain and mapped reads share that loop.
func TestLibFDBC_RangeShardPairSoftLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env := rspSetup(t, ctx)
	f := env.f
	begin, end := f.indexRange()
	mapper := f.getRangeMapper()
	index := make(map[string]int, f.n)
	for i := 0; i < f.n; i++ {
		index[string(f.indexEntryKey(i))] = i
	}
	rowIndices := func(n int, key func(int) []byte) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = index[string(key(i))]
		}
		return out
	}
	shardOf := func(key []byte) int {
		return sort.Search(len(env.boundaries), func(i int) bool { return bytes.Compare(env.boundaries[i], key) > 0 })
	}
	targets := []struct {
		cMode, iteration int
		goMode           gofdb.StreamingMode
	}{
		{libfdbc.CStreamingModeIterator, 5, gofdb.StreamingModeIterator},
		{libfdbc.CStreamingModeIterator, 6, gofdb.StreamingModeIterator},
		{libfdbc.CStreamingModeIterator, 7, gofdb.StreamingModeIterator},
		{libfdbc.CStreamingModeIterator, 10, gofdb.StreamingModeIterator},
		{libfdbc.CStreamingModeSerial, 1, gofdb.StreamingModeSerial},
	}
	crossings := map[string]int{}
	for j, boundary := range env.boundaries {
		next := sort.Search(f.n, func(i int) bool { return bytes.Compare(f.indexEntryKey(i), boundary) >= 0 })
		// The emptied starts leave nothing to read in their first shard; the
		// controls start on a key, so their first shard always returns rows.
		for _, s := range []struct {
			name       string
			begin, end []byte
			reverse    bool
		}{
			{"forward/emptied", append(f.indexEntryKey(next-1), 0), end, false},
			{"forward/at_key", f.indexEntryKey(next), end, false},
			{"reverse/emptied", begin, f.indexEntryKey(next), true},
			{"reverse/at_key", begin, append(f.indexEntryKey(next-1), 0), true},
		} {
			for _, target := range targets {
				byteTarget, err := gofdb.ModeTargetBytes(target.goMode, target.iteration)
				if err != nil {
					t.Fatal(err)
				}
				name := fmt.Sprintf("%s/boundary=%d/mode=%d/iteration=%d", s.name, j, target.cMode, target.iteration)
				// libfdb_c's RYW range read caps its storage read at REPLY_BYTE_LIMIT
				// (ReadYourWrites.actor.cpp:569-590); with RYW disabled it does not.
				for _, path := range []struct {
					name                  string
					snapshot, rywDisabled bool
				}{{"plain", false, false}, {"plain_snapshot", true, false}, {"plain_ryw_disabled", false, true}} {
					t.Run(name+"/"+path.name, func(t *testing.T) {
						ct := rspCreateTransaction(t, env.cdb)
						defer ct.Close()
						gt := env.goDB.CreateTransaction()
						defer gt.Cancel()
						if path.rywDisabled {
							if err := ct.DisableReadYourWrites(); err != nil {
								t.Fatal(err)
							}
							gt.SetReadYourWritesDisable()
						}
						version, err := ct.GetReadVersion()
						if err != nil {
							t.Fatal(err)
						}
						gt.SetReadVersion(version)
						cRows, cMore, cErr := libfdbc.CGetRangeBatch(ct, s.begin, s.end, 0, 0, target.cMode, target.iteration, s.reverse, path.snapshot)
						if cErr != nil || len(cRows) == 0 {
							t.Fatalf("C rows=%d err=%v", len(cRows), cErr)
						}
						if shardOf(cRows[0].Key) != shardOf(cRows[len(cRows)-1].Key) {
							crossings[path.name+"/"+s.name]++
						}
						read := gt.GetRangeWithByteTarget
						if path.snapshot {
							read = gt.Snapshot().GetRangeWithByteTarget
						}
						gRows, gMore, gErr := read(ctx, s.begin, s.end, 0, byteTarget, s.reverse)
						if gErr != nil {
							t.Fatal(gErr)
						}
						cIndices := rowIndices(len(cRows), func(i int) []byte { return cRows[i].Key })
						gIndices := rowIndices(len(gRows), func(i int) []byte { return gRows[i].Key })
						if cMore != gMore || !reflect.DeepEqual(cIndices, gIndices) {
							t.Fatalf("C more=%v rows=%v; Go more=%v rows=%v", cMore, cIndices, gMore, gIndices)
						}
						for i := range cRows {
							if !bytes.Equal(cRows[i].Key, gRows[i].Key) || !bytes.Equal(cRows[i].Value, gRows[i].Value) {
								t.Fatalf("row %d differs", i)
							}
						}
					})
				}
				t.Run(name+"/mapped", func(t *testing.T) {
					ct := rspCreateTransaction(t, env.cdb)
					defer ct.Close()
					version, err := ct.GetReadVersion()
					if err != nil {
						t.Fatal(err)
					}
					cRows, cMore, cErr := libfdbc.CGetMappedRangeBatch(ct, s.begin, s.end, mapper, 0, 0, target.cMode, target.iteration, s.reverse, false)
					if cErr != nil || len(cRows) == 0 {
						t.Fatalf("C rows=%d err=%v", len(cRows), cErr)
					}
					if shardOf(cRows[0].Key) != shardOf(cRows[len(cRows)-1].Key) {
						crossings["mapped/"+s.name]++
					}
					gt := env.goDB.CreateTransaction()
					defer gt.Cancel()
					gt.SetReadVersion(version)
					gRows, gMore, gErr := gt.GetMappedRangeWithByteTarget(ctx, s.begin, s.end, mapper, 0, byteTarget, s.reverse)
					if gErr != nil {
						t.Fatal(gErr)
					}
					cIndices := rowIndices(len(cRows), func(i int) []byte { return cRows[i].Key })
					gIndices := rowIndices(len(gRows), func(i int) []byte { return gRows[i].Key })
					if cMore != gMore || !reflect.DeepEqual(cIndices, gIndices) {
						t.Fatalf("C more=%v rows=%v; Go more=%v rows=%v", cMore, cIndices, gMore, gIndices)
					}
					for i := range cRows {
						mdCompareRangeRow(t, cRows[i], gRows[i])
					}
				})
			}
		}
	}
	for _, kind := range []string{"plain", "plain_snapshot", "plain_ryw_disabled", "mapped"} {
		for _, direction := range []string{"forward", "reverse"} {
			if crossings[kind+"/"+direction+"/emptied"] == 0 {
				t.Errorf("no %s %s read from an emptied shard spanned two shards: %v", kind, direction, crossings)
			}
			if crossings[kind+"/"+direction+"/at_key"] != 0 {
				t.Errorf("a %s %s read whose first shard returned rows spanned two shards: %v", kind, direction, crossings)
			}
		}
	}
	if !reflect.DeepEqual(rspBoundaries(t, env.backend, f), env.boundaries) {
		t.Fatal("physical boundaries moved during the comparison")
	}
	t.Logf("%d interior boundaries; reads spanning two shards: %v", len(env.boundaries), crossings)
}
