package recordlayer

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/internal/recordlayer/keystoretest"
	"fdb.dev/internal/recordlayer/rlcrypto"
)

// JCEKS stores are read as Java's JceKeyStore reads them. A store is written
// into a temp dir by keystoretest.WriteJCEKS (no key store file may be
// committed); the bytes the JDK itself writes are pinned here by a sealed key
// and a serialized KeyRep cut from stores JDK 21's keytool wrote, and read
// whole by the JVM spec "TransformedRecordSerializer records are read and
// written as Java's".

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
		got, err := rlcrypto.PBEWithMD5AndTripleDESDecrypt(c, "entry-pass", s, 1000)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("salt %s: %x, %v", salt, got, err)
		}
		sealed, err := keystoretest.SealPBEWithMD5AndTripleDES(want, "entry-pass", s, 1000)
		if err != nil || !bytes.Equal(sealed, c) {
			t.Errorf("salt %s: the test writer seals %x, %v", salt, sealed, err)
		}
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
