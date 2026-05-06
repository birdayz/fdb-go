package api

// KeySet names a set of key-column bindings used by the direct-access
// API to identify a row. Mirrors Java's
// com.apple.foundationdb.relational.api.KeySet.
//
// KeySet is **mutable**. `SetKeyColumn` and `SetKeyColumns` update the
// receiver in place and return it for chaining, not a new value. The
// returned pointer is always the same as the receiver on success —
// this matches Java's `return this` idiom. `EmptyKeySet()` returns an
// immutable singleton; calling `SetKey*` on it returns an error and
// does not produce a new mutable copy.
//
// If you need copy-on-write semantics, construct a fresh `NewKeySet()`
// and populate it.
type KeySet struct {
	// columns is the mutable backing map. Nil = empty.
	columns map[string]any
	// frozen marks the KeySet returned by EmptyKeySet() — modifications
	// return an error instead of mutating.
	frozen bool
}

var emptyKeySet = &KeySet{frozen: true}

// EmptyKeySet returns the immutable empty KeySet (matches Java's
// KeySet.EMPTY).
func EmptyKeySet() *KeySet { return emptyKeySet }

// NewKeySet returns an empty, mutable KeySet.
func NewKeySet() *KeySet { return &KeySet{} }

// SetKeyColumn sets columnName=value, returning the receiver for
// chaining. Returns ErrCodeUnsupportedOperation if the receiver is
// frozen (i.e. the EmptyKeySet sentinel).
func (k *KeySet) SetKeyColumn(columnName string, value any) (*KeySet, error) {
	if k.frozen {
		return nil, NewError(ErrCodeUnsupportedOperation, "empty KeySet cannot be modified")
	}
	if k.columns == nil {
		k.columns = map[string]any{}
	}
	k.columns[columnName] = value
	return k, nil
}

// SetKeyColumns merges keyMap into the receiver. Returns
// ErrCodeUnsupportedOperation on a frozen KeySet.
func (k *KeySet) SetKeyColumns(keyMap map[string]any) (*KeySet, error) {
	if k.frozen {
		return nil, NewError(ErrCodeUnsupportedOperation, "empty KeySet cannot be modified")
	}
	if k.columns == nil && len(keyMap) > 0 {
		k.columns = make(map[string]any, len(keyMap))
	}
	for key, v := range keyMap {
		k.columns[key] = v
	}
	return k, nil
}

// ToMap returns a read-only copy of the column bindings. Mutating the
// returned map does not affect the KeySet.
func (k *KeySet) ToMap() map[string]any {
	if len(k.columns) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(k.columns))
	for key, v := range k.columns {
		out[key] = v
	}
	return out
}

// NumColumns returns the number of bound columns without allocating.
func (k *KeySet) NumColumns() int { return len(k.columns) }
