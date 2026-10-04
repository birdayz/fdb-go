package recordlayer

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	mathrand "math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"fdb.dev/pkg/dst"
)

// transform is transformRead with the reader of the unions these tests write
// (unionOf: a message holding one bytes field, read as a BytesValue).
func (s *TransformedRecordSerializer) transform(union []byte, env *dst.Env) ([]byte, error) {
	return s.transformRead(union, env, func(b []byte) (proto.Message, error) {
		m := &wrapperspb.BytesValue{}
		if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(b, m); err != nil {
			return nil, err
		}
		if len(m.ProtoReflect().GetUnknown()) != 0 {
			return nil, errors.New("unknown fields")
		}
		return m, nil
	})
}

// The tests below port TransformedRecordSerializerTest's cases over a union
// message's bytes (the transformation never looks inside them).

const sonnet108 = "What's in the brain that ink may character " +
	"Which hath not figured to thee my true spirit? " +
	"What's new to speak, what now to register, " +
	"That may express my love or thy dear merit? " +
	"Nothing, sweet boy; but yet, like prayers divine, " +
	"I must each day say o'er the very same, " +
	"Counting no old thing old, thou mine, I thine, " +
	"Even as when first I hallowed thy fair name."

// unionOf is a union message holding inner as record field 1.
func unionOf(inner string) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte(inner))
}

func mustSerializer(t *testing.T, b *TransformedRecordSerializerBuilder) *TransformedRecordSerializer {
	t.Helper()
	s, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func wantSerializationError(t *testing.T, err error, contains string) {
	t.Helper()
	var se *RecordSerializationError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), contains) {
		t.Fatalf("error = %v, want a RecordSerializationError containing %q", err, contains)
	}
}

// readOldFormat, noTransformations: a bare union message reads as itself; with
// no transformation the stored form is 02 then the message.
func TestTransformedSerializer_NoTransformations(t *testing.T) {
	t.Parallel()
	s := mustSerializer(t, NewTransformedRecordSerializerBuilder())
	union := unionOf("x")
	back, err := s.untransform(union)
	if err != nil || !bytes.Equal(back, union) {
		t.Fatalf("a bare union message read as %x, %v", back, err)
	}
	stored, err := s.transform(union, nil)
	if err != nil || stored[0] != transformPrefixClear || !bytes.Equal(stored[1:], union) {
		t.Fatalf("no transformation stored %x, %v; want 02 then the message", stored, err)
	}
	back, err = s.untransform(stored)
	if err != nil || !bytes.Equal(back, union) {
		t.Fatalf("02-prefixed read as %x, %v", back, err)
	}
}

// compressSmallRecordWhenSerializing: a body of five bytes or fewer, or one
// deflate does not shrink below the body less the header, stays clear.
func TestTransformedSerializer_CompressionIsKeptOnlyWhenItShrinks(t *testing.T) {
	t.Parallel()
	s := mustSerializer(t, NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true))
	for _, union := range [][]byte{unionOf("abc"), unionOf("abcd"), unionOf("z!Q")} {
		stored, err := s.transform(union, nil)
		if err != nil || stored[0] != transformPrefixClear {
			t.Fatalf("a %d-byte union compressed: %x, %v", len(union), stored, err)
		}
	}
	random := make([]byte, 200)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	stored, err := s.transform(unionOf(string(random)), nil)
	if err != nil || stored[0] != transformPrefixClear {
		t.Fatalf("an incompressible union compressed: prefix %x, %v", stored[0], err)
	}
	union := unionOf(sonnet108 + sonnet108)
	stored, err = s.transform(union, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stored[0] != transformPrefixCompressed || stored[1] != maxCompressionVersion ||
		binary.BigEndian.Uint32(stored[2:6]) != uint32(len(union)) || len(stored) >= len(union) { //nolint:gosec
		t.Fatalf("a compressible union stored as prefix %x version %x length %d (%d bytes of %d)",
			stored[0], stored[1], binary.BigEndian.Uint32(stored[2:6]), len(stored), len(union))
	}
	// The zlib header's FDICT bit is clear and a checksum is present: the
	// stream finished (Java's deflater.finish()).
	if back, err := s.untransform(stored); err != nil || !bytes.Equal(back, union) {
		t.Fatalf("compressed union read back as %d bytes, %v", len(back), err)
	}
	for _, level := range []int{DeflaterDefaultCompression, 0, 1, 9} {
		ls := mustSerializer(t, NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true).SetCompressionLevel(level))
		stored, err := ls.transform(union, nil)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		if back, err := ls.untransform(stored); err != nil || !bytes.Equal(back, union) {
			t.Fatalf("level %d round trip: %v", level, err)
		}
	}
	// A level outside Deflater's range builds, as in Java, and the first
	// record compressed fails ("invalid compression level", Deflater's
	// IllegalArgumentException); one too short to compress does not reach it.
	bad := mustSerializer(t, NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true).SetCompressionLevel(10))
	_, err = bad.transform(union, nil)
	// Java's Deflater throws IllegalArgumentException, not a RecordCoreException.
	var arg *IllegalArgumentError
	if !errors.As(err, &arg) || arg.Message != "invalid compression level" {
		t.Fatalf("level 10: %v, want invalid compression level", err)
	}
	if _, err := bad.transform(unionOf(""), nil); err != nil {
		t.Fatalf("level 10 over a record too short to compress: %v", err)
	}
}

