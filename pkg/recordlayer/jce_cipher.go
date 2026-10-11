package recordlayer

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// The AES transformations TransformedRecordSerializerJCE can run. Java
// initializes every cipher with a 16-byte IvParameterSpec
// (TransformedRecordSerializerJCE.java:67-72, :98-106), so of SunJCE's AES
// transformations only those whose mode takes a 16-byte IV work: CBC, PCBC and
// CTS (block modes), CTR, and CFB and OFB with a feedback unit of 1 to 16 bytes.
// ECB refuses an IV, GCM a plain IvParameterSpec, and KW and KWP a 16-byte one.
// The modes are NIST SP 800-38A's and its addendum's (CTS is CBC-CS3); OFB with
// a unit below the block is FIPS 81's, whose register takes the cipher output's
// leading unit. Which names SunJCE accepts, with which keys, and how each mode
// treats lengths and padding were measured on JDK 21 (jce_cipher_test.go).

type jceMode int

const (
	jceCBC jceMode = iota
	jcePCBC
	jceCTS
	jceCTR
	jceCFB
	jceOFB
)

type jcePadding int

const (
	jceNoPadding jcePadding = iota
	jcePKCS5Padding
	jceISO10126Padding
)

// jceKeyCheck is the key check SunJCE makes when a cipher is initialized,
// before it looks at the IV.
type jceKeyCheck int

const (
	jceAnyAESKey jceKeyCheck = iota // 16, 24 or 32 bytes
	jceSizedKey                     // keySize bytes, "The key must be N bytes"
	jceWrapKey                      // keySize bytes, "Invalid key length: N bytes" (the AES_n key wraps)
)

// jceTransformation is a transformation name as Cipher.getInstance finds it.
type jceTransformation struct {
	name     string
	keySize  int
	keyCheck jceKeyCheck
	mode     jceMode
	unit     int // CFB and OFB: the feedback unit in bytes
	padding  jcePadding
	// initRefusal is set when getInstance finds the name but init refuses the
	// IV (ECB, GCM, KW, KWP). A wrong key is refused first, except under GCM.
	initRefusal     string
	refuseBeforeKey bool
}

// aesOID is the NIST arc of AES, an alias of AES. Its sub-arcs 1 to 8, 21 to 28
// and 41 to 48 (but 7) are SunJCE's aliases of AES_128, AES_192 and AES_256 with
// ECB, CBC, OFB, CFB, KW, GCM and KWP.
const aesOID = "2.16.840.1.101.3.4.1"

const (
	jceECBRefusal = "ECB mode cannot use IV"
	jceGCMRefusal = "AlgorithmParameterSpec not of GCMParameterSpec"
	jceKWRefusal  = "Invalid IV"
)

// jceTransformations caches parses by name: a store names a few ciphers, and
// every record it encrypts or decrypts looks one up.
var jceTransformations sync.Map // string → jceParse

type jceParse struct {
	t   jceTransformation
	err error
}

// parseJCETransformation reads a name as Cipher.getInstance does: one token or
// three, "/"-separated, each trimmed of control characters and spaces and upper-
// cased as Java's toUpperCase(Locale.ENGLISH) does (so "ı" is "I" but the Kelvin
// sign is not "K"), none empty. Its error is getInstance's NoSuchAlgorithmException.
func parseJCETransformation(name string) (jceTransformation, error) {
	if p, ok := jceTransformations.Load(name); ok {
		return p.(jceParse).t, p.(jceParse).err
	}
	t, ok := parseJCETokens(name)
	var err error
	if !ok {
		t, err = jceTransformation{}, fmt.Errorf("unsupported cipher %s: Cannot find any provider supporting it", name)
	}
	jceTransformations.Store(name, jceParse{t, err})
	return t, err
}

