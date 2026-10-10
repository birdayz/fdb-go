package bench

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/wire"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// ThreadSafeTransaction.cpp (7.3.77) copies mutation arguments before returning.
// Apple's futures.go copies read results per future, not per Get on one future.
// These adapters deliberately retain the returned byte slices: normGo/normC would
// copy them and hide the ownership bug before the test can mutate them.
type ownershipTxn struct {
	set        func([]byte, []byte)
	clear      func([]byte)
	clearRange func([]byte, []byte) error
	add        func([]byte, []byte)
	get        func([]byte, bool) ([]byte, error)
	getKey     func([]byte, bool) ([]byte, error)
	getRange   func([]byte, []byte, int, bool, bool) ([]kvPair, error)
	pin        func(int64)
	disableRYW func() error
	commit     func() error
	cancel     func()
}

var ownershipSequence atomic.Uint64

func ownershipPrefix(t *testing.T) []byte {
	t.Helper()
	prefix := []byte(fmt.Sprintf("ownership/%d/%d/%s/", os.Getpid(), ownershipSequence.Add(1), t.Name()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		tx := ownershipOpen(t, ctx, "cgo")
		defer tx.cancel()
		if err := tx.clearRange(prefix, ownershipKey(prefix, 0xff)); err != nil {
			t.Errorf("ownership cleanup clear: %v", err)
			return
		}
		if err := tx.commit(); err != nil {
			t.Errorf("ownership cleanup commit: %v", err)
		}
	})
	return prefix
}

func ownershipKey(prefix []byte, suffix byte) []byte {
	return append(bytes.Clone(prefix), suffix)
}