// decompressWithoutAdler: a stream flushed but never finished (no final block,
// no checksum), which an old Java writer stored, reads.
func TestTransformedSerializer_DecompressWithoutAdler(t *testing.T) {
	t.Parallel()
	union := unionOf(sonnet108)
	var deflated bytes.Buffer
	deflated.Write([]byte{0x78, 0xDA}) // zlib header, best compression, no dictionary
	fw, err := flate.NewWriter(&deflated, 9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(union); err != nil {
		t.Fatal(err)
	}
	if err := fw.Flush(); err != nil { // a sync flush, never Close: no final block, no Adler-32
		t.Fatal(err)
	}
	stored := []byte{transformPrefixCompressed, maxCompressionVersion, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(stored[2:6], uint32(len(union))) //nolint:gosec
	stored = append(stored, deflated.Bytes()...)
	s := mustSerializer(t, NewTransformedRecordSerializerBuilder())
	back, err := s.untransform(stored)
	if err != nil || !bytes.Equal(back, union) {
		t.Fatalf("a record without its Adler-32 read as %d bytes, %v", len(back), err)
	}
}

func compressedSonnet(t *testing.T) (*TransformedRecordSerializer, []byte, []byte) {
	t.Helper()
	s := mustSerializer(t, NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true).SetCompressionLevel(9))
	union := unionOf(sonnet108)
	stored, err := s.transform(union, nil)
	if err != nil || stored[0] != transformPrefixCompressed {
		t.Fatalf("setup: %x, %v", stored[:1], err)
	}
	return s, union, stored
}

// unknownCompressionVersion, decompressionError, incorrectUncompressedSize.
func TestTransformedSerializer_DecompressionRefusals(t *testing.T) {
	t.Parallel()
	s, union, stored := compressedSonnet(t)
	mutate := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), stored...)) }
	for _, c := range []struct {
		name, want string
		data       []byte
	}{
		{"version bumped", "unknown compression version", mutate(func(b []byte) []byte { b[1]++; return b })},
		{"version zero", "unknown compression version", mutate(func(b []byte) []byte { b[1] = 0; return b })},
		{"negative length", "invalid decompressed length", mutate(func(b []byte) []byte { b[2] = 0x80; return b })},
		{"length one more", "decompressed record too small", mutate(func(b []byte) []byte {
			binary.BigEndian.PutUint32(b[2:6], uint32(len(union)+1)) //nolint:gosec
			return b
		})},
		{"length one less", "decompressed record too large", mutate(func(b []byte) []byte {
			binary.BigEndian.PutUint32(b[2:6], uint32(len(union)-1)) //nolint:gosec
			return b
		})},
		{"checksum corrupt", "decompression error", mutate(func(b []byte) []byte { b[len(b)-1]++; return b })},
		{"trailing garbage", "decompressed record too large", mutate(func(b []byte) []byte { return append(b, 0) })},
		{"header truncated", "decompression error", stored[:4]},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.untransform(c.data)
			wantSerializationError(t, err, c.want)
		})
	}
}

// unrecognizedEncoding, malformedVarintEncoding, invalidKeyNumberEncoding.
func TestTransformedSerializer_PrefixRefusals(t *testing.T) {
	t.Parallel()
	s := mustSerializer(t, NewTransformedRecordSerializerBuilder())
	pad := func(b []byte) []byte { return append(b, make([]byte, 10)...) }
	for _, c := range []struct {
		name, want string
		data       []byte
	}{
		{"type 7, key 1 (0x0f)", "unrecognized transformation encoding", pad([]byte{0x0f})},
		{"type 3", "unrecognized transformation encoding", pad([]byte{0x03})},
		{"type 0", "unrecognized transformation encoding", pad([]byte{0x00})},
		{"compressed with a key", "unrecognized transformation encoding", pad([]byte{0x0c})},
		{"six 0xFF", "transformation prefix malformed", bytes.Repeat([]byte{0xFF}, 6)},
		{"ten 0xFF", "transformation prefix too long", bytes.Repeat([]byte{0xFF}, 10)},
		{"empty", "transformation prefix malformed", nil},
		{
			"encrypted, key past int", "unrecognized transformation encoding",
			pad(protowire.AppendVarint(nil, transformPrefixEncrypted+(uint64(1<<31))<<3)),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.untransform(c.data)
			wantSerializationError(t, err, c.want)
		})
	}
}

// varintEncoding: the prefix is a protobuf varint.
func TestTransformedSerializer_PrefixIsAProtobufVarint(t *testing.T) {
	t.Parallel()
	r := mathrand.New(mathrand.NewPCG(1, 2))
	for i := 0; i < 200; i++ {
		key := int32(r.Uint32()) //nolint:gosec
		st := &transformState{data: []byte("x"), encrypted: true, compressed: i%2 == 0, keyNumber: key}
		out := encodeTransformPrefix(st)
		want := uint64(transformPrefixEncrypted) | uint64(key)<<transformKeyShift //nolint:gosec
		if st.compressed {
			want |= transformPrefixCompressed
		}
		if got := protowire.AppendVarint(nil, want); !bytes.Equal(out[:len(got)], got) {
			t.Fatalf("key %d: prefix %x, want the varint %x", key, out[:len(got)], got)
		}
		back, prefixed, err := decodeTransformPrefix(out)
		if err != nil || !prefixed || back.keyNumber != key || back.compressed != st.compressed || !back.encrypted ||
			string(back.data) != "x" {
			t.Fatalf("key %d decoded as %+v, %t, %v", key, back, prefixed, err)
		}
	}
}

