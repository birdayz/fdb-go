package recordlayer

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The AES transformations TransformedRecordSerializerJCE can run. Java
// initializes every cipher with a 16-byte IvParameterSpec
// (TransformedRecordSerializerJCE.java:67-72, :98-106), so of SunJCE's AES
// transformations only those whose mode takes a 16-byte IV work: CBC, PCBC and
// CTS (block modes), CTR, and CFB and OFB with a feedback unit of 8 to 128 bits.
// ECB refuses an IV, GCM a plain IvParameterSpec, and KW and KWP a 16-byte one.
// The modes are NIST SP 800-38A's and its addendum's (CTS is CBC-CS3); OFB with
// a unit below the block is FIPS 81's, whose register takes the cipher output's
// leading unit. What names SunJCE accepts, and how each mode treats lengths and
// padding, was measured on JDK 21 (jce_cipher_test.go).

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

// jceTransformation is a transformation name parsed as Cipher.getInstance
// parses it.
type jceTransformation struct {
	keySize int // the one key size in bytes an AES_n name or a NIST OID names; 0 for any
	mode    jceMode
	unit    int // CFB and OFB: the feedback unit in bytes
	padding jcePadding
}

// aesOID is the NIST arc of AES. Its sub-arcs .1-.4, .21-.24 and .41-.44 are
// SunJCE's aliases of AES_128, AES_192 and AES_256 with ECB, CBC, OFB and CFB
// and NoPadding; the arc itself is an alias of AES.
const aesOID = "2.16.840.1.101.3.4.1"

// parseJCETransformation reads a name as Cipher.getInstance does: one token or
// three, "/"-separated, each trimmed of control characters and spaces and
// matched case-insensitively, none empty. One token is an algorithm with its
// provider's default mode: AES's is ECB, which takes no IV, and an OID names
// its own AES_n transformation. Of three, the AES_n forms name only the
// transformations SunJCE registers (CBC, CFB and OFB with NoPadding), while
// AES and the OIDs take any mode and padding, an OID keeping its key size.
func parseJCETransformation(name string) (jceTransformation, error) {
	refuse := func() (jceTransformation, error) {
		return jceTransformation{}, fmt.Errorf("unsupported cipher %s: not an AES transformation the JCE runs under a 16-byte IV", name)
	}
	tokens := strings.Split(name, "/")
	for i, tok := range tokens {
		tokens[i] = strings.ToUpper(strings.TrimFunc(tok, func(r rune) bool { return r <= ' ' }))
		if tokens[i] == "" {
			return refuse()
		}
	}
	var t jceTransformation
	switch len(tokens) {
	case 1:
		size, arc, ok := aesOIDAlias(tokens[0])
		if !ok || arc < 2 || arc > 4 {
			return refuse()
		}
		t.keySize, t.unit, t.mode = size, aes.BlockSize, oidModes[arc]
		return t, nil
	case 3:
	default:
		return refuse()
	}
	aesN, arc := false, 0
	switch alg := tokens[0]; alg {
	case "AES":
	case "AES_128", "AES_192", "AES_256":
		aesN = true
		t.keySize, _ = strconv.Atoi(alg[4:])
		t.keySize /= 8
	default:
		size, a, ok := aesOIDAlias(alg)
		if !ok || a > 4 {
			return refuse()
		}
		t.keySize, arc = size, a
	}
	switch tokens[2] {
	case "NOPADDING":
	case "PKCS5PADDING":
		t.padding = jcePKCS5Padding
	case "ISO10126PADDING":
		t.padding = jceISO10126Padding
	default:
		return refuse()
	}
	mode := tokens[1]
	switch {
	case mode == "ECB" && arc >= 2:
		// Measured: an OID of CBC, OFB or CFB keeps its own mode under "ECB".
		t.mode, t.unit = oidModes[arc], aes.BlockSize
	case mode == "CBC":
		t.mode = jceCBC
	case mode == "PCBC":
		t.mode = jcePCBC
	case mode == "CTS":
		t.mode = jceCTS
	case mode == "CTR":
		t.mode = jceCTR
	case strings.HasPrefix(mode, "CFB"), strings.HasPrefix(mode, "OFB"):
		t.mode, t.unit = jceCFB, aes.BlockSize
		if mode[0] == 'O' {
			t.mode = jceOFB
		}
		if bits := mode[3:]; bits != "" {
			// Only the canonical widths: SunJCE refuses CFB008 and CFB+8.
			n, err := strconv.Atoi(bits)
			if err != nil || strconv.Itoa(n) != bits || n < 8 || n > 128 || n%8 != 0 {
				return refuse()
			}
			t.unit = n / 8
		}
	default:
		return refuse()
	}
	if (t.mode == jceCTR || t.mode == jceCTS) && t.padding != jceNoPadding {
		return refuse()
	}
	if aesN && (t.padding != jceNoPadding || mode != "CBC" && mode != "CFB" && mode != "OFB") {
		return refuse()
	}
	return t, nil
}

