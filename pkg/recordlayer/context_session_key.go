package recordlayer

// ContextSessionKey is Java's ContextSessionKey: a typed key for a well-known
// value in an FDBRecordContext's session. Keys are equal by name, as Java's
// equals is, and never equal a plain string session key.
type ContextSessionKey[T any] struct {
	name string
}

// contextSessionKeyID is a typed key's identity in the session map.
type contextSessionKeyID string

// Name is the key's name.
func (k ContextSessionKey[T]) Name() string { return k.name }

// The index-update sets Java 4.14 records per transaction (#4289,
// FDBRecordStore.updateSecondaryIndexes), for diagnosing conflicts with an
// indexer: the indexes a record write updated, by the state they were in.
//
// WriteOnlyIndexesUpdated and WriteOnlyWithQueueIndexesUpdated share Java's
// name "writeOnlyIndexesUpdated" (ContextSessionKey.java:45,51), and keys are
// equal by name, so in Java both are one set holding the ordinary and the
// queued write-only indexes. Go keeps the shared name, so it reads the same.
var (
	WriteOnlyIndexesUpdated          = ContextSessionKey[map[string]struct{}]{"writeOnlyIndexesUpdated"}
	WriteOnlyWithQueueIndexesUpdated = ContextSessionKey[map[string]struct{}]{"writeOnlyIndexesUpdated"}
	// ReadableIndexesUpdated holds READABLE and READABLE_UNIQUE_PENDING
	// indexes alike.
	ReadableIndexesUpdated = ContextSessionKey[map[string]struct{}]{"readableIndexesUpdated"}
)

// GetInSession is Java's getInSession(ContextSessionKey): the value stored
// under key in rc's session, or T's zero value (Java's null) when absent. A set
// is returned as a copy.
func GetInSession[T any](rc *FDBRecordContext, key ContextSessionKey[T]) T {
	rc.sessionMu.Lock()
	defer rc.sessionMu.Unlock()
	v, _ := rc.session[contextSessionKeyID(key.name)].(T)
	if set, ok := any(v).(map[string]struct{}); ok && set != nil {
		out := make(map[string]struct{}, len(set))
		for e := range set {
			out[e] = struct{}{}
		}
		v, _ = any(out).(T)
	}
	return v
}

// PutInSession is Java's putInSession(ContextSessionKey, value): it replaces
// any value under key.
func PutInSession[T any](rc *FDBRecordContext, key ContextSessionKey[T], value T) {
	rc.sessionMu.Lock()
	defer rc.sessionMu.Unlock()
	if rc.session == nil {
		rc.session = make(map[any]any)
	}
	rc.session[contextSessionKeyID(key.name)] = value
}

// addToSessionSet is Java's addToSessionSet: it adds value to the set under
// key, creating the set first.
func (rc *FDBRecordContext) addToSessionSet(key ContextSessionKey[map[string]struct{}], value string) {
	rc.sessionMu.Lock()
	defer rc.sessionMu.Unlock()
	if rc.session == nil {
		rc.session = make(map[any]any)
	}
	id := contextSessionKeyID(key.name)
	set, _ := rc.session[id].(map[string]struct{})
	if set == nil {
		set = make(map[string]struct{})
		rc.session[id] = set
	}
	set[value] = struct{}{}
}