func aesKey(t *testing.T, n int) []byte {
	t.Helper()
	k := make([]byte, n)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// encryptWhenSerializing: encrypted, and compressed then encrypted, round trip
// under a fixed key of each AES size; the prefix names the transformation and
// key 0.
func TestTransformedSerializer_EncryptWhenSerializing(t *testing.T) {
	t.Parallel()
	union := unionOf(sonnet108)
	for _, keySize := range []int{16, 24, 32} {
		for _, compress := range []bool{false, true} {
			s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().
				SetEncryptWhenSerializing(true).SetCompressWhenSerializing(compress).
				SetEncryptionKey(aesKey(t, keySize)).SetWriteValidationRatio(1).SetWriteEncryptionValidationRatio(1))
			stored, err := s.transform(union, nil)
			if err != nil {
				t.Fatalf("key %d compress %t: %v", keySize, compress, err)
			}
			want := byte(transformPrefixEncrypted)
			if compress {
				want = transformPrefixCompressedThenEncrypted
			}
			if stored[0] != want || (len(stored)-1-cipherIVSize)%16 != 0 {
				t.Fatalf("key %d compress %t: stored prefix %x, %d bytes", keySize, compress, stored[0], len(stored))
			}
			if bytes.Contains(stored, []byte("brain")) {
				t.Fatalf("key %d: the plain text is in the stored bytes", keySize)
			}
			back, err := s.untransform(stored)
			if err != nil || !bytes.Equal(back, union) {
				t.Fatalf("key %d compress %t read back %d bytes, %v", keySize, compress, len(back), err)
			}
		}
	}
}

// cannotDecryptWithoutKey, cannotDecryptUnknownKey, keyDoesNotMatchAlgorithm,
// buildWithoutSettingEncryption, invalidKeyManagerBuilder.
func TestTransformedSerializer_EncryptionRefusals(t *testing.T) {
	t.Parallel()
	key := aesKey(t, 16)
	enc := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).SetEncryptionKey(key))
	stored, err := enc.transform(unionOf(sonnet108), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mustSerializer(t, NewTransformedRecordSerializerBuilder()).untransform(stored)
	wantSerializationError(t, err, "this serializer cannot decrypt")
	_, err = plainTransformedReader.untransform(stored)
	wantSerializationError(t, err, "this serializer cannot decrypt")
	_, err = mustSerializer(t, NewTransformedRecordSerializerJCEBuilder()).untransform(stored)
	wantSerializationError(t, err, "missing encryption key or provider during decryption")
	wrong, err := keyWithBadPadding(stored, 16)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptionKey(wrong)).untransform(stored)
	wantSerializationError(t, err, "decryption error")

	// buildWithoutSettingEncryption and invalidKeyManagerBuilder: Java refuses
	// both at build() (RecordCoreArgumentException).
	for _, c := range []struct {
		b    *TransformedRecordSerializerBuilder
		want string
	}{
		{NewTransformedRecordSerializerBuilder().SetEncryptWhenSerializing(true), "cannot encrypt when serializing using this class"},
		{NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true), "cannot encrypt when serializing if encryption key is not set"},
		// A cipher name or a random source without a key is no key.
		{NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).SetCipherName(DefaultCipher), "cannot encrypt when serializing if encryption key is not set"},
	} {
		_, err := c.b.Build()
		var arg *RecordCoreArgumentError
		if !errors.As(err, &arg) || arg.Message != c.want {
			t.Errorf("Build: %v, want RecordCoreArgumentError %q", err, c.want)
		}
	}
	// The encrypt path's own refusals, which Build keeps a serializer from
	// reaching, are Java's defensive arms (TransformedRecordSerializer.encrypt,
	// TransformedRecordSerializerJCE.encrypt): driven on serializers built
	// without encryption and asked to encrypt.
	plainState := &transformState{data: []byte("x")}
	wantSerializationError(t, mustSerializer(t, NewTransformedRecordSerializerBuilder()).encrypt(plainState, nil), "this serializer cannot encrypt")
	wantSerializationError(t, mustSerializer(t, NewTransformedRecordSerializerJCEBuilder()).encrypt(plainState, nil),
		"attempted to encrypt without setting key manager (cipher name and key)")

	// A key number the manager does not serve.
	wrongKey := append([]byte(nil), stored...)
	wrongKey[0] = transformPrefixEncrypted | 1<<transformKeyShift
	_, err = enc.untransform(wrongKey)
	wantSerializationError(t, err, "only provide key number 0")

	// A cipher Go does not implement.
	other := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
		SetEncryptionKey(key).SetCipherName("AES/GCM/NoPadding"))
	_, err = other.transform(unionOf("x"), nil)
	wantSerializationError(t, err, "encryption error: unsupported cipher AES/GCM/NoPadding")
	// Transformation names match as Cipher.getInstance matches them,
	// case-insensitively; a bare "AES" is the JCE's ECB default, not this.
	lower := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
		SetEncryptionKey(key).SetCipherName("aes/cbc/pkcs5padding"))
	if _, err := lower.transform(unionOf("x"), nil); err != nil {
		t.Errorf("a lower-case transformation name: %v", err)
	}
	ecb := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
		SetEncryptionKey(key).SetCipherName("AES"))
	_, err = ecb.transform(unionOf("x"), nil)
	wantSerializationError(t, err, "encryption error: unsupported cipher AES")
	// The decrypt side wraps a security failure as "decryption error": a
	// stored record read under a key of no AES size.
	_, err = mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptionKey(aesKey(t, 7))).untransform(stored)
	wantSerializationError(t, err, "decryption error")
	// A key no AES size.
	_, err = mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
		SetEncryptionKey(aesKey(t, 7))).transform(unionOf("x"), nil)
	wantSerializationError(t, err, "encryption error")

	for _, b := range []*TransformedRecordSerializerBuilder{
		NewTransformedRecordSerializerJCEBuilder().SetKeyManager(NewFixedZeroKeyManager(key, "", nil)).SetEncryptionKey(key),
		NewTransformedRecordSerializerJCEBuilder().SetKeyManager(NewFixedZeroKeyManager(key, "", nil)).SetCipherName(DefaultCipher),
		NewTransformedRecordSerializerJCEBuilder().SetKeyManager(NewFixedZeroKeyManager(key, "", nil)).SetRandom(rand.Reader),
	} {
		var arg *RecordCoreArgumentError
		if _, err := b.Build(); !errors.As(err, &arg) || !strings.HasPrefix(arg.Message, "cannot specify both key manager and") {
			t.Errorf("a key manager mixed with a single-key setting built: %v", err)
		}
	}
}

