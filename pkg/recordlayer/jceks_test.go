package recordlayer

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/keystoretest"
)

// JCEKS stores are read as Java's JceKeyStore reads them. A store is written
// into a temp dir by keystoretest.WriteJCEKS (no key store file may be
// committed); the bytes the JDK itself writes are pinned here by a sealed key
// and a serialized KeyRep cut from stores JDK 21's keytool wrote, and read
// whole by the JVM spec "TransformedRecordSerializer records are read and
// written as Java's".

// TestJCEKS_UnsealsWhatTheJDKSealed: a secret key entry's serialized
// SealedObject, cut whole from a JCEKS store JDK 21's keytool wrote (keytool
// -genseckey -keyalg AES -keysize 128, key password "entry-pass"; salt
// 6531ca72f1a9b20a, 200000 iterations), unsealed to the key the JDK read back.
func TestJCEKS_UnsealsWhatTheJDKSealed(t *testing.T) {
	t.Parallel()
	outer, _ := hex.DecodeString(
		"aced000573720033636f6d2e73756e2e63727970746f2e70726f76696465722e5365616c65644f626a656374466f724b" +
			"657950726f746563746f72cd57ca59e730bb53020000787200196a617661782e63727970746f2e5365616c65644f626a" +
			"6563743e363da6c3b754700200045b000d656e636f646564506172616d737400025b425b0010656e6372797074656443" +
			"6f6e74656e7471007e00024c0009706172616d73416c677400124c6a6176612f6c616e672f537472696e673b4c000773" +
			"65616c416c6771007e00037870757200025b42acf317f8060854e0020000787000000011300f04086531ca72f1a9b20a" +
			"0203030d407571007e00050000009020b5383c09fd7d3b6d94b495fb29b821125016b2124bf9bc83d586a2a4400a1cff" +
			"e437c50f84d1035c0190011a60425b95989124bf8bbe7ab4535535b9baec879ff59444068736be0a56d61a5add0e54e7" +
			"781f774cdfb20e49e22fbe00f1ccf4aede817f9666cccd4f8bbc92809d77862bd0cc54d53bd9bbe138d0ffab0eb35c61" +
			"da484281b6e857980518dd7c9c94e5740016504245576974684d4435416e64547269706c654445537400165042455769" +
			"74684d4435416e64547269706c65444553")
	sealed, n, err := readJavaObject(outer, nil)
	if err != nil || n != len(outer) {
		t.Fatalf("read %d of %d bytes: %v", n, len(outer), err)
	}
	k, err := unsealJCEKSKey(sealed, "entry-pass")
	if err != nil || k.algorithm != "AES" || hex.EncodeToString(k.bytes) != "0266730ab1dd2d62beaf03947ad9d3b4" {
		t.Fatalf("got %s %x, %v", k.algorithm, k.bytes, err)
	}
	if _, err := unsealJCEKSKey(sealed, "wrong-pass"); err == nil {
		t.Error("a wrong password unsealed the key")
	}
}

// TestJCEKS_PBEMatchesTheJDK: JDK 21's PBEWithMD5AndTripleDES Cipher (key
// password "entry-pass", 1000 iterations) over the 17 bytes 00..10, under a
// salt of two equal halves, which the JDK reverses the first of, and one of
// two different halves.
func TestJCEKS_PBEMatchesTheJDK(t *testing.T) {
	t.Parallel()
	want, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f10")
	for salt, ct := range map[string]string{
		"0102030401020304": "330bccbba881b94fe3fbda88a4c4435bb7c0fd8ced68a4d4",
		"0102030405060708": "c79178ebcb508631f142ba86170322ba20fc3d4998cfd4ef",
	} {
		s, _ := hex.DecodeString(salt)
		c, _ := hex.DecodeString(ct)
		got, err := pbeWithMD5AndTripleDESDecrypt(c, "entry-pass", s, 1000)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("salt %s: %x, %v", salt, got, err)
		}
		sealed, err := keystoretest.SealPBEWithMD5AndTripleDES(want, "entry-pass", s, 1000)
		if err != nil || !bytes.Equal(sealed, c) {
			t.Errorf("salt %s: the test writer seals %x, %v", salt, sealed, err)
		}
	}
}

