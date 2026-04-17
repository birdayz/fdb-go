// Package keyspace provides a logical directory tree abstraction over FDB.
//
// KeySpace defines a schema of named directories with typed keys.
// KeySpacePath navigates the tree to produce FDB-ready tuples and subspaces.
//
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.keyspace package.
package keyspace

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// KeyType defines the FDB tuple type for a directory's key.
// Matches Java's KeySpaceDirectory.KeyType enum.
type KeyType int

const (
	KeyTypeNull    KeyType = iota // nil values
	KeyTypeBytes                  // []byte
	KeyTypeString                 // string
	KeyTypeLong                   // int64 (also accepts int, int32)
	KeyTypeFloat                  // float32
	KeyTypeDouble                 // float64
	KeyTypeBoolean                // bool
	KeyTypeUUID                   // tuple.UUID
)

// String returns the name of the key type.
func (t KeyType) String() string {
	switch t {
	case KeyTypeNull:
		return "NULL"
	case KeyTypeBytes:
		return "BYTES"
	case KeyTypeString:
		return "STRING"
	case KeyTypeLong:
		return "LONG"
	case KeyTypeFloat:
		return "FLOAT"
	case KeyTypeDouble:
		return "DOUBLE"
	case KeyTypeBoolean:
		return "BOOLEAN"
	case KeyTypeUUID:
		return "UUID"
	default:
		return fmt.Sprintf("KeyType(%d)", t)
	}
}

// ValidateValue checks if a value is compatible with this key type.
func (t KeyType) ValidateValue(v any) error {
	if v == nil {
		if t == KeyTypeNull {
			return nil
		}
		return fmt.Errorf("nil value not allowed for key type %s", t)
	}
	switch t {
	case KeyTypeNull:
		return fmt.Errorf("non-nil value for NULL key type")
	case KeyTypeBytes:
		if _, ok := v.([]byte); !ok {
			return fmt.Errorf("expected []byte for BYTES key type, got %T", v)
		}
	case KeyTypeString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string for STRING key type, got %T", v)
		}
	case KeyTypeLong:
		switch v.(type) {
		case int64, int, int32:
		default:
			return fmt.Errorf("expected int64/int/int32 for LONG key type, got %T", v)
		}
	case KeyTypeFloat:
		if _, ok := v.(float32); !ok {
			return fmt.Errorf("expected float32 for FLOAT key type, got %T", v)
		}
	case KeyTypeDouble:
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("expected float64 for DOUBLE key type, got %T", v)
		}
	case KeyTypeBoolean:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected bool for BOOLEAN key type, got %T", v)
		}
	case KeyTypeUUID:
		if _, ok := v.(tuple.UUID); !ok {
			return fmt.Errorf("expected tuple.UUID for UUID key type, got %T", v)
		}
	}
	return nil
}

// ResolverFunc resolves a directory value before storing it in the path.
// For example, a DirectoryLayerDirectory would resolve a string name to
// a compact int64 via FDB's directory layer.
//
// The function receives the raw value and returns the resolved value.
// Phase 2 (LocatableResolver) will plug into this hook.
type ResolverFunc func(value any) (any, error)

// Directory is a node in the KeySpace tree. Each directory has a name,
// a key type, optional constant value, and optional children.
//
// Matches Java's KeySpaceDirectory.
type Directory struct {
	Name     string
	KeyType  KeyType
	Value    any          // constant value, or nil for any-value
	Resolver ResolverFunc // optional resolver for value transformation

	// inputValidator overrides KeyType.ValidateValue for input validation.
	// Used by ResolverDirectory which accepts strings (logical names) but
	// stores longs (resolved values). Matches Java's isValueValid pattern.
	inputValidator func(any) error

	parent   *Directory
	children []*Directory
	childMap map[string]*Directory
}

// validateInput checks a value against the directory's input contract.
// If inputValidator is set, uses that; otherwise uses KeyType.ValidateValue.
func (d *Directory) validateInput(value any) error {
	if d.inputValidator != nil {
		return d.inputValidator(value)
	}
	return d.KeyType.ValidateValue(value)
}

// NewDirectory creates a new directory node that accepts any value of the given type.
func NewDirectory(name string, keyType KeyType) *Directory {
	return &Directory{
		Name:     name,
		KeyType:  keyType,
		childMap: make(map[string]*Directory),
	}
}

// NewConstantDirectory creates a directory node with a fixed constant value.
func NewConstantDirectory(name string, keyType KeyType, value any) *Directory {
	return &Directory{
		Name:     name,
		KeyType:  keyType,
		Value:    value,
		childMap: make(map[string]*Directory),
	}
}

