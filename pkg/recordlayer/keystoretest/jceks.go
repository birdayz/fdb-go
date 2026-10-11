// Portions derived from pyjks (jks/jks.py, jks/sun_crypto.py),
// Copyright (c) 2013 Kurt Rose; Jeroen De Ridder
// Licensed under the MIT License (see NOTICE); translated to Go and modified.

package keystoretest

import (
	"bytes"
	"crypto/cipher"
	"crypto/des"
	"crypto/md5" //nolint:gosec // PBEWithMD5AndTripleDES is MD5-based
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // the JCEKS integrity digest is SHA-1
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"unicode/utf16"
)

// JCEKSEntry is one entry of a JCEKS store WriteJCEKS writes: a secret key
// (its algorithm "AES" when empty, serialized as a KeyRep when KeyRep is set,
// as a key class that replaces itself is), or, with Kind, a private key or
// trusted certificate entry holding Key as its opaque bytes.
type JCEKSEntry struct {
	Alias     string
	Key       []byte
	Algorithm string
	KeyRep    bool
	Kind      JCEKSKind
}

// JCEKSKind is an entry's tag.
type JCEKSKind int

const (
	JCEKSSecretKey   JCEKSKind = 3
	JCEKSPrivateKey  JCEKSKind = 1
	JCEKSTrustedCert JCEKSKind = 2
)

// The serialized class descriptors the JDK writes for a sealed secret key,
// copied from a JCEKS store JDK 21 wrote: SealedObjectForKeyProtector over
// SealedObject (encodedParams, encryptedContent, paramsAlg, sealAlg), byte[],
// SecretKeySpec (algorithm, key), and KeyRep (algorithm, encoded, format, type)
// with its Type enum.
const (
	sealedObjectHead = "aced000573720033636f6d2e73756e2e63727970746f2e70726f76696465722e5365616c65644f626a656374466f724b65" +
		"7950726f746563746f72cd57ca59e730bb53020000787200196a617661782e63727970746f2e5365616c65644f626a6563743e363da6c3b7" +
		"54700200045b000d656e636f646564506172616d737400025b425b0010656e63727970746564436f6e74656e7471007e00024c0009706172" +
		"616d73416c677400124c6a6176612f6c616e672f537472696e673b4c00077365616c416c6771007e00037870"
	byteArrayDesc     = "757200025b42acf317f8060854e00200007870"
	byteArrayRef      = "7571007e0005" // byte[]'s descriptor again, its handle in the sealed object's stream
	secretKeySpecHead = "aced00057372001f6a617661782e63727970746f2e737065632e5365637265744b6579537065635b470b66e230614d02" +
		"00024c0009616c676f726974686d7400124c6a6176612f6c616e672f537472696e673b5b00036b65797400025b427870"
	keyRepHead = "aced0005737200146a6176612e73656375726974792e4b6579526570bdf94fb3889aa5430200044c0009616c676f72697468" +
		"6d7400124c6a6176612f6c616e672f537472696e673b5b0007656e636f6465647400025b424c0006666f726d617471007e00014c0004" +
		"7479706574001b4c6a6176612f73656375726974792f4b657952657024547970653b7870"
	keyRepSecret = "7e7200196a6176612e73656375726974792e4b6579526570245479706500000000000000001200007872000e6a6176612e6c616e" +
		"672e456e756d00000000000000001200007870740006534543524554"
)

