package client

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"strings"
)

var (
	oidPBES2       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidDESEDE3CBC  = asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 7}
	oidAES128CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	oidHMACSHA1    = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACSHA224  = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 8}
	oidHMACSHA256  = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHMACSHA384  = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 10}
	oidHMACSHA512  = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}
	errKeyPassword = errors.New("wrong password or corrupt key")
)

// keySchemeError reports a key encryption the client does not decrypt: Part names
// the refused step and OID its algorithm (nil for legacy PEM encryption).
type keySchemeError struct {
	Part string
	OID  asn1.ObjectIdentifier
}

func (e *keySchemeError) Error() string {
	if e.OID == nil {
		return "legacy encrypted PEM key: convert it with openssl pkcs8 -topk8"
	}
	return fmt.Sprintf("unsupported key %s %v: only PBES2 with PBKDF2 and AES-CBC or DES-EDE3-CBC is supported", e.Part, e.OID)
}

// encryptedPrivateKeyInfo is RFC 5958's EncryptedPrivateKeyInfo.
type encryptedPrivateKeyInfo struct {
	Algorithm     pkix.AlgorithmIdentifier
	EncryptedData []byte
}

// pbes2Params is RFC 8018's PBES2-params.
type pbes2Params struct {
	KeyDerivationFunc pkix.AlgorithmIdentifier
	EncryptionScheme  pkix.AlgorithmIdentifier
}

// pbkdf2Params is RFC 8018's PBKDF2-params, with the salt as an OCTET STRING.
type pbkdf2Params struct {
	Salt           []byte
	IterationCount int
	KeyLength      int                      `asn1:"optional"`
	PRF            pkix.AlgorithmIdentifier `asn1:"optional"`
}

// decryptKeyPEM returns keyPEM with its private key decrypted. An encrypted PKCS#8
// key is decrypted with password, as OpenSSL's password callback does; any other key
// is returned as it is, ignoring the password.
func decryptKeyPEM(keyPEM []byte, password string) ([]byte, error) {
	for rest := keyPEM; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return keyPEM, nil
		}
		if !strings.HasSuffix(block.Type, "PRIVATE KEY") {
			continue
		}
		if strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED") {
			return nil, &keySchemeError{Part: "legacy PEM encryption"}
		}
		if block.Type != "ENCRYPTED PRIVATE KEY" {
			return keyPEM, nil
		}
		der, err := decryptPKCS8(block.Bytes, password)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
	}
}

// decryptPKCS8 decrypts an EncryptedPrivateKeyInfo under PBES2 with PBKDF2 and an
// AES-CBC or DES-EDE3-CBC cipher, the schemes openssl pkcs8 -topk8 -v2 writes.
func decryptPKCS8(der []byte, password string) ([]byte, error) {
	var info encryptedPrivateKeyInfo
	if rest, err := asn1.Unmarshal(der, &info); err != nil || len(rest) != 0 {
		return nil, errors.New("malformed encrypted private key")
	}
	if !info.Algorithm.Algorithm.Equal(oidPBES2) {
		return nil, &keySchemeError{Part: "encryption", OID: info.Algorithm.Algorithm}
	}
	var params pbes2Params
	if _, err := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &params); err != nil {
		return nil, errors.New("malformed PBES2 parameters")
	}
	if !params.KeyDerivationFunc.Algorithm.Equal(oidPBKDF2) {
		return nil, &keySchemeError{Part: "derivation", OID: params.KeyDerivationFunc.Algorithm}
	}
	var kdf pbkdf2Params
	if _, err := asn1.Unmarshal(params.KeyDerivationFunc.Parameters.FullBytes, &kdf); err != nil {
		return nil, errors.New("malformed PBKDF2 parameters")
	}
	prf, err := pbkdf2PRF(kdf.PRF.Algorithm)
	if err != nil {
		return nil, err
	}
	newCipher, keyLen, err := pbes2Cipher(params.EncryptionScheme.Algorithm)
	if err != nil {
		return nil, err
	}
	if kdf.KeyLength != 0 && kdf.KeyLength != keyLen {
		return nil, fmt.Errorf("PBKDF2 key length %d does not fit the cipher", kdf.KeyLength)
	}
	var iv []byte
	if _, err := asn1.Unmarshal(params.EncryptionScheme.Parameters.FullBytes, &iv); err != nil {
		return nil, errors.New("malformed cipher IV")
	}
	key, err := pbkdf2.Key(prf, password, kdf.Salt, kdf.IterationCount, keyLen)
	if err != nil {
		return nil, err
	}
	block, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	data := info.EncryptedData
	if len(iv) != block.BlockSize() || len(data) == 0 || len(data)%block.BlockSize() != 0 {
		return nil, errKeyPassword
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, data)
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > block.BlockSize() || !bytes.Equal(plain[len(plain)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errKeyPassword
	}
	plain = plain[:len(plain)-pad]
	if _, err := x509.ParsePKCS8PrivateKey(plain); err != nil {
		return nil, errKeyPassword
	}
	return plain, nil
}

func pbkdf2PRF(oid asn1.ObjectIdentifier) (func() hash.Hash, error) {
	switch {
	case len(oid) == 0, oid.Equal(oidHMACSHA1):
		return sha1.New, nil // the PBKDF2-params default
	case oid.Equal(oidHMACSHA224):
		return sha256.New224, nil
	case oid.Equal(oidHMACSHA256):
		return sha256.New, nil
	case oid.Equal(oidHMACSHA384):
		return sha512.New384, nil
	case oid.Equal(oidHMACSHA512):
		return sha512.New, nil
	}
	return nil, &keySchemeError{Part: "PBKDF2 PRF", OID: oid}
}

func pbes2Cipher(oid asn1.ObjectIdentifier) (func([]byte) (cipher.Block, error), int, error) {
	switch {
	case oid.Equal(oidAES128CBC):
		return aes.NewCipher, 16, nil
	case oid.Equal(oidAES192CBC):
		return aes.NewCipher, 24, nil
	case oid.Equal(oidAES256CBC):
		return aes.NewCipher, 32, nil
	case oid.Equal(oidDESEDE3CBC):
		return des.NewTripleDESCipher, 24, nil
	}
	return nil, 0, &keySchemeError{Part: "cipher", OID: oid}
}
