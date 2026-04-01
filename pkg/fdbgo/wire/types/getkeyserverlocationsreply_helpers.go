package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// LocationResult is a parsed element from the GetKeyServerLocationsReply results vector:
// a key range + the storage servers that own it.
type LocationResult struct {
	Begin   []byte         // shard begin key
	End     []byte         // shard end key
	Servers []EndpointInfo // storage server endpoints
}

// ParseGetKeyServerLocationsResults parses the results field (slot 0) of
// a GetKeyServerLocationsReply. The Reader must be positioned at the reply
// message (after ErrorOr unwrapping).
//
// Wire structure: vector<pair<KeyRangeRef, vector<StorageServerInterface>>>
//
//	pair slot 0: KeyRangeRef (nested struct)
//	  inner slot 0: begin (bytes)
//	  inner slot 1: end (bytes)
//	pair slot 1: vector<StorageServerInterface>
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
			// Slot 2 = getValue endpoint.
			ep, err := ReadEndpointFromSlot(ssR, 2)
			if err != nil || ep.First == 0 {
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