// TestJCEKS_ReadsAKeyRep: the serialized key of a DESede secret key entry
// JDK 21's keytool wrote, a KeyRep (DESedeKey replaces itself with one),
// unsealed.
func TestJCEKS_ReadsAKeyRep(t *testing.T) {
	t.Parallel()
	plain, _ := hex.DecodeString(
		"aced0005737200146a6176612e73656375726974792e4b6579526570bdf94fb3889aa5430200044c0009616c676f7269" +
			"74686d7400124c6a6176612f6c616e672f537472696e673b5b0007656e636f6465647400025b424c0006666f726d6174" +
			"71007e00014c00047479706574001b4c6a6176612f73656375726974792f4b657952657024547970653b787074000644" +
			"4553656465757200025b42acf317f8060854e00200007870000000181916b97a084c2c7931c8fda134343b3ba7c81a9b" +
			"7383ad4f7400035241577e7200196a6176612e73656375726974792e4b65795265702454797065000000000000000012" +
			"00007872000e6a6176612e6c616e672e456e756d00000000000000001200007870740006534543524554")
	v, _, err := readJavaObject(plain, func(name string) bool { return jceksKeyClasses[name] })
	if err != nil {
		t.Fatal(err)
	}
	obj := v.(*javaObject)
	typ, _ := obj.fields["type"].(javaEnum)
	if obj.class.name != "java.security.KeyRep" || obj.fields["algorithm"] != "DESede" || obj.fields["format"] != "RAW" ||
		typ.constant != "SECRET" || !typ.class.isA("java.lang.Enum") {
		t.Fatalf("got %+v", obj.fields)
	}
	if key, _ := obj.fields["encoded"].([]byte); hex.EncodeToString(key) != "1916b97a084c2c7931c8fda134343b3ba7c81a9b7383ad4f" {
		t.Errorf("encoded %x", key)
	}
}

// writeJCEKS writes a JCEKS store into a temp dir and returns its path.
func writeJCEKS(t *testing.T, storePassword, keyPassword string, entries ...keystoretest.JCEKSEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.jceks")
	if err := keystoretest.WriteJCEKS(path, storePassword, keyPassword, 1000, entries); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJCEKS_KeyManagerReadsTheStore(t *testing.T) {
	t.Parallel()
	k16, k24, k32 := fixtureKey(0x01, 16), fixtureKey(0x40, 24), fixtureKey(0x80, 32)
	path := writeJCEKS(t, "store pass!", "entry-pass",
		keystoretest.JCEKSEntry{Alias: "Key-A", Key: k16},
		keystoretest.JCEKSEntry{Alias: "key-b", Key: k24},
		keystoretest.JCEKSEntry{Alias: "kc", Key: k32},
		keystoretest.JCEKSEntry{Alias: "des3", Key: fixtureKey(0x10, 24), Algorithm: "DESede", KeyRep: true},
		keystoretest.JCEKSEntry{Alias: "rsa", Key: []byte("not parsed"), Kind: keystoretest.JCEKSPrivateKey},
		keystoretest.JCEKSEntry{Alias: "cert", Key: []byte("not parsed"), Kind: keystoretest.JCEKSTrustedCert},
	)
	km, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(path).SetKeyStorePassword("store pass!").
		SetKeyEntryPassword("entry-pass").SetKeyEntryAliases([]string{"KEY-A", "Key-B", "kc", "des3", "rsa", "cert", "absent"}).Build()
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range [][]byte{k16, k24, k32, fixtureKey(0x10, 24)} {
		got, err := km.GetKey(int32(i)) //nolint:gosec
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("key %d: %x, %v", i, got, err)
		}
	}
	if alg, err := km.KeyAlgorithm(3); err != nil || alg != "DESede" {
		t.Errorf("the KeyRep's algorithm: %q, %v", alg, err)
	}
	// A private key, a trusted certificate and an absent alias serve no key.
	for i := int32(4); i <= 6; i++ {
		_, err := km.GetKey(i)
		wantSerializationError(t, err, "cannot load key")
	}
}