// flakyKeyManager fails GetKey its first failures calls: a transient key
// service, which Java's reattempts are for.
type flakyKeyManager struct {
	*FixedZeroKeyManager
	failures int
}

func (m *flakyKeyManager) GetKey(keyNumber int32) ([]byte, error) {
	if m.failures > 0 {
		m.failures--
		return nil, serializationError("key service unavailable")
	}
	return m.FixedZeroKeyManager.GetKey(keyNumber)
}

// transientDecryptionThrowingFailureTest, permanentlyCorruptRecordTest.
func TestTransformedSerializer_DeserializeReattempts(t *testing.T) {
	t.Parallel()
	key := aesKey(t, 16)
	union := unionOf(sonnet108)
	stored, err := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
		SetCompressWhenSerializing(true).SetEncryptionKey(key)).transform(union, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		failures, reattempts int
		failOnReattempt      bool
		want                 string // "" reads
	}{
		{0, 0, false, ""},
		{1, 0, false, "key service unavailable"},
		{1, 1, false, ""},
		{2, 1, false, "key service unavailable"},
		{1, 1, true, "deserialization error"},
		{0, 3, true, ""},
	} {
		km := &flakyKeyManager{FixedZeroKeyManager: NewFixedZeroKeyManager(key, "", nil), failures: c.failures}
		s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetKeyManager(km).
			SetDeserializeReattemptCount(c.reattempts).SetFailOnDeserializeReattempt(c.failOnReattempt))
		back, err := s.untransform(stored)
		if c.want == "" {
			if err != nil || !bytes.Equal(back, union) {
				t.Errorf("%+v: read %d bytes, %v", c, len(back), err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c, err, c.want)
		}
	}
	// A permanently corrupt body fails every attempt.
	corrupt := append([]byte(nil), stored...)
	corrupt[1+cipherIVSize] ^= 0xFF
	s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptionKey(key).SetDeserializeReattemptCount(3))
	if _, err := s.untransform(corrupt); err == nil {
		t.Fatal("a corrupt encrypted body read")
	}
}

// corruptAnyBit: every single-bit corruption of a compressed record's deflated
// body (from byte 6, past the prefix, version and length) either reads the
// same record or fails with a decompression error, never a different record.
func TestTransformedSerializer_CorruptAnyBit(t *testing.T) {
	t.Parallel()
	s := mustSerializer(t, NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true).
		SetCompressionLevel(9).SetWriteValidationRatio(1))
	union := unionOf(sonnet108 + "\n" + sonnet108)
	stored, err := s.transform(union, nil)
	if err != nil || stored[0] != transformPrefixCompressed {
		t.Fatalf("setup: %v", err)
	}
	for i := 6; i < len(stored); i++ {
		for b := 0; b < 8; b++ {
			c := append([]byte(nil), stored...)
			c[i] ^= 1 << b
			back, err := s.untransform(c)
			if err == nil {
				if !bytes.Equal(back, union) {
					t.Fatalf("byte %d bit %d flipped read as another record", i, b)
				}
				continue
			}
			msg := err.Error()
			if !strings.Contains(msg, "decompression error") && !strings.Contains(msg, "decompressed record too small") &&
				!strings.Contains(msg, "decompressed record too large") {
				t.Fatalf("byte %d bit %d flipped: %v", i, b, err)
			}
		}
	}
}