func ownershipOpen(t *testing.T, ctx context.Context, name string) ownershipTxn {
	t.Helper()
	switch name {
	case "raw-go":
		tx := goRawClient.CreateTransaction()
		tx.SetTimeout(10000)
		return ownershipTxn{
			set: tx.Set, clear: tx.Clear, clearRange: tx.ClearRange,
			add: func(k, v []byte) { tx.Atomic(client.MutAddValue, k, v) },
			get: func(k []byte, snapshot bool) ([]byte, error) {
				if snapshot {
					return tx.Snapshot().Get(ctx, k)
				}
				return tx.Get(ctx, k)
			},
			getKey: func(k []byte, snapshot bool) ([]byte, error) {
				if snapshot {
					return tx.Snapshot().GetKey(ctx, k, false, 1)
				}
				return tx.GetKey(ctx, k, false, 1)
			},
			getRange: func(b, e []byte, limit int, reverse, snapshot bool) ([]kvPair, error) {
				var rows []client.KeyValue
				var err error
				if snapshot {
					rows, _, err = tx.Snapshot().GetRangeWithByteTarget(ctx, b, e, limit, 0, reverse)
				} else {
					rows, _, err = tx.GetRangeWithByteTarget(ctx, b, e, limit, 0, reverse)
				}
				out := make([]kvPair, len(rows))
				for i, row := range rows {
					out[i] = kvPair{row.Key, row.Value}
				}
				return out, err
			},
			pin:        tx.SetReadVersion,
			disableRYW: func() error { tx.SetReadYourWritesDisable(); return nil },
			commit:     func() error { return tx.Commit(ctx) }, cancel: tx.Cancel,
		}
	case "facade-go":
		tx, err := goClient.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Options().SetTimeout(10000); err != nil {
			tx.Cancel()
			t.Fatal(err)
		}
		return ownershipTxn{
			set:   func(k, v []byte) { tx.Set(gofdb.Key(k), v) },
			clear: func(k []byte) { tx.Clear(gofdb.Key(k)) },
			clearRange: func(b, e []byte) error {
				tx.ClearRange(gofdb.KeyRange{Begin: gofdb.Key(b), End: gofdb.Key(e)})
				return nil
			},
			add: func(k, v []byte) { tx.Add(gofdb.Key(k), v) },
			get: func(k []byte, snapshot bool) ([]byte, error) {
				if snapshot {
					return tx.Snapshot().Get(gofdb.Key(k)).Get()
				}
				return tx.Get(gofdb.Key(k)).Get()
			},
			getKey: func(k []byte, snapshot bool) ([]byte, error) {
				s := gofdb.FirstGreaterOrEqual(gofdb.Key(k))
				if snapshot {
					return tx.Snapshot().GetKey(s).Get()
				}
				return tx.GetKey(s).Get()
			},
			getRange: func(b, e []byte, limit int, reverse, snapshot bool) ([]kvPair, error) {
				r := gofdb.KeyRange{Begin: gofdb.Key(b), End: gofdb.Key(e)}
				opts := gofdb.RangeOptions{Limit: limit, Reverse: reverse}
				var rows []gofdb.KeyValue
				var err error
				if snapshot {
					rows, err = tx.Snapshot().GetRange(r, opts).GetSliceWithError()
				} else {
					rows, err = tx.GetRange(r, opts).GetSliceWithError()
				}
				out := make([]kvPair, len(rows))
				for i, row := range rows {
					out[i] = kvPair{row.Key, row.Value}
				}
				return out, err
			},
			pin: tx.SetReadVersion, disableRYW: tx.Options().SetReadYourWritesDisable,
			commit: func() error { return tx.Commit().Get() }, cancel: tx.Cancel,
		}
	case "cgo":
		tx, err := cgoClient.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Options().SetTimeout(10000); err != nil {
			tx.Cancel()
			t.Fatal(err)
		}
		return ownershipTxn{
			set:   func(k, v []byte) { tx.Set(cgofdb.Key(k), v) },
			clear: func(k []byte) { tx.Clear(cgofdb.Key(k)) },
			clearRange: func(b, e []byte) error {
				tx.ClearRange(cgofdb.KeyRange{Begin: cgofdb.Key(b), End: cgofdb.Key(e)})
				return nil
			},
			add: func(k, v []byte) { tx.Add(cgofdb.Key(k), v) },
			get: func(k []byte, snapshot bool) ([]byte, error) {
				if snapshot {
					return tx.Snapshot().Get(cgofdb.Key(k)).Get()
				}
				return tx.Get(cgofdb.Key(k)).Get()
			},
			getKey: func(k []byte, snapshot bool) ([]byte, error) {
				s := cgofdb.FirstGreaterOrEqual(cgofdb.Key(k))
				if snapshot {
					return tx.Snapshot().GetKey(s).Get()
				}
				return tx.GetKey(s).Get()
			},
			getRange: func(b, e []byte, limit int, reverse, snapshot bool) ([]kvPair, error) {
				r := cgofdb.KeyRange{Begin: cgofdb.Key(b), End: cgofdb.Key(e)}
				opts := cgofdb.RangeOptions{Limit: limit, Reverse: reverse, Mode: cgofdb.StreamingModeWantAll}
				var rows []cgofdb.KeyValue
				var err error
				if snapshot {
					rows, err = tx.Snapshot().GetRange(r, opts).GetSliceWithError()
				} else {
					rows, err = tx.GetRange(r, opts).GetSliceWithError()
				}
				out := make([]kvPair, len(rows))
				for i, row := range rows {
					out[i] = kvPair{row.Key, row.Value}
				}
				return out, err
			},
			pin: tx.SetReadVersion, disableRYW: tx.Options().SetReadYourWritesDisable,
			commit: func() error { return tx.Commit().Get() }, cancel: tx.Cancel,
		}
	default:
		t.Fatalf("unknown ownership client %q", name)
		return ownershipTxn{}
	}
}

