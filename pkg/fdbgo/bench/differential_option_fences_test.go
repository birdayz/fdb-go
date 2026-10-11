package bench

import (
	"fmt"
	"os"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// READ_LOCK_AWARE makes a commit with writes fail 2023 unless LOCK_AWARE is also set
// (NativeAPI.actor.cpp:7071-7091, :6810); SIZE_LIMIT outside [32, 1e7] becomes the deferred
// error 2006 for every later op (:7067-7069). Each case must return libfdb_c's code.

type optionFenceCase struct {
	name string
	want int // libfdb_c's code; both clients must return it
	goFn func(tx gofdb.Transaction) int
	cFn  func(tx cgofdb.Transaction) int
}

func runOptionFenceCases(t *testing.T, cases []optionFenceCase) {
	t.Helper()
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			goCode := goRYWSeq(t, tc.goFn)
			cCode := cgoRYWSeq(t, tc.cFn)
			if goCode != cCode || cCode != tc.want {
				t.Errorf("go=%d cgo=%d, want both %d", goCode, cCode, tc.want)
			}
		})
	}
}

func TestDifferential_ReadLockAwareReadOnly(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("rlaro_%d_", os.Getpid())
	gk := func(s string) gofdb.Key { return gofdb.Key(pfx + s) }
	ck := func(s string) cgofdb.Key { return cgofdb.Key(pfx + s) }
	gCommit := func(tx gofdb.Transaction) int { return fdbErrorCode(tx.Commit().Get()) }
	cCommit := func(tx cgofdb.Transaction) int { return fdbErrorCode(tx.Commit().Get()) }

	runOptionFenceCases(t, []optionFenceCase{
		{
			"set_commit", 2023,
			func(tx gofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				tx.Set(gk("a"), []byte("v"))
				return gCommit(tx)
			},
			func(tx cgofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				tx.Set(ck("a"), []byte("v"))
				return cCommit(tx)
			},
		},
		{
			"write_conflict_only_commit", 2023,
			func(tx gofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				_ = tx.AddWriteConflictKey(gk("b"))
				return gCommit(tx)
			},
			func(tx cgofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				_ = tx.AddWriteConflictKey(ck("b"))
				return cCommit(tx)
			},
		},
		{
			"set_then_get", 0,
			func(tx gofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				tx.Set(gk("c"), []byte("v"))
				_, e := tx.Get(gk("c")).Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				tx.Set(ck("c"), []byte("v"))
				_, e := tx.Get(ck("c")).Get()
				return fdbErrorCode(e)
			},
		},
		{
			"read_only_commit", 0,
			func(tx gofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				_, _ = tx.Get(gk("d")).Get()
				return gCommit(tx)
			},
			func(tx cgofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				_, _ = tx.Get(ck("d")).Get()
				return cCommit(tx)
			},
		},
		{
			"lock_aware_first_set_commit", 0,
			func(tx gofdb.Transaction) int {
				_ = tx.Options().SetLockAware()
				_ = tx.Options().SetReadLockAware()
				tx.Set(gk("e"), []byte("v"))
				return gCommit(tx)
			},
			func(tx cgofdb.Transaction) int {
				_ = tx.Options().SetLockAware()
				_ = tx.Options().SetReadLockAware()
				tx.Set(ck("e"), []byte("v"))
				return cCommit(tx)
			},
		},
		{
			"lock_aware_after_set_commit", 0,
			func(tx gofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				_ = tx.Options().SetLockAware()
				tx.Set(gk("f"), []byte("v"))
				return gCommit(tx)
			},
			func(tx cgofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				_ = tx.Options().SetLockAware()
				tx.Set(ck("f"), []byte("v"))
				return cCommit(tx)
			},
		},
		{
			"reset_clears", 0,
			func(tx gofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				tx.Reset()
				tx.Set(gk("g"), []byte("v"))
				return gCommit(tx)
			},
			func(tx cgofdb.Transaction) int {
				_ = tx.Options().SetReadLockAware()
				tx.Reset()
				tx.Set(ck("g"), []byte("v"))
				return cCommit(tx)
			},
		},
	})
}

