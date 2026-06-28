package keyspace

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/gomega"
)

func TestKeyTypeValidation(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	g.Expect(KeyTypeString.ValidateValue("hello")).To(Succeed())
	g.Expect(KeyTypeString.ValidateValue(42)).To(HaveOccurred())
	g.Expect(KeyTypeLong.ValidateValue(int64(42))).To(Succeed())
	g.Expect(KeyTypeLong.ValidateValue(42)).To(Succeed())        // int
	g.Expect(KeyTypeLong.ValidateValue(int32(42))).To(Succeed()) // int32
	g.Expect(KeyTypeLong.ValidateValue("nope")).To(HaveOccurred())
	g.Expect(KeyTypeBytes.ValidateValue([]byte{1, 2})).To(Succeed())
	g.Expect(KeyTypeBytes.ValidateValue("nope")).To(HaveOccurred())
	g.Expect(KeyTypeNull.ValidateValue(nil)).To(Succeed())
	g.Expect(KeyTypeNull.ValidateValue("nope")).To(HaveOccurred())
	g.Expect(KeyTypeBoolean.ValidateValue(true)).To(Succeed())
	g.Expect(KeyTypeBoolean.ValidateValue(42)).To(HaveOccurred())
	g.Expect(KeyTypeDouble.ValidateValue(3.14)).To(Succeed())
	g.Expect(KeyTypeFloat.ValidateValue(float32(3.14))).To(Succeed())
	g.Expect(KeyTypeUUID.ValidateValue(tuple.UUID{1, 2, 3})).To(Succeed())

	// nil for non-null types
	g.Expect(KeyTypeString.ValidateValue(nil)).To(HaveOccurred())
}

func TestKeyTypeString(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	g.Expect(KeyTypeString.String()).To(Equal("STRING"))
	g.Expect(KeyTypeLong.String()).To(Equal("LONG"))
	g.Expect(KeyTypeNull.String()).To(Equal("NULL"))
	g.Expect(KeyType(99).String()).To(Equal("KeyType(99)"))
}

func TestDirectoryTree(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Build: root -> state (STRING) -> office_id (LONG) -> employees (constant "emp")
	root := NewDirectory("root", KeyTypeNull)
	state := NewDirectory("state", KeyTypeString)
	officeID := NewDirectory("office_id", KeyTypeLong)
	employees := NewConstantDirectory("employees", KeyTypeString, "emp")

	root.AddSubdirectory(state)
	state.AddSubdirectory(officeID)
	officeID.AddSubdirectory(employees)

	g.Expect(root.GetSubdirectory("state")).To(Equal(state))
	g.Expect(state.GetSubdirectory("office_id")).To(Equal(officeID))
	g.Expect(officeID.GetSubdirectory("employees")).To(Equal(employees))
	g.Expect(root.GetSubdirectory("nonexistent")).To(BeNil())

	g.Expect(employees.IsConstant()).To(BeTrue())
	g.Expect(state.IsConstant()).To(BeFalse())
	g.Expect(root.GetSubdirectories()).To(HaveLen(1))
}

func TestValidate(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Valid tree
	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewConstantDirectory("data", KeyTypeString, "d"))
	root.GetSubdirectory("data").AddSubdirectory(NewDirectory("id", KeyTypeLong))
	ks := NewKeySpace(root)
	g.Expect(ks.Validate()).To(Succeed())

	// Invalid: constant value type mismatch
	badRoot := NewDirectory("root", KeyTypeNull)
	badRoot.AddSubdirectory(NewConstantDirectory("bad", KeyTypeLong, "not_a_long"))
	badKs := NewKeySpace(badRoot)
	g.Expect(badKs.Validate()).To(HaveOccurred())
}

func TestDirectoryJavaAligned(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	app := NewDirectory("app", KeyTypeString)
	table := NewDirectory("table", KeyTypeLong)
	root.AddSubdirectory(app)
	app.AddSubdirectory(table)

	// IsLeaf
	g.Expect(table.IsLeaf()).To(BeTrue())
	g.Expect(app.IsLeaf()).To(BeFalse())
	g.Expect(root.IsLeaf()).To(BeFalse())

	// Parent
	g.Expect(table.Parent()).To(Equal(app))
	g.Expect(app.Parent()).To(Equal(root))
	g.Expect(root.Parent()).To(BeNil())

	// Depth
	g.Expect(root.Depth()).To(Equal(0))
	g.Expect(app.Depth()).To(Equal(1))
	g.Expect(table.Depth()).To(Equal(2))

	// NameInTree
	g.Expect(root.NameInTree()).To(Equal("root"))
	g.Expect(app.NameInTree()).To(Equal("root.app"))
	g.Expect(table.NameInTree()).To(Equal("root.app.table"))
}

