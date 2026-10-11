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
		// The counter is the whole block, wrapping from all ones to zero
		// (measured on JDK 21.0.9).
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
		got = got[len(iv):]
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
	jdkLengths = [9]int{0, 1, 15, 16, 17, 31, 32, 33, 48}
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
	out, err := tr.encrypt(block, jdkIV, data, rand.Reader)
	if err != nil {
		return nil, err
	}
	return out[len(jdkIV):], nil
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
	want := message // IllegalBlockSizeException, InvalidKeyException: Java's message
	switch class {
	case "BadPaddingException":
		want = "bad padding"
	case "NoSuchAlgorithmException", "InvalidAlgorithmParameterException":
		want = "unsupported cipher"
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: %x, %v; Java throws %s", what, got, err, failure)
	}
}

// jdkCodeMatches says Go's err is what a jdkAccepts code says Java threw. N and
// P are Go's "unsupported cipher", K its key refusals (Java's messages), B its
// length refusals (Java's messages).
func jdkCodeMatches(code rune, err error) bool {
	if code == '+' || err == nil {
		return code == '+' && err == nil
	}
	msg := err.Error()
	switch code {
	case 'N', 'P':
		return strings.Contains(msg, "unsupported cipher")
	case 'K':
		return strings.Contains(msg, "The key must be") || strings.Contains(msg, "Invalid key length") || strings.Contains(msg, "Invalid AES key length")
	case 'B':
		return strings.Contains(msg, "Input length not multiple of 16 bytes") || strings.Contains(msg, "input is too short!")
	}
	return false
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
		for i, code := range strings.ReplaceAll(c.codes, " ", "") {
			size, n := []int{16, 24, 32}[i%3], []int{17, 32}[i/3]
			_, err := jdkCrypt(c.name, size, false, jdkData[:n])
			if !jdkCodeMatches(code, err) {
				t.Errorf("%q, %d-byte key, %d bytes: Go %v, Java %c", c.name, size, n, err, code)
			}
		}
	}
}

