// Portions derived from FoundationDB Record Layer (
// TransformedRecordSerializer.java, TransformedRecordSerializerJCE.java,
// TransformedRecordSerializerPrefix.java, SerializationKeyManager.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"errors"
	"io"

	"google.golang.org/protobuf/proto"

	"fdb.dev/internal/recordlayer/rlcrypto"
)

// DefaultCipher is Java's CipherPool.DEFAULT_CIPHER. Go implements every AES
// transformation Java's serializer can run (internal/recordlayer/rlcrypto/jce_cipher.go).
const DefaultCipher = rlcrypto.DefaultCipher

// Java's java.util.zip.Deflater levels, which compress/zlib shares.
const (
	DeflaterDefaultCompression = rlcrypto.DeflaterDefaultCompression
	DeflaterBestCompression    = rlcrypto.DeflaterBestCompression
)

// SerializationKeyManager is Java's SerializationKeyManager: the key number a
// record is written with, and per key number the key, the cipher name and the
// random source the IV is drawn from. A nil random source (Java always has a
// SecureRandom) is the writing store's randomness: its DST env's, crypto/rand
// in production, so a seeded simulation replays the IV like every other
// persisted random byte.
type SerializationKeyManager = rlcrypto.SerializationKeyManager

// KeyAlgorithmReporter is implemented by a SerializationKeyManager that knows
// each key's algorithm, as a Java Key carries it (KeyStoreSerializationKeyManager
// does). Java's AES cipher refuses a key of another algorithm when it is
// initialized ("Wrong algorithm: AES or Rijndael required"); a manager that
// does not report algorithms hands Go raw AES keys.
type KeyAlgorithmReporter = rlcrypto.KeyAlgorithmReporter

// FixedZeroKeyManager is Java's FixedZeroKeyManager: one key, served as key number 0 only.
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
	zlib *rlcrypto.ZlibFreeLists
	// Brotli is an explicit Go-only format; unset readers reject it.
	goOnlyBrotliCompression bool
	brotliCompressionLevel  int
	brotli                  *rlcrypto.BrotliFreeLists
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

