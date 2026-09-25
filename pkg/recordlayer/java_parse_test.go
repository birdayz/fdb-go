package recordlayer

import (
	"errors"
	"math"
	"strings"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

// The expected values below were printed by JDK 21's Integer.parseInt and
// Boolean.parseBoolean and Guava 33.7.1's Hashing.murmur3_32_fixed()
// .hashBytes(bytes).asInt(); the conformance suite checks the same parsers
// through Java's own option checks ("Index option changes in meta-data
// evolution") and the hash through byte-identical ranked sets ("RANK ranked
// set per hash function").

func TestJavaParseIntMatchesIntegerParseInt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want int32
		ok   bool
	}{
		{"", 0, false},
		{"+", 0, false},
		{"-", 0, false},
		{"12", 12, true},
		{"+12", 12, true},
		{"-12", -12, true},
		{"2147483647", 2147483647, true},
		{"-2147483648", -2147483648, true},
		{"2147483648", 0, false},
		{"-2147483649", 0, false},
		{" 1", 0, false},
		{"1 ", 0, false},
		{"\u0663", 3, true},        // ARABIC-INDIC DIGIT THREE
		{"\uff11\uff12", 12, true}, // FULLWIDTH DIGITS
		{"0x10", 0, false},
		{"1_0", 0, false},
		{"\U0001D7CF", 0, false}, // MATHEMATICAL BOLD DIGIT ONE: two UTF-16 surrogates in Java
		{"007", 7, true},
		{"\u0661\u0660", 10, true},
		{"99999999999999999999", 0, false},
	} {
		got, err := javaParseInt(c.in)
		if c.ok {
			if err != nil || got != c.want {
				t.Errorf("javaParseInt(%q) = %d, %v; Java gives %d", c.in, got, err, c.want)
			}
			continue
		}
		var nf *NumberFormatError
		if !errors.As(err, &nf) {
			t.Errorf("javaParseInt(%q) = %d, %v; Java throws NumberFormatException", c.in, got, err)
			continue
		}
		if want := `For input string: "` + c.in + `"`; nf.Error() != want {
			t.Errorf("javaParseInt(%q) message %q, Java's is %q", c.in, nf.Error(), want)
		}
		// NumberFormatException extends IllegalArgumentException.
		var ia *IllegalArgumentError
		if !errors.As(err, &ia) || ia.Message != nf.Error() {
			t.Errorf("javaParseInt(%q): the NumberFormatError is not an IllegalArgumentError: %v", c.in, ia)
		}
	}
}

func TestJavaParseBooleanMatchesBooleanParseBoolean(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"true": true, "TRUE": true, "True": true, "tRuE": true,
		"yes": false, "1": false, "": false, " true": false, "false": false,
	} {
		if got := javaParseBoolean(in); got != want {
			t.Errorf("javaParseBoolean(%q) = %t; Java gives %t", in, got, want)
		}
	}
}

func TestMurmur3HashMatchesGuava(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int32{
		"":             0,
		"a":            1009084850,
		"hello":        613153351,
		"hello, world": 345750399,
		"The quick brown fox jumps over the lazy dog": 776992547,
		"abc":   -1277324294,
		"abcd":  1139631978,
		"abcde": -392455434,
		// Bytes above 0x7f: Guava's tail reads them unsigned.
		string([]byte{0xff, 0x80, 0x00, 0x7f, 0x01}): -1131820900,
	} {
		if got := murmur3Hash([]byte(in)); got != want {
			t.Errorf("murmur3Hash(%q) = %d; Guava gives %d", in, got, want)
		}
	}
}

