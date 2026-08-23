package recordlayer

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// A PROTOC DIFFERENTIAL OVER THE PACKAGE-RELATIONSHIP SPACE, committed rather
// than run once and described in prose.
//
// The claim this pass rests on -- that the seeded walk agrees with protoc
// exactly -- was established by a throwaway differential and then cited in four
// places while the harness existed nowhere in the tree. That is the failure the
// "every proof gets committed as a test" rule names, and it had exactly the
// consequence the rule predicts: deleting the symbol seeding outright, which a
// differential scores at over a thousand divergences, left the whole package
// GREEN. The only instrument that could see the change lived in a comment.
//
// HOW IT STAYS HERMETIC. Running protoc at test time would make the result a
// statement about the machine, so ground truth is a committed golden and the
// normal run only reads it. Regenerate with:
//
//	go test ./pkg/recordlayer -run TestAbsolutizeAgreesWithProtoc -update-protoc-golden
//
// which renders every case as .proto source, compiles each with protoc, records
// what protoc resolved the probed reference to, and rewrites the golden.
//
// WHAT IT DOES NOT COVER, written first because a positive claim about a corpus
// is what goes stale:
//
//   - The root-vs-package FALLBACK. Every case here has a COMPLETE dependency
//     set, which is the condition for protoc accepting at all -- and with
//     complete dependencies the walk always finds the first component, so the
//     fallback is unreachable and this corpus is silent about it. Covered by
//     TestAbsolutizeFieldTypeNamesFallsBackToRootNotToThePackage.
//   - The EXCLUDED-dependency path, where imports arrive through the global
//     registry rather than the stored dependency list. Covered by
//     TestAbsolutizeFieldTypeNamesSeesGloballyRegisteredImports.
//   - Positions where Go deliberately follows Java AGAINST protoc. The
//     field-shadowed extendee is one; protoc rejects it, so it never enters the
//     accepted set. Covered by TestExtendeeSkipsAFieldShadowAsJavaDoes.
//   - Services, which no case here declares. Covered by
//     TestAbsolutizeFieldTypeNamesTreatsAServiceAsAnAggregate.
//
// So the agreement number below is about ONE axis -- package relationship
// crossed with write position -- and that is the axis the hand-written arms
// cover thinnest.
var updateProtocGolden = flag.Bool("update-protoc-golden", false,
	"regenerate testdata/absolutize_protoc_golden.txt by running protoc")

const protocGoldenPath = "testdata/absolutize_protoc_golden.txt"

// diffPosition is which descriptor field the probed reference is written into.
// All five are separate writes in absolutizeFieldTypeNames, and two of them have
// diverged from the others before by being added at different times.
type diffPosition string

const (
	posField           diffPosition = "field"
	posMsgExtType      diffPosition = "msgext_type"
	posFileExtType     diffPosition = "fileext_type"
	posMsgExtExtendee  diffPosition = "msgext_extendee"
	posFileExtExtendee diffPosition = "fileext_extendee"
)

var diffPositions = []diffPosition{
	posField, posMsgExtType, posFileExtType, posMsgExtExtendee, posFileExtExtendee,
}

// diffCase is one probe: a package relationship, an optional shadowing
// declaration in the referring scope, a write position, and the name as source
// writes it.
type diffCase struct {
	mainPkg   string
	depPkg    string
	shadow    bool
	position  diffPosition
	asWritten string
}

func (c diffCase) key() string {
	return strings.Join([]string{
		emptyAsDash(c.mainPkg), emptyAsDash(c.depPkg),
		strconv.FormatBool(c.shadow), string(c.position), c.asWritten,
	}, "|")
}

func emptyAsDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dashAsEmpty(s string) string {
	if s == "-" {
		return ""
	}
	return s
}

// diffPackagePairs crosses the package relationships that decide how far the
// outward walk climbs. Same, ancestor and descendant are the ones where a
// stopped walk and a root fallback give different answers -- the rest are
// controls that must keep agreeing.
var diffPackagePairs = [][2]string{
	{"", ""},               // neither packaged: root and package prefix coincide
	{"probe", ""},          // packaged main, unpackaged dep
	{"", "probe"},          // unpackaged main, packaged dep
	{"probe", "probe"},     // SAME package
	{"a.b", "a"},           // dep in an ancestor package
	{"a.b.c", "a.b"},       // dep in a nearer ancestor
	{"a", "a.b"},           // dep in a descendant package
	{"a.b", "a.b.c"},       // dep in a deeper descendant
	{"x.y", "x.z"},         // siblings under a shared parent
	{"one.two", "three"},   // unrelated
	{"a.b", "a.b"},         // same multi-component package
	{"com.example", "com"}, // the tuple_fields.proto shape
}

// diffTargets are what the dependency exposes. Both carry extension ranges so
// either can stand in an extendee position.
var diffTargets = []string{"X", "X.Y"}

const (
	diffDepPath  = "diffdep.proto"
	diffMainPath = "diffmain.proto"
)

// buildDiffCases enumerates the corpus: for each package pair and target, every
// SUFFIX of the target's fully-qualified name, which is what makes the walk
// climb a different distance per case.
func buildDiffCases() []diffCase {
	var cases []diffCase
	for _, pair := range diffPackagePairs {
		mainPkg, depPkg := pair[0], pair[1]
		for _, target := range diffTargets {
			var parts []string
			if depPkg != "" {
				parts = append(parts, strings.Split(depPkg, ".")...)
			}
			parts = append(parts, strings.Split(target, ".")...)
			for i := range parts {
				asWritten := strings.Join(parts[i:], ".")
				for _, shadow := range []bool{false, true} {
					for _, pos := range diffPositions {
						cases = append(cases, diffCase{
							mainPkg: mainPkg, depPkg: depPkg,
							shadow: shadow, position: pos, asWritten: asWritten,
						})
					}
				}
			}
		}
	}
	return cases
}

// mirrorParts is the shadowing declaration: `a.b.C` becomes nested messages
// `a { b { C { extensions } } }` inside the referring message.
//
// It mirrors the WHOLE written name rather than just its head on purpose. A
// shadow declaring only the first component makes protoc reject every case ("not
// defined"), and a corpus of rejections measures nothing.
func mirrorParts(parts []string) *descriptorpb.DescriptorProto {
	inner := &descriptorpb.DescriptorProto{
		Name: proto.String(parts[len(parts)-1]),
		ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
			{Start: proto.Int32(1000), End: proto.Int32(2000)},
		},
	}
	for i := len(parts) - 2; i >= 0; i-- {
		inner = &descriptorpb.DescriptorProto{
			Name:       proto.String(parts[i]),
			NestedType: []*descriptorpb.DescriptorProto{inner},
		}
	}
	return inner
}

