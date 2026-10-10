package rywlocal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/wire"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

type rangeExpectation struct {
	begin, end []byte
	rows       []client.KeyValue
	limit      int
	bytes      int
	more       bool
	errorCode  int
	remote     bool
	snapshot   bool
}

type rangeCase struct {
	name  string
	setup func(*testing.T, *client.Transaction, func(string) []byte, bool) rangeExpectation
}

func localRangeCases() []rangeCase {
	cases := []rangeCase{
		{"single_set_exact_span", func(_ *testing.T, tx *client.Transaction, key func(string) []byte, _ bool) rangeExpectation {
			tx.Set(key("k"), nil)
			return rangeExpectation{begin: key("k"), end: key("k\x00"), rows: []client.KeyValue{{Key: key("k"), Value: []byte{}}}, bytes: client.ByteLimitUnlimited}
		}},
		{"clear_exact_span", func(t *testing.T, tx *client.Transaction, key func(string) []byte, _ bool) rangeExpectation {
			if err := tx.ClearRange(key("a"), key("z")); err != nil {
				t.Fatal(err)
			}
			return rangeExpectation{begin: key("a"), end: key("z"), bytes: client.ByteLimitUnlimited}
		}},
		{"unknown_before_first_row", func(_ *testing.T, tx *client.Transaction, key func(string) []byte, reverse bool) rangeExpectation {
			tx.Set(key("k"), []byte("local"))
			first := "a"
			if reverse {
				first = "y"
			}
			return rangeExpectation{
				begin: key("a"), end: key("z"), limit: 1, bytes: 1000000, more: true, remote: true,
				rows: []client.KeyValue{{Key: key(first), Value: []byte(first)}},
			}
		}},
		{"dependent_atomic_unknown", func(_ *testing.T, tx *client.Transaction, key func(string) []byte, _ bool) rangeExpectation {
			tx.Atomic(client.MutAddValue, key("m"), []byte{1})
			return rangeExpectation{
				begin: key("m"), end: key("m\x00"), bytes: client.ByteLimitUnlimited, remote: true,
				rows: []client.KeyValue{{Key: key("m"), Value: []byte{2}}},
			}
		}},
		{"independent_atomic_cleared", func(t *testing.T, tx *client.Transaction, key func(string) []byte, _ bool) rangeExpectation {
			if err := tx.ClearRange(key("m"), key("m\x00")); err != nil {
				t.Fatal(err)
			}
			tx.Atomic(client.MutAddValue, key("m"), []byte{1})
			return rangeExpectation{
				begin: key("m"), end: key("m\x00"), bytes: client.ByteLimitUnlimited,
				rows: []client.KeyValue{{Key: key("m"), Value: []byte{1}}},
			}
		}},
		{"compare_clear_phantom", func(_ *testing.T, tx *client.Transaction, key func(string) []byte, _ bool) rangeExpectation {
			tx.Set(key("m"), []byte{1})
			tx.Atomic(client.MutCompareAndClear, key("m"), []byte{1})
			return rangeExpectation{begin: key("m"), end: key("m\x00"), limit: 1, bytes: client.ByteLimitUnlimited}
		}},
		{"bypass_independent_versionstamp", func(_ *testing.T, tx *client.Transaction, key func(string) []byte, _ bool) rangeExpectation {
			tx.SetBypassUnreadable(true)
			tx.Atomic(client.MutSetVersionstampedValue, key("m"), make([]byte, 14))
			return rangeExpectation{
				begin: key("m"), end: key("m\x00"), bytes: client.ByteLimitUnlimited,
				errorCode: 1036,
			}
		}},
	}
	for _, stop := range []string{"row_limit", "byte_limit", "byte_target_before_unknown", "unlimited_before_unknown"} {
		cases = append(cases, rangeCase{"known_prefix/" + stop, func(_ *testing.T, tx *client.Transaction, key func(string) []byte, reverse bool) rangeExpectation {
			first := "b"
			begin, end := key("b"), key("z")
			if reverse {
				first, begin, end = "x", key("a"), key("x\x00")
			}
			tx.Set(key(first), []byte("local"))
			e := rangeExpectation{
				begin: begin, end: end, bytes: client.ByteLimitUnlimited, more: true,
				rows: []client.KeyValue{{Key: key(first), Value: []byte("local")}},
			}
			switch stop {
			case "row_limit":
				e.limit = 1
			case "byte_limit":
				e.bytes = 8 + len(key(first)) + len("local")
			case "byte_target_before_unknown":
				e.bytes = 1000000
			case "unlimited_before_unknown":
				e.more, e.remote = false, true
				last := "y"
				if reverse {
					last = "a"
				}
				e.rows = append(e.rows, client.KeyValue{Key: key("m"), Value: []byte{1}}, client.KeyValue{Key: key(last), Value: []byte(last)})
			}
			return e
		}})
	}
	for _, stop := range []string{"unlimited", "exact_rows", "exact_bytes", "bytes_one_beyond", "large_bytes"} {
		cases = append(cases, rangeCase{"known_span/" + stop, func(t *testing.T, tx *client.Transaction, key func(string) []byte, reverse bool) rangeExpectation {
			if err := tx.ClearRange(key("a"), key("z")); err != nil {
				t.Fatal(err)
			}
			tx.Set(key("b"), nil)
			tx.Set(key("x"), []byte("two"))
			e := rangeExpectation{
				begin: key("a"), end: key("z"), bytes: client.ByteLimitUnlimited,
				rows: []client.KeyValue{{Key: key("b"), Value: []byte{}}, {Key: key("x"), Value: []byte("two")}},
			}
			if reverse {
				e.rows[0], e.rows[1] = e.rows[1], e.rows[0]
			}
			switch stop {
			case "exact_rows":
				e.limit, e.more = 2, true
			case "exact_bytes":
				e.bytes = 8 + len(e.rows[0].Key) + len(e.rows[0].Value)
				e.rows, e.more = e.rows[:1], true
			case "bytes_one_beyond":
				e.bytes = 8 + len(e.rows[0].Key) + len(e.rows[0].Value) + 1
				e.more = true
			case "large_bytes":
				e.bytes = 1000000
			}
			return e
		}})
	}
	for _, stop := range []string{"row_limit", "byte_limit", "reach_unreadable", "large_bytes_reach_unreadable"} {
		cases = append(cases, rangeCase{"unreadable/" + stop, func(t *testing.T, tx *client.Transaction, key func(string) []byte, reverse bool) rangeExpectation {
			if err := tx.ClearRange(key("a"), key("z")); err != nil {
				t.Fatal(err)
			}
			tx.Set(key("b"), []byte("local"))
			tx.Set(key("x"), []byte("local"))
			tx.Atomic(client.MutSetVersionstampedValue, key("m"), make([]byte, 14))
			first := "b"
			if reverse {
				first = "x"
			}
			e := rangeExpectation{
				begin: key("a"), end: key("z"), bytes: client.ByteLimitUnlimited, more: true,
				rows: []client.KeyValue{{Key: key(first), Value: []byte("local")}},
			}
			switch stop {
			case "row_limit":
				e.limit = 1
			case "byte_limit":
				e.bytes = 8 + len(key(first)) + len("local")
			default:
				e.rows, e.more, e.errorCode = nil, false, 1036
				if stop == "large_bytes_reach_unreadable" {
					e.bytes = 1000000
				}
			}
			return e
		}})
	}
	for _, mode := range []string{"plain", "dependent_atomic", "snapshot_cache_only"} {
		for _, stop := range []string{"tiny_bytes", "exact_bytes", "bytes_one_beyond"} {
			cases = append(cases, rangeCase{"cold_exact_span/" + mode + "/" + stop, func(_ *testing.T, tx *client.Transaction, key func(string) []byte, _ bool) rangeExpectation {
				e := rangeExpectation{
					begin: key("a"), end: key("a\x00"), bytes: 8 + len(key("a")) + 1, more: true, remote: true,
					rows: []client.KeyValue{{Key: key("a"), Value: []byte("a")}},
				}
				switch mode {
				case "dependent_atomic":
					tx.Atomic(client.MutOr, key("a"), []byte{0})
				case "snapshot_cache_only":
					tx.SetSnapshotRYWDisable()
					tx.Set(key("a"), []byte("ignored"))
					e.snapshot = true
				}
				if stop == "tiny_bytes" {
					e.bytes = 1
				} else if stop == "bytes_one_beyond" {
					e.bytes++
					e.more = false
				}
				return e
			}})
		}
	}
	for _, bypass := range []bool{false, true} {
		for _, stop := range []string{"row_limit", "tiny_bytes", "exact_bytes", "bytes_with_row_limit", "bytes_one_beyond", "large_bytes_reach_unreadable"} {
			cases = append(cases, rangeCase{fmt.Sprintf("cold_prefix/bypass=%t/%s", bypass, stop), func(_ *testing.T, tx *client.Transaction, key func(string) []byte, reverse bool) rangeExpectation {
				first, unreadable := "a", "b"
				if reverse {
					first, unreadable = "y", "x"
				}
				tx.SetBypassUnreadable(bypass)
				tx.Atomic(client.MutSetVersionstampedValue, key(unreadable), make([]byte, 14))
				e := rangeExpectation{
					begin: key("a"), end: key("z"), bytes: 8 + len(key(first)) + 1, more: true, remote: true,
					rows: []client.KeyValue{{Key: key(first), Value: []byte(first)}},
				}
				switch stop {
				case "row_limit":
					e.limit, e.bytes = 1, client.ByteLimitUnlimited
				case "tiny_bytes":
					e.bytes = 1
				case "bytes_with_row_limit":
					e.limit = 10
				case "bytes_one_beyond":
					// Storage charges more per row than the client's eight bytes;
					// its truncated reply leaves an unknown gap and permits a soft stop.
					e.bytes++
				case "large_bytes_reach_unreadable":
					e.bytes = 1000000
					e.rows, e.more, e.errorCode = nil, false, 1036
				}
				return e
			}})
		}
	}
	return cases
}

