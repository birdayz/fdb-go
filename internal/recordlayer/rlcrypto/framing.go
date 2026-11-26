// Portions derived from FoundationDB Record Layer (
// TransformedRecordSerializer.java, TransformedRecordSerializerJCE.java,
// TransformedRecordSerializerPrefix.java, SerializationKeyManager.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rlcrypto

import (
	"bytes"
	"compress/zlib"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"fdb.dev/internal/unicode15"
)

// Java's TransformedRecordSerializerPrefix constants
// (TransformedRecordSerializerPrefix.java:68-74): the low three bits of the
// varint prefix are the transformation, the rest the encryption key number.
const (
	TransformPrefixEncrypted               = 1
	TransformPrefixClear                   = 2
	TransformPrefixCompressed              = 4
	TransformPrefixCompressedThenEncrypted = 5
	TransformTypeMask                      = 0x07
	TransformKeyShift                      = 3
)

// Java's compression header (TransformedRecordSerializer.java:67-68): the
// version byte, then the uncompressed length as a big-endian int.
const (
	MinCompressionVersion = 1
	MaxCompressionVersion = 1
	CompressionHeaderSize = 5
)

// DefaultCipher is Java's CipherPool.DEFAULT_CIPHER. Go implements every AES
// transformation Java's serializer can run (jce_cipher.go).
const DefaultCipher = "AES/CBC/PKCS5Padding"

// CipherIVSize is CipherPool.IV_SIZE: an encrypted body begins with the IV.
const CipherIVSize = 16

// Java's java.util.zip.Deflater levels, which compress/zlib shares.
const (
	DeflaterDefaultCompression = -1
	DeflaterBestCompression    = 9
)

// ErrInvalidCompressionLevel indicates a Deflater level outside [-1, 9].
var ErrInvalidCompressionLevel = errors.New("invalid compression level")

// SerializationError reports a framing, compression, or cipher failure from rlcrypto.
// GeneralSecurity marks cipher failures (Java's GeneralSecurityException).
type SerializationError struct {
	Message         string
	Cause           error
	GeneralSecurity bool
}

func (e *SerializationError) Error() string {
	switch {
	case e.Message != "" && e.Cause != nil:
		return e.Message + ": " + e.Cause.Error()
	case e.Message != "":
		return e.Message
	}
	return fmt.Sprintf("failed to serialize record: %v", e.Cause)
}

func (e *SerializationError) Unwrap() error {
	return e.Cause
}

// SerializationKeyManager is Java's SerializationKeyManager: the key number a
// record is written with, and per key number the key, the cipher name and the
// random source the IV is drawn from. A nil random source (Java always has a
// SecureRandom) is the writing store's randomness: its DST env's, crypto/rand
// in production, so a seeded simulation replays the IV like every other
// persisted random byte.
type SerializationKeyManager interface {
	GetSerializationKey() int32
	GetKey(keyNumber int32) ([]byte, error)
	GetCipher(keyNumber int32) (string, error)
	GetRandom(keyNumber int32) (io.Reader, error)
}

// KeyAlgorithmReporter is implemented by a SerializationKeyManager that knows
// each key's algorithm, as a Java Key carries it (KeyStoreSerializationKeyManager
// does). Java's AES cipher refuses a key of another algorithm when it is
// initialized ("Wrong algorithm: AES or Rijndael required"); a manager that
// does not report algorithms hands Go raw AES keys.
type KeyAlgorithmReporter interface {
	KeyAlgorithm(keyNumber int32) (string, error)
}

// TransformState is TransformedRecordSerializerState: the data, and what the
// prefix said of it.
type TransformState struct {
	Data       []byte
	Compressed bool
	Encrypted  bool
	KeyNumber  int32
}