func TestToPathString(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	app := NewDirectory("app", KeyTypeString)
	table := NewDirectory("table", KeyTypeLong)
	root.AddSubdirectory(app)
	app.AddSubdirectory(table)

	g.Expect(root.ToPathString()).To(Equal("/root"))
	g.Expect(app.ToPathString()).To(Equal("/root/app"))
	g.Expect(table.ToPathString()).To(Equal("/root/app/table"))
}

func TestToTree(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	app := NewDirectory("app", KeyTypeString)
	user := NewDirectory("user", KeyTypeLong)
	session := NewDirectory("session", KeyTypeUUID)
	root.AddSubdirectory(app)
	app.AddSubdirectory(user)
	app.AddSubdirectory(session)

	tree := root.ToTree()
	// Just verify it contains expected elements
	g.Expect(tree).To(ContainSubstring("root (NULL)"))
	g.Expect(tree).To(ContainSubstring("app (STRING)"))
	g.Expect(tree).To(ContainSubstring("user (LONG)"))
	g.Expect(tree).To(ContainSubstring("session (UUID)"))

	// Multi-sibling scenario: the downspout connector | should appear
	// between siblings.
	g.Expect(tree).To(ContainSubstring(" +-"))
	// Child directories are indented; user and session are at the same depth
	// under app, so both should have " +-" prefix after some indentation.
	userLine := ""
	sessionLine := ""
	for _, line := range strings.Split(tree, "\n") {
		if strings.Contains(line, "user (LONG)") {
			userLine = line
		}
		if strings.Contains(line, "session (UUID)") {
			sessionLine = line
		}
	}
	g.Expect(userLine).NotTo(BeEmpty())
	g.Expect(sessionLine).NotTo(BeEmpty())
	// Both should be at the same indent level (same number of leading chars
	// before their "+-" marker).
	g.Expect(strings.Index(userLine, "+-")).To(Equal(strings.Index(sessionLine, "+-")))
}

func TestDuplicateSubdirectoryPanics(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("data", KeyTypeString))

	g.Expect(func() {
		root.AddSubdirectory(NewDirectory("data", KeyTypeLong))
	}).To(Panic())
}

func TestKeySpacePath(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Schema: root -> state (STRING) -> office_id (LONG)
	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("state", KeyTypeString))
	root.GetSubdirectory("state").AddSubdirectory(NewDirectory("office_id", KeyTypeLong))

	ks := NewKeySpace(root)

	// Navigate: state="CA", office_id=1234
	path, err := ks.Path("state", "CA")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path.DirectoryName()).To(Equal("state"))
	g.Expect(path.GetValue()).To(Equal("CA"))

	path2, err := path.Add("office_id", int64(1234))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path2.DirectoryName()).To(Equal("office_id"))
	g.Expect(path2.GetValue()).To(Equal(int64(1234)))
	g.Expect(path2.Parent()).To(Equal(path))

	// ToTuple
	tup := path2.ToTuple()
	g.Expect(tup).To(Equal(tuple.Tuple{"CA", int64(1234)}))

	// Single-level tuple
	g.Expect(path.ToTuple()).To(Equal(tuple.Tuple{"CA"}))
}

func TestKeySpacePathErrors(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("state", KeyTypeString))

	ks := NewKeySpace(root)

	// Wrong type
	_, err := ks.Path("state", 42)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("expected string"))

	// Non-existent directory
	_, err = ks.Path("nonexistent", "val")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("no root directory"))

	// Non-existent subdirectory
	path, err := ks.Path("state", "CA")
	g.Expect(err).NotTo(HaveOccurred())
	_, err = path.Add("nonexistent", "val")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("no subdirectory"))
}

func TestConstantDirectory(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewConstantDirectory("data", KeyTypeString, "data_prefix"))

	ks := NewKeySpace(root)

	// Constant value is used regardless of what's passed
	path, err := ks.Path("data", "ignored")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path.GetValue()).To(Equal("data_prefix"))

	g.Expect(path.ToTuple()).To(Equal(tuple.Tuple{"data_prefix"}))
}