// TestTransformedSerializer_RandomnessIsSeamed pins the DST seam: a key
// manager without a random source draws the IV from the writing store's env,
// so one seed stores the same bytes twice and another seed different ones; a
// nil env is production randomness. The validation sample's coin comes from
// the env too, so a seeded run validates the same writes.
func TestTransformedSerializer_RandomnessIsSeamed(t *testing.T) {
	t.Parallel()
	s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().
		SetEncryptWhenSerializing(true).SetEncryptionKey(aesKey(t, 16)))
	union := unionOf(sonnet108)
	write := func(env *dst.Env) []byte {
		t.Helper()
		out, err := s.transform(union, env)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if a, b := write(dst.NewSim(7)), write(dst.NewSim(7)); !bytes.Equal(a, b) {
		t.Errorf("one seed stored different IVs: %x vs %x", a[1:17], b[1:17])
	}
	if a, c := write(dst.NewSim(7)), write(dst.NewSim(8)); bytes.Equal(a, c) {
		t.Error("two seeds stored the same IV: the IV does not come from the env")
	}
	if d, e := write(nil), write(nil); bytes.Equal(d, e) {
		t.Error("two production writes stored the same IV: the crypto/rand fallback is not live")
	}
	// A random source the key manager sets is used as given.
	fixed := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
		SetEncryptionKey(aesKey(t, 16)).SetRandom(bytes.NewReader(bytes.Repeat([]byte{0xab}, 64))))
	out, err := fixed.transform(union, dst.NewSim(7))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[1:17], bytes.Repeat([]byte{0xab}, 16)) {
		t.Errorf("IV %x, want the key manager's own random source", out[1:17])
	}

	samples := func(seed uint64) int {
		env, n := dst.NewSim(seed), 0
		for range 256 {
			if shouldIncludeInSample(0.5, env) {
				n++
			}
		}
		return n
	}
	if a, b := samples(3), samples(3); a != b || a == 0 || a == 256 {
		t.Errorf("seeded validation samples %d and %d: want equal, and neither none nor all of 256", a, b)
	}
	if !shouldIncludeInSample(1, nil) || shouldIncludeInSample(0, nil) {
		t.Error("ratio 1 must always sample and ratio 0 never")
	}
}

// validationKeyManager serves keys by number with zero IVs, so its cipher
// texts are fixed. When later is set, every GetKey after the first serves it
// (or fails with laterErr): a record encrypted under one key and read back,
// by a write-time validation, under another.
type validationKeyManager struct {
	keys             map[int32][]byte
	serializationKey int32
	later            []byte
	laterErr         error
	calls            int
}

func (m *validationKeyManager) GetSerializationKey() int32 { return m.serializationKey }

func (m *validationKeyManager) GetKey(keyNumber int32) ([]byte, error) {
	m.calls++
	if m.calls > 1 && m.laterErr != nil {
		return nil, m.laterErr
	}
	if m.calls > 1 && m.later != nil {
		return m.later, nil
	}
	k, ok := m.keys[keyNumber]
	if !ok {
		return nil, serializationError("key number out of range")
	}
	return k, nil
}

func (m *validationKeyManager) GetCipher(int32) (string, error) { return DefaultCipher, nil }

func (m *validationKeyManager) GetRandom(int32) (io.Reader, error) {
	return bytes.NewReader(make([]byte, cipherIVSize)), nil
}

// keyWithBadPadding is a key, found by search from a fixed start, under which
// a stored encrypted record (its transformation prefix, then the IV and the
// cipher text) decrypts to bad PKCS5 padding. A wrong key chosen at random
// decrypts to valid padding about one time in 255, and then reads garbage
// instead of failing "decryption error"; a test asserting that failure must
// use a key it has checked. Computed with crypto/aes directly.
func keyWithBadPadding(stored []byte, keyLen int) ([]byte, error) {
	st, _, err := decodeTransformPrefix(stored)
	if err != nil {
		return nil, err
	}
	if !st.encrypted || len(st.data) < 2*aes.BlockSize || len(st.data)%aes.BlockSize != 0 {
		return nil, errors.New("keyWithBadPadding: not an encrypted record")
	}
	prev := st.data[len(st.data)-2*aes.BlockSize : len(st.data)-aes.BlockSize]
	last := st.data[len(st.data)-aes.BlockSize:]
	for i := 0; i < 1<<16; i++ {
		k := bytes.Repeat([]byte{0xa5}, keyLen)
		k[0], k[1] = byte(i), byte(i>>8)
		b, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		out := make([]byte, aes.BlockSize)
		b.Decrypt(out, last)
		for j := range out {
			out[j] ^= prev[j]
		}
		p := int(out[aes.BlockSize-1])
		if p < 1 || p > aes.BlockSize || !bytes.Equal(out[aes.BlockSize-p:], bytes.Repeat([]byte{byte(p)}, p)) {
			return k, nil
		}
	}
	return nil, errors.New("keyWithBadPadding: none found")
}

// wrongKeys finds, by search from a fixed start, a key under which data's
// cipher text (encrypted under key with a zero IV) decrypts to bad padding,
// and one under which it decrypts to valid padding over other bytes. Computed
// with crypto/aes directly, not by the serializer under test.
func wrongKeys(t *testing.T, key, data []byte) (badPadding, goodPadding []byte) {
	t.Helper()
	badPadding, goodPadding, err := wrongKeysFor(key, data)
	if err != nil {
		t.Fatal(err)
	}
	return badPadding, goodPadding
}