func mirrorSource(parts []string, pad string) string {
	if len(parts) == 1 {
		return fmt.Sprintf("%smessage %s { extensions 1000 to 2000; }\n", pad, parts[0])
	}
	return fmt.Sprintf("%smessage %s {\n%s%s}\n",
		pad, parts[0], mirrorSource(parts[1:], pad+"  "), pad)
}

// depDescriptor and depSource are the dependency, in the two forms that must
// agree. crossCheckStructure compares them at regeneration time -- see its own
// comment for exactly which drifts it does and does not catch.
func (c diffCase) depDescriptor() *descriptorpb.FileDescriptorProto {
	fd := &descriptorpb.FileDescriptorProto{
		Name:   proto.String(diffDepPath),
		Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("X"),
			ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
				{Start: proto.Int32(1000), End: proto.Int32(2000)},
			},
			NestedType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("Y"),
				ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
					{Start: proto.Int32(1000), End: proto.Int32(2000)},
				},
			}},
		}},
	}
	if c.depPkg != "" {
		fd.Package = proto.String(c.depPkg)
	}
	return fd
}

func (c diffCase) depSource() string {
	var b strings.Builder
	b.WriteString("syntax = \"proto2\";\n")
	if c.depPkg != "" {
		fmt.Fprintf(&b, "package %s;\n", c.depPkg)
	}
	b.WriteString("message X {\n  extensions 1000 to 2000;\n" +
		"  message Y { extensions 1000 to 2000; }\n}\n")
	return b.String()
}

// mainDescriptor builds the file under test with the AS-WRITTEN name at the
// probed position and nothing else relative.
func (c diffCase) mainDescriptor() *descriptorpb.FileDescriptorProto {
	anchor := &descriptorpb.DescriptorProto{
		Name: proto.String("Anchor"),
		ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{
			{Start: proto.Int32(1000), End: proto.Int32(2000)},
		},
	}
	anchorRef := ".Anchor"
	if c.mainPkg != "" {
		anchorRef = "." + c.mainPkg + ".Anchor"
	}

	host := &descriptorpb.DescriptorProto{Name: proto.String("Host")}
	if c.shadow {
		host.NestedType = append(host.NestedType, mirrorParts(strings.Split(c.asWritten, ".")))
	}

	fd := &descriptorpb.FileDescriptorProto{
		Name:        proto.String(diffMainPath),
		Syntax:      proto.String("proto2"),
		Dependency:  []string{diffDepPath},
		MessageType: []*descriptorpb.DescriptorProto{anchor, host},
	}
	if c.mainPkg != "" {
		fd.Package = proto.String(c.mainPkg)
	}

	switch c.position {
	case posField:
		host.Field = []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("f"), Number: proto.Int32(1),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: proto.String(c.asWritten),
		}}
	case posMsgExtType:
		host.Extension = []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("g"), Number: proto.Int32(1001),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: proto.String(c.asWritten),
			Extendee: proto.String(anchorRef),
		}}
	case posMsgExtExtendee:
		host.Extension = []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("k"), Number: proto.Int32(1005),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Extendee: proto.String(c.asWritten),
		}}
	case posFileExtType:
		fd.Extension = []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("i"), Number: proto.Int32(1002),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: proto.String(c.asWritten),
			Extendee: proto.String(anchorRef),
		}}
	case posFileExtExtendee:
		fd.Extension = []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("j"), Number: proto.Int32(1006),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Extendee: proto.String(c.asWritten),
		}}
	}
	return fd
}

func (c diffCase) mainSource() string {
	var b strings.Builder
	b.WriteString("syntax = \"proto2\";\n")
	if c.mainPkg != "" {
		fmt.Fprintf(&b, "package %s;\n", c.mainPkg)
	}
	fmt.Fprintf(&b, "import %q;\n", diffDepPath)
	b.WriteString("message Anchor { extensions 1000 to 2000; }\n")
	b.WriteString("message Host {\n")
	if c.shadow {
		b.WriteString(mirrorSource(strings.Split(c.asWritten, "."), "  "))
	}
	switch c.position {
	case posField:
		fmt.Fprintf(&b, "  optional %s f = 1;\n", c.asWritten)
	case posMsgExtType:
		fmt.Fprintf(&b, "  extend Anchor { optional %s g = 1001; }\n", c.asWritten)
	case posMsgExtExtendee:
		fmt.Fprintf(&b, "  extend %s { optional string k = 1005; }\n", c.asWritten)
	}
	b.WriteString("}\n")
	switch c.position {
	case posFileExtType:
		fmt.Fprintf(&b, "extend Anchor { optional %s i = 1002; }\n", c.asWritten)
	case posFileExtExtendee:
		fmt.Fprintf(&b, "extend %s { optional string j = 1006; }\n", c.asWritten)
	}
	return b.String()
}

// readPosition returns whatever now sits at the probed position.
func (c diffCase) readPosition(fd *descriptorpb.FileDescriptorProto) string {
	host := fd.MessageType[1]
	switch c.position {
	case posField:
		return host.Field[0].GetTypeName()
	case posMsgExtType:
		return host.Extension[0].GetTypeName()
	case posMsgExtExtendee:
		return host.Extension[0].GetExtendee()
	case posFileExtType:
		return fd.Extension[0].GetTypeName()
	case posFileExtExtendee:
		return fd.Extension[0].GetExtendee()
	}
	return ""
}

// rejectedMarker is the recorded verdict for a case protoc refused to compile.
//
// Rejections are RECORDED rather than left absent, and that is the whole point.
// An earlier version stored only the accepted cases and treated "no entry" as
// "protoc rejected it" -- so deleting a data line, or adding a case to
// buildDiffCases without regenerating, silently removed it from the comparison
// while every test stayed green. A corpus that cannot tell "verified absent"
// from "never recorded" is the empty-set false green wearing a data file.
const rejectedMarker = "REJECTED"

