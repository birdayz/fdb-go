package recordlayer

import (
	"crypto/sha1" //nolint:gosec // the JCEKS integrity digest is SHA-1
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"fdb.dev/internal/recordlayer/keystoretest"
	"fdb.dev/internal/recordlayer/rlcrypto"
	"fdb.dev/internal/recordlayer/rlwire"
)

// Malformed serialized objects and JCEKS stores are refused, each for its
// reason, before anything is trusted. Streams are written here as hex: a
// class descriptor is 72, its name, a serialVersionUID, its flags, its fields,
// 78, then its superclass (70 for none).

const (
	jsHead = "aced0005"
	jsUID  = "0000000000000001"
)

func jsHex(parts ...string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(strings.Join(parts, ""), " ", ""))
	if err != nil {
		panic(err)
	}
	return b
}

func jsUTF(s string) string    { return fmt.Sprintf("%04x", len(s)) + hex.EncodeToString([]byte(s)) }
func jsString(s string) string { return "74" + jsUTF(s) }

// jsDesc is a class descriptor whose fields are object fields, each a
// name and a type.
func jsDesc(name, flags, super string, fields ...[2]string) string {
	d := "72" + jsUTF(name) + jsUID + flags + fmt.Sprintf("%04x", len(fields))
	for _, f := range fields {
		d += hex.EncodeToString([]byte{f[1][0]}) + jsUTF(f[0]) + jsString(f[1])
	}
	return d + "78" + super
}

func jsBytes(b []byte) string {
	return "75" + jsDesc("[B", "02", "70") + fmt.Sprintf("%08x", len(b)) + hex.EncodeToString(b)
}

// The 30-byte JCEKS store that, read before a superclass had to be complete,
// looped forever: a secret key entry whose class descriptor names itself as its
// superclass.
const jceksSelfSuperclass = "cececece" + "00000002" + "00000001" +
	"00000003" + "0001" + "6b" + "0000000000000000" +
	"aced0005" + "73" + "72" + "0001" + "41" + "0000000000000000" + "02" + "0000" + "78" + "71007e0000"

// jceksStore is a store of raw entries under storePassword's digest.
func jceksStore(version int, storePassword string, entries ...string) []byte {
	return jceksSigned(jsHex(fmt.Sprintf("cececece%08x%08x", version, len(entries))+strings.Join(entries, "")), storePassword)
}

// jceksSigned is data followed by its digest under storePassword.
func jceksSigned(data []byte, storePassword string) []byte {
	h := sha1.New() //nolint:gosec // the JCEKS integrity digest is SHA-1
	for _, c := range utf16.Encode([]rune(storePassword)) {
		h.Write([]byte{byte(c >> 8), byte(c)})
	}
	h.Write([]byte("Mighty Aphrodite"))
	h.Write(data)
	return h.Sum(data)
}

// jceksEntry is an entry: its tag, its alias (as encoded), a creation date,
// then its body.
func jceksEntry(tag int, aliasHex, body string) string {
	return fmt.Sprintf("%08x%04x", tag, len(aliasHex)/2) + aliasHex + "0000000000000000" + body
}

// sealedKey is a SealedObject over inner, sealed under password.
func sealedKey(t *testing.T, sealAlg string, salt []byte, iterations int, password string, inner []byte) string {
	t.Helper()
	iter := big.NewInt(int64(iterations)).Bytes()
	if len(iter) == 0 || iter[0]&0x80 != 0 {
		iter = append([]byte{0}, iter...) // DER: minimal, non-negative
	}
	params := jsHex(fmt.Sprintf("30%02x04%02x", 2+len(salt)+2+len(iter), len(salt)), hex.EncodeToString(salt),
		fmt.Sprintf("02%02x", len(iter)), hex.EncodeToString(iter))
	content := []byte{0, 0, 0, 0, 0, 0, 0, 0}
	if iterations > 0 && iterations <= rlcrypto.JCEKSMaxIterations && len(salt) == 8 {
		var err error
		if content, err = keystoretest.SealPBEWithMD5AndTripleDES(inner, password, salt, iterations); err != nil {
			t.Fatal(err)
		}
	}
	return jsHead + "73" + jsDesc("com.sun.crypto.provider.SealedObjectForKeyProtector", "02",
		jsDesc("javax.crypto.SealedObject", "02", "70",
			[2]string{"encodedParams", "[B"}, [2]string{"encryptedContent", "[B"},
			[2]string{"paramsAlg", "Ljava/lang/String;"}, [2]string{"sealAlg", "Ljava/lang/String;"})) +
		jsBytes(params) + jsBytes(content) + jsString(sealAlg) + jsString(sealAlg)
}

