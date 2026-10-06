package recordlayer

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/dst"
)

// Java's TransformedRecordSerializerPrefix constants
// (TransformedRecordSerializerPrefix.java:68-74): the low three bits of the
// varint prefix are the transformation, the rest the encryption key number.
const (
	transformPrefixEncrypted               = 1
	transformPrefixClear                   = 2
	transformPrefixCompressed              = 4
	transformPrefixCompressedThenEncrypted = 5
	transformTypeMask                      = 0x07
	transformKeyShift                      = 3
)

// Java's compression header (TransformedRecordSerializer.java:67-68): the
// version byte, then the uncompressed length as a big-endian int.
const (
	minCompressionVersion = 1
	maxCompressionVersion = 1
	compressionHeaderSize = 5
)

// DefaultCipher is Java's CipherPool.DEFAULT_CIPHER, and the one cipher Go
// implements.
const DefaultCipher = "AES/CBC/PKCS5Padding"

// cipherIVSize is CipherPool.IV_SIZE: an encrypted body begins with the IV.
const cipherIVSize = 16

// Java's java.util.zip.Deflater levels, which compress/zlib shares.
const (
	DeflaterDefaultCompression = -1
	DeflaterBestCompression    = 9
)

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

// FixedZeroKeyManager is Java's FixedZeroKeyManager: one key, served as key
// number 0 only.
type FixedZeroKeyManager struct {
	key        []byte
	cipherName string
	random     io.Reader
}

// NewFixedZeroKeyManager returns a key manager serving key as key number 0,
// with cipherName (DefaultCipher when empty) and random (the writing store's
// randomness when nil).
func NewFixedZeroKeyManager(key []byte, cipherName string, random io.Reader) *FixedZeroKeyManager {
	if cipherName == "" {
		cipherName = DefaultCipher
	}
	return &FixedZeroKeyManager{key: append([]byte(nil), key...), cipherName: cipherName, random: random}
}

func (m *FixedZeroKeyManager) GetSerializationKey() int32 { return 0 }

func (m *FixedZeroKeyManager) GetKey(keyNumber int32) ([]byte, error) {
	if keyNumber != 0 {
		return nil, serializationError("only provide key number 0")
	}
	return m.key, nil
}

func (m *FixedZeroKeyManager) GetCipher(keyNumber int32) (string, error) {
	if keyNumber != 0 {
		return "", serializationError("only provide key number 0")
	}
	return m.cipherName, nil
}

func (m *FixedZeroKeyManager) GetRandom(keyNumber int32) (io.Reader, error) {
	if keyNumber != 0 {
		return nil, serializationError("only provide key number 0")
	}
	return m.random, nil
}

// TransformedRecordSerializer is Java's TransformedRecordSerializer and, built
// with NewTransformedRecordSerializerJCEBuilder, TransformedRecordSerializerJCE:
// a record's serialized union message is compressed, then encrypted, then
// prefixed on the way out, and the reverse on the way in
// (TransformedRecordSerializer.java:213-240, :360-406). A store configured with
// one writes through it (StoreBuilder.SetSerializer); every store reads through
// it, whatever it writes with (readStoredRecord).
type TransformedRecordSerializer struct {
	compressWhenSerializing        bool
	compressionLevel               int
	encryptWhenSerializing         bool
	writeValidationRatio           float64
	writeEncryptionValidationRatio float64
	failOnDeserializeReattempt     bool
	deserializeReattemptCount      int
	keyManager                     SerializationKeyManager
	// jce is whether this is Java's JCE subclass, which can encrypt when it
	// has a key manager; the plain class cannot, and says so.
	jce bool
	// zlib is where compress and decompress take their zlib state from; nil
	// is the process-wide lists. A test gives one serializer private lists to
	// observe its reuse.
	zlib *zlibFreeLists
}

func (s *TransformedRecordSerializer) zlibLists() *zlibFreeLists {
	if s.zlib != nil {
		return s.zlib
	}
	return zlibState
}

