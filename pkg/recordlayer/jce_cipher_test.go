package recordlayer

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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

// The cipher text, plain text or exception Java gives for each transformation
// and length, measured on JDK 21.0.9's SunJCE (Cipher.getInstance, then init
// with a SecretKeySpec of jdkKey's first keySize bytes and an IvParameterSpec of
// jdkIV, then doFinal). Go must give the same bytes, or fail where Java threw:
// with Java's message where Go reports it, else with Go's equivalent.
var (
	jdkKey = func() []byte {
		b := make([]byte, 32)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}()
	jdkIV = func() []byte {
		b := make([]byte, 16)
		for i := range b {
			b[i] = byte(15 - i)
		}
		return b
	}()
	jdkData = func() []byte {
		b := make([]byte, 64)
		for i := range b {
			b[i] = byte(i*37 + 11)
		}
		return b
	}()
	jdkLengths = [8]int{0, 1, 15, 16, 17, 31, 32, 33}
)

func jdkCrypt(name string, keySize int, decrypt bool, data []byte) ([]byte, error) {
	tr, err := parseJCETransformation(name)
	if err != nil {
		return nil, err
	}
	block, err := tr.newBlock(jdkKey[:keySize])
	if err != nil {
		return nil, err
	}
	if decrypt {
		return tr.decrypt(block, jdkIV, data)
	}
	return tr.encrypt(block, jdkIV, data, rand.Reader)
}

// wantJDKOutcome checks Go's result against a table cell: Java's output in hex,
// or "!" and the exception Java threw.
func wantJDKOutcome(t *testing.T, what string, got []byte, err error, cell string) {
	t.Helper()
	failure, isFailure := strings.CutPrefix(cell, "!")
	if !isFailure {
		if err != nil || hex.EncodeToString(got) != cell {
			t.Errorf("%s: %x, %v; Java gives %s", what, got, err, cell)
		}
		return
	}
	class, message, _ := strings.Cut(failure, ": ")
	want := map[string]string{
		"BadPaddingException":                "bad padding",
		"NoSuchAlgorithmException":           "unsupported cipher",
		"InvalidAlgorithmParameterException": "unsupported cipher",
	}[class]
	if want == "" {
		want = message // IllegalBlockSizeException, InvalidKeyException: Java's message
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: %x, %v; Java throws %s", what, got, err, failure)
	}
}

func TestJCETransformation_MatchesJDK(t *testing.T) {
	t.Parallel()
	for _, c := range jdkEncrypt {
		for i, n := range jdkLengths {
			what := fmt.Sprintf("encrypt %s, %d-byte key, %d bytes", c.name, c.keySize, n)
			got, err := jdkCrypt(c.name, c.keySize, false, jdkData[:n])
			wantJDKOutcome(t, what, got, err, c.out[i])
			if err == nil {
				back, err := jdkCrypt(c.name, c.keySize, true, got)
				if err != nil || !bytes.Equal(back, jdkData[:n]) {
					t.Errorf("%s: decrypts to %x, %v", what, back, err)
				}
			}
		}
	}
	for _, c := range jdkDecrypt {
		for i, n := range jdkLengths {
			got, err := jdkCrypt(c.name, c.keySize, true, jdkData[:n])
			wantJDKOutcome(t, fmt.Sprintf("decrypt %s, %d bytes", c.name, n), got, err, c.out[i])
		}
	}
	// ISO 10126 filler is random: Go reads Java's cipher text.
	for _, c := range jdkISO10126 {
		for i, n := range jdkLengths {
			ct, err := hex.DecodeString(c.out[i])
			if err != nil {
				t.Fatal(err)
			}
			got, err := jdkCrypt(c.name, c.keySize, true, ct)
			if err != nil || !bytes.Equal(got, jdkData[:n]) {
				t.Errorf("decrypt Java's %s of %d bytes: %x, %v", c.name, n, got, err)
			}
		}
	}
	for _, c := range jdkAccepts {
		for i, size := range []int{16, 24, 32} {
			_, err := jdkCrypt(c.name, size, false, jdkData[:17])
			if (err == nil) != (c.keys[i] == '+') {
				t.Errorf("%q, %d-byte key, 17 bytes: Go %v, Java %c", c.name, size, err, c.keys[i])
			}
		}
	}
}

