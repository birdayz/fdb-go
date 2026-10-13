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