// The zlib state is reused across records. A fresh compress/flate writer
// allocates about a megabyte of compressor state and a fresh reader some 40
// KiB, and both ran once per record: under the race detector a full scan read
// 81k compressed rows in 3.3 GiB of allocation before the driver's 4 s read
// budget ran out, where clear rows read all 100k in 208 MiB. Java constructs a
// Deflater/Inflater per call too, but those wrap native zlib; the bytes are the
// same either way, since Reset restores a writer's or reader's exact initial
// state.
//
// Bounded free lists, not sync.Pool: under the race detector sync.Pool drops a
// quarter of what is returned to it, which at a megabyte per writer left the
// race build allocating a quarter of the unpooled cost — the build whose read
// window this exists for. The lists never shrink, so their bounds are the
// memory they can hold: zlibWriterFreeListSize writers of about 1.1 MiB per
// compression level in use (stores use one level), and zlibReaderFreeListSize
// readers of about 41 KiB. Beyond a bound a released state is dropped, and the
// record that would have reused it builds fresh state, as Java does for every
// record. The bound on writers covers the compressions one process runs at once
// at one level: a writer is held only across the CPU work of one record. Only
// the 1M stress load, four at a time, was measured; TestFDB_Ingest_Parallelism
// runs 8 and 16 workers, and past the bound a record builds fresh state.
//
// What the reused state still holds: Reset clears a writer's and a reader's
// indices, not their buffers. A released writer keeps plaintext of the records
// it compressed in its window (64 KiB at levels 2-9, 65535 bytes at BestSpeed)
// and in its token buffer, whose literal tokens each carry an input byte (16 Ki
// tokens at levels 2-9, 65535 at BestSpeed); at BestSpeed also in the previous
// block's backing array and in the match table, whose 16 Ki entries keep four
// input bytes each. A released reader keeps up to 32 KiB of the records it
// decompressed (its history). When a shorter record follows a longer one the
// tail of the older survives beside the newer. For an encrypting store that is
// plaintext of earlier records, held until overwritten.
const (
	zlibWriterFreeListSize = 8
	zlibReaderFreeListSize = 64
)

// DefaultZlibFreeLists is the process-wide pool of reusable zlib writers and readers.
var DefaultZlibFreeLists = NewZlibFreeLists(zlibWriterFreeListSize, zlibReaderFreeListSize)

// ZlibFreeLists keeps released zlib writers (one list per compression level) and
// readers for reuse.
type ZlibFreeLists struct {
	Writers [DeflaterBestCompression - DeflaterDefaultCompression + 1]chan *zlib.Writer
	Readers chan io.ReadCloser
}

// NewZlibFreeLists allocates bounded channels for reusable zlib writers and readers.
func NewZlibFreeLists(writers, readers int) *ZlibFreeLists {
	l := &ZlibFreeLists{Readers: make(chan io.ReadCloser, readers)}
	for i := range l.Writers {
		l.Writers[i] = make(chan *zlib.Writer, writers)
	}
	return l
}

// AcquireWriter returns a zlib writer at level over dst, reused when one is
// free. The level has been range-checked by the caller.
func (l *ZlibFreeLists) AcquireWriter(dst io.Writer, level int) (*zlib.Writer, error) {
	select {
	case w := <-l.Writers[level-DeflaterDefaultCompression]:
		w.Reset(dst)
		return w, nil
	default:
		return zlib.NewWriterLevel(dst, level)
	}
}

// ReleaseWriter resets w and returns it to l if space permits.
func (l *ZlibFreeLists) ReleaseWriter(w *zlib.Writer, level int) {
	w.Reset(nil)
	select {
	case l.Writers[level-DeflaterDefaultCompression] <- w:
	default:
	}
}

// AcquireReader returns a zlib reader over src with its header read, reused
// when one is free. A header error is returned as zlib.NewReader returns it,
// and the reader goes back on the list.
func (l *ZlibFreeLists) AcquireReader(src io.Reader) (io.ReadCloser, error) {
	select {
	case r := <-l.Readers:
		if err := r.(zlib.Resetter).Reset(src, nil); err != nil {
			l.ReleaseReader(r)
			return nil, err
		}
		return r, nil
	default:
		return zlib.NewReader(src)
	}
}

// zlibEmptyHeader is a zlib stream header (deflate, 32 KiB window, no
// dictionary) a released reader is reset onto, so that the reader stops
// referring to the record it last read.
var zlibEmptyHeader = []byte{0x78, 0x9c}

