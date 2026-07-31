package javacorpus

// javaRandom is java.util.Random's linear congruential generator.
//
// It is here because the corpus's `!randomStr seed N length M` tag is defined
// by RandomStringParser.generate as "the string java.util.Random(N) produces",
// not as "some deterministic string". A different PRNG would be deterministic
// and still disagree with every expectation the corpus records, so the
// algorithm itself is part of the format.
type javaRandom struct {
	seed int64
}

const (
	javaMultiplier = 0x5DEECE66D
	javaAddend     = 0xB
	javaMask       = (int64(1) << 48) - 1
)

func newJavaRandom(seed int64) *javaRandom {
	return &javaRandom{seed: (seed ^ javaMultiplier) & javaMask}
}

func (r *javaRandom) next(bits uint) int32 {
	r.seed = (r.seed*javaMultiplier + javaAddend) & javaMask
	return int32(r.seed >> (48 - bits))
}

// nextInt mirrors java.util.Random.nextInt(int bound), rejection loop included.
// The power-of-two fast path is not an optimisation here: it draws a different
// value than the modulo path would, so omitting it would desynchronise the
// stream for those bounds.
func (r *javaRandom) nextInt(bound int32) int32 {
	if bound&-bound == bound {
		return int32((int64(bound) * int64(r.next(31))) >> 31)
	}
	for {
		bits := r.next(31)
		val := bits % bound
		if bits-val+(bound-1) >= 0 {
			return val
		}
	}
}

// randomStrAlphabet is RandomStringParser.ALPHABET.
const randomStrAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// randomString reproduces RandomStringParser.generate(seed, length).
func randomString(seed int64, length int) string {
	r := newJavaRandom(seed)
	out := make([]byte, length)
	for i := range out {
		out[i] = randomStrAlphabet[r.nextInt(int32(len(randomStrAlphabet)))]
	}
	return string(out)
}