// readProtocGolden loads the committed expectations: EVERY enumerated case,
// accepted ones mapped to protoc's resolved name and rejected ones to
// rejectedMarker.
//
// A missing, empty or unparseable golden is a FATAL error, never a skip: an
// absent corpus renders as "nothing disagreed", which is the empty-set false
// green this file exists to prevent.
func readProtocGolden(t *testing.T) (golden map[string]string, accepted, rejected int) {
	t.Helper()

	f, err := os.Open(protocGoldenPath)
	if err != nil {
		t.Fatalf("opening %s: %v\nRegenerate with:\n"+
			"  go test ./pkg/recordlayer -run TestAbsolutizeAgreesWithProtoc -update-protoc-golden",
			protocGoldenPath, err)
	}
	defer func() { _ = f.Close() }()

	golden = map[string]string{}
	headerAccepted, headerRejected := -1, -1
	headerDigest := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if n, ok := strings.CutPrefix(line, "#accepted "); ok {
			headerAccepted, _ = strconv.Atoi(strings.TrimSpace(n))
			continue
		}
		if n, ok := strings.CutPrefix(line, "#rejected "); ok {
			headerRejected, _ = strconv.Atoi(strings.TrimSpace(n))
			continue
		}
		if d, ok := strings.CutPrefix(line, "#digest "); ok {
			headerDigest = strings.TrimSpace(d)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, resolved, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("malformed golden line %q in %s", line, protocGoldenPath)
		}
		if _, dup := golden[key]; dup {
			t.Fatalf("golden holds two entries for case %q", key)
		}
		golden[key] = resolved
		if resolved == rejectedMarker {
			rejected++
		} else {
			accepted++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading %s: %v", protocGoldenPath, err)
	}

	// The header counts are a checksum on the rows, so a hand-edited golden
	// cannot quietly disagree with its own summary.
	if headerAccepted != accepted || headerRejected != rejected {
		t.Fatalf("%s header says accepted=%d rejected=%d but the rows say accepted=%d rejected=%d "+
			"-- the file has been edited without regenerating",
			protocGoldenPath, headerAccepted, headerRejected, accepted, rejected)
	}

	// COUNTS ALONE ARE NOT ENOUGH, because they are editable in the same pass as
	// the row. Flipping one accepted row to REJECTED and decrementing #accepted
	// while incrementing #rejected is a three-line edit that satisfies every
	// count check, both set-equality directions and the dimension floors -- and
	// quietly removes a case from the comparison. The digest moves for any row
	// edit, whatever the headers say, and closes that one.
	//
	// WHAT THE DIGEST IS NOT: tamper-proof. It detects DRIFT -- a row changed
	// without regenerating -- and nothing more. Anyone editing the file can also
	// recompute it, and a COUNT-CONSERVING SWAP walks past every guard here:
	// turn one accepted row into REJECTED and one REJECTED row into a fabricated
	// answer copied from Go's own output. Both counts are unchanged, so the
	// floor and the headers hold; the digest is recomputed; set equality holds;
	// every dimension floor holds. The forged row then "agrees" by construction
	// and a real single-key divergence hides behind it -- demonstrated.
	//
	// There is no cryptographic answer available here: the file and the checker
	// ship together, so any value the checker can compute, an editor can too.
	// The actual defence is REGENERATION against protoc -- the command in this
	// file's header -- and review of the resulting diff. These guards exist to
	// make an accidental or lazy edit loud, not to stop a deliberate one.
	//
	// The failure below therefore does NOT print the computed digest. Printing
	// it hands over the one value needed to complete the forgery, which is a
	// strange thing for a tamper check to do even if it is only a drift check.
	if headerDigest == "" {
		t.Fatalf("%s carries no #digest line; regenerate it", protocGoldenPath)
	}
	if goldenDigest(golden) != headerDigest {
		t.Fatalf("%s: the data rows do not match the recorded #digest. A row was edited by hand. "+
			"Regenerate with the command in this file's header rather than adjusting the header "+
			"values, which the same edit can satisfy.", protocGoldenPath)
	}

	// A MAGNITUDE FLOOR, because every other check here is shape-only and the
	// counts are whatever the last regeneration produced. A change that made
	// protoc reject most of the corpus would regenerate to a much smaller
	// accepted set, keep every dimension non-empty, and pass everything above.
	// The floor is what makes the population part of the claim; raise it when
	// the corpus legitimately grows.
	if accepted < minAcceptedCases {
		t.Fatalf("%s carries only %d accepted cases, below the recorded floor of %d. Either the "+
			"corpus shrank (a generator change making protoc reject cases it used to accept), or "+
			"it legitimately grew smaller and this floor needs revising -- deliberately, not by "+
			"regenerating past it.", protocGoldenPath, accepted, minAcceptedCases)
	}
	return golden, accepted, rejected
}

// minAcceptedCases is the floor for the accepted corpus, recorded from the
// generation that produced the committed golden. See the magnitude check above
// for why a floor is needed on top of the counts and the digest.
const minAcceptedCases = 526