func rangeCluster(t *testing.T) *client.ClusterFile {
	t.Helper()
	setup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730), foundationdbtc.WithDirectIP())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			t.Error(err)
		}
	})
	cluster, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := client.ParseClusterString(cluster)
	if err != nil {
		t.Fatal(err)
	}
	return cf
}

func TestRangeLocalSegments(t *testing.T) {
	t.Parallel()
	cf := rangeCluster(t)
	for _, tc := range localRangeCases() {
		for _, reverse := range []bool{false, true} {
			for _, ancient := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/reverse=%t/ancient=%t", tc.name, reverse, ancient), func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					db, err := client.OpenDatabaseFromConfig(ctx, cf, client.WithAPIVersion(730))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					key := func(s string) []byte { return []byte(t.Name() + "/" + s) }
					_, err = db.Transact(ctx, func(tx *client.Transaction) (any, error) {
						tx.Set(key("a"), []byte("a"))
						tx.Set(key("m"), []byte{1})
						tx.Set(key("y"), []byte("y"))
						return nil, nil
					})
					if err != nil {
						t.Fatal(err)
					}
					tx := db.CreateTransaction()
					defer tx.Cancel()
					want := tc.setup(t, tx, key, reverse)
					if ancient {
						tx.SetReadVersion(1)
						if want.remote {
							want.errorCode = 1007
						}
					}
					before := db.Metrics()
					readRange := tx.GetRangeWithByteTarget
					if want.snapshot {
						readRange = tx.Snapshot().GetRangeWithByteTarget
					}
					rows, more, err := readRange(ctx, want.begin, want.end, want.limit, want.bytes, reverse)
					if want.errorCode != 0 {
						var fdbErr *wire.FDBError
						if !errors.As(err, &fdbErr) || fdbErr.Code != want.errorCode {
							t.Fatalf("range error=%v, want FDB%d", err, want.errorCode)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if len(rows) != len(want.rows) || more != want.more {
							t.Fatalf("rows=%v more=%t, want %v more=%t", rows, more, want.rows, want.more)
						}
						for i := range rows {
							if !bytes.Equal(rows[i].Key, want.rows[i].Key) || !bytes.Equal(rows[i].Value, want.rows[i].Value) {
								t.Fatalf("row %d=%q:%x, want %q:%x", i, rows[i].Key, rows[i].Value, want.rows[i].Key, want.rows[i].Value)
							}
						}
					}
					after := db.Metrics()
					wantGRV := int64(0)
					if want.remote && !ancient {
						wantGRV = 1
					}
					if got := after.TransactionReadVersionsCompleted - before.TransactionReadVersionsCompleted; got != wantGRV {
						t.Errorf("LOCAL_RANGE_GRV: acquired %d versions, want %d", got, wantGRV)
					}
					if after.GRVCacheHits != before.GRVCacheHits {
						t.Error("range used a cached GRV")
					}
					if !want.remote && !ancient {
						if _, ok := tx.ReadVersionInstant(); ok {
							t.Error("local range established an MVCC anchor")
						}
					}
				})
			}
		}
	}
}

