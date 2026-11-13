//go:build bazelrunfiles

package bench

import (
	"fmt"
	"testing"
	"time"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Pin NativeAPI::setVersion's validation order against the shipped 7.3.77 C
// client. The void setter defers failure; a later read must expose its code.
func TestDifferential_SetReadVersionContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want int
		g    func(gofdb.Transaction) error
		c    func(cgofdb.Transaction) error
	}{
		{"zero", 2011, func(tr gofdb.Transaction) error { tr.SetReadVersion(0); return nil }, func(tr cgofdb.Transaction) error { tr.SetReadVersion(0); return nil }},
		{"negative", 2011, func(tr gofdb.Transaction) error { tr.SetReadVersion(-1); return nil }, func(tr cgofdb.Transaction) error { tr.SetReadVersion(-1); return nil }},
		{"already set", 2010, func(tr gofdb.Transaction) error { tr.SetReadVersion(123); tr.SetReadVersion(456); return nil }, func(tr cgofdb.Transaction) error { tr.SetReadVersion(123); tr.SetReadVersion(456); return nil }},
		{"already set before invalid", 2010, func(tr gofdb.Transaction) error { tr.SetReadVersion(123); tr.SetReadVersion(0); return nil }, func(tr cgofdb.Transaction) error { tr.SetReadVersion(123); tr.SetReadVersion(0); return nil }},
		{"grv before invalid", 2010, func(tr gofdb.Transaction) error {
			if _, err := tr.GetReadVersion().Get(); err != nil {
				return err
			}
			tr.SetReadVersion(-1)
			return nil
		}, func(tr cgofdb.Transaction) error {
			if _, err := tr.GetReadVersion().Get(); err != nil {
				return err
			}
			tr.SetReadVersion(-1)
			return nil
		}},
		{"prior deferred error", 2011, func(tr gofdb.Transaction) error { tr.SetReadVersion(0); tr.SetReadVersion(123); return nil }, func(tr cgofdb.Transaction) error { tr.SetReadVersion(0); tr.SetReadVersion(123); return nil }},
		{"cancel before invalid", 2011, func(tr gofdb.Transaction) error { tr.Cancel(); tr.SetReadVersion(0); return nil }, func(tr cgofdb.Transaction) error { tr.Cancel(); tr.SetReadVersion(0); return nil }},
		{"timeout before invalid", 2011, func(tr gofdb.Transaction) error {
			if err := tr.Options().SetTimeout(1); err != nil {
				return err
			}
			if err := observeReadTimeout(func() error {
				_, err := tr.GetEstimatedRangeSizeBytes(gofdb.KeyRange{Begin: gofdb.Key("a"), End: gofdb.Key("b")}).Get()
				return err
			}, tr.Cancel); err != nil {
				return err
			}
			tr.SetReadVersion(0)
			return nil
		}, func(tr cgofdb.Transaction) error {
			if err := tr.Options().SetTimeout(1); err != nil {
				return err
			}
			if err := observeReadTimeout(func() error {
				_, err := tr.GetEstimatedRangeSizeBytes(cgofdb.KeyRange{Begin: cgofdb.Key("a"), End: cgofdb.Key("b")}).Get()
				return err
			}, tr.Cancel); err != nil {
				return err
			}
			tr.SetReadVersion(0)
			return nil
		}},
		{"reset clears error", 0, func(tr gofdb.Transaction) error { tr.SetReadVersion(0); tr.Reset(); tr.SetReadVersion(123); return nil }, func(tr cgofdb.Transaction) error {
			tr.SetReadVersion(0)
			tr.Reset()
			tr.SetReadVersion(123)
			return nil
		}},
		{"valid", 0, func(tr gofdb.Transaction) error { tr.SetReadVersion(123); return nil }, func(tr cgofdb.Transaction) error { tr.SetReadVersion(123); return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := goClient.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			c, err := cgoClient.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.g(g); err != nil {
				t.Fatalf("Go setup: %v", err)
			}
			if err := tc.c(c); err != nil {
				t.Fatalf("C setup: %v", err)
			}
			gv, ge := g.GetReadVersion().Get()
			cv, ce := c.GetReadVersion().Get()
			if gc, cc := fdbErrorCode(ge), fdbErrorCode(ce); gc != tc.want || cc != tc.want {
				t.Fatalf("GetReadVersion Go=%d(%v) C=%d(%v) want%d", gc, ge, cc, ce, tc.want)
			}
			if tc.want == 0 && (gv != 123 || cv != 123) {
				t.Fatalf("valid version Go=%d C=%d want123", gv, cv)
			}
		})
	}
}

// Non-tenant metrics check resetPromise without admitting a read version, so
// observing timeout does not change the setter's expected invalid-version error.
func observeReadTimeout(read func() error, cancel func()) error {
	stop := time.AfterFunc(5*time.Second, cancel)
	defer stop.Stop()
	for {
		err := read()
		switch fdbErrorCode(err) {
		case 1031:
			return nil
		case 0:
			time.Sleep(time.Millisecond)
		default:
			return fmt.Errorf("observe timeout before invalid setter: %v", err)
		}
	}
}

func TestDifferential_OnErrorRejectsPostPinnedVersionCodes(t *testing.T) {
	t.Parallel()
	for _, code := range []int{1235, 1242} {
		g, err := goClient.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		c, err := cgoClient.CreateTransaction()
		if err != nil {
			t.Fatal(err)
		}
		ge := g.OnError(gofdb.Error{Code: code}).Get()
		ce := c.OnError(cgofdb.Error{Code: code}).Get()
		if gc, cc := fdbErrorCode(ge), fdbErrorCode(ce); gc != code || cc != code {
			t.Fatalf("OnError(%d) Go=%d(%v) C=%d(%v)", code, gc, ge, cc, ce)
		}
	}
}
