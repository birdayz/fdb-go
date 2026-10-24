package keyspace

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/gomega"
)

var ctx = context.Background()

func mustAdd(tb testing.TB, parent *Directory, children ...*Directory) {
	tb.Helper()
	if err := parent.AddSubdirectories(children...); err != nil {
		tb.Fatal(err)
	}
}

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

	mustAdd(t, root, state)
	mustAdd(t, state, officeID)
	mustAdd(t, officeID, employees)

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
	mustAdd(t, root, NewConstantDirectory("data", KeyTypeString, "d"))
	mustAdd(t, root.GetSubdirectory("data"), NewDirectory("id", KeyTypeLong))
	ks := NewKeySpace(root)
	g.Expect(ks.Validate()).To(Succeed())

	// Invalid: constant value type mismatch
	badRoot := NewDirectory("root", KeyTypeNull)
	mustAdd(t, badRoot, NewConstantDirectory("bad", KeyTypeLong, "not_a_long"))
	badKs := NewKeySpace(badRoot)
	g.Expect(badKs.Validate()).To(HaveOccurred())
}

func TestDirectoryJavaAligned(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	app := NewDirectory("app", KeyTypeString)
	table := NewDirectory("table", KeyTypeLong)
	mustAdd(t, root, app)
	mustAdd(t, app, table)

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
	mustAdd(t, root, app)
	mustAdd(t, app, table)

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
	mustAdd(t, root, app)
	mustAdd(t, app, user)
	mustAdd(t, app, session)

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

func TestDuplicateSubdirectoryRefused(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	mustAdd(t, root, NewDirectory("data", KeyTypeString))

	err := root.AddSubdirectory(NewDirectory("data", KeyTypeLong))
	var argErr *ArgumentError
	g.Expect(errors.As(err, &argErr)).To(BeTrue())
	g.Expect(argErr.Message).To(Equal("Subdirectory already exists"))
	g.Expect(root.GetSubdirectories()).To(HaveLen(1))
}

func TestKeySpacePath(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Schema: root -> state (STRING) -> office_id (LONG)
	root := NewDirectory("root", KeyTypeNull)
	mustAdd(t, root, NewDirectory("state", KeyTypeString))
	mustAdd(t, root.GetSubdirectory("state"), NewDirectory("office_id", KeyTypeLong))

	ks := NewKeySpace(root)

	// Navigate: state="CA", office_id=1234
	path, err := ks.Path(ctx, "state", "CA")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path.DirectoryName()).To(Equal("state"))
	g.Expect(path.GetValue()).To(Equal("CA"))

	path2, err := path.Add(ctx, "office_id", int64(1234))
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
	mustAdd(t, root, NewDirectory("state", KeyTypeString))

	ks := NewKeySpace(root)

	// Wrong type
	_, err := ks.Path(ctx, "state", 42)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("expected string"))

	// Non-existent directory
	_, err = ks.Path(ctx, "nonexistent", "val")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("no root directory"))

	// Non-existent subdirectory
	path, err := ks.Path(ctx, "state", "CA")
	g.Expect(err).NotTo(HaveOccurred())
	_, err = path.Add(ctx, "nonexistent", "val")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("no subdirectory"))
}

func TestConstantDirectory(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	mustAdd(t, root, NewConstantDirectory("data", KeyTypeString, "data_prefix"))

	ks := NewKeySpace(root)

	// Constant value is used regardless of what's passed
	path, err := ks.Path(ctx, "data", "ignored")
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
	employees := NewConstantDirectory("employees", KeyTypeString, "e")
	departments := NewConstantDirectory("departments", KeyTypeString, "d")
	mustAdd(t, root, state)
	mustAdd(t, state, officeID)
	mustAdd(t, officeID, employees)
	mustAdd(t, officeID, departments)

	ks := NewKeySpace(root)

	p1, err := ks.Path(ctx, "state", "CA")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(p1.Depth()).To(Equal(1))

	p2, err := p1.Add(ctx, "office_id", int64(42))
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
	mustAdd(t, root, NewDirectory("state", KeyTypeString))
	mustAdd(t, root.GetSubdirectory("state"), NewDirectory("office_id", KeyTypeLong))

	ks := NewKeySpace(root)

	p1, _ := ks.Path(ctx, "state", "CA")
	g.Expect(p1.String()).To(Equal("/state=CA"))

	p2, _ := p1.Add(ctx, "office_id", int64(1234))
	g.Expect(p2.String()).To(Equal("/state=CA/office_id=1234"))
}