// oidModes are the modes of the AES OIDs' sub-arcs 2 to 4.
var oidModes = [...]jceMode{2: jceCBC, 3: jceOFB, 4: jceCFB}

// aesOIDAlias reads tok, upper-cased, as an AES OID with or without "OID.":
// the key size its sub-arc names (0 for the arc itself) and the sub-arc's
// mode digit (1 ECB, 2 CBC, 3 OFB, 4 CFB, 0 for the arc itself).
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
	for size, base := range map[int]int{16: 0, 24: 20, 32: 40} {
		if d := n - base; d >= 1 && d <= 4 {
			return size, d, true
		}
	}
	return 0, 0, false
}

// newBlock is AES under key, which an AES_n name or an AES OID requires to be
// of its size (Java's InvalidKeyException).
func (t jceTransformation) newBlock(key []byte) (cipher.Block, error) {
	if t.keySize != 0 && len(key) != t.keySize {
		return nil, fmt.Errorf("The key must be %d bytes", t.keySize)
	}
	return aes.NewCipher(key)
}

// encrypt pads plain and enciphers it under iv. ISO 10126 padding draws its
// filler from random.
func (t jceTransformation) encrypt(block cipher.Block, iv, plain []byte, random io.Reader) ([]byte, error) {
	if t.padding != jceNoPadding {
		n := aes.BlockSize - len(plain)%aes.BlockSize
		padded := make([]byte, len(plain)+n)
		copy(padded, plain)
		pad := padded[len(plain):]
		if t.padding == jceISO10126Padding {
			if _, err := io.ReadFull(random, pad[:n-1]); err != nil {
				return nil, err
			}
		} else {
			for i := range pad {
				pad[i] = byte(n)
			}
		}
		pad[n-1] = byte(n)
		plain = padded
	} else if err := t.checkLength(len(plain)); err != nil {
		return nil, err
	}
	return t.crypt(block, iv, plain, false), nil
}

// decrypt deciphers text under iv and removes its padding.
func (t jceTransformation) decrypt(block cipher.Block, iv, text []byte) ([]byte, error) {
	if t.padding != jceNoPadding && len(text)%aes.BlockSize != 0 {
		return nil, errors.New("Input length must be multiple of 16 when decrypting with padded cipher")
	}
	if err := t.checkLength(len(text)); err != nil {
		return nil, err
	}
	plain := t.crypt(block, iv, text, true)
	// Java deciphers no input to no output, padded or not.
	if t.padding == jceNoPadding || len(plain) == 0 {
		return plain, nil
	}
	n := int(plain[len(plain)-1])
	if n == 0 || n > aes.BlockSize || n > len(plain) {
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

func (t jceTransformation) crypt(block cipher.Block, iv, src []byte, decrypt bool) []byte {
	out := make([]byte, len(src))
	switch t.mode {
	case jceCBC:
		if decrypt {
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, src)
		} else {
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, src)
		}
	case jcePCBC:
		pcbc(block, iv, out, src, decrypt)
	case jceCTS:
		cts(block, iv, out, src, decrypt)
	case jceCTR:
		cipher.NewCTR(block, iv).XORKeyStream(out, src)
	case jceCFB, jceOFB:
		feedback(block, iv, out, src, t.unit, t.mode == jceOFB, decrypt)
	}
	return out
}