func parseJCETokens(name string) (jceTransformation, bool) {
	tokens := strings.Split(name, "/")
	for i, tok := range tokens {
		tokens[i] = strings.ToUpper(strings.TrimFunc(tok, func(r rune) bool { return r <= ' ' }))
		if tokens[i] == "" {
			return jceTransformation{}, false
		}
	}
	t := jceTransformation{name: name, unit: aes.BlockSize}
	if len(tokens) == 1 {
		// An algorithm alone takes its provider's default mode: AES's is ECB,
		// and an OID names its own transformation.
		if tokens[0] == "AES" {
			t.initRefusal = jceECBRefusal
			return t, true
		}
		size, arc, ok := aesOIDAlias(tokens[0])
		if !ok {
			return t, false
		}
		t.keySize, t.keyCheck = size, jceSizedKey
		switch arc {
		case 0, 1:
			t.initRefusal = jceECBRefusal
		case 2:
			t.mode = jceCBC
		case 3:
			t.mode = jceOFB
		case 4:
			t.mode = jceCFB
		case 5, 8:
			t.keyCheck, t.initRefusal = jceWrapKey, jceKWRefusal
		case 6:
			t.initRefusal, t.refuseBeforeKey = jceGCMRefusal, true
		}
		if arc == 0 {
			t.keyCheck = jceAnyAESKey
		}
		return t, true
	}
	if len(tokens) != 3 {
		return t, false
	}
	alg, mode := tokens[0], tokens[1]
	switch tokens[2] {
	case "NOPADDING":
	case "PKCS5PADDING":
		t.padding = jcePKCS5Padding
	case "ISO10126PADDING":
		t.padding = jceISO10126Padding
	default:
		return t, false
	}
	switch alg {
	case "AES":
		// AES's registered transformations, then its general cipher.
		switch {
		case mode == "GCM" && t.padding == jceNoPadding:
			t.initRefusal, t.refuseBeforeKey = jceGCMRefusal, true
		case mode == "KW" && t.padding != jceISO10126Padding, mode == "KWP" && t.padding == jceNoPadding:
			t.initRefusal = jceKWRefusal
		case mode == "ECB":
			t.initRefusal = jceECBRefusal
		default:
			return t, t.setMode(mode)
		}
		return t, true
	case "AES_128", "AES_192", "AES_256":
		// Only the transformations SunJCE registers under these names.
		bits, _ := strconv.Atoi(alg[4:])
		t.keySize, t.keyCheck = bits/8, jceSizedKey
		switch {
		case t.padding == jcePKCS5Padding && mode == "KW":
			t.keyCheck, t.initRefusal = jceWrapKey, jceKWRefusal
		case t.padding != jceNoPadding:
			return t, false
		case mode == "CBC", mode == "CFB", mode == "OFB":
			return t, t.setMode(mode)
		case mode == "ECB":
			t.initRefusal = jceECBRefusal
		case mode == "GCM":
			t.initRefusal, t.refuseBeforeKey = jceGCMRefusal, true
		case mode == "KW", mode == "KWP":
			t.keyCheck, t.initRefusal = jceWrapKey, jceKWRefusal
		default:
			return t, false
		}
		return t, true
	}
	// An OID with a mode and padding is the cipher its alias names, set to that
	// mode: the ECB, CBC, OFB and CFB ciphers take the general modes, keeping their
	// key size; the GCM, KW and KWP ciphers their own mode only.
	size, arc, ok := aesOIDAlias(alg)
	if !ok {
		return t, false
	}
	t.keySize, t.keyCheck = size, jceSizedKey
	switch arc {
	case 0:
		t.keyCheck = jceAnyAESKey
		if mode == "ECB" {
			t.initRefusal = jceECBRefusal
			return t, true
		}
		return t, t.setMode(mode)
	case 1, 2, 3, 4:
		switch {
		case mode != "ECB":
			return t, t.setMode(mode)
		case arc == 1:
			t.initRefusal = jceECBRefusal
		default:
			// Measured: the CBC, OFB and CFB ciphers keep their mode under "ECB".
			t.mode = [...]jceMode{2: jceCBC, 3: jceOFB, 4: jceCFB}[arc]
		}
		return t, true
	case 5, 8:
		if t.padding != jceNoPadding || arc == 5 && mode != "KW" || arc == 8 && mode != "KWP" {
			return t, false
		}
		t.keyCheck, t.initRefusal = jceWrapKey, jceKWRefusal
		return t, true
	case 6:
		if t.padding != jceNoPadding || mode != "GCM" {
			return t, false
		}
		t.initRefusal, t.refuseBeforeKey = jceGCMRefusal, true
		return t, true
	}
	return t, false
}

// setMode sets one of the general cipher's modes that take a 16-byte IV. CTR
// and CTS take NoPadding only; CFB and OFB take a unit of whole bytes, its width
// written canonically (SunJCE refuses CFB008 and CFB+8).
func (t *jceTransformation) setMode(mode string) bool {
	switch {
	case mode == "CBC":
		t.mode = jceCBC
	case mode == "PCBC":
		t.mode = jcePCBC
	case mode == "CTS":
		t.mode = jceCTS
	case mode == "CTR":
		t.mode = jceCTR
	case strings.HasPrefix(mode, "CFB"), strings.HasPrefix(mode, "OFB"):
		t.mode = jceCFB
		if mode[0] == 'O' {
			t.mode = jceOFB
		}
		if bits := mode[3:]; bits != "" {
			n, err := strconv.Atoi(bits)
			if err != nil || strconv.Itoa(n) != bits || n < 8 || n > 128 || n%8 != 0 {
				return false
			}
			t.unit = n / 8
		}
	default:
		return false
	}
	return t.padding == jceNoPadding || t.mode != jceCTR && t.mode != jceCTS
}

