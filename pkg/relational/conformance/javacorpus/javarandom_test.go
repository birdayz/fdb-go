package javacorpus

import "testing"

// The vectors below were produced by a real JVM (java.util.Random on the JDK
// this repo's Java toolchain pins) and are committed because the conclusion
// they justify — "the Go port of java.util.Random is bit-identical" — outlives
// the run that measured it.
//
// They matter because `!randomStr seed N length M` is not "some deterministic
// string": RandomStringParser defines it as the string java.util.Random(N)
// produces over the alphabet a-z0-9. A different PRNG would be perfectly
// deterministic and still disagree with every expectation the corpus records,
// so the algorithm IS part of the wire of this format.
//
// Independent corroboration that these are the right vectors: the corpus file
// large-record-fails.yamsql inserts `!randomStr seed 123234 length …`, and the
// query the runner actually issues begins with exactly
// "90m4egmlynfs3m33lskzb9jnkdjkdot6quoy64ch" — the JVM's answer for
// seed 123234, below.
func TestJavaRandomMatchesJVM(t *testing.T) {
	t.Parallel()

	strings := []struct {
		seed int64
		len  int
		want string
	}{
		{0, 32, "yqnl939vpcfr3k96epfu5auxi2p8a933"},
		{42, 32, "0daiszfch3c0yai6ygtr6507xj8wt4wm"},
		{123234, 40, "90m4egmlynfs3m33lskzb9jnkdjkdot6quoy64ch"},
	}
	for _, tc := range strings {
		if got := randomString(tc.seed, tc.len); got != tc.want {
			t.Errorf("randomString(%d, %d)\n got: %q\nwant: %q", tc.seed, tc.len, got, tc.want)
		}
	}

	// nextInt has two code paths and they draw DIFFERENT values: a power-of-two
	// bound takes a multiply-shift shortcut, everything else takes a modulo
	// with a rejection loop. Both are covered, because implementing only the
	// general path would still look right for bound 10 and be wrong for 16.
	t.Run("nextInt", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			seed  int64
			bound int32
			want  []int32
		}{
			{12345, 16, []int32{5, 8, 14, 14, 13, 0, 5, 1}},
			{7, 10, []int32{6, 4, 5, 4, 0, 4, 8, 9}},
		}
		for _, tc := range cases {
			r := newJavaRandom(tc.seed)
			for i, want := range tc.want {
				if got := r.nextInt(tc.bound); got != want {
					t.Errorf("Random(%d).nextInt(%d) draw %d = %d, want %d",
						tc.seed, tc.bound, i, got, want)
				}
			}
		}
	})
}

// TestExpandRandomStr pins the descriptor grammar, including the
// case-insensitivity RandomStringParser's pattern carries and the rejection of
// a malformed descriptor.
func TestExpandRandomStr(t *testing.T) {
	t.Parallel()

	got, err := expandRandomStr("seed 42 length 32")
	if err != nil {
		t.Fatalf("expandRandomStr: %v", err)
	}
	if want := "0daiszfch3c0yai6ygtr6507xj8wt4wm"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if upper, err := expandRandomStr("SEED 42 LENGTH 32"); err != nil || upper != got {
		t.Errorf("descriptor keywords are case-insensitive in Java; got %q, %v", upper, err)
	}
	for _, bad := range []string{"seed 42", "length 32 seed 42", "seed x length 3", ""} {
		if _, err := expandRandomStr(bad); err == nil {
			t.Errorf("expandRandomStr(%q) should be rejected", bad)
		}
	}
}
