// Package keyspace provides a logical directory tree abstraction over FDB.
//
// KeySpace defines a schema of named directories with typed keys.
// KeySpacePath navigates the tree to produce FDB-ready tuples and subspaces.
//
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.keyspace package.
package keyspace

import (
	"fmt"

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

// Directory is a node in the KeySpace tree. Each directory has a name,
// a key type, optional constant value, and optional children.
//
// Matches Java's KeySpaceDirectory.
type Directory struct {
	Name     string
	KeyType  KeyType
	Value    any // constant value, or nil for any-value
	parent   *Directory
	children []*Directory
	childMap map[string]*Directory
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
func (d *Directory) AddSubdirectory(child *Directory) *Directory {
	child.parent = d
	d.children = append(d.children, child)
	d.childMap[child.Name] = child
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

// Path starts navigating the key space from a named root subdirectory with a value.
func (ks *KeySpace) Path(name string, value any) (*Path, error) {
	dir := ks.root.GetSubdirectory(name)
	if dir == nil {
		return nil, fmt.Errorf("keyspace: no root directory named %q", name)
	}
	if dir.IsConstant() {
		value = dir.Value
	} else if err := dir.KeyType.ValidateValue(value); err != nil {
		return nil, fmt.Errorf("keyspace: directory %q: %w", name, err)
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
	} else if err := dir.KeyType.ValidateValue(value); err != nil {
		return nil, fmt.Errorf("keyspace: directory %q.%q: %w", p.directory.Name, name, err)
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