// wrongKeysFor is wrongKeys returning its failure, for a spec without a
// *testing.T.
func wrongKeysFor(key, data []byte) (badPadding, goodPadding []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	pad := aes.BlockSize - len(data)%aes.BlockSize
	plain := append(append([]byte(nil), data...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	text := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(text, plain)
	for i := 0; i < 1<<16 && (badPadding == nil || goodPadding == nil); i++ {
		k := bytes.Repeat([]byte{0x5a}, len(key))
		k[0], k[1] = byte(i), byte(i>>8)
		b, err := aes.NewCipher(k)
		if err != nil {
			return nil, nil, err
		}
		out := make([]byte, len(text))
		cipher.NewCBCDecrypter(b, make([]byte, aes.BlockSize)).CryptBlocks(out, text)
		p := int(out[len(out)-1])
		valid := p >= 1 && p <= aes.BlockSize && bytes.Equal(out[len(out)-p:], bytes.Repeat([]byte{byte(p)}, p))
		switch {
		case valid && goodPadding == nil:
			goodPadding = k
		case !valid && badPadding == nil:
			badPadding = k
		}
	}
	if badPadding == nil || goodPadding == nil {
		return nil, nil, errors.New("no wrong key of each kind found")
	}
	return badPadding, goodPadding, nil
}

// TestTransformedSerializer_WriteValidation drives each arm of Java's two
// sampled write-time validations (TransformedRecordSerializer.validateEncryption
// and RecordSerializer.validateSerialization), each Java's
// RecordSerializationValidationException, and the arm Java leaves out of it.
func TestTransformedSerializer_WriteValidation(t *testing.T) {
	t.Parallel()
	union := unionOf(sonnet108)
	key0, key1 := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16)
	badKey, goodPadKey := wrongKeys(t, key1, union)
	build := func(km *validationKeyManager, writeRatio, encryptionRatio float64) *TransformedRecordSerializer {
		return mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
			SetKeyManager(km).SetWriteValidationRatio(writeRatio).SetWriteEncryptionValidationRatio(encryptionRatio))
	}
	keys := map[int32][]byte{0: key0, 1: key1}

	// Validated under the key it was written with. Java validates the
	// encryption under key 0 (a fresh TransformedRecordSerializerState), so it
	// fails this write; Go accepts it, the bytes being the same
	// (DIVERGENCES.md).
	s := build(&validationKeyManager{keys: keys, serializationKey: 1}, 1, 1)
	stored, err := s.transform(union, nil)
	if err != nil {
		t.Fatalf("a validated write under key 1: %v", err)
	}
	if st, _, err := decodeTransformPrefix(stored); err != nil || st.keyNumber != 1 || !st.encrypted {
		t.Fatalf("the prefix: %+v, %v, want encrypted under key 1", st, err)
	}
	if back, err := s.untransform(stored); err != nil || !bytes.Equal(back, union) {
		t.Fatalf("read back: %v", err)
	}

	for _, c := range []struct {
		name                        string
		km                          *validationKeyManager
		writeRatio, encryptionRatio float64
		want                        string // the validation error's message
		wantCause                   string // its cause's text, "" none, "*" the reader's (any text but a cipher failure's)
	}{
		{
			"encryption validation, a cipher failure", &validationKeyManager{keys: keys, serializationKey: 1, later: badKey},
			0, 1, "encryption validation error: decryption failed", "bad padding",
		},
		{
			"encryption validation, other bytes", &validationKeyManager{keys: keys, serializationKey: 1, later: goodPadKey},
			0, 1, "encryption validation error: decrypted bytes do not match original", "",
		},
		{
			"serialization validation, a read failure", &validationKeyManager{keys: keys, serializationKey: 1, later: badKey},
			1, 0, "cannot deserialize record", "decryption error: bad padding",
		},
		{
			// Other bytes that do not parse as a union: Java deserializes
			// what it reads back, so it is "cannot deserialize record", not a
			// mismatch (the mismatch arm is the readers' case below).
			"serialization validation, other bytes", &validationKeyManager{keys: keys, serializationKey: 1, later: goodPadKey},
			1, 0, "cannot deserialize record", "*",
		},
		{
			"serialization validation, a key manager's refusal", &validationKeyManager{keys: keys, serializationKey: 1, laterErr: serializationError("key service unavailable")},
			1, 0, "cannot deserialize record", "key service unavailable",
		},
	} {
		_, err := build(c.km, c.writeRatio, c.encryptionRatio).transform(union, nil)
		var ve *RecordSerializationValidationError
		if !errors.As(err, &ve) || ve.Message != c.want {
			t.Errorf("%s: %v, want a RecordSerializationValidationError %q", c.name, err, c.want)
			continue
		}
		switch {
		case c.wantCause == "" && ve.Cause != nil:
			t.Errorf("%s: cause %v, want none", c.name, ve.Cause)
		case c.wantCause == "*" && (ve.Cause == nil || strings.Contains(ve.Cause.Error(), "decryption error")):
			// protobuf varies its error text, so the reader's refusal is
			// asserted as a cause that is not the cipher's.
			t.Errorf("%s: cause %v, want the reader's refusal", c.name, ve.Cause)
		case c.wantCause != "" && c.wantCause != "*" && (ve.Cause == nil || ve.Cause.Error() != c.wantCause):
			t.Errorf("%s: cause %v, want %q", c.name, ve.Cause, c.wantCause)
		}
		// Java's RecordSerializationValidationException is a
		// RecordCoreException, not a RecordSerializationException; the
		// encryption validation's cause is the GeneralSecurityException itself.
		var se *RecordSerializationError
		if c.encryptionRatio == 1 && errors.As(err, &se) {
			t.Errorf("%s: a RecordSerializationError in the chain: %v", c.name, se)
		}
	}

	// With a reader, as a store validates, the bytes read back are
	// deserialized and the records compared (Java's validateSerialization):
	// bytes that do not deserialize are "cannot deserialize record", a record
	// that differs is a mismatch, and one equal to the written record passes
	// though its bytes differ.
	written := &wrapperspb.BytesValue{Value: []byte(sonnet108)}
	for _, c := range []struct {
		name string
		read unionReader
		want string // "" passes
	}{
		{"bytes that do not deserialize", func(b []byte) (proto.Message, error) {
			if !bytes.Equal(b, union) {
				return nil, errors.New("not a record")
			}
			return written, nil
		}, "cannot deserialize record"},
		{"a record that differs", func(b []byte) (proto.Message, error) {
			return &wrapperspb.BytesValue{Value: b}, nil
		}, "record serialization mismatch"},
		{"an equal record from other bytes", func([]byte) (proto.Message, error) { return written, nil }, ""},
	} {
		km := &validationKeyManager{keys: keys, serializationKey: 1, later: goodPadKey}
		_, err := build(km, 1, 0).transformRead(union, nil, c.read)
		var ve *RecordSerializationValidationError
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v, want it to pass", c.name, err)
		case c.want != "" && (!errors.As(err, &ve) || ve.Message != c.want):
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}

	// The encryption validation catches only a cipher failure: a key
	// manager's own refusal propagates as it is, as Java's catch of
	// GeneralSecurityException lets a RecordSerializationException through.
	_, err = build(&validationKeyManager{keys: keys, serializationKey: 1, laterErr: serializationError("key service unavailable")}, 0, 1).transform(union, nil)
	var ve *RecordSerializationValidationError
	var se *RecordSerializationError
	if errors.As(err, &ve) || !errors.As(err, &se) || se.Message != "key service unavailable" {
		t.Errorf("a key manager's refusal under the encryption validation: %v, want it unwrapped", err)
	}
}