// goldenDigest hashes the golden's DATA rows, independent of header counts and
// of row order.
func goldenDigest(golden map[string]string) string {
	keys := make([]string, 0, len(golden))
	for k := range golden {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\t%s\n", k, golden[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// TestAbsolutizeAgreesWithProtoc replays the committed corpus.
//
// The assertion is EXACT agreement on every protoc-accepted case: no tolerance,
// no allowlist. Java and protoc agree on every shape the corpus contains -- the
// one axis where they do not is the field-shadowed extendee, which protoc
// rejects and which therefore never reaches the accepted set -- so a divergence
// here is a divergence from Java too.
func TestAbsolutizeAgreesWithProtoc(t *testing.T) {
	if *updateProtocGolden {
		regenerateProtocGolden(t)
		return
	}
	t.Parallel()

	golden, acceptedCount, rejected := readProtocGolden(t)
	cases := buildDiffCases()

	// POPULATION FIRST. Both sides going empty would make every assertion below
	// vacuous while the test still reported green.
	if acceptedCount == 0 {
		t.Fatalf("%s carries no accepted cases, so this test asserts nothing", protocGoldenPath)
	}
	if len(cases) == 0 {
		t.Fatal("buildDiffCases enumerated nothing, so there is nothing for the golden to match")
	}

	// THE TWO SIDES MUST BE THE SAME SET, in both directions. One direction
	// alone is not enough: checking only for orphans misses a case the
	// enumerator added, and checking only for missing entries misses one it
	// dropped. Together they force the golden to be regenerated whenever
	// buildDiffCases changes at all.
	if len(golden) != len(cases) {
		t.Fatalf("%s records %d cases but buildDiffCases enumerates %d. Regenerate:\n"+
			"  go test ./pkg/recordlayer -run TestAbsolutizeAgreesWithProtoc -update-protoc-golden",
			protocGoldenPath, len(golden), len(cases))
	}

	// Every golden entry must still be enumerated. Without this, narrowing
	// buildDiffCases would drop coverage silently while the survivors all passed.
	enumerated := map[string]bool{}
	for _, c := range cases {
		enumerated[c.key()] = true
	}
	var orphaned []string
	for key := range golden {
		if !enumerated[key] {
			orphaned = append(orphaned, key)
		}
	}
	if len(orphaned) > 0 {
		sort.Strings(orphaned)
		t.Fatalf("%d golden cases are no longer enumerated (first: %s) -- the corpus shrank "+
			"without the golden being regenerated, so coverage was dropped silently",
			len(orphaned), orphaned[0])
	}

	checked, divergences := 0, 0
	for _, c := range cases {
		want, ok := golden[c.key()]
		if !ok {
			t.Fatalf("case %s has no recorded verdict in %s. A case with no entry is NOT a "+
				"rejection -- rejections are recorded explicitly -- so this is an unregenerated "+
				"golden, and treating it as absent is how coverage disappears silently.",
				c.key(), protocGoldenPath)
		}
		if want == rejectedMarker {
			continue // protoc refused this shape; Go's answer is unconstrained
		}
		checked++

		main := c.mainDescriptor()
		absolutizeFieldTypeNames(main, c.depDescriptor())

		if got := c.readPosition(main); got != want {
			divergences++
			if divergences <= 10 {
				t.Errorf("case %s\n  as written: %s\n  protoc:     %s\n  go:         %s",
					c.key(), c.asWritten, want, got)
			}
		}
	}

	if checked != acceptedCount {
		t.Fatalf("compared %d cases against a golden holding %d accepted -- enumeration and "+
			"golden disagree, so any agreement count is not about the whole of it",
			checked, acceptedCount)
	}
	if divergences > 0 {
		t.Fatalf("%d of %d protoc-accepted cases resolve differently in Go (%d further cases "+
			"protoc rejected and this corpus excludes)", divergences, checked, rejected)
	}
	t.Logf("protoc differential: %d accepted cases, 0 divergences (%d rejected by protoc)",
		checked, rejected)
}

// TestProtocGoldenCoversEveryDimension guards the corpus's own shape.
//
// The agreement count is an aggregate, and an aggregate cannot notice that a
// whole POSITION or a whole package RELATIONSHIP stopped being generated -- the
// survivors would still agree and the number would still look large. Each
// position is a separate write in the production pass, and the three
// relationships named here are the only ones where a stopped walk and the root
// fallback give different answers.
func TestProtocGoldenCoversEveryDimension(t *testing.T) {
	t.Parallel()

	golden, acceptedCount, _ := readProtocGolden(t)
	if acceptedCount == 0 {
		t.Fatalf("%s carries no accepted cases", protocGoldenPath)
	}

	positions := map[diffPosition]int{}
	relationships := map[string]int{}
	shadowed := 0
	for key, verdict := range golden {
		parts := strings.Split(key, "|")
		if len(parts) != 5 {
			t.Fatalf("malformed golden key %q", key)
		}
		// ACCEPTED cases only. A dimension present solely as rejections is not
		// covered by the differential -- rejected cases are never compared --
		// so counting them here would let a dimension look measured while
		// contributing nothing to the agreement claim.
		if verdict == rejectedMarker {
			continue
		}
		mainPkg, depPkg := dashAsEmpty(parts[0]), dashAsEmpty(parts[1])
		positions[diffPosition(parts[3])]++
		if parts[2] == "true" {
			shadowed++
		}
		switch {
		case mainPkg != "" && mainPkg == depPkg:
			relationships["same"]++
		case depPkg != "" && strings.HasPrefix(mainPkg, depPkg+"."):
			relationships["ancestor"]++
		case mainPkg != "" && strings.HasPrefix(depPkg, mainPkg+"."):
			relationships["descendant"]++
		}
	}

	for _, pos := range diffPositions {
		if positions[pos] == 0 {
			t.Errorf("no accepted case writes at position %q, so the differential does not "+
				"measure that write at all", pos)
		}
	}
	for _, rel := range []string{"same", "ancestor", "descendant"} {
		if relationships[rel] == 0 {
			t.Errorf("no accepted case puts the dependency in a %s package. Those are the "+
				"relationships where a stopped walk and the root fallback differ, so without "+
				"them this corpus cannot see the symbol seeding at all", rel)
		}
	}
	if shadowed == 0 {
		// NOT a first-component claim. `mirrorParts` mirrors the WHOLE written
		// name, so on every case here the first-component rule and a
		// whole-reference rule give the same answer -- measured: replacing the
		// first-component split outright leaves this corpus at 526 accepted, 0
		// divergences. What the shadowed cases establish is only that a
		// shadowing declaration in the referring scope is exercised at all;
		// TestAbsolutizeFieldTypeNamesResolvesTheFirstComponentOutward is what
		// pins the rule, and it reddens under that mutation.
		t.Error("no accepted case declares a shadowing type in the referring scope, so the " +
			"corpus never exercises a reference that the referring scope could satisfy")
	}
}

// regenerateProtocGolden rebuilds the golden by compiling every case with
// protoc. It runs only under -update-protoc-golden and is the only code here
// that touches the filesystem or spawns a process.
func regenerateProtocGolden(t *testing.T) {
	t.Helper()

	protoc, err := exec.LookPath("protoc")
	if err != nil {
		t.Fatalf("protoc not found on PATH: %v -- regeneration needs it (the normal test run "+
			"does not)", err)
	}

	cases := buildDiffCases()
	dir := t.TempDir()
	depFile := filepath.Join(dir, diffDepPath)
	mainFile := filepath.Join(dir, diffMainPath)
	setFile := filepath.Join(dir, "out.pb")

	// results holds EVERY enumerated case, accepted or rejected, so the golden
	// records a verdict for each and a missing entry can be treated as an error
	// rather than as a silent rejection.
	results := map[string]string{}
	accepted, rejected := 0, 0
	for _, c := range cases {
		if err := os.WriteFile(depFile, []byte(c.depSource()), 0o600); err != nil {
			t.Fatalf("writing dep source: %v", err)
		}
		if err := os.WriteFile(mainFile, []byte(c.mainSource()), 0o600); err != nil {
			t.Fatalf("writing main source: %v", err)
		}

		cmd := exec.Command(protoc, "-I", dir, "--descriptor_set_out="+setFile,
			diffMainPath, diffDepPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			// A protoc REJECTION is data: the shape cannot come out of a .proto
			// file, so Go's answer is unconstrained and the case is recorded as
			// rejected.
			//
			// A protoc FAILURE is not data, and the two arrive identically as a
			// non-zero exit. A bad flag, an unwritable output path or a missing
			// input would otherwise be silently counted as "protoc rejected
			// it", shrinking the corpus while the totals still looked large --
			// so a rejection has to look like one. protoc reports a source-level
			// rejection as `<file>:<line>:<col>: <message>`; anything else is
			// the harness being broken and must stop generation rather than
			// quietly reduce coverage.
			text := string(out)
			if !hasSourceLevelError(text) {
				t.Fatalf("protoc failed on case %s without a source-level ERROR, which means "+
					"the invocation is broken rather than the descriptor rejected:\n%s\nerr: %v",
					c.key(), text, err)
			}
			results[c.key()] = rejectedMarker
			rejected++
			continue
		}

		raw, err := os.ReadFile(setFile)
		if err != nil {
			t.Fatalf("reading descriptor set: %v", err)
		}
		var set descriptorpb.FileDescriptorSet
		if err := proto.Unmarshal(raw, &set); err != nil {
			t.Fatalf("unmarshalling descriptor set: %v", err)
		}
		var compiled, compiledDep *descriptorpb.FileDescriptorProto
		for _, fd := range set.File {
			switch fd.GetName() {
			case diffMainPath:
				compiled = fd
			case diffDepPath:
				compiledDep = fd
			}
		}
		if compiled == nil {
			t.Fatalf("protoc produced no descriptor for %s (case %s)", diffMainPath, c.key())
		}
		if compiledDep == nil {
			t.Fatalf("protoc produced no descriptor for %s (case %s)", diffDepPath, c.key())
		}

		crossCheckStructure(t, c, compiled, compiledDep)

		resolved := c.readPosition(compiled)
		if resolved == "" || resolved[0] != '.' {
			t.Fatalf("protoc resolved case %s to %q, which is not an absolute name -- the probe "+
				"is reading the wrong position", c.key(), resolved)
		}
		results[c.key()] = resolved
		accepted++
	}

	if accepted == 0 {
		t.Fatalf("protoc accepted none of the %d cases; the corpus would assert nothing", len(cases))
	}

	// THE FLOOR APPLIES HERE TOO, BEFORE WRITING. The reader-side check runs on
	// a normal run, and `-update-protoc-golden` returns before ever reaching it
	// -- so a change that made protoc reject most of the corpus would regenerate
	// a shrunken golden and report PASS, with the failure deferred to whoever
	// ran the suite next. The guard belongs on the side that produces the file.
	if accepted < minAcceptedCases {
		t.Fatalf("regeneration produced only %d accepted cases, below the floor of %d. Something "+
			"made protoc reject cases it used to accept -- fix that rather than lowering the "+
			"floor, and if the shrink is intended, lower it deliberately in the same change.",
			accepted, minAcceptedCases)
	}
	if len(results) != len(cases) {
		t.Fatalf("recorded %d verdicts for %d enumerated cases -- every case must get one, or a "+
			"missing entry becomes indistinguishable from a rejection", len(results), len(cases))
	}

	keys := make([]string, 0, len(results))
	for k := range results {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out strings.Builder
	fmt.Fprintf(&out, "# protoc resolution ground truth for absolutizeFieldTypeNames.\n")
	fmt.Fprintf(&out, "# Regenerate: go test ./pkg/recordlayer -run TestAbsolutizeAgreesWithProtoc -update-protoc-golden\n")
	fmt.Fprintf(&out, "# key = mainPkg|depPkg|shadow|position|asWritten   (- means unpackaged)\n")
	fmt.Fprintf(&out, "# value = protoc's resolved name, or %s when protoc refused the descriptor.\n", rejectedMarker)
	fmt.Fprintf(&out, "# Rejections are recorded, not omitted: a case with no entry is an error,\n")
	fmt.Fprintf(&out, "# so a dropped line cannot masquerade as a shape protoc declined.\n")
	// The protoc that produced this. Ground truth is only meaningful against a
	// named compiler: `exec.LookPath` takes whatever is on PATH, so regenerating
	// on another machine can silently rewrite the expectations under a different
	// protoc without anything in the file changing to say so.
	version := "unknown"
	if v, err := exec.Command(protoc, "--version").Output(); err == nil {
		version = strings.TrimSpace(string(v))
	}
	fmt.Fprintf(&out, "# generated by: %s\n", version)
	fmt.Fprintf(&out, "#accepted %d\n", accepted)
	fmt.Fprintf(&out, "#rejected %d\n", rejected)
	fmt.Fprintf(&out, "#digest %s\n", goldenDigest(results))
	for _, k := range keys {
		fmt.Fprintf(&out, "%s\t%s\n", k, results[k])
	}

	if err := os.MkdirAll(filepath.Dir(protocGoldenPath), 0o755); err != nil {
		t.Fatalf("creating testdata dir: %v", err)
	}
	if err := os.WriteFile(protocGoldenPath, []byte(out.String()), 0o600); err != nil {
		t.Fatalf("writing %s: %v", protocGoldenPath, err)
	}
	t.Logf("wrote %s: %d accepted, %d rejected by protoc", protocGoldenPath, accepted, rejected)
}

// hasSourceLevelError reports whether protoc's output carries a real
// source-level diagnostic, as opposed to a warning or a harness failure.
//
// WARNINGS WEAR THE SAME SHAPE, which is what makes the naive check wrong. An
// earlier version asked only whether the output mentioned `<file>:`, and protoc
// emits `diffmain.proto:3:1: warning: Import diffdep.proto is unused.` whenever
// the shadow satisfies the reference and the import goes unused -- true for
// EVERY shadow=true case, half the corpus. Worse, that warning is printed
// alongside a genuine harness failure, so the exact example the guard was
// written for:
//
//	$ protoc -I . --descriptor_set_out=/nonexistent/out.pb diffmain.proto diffdep.proto
//	diffmain.proto:3:1: warning: Import diffdep.proto is unused.
//	/nonexistent/out.pb: No such file or directory
//
// satisfied it, and would have been recorded as a protoc rejection. Rejections
// are skipped by the comparison, so the corpus would shrink in the direction
// that still reads as success.
//
// A line is a rejection only if it NAMES one of the two source files AND is not
// a warning.
//
// It matches on containment rather than prefix, because protoc reports the path
// it was given: the generator passes `-I <absolute dir>`, so diagnostics arrive
// as `/tmp/xxx/diffmain.proto:5:12: "Y" is not defined.` A prefix check silently
// matches nothing there and turns every genuine rejection into a fatal
// "invocation is broken" -- which is at least loud, unlike the failure this
// function exists to prevent, but is still wrong. Both spellings are pinned.
func hasSourceLevelError(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, ": warning:") {
			continue
		}
		for _, f := range []string{diffMainPath, diffDepPath} {
			i := strings.Index(line, f+":")
			if i < 0 {
				continue
			}
			if hasLineColumn(line[i+len(f)+1:]) {
				return true
			}
		}
	}
	return false
}

// hasLineColumn reports whether what follows a file name in a protoc diagnostic
// begins with `<line>:<column>:`.
//
// NAMING THE FILE IS NOT ENOUGH. protoc reports invocation-level failures with
// the file name too, and one of them is the missing-input case:
//
//	Could not make proto path relative: diffdep.proto: No such file or directory
//
// A containment check alone reads that as a source rejection and records the
// case REJECTED, which is precisely the silent-shrink this helper exists to
// prevent, arriving through the helper itself.
//
// The rule is a POSITIONAL line, not "every diagnostic has a position". protoc
// emits genuine source-level errors without one -- `diffdep.proto: File not
// found.` is such a line -- so the strict reading of this predicate is "at
// least one positional non-warning line naming a source file". That is
// sufficient because protoc pairs the position-free forms with a positional
// line (measured: the `File not found.` pair returns true), and because the
// failure direction is safe: a rejection misread as a harness failure aborts
// generation loudly, where the reverse silently shrinks the corpus.
func hasLineColumn(rest string) bool {
	digits := func(s string) (int, bool) {
		n := 0
		for n < len(s) && s[n] >= '0' && s[n] <= '9' {
			n++
		}
		return n, n > 0
	}
	n, ok := digits(rest)
	if !ok || n >= len(rest) || rest[n] != ':' {
		return false
	}
	rest = rest[n+1:]
	n, ok = digits(rest)
	return ok && n < len(rest) && rest[n] == ':'
}

// The probe-line extraction gets its own arms, because the check that uses it
// runs only during regeneration and would otherwise never be asserted on --
// which is how its predecessor shipped satisfiable by an unrelated declaration.
// EVERY POSITION, because crossCheckStructure runs only under
// -update-protoc-golden and therefore asserts nothing during a normal suite
// run. Driving one position leaves the other four markers unpinned: breaking
// all four of them left the whole package green, measured. That is the same
// never-asserted-on gap that let the predecessor ship inert.
func TestProbeLineFindsEveryPositionsStatement(t *testing.T) {
	t.Parallel()

	for _, pos := range diffPositions {
		t.Run(string(pos), func(t *testing.T) {
			t.Parallel()

			// Shadowed and single-component, the shape that defeats a
			// whole-file search, exercised at every write position.
			c := diffCase{mainPkg: "probe", depPkg: "", shadow: true, position: pos, asWritten: "X"}
			src := c.mainSource()

			line, ok := probeLine(src, pos)
			if !ok {
				t.Fatalf("probeLine found no statement for position %q. Its marker no longer "+
					"matches what mainSource emits, so crossCheckStructure would abort every "+
					"regeneration -- or, if the marker matched something else, silently check "+
					"the wrong line.\n%s", pos, src)
			}
			if strings.Contains(line, "message ") {
				t.Fatalf("probeLine returned the shadow declaration %q for position %q rather "+
					"than the probe statement", strings.TrimSpace(line), pos)
			}
			if !strings.Contains(line, " X ") {
				t.Fatalf("probe line %q for position %q does not carry the as-written name",
					strings.TrimSpace(line), pos)
			}

			// The marker must be UNIQUE IN THE FILE, which is the property that
			// makes first-match-wins safe. Distinctness across positions is not
			// enough on its own.
			// Counted in LINES, matching what probeLine actually scans. Counting
			// OCCURRENCES agrees on today's emitter and would diverge the moment
			// two landed on one line -- asserting the wrong quantity for the
			// right reason.
			marker := probeFieldMarkers[pos]
			lines := 0
			for _, l := range strings.Split(src, "\n") {
				if strings.Contains(l, marker) {
					lines++
				}
			}
			if lines != 1 {
				t.Fatalf("marker %q appears on %d lines of the %q source, so probeLine cannot "+
					"identify the probe statement:\n%s", marker, lines, pos, src)
			}
		})
	}
}

// SINGLENESS ENFORCEMENT NEEDS ITS OWN ARM, because every source the emitter
// actually produces contains exactly one marker -- so reverting probeLine to
// first-match-wins leaves every other test here green, and the differential
// stays green too since crossCheckStructure runs only during regeneration. The
// behaviour would be unpinned by construction.
//
// A future corpus shape with two matching lines would then have the check
// silently inspect the first statement, which is the same wrong-line failure
// the scoping was introduced to prevent.
func TestProbeLineRefusesADuplicateMarker(t *testing.T) {
	t.Parallel()

	marker := probeFieldMarkers[posField]

	single := "message Host {\n  optional X " + marker + "\n}\n"
	if _, ok := probeLine(single, posField); !ok {
		t.Fatalf("probeLine rejected a source with exactly one %q, so this arm's negative case "+
			"below would prove nothing:\n%s", marker, single)
	}

	// Two lines carrying the marker. First-match-wins would return the first
	// and report success; the contract is to refuse.
	double := single[:len(single)-2] + "  optional Y " + marker + "\n}\n"
	if n := strings.Count(double, marker); n != 2 {
		t.Fatalf("the fixture carries %d markers, not 2, so it does not express the ambiguity "+
			"this arm is named for:\n%s", n, double)
	}
	if line, ok := probeLine(double, posField); ok {
		t.Fatalf("probeLine returned %q for a source with TWO matching lines. Returning the "+
			"first silently checks the wrong statement -- exactly the failure the probe-line "+
			"scoping exists to prevent.", strings.TrimSpace(line))
	}
}

func TestProbeLineIsScopedToTheStatementUnderTest(t *testing.T) {
	t.Parallel()

	// A shadowed single-component case: mirrorSource emits `message X { ... }`,
	// which is what defeated a whole-file search.
	c := diffCase{mainPkg: "probe", depPkg: "", shadow: true, position: posField, asWritten: "X"}
	src := c.mainSource()

	if !strings.Contains(src, "message X {") {
		t.Fatalf("fixture no longer emits a shadow declaration for `X`, so this arm cannot "+
			"exercise the collision it exists for:\n%s", src)
	}

	line, ok := probeLine(src, posField)
	if !ok {
		t.Fatalf("probeLine found no statement for %q in:\n%s", posField, src)
	}
	if strings.Contains(line, "message ") {
		t.Fatalf("probeLine returned the shadow declaration %q rather than the probe statement",
			strings.TrimSpace(line))
	}
	if !strings.Contains(line, " X ") {
		t.Fatalf("probe line %q does not carry the as-written name", strings.TrimSpace(line))
	}

	// THE DRIFT THE OLD CHECK MISSED: qualify the probe while the shadow keeps
	// declaring the bare name. A whole-file search still succeeds; a probe-line
	// search must not.
	drifted := strings.Replace(src, "optional X f = 1;", "optional probe.X f = 1;", 1)
	if drifted == src {
		t.Fatal("the fixture's probe statement is not `optional X f = 1;` any more, so this arm " +
			"is simulating a drift that cannot occur")
	}
	if !strings.Contains(drifted, " X ") {
		t.Fatal("the drifted source no longer contains ` X ` anywhere, so it would be caught by " +
			"a whole-file search too and this arm proves nothing about scoping")
	}
	driftedLine, ok := probeLine(drifted, posField)
	if !ok {
		t.Fatalf("probeLine found no statement in the drifted source:\n%s", drifted)
	}
	if strings.Contains(driftedLine, " X ") {
		t.Fatalf("probe line %q still reads as carrying the bare name after the probe was "+
			"qualified -- the check is not scoped to the statement under test",
			strings.TrimSpace(driftedLine))
	}
}

// probeFieldMarkers identifies each position's emitted statement by the field
// name and number only that position writes. Keeping them distinct is what lets
// the cross-check look at the probe statement rather than at the whole file.
var probeFieldMarkers = map[diffPosition]string{
	posField:           "f = 1;",
	posMsgExtType:      "g = 1001;",
	posFileExtType:     "i = 1002;",
	posMsgExtExtendee:  "k = 1005;",
	posFileExtExtendee: "j = 1006;",
}

// probeLine returns the single line of `src` carrying the probed reference, and
// reports false if there is not exactly one.
//
// SINGLENESS IS ENFORCED, not assumed. An earlier version returned the first
// match while its doc claimed "the single line" -- true of today's emitter
// (measured: exactly one match per case across all 680) but a property of the
// data rather than of the function. A second matching line would silently make
// the caller check the wrong statement, which is precisely the class of defect
// this whole check exists to close, so the function refuses instead.
func probeLine(src string, pos diffPosition) (string, bool) {
	marker, ok := probeFieldMarkers[pos]
	if !ok {
		return "", false
	}
	found, seen := "", 0
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, marker) {
			found, seen = line, seen+1
		}
	}
	if seen != 1 {
		return "", false
	}
	return found, true
}

