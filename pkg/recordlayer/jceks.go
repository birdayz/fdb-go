// Portions derived from pyjks (jks/jks.py, jks/sun_crypto.py),
// Copyright (c) 2013 Kurt Rose
// Licensed under the MIT License (see NOTICE); translated to Go and modified.

package recordlayer

import (
	"bytes"
	"crypto/cipher"
	"crypto/des"
	"crypto/md5"  //nolint:gosec // PBEWithMD5AndTripleDES is MD5-based
	"crypto/sha1" //nolint:gosec // the JCEKS integrity digest is SHA-1
	"crypto/subtle"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"unicode/utf16"
)

// A JCEKS key store (SunJCE's JceKeyStore), the type Java's
// KeyStore.getInstance(File, password) detects by its magic: the magic, the
// version (1 or 2), the entry count, the entries, then a SHA-1 digest keyed by
// the store password. Each entry is a tag, its alias (modified UTF-8, folded to
// lower case when stored), a timestamp, then by tag: a private key and its
// certificate chain (1), a trusted certificate (2), or a secret key (3), a
// serialized SealedObject whose content is the key, serialized, sealed under
// the key password with PBEWithMD5AndTripleDES.

var jceksMagic = []byte{0xCE, 0xCE, 0xCE, 0xCE}

const (
	jceksTagPrivateKey    = 1
	jceksTagTrustedCert   = 2
	jceksTagSecretKey     = 3
	jceksPBEAlgorithm     = "PBEWithMD5AndTripleDES"
	jceksIntegrityWhitens = "Mighty Aphrodite"
	// jceksMaxIterations is the most PBE iterations JDK 21 unseals a key under
	// ("PBE iteration count too large" above it, measured); its java.security
	// documents 5000000 as the top of jdk.jceks.iterationCount's valid range.
	jceksMaxIterations = 5000000
)

// jceksSealedClass is the class a secret key entry holds, and
// jceksSealedClasses the classes its serialized form names.
const jceksSealedClass = "com.sun.crypto.provider.SealedObjectForKeyProtector"

var jceksSealedClasses = map[string]bool{jceksSealedClass: true, "javax.crypto.SealedObject": true}

// jceksKeyClasses are the classes JDK 21 deserializes a sealed key as: its
// java.security's default jceks.key.serialFilter admits these and rejects all
// others (byte[] is a primitive array, which the filter admits).
var jceksKeyClasses = map[string]bool{
	"java.lang.Enum":                  true,
	"java.security.KeyRep":            true,
	"java.security.KeyRep$Type":       true,
	"javax.crypto.spec.SecretKeySpec": true,
}

// readJCEKSEntries reads a JCEKS store's entries by alias, and checks its
// digest with the store password as JceKeyStore.engineLoad does. A secret-key
// entry is decrypted only when its key is asked for, as Java's getEntry does.
func readJCEKSEntries(data []byte, storePassword string) (map[string]keyStoreEntry, error) {
	r := &javaStreamReader{data: data}
	head, err := r.take(12)
	if err != nil || !bytes.Equal(head[:4], jceksMagic) {
		return nil, errors.New("Invalid keystore format")
	}
	version := binary.BigEndian.Uint32(head[4:])
	if version != 1 && version != 2 {
		return nil, errors.New("Invalid keystore format")
	}
	// A Java int: a negative count reads no entries (measured).
	count := int(int32(binary.BigEndian.Uint32(head[8:]))) //nolint:gosec // a Java int
	entries := map[string]keyStoreEntry{}
	for range count {
		tag, err := r.i32()
		if err != nil {
			return nil, err
		}
		// A later entry of an alias replaces an earlier one, as the JDK's
		// table of entries does (measured).
		alias, err := r.utf()
		if err != nil {
			return nil, err
		}
		if _, err := r.take(8); err != nil { // creation date
			return nil, err
		}
		switch tag {
		case jceksTagPrivateKey:
			if err := r.skipPrivateKeyEntry(version); err != nil {
				return nil, err
			}
			entries[alias] = notASecretKey("a private key entry")
		case jceksTagTrustedCert:
			if err := r.skipCertificate(version); err != nil {
				return nil, err
			}
			entries[alias] = notASecretKey("a trusted certificate entry")
		case jceksTagSecretKey:
			// JDK 21 loads only a SealedObjectForKeyProtector here: a plain
			// SealedObject fails the load as "Invalid secret key format", and
			// another object a cast (measured); Go refuses both as the first.
			sealed, n, err := readJavaObject(data[r.pos:], func(name string) bool { return jceksSealedClasses[name] })
			if obj, ok := sealed.(*javaObject); err != nil || !ok || obj.class.name != jceksSealedClass {
				return nil, errors.New("Invalid secret key format")
			}
			r.pos += n
			entries[alias] = func(password string) (secretKey, error) { return unsealJCEKSKey(sealed, password) }
		default:
			return nil, errors.New("Unrecognized keystore entry")
		}
	}
	digest, err := r.take(sha1.Size)
	if err != nil {
		return nil, err
	}
	h := sha1.New() //nolint:gosec // the JCEKS integrity digest is SHA-1
	runes := []rune(storePassword)
	units := utf16.Encode(runes)
	for _, c := range units {
		h.Write([]byte{byte(c >> 8), byte(c)})
	}
	clear(runes)
	clear(units)
	h.Write([]byte(jceksIntegrityWhitens))
	h.Write(data[:r.pos-sha1.Size])
	if subtle.ConstantTimeCompare(h.Sum(nil), digest) != 1 {
		return nil, errors.New("Keystore was tampered with, or password was incorrect")
	}
	return entries, nil
}

