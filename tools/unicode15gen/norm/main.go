// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Normalization table generator.
// Data read from the web.
// See forminfo.go for a description of the trie values associated with each rune.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"fdb.dev/tools/unicode15gen/internal/gen"
	"fdb.dev/tools/unicode15gen/internal/ucd"
)

func main() {
	gen.Init()
	loadUnicodeData()
	compactCCC()
	loadCompositionExclusions()
	completeCharFields(FCanonical)
	completeCharFields(FCompatibility)
	computeNonStarterCounts()
	verifyComputed()
	printChars()
	testDerived()
	printTestdata()
	makeTables()
}

var (
	tablelist = flag.String("tables",
		"all",
		"comma-separated list of which tables to generate; "+
			"can be 'decomp', 'recomp', 'info' and 'all'")
	test = flag.Bool("test",
		false,
		"test existing tables against DerivedNormalizationProps and generate test data for regression testing")
	verbose = flag.Bool("verbose",
		false,
		"write data to stdout as it is parsed")
)

const MaxChar = 0x10FFFF // anything above this shouldn't exist

// Quick Check properties of runes allow us to quickly
// determine whether a rune may occur in a normal form.
// For a given normal form, a rune may be guaranteed to occur
// verbatim (QC=Yes), may or may not combine with another
// rune (QC=Maybe), or may not occur (QC=No).
type QCResult int

const (
	QCUnknown QCResult = iota
	QCYes
	QCNo
	QCMaybe
)

func (r QCResult) String() string {
	switch r {
	case QCYes:
		return "Yes"
	case QCNo:
		return "No"
	case QCMaybe:
		return "Maybe"
	}
	return "***UNKNOWN***"
}

const (
	FCanonical     = iota // NFC or NFD
	FCompatibility        // NFKC or NFKD
	FNumberOfFormTypes
)

const (
	MComposed   = iota // NFC or NFKC
	MDecomposed        // NFD or NFKD
	MNumberOfModes
)

// This contains only the properties we're interested in.
type Char struct {
	name          string
	codePoint     rune  // if zero, this index is not a valid code point.
	ccc           uint8 // canonical combining class
	origCCC       uint8
	excludeInComp bool // from CompositionExclusions.txt
	compatDecomp  bool // it has a compatibility expansion

	nTrailingNonStarters uint8
	nLeadingNonStarters  uint8 // must be equal to trailing if non-zero

	forms [FNumberOfFormTypes]FormInfo // For FCanonical and FCompatibility

	state State
}

var (
	chars  = make([]Char, MaxChar+1)
	cccMap = make(map[uint8]uint8)
)

func (c Char) String() string {
	buf := new(bytes.Buffer)

	fmt.Fprintf(buf, "%U [%s]:\n", c.codePoint, c.name)
	fmt.Fprintf(buf, "  ccc: %v\n", c.ccc)
	fmt.Fprintf(buf, "  excludeInComp: %v\n", c.excludeInComp)
	fmt.Fprintf(buf, "  compatDecomp: %v\n", c.compatDecomp)
	fmt.Fprintf(buf, "  state: %v\n", c.state)
	fmt.Fprintf(buf, "  NFC:\n")
	fmt.Fprint(buf, c.forms[FCanonical])
	fmt.Fprintf(buf, "  NFKC:\n")
	fmt.Fprint(buf, c.forms[FCompatibility])

	return buf.String()
}

// In UnicodeData.txt, some ranges are marked like this:
//
//	3400;<CJK Ideograph Extension A, First>;Lo;0;L;;;;;N;;;;;
//	4DB5;<CJK Ideograph Extension A, Last>;Lo;0;L;;;;;N;;;;;
//
// parseCharacter keeps a state variable indicating the weirdness.
type State int

const (
	SNormal State = iota // known to be zero for the type
	SFirst
	SLast
	SMissing
)

var lastChar = rune('\u0000')