func TestJCEKS_Refusals(t *testing.T) {
	t.Parallel()
	path := writeJCEKS(t, "storepass", "keypass", keystoretest.JCEKSEntry{Alias: "k", Key: fixtureKey(0x01, 16)})
	build := func(storePassword string) (*KeyStoreSerializationKeyManager, error) {
		return NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(path).SetKeyStorePassword(storePassword).
			SetKeyEntryPassword("keypass").SetDefaultKeyEntryAlias("k").Build()
	}
	// The wrong store password fails the digest, as JceKeyStore's load does.
	_, err := build("wrong")
	var argErr *RecordCoreArgumentError
	if !errors.As(err, &argErr) || argErr.Message != "Key store loading failed" ||
		!strings.Contains(argErr.Cause.Error(), "Keystore was tampered with, or password was incorrect") {
		t.Errorf("wrong store password: %v", err)
	}
	// A key password that does not unseal the key, or is not ASCII.
	for _, pw := range []string{"wrong", "k\u00e9y"} {
		km, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(path).SetKeyStorePassword("storepass").
			SetKeyEntryPassword(pw).SetDefaultKeyEntryAlias("k").Build()
		if err != nil {
			t.Fatal(err)
		}
		_, err = km.GetKey(0)
		wantSerializationError(t, err, "cannot load key")
	}
	// A changed byte fails the digest; a cut store is truncated.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"changed": append(append([]byte(nil), data[:20]...), append([]byte{data[20] ^ 1}, data[21:]...)...),
		"cut":     data[:len(data)-30],
	} {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := build("storepass"); !errors.As(err, &argErr) || argErr.Message != "Key store loading failed" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The empty password seals and keys a JCEKS store, as the JDK writes one.
func TestJCEKS_EmptyPasswords(t *testing.T) {
	t.Parallel()
	path := writeJCEKS(t, "", "", keystoretest.JCEKSEntry{Alias: "k", Key: fixtureKey(0x01, 32)})
	km, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(path).SetDefaultKeyEntryAlias("k").Build()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := km.GetKey(0); err != nil || !bytes.Equal(got, fixtureKey(0x01, 32)) {
		t.Errorf("%x, %v", got, err)
	}
}

// Records round-trip through a serializer keyed from a JCEKS store; a DESede
// key is refused by the AES cipher, as Java's refuses it.
func TestJCEKS_EncryptsRecords(t *testing.T) {
	t.Parallel()
	path := writeJCEKS(t, "storepass", "storepass",
		keystoretest.JCEKSEntry{Alias: "aes", Key: fixtureKey(0x01, 16)},
		keystoretest.JCEKSEntry{Alias: "des3", Key: fixtureKey(0x10, 24), Algorithm: "DESede", KeyRep: true})
	for _, c := range []struct{ alias, err string }{{"aes", ""}, {"des3", "Wrong algorithm: AES or Rijndael required"}} {
		km, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(path).SetKeyStorePassword("storepass").
			SetDefaultKeyEntryAlias(c.alias).Build()
		if err != nil {
			t.Fatal(err)
		}
		s := mustSerializer(t, NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).SetKeyManager(km))
		stored, err := s.transform(unionOf("record"), nil)
		if c.err != "" {
			wantSerializationError(t, err, c.err)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if got, err := s.untransform(stored); err != nil || !bytes.Equal(got, unionOf("record")) {
			t.Errorf("read %x, %v", got, err)
		}
	}
}
