package client

import (
	"bytes"

	"fdb.dev/pkg/fdbgo/wire/types"
)

// KeySelectorRef's helpers (FDBTypes.h:631-700), used where a selector goes to
// storage unresolved.

// firstGreaterOrEqual is FDBTypes.h:690-692.
func firstGreaterOrEqual(key []byte) KeySelector { return KeySelector{Key: key, Offset: 1} }

// firstGreaterThan is FDBTypes.h:687-689.
func firstGreaterThan(key []byte) KeySelector { return KeySelector{Key: key, OrEqual: true, Offset: 1} }

// keyAfterSelectorKey is keyAfter (FDBTypes.h:581-593), which leaves \xff\xff
// unchanged.
func keyAfterSelectorKey(key []byte) []byte {
	if bytes.Equal(key, allKeysEnd) {
		return key
	}
	return keyAfterBytes(key)
}

// removeOrEqual is FDBTypes.h:645-650.
func (s KeySelector) removeOrEqual() KeySelector {
	if s.OrEqual {
		return KeySelector{Key: keyAfterSelectorKey(s.Key), Offset: s.Offset}
	}
	return s
}

func (s KeySelector) isFirstGreaterOrEqual() bool { return !s.OrEqual && s.Offset == 1 }
func (s KeySelector) isFirstGreaterThan() bool    { return s.OrEqual && s.Offset == 1 }
func (s KeySelector) isLastLessOrEqual() bool     { return s.OrEqual && s.Offset == 0 }

// isBackward is FDBTypes.h:659-661: the selector resolves from keys below its key.
func (s KeySelector) isBackward() bool { return !s.OrEqual && s.Offset <= 0 }

// isDefinitelyGreater is FDBTypes.h:667: whatever the data, s resolves to a key > k.
func (s KeySelector) isDefinitelyGreater(k []byte) bool {
	if s.Offset < 1 {
		return false
	}
	if s.isFirstGreaterOrEqual() {
		return bytes.Compare(s.Key, k) > 0
	}
	return bytes.Compare(s.Key, k) >= 0
}

// isDefinitelyLess is FDBTypes.h:669: whatever the data, s resolves to a key < k.
func (s KeySelector) isDefinitelyLess(k []byte) bool {
	if s.Offset > 0 {
		return false
	}
	if s.isLastLessOrEqual() {
		return bytes.Compare(s.Key, k) < 0
	}
	return bytes.Compare(s.Key, k) <= 0
}

// ref is the selector as a request field.
func (s KeySelector) ref() types.KeySelectorRef {
	return types.KeySelectorRef{Key: s.Key, OrEqual: s.OrEqual, Offset: s.Offset}
}