// aesOIDAlias reads tok, upper-cased, as an AES OID with or without "OID.":
// the key size its sub-arc names, and the sub-arc less 0, 20 or 40 (1 ECB,
// 2 CBC, 3 OFB, 4 CFB, 5 KW, 6 GCM, 8 KWP), both 0 for the arc itself.
func aesOIDAlias(tok string) (keySize, arc int, ok bool) {
	tok = strings.TrimPrefix(tok, "OID.")
	if tok == aesOID {
		return 0, 0, true
	}
	sub, found := strings.CutPrefix(tok, aesOID+".")
	if !found {
		return 0, 0, false
	}
	n, err := strconv.Atoi(sub)
	if err != nil || strconv.Itoa(n) != sub {
		return 0, 0, false
	}
	switch {
	case n >= 1 && n <= 8:
		keySize, arc = 16, n
	case n >= 21 && n <= 28:
		keySize, arc = 24, n-20
	case n >= 41 && n <= 48:
		keySize, arc = 32, n-40
	default:
		return 0, 0, false
	}
	return keySize, arc, arc != 7
}

// newBlock is AES under key, initialized as SunJCE initializes the cipher:
// the key checked, then the IV.
func (t jceTransformation) newBlock(key []byte) (cipher.Block, error) {
	if t.initRefusal != "" && t.refuseBeforeKey {
		return nil, fmt.Errorf("unsupported cipher %s: %s", t.name, t.initRefusal)
	}
	switch {
	case t.keyCheck == jceSizedKey && len(key) != t.keySize:
		return nil, fmt.Errorf("The key must be %d bytes", t.keySize)
	case t.keyCheck == jceWrapKey && len(key) != t.keySize:
		return nil, fmt.Errorf("Invalid key length: %d bytes", len(key))
	case len(key) != 16 && len(key) != 24 && len(key) != 32:
		return nil, fmt.Errorf("Invalid AES key length: %d bytes", len(key))
	case t.initRefusal != "":
		return nil, fmt.Errorf("unsupported cipher %s: %s", t.name, t.initRefusal)
	}
	return aes.NewCipher(key)
}

// encrypt returns iv, then the cipher text of plain padded, in one allocation.
// ISO 10126 padding draws its filler from filler.
func (t jceTransformation) encrypt(block cipher.Block, iv, plain []byte, filler io.Reader) ([]byte, error) {
	n := 0
	if t.padding != jceNoPadding {
		n = aes.BlockSize - len(plain)%aes.BlockSize
	} else if err := t.checkLength(len(plain)); err != nil {
		return nil, err
	}
	out := make([]byte, len(iv)+len(plain)+n)
	copy(out, iv)
	text := out[len(iv):]
	copy(text, plain)
	if n > 0 {
		pad := text[len(plain):]
		if t.padding == jceISO10126Padding {
			if _, err := io.ReadFull(filler, pad[:n-1]); err != nil {
				return nil, err
			}
		} else {
			for i := range pad {
				pad[i] = byte(n)
			}
		}
		pad[n-1] = byte(n)
	}
	t.crypt(block, iv, text, text, false)
	return out, nil
}

// decrypt deciphers text under iv and removes its padding.
func (t jceTransformation) decrypt(block cipher.Block, iv, text []byte) ([]byte, error) {
	if t.padding != jceNoPadding && len(text)%aes.BlockSize != 0 {
		return nil, errors.New("Input length must be multiple of 16 when decrypting with padded cipher")
	}
	if err := t.checkLength(len(text)); err != nil {
		return nil, err
	}
	plain := make([]byte, len(text))
	t.crypt(block, iv, plain, text, true)
	// Java deciphers no input to no output, padded or not.
	if t.padding == jceNoPadding || len(plain) == 0 {
		return plain, nil
	}
	// A padded plain text is at least one block, so n never exceeds it.
	n := int(plain[len(plain)-1])
	if n == 0 || n > aes.BlockSize {
		return nil, errors.New("bad padding")
	}
	if t.padding == jcePKCS5Padding {
		for _, p := range plain[len(plain)-n:] {
			if int(p) != n {
				return nil, errors.New("bad padding")
			}
		}
	}
	return plain[:len(plain)-n], nil
}

// checkLength refuses an unpadded length the mode cannot take: CBC and PCBC
// take whole blocks, CTS at least one block or nothing.
func (t jceTransformation) checkLength(n int) error {
	switch {
	case (t.mode == jceCBC || t.mode == jcePCBC) && n%aes.BlockSize != 0:
		return errors.New("Input length not multiple of 16 bytes")
	case t.mode == jceCTS && n > 0 && n < aes.BlockSize:
		return errors.New("input is too short!")
	}
	return nil
}

