// Portions derived from FoundationDB Record Layer (
// KeyStoreSerializationKeyManager.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha1" //nolint:gosec // PKCS #12's legacy KDF and MAC are SHA-1 in key stores older JDKs wrote
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/asn1"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf16"
)

// KeyStoreSerializationKeyManager is Java's KeyStoreSerializationKeyManager: the
// keys are secret-key entries of a key store file, key number i the entry named
// by the i-th alias, and the serialization key the default alias's number.
// Go reads a PKCS12 key store (the JDK's default key store type, and what
// keytool writes); Java's KeyStore.getInstance(File, password) also detects a
// JCEKS file, which Go refuses (declared in DIVERGENCES.md).
type KeyStoreSerializationKeyManager struct {
	// entries are the shrouded secret keys by alias, folded as Java's
	// PKCS12KeyStore folds them; each is decrypted in GetKey, as Java's
	// getKey decrypts, so a wrong key-entry password fails there.
	entries       map[string][]byte
	entryPassword string
	// decrypted caches each key number's decrypted key, so a record's IV write
	// does not re-run PBKDF2 per call. Java decrypts in every getKey; only a
	// success is cached here, so a key that does not decrypt fails every time,
	// as it does in Java.
	decryptedMu      sync.Mutex
	decrypted        map[int32]secretKey
	keyEntryAliases  []string
	defaultKeyNumber int32
	cipherName       string
	random           io.Reader
}

// KeyStoreSerializationKeyManagerBuilder is Java's KeyStoreSerializationKeyManager.Builder.
type KeyStoreSerializationKeyManagerBuilder struct {
	keyStoreFileName     string
	keyStorePassword     *string
	keyEntryPassword     *string
	defaultKeyEntryAlias *string
	keyEntryAliases      []string
	keyEntryAliasesSet   bool
	cipherName           string
	random               io.Reader
}

// NewKeyStoreSerializationKeyManagerBuilder is KeyStoreSerializationKeyManager.newBuilder.
func NewKeyStoreSerializationKeyManagerBuilder() *KeyStoreSerializationKeyManagerBuilder {
	return &KeyStoreSerializationKeyManagerBuilder{cipherName: DefaultCipher}
}

func (b *KeyStoreSerializationKeyManagerBuilder) SetKeyStoreFileName(name string) *KeyStoreSerializationKeyManagerBuilder {
	b.keyStoreFileName = name
	return b
}

func (b *KeyStoreSerializationKeyManagerBuilder) SetKeyStorePassword(p string) *KeyStoreSerializationKeyManagerBuilder {
	b.keyStorePassword = &p
	return b
}

func (b *KeyStoreSerializationKeyManagerBuilder) SetKeyEntryPassword(p string) *KeyStoreSerializationKeyManagerBuilder {
	b.keyEntryPassword = &p
	return b
}

func (b *KeyStoreSerializationKeyManagerBuilder) SetDefaultKeyEntryAlias(a string) *KeyStoreSerializationKeyManagerBuilder {
	b.defaultKeyEntryAlias = &a
	return b
}

func (b *KeyStoreSerializationKeyManagerBuilder) SetKeyEntryAliases(aliases []string) *KeyStoreSerializationKeyManagerBuilder {
	b.keyEntryAliases = append([]string(nil), aliases...)
	b.keyEntryAliasesSet = true
	return b
}

func (b *KeyStoreSerializationKeyManagerBuilder) SetCipherName(name string) *KeyStoreSerializationKeyManagerBuilder {
	b.cipherName = name
	return b
}

func (b *KeyStoreSerializationKeyManagerBuilder) SetRandom(r io.Reader) *KeyStoreSerializationKeyManagerBuilder {
	b.random = r
	return b
}