// WriteJCEKS writes a JCEKS key store holding entries, each secret key sealed
// under keyPassword with PBEWithMD5AndTripleDES over iterations rounds (the JDK
// uses 200000; a test may use fewer), and the store's digest keyed by
// storePassword. It is not compatibility evidence: Go's reader is pinned
// against stores the JDK writes by the JVM spec (conformance).
func WriteJCEKS(path, storePassword, keyPassword string, iterations int, entries []JCEKSEntry) error {
	var out bytes.Buffer
	out.Write([]byte{0xce, 0xce, 0xce, 0xce, 0, 0, 0, 2})
	writeU32(&out, len(entries))
	for _, e := range entries {
		kind := e.Kind
		if kind == 0 {
			kind = JCEKSSecretKey
		}
		writeU32(&out, int(kind))
		writeUTF(&out, strings.ToLower(e.Alias))
		out.Write(make([]byte, 8)) // creation date
		switch kind {
		case JCEKSPrivateKey:
			writeU32(&out, len(e.Key))
			out.Write(e.Key)
			writeU32(&out, 0) // no certificate chain
		case JCEKSTrustedCert:
			writeUTF(&out, "X.509")
			writeU32(&out, len(e.Key))
			out.Write(e.Key)
		default:
			sealed, err := sealKey(e, keyPassword, iterations)
			if err != nil {
				return err
			}
			out.Write(sealed)
		}
	}
	h := sha1.New() //nolint:gosec // the JCEKS integrity digest is SHA-1
	for _, c := range utf16.Encode([]rune(storePassword)) {
		h.Write([]byte{byte(c >> 8), byte(c)})
	}
	h.Write([]byte("Mighty Aphrodite"))
	h.Write(out.Bytes())
	out.Write(h.Sum(nil))
	return os.WriteFile(path, out.Bytes(), 0o600)
}

func sealKey(e JCEKSEntry, password string, iterations int) ([]byte, error) {
	alg := e.Algorithm
	if alg == "" {
		alg = "AES"
	}
	var key bytes.Buffer
	if e.KeyRep {
		key.Write(mustHex(keyRepHead))
		writeString(&key, alg)
		key.Write(mustHex(byteArrayDesc))
		writeU32(&key, len(e.Key))
		key.Write(e.Key)
		writeString(&key, "RAW")
		key.Write(mustHex(keyRepSecret))
	} else {
		key.Write(mustHex(secretKeySpecHead))
		writeString(&key, alg)
		key.Write(mustHex(byteArrayDesc))
		writeU32(&key, len(e.Key))
		key.Write(e.Key)
	}
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	params, err := asn1.Marshal(struct {
		Salt       []byte
		Iterations int
	}{salt, iterations})
	if err != nil {
		return nil, err
	}
	content, err := pbeWithMD5AndTripleDES(key.Bytes(), password, salt, iterations)
	if err != nil {
		return nil, err
	}
	var obj bytes.Buffer
	obj.Write(mustHex(sealedObjectHead))
	obj.Write(mustHex(byteArrayDesc))
	writeU32(&obj, len(params))
	obj.Write(params)
	obj.Write(mustHex(byteArrayRef))
	writeU32(&obj, len(content))
	obj.Write(content)
	writeString(&obj, "PBEWithMD5AndTripleDES") // paramsAlg
	writeString(&obj, "PBEWithMD5AndTripleDES") // sealAlg
	return obj.Bytes(), nil
}

// SealPBEWithMD5AndTripleDES encrypts plain as SunJCE's PBEWithMD5AndTripleDES
// does, for tests that build a sealed key themselves.
func SealPBEWithMD5AndTripleDES(plain []byte, password string, salt []byte, iterations int) ([]byte, error) {
	return pbeWithMD5AndTripleDES(plain, password, salt, iterations)
}

// pbeWithMD5AndTripleDES encrypts as SunJCE's PBEWithMD5AndTripleDES does.
func pbeWithMD5AndTripleDES(plain []byte, password string, salt []byte, iterations int) ([]byte, error) {
	halves := [2][]byte{append([]byte(nil), salt[:4]...), append([]byte(nil), salt[4:]...)}
	if bytes.Equal(halves[0], halves[1]) {
		slices.Reverse(halves[0]) // as JDK 21 does
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
	n := des.BlockSize - len(plain)%des.BlockSize
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(n)}, n)...)
	cipher.NewCBCEncrypter(block, derived[24:]).CryptBlocks(padded, padded)
	return padded, nil
}

func writeU32(b *bytes.Buffer, n int) {
	var v [4]byte
	binary.BigEndian.PutUint32(v[:], uint32(n)) //nolint:gosec // test sizes
	b.Write(v[:])
}

func writeUTF(b *bytes.Buffer, s string) {
	var v [2]byte
	binary.BigEndian.PutUint16(v[:], uint16(len(s))) //nolint:gosec // test strings
	b.Write(v[:])
	b.WriteString(s)
}

// writeString writes a new String object.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte(0x74)
	writeUTF(b, s)
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