func (c Char) isValid() bool {
	return c.codePoint != 0 && c.state != SMissing
}

type FormInfo struct {
	quickCheck [MNumberOfModes]QCResult // index: MComposed or MDecomposed
	verified   [MNumberOfModes]bool     // index: MComposed or MDecomposed

	combinesForward  bool // May combine with rune on the right
	combinesBackward bool // May combine with rune on the left
	isOneWay         bool // Never appears in result
	inDecomp         bool // Some decompositions result in this char.
	suffixDecomp     bool // Appears after first rune of a decomposition
	decomp           Decomposition
	expandedDecomp   Decomposition
}

func (f FormInfo) String() string {
	buf := bytes.NewBuffer(make([]byte, 0))

	fmt.Fprintf(buf, "    quickCheck[C]: %v\n", f.quickCheck[MComposed])
	fmt.Fprintf(buf, "    quickCheck[D]: %v\n", f.quickCheck[MDecomposed])
	fmt.Fprintf(buf, "    cmbForward: %v\n", f.combinesForward)
	fmt.Fprintf(buf, "    cmbBackward: %v\n", f.combinesBackward)
	fmt.Fprintf(buf, "    isOneWay: %v\n", f.isOneWay)
	fmt.Fprintf(buf, "    inDecomp: %v\n", f.inDecomp)
	fmt.Fprintf(buf, "    decomposition: %X\n", f.decomp)
	fmt.Fprintf(buf, "    expandedDecomp: %X\n", f.expandedDecomp)

	return buf.String()
}

type Decomposition []rune

func parseDecomposition(s string, skipfirst bool) (a []rune, err error) {
	decomp := strings.Split(s, " ")
	if len(decomp) > 0 && skipfirst {
		decomp = decomp[1:]
	}
	for _, d := range decomp {
		point, err := strconv.ParseUint(d, 16, 64)
		if err != nil {
			return a, err
		}
		a = append(a, rune(point))
	}
	return a, nil
}

func loadUnicodeData() {
	f := gen.OpenUCDFile("UnicodeData.txt")
	defer f.Close()
	p := ucd.New(f)
	for p.Next() {
		r := p.Rune(ucd.CodePoint)
		char := &chars[r]

		char.ccc = uint8(p.Uint(ucd.CanonicalCombiningClass))
		decmap := p.String(ucd.DecompMapping)

		exp, err := parseDecomposition(decmap, false)
		isCompat := false
		if err != nil {
			if len(decmap) > 0 {
				exp, err = parseDecomposition(decmap, true)
				if err != nil {
					log.Fatalf(`%U: bad decomp |%v|: "%s"`, r, decmap, err)
				}
				isCompat = true
			}
		}

		char.name = p.String(ucd.Name)
		char.codePoint = r
		char.forms[FCompatibility].decomp = exp
		if !isCompat {
			char.forms[FCanonical].decomp = exp
		} else {
			char.compatDecomp = true
		}
		if len(decmap) > 0 {
			char.forms[FCompatibility].decomp = exp
		}
	}
	if err := p.Err(); err != nil {
		log.Fatal(err)
	}
}

// compactCCC converts the sparse set of CCC values to a contiguous one,
// reducing the number of bits needed from 8 to 6.
func compactCCC() {
	m := make(map[uint8]uint8)
	for i := range chars {
		c := &chars[i]
		m[c.ccc] = 0
	}
	cccs := []int{}
	for v := range m {
		cccs = append(cccs, int(v))
	}
	sort.Ints(cccs)
	for i, c := range cccs {
		cccMap[uint8(i)] = uint8(c)
		m[uint8(c)] = uint8(i)
	}
	for i := range chars {
		c := &chars[i]
		c.origCCC = c.ccc
		c.ccc = m[c.ccc]
	}
	if len(m) >= 1<<6 {
		log.Fatalf("too many difference CCC values: %d >= 64", len(m))
	}
}

