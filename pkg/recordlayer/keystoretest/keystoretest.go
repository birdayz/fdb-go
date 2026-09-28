// Package keystoretest writes PKCS12 key stores for tests, in the layout the
// JDK's PKCS12KeyStore writes since JDK 12: each AES secret key in a secret
// bag (a PKCS #8 PrivateKeyInfo shrouded under PBES2: PBKDF2 with
// HmacSHA256, AES-256-CBC), the bags in one unencrypted data safe, and an
// HmacSHA256 MAC keyed by the PKCS #12 KDF.
//
// It exists because no key store file may be committed (cmd/secretscan
// refuses one by name, whatever it holds), so a test that needs one writes
// it. It is NOT the compatibility evidence: Go's reader is pinned against
// stores the JDK itself writes by the JVM spec "TransformedRecordSerializer
// records are read and written as Java's" (conformance). This writer only
// gives the key manager's logic tests, and the Java corpus run, a store.
package keystoretest

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"hash"
	"os"
	"strings"
	"unicode/utf16"
)

// Entry is one secret key entry: its alias and its AES key bytes.
type Entry struct {
	Alias string
	Key   []byte
}

var (
	oidData                = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidSecretBag           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 5}
	oidPKCS8ShroudedKeyBag = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 2}
	oidFriendlyName        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 20}
	oidPBES2               = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2              = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHmacSHA256          = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidAES256CBC           = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	oidAES                 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1}
	oidSHA256              = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
)

const iterations = 10000 // the JDK's default for both the key protection and the MAC

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type pbkdf2Params struct {
	Salt           []byte
	IterationCount int
	PRF            algorithmIdentifier
}

type pbes2Params struct {
	KeyDerivationFunc algorithmIdentifier
	EncryptionScheme  algorithmIdentifier
}

type privateKeyInfo struct {
	Version    int
	Algorithm  algorithmIdentifier
	PrivateKey []byte
}

type encryptedPrivateKeyInfo struct {
	Algorithm     algorithmIdentifier
	EncryptedData []byte
}

type secretBag struct {
	SecretTypeID asn1.ObjectIdentifier
	SecretValue  asn1.RawValue
}

type attribute struct {
	ID    asn1.ObjectIdentifier
	Value asn1.RawValue
}

type safeBag struct {
	ID         asn1.ObjectIdentifier
	Value      asn1.RawValue
	Attributes []attribute `asn1:"set"`
}

type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue
}

type digestInfo struct {
	Algorithm algorithmIdentifier
	Digest    []byte
}

type macData struct {
	Mac        digestInfo
	MacSalt    []byte
	Iterations int
}

type pfx struct {
	Version  int
	AuthSafe contentInfo
	MacData  macData
}

// WritePKCS12 writes the entries to path, the store under storePassword and
// each key under entryPassword. Aliases are stored lower-cased, as the JDK
// stores them.
func WritePKCS12(path, storePassword, entryPassword string, entries []Entry) error {
	der, err := EncodePKCS12(storePassword, entryPassword, entries)
	if err != nil {
		return err
	}
	return os.WriteFile(path, der, 0o600)
}

