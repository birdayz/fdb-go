package recordlayer

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	mathrand "math/rand/v2"
	"sync"
	"testing"

	"github.com/andybalholm/brotli"
)

func brotliSerializer(t *testing.T, level int) *TransformedRecordSerializer {
	t.Helper()
	return mustSerializer(t, NewTransformedRecordSerializerBuilder().SetGoOnlyBrotliCompression(level))
}

func TestTransformedSerializer_BrotliOptInAndLegacyBytes(t *testing.T) {
	t.Parallel()
	union := unionOf(sonnet108 + sonnet108)
	legacy := mustSerializer(t, NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true))
	v1, err := legacy.transform(union, nil)
	if err != nil {
		t.Fatal(err)
	}
	var fresh bytes.Buffer
	w, err := zlib.NewWriterLevel(&fresh, DeflaterBestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(union); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want := []byte{transformPrefixCompressed, 1, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(want[2:6], uint32(len(union))) //nolint:gosec
	want = append(want, fresh.Bytes()...)
	if !bytes.Equal(v1, want) {
		t.Fatal("default compression changed Java's version-1 bytes")
	}
	for level := 0; level <= maxBrotliCompressionLevel; level++ {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			s := brotliSerializer(t, level)
			v2, err := s.transform(union, nil)
			if err != nil {
				t.Fatal(err)
			}
			if v2[0] != transformPrefixCompressed || v2[1] != brotliCompressionVersion || len(v2) >= len(union) {
				t.Fatalf("Brotli did not store a smaller v2 body: raw=%d stored=%d prefix=%x", len(union), len(v2), v2[:2])
			}
			if binary.BigEndian.Uint32(v2[2:6]) != uint32(len(union)) || binary.BigEndian.Uint32(v2[6:10]) != crc32.Checksum(union, brotliChecksumTable) { //nolint:gosec
				t.Fatal("wrong length or CRC32C in version-2 header")
			}
			for _, encoded := range [][]byte{union, append([]byte{transformPrefixClear}, union...), v1, v2} {
				back, err := s.untransform(encoded)
				if err != nil || !bytes.Equal(back, union) || sha256.Sum256(back) != sha256.Sum256(union) {
					t.Fatalf("opted reader lost exact bytes: %v", err)
				}
			}
			for _, reader := range []*TransformedRecordSerializer{plainTransformedReader, legacy, mustSerializer(t, NewTransformedRecordSerializerBuilder())} {
				_, err := reader.untransform(v2)
				wantSerializationError(t, err, "unknown compression version")
			}
			readOnly := mustSerializer(t, NewTransformedRecordSerializerBuilder().SetGoOnlyBrotliCompression(level).SetCompressWhenSerializing(false))
			if back, err := readOnly.untransform(v2); err != nil || !bytes.Equal(back, union) {
				t.Fatalf("read-only opt-in: %v", err)
			}
			clear, err := readOnly.transform(union, nil)
			if err != nil || clear[0] != transformPrefixClear || !bytes.Equal(clear[1:], union) {
				t.Fatalf("disabled compressed writes: %v", err)
			}
		})
	}
	for _, level := range []int{-1, 5, 11} {
		_, err := NewTransformedRecordSerializerBuilder().SetGoOnlyBrotliCompression(level).Build()
		var arg *RecordCoreArgumentError
		if !errors.As(err, &arg) {
			t.Fatalf("level %d admitted: %v", level, err)
		}
	}
}

func TestTransformedSerializer_BrotliFallbackAndBounds(t *testing.T) {
	t.Parallel()
	rng := mathrand.New(mathrand.NewPCG(283, 11))
	random := make([]byte, 4096)
	for i := range random {
		random[i] = byte(rng.Uint64())
	}
	for level := 0; level <= maxBrotliCompressionLevel; level++ {
		s := brotliSerializer(t, level)
		for _, union := range [][]byte{unionOf(""), unionOf("short"), unionOf(string(random))} {
			stored, err := s.transform(union, nil)
			if err != nil || stored[0] != transformPrefixClear || !bytes.Equal(stored[1:], union) {
				t.Fatalf("level %d did not keep an unprofitable body clear: %v", level, err)
			}
			back, err := s.untransform(stored)
			if err != nil || !bytes.Equal(back, union) {
				t.Fatalf("clear fallback changed bytes: %v", err)
			}
		}
	}
	s := brotliSerializer(t, 1)
	limit := make([]byte, maxBrotliRecordBytes+1)
	_, err := s.transform(limit, nil)
	wantSerializationError(t, err, "exceeds 10000000 bytes")
	stored, err := s.transform(limit[:maxBrotliRecordBytes], nil)
	if err != nil || stored[0] != transformPrefixCompressed {
		t.Fatalf("exact size cap was refused: %v", err)
	}
	back, err := s.untransform(stored)
	if err != nil || !bytes.Equal(back, limit[:maxBrotliRecordBytes]) {
		t.Fatalf("exact size cap did not round trip: %v", err)
	}
}

