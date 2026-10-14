//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	fdbclient "fdb.dev/pkg/fdbgo/client"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/fdbgo/libfdbc"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// FDB 7.3.77 does not support raw mapped selectors resolving to begin > end.
// Compare exhausted ranges with ordinary C reads and plain mapped selectors.
func TestLibFDBC_MappedSkipResolvesBeforeMappedRead(t *testing.T) {
	t.Parallel()
	startup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctr, err := foundationdbtc.Run(startup, "", foundationdbtc.WithDirectIP())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelStop()
		foundationdbtc.TerminateT(t, stop, ctr)
	}()
	cf, err := ctr.ClusterFile(startup)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(path, []byte(cf), 0o600); err != nil {
		t.Fatal(err)
	}
	cdb, err := libfdbc.COpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	openCtx, openCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer openCancel()
	db, err := fdbclient.OpenDatabase(openCtx, path, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	newCTransaction := func(t *testing.T) libfdbc.CTxnHandle {
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
	prefix := t.Name()
	seed := newCTransaction(t)
	defer seed.Close()
	for i := 0; i < 12; i++ {
		seed.Set(tuple.Tuple{prefix, "IDX", i, i}.Pack(), []byte(fmt.Sprint(i)))
		for j := 0; j < 3; j++ {
			seed.Set(tuple.Tuple{prefix, "REC", i, j}.Pack(), bytes.Repeat([]byte{byte(i), byte(j)}, 512))
		}
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	seed.Close()
	begin := tuple.Tuple{prefix, "IDX"}.Pack()
	end := append([]byte(nil), begin...)
	end[len(end)-1]++
	mapper := tuple.Tuple{prefix, "REC", "{K[3]}", "{...}"}.Pack()

	// The ordinary C index scan is independent of the mapped selector path.
	// Check its population AND values against the fixture before slicing it.
	indexTr := newCTransaction(t)
	defer indexTr.Close()
	var index []libfdbc.CKeyValue
	cursor := begin
	for iteration := 1; ; iteration++ {
		rows, more, err := libfdbc.CGetRangeBatch(indexTr, cursor, end, 0, 0, libfdbc.CStreamingModeWantAll, iteration, false, false)
		if err != nil {
			t.Fatalf("ordinary C index scan: %v", err)
		}
		index = append(index, rows...)
		if len(index) > 12 {
			t.Fatalf("ordinary C index scan exceeded fixture: %d rows", len(index))
		}
		if !more {
			break
		}
		if len(rows) == 0 {
			t.Fatal("ordinary C index scan made no progress")
		}
		cursor = append(append([]byte(nil), rows[len(rows)-1].Key...), 0)
	}
	indexTr.Close()
	if len(index) != 12 {
		t.Fatalf("ordinary C index scan: got %d rows, want 12", len(index))
	}
	for i, row := range index {
		if !bytes.Equal(row.Key, tuple.Tuple{prefix, "IDX", i, i}.Pack()) || !bytes.Equal(row.Value, []byte(fmt.Sprint(i))) {
			t.Fatalf("ordinary C index row %d: got (%x,%q)", i, row.Key, row.Value)
		}
	}

	readGo := func(t *testing.T, skip int32, reverse bool) ([]fdbclient.MappedKeyValue, bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tr := db.CreateTransaction()
		defer tr.Cancel()
		rows, more, err := tr.GetMappedRangeWithByteTargetAndSkip(ctx, begin, end, mapper, 4, fdbclient.ByteLimitUnlimited, skip, reverse)
		if err != nil {
			t.Fatalf("Go mapped skip=%d reverse=%v: %v", skip, reverse, err)
		}
		return rows, more
	}
	readC := func(t *testing.T, skip int32, reverse bool) ([]libfdbc.CMappedRow, bool) {
		t.Helper()
		// Ordinary reads provide the bounds without unsupported mapped selectors.
		b, e := begin, end
		if skip >= int32(len(index)) {
			b = e
		} else if skip > 0 {
			if reverse {
				e = index[len(index)-int(skip)].Key
			} else {
				b = index[int(skip)].Key
			}
		}
		tr := newCTransaction(t)
		defer tr.Close()
		rows, more, err := libfdbc.CGetMappedRangeBatch(tr, b, e, mapper, 4, 0, libfdbc.CStreamingModeExact, 1, reverse, false)
		if err != nil {
			t.Fatalf("C exact mapped bounds: %v", err)
		}
		return rows, more
	}
	checkRows := func(t *testing.T, goRows []fdbclient.MappedKeyValue, goMore bool, cRows []libfdbc.CMappedRow, cMore bool, want []int, wantMore bool) {
		t.Helper()
		if len(goRows) != len(want) || len(cRows) != len(want) || goMore != wantMore || cMore != wantMore {
			t.Fatalf("mapped rows/more: Go=%d/%v C=%d/%v want=%d/%v", len(goRows), goMore, len(cRows), cMore, len(want), wantMore)
		}
		for n, i := range want {
			g, c, primary := goRows[n], cRows[n], index[i]
			if !bytes.Equal(g.Key, primary.Key) || !bytes.Equal(g.Value, primary.Value) || !bytes.Equal(c.Key, primary.Key) || !bytes.Equal(c.Value, primary.Value) {
				t.Fatalf("mapped primary %d: Go=(%x,%q) C=(%x,%q) want=(%x,%q)", n, g.Key, g.Value, c.Key, c.Value, primary.Key, primary.Value)
			}
			if g.Kind != fdbclient.MappedResultGetRange || c.Kind != libfdbc.CMappedKindGetRange || len(g.GetRange.Rows) != 3 || len(c.RangeRows) != 3 || g.GetRange.More {
				t.Fatalf("mapped secondary %d: Go kind=%v rows=%d more=%v C kind=%v rows=%d", n, g.Kind, len(g.GetRange.Rows), g.GetRange.More, c.Kind, len(c.RangeRows))
			}
			if !bytes.Equal(g.GetRange.Begin.Key, c.RangeBeginKey) || !bytes.Equal(g.GetRange.End.Key, c.RangeEndKey) {
				t.Fatalf("mapped secondary %d bounds differ: Go=[%x,%x) C=[%x,%x)", n, g.GetRange.Begin.Key, g.GetRange.End.Key, c.RangeBeginKey, c.RangeEndKey)
			}
			for j := 0; j < 3; j++ {
				key := tuple.Tuple{prefix, "REC", i, j}.Pack()
				value := bytes.Repeat([]byte{byte(i), byte(j)}, 512)
				gkv, ckv := g.GetRange.Rows[j], c.RangeRows[j]
				if !bytes.Equal(gkv.Key, key) || !bytes.Equal(gkv.Value, value) || !bytes.Equal(ckv.Key, key) || !bytes.Equal(ckv.Value, value) {
					t.Fatalf("mapped secondary %d row %d differs from seeded record", n, j)
				}
			}
		}
	}
	storageRoles := func(t *testing.T) map[string]string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		status, err := ctr.FDBCLIExec(ctx, "status json")
		if err != nil {
			t.Fatalf("storage status: %v\n%s", err, status)
		}
		roles, err := mappedSkipStorageRoles([]byte(status))
		if err != nil {
			t.Fatalf("storage status: %v\n%s", err, status)
		}
		return roles
	}
	before := storageRoles(t)
	for _, tc := range []struct {
		name    string
		skip    int32
		reverse bool
		want    []int
		more    bool
	}{
		{name: "forward12", skip: 12},
		{name: "forward14", skip: 14},
		{name: "reverse12", skip: 12, reverse: true},
		{name: "reverse14", skip: 14, reverse: true},
		{name: "forward_max_minus_one", skip: math.MaxInt32 - 1},
		{name: "forward_max", skip: math.MaxInt32},
		{name: "reverse_max_minus_one", skip: math.MaxInt32 - 1, reverse: true},
		{name: "reverse_max", skip: math.MaxInt32, reverse: true},
		{name: "forward3", skip: 3, want: []int{3, 4, 5, 6}, more: true},
		{name: "reverse3", skip: 3, reverse: true, want: []int{8, 7, 6, 5}, more: true},
		{name: "forward_zero", want: []int{0, 1, 2, 3}, more: true},
		{name: "reverse_zero", reverse: true, want: []int{11, 10, 9, 8}, more: true},
		{name: "forward_negative", skip: -3, want: []int{0, 1, 2, 3}, more: true},
		{name: "reverse_negative", skip: -3, reverse: true, want: []int{11, 10, 9, 8}, more: true},
	} {
		if !t.Run(tc.name, func(t *testing.T) {
			g, gm := readGo(t, tc.skip, tc.reverse)
			c, cm := readC(t, tc.skip, tc.reverse)
			checkRows(t, g, gm, c, cm, tc.want, tc.more)

			// Both clients must remain usable on the same storage roles.
			g, gm = readGo(t, 0, false)
			c, cm = readC(t, 0, false)
			checkRows(t, g, gm, c, cm, []int{0, 1, 2, 3}, true)
			if after := storageRoles(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("storage roles/processes changed: before=%v after=%v", before, after)
			}
		}) {
			return
		}
	}
}

// Establish the query client's cluster floor before pinning a C snapshot.
func primeMappedSnapshotClient(t *testing.T, ctx context.Context, db *fdbclient.Database) int64 {
	t.Helper()
	tr := db.CreateTransaction()
	defer tr.Cancel()
	version, err := tr.GetReadVersion(ctx)
	if err != nil || version <= 0 {
		t.Fatalf("prime query client read version: version=%d err=%v", version, err)
	}
	return version
}

// A full primary prefetch can report More at the final key. An unlimited-byte
// read must finish successfully when its next continuation has equal bounds.
func TestLibFDBC_MappedSkipEqualContinuation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctr, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithDirectIP())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelStop()
		foundationdbtc.TerminateT(t, stop, ctr)
	}()
	cf, err := ctr.ClusterFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(path, []byte(cf), 0o600); err != nil {
		t.Fatal(err)
	}
	cdb, err := libfdbc.COpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	db, err := fdbclient.OpenDatabase(ctx, path, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	floor := primeMappedSnapshotClient(t, ctx, db)
	facade := gofdb.WrapDatabase(db)
	prefix := t.Name()
	keys := [][]byte{tuple.Tuple{prefix, "IDX", 0}.Pack(), tuple.Tuple{prefix, "IDX", 1}.Pack()}
	value := bytes.Repeat([]byte("v"), 20000)
	mapper := tuple.Tuple{prefix, "REC", "{K[2]}", "{...}"}.Pack()
	begin, end := keys[0], append(bytes.Clone(keys[1]), 0)
	seed, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	if err := seed.SetTimeout(10000); err != nil {
		t.Fatal(err)
	}
	for i, key := range keys {
		seed.Set(key, value)
		seed.Set(tuple.Tuple{prefix, "REC", i, 0}.Pack(), []byte("secondary"))
		primaryBytes := len(key) + len(value)
		if primaryBytes <= 16000 || primaryBytes+8 >= 80000 {
			t.Fatalf("primary %d misses 80KB prefetch window: bytes=%d charge=%d", i, primaryBytes, primaryBytes+8)
		}
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}
	seed.Close()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			ct, err := cdb.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			defer ct.Close()
			if err := ct.SetTimeout(10000); err != nil {
				t.Fatal(err)
			}
			version, err := ct.GetReadVersion()
			if err != nil {
				t.Fatal(err)
			}
			// Advance the cluster after the shared snapshot without touching mapped keys.
			advance, err := cdb.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			defer advance.Close()
			if err := advance.SetTimeout(10000); err != nil {
				t.Fatal(err)
			}
			advance.Set(tuple.Tuple{prefix, "VERSION", reverse}.Pack(), []byte("advance"))
			if err := advance.Commit(); err != nil {
				t.Fatal(err)
			}
			probe, err := cdb.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Close()
			if err := probe.SetTimeout(10000); err != nil {
				t.Fatal(err)
			}
			current, err := probe.GetReadVersion()
			if err != nil || version < floor || current <= version {
				t.Fatalf("snapshot order: primed=%d shared=%d current=%d err=%v", floor, version, current, err)
			}
			t.Logf("snapshot order: primed=%d shared=%d current=%d", floor, version, current)
			index, resolvedBegin, resolvedEnd := 1, keys[1], end
			if reverse {
				index, resolvedBegin, resolvedEnd = 0, begin, keys[1]
			}
			// With two row slots and an 80KB byte budget, one 20KB primary
			// can report More only through the storage prefetch boundary.
			witness, witnessMore, err := libfdbc.CGetMappedRangeBatch(ct, resolvedBegin, resolvedEnd, mapper, 2, 80000, libfdbc.CStreamingModeExact, 1, reverse, false)
			if err != nil || len(witness) != 1 || !witnessMore || !bytes.Equal(witness[0].Key, keys[index]) {
				t.Fatalf("storage More witness: C rows=%d more=%v err=%v", len(witness), witnessMore, err)
			}
			// The bounded C witness is a row oracle only. Its raw unbounded
			// continuation can resolve beyond end, outside FDB 7.3.77's mapped contract.
			cRows := witness
			secondary := tuple.Tuple{prefix, "REC", index, 0}.Pack()
			if !bytes.Equal(cRows[0].Key, keys[index]) || !bytes.Equal(cRows[0].Value, value) ||
				cRows[0].Kind != libfdbc.CMappedKindGetRange || len(cRows[0].RangeRows) != 1 ||
				!bytes.Equal(cRows[0].RangeRows[0].Key, secondary) || string(cRows[0].RangeRows[0].Value) != "secondary" {
				t.Fatal("bounded C witness differs from seeded primary/secondary")
			}
			checkGo := func(rows []gofdb.MappedKeyValue, more bool, err error) {
				t.Helper()
				if err != nil || len(rows) != 1 || more {
					t.Fatalf("Go unlimited equal continuation: rows=%d more=%v err=%v", len(rows), more, err)
				}
				row := rows[0]
				if !bytes.Equal(row.Key, cRows[0].Key) || !bytes.Equal(row.Value, cRows[0].Value) ||
					row.Kind != fdbclient.MappedResultGetRange || len(row.GetRange.Rows) != 1 || row.GetRange.More ||
					!bytes.Equal(row.GetRange.Begin.Key, cRows[0].RangeBeginKey) || !bytes.Equal(row.GetRange.End.Key, cRows[0].RangeEndKey) ||
					!bytes.Equal(row.GetRange.Rows[0].Key, secondary) || string(row.GetRange.Rows[0].Value) != "secondary" {
					t.Fatal("Go unlimited result differs from C and seeded primary/secondary")
				}
			}
			gt := db.CreateTransaction()
			defer gt.Cancel()
			gt.SetReadVersion(version)
			rows, more, err := gt.GetMappedRangeWithByteTargetAndSkip(ctx, begin, end, mapper, 2, fdbclient.ByteLimitUnlimited, 1, reverse)
			checkGo(rows, more, err)
			ft, err := facade.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			defer ft.Cancel()
			ft.SetReadVersion(version)
			reader, ok := any(ft).(gofdb.MappedRangeSkipReader)
			if !ok {
				t.Fatal("pure-Go facade lacks mapped Skip capability")
			}
			rows, err = reader.GetMappedRangeWithSkip(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, mapper,
				gofdb.RangeOptions{Limit: 2, Mode: gofdb.StreamingModeExact, Reverse: reverse}, 1).GetSliceWithError()
			checkGo(rows, false, err)
		})
	}
}