// EncodePKCS12 is WritePKCS12's bytes.
func EncodePKCS12(storePassword, entryPassword string, entries []Entry) ([]byte, error) {
	var bags []safeBag
	for _, e := range entries {
		shrouded, err := shroud(e.Key, entryPassword)
		if err != nil {
			return nil, err
		}
		// secretValue [0] EXPLICIT is an OCTET STRING holding the
		// EncryptedPrivateKeyInfo's DER (the JDK's putOctetString).
		sb, err := asn1.Marshal(secretBag{SecretTypeID: oidPKCS8ShroudedKeyBag, SecretValue: explicit0(mustMarshal(mustMarshal(shrouded)))})
		if err != nil {
			return nil, err
		}
		bags = append(bags, safeBag{
			ID:    oidSecretBag,
			Value: explicit0(sb),
			Attributes: []attribute{{
				ID:    oidFriendlyName,
				Value: asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: bmpString(strings.ToLower(e.Alias))},
			}},
		})
	}
	safeContents, err := asn1.Marshal(bags)
	if err != nil {
		return nil, err
	}
	authSafe, err := asn1.Marshal([]contentInfo{{ContentType: oidData, Content: explicit0(mustMarshal(safeContents))}})
	if err != nil {
		return nil, err
	}
	salt := randomBytes(20)
	macKey := kdf(sha256.New, 3, bmpPassword(storePassword), salt, iterations, sha256.Size)
	mac := hmac.New(sha256.New, macKey)
	mac.Write(authSafe)
	return asn1.Marshal(pfx{
		Version:  3,
		AuthSafe: contentInfo{ContentType: oidData, Content: explicit0(mustMarshal(authSafe))},
		MacData: macData{
			Mac:        digestInfo{Algorithm: algorithmIdentifier{Algorithm: oidSHA256, Parameters: asn1.NullRawValue}, Digest: mac.Sum(nil)},
			MacSalt:    salt,
			Iterations: iterations,
		},
	})
}

// shroud wraps key as the JDK's PKCS12KeyStore wraps a SecretKeyEntry: a
// PrivateKeyInfo naming AES, encrypted under PBES2.
func shroud(key []byte, password string) (encryptedPrivateKeyInfo, error) {
	plain, err := asn1.Marshal(privateKeyInfo{Algorithm: algorithmIdentifier{Algorithm: oidAES}, PrivateKey: key})
	if err != nil {
		return encryptedPrivateKeyInfo{}, err
	}
	salt, iv := randomBytes(20), randomBytes(16)
	aesKey, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		return encryptedPrivateKeyInfo{}, err
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return encryptedPrivateKeyInfo{}, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)
	enc := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, plain)
	params, err := asn1.Marshal(pbes2Params{
		KeyDerivationFunc: algorithmIdentifier{Algorithm: oidPBKDF2, Parameters: rawOf(mustMarshal(pbkdf2Params{
			Salt: salt, IterationCount: iterations, PRF: algorithmIdentifier{Algorithm: oidHmacSHA256, Parameters: asn1.NullRawValue},
		}))},
		EncryptionScheme: algorithmIdentifier{Algorithm: oidAES256CBC, Parameters: rawOf(mustMarshal(iv))},
	})
	if err != nil {
		return encryptedPrivateKeyInfo{}, err
	}
	return encryptedPrivateKeyInfo{Algorithm: algorithmIdentifier{Algorithm: oidPBES2, Parameters: rawOf(params)}, EncryptedData: enc}, nil
}

func explicit0(der []byte) asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: der}
}

func rawOf(der []byte) asn1.RawValue {
	var v asn1.RawValue
	if _, err := asn1.Unmarshal(der, &v); err != nil {
		panic(err)
	}
	return v
}

func mustMarshal(v any) []byte {
	b, err := asn1.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// bmpString is a DER BMPString.
func bmpString(s string) []byte {
	var body []byte
	for _, c := range utf16.Encode([]rune(s)) {
		body = append(body, byte(c>>8), byte(c))
	}
	return mustMarshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: 30, Bytes: body})
}

// bmpPassword is the JDK's PKCS12KeyStore.derive password encoding: UTF-16BE
// and a two-byte terminator, except that "\x00" is the empty password with no
// terminator.
func bmpPassword(password string) []byte {
	if password == "\x00" {
		return nil
	}
	var out []byte
	for _, c := range utf16.Encode([]rune(password)) {
		out = append(out, byte(c>>8), byte(c))
	}
	return append(out, 0, 0)
}

// kdf is RFC 7292 appendix B.2.
func kdf(h func() hash.Hash, id byte, password, salt []byte, iterations, size int) []byte {
	u, v := h().Size(), h().BlockSize()
	fill := func(in []byte) []byte {
		if len(in) == 0 {
			return nil
		}
		out := make([]byte, v*((len(in)+v-1)/v))
		for i := range out {
			out[i] = in[i%len(in)]
		}
		return out
	}
	d := bytes.Repeat([]byte{id}, v)
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