// crossCheckStructure catches DRIFT between the two renderings of a case.
//
// The test builds its input as a descriptor while the generator hands protoc
// .proto text, so the corpus is only meaningful while those two describe the
// same file. Nothing else would notice them parting: the expectation would
// simply become a fact about a file the test never builds, and the differential
// would keep reporting agreement about the wrong thing.
//
// WHAT IT DOES NOT CATCH, listed first and from shapes actually run, because an
// earlier version of this comment claimed "any drift between them" and that was
// broader than the code by some distance:
//
//   - Field and extension NAMES and NUMBERS. readPosition indexes [0], so a
//     renamed or renumbered field is invisible here.
//   - Anything below the message tree in the DEPENDENCY beyond its type names --
//     extension ranges, for instance, which both renderings hard-code
//     separately.
//   - Enum VALUES and service METHODS. Only the declaring names are compared.
//
// What it does catch: the package of both files, the full nested message tree of
// both, their top-level enum and service names, and -- the one that actually
// matters, since it is the thing under test -- that protoc resolved from the
// same AS-WRITTEN name the descriptor carries at the probed position.
func crossCheckStructure(
	t *testing.T,
	c diffCase,
	compiled, compiledDep *descriptorpb.FileDescriptorProto,
) {
	t.Helper()

	built := c.mainDescriptor()
	if got, want := compiled.GetPackage(), built.GetPackage(); got != want {
		t.Fatalf("case %s: protoc compiled package %q, the descriptor builder produced %q",
			c.key(), got, want)
	}
	names := func(fd *descriptorpb.FileDescriptorProto) []string {
		var out []string
		var walk func(prefix string, msgs []*descriptorpb.DescriptorProto)
		walk = func(prefix string, msgs []*descriptorpb.DescriptorProto) {
			for _, m := range msgs {
				full := prefix + m.GetName()
				out = append(out, full)
				walk(full+".", m.GetNestedType())
			}
		}
		walk("", fd.GetMessageType())
		sort.Strings(out)
		return out
	}
	got, want := names(compiled), names(built)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("case %s: message trees differ.\n  protoc:  %v\n  builder: %v\n"+
			"mainSource and mainDescriptor have drifted apart, so the golden would record "+
			"protoc's answer for a file the test never builds", c.key(), got, want)
	}

	// THE DEPENDENCY, which went unchecked entirely. depSource() writes its
	// messages as text while depDescriptor() builds them programmatically, so a
	// rename or an added type in one alone would leave the golden recording
	// protoc's answer for a different dependency than the test is handed --
	// exactly the drift this function exists to refuse, on the file it was not
	// looking at.
	builtDep := c.depDescriptor()
	if got, want := compiledDep.GetPackage(), builtDep.GetPackage(); got != want {
		t.Fatalf("case %s: protoc compiled dependency package %q, the builder produced %q",
			c.key(), got, want)
	}
	gotDep, wantDep := names(compiledDep), names(builtDep)
	if strings.Join(gotDep, ",") != strings.Join(wantDep, ",") {
		t.Fatalf("case %s: dependency message trees differ.\n  protoc:  %v\n  builder: %v\n"+
			"depSource and depDescriptor have drifted apart", c.key(), gotDep, wantDep)
	}

	// Top-level enums and services, for both files: `names` walks messages only,
	// so a declaration of either kind would otherwise be invisible.
	flat := func(fd *descriptorpb.FileDescriptorProto) string {
		var parts []string
		for _, e := range fd.GetEnumType() {
			parts = append(parts, "enum:"+e.GetName())
		}
		for _, s := range fd.GetService() {
			parts = append(parts, "svc:"+s.GetName())
		}
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	if a, b := flat(compiled), flat(built); a != b {
		t.Fatalf("case %s: main enum/service declarations differ: protoc %q, builder %q", c.key(), a, b)
	}
	if a, b := flat(compiledDep), flat(builtDep); a != b {
		t.Fatalf("case %s: dependency enum/service declarations differ: protoc %q, builder %q",
			c.key(), a, b)
	}

	// THE PROBED SPELLING ITSELF -- the one field the whole corpus is about.
	//
	// Compared DIRECTLY against the emitted source, not inferred from protoc's
	// resolved name. Inference cannot do this job: if the descriptor carries `X`
	// while the source emits a qualified `probe.X`, protoc stores `.probe.X`,
	// which ends in `X` and satisfies any suffix test -- while qualification is
	// exactly the thing that changes how a shadow resolves, so the golden would
	// be recording protoc's answer to a different question.
	//
	// SCOPED TO THE PROBE'S OWN LINE, not the whole file. A whole-file search is
	// satisfiable by a DIFFERENT declaration: with shadow=true and a
	// single-component name, mirrorSource already emits `message X { ... }`, so
	// searching the file for ` X ` succeeds no matter what the probe says. The
	// probe could drift from `optional X f` to `optional probe.X f` -- which is
	// precisely the qualification that changes shadow resolution -- and
	// regeneration would silently rewrite the golden from `.probe.Host.X` to
	// `.probe.X` with this check green.
	//
	// The probe line is found by the field name unique to each position, so the
	// assertion is about the statement under test and nothing else.
	src := c.mainSource()
	line, ok := probeLine(src, c.position)
	if !ok {
		t.Fatalf("case %s: probeLine found no UNIQUE statement for position %q -- either no line "+
			"carries its marker, or MORE THAN ONE does and the singleness refusal fired:\n%s",
			c.key(), c.position, src)
	}
	if !strings.Contains(line, " "+c.asWritten+" ") {
		t.Fatalf("case %s: the probe statement is %q, which does not carry the as-written name "+
			"%q as a whole token.\nmainSource and mainDescriptor disagree about the reference "+
			"under test, so the golden would record an answer to a different question",
			c.key(), strings.TrimSpace(line), c.asWritten)
	}
	// And protoc's answer must still be an absolute name ending in it, which
	// catches the reverse drift (the source right, the descriptor's probed
	// position wrong).
	if resolved := c.readPosition(compiled); !strings.HasSuffix(resolved, "."+c.asWritten) {
		t.Fatalf("case %s: protoc resolved the probed position to %q, which does not end in "+
			"%q", c.key(), resolved, c.asWritten)
	}
}