func (l *ZlibFreeLists) ReleaseReader(r io.ReadCloser) {
	// Drop the reference to the last record's compressed bytes. The inflate
	// history (up to 32 KiB of what this reader decompressed, not only the last
	// record) stays in the reused buffer until it is overwritten; the design
	// states that retention.
	_ = r.(zlib.Resetter).Reset(bytes.NewReader(zlibEmptyHeader), nil)
	select {
	case l.Readers <- r:
	default:
	}
}

// CompressZlib is Java's compress (TransformedRecordSerializer.java:125-170): only
// a body of more than five bytes, and only when the deflated form is strictly
// shorter than the body less the five-byte header.
func CompressZlib(st *TransformState, compressionLevel int, lists *ZlibFreeLists) error {
	if lists == nil {
		lists = DefaultZlibFreeLists
	}
	if len(st.Data) <= CompressionHeaderSize {
		return nil
	}
	// Java constructs its Deflater here, whose constructor refuses a level
	// outside [-1, 9] with IllegalArgumentException "invalid compression level".
	if compressionLevel < DeflaterDefaultCompression || compressionLevel > DeflaterBestCompression {
		return ErrInvalidCompressionLevel
	}
	var buf bytes.Buffer
	w, err := lists.AcquireWriter(&buf, compressionLevel)
	if err != nil {
		return &SerializationError{Cause: err}
	}
	defer lists.ReleaseWriter(w, compressionLevel)
	if _, err := w.Write(st.Data); err != nil {
		return &SerializationError{Cause: err}
	}
	if err := w.Close(); err != nil {
		return &SerializationError{Cause: err}
	}
	if buf.Len() >= len(st.Data)-CompressionHeaderSize {
		st.Compressed = false
		return nil
	}
	out := make([]byte, CompressionHeaderSize+buf.Len())
	out[0] = MaxCompressionVersion
	binary.BigEndian.PutUint32(out[1:5], uint32(len(st.Data))) //nolint:gosec
	copy(out[CompressionHeaderSize:], buf.Bytes())
	st.Data = out
	st.Compressed = true
	return nil
}

// DecompressZlib is Java's decompress (TransformedRecordSerializer.java:251-285).
func DecompressZlib(st *TransformState, lists *ZlibFreeLists) error {
	if lists == nil {
		lists = DefaultZlibFreeLists
	}
	if len(st.Data) < CompressionHeaderSize {
		return &SerializationError{Message: "decompression error"}
	}
	version := int8(st.Data[0]) //nolint:gosec
	if version < MinCompressionVersion || version > MaxCompressionVersion {
		return &SerializationError{Message: "unknown compression version"}
	}
	length := int32(binary.BigEndian.Uint32(st.Data[1:5])) //nolint:gosec
	if length < 0 {
		return &SerializationError{Message: "invalid decompressed length"}
	}
	in := bytes.NewReader(st.Data[CompressionHeaderSize:])
	r, err := lists.AcquireReader(in)
	if err != nil {
		return &SerializationError{Message: "decompression error", Cause: err}
	}
	defer lists.ReleaseReader(r)
	out := make([]byte, length)
	n, err := io.ReadFull(r, out)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return &SerializationError{Message: "decompression error", Cause: err}
	}
	if n < int(length) {
		return &SerializationError{Message: "decompressed record too small"}
	}
	// Input left over, output beyond the length or trailing bytes after the
	// stream: Java's Inflater reports them as remaining input.
	var one [1]byte
	m, err := r.Read(one[:])
	if m > 0 {
		return &SerializationError{Message: "decompressed record too large"}
	}
	switch {
	case err == nil, errors.Is(err, io.EOF):
		// The stream ended, its Adler-32 checked.
	case errors.Is(err, io.ErrUnexpectedEOF) && in.Len() == 0:
		// A stream flushed but never finished: no final block and no Adler-32.
		// An old Java writer stored such records (fdb-record-layer#2691), and
		// Java's Inflater reads them, having filled the declared length with
		// no input left (TransformedRecordSerializerTest.decompressWithoutAdler).
	default:
		return &SerializationError{Message: "decompression error", Cause: err}
	}
	if in.Len() > 0 {
		return &SerializationError{Message: "decompressed record too large"}
	}
	st.Data = out
	return nil
}

