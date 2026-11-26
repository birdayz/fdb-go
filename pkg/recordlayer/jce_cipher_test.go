package recordlayer

import (
	"bytes"
	"crypto/aes"
	"strings"
	"testing"
)

// Records round-trip through the serializer under every mode family, names
// spelled as Cipher.getInstance accepts them.
func TestTransformedSerializer_AESModes(t *testing.T) {
	t.Parallel()
	key16, key24, key32 := aesKey(t, 16), aesKey(t, 24), aesKey(t, 32)
	for _, c := range []struct {
		name string
		key  []byte
	}{
		{"AES/CBC/PKCS5Padding", key16},
		{"AES/CBC/ISO10126Padding", key24},
		{"AES/PCBC/PKCS5Padding", key32},
		{"AES/PCBC/ISO10126Padding", key16},
		{"AES/CTS/NoPadding", key16},
		{"AES/CTR/NoPadding", key32},
		{"AES/CFB/NoPadding", key16},
		{"AES/CFB8/NoPadding", key16},
		{"aes/cfb24/nopadding", key24},
		{"AES/CFB120/PKCS5Padding", key16},
		{"AES/OFB/NoPadding", key16},
		{"AES/OFB32/ISO10126Padding", key32},
		{" AES / OFB / PKCS5Padding ", key16},
		{"AES_128/CBC/NoPadding", key16},
		{"AES_192/CFB/NoPadding", key24},
		{"AES_256/OFB/NoPadding", key32},
		{"2.16.840.1.101.3.4.1.42", key32},
		{"OID.2.16.840.1.101.3.4.1.23/CTS/NoPadding", key24},
	} {
		s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
			SetEncryptionKey(c.key).SetCipherName(c.name).SetWriteValidationRatio(1))
		for _, n := range []int{0, 13, 14, 31, 100} {
			union := unionOf(strings.Repeat("r", n))
			switch {
			case strings.Contains(c.name, "CBC/NoPadding"), strings.HasSuffix(c.name, ".42"):
				union = unionOf(strings.Repeat("r", 30)) // a block mode without padding takes whole blocks
			case strings.Contains(c.name, "CTS") && len(union) < aes.BlockSize:
				continue // CTS takes at least one block
			}
			stored, err := s.transform(union, nil)
			if err != nil {
				t.Fatalf("%s, %d bytes: %v", c.name, n, err)
			}
			got, err := s.untransform(stored)
			if err != nil || !bytes.Equal(got, union) {
				t.Fatalf("%s, %d bytes: read %x, %v", c.name, n, got, err)
			}
		}
	}
}

// A name Java's JCE refuses, or cannot run under a 16-byte IvParameterSpec,
// fails the record with Java's "encryption error", as does a length or a key
// the transformation cannot take.
func TestTransformedSerializer_RefusedTransformations(t *testing.T) {
	t.Parallel()
	encrypt := func(name string, key []byte) error {
		s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
			SetEncryptionKey(key).SetCipherName(name))
		_, err := s.transform(unionOf("x"), nil)
		return err
	}
	for _, name := range []string{"AES", "AES/GCM/NoPadding", "Rijndael/CBC/PKCS5Padding", "AES//CBC/PKCS5Padding"} {
		wantSerializationError(t, encrypt(name, aesKey(t, 16)), "encryption error: unsupported cipher "+name)
	}
	wantSerializationError(t, encrypt("AES/PCBC/NoPadding", aesKey(t, 16)), "encryption error: Input length not multiple of 16 bytes")
	wantSerializationError(t, encrypt("AES/CTS/NoPadding", aesKey(t, 16)), "encryption error: input is too short!")
	wantSerializationError(t, encrypt("AES_128/CFB/NoPadding", aesKey(t, 32)), "encryption error: The key must be 16 bytes")
}