// TestRankedSetConfigParsesAsJavaDoes pins parseRankedSetConfig to
// RankedSetIndexHelper.getConfig: the hash function by exact name among
// Java's four, the level count by Integer.parseInt and setNLevels' [2, 8],
// count-duplicates by Boolean.parseBoolean, and the first bad option, in
// Java's order, the error.
func TestRankedSetConfigParsesAsJavaDoes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		options map[string]string
		hash    string
		levels  int
		dups    bool
		errText string
	}{
		{"defaults", nil, "JDK", 6, false, ""},
		{"JDK", map[string]string{IndexOptionRankHashFunction: "JDK"}, "JDK", 6, false, ""},
		{"CRC", map[string]string{IndexOptionRankHashFunction: "CRC"}, "CRC", 6, false, ""},
		{"RANDOM", map[string]string{IndexOptionRankHashFunction: "RANDOM"}, "RANDOM", 6, false, ""},
		{"MURMUR3", map[string]string{IndexOptionRankHashFunction: "MURMUR3"}, "MURMUR3", 6, false, ""},
		{"a hash name in another case", map[string]string{IndexOptionRankHashFunction: "crc"}, "", 0, false, "hash function not found: crc"},
		{"an empty hash name", map[string]string{IndexOptionRankHashFunction: ""}, "", 0, false, "hash function not found: "},
		{"two levels", map[string]string{IndexOptionRankNLevels: "2"}, "JDK", 2, false, ""},
		{"eight levels", map[string]string{IndexOptionRankNLevels: "8"}, "JDK", 8, false, ""},
		{"one level", map[string]string{IndexOptionRankNLevels: "1"}, "", 0, false, "levels must be between 2 and 8"},
		{"nine levels", map[string]string{IndexOptionRankNLevels: "9"}, "", 0, false, "levels must be between 2 and 8"},
		{"levels not an int", map[string]string{IndexOptionRankNLevels: "six"}, "", 0, false, `For input string: "six"`},
		{"the hash is read first", map[string]string{IndexOptionRankHashFunction: "X", IndexOptionRankNLevels: "six"}, "", 0, false, "hash function not found: X"},
		{"duplicates TRUE", map[string]string{IndexOptionRankCountDuplicates: "TRUE"}, "JDK", 6, true, ""},
		{"duplicates yes", map[string]string{IndexOptionRankCountDuplicates: "yes"}, "JDK", 6, false, ""},
	} {
		config, err := parseRankedSetConfig(&Index{Name: "r", Type: IndexTypeRank, Options: c.options})
		if c.errText != "" {
			if err == nil || err.Error() != c.errText {
				t.Errorf("%s: err %v, want %q", c.name, err, c.errText)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if config.HashFunctionName != c.hash || config.NLevels != c.levels || config.CountDuplicates != c.dups {
			t.Errorf("%s: got hash %s levels %d dups %t, want %s %d %t", c.name, config.HashFunctionName, config.NLevels, config.CountDuplicates, c.hash, c.levels, c.dups)
		}
		if c.hash != rankedSetHashRandom {
			got, errGot := config.HashFunction([]byte("probe"))
			want, errWant := rankedSetHashFunctions[c.hash]([]byte("probe"))
			if errGot != nil || errWant != nil || got != want {
				t.Errorf("%s: the config's function is not %s's", c.name, c.hash)
			}
		}
	}
	// The unknown name is Java's RecordCoreArgumentException, the level range
	// its IllegalArgumentException.
	_, err := parseRankedSetConfig(&Index{Options: map[string]string{IndexOptionRankHashFunction: "SHA"}})
	var argErr *RecordCoreArgumentError
	if !errors.As(err, &argErr) {
		t.Errorf("unknown hash: %T, want *RecordCoreArgumentError", err)
	}
	_, err = parseRankedSetConfig(&Index{Options: map[string]string{IndexOptionRankNLevels: "0"}})
	var iaErr *IllegalArgumentError
	if !errors.As(err, &iaErr) {
		t.Errorf("level range: %T, want *IllegalArgumentError", err)
	}
}

// TestRTreeConfigParsesAsJavaDoes pins parseRTreeConfig to
// MultiDimensionalIndexHelper.getConfig, the Hilbert-values quirk included:
// the flag is read only with the storage option set, and then absent means
// false.
func TestRTreeConfigParsesAsJavaDoes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		options map[string]string
		want    RTreeConfig
		errText string
	}{
		{"defaults", nil, RTreeConfig{MinM: 16, MaxM: 32, SplitS: 2, Storage: RTreeStorageByNode, StoreHilbertValues: true}, ""},
		{"Hilbert false without storage keeps the default", map[string]string{IndexOptionRTreeStoreHilbertValues: "false"}, RTreeConfig{MinM: 16, MaxM: 32, SplitS: 2, Storage: RTreeStorageByNode, StoreHilbertValues: true}, ""},
		{"storage alone stores no Hilbert values", map[string]string{IndexOptionRTreeStorage: "BY_NODE"}, RTreeConfig{MinM: 16, MaxM: 32, SplitS: 2, Storage: RTreeStorageByNode, StoreHilbertValues: false}, ""},
		{"storage with Hilbert TRUE", map[string]string{IndexOptionRTreeStorage: "BY_SLOT", IndexOptionRTreeStoreHilbertValues: "TRUE"}, RTreeConfig{MinM: 16, MaxM: 32, SplitS: 2, Storage: RTreeStorageBySlot, StoreHilbertValues: true}, ""},
		{"node slot index True", map[string]string{IndexOptionRTreeUseNodeSlotIndex: "True"}, RTreeConfig{MinM: 16, MaxM: 32, SplitS: 2, Storage: RTreeStorageByNode, StoreHilbertValues: true, UseNodeSlotIndex: true}, ""},
		{"any int for M and S", map[string]string{IndexOptionRTreeMinM: "0", IndexOptionRTreeMaxM: "-3", IndexOptionRTreeSplitS: "+5"}, RTreeConfig{MinM: 0, MaxM: -3, SplitS: 5, Storage: RTreeStorageByNode, StoreHilbertValues: true}, ""},
		{"storage in lower case", map[string]string{IndexOptionRTreeStorage: "by_slot"}, RTreeConfig{}, "No enum constant com.apple.foundationdb.async.rtree.RTree.Storage.by_slot"},
		{"minM not an int", map[string]string{IndexOptionRTreeMinM: "16.0"}, RTreeConfig{}, `For input string: "16.0"`},
		{"minM is read before storage", map[string]string{IndexOptionRTreeMinM: "x", IndexOptionRTreeStorage: "nope"}, RTreeConfig{}, `For input string: "x"`},
	} {
		got, err := parseRTreeConfig(&Index{Name: "md", Type: IndexTypeMultidimensional, Options: c.options}, 0)
		if c.errText != "" {
			if err == nil || err.Error() != c.errText {
				t.Errorf("%s: err %v, want %q", c.name, err, c.errText)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
}

// TestPermutedSizeIsReadAsJavaReadsIt pins the permutedSize option to
// PermutedMinMaxIndexMaintainer.getPermutedSize and the validator of
// PermutedMinMaxIndexMaintainerFactory (:66-80): Build refuses what Java's
// validation refuses, with Java's text and class, and an accepted size is the
// size the maintainer writes at, a digit Integer.parseInt reads included.
func TestPermutedSizeIsReadAsJavaReadsIt(t *testing.T) {
	t.Parallel()
	grouped := GroupBy(Field("price"), Concat(Field("order_id"), Field("quantity")))
	for _, c := range []struct {
		name    string
		root    KeyExpression
		options map[string]string
		size    int
		errText string
		// class is Java's: KeyExpression.InvalidExpressionException ("key"),
		// NumberFormatException ("number") or MetaDataException ("meta").
		class string
	}{
		{"a decimal size", grouped, map[string]string{IndexOptionPermutedSize: "1"}, 1, "", ""},
		{"a signed size", grouped, map[string]string{IndexOptionPermutedSize: "+2"}, 2, "", ""},
		{"an Arabic-Indic digit", grouped, map[string]string{IndexOptionPermutedSize: "\u0662"}, 2, "", ""},
		{"zero", grouped, map[string]string{IndexOptionPermutedSize: "0"}, 0, "", ""},
		{"absent", grouped, nil, 0, "permuted size not specified", "meta"},
		{"not an int", grouped, map[string]string{IndexOptionPermutedSize: "1.0"}, 0, `For input string: "1.0"`, "number"},
		{"negative", grouped, map[string]string{IndexOptionPermutedSize: "-1"}, 0, "permuted size cannot be negative", "meta"},
		{"past the grouping", grouped, map[string]string{IndexOptionPermutedSize: "3"}, 0, "permuted size cannot be larger than grouping size", "meta"},
		{"no grouping", Concat(Field("order_id"), Field("price")), map[string]string{IndexOptionPermutedSize: "1"}, 0, "index type requires grouping", "key"},
		{"nothing grouped", GroupBy(EmptyKey(), Concat(Field("order_id"), Field("price"))), map[string]string{IndexOptionPermutedSize: "1"}, 0, "index type requires grouping at least 1 fields", "key"},
		{"grouping read before the size", Concat(Field("order_id"), Field("price")), nil, 0, "index type requires grouping", "key"},
	} {
		for _, typ := range []string{IndexTypePermutedMin, IndexTypePermutedMax} {
			idx := &Index{Name: "perm", Type: typ, RootExpression: c.root, Options: c.options}
			md, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) { b.AddIndex("Order", idx) })
			if c.errText != "" {
				var keyErr *KeyExpressionError
				var mdErr *MetaDataError
				var nfErr *NumberFormatError
				classOK := errors.As(err, &keyErr) == (c.class == "key") &&
					errors.As(err, &nfErr) == (c.class == "number") &&
					errors.As(err, &mdErr) == (c.class == "meta")
				if err == nil || err.Error() != c.errText || !classOK {
					t.Errorf("%s %s: Build = %v (%T), want %q", c.name, typ, err, err, c.errText)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s %s: Build = %v", c.name, typ, err)
				continue
			}
			m, err := newPermutedMinMaxIndexMaintainer(md.GetIndex("perm"), subspace.FromBytes([]byte("i")), subspace.FromBytes([]byte("s")), nil, nil, typ == IndexTypePermutedMax)
			if err != nil || m.permutedSize != c.size {
				t.Errorf("%s %s: maintainer size %v, %v; want %d", c.name, typ, m, err, c.size)
			}
		}
	}
	// The maintainer refuses what Java's constructor refuses, for an index
	// changed after Build.
	idx := &Index{Name: "perm", Type: IndexTypePermutedMax, RootExpression: grouped, Options: map[string]string{IndexOptionPermutedSize: "x"}}
	if _, err := newPermutedMinMaxIndexMaintainer(idx, subspace.FromBytes([]byte("i")), subspace.FromBytes([]byte("s")), nil, nil, true); err == nil || err.Error() != `For input string: "x"` {
		t.Errorf("maintainer of an unparsable size: %v", err)
	}
}

// TestRankedSetHashFunctionsAreJavas pins each name of the rankHashFunction
// option to the function Java's RankedSetHashFunctions maps it to, by its
// value on one input as JDK 21 and Guava compute it (Arrays.hashCode, CRC32,
// murmur3_32_fixed); RANDOM is the random function, by identity.
func TestRankedSetHashFunctionsAreJavas(t *testing.T) {
	t.Parallel()
	in := []byte("hello")
	for name, want := range map[string]int32{
		rankedSetHashJDK:     127791473,
		rankedSetHashCRC:     907060870,
		rankedSetHashMurmur3: 613153351,
	} {
		if got, err := rankedSetHashFunctions[name](in); err != nil || got != want {
			t.Errorf("%s(%q) = %d, %v; Java gives %d", name, in, got, err, want)
		}
	}
	// RANDOM draws a new int per call whatever the key.
	random, draws := rankedSetHashFunctions[rankedSetHashRandom], map[int32]bool{}
	for range 16 {
		h, err := random(in)
		if err != nil {
			t.Fatal(err)
		}
		draws[h] = true
	}
	if len(draws) < 2 {
		t.Error("RANDOM gave one value for 16 calls")
	}
	if len(rankedSetHashFunctions) != 4 {
		t.Errorf("%d hash functions; Java knows four", len(rankedSetHashFunctions))
	}
}

// TestRandomRankHashReplaysUnderASeededEnv pins that a RANDOM ranked set's
// draws go through the DST seam: two configurations bound to envs of one seed
// draw the same sequence, so a seeded simulation writes the same ranked-set
// bytes on replay.
func TestRandomRankHashReplaysUnderASeededEnv(t *testing.T) {
	t.Parallel()
	config, err := parseRankedSetConfig(&Index{Options: map[string]string{IndexOptionRankHashFunction: "RANDOM"}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := config.withEnv(dst.NewSim(42)), config.withEnv(dst.NewSim(42))
	for i := range 16 {
		x, errX := a.HashFunction(nil)
		y, errY := b.HashFunction(nil)
		if errX != nil || errY != nil || x != y {
			t.Fatalf("draw %d: %d (%v) and %d (%v) under one seed", i, x, errX, y, errY)
		}
	}
	// A pure hash is left as it is.
	jdk := defaultRankedSetConfig.withEnv(dst.NewSim(42))
	if h, err := jdk.HashFunction([]byte("hello")); err != nil || h != 127791473 {
		t.Error("withEnv changed the JDK hash")
	}
}

// failingRandomness is a randomness source whose every read fails.
type failingRandomness struct{}

func (failingRandomness) Read([]byte) (int, error) { return 0, errors.New("entropy exhausted") }

// TestRandomRankHashFailsTheWriteOnAFailedRead pins that a RANDOM draw whose
// read fails fails the insert before anything is written: a zero hash would
// put the key on every level.
func TestRandomRankHashFailsTheWriteOnAFailedRead(t *testing.T) {
	t.Parallel()
	config, err := parseRankedSetConfig(&Index{Options: map[string]string{IndexOptionRankHashFunction: "RANDOM"}})
	if err != nil {
		t.Fatal(err)
	}
	env := &dst.Env{Random: failingRandomness{}}
	rs := newRankedSet(subspace.FromBytes([]byte("rs")), config.withEnv(env))
	// A nil transaction: the draw comes before the first read or write, so an
	// Add that goes on past a failed draw reaches the transaction and panics.
	// That is reported as this test's failure, not left to end the binary.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Add went on past the failed read to the transaction: %v", r)
		}
	}()
	if _, err := rs.Add(nil, []byte("key")); err == nil || !strings.Contains(err.Error(), "entropy exhausted") {
		t.Fatalf("Add = %v, want the failed read", err)
	}
}

// javaParseDouble accepts what Java's Double.parseDouble accepts and refuses
// the rest with Java's text (FloatingDecimal.readJavaFormatString).
func TestJavaParseDouble(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want float64
	}{
		{"0.5", 0.5},
		{" 0.5 ", 0.5},
		{"\t1\n", 1},
		{"1.5d", 1.5},
		{"1.5F", 1.5},
		{"+2", 2},
		{"-2.", -2},
		{".5", 0.5},
		{"1e3", 1000},
		{"1E-2", 0.01},
		{"1e+2", 100},
		{"0x1.8p1", 3},
		{"0X10P0d", 16},
		{"Infinity", math.Inf(1)},
		{"-Infinity", math.Inf(-1)},
		{"+Infinity", math.Inf(1)},
		{"1e400", math.Inf(1)},
		{"-1e400", math.Inf(-1)},
		{"1e-400", 0},
	} {
		got, err := javaParseDouble(c.in)
		if err != nil || got != c.want {
			t.Errorf("javaParseDouble(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"NaN", "-NaN", "+NaN", " NaN "} {
		if got, err := javaParseDouble(in); err != nil || !math.IsNaN(got) {
			t.Errorf("javaParseDouble(%q) = %v, %v; want NaN", in, got, err)
		}
	}
	for _, c := range []struct{ in, text string }{
		{"", "empty String"},
		{"   ", "empty String"},
		{"inf", `For input string: "inf"`},
		{"nan", `For input string: "nan"`},
		{"infinity", `For input string: "infinity"`},
		{"1_000", `For input string: "1_000"`},
		{".", `For input string: "."`},
		{"1e", `For input string: "1e"`},
		{"0x1.8", `For input string: "0x1.8"`},
		{"1.5dd", `For input string: "1.5dd"`},
		{" x ", `For input string: "x"`},
		{"１", `For input string: "１"`},
		{"NaNd", `For input string: "NaNd"`},
	} {
		_, err := javaParseDouble(c.in)
		var nfe *NumberFormatError
		if !errors.As(err, &nfe) || err.Error() != c.text {
			t.Errorf("javaParseDouble(%q) err = %v, want NumberFormatError %q", c.in, err, c.text)
		}
	}
}