func TestTransformedSerializer_BrotliRejectsCorruption(t *testing.T) {
	t.Parallel()
	s := brotliSerializer(t, 1)
	union := unionOf(sonnet108 + sonnet108)
	stored, err := s.transform(union, nil)
	if err != nil || stored[0] != transformPrefixCompressed {
		t.Fatalf("compression control: %v", err)
	}
	mutate := func(f func([]byte) []byte) []byte { return f(bytes.Clone(stored)) }
	cases := []struct {
		name, want string
		data       []byte
	}{
		{"unknown version", "unknown compression version", mutate(func(b []byte) []byte { b[1] = 3; return b })},
		{"negative length", "invalid decompressed length", mutate(func(b []byte) []byte { b[2] = 0x80; return b })},
		{"over cap", "invalid decompressed length", mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[2:6], maxBrotliRecordBytes+1); return b })},
		{"short declared length", "decompressed record too large", mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[2:6], uint32(len(union)-1)); return b })}, //nolint:gosec
		{"long declared length", "decompressed record too small", mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[2:6], uint32(len(union)+1)); return b })},  //nolint:gosec
		{"checksum", "decompression checksum mismatch", mutate(func(b []byte) []byte { b[6] ^= 0xff; return b })},
		{"stream missing", "decompression error", stored[:10]},
		{"truncated stream", "decompression error", stored[:len(stored)-1]},
		{"trailing input", "decompression", mutate(func(b []byte) []byte { return append(b, 0) })},
		{"concatenated streams", "decompression", mutate(func(b []byte) []byte { return append(b, stored[10:]...) })},
		{"invalid stream", "decompression error", append(bytes.Clone(stored[:10]), 0xff, 0xff, 0xff)},
	}
	for n := 0; n < 10; n++ {
		cases = append(cases, struct {
			name, want string
			data       []byte
		}{fmt.Sprintf("truncated header %d", n), "", stored[:n]})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.untransform(c.data)
			wantSerializationError(t, err, c.want)
			back, err := s.untransform(stored)
			if err != nil || !bytes.Equal(back, union) {
				t.Fatalf("reader poisoned after refusal: %v", err)
			}
		})
	}
	bomb, err := s.transform(unionOf(string(bytes.Repeat([]byte{'x'}, 200_000))), nil)
	if err != nil || bomb[0] != transformPrefixCompressed {
		t.Fatalf("expansion control: %v", err)
	}
	binary.BigEndian.PutUint32(bomb[2:6], 1)
	_, err = s.untransform(bomb)
	wantSerializationError(t, err, "decompressed record too large")
}

func TestTransformedSerializer_BrotliFailedReadersAreDiscarded(t *testing.T) {
	t.Parallel()
	union := unionOf(sonnet108 + sonnet108)
	writer := brotliSerializer(t, 1)
	stored, err := writer.transform(union, nil)
	if err != nil || stored[0] != transformPrefixCompressed {
		t.Fatalf("compression control: %v", err)
	}
	mutate := func(f func([]byte) []byte) []byte { return f(bytes.Clone(stored)) }
	for name, malformed := range map[string][]byte{
		"missing stream": stored[:10],
		"truncated":      stored[:len(stored)-1],
		"trailing":       append(bytes.Clone(stored), 0),
		"concatenated":   append(bytes.Clone(stored), stored[10:]...),
		"invalid":        append(bytes.Clone(stored[:10]), 0xff, 0xff, 0xff),
		"short length": mutate(func(b []byte) []byte {
			binary.BigEndian.PutUint32(b[2:6], uint32(len(union)-1)) //nolint:gosec
			return b
		}),
		"long length": mutate(func(b []byte) []byte {
			binary.BigEndian.PutUint32(b[2:6], uint32(len(union)+1)) //nolint:gosec
			return b
		}),
		"checksum": mutate(func(b []byte) []byte { b[6] ^= 0xff; return b }),
	} {
		t.Run(name, func(t *testing.T) {
			s := brotliSerializer(t, 1)
			s.brotli = newBrotliFreeLists(1, 1)
			back, err := s.untransform(stored)
			if err != nil || !bytes.Equal(back, union) || len(s.brotli.readers) != 1 {
				t.Fatalf("successful decode did not populate the private pool: %v", err)
			}
			failed := <-s.brotli.readers
			s.brotli.readers <- failed
			if _, err := s.untransform(malformed); err == nil {
				t.Fatal("malformed control was accepted")
			}
			if len(s.brotli.readers) != 0 {
				t.Fatal("failed, incomplete or unchecked decoder was returned to the pool")
			}
			back, err = s.untransform(stored)
			if err != nil || !bytes.Equal(back, union) || len(s.brotli.readers) != 1 {
				t.Fatalf("valid record after a refusal did not decode cleanly: %v", err)
			}
			replacement := <-s.brotli.readers
			if replacement == failed {
				t.Fatal("refused decoder was reused instead of discarded")
			}
			s.brotli.readers <- replacement
		})
	}
}