func TestLocalRangePreservesDependentAtomicConflict(t *testing.T) {
	t.Parallel()
	cf := rangeCluster(t)
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			db, err := client.OpenDatabaseFromConfig(ctx, cf, client.WithAPIVersion(730))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			key := []byte(t.Name())
			_, err = db.Transact(ctx, func(tx *client.Transaction) (any, error) { tx.Set(key, []byte{1}); return nil, nil })
			if err != nil {
				t.Fatal(err)
			}
			tx := db.CreateTransaction()
			defer tx.Cancel()
			if value, err := tx.Snapshot().Get(ctx, key); err != nil || !bytes.Equal(value, []byte{1}) {
				t.Fatalf("snapshot warmup=%x, %v", value, err)
			}
			tx.Atomic(client.MutAddValue, key, []byte{1})
			rows, more, err := tx.GetRangeWithByteTarget(ctx, key, append(bytes.Clone(key), 0), 0, client.ByteLimitUnlimited, reverse)
			if err != nil || more || len(rows) != 1 || !bytes.Equal(rows[0].Value, []byte{2}) {
				t.Fatalf("cached-base range=%v more=%t err=%v", rows, more, err)
			}
			_, err = db.Transact(ctx, func(writer *client.Transaction) (any, error) { writer.Set(key, []byte{9}); return nil, nil })
			if err != nil {
				t.Fatal(err)
			}
			err = tx.Commit(ctx)
			var fdbErr *wire.FDBError
			if !errors.As(err, &fdbErr) || fdbErr.Code != 1020 {
				t.Fatalf("dependent atomic lost its range read conflict: %v, want FDB1020", err)
			}
		})
	}
}
