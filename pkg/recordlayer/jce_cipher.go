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
// transformations only those whose mode takes a 16-byte IV work: CBC and PCBC
// (block modes), CTR, and CFB and OFB with a feedback unit of 8 to 128 bits.
// ECB refuses an IV, GCM a plain IvParameterSpec, and KW and KWP a 16-byte one.
// The modes are NIST SP 800-38A's; OFB with a unit below the block is FIPS 81's,
// whose register takes the cipher output's leading unit.

type jceMode int

const (
	jceCBC jceMode = iota
	jcePCBC
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
	keySize int // the AES_128, AES_192 and AES_256 forms' one key size in bytes; 0 for any
	mode    jceMode
	unit    int // CFB and OFB: the feedback unit in bytes
	padding jcePadding
}

// parseJCETransformation reads "algorithm/mode/padding" as Cipher.getInstance
// tokenizes it: "/"-separated tokens, empty ones skipped, each trimmed, matched
// case-insensitively. A bare algorithm is the provider's ECB default, which
// takes no IV. SunJCE registers the AES_128, AES_192 and AES_256 forms with
// NoPadding only.
func parseJCETransformation(name string) (jceTransformation, error) {
	var tokens []string
	for _, tok := range strings.Split(name, "/") {
		if tok != "" {
			tokens = append(tokens, strings.ToUpper(strings.TrimSpace(tok)))
		}
	}
	refuse := func() (jceTransformation, error) {
		return jceTransformation{}, fmt.Errorf("unsupported cipher %s: not an AES transformation the JCE runs under a 16-byte IV", name)
	}
	if len(tokens) != 3 {
		return refuse()
	}
	var t jceTransformation
	switch tokens[0] {
	case "AES", "RIJNDAEL":
	case "AES_128":
		t.keySize = 16
	case "AES_192":
		t.keySize = 24
	case "AES_256":
		t.keySize = 32
	default:
		return refuse()
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
	case mode == "CBC":
		t.mode = jceCBC
	case mode == "PCBC":
		t.mode = jcePCBC
	case mode == "CTR":
		t.mode = jceCTR
	case strings.HasPrefix(mode, "CFB"), strings.HasPrefix(mode, "OFB"):
		t.mode, t.unit = jceCFB, aes.BlockSize
		if mode[0] == 'O' {
			t.mode = jceOFB
		}
		if bits := mode[3:]; bits != "" {
			n, err := strconv.Atoi(bits)
			if err != nil || n < 8 || n > 128 || n%8 != 0 {
				return refuse()
			}
			t.unit = n / 8
		}
	default:
		return refuse()
	}
	if t.mode == jceCTR && t.padding != jceNoPadding {
		return refuse()
	}
	if t.keySize != 0 && (t.padding != jceNoPadding || mode != "CBC" && mode != "CFB" && mode != "OFB") {
		return refuse()
	}
	return t, nil
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
	} else if t.blockMode() && len(plain)%aes.BlockSize != 0 {
		return nil, errors.New("Input length not multiple of 16 bytes")
	}
	return t.crypt(block, iv, plain, false), nil
}

// decrypt deciphers text under iv and removes its padding.
func (t jceTransformation) decrypt(block cipher.Block, iv, text []byte) ([]byte, error) {
	if (t.padding != jceNoPadding || t.blockMode()) && len(text)%aes.BlockSize != 0 {
		return nil, errors.New("Input length not multiple of 16 bytes")
	}
	plain := t.crypt(block, iv, text, true)
	if t.padding == jceNoPadding {
		return plain, nil
	}
	if len(plain) == 0 {
		return nil, errors.New("bad padding")
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

// blockMode says the mode enciphers whole blocks only.
func (t jceTransformation) blockMode() bool { return t.mode == jceCBC || t.mode == jcePCBC }

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