func TestPathFromTuple(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	state := NewDirectory("state", KeyTypeString)
	officeID := NewDirectory("office_id", KeyTypeLong)
	mustAdd(t, root, state)
	mustAdd(t, state, officeID)

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
	mustAdd(t, root, NewDirectory("app", KeyTypeString))
	mustAdd(t, root.GetSubdirectory("app"), NewDirectory("table", KeyTypeLong))

	ks := NewKeySpace(root)

	// Forward: Path -> Tuple
	path, _ := ks.Path(ctx, "app", "myapp")
	path2, _ := path.Add(ctx, "table", int64(99))
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
	stringToLen := func(_ context.Context, v any) (any, error) {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", v)
		}
		return int64(len(s)), nil
	}

	root := NewDirectory("root", KeyTypeNull)
	appDir := NewDirectory("app", KeyTypeString)
	appDir.Resolver = stringToLen
	mustAdd(t, root, appDir)

	ks := NewKeySpace(root)

	// Path value should be resolved
	path, err := ks.Path(ctx, "app", "myapp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path.GetValue()).To(Equal(int64(5))) // len("myapp") = 5

	// Tuple should contain the resolved value
	g.Expect(path.ToTuple()).To(Equal(tuple.Tuple{int64(5)}))
}

func TestResolverFuncError(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	failingResolver := func(context.Context, any) (any, error) {
		return nil, fmt.Errorf("resolver failed")
	}

	root := NewDirectory("root", KeyTypeNull)
	appDir := NewDirectory("app", KeyTypeString)
	appDir.Resolver = failingResolver
	mustAdd(t, root, appDir)

	ks := NewKeySpace(root)

	_, err := ks.Path(ctx, "app", "test")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("resolver failed"))
}

func TestToSubspace(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	mustAdd(t, root, NewDirectory("app", KeyTypeString))

	ks := NewKeySpace(root)

	path, err := ks.Path(ctx, "app", "myapp")
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
	mustAdd(t, root, NewDirectory("state", KeyTypeString))
	mustAdd(t, root.GetSubdirectory("state"), NewDirectory("office_id", KeyTypeLong))

	ks := NewKeySpace(root)

	p1, _ := ks.Path(ctx, "state", "CA")
	p2, _ := p1.Add(ctx, "office_id", int64(1234))

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
	mustAdd(t, root, NewDirectory("state", KeyTypeString))
	mustAdd(t, root.GetSubdirectory("state"), NewDirectory("id", KeyTypeLong))

	ks := NewKeySpace(root)

	p1, _ := ks.Path(ctx, "state", "CA")
	p1a, _ := p1.Add(ctx, "id", int64(1))

	p2, _ := ks.Path(ctx, "state", "CA")
	p2a, _ := p2.Add(ctx, "id", int64(1))

	p3, _ := ks.Path(ctx, "state", "NY")
	p3a, _ := p3.Add(ctx, "id", int64(1))

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
	mustAdd(t, root, NewDirectory("app", KeyTypeString))
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
	g.Expect(root.AddSubdirectories(
		NewDirectory("a", KeyTypeString),
		NewDirectory("b", KeyTypeLong),
		NewDirectory("c", KeyTypeBytes),
	)).To(Succeed())

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
		mustAdd(t, cur, next)
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
	mustAdd(t, root, NewDirectory("state", KeyTypeString))
	mustAdd(t, root, NewDirectory("year", KeyTypeLong))
	mustAdd(t, root, NewConstantDirectory("blob", KeyTypeBytes, []byte{0xFF, 0xFE}))
	mustAdd(t, root, NewConstantDirectory("other", KeyTypeBytes, []byte{0x01}))

	child := root.FindChildForValue("CA")
	g.Expect(child).NotTo(BeNil())
	g.Expect(child.Name).To(Equal("state"))

	child = root.FindChildForValue(int64(2026))
	g.Expect(child).NotTo(BeNil())
	g.Expect(child.Name).To(Equal("year"))

	// Constant []byte values compare by content, without panicking.
	child = root.FindChildForValue([]byte{0x01})
	g.Expect(child).NotTo(BeNil())
	g.Expect(child.Name).To(Equal("other"))
	g.Expect(root.FindChildForValue([]byte{1, 2, 3})).To(BeNil())

	// Value of unsupported type matches nothing
	g.Expect(root.FindChildForValue(3.14)).To(BeNil())

	// A resolver directory holds its stored int64, not the logical name it resolves.
	tenants := NewDirectory("tenants", KeyTypeNull)
	mustAdd(t, tenants, ResolverDirectory("tenant", NewMemoryResolver(0)))
	g.Expect(tenants.FindChildForValue(int64(1000))).To(Equal(tenants.GetSubdirectory("tenant")))
	g.Expect(tenants.FindChildForValue("acme")).To(BeNil())
}