// TestTransformedSerializer_ZlibStateIsReused pins that compressing and
// decompressing a record reuse zlib state. A fresh compress/flate writer
// allocates about a megabyte and a fresh reader some 40 KiB; allocated per
// record, a race-build scan read 81k compressed rows in 3.3 GiB before the
// driver's read budget ran out. A serializer given private lists is driven
// through transform/untransform, and the lists must hold the very writer and
// reader it used — deterministic whatever else runs, and in the race build,
// where sync.Pool would drop a quarter of what it is given back. The lists have
// room for two of each, so a serializer that built fresh state per record and
// released it leaves two there, and one that kept what it took leaves none;
// both are refused. The bytes must not change: each stored stream is compared
// with a fresh writer's, every record on the private lists is compared with it
// and read back whole, and every record round-trips through the shared lists at
// two levels alternated (a writer never serves another level), including a
// reader reused after a refused header and after errors inside a stream.
func TestTransformedSerializer_ZlibStateIsReused(t *testing.T) {
	t.Parallel()
	union := unionOf(sonnet108 + sonnet108)
	fast := mustSerializer(t, NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true).SetCompressionLevel(1))
	best := mustSerializer(t, NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true))
	wantFast, err := fast.transform(union, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantBest, err := best.transform(union, nil)
	if err != nil {
		t.Fatal(err)
	}
	if wantFast[0] != transformPrefixCompressed || wantBest[0] != transformPrefixCompressed {
		t.Fatal("fixture: the union must compress at both levels")
	}
	// The expected bytes come from a FRESH writer, not the shared lists: after
	// the prefix and the five-byte compression header, each stored record is
	// exactly what a new zlib writer at its level writes.
	for _, c := range []struct {
		level  int
		stored []byte
	}{{1, wantFast}, {DeflaterBestCompression, wantBest}} {
		var fresh bytes.Buffer
		w, err := zlib.NewWriterLevel(&fresh, c.level)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(union); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(c.stored[1+compressionHeaderSize:], fresh.Bytes()) {
			t.Fatalf("level %d: the stored stream is not a fresh writer's", c.level)
		}
	}
	for i := 0; i < 20; i++ {
		for _, c := range []struct {
			s    *TransformedRecordSerializer
			want []byte
		}{{fast, wantFast}, {best, wantBest}} {
			got, err := c.s.transform(union, nil)
			if err != nil || !bytes.Equal(got, c.want) {
				t.Fatalf("round %d: a reused writer wrote different bytes (%v)", i, err)
			}
			back, err := c.s.untransform(got)
			if err != nil || !bytes.Equal(back, union) {
				t.Fatalf("round %d: a reused reader read %d bytes, %v", i, len(back), err)
			}
		}
		corrupt := bytes.Clone(wantBest)
		corrupt[6] ^= 0xff // the zlib header's first byte
		if _, err := best.untransform(corrupt); err == nil {
			t.Fatalf("round %d: a corrupt zlib header read", i)
		}
	}

	// The serializer's own path, on private lists with room for TWO of each: a
	// serializer that takes its state from the lists and gives it back leaves
	// exactly one writer and one reader there, the same ones record after
	// record. One that built fresh state and gave it back would leave two (the
	// lists have room for the second, so nothing is dropped); one that did not
	// give it back would leave none. Draining the lists after the first record
	// to learn which writer and reader it used is harmless for the same
	// reason: they go back onto lists with room to spare.
	private := mustSerializer(t, NewTransformedRecordSerializerBuilder().SetCompressWhenSerializing(true))
	private.zlib = newZlibFreeLists(2, 2)
	level := DeflaterBestCompression - DeflaterDefaultCompression
	holds := func(when string) {
		t.Helper()
		if len(private.zlib.writers[level]) != 1 || len(private.zlib.readers) != 1 {
			t.Fatalf("%s the private lists hold %d writer(s) and %d reader(s), want 1 and 1: the serializer "+
				"built zlib state it did not take from its lists (2) or kept what it took (0)",
				when, len(private.zlib.writers[level]), len(private.zlib.readers))
		}
	}
	roundTrip := func(when string) {
		t.Helper()
		got, err := private.transform(union, nil)
		if err != nil || !bytes.Equal(got, wantBest) {
			t.Fatalf("%s: the private lists' writer wrote different bytes (%v)", when, err)
		}
		back, err := private.untransform(got)
		if err != nil || !bytes.Equal(back, union) {
			t.Fatalf("%s: the private lists' reader read %d bytes, %v", when, len(back), err)
		}
	}
	roundTrip("the first record")
	holds("after the first record each way")
	usedWriter := <-private.zlib.writers[level]
	usedReader := <-private.zlib.readers
	private.zlib.writers[level] <- usedWriter
	private.zlib.readers <- usedReader
	roundTrip("the second record")
	holds("after the second record each way")
	// A reader goes back onto the list after an error in the middle of a stream
	// too, and reads the next record whole: a corrupt Adler-32 (the stream's last
	// four bytes) fails after the declared length was read, a corrupt deflate
	// body fails while reading it.
	badChecksum := bytes.Clone(wantBest)
	badChecksum[len(badChecksum)-1] ^= 0xff
	// The first deflate block's header, after the serializer's prefix byte, the
	// five-byte compression header and the two-byte zlib header: BTYPE 11 is
	// reserved, a corrupt input whatever bytes this Go version's compressor
	// wrote. (A flipped byte mid-stream can decode to other bytes and fail only
	// at the checksum, which Go 1.26.6's stream here does.)
	badBody := bytes.Clone(wantBest)
	badBody[8] |= 0x06
	// Each fixture must fail where it claims to: an accepted stream, or one
	// refused for another reason, would leave the reader's release untested on
	// that path.
	if _, err := private.untransform(badChecksum); !errors.Is(err, zlib.ErrChecksum) {
		t.Fatalf("fixture: the corrupt Adler-32 read %v, want zlib.ErrChecksum", err)
	}
	var corruptBody flate.CorruptInputError
	if _, err := private.untransform(badBody); !errors.As(err, &corruptBody) {
		t.Fatalf("fixture: the corrupt deflate body read %v, want a flate.CorruptInputError", err)
	}
	roundTrip("the record after two corrupt streams")
	holds("after the corrupt streams")
	if w := <-private.zlib.writers[level]; w != usedWriter {
		t.Fatal("a later record compressed with a writer the lists did not hand it")
	}
	if r := <-private.zlib.readers; r != usedReader {
		t.Fatal("a later record decompressed with a reader the lists did not hand it")
	}

	l := newZlibFreeLists(1, 1)
	var sink bytes.Buffer
	w, err := l.acquireWriter(&sink, DeflaterBestCompression)
	if err != nil {
		t.Fatal(err)
	}
	l.releaseWriter(w, DeflaterBestCompression)
	if again, _ := l.acquireWriter(&sink, DeflaterBestCompression); again != w {
		t.Fatal("a released writer was not reused at its level")
	}
	l.releaseWriter(w, DeflaterBestCompression)
	if other, _ := l.acquireWriter(&sink, 1); other == w {
		t.Fatal("a level-9 writer served level 1")
	}
	body := wantBest[6:] // the zlib stream after the prefix and header
	r, err := l.acquireReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	l.releaseReader(r)
	if _, err := l.acquireReader(bytes.NewReader([]byte{0xff, 0xff})); err == nil {
		t.Fatal("a refused header read")
	}
	again, err := l.acquireReader(bytes.NewReader(body))
	if err != nil || again != r {
		t.Fatalf("the reader, returned after a refused header, was not reused (%v)", err)
	}
	out, err := io.ReadAll(again)
	if err != nil || !bytes.Equal(out, union) {
		t.Fatalf("a reused reader read %d bytes, %v", len(out), err)
	}
}

// TestDecodeTransformPrefixDoesNotAllocate: the prefix is decoded once per
// record read, so its state is a value. Allocation accounting is process-wide,
// so the measurement runs in its own process, away from parallel tests.
func TestDecodeTransformPrefixDoesNotAllocate(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const bench = "BenchmarkDecodeTransformPrefixAllocations"
	output, err := exec.CommandContext(t.Context(), executable,
		"-test.run=^$", "-test.bench=^"+bench+"$", "-test.benchtime=1x").CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte(bench)) {
		t.Fatalf("allocation benchmark: %v\n%s", err, output)
	}
}

func BenchmarkDecodeTransformPrefixAllocations(b *testing.B) {
	stored := append(protowire.AppendVarint(nil, transformPrefixCompressed), 'x')
	var st transformState
	var prefixed bool
	allocations := testing.AllocsPerRun(100, func() {
		st, prefixed, _ = decodeTransformPrefix(stored)
	})
	if !prefixed || !st.compressed || string(st.data) != "x" {
		b.Fatalf("decoded %+v, prefixed=%t", st, prefixed)
	}
	if allocations != 0 {
		b.Fatalf("decodeTransformPrefix allocated %.0f times per record", allocations)
	}
}