func TestDifferential_SizeLimitOutOfRange(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("szrange_%d_", os.Getpid())
	var cases []optionFenceCase
	for _, bad := range []int64{0, -1, 31, 10_000_001} {
		bad := bad
		gk := gofdb.Key(fmt.Sprintf("%s%d", pfx, bad))
		ck := cgofdb.Key(fmt.Sprintf("%s%d", pfx, bad))
		cases = append(cases,
			optionFenceCase{
				fmt.Sprintf("%d_set_option", bad), 0,
				func(tx gofdb.Transaction) int { return fdbErrorCode(tx.Options().SetSizeLimit(bad)) },
				func(tx cgofdb.Transaction) int { return fdbErrorCode(tx.Options().SetSizeLimit(bad)) },
			},
			optionFenceCase{
				fmt.Sprintf("%d_get", bad), 2006,
				func(tx gofdb.Transaction) int {
					_ = tx.Options().SetSizeLimit(bad)
					_, e := tx.Get(gk).Get()
					return fdbErrorCode(e)
				},
				func(tx cgofdb.Transaction) int {
					_ = tx.Options().SetSizeLimit(bad)
					_, e := tx.Get(ck).Get()
					return fdbErrorCode(e)
				},
			},
			optionFenceCase{
				fmt.Sprintf("%d_grv", bad), 2006,
				func(tx gofdb.Transaction) int {
					_ = tx.Options().SetSizeLimit(bad)
					_, e := tx.GetReadVersion().Get()
					return fdbErrorCode(e)
				},
				func(tx cgofdb.Transaction) int {
					_ = tx.Options().SetSizeLimit(bad)
					_, e := tx.GetReadVersion().Get()
					return fdbErrorCode(e)
				},
			},
			optionFenceCase{
				fmt.Sprintf("%d_set_commit", bad), 2006,
				func(tx gofdb.Transaction) int {
					_ = tx.Options().SetSizeLimit(bad)
					tx.Set(gk, []byte("v"))
					return fdbErrorCode(tx.Commit().Get())
				},
				func(tx cgofdb.Transaction) int {
					_ = tx.Options().SetSizeLimit(bad)
					tx.Set(ck, []byte("v"))
					return fdbErrorCode(tx.Commit().Get())
				},
			},
			optionFenceCase{
				fmt.Sprintf("%d_after_valid_commit", bad), 2006,
				func(tx gofdb.Transaction) int {
					_ = tx.Options().SetSizeLimit(1000)
					_ = tx.Options().SetSizeLimit(bad)
					tx.Set(gk, []byte("v"))
					return fdbErrorCode(tx.Commit().Get())
				},
				func(tx cgofdb.Transaction) int {
					_ = tx.Options().SetSizeLimit(1000)
					_ = tx.Options().SetSizeLimit(bad)
					tx.Set(ck, []byte("v"))
					return fdbErrorCode(tx.Commit().Get())
				},
			},
			optionFenceCase{
				fmt.Sprintf("%d_reset_commit", bad), 0,
				func(tx gofdb.Transaction) int {
					_ = tx.Options().SetSizeLimit(bad)
					tx.Reset()
					tx.Set(gk, []byte("v"))
					return fdbErrorCode(tx.Commit().Get())
				},
				func(tx cgofdb.Transaction) int {
					_ = tx.Options().SetSizeLimit(bad)
					tx.Reset()
					tx.Set(ck, []byte("v"))
					return fdbErrorCode(tx.Commit().Get())
				},
			},
		)
	}
	for _, ok := range []int64{32, 10_000_000} {
		ok := ok
		cases = append(cases, optionFenceCase{
			fmt.Sprintf("%d_get", ok), 0,
			func(tx gofdb.Transaction) int {
				_ = tx.Options().SetSizeLimit(ok)
				_, e := tx.Get(gofdb.Key(pfx)).Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				_ = tx.Options().SetSizeLimit(ok)
				_, e := tx.Get(cgofdb.Key(pfx)).Get()
				return fdbErrorCode(e)
			},
		})
	}
	runOptionFenceCases(t, cases)
}