// skipPrivateKeyEntry and skipCertificate step over entries Go never serves.
// Unlike Java, they do not parse the key or certificates (declared).
func (r *javaStreamReader) skipPrivateKeyEntry(version uint32) error {
	n, err := r.i32()
	if err != nil {
		return err
	}
	if _, err := r.take(n); err != nil {
		return err
	}
	chain, err := r.i32()
	if err != nil {
		return err
	}
	for range chain {
		if err := r.skipCertificate(version); err != nil {
			return err
		}
	}
	return nil
}

// skipCertificate steps over a certificate: its type (from version 2, as the
// JDK reads it, measured), then its encoding.
func (r *javaStreamReader) skipCertificate(version uint32) error {
	if version >= 2 {
		if _, err := r.utf(); err != nil {
			return err
		}
	}
	n, err := r.i32()
	if err != nil {
		return err
	}
	_, err = r.take(n)
	return err
}

// notASecretKey is an entry Java's key manager cannot serve: its cast of the
// entry to a SecretKeyEntry fails.
func notASecretKey(kind string) keyStoreEntry {
	return func(string) (secretKey, error) { return secretKey{}, fmt.Errorf("%s, not a secret key entry", kind) }
}

// unsealJCEKSKey opens a secret-key entry's SealedObject under the key
// password: PBEWithMD5AndTripleDES, its salt and iteration count the object's
// encoded parameters, around a serialized SecretKeySpec, or the KeyRep a key
// class that replaces itself is serialized as. JDK 21's JceKeyStore admits no
// other class when it deserializes the key (its jceks.key.serialFilter).
func unsealJCEKSKey(sealed any, password string) (secretKey, error) {
	obj, ok := sealed.(*javaObject)
	if !ok || obj.class.name != jceksSealedClass {
		return secretKey{}, errors.New("Invalid secret key format")
	}
	sealAlg, _ := obj.fields["sealAlg"].(string)
	paramsAlg, _ := obj.fields["paramsAlg"].(string)
	content, _ := obj.fields["encryptedContent"].([]byte)
	params, _ := obj.fields["encodedParams"].([]byte)
	if sealAlg != jceksPBEAlgorithm || paramsAlg != jceksPBEAlgorithm || len(params) == 0 {
		return secretKey{}, fmt.Errorf("a secret key sealed with %q", sealAlg)
	}
	if err := checkPBEPassword(password); err != nil {
		return secretKey{}, err
	}
	var pbe struct {
		Salt       []byte
		Iterations int
	}
	if rest, err := asn1.Unmarshal(params, &pbe); err != nil || len(rest) != 0 || len(pbe.Salt) != 8 {
		return secretKey{}, errors.New("bad PBEWithMD5AndTripleDES parameters")
	}
	switch {
	case pbe.Iterations < 1:
		return secretKey{}, errors.New("IterationCount must be a positive number")
	case pbe.Iterations > jceksMaxIterations:
		return secretKey{}, errors.New("PBE iteration count too large")
	}
	plain, err := pbeWithMD5AndTripleDESDecrypt(content, password, pbe.Salt, pbe.Iterations)
	if err != nil {
		return secretKey{}, err
	}
	defer clear(plain[:cap(plain)])
	key, _, err := readJavaObject(plain, func(name string) bool { return jceksKeyClasses[name] })
	if err != nil {
		return secretKey{}, err
	}
	k, ok := key.(*javaObject)
	if !ok {
		return secretKey{}, errors.New("the sealed key is not an object")
	}
	switch k.class.name {
	case "javax.crypto.spec.SecretKeySpec":
		alg, _ := k.fields["algorithm"].(string)
		b, ok := k.fields["key"].([]byte)
		if !ok || alg == "" {
			return secretKey{}, errors.New("a malformed SecretKeySpec")
		}
		return secretKey{bytes: b, algorithm: alg}, nil // the parser's own copy
	case "java.security.KeyRep":
		alg, _ := k.fields["algorithm"].(string)
		format, _ := k.fields["format"].(string)
		typ, _ := k.fields["type"].(javaEnum)
		b, ok := k.fields["encoded"].([]byte)
		if !ok || alg == "" || typ.class == nil || typ.class.name != "java.security.KeyRep$Type" || typ.constant != "SECRET" || format != "RAW" {
			return secretKey{}, fmt.Errorf("a KeyRep of type %q, format %q, not a raw secret key", typ.constant, format)
		}
		return secretKey{bytes: b, algorithm: alg}, nil // the parser's own copy
	}
	return secretKey{}, fmt.Errorf("a sealed %s, not a secret key", k.class.name)
}