func ownershipVersion(t *testing.T) int64 {
	t.Helper()
	tx, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Cancel()
	if err := tx.Options().SetTimeout(10000); err != nil {
		t.Fatal(err)
	}
	v, err := tx.GetReadVersion().Get()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func ownershipSeed(t *testing.T, ctx context.Context, prefix []byte, values [][]byte) {
	t.Helper()
	tx := ownershipOpen(t, ctx, "cgo")
	defer tx.cancel()
	for i, v := range values {
		tx.set(ownershipKey(prefix, 'a'+byte(i)), v)
	}
	if err := tx.commit(); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// Freeze observations only AFTER mutating the live result and making new reads.
func ownershipRows(prefix []byte, rows []kvPair) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = fmt.Sprintf("%x=%x", bytes.TrimPrefix(row.k, prefix), row.v)
	}
	return out
}

func ownershipWant(prefix []byte, values [][]byte) []string {
	var rows []kvPair
	for i, v := range values {
		if v != nil {
			rows = append(rows, kvPair{ownershipKey(prefix, 'a'+byte(i)), v})
		}
	}
	return ownershipRows(prefix, rows)
}

func ownershipEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %v; want %v", label, got, want)
	}
}

func ownershipNumber(n uint64) []byte {
	v := make([]byte, 8)
	binary.LittleEndian.PutUint64(v, n)
	return v
}

func TestDifferential_MutationArgumentOwnership(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		apply func(ownershipTxn, []byte) error
		want  [][]byte
	}{
		{"set_key", func(tx ownershipTxn, p []byte) error {
			k := ownershipKey(p, 'a')
			tx.set(k, []byte("owned"))
			k[len(k)-1] = 'b'
			return nil
		}, [][]byte{[]byte("owned"), ownershipNumber(11), ownershipNumber(12), ownershipNumber(13)}},
		{"set_value", func(tx ownershipTxn, p []byte) error {
			v := []byte("owned")
			tx.set(ownershipKey(p, 'a'), v)
			copy(v, "wrong")
			return nil
		}, [][]byte{[]byte("owned"), ownershipNumber(11), ownershipNumber(12), ownershipNumber(13)}},
		{"clear_key", func(tx ownershipTxn, p []byte) error {
			k := ownershipKey(p, 'b')
			tx.clear(k)
			// Keep the aliased range legal: [a,b\x00) wrongly clears a too.
			k[len(k)-1] = 'a'
			return nil
		}, [][]byte{ownershipNumber(10), nil, ownershipNumber(12), ownershipNumber(13)}},
		{"clear_range_begin", func(tx ownershipTxn, p []byte) error {
			b, e := ownershipKey(p, 'b'), ownershipKey(p, 'd')
			err := tx.clearRange(b, e)
			b[len(b)-1] = 'c'
			return err
		}, [][]byte{ownershipNumber(10), nil, nil, ownershipNumber(13)}},
		{"clear_range_end", func(tx ownershipTxn, p []byte) error {
			b, e := ownershipKey(p, 'b'), ownershipKey(p, 'd')
			err := tx.clearRange(b, e)
			e[len(e)-1] = 'c'
			return err
		}, [][]byte{ownershipNumber(10), nil, nil, ownershipNumber(13)}},
		{"add_key", func(tx ownershipTxn, p []byte) error {
			k := ownershipKey(p, 'a')
			tx.add(k, ownershipNumber(3))
			k[len(k)-1] = 'b'
			return nil
		}, [][]byte{ownershipNumber(13), ownershipNumber(11), ownershipNumber(12), ownershipNumber(13)}},
		{"add_operand", func(tx ownershipTxn, p []byte) error {
			v := ownershipNumber(3)
			tx.add(ownershipKey(p, 'a'), v)
			v[0] = 9
			return nil
		}, [][]byte{ownershipNumber(13), ownershipNumber(11), ownershipNumber(12), ownershipNumber(13)}},
	}
	for _, tc := range cases {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ryw_disabled=%t", tc.name, disabled), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				prefixes := make(map[string][]byte)
				seed := [][]byte{ownershipNumber(10), ownershipNumber(11), ownershipNumber(12), ownershipNumber(13)}
				for _, name := range []string{"cgo", "raw-go", "facade-go"} {
					p := ownershipPrefix(t)
					prefixes[name] = p
					ownershipSeed(t, ctx, p, seed)
				}
				ownershipCompare(t, ctx, func(name string, version int64) ([]string, error) {
					p := prefixes[name]
					tx := ownershipOpen(t, ctx, name)
					defer tx.cancel()
					want := tc.want
					if disabled {
						if err := tx.disableRYW(); err != nil {
							return nil, err
						}
						want = seed
					}
					tx.pin(version)
					if err := tc.apply(tx, p); err != nil {
						return nil, err
					}
					rows, err := tx.getRange(p, ownershipKey(p, 0xff), 0, false, false)
					if err != nil {
						return nil, err
					}
					got := ownershipRows(p, rows)
					ownershipEqual(t, name+" uncommitted", got, ownershipWant(p, want))
					return got, nil
				})
				for _, name := range []string{"cgo", "raw-go", "facade-go"} {
					p := prefixes[name]
					func() {
						tx := ownershipOpen(t, ctx, name)
						defer tx.cancel()
						if disabled {
							if err := tx.disableRYW(); err != nil {
								t.Fatal(err)
							}
						}
						if err := tc.apply(tx, p); err != nil {
							t.Fatalf("%s mutation: %v", name, err)
						}
						// Commit without reading: a RYW fold must not repair an aliased operand.
						if err := tx.commit(); err != nil {
							t.Fatalf("%s commit: %v", name, err)
						}
					}()
				}
				ownershipCompare(t, ctx, func(name string, version int64) ([]string, error) {
					p := prefixes[name]
					tx := ownershipOpen(t, ctx, name)
					defer tx.cancel()
					tx.pin(version)
					rows, err := tx.getRange(p, ownershipKey(p, 0xff), 0, false, false)
					if err != nil {
						return nil, err
					}
					got := ownershipRows(p, rows)
					ownershipEqual(t, name+" persisted", got, ownershipWant(p, tc.want))
					// The C reader independently verifies every writer, including raw Go.
					ref := ownershipOpen(t, ctx, "cgo")
					defer ref.cancel()
					ref.pin(version)
					rows, err = ref.getRange(p, ownershipKey(p, 0xff), 0, false, false)
					if err != nil {
						return nil, err
					}
					ownershipEqual(t, name+" persisted via C", ownershipRows(p, rows), ownershipWant(p, tc.want))
					return got, nil
				})
			})
		}
	}
}