// SetGoOnlyBrotliCompression opts into Java-incompatible version 2 reads/writes at levels 0..4.
// Disabling compression preserves read permission; SetCompressionLevel still selects only zlib level.
func (b *TransformedRecordSerializerBuilder) SetGoOnlyBrotliCompression(level int) *TransformedRecordSerializerBuilder {
	b.s.goOnlyBrotliCompression = true
	b.s.brotliCompressionLevel = level
	b.s.compressWhenSerializing = true
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
	if s.goOnlyBrotliCompression && (s.brotliCompressionLevel < 0 || s.brotliCompressionLevel > rlcrypto.MaxBrotliCompressionLevel) {
		return nil, &RecordCoreArgumentError{Message: "Go-only Brotli compression level must be 0 through 4"}
	}
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

func wrapCryptoError(err error) error {
	if err == nil {
		return nil
	}
	// Direct comparison and type assertion (instead of errors.Is/errors.As)
	// are intentional so a key-manager error's custom As() method is not invoked
	// here before store.readStoredRecord inspects it (pinned by
	// TestReadStoredRecordErrorAnnotation).
	if err == rlcrypto.ErrInvalidCompressionLevel {
		return &IllegalArgumentError{Message: "invalid compression level"}
	}
	if se, ok := err.(*rlcrypto.SerializationError); ok {
		return &RecordSerializationError{
			Message:         se.Message,
			Cause:           se.Cause,
			generalSecurity: se.GeneralSecurity,
		}
	}
	return err
}

type transformState = rlcrypto.TransformState

// unionReader deserializes a union message's bytes into the record they hold,
// as a store's inner serializer does.
type unionReader func(union []byte) (proto.Message, error)

// transformRead is Java's serialize after the inner serializer: compress,
// encrypt, then prefix (TransformedRecordSerializer.java:213-240). env is the
// writing store's: the IV of a key manager without a random source, and the
// validation samples, are drawn from it. read is the store's union reader,
// through which the write-time serialization validation deserializes what it
// reads back (RecordSerializer.validateSerialization).
func (s *TransformedRecordSerializer) transformRead(union []byte, env *Env, read unionReader) ([]byte, error) {
	st := &transformState{Data: union}
	if s.compressWhenSerializing {
		if err := s.compress(st); err != nil {
			return nil, err
		}
	}
	if s.encryptWhenSerializing {
		beforeEncrypt := st.Data
		if err := s.encrypt(st, env); err != nil {
			return nil, err
		}
		if rlcrypto.ShouldIncludeInSample(s.writeEncryptionValidationRatio, env) {
			// Java decrypts with a fresh state, whose key number is 0
			// (TransformedRecordSerializer.validateEncryption), so it fails a
			// validated write under any other key. Go decrypts with the key the
			// record was written under: the bytes are the same, and Go accepts
			// the write Java refuses (DIVERGENCES.md).
			// Only a cipher failure is the validation's; a key manager's own
			// refusal propagates as it is, as Java catches only
			// GeneralSecurityException there.
			verify := &transformState{Data: st.Data, Encrypted: true, KeyNumber: st.KeyNumber}
			if err := s.decrypt(verify); err != nil {
				var se *RecordSerializationError
				if errors.As(err, &se) && se.generalSecurity {
					return nil, &RecordSerializationValidationError{Message: "encryption validation error: decryption failed", Cause: se.Cause}
				}
				return nil, err
			}
			if !bytes.Equal(verify.Data, beforeEncrypt) {
				return nil, &RecordSerializationValidationError{Message: "encryption validation error: decrypted bytes do not match original"}
			}
		}
	}
	out := rlcrypto.EncodeTransformPrefix(st)
	if rlcrypto.ShouldIncludeInSample(s.writeValidationRatio, env) {
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

// untransform is Java's deserialize before the inner serializer: a bare union
// message is returned as it is; a prefixed one is decrypted, then decompressed,
// with Java's retries (TransformedRecordSerializer.java:360-406).
func (s *TransformedRecordSerializer) untransform(stored []byte) ([]byte, error) {
	st, prefixed, err := rlcrypto.DecodeTransformPrefix(stored)
	if err != nil || !prefixed {
		return stored, wrapCryptoError(err)
	}
	var lastErr error
	succeededAt := -1
	var out []byte
	for attempt := 0; attempt <= s.deserializeReattemptCount; attempt++ {
		attemptState := st
		if attempt != 0 {
			attemptState, _, err = rlcrypto.DecodeTransformPrefix(stored)
			if err != nil {
				return nil, wrapCryptoError(err)
			}
		}
		if err := s.decryptAndDecompress(&attemptState); err != nil {
			lastErr = err
			continue
		}
		succeededAt = attempt
		out = attemptState.Data
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
	if st.Encrypted {
		if err := s.decrypt(st); err != nil {
			return err
		}
	}
	if st.Compressed {
		if len(st.Data) >= rlcrypto.CompressionHeaderSize && st.Data[0] == rlcrypto.BrotliCompressionVersion {
			if !s.goOnlyBrotliCompression {
				return serializationError("unknown compression version")
			}
			return wrapCryptoError(rlcrypto.DecompressBrotliTransformed(st, s.brotli))
		}
		return wrapCryptoError(rlcrypto.DecompressZlib(st, s.zlib))
	}
	return nil
}

func (s *TransformedRecordSerializer) compress(st *transformState) error {
	if s.goOnlyBrotliCompression {
		return wrapCryptoError(rlcrypto.CompressBrotli(st, s.brotliCompressionLevel, s.brotli))
	}
	return wrapCryptoError(rlcrypto.CompressZlib(st, s.compressionLevel, s.zlib))
}

func (s *TransformedRecordSerializer) encrypt(st *transformState, env *Env) error {
	return wrapCryptoError(rlcrypto.EncryptState(st, s.jce, s.keyManager, env))
}

func (s *TransformedRecordSerializer) decrypt(st *transformState) error {
	return wrapCryptoError(rlcrypto.DecryptState(st, s.jce, s.keyManager))
}

// plainTransformedReader is the reader of a store that writes with no
// serializer: it decodes every prefix but cannot decrypt.
var plainTransformedReader = &TransformedRecordSerializer{compressionLevel: DeflaterBestCompression}
