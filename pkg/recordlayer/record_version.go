package recordlayer

import (
	"encoding/binary"
	"fmt"
)

// FDBRecordVersion represents a version associated with a record in the store.
// Matches Java's FDBRecordVersion exactly:
// - 12 bytes total: 10-byte global version (FDB versionstamp) + 2-byte local version
// - Global version: 8-byte DB commit version (big-endian) + 2-byte batch ordering
// - Local version: per-transaction counter (big-endian uint16)
//
// Versions can be "complete" (global version known) or "incomplete" (pending commit).
// Incomplete versions have all-0xFF in the global version bytes.
type FDBRecordVersion struct {
	// raw holds the 12-byte version. Always len=12.
	raw [VersionBytes]byte
	// complete indicates whether the global version is known.
	complete bool
}

const (
	// VersionBytes is the total size of a record version.
	VersionBytes = 12
	// GlobalVersionBytes is the size of the FDB versionstamp portion.
	GlobalVersionBytes = 10
	// LocalVersionBytes is the size of the local version counter.
	LocalVersionBytes = 2
)

// incompleteGlobalVersionMarker is 10 bytes of 0xFF, indicating an incomplete version.
var incompleteGlobalVersionMarker = [GlobalVersionBytes]byte{
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
}

// NewCompleteVersion creates a complete version from 10-byte global version and local version.
func NewCompleteVersion(globalVersion []byte, localVersion int) (*FDBRecordVersion, error) {
	if len(globalVersion) != GlobalVersionBytes {
		return nil, fmt.Errorf("global version must be %d bytes, got %d", GlobalVersionBytes, len(globalVersion))
	}
	if localVersion < 0 || localVersion > 0xFFFF {
		return nil, fmt.Errorf("local version must be 0-65535, got %d", localVersion)
	}
	var raw [VersionBytes]byte
	copy(raw[:GlobalVersionBytes], globalVersion)
	binary.BigEndian.PutUint16(raw[GlobalVersionBytes:], uint16(localVersion))
	return &FDBRecordVersion{raw: raw, complete: true}, nil
}

// CompleteVersionFromBytes parses a complete 12-byte version.
func CompleteVersionFromBytes(b []byte) (*FDBRecordVersion, error) {
	if len(b) != VersionBytes {
		return nil, fmt.Errorf("version must be %d bytes, got %d", VersionBytes, len(b))
	}
	var raw [VersionBytes]byte
	copy(raw[:], b)
	return &FDBRecordVersion{raw: raw, complete: true}, nil
}

// IncompleteVersion creates an incomplete version with the given local version.
// The global version is set to all-0xFF (will be filled in at commit time).
func IncompleteVersion(localVersion int) (*FDBRecordVersion, error) {
	if localVersion < 0 || localVersion > 0xFFFF {
		return nil, fmt.Errorf("local version must be 0-65535, got %d", localVersion)
	}
	var raw [VersionBytes]byte
	copy(raw[:GlobalVersionBytes], incompleteGlobalVersionMarker[:])
	binary.BigEndian.PutUint16(raw[GlobalVersionBytes:], uint16(localVersion))
	return &FDBRecordVersion{raw: raw, complete: false}, nil
}

// IsComplete returns true if the global version is known.
func (v *FDBRecordVersion) IsComplete() bool {
	return v.complete
}

// GetLocalVersion returns the 2-byte local version counter.
func (v *FDBRecordVersion) GetLocalVersion() int {
	return int(binary.BigEndian.Uint16(v.raw[GlobalVersionBytes:]))
}

// GetGlobalVersion returns the 10-byte global versionstamp.
// Panics if the version is incomplete.
func (v *FDBRecordVersion) GetGlobalVersion() []byte {
	if !v.complete {
		panic("cannot get global version of incomplete FDBRecordVersion")
	}
	result := make([]byte, GlobalVersionBytes)
	copy(result, v.raw[:GlobalVersionBytes])
	return result
}

// GetDBVersion returns the 8-byte FDB commit version (big-endian).
func (v *FDBRecordVersion) GetDBVersion() int64 {
	if !v.complete {
		panic("cannot get DB version of incomplete FDBRecordVersion")
	}
	return int64(binary.BigEndian.Uint64(v.raw[:8]))
}

// ToBytes serializes the version to 12 bytes.
func (v *FDBRecordVersion) ToBytes() []byte {
	result := make([]byte, VersionBytes)
	copy(result, v.raw[:])
	return result
}

// WithCommittedVersion completes an incomplete version using the committed versionstamp.
// This is called after transaction commit when the real versionstamp is known.
func (v *FDBRecordVersion) WithCommittedVersion(committedVersion []byte) (*FDBRecordVersion, error) {
	if len(committedVersion) != GlobalVersionBytes {
		return nil, fmt.Errorf("committed version must be %d bytes, got %d", GlobalVersionBytes, len(committedVersion))
	}
	var raw [VersionBytes]byte
	copy(raw[:GlobalVersionBytes], committedVersion)
	// Preserve the local version
	binary.BigEndian.PutUint16(raw[GlobalVersionBytes:], uint16(v.GetLocalVersion()))
	return &FDBRecordVersion{raw: raw, complete: true}, nil
}