// jdkEncrypt: Java's cipher text of jdkData's first 0, 1, 15, 16, 17, 31, 32 and 33
// bytes, or "!" and the exception it threw.
var jdkEncrypt = []struct {
	name    string
	keySize int
	out     [8]string
}{
	{"AES/CBC/NoPadding", 16, [8]string{"", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "7b1eb12dbae30af43643545968377b67", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f", "!IllegalBlockSizeException: Input length not multiple of 16 bytes"}},
	{"AES/CBC/PKCS5Padding", 16, [8]string{"efddc425a6fa0c5f25e444092eb0f503", "05f9261a790e32c5f0a584b29993baf9", "f43efc2197413a9750aa26e7676d734f", "7b1eb12dbae30af43643545968377b67020fb2b791d325698a069999f37145e0", "7b1eb12dbae30af43643545968377b67d7d617162390e0e82abc77d0f387826a", "7b1eb12dbae30af43643545968377b6749327d5b75acad3f79c605f89abf1173", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4fe629e27a71c905351c729d95aa79aea1", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f45300995150059d0b4d58c9e061d8fdc"}},
	{"AES/PCBC/NoPadding", 16, [8]string{"", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "7b1eb12dbae30af43643545968377b67", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "7b1eb12dbae30af43643545968377b67c1182fc40bbdcf93abcc9885833687c0", "!IllegalBlockSizeException: Input length not multiple of 16 bytes"}},
	{"AES/PCBC/PKCS5Padding", 24, [8]string{"70f89353d0ccb40b5e0e95492b35dbdc", "6cc61a63cdf93b97c58766acbae4c24a", "97139df1d9eecbf4bec9a6cefbf1c0c1", "d2e80d30356748290bd16c1cb534d46984e3f1b106d84e0f40291cf68afca3d2", "d2e80d30356748290bd16c1cb534d4691987297238a93bfc49842cd6b6d100f4", "d2e80d30356748290bd16c1cb534d469ca813ca89f9773b2b42307886d96f85d", "d2e80d30356748290bd16c1cb534d4699dd5fc50f8dcd39c0bf5d7992c90ab7c93c1c52663741f66adc870a136679fa5", "d2e80d30356748290bd16c1cb534d4699dd5fc50f8dcd39c0bf5d7992c90ab7c536b26cea342a005d8b988d1266da263"}},
	{"AES/CTS/NoPadding", 16, [8]string{"", "!IllegalBlockSizeException: input is too short!", "!IllegalBlockSizeException: input is too short!", "7b1eb12dbae30af43643545968377b67", "9da05de178ba95cf9ae0fd953de546ef7b", "fe35fdd57c387eba2daf99e14e9a63677b1eb12dbae30af43643545968377b", "04181653c38c6df5342c827bfc587f4f7b1eb12dbae30af43643545968377b67", "7b1eb12dbae30af43643545968377b678e8e5ce61c54a0a0cf91a4fe0ab4a89604"}},
	{"AES/CTS/NoPadding", 32, [8]string{"", "!IllegalBlockSizeException: input is too short!", "!IllegalBlockSizeException: input is too short!", "ce9d699cf4634613504b7761c0ed0642", "6b5f4c3c525e7e078440c9d27b950317ce", "3299f4debe9c7f99896de24bad8e3c2fce9d699cf4634613504b7761c0ed06", "4701964905cd00dc2af8ee4677b61892ce9d699cf4634613504b7761c0ed0642", "ce9d699cf4634613504b7761c0ed064228d14f2ab3ca96b3580cd9c965e751b247"}},
	{"AES/CTR/NoPadding", 16, [8]string{"", "2b", "2b99ace82b88b2e63747817eab4288", "2b99ace82b88b2e63747817eab42885c", "2b99ace82b88b2e63747817eab42885c1c", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560d9", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560d910"}},
	{"AES/CFB/NoPadding", 16, [8]string{"", "2b", "2b99ace82b88b2e63747817eab4288", "2b99ace82b88b2e63747817eab42885c", "2b99ace82b88b2e63747817eab42885c50", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd36773985", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739850f"}},
	{"AES/CFB/PKCS5Padding", 16, [8]string{"30b9e982a45c4bf8140feccc7cbe897a", "2ba6f69dbb4354e70b10f3d363a19665", "2b99ace82b88b2e63747817eab42886b", "2b99ace82b88b2e63747817eab42885c1befb07762f3663b396bbe2f315b4813", "2b99ace82b88b2e63747817eab42885c50f0af687dec79242674a1302e44570c", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd36773902", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd36773985b4480c5228dab8d34b17e89dc97749e6", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739850f57134d37c5a7cc5408f782d66856f9"}},
	{"AES/CFB8/NoPadding", 16, [8]string{"", "2b", "2bdd3908f09ef99359ff21ffccdc46", "2bdd3908f09ef99359ff21ffccdc46e6", "2bdd3908f09ef99359ff21ffccdc46e6e5", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b63", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b6374", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b637420"}},
	{"AES/CFB8/PKCS5Padding", 16, [8]string{"30b8d55649f3636b7c63c7eba65988fe", "2be2ce452dda17af56f04886881005df", "2bdd3908f09ef99359ff21ffccdc46d1", "2bdd3908f09ef99359ff21ffccdc46e6ae7405c74aef94799927baddf5627582", "2bdd3908f09ef99359ff21ffccdc46e6e534feba78e278ac609bea4b45319fec", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b63f3", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b63749b66ffb6ca71b37c9e2e23e819af995b", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b637420e70369a2b848edb92d132051fd1690"}},
	{"AES/CFB24/NoPadding", 24, [8]string{"", "21", "2197743c380f949f23b871aebb1a17", "2197743c380f949f23b871aebb1a17e0", "2197743c380f949f23b871aebb1a17e0f1", "2197743c380f949f23b871aebb1a17e0f17ba8253c08fb37faa00145216351", "2197743c380f949f23b871aebb1a17e0f17ba8253c08fb37faa001452163516a", "2197743c380f949f23b871aebb1a17e0f17ba8253c08fb37faa001452163516a78"}},
	{"AES/CFB64/NoPadding", 16, [8]string{"", "2b", "2b99ace82b88b2e61b31feccc12ca4", "2b99ace82b88b2e61b31feccc12ca4b0", "2b99ace82b88b2e61b31feccc12ca4b0a9", "2b99ace82b88b2e61b31feccc12ca4b0a92e4966333b214f2c380e8d8bc5cd", "2b99ace82b88b2e61b31feccc12ca4b0a92e4966333b214f2c380e8d8bc5cded", "2b99ace82b88b2e61b31feccc12ca4b0a92e4966333b214f2c380e8d8bc5cded3d"}},
	{"AES/CFB120/PKCS5Padding", 16, [8]string{"30b9e982a45c4bf8140feccc7cbe8915", "2ba6f69dbb4354e70b10f3d363a19614", "2b99ace82b88b2e63747817eab4288ca", "2b99ace82b88b2e63747817eab4288fd17fa9c90a5b3619718bdcf47f6460a68", "2b99ace82b88b2e63747817eab4288fd5ce5838fbaac7e8807a2d058e959a5b8", "2b99ace82b88b2e63747817eab4288fd5c6a294a5ab748d98b0512a5f16aa2d3", "2b99ace82b88b2e63747817eab4288fd5c6a294a5ab748d98b0512a5f16aa254012e44eb24db51fd9829b1b1020b78f0", "2b99ace82b88b2e63747817eab4288fd5c6a294a5ab748d98b0512a5f16aa254ba315bf43bc44ee28736aeae1da4263c"}},
	{"AES/OFB/NoPadding", 16, [8]string{"", "2b", "2b99ace82b88b2e63747817eab4288", "2b99ace82b88b2e63747817eab42885c", "2b99ace82b88b2e63747817eab42885cbf", "2b99ace82b88b2e63747817eab42885cbf8e88a5a876999b07ace679c20811", "2b99ace82b88b2e63747817eab42885cbf8e88a5a876999b07ace679c20811cd", "2b99ace82b88b2e63747817eab42885cbf8e88a5a876999b07ace679c20811cd20"}},
	{"AES/OFB/PKCS5Padding", 32, [8]string{"62a1f3285c635f3b63bad4da9a5295b1", "79beec37437c40247ca5cbc5854d8aae", "7981b642d3b7a62540f2b9684dae94a0", "7981b642d3b7a62540f2b9684dae949718285ac7dcee174f5a49c2dd199704e7", "7981b642d3b7a62540f2b9684dae9497533745d8c3f108504556ddc206881bf8", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb75f6", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb757119c58bb244c15b9ba327dbb403c75c1b", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb7571a2da94ad5bde4484bc38c4ab1cd84304"}},
	{"AES/OFB8/NoPadding", 16, [8]string{"", "2b", "2bb55a44bc3fdf4be56b2e2dfc01df", "2bb55a44bc3fdf4be56b2e2dfc01dffa", "2bb55a44bc3fdf4be56b2e2dfc01dffaa3", "2bb55a44bc3fdf4be56b2e2dfc01dffaa3dd163d09988de9a818e4b25b493b", "2bb55a44bc3fdf4be56b2e2dfc01dffaa3dd163d09988de9a818e4b25b493b85", "2bb55a44bc3fdf4be56b2e2dfc01dffaa3dd163d09988de9a818e4b25b493b8532"}},
	{"AES/OFB64/NoPadding", 16, [8]string{"", "2b", "2b99ace82b88b2e684b71ed8428034", "2b99ace82b88b2e684b71ed842803455", "2b99ace82b88b2e684b71ed84280345595", "2b99ace82b88b2e684b71ed842803455953fe0fc6dc476cb0312e1040a4a8e", "2b99ace82b88b2e684b71ed842803455953fe0fc6dc476cb0312e1040a4a8ec1", "2b99ace82b88b2e684b71ed842803455953fe0fc6dc476cb0312e1040a4a8ec169"}},
	{"AES/OFB120/NoPadding", 16, [8]string{"", "2b", "2b99ace82b88b2e63747817eab4288", "2b99ace82b88b2e63747817eab4288e1", "2b99ace82b88b2e63747817eab4288e197", "2b99ace82b88b2e63747817eab4288e197afa1ae32756c249f5f4e350b47bd", "2b99ace82b88b2e63747817eab4288e197afa1ae32756c249f5f4e350b47bde5", "2b99ace82b88b2e63747817eab4288e197afa1ae32756c249f5f4e350b47bde5f3"}},
	{"AES_192/CFB/NoPadding", 24, [8]string{"", "21", "2197745a509c6cf521ced2d29d67a6", "2197745a509c6cf521ced2d29d67a6ae", "2197745a509c6cf521ced2d29d67a6ae3a", "2197745a509c6cf521ced2d29d67a6ae3a356328cabec929b33f41f555bd1f", "2197745a509c6cf521ced2d29d67a6ae3a356328cabec929b33f41f555bd1fbe", "2197745a509c6cf521ced2d29d67a6ae3a356328cabec929b33f41f555bd1fbe38"}},
	{"AES_256/OFB/NoPadding", 32, [8]string{"", "79", "7981b642d3b7a62540f2b9684dae94", "7981b642d3b7a62540f2b9684dae9497", "7981b642d3b7a62540f2b9684dae949753", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb75", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb7571", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb7571a2"}},
	{"2.16.840.1.101.3.4.1.42", 32, [8]string{"", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "ce9d699cf4634613504b7761c0ed0642", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "ce9d699cf4634613504b7761c0ed06424701964905cd00dc2af8ee4677b61892", "!IllegalBlockSizeException: Input length not multiple of 16 bytes"}},
	{"2.16.840.1.101.3.4.1.2/CTR/NoPadding", 16, [8]string{"", "2b", "2b99ace82b88b2e63747817eab4288", "2b99ace82b88b2e63747817eab42885c", "2b99ace82b88b2e63747817eab42885c1c", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560d9", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560d910"}},
	{"2.16.840.1.101.3.4.1.23/ECB/PKCS5Padding", 24, [8]string{"3ab73130df4895eb0286bf604a9ba788", "21a82e2fc0578af41d99a07f5584b897", "2197745a509c6cf521ced2d29d67a699", "2197745a509c6cf521ced2d29d67a6ae9c0b4a6ecaad5a25fbbffd07decca386", "2197745a509c6cf521ced2d29d67a6aed7145571d5b2453ae4a0e218c1d3bc99", "2197745a509c6cf521ced2d29d67a6aed79bffb435a9736b680720e5d9e0d297", "2197745a509c6cf521ced2d29d67a6aed79bffb435a9736b680720e5d9e0d210e6612c8170ca4a77c6a2abc98fe1aa0b", "2197745a509c6cf521ced2d29d67a6aed79bffb435a9736b680720e5d9e0d2105d7e339e6fd55568d9bdb4d690feb514"}},
	{"2.16.840.1.101.3.4.1.44/CTS/NoPadding", 32, [8]string{"", "!IllegalBlockSizeException: input is too short!", "!IllegalBlockSizeException: input is too short!", "ce9d699cf4634613504b7761c0ed0642", "6b5f4c3c525e7e078440c9d27b950317ce", "3299f4debe9c7f99896de24bad8e3c2fce9d699cf4634613504b7761c0ed06", "4701964905cd00dc2af8ee4677b61892ce9d699cf4634613504b7761c0ed0642", "ce9d699cf4634613504b7761c0ed064228d14f2ab3ca96b3580cd9c965e751b247"}},
}

// jdkDecrypt: Java's plain text of jdkData's first bytes read as cipher text.
var jdkDecrypt = []struct {
	name    string
	keySize int
	out     [8]string
}{
	{"AES/CBC/NoPadding", 16, [8]string{"", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "74492210e51a8cb1f2702f244a07eea5", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "74492210e51a8cb1f2702f244a07eea533491013b2c452fe73e66b3cab7e5ee0", "!IllegalBlockSizeException: Input length not multiple of 16 bytes"}},
	{"AES/CBC/PKCS5Padding", 16, [8]string{"", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher"}},
	{"AES/CBC/ISO10126Padding", 16, [8]string{"", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher"}},
	{"AES/PCBC/PKCS5Padding", 16, [8]string{"", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher"}},
	{"AES/CFB/PKCS5Padding", 16, [8]string{"", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher"}},
	{"AES/OFB8/ISO10126Padding", 16, [8]string{"", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher"}},
	{"AES/CTS/NoPadding", 16, [8]string{"", "!IllegalBlockSizeException: input is too short!", "!IllegalBlockSizeException: input is too short!", "74492210e51a8cb1f2702f244a07eea5", "e9a23ce74b80545b1cb5bc39f5458d2f20", "4bc119d02570b0acd794d981bc64948220c78ad60104bce776dee7d25e398e", "37774865260ab2f847b8139a6f904ed620c78ad60104bce776dee7d25e398e23", "74492210e51a8cb1f2702f244a07eea580d9d5c4014d10c6552444c9e866dde693"}},
}

// jdkISO10126: Java's cipher text, under its random filler, of jdkData's first
// 0, 1, 15, 16, 17, 31, 32 and 33 bytes.
var jdkISO10126 = []struct {
	name    string
	keySize int
	out     [8]string
}{
	{"AES/CBC/ISO10126Padding", 16, [8]string{"f95ec8f58186a9ce6cd41142dcbc9408", "18d284071ce8fafd7ba111ddcf8a2fee", "f43efc2197413a9750aa26e7676d734f", "7b1eb12dbae30af43643545968377b67682e0a69f2fcf02f36c3b9ca46151a90", "7b1eb12dbae30af43643545968377b678a114accf7b0db882ec5856d2aa1117b", "7b1eb12dbae30af43643545968377b6749327d5b75acad3f79c605f89abf1173", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f513e7eccae830eed79943ebd892caa74", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f99521451b8cf91301203edd63f430125"}},
	{"AES/PCBC/ISO10126Padding", 16, [8]string{"fb08ea95f9a03291f85d61fe94dfc6a2", "47e473192f7e9d2c0e5efbe5c17f2256", "f43efc2197413a9750aa26e7676d734f", "7b1eb12dbae30af43643545968377b673634b0c825aa94bb7ccb879206aca4a4", "7b1eb12dbae30af43643545968377b678c7b0e6a3ec37b33ba809258411d528f", "7b1eb12dbae30af43643545968377b67009abeff1592ed0df9ce7fe1a64393ac", "7b1eb12dbae30af43643545968377b67c1182fc40bbdcf93abcc9885833687c06f0a883aeb84e0c543da4a4c11020e79", "7b1eb12dbae30af43643545968377b67c1182fc40bbdcf93abcc9885833687c0e05099dd88e3ecc050fd0592ee6ad312"}},
	{"AES/CFB/ISO10126Padding", 16, [8]string{"fee26c72474b607b3b235a4ee131127a", "2b06580a07bbebd6cf272bb7995ae365", "2b99ace82b88b2e63747817eab42886b", "2b99ace82b88b2e63747817eab42885cc8845b624a2b7b17cfd7e312178d7a13", "2b99ace82b88b2e63747817eab42885c503cea035f16c63f626cbf2916c81f0c", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd36773902", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd36773985b1df51e61ad273087d0ca44b397188e6", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739850fc92ad60afadd07765e31ba5708f2f9"}},
	{"AES/OFB32/ISO10126Padding", 16, [8]string{"e72c044da2c5b08c90926994f1f82f95", "2b302bbc9e2b9db6dccd4a28d5f1e48a", "2b99ace8a96cdc8cdc8edc0f23945b84", "2b99ace8a96cdc8cdc8edc0f23945bb3c767ba61d309fc519143ac2f8723c248", "2b99ace8a96cdc8cdc8edc0f23945bb3fa318ed774cf91d576b51012b00d5357", "2b99ace8a96cdc8cdc8edc0f23945bb3faee5b544631321dad46bcc02fdc5659", "2b99ace8a96cdc8cdc8edc0f23945bb3faee5b544631321dad46bcc02fdc56de67fcf8942328889223968bfb4a91dc2c", "2b99ace8a96cdc8cdc8edc0f23945bb3faee5b544631321dad46bcc02fdc56de5c595557aca62f955cb06a378b6e3533"}},
	{"OID.2.16.840.1.101.3.4.1.42/CFB8/ISO10126Padding", 32, [8]string{"b409336e7f72cce81a60ec1b687f422a", "79ec84fd15dd77b70875192b0ae9e1aa", "796ab31c7f5acb9c0330101294234e40", "796ab31c7f5acb9c0330101294234e77d229a6be18631d04c47692b3be745bdd", "796ab31c7f5acb9c0330101294234e774659787834ab6da2b3762c23fac92115", "796ab31c7f5acb9c0330101294234e77462176462c46875064e82582a306da21", "796ab31c7f5acb9c0330101294234e77462176462c46875064e82582a306daa68738fb8f4af72f2e744a915b7ad216f4", "796ab31c7f5acb9c0330101294234e77462176462c46875064e82582a306daa65eeed33b8d2359f7c3474aace0ca9416"}},
}

// jdkAccepts: whether Java encrypts jdkData's first 17 bytes under each name
// with a 16-, 24- and 32-byte key ("+" it does, "-" it throws).
var jdkAccepts = []struct{ name, keys string }{
	{"AES/CBC/NoPadding", "---"},
	{"AES/CBC/PKCS5Padding", "+++"},
	{"AES/CBC/ISO10126Padding", "+++"},
	{"AES/PCBC/NoPadding", "---"},
	{"AES/PCBC/PKCS5Padding", "+++"},
	{"AES/PCBC/ISO10126Padding", "+++"},
	{"AES/CTS/NoPadding", "+++"},
	{"AES/CTR/NoPadding", "+++"},
	{"AES/CFB/NoPadding", "+++"},
	{"AES/CFB/PKCS5Padding", "+++"},
	{"AES/CFB/ISO10126Padding", "+++"},
	{"AES/CFB8/NoPadding", "+++"},
	{"AES/CFB8/PKCS5Padding", "+++"},
	{"AES/CFB16/NoPadding", "+++"},
	{"AES/CFB24/NoPadding", "+++"},
	{"AES/CFB64/NoPadding", "+++"},
	{"AES/CFB120/PKCS5Padding", "+++"},
	{"AES/CFB128/NoPadding", "+++"},
	{"AES/OFB/NoPadding", "+++"},
	{"AES/OFB/PKCS5Padding", "+++"},
	{"AES/OFB8/NoPadding", "+++"},
	{"AES/OFB32/ISO10126Padding", "+++"},
	{"AES/OFB64/NoPadding", "+++"},
	{"AES/OFB120/NoPadding", "+++"},
	{"AES/OFB128/PKCS5Padding", "+++"},
	{"aes/cbc/pkcs5padding", "+++"},
	{" AES / OFB / PKCS5Padding ", "+++"},
	{"AES /CBC/PKCS5Padding\t", "+++"},
	{"aes/cfb24/nopadding", "+++"},
	{"AES/Cfb16/NoPadding", "+++"},
	{"AES_128/CBC/NoPadding", "---"},
	{"AES_128/CFB/NoPadding", "+--"},
	{"AES_128/OFB/NoPadding", "+--"},
	{"AES_192/CBC/NoPadding", "---"},
	{"AES_192/CFB/NoPadding", "-+-"},
	{"AES_256/OFB/NoPadding", "--+"},
	{"aes_256/cfb/nopadding", "--+"},
	{"2.16.840.1.101.3.4.1.2", "---"},
	{"2.16.840.1.101.3.4.1.3", "+--"},
	{"2.16.840.1.101.3.4.1.4", "+--"},
	{"OID.2.16.840.1.101.3.4.1.22", "---"},
	{"oid.2.16.840.1.101.3.4.1.23", "-+-"},
	{"2.16.840.1.101.3.4.1.24", "-+-"},
	{"2.16.840.1.101.3.4.1.42", "---"},
	{"OID.2.16.840.1.101.3.4.1.43", "--+"},
	{" 2.16.840.1.101.3.4.1.44 ", "--+"},
	{"2.16.840.1.101.3.4.1/CTS/NoPadding", "+++"},
	{"OID.2.16.840.1.101.3.4.1/CBC/PKCS5Padding", "+++"},
	{"2.16.840.1.101.3.4.1.1/CBC/PKCS5Padding", "+--"},
	{"2.16.840.1.101.3.4.1.2/CTR/NoPadding", "+--"},
	{"2.16.840.1.101.3.4.1.22/PCBC/PKCS5Padding", "-+-"},
	{"OID.2.16.840.1.101.3.4.1.42/CFB8/ISO10126Padding", "--+"},
	{"2.16.840.1.101.3.4.1.44/CTS/NoPadding", "--+"},
	{"2.16.840.1.101.3.4.1.2/ECB/NoPadding", "---"},
	{"2.16.840.1.101.3.4.1.23/ECB/PKCS5Padding", "-+-"},
	{"2.16.840.1.101.3.4.1.44/ECB/ISO10126Padding", "--+"},
	{"AES", "---"},
	{"AES/ECB/PKCS5Padding", "---"},
	{"AES/ECB/NoPadding", "---"},
	{"AES/GCM/NoPadding", "---"},
	{"AES/KW/NoPadding", "---"},
	{"AES/KWP/NoPadding", "---"},
	{"AES/CTS/PKCS5Padding", "---"},
	{"AES/CTS/ISO10126Padding", "---"},
	{"ChaCha20-Poly1305", "---"},
	{"ChaCha20/None/NoPadding", "---"},
	{"DESede/CBC/PKCS5Padding", "---"},
	{"DES/CBC/PKCS5Padding", "---"},
	{"Blowfish/CBC/PKCS5Padding", "---"},
	{"Rijndael/CBC/PKCS5Padding", "---"},
	{"AES/CTR/PKCS5Padding", "---"},
	{"AES/CTR/ISO10126Padding", "---"},
	{"AES/CFB4/NoPadding", "---"},
	{"AES/CFB12/NoPadding", "---"},
	{"AES/CFB136/NoPadding", "---"},
	{"AES/OFB0/NoPadding", "---"},
	{"AES/CFB0/NoPadding", "---"},
	{"AES/CFB008/NoPadding", "---"},
	{"AES/CFB+8/NoPadding", "---"},
	{"AES/CFB 8/NoPadding", "---"},
	{"AES/CFB-8/NoPadding", "---"},
	{"AES/CBC/PKCS7Padding", "---"},
	{"AES/XYZ/NoPadding", "---"},
	{"AES_128", "---"},
	{"AES_128/CBC/PKCS5Padding", "---"},
	{"AES_256/CFB8/NoPadding", "---"},
	{"AES_128/CTR/NoPadding", "---"},
	{"AES_128/PCBC/NoPadding", "---"},
	{"AES_128/CTS/NoPadding", "---"},
	{"AES_128/ECB/NoPadding", "---"},
	{"AES_192/GCM/NoPadding", "---"},
	{"AES/CBC", "---"},
	{"AES//PKCS5Padding", "---"},
	{"AES//CBC/PKCS5Padding", "---"},
	{"AES/CBC/PKCS5Padding/Extra", "---"},
	{"AES/CBC/NoPadding/", "---"},
	{"/AES/CBC/NoPadding", "---"},
	{"", "---"},
	{"2.16.840.1.101.3.4.1", "---"},
	{"OID.2.16.840.1.101.3.4.1", "---"},
	{"2.16.840.1.101.3.4.1.1", "---"},
	{"2.16.840.1.101.3.4.1.6", "---"},
	{"2.16.840.1.101.3.4.1.5", "---"},
	{"2.16.840.1.101.3.4.1.8", "---"},
	{"2.16.840.1.101.3.4.1.7", "---"},
	{"2.16.840.1.101.3.4.1.21", "---"},
	{"2.16.840.1.101.3.4.1.46", "---"},
	{"2.16.840.1.101.3.4.1.02", "---"},
	{"2.16.840.1.101.3.4.1.6/CBC/PKCS5Padding", "---"},
	{"2.16.840.1.101.3.4.1.5/CBC/PKCS5Padding", "---"},
	{"2.16.840.1.101.3.4.1.2/GCM/NoPadding", "---"},
	{"2.16.840.1.101.3.4.1.2/CTR/PKCS5Padding", "---"},
	{"2.16.840.1.101.3.4.1.1/ECB/NoPadding", "---"},
	{"2.16.840.1.101.3.4.1/ECB/NoPadding", "---"},
	{"OID2.16.840.1.101.3.4.1.2", "---"},
}