// The REJECTION/FAILURE discriminator gets its own arms, because it decides
// whether a case enters the comparison at all and it runs only during
// regeneration -- so a full corpus run exercises it without ever asserting on
// it. It was wrong for exactly that reason: unconditionally satisfied on the
// half of the corpus that carries an unused-import warning.
//
// Every string below is real protoc 35.1 output, not a paraphrase.
func TestSourceLevelErrorSeparatesRejectionFromHarnessFailure(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{
			name: "a genuine rejection",
			text: "diffmain.proto:6:12: \"A\" is not a message type.\n",
			want: true,
		},
		{
			// THE SPELLING THE GENERATOR ACTUALLY SEES. It passes an absolute
			// -I, so protoc reports an absolute path; a prefix match finds
			// nothing here and calls every real rejection a broken invocation.
			name: "a rejection reported with an absolute path",
			text: "/home/u/.cache/gotmp/TestX123/001/diffmain.proto:5:12: \"Y\" is not defined.\n",
			want: true,
		},
		{
			name: "an absolute-path warning is still not a rejection",
			text: "/home/u/.cache/gotmp/TestX123/001/diffmain.proto:3:1: warning: Import diffdep.proto is unused.\n",
			want: false,
		},
		{
			name: "a rejection in the dependency",
			text: "diffdep.proto:3:1: Expected top-level statement (e.g. \"message\").\n",
			want: true,
		},
		{
			name: "an unused-import warning alone is NOT a rejection",
			text: "diffmain.proto:3:1: warning: Import diffdep.proto is unused.\n",
			want: false,
		},
		{
			// THE CASE THAT DEFEATED THE OLD CHECK. A harness failure that
			// happens to be preceded by a warning naming the source file.
			name: "unwritable output path, preceded by a warning",
			text: "diffmain.proto:3:1: warning: Import diffdep.proto is unused.\n" +
				"/nonexistent/out.pb: No such file or directory\n",
			want: false,
		},
		{
			name: "a bad flag names no source file",
			text: "Unknown flag: --not-a-flag\n",
			want: false,
		},
		{
			// NAMES THE FILE, CARRIES NO POSITION. protoc's missing-input
			// failure, which a containment check alone reads as a rejection.
			name: "a missing input is a harness failure, not a rejection",
			text: "Could not make proto path relative: diffdep.proto: No such file or directory\n",
			want: false,
		},
		{
			name: "a file named with no line:column at all",
			text: "diffmain.proto: some invocation-level complaint\n",
			want: false,
		},
		{
			// A GENUINE source-level error that carries NO position. This is
			// why the predicate is "at least one positional line" rather than
			// "every diagnostic has a position" -- the second claim is false,
			// and this row is the counterexample.
			name: "a positionless source error alone does not qualify",
			text: "diffdep.proto: File not found.\n",
			want: false,
		},
		{
			// ...and the pair protoc actually emits, which does. The safety of
			// the predicate rests on this pairing, so it is pinned rather than
			// asserted in prose. Stated at the strength it was measured at:
			// across the protoc 35.1 failure modes probed here, a positionless
			// source error always arrived alongside a positional line -- not
			// "never" in the absolute, which nothing here establishes.
			name: "the positionless error paired with its positional line",
			text: "diffdep.proto: File not found.\n" +
				"diffmain.proto:2:1: Import \"diffdep.proto\" was not found or had errors.\n",
			want: true,
		},
		{
			name: "a warning and a real error together is still a rejection",
			text: "diffmain.proto:3:1: warning: Import diffdep.proto is unused.\n" +
				"diffmain.proto:6:12: \"A\" is not a message type.\n",
			want: true,
		},
		{
			name: "empty output is not a rejection",
			text: "",
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasSourceLevelError(tc.text); got != tc.want {
				t.Fatalf("hasSourceLevelError(%q) = %v, want %v.\n"+
					"A false positive records a harness failure as a protoc rejection, and "+
					"rejections are skipped by the comparison -- the corpus shrinks silently, in "+
					"the direction that still reads as success.", tc.text, got, tc.want)
			}
		})
	}
}