// A stale shared pin restarts the entire comparison, never just one client.
func ownershipCompare(t *testing.T, ctx context.Context, run func(string, int64) ([]string, error)) {
	t.Helper()
	for attempt := 0; attempt < 8 && ctx.Err() == nil; attempt++ {
		version := ownershipVersion(t)
		var reference []string
		retry := false
		for _, name := range []string{"cgo", "raw-go", "facade-go"} {
			got, err := run(name, version)
			if err != nil {
				var rawError *wire.FDBError
				if isFDBRetryable(err) || (errors.As(err, &rawError) && gofdb.IsRetryable(int(rawError.Code))) {
					retry = true
					break
				}
				t.Fatalf("%s ownership probe: %v", name, err)
			}
			if name == "cgo" {
				reference = got
			} else {
				ownershipEqual(t, name+" vs cgo", got, reference)
			}
		}
		if !retry {
			return
		}
	}
	t.Fatalf("ownership comparison exhausted fresh-version attempts: %v", ctx.Err())
}

func TestDifferential_ReadResultOwnership(t *testing.T) {
	t.Parallel()
	for _, local := range []bool{false, true} {
		for _, snapshot := range []bool{false, true} {
			for _, operation := range []string{"get", "get_key", "range_key_forward", "range_value_forward", "range_key_reverse", "range_value_reverse"} {
				t.Run(fmt.Sprintf("local=%t/snapshot=%t/%s", local, snapshot, operation), func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					p := ownershipPrefix(t)
					seed := [][]byte{[]byte("server-a"), []byte("server-b"), []byte("server-c"), []byte("server-d")}
					ownershipSeed(t, ctx, p, seed)
					want := seed
					if local {
						want = [][]byte{[]byte("local-a"), []byte("local-b"), []byte("local-c"), []byte("local-d")}
					}
					ownershipCompare(t, ctx, func(name string, version int64) ([]string, error) {
						tx := ownershipOpen(t, ctx, name)
						defer tx.cancel()
						tx.pin(version)
						if local {
							for i, v := range want {
								tx.set(ownershipKey(p, 'a'+byte(i)), bytes.Clone(v))
							}
						}
						return ownershipReadProbe(t, tx, name, p, want, operation, snapshot)
					})
				})
			}
		}
	}
}

