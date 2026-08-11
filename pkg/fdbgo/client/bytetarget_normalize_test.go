package client

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// TestNormalizeByteTarget pins the C API's boundary normalization for a per-fetch byte target
// (fdb_c.cpp:993, "Zero at the C API maps to infinity at lower levels").
//
// Zero is the case that matters and the one a caller is most likely to supply, because it is
// the Go zero value: an unset int reaching an exported GetRangeWithByteTarget would otherwise
// travel to the wire as LimitBytes: 0, a request for a reply that can hold nothing. C++ never
// builds such a request — it asserts a positive byte limit on every range request
// (ASSERT(req.limitBytes > 0), NativeAPI.actor.cpp:4300 and :4682) — so the normalization has
// to happen at the exported boundary, before anything downstream can see the zero.
//
// The rejection arm is separate on purpose: a value below BYTE_LIMIT_UNLIMITED is INVALID, not
// something to clamp, exactly as a row limit < -1 is range_limits_invalid rather than being
// silently normalized to unlimited. Clamping it would turn a caller's mistake into a silent
// full-speed scan.
func TestNormalizeByteTarget(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name    string
		in      int
		want    int
		wantErr bool
	}{
		{"zero_becomes_unlimited", 0, ByteLimitUnlimited, false},
		{"unlimited_passes_through", ByteLimitUnlimited, ByteLimitUnlimited, false},
		{"positive_passes_through", 4096, 4096, false},
		{"one_passes_through", 1, 1, false},
		{"below_unlimited_is_invalid", -2, 0, true},
		{"far_below_is_invalid", -99999, 0, true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeByteTarget(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("normalizeByteTarget(%d) = %d, nil; want range_limits_invalid — a "+
						"byte target below BYTE_LIMIT_UNLIMITED is invalid, not something to clamp",
						c.in, got)
				}
				var fe *wire.FDBError
				if !errors.As(err, &fe) || fe.Code != ErrRangeLimitsInvalid {
					t.Fatalf("normalizeByteTarget(%d) error = %v; want FDBError{Code: %d}",
						c.in, err, ErrRangeLimitsInvalid)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeByteTarget(%d): unexpected error %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("normalizeByteTarget(%d) = %d, want %d. Zero must become "+
					"ByteLimitUnlimited: forwarded as-is it reaches the wire as LimitBytes: 0, "+
					"which C++ never emits (it asserts req.limitBytes > 0)", c.in, got, c.want)
			}
		})
	}
}