// crypt enciphers or deciphers src into dst, which is src itself or apart
// from it. The length has passed checkLength (padded input is whole blocks).
func (t jceTransformation) crypt(block cipher.Block, iv, dst, src []byte, decrypt bool) {
	switch t.mode {
	case jceCBC:
		cbc(block, iv, dst, src, decrypt)
	case jcePCBC:
		pcbc(block, iv, dst, src, decrypt)
	case jceCTS:
		cts(block, iv, dst, src, decrypt)
	case jceCTR:
		cipher.NewCTR(block, iv).XORKeyStream(dst, src)
	case jceCFB, jceOFB:
		feedback(block, iv, dst, src, t.unit, t.mode == jceOFB, decrypt)
	}
}

func cbc(block cipher.Block, iv, dst, src []byte, decrypt bool) {
	if decrypt {
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(dst, src)
	} else {
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(dst, src)
	}
}

// pcbc is propagating CBC: each block is chained with the previous block's
// plain text XOR its cipher text, the first with the IV.
func pcbc(block cipher.Block, iv, dst, src []byte, decrypt bool) {
	var chain, in [aes.BlockSize]byte
	copy(chain[:], iv)
	for i := 0; i < len(src); i += aes.BlockSize {
		copy(in[:], src[i:]) // dst may be src
		out := dst[i : i+aes.BlockSize]
		if decrypt {
			block.Decrypt(out, in[:])
			subtle.XORBytes(out, out, chain[:])
			subtle.XORBytes(chain[:], out, in[:])
		} else {
			subtle.XORBytes(out, in[:], chain[:])
			block.Encrypt(out, out)
			subtle.XORBytes(chain[:], in[:], out)
		}
	}
}

// cts is CBC-CS3 (SP 800-38A addendum): CBC over the input padded with zeros
// to whole blocks, the last two blocks swapped whenever there are two, aligned
// or not, and the then-last truncated to the final partial block's length.
// One block, or none, is plain CBC.
func cts(block cipher.Block, iv, dst, src []byte, decrypt bool) {
	n := len(src)
	if n <= aes.BlockSize {
		cbc(block, iv, dst, src, decrypt)
		return
	}
	full := (n - 1) / aes.BlockSize * aes.BlockSize // whole blocks before the last one, partial or whole
	head := full - aes.BlockSize                    // blocks chained as plain CBC
	d := n - full                                   // the last block's length, 1 to 16
	var chain, buf, prev [aes.BlockSize]byte
	copy(chain[:], iv)
	if head > 0 {
		if decrypt {
			copy(chain[:], src[head-aes.BlockSize:head]) // before dst, which may be src, is written
		}
		cbc(block, iv, dst[:head], src[:head], decrypt)
		if !decrypt {
			copy(chain[:], dst[head-aes.BlockSize:head])
		}
	}
	if decrypt {
		// src ends with C_k whole, then the leading d bytes of C_(k-1); the
		// rest of C_(k-1) is the tail of C_k deciphered.
		block.Decrypt(buf[:], src[head:full])
		copy(prev[:], src[full:])
		copy(prev[d:], buf[d:])
		subtle.XORBytes(dst[full:], buf[:d], src[full:])
		block.Decrypt(buf[:], prev[:])
		subtle.XORBytes(dst[head:full], buf[:], chain[:])
		return
	}
	subtle.XORBytes(buf[:], src[head:full], chain[:])
	block.Encrypt(prev[:], buf[:])
	clear(buf[:])
	copy(buf[:], src[full:])
	subtle.XORBytes(buf[:], buf[:], prev[:])
	block.Encrypt(dst[head:full], buf[:])
	copy(dst[full:], prev[:d])
}

// feedback is CFB or OFB with a unit of unit bytes: each unit is XORed with
// the leading unit of the enciphered register, which then shifts that unit
// out and takes in the cipher text (CFB) or the cipher output (OFB). A final
// partial unit uses as much of the output as it needs.
func feedback(block cipher.Block, iv, dst, src []byte, unit int, ofb, decrypt bool) {
	var reg, o, in [aes.BlockSize]byte
	copy(reg[:], iv)
	for i := 0; i < len(src); i += unit {
		block.Encrypt(o[:], reg[:])
		seg := in[:copy(in[:unit], src[i:])] // dst may be src
		out := dst[i : i+len(seg)]
		subtle.XORBytes(out, seg, o[:])
		next := o[:unit]
		switch {
		case ofb:
		case decrypt:
			next = seg
		default:
			next = out
		}
		copy(reg[:], reg[unit:])
		copy(reg[aes.BlockSize-unit:], next)
	}
}
