package fdb_test

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	. "github.com/onsi/gomega"
)

func TestPrintable(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// ASCII printable characters pass through unchanged.
	g.Expect(fdb.Printable([]byte("hello"))).To(Equal("hello"))

	// Backslash is escaped to double backslash.
	g.Expect(fdb.Printable([]byte(`a\b`))).To(Equal(`a\\b`))

	// Non-printable bytes are escaped as \xHH.
	g.Expect(fdb.Printable([]byte{0x00, 0x01, 0x1f})).To(Equal(`\x00\x01\x1f`))

	// High bytes (>= 127) are escaped.
	g.Expect(fdb.Printable([]byte{0x80, 0xff})).To(Equal(`\x80\xff`))

	// Mixed: printable + non-printable + backslash.
	g.Expect(fdb.Printable([]byte{0x41, 0x00, 0x5c, 0x42})).To(Equal(`A\x00\\B`))

	// Empty input returns empty string.
	g.Expect(fdb.Printable(nil)).To(Equal(""))
	g.Expect(fdb.Printable([]byte{})).To(Equal(""))

	// Space (0x20) is printable, DEL (0x7f) is not.
	g.Expect(fdb.Printable([]byte{0x20})).To(Equal(" "))
	g.Expect(fdb.Printable([]byte{0x7f})).To(Equal(`\x7f`))
}

func TestKeyString(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Key.String() delegates to Printable.
	k := fdb.Key([]byte{0x41, 0x00, 0x42})
	g.Expect(k.String()).To(Equal(`A\x00B`))

	// Nil key.
	var nilKey fdb.Key
	g.Expect(nilKey.String()).To(Equal(""))
}

func TestPrefixRangeCoversBeginCopy(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	prefix := []byte{0xAB, 0xCD}
	kr, err := fdb.PrefixRange(prefix)
	g.Expect(err).NotTo(HaveOccurred())

	begin, end := kr.FDBRangeKeys()
	g.Expect([]byte(begin.FDBKey())).To(Equal([]byte{0xAB, 0xCD}))
	g.Expect([]byte(end.FDBKey())).To(Equal([]byte{0xAB, 0xCE}))

	// Verify the begin key is a copy (mutating prefix doesn't affect it).
	prefix[0] = 0xFF
	g.Expect([]byte(begin.FDBKey())).To(Equal([]byte{0xAB, 0xCD}))
}