// AddSubdirectory adds a child directory. Returns the parent for chaining.
// Panics if a child with the same name already exists.
func (d *Directory) AddSubdirectory(child *Directory) *Directory {
	if _, exists := d.childMap[child.Name]; exists {
		panic(fmt.Sprintf("keyspace: duplicate subdirectory name %q in directory %q", child.Name, d.Name))
	}
	child.parent = d
	d.children = append(d.children, child)
	d.childMap[child.Name] = child
	return d
}

// AddSubdirectories adds multiple child directories in one call.
// Returns the parent for chaining. Panics on duplicate names.
func (d *Directory) AddSubdirectories(children ...*Directory) *Directory {
	for _, child := range children {
		d.AddSubdirectory(child)
	}
	return d
}

// GetSubdirectory returns a child directory by name, or nil if not found.
func (d *Directory) GetSubdirectory(name string) *Directory {
	return d.childMap[name]
}

// GetSubdirectories returns all child directories.
func (d *Directory) GetSubdirectories() []*Directory {
	return d.children
}

// IsConstant returns true if this directory has a fixed value.
func (d *Directory) IsConstant() bool {
	return d.Value != nil
}

// IsLeaf returns true if this directory has no children.
// Matches Java's KeySpaceDirectory.isLeaf().
func (d *Directory) IsLeaf() bool {
	return len(d.children) == 0
}

// Parent returns the parent directory, or nil if this is the root.
// Matches Java's KeySpaceDirectory.getParent().
func (d *Directory) Parent() *Directory {
	return d.parent
}

// Depth returns the distance from this directory to the root (0 for root).
// Matches Java's KeySpaceDirectory.depth().
func (d *Directory) Depth() int {
	n := 0
	for cur := d.parent; cur != nil; cur = cur.parent {
		n++
	}
	return n
}

// NameInTree returns the dot-separated path from root to this directory.
// Matches Java's KeySpaceDirectory.getNameInTree().
func (d *Directory) NameInTree() string {
	if d.parent == nil {
		return d.Name
	}
	return d.parent.NameInTree() + "." + d.Name
}

// ToPathString returns a slash-separated path from root to this directory.
// Matches Java's KeySpaceDirectory.toPathString().
func (d *Directory) ToPathString() string {
	var sb strings.Builder
	appendDirPath(&sb, d)
	return sb.String()
}

func appendDirPath(sb *strings.Builder, dir *Directory) {
	if dir.parent != nil {
		appendDirPath(sb, dir.parent)
	}
	sb.WriteString("/")
	sb.WriteString(dir.Name)
}

// ToTree returns an ASCII tree representation of this directory and its subtree.
// Matches Java's KeySpaceDirectory.toTree().
func (d *Directory) ToTree() string {
	var sb strings.Builder
	writeTreeLine(&sb, d, 0, false, nil)
	return sb.String()
}

func writeTreeLine(sb *strings.Builder, dir *Directory, indent int, hasSibling bool, downspouts []bool) {
	for i := 0; i < indent; i++ {
		if i < len(downspouts) && downspouts[i] {
			sb.WriteString(" | ")
		} else {
			sb.WriteString("   ")
		}
	}
	if dir.parent != nil {
		sb.WriteString(" +-")
	}
	sb.WriteString(dir.Name)
	sb.WriteString(" (")
	sb.WriteString(dir.KeyType.String())
	if dir.IsConstant() {
		sb.WriteString(fmt.Sprintf("=%v", dir.Value))
	}
	sb.WriteString(")\n")

	childDownspouts := make([]bool, indent+1)
	copy(childDownspouts, downspouts)
	childDownspouts[indent] = hasSibling

	for i, child := range dir.children {
		childHasSibling := i < len(dir.children)-1
		writeTreeLine(sb, child, indent+1, childHasSibling, childDownspouts)
	}
}

// KeySpace is the root of a directory tree. It holds one or more root directories.
//
// Matches Java's KeySpace.
type KeySpace struct {
	root *Directory
}

// NewKeySpace creates a new key space with the given root directory.
func NewKeySpace(root *Directory) *KeySpace {
	return &KeySpace{root: root}
}

// Root returns the root directory.
func (ks *KeySpace) Root() *Directory {
	return ks.root
}

// String returns a pretty-printed tree representation of the whole KeySpace schema.
func (ks *KeySpace) String() string {
	return ks.root.ToTree()
}

// Validate checks the tree for structural errors:
// - Constant values must match their declared key type
// - No nil children
func (ks *KeySpace) Validate() error {
	return validateDirectory(ks.root, nil)
}