// EncryptState is TransformedRecordSerializerJCE.encrypt: the IV, then the
// cipher text of the body under km's serialization key.
func EncryptState(st *TransformState, jce bool, km SerializationKeyManager, fallbackRandom io.Reader) error {
	if !jce {
		return &SerializationError{Message: "this serializer cannot encrypt"}
	}
	if km == nil {
		return &SerializationError{Message: "attempted to encrypt without setting key manager (cipher name and key)"}
	}
	// Java draws the IV before it looks the cipher up.
	keyNumber := km.GetSerializationKey()
	random, err := km.GetRandom(keyNumber)
	if err != nil {
		return err
	}
	if random == nil {
		random = fallbackRandom
	}
	var iv [CipherIVSize]byte
	if _, err := io.ReadFull(random, iv[:]); err != nil {
		return &SerializationError{Message: "encryption error", Cause: err}
	}
	block, t, err := cipherBlock(km, keyNumber, "encryption error")
	if err != nil {
		return err
	}
	// ISO 10126 filler comes from the store's randomness, not the key
	// manager's: Java's cipher draws it from its own SecureRandom, so a key
	// manager's seeded random gives the same IV sequence in both engines.
	out, err := t.Encrypt(block, iv[:], st.Data, fallbackRandom)
	if err != nil {
		return &SerializationError{Message: "encryption error", Cause: err, GeneralSecurity: true}
	}
	st.Data = out
	st.Encrypted = true
	st.KeyNumber = keyNumber
	return nil
}

// DecryptState is TransformedRecordSerializerJCE.decrypt.
func DecryptState(st *TransformState, jce bool, km SerializationKeyManager) error {
	if !jce {
		return &SerializationError{Message: "this serializer cannot decrypt"}
	}
	if km == nil {
		return &SerializationError{Message: "missing encryption key or provider during decryption"}
	}
	// Shorter than the IV: Java's decrypt copies the IV out of it, before it
	// looks the cipher up, and fails with an array bound exception, not a
	// cipher failure (so it is not the encryption validation's either).
	if len(st.Data) < CipherIVSize {
		return &SerializationError{Message: "decryption error", Cause: fmt.Errorf("%d bytes, shorter than the %d-byte IV", len(st.Data), CipherIVSize)}
	}
	block, t, err := cipherBlock(km, st.KeyNumber, "decryption error")
	if err != nil {
		return err
	}
	plain, err := t.Decrypt(block, st.Data[:CipherIVSize], st.Data[CipherIVSize:])
	if err != nil {
		return &SerializationError{Message: "decryption error", Cause: err, GeneralSecurity: true}
	}
	st.Data = plain
	return nil
}

// cipherBlock resolves key number's cipher and key, as Java's encrypt and
// decrypt do inside their try: a key manager's own refusal ("only provide key
// number 0", "cannot load key") propagates as it is, and a security failure
// (a cipher the JCE does not provide, a key it rejects) is Java's
// GeneralSecurityException, wrapped as op, "encryption error" or "decryption
// error". Transformation names are parsed as Cipher.getInstance parses them
// (ParseJCETransformation).
func cipherBlock(km SerializationKeyManager, keyNumber int32, op string) (cipher.Block, JCETransformation, error) {
	var t JCETransformation
	name, err := km.GetCipher(keyNumber)
	if err != nil {
		return nil, t, err
	}
	if t, err = ParseJCETransformation(name); err != nil {
		return nil, t, &SerializationError{Message: op, Cause: err, GeneralSecurity: true}
	}
	key, err := km.GetKey(keyNumber)
	if err != nil {
		return nil, t, err
	}
	if reporter, ok := km.(KeyAlgorithmReporter); ok {
		algorithm, err := reporter.KeyAlgorithm(keyNumber)
		if err != nil {
			return nil, t, err
		}
		if !unicode15.EqualFold(algorithm, "AES") && !unicode15.EqualFold(algorithm, "Rijndael") {
			return nil, t, &SerializationError{
				Message:         op,
				Cause:           fmt.Errorf("Wrong algorithm: AES or Rijndael required (key algorithm %s)", algorithm),
				GeneralSecurity: true,
			}
		}
	}
	block, err := t.NewBlock(key)
	if err != nil {
		return nil, t, &SerializationError{Message: op, Cause: err, GeneralSecurity: true}
	}
	return block, t, nil
}

