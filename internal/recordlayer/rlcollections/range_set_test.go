package rlcollections

import (
	"bytes"
	"errors"
	"testing"
	"unicode/utf8"
)

func TestRangeSetCheckKeyAndHelpers(t *testing.T) {
	t.Parallel()
	var emptyErr *RangeSetEmptyKeyError
	if err := rangeSetCheckKey(nil); !errors.As(err, &emptyErr) {
		t.Fatalf("nil key: got %v, want RangeSetEmptyKeyError", err)
	}
	if err := rangeSetCheckKey([]byte{}); !errors.As(err, &emptyErr) {
		t.Fatalf("empty key: got %v, want RangeSetEmptyKeyError", err)
	}
	var tooLargeErr *RangeSetKeyTooLargeError
	err := rangeSetCheckKey([]byte{0xff})
	if !errors.As(err, &tooLargeErr) {
		t.Fatalf("0xff key: got %v, want RangeSetKeyTooLargeError", err)
	}
	if !utf8.ValidString(err.Error()) {
		t.Fatalf("0xff error string is not valid UTF-8: %q", err.Error())
	}
	if want := `rangeset: key ff must be less than \xff`; err.Error() != want {
		t.Fatalf("0xff error string: got %q, want %q", err.Error(), want)
	}
	if err := rangeSetCheckKey([]byte{0xff, 0x00}); !errors.As(err, &tooLargeErr) {
		t.Fatalf("0xff00 key: got %v, want RangeSetKeyTooLargeError", err)
	}
	if err := rangeSetCheckKey([]byte{0xfe, 0xff}); err != nil {
		t.Fatalf("0xfeff key: unexpected error %v", err)
	}
	if got := rangeSetKeyAfter([]byte{0x01, 0x02}); !bytes.Equal(got, []byte{0x01, 0x02, 0x00}) {
		t.Fatalf("rangeSetKeyAfter = %x, want 010200", got)
	}
	_, _, _, err = rangeSetNormalizeBounds([]byte{0x50}, []byte{0x10})
	var invErr *RangeSetInvertedRangeError
	if !errors.As(err, &invErr) {
		t.Fatalf("inverted range: got %v, want RangeSetInvertedRangeError", err)
	}
}