// pcbc is propagating CBC: each block is chained with the previous block's
// plain text XOR its cipher text, the first with the IV.
func pcbc(block cipher.Block, iv, out, src []byte, decrypt bool) {
	chain := append([]byte(nil), iv...)
	buf := make([]byte, aes.BlockSize)
	for i := 0; i < len(src); i += aes.BlockSize {
		in, dst := src[i:i+aes.BlockSize], out[i:i+aes.BlockSize]
		if decrypt {
			block.Decrypt(buf, in)
			for j := range buf {
				dst[j] = buf[j] ^ chain[j]
				chain[j] = dst[j] ^ in[j]
			}
		} else {
			for j := range buf {
				buf[j] = in[j] ^ chain[j]
			}
			block.Encrypt(dst, buf)
			for j := range chain {
				chain[j] = in[j] ^ dst[j]
			}
		}
	}
}

// cts is CBC-CS3 (SP 800-38A addendum): CBC over the input padded with zeros
// to whole blocks, the last two blocks swapped whenever there are two, aligned
// or not, and the then-last truncated to the final partial block's length.
// One block is plain CBC.
func cts(block cipher.Block, iv, out, src []byte, decrypt bool) {
	n := len(src)
	if n <= aes.BlockSize {
		if decrypt {
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, src)
		} else {
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, src)
		}
		return
	}
	full := (n - 1) / aes.BlockSize * aes.BlockSize // whole blocks before the last (partial or whole) one
	head := full - aes.BlockSize                    // blocks chained as plain CBC
	d := n - full                                   // the last block's length, 1 to 16
	chain := iv
	if head > 0 {
		chain = src[head-aes.BlockSize : head]
		if decrypt {
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(out[:head], src[:head])
		} else {
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[:head], src[:head])
			chain = out[head-aes.BlockSize : head]
		}
	}
	buf := make([]byte, aes.BlockSize)
	if decrypt {
		// src ends with C_k (whole) then the leading d bytes of C_(k-1).
		block.Decrypt(buf, src[head:full])
		prev := make([]byte, aes.BlockSize) // C_(k-1): its leading d bytes, then the tail of buf
		copy(prev, src[full:])
		copy(prev[d:], buf[d:])
		for j := 0; j < d; j++ {
			out[full+j] = buf[j] ^ src[full+j]
		}
		block.Decrypt(buf, prev)
		for j := range buf {
			out[head+j] = buf[j] ^ chain[j]
		}
		return
	}
	prev := make([]byte, aes.BlockSize) // C_(k-1) = E(P_(k-1) XOR chain)
	for j := range buf {
		buf[j] = src[head+j] ^ chain[j]
	}
	block.Encrypt(prev, buf)
	clear(buf)
	copy(buf, src[full:])
	for j := range buf {
		buf[j] ^= prev[j]
	}
	block.Encrypt(out[head:full], buf)
	copy(out[full:], prev[:d])
}

// feedback is CFB or OFB with a unit of unit bytes: each unit is XORed with
// the leading unit of the enciphered register, which then shifts that unit
// out and takes in the cipher text (CFB) or the cipher output (OFB). A final
// partial unit uses as much of the output as it needs.
func feedback(block cipher.Block, iv, out, src []byte, unit int, ofb, decrypt bool) {
	reg := append([]byte(nil), iv...)
	o := make([]byte, aes.BlockSize)
	for i := 0; i < len(src); i += unit {
		block.Encrypt(o, reg)
		end := min(i+unit, len(src))
		for j := i; j < end; j++ {
			out[j] = src[j] ^ o[j-i]
		}
		next := o[:unit]
		if !ofb {
			if decrypt {
				next = src[i:end]
			} else {
				next = out[i:end]
			}
		}
		if len(next) < unit {
			break
		}
		copy(reg, reg[unit:])
		copy(reg[aes.BlockSize-unit:], next)
	}
}