// ShouldIncludeInSample is Java's: always at a ratio of 1 or more, never at 0
// or less, else with probability ratio. The coin comes from r (the store's
// env), so a seeded run validates the same writes.
func ShouldIncludeInSample(ratio float64, r io.Reader) bool {
	if ratio >= 1.0 {
		return true
	}
	if ratio <= 0.0 {
		return false
	}
	var b [8]byte
	if r == nil {
		return true
	}
	if _, err := r.Read(b[:]); err != nil {
		return true // validating a write that need not be is the safe side
	}
	return float64(binary.BigEndian.Uint64(b[:])>>11)/(1<<53) < ratio
}

// EncodeTransformPrefix is TransformedRecordSerializerPrefix.encodePrefix.
func EncodeTransformPrefix(st *TransformState) []byte {
	var prefix uint64
	if !st.Compressed && !st.Encrypted {
		prefix = TransformPrefixClear
	} else {
		if st.Compressed {
			prefix |= TransformPrefixCompressed
		}
		if st.Encrypted {
			prefix |= TransformPrefixEncrypted
			prefix |= uint64(st.KeyNumber) << TransformKeyShift //nolint:gosec
		}
	}
	out := make([]byte, 0, len(st.Data)+10)
	for {
		b := byte(prefix & 0x7F)
		prefix >>= 7
		if prefix != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if prefix == 0 {
			break
		}
	}
	return append(out, st.Data...)
}

// DecodeTransformPrefix is TransformedRecordSerializerPrefix.decodePrefix and
// readVarint: prefixed reports whether the data carries a prefix at all (a
// bare union message's first field is length-delimited with a positive field
// number, which reads as the clear type with a nonzero key).
// The state is returned by value: it is decoded once per record read.
func DecodeTransformPrefix(data []byte) (st TransformState, prefixed bool, err error) {
	var prefix uint64
	n := 0
	for {
		if n >= len(data) {
			return TransformState{}, false, &SerializationError{Message: "transformation prefix malformed"}
		}
		b := data[n]
		if n == 9 && b&0xFE != 0 {
			return TransformState{}, false, &SerializationError{Message: "transformation prefix too long"}
		}
		prefix |= uint64(b&0x7F) << (7 * n)
		n++
		if b&0x80 == 0 {
			break
		}
	}
	typ := prefix & TransformTypeMask
	// Java shifts the signed long arithmetically; a remaining key of 2^63 or
	// more is negative there, and out of the int range either way.
	remaining := int64(prefix) >> TransformKeyShift //nolint:gosec
	if typ == TransformPrefixClear && remaining != 0 {
		return TransformState{}, false, nil
	}
	st = TransformState{Data: data[n:]}
	valid := true
	switch typ {
	case TransformPrefixClear:
	case TransformPrefixCompressedThenEncrypted:
		st.Encrypted, st.Compressed = true, true
	case TransformPrefixEncrypted:
		st.Encrypted = true
	case TransformPrefixCompressed:
		st.Compressed = true
	default:
		valid = false
	}
	if st.Encrypted {
		if remaining < -1<<31 || remaining > 1<<31-1 {
			valid = false
		} else {
			st.KeyNumber = int32(remaining)
		}
	} else if remaining != 0 {
		valid = false
	}
	if !valid {
		return TransformState{}, false, &SerializationError{Message: "unrecognized transformation encoding"}
	}
	return st, true, nil
}