// TransformedRecordSerializerBuilder is Java's TransformedRecordSerializer.Builder
// (and TransformedRecordSerializerJCE.Builder for a JCE builder).
type TransformedRecordSerializerBuilder struct {
	s             TransformedRecordSerializer
	encryptionKey []byte
	cipherName    string
	random        io.Reader
}

// NewTransformedRecordSerializerBuilder is TransformedRecordSerializer.newDefaultBuilder:
// no compression, level BEST_COMPRESSION, no encryption.
func NewTransformedRecordSerializerBuilder() *TransformedRecordSerializerBuilder {
	return &TransformedRecordSerializerBuilder{s: TransformedRecordSerializer{compressionLevel: DeflaterBestCompression}}
}

// NewTransformedRecordSerializerJCEBuilder is TransformedRecordSerializerJCE.newDefaultBuilder.
func NewTransformedRecordSerializerJCEBuilder() *TransformedRecordSerializerBuilder {
	b := NewTransformedRecordSerializerBuilder()
	b.s.jce = true
	return b
}

func (b *TransformedRecordSerializerBuilder) SetCompressWhenSerializing(v bool) *TransformedRecordSerializerBuilder {
	b.s.compressWhenSerializing = v
	return b
}

// SetCompressionLevel takes Java's Deflater levels: -1 (the default) or 0-9.
func (b *TransformedRecordSerializerBuilder) SetCompressionLevel(level int) *TransformedRecordSerializerBuilder {
	b.s.compressionLevel = level
	return b
}

func (b *TransformedRecordSerializerBuilder) SetEncryptWhenSerializing(v bool) *TransformedRecordSerializerBuilder {
	b.s.encryptWhenSerializing = v
	return b
}

func (b *TransformedRecordSerializerBuilder) SetWriteValidationRatio(r float64) *TransformedRecordSerializerBuilder {
	b.s.writeValidationRatio = r
	return b
}

func (b *TransformedRecordSerializerBuilder) SetWriteEncryptionValidationRatio(r float64) *TransformedRecordSerializerBuilder {
	b.s.writeEncryptionValidationRatio = r
	return b
}

func (b *TransformedRecordSerializerBuilder) SetFailOnDeserializeReattempt(v bool) *TransformedRecordSerializerBuilder {
	b.s.failOnDeserializeReattempt = v
	return b
}

func (b *TransformedRecordSerializerBuilder) SetDeserializeReattemptCount(n int) *TransformedRecordSerializerBuilder {
	b.s.deserializeReattemptCount = n
	return b
}

// SetKeyManager, SetEncryptionKey, SetCipherName and SetRandom are the JCE
// builder's; a key manager and the three single-key settings are exclusive, as
// Java's resolveKeyManager has them (TransformedRecordSerializerJCE.java:325-349).
func (b *TransformedRecordSerializerBuilder) SetKeyManager(km SerializationKeyManager) *TransformedRecordSerializerBuilder {
	b.s.keyManager = km
	return b
}

func (b *TransformedRecordSerializerBuilder) SetEncryptionKey(key []byte) *TransformedRecordSerializerBuilder {
	b.encryptionKey = append([]byte(nil), key...)
	return b
}

func (b *TransformedRecordSerializerBuilder) SetCipherName(name string) *TransformedRecordSerializerBuilder {
	b.cipherName = name
	return b
}

func (b *TransformedRecordSerializerBuilder) SetRandom(r io.Reader) *TransformedRecordSerializerBuilder {
	b.random = r
	return b
}