func validateDirectory(d *Directory, path []string) error {
	if d == nil {
		return fmt.Errorf("keyspace: nil directory at path %v", path)
	}
	currentPath := append(path, d.Name)

	if d.IsConstant() {
		if err := d.KeyType.ValidateValue(d.Value); err != nil {
			return fmt.Errorf("keyspace: constant value in %v: %w", currentPath, err)
		}
	}

	for _, child := range d.children {
		if err := validateDirectory(child, currentPath); err != nil {
			return err
		}
	}
	return nil
}

// PathFromTuple resolves a tuple back to a path by matching values against
// the directory tree. Returns the deepest matching path and any remaining
// tuple elements that didn't match a directory.
//
// Matches Java's KeySpace.resolveFromKey / KeySpaceDirectory.pathFromKey.
func (ks *KeySpace) PathFromTuple(t tuple.Tuple) (*Path, tuple.Tuple, error) {
	if len(t) == 0 {
		return nil, nil, fmt.Errorf("keyspace: empty tuple")
	}

	// First pass: prefer constant directories with matching values.
	for _, dir := range ks.root.children {
		if dir.IsConstant() && recordTypeKeyEquals(dir.Value, t[0]) {
			path := &Path{directory: dir, value: t[0]}
			return resolveRemaining(path, t[1:])
		}
	}
	// Second pass: fall back to open-type directories.
	for _, dir := range ks.root.children {
		if dir.IsConstant() {
			continue
		}
		if err := dir.KeyType.ValidateValue(t[0]); err == nil {
			path := &Path{directory: dir, value: t[0]}
			return resolveRemaining(path, t[1:])
		}
	}

	return nil, t, fmt.Errorf("keyspace: no root directory matches tuple value %v (%T)", t[0], t[0])
}

