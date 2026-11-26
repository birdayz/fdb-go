package rlcrypto

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"

	"github.com/andybalholm/brotli"
)

// Version 1 remains Java's zlib format. Version 2 is Go-only: version byte,
// big-endian uncompressed length, CRC32C of the plaintext, then a Brotli stream.
const (
	BrotliCompressionVersion  = 2
	BrotliCompressionHeader   = 9
	MaxBrotliCompressionLevel = 4
	MaxBrotliRecordBytes      = 10_000_000
	brotliWriterFreeListSize  = 2
	brotliReaderFreeListSize  = 8
)

var (
	brotliChecksumTable    = crc32.MakeTable(crc32.Castagnoli)
	DefaultBrotliFreeLists = NewBrotliFreeLists(brotliWriterFreeListSize, brotliReaderFreeListSize)
)

// BrotliChecksum returns the Castagnoli CRC32C checksum used in Go-only Brotli v2 framing.
func BrotliChecksum(b []byte) uint32 {
	return crc32.Checksum(b, brotliChecksumTable)
}

// BrotliFreeLists keeps released Brotli writers and readers for reuse.
// Each call owns one codec state; bounded free lists can retain plaintext windows
// until reuse or collection, as the zlib lists do.
type BrotliFreeLists struct {
	Writers [MaxBrotliCompressionLevel + 1]chan *brotli.Writer
	Readers chan *brotli.Reader
}

// NewBrotliFreeLists allocates a bounded pool of Brotli writers and readers.
func NewBrotliFreeLists(writers, readers int) *BrotliFreeLists {
	l := &BrotliFreeLists{Readers: make(chan *brotli.Reader, readers)}
	for i := range l.Writers {
		l.Writers[i] = make(chan *brotli.Writer, writers)
	}
	return l
}

// AcquireWriter returns a Brotli writer configured for level over dst.
func (l *BrotliFreeLists) AcquireWriter(dst io.Writer, level int) *brotli.Writer {
	select {
	case w := <-l.Writers[level]:
		w.Reset(dst)
		return w
	default:
		return brotli.NewWriterOptions(dst, brotli.WriterOptions{Quality: level, LGWin: 20})
	}
}

// ReleaseWriter resets w and returns it to l when capacity allows.
func (l *BrotliFreeLists) ReleaseWriter(w *brotli.Writer, level int) {
	w.Reset(nil)
	select {
	case l.Writers[level] <- w:
	default:
	}
}

// AcquireReader returns a Brotli reader positioned over src.
func (l *BrotliFreeLists) AcquireReader(src io.Reader) *brotli.Reader {
	select {
	case r := <-l.Readers:
		_ = r.Reset(src) // Brotli's Reset always returns nil.
		return r
	default:
		return brotli.NewReader(src)
	}
}

// ReleaseReader resets r and returns it to l when capacity allows.
// Only a fully consumed and validated reader may be returned: Brotli's Reset
// can retain buffered input after an error that did not set error_code negative.
func (l *BrotliFreeLists) ReleaseReader(r *brotli.Reader) {
	_ = r.Reset(nil)
	select {
	case l.Readers <- r:
	default:
	}
}

// CompressBrotli compresses st.Data using Go-only Brotli v2 framing when smaller.
func CompressBrotli(st *TransformState, level int, lists *BrotliFreeLists) error {
	if lists == nil {
		lists = DefaultBrotliFreeLists
	}
	if len(st.Data) > MaxBrotliRecordBytes {
		return &SerializationError{Message: "Go-only Brotli record exceeds 10000000 bytes"}
	}
	if len(st.Data) <= BrotliCompressionHeader {
		return nil
	}
	var buf bytes.Buffer
	w := lists.AcquireWriter(&buf, level)
	defer lists.ReleaseWriter(w, level)
	if _, err := w.Write(st.Data); err != nil {
		return &SerializationError{Message: "compression error", Cause: err}
	}
	if err := w.Close(); err != nil {
		return &SerializationError{Message: "compression error", Cause: err}
	}
	if buf.Len() >= len(st.Data)-BrotliCompressionHeader {
		return nil
	}
	out := make([]byte, BrotliCompressionHeader+buf.Len())
	out[0] = BrotliCompressionVersion
	binary.BigEndian.PutUint32(out[1:5], uint32(len(st.Data))) //nolint:gosec
	binary.BigEndian.PutUint32(out[5:9], BrotliChecksum(st.Data))
	copy(out[BrotliCompressionHeader:], buf.Bytes())
	st.Data, st.Compressed = out, true
	return nil
}

// DecompressBrotliTransformed decompresses a Go-only Brotli v2 payload in st.Data.
func DecompressBrotliTransformed(st *TransformState, lists *BrotliFreeLists) error {
	if lists == nil {
		lists = DefaultBrotliFreeLists
	}
	if len(st.Data) < BrotliCompressionHeader {
		return &SerializationError{Message: "decompression error"}
	}
	length := binary.BigEndian.Uint32(st.Data[1:5])
	if length > MaxBrotliRecordBytes {
		return &SerializationError{Message: "invalid decompressed length"}
	}
	in := bytes.NewReader(st.Data[BrotliCompressionHeader:])
	r := lists.AcquireReader(in)
	// Do not allocate from an untrusted declared length. The extra byte detects
	// expansion beyond the header, including streams with a dishonest size.
	out, err := io.ReadAll(io.LimitReader(r, int64(length)+1))
	if err != nil {
		return &SerializationError{Message: "decompression error", Cause: err}
	}
	if len(out) < int(length) {
		return &SerializationError{Message: "decompressed record too small"}
	}
	if len(out) > int(length) {
		return &SerializationError{Message: "decompressed record too large"}
	}
	if in.Len() != 0 {
		return &SerializationError{Message: "decompressed record too large"}
	}
	if BrotliChecksum(out) != binary.BigEndian.Uint32(st.Data[5:9]) {
		return &SerializationError{Message: "decompression checksum mismatch"}
	}
	st.Data = out
	lists.ReleaseReader(r)
	return nil
}