// Build is Java's build(): the plain class refuses to encrypt, and the JCE
// class resolves its key manager (resolveKeyManager). A compression level
// outside Deflater's range is not refused here; as in Java, the first record
// compressed fails (compress).
func (b *TransformedRecordSerializerBuilder) Build() (*TransformedRecordSerializer, error) {
	s := b.s
	if !s.jce {
		// TransformedRecordSerializer.Builder.build (:568-571): the plain
		// class has no key, so it refuses to be built encrypting.
		if s.encryptWhenSerializing {
			return nil, &RecordCoreArgumentError{Message: "cannot encrypt when serializing using this class"}
		}
		return &s, nil
	}
	if s.keyManager != nil {
		switch {
		case b.encryptionKey != nil:
			return nil, &RecordCoreArgumentError{Message: "cannot specify both key manager and encryption key"}
		case b.cipherName != "":
			return nil, &RecordCoreArgumentError{Message: "cannot specify both key manager and cipher name"}
		case b.random != nil:
			return nil, &RecordCoreArgumentError{Message: "cannot specify both key manager and secure random"}
		}
		return &s, nil
	}
	if b.encryptionKey != nil {
		s.keyManager = NewFixedZeroKeyManager(b.encryptionKey, b.cipherName, b.random)
		return &s, nil
	}
	// resolveKeyManager's last arm (TransformedRecordSerializerJCE.java:345-349).
	if s.encryptWhenSerializing {
		return nil, &RecordCoreArgumentError{Message: "cannot encrypt when serializing if encryption key is not set"}
	}
	return &s, nil
}

// serializationError is Java's RecordSerializationException with its message.
func serializationError(message string) error {
	return &RecordSerializationError{Message: message}
}

// securityError is a cipher failure, Java's GeneralSecurityException, as
// encrypt's and decrypt's callers wrap it: op is "encryption error" or
// "decryption error".
func securityError(op string, cause error) *RecordSerializationError {
	return &RecordSerializationError{Message: op, Cause: cause, generalSecurity: true}
}

// transformState is TransformedRecordSerializerState: the data, and what the
// prefix said of it.
type transformState struct {
	data       []byte
	compressed bool
	encrypted  bool
	keyNumber  int32
}

// unionReader deserializes a union message's bytes into the record they hold,
// as a store's inner serializer does.
type unionReader func(union []byte) (proto.Message, error)

