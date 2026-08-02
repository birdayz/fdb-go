package embedded

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sort"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// indexStatePlanningSignature identifies the exact secondary-index candidate
// universe available to a planning run. Only strictly READABLE indexes are
// admitted, so all non-readable states are equivalent for plan selection and
// the sorted set of their names is the minimal complete identity. Names are
// length-prefixed before base64 encoding; neither name boundaries nor the
// plan-cache component delimiter can collide.
//
// nil means that no authoritative store-state snapshot exists (offline plan
// harnesses only) and returns "". A live all-readable snapshot returns a
// non-empty versioned signature, so production can never silently degrade to
// the offline "assume readable" convention.
func indexStatePlanningSignature(states map[string]recordlayer.IndexState) string {
	if states == nil {
		return ""
	}
	nonReadable := make([]string, 0, len(states))
	for name, state := range states {
		if state != recordlayer.IndexStateReadable {
			nonReadable = append(nonReadable, name)
		}
	}
	sort.Strings(nonReadable)

	// Version byte makes the encoding evolvable. An unsigned 64-bit length
	// avoids architecture-dependent encodings and is injective for every Go
	// string that could name an index.
	payload := make([]byte, 1, 1+len(nonReadable)*8)
	payload[0] = 1
	var length [8]byte
	for _, name := range nonReadable {
		binary.BigEndian.PutUint64(length[:], uint64(len(name)))
		payload = append(payload, length[:]...)
		payload = append(payload, name...)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func validatePlanningIndexStateSignature(
	expected string,
	states map[string]recordlayer.IndexState,
) error {
	if expected == "" || indexStatePlanningSignature(states) == expected {
		return nil
	}
	return api.NewError(api.ErrCodeSerializationFailure,
		"query plan is stale because index states changed; replan the statement")
}

// loadPlanningIndexStates opens the record store before planning and reads the
// authoritative state of every metadata index. This is correctness metadata,
// unlike best-effort table statistics: an unavailable snapshot is an error,
// because assuming READABLE could admit a partial WRITE_ONLY/DISABLED index or
// assume uniqueness while READABLE_UNIQUE_PENDING still contains violations.
func (g *cascadesGenerator) loadPlanningIndexStates(
	ctx context.Context,
	md *recordlayer.RecordMetaData,
) (map[string]recordlayer.IndexState, string, error) {
	// planSelectCascades is also the package's DB-less planning harness entry
	// point. The public live SELECT/DML routes reject or divert before reaching
	// it when no DB exists, so nil here is an explicitly offline convention,
	// never a production fallback after an FDB failure.
	if g == nil || g.c == nil || g.c.sess == nil || g.c.sess.DB == nil {
		return nil, "", nil
	}
	ss, err := g.c.sess.Keyspace.SchemaSubspace(g.c.sess.DBPath, g.c.sess.Schema)
	if err != nil {
		return nil, "", err
	}
	result, err := g.c.sess.DB.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := g.c.newStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			// Planning correctness cannot inherit the database-level store-state
			// cache: non-cacheable stores do not always bump its version stamp on
			// an index-state transition. Read FDB at this transaction's version.
			SetStoreStateCache(recordlayer.PassThroughStoreStateCache()).
			Open()
		if openErr != nil {
			return nil, openErr
		}
		return store.GetAllIndexStates(), nil
	})
	if err != nil {
		return nil, "", err
	}
	states, ok := result.(map[string]recordlayer.IndexState)
	if !ok || states == nil {
		return nil, "", errors.New("embedded planner: record store returned no index-state snapshot")
	}
	return states, indexStatePlanningSignature(states), nil
}
