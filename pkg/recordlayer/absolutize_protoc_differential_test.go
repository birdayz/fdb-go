package recordlayer

import (
	"bufio"
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
// agree. Any drift between them is caught at regeneration time by
// crossCheckStructure.
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

// readProtocGolden loads the committed expectations.
//
// A missing, empty or unparseable golden is a FATAL error, never a skip: an
// absent corpus renders as "nothing disagreed", which is the empty-set false
// green this file exists to prevent.
func readProtocGolden(t *testing.T) (map[string]string, int) {
	t.Helper()

	f, err := os.Open(protocGoldenPath)
	if err != nil {
		t.Fatalf("opening %s: %v\nRegenerate with:\n"+
			"  go test ./pkg/recordlayer -run TestAbsolutizeAgreesWithProtoc -update-protoc-golden",
			protocGoldenPath, err)
	}
	defer func() { _ = f.Close() }()

	golden := map[string]string{}
	rejected := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if n, ok := strings.CutPrefix(line, "#rejected "); ok {
			rejected, _ = strconv.Atoi(strings.TrimSpace(n))
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, resolved, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("malformed golden line %q in %s", line, protocGoldenPath)
		}
		golden[key] = resolved
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading %s: %v", protocGoldenPath, err)
	}
	return golden, rejected
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

	golden, rejected := readProtocGolden(t)
	cases := buildDiffCases()

	// POPULATION FIRST. Both sides going empty would make every assertion below
	// vacuous while the test still reported green.
	if len(golden) == 0 {
		t.Fatalf("%s carries no accepted cases, so this test asserts nothing", protocGoldenPath)
	}
	if len(cases) == 0 {
		t.Fatal("buildDiffCases enumerated nothing, so there is nothing for the golden to match")
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
			continue // protoc rejected this shape at generation time
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

	if checked != len(golden) {
		t.Fatalf("compared %d cases against a golden holding %d -- enumeration and golden "+
			"disagree about the corpus, so any agreement count is not about the whole of it",
			checked, len(golden))
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

	golden, _ := readProtocGolden(t)
	if len(golden) == 0 {
		t.Fatalf("%s carries no accepted cases", protocGoldenPath)
	}

	positions := map[diffPosition]int{}
	relationships := map[string]int{}
	shadowed := 0
	for key := range golden {
		parts := strings.Split(key, "|")
		if len(parts) != 5 {
			t.Fatalf("malformed golden key %q", key)
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
		t.Error("no accepted case declares a shadowing type in the referring scope, so the " +
			"first-component-outward rule is unmeasured here")
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

	accepted := map[string]string{}
	rejected := 0
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
			// A protoc rejection is data, not a failure: it means the shape
			// cannot come out of a .proto file and Go's answer is unconstrained.
			_ = out
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
		var compiled *descriptorpb.FileDescriptorProto
		for _, fd := range set.File {
			if fd.GetName() == diffMainPath {
				compiled = fd
			}
		}
		if compiled == nil {
			t.Fatalf("protoc produced no descriptor for %s (case %s)", diffMainPath, c.key())
		}

		crossCheckStructure(t, c, compiled)

		resolved := c.readPosition(compiled)
		if resolved == "" || resolved[0] != '.' {
			t.Fatalf("protoc resolved case %s to %q, which is not an absolute name -- the probe "+
				"is reading the wrong position", c.key(), resolved)
		}
		accepted[c.key()] = resolved
	}

	if len(accepted) == 0 {
		t.Fatalf("protoc accepted none of the %d cases; the corpus would assert nothing", len(cases))
	}

	keys := make([]string, 0, len(accepted))
	for k := range accepted {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out strings.Builder
	fmt.Fprintf(&out, "# protoc resolution ground truth for absolutizeFieldTypeNames.\n")
	fmt.Fprintf(&out, "# Regenerate: go test ./pkg/recordlayer -run TestAbsolutizeAgreesWithProtoc -update-protoc-golden\n")
	fmt.Fprintf(&out, "# key = mainPkg|depPkg|shadow|position|asWritten   (- means unpackaged)\n")
	fmt.Fprintf(&out, "#accepted %d\n", len(accepted))
	fmt.Fprintf(&out, "#rejected %d\n", rejected)
	for _, k := range keys {
		fmt.Fprintf(&out, "%s\t%s\n", k, accepted[k])
	}

	if err := os.MkdirAll(filepath.Dir(protocGoldenPath), 0o755); err != nil {
		t.Fatalf("creating testdata dir: %v", err)
	}
	if err := os.WriteFile(protocGoldenPath, []byte(out.String()), 0o600); err != nil {
		t.Fatalf("writing %s: %v", protocGoldenPath, err)
	}
	t.Logf("wrote %s: %d accepted, %d rejected by protoc", protocGoldenPath, len(accepted), rejected)
}

// crossCheckStructure catches DRIFT between the two renderings of a case.
//
// The test builds its input as a descriptor while the generator hands protoc
// .proto text, so the corpus is only meaningful while those two describe the
// same file. Nothing else would notice them parting: the expectation would
// simply become a fact about a file the test never builds, and the differential
// would keep reporting agreement about the wrong thing.
func crossCheckStructure(t *testing.T, c diffCase, compiled *descriptorpb.FileDescriptorProto) {
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
}
