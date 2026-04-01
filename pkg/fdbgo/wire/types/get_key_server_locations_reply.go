package types

// GetKeyServerLocationsReply — fdbclient/include/fdbclient/CommitProxyInterface.h:414
//
// C++ serialize: serializer(ar, results, resultsTssMapping, resultsTagMapping, arena)
//
//	slot 0: results — vector<pair<KeyRangeRef, vector<StorageServerInterface>>>
//	slot 1: resultsTssMapping — vector<pair<UID, StorageServerInterface>>
//	slot 2: resultsTagMapping — vector<pair<UID, Tag>>
//	(arena skipped)
//
// Each pair is a nested struct via serializable_traits::serialize(ar, p.first, p.second).
//
// pair<KeyRangeRef, vector<SS>> has 2 slots:
//
//	pair slot 0: KeyRangeRef (nested struct, auto-derived from struct fields)
//	  KeyRangeRef slot 0: begin (bytes/StringRef)
//	  KeyRangeRef slot 1: end (bytes/StringRef)
//	pair slot 1: vector<StorageServerInterface>

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// LocationResult is a parsed element from the results vector:
// a key range + the storage servers that own it.
type LocationResult struct {
	Begin   []byte         // shard begin key
	End     []byte         // shard end key
	Servers []EndpointInfo // storage server endpoints
}

// ParseGetKeyServerLocationsResults parses the results field (slot 0) of
// a GetKeyServerLocationsReply. The Reader must be positioned at the reply
// message (after ErrorOr unwrapping).
func ParseGetKeyServerLocationsResults(r *wire.Reader) ([]LocationResult, error) {
	resultCount, err := r.ReadVectorCount(GetKeyServerLocationsReplySlotResults)
	if err != nil || resultCount == 0 {
		return nil, nil
	}

	results := make([]LocationResult, 0, resultCount)
	for i := 0; i < resultCount; i++ {
		pairR, err := r.ReadVectorElementReader(GetKeyServerLocationsReplySlotResults, i)
		if err != nil {
			continue
		}

		var loc LocationResult

		// Pair slot 0: KeyRangeRef (nested struct with begin/end bytes).
		if krR, err := pairR.ReadNestedReader(0); err == nil {
			loc.Begin = krR.ReadBytes(0)
			loc.End = krR.ReadBytes(1)
		}

		// Pair slot 1: vector<StorageServerInterface>.
		ssCount, err := pairR.ReadVectorCount(1)
		if err != nil || ssCount == 0 {
			continue
		}
		for j := 0; j < ssCount; j++ {
			ssR, err := pairR.ReadVectorElementReader(1, j)
			if err != nil {
				continue
			}
			// StorageServerInterface has many endpoint fields.
			// Slot 2 = getValue endpoint (the primary one we route to).
			ep, err := ReadEndpointFromSlot(ssR, 2)
			if err != nil || ep.First == 0 {
				// Fallback: scan for any valid endpoint.
				nf := ssR.VTableLength() - 2
				for s := 0; s < nf; s++ {
					ep, err = ReadEndpointFromSlot(ssR, s)
					if err == nil && ep.First != 0 {
						break
					}
				}
			}
			if ep.First != 0 {
				loc.Servers = append(loc.Servers, ep)
			}
		}

		if len(loc.Servers) > 0 {
			results = append(results, loc)
		}
	}

	return results, nil
}
