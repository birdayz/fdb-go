package fdb

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func apiVersionCode(err error) int {
	var fe Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return -1
}

// TestSelectAPIVersionBounds pins APIVersion against libfdb_c + the Apple binding:
// unsupported versions are api_version_not_supported (2203), a different version
// after selection is api_version_already_set (2201) and that check comes first.
func TestSelectAPIVersionBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		set, version, want int // want 0 = accepted
	}{
		{0, -1, 2203},
		{0, 0, 2203},
		{0, 13, 2203},  // libfdb_c's own floor (noBackwardsCompatibility)
		{0, 200, 2203}, // the Apple binding's floor
		{0, 509, 2203},
		{0, 510, 0},
		{0, 730, 0},
		{0, 731, 2203},
		{0, 1 << 31, 2203},
		{0, 1<<32 + 730, 2203}, // must not wrap to 730 through int32
		{730, 730, 0},
		{730, 731, 2201},
		{730, 509, 2201},
		{730, 510, 2201},
		{510, 730, 2201},
	} {
		t.Run(fmt.Sprintf("set%d/select%d", tc.set, tc.version), func(t *testing.T) {
			t.Parallel()
			var cur atomic.Int32
			cur.Store(int32(tc.set))
			err := selectAPIVersion(&cur, tc.version)
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("selectAPIVersion(%d) with %d set = %v, want nil", tc.version, tc.set, err)
				}
				if got := cur.Load(); int(got) != tc.version {
					t.Fatalf("selected version = %d, want %d", got, tc.version)
				}
				return
			}
			if got := apiVersionCode(err); got != tc.want {
				t.Fatalf("selectAPIVersion(%d) with %d set = %v, want code %d", tc.version, tc.set, err, tc.want)
			}
			if got := cur.Load(); int(got) != tc.set {
				t.Fatalf("rejected select changed state: %d, want %d", got, tc.set)
			}
		})
	}
}

// TestSelectAPIVersionRace: concurrent selections of different versions settle on
// exactly one; every caller of that version succeeds and every other gets 2201.
func TestSelectAPIVersionRace(t *testing.T) {
	t.Parallel()
	var cur atomic.Int32
	const n = 64
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = selectAPIVersion(&cur, 710+10*(i%3))
		}()
	}
	wg.Wait()
	won := int(cur.Load())
	if won != 710 && won != 720 && won != 730 {
		t.Fatalf("selected version = %d", won)
	}
	for i, err := range errs {
		v := 710 + 10*(i%3)
		if v == won && err != nil {
			t.Fatalf("select(%d) = %v after %d won", v, err, won)
		}
		if v != won && apiVersionCode(err) != 2201 {
			t.Fatalf("select(%d) = %v after %d won, want 2201", v, err, won)
		}
	}
}

// TestMustAPIVersionPanicsWithAPIVersionError: MustAPIVersion panics with exactly
// APIVersion's error (TestMain selected 730, so 731 is already_set).
func TestMustAPIVersionPanicsWithAPIVersionError(t *testing.T) {
	t.Parallel()
	defer func() {
		if got := apiVersionCode(asError(recover())); got != 2201 {
			t.Fatalf("MustAPIVersion(731) panic code = %d, want 2201", got)
		}
	}()
	MustAPIVersion(731)
}

func asError(v any) error {
	err, _ := v.(error)
	return err
}
