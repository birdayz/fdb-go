// Portions derived from FoundationDB Record Layer (
// KeyStoreSerializationKeyManager.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"fdb.dev/internal/recordlayer/rlcrypto"
)

// KeyStoreSerializationKeyManager is Java's KeyStoreSerializationKeyManager: the
// keys are secret-key entries of a key store file, key number i the entry named
// by the i-th alias, and the serialization key the default alias's number.
// Go reads the key store types Java's KeyStore.getInstance(File, password)
// detects that can hold a secret key: PKCS12 (the JDK's default, and what
// keytool writes) and JCEKS (rlcrypto/jceks.go).
type KeyStoreSerializationKeyManager struct {
	// entries are the key store's entries by alias, folded as Java's key
	// stores fold them; each is decrypted in GetKey, as Java's getKey
	// decrypts, so a wrong key-entry password fails there.
	entries       map[string]rlcrypto.KeyStoreEntry
	entryPassword string
	// decrypted caches each key number's decrypted key, so a record's IV write
	// does not re-run PBKDF2 per call. Java decrypts in every getKey; only a
	// success is cached here, so a key that does not decrypt fails every time,
	// as it does in Java.
	decryptedMu      sync.Mutex
	decrypted        map[int32]rlcrypto.SecretKey
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
	var entries map[string]rlcrypto.KeyStoreEntry
	if rlcrypto.HasJCEKSMagic(data) {
		entries, err = rlcrypto.ReadJCEKSEntries(data, storePassword)
	} else {
		entries, err = rlcrypto.ReadPKCS12Entries(data, storePassword)
	}
	if err != nil {
		return nil, &RecordCoreArgumentError{Message: "Key store loading failed", Cause: err}
	}
	aliases, defaultKeyNumber, err := b.resolveAliases()
	if err != nil {
		return nil, err
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

func (b *KeyStoreSerializationKeyManagerBuilder) resolveAliases() ([]string, int32, error) {
	switch {
	case !b.keyEntryAliasesSet && b.defaultKeyEntryAlias == nil:
		return nil, 0, &RecordCoreArgumentError{Message: "must specify key alias list or single default alias"}
	case !b.keyEntryAliasesSet:
		return []string{*b.defaultKeyEntryAlias}, 0, nil
	case b.defaultKeyEntryAlias == nil:
		if len(b.keyEntryAliases) == 0 {
			return nil, 0, &RecordCoreArgumentError{Message: "need at least one key alias"}
		}
		return b.keyEntryAliases, int32(len(b.keyEntryAliases) - 1), nil //nolint:gosec
	default:
		for i, a := range b.keyEntryAliases {
			if a == *b.defaultKeyEntryAlias {
				return b.keyEntryAliases, int32(i), nil //nolint:gosec
			}
		}
		return nil, 0, &RecordCoreArgumentError{Message: "default key alias not in key alias list"}
	}
}

func (m *KeyStoreSerializationKeyManager) GetSerializationKey() int32 { return m.defaultKeyNumber }

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
	return append([]byte(nil), k.Bytes...), nil
}

// KeyAlgorithm is the key's algorithm, the name Java's key store gives the
// algorithm identifier it stored with the key ("AES" for an AES key), which the
// cipher checks (KeyAlgorithmReporter).
func (m *KeyStoreSerializationKeyManager) KeyAlgorithm(keyNumber int32) (string, error) {
	k, err := m.secretKey(keyNumber)
	if err != nil {
		return "", err
	}
	return k.Algorithm, nil
}

func (m *KeyStoreSerializationKeyManager) secretKey(keyNumber int32) (rlcrypto.SecretKey, error) {
	if keyNumber < 0 || int(keyNumber) >= len(m.keyEntryAliases) {
		return rlcrypto.SecretKey{}, serializationError("key number out of range")
	}
	m.decryptedMu.Lock()
	defer m.decryptedMu.Unlock()
	if k, ok := m.decrypted[keyNumber]; ok {
		return k, nil
	}
	alias := m.keyEntryAliases[keyNumber]
	entry, ok := m.entries[rlcrypto.FoldAlias(alias)]
	if !ok {
		return rlcrypto.SecretKey{}, &RecordSerializationError{Message: "cannot load key", Cause: fmt.Errorf("no secret key entry %q", alias)}
	}
	k, err := entry(m.entryPassword)
	if err != nil {
		return rlcrypto.SecretKey{}, &RecordSerializationError{Message: "cannot load key", Cause: err}
	}
	if m.decrypted == nil {
		m.decrypted = map[int32]rlcrypto.SecretKey{}
	}
	m.decrypted[keyNumber] = k
	return k, nil
}

func (m *KeyStoreSerializationKeyManager) GetCipher(int32) (string, error) { return m.cipherName, nil }

func (m *KeyStoreSerializationKeyManager) GetRandom(int32) (io.Reader, error) { return m.random, nil }
