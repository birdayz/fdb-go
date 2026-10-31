package recordlayer

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
	brotliCompressionVersion  = 2
	brotliCompressionHeader   = 9
	maxBrotliCompressionLevel = 4
	maxBrotliRecordBytes      = 10_000_000
	brotliWriterFreeListSize  = 2
	brotliReaderFreeListSize  = 8
)

var (
	brotliChecksumTable = crc32.MakeTable(crc32.Castagnoli)
	brotliState         = newBrotliFreeLists(brotliWriterFreeListSize, brotliReaderFreeListSize)
)

// Reuse is bounded independently of the number of concurrent stores. Each
// acquired state belongs to one call. Like the zlib lists, released states can
// retain plaintext in their codec windows until reused or garbage-collected.
type brotliFreeLists struct {
	writers [maxBrotliCompressionLevel + 1]chan *brotli.Writer
	readers chan *brotli.Reader
}

func newBrotliFreeLists(writers, readers int) *brotliFreeLists {
	l := &brotliFreeLists{readers: make(chan *brotli.Reader, readers)}
	for i := range l.writers {
		l.writers[i] = make(chan *brotli.Writer, writers)
	}
	return l
}

func (s *TransformedRecordSerializer) brotliLists() *brotliFreeLists {
	if s.brotli != nil {
		return s.brotli
	}
	return brotliState
}

func (l *brotliFreeLists) acquireWriter(dst io.Writer, level int) *brotli.Writer {
	select {
	case w := <-l.writers[level]:
		w.Reset(dst)
		return w
	default:
		return brotli.NewWriterOptions(dst, brotli.WriterOptions{Quality: level, LGWin: 20})
	}
}

func (l *brotliFreeLists) releaseWriter(w *brotli.Writer, level int) {
	w.Reset(nil)
	select {
	case l.writers[level] <- w:
	default:
	}
}

func (l *brotliFreeLists) acquireReader(src io.Reader) *brotli.Reader {
	select {
	case r := <-l.readers:
		_ = r.Reset(src) // Brotli's Reset always returns nil.
		return r
	default:
		return brotli.NewReader(src)
	}
}

// Only a fully consumed and validated reader may be returned: Brotli's Reset
// can retain buffered input after an error that did not set error_code negative.
func (l *brotliFreeLists) releaseReader(r *brotli.Reader) {
	_ = r.Reset(nil)
	select {
	case l.readers <- r:
	default:
	}
}

func (s *TransformedRecordSerializer) compressBrotli(st *transformState) error {
	if len(st.data) > maxBrotliRecordBytes {
		return serializationError("Go-only Brotli record exceeds 10000000 bytes")
	}
	if len(st.data) <= brotliCompressionHeader {
		return nil
	}
	var buf bytes.Buffer
	lists := s.brotliLists()
	w := lists.acquireWriter(&buf, s.brotliCompressionLevel)
	defer lists.releaseWriter(w, s.brotliCompressionLevel)
	if _, err := w.Write(st.data); err != nil {
		return &RecordSerializationError{Message: "compression error", Cause: err}
	}
	if err := w.Close(); err != nil {
		return &RecordSerializationError{Message: "compression error", Cause: err}
	}
	if buf.Len() >= len(st.data)-brotliCompressionHeader {
		return nil
	}
	out := make([]byte, brotliCompressionHeader+buf.Len())
	out[0] = brotliCompressionVersion
	binary.BigEndian.PutUint32(out[1:5], uint32(len(st.data))) //nolint:gosec
	binary.BigEndian.PutUint32(out[5:9], crc32.Checksum(st.data, brotliChecksumTable))
	copy(out[brotliCompressionHeader:], buf.Bytes())
	st.data, st.compressed = out, true
	return nil
}

func decompressBrotliTransformed(st *transformState, lists *brotliFreeLists) error {
	if len(st.data) < brotliCompressionHeader {
		return serializationError("decompression error")
	}
	length := binary.BigEndian.Uint32(st.data[1:5])
	if length > maxBrotliRecordBytes {
		return serializationError("invalid decompressed length")
	}
	in := bytes.NewReader(st.data[brotliCompressionHeader:])
	r := lists.acquireReader(in)
	// Do not allocate from an untrusted declared length. The extra byte detects
	// expansion beyond the header, including streams with a dishonest size.
	out, err := io.ReadAll(io.LimitReader(r, int64(length)+1))
	if err != nil {
		return &RecordSerializationError{Message: "decompression error", Cause: err}
	}
	if len(out) < int(length) {
		return serializationError("decompressed record too small")
	}
	if len(out) > int(length) {
		return serializationError("decompressed record too large")
	}
	if in.Len() != 0 {
		return serializationError("decompressed record too large")
	}
	if crc32.Checksum(out, brotliChecksumTable) != binary.BigEndian.Uint32(st.data[5:9]) {
		return serializationError("decompression checksum mismatch")
	}
	st.data = out
	lists.releaseReader(r)
	return nil
}
