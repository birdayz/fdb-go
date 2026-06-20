package recordlayer

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
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

// isGlobalVersionComplete returns true if the global version bytes are NOT the
// incomplete marker (all 0xFF). Matches Java's isGlobalVersionComplete().
func isGlobalVersionComplete(globalBytes []byte) bool {
	for i := 0; i < GlobalVersionBytes && i < len(globalBytes); i++ {
		if globalBytes[i] != incompleteGlobalVersionMarker[i] {
			return true
		}
	}
	return false
}

// NewCompleteVersion creates a complete version from 10-byte global version and local version.
func NewCompleteVersion(globalVersion []byte, localVersion int) (*FDBRecordVersion, error) {
	if len(globalVersion) != GlobalVersionBytes {
		return nil, fmt.Errorf("global version must be %d bytes, got %d", GlobalVersionBytes, len(globalVersion))
	}
	if !isGlobalVersionComplete(globalVersion) {
		return nil, fmt.Errorf("specified version has incomplete global version")
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
// Rejects versions with all-0xFF global bytes (incomplete marker).
// Matches Java's FDBRecordVersion.complete(byte[], boolean) constructor validation.
func CompleteVersionFromBytes(b []byte) (*FDBRecordVersion, error) {
	if len(b) != VersionBytes {
		return nil, fmt.Errorf("version must be %d bytes, got %d", VersionBytes, len(b))
	}
	if !isGlobalVersionComplete(b[:GlobalVersionBytes]) {
		return nil, fmt.Errorf("specified version has incomplete global version")
	}
	var raw [VersionBytes]byte
	copy(raw[:], b)
	return &FDBRecordVersion{raw: raw, complete: true}, nil
}

// completeVersionFromBytesUnchecked wraps 12 raw bytes as a complete version WITHOUT
// rejecting the all-0xFF (incomplete) global marker. Matches Java's
// FDBRecordVersion.complete(byte[], boolean), which forces complete=true regardless of
// the bytes — used when reading legacy-format versions from the RecordVersionKey(8)
// subspace, where a stored value is always a committed (complete) version.
func completeVersionFromBytesUnchecked(b []byte) (*FDBRecordVersion, error) {
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
// Returns an error if the version is incomplete.
func (v *FDBRecordVersion) GetGlobalVersion() ([]byte, error) {
	if !v.complete {
		return nil, fmt.Errorf("cannot get global version of incomplete FDBRecordVersion")
	}
	result := make([]byte, GlobalVersionBytes)
	copy(result, v.raw[:GlobalVersionBytes])
	return result, nil
}

// GetDBVersion returns the 8-byte FDB commit version (big-endian).
// Returns an error if the version is incomplete.
func (v *FDBRecordVersion) GetDBVersion() (int64, error) {
	if !v.complete {
		return 0, fmt.Errorf("cannot get DB version of incomplete FDBRecordVersion")
	}
	return int64(binary.BigEndian.Uint64(v.raw[:8])), nil
}

// ToBytes serializes the version to 12 bytes.
func (v *FDBRecordVersion) ToBytes() []byte {
	result := make([]byte, VersionBytes)
	copy(result, v.raw[:])
	return result
}

// Equal returns true if two versions are equal.
// Complete versions compare all 12 bytes; incomplete versions compare local version only.
// Matches Java's FDBRecordVersion.equals().
func (v *FDBRecordVersion) Equal(other *FDBRecordVersion) bool {
	if v == nil || other == nil {
		return v == other
	}
	if v.complete != other.complete {
		return false
	}
	if v.complete {
		return v.raw == other.raw
	}
	return v.GetLocalVersion() == other.GetLocalVersion()
}

// Less returns true if v sorts before other.
// Complete versions sort before incomplete. Among versions of the same completeness,
// lexicographic byte comparison of the full 12-byte raw representation.
// Matches Java's FDBRecordVersion.compareTo().
func (v *FDBRecordVersion) Less(other *FDBRecordVersion) bool {
	if v == nil || other == nil {
		return v == nil && other != nil
	}
	if v.complete != other.complete {
		// Complete sorts before incomplete (Java: complete < incomplete).
		return v.complete
	}
	return bytes.Compare(v.raw[:], other.raw[:]) < 0
}

// String returns a human-readable representation matching Java's toString().
// Format: "FDBRecordVersion(complete=<bool>, raw=<hex>)"
func (v *FDBRecordVersion) String() string {
	if v == nil {
		return "FDBRecordVersion(nil)"
	}
	return fmt.Sprintf("FDBRecordVersion(complete=%t, raw=%s)", v.complete, hex.EncodeToString(v.raw[:]))
}

// MinVersion returns the minimum possible complete version (all zeros).
// Matches Java's FDBRecordVersion.MIN_VERSION.
func MinVersion() *FDBRecordVersion {
	return &FDBRecordVersion{complete: true}
}

// MaxVersion returns the maximum possible complete version.
// Matches Java's FDBRecordVersion.MAX_VERSION:
//
//	global bytes 0-8: 0xFF, byte 9: 0xFE (0xFF...FF would mean incomplete)
//	local bytes 10-11: 0xFF
func MaxVersion() *FDBRecordVersion {
	var raw [VersionBytes]byte
	// Global version: first 9 bytes 0xFF, byte 9 = 0xFE
	// (all-0xFF global version = incomplete marker, so max complete has byte 9 = 0xFE)
	for i := 0; i < 9; i++ {
		raw[i] = 0xFF
	}
	raw[9] = 0xFE
	// Local version: 0xFFFF
	raw[10] = 0xFF
	raw[11] = 0xFF
	return &FDBRecordVersion{raw: raw, complete: true}
}

// FirstInDBVersion returns the first version with the given DB commit version.
// The batch and local portions are set to 0.
// Matches Java's FDBRecordVersion.firstInDBVersion().
func FirstInDBVersion(dbVersion int64) *FDBRecordVersion {
	var raw [VersionBytes]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(dbVersion))
	// bytes 8-9 (batch) = 0, bytes 10-11 (local) = 0
	return &FDBRecordVersion{raw: raw, complete: true}
}

// LastInDBVersion returns the last version with the given DB commit version.
// The batch portion is set to 0xFFFF and local to 0xFFFF.
// Matches Java's FDBRecordVersion.lastInDBVersion().
func LastInDBVersion(dbVersion int64) *FDBRecordVersion {
	var raw [VersionBytes]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(dbVersion))
	raw[8] = 0xFF
	raw[9] = 0xFF
	raw[10] = 0xFF
	raw[11] = 0xFF
	return &FDBRecordVersion{raw: raw, complete: true}
}

// FirstInGlobalVersion returns the first version with the given 10-byte global version.
// The local version is set to 0.
// Matches Java's FDBRecordVersion.firstInGlobalVersion().
func FirstInGlobalVersion(globalVersion []byte) (*FDBRecordVersion, error) {
	return NewCompleteVersion(globalVersion, 0)
}

// LastInGlobalVersion returns the last version with the given 10-byte global version.
// The local version is set to 0xFFFF.
// Matches Java's FDBRecordVersion.lastInGlobalVersion().
func LastInGlobalVersion(globalVersion []byte) (*FDBRecordVersion, error) {
	return NewCompleteVersion(globalVersion, 0xFFFF)
}

// Next returns the version immediately after this one.
// For complete versions: treats all 12 bytes as big-endian unsigned integer, +1 with carry.
// For incomplete versions: only increments local version (last 2 bytes).
// Matches Java's FDBRecordVersion.next().
func (v *FDBRecordVersion) Next() (*FDBRecordVersion, error) {
	if v.complete {
		var raw [VersionBytes]byte
		copy(raw[:], v.raw[:])
		stopped := false
		for i := VersionBytes - 1; i >= 0; i-- {
			if raw[i] == 0xFF {
				raw[i] = 0x00
			} else {
				raw[i]++
				stopped = true
				break
			}
		}
		if !stopped || !isGlobalVersionComplete(raw[:GlobalVersionBytes]) {
			return nil, fmt.Errorf("attempted to increment maximum version")
		}
		return &FDBRecordVersion{raw: raw, complete: true}, nil
	}
	// Incomplete: only touch local version
	local := v.GetLocalVersion()
	if local >= 0xFFFF {
		return nil, fmt.Errorf("cannot get next version: already at max local version")
	}
	var raw [VersionBytes]byte
	copy(raw[:], v.raw[:])
	binary.BigEndian.PutUint16(raw[GlobalVersionBytes:], uint16(local+1))
	return &FDBRecordVersion{raw: raw, complete: false}, nil
}

// Prev returns the version immediately before this one.
// For complete versions: treats all 12 bytes as big-endian unsigned integer, -1 with borrow.
// For incomplete versions: only decrements local version (last 2 bytes).
// Matches Java's FDBRecordVersion.prev().
func (v *FDBRecordVersion) Prev() (*FDBRecordVersion, error) {
	if v.complete {
		var raw [VersionBytes]byte
		copy(raw[:], v.raw[:])
		stopped := false
		for i := VersionBytes - 1; i >= 0; i-- {
			if raw[i] == 0x00 {
				raw[i] = 0xFF
			} else {
				raw[i]--
				stopped = true
				break
			}
		}
		if !stopped {
			return nil, fmt.Errorf("attempted to decrement minimum version")
		}
		return &FDBRecordVersion{raw: raw, complete: true}, nil
	}
	// Incomplete: only touch local version
	local := v.GetLocalVersion()
	if local <= 0 {
		return nil, fmt.Errorf("cannot get prev version: already at min local version")
	}
	var raw [VersionBytes]byte
	copy(raw[:], v.raw[:])
	binary.BigEndian.PutUint16(raw[GlobalVersionBytes:], uint16(local-1))
	return &FDBRecordVersion{raw: raw, complete: false}, nil
}

// FromVersionstamp creates a complete FDBRecordVersion from an FDB tuple.Versionstamp.
// Matches Java's FDBRecordVersion.fromVersionstamp(Versionstamp).
func FromVersionstamp(vs tuple.Versionstamp) *FDBRecordVersion {
	var raw [VersionBytes]byte
	copy(raw[:GlobalVersionBytes], vs.TransactionVersion[:])
	binary.BigEndian.PutUint16(raw[GlobalVersionBytes:], vs.UserVersion)
	return &FDBRecordVersion{raw: raw, complete: true}
}

// ToVersionstamp converts this complete version to an FDB tuple.Versionstamp.
// Returns an error if the version is incomplete.
// Matches Java's FDBRecordVersion.toVersionstamp().
func (v *FDBRecordVersion) ToVersionstamp() (tuple.Versionstamp, error) {
	if !v.complete {
		return tuple.Versionstamp{}, fmt.Errorf("cannot convert incomplete FDBRecordVersion to Versionstamp")
	}
	var vs tuple.Versionstamp
	copy(vs.TransactionVersion[:], v.raw[:GlobalVersionBytes])
	vs.UserVersion = binary.BigEndian.Uint16(v.raw[GlobalVersionBytes:])
	return vs, nil
}

// WithCommittedVersion completes an incomplete version using the committed versionstamp.
// This is called after transaction commit when the real versionstamp is known.
// Returns an error if the version is already complete.
// Matches Java's FDBRecordVersion.withCommittedVersion().
func (v *FDBRecordVersion) WithCommittedVersion(committedVersion []byte) (*FDBRecordVersion, error) {
	if v.complete {
		return nil, fmt.Errorf("version is already complete")
	}
	if len(committedVersion) != GlobalVersionBytes {
		return nil, fmt.Errorf("committed version must be %d bytes, got %d", GlobalVersionBytes, len(committedVersion))
	}
	var raw [VersionBytes]byte
	copy(raw[:GlobalVersionBytes], committedVersion)
	// Preserve the local version
	binary.BigEndian.PutUint16(raw[GlobalVersionBytes:], uint16(v.GetLocalVersion()))
	return &FDBRecordVersion{raw: raw, complete: true}, nil
}