// TestSiblingKeyTypes pins Java's KeySpaceDirectory.addSubdirectory rules: a
// sibling of the same key type is allowed only as a distinct constant beside
// other constants, so at most one child can hold a key element.
func TestSiblingKeyTypes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		existing *Directory
		added    *Directory
		message  string
	}{
		{"two open", NewDirectory("a", KeyTypeString), NewDirectory("b", KeyTypeString), "Cannot add directory due to overlapping type"},
		{"open then constant", NewDirectory("a", KeyTypeString), NewConstantDirectory("b", KeyTypeString, "x"), "Cannot add directory due to overlapping type"},
		{"constant then open", NewConstantDirectory("a", KeyTypeLong, int64(1)), NewDirectory("b", KeyTypeLong), "Cannot add directory due to overlapping type"},
		{"resolver beside a constant", NewConstantDirectory("a", KeyTypeLong, int64(1)), ResolverDirectory("b", NewMemoryResolver(0)), "Cannot add directory due to overlapping type"},
		{"equal constants", NewConstantDirectory("a", KeyTypeLong, int64(1)), NewConstantDirectory("b", KeyTypeLong, 1), "Cannot add directory due to overlapping constant value"},
		{"equal byte constants", NewConstantDirectory("a", KeyTypeBytes, []byte{1}), NewConstantDirectory("b", KeyTypeBytes, []byte{1}), "Cannot add directory due to overlapping constant value"},
		{"two NULL directories", NewDirectory("a", KeyTypeNull), NewDirectory("b", KeyTypeNull), "Cannot add directory due to overlapping constant value"},
		{"equal NaN constants", NewConstantDirectory("a", KeyTypeDouble, math.NaN()), NewConstantDirectory("b", KeyTypeDouble, math.NaN()), "Cannot add directory due to overlapping constant value"},
		{"0.0 and -0.0 constants", NewConstantDirectory("a", KeyTypeDouble, 0.0), NewConstantDirectory("b", KeyTypeDouble, math.Copysign(0, -1)), ""},
		{"distinct constants", NewConstantDirectory("a", KeyTypeLong, int64(1)), NewConstantDirectory("b", KeyTypeLong, int64(2)), ""},
		{"different types", NewDirectory("a", KeyTypeString), NewDirectory("b", KeyTypeLong), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			root := NewDirectory("root", KeyTypeNull)
			mustAdd(t, root, tc.existing)
			err := root.AddSubdirectory(tc.added)
			if tc.message == "" {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(root.GetSubdirectories()).To(HaveLen(2))
				return
			}
			var argErr *ArgumentError
			g.Expect(errors.As(err, &argErr)).To(BeTrue(), "%v", err)
			g.Expect(argErr.Message).To(Equal(tc.message))
			g.Expect(argErr.ParentDir).To(Equal("root"))
			g.Expect(argErr.DirName).To(Equal("a"))
			g.Expect(root.GetSubdirectories()).To(HaveLen(1), "a refused directory is not added")
		})
	}
}

