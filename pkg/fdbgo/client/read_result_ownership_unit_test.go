package client

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

func TestReadResultOwnership_Locations(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	db := &database{}
	db.locCache.insertSorted([]locationEntry{{
		begin: []byte("a"), end: []byte("z"),
		servers: []ServerInfo{{Address: "original:4500"}},
	}})
	tx := &Transaction{db: db}
	defer tx.Cancel()
	locations, err := tx.GetLocations(ctx, []byte("b"), []byte("y"), 1)
	if err != nil || len(locations) != 1 || len(locations[0].Servers) != 1 {
		t.Fatalf("locations = %v, %v", locations, err)
	}
	locations[0].ShardBegin[0] = 'b'
	locations[0].ShardEnd[0] = 'y'
	locations[0].Servers[0].Address = "modified:4500"
	entry := db.locCache.entries[0]
	if string(entry.begin) != "a" || string(entry.end) != "z" || entry.servers[0].Address != "original:4500" {
		t.Fatalf("OWNERSHIP_LOCATIONS: public result changed shared cache: %q..%q, %v", entry.begin, entry.end, entry.servers)
	}
	other := &Transaction{db: db}
	defer other.Cancel()
	locations, err = other.GetLocations(ctx, []byte("b"), []byte("y"), 1)
	if err != nil || len(locations) != 1 || string(locations[0].ShardBegin) != "a" || string(locations[0].ShardEnd) != "z" || locations[0].Servers[0].Address != "original:4500" {
		t.Fatalf("independent transaction locations = %v, %v", locations, err)
	}
}

// Internal caches lend immutable bytes; public reads must not lend them onward.
func TestReadResultOwnership_Point(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"write", "storage"} {
		for _, surface := range []string{"get", "snapshot", "snapshot_no_writes", "pipelined"} {
			t.Run(source+"/"+surface, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				tx := &Transaction{}
				defer tx.Cancel()
				key := []byte("ownership/key")
				if source == "write" {
					tx.Set(key, []byte("AAAA"))
				} else {
					tx.ryw.cachePointResult(key, []byte("AAAA"))
				}
				if surface == "snapshot_no_writes" {
					// Keep the snapshot answer locally known without a network fixture.
					tx.ryw.cachePointResult(key, []byte("AAAA"))
					tx.SetSnapshotRYWDisable()
				}
				read := func() ([]byte, error) {
					switch surface {
					case "snapshot", "snapshot_no_writes":
						return tx.Snapshot().Get(ctx, key)
					case "pipelined":
						value, pending, err := tx.GetPipelined(ctx, key)
						if pending != nil {
							t.Fatal("known point unexpectedly issued a pending read")
						}
						return value, err
					default:
						return tx.Get(ctx, key)
					}
				}
				first, err := read()
				if err != nil || string(first) != "AAAA" {
					t.Fatalf("first read = %q, %v", first, err)
				}
				first[0] = 'Z'
				second, err := read()
				if err != nil || string(second) != "AAAA" {
					t.Fatalf("OWNERSHIP_POINT: independent read = %q, %v; want AAAA", second, err)
				}
			})
		}
	}
}

func TestReadResultOwnership_Range(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"write", "storage"} {
		for _, snapshot := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				for _, limit := range []int{0, 1} {
					t.Run(fmt.Sprintf("%s/snapshot=%t/reverse=%t/limit=%d", source, snapshot, reverse, limit), func(t *testing.T) {
						t.Parallel()
						ctx, cancel := context.WithTimeout(context.Background(), time.Second)
						defer cancel()
						tx := &Transaction{}
						defer tx.Cancel()
						begin, end := []byte("ownership/"), []byte("ownership0")
						rows := []KeyValue{{Key: []byte("ownership/a"), Value: []byte("AAAA")}, {Key: []byte("ownership/b"), Value: []byte("BBBB")}}
						if source == "write" {
							if err := tx.ClearRange(begin, end); err != nil {
								t.Fatal(err)
							}
							for _, row := range rows {
								tx.Set(row.Key, row.Value)
							}
						} else {
							tx.ryw.cacheServerResult(begin, end, rows, false, false)
						}
						read := func() ([]KeyValue, bool, error) {
							if snapshot {
								return tx.Snapshot().GetRangeWithByteTarget(ctx, begin, end, limit, ByteLimitUnlimited, reverse)
							}
							return tx.GetRangeWithByteTarget(ctx, begin, end, limit, ByteLimitUnlimited, reverse)
						}
						first, _, err := read()
						if err != nil || len(first) == 0 {
							t.Fatalf("first range = %v, %v", first, err)
						}
						key, value := bytes.Clone(first[0].Key), bytes.Clone(first[0].Value)
						first[0].Key[len(first[0].Key)-1] = 'z'
						first[0].Value[0] = 'Z'
						got, err := tx.Get(ctx, key)
						if err != nil || !bytes.Equal(got, value) {
							t.Errorf("OWNERSHIP_RANGE_POINT: %q = %q, %v; want %q", key, got, err, value)
						}
						second, _, err := read()
						if err != nil || len(second) != len(first) || !bytes.Equal(second[0].Key, key) || !bytes.Equal(second[0].Value, value) {
							t.Fatalf("OWNERSHIP_RANGE: independent range = %v, %v; want first row %q=%q", second, err, key, value)
						}
					})
				}
			}
		}
	}
}

func TestReadResultOwnership_KeyspaceEnd(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("snapshot=%t", snapshot), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			tx := &Transaction{}
			defer tx.Cancel()
			tx.SetReadYourWritesDisable()
			tx.SetReadSystemKeys()
			tx.SetReadVersion(1)
			var key []byte
			var err error
			if snapshot {
				key, err = tx.Snapshot().GetKey(ctx, []byte{0xff, 0xff}, false, 1)
			} else {
				key, err = tx.GetKey(ctx, []byte{0xff, 0xff}, false, 1)
			}
			if err != nil || !bytes.Equal(key, []byte{0xff, 0xff}) {
				t.Fatalf("keyspace end = %x, %v", key, err)
			}
			// Do not scribble over a process-wide constant in a parallel test.
			if &key[0] == &allKeysEnd[0] {
				t.Fatal("OWNERSHIP_KEYSPACE_END: result aliases the global keyspace boundary")
			}
			key[0] = 0
			if !bytes.Equal(allKeysEnd, []byte{0xff, 0xff}) {
				t.Fatalf("global keyspace boundary changed: %x", allKeysEnd)
			}
		})
	}
}

func TestReadResultOwnership_EmptyAndAbsent(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("snapshot=%t", snapshot), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			tx := &Transaction{}
			defer tx.Cancel()
			tx.Set([]byte("empty"), nil)
			tx.Clear([]byte("absent"))
			for _, key := range []string{"empty", "absent"} {
				var got []byte
				var err error
				if snapshot {
					got, err = tx.Snapshot().Get(ctx, []byte(key))
				} else {
					got, err = tx.Get(ctx, []byte(key))
				}
				if err != nil || len(got) != 0 || (got == nil) != (key == "absent") {
					t.Fatalf("%s: got %v (nil=%t), %v", key, got, got == nil, err)
				}
			}
		})
	}
}