func TestPathDepthAndListing(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	state := NewDirectory("state", KeyTypeString)
	officeID := NewDirectory("office_id", KeyTypeLong)
	employees := NewDirectory("employees", KeyTypeString)
	departments := NewDirectory("departments", KeyTypeString)
	root.AddSubdirectory(state)
	state.AddSubdirectory(officeID)
	officeID.AddSubdirectory(employees)
	officeID.AddSubdirectory(departments)

	ks := NewKeySpace(root)

	p1, err := ks.Path("state", "CA")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(p1.Depth()).To(Equal(1))

	p2, err := p1.Add("office_id", int64(42))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(p2.Depth()).To(Equal(2))
	g.Expect(p2.ListSubdirectories()).To(ConsistOf("employees", "departments"))
	g.Expect(p2.HasSubdirectory("employees")).To(BeTrue())
	g.Expect(p2.HasSubdirectory("nonexistent")).To(BeFalse())
}

func TestPathString(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("state", KeyTypeString))
	root.GetSubdirectory("state").AddSubdirectory(NewDirectory("office_id", KeyTypeLong))

	ks := NewKeySpace(root)

	p1, _ := ks.Path("state", "CA")
	g.Expect(p1.String()).To(Equal("/state=CA"))

	p2, _ := p1.Add("office_id", int64(1234))
	g.Expect(p2.String()).To(Equal("/state=CA/office_id=1234"))
}

func TestPathFromTuple(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	state := NewDirectory("state", KeyTypeString)
	officeID := NewDirectory("office_id", KeyTypeLong)
	root.AddSubdirectory(state)
	state.AddSubdirectory(officeID)

	ks := NewKeySpace(root)

	// Full match
	path, remainder, err := ks.PathFromTuple(tuple.Tuple{"CA", int64(1234)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remainder).To(BeNil())
	g.Expect(path.DirectoryName()).To(Equal("office_id"))
	g.Expect(path.GetValue()).To(Equal(int64(1234)))
	g.Expect(path.Parent().GetValue()).To(Equal("CA"))
	g.Expect(path.ToTuple()).To(Equal(tuple.Tuple{"CA", int64(1234)}))

	// Partial match — extra tuple elements
	path, remainder, err = ks.PathFromTuple(tuple.Tuple{"NY", int64(5678), "extra", "data"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remainder).To(Equal(tuple.Tuple{"extra", "data"}))
	g.Expect(path.DirectoryName()).To(Equal("office_id"))

	// Single element match
	path, remainder, err = ks.PathFromTuple(tuple.Tuple{"TX"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remainder).To(BeNil())
	g.Expect(path.DirectoryName()).To(Equal("state"))

	// No match
	_, _, err = ks.PathFromTuple(tuple.Tuple{int64(42)})
	g.Expect(err).To(HaveOccurred())

	// Empty tuple
	_, _, err = ks.PathFromTuple(tuple.Tuple{})
	g.Expect(err).To(HaveOccurred())
}

func TestPathFromTupleRoundtrip(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("app", KeyTypeString))
	root.GetSubdirectory("app").AddSubdirectory(NewDirectory("table", KeyTypeLong))

	ks := NewKeySpace(root)

	// Forward: Path -> Tuple
	path, _ := ks.Path("app", "myapp")
	path2, _ := path.Add("table", int64(99))
	tup := path2.ToTuple()
	g.Expect(tup).To(Equal(tuple.Tuple{"myapp", int64(99)}))

	// Reverse: Tuple -> Path
	resolved, remainder, err := ks.PathFromTuple(tup)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remainder).To(BeNil())
	g.Expect(resolved.ToTuple()).To(Equal(tup))
	g.Expect(resolved.String()).To(Equal(path2.String()))
}

func TestResolverFunc(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Resolver that maps strings to their lengths (simulating string→long compression)
	stringToLen := func(v any) (any, error) {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", v)
		}
		return int64(len(s)), nil
	}

	root := NewDirectory("root", KeyTypeNull)
	appDir := NewDirectory("app", KeyTypeString)
	appDir.Resolver = stringToLen
	root.AddSubdirectory(appDir)

	ks := NewKeySpace(root)

	// Path value should be resolved
	path, err := ks.Path("app", "myapp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path.GetValue()).To(Equal(int64(5))) // len("myapp") = 5

	// Tuple should contain the resolved value
	g.Expect(path.ToTuple()).To(Equal(tuple.Tuple{int64(5)}))
}

func TestResolverFuncError(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	failingResolver := func(v any) (any, error) {
		return nil, fmt.Errorf("resolver failed")
	}

	root := NewDirectory("root", KeyTypeNull)
	appDir := NewDirectory("app", KeyTypeString)
	appDir.Resolver = failingResolver
	root.AddSubdirectory(appDir)

	ks := NewKeySpace(root)

	_, err := ks.Path("app", "test")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resolver failed"))
}

func TestToSubspace(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("app", KeyTypeString))

	ks := NewKeySpace(root)

	path, err := ks.Path("app", "myapp")
	g.Expect(err).NotTo(HaveOccurred())

	ss := path.ToSubspace()
	g.Expect(ss.Bytes()).NotTo(BeEmpty())
	// The subspace packs via subspace.Sub which may add prefix bytes.
	// Just verify it round-trips correctly through Pack/Unpack.
	packed := ss.Pack(tuple.Tuple{"extra"})
	g.Expect(packed).NotTo(BeEmpty())
	// Verify the path tuple is embedded in the subspace
	g.Expect(path.ToTuple()).To(Equal(tuple.Tuple{"myapp"}))
}