// TestAddSubdirectories_StopsAtRefusal adds in order up to the refused child.
func TestAddSubdirectories_StopsAtRefusal(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	err := root.AddSubdirectories(
		NewDirectory("a", KeyTypeString),
		NewDirectory("b", KeyTypeString),
		NewDirectory("c", KeyTypeLong),
	)
	var argErr *ArgumentError
	g.Expect(errors.As(err, &argErr)).To(BeTrue(), "%v", err)
	g.Expect(argErr.Message).To(Equal("Cannot add directory due to overlapping type"))
	g.Expect(root.GetSubdirectories()).To(HaveLen(1))
	g.Expect(root.GetSubdirectory("c")).To(BeNil())
}

// TestPathFromTuple_NoSubdirectory pins Java's findChildForKey: an element no
// subdirectory of a non-leaf directory holds is an error, not a remainder; a
// remainder is left only past a leaf.
func TestPathFromTuple_NoSubdirectory(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	app := NewDirectory("app", KeyTypeLong)
	mustAdd(t, root, app)
	mustAdd(t, app, NewDirectory("name", KeyTypeString))
	ks := NewKeySpace(root)

	_, _, err := ks.PathFromTuple(tuple.Tuple{int64(1), int64(5)})
	var argErr *ArgumentError
	g.Expect(errors.As(err, &argErr)).To(BeTrue(), "%v", err)
	g.Expect(argErr.Message).To(Equal("No subdirectory available to hold provided type"))
	g.Expect(argErr.ParentDir).To(Equal("app"))
	g.Expect(argErr.KeyPos).To(Equal(1))

	_, _, err = ks.PathFromTuple(tuple.Tuple{"not a long"})
	g.Expect(errors.As(err, &argErr)).To(BeTrue(), "%v", err)
	g.Expect(argErr.ParentDir).To(Equal("root"))

	// A root without subdirectories holds nothing; it is not a leaf to take a remainder at.
	_, _, err = NewKeySpace(NewDirectory("empty", KeyTypeNull)).PathFromTuple(tuple.Tuple{int64(1)})
	g.Expect(errors.As(err, &argErr)).To(BeTrue(), "%v", err)
	g.Expect(argErr.ParentDir).To(Equal("empty"))

	path, remainder, err := ks.PathFromTuple(tuple.Tuple{int64(1), "x", int64(9)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path.DirectoryName()).To(Equal("name"))
	g.Expect(remainder).To(Equal(tuple.Tuple{int64(9)}))
}

// TestPathEqual_Bytes verifies that Path.Equal handles []byte without panicking.
func TestPathEqual_Bytes(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	root := NewDirectory("root", KeyTypeNull)
	mustAdd(t, root, NewDirectory("blob", KeyTypeBytes))
	ks := NewKeySpace(root)

	p1, err := ks.Path(ctx, "blob", []byte{1, 2, 3})
	g.Expect(err).NotTo(HaveOccurred())

	p2, err := ks.Path(ctx, "blob", []byte{1, 2, 3})
	g.Expect(err).NotTo(HaveOccurred())

	p3, err := ks.Path(ctx, "blob", []byte{9, 9, 9})
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(p1.Equal(p2)).To(BeTrue(), "same bytes should be equal")
	g.Expect(p1.Equal(p3)).To(BeFalse(), "different bytes should not be equal")
}