// CompositionExclusions.txt has form:
// 0958    # ...
// See https://unicode.org/reports/tr44/ for full explanation
func loadCompositionExclusions() {
	f := gen.OpenUCDFile("CompositionExclusions.txt")
	defer f.Close()
	p := ucd.New(f)
	for p.Next() {
		c := &chars[p.Rune(0)]
		if c.excludeInComp {
			log.Fatalf("%U: Duplicate entry in exclusions.", c.codePoint)
		}
		c.excludeInComp = true
	}
	if e := p.Err(); e != nil {
		log.Fatal(e)
	}
}

// hasCompatDecomp returns true if any of the recursive
// decompositions contains a compatibility expansion.
// In this case, the character may not occur in NFK*.
func hasCompatDecomp(r rune) bool {
	c := &chars[r]
	if c.compatDecomp {
		return true
	}
	for _, d := range c.forms[FCompatibility].decomp {
		if hasCompatDecomp(d) {
			return true
		}
	}
	return false
}

// Hangul related constants.
const (
	HangulBase = 0xAC00
	HangulEnd  = 0xD7A4 // hangulBase + Jamo combinations (19 * 21 * 28)

	JamoLBase = 0x1100
	JamoLEnd  = 0x1113
	JamoVBase = 0x1161
	JamoVEnd  = 0x1176
	JamoTBase = 0x11A8
	JamoTEnd  = 0x11C3

	JamoLVTCount = 19 * 21 * 28
	JamoTCount   = 28
)

func isHangul(r rune) bool {
	return HangulBase <= r && r < HangulEnd
}

func isHangulWithoutJamoT(r rune) bool {
	if !isHangul(r) {
		return false
	}
	r -= HangulBase
	return r < JamoLVTCount && r%JamoTCount == 0
}

func ccc(r rune) uint8 {
	return chars[r].ccc
}

// Insert a rune in a buffer, ordered by Canonical Combining Class.
func insertOrdered(b Decomposition, r rune) Decomposition {
	n := len(b)
	b = append(b, 0)
	cc := ccc(r)
	if cc > 0 {
		// Use bubble sort.
		for ; n > 0; n-- {
			if ccc(b[n-1]) <= cc {
				break
			}
			b[n] = b[n-1]
		}
	}
	b[n] = r
	return b
}

// Recursively decompose.
func decomposeRecursive(form int, r rune, d Decomposition) Decomposition {
	dcomp := chars[r].forms[form].decomp
	if len(dcomp) == 0 {
		return insertOrdered(d, r)
	}
	for _, c := range dcomp {
		d = decomposeRecursive(form, c, d)
	}
	return d
}

