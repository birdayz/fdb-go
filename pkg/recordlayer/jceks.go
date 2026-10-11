// Portions derived from pyjks (jks/jks.py, jks/sun_crypto.py),
// Copyright (c) 2013 Kurt Rose; Jeroen De Ridder
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
	"unicode/utf16"
)

// A JCEKS key store (SunJCE's JceKeyStore), the type Java's
// KeyStore.getInstance(File, password) detects by its magic: the magic, the
// version (2), the entry count, the entries, then a SHA-1 digest keyed by the
// store password. Each entry is a tag, its alias (modified UTF-8, folded to
// lower case when stored), a timestamp, then by tag: a private key and its
// certificate chain (1), a trusted certificate (2), or a secret key (3), a
// serialized SealedObject whose content is the key, serialized, sealed under
// the key password with PBEWithMD5AndTripleDES.

var jceksMagic = []byte{0xCE, 0xCE, 0xCE, 0xCE}

const (
	jceksVersion          = 2
	jceksTagPrivateKey    = 1
	jceksTagTrustedCert   = 2
	jceksTagSecretKey     = 3
	jceksPBEAlgorithm     = "PBEWithMD5AndTripleDES"
	jceksIntegrityWhitens = "Mighty Aphrodite"
)

// readJCEKSEntries reads a JCEKS store's entries by alias, and checks its
// digest with the store password as JceKeyStore.engineLoad does. A secret-key
// entry is decrypted only when its key is asked for, as Java's getEntry does.
func readJCEKSEntries(data []byte, storePassword string) (map[string]keyStoreEntry, error) {
	r := &javaStreamReader{data: data}
	head, err := r.take(12)
	if err != nil || !bytes.Equal(head[:4], jceksMagic) {
		return nil, errors.New("Invalid keystore format")
	}
	if v := binary.BigEndian.Uint32(head[4:]); v != jceksVersion {
		return nil, errors.New("Invalid keystore format")
	}
	count := int(binary.BigEndian.Uint32(head[8:]))
	entries := map[string]keyStoreEntry{}
	for range count {
		tag, err := r.i32()
		if err != nil {
			return nil, err
		}
		alias, err := r.utf(false)
		if err != nil {
			return nil, err
		}
		if _, err := r.take(8); err != nil { // creation date
			return nil, err
		}
		if _, dup := entries[alias]; dup {
			return nil, fmt.Errorf("duplicate alias %q", alias)
		}
		switch tag {
		case jceksTagPrivateKey:
			if err := r.skipPrivateKeyEntry(); err != nil {
				return nil, err
			}
			entries[alias] = notASecretKey("a private key entry")
		case jceksTagTrustedCert:
			if err := r.skipCertificate(); err != nil {
				return nil, err
			}
			entries[alias] = notASecretKey("a trusted certificate entry")
		case jceksTagSecretKey:
			sealed, n, err := readJavaObject(data[r.pos:])
			if err != nil {
				return nil, err
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
	for _, c := range utf16.Encode([]rune(storePassword)) {
		h.Write([]byte{byte(c >> 8), byte(c)})
	}
	h.Write([]byte(jceksIntegrityWhitens))
	h.Write(data[:r.pos-sha1.Size])
	if subtle.ConstantTimeCompare(h.Sum(nil), digest) != 1 {
		return nil, errors.New("Keystore was tampered with, or password was incorrect")
	}
	return entries, nil
}

func (r *javaStreamReader) skipPrivateKeyEntry() error {
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
		if err := r.skipCertificate(); err != nil {
			return err
		}
	}
	return nil
}

func (r *javaStreamReader) skipCertificate() error {
	if _, err := r.utf(false); err != nil { // certificate type
		return err
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
	if !ok || !obj.class.isA("javax.crypto.SealedObject") {
		return secretKey{}, errors.New("a secret key entry is not a SealedObject")
	}
	sealAlg, _ := obj.fields["sealAlg"].(string)
	paramsAlg, _ := obj.fields["paramsAlg"].(string)
	content, _ := obj.fields["encryptedContent"].([]byte)
	params, _ := obj.fields["encodedParams"].([]byte)
	if sealAlg != jceksPBEAlgorithm || paramsAlg != jceksPBEAlgorithm || len(params) == 0 {
		return secretKey{}, fmt.Errorf("a secret key sealed with %q", sealAlg)
	}
	if err := checkJCEKSPassword(password); err != nil {
		return secretKey{}, err
	}
	var pbe struct {
		Salt       []byte
		Iterations int
	}
	if rest, err := asn1.Unmarshal(params, &pbe); err != nil || len(rest) != 0 || len(pbe.Salt) != 8 || pbe.Iterations < 1 {
		return secretKey{}, errors.New("bad PBEWithMD5AndTripleDES parameters")
	}
	plain, err := pbeWithMD5AndTripleDESDecrypt(content, password, pbe.Salt, pbe.Iterations)
	if err != nil {
		return secretKey{}, err
	}
	key, _, err := readJavaObject(plain)
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
		return secretKey{bytes: b, algorithm: alg}, nil
	case "java.security.KeyRep":
		alg, _ := k.fields["algorithm"].(string)
		format, _ := k.fields["format"].(string)
		typ, _ := k.fields["type"].(javaEnum)
		b, ok := k.fields["encoded"].([]byte)
		if !ok || alg == "" || typ.constant != "SECRET" || format != "RAW" {
			return secretKey{}, fmt.Errorf("a KeyRep of type %q, format %q, not a raw secret key", typ.constant, format)
		}
		return secretKey{bytes: b, algorithm: alg}, nil
	}
	return secretKey{}, fmt.Errorf("a sealed %s, not a secret key", k.class.name)
}

// checkJCEKSPassword is the PBEKey check a key password passes through: a
// character outside printable ASCII is "Password is not ASCII" (measured on
// JDK 21). Unlike PKCS12's, a JCEKS key may have the empty password.
func checkJCEKSPassword(password string) error {
	for _, c := range password {
		if c < 0x20 || c > 0x7e {
			return errors.New("Password is not ASCII")
		}
	}
	return nil
}

// pbeWithMD5AndTripleDESDecrypt is SunJCE's PBEWithMD5AndTripleDES: PKCS #5
// PBES1 with DES-EDE3. The salt is split in halves (the first reordered when
// they are equal, as SunJCE reorders it); each half is digested with the
// password, iterations times, through MD5; the two digests are the 24-byte
// key and, last, the 8-byte IV; the content is DES-EDE3-CBC, PKCS #5 padded.
func pbeWithMD5AndTripleDESDecrypt(data []byte, password string, salt []byte, iterations int) ([]byte, error) {
	halves := [2][]byte{append([]byte(nil), salt[:4]...), append([]byte(nil), salt[4:]...)}
	if bytes.Equal(halves[0], halves[1]) {
		s := halves[0]
		s[0], s[1], s[2] = s[3], s[0], s[1]
	}
	var derived []byte
	for _, d := range halves {
		for range iterations {
			sum := md5.Sum(append(d, password...)) //nolint:gosec // the algorithm's digest
			d = sum[:]
		}
		derived = append(derived, d...)
	}
	block, err := des.NewTripleDESCipher(derived[:24])
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data)%des.BlockSize != 0 {
		return nil, errors.New("Input length must be multiple of 8 when decrypting with padded cipher")
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, derived[24:]).CryptBlocks(plain, data)
	n := int(plain[len(plain)-1])
	if n == 0 || n > des.BlockSize || !bytes.Equal(plain[len(plain)-n:], bytes.Repeat([]byte{byte(n)}, n)) {
		return nil, errors.New("Given final block not properly padded. Such issues can arise if a bad key is used during decryption.")
	}
	return plain[:len(plain)-n], nil
}
