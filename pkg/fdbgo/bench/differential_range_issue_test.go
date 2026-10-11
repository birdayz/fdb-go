package bench

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// rangeIssueTxn is one facade transaction under test; GetRange returns a
// function that drains the RangeResult later.
type rangeIssueTxn struct {
	set      func(k, v []byte)
	clear    func(k []byte)
	getRange func(begin, end []byte) func() ([]kvPair, error)
	pinRV    func()
	commit   func() int
	cancel   func()
}

func rangeIssueOpen(t *testing.T, name string) rangeIssueTxn {
	t.Helper()
	switch name {
	case "go":
		tx, err := goClient.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		return rangeIssueTxn{
			set: func(k, v []byte) { tx.Set(gofdb.Key(k), v) }, clear: func(k []byte) { tx.Clear(gofdb.Key(k)) },
			pinRV: func() {
				if _, err := tx.GetReadVersion().Get(); err != nil {
					t.Fatal(err)
				}
			},
			getRange: func(b, e []byte) func() ([]kvPair, error) {
				rr := tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key(b), End: gofdb.Key(e)}, gofdb.RangeOptions{})
				return func() ([]kvPair, error) {
					kvs, err := rr.GetSliceWithError()
					return normGo(kvs), err
				}
			},
			commit: func() int {
				var fe gofdb.Error
				if err := tx.Commit().Get(); errors.As(err, &fe) {
					return fe.Code
				} else if err != nil {
					t.Fatalf("go commit: %v", err)
				}
				return 0
			},
			cancel: tx.Cancel,
		}
	case "cgo":
		tx, err := cgoClient.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		return rangeIssueTxn{
			set: func(k, v []byte) { tx.Set(cgofdb.Key(k), v) }, clear: func(k []byte) { tx.Clear(cgofdb.Key(k)) },
			pinRV: func() {
				if _, err := tx.GetReadVersion().Get(); err != nil {
					t.Fatal(err)
				}
			},
			getRange: func(b, e []byte) func() ([]kvPair, error) {
				rr := tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(b), End: cgofdb.Key(e)}, cgofdb.RangeOptions{})
				return func() ([]kvPair, error) {
					kvs, err := rr.GetSliceWithError()
					return normC(kvs), err
				}
			},
			commit: func() int {
				var fe cgofdb.Error
				if err := tx.Commit().Get(); errors.As(err, &fe) {
					return fe.Code
				} else if err != nil {
					t.Fatalf("cgo commit: %v", err)
				}
				return 0
			},
			cancel: tx.Cancel,
		}
	}
	t.Fatalf("unknown client %q", name)
	return rangeIssueTxn{}
}

func rangeIssueWrite(t *testing.T, name string, k, v []byte) {
	t.Helper()
	w := rangeIssueOpen(t, name)
	defer w.cancel()
	w.set(k, v)
	if code := w.commit(); code != 0 {
		t.Fatalf("%s seed commit: %d", name, code)
	}
}

// GetRange issues its first batch at the call (Apple transaction.go:295-296), so the
// rows reflect the write map then (ReadYourWrites.actor.cpp:382), and an unconsumed
// read still conflicts and is waited for by commit (:1359-1360, :1767).
func TestDifferential_GetRangeIssuesAtCall(t *testing.T) {
	t.Parallel()
	results := map[string]string{}
	for _, name := range []string{"go", "cgo"} {
		prefix := ownershipPrefix(t)
		k0, k1, k5, end := ownershipKey(prefix, '0'), ownershipKey(prefix, '1'), ownershipKey(prefix, '5'), ownershipKey(prefix, 0xff)
		rangeIssueWrite(t, name, k0, []byte("a"))
		rangeIssueWrite(t, name, k1, []byte("b"))

		tx := rangeIssueOpen(t, name)
		drain := tx.getRange(prefix, end)
		tx.set(k5, []byte("late"))
		tx.clear(k0)
		rows, err := drain()
		tx.cancel()

		// The unconsumed read: a concurrent write to its range fails the commit.
		tx = rangeIssueOpen(t, name)
		tx.pinRV() // the read's GRV is asynchronous in both clients
		tx.getRange(k1, ownershipKey(prefix, '2'))
		rangeIssueWrite(t, name, k1, []byte("changed"))
		tx.set(ownershipKey(prefix, 'w'), []byte("x"))
		code := tx.commit()
		tx.cancel()

		var got []string
		for _, kv := range rows {
			got = append(got, string(bytes.TrimPrefix(kv.k, prefix))+"="+string(kv.v))
		}
		results[name] = fmt.Sprintf("rows=%q err=%v commit=%d", got, err, code)
	}
	if !reflect.DeepEqual(results["go"], results["cgo"]) {
		t.Fatalf("pure-Go and libfdb_c diverge:\n  go:  %s\n  cgo: %s", results["go"], results["cgo"])
	}
	t.Logf("RANGE-ISSUE %s", results["cgo"])
}