func secretKeySpec(alg string, key []byte) []byte {
	return jsHex(jsHead, "73", jsDesc("javax.crypto.spec.SecretKeySpec", "02", "70",
		[2]string{"algorithm", "Ljava/lang/String;"}, [2]string{"key", "[B"}), jsString(alg), jsBytes(key))
}

func keyRep(typeClass, constant, format string, key []byte) []byte {
	return jsHex(jsHead, "73", jsDesc("java.security.KeyRep", "02", "70",
		[2]string{"algorithm", "Ljava/lang/String;"}, [2]string{"encoded", "[B"},
		[2]string{"format", "Ljava/lang/String;"}, [2]string{"type", "Ljava/security/KeyRep$Type;"}),
		jsString("AES"), jsBytes(key), jsString(format),
		"7e", jsDesc(typeClass, "12", jsDesc("java.lang.Enum", "12", "70")), jsString(constant))
}

func TestJCEKS_UnsealRefusals(t *testing.T) {
	t.Parallel()
	salt := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	key := fixtureKey(0x01, 16)
	for _, c := range []struct {
		name, sealed, want string
	}{
		{"not a SealedObject", jsHead + jsString("x"), "Invalid secret key format"},
		{"another seal", sealedKey(t, "PBEWithMD5AndDES", salt, 10, "kp", secretKeySpec("AES", key)), "sealed with"},
		{"a 7-byte salt", sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt[:7], 10, "kp", nil), "bad PBEWithMD5AndTripleDES parameters"},
		{"no iterations", sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt, 0, "kp", nil), "IterationCount must be a positive number"},
		{"too many iterations", sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt, rlcrypto.JCEKSMaxIterations+1, "kp", nil), "PBE iteration count too large"},
		{"a wrong key password", sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt, 10, "other", secretKeySpec("AES", key)), "not properly padded"},
		{"a non-key class", sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt, 10, "kp", jsHex(jsHead, "73", jsDesc("java.util.Date", "02", "70"))), "class java.util.Date rejected"},
		{"an Enum object", sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt, 10, "kp", jsHex(jsHead, "73", jsDesc("java.lang.Enum", "02", "70"))), "not a secret key"},
		{"a public KeyRep", sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt, 10, "kp", keyRep("java.security.KeyRep$Type", "PUBLIC", "RAW", key)), "not a raw secret key"},
		{"an X.509 KeyRep", sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt, 10, "kp", keyRep("java.security.KeyRep$Type", "SECRET", "X.509", key)), "not a raw secret key"},
		{"a KeyRep typed by Enum itself", sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt, 10, "kp", keyRep("java.lang.Enum", "SECRET", "RAW", key)), "not a raw secret key"},
	} {
		sealed, _, err := rlwire.ReadJavaObject(jsHex(c.sealed), nil)
		if err == nil {
			_, err = rlcrypto.UnsealJCEKSKey(sealed, "kp")
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// The same sealed key, well formed, unseals; as a KeyRep too.
	for _, inner := range [][]byte{secretKeySpec("AES", key), keyRep("java.security.KeyRep$Type", "SECRET", "RAW", key)} {
		sealed, _, err := rlwire.ReadJavaObject(jsHex(sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt, 10, "kp", inner)), nil)
		if err != nil {
			t.Fatal(err)
		}
		if k, err := rlcrypto.UnsealJCEKSKey(sealed, "kp"); err != nil || string(k.Bytes) != string(key) {
			t.Errorf("%x, %v", k.Bytes, err)
		}
	}
}