// Build is Java's build(): load the key store (its password "" when unset),
// then resolve the aliases and the default key number as Java does.
func (b *KeyStoreSerializationKeyManagerBuilder) Build() (*KeyStoreSerializationKeyManager, error) {
	if b.keyStoreFileName == "" {
		return nil, &RecordCoreArgumentError{Message: "must specify key store file name"}
	}
	storePassword := ""
	if b.keyStorePassword != nil {
		storePassword = *b.keyStorePassword
	}
	data, err := os.ReadFile(b.keyStoreFileName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &RecordCoreArgumentError{Message: "Key store not found", Cause: err}
		}
		return nil, &RecordCoreArgumentError{Message: "Key store loading failed", Cause: err}
	}
	entries, err := readPKCS12SecretKeyEntries(data, storePassword)
	if err != nil {
		return nil, &RecordCoreArgumentError{Message: "Key store loading failed", Cause: err}
	}
	var aliases []string
	var defaultKeyNumber int32
	switch {
	case !b.keyEntryAliasesSet && b.defaultKeyEntryAlias == nil:
		return nil, &RecordCoreArgumentError{Message: "must specify key alias list or single default alias"}
	case !b.keyEntryAliasesSet:
		aliases = []string{*b.defaultKeyEntryAlias}
	case b.defaultKeyEntryAlias == nil:
		if len(b.keyEntryAliases) == 0 {
			return nil, &RecordCoreArgumentError{Message: "need at least one key alias"}
		}
		aliases = b.keyEntryAliases
		defaultKeyNumber = int32(len(aliases) - 1) //nolint:gosec
	default:
		aliases = b.keyEntryAliases
		defaultKeyNumber = -1
		for i, a := range aliases {
			if a == *b.defaultKeyEntryAlias {
				defaultKeyNumber = int32(i) //nolint:gosec
				break
			}
		}
		if defaultKeyNumber < 0 {
			return nil, &RecordCoreArgumentError{Message: "default key alias not in key alias list"}
		}
	}
	entryPassword := storePassword
	if b.keyEntryPassword != nil {
		entryPassword = *b.keyEntryPassword
	}
	// No random source is the writing store's randomness (SerializationKeyManager).
	return &KeyStoreSerializationKeyManager{
		entries: entries, entryPassword: entryPassword, keyEntryAliases: aliases, defaultKeyNumber: defaultKeyNumber,
		cipherName: b.cipherName, random: b.random,
	}, nil
}

func (m *KeyStoreSerializationKeyManager) GetSerializationKey() int32 { return m.defaultKeyNumber }

// secretKey is a key store's secret key: its bytes and its algorithm, as a
// Java SecretKeySpec carries both.
type secretKey struct {
	bytes     []byte
	algorithm string
}

// GetKey is Java's getKey. A key that does not decrypt is Java's
// UnrecoverableKeyException, "cannot load key". An alias with no secret-key
// entry is, in Java, a null entry dereferenced (a NullPointerException) or a
// ClassCastException; Go reports the same "cannot load key" as a structured
// error rather than panicking.
func (m *KeyStoreSerializationKeyManager) GetKey(keyNumber int32) ([]byte, error) {
	k, err := m.secretKey(keyNumber)
	if err != nil {
		return nil, err
	}
	// A copy: the decrypted key is cached, and a caller that clears the slice
	// it was handed must not clear the cache.
	return append([]byte(nil), k.bytes...), nil
}

// KeyAlgorithm is the key's algorithm, the name Java's key store gives the
// algorithm identifier it stored with the key ("AES" for an AES key), which the
// cipher checks (KeyAlgorithmReporter).
func (m *KeyStoreSerializationKeyManager) KeyAlgorithm(keyNumber int32) (string, error) {
	k, err := m.secretKey(keyNumber)
	if err != nil {
		return "", err
	}
	return k.algorithm, nil
}

func (m *KeyStoreSerializationKeyManager) secretKey(keyNumber int32) (secretKey, error) {
	if keyNumber < 0 || int(keyNumber) >= len(m.keyEntryAliases) {
		return secretKey{}, serializationError("key number out of range")
	}
	m.decryptedMu.Lock()
	defer m.decryptedMu.Unlock()
	if k, ok := m.decrypted[keyNumber]; ok {
		return k, nil
	}
	alias := m.keyEntryAliases[keyNumber]
	shrouded, ok := m.entries[foldAlias(alias)]
	if !ok {
		return secretKey{}, &RecordSerializationError{Message: "cannot load key", Cause: fmt.Errorf("no secret key entry %q", alias)}
	}
	if err := checkPBEPassword(m.entryPassword); err != nil {
		return secretKey{}, &RecordSerializationError{Message: "cannot load key", Cause: err}
	}
	k, err := retryWithZero(m.entryPassword, func(pw string) (secretKey, error) {
		return decryptShroudedSecretKey(shrouded, pw)
	})
	if err != nil {
		return secretKey{}, &RecordSerializationError{Message: "cannot load key", Cause: err}
	}
	if m.decrypted == nil {
		m.decrypted = map[int32]secretKey{}
	}
	m.decrypted[keyNumber] = k
	return k, nil
}