// jdkEncrypt: Java's cipher text of jdkData's first 0, 1, 15, 16, 17, 31, 32, 33 and
// 48 bytes, or "!" and the exception it threw.
var jdkEncrypt = []struct {
	name    string
	keySize int
	out     [9]string
}{
	{"AES/CBC/NoPadding", 16, [9]string{"", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "7b1eb12dbae30af43643545968377b67", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f0274258d82ec4273c9739ccc177fc89b"}},
	{"AES/CBC/PKCS5Padding", 16, [9]string{"efddc425a6fa0c5f25e444092eb0f503", "05f9261a790e32c5f0a584b29993baf9", "f43efc2197413a9750aa26e7676d734f", "7b1eb12dbae30af43643545968377b67020fb2b791d325698a069999f37145e0", "7b1eb12dbae30af43643545968377b67d7d617162390e0e82abc77d0f387826a", "7b1eb12dbae30af43643545968377b6749327d5b75acad3f79c605f89abf1173", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4fe629e27a71c905351c729d95aa79aea1", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f45300995150059d0b4d58c9e061d8fdc", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f0274258d82ec4273c9739ccc177fc89b3ff15925b994a6bfb4ad69219eecc00e"}},
	{"AES/PCBC/NoPadding", 16, [9]string{"", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "7b1eb12dbae30af43643545968377b67", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "7b1eb12dbae30af43643545968377b67c1182fc40bbdcf93abcc9885833687c0", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "7b1eb12dbae30af43643545968377b67c1182fc40bbdcf93abcc9885833687c0785017372dffb259814bf70a18c167a2"}},
	{"AES/PCBC/PKCS5Padding", 24, [9]string{"70f89353d0ccb40b5e0e95492b35dbdc", "6cc61a63cdf93b97c58766acbae4c24a", "97139df1d9eecbf4bec9a6cefbf1c0c1", "d2e80d30356748290bd16c1cb534d46984e3f1b106d84e0f40291cf68afca3d2", "d2e80d30356748290bd16c1cb534d4691987297238a93bfc49842cd6b6d100f4", "d2e80d30356748290bd16c1cb534d469ca813ca89f9773b2b42307886d96f85d", "d2e80d30356748290bd16c1cb534d4699dd5fc50f8dcd39c0bf5d7992c90ab7c93c1c52663741f66adc870a136679fa5", "d2e80d30356748290bd16c1cb534d4699dd5fc50f8dcd39c0bf5d7992c90ab7c536b26cea342a005d8b988d1266da263", "d2e80d30356748290bd16c1cb534d4699dd5fc50f8dcd39c0bf5d7992c90ab7cfb8daab084d0e6c053c3c4dd0eb0824c23a4760367250944783feeccf5a2c0a0"}},
	{"AES/CTS/NoPadding", 16, [9]string{"", "!IllegalBlockSizeException: input is too short!", "!IllegalBlockSizeException: input is too short!", "7b1eb12dbae30af43643545968377b67", "9da05de178ba95cf9ae0fd953de546ef7b", "fe35fdd57c387eba2daf99e14e9a63677b1eb12dbae30af43643545968377b", "04181653c38c6df5342c827bfc587f4f7b1eb12dbae30af43643545968377b67", "7b1eb12dbae30af43643545968377b678e8e5ce61c54a0a0cf91a4fe0ab4a89604", "7b1eb12dbae30af43643545968377b670274258d82ec4273c9739ccc177fc89b04181653c38c6df5342c827bfc587f4f"}},
	{"AES/CTS/NoPadding", 32, [9]string{"", "!IllegalBlockSizeException: input is too short!", "!IllegalBlockSizeException: input is too short!", "ce9d699cf4634613504b7761c0ed0642", "6b5f4c3c525e7e078440c9d27b950317ce", "3299f4debe9c7f99896de24bad8e3c2fce9d699cf4634613504b7761c0ed06", "4701964905cd00dc2af8ee4677b61892ce9d699cf4634613504b7761c0ed0642", "ce9d699cf4634613504b7761c0ed064228d14f2ab3ca96b3580cd9c965e751b247", "ce9d699cf4634613504b7761c0ed0642a18feb5118ee2ac2494e44c487621c064701964905cd00dc2af8ee4677b61892"}},
	{"AES/CTR/NoPadding", 16, [9]string{"", "2b", "2b99ace82b88b2e63747817eab4288", "2b99ace82b88b2e63747817eab42885c", "2b99ace82b88b2e63747817eab42885c1c", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560d9", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560d910", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560d9100f0da6d1a7e014023ba81d1700c543"}},
	{"AES/CFB/NoPadding", 16, [9]string{"", "2b", "2b99ace82b88b2e63747817eab4288", "2b99ace82b88b2e63747817eab42885c", "2b99ace82b88b2e63747817eab42885c50", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd36773985", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739850f", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739850f88e95807ae216d88ffe5cfbeebe820"}},
	{"AES/CFB/PKCS5Padding", 16, [9]string{"30b9e982a45c4bf8140feccc7cbe897a", "2ba6f69dbb4354e70b10f3d363a19665", "2b99ace82b88b2e63747817eab42886b", "2b99ace82b88b2e63747817eab42885c1befb07762f3663b396bbe2f315b4813", "2b99ace82b88b2e63747817eab42885c50f0af687dec79242674a1302e44570c", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd36773902", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd36773985b4480c5228dab8d34b17e89dc97749e6", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739850f57134d37c5a7cc5408f782d66856f9", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739850f88e95807ae216d88ffe5cfbeebe82070114e494cf269c43cf09c193db1377d"}},
	{"AES/CFB8/NoPadding", 16, [9]string{"", "2b", "2bdd3908f09ef99359ff21ffccdc46", "2bdd3908f09ef99359ff21ffccdc46e6", "2bdd3908f09ef99359ff21ffccdc46e6e5", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b63", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b6374", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b637420", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b63742038c793d8b5e3c7fde949af4affae39"}},
	{"AES/CFB8/PKCS5Padding", 16, [9]string{"30b8d55649f3636b7c63c7eba65988fe", "2be2ce452dda17af56f04886881005df", "2bdd3908f09ef99359ff21ffccdc46d1", "2bdd3908f09ef99359ff21ffccdc46e6ae7405c74aef94799927baddf5627582", "2bdd3908f09ef99359ff21ffccdc46e6e534feba78e278ac609bea4b45319fec", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b63f3", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b63749b66ffb6ca71b37c9e2e23e819af995b", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b637420e70369a2b848edb92d132051fd1690", "2bdd3908f09ef99359ff21ffccdc46e6e5bb57f8a041e12a817b704be52b63742038c793d8b5e3c7fde949af4affae39b06808c122cb98d1cb3b15a4a0d67fea"}},
	{"AES/CFB24/NoPadding", 24, [9]string{"", "21", "2197743c380f949f23b871aebb1a17", "2197743c380f949f23b871aebb1a17e0", "2197743c380f949f23b871aebb1a17e0f1", "2197743c380f949f23b871aebb1a17e0f17ba8253c08fb37faa00145216351", "2197743c380f949f23b871aebb1a17e0f17ba8253c08fb37faa001452163516a", "2197743c380f949f23b871aebb1a17e0f17ba8253c08fb37faa001452163516a78", "2197743c380f949f23b871aebb1a17e0f17ba8253c08fb37faa001452163516a78b887e8fc38918cd5b7a8e787160a35"}},
	{"AES/CFB64/NoPadding", 16, [9]string{"", "2b", "2b99ace82b88b2e61b31feccc12ca4", "2b99ace82b88b2e61b31feccc12ca4b0", "2b99ace82b88b2e61b31feccc12ca4b0a9", "2b99ace82b88b2e61b31feccc12ca4b0a92e4966333b214f2c380e8d8bc5cd", "2b99ace82b88b2e61b31feccc12ca4b0a92e4966333b214f2c380e8d8bc5cded", "2b99ace82b88b2e61b31feccc12ca4b0a92e4966333b214f2c380e8d8bc5cded3d", "2b99ace82b88b2e61b31feccc12ca4b0a92e4966333b214f2c380e8d8bc5cded3d3a7e3b39856984bc6798d9bf4250ed"}},
	{"AES/CFB120/PKCS5Padding", 16, [9]string{"30b9e982a45c4bf8140feccc7cbe8915", "2ba6f69dbb4354e70b10f3d363a19614", "2b99ace82b88b2e63747817eab4288ca", "2b99ace82b88b2e63747817eab4288fd17fa9c90a5b3619718bdcf47f6460a68", "2b99ace82b88b2e63747817eab4288fd5ce5838fbaac7e8807a2d058e959a5b8", "2b99ace82b88b2e63747817eab4288fd5c6a294a5ab748d98b0512a5f16aa2d3", "2b99ace82b88b2e63747817eab4288fd5c6a294a5ab748d98b0512a5f16aa254012e44eb24db51fd9829b1b1020b78f0", "2b99ace82b88b2e63747817eab4288fd5c6a294a5ab748d98b0512a5f16aa254ba315bf43bc44ee28736aeae1da4263c", "2b99ace82b88b2e63747817eab4288fd5c6a294a5ab748d98b0512a5f16aa254baeea1e10bafc8435bc1bce3751bc960eb35f7c1200d3bb4a6f27fc51b965afd"}},
	{"AES/OFB/NoPadding", 16, [9]string{"", "2b", "2b99ace82b88b2e63747817eab4288", "2b99ace82b88b2e63747817eab42885c", "2b99ace82b88b2e63747817eab42885cbf", "2b99ace82b88b2e63747817eab42885cbf8e88a5a876999b07ace679c20811", "2b99ace82b88b2e63747817eab42885cbf8e88a5a876999b07ace679c20811cd", "2b99ace82b88b2e63747817eab42885cbf8e88a5a876999b07ace679c20811cd20", "2b99ace82b88b2e63747817eab42885cbf8e88a5a876999b07ace679c20811cd204cea0808080e536373280c27cd3e4b"}},
	{"AES/OFB/PKCS5Padding", 32, [9]string{"62a1f3285c635f3b63bad4da9a5295b1", "79beec37437c40247ca5cbc5854d8aae", "7981b642d3b7a62540f2b9684dae94a0", "7981b642d3b7a62540f2b9684dae949718285ac7dcee174f5a49c2dd199704e7", "7981b642d3b7a62540f2b9684dae9497533745d8c3f108504556ddc206881bf8", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb75f6", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb757119c58bb244c15b9ba327dbb403c75c1b", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb7571a2da94ad5bde4484bc38c4ab1cd84304", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb7571a2056eb86bb5c22560cfd6e6745bfddd7e7b9883d56a7b678b9ff3912f87523d"}},
	{"AES/OFB8/NoPadding", 16, [9]string{"", "2b", "2bb55a44bc3fdf4be56b2e2dfc01df", "2bb55a44bc3fdf4be56b2e2dfc01dffa", "2bb55a44bc3fdf4be56b2e2dfc01dffaa3", "2bb55a44bc3fdf4be56b2e2dfc01dffaa3dd163d09988de9a818e4b25b493b", "2bb55a44bc3fdf4be56b2e2dfc01dffaa3dd163d09988de9a818e4b25b493b85", "2bb55a44bc3fdf4be56b2e2dfc01dffaa3dd163d09988de9a818e4b25b493b8532", "2bb55a44bc3fdf4be56b2e2dfc01dffaa3dd163d09988de9a818e4b25b493b8532cc880741f22c400c4922c0fa406ee3"}},
	{"AES/OFB64/NoPadding", 16, [9]string{"", "2b", "2b99ace82b88b2e684b71ed8428034", "2b99ace82b88b2e684b71ed842803455", "2b99ace82b88b2e684b71ed84280345595", "2b99ace82b88b2e684b71ed842803455953fe0fc6dc476cb0312e1040a4a8e", "2b99ace82b88b2e684b71ed842803455953fe0fc6dc476cb0312e1040a4a8ec1", "2b99ace82b88b2e684b71ed842803455953fe0fc6dc476cb0312e1040a4a8ec169", "2b99ace82b88b2e684b71ed842803455953fe0fc6dc476cb0312e1040a4a8ec169ae755152c0757c63a9598331bd5831"}},
	{"AES/OFB120/NoPadding", 16, [9]string{"", "2b", "2b99ace82b88b2e63747817eab4288", "2b99ace82b88b2e63747817eab4288e1", "2b99ace82b88b2e63747817eab4288e197", "2b99ace82b88b2e63747817eab4288e197afa1ae32756c249f5f4e350b47bd", "2b99ace82b88b2e63747817eab4288e197afa1ae32756c249f5f4e350b47bde5", "2b99ace82b88b2e63747817eab4288e197afa1ae32756c249f5f4e350b47bde5f3", "2b99ace82b88b2e63747817eab4288e197afa1ae32756c249f5f4e350b47bde5f3181bafbb5b60a48b2a50fc410c3994"}},
	{"AES_192/CFB/NoPadding", 24, [9]string{"", "21", "2197745a509c6cf521ced2d29d67a6", "2197745a509c6cf521ced2d29d67a6ae", "2197745a509c6cf521ced2d29d67a6ae3a", "2197745a509c6cf521ced2d29d67a6ae3a356328cabec929b33f41f555bd1f", "2197745a509c6cf521ced2d29d67a6ae3a356328cabec929b33f41f555bd1fbe", "2197745a509c6cf521ced2d29d67a6ae3a356328cabec929b33f41f555bd1fbe38", "2197745a509c6cf521ced2d29d67a6ae3a356328cabec929b33f41f555bd1fbe38b43e82ec6a4856448ece0ed25337ab"}},
	{"AES_256/OFB/NoPadding", 32, [9]string{"", "79", "7981b642d3b7a62540f2b9684dae94", "7981b642d3b7a62540f2b9684dae9497", "7981b642d3b7a62540f2b9684dae949753", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb75", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb7571", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb7571a2", "7981b642d3b7a62540f2b9684dae949753b8ef1d23ea3e01c9f11f3f1ebb7571a2056eb86bb5c22560cfd6e6745bfddd"}},
	{"2.16.840.1.101.3.4.1.42", 32, [9]string{"", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "ce9d699cf4634613504b7761c0ed0642", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "ce9d699cf4634613504b7761c0ed06424701964905cd00dc2af8ee4677b61892", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "ce9d699cf4634613504b7761c0ed06424701964905cd00dc2af8ee4677b61892a18feb5118ee2ac2494e44c487621c06"}},
	{"2.16.840.1.101.3.4.1.2/CTR/NoPadding", 16, [9]string{"", "2b", "2b99ace82b88b2e63747817eab4288", "2b99ace82b88b2e63747817eab42885c", "2b99ace82b88b2e63747817eab42885c1c", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560d9", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560d910", "2b99ace82b88b2e63747817eab42885c1c26016f9a4a591aed1a5c1e5e0560d9100f0da6d1a7e014023ba81d1700c543"}},
	{"2.16.840.1.101.3.4.1.23/ECB/PKCS5Padding", 24, [9]string{"3ab73130df4895eb0286bf604a9ba788", "21a82e2fc0578af41d99a07f5584b897", "2197745a509c6cf521ced2d29d67a699", "2197745a509c6cf521ced2d29d67a6ae9c0b4a6ecaad5a25fbbffd07decca386", "2197745a509c6cf521ced2d29d67a6aed7145571d5b2453ae4a0e218c1d3bc99", "2197745a509c6cf521ced2d29d67a6aed79bffb435a9736b680720e5d9e0d297", "2197745a509c6cf521ced2d29d67a6aed79bffb435a9736b680720e5d9e0d210e6612c8170ca4a77c6a2abc98fe1aa0b", "2197745a509c6cf521ced2d29d67a6aed79bffb435a9736b680720e5d9e0d2105d7e339e6fd55568d9bdb4d690feb514", "2197745a509c6cf521ced2d29d67a6aed79bffb435a9736b680720e5d9e0d2105da1c98b5fbed3c9054aa69bf87d0bcdd38d2d99f545ad5fb4e46cb9abd4bdee"}},
	{"2.16.840.1.101.3.4.1.44/CTS/NoPadding", 32, [9]string{"", "!IllegalBlockSizeException: input is too short!", "!IllegalBlockSizeException: input is too short!", "ce9d699cf4634613504b7761c0ed0642", "6b5f4c3c525e7e078440c9d27b950317ce", "3299f4debe9c7f99896de24bad8e3c2fce9d699cf4634613504b7761c0ed06", "4701964905cd00dc2af8ee4677b61892ce9d699cf4634613504b7761c0ed0642", "ce9d699cf4634613504b7761c0ed064228d14f2ab3ca96b3580cd9c965e751b247", "ce9d699cf4634613504b7761c0ed0642a18feb5118ee2ac2494e44c487621c064701964905cd00dc2af8ee4677b61892"}},
}

// jdkDecrypt: Java's plain text of jdkData's first bytes read as cipher text.
var jdkDecrypt = []struct {
	name    string
	keySize int
	out     [9]string
}{
	{"AES/CBC/NoPadding", 16, [9]string{"", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "74492210e51a8cb1f2702f244a07eea5", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "74492210e51a8cb1f2702f244a07eea533491013b2c452fe73e66b3cab7e5ee0", "!IllegalBlockSizeException: Input length not multiple of 16 bytes", "74492210e51a8cb1f2702f244a07eea533491013b2c452fe73e66b3cab7e5ee01b0e4a39141781b46278f865ee9adf91"}},
	{"AES/CBC/PKCS5Padding", 16, [9]string{"", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption."}},
	{"AES/CBC/ISO10126Padding", 16, [9]string{"", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption."}},
	{"AES/PCBC/PKCS5Padding", 16, [9]string{"", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption."}},
	{"AES/CFB/PKCS5Padding", 16, [9]string{"", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption."}},
	{"AES/OFB8/ISO10126Padding", 16, [9]string{"", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption.", "!IllegalBlockSizeException: Input length must be multiple of 16 when decrypting with padded cipher", "!BadPaddingException: Given final block not properly padded. Such issues can arise if a bad key is used during decryption."}},
	{"AES/CTS/NoPadding", 16, [9]string{"", "!IllegalBlockSizeException: input is too short!", "!IllegalBlockSizeException: input is too short!", "74492210e51a8cb1f2702f244a07eea5", "e9a23ce74b80545b1cb5bc39f5458d2f20", "4bc119d02570b0acd794d981bc64948220c78ad60104bce776dee7d25e398e", "37774865260ab2f847b8139a6f904ed620c78ad60104bce776dee7d25e398e23", "74492210e51a8cb1f2702f244a07eea580d9d5c4014d10c6552444c9e866dde693", "74492210e51a8cb1f2702f244a07eea54bbeba8964c751e4d28848353e4aaf2193a9b0731264325e93460bdc0b1efe00"}},
}

// jdkISO10126: Java's cipher text, under its random filler, of jdkData's first
// 0, 1, 15, 16, 17, 31, 32, 33 and 48 bytes.
var jdkISO10126 = []struct {
	name    string
	keySize int
	out     [9]string
}{
	{"AES/CBC/ISO10126Padding", 16, [9]string{"85fe88d87e44dc3937277c66b43f0fcc", "216fd94d06feef4e5f0d648f39fe7880", "f43efc2197413a9750aa26e7676d734f", "7b1eb12dbae30af43643545968377b67136f964f7ff76ef7802cfa5af04bd988", "7b1eb12dbae30af43643545968377b6743d6359e35018455a4724495791481ad", "7b1eb12dbae30af43643545968377b6749327d5b75acad3f79c605f89abf1173", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f2bce0c5c94625673b54d20ed97640504", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f9974a233173dff56f46d4f1fffdb1602", "7b1eb12dbae30af43643545968377b6704181653c38c6df5342c827bfc587f4f0274258d82ec4273c9739ccc177fc89bd41ef178ede72138998a3c881c5eefaa"}},
	{"AES/PCBC/ISO10126Padding", 16, [9]string{"3715feee7fec2f6efe39a8ee499aa916", "c996711937f8b5ae5027e5a0c43e2e6e", "f43efc2197413a9750aa26e7676d734f", "7b1eb12dbae30af43643545968377b67054da6e959465cee9f7851ba6c42551f", "7b1eb12dbae30af43643545968377b6726df900262c43deee0b81e9b43985164", "7b1eb12dbae30af43643545968377b67009abeff1592ed0df9ce7fe1a64393ac", "7b1eb12dbae30af43643545968377b67c1182fc40bbdcf93abcc9885833687c03e9a98bd851897f05d8d21ca38502307", "7b1eb12dbae30af43643545968377b67c1182fc40bbdcf93abcc9885833687c06ac7dba778bdafc86c94b8ae53bb828f", "7b1eb12dbae30af43643545968377b67c1182fc40bbdcf93abcc9885833687c0785017372dffb259814bf70a18c167a2596112aa48ddfb9185930854106f2c39"}},
	{"AES/CFB/ISO10126Padding", 16, [9]string{"0bb61423281f3333a9d04a08cfe6927a", "2b204f62b1342074fd2dfed0b7ad5f65", "2b99ace82b88b2e63747817eab42886b", "2b99ace82b88b2e63747817eab42885c84005368dcd4ec1b1a922d834f2a3c13", "2b99ace82b88b2e63747817eab42885c500fe88ae4cef6a980073fa05c22760c", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd36773902", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd36773985b94ebed89c33283d700c3fef1f4013e6", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739850fb4cc67b5ef942ab06b595b340f17f9", "2b99ace82b88b2e63747817eab42885c507f05ad9df74f75aad363cd367739850f88e95807ae216d88ffe5cfbeebe82070194cd78538fe5b49f4266bdead227d"}},
	{"AES/OFB32/ISO10126Padding", 16, [9]string{"ba36c04f0dda5a2be487a6dfbab9de95", "2b5928adc4887f03126af8edea6a5d8a", "2b99ace8a96cdc8cdc8edc0f23945b84", "2b99ace8a96cdc8cdc8edc0f23945bb3580ba630a8aebc68081f2b9fb3278a48", "2b99ace8a96cdc8cdc8edc0f23945bb3facb28a78dd905853f9c3c6464ccbd57", "2b99ace8a96cdc8cdc8edc0f23945bb3faee5b544631321dad46bcc02fdc5659", "2b99ace8a96cdc8cdc8edc0f23945bb3faee5b544631321dad46bcc02fdc56defa5391690bc771a61e36f8eddeebc42c", "2b99ace8a96cdc8cdc8edc0f23945bb3faee5b544631321dad46bcc02fdc56de5c813e5f04b7b78deb40d12c1719db33", "2b99ace8a96cdc8cdc8edc0f23945bb3faee5b544631321dad46bcc02fdc56de5cfe9a781610763d7ce8964e9443dbeacea5bf09315d6a15ddc151d716c574a6"}},
	{"OID.2.16.840.1.101.3.4.1.42/CFB8/ISO10126Padding", 32, [9]string{"1aa7328e55f0a28c2c3a7780a9295807", "798565fe98f5fd7053cecbbeb7a43021", "796ab31c7f5acb9c0330101294234e40", "796ab31c7f5acb9c0330101294234e77f5ce74f005857cec936aaf4b339b0c49", "796ab31c7f5acb9c0330101294234e7746f84294cf486f71d99fcb8ec3bf8d98", "796ab31c7f5acb9c0330101294234e77462176462c46875064e82582a306da21", "796ab31c7f5acb9c0330101294234e77462176462c46875064e82582a306daa63415f8a54c97694cd7bf6b1fd5a6a0f6", "796ab31c7f5acb9c0330101294234e77462176462c46875064e82582a306daa65eff1130ae7999de5465a9e544049662", "796ab31c7f5acb9c0330101294234e77462176462c46875064e82582a306daa65ee7ac987e0d15cd37e8885bc304d58d376c6e7c2112e89c8d53919229268561"}},
}

// jdkAccepts: what Java does encrypting jdkData's first 17 bytes, then its first
// 32, under each name with a 16-, 24- and 32-byte key: "+" it encrypts, else
// the exception it throws: N NoSuchAlgorithmException (getInstance), P
// InvalidAlgorithmParameterException and K InvalidKeyException (init, which
// checks the key first), B IllegalBlockSizeException (doFinal).
var jdkAccepts = []struct{ name, codes string }{
	{"AES/CBC/NoPadding", "BBB +++"},
	{"AES/CBC/PKCS5Padding", "+++ +++"},
	{"AES/CBC/ISO10126Padding", "+++ +++"},
	{"AES/PCBC/NoPadding", "BBB +++"},
	{"AES/PCBC/PKCS5Padding", "+++ +++"},
	{"AES/PCBC/ISO10126Padding", "+++ +++"},
	{"AES/CTS/NoPadding", "+++ +++"},
	{"AES/CTR/NoPadding", "+++ +++"},
	{"AES/CFB/NoPadding", "+++ +++"},
	{"AES/CFB/PKCS5Padding", "+++ +++"},
	{"AES/CFB/ISO10126Padding", "+++ +++"},
	{"AES/CFB8/NoPadding", "+++ +++"},
	{"AES/CFB8/PKCS5Padding", "+++ +++"},
	{"AES/CFB16/NoPadding", "+++ +++"},
	{"AES/CFB24/NoPadding", "+++ +++"},
	{"AES/CFB64/NoPadding", "+++ +++"},
	{"AES/CFB120/PKCS5Padding", "+++ +++"},
	{"AES/CFB128/NoPadding", "+++ +++"},
	{"AES/OFB/NoPadding", "+++ +++"},
	{"AES/OFB/PKCS5Padding", "+++ +++"},
	{"AES/OFB8/NoPadding", "+++ +++"},
	{"AES/OFB32/ISO10126Padding", "+++ +++"},
	{"AES/OFB64/NoPadding", "+++ +++"},
	{"AES/OFB120/NoPadding", "+++ +++"},
	{"AES/OFB128/PKCS5Padding", "+++ +++"},
	{"aes/cbc/pkcs5padding", "+++ +++"},
	{" AES / OFB / PKCS5Padding ", "+++ +++"},
	{"AES /CBC/PKCS5Padding\t", "+++ +++"},
	{"aes/cfb24/nopadding", "+++ +++"},
	{"AES/Cfb16/NoPadding", "+++ +++"},
	{"AES_128/CBC/NoPadding", "BKK +KK"},
	{"AES_128/CFB/NoPadding", "+KK +KK"},
	{"AES_128/OFB/NoPadding", "+KK +KK"},
	{"AES_192/CBC/NoPadding", "KBK K+K"},
	{"AES_192/CFB/NoPadding", "K+K K+K"},
	{"AES_256/OFB/NoPadding", "KK+ KK+"},
	{"aes_256/cfb/nopadding", "KK+ KK+"},
	{"2.16.840.1.101.3.4.1.2", "BKK +KK"},
	{"2.16.840.1.101.3.4.1.3", "+KK +KK"},
	{"2.16.840.1.101.3.4.1.4", "+KK +KK"},
	{"OID.2.16.840.1.101.3.4.1.22", "KBK K+K"},
	{"oid.2.16.840.1.101.3.4.1.23", "K+K K+K"},
	{"2.16.840.1.101.3.4.1.24", "K+K K+K"},
	{"2.16.840.1.101.3.4.1.42", "KKB KK+"},
	{"OID.2.16.840.1.101.3.4.1.43", "KK+ KK+"},
	{" 2.16.840.1.101.3.4.1.44 ", "KK+ KK+"},
	{"2.16.840.1.101.3.4.1/CTS/NoPadding", "+++ +++"},
	{"OID.2.16.840.1.101.3.4.1/CBC/PKCS5Padding", "+++ +++"},
	{"2.16.840.1.101.3.4.1.1/CBC/PKCS5Padding", "+KK +KK"},
	{"2.16.840.1.101.3.4.1.2/CTR/NoPadding", "+KK +KK"},
	{"2.16.840.1.101.3.4.1.22/PCBC/PKCS5Padding", "K+K K+K"},
	{"OID.2.16.840.1.101.3.4.1.42/CFB8/ISO10126Padding", "KK+ KK+"},
	{"2.16.840.1.101.3.4.1.44/CTS/NoPadding", "KK+ KK+"},
	{"2.16.840.1.101.3.4.1.2/ECB/NoPadding", "BKK +KK"},
	{"2.16.840.1.101.3.4.1.23/ECB/PKCS5Padding", "K+K K+K"},
	{"2.16.840.1.101.3.4.1.44/ECB/ISO10126Padding", "KK+ KK+"},
	{"AES/CBC/PKCS5PADD\u0130NG", "NNN NNN"},
	{"AES/CBC/PKCS5Padd\u0131ng", "+++ +++"},
	{"AES/CBC/P\u212aCS5Padding", "NNN NNN"},
	{"AES/CBC/PKC\u017f5Padding", "+++ +++"},
	{"\u00e4es/CBC/NoPadding", "NNN NNN"},
	{"o\u0131d.2.16.840.1.101.3.4.1.2", "BKK +KK"},
	{"AES/GCM/PKCS5Padding", "NNN NNN"},
	{"AES/KW/PKCS5Padding", "PPP PPP"},
	{"AES/KWP/NoPadding", "PPP PPP"},
	{"AES/KWP/PKCS5Padding", "NNN NNN"},
	{"AES/ECB/ISO10126Padding", "PPP PPP"},
	{"AES_128/GCM/NoPadding", "PPP PPP"},
	{"AES_192/KW/PKCS5Padding", "KPK KPK"},
	{"AES_256/KWP/NoPadding", "KKP KKP"},
	{"AES_128/KW/ISO10126Padding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.25", "KPK KPK"},
	{"2.16.840.1.101.3.4.1.48", "KKP KKP"},
	{"2.16.840.1.101.3.4.1.2/KW/NoPadding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.21/ECB/NoPadding", "KPK KPK"},
	{"2.16.840.1.101.3.4.1.21/CBC/NoPadding", "KBK K+K"},
	{"2.16.840.1.101.3.4.1/GCM/NoPadding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1/KW/NoPadding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.5/KW/NoPadding", "PKK PKK"},
	{"2.16.840.1.101.3.4.1.5/KW/PKCS5Padding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.5/KWP/NoPadding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.8/KWP/NoPadding", "PKK PKK"},
	{"2.16.840.1.101.3.4.1.8/KW/NoPadding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.6/GCM/NoPadding", "PPP PPP"},
	{"2.16.840.1.101.3.4.1.26/GCM/NoPadding", "PPP PPP"},
	{"2.16.840.1.101.3.4.1.1/GCM/NoPadding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.45/ECB/NoPadding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.5/ECB/NoPadding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.1/ECB/PKCS5Padding", "PKK PKK"},
	{"AES_192/ECB/PKCS5Padding", "NNN NNN"},
	{"AES/KW/ISO10126Padding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.2/CTS/NoPadding", "+KK +KK"},
	{"2.16.840.1.101.3.4.1.44/ECB/NoPadding", "KK+ KK+"},
	{"2.16.840.1.101.3.4.1.9", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.49", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.0", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.20", "NNN NNN"},
	{"AES", "PPP PPP"},
	{"AES/ECB/PKCS5Padding", "PPP PPP"},
	{"AES/ECB/NoPadding", "PPP PPP"},
	{"AES/GCM/NoPadding", "PPP PPP"},
	{"AES/KW/NoPadding", "PPP PPP"},
	{"AES/KWP/NoPadding", "PPP PPP"},
	{"AES/CTS/PKCS5Padding", "NNN NNN"},
	{"AES/CTS/ISO10126Padding", "NNN NNN"},
	{"ChaCha20-Poly1305", "PPP PPP"},
	{"ChaCha20/None/NoPadding", "PPP PPP"},
	{"DESede/CBC/PKCS5Padding", "PPP PPP"},
	{"DES/CBC/PKCS5Padding", "PPP PPP"},
	{"Blowfish/CBC/PKCS5Padding", "PPP PPP"},
	{"Rijndael/CBC/PKCS5Padding", "NNN NNN"},
	{"AES/CTR/PKCS5Padding", "NNN NNN"},
	{"AES/CTR/ISO10126Padding", "NNN NNN"},
	{"AES/CFB4/NoPadding", "NNN NNN"},
	{"AES/CFB12/NoPadding", "NNN NNN"},
	{"AES/CFB136/NoPadding", "NNN NNN"},
	{"AES/OFB0/NoPadding", "NNN NNN"},
	{"AES/CFB0/NoPadding", "NNN NNN"},
	{"AES/CFB008/NoPadding", "NNN NNN"},
	{"AES/CFB+8/NoPadding", "NNN NNN"},
	{"AES/CFB 8/NoPadding", "NNN NNN"},
	{"AES/CFB-8/NoPadding", "NNN NNN"},
	{"AES/CBC/PKCS7Padding", "NNN NNN"},
	{"AES/XYZ/NoPadding", "NNN NNN"},
	{"AES_128", "NNN NNN"},
	{"AES_128/CBC/PKCS5Padding", "NNN NNN"},
	{"AES_256/CFB8/NoPadding", "NNN NNN"},
	{"AES_128/CTR/NoPadding", "NNN NNN"},
	{"AES_128/PCBC/NoPadding", "NNN NNN"},
	{"AES_128/CTS/NoPadding", "NNN NNN"},
	{"AES_128/ECB/NoPadding", "PKK PKK"},
	{"AES_192/GCM/NoPadding", "PPP PPP"},
	{"AES/CBC", "NNN NNN"},
	{"AES//PKCS5Padding", "NNN NNN"},
	{"AES//CBC/PKCS5Padding", "NNN NNN"},
	{"AES/CBC/PKCS5Padding/Extra", "NNN NNN"},
	{"AES/CBC/NoPadding/", "NNN NNN"},
	{"/AES/CBC/NoPadding", "NNN NNN"},
	{"", "NNN NNN"},
	{"2.16.840.1.101.3.4.1", "PPP PPP"},
	{"OID.2.16.840.1.101.3.4.1", "PPP PPP"},
	{"2.16.840.1.101.3.4.1.1", "PKK PKK"},
	{"2.16.840.1.101.3.4.1.6", "PPP PPP"},
	{"2.16.840.1.101.3.4.1.5", "PKK PKK"},
	{"2.16.840.1.101.3.4.1.8", "PKK PKK"},
	{"2.16.840.1.101.3.4.1.7", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.21", "KPK KPK"},
	{"2.16.840.1.101.3.4.1.46", "PPP PPP"},
	{"2.16.840.1.101.3.4.1.02", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.6/CBC/PKCS5Padding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.5/CBC/PKCS5Padding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.2/GCM/NoPadding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.2/CTR/PKCS5Padding", "NNN NNN"},
	{"2.16.840.1.101.3.4.1.1/ECB/NoPadding", "PKK PKK"},
	{"2.16.840.1.101.3.4.1/ECB/NoPadding", "PPP PPP"},
	{"OID2.16.840.1.101.3.4.1.2", "NNN NNN"},
}