func TestJCEKS_StoreRefusals(t *testing.T) {
	t.Parallel()
	ok := jceksEntry(3, "6b", sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 10, "kp", secretKeySpec("AES", fixtureKey(1, 16))))
	good := jceksStore(2, "sp", ok)
	for _, c := range []struct {
		name string
		data []byte
		want string
	}{
		{"a 3rd version", jceksStore(3, "sp", ok), "Invalid keystore format"},
		{"an unknown tag", jceksStore(2, "sp", jceksEntry(4, "6b", "")), "Unrecognized keystore entry"},
		{"a cut digest", good[:len(good)-1], "truncated"},
		{"a changed digest", append(good[:len(good)-1:len(good)-1], good[len(good)-1]^1), "Keystore was tampered with"},
		{"the self-superclass crasher", jsHex(jceksSelfSuperclass), "Invalid secret key format"},
		{"a plain SealedObject", jceksStore(2, "sp", jceksEntry(3, "6b", jsHead+"73"+jsDesc("javax.crypto.SealedObject", "02", "70"))), "Invalid secret key format"},
		{"not a SealedObject", jceksStore(2, "sp", jceksEntry(3, "6b", jsHead+jsString("x"))), "Invalid secret key format"},
		{"a malformed alias", jceksStore(2, "sp", jceksEntry(3, "f09f9880", "")), "malformed input"},
	} {
		_, err := rlcrypto.ReadJCEKSEntries(c.data, "sp")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// A negative entry count, a Java int, reads no entries (measured).
	if entries, err := rlcrypto.ReadJCEKSEntries(jceksSigned(jsHex("cececece00000002ffffffff"), "sp"), "sp"); err != nil || len(entries) != 0 {
		t.Errorf("a negative count: %v, %v", entries, err)
	}
}

// What the JDK reads that is unusual, measured on JDK 21: a version 1 store
// (whose certificates carry no type), a later entry of an alias replacing an
// earlier one, and aliases in modified UTF-8 (NUL as C0 80 or 00, a
// supplementary character as encoded surrogates).
func TestJCEKS_ReadsAsTheJDK(t *testing.T) {
	t.Parallel()
	salt := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	sealed := func(k []byte) string {
		return sealedKey(t, rlcrypto.JCEKSPBEAlgorithm, salt, 10, "kp", secretKeySpec("AES", k))
	}
	k1, k2 := fixtureKey(0x01, 16), fixtureKey(0x20, 16)
	for _, c := range []struct {
		name  string
		data  []byte
		alias string
		want  []byte
	}{
		{"version 1, an untyped certificate", jceksStore(1, "sp", jceksEntry(3, "6b", sealed(k1)), jceksEntry(2, "63", "00000003300100")), "k", k1},
		{"a duplicate alias", jceksStore(2, "sp", jceksEntry(3, "6b", sealed(k1)), jceksEntry(3, "6b", sealed(k2))), "k", k2},
		{"NUL as C0 80", jceksStore(2, "sp", jceksEntry(3, "61c08062", sealed(k1))), "a\x00b", k1},
		{"NUL as 00", jceksStore(2, "sp", jceksEntry(3, "610062", sealed(k1))), "a\x00b", k1},
		{"a supplementary character as surrogates", jceksStore(2, "sp", jceksEntry(3, "78eda0bdedb880", sealed(k1))), "x\U0001F600", k1},
	} {
		path := filepath.Join(t.TempDir(), "keys.jceks")
		if err := os.WriteFile(path, c.data, 0o600); err != nil {
			t.Fatal(err)
		}
		km, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(path).SetKeyStorePassword("sp").
			SetKeyEntryPassword("kp").SetDefaultKeyEntryAlias(c.alias).Build()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got, err := km.GetKey(0); err != nil || string(got) != string(c.want) {
			t.Errorf("%s: %x, %v", c.name, got, err)
		}
	}
}

// FuzzReadJCEKSEntries: any store is read or refused without a panic.
// Reading decrypts nothing (keys unseal when asked for), so no input reaches
// the PBE iterations.
func FuzzReadJCEKSEntries(f *testing.F) {
	f.Add(jsHex(jceksSelfSuperclass))
	f.Add(jceksStore(2, "sp", jceksEntry(3, "6b", jsHead+"73"+jsDesc("A", "02", "70"))))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			return
		}
		_, _ = rlcrypto.ReadJCEKSEntries(data, "sp")
	})
}