func (m *KeyStoreSerializationKeyManager) GetCipher(int32) (string, error) { return m.cipherName, nil }

func (m *KeyStoreSerializationKeyManager) GetRandom(int32) (io.Reader, error) { return m.random, nil }

// checkPBEPassword is the JDK's com.sun.crypto.provider.PBEKey constructor
// check, which every key-entry decryption and the store's MAC pass through
// (PKCS12KeyStore derives both keys through a PBEKey): a password character
// outside printable ASCII, 0x20-0x7E, is an InvalidKeySpecException "Password
// is not ASCII". Measured against the JVM for a key password and for a store
// password (the JDK refuses to store either). The one-character password
// "\x00" is accepted: PBEKey reads it as the empty password with no NUL
// terminator (bmpPassword), which retryWithZero tries.
func checkPBEPassword(password string) error {
	if password == "\x00" {
		return nil
	}
	for _, c := range password {
		if c < 0x20 || c > 0x7e {
			return errors.New("Password is not ASCII")
		}
	}
	return nil
}

// retryWithZero is PKCS12KeyStore.RetryWithZero, around each key-entry
// decryption and the MAC check: an attempt under the empty password that fails
// is retried under "\x00", the empty password with no NUL terminator, which
// other PKCS #12 writers use for it. The second attempt's failure is the one
// reported, as in the JDK.
func retryWithZero[T any](password string, attempt func(string) (T, error)) (T, error) {
	v, err := attempt(password)
	if err != nil && password == "" {
		return attempt("\x00")
	}
	return v, err
}

// foldAlias folds an alias as Java's PKCS12KeyStore does: entries are stored
// and looked up by alias.toLowerCase(Locale.ENGLISH). strings.ToLower is
// Unicode simple case mapping, which Java's matches outside its few
// context-sensitive special cases (final sigma, dotted capital I).
func foldAlias(s string) string { return strings.ToLower(s) }

// PKCS #12 (RFC 7292) and the algorithms JDK key stores use.
var (
	oidData                    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidEncryptedData           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 6}
	oidSecretBag               = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 5}
	oidPKCS8ShroudedKeyBag     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 2}
	oidFriendlyName            = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 20}
	oidPBES2                   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2                  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHmacSHA1                = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHmacSHA224              = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 8}
	oidHmacSHA256              = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHmacSHA384              = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 10}
	oidHmacSHA512              = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}
	oidAES128CBC               = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC               = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC               = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	oidPBEWithSHAAnd3KeyDESede = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 3}
	oidSHA1                    = asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
	oidSHA224                  = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 4}
	oidSHA256                  = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidSHA384                  = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2}
	oidSHA512                  = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3}
)

type pfxPDU struct {
	Version  int
	AuthSafe contentInfo
	MacData  macData `asn1:"optional"`
}