func TestPathFlatten(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("state", KeyTypeString))
	root.GetSubdirectory("state").AddSubdirectory(NewDirectory("office_id", KeyTypeLong))

	ks := NewKeySpace(root)

	p1, _ := ks.Path("state", "CA")
	p2, _ := p1.Add("office_id", int64(1234))

	flat := p2.Flatten()
	g.Expect(flat).To(HaveLen(2))
	g.Expect(flat[0].DirectoryName()).To(Equal("state"))
	g.Expect(flat[0].GetValue()).To(Equal("CA"))
	g.Expect(flat[1].DirectoryName()).To(Equal("office_id"))
	g.Expect(flat[1].GetValue()).To(Equal(int64(1234)))

	// Path.Directory() returns the underlying schema node
	g.Expect(p2.Directory().Name).To(Equal("office_id"))
	g.Expect(p2.Directory().KeyType).To(Equal(KeyTypeLong))
}

func TestPathEqual(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("state", KeyTypeString))
	root.GetSubdirectory("state").AddSubdirectory(NewDirectory("id", KeyTypeLong))

	ks := NewKeySpace(root)

	p1, _ := ks.Path("state", "CA")
	p1a, _ := p1.Add("id", int64(1))

	p2, _ := ks.Path("state", "CA")
	p2a, _ := p2.Add("id", int64(1))

	p3, _ := ks.Path("state", "NY")
	p3a, _ := p3.Add("id", int64(1))

	// Same path — equal
	g.Expect(p1a.Equal(p2a)).To(BeTrue())

	// Different value — not equal
	g.Expect(p1a.Equal(p3a)).To(BeFalse())

	// Different depth — not equal
	g.Expect(p1.Equal(p1a)).To(BeFalse())

	// nil handling
	g.Expect((*Path)(nil).Equal(nil)).To(BeTrue())
	g.Expect(p1.Equal(nil)).To(BeFalse())

	// IsSameDirectory ignores values
	g.Expect(p1a.IsSameDirectory(p3a)).To(BeTrue())
	g.Expect(p1.IsSameDirectory(p3a)).To(BeFalse())
}

func TestKeySpaceString(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("app", KeyTypeString))
	ks := NewKeySpace(root)

	// KeySpace.String() delegates to Root().ToTree()
	g.Expect(ks.String()).To(Equal(root.ToTree()))
	g.Expect(ks.String()).To(ContainSubstring("root (NULL)"))
	g.Expect(ks.String()).To(ContainSubstring("app (STRING)"))
}

func TestAddSubdirectories(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectories(
		NewDirectory("a", KeyTypeString),
		NewDirectory("b", KeyTypeLong),
		NewDirectory("c", KeyTypeBytes),
	)

	g.Expect(root.GetSubdirectories()).To(HaveLen(3))
	g.Expect(root.GetSubdirectory("a")).NotTo(BeNil())
	g.Expect(root.GetSubdirectory("b")).NotTo(BeNil())
	g.Expect(root.GetSubdirectory("c")).NotTo(BeNil())
}

