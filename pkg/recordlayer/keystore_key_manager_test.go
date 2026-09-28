package recordlayer

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // the PKCS #12 KDF vectors are SHA-1
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/keystoretest"
)

// These tests read key stores keystoretest writes into a temp dir, in the JDK
// 12+ layout: no key store file may be committed (cmd/secretscan). What they
// pin is the key manager's logic. Reading the bytes the JDK itself writes
// (PBES2, the empty password, a separate entry password, legacy 3DES under an
// HmacPBESHA1 MAC, the JCEKS refusal, the non-ASCII refusal) is pinned against
// the JVM by the conformance spec "TransformedRecordSerializer records are
// read and written as Java's". Each store holds "k1" (16 bytes 0x01..0x10) and
// "Mixed-Case" (32 bytes 0x20..0x3f), stored under the alias "mixed-case".

func fixtureKey(first byte, n int) []byte {
	k := make([]byte, n)
	for i := range k {
		k[i] = first + byte(i)
	}
	return k
}

var (
	keyK1        = fixtureKey(0x01, 16)
	keyMixedCase = fixtureKey(0x20, 32)
)

// writeKeyStore writes the two keys under storePassword and entryPassword
// and returns the file's path.
func writeKeyStore(t *testing.T, storePassword, entryPassword string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.p12")
	err := keystoretest.WritePKCS12(path, storePassword, entryPassword, []keystoretest.Entry{
		{Alias: "k1", Key: keyK1}, {Alias: "Mixed-Case", Key: keyMixedCase},
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPKCS12KDF pins RFC 7292 appendix B.2 with the vectors
// golang.org/x/crypto/pkcs12 checks (pbkdf_test.go): one long key, and one
// whose intermediate I_j gains a leading zero byte, the case a big-integer
// implementation of step 6C gets wrong.
func TestPKCS12KDF(t *testing.T) {
	t.Parallel()
	got := pkcs12KDF(sha1.New, 1, bmpPassword("sesame"), bytes.Repeat([]byte{0xff}, 8), 2048, 24)
	want := []byte("\x7c\xd9\xfd\x3e\x2b\x3b\xe7\x69\x1a\x44\xe3\xbe\xf0\xf9\xea\x0f\xb9\xb8\x97\xd4\xe3\x25\xd9\xd1")
	if !bytes.Equal(got, want) {
		t.Errorf("long key: got %x, want %x", got, want)
	}
	got = pkcs12KDF(sha1.New, 1, []byte{0, 0}, []byte("\xf3\x7e\x05\xb5\x18\x32\x4b\x4b"), 2048, 24)
	want = []byte("\x00\xf7\x59\xff\x47\xd1\x4d\xd0\x36\x65\xd5\x94\x3c\xb3\xc4\xa3\x9a\x25\x55\xc0\x2a\xed\x66\xe1")
	if !bytes.Equal(got, want) {
		t.Errorf("leading zero: got %x, want %x", got, want)
	}
}

// TestBMPPassword pins the JDK's PKCS12KeyStore.derive encoding: UTF-16BE
// with a two-byte terminator, the empty password being the terminator alone,
// and only "\x00" empty. The JVM spec's empty-password key store is the
// end-to-end check of the middle case against the JDK.
func TestBMPPassword(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want []byte
	}{
		{"", []byte{0, 0}},
		{"\x00", nil},
		{"ab", []byte{0, 'a', 0, 'b', 0, 0}},
		{"é𝄞", []byte{0x00, 0xe9, 0xd8, 0x34, 0xdd, 0x1e, 0, 0}},
	} {
		if got := bmpPassword(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("bmpPassword(%q) = %x, want %x", c.in, got, c.want)
		}
	}
}

// TestKeyStoreKeyManager_GetKeyReturnsACopy: the decrypted key is cached, and
// a caller that clears the key it was handed (as a careful caller does after
// use) must not clear it for the next.
func TestKeyStoreKeyManager_GetKeyReturnsACopy(t *testing.T) {
	t.Parallel()
	km, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(writeKeyStore(t, "storepass", "storepass")).
		SetKeyStorePassword("storepass").SetDefaultKeyEntryAlias("k1").Build()
	if err != nil {
		t.Fatal(err)
	}
	first, err := km.GetKey(0)
	if err != nil {
		t.Fatal(err)
	}
	clear(first)
	if again, err := km.GetKey(0); err != nil || !bytes.Equal(again, keyK1) {
		t.Errorf("after clearing a returned key, GetKey(0) = %x, %v; want %x", again, err, keyK1)
	}
}

// TestKeyStoreKeyManager_RetryWithZero: a key store written under "\x00", the
// empty password with no NUL terminator (PBEKey admits it; the PKCS #12 KDF
// reads it as no bytes and PBKDF2 as one zero byte), loads under "\x00" and,
// by the JDK's PKCS12KeyStore.RetryWithZero around the MAC check and each
// key's decryption, under the empty password. A store written under the empty
// password does not load under "\x00" (the retry runs one way only). The JVM
// spec reads such a store the JDK writes.
func TestKeyStoreKeyManager_RetryWithZero(t *testing.T) {
	t.Parallel()
	load := func(path, password string) (*KeyStoreSerializationKeyManager, error) {
		return NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(path).
			SetKeyStorePassword(password).SetKeyEntryAliases([]string{"k1", "Mixed-Case"}).Build()
	}
	nul := writeKeyStore(t, "\x00", "\x00")
	for _, pw := range []string{"\x00", ""} {
		km, err := load(nul, pw)
		if err != nil {
			t.Fatalf("the \\x00 store under %q: %v", pw, err)
		}
		for i, want := range [][]byte{keyK1, keyMixedCase} {
			if got, err := km.GetKey(int32(i)); err != nil || !bytes.Equal(got, want) { //nolint:gosec
				t.Errorf("the \\x00 store under %q, key %d: %x, %v", pw, i, got, err)
			}
		}
	}
	// The MAC holds under \x00 and the key does not: the key's own retry is
	// what reads it (a key password apart from the store's).
	mixed := writeKeyStore(t, "storepass", "\x00")
	km, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(mixed).SetKeyStorePassword("storepass").
		SetKeyEntryPassword("").SetDefaultKeyEntryAlias("k1").Build()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := km.GetKey(0); err != nil || !bytes.Equal(got, keyK1) {
		t.Errorf("a \\x00 key password read under the empty one: %x, %v", got, err)
	}
	empty := writeKeyStore(t, "", "")
	var argErr *RecordCoreArgumentError
	if _, err := load(empty, "\x00"); !errors.As(err, &argErr) || argErr.Message != "Key store loading failed" {
		t.Errorf("the empty-password store under \\x00: %v, want Key store loading failed", err)
	}
}

// TestKeyStoreKeyManager_ReadsAKeyStore reads each key by its alias, under a
// password and under the empty password Java's builder defaults to.
func TestKeyStoreKeyManager_ReadsAKeyStore(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		file, password string
		setPassword    bool
	}{
		{writeKeyStore(t, "storepass", "storepass"), "storepass", true},
		{writeKeyStore(t, "", ""), "", false}, // Java's builder defaults the password to ""
		{writeKeyStore(t, "", ""), "", true},
	} {
		b := NewKeyStoreSerializationKeyManagerBuilder().
			SetKeyStoreFileName(c.file).
			SetKeyEntryAliases([]string{"k1", "Mixed-Case"})
		if c.setPassword {
			b.SetKeyStorePassword(c.password)
		}
		km, err := b.Build()
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		for i, want := range [][]byte{keyK1, keyMixedCase} {
			got, err := km.GetKey(int32(i))
			if err != nil {
				t.Fatalf("%s: key %d: %v", c.file, i, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: key %d = %x, want %x", c.file, i, got, want)
			}
		}
		if got := km.GetSerializationKey(); got != 1 {
			t.Errorf("%s: serialization key %d, want the last alias's 1", c.file, got)
		}
		if cipher, _ := km.GetCipher(0); cipher != DefaultCipher {
			t.Errorf("%s: cipher %q, want %q", c.file, cipher, DefaultCipher)
		}
	}
}

// TestKeyStoreKeyManager_AliasesFoldAsTheJDKs: the JDK stores and looks up a
// PKCS12 alias lower-cased, so any spelling of "Mixed-Case" finds it.
func TestKeyStoreKeyManager_AliasesFoldAsTheJDKs(t *testing.T) {
	t.Parallel()
	store := writeKeyStore(t, "storepass", "storepass")
	for _, alias := range []string{"Mixed-Case", "mixed-case", "MIXED-CASE"} {
		km, err := NewKeyStoreSerializationKeyManagerBuilder().
			SetKeyStoreFileName(store).
			SetKeyStorePassword("storepass").
			SetDefaultKeyEntryAlias(alias).
			Build()
		if err != nil {
			t.Fatalf("%s: %v", alias, err)
		}
		if got, err := km.GetKey(0); err != nil || !bytes.Equal(got, keyMixedCase) {
			t.Errorf("%s: GetKey(0) = %x, %v", alias, got, err)
		}
	}
}

// TestKeyStoreKeyManager_EntryPassword: a key-entry password distinct from
// the store's. Java decrypts a key in getKey, not when the store loads, so a
// wrong or missing entry password builds and then fails there as "cannot load
// key".
func TestKeyStoreKeyManager_EntryPassword(t *testing.T) {
	t.Parallel()
	store := writeKeyStore(t, "storepass", "entrypass")
	build := func(entryPassword *string) *KeyStoreSerializationKeyManager {
		b := NewKeyStoreSerializationKeyManagerBuilder().
			SetKeyStoreFileName(store).
			SetKeyStorePassword("storepass").
			SetDefaultKeyEntryAlias("k1")
		if entryPassword != nil {
			b.SetKeyEntryPassword(*entryPassword)
		}
		km, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return km
	}
	right := "entrypass"
	if got, err := build(&right).GetKey(0); err != nil || !bytes.Equal(got, keyK1) {
		t.Errorf("right entry password: %x, %v", got, err)
	}
	wrong := "nope"
	for name, km := range map[string]*KeyStoreSerializationKeyManager{
		"wrong entry password":                build(&wrong),
		"entry password defaulted from store": build(nil),
	} {
		_, err := km.GetKey(0)
		var serErr *RecordSerializationError
		if !errors.As(err, &serErr) || serErr.Message != "cannot load key" || serErr.Cause == nil {
			t.Errorf("%s: got %v, want a RecordSerializationError \"cannot load key\"", name, err)
		}
	}
}

// TestKeyStoreKeyManager_KeyPasswordIsPrintableASCII: the JDK decrypts a key
// entry only under a printable-ASCII password (PBEKey: "Password is not
// ASCII", measured by the JVM spec in transformed_serializer_conformance_test.go),
// so a key store loads under a password outside it and its keys do not.
func TestKeyStoreKeyManager_KeyPasswordIsPrintableASCII(t *testing.T) {
	t.Parallel()
	store := writeKeyStore(t, "storepass", "storepass")
	for _, pw := range []string{"pässwörd", "tab\there", "del\x7f"} {
		km, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(store).
			SetKeyStorePassword("storepass").SetKeyEntryPassword(pw).SetDefaultKeyEntryAlias("k1").Build()
		if err != nil {
			t.Fatalf("%q: the store loads: %v", pw, err)
		}
		_, err = km.GetKey(0)
		var serErr *RecordSerializationError
		if !errors.As(err, &serErr) || serErr.Error() != "cannot load key: Password is not ASCII" {
			t.Errorf("%q: got %v, want \"cannot load key: Password is not ASCII\"", pw, err)
		}
	}
	// A store password outside printable ASCII fails the MAC check, as the
	// JDK's does (it will not write such a store).
	nonASCIIStore := filepath.Join(t.TempDir(), "store.p12")
	if err := keystoretest.WritePKCS12(nonASCIIStore, "pässwörd", "storepass", []keystoretest.Entry{{Alias: "k1", Key: keyK1}}); err != nil {
		t.Fatal(err)
	}
	_, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(nonASCIIStore).
		SetKeyStorePassword("pässwörd").SetKeyEntryPassword("storepass").SetDefaultKeyEntryAlias("k1").Build()
	var argErr *RecordCoreArgumentError
	if !errors.As(err, &argErr) || argErr.Message != "Key store loading failed" || argErr.Cause == nil ||
		!strings.Contains(argErr.Cause.Error(), "Password is not ASCII") {
		t.Errorf("a non-ASCII store password: %v, want Key store loading failed: Password is not ASCII", err)
	}
	// Every printable ASCII character is admitted (the wrong password then
	// fails the decryption, not the check).
	all := make([]byte, 0, 0x7f-0x20)
	for c := byte(0x20); c <= 0x7e; c++ {
		all = append(all, c)
	}
	if err := checkPBEPassword(string(all)); err != nil {
		t.Errorf("printable ASCII refused: %v", err)
	}
}

// TestKeyStoreKeyManager_BuildRefusals ports Builder.build()'s refusals, each
// a RecordCoreArgumentException in Java.
func TestKeyStoreKeyManager_BuildRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.p12")
	if err := os.WriteFile(garbage, []byte("not a key store"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A JCEKS file starts with the magic 0xCECECECE (the JVM spec refuses a
	// whole one the JDK writes).
	jceks := filepath.Join(dir, "store.jceks")
	if err := os.WriteFile(jceks, []byte{0xce, 0xce, 0xce, 0xce, 0, 0, 0, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	store := writeKeyStore(t, "storepass", "storepass")
	for _, c := range []struct {
		name  string
		b     *KeyStoreSerializationKeyManagerBuilder
		want  string
		cause string
	}{
		{
			"no file", NewKeyStoreSerializationKeyManagerBuilder().SetDefaultKeyEntryAlias("k1"),
			"must specify key store file name", "",
		},
		{"missing file", NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(filepath.Join(dir, "absent.p12")).
			SetDefaultKeyEntryAlias("k1"), "Key store not found", "no such file"},
		{"wrong store password", NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(store).
			SetKeyStorePassword("wrong").SetDefaultKeyEntryAlias("k1"), "Key store loading failed", "keystore password was incorrect"},
		{"not a key store", NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(garbage).
			SetDefaultKeyEntryAlias("k1"), "Key store loading failed", "not a PKCS12 key store"},
		{"JCEKS", NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(jceks).
			SetKeyStorePassword("storepass").SetDefaultKeyEntryAlias("k1"), "Key store loading failed", "JCEKS"},
		{
			"no aliases", NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(store).SetKeyStorePassword("storepass"),
			"must specify key alias list or single default alias", "",
		},
		{"empty alias list", NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(store).SetKeyStorePassword("storepass").
			SetKeyEntryAliases(nil), "need at least one key alias", ""},
		{"default not listed", NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(store).SetKeyStorePassword("storepass").
			SetKeyEntryAliases([]string{"k1"}).SetDefaultKeyEntryAlias("Mixed-Case"), "default key alias not in key alias list", ""},
	} {
		_, err := c.b.Build()
		var argErr *RecordCoreArgumentError
		if !errors.As(err, &argErr) || argErr.Message != c.want {
			t.Errorf("%s: got %v, want RecordCoreArgumentError %q", c.name, err, c.want)
			continue
		}
		if c.cause == "" {
			if argErr.Cause != nil {
				t.Errorf("%s: unexpected cause %v", c.name, argErr.Cause)
			}
		} else if argErr.Cause == nil || !strings.Contains(argErr.Cause.Error(), c.cause) {
			t.Errorf("%s: cause %v, want one mentioning %q", c.name, argErr.Cause, c.cause)
		}
	}
	// The missing file's cause is the OS's, reachable through the chain.
	_, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(filepath.Join(dir, "absent.p12")).
		SetDefaultKeyEntryAlias("k1").Build()
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v does not unwrap to os.ErrNotExist", err)
	}
}

// TestKeyStoreKeyManager_DefaultKeyNumber ports build()'s three ways to name
// the key records are written with.
func TestKeyStoreKeyManager_DefaultKeyNumber(t *testing.T) {
	t.Parallel()
	store := writeKeyStore(t, "storepass", "storepass")
	base := func() *KeyStoreSerializationKeyManagerBuilder {
		return NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(store).SetKeyStorePassword("storepass")
	}
	for _, c := range []struct {
		name string
		b    *KeyStoreSerializationKeyManagerBuilder
		want int32
	}{
		{"default alias only", base().SetDefaultKeyEntryAlias("Mixed-Case"), 0},
		{"alias list only: the last", base().SetKeyEntryAliases([]string{"k1", "Mixed-Case", "k1"}), 2},
		{"both: the default's index", base().SetKeyEntryAliases([]string{"Mixed-Case", "k1"}).SetDefaultKeyEntryAlias("k1"), 1},
		// indexOf: the first occurrence, as Java's List.indexOf.
		{"both, repeated: the first", base().SetKeyEntryAliases([]string{"k1", "Mixed-Case", "k1"}).SetDefaultKeyEntryAlias("k1"), 0},
	} {
		km, err := c.b.Build()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := km.GetSerializationKey(); got != c.want {
			t.Errorf("%s: serialization key %d, want %d", c.name, got, c.want)
		}
	}
}

// TestKeyStoreKeyManager_GetKeyRefusals: a key number outside the alias list
// is Java's "key number out of range"; an alias with no secret-key entry is
// "cannot load key".
func TestKeyStoreKeyManager_GetKeyRefusals(t *testing.T) {
	t.Parallel()
	km, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(writeKeyStore(t, "storepass", "storepass")).
		SetKeyStorePassword("storepass").SetKeyEntryAliases([]string{"k1", "absent"}).Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		n    int32
		want string
	}{{-1, "key number out of range"}, {2, "key number out of range"}, {1, "cannot load key"}} {
		_, err := km.GetKey(c.n)
		var serErr *RecordSerializationError
		if !errors.As(err, &serErr) || serErr.Message != c.want {
			t.Errorf("GetKey(%d): got %v, want %q", c.n, err, c.want)
		}
	}
}

// TestKeyStoreKeyManager_EncryptsRecords: the key manager drives the JCE
// serializer, and a record written under one key number is read by a manager
// whose default is another key, since the prefix names the key.
func TestKeyStoreKeyManager_EncryptsRecords(t *testing.T) {
	t.Parallel()
	store := writeKeyStore(t, "storepass", "storepass")
	manager := func(def string) *KeyStoreSerializationKeyManager {
		km, err := NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(store).
			SetKeyStorePassword("storepass").SetKeyEntryAliases([]string{"k1", "Mixed-Case"}).SetDefaultKeyEntryAlias(def).Build()
		if err != nil {
			t.Fatal(err)
		}
		return km
	}
	serializer := func(km SerializationKeyManager) *TransformedRecordSerializer {
		s, err := NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).SetKeyManager(km).Build()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	union := unionOf(sonnet108)
	stored, err := serializer(manager("k1")).transform(union, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stored[0] != 1 { // encrypted, key 0
		t.Fatalf("prefix %x, want 01 (encrypted under key 0)", stored[0])
	}
	got, err := serializer(manager("Mixed-Case")).untransform(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, union) {
		t.Fatalf("round trip changed the record")
	}
	stored, err = serializer(manager("Mixed-Case")).transform(union, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stored[0] != 1<<3|1 { // encrypted, key 1
		t.Fatalf("prefix %x, want 09 (encrypted under key 1)", stored[0])
	}
	if got, err := serializer(manager("k1")).untransform(stored); err != nil || !bytes.Equal(got, union) {
		t.Fatalf("key-1 record through a key-0 manager: %v", err)
	}
}