// transformRead is Java's serialize after the inner serializer: compress,
// encrypt, then prefix (TransformedRecordSerializer.java:213-240). env is the
// writing store's: the IV of a key manager without a random source, and the
// validation samples, are drawn from it. read is the store's union reader,
// through which the write-time serialization validation deserializes what it
// reads back (RecordSerializer.validateSerialization).
func (s *TransformedRecordSerializer) transformRead(union []byte, env *dst.Env, read unionReader) ([]byte, error) {
	st := &transformState{data: union}
	if s.compressWhenSerializing {
		if err := s.compress(st); err != nil {
			return nil, err
		}
	}
	if s.encryptWhenSerializing {
		beforeEncrypt := st.data
		if err := s.encrypt(st, env); err != nil {
			return nil, err
		}
		if shouldIncludeInSample(s.writeEncryptionValidationRatio, env) {
			// Java decrypts with a fresh state, whose key number is 0
			// (TransformedRecordSerializer.validateEncryption), so it fails a
			// validated write under any other key. Go decrypts with the key the
			// record was written under: the bytes are the same, and Go accepts
			// the write Java refuses (DIVERGENCES.md).
			// Only a cipher failure is the validation's; a key manager's own
			// refusal propagates as it is, as Java catches only
			// GeneralSecurityException there.
			verify := &transformState{data: st.data, encrypted: true, keyNumber: st.keyNumber}
			if err := s.decrypt(verify); err != nil {
				var se *RecordSerializationError
				if errors.As(err, &se) && se.generalSecurity {
					return nil, &RecordSerializationValidationError{Message: "encryption validation error: decryption failed", Cause: se.Cause}
				}
				return nil, err
			}
			if !bytes.Equal(verify.data, beforeEncrypt) {
				return nil, &RecordSerializationValidationError{Message: "encryption validation error: decrypted bytes do not match original"}
			}
		}
	}
	out := encodeTransformPrefix(st)
	if shouldIncludeInSample(s.writeValidationRatio, env) {
		back, err := s.untransform(out)
		if err != nil {
			return nil, &RecordSerializationValidationError{Message: "cannot deserialize record", Cause: err}
		}
		if err := validateReadBack(union, back, read); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// validateReadBack is the rest of Java's RecordSerializer.validateSerialization
// (RecordSerializer.java:113-127) once the stored bytes are read back to a
// union: it deserializes them and compares the record with the one written
// (Objects.equals), so bytes that do not deserialize are "cannot deserialize
// record" and only a record that does and differs is "record serialization
// mismatch". Equal bytes are an equal record.
func validateReadBack(union, back []byte, read unionReader) error {
	if bytes.Equal(back, union) {
		return nil
	}
	if read == nil {
		return &RecordSerializationValidationError{Message: "cannot deserialize record", Cause: errors.New("no union reader")}
	}
	got, err := read(back)
	if err != nil {
		return &RecordSerializationValidationError{Message: "cannot deserialize record", Cause: err}
	}
	written, err := read(union)
	if err != nil {
		return &RecordSerializationValidationError{Message: "cannot deserialize record", Cause: err}
	}
	if !proto.Equal(got, written) {
		return &RecordSerializationValidationError{Message: "record serialization mismatch"}
	}
	return nil
}

// shouldIncludeInSample is Java's: always at a ratio of 1 or more, never at 0
// or less, else with probability ratio. The coin comes from env, so a seeded
// run validates the same writes.
func shouldIncludeInSample(ratio float64, env *dst.Env) bool {
	if ratio >= 1.0 {
		return true
	}
	if ratio <= 0.0 {
		return false
	}
	var b [8]byte
	if _, err := env.Read(b[:]); err != nil {
		return true // validating a write that need not be is the safe side
	}
	return float64(binary.BigEndian.Uint64(b[:])>>11)/(1<<53) < ratio
}

// untransform is Java's deserialize before the inner serializer: a bare union
// message is returned as it is; a prefixed one is decrypted, then decompressed,
// with Java's retries (TransformedRecordSerializer.java:360-406).
func (s *TransformedRecordSerializer) untransform(stored []byte) ([]byte, error) {
	st, prefixed, err := decodeTransformPrefix(stored)
	if err != nil || !prefixed {
		return stored, err
	}
	var lastErr error
	succeededAt := -1
	var out []byte
	for attempt := 0; attempt <= s.deserializeReattemptCount; attempt++ {
		attemptState := st
		if attempt != 0 {
			attemptState, _, err = decodeTransformPrefix(stored)
			if err != nil {
				return nil, err
			}
		}
		if err := s.decryptAndDecompress(&attemptState); err != nil {
			lastErr = err
			continue
		}
		succeededAt = attempt
		out = attemptState.data
		break
	}
	switch {
	case succeededAt < 0:
		// Java rethrows the last failure with RETRY_COUNT and RESULT=failure
		// added; a serialization error carries them, any other error (a key
		// manager's own) is returned as it is.
		if rse, ok := lastErr.(*RecordSerializationError); ok {
			annotated := *rse
			annotated.RetryCount, annotated.RetryResult = s.deserializeReattemptCount, "failure"
			return nil, &annotated
		}
		return nil, lastErr
	case succeededAt > 0 && s.failOnDeserializeReattempt:
		return nil, &RecordSerializationError{
			Message: "deserialization error", Cause: lastErr,
			RetryCount: succeededAt, RetryResult: "success",
		}
	}
	return out, nil
}

func (s *TransformedRecordSerializer) decryptAndDecompress(st *transformState) error {
	if st.encrypted {
		if err := s.decrypt(st); err != nil {
			return err
		}
	}
	if st.compressed {
		return decompressTransformed(st, s.zlibLists())
	}
	return nil
}

// compress is Java's compress (TransformedRecordSerializer.java:125-170): only
// a body of more than five bytes, and only when the deflated form is strictly
// shorter than the body less the five-byte header.
func (s *TransformedRecordSerializer) compress(st *transformState) error {
	if len(st.data) <= compressionHeaderSize {
		return nil
	}
	// Java constructs its Deflater here, whose constructor refuses a level
	// outside [-1, 9] with IllegalArgumentException "invalid compression level".
	if s.compressionLevel < DeflaterDefaultCompression || s.compressionLevel > DeflaterBestCompression {
		return &IllegalArgumentError{Message: "invalid compression level"}
	}
	var buf bytes.Buffer
	lists := s.zlibLists()
	w, err := lists.acquireWriter(&buf, s.compressionLevel)
	if err != nil {
		return &RecordSerializationError{Cause: err}
	}
	defer lists.releaseWriter(w, s.compressionLevel)
	if _, err := w.Write(st.data); err != nil {
		return &RecordSerializationError{Cause: err}
	}
	if err := w.Close(); err != nil {
		return &RecordSerializationError{Cause: err}
	}
	if buf.Len() >= len(st.data)-compressionHeaderSize {
		st.compressed = false
		return nil
	}
	out := make([]byte, compressionHeaderSize+buf.Len())
	out[0] = maxCompressionVersion
	binary.BigEndian.PutUint32(out[1:5], uint32(len(st.data))) //nolint:gosec
	copy(out[compressionHeaderSize:], buf.Bytes())
	st.data = out
	st.compressed = true
	return nil
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

var zlibState = newZlibFreeLists(zlibWriterFreeListSize, zlibReaderFreeListSize)

// zlibFreeLists keeps released zlib writers (one list per compression level) and
// readers for reuse.
type zlibFreeLists struct {
	writers [DeflaterBestCompression - DeflaterDefaultCompression + 1]chan *zlib.Writer
	readers chan io.ReadCloser
}

func newZlibFreeLists(writers, readers int) *zlibFreeLists {
	l := &zlibFreeLists{readers: make(chan io.ReadCloser, readers)}
	for i := range l.writers {
		l.writers[i] = make(chan *zlib.Writer, writers)
	}
	return l
}

// acquireWriter returns a zlib writer at level over dst, reused when one is
// free. The level has been range-checked by the caller.
func (l *zlibFreeLists) acquireWriter(dst io.Writer, level int) (*zlib.Writer, error) {
	select {
	case w := <-l.writers[level-DeflaterDefaultCompression]:
		w.Reset(dst)
		return w, nil
	default:
		return zlib.NewWriterLevel(dst, level)
	}
}

func (l *zlibFreeLists) releaseWriter(w *zlib.Writer, level int) {
	w.Reset(nil)
	select {
	case l.writers[level-DeflaterDefaultCompression] <- w:
	default:
	}
}

// acquireReader returns a zlib reader over src with its header read, reused
// when one is free. A header error is returned as zlib.NewReader returns it,
// and the reader goes back on the list.
func (l *zlibFreeLists) acquireReader(src io.Reader) (io.ReadCloser, error) {
	select {
	case r := <-l.readers:
		if err := r.(zlib.Resetter).Reset(src, nil); err != nil {
			l.releaseReader(r)
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

func (l *zlibFreeLists) releaseReader(r io.ReadCloser) {
	// Drop the reference to the last record's compressed bytes. The inflate
	// history (up to 32 KiB of what this reader decompressed, not only the last
	// record) stays in the reused buffer until it is overwritten; the design
	// states that retention.
	_ = r.(zlib.Resetter).Reset(bytes.NewReader(zlibEmptyHeader), nil)
	select {
	case l.readers <- r:
	default:
	}
}

// decompressTransformed is Java's decompress (TransformedRecordSerializer.java:251-285).
func decompressTransformed(st *transformState, lists *zlibFreeLists) error {
	if len(st.data) < compressionHeaderSize {
		return serializationError("decompression error")
	}
	version := int8(st.data[0]) //nolint:gosec
	if version < minCompressionVersion || version > maxCompressionVersion {
		return serializationError("unknown compression version")
	}
	length := int32(binary.BigEndian.Uint32(st.data[1:5])) //nolint:gosec
	if length < 0 {
		return serializationError("invalid decompressed length")
	}
	in := bytes.NewReader(st.data[compressionHeaderSize:])
	r, err := lists.acquireReader(in)
	if err != nil {
		return &RecordSerializationError{Message: "decompression error", Cause: err}
	}
	defer lists.releaseReader(r)
	out := make([]byte, length)
	n, err := io.ReadFull(r, out)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return &RecordSerializationError{Message: "decompression error", Cause: err}
	}
	if n < int(length) {
		return serializationError("decompressed record too small")
	}
	// Input left over, output beyond the length or trailing bytes after the
	// stream: Java's Inflater reports them as remaining input.
	var one [1]byte
	m, err := r.Read(one[:])
	if m > 0 {
		return serializationError("decompressed record too large")
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
		return &RecordSerializationError{Message: "decompression error", Cause: err}
	}
	if in.Len() > 0 {
		return serializationError("decompressed record too large")
	}
	st.data = out
	return nil
}

// encrypt is TransformedRecordSerializerJCE.encrypt: the IV, then the cipher
// text of the body under the key manager's serialization key.
func (s *TransformedRecordSerializer) encrypt(st *transformState, env *dst.Env) error {
	if !s.jce {
		return serializationError("this serializer cannot encrypt")
	}
	if s.keyManager == nil {
		return serializationError("attempted to encrypt without setting key manager (cipher name and key)")
	}
	keyNumber := s.keyManager.GetSerializationKey()
	block, err := s.cipherBlock(keyNumber, "encryption error")
	if err != nil {
		return err
	}
	random, err := s.keyManager.GetRandom(keyNumber)
	if err != nil {
		return err
	}
	if random == nil {
		random = env
	}
	iv := make([]byte, cipherIVSize)
	if _, err := io.ReadFull(random, iv); err != nil {
		return &RecordSerializationError{Message: "encryption error", Cause: err}
	}
	padding := aes.BlockSize - len(st.data)%aes.BlockSize
	plain := make([]byte, len(st.data)+padding)
	copy(plain, st.data)
	for i := len(st.data); i < len(plain); i++ {
		plain[i] = byte(padding)
	}
	out := make([]byte, cipherIVSize+len(plain))
	copy(out, iv)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[cipherIVSize:], plain)
	st.data = out
	st.encrypted = true
	st.keyNumber = keyNumber
	return nil
}

// decrypt is TransformedRecordSerializerJCE.decrypt.
func (s *TransformedRecordSerializer) decrypt(st *transformState) error {
	if !s.jce {
		return serializationError("this serializer cannot decrypt")
	}
	if s.keyManager == nil {
		return serializationError("missing encryption key or provider during decryption")
	}
	block, err := s.cipherBlock(st.keyNumber, "decryption error")
	if err != nil {
		return err
	}
	// Shorter than the IV: Java's decrypt copies the IV out of it and fails
	// with an array bound exception, not a cipher failure (so it is not the
	// encryption validation's either).
	if len(st.data) < cipherIVSize {
		return &RecordSerializationError{Message: "decryption error", Cause: fmt.Errorf("%d bytes, shorter than the %d-byte IV", len(st.data), cipherIVSize)}
	}
	iv, text := st.data[:cipherIVSize], st.data[cipherIVSize:]
	if len(text) == 0 || len(text)%aes.BlockSize != 0 {
		return securityError("decryption error", errors.New("input length not multiple of 16 bytes"))
	}
	plain := make([]byte, len(text))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, text)
	padding := int(plain[len(plain)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(plain) {
		return securityError("decryption error", errors.New("bad padding"))
	}
	for _, p := range plain[len(plain)-padding:] {
		if int(p) != padding {
			return securityError("decryption error", errors.New("bad padding"))
		}
	}
	st.data = plain[:len(plain)-padding]
	return nil
}

// KeyAlgorithmReporter is implemented by a SerializationKeyManager that knows
// each key's algorithm, as a Java Key carries it (KeyStoreSerializationKeyManager
// does). Java's AES cipher refuses a key of another algorithm when it is
// initialized ("Wrong algorithm: AES or Rijndael required"); a manager that
// does not report algorithms hands Go raw AES keys.
type KeyAlgorithmReporter interface {
	KeyAlgorithm(keyNumber int32) (string, error)
}

// cipherBlock resolves key number's cipher and key, as Java's encrypt and
// decrypt do inside their try: a key manager's own refusal ("only provide key
// number 0", "cannot load key") propagates as it is, and a security failure
// (a cipher the JCE does not provide, a key it rejects) is Java's
// GeneralSecurityException, wrapped as op, "encryption error" or "decryption
// error". Transformation names match case-insensitively, as
// Cipher.getInstance matches them.
func (s *TransformedRecordSerializer) cipherBlock(keyNumber int32, op string) (cipher.Block, error) {
	name, err := s.keyManager.GetCipher(keyNumber)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(name, DefaultCipher) {
		return nil, securityError(op, fmt.Errorf("unsupported cipher %s: Go implements %s only", name, DefaultCipher))
	}
	key, err := s.keyManager.GetKey(keyNumber)
	if err != nil {
		return nil, err
	}
	if reporter, ok := s.keyManager.(KeyAlgorithmReporter); ok {
		algorithm, err := reporter.KeyAlgorithm(keyNumber)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(algorithm, "AES") && !strings.EqualFold(algorithm, "Rijndael") {
			return nil, securityError(op, fmt.Errorf("Wrong algorithm: AES or Rijndael required (key algorithm %s)", algorithm))
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, securityError(op, err)
	}
	return block, nil
}

// encodeTransformPrefix is TransformedRecordSerializerPrefix.encodePrefix.
func encodeTransformPrefix(st *transformState) []byte {
	var prefix uint64
	if !st.compressed && !st.encrypted {
		prefix = transformPrefixClear
	} else {
		if st.compressed {
			prefix |= transformPrefixCompressed
		}
		if st.encrypted {
			prefix |= transformPrefixEncrypted
			prefix |= uint64(st.keyNumber) << transformKeyShift //nolint:gosec
		}
	}
	out := make([]byte, 0, len(st.data)+10)
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
	return append(out, st.data...)
}

// decodeTransformPrefix is TransformedRecordSerializerPrefix.decodePrefix and
// readVarint: prefixed reports whether the data carries a prefix at all (a
// bare union message's first field is length-delimited with a positive field
// number, which reads as the clear type with a nonzero key).
// The state is returned by value: it is decoded once per record read.
func decodeTransformPrefix(data []byte) (st transformState, prefixed bool, err error) {
	var prefix uint64
	n := 0
	for {
		if n >= len(data) {
			return transformState{}, false, serializationError("transformation prefix malformed")
		}
		b := data[n]
		if n == 9 && b&0xFE != 0 {
			return transformState{}, false, serializationError("transformation prefix too long")
		}
		prefix |= uint64(b&0x7F) << (7 * n)
		n++
		if b&0x80 == 0 {
			break
		}
	}
	typ := prefix & transformTypeMask
	// Java shifts the signed long arithmetically; a remaining key of 2^63 or
	// more is negative there, and out of the int range either way.
	remaining := int64(prefix) >> transformKeyShift //nolint:gosec
	if typ == transformPrefixClear && remaining != 0 {
		return transformState{}, false, nil
	}
	st = transformState{data: data[n:]}
	valid := true
	switch typ {
	case transformPrefixClear:
	case transformPrefixCompressedThenEncrypted:
		st.encrypted, st.compressed = true, true
	case transformPrefixEncrypted:
		st.encrypted = true
	case transformPrefixCompressed:
		st.compressed = true
	default:
		valid = false
	}
	if st.encrypted {
		if remaining < -1<<31 || remaining > 1<<31-1 {
			valid = false
		} else {
			st.keyNumber = int32(remaining)
		}
	} else if remaining != 0 {
		valid = false
	}
	if !valid {
		return transformState{}, false, serializationError("unrecognized transformation encoding")
	}
	return st, true, nil
}

// plainTransformedReader is the reader of a store that writes with no
// serializer: it decodes every prefix but cannot decrypt.
var plainTransformedReader = &TransformedRecordSerializer{compressionLevel: DeflaterBestCompression}