func TestTransformedSerializer_BrotliEncryptionAndValidation(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x28}, 16)
	s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetGoOnlyBrotliCompression(1).
		SetEncryptWhenSerializing(true).SetEncryptionKey(key).SetWriteValidationRatio(1).SetWriteEncryptionValidationRatio(1))
	union := unionOf(sonnet108 + sonnet108)
	stored, err := s.transform(union, nil)
	if err != nil || stored[0] != transformPrefixCompressedThenEncrypted {
		t.Fatalf("compression/encryption/validation pipeline: %v", err)
	}
	back, err := s.untransform(stored)
	if err != nil || !bytes.Equal(back, union) {
		t.Fatalf("encrypted Brotli round trip: %v", err)
	}
	legacy := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptionKey(key))
	_, err = legacy.untransform(stored)
	wantSerializationError(t, err, "unknown compression version")
	_, err = plainTransformedReader.untransform(stored)
	wantSerializationError(t, err, "cannot decrypt")
}

func TestTransformedSerializer_BrotliStateReuseAndConcurrentOwnership(t *testing.T) {
	t.Parallel()
	s := brotliSerializer(t, 1)
	s.brotli = newBrotliFreeLists(2, 2)
	union := unionOf(sonnet108 + sonnet108)
	stored, err := s.transform(union, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.untransform(stored); err != nil {
		t.Fatal(err)
	}
	if len(s.brotli.writers[1]) != 1 || len(s.brotli.readers) != 1 {
		t.Fatal("first call did not return its states")
	}
	writer, reader := <-s.brotli.writers[1], <-s.brotli.readers
	s.brotli.writers[1] <- writer
	s.brotli.readers <- reader
	for i := 0; i < 5; i++ {
		got, err := s.transform(union, nil)
		if err != nil || !bytes.Equal(got, stored) {
			t.Fatalf("reused writer changed bytes: %v", err)
		}
		back, err := s.untransform(got)
		if err != nil || !bytes.Equal(back, union) {
			t.Fatalf("reused reader changed bytes: %v", err)
		}
		if len(s.brotli.writers[1]) != 1 || len(s.brotli.readers) != 1 {
			t.Fatal("state reuse grew or emptied a list")
		}
		gotWriter, gotReader := <-s.brotli.writers[1], <-s.brotli.readers
		if gotWriter != writer || gotReader != reader {
			t.Fatal("states were replaced rather than reused")
		}
		s.brotli.writers[1] <- gotWriter
		s.brotli.readers <- gotReader
	}
	var fresh bytes.Buffer
	w := brotli.NewWriterOptions(&fresh, brotli.WriterOptions{Quality: 1, LGWin: 20})
	if _, err := w.Write(union); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil || !bytes.Equal(fresh.Bytes(), stored[1+brotliCompressionHeader:]) {
		t.Fatalf("reused writer differs from a fresh writer: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for worker := 0; worker < 12; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			body := unionOf(fmt.Sprintf("worker-%d-%s", worker, sonnet108+sonnet108))
			for i := 0; i < 8; i++ {
				encoded, err := s.transform(body, nil)
				if err != nil {
					errs <- err
					return
				}
				back, err := s.untransform(encoded)
				if err != nil || !bytes.Equal(back, body) {
					errs <- fmt.Errorf("worker %d round trip: %w", worker, err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(s.brotli.writers[1]) > 2 || len(s.brotli.readers) > 2 {
		t.Fatal("reuse exceeded the bounded lists")
	}
	// Simultaneous acquires must be distinct, and excess releases are dropped.
	writers := []*brotli.Writer{s.brotli.acquireWriter(&fresh, 1), s.brotli.acquireWriter(&fresh, 1), s.brotli.acquireWriter(&fresh, 1)}
	readers := []*brotli.Reader{s.brotli.acquireReader(bytes.NewReader(stored[1+brotliCompressionHeader:])), s.brotli.acquireReader(bytes.NewReader(stored[1+brotliCompressionHeader:])), s.brotli.acquireReader(bytes.NewReader(stored[1+brotliCompressionHeader:]))}
	if writers[0] == writers[1] || writers[0] == writers[2] || writers[1] == writers[2] || readers[0] == readers[1] || readers[0] == readers[2] || readers[1] == readers[2] {
		t.Fatal("a state has concurrent owners")
	}
	for _, writer := range writers {
		s.brotli.releaseWriter(writer, 1)
	}
	for _, reader := range readers {
		back, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(back, union) {
			t.Fatalf("reader cannot be released before a complete validated stream: %v", err)
		}
		s.brotli.releaseReader(reader)
	}
	if len(s.brotli.writers[1]) != 2 || len(s.brotli.readers) != 2 {
		t.Fatal("release failed to enforce list capacities")
	}
}