func completeCharFields(form int) {
	// Phase 0: pre-expand decomposition.
	for i := range chars {
		f := &chars[i].forms[form]
		if len(f.decomp) == 0 {
			continue
		}
		exp := make(Decomposition, 0)
		for _, c := range f.decomp {
			exp = decomposeRecursive(form, c, exp)
		}
		f.expandedDecomp = exp
	}

	// Phase 1: composition exclusion, mark decomposition.
	for i := range chars {
		c := &chars[i]
		f := &c.forms[form]

		// Marks script-specific exclusions and version restricted.
		f.isOneWay = c.excludeInComp

		// Singletons
		f.isOneWay = f.isOneWay || len(f.decomp) == 1

		// Non-starter decompositions
		if len(f.decomp) > 1 {
			chk := c.ccc != 0 || chars[f.decomp[0]].ccc != 0
			f.isOneWay = f.isOneWay || chk
		}

		// Runes that decompose into more than two runes.
		f.isOneWay = f.isOneWay || len(f.decomp) > 2

		if form == FCompatibility {
			f.isOneWay = f.isOneWay || hasCompatDecomp(c.codePoint)
		}

		for i, r := range f.decomp {
			chars[r].forms[form].inDecomp = true
			if i > 0 {
				chars[r].forms[form].suffixDecomp = true
			}
		}
	}

	// Phase 2: forward and backward combining.
	for i := range chars {
		c := &chars[i]
		f := &c.forms[form]

		if !f.isOneWay && len(f.decomp) == 2 {
			f0 := &chars[f.decomp[0]].forms[form]
			f1 := &chars[f.decomp[1]].forms[form]
			if !f0.isOneWay {
				f0.combinesForward = true
			}
			if !f1.isOneWay {
				f1.combinesBackward = true
			}
		}
		if isHangulWithoutJamoT(rune(i)) {
			f.combinesForward = true
		}
		if (i & 0xffff00) == JamoLBase {
			if JamoLBase <= i && i < JamoLEnd {
				f.combinesForward = true
			}
			if JamoVBase <= i && i < JamoVEnd {
				f.combinesBackward = true
				f.combinesForward = true
			}
			if JamoTBase <= i && i < JamoTEnd {
				f.combinesBackward = true
			}
		}
	}

	// Phase 2½: backward combining propagation.
	for i := range chars {
		c := &chars[i]
		f := &c.forms[form]

		// If the first rune of f's decomposition combines backward,
		// then f itself must be considered to combine backward.
		// This handles the "MaybeNo" runes introduced in Unicode 16.
		// https://www.unicode.org/reports/tr15/tr15-56.html#Contexts_Care
		if !f.isOneWay && len(f.decomp) > 0 {
			f0 := &chars[f.decomp[0]].forms[form]
			if f0.combinesBackward {
				f.combinesBackward = true
			}
		}
	}

	// Phase 3: quick check values.
	for i := range chars {
		c := &chars[i]
		f := &c.forms[form]

		switch {
		case len(f.decomp) > 0:
			f.quickCheck[MDecomposed] = QCNo
		case isHangul(rune(i)):
			f.quickCheck[MDecomposed] = QCNo
		default:
			f.quickCheck[MDecomposed] = QCYes
		}
		switch {
		case f.isOneWay:
			f.quickCheck[MComposed] = QCNo
		case !f.combinesBackward:
			f.quickCheck[MComposed] = QCYes
		default:
			f.quickCheck[MComposed] = QCMaybe
		}
	}
}

func computeNonStarterCounts() {
	// Phase 4: leading and trailing non-starter count
	for i := range chars {
		c := &chars[i]

		runes := []rune{rune(i)}
		// We always use FCompatibility so that the CGJ insertion points do not
		// change for repeated normalizations with different forms.
		if exp := c.forms[FCompatibility].expandedDecomp; len(exp) > 0 {
			runes = exp
		}
		// We consider runes that combine backwards to be non-starters for the
		// purpose of Stream-Safe Text Processing.
		for _, r := range runes {
			if cr := &chars[r]; cr.ccc == 0 && !cr.forms[FCompatibility].combinesBackward {
				break
			}
			c.nLeadingNonStarters++
		}
		for i := len(runes) - 1; i >= 0; i-- {
			if cr := &chars[runes[i]]; cr.ccc == 0 && !cr.forms[FCompatibility].combinesBackward {
				break
			}
			c.nTrailingNonStarters++
		}
		if c.nTrailingNonStarters > 3 {
			log.Fatalf("%U: Decomposition with more than 3 (%d) trailing modifiers (%U)", i, c.nTrailingNonStarters, runes)
		}

		if isHangul(rune(i)) {
			c.nTrailingNonStarters = 2
			if isHangulWithoutJamoT(rune(i)) {
				c.nTrailingNonStarters = 1
			}
		}

		if l, t := c.nLeadingNonStarters, c.nTrailingNonStarters; l > 0 && l != t {
			log.Fatalf("%U: number of leading and trailing non-starters should be equal (%d vs %d)", i, l, t)
		}
		if t := c.nTrailingNonStarters; t > 3 {
			log.Fatalf("%U: number of trailing non-starters is %d > 3", i, t)
		}
	}
}