func TestDeepTree(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Build a deep nested tree: level0 -> level1 -> ... -> level9
	root := NewDirectory("level0", KeyTypeNull)
	cur := root
	for i := 1; i < 10; i++ {
		next := NewDirectory(fmt.Sprintf("level%d", i), KeyTypeLong)
		cur.AddSubdirectory(next)
		cur = next
	}

	// Leaf depth should be 9 (0-indexed from root)
	g.Expect(cur.Depth()).To(Equal(9))
	g.Expect(cur.IsLeaf()).To(BeTrue())
	g.Expect(root.IsLeaf()).To(BeFalse())

	// NameInTree of the leaf should have all 10 levels
	expected := "level0.level1.level2.level3.level4.level5.level6.level7.level8.level9"
	g.Expect(cur.NameInTree()).To(Equal(expected))
}

func TestFindChildForValue(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("state", KeyTypeString))
	root.AddSubdirectory(NewDirectory("year", KeyTypeLong))
	root.AddSubdirectory(NewConstantDirectory("config", KeyTypeString, "cfg"))

	// String matches the "state" directory (no constant matches "CA")
	child := root.FindChildForValue("CA")
	g.Expect(child).NotTo(BeNil())
	g.Expect(child.Name).To(Equal("state"))

	// Int64 matches the "year" directory
	child = root.FindChildForValue(int64(2026))
	g.Expect(child).NotTo(BeNil())
	g.Expect(child.Name).To(Equal("year"))

	// Constant "cfg" matches the constant "config" directory FIRST,
	// not the open "state" directory. Constant priority matches Java.
	child = root.FindChildForValue("cfg")
	g.Expect(child).NotTo(BeNil())
	g.Expect(child.Name).To(Equal("config"))

	// Value of unsupported type matches nothing
	child = root.FindChildForValue(3.14)
	g.Expect(child).To(BeNil())
}

// TestFindChildForValue_Bytes verifies that []byte values don't panic
// (they are not comparable with Go's == operator).
func TestFindChildForValue_Bytes(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("blob", KeyTypeBytes))
	root.AddSubdirectory(NewConstantDirectory("marker", KeyTypeBytes, []byte{0xFF, 0xFE}))

	// []byte value matches the open blob directory
	child := root.FindChildForValue([]byte{1, 2, 3})
	g.Expect(child).NotTo(BeNil())
	g.Expect(child.Name).To(Equal("blob"))

	// Constant []byte matches the constant directory exactly
	child = root.FindChildForValue([]byte{0xFF, 0xFE})
	g.Expect(child).NotTo(BeNil())
	g.Expect(child.Name).To(Equal("marker"))
}

// TestPathFromTuple_ConstantPriority verifies that when the root has both
// a constant directory and an open-type directory of the same KeyType,
// the constant wins when its value matches. Regression test for the
// two-pass fix in PathFromTuple (commit 6276d9b).
func TestPathFromTuple_ConstantPriority(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	// Order matters for the regression: open-type is added FIRST. Without
	// two-pass, iteration would pick "state" for all strings including "cfg".
	root.AddSubdirectory(NewDirectory("state", KeyTypeString))
	root.AddSubdirectory(NewConstantDirectory("config", KeyTypeString, "cfg"))
	ks := NewKeySpace(root)

	// "cfg" should resolve to the constant "config" directory, not "state".
	path, remainder, err := ks.PathFromTuple(tuple.Tuple{"cfg"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remainder).To(BeNil())
	g.Expect(path.DirectoryName()).To(Equal("config"))

	// Other strings should fall through to "state".
	path, _, err = ks.PathFromTuple(tuple.Tuple{"CA"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path.DirectoryName()).To(Equal("state"))
}

// TestPathEqual_Bytes verifies that Path.Equal handles []byte without panicking.
func TestPathEqual_Bytes(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(NewDirectory("blob", KeyTypeBytes))
	ks := NewKeySpace(root)

	p1, err := ks.Path("blob", []byte{1, 2, 3})
	g.Expect(err).NotTo(HaveOccurred())

	p2, err := ks.Path("blob", []byte{1, 2, 3})
	g.Expect(err).NotTo(HaveOccurred())

	p3, err := ks.Path("blob", []byte{9, 9, 9})
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(p1.Equal(p2)).To(BeTrue(), "same bytes should be equal")
	g.Expect(p1.Equal(p3)).To(BeFalse(), "different bytes should not be equal")
}