type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"tag:0,explicit,optional"`
}

type macData struct {
	Mac        digestInfo
	MacSalt    []byte
	Iterations int `asn1:"optional,default:1"`
}

type digestInfo struct {
	Algorithm algorithmIdentifier
	Digest    []byte
}

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type encryptedData struct {
	Version              int
	EncryptedContentInfo encryptedContentInfo
}

type encryptedContentInfo struct {
	ContentType                asn1.ObjectIdentifier
	ContentEncryptionAlgorithm algorithmIdentifier
	EncryptedContent           []byte `asn1:"tag:0,optional"`
}

type safeBag struct {
	ID         asn1.ObjectIdentifier
	Value      asn1.RawValue     `asn1:"tag:0,explicit"`
	Attributes []pkcs12Attribute `asn1:"set,optional"`
}

type pkcs12Attribute struct {
	ID    asn1.ObjectIdentifier
	Value asn1.RawValue `asn1:"set"`
}

type secretBag struct {
	SecretTypeID asn1.ObjectIdentifier
	SecretValue  []byte `asn1:"tag:0,explicit"`
}

type encryptedPrivateKeyInfo struct {
	Algorithm     algorithmIdentifier
	EncryptedData []byte
}

type privateKeyInfo struct {
	Version    int
	Algorithm  algorithmIdentifier
	PrivateKey []byte
}

type pbes2Params struct {
	KeyDerivationFunc algorithmIdentifier
	EncryptionScheme  algorithmIdentifier
}

type pbkdf2Params struct {
	Salt           []byte
	IterationCount int
	KeyLength      int                 `asn1:"optional"`
	PRF            algorithmIdentifier `asn1:"optional"`
}

type pbeParams struct {
	Salt       []byte
	Iterations int
}

// readPKCS12SecretKeyEntries reads every secret-key entry of a PKCS12 key
// store, still shrouded, by its folded friendly name: Java's
// PKCS12KeyStore.engineLoad. The MAC is checked with the store password, as
// the JDK checks it ("keystore password was incorrect").
func readPKCS12SecretKeyEntries(der []byte, storePassword string) (map[string][]byte, error) {
	if len(der) >= 4 && bytes.Equal(der[:4], []byte{0xCE, 0xCE, 0xCE, 0xCE}) {
		return nil, errors.New("a JCEKS key store: Go reads PKCS12 key stores only (keytool -importkeystore -deststoretype PKCS12 converts one)")
	}
	var pfx pfxPDU
	if rest, err := asn1.Unmarshal(der, &pfx); err != nil {
		return nil, fmt.Errorf("not a PKCS12 key store: %w", err)
	} else if len(rest) != 0 {
		return nil, errors.New("not a PKCS12 key store: trailing data")
	}
	if pfx.Version != 3 {
		return nil, fmt.Errorf("PKCS12 version %d, want 3", pfx.Version)
	}
	if !pfx.AuthSafe.ContentType.Equal(oidData) {
		return nil, errors.New("PKCS12 authSafe is not data (a public-key integrity mode Go does not read)")
	}
	var authSafe []byte
	if _, err := asn1.Unmarshal(pfx.AuthSafe.Content.Bytes, &authSafe); err != nil {
		return nil, fmt.Errorf("PKCS12 authSafe: %w", err)
	}
	if len(pfx.MacData.Mac.Digest) > 0 {
		if _, err := retryWithZero(storePassword, func(pw string) (struct{}, error) {
			return struct{}{}, verifyPKCS12MAC(pfx.MacData, authSafe, pw)
		}); err != nil {
			return nil, err
		}
	}
	var contents []contentInfo
	if _, err := asn1.Unmarshal(authSafe, &contents); err != nil {
		return nil, fmt.Errorf("PKCS12 authenticated safe: %w", err)
	}
	entries := map[string][]byte{}
	for _, ci := range contents {
		var safeContents []byte
		switch {
		case ci.ContentType.Equal(oidData):
			if _, err := asn1.Unmarshal(ci.Content.Bytes, &safeContents); err != nil {
				return nil, fmt.Errorf("PKCS12 safe contents: %w", err)
			}
		case ci.ContentType.Equal(oidEncryptedData):
			// Certificates; a JDK key store keeps secret keys in data safes.
			continue
		default:
			continue
		}
		var bags []safeBag
		if _, err := asn1.Unmarshal(safeContents, &bags); err != nil {
			return nil, fmt.Errorf("PKCS12 safe bags: %w", err)
		}
		for _, bag := range bags {
			if !bag.ID.Equal(oidSecretBag) {
				continue
			}
			alias, err := bagFriendlyName(bag.Attributes)
			if err != nil {
				return nil, err
			}
			var sb secretBag
			if _, err := asn1.Unmarshal(bag.Value.Bytes, &sb); err != nil {
				return nil, fmt.Errorf("PKCS12 secret bag %q: %w", alias, err)
			}
			if !sb.SecretTypeID.Equal(oidPKCS8ShroudedKeyBag) {
				return nil, fmt.Errorf("PKCS12 secret bag %q: secret type %v", alias, sb.SecretTypeID)
			}
			entries[foldAlias(alias)] = sb.SecretValue
		}
	}
	return entries, nil
}

func bagFriendlyName(attrs []pkcs12Attribute) (string, error) {
	for _, a := range attrs {
		if !a.ID.Equal(oidFriendlyName) {
			continue
		}
		var bmp asn1.RawValue
		if _, err := asn1.Unmarshal(a.Value.Bytes, &bmp); err != nil || bmp.Tag != 30 || len(bmp.Bytes)%2 != 0 {
			return "", errors.New("PKCS12 friendlyName is not a BMPString")
		}
		u := make([]uint16, len(bmp.Bytes)/2)
		for i := range u {
			u[i] = uint16(bmp.Bytes[2*i])<<8 | uint16(bmp.Bytes[2*i+1])
		}
		return string(utf16.Decode(u)), nil
	}
	return "", errors.New("PKCS12 secret bag without a friendlyName")
}

// decryptShroudedSecretKey decrypts an EncryptedPrivateKeyInfo holding a
// secret key as a PKCS #8 PrivateKeyInfo (the key's algorithm, then its raw
// bytes), as the JDK's PKCS12KeyStore wraps a SecretKeyEntry.
func decryptShroudedSecretKey(der []byte, password string) (secretKey, error) {
	var epki encryptedPrivateKeyInfo
	if _, err := asn1.Unmarshal(der, &epki); err != nil {
		return secretKey{}, err
	}
	plain, err := pbeDecrypt(epki.Algorithm, epki.EncryptedData, password)
	if err != nil {
		return secretKey{}, err
	}
	var pki privateKeyInfo
	if _, err := asn1.Unmarshal(plain, &pki); err != nil {
		return secretKey{}, fmt.Errorf("the decrypted key is not a PrivateKeyInfo (wrong key password?): %w", err)
	}
	return secretKey{bytes: pki.PrivateKey, algorithm: keyAlgorithmName(pki.Algorithm.Algorithm)}, nil
}

// oidAESKey is the algorithm identifier the JDK stores with an AES secret key
// (AlgorithmId.get("AES")).
var oidAESKey = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1}

// keyAlgorithmName is the name the JDK reads a stored key's algorithm
// identifier back as: "AES" for AES's, otherwise the identifier itself, which
// is not AES, and the cipher refuses it as Java's does.
func keyAlgorithmName(oid asn1.ObjectIdentifier) string {
	if oid.Equal(oidAESKey) {
		return "AES"
	}
	return oid.String()
}

// pbeDecrypt decrypts under PBES2 (PBKDF2 with an HMAC-SHA PRF, AES-CBC: the
// JDK's default key protection since 12) or the legacy
// pbeWithSHAAnd3-KeyTripleDES-CBC (older JDKs).
func pbeDecrypt(alg algorithmIdentifier, data []byte, password string) ([]byte, error) {
	switch {
	case alg.Algorithm.Equal(oidPBES2):
		var params pbes2Params
		if _, err := asn1.Unmarshal(alg.Parameters.FullBytes, &params); err != nil {
			return nil, err
		}
		if !params.KeyDerivationFunc.Algorithm.Equal(oidPBKDF2) {
			return nil, fmt.Errorf("PBES2 key derivation %v", params.KeyDerivationFunc.Algorithm)
		}
		var kdf pbkdf2Params
		if _, err := asn1.Unmarshal(params.KeyDerivationFunc.Parameters.FullBytes, &kdf); err != nil {
			return nil, err
		}
		prf := sha1.New
		if len(kdf.PRF.Algorithm) > 0 {
			var err error
			if prf, err = hmacPRF(kdf.PRF.Algorithm); err != nil {
				return nil, err
			}
		}
		var keyLen int
		switch {
		case params.EncryptionScheme.Algorithm.Equal(oidAES128CBC):
			keyLen = 16
		case params.EncryptionScheme.Algorithm.Equal(oidAES192CBC):
			keyLen = 24
		case params.EncryptionScheme.Algorithm.Equal(oidAES256CBC):
			keyLen = 32
		default:
			return nil, fmt.Errorf("PBES2 encryption scheme %v", params.EncryptionScheme.Algorithm)
		}
		var iv []byte
		if _, err := asn1.Unmarshal(params.EncryptionScheme.Parameters.FullBytes, &iv); err != nil {
			return nil, err
		}
		// The JDK derives a PBKDF2 key from the password's UTF-8 bytes.
		key, err := pbkdf2.Key(prf, password, kdf.Salt, kdf.IterationCount, keyLen)
		if err != nil {
			return nil, err
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cbcDecryptPKCS5(block, iv, data)
	case alg.Algorithm.Equal(oidPBEWithSHAAnd3KeyDESede):
		var params pbeParams
		if _, err := asn1.Unmarshal(alg.Parameters.FullBytes, &params); err != nil {
			return nil, err
		}
		pw := bmpPassword(password)
		key := pkcs12KDF(sha1.New, 1, pw, params.Salt, params.Iterations, 24)
		iv := pkcs12KDF(sha1.New, 2, pw, params.Salt, params.Iterations, 8)
		block, err := des.NewTripleDESCipher(key)
		if err != nil {
			return nil, err
		}
		return cbcDecryptPKCS5(block, iv, data)
	}
	return nil, fmt.Errorf("key protection algorithm %v", alg.Algorithm)
}

func hmacPRF(oid asn1.ObjectIdentifier) (func() hash.Hash, error) {
	switch {
	case oid.Equal(oidHmacSHA1):
		return sha1.New, nil
	case oid.Equal(oidHmacSHA224):
		return sha256.New224, nil
	case oid.Equal(oidHmacSHA256):
		return sha256.New, nil
	case oid.Equal(oidHmacSHA384):
		return sha512.New384, nil
	case oid.Equal(oidHmacSHA512):
		return sha512.New, nil
	}
	return nil, fmt.Errorf("PBKDF2 PRF %v", oid)
}

func digestHash(oid asn1.ObjectIdentifier) (func() hash.Hash, error) {
	switch {
	case oid.Equal(oidSHA1):
		return sha1.New, nil
	case oid.Equal(oidSHA224):
		return sha256.New224, nil
	case oid.Equal(oidSHA256):
		return sha256.New, nil
	case oid.Equal(oidSHA384):
		return sha512.New384, nil
	case oid.Equal(oidSHA512):
		return sha512.New, nil
	}
	return nil, fmt.Errorf("PKCS12 MAC digest %v", oid)
}

// verifyPKCS12MAC is RFC 7292's integrity check: an HMAC over the
// authenticated safe under a key the PKCS #12 KDF derives (ID 3) from the
// password.
func verifyPKCS12MAC(md macData, authSafe []byte, password string) error {
	// The JDK derives the MAC key through PBEKey as well, so a store password
	// outside printable ASCII is refused (measured: the JDK will not store it).
	if err := checkPBEPassword(password); err != nil {
		return err
	}
	h, err := digestHash(md.Mac.Algorithm.Algorithm)
	if err != nil {
		return err
	}
	key := pkcs12KDF(h, 3, bmpPassword(password), md.MacSalt, md.Iterations, h().Size())
	mac := hmac.New(h, key)
	mac.Write(authSafe)
	if subtle.ConstantTimeCompare(mac.Sum(nil), md.Mac.Digest) != 1 {
		return errors.New("keystore password was incorrect")
	}
	return nil
}

// bmpPassword is a password as the JDK's PKCS12KeyStore.derive feeds PKCS
// #12's KDF: UTF-16BE with a two-byte terminator, so the empty password is
// the terminator alone; only the one-character password "\x00" is empty.
func bmpPassword(password string) []byte {
	if password == "\x00" {
		return nil
	}
	u := utf16.Encode([]rune(password))
	out := make([]byte, 0, 2*len(u)+2)
	for _, c := range u {
		out = append(out, byte(c>>8), byte(c))
	}
	return append(out, 0, 0)
}

// pkcs12KDF is RFC 7292 appendix B.2.
func pkcs12KDF(h func() hash.Hash, id byte, password, salt []byte, iterations, size int) []byte {
	u := h().Size()
	v := h().BlockSize()
	d := bytes.Repeat([]byte{id}, v)
	fill := func(in []byte) []byte {
		if len(in) == 0 {
			return nil
		}
		n := v * ((len(in) + v - 1) / v)
		out := make([]byte, n)
		for i := range out {
			out[i] = in[i%len(in)]
		}
		return out
	}
	i := append(fill(salt), fill(password)...)
	var out []byte
	for len(out) < size {
		hh := h()
		hh.Write(d)
		hh.Write(i)
		a := hh.Sum(nil)
		for r := 1; r < iterations; r++ {
			hh = h()
			hh.Write(a)
			a = hh.Sum(nil)
		}
		out = append(out, a...)
		if len(out) >= size {
			break
		}
		b := fill(a[:u])[:v]
		for j := 0; j < len(i); j += v {
			carry := 1
			for k := v - 1; k >= 0; k-- {
				sum := int(i[j+k]) + int(b[k]) + carry
				i[j+k] = byte(sum)
				carry = sum >> 8
			}
		}
	}
	return out[:size]
}

func cbcDecryptPKCS5(block cipher.Block, iv, data []byte) ([]byte, error) {
	bs := block.BlockSize()
	if len(iv) != bs || len(data) == 0 || len(data)%bs != 0 {
		return nil, errors.New("encrypted key has a bad length")
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > bs {
		return nil, errors.New("bad padding (wrong key password?)")
	}
	for _, p := range out[len(out)-pad:] {
		if int(p) != pad {
			return nil, errors.New("bad padding (wrong key password?)")
		}
	}
	return out[:len(out)-pad], nil
}
