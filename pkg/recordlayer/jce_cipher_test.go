package recordlayer

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"strings"
	"testing"
)

// NIST SP 800-38A's AES-128 key, IV, counter block and plain text (F.2.1,
// F.3.7, F.3.13, F.4.1, F.5.1).
const (
	sp80038aKey   = "2b7e151628aed2a6abf7158809cf4f3c"
	sp80038aIV    = "000102030405060708090a0b0c0d0e0f"
	sp80038aCTR   = "f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff"
	sp80038aPlain = "6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e51" +
		"30c81c46a35ce411e5fbc1191a0a52eff69f2445df4f9b17ad2b417be66c3710"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Known answers for each mode. The NIST ones are SP 800-38A's; the unit
// widths and PCBC it does not cover were computed from the mode definitions
// over an independent AES (the Python cryptography package's), which
// reproduces the NIST ones.
func TestJCETransformation_KnownAnswers(t *testing.T) {
	t.Parallel()
	block, err := aes.NewCipher(unhex(t, sp80038aKey))
	if err != nil {
		t.Fatal(err)
	}
	plain := unhex(t, sp80038aPlain)
	for _, c := range []struct {
		name, iv string
		plainLen int
		want     string
	}{
		{"AES/CBC/NoPadding", sp80038aIV, 64, "7649abac8119b246cee98e9b12e9197d5086cb9b507219ee95db113a917678b273bed6b8e3c1743b7116e69e222295163ff1caa1681fac09120eca307586e1a7"},
		{"AES/CFB8/NoPadding", sp80038aIV, 18, "3b79424c9c0dd436bace9e0ed4586a4f32b9"},
		{"AES/CFB/NoPadding", sp80038aIV, 64, "3b3fd92eb72dad20333449f8e83cfb4ac8a64537a0b3a93fcde3cdad9f1ce58b26751f67a3cbb140b1808cf187a4f4dfc04b05357c5d1c0eeac4c66f9ff7f2e6"},
		{"AES/CFB128/NoPadding", sp80038aIV, 64, "3b3fd92eb72dad20333449f8e83cfb4ac8a64537a0b3a93fcde3cdad9f1ce58b26751f67a3cbb140b1808cf187a4f4dfc04b05357c5d1c0eeac4c66f9ff7f2e6"},
		{"AES/OFB/NoPadding", sp80038aIV, 64, "3b3fd92eb72dad20333449f8e83cfb4a7789508d16918f03f53c52dac54ed8259740051e9c5fecf64344f7a82260edcc304c6528f659c77866a510d9c1d6ae5e"},
		{"AES/CTR/NoPadding", sp80038aCTR, 64, "874d6191b620e3261bef6864990db6ce9806f66b7970fdff8617187bb9fffdff5ae4df3edbd5d35e5b4f09020db03eab1e031dda2fbe03d1792170a0f3009cee"},
		// The counter is the whole block, wrapping from all ones to zero.
		{"AES/CTR/NoPadding", strings.Repeat("ff", 16), 32, "e13338e36cb71962e00d020b4cedbd86d3dae15b04bb352fa0f59febfcb4da3e"},
		// Partial final units.
		{"AES/CFB/NoPadding", sp80038aIV, 21, "3b3fd92eb72dad20333449f8e83cfb4ac8a64537a0"},
		{"AES/CFB16/NoPadding", sp80038aIV, 21, "3b3fe49824de6332ce228e5b8131a24ab56faa8dc5"},
		{"AES/CFB64/NoPadding", sp80038aIV, 21, "3b3fd92eb72dad20764bc8b40ee0de40f857ab76f3"},
		{"AES/OFB8/NoPadding", sp80038aIV, 21, "3b95b11c62c9b759f605bbce1812c67c7a3a9516c4"},
		{"AES/OFB64/NoPadding", sp80038aIV, 21, "3b3fd92eb72dad2024210cae442ed33f9a5fbc8210"},
		{"AES/PCBC/NoPadding", sp80038aIV, 64, "7649abac8119b246cee98e9b12e9197d9e8baff12ad5270a0d1eef93d70379945700b39803779fa35a3c600a49a163c033ae199f27379f21be6dd57d295cc87d"},
	} {
		tr, err := parseJCETransformation(c.name)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		iv := unhex(t, c.iv)
		got, err := tr.encrypt(block, iv, plain[:c.plainLen], nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if hex.EncodeToString(got) != c.want {
			t.Errorf("%s over %d bytes: %x, want %s", c.name, c.plainLen, got, c.want)
		}
		back, err := tr.decrypt(block, iv, got)
		if err != nil || !bytes.Equal(back, plain[:c.plainLen]) {
			t.Errorf("%s: decrypts to %x, %v", c.name, back, err)
		}
	}

	// ISO 10126 padding's filler is random: CBC over the 21 plain bytes, ten
	// filler bytes 0xa5 and the length 11 decrypts to the 21 bytes.
	tr, err := parseJCETransformation("AES/CBC/ISO10126Padding")
	if err != nil {
		t.Fatal(err)
	}
	got, err := tr.decrypt(block, unhex(t, sp80038aIV), unhex(t, "7649abac8119b246cee98e9b12e9197d3cbd56aa6cb0ff2ad823968d55b51a1a"))
	if err != nil || !bytes.Equal(got, plain[:21]) {
		t.Errorf("ISO10126Padding: %x, %v", got, err)
	}
}

// Records round-trip through the serializer under every transformation Java's
// can run, names spelled as Cipher.getInstance accepts them.
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
		{"AES/CTR/NoPadding", key16},
		{"Rijndael/CTR/NoPadding", key32},
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
	} {
		s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
			SetEncryptionKey(c.key).SetCipherName(c.name).SetWriteValidationRatio(1))
		for _, n := range []int{0, 13, 14, 31, 100} {
			union := unionOf(strings.Repeat("r", n))
			if strings.HasSuffix(c.name, "CBC/NoPadding") {
				union = unionOf(strings.Repeat("r", 30)) // 32 bytes: a block mode without padding takes whole blocks
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

// Transformations Java's JCE refuses, or cannot run under a 16-byte
// IvParameterSpec, fail the record with Java's "encryption error".
func TestTransformedSerializer_RefusedTransformations(t *testing.T) {
	t.Parallel()
	key := aesKey(t, 16)
	for _, name := range []string{
		"AES",                        // ECB, the provider's default, takes no IV
		"AES/ECB/PKCS5Padding",       // likewise
		"AES/GCM/NoPadding",          // needs a GCMParameterSpec
		"AES/KW/NoPadding",           // an 8-byte IV
		"AES/CTS/NoPadding",          // not implemented
		"ChaCha20-Poly1305",          // a 12-byte nonce
		"ChaCha20/None/NoPadding",    // a ChaCha20ParameterSpec
		"DESede/CBC/PKCS5Padding",    // an 8-byte block
		"DES/CBC/PKCS5Padding",       // likewise
		"Blowfish/CBC/PKCS5Padding",  // likewise
		"AES/CTR/PKCS5Padding",       // CTR takes NoPadding only
		"AES/CFB4/NoPadding",         // not a whole byte
		"AES/CFB136/NoPadding",       // wider than the block
		"AES/OFB0/NoPadding",         // no unit
		"AES/CBC/PKCS7Padding",       // not a SunJCE padding
		"AES_128/CBC/PKCS5Padding",   // AES_128 is registered with NoPadding only
		"AES_256/CFB8/NoPadding",     // and with whole-block CFB and OFB only
		"AES_128/CTR/NoPadding",      // and not with CTR
		"AES/CBC",                    // two tokens
		"AES//PKCS5Padding",          // an empty token is skipped: two tokens
		"AES/CBC/PKCS5Padding/Extra", // four
		"AES/CBC/NoPadding/",         // a trailing separator is skipped, but the name is not found
	} {
		s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
			SetEncryptionKey(key).SetCipherName(name))
		_, err := s.transform(unionOf("x"), nil)
		if name == "AES/CBC/NoPadding/" {
			// Three tokens once the empty one is skipped, so Java finds it.
			if err == nil || !strings.Contains(err.Error(), "Input length not multiple of 16 bytes") {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		wantSerializationError(t, err, "encryption error: unsupported cipher "+name)
	}

	// A block mode without padding takes whole blocks only, as Java's
	// IllegalBlockSizeException does.
	for _, name := range []string{"AES/CBC/NoPadding", "AES/PCBC/NoPadding"} {
		s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
			SetEncryptionKey(key).SetCipherName(name))
		_, err := s.transform(unionOf("x"), nil)
		wantSerializationError(t, err, "encryption error: Input length not multiple of 16 bytes")
	}

	// The AES_n forms take a key of that size only.
	s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).
		SetEncryptionKey(aesKey(t, 32)).SetCipherName("AES_128/CFB/NoPadding"))
	_, err := s.transform(unionOf("x"), nil)
	wantSerializationError(t, err, "encryption error: Wrong key size")
}