// resolveRemaining walks deeper into the tree matching remaining tuple elements.
func resolveRemaining(path *Path, remaining tuple.Tuple) (*Path, tuple.Tuple, error) {
	for len(remaining) > 0 {
		matched := false
		// Prefer constant directories first (matches Java priority)
		for _, dir := range path.directory.children {
			if dir.IsConstant() && recordTypeKeyEquals(dir.Value, remaining[0]) {
				path = &Path{directory: dir, parent: path, value: remaining[0]}
				remaining = remaining[1:]
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		// Fall back to open-type matches
		for _, dir := range path.directory.children {
			if dir.IsConstant() {
				continue
			}
			if err := dir.KeyType.ValidateValue(remaining[0]); err == nil {
				path = &Path{directory: dir, parent: path, value: remaining[0]}
				remaining = remaining[1:]
				matched = true
				break
			}
		}
		if !matched {
			break // remaining elements don't match any child directory
		}
	}
	if len(remaining) > 0 {
		return path, remaining, nil
	}
	return path, nil, nil
}

// recordTypeKeyEquals compares values with int type normalization
// (FDB tuples decode ints as int64, but constants may be int).
// Uses reflect.DeepEqual to safely handle non-comparable types like []byte.
func recordTypeKeyEquals(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	aInt, aOk := toInt64(a)
	bInt, bOk := toInt64(b)
	return aOk && bOk && aInt == bInt
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	default:
		return 0, false
	}
}

// Path starts navigating the key space from a named root subdirectory with a value.
func (ks *KeySpace) Path(name string, value any) (*Path, error) {
	dir := ks.root.GetSubdirectory(name)
	if dir == nil {
		return nil, fmt.Errorf("keyspace: no root directory named %q", name)
	}
	if dir.IsConstant() {
		value = dir.Value
	} else if err := dir.validateInput(value); err != nil {
		return nil, fmt.Errorf("keyspace: directory %q: %w", name, err)
	}
	if dir.Resolver != nil {
		resolved, err := dir.Resolver(value)
		if err != nil {
			return nil, fmt.Errorf("keyspace: directory %q resolve: %w", name, err)
		}
		value = resolved
	}
	return &Path{
		directory: dir,
		value:     value,
	}, nil
}

// Path represents a position in the key space tree with a resolved value.
//
// Matches Java's KeySpacePath.
type Path struct {
	directory *Directory
	parent    *Path
	value     any
}

// Add navigates to a child directory with the given value.
func (p *Path) Add(name string, value any) (*Path, error) {
	dir := p.directory.GetSubdirectory(name)
	if dir == nil {
		return nil, fmt.Errorf("keyspace: directory %q has no subdirectory named %q", p.directory.Name, name)
	}
	if dir.IsConstant() {
		value = dir.Value
	} else if err := dir.validateInput(value); err != nil {
		return nil, fmt.Errorf("keyspace: directory %q.%q: %w", p.directory.Name, name, err)
	}
	if dir.Resolver != nil {
		resolved, err := dir.Resolver(value)
		if err != nil {
			return nil, fmt.Errorf("keyspace: directory %q.%q resolve: %w", p.directory.Name, name, err)
		}
		value = resolved
	}
	return &Path{
		directory: dir,
		parent:    p,
		value:     value,
	}, nil
}

// ToTuple converts this path to an FDB tuple containing all values from root to here.
func (p *Path) ToTuple() tuple.Tuple {
	// Count depth
	depth := 0
	for cur := p; cur != nil; cur = cur.parent {
		depth++
	}
	// Build tuple in reverse
	t := make(tuple.Tuple, depth)
	for cur := p; cur != nil; cur = cur.parent {
		depth--
		t[depth] = cur.value
	}
	return t
}

// ToSubspace converts this path to an FDB subspace.
func (p *Path) ToSubspace() subspace.Subspace {
	return subspace.Sub(p.ToTuple())
}

// DirectoryName returns the name of the current directory.
func (p *Path) DirectoryName() string {
	return p.directory.Name
}

// Value returns the resolved value at this path position.
func (p *Path) GetValue() any {
	return p.value
}

// Parent returns the parent path, or nil if this is a root path.
func (p *Path) Parent() *Path {
	return p.parent
}

// Depth returns the number of path elements from root to this position.
func (p *Path) Depth() int {
	n := 0
	for cur := p; cur != nil; cur = cur.parent {
		n++
	}
	return n
}

// ListSubdirectories returns the names of available child directories at this position.
func (p *Path) ListSubdirectories() []string {
	children := p.directory.GetSubdirectories()
	names := make([]string, len(children))
	for i, c := range children {
		names[i] = c.Name
	}
	return names
}

// HasSubdirectory returns true if a child directory with the given name exists.
func (p *Path) HasSubdirectory(name string) bool {
	return p.directory.GetSubdirectory(name) != nil
}

// ToRange returns an FDB key range that covers all entries under this path.
// Useful for scanning or clearing all data in a directory subtree.
func (p *Path) ToRange() (fdb.KeyRange, error) {
	ss := p.ToSubspace()
	begin, end := ss.FDBRangeKeys()
	return fdb.KeyRange{
		Begin: begin.FDBKey(),
		End:   end.FDBKey(),
	}, nil
}

// FullPath returns a human-readable representation of the path from root to here.
// E.g., "/state=CA/office_id=1234".
func (p *Path) String() string {
	if p.parent == nil {
		return fmt.Sprintf("/%s=%v", p.directory.Name, p.value)
	}
	return fmt.Sprintf("%s/%s=%v", p.parent.String(), p.directory.Name, p.value)
}

// Flatten returns all path elements from root to this position, in order.
// Matches Java's KeySpacePath.flatten().
func (p *Path) Flatten() []*Path {
	depth := p.Depth()
	result := make([]*Path, depth)
	cur := p
	for i := depth - 1; i >= 0; i-- {
		result[i] = cur
		cur = cur.parent
	}
	return result
}

// Equal reports whether two paths reference the same directories with the
// same values at each level. Directories are compared by pointer identity
// (schema reuse) and values via recordTypeKeyEquals (reflect.DeepEqual +
// int type normalization, safe for []byte and other non-comparable types).
func (p *Path) Equal(other *Path) bool {
	if p == nil || other == nil {
		return p == other
	}
	if p.directory != other.directory {
		return false
	}
	if !recordTypeKeyEquals(p.value, other.value) {
		return false
	}
	return p.parent.Equal(other.parent)
}

// IsSameDirectory returns true if two paths reference the same directory
// in the schema tree (ignoring values).
func (p *Path) IsSameDirectory(other *Path) bool {
	if p == nil || other == nil {
		return p == other
	}
	return p.directory == other.directory
}

// Directory returns the directory schema at this path position.
// Matches Java's KeySpacePath.getDirectory().
func (p *Path) Directory() *Directory {
	return p.directory
}

// FindChildForValue returns the child directory that accepts the given value,
// or nil if no child matches. Constant directories are checked first (matches
// Java's findChildForValue which prioritizes exact constant matches over
// open-type matches). Used for reverse resolution.
func (d *Directory) FindChildForValue(value any) *Directory {
	// First pass: prefer constant directories with matching values.
	for _, child := range d.children {
		if child.IsConstant() && recordTypeKeyEquals(child.Value, value) {
			return child
		}
	}
	// Second pass: fall back to open-type directories whose type accepts the value.
	for _, child := range d.children {
		if child.IsConstant() {
			continue
		}
		if err := child.validateInput(value); err == nil {
			return child
		}
	}
	return nil
}