// Status.actor.cpp identifies processes by cluster.processes map keys and each
// storage role by its own ID; other roles may legitimately change independently.
func mappedSkipStorageRoles(raw []byte) (map[string]string, error) {
	var status struct {
		Cluster struct {
			Processes map[string]struct {
				Roles []struct {
					Role string `json:"role"`
					ID   string `json:"id"`
				} `json:"roles"`
			} `json:"processes"`
		} `json:"cluster"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, fmt.Errorf("decode status JSON: %w", err)
	}
	roles := make(map[string]string)
	for processID, process := range status.Cluster.Processes {
		for _, role := range process.Roles {
			if role.Role != "storage" {
				continue
			}
			if processID == "" || role.ID == "" {
				return nil, fmt.Errorf("storage role has empty identity: process=%q role=%q", processID, role.ID)
			}
			if _, duplicate := roles[role.ID]; duplicate {
				return nil, fmt.Errorf("duplicate storage role %q", role.ID)
			}
			roles[role.ID] = processID
		}
	}
	if len(roles) == 0 {
		return nil, fmt.Errorf("status contains no storage roles")
	}
	return roles, nil
}

func TestMappedSkipStorageRolePopulation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		json string
		want map[string]string
	}{
		{name: "malformed", json: `{`},
		{name: "no_cluster", json: `{}`},
		{name: "no_processes", json: `{"cluster":{"processes":{}}}`},
		{name: "no_roles", json: `{"cluster":{"processes":{"p":{}}}}`},
		{name: "nonstorage_only", json: `{"cluster":{"processes":{"p":{"roles":[{"role":"log","id":"l"}]}}}}`},
		{name: "empty_process", json: `{"cluster":{"processes":{"":{"roles":[{"role":"storage","id":"s"}]}}}}`},
		{name: "empty_role", json: `{"cluster":{"processes":{"p":{"roles":[{"role":"storage"}]}}}}`},
		{name: "duplicate_role", json: `{"cluster":{"processes":{"p":{"roles":[{"role":"storage","id":"s"},{"role":"storage","id":"s"}]}}}}`},
		{name: "storage_and_other_roles", json: `{"cluster":{"processes":{"p1":{"roles":[{"role":"storage","id":"s1"},{"role":"log","id":"l1"}]},"p2":{"roles":[{"role":"storage","id":"s2"},{"role":"storage","id":"s3"}]}}}}`, want: map[string]string{"s1": "p1", "s2": "p2", "s3": "p2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mappedSkipStorageRoles([]byte(tc.json))
			if tc.want == nil {
				if err == nil {
					t.Fatalf("accepted invalid storage population: %v", got)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("storage identities=%v err=%v want=%v", got, err, tc.want)
			}
		})
	}
}

func TestCTxnSetTimeoutRejectsClosedHandle(t *testing.T) {
	t.Parallel()
	var tr libfdbc.CTxnHandle
	var fdbErr *libfdbc.CFDBError
	if err := tr.SetTimeout(10000); !errors.As(err, &fdbErr) || fdbErr.Code != 2015 {
		t.Fatalf("closed transaction SetTimeout: got %v, want error 2015", err)
	}
}