func ownershipReadProbe(t *testing.T, tx ownershipTxn, name string, p []byte, want [][]byte, operation string, snapshot bool) ([]string, error) {
	t.Helper()
	key := ownershipKey(p, 'a')
	index := 0
	switch operation {
	case "get":
		v, err := tx.get(key, snapshot)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(v, want[0]) {
			t.Fatalf("%s initial Get: %x, want %x", name, v, want[0])
		}
		v[0] = '!'
	case "get_key":
		k, err := tx.getKey(key, snapshot)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(k, key) {
			t.Fatalf("%s initial GetKey: %x, want %x", name, k, key)
		}
		k[len(k)-1] = 'x'
	default:
		reverse := operation == "range_key_reverse" || operation == "range_value_reverse"
		rows, err := tx.getRange(p, ownershipKey(p, 0xff), 2, reverse, snapshot)
		if err != nil {
			return nil, err
		}
		if len(rows) != 2 {
			t.Fatalf("%s initial limited range: %d rows, want 2", name, len(rows))
		}
		first, second, affected := 0, 1, 0
		if reverse {
			first, second, affected = 3, 2, 1
			index = 2
		}
		initial := []kvPair{{ownershipKey(p, 'a'+byte(first)), want[first]}, {ownershipKey(p, 'a'+byte(second)), want[second]}}
		ownershipEqual(t, name+" initial range", ownershipRows(p, rows), ownershipRows(p, initial))
		key = ownershipKey(p, 'a'+byte(index))
		// A limited reverse fetch uses its last (smallest) key as the cache's begin boundary.
		row := rows[affected]
		if operation == "range_key_forward" || operation == "range_key_reverse" {
			if len(row.k) == 0 {
				t.Fatal("empty range key")
			}
			row.k[len(row.k)-1] = 'x'
		} else {
			if len(row.v) == 0 {
				t.Fatal("empty range value")
			}
			row.v[0] = '!'
		}
	}
	// Each call constructs a new future; a single future may memoize mutable bytes.
	v, err := tx.get(key, snapshot)
	if err != nil {
		return nil, err
	}
	point := fmt.Sprintf("%x", v)
	ownershipEqual(t, name+" independent Get", []string{point}, []string{fmt.Sprintf("%x", want[index])})
	k, err := tx.getKey(key, snapshot)
	if err != nil {
		return nil, err
	}
	selector := fmt.Sprintf("%x", bytes.TrimPrefix(k, p))
	ownershipEqual(t, name+" independent GetKey", []string{selector}, []string{fmt.Sprintf("%x", []byte{'a' + byte(index)})})
	rows, err := tx.getRange(p, ownershipKey(p, 0xff), 0, false, snapshot)
	if err != nil {
		return nil, err
	}
	got := ownershipRows(p, rows)
	ownershipEqual(t, name+" independent range", got, ownershipWant(p, want))
	return append([]string{point, selector}, got...), nil
}