// pbeWithMD5AndTripleDESDecrypt is SunJCE's PBEWithMD5AndTripleDES: PKCS #5
// PBES1 with DES-EDE3. The salt is split in halves, the first reversed when
// they are equal (as JDK 21 does, measured; pyjks documents a different
// reordering by older JDKs); each half is digested with the password,
// iterations times, through MD5; the two digests are the 24-byte key and,
// last, the 8-byte IV; the content is DES-EDE3-CBC, PKCS #5 padded.
func pbeWithMD5AndTripleDESDecrypt(data []byte, password string, salt []byte, iterations int) ([]byte, error) {
	if len(data) == 0 || len(data)%des.BlockSize != 0 {
		return nil, errors.New("Input length must be multiple of 8 when decrypting with padded cipher")
	}
	derived := pbeWithMD5AndTripleDESKey(password, salt, iterations)
	defer clear(derived)
	block, err := des.NewTripleDESCipher(derived[:24])
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, derived[24:]).CryptBlocks(plain, data)
	n := int(plain[len(plain)-1])
	if n == 0 || n > des.BlockSize || !bytes.Equal(plain[len(plain)-n:], bytes.Repeat([]byte{byte(n)}, n)) {
		clear(plain)
		return nil, errors.New("Given final block not properly padded. Such issues can arise if a bad key is used during decryption.")
	}
	return plain[:len(plain)-n], nil
}

// pbeWithMD5AndTripleDESKey derives the 24-byte key, then the 8-byte IV.
func pbeWithMD5AndTripleDESKey(password string, salt []byte, iterations int) []byte {
	pw := []byte(password)
	defer clear(pw)
	halves := [2][]byte{bytes.Clone(salt[:4]), bytes.Clone(salt[4:8])}
	if bytes.Equal(halves[0], halves[1]) {
		slices.Reverse(halves[0])
	}
	derived := make([]byte, 0, 2*md5.Size)
	buf := make([]byte, 0, md5.Size+len(pw))
	defer func() { clear(buf[:cap(buf)]) }()
	for _, d := range halves {
		for range iterations {
			buf = append(append(buf[:0], d...), pw...)
			sum := md5.Sum(buf) //nolint:gosec // the algorithm's digest
			d = sum[:]
		}
		derived = append(derived, d...)
	}
	return derived
}
