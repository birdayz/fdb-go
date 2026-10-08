// Package fnv64 hashes planner identity streams without string-to-byte copies.
package fnv64

import (
	"io"
	"strconv"
)

type digest struct{ state uint64 }

const (
	offset = 14695981039346656037
	prime  = 1099511628211
)

// New returns an FNV-1a writer supporting both bytes and strings.
func New() *digest { return &digest{state: offset} }

// Sum64 returns the hash of the bytes written so far.
func (h *digest) Sum64() uint64 { return h.state }

func (h *digest) Write(p []byte) (int, error) {
	s := h.state
	for _, b := range p {
		s ^= uint64(b)
		s *= prime
	}
	h.state = s
	return len(p), nil
}

// WriteInt appends a decimal integer without an escaping scratch buffer on a digest.
func WriteInt(w io.Writer, n int64) {
	if h, ok := w.(*digest); ok {
		var scratch [20]byte
		_, _ = h.Write(strconv.AppendInt(scratch[:0], n, 10))
		return
	}
	_, _ = io.WriteString(w, strconv.FormatInt(n, 10))
}

// WriteHex appends a hexadecimal integer without a temporary string on a digest.
func WriteHex(w io.Writer, n uint64) {
	if h, ok := w.(*digest); ok {
		var scratch [16]byte
		_, _ = h.Write(strconv.AppendUint(scratch[:0], n, 16))
		return
	}
	_, _ = io.WriteString(w, strconv.FormatUint(n, 16))
}

// WriteString avoids io.WriteString's allocating fallback through Write.
func (h *digest) WriteString(str string) (int, error) {
	s := h.state
	for i := 0; i < len(str); i++ {
		s ^= uint64(str[i])
		s *= prime
	}
	h.state = s
	return len(str), nil
}
