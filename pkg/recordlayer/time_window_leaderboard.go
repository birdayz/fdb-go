package recordlayer

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"sort"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// AllTimeLeaderboardType is the type value for the all-time leaderboard.
// Matches Java's TimeWindowLeaderboard.ALL_TIME_LEADERBOARD_TYPE.
const AllTimeLeaderboardType = 0

// subDirectoryPrefix is the tuple prefix used to store per-group sub-directory protos
// in the secondary subspace. Matches Java's SUB_DIRECTORY_PREFIX = Tuple.from((Object)null).
var subDirectoryPrefix = tuple.Tuple{nil}

// leaderboard represents a single time window leaderboard within the directory.
// Matches Java's TimeWindowLeaderboard.
type leaderboard struct {
	Type           int
	StartTimestamp int64
	EndTimestamp   int64
	SubspaceKey    tuple.Tuple
	NLevels        int
}

// containsTimestamp returns true if the given timestamp falls within this leaderboard's window.
// Uses half-open interval [start, end). Matches Java's TimeWindowLeaderboard.containsTimestamp().
func (lb *leaderboard) containsTimestamp(ts int64) bool {
	return lb.StartTimestamp <= ts && ts < lb.EndTimestamp
}

// leaderboardDirectory manages all active leaderboards and their per-group metadata.
// Stored as a proto in FDB at the secondary subspace root.
// Matches Java's TimeWindowLeaderboardDirectory.
type leaderboardDirectory struct {
	HighScoreFirst  bool
	UpdateTimestamp int64
	NextKey         int

	// leaderboards grouped by type, sorted within each type.
	leaderboards map[int][]*leaderboard

	// subdirectories caches per-group sub-directory metadata.
	subdirectories map[string]*leaderboardSubDirectory
}

// leaderboardSubDirectory stores per-group metadata (highScoreFirst override).
// Matches Java's TimeWindowLeaderboardSubDirectory.
type leaderboardSubDirectory struct {
	Group          tuple.Tuple
	HighScoreFirst bool
}

// newLeaderboardDirectoryFromProto creates a directory from a proto message.
func newLeaderboardDirectoryFromProto(pb *gen.TimeWindowLeaderboardDirectory) (*leaderboardDirectory, error) {
	dir := &leaderboardDirectory{
		HighScoreFirst:  pb.GetHighScoreFirst(),
		UpdateTimestamp: int64(pb.GetUpdateTimestamp()),
		NextKey:         int(pb.GetNextKey()),
		leaderboards:    make(map[int][]*leaderboard),
		subdirectories:  make(map[string]*leaderboardSubDirectory),
	}
	for _, lbpb := range pb.GetLeaderboards() {
		lb := &leaderboard{
			Type:           int(lbpb.GetType()),
			StartTimestamp: int64(lbpb.GetStartTimestamp()),
			EndTimestamp:   int64(lbpb.GetEndTimestamp()),
			NLevels:        int(lbpb.GetNlevels()),
		}
		if lbpb.SubspaceKey != nil {
			sk, err := fastUnpack(lbpb.SubspaceKey)
			if err != nil {
				return nil, fmt.Errorf("leaderboard directory: unpack subspace key: %w", err)
			}
			lb.SubspaceKey = sk
		}
		dir.leaderboards[lb.Type] = append(dir.leaderboards[lb.Type], lb)
	}
	// Sort each type's leaderboards by startTimestamp ascending.
	for _, lbs := range dir.leaderboards {
		sort.Slice(lbs, func(i, j int) bool {
			if lbs[i].StartTimestamp != lbs[j].StartTimestamp {
				return lbs[i].StartTimestamp < lbs[j].StartTimestamp
			}
			// Wider windows first (endTimestamp descending).
			return lbs[i].EndTimestamp > lbs[j].EndTimestamp
		})
	}
	return dir, nil
}

// toProto serializes the directory to a proto message.
func (d *leaderboardDirectory) toProto() *gen.TimeWindowLeaderboardDirectory {
	pb := &gen.TimeWindowLeaderboardDirectory{
		UpdateTimestamp: proto.Uint64(uint64(d.UpdateTimestamp)),
		HighScoreFirst:  proto.Bool(d.HighScoreFirst),
		NextKey:         proto.Uint32(uint32(d.NextKey)),
	}
	for _, lbs := range d.leaderboards {
		for _, lb := range lbs {
			lbpb := &gen.TimeWindowLeaderboard{
				Type:           proto.Uint32(uint32(lb.Type)),
				StartTimestamp: proto.Uint64(uint64(lb.StartTimestamp)),
				EndTimestamp:   proto.Uint64(uint64(lb.EndTimestamp)),
				Nlevels:        proto.Int32(int32(lb.NLevels)),
			}
			if lb.SubspaceKey != nil {
				lbpb.SubspaceKey = lb.SubspaceKey.Pack()
			}
			pb.Leaderboards = append(pb.Leaderboards, lbpb)
		}
	}
	return pb
}

// addLeaderboard creates a new leaderboard with an auto-assigned subspace key.
// Matches Java's TimeWindowLeaderboardDirectory.addLeaderboard().
func (d *leaderboardDirectory) addLeaderboard(typ int, start, end int64, nlevels int) *leaderboard {
	lb := &leaderboard{
		Type:           typ,
		StartTimestamp: start,
		EndTimestamp:   end,
		SubspaceKey:    tuple.Tuple{int64(d.NextKey)},
		NLevels:        nlevels,
	}
	d.NextKey++
	d.leaderboards[typ] = append(d.leaderboards[typ], lb)
	// Re-sort after adding.
	lbs := d.leaderboards[typ]
	sort.Slice(lbs, func(i, j int) bool {
		if lbs[i].StartTimestamp != lbs[j].StartTimestamp {
			return lbs[i].StartTimestamp < lbs[j].StartTimestamp
		}
		return lbs[i].EndTimestamp > lbs[j].EndTimestamp
	})
	return lb
}

// oldestLeaderboardMatching finds the first leaderboard of the given type that
// contains the given timestamp. Returns nil if no match.
// Matches Java's TimeWindowLeaderboardDirectory.oldestLeaderboardMatching().
func (d *leaderboardDirectory) oldestLeaderboardMatching(typ int, timestamp int64) *leaderboard {
	lbs, ok := d.leaderboards[typ]
	if !ok {
		return nil
	}
	for _, lb := range lbs {
		if lb.containsTimestamp(timestamp) {
			return lb
		}
	}
	return nil
}

// findLeaderboard finds a leaderboard with exact matching (type, start, end).
func (d *leaderboardDirectory) findLeaderboard(typ int, start, end int64) *leaderboard {
	lbs, ok := d.leaderboards[typ]
	if !ok {
		return nil
	}
	for _, lb := range lbs {
		if lb.StartTimestamp == start && lb.EndTimestamp == end {
			return lb
		}
	}
	return nil
}

// allLeaderboards returns all leaderboards across all types.
func (d *leaderboardDirectory) allLeaderboards() []*leaderboard {
	var all []*leaderboard
	for _, lbs := range d.leaderboards {
		all = append(all, lbs...)
	}
	return all
}

// removeLeaderboard removes a leaderboard from the directory.
func (d *leaderboardDirectory) removeLeaderboard(lb *leaderboard) {
	lbs, ok := d.leaderboards[lb.Type]
	if !ok {
		return
	}
	for i, existing := range lbs {
		if existing.StartTimestamp == lb.StartTimestamp && existing.EndTimestamp == lb.EndTimestamp {
			d.leaderboards[lb.Type] = append(lbs[:i], lbs[i+1:]...)
			break
		}
	}
	if len(d.leaderboards[lb.Type]) == 0 {
		delete(d.leaderboards, lb.Type)
	}
}

// getSubDirectory returns cached sub-directory for a group, or nil.
func (d *leaderboardDirectory) getSubDirectory(group tuple.Tuple) *leaderboardSubDirectory {
	return d.subdirectories[string(group.Pack())]
}

// addSubDirectory caches a sub-directory.
func (d *leaderboardDirectory) addSubDirectory(sub *leaderboardSubDirectory) {
	key := string(sub.Group.Pack())
	d.subdirectories[key] = sub
}

// loadDirectory loads the leaderboard directory from FDB.
// Returns nil if no directory exists yet.
func loadLeaderboardDirectory(tx fdb.ReadTransaction, extraSubspace subspace.Subspace) (*leaderboardDirectory, error) {
	bytes, err := tx.Get(fdb.Key(extraSubspace.Bytes())).Get()
	if err != nil {
		return nil, fmt.Errorf("loadLeaderboardDirectory: %w", err)
	}
	if bytes == nil {
		return nil, nil
	}
	pb := &gen.TimeWindowLeaderboardDirectory{}
	if err := pb.UnmarshalVT(bytes); err != nil {
		return nil, fmt.Errorf("loadLeaderboardDirectory: unmarshal: %w", err)
	}
	return newLeaderboardDirectoryFromProto(pb)
}

// saveLeaderboardDirectory saves the directory to FDB.
func saveLeaderboardDirectory(tx fdb.WritableTransaction, extraSubspace subspace.Subspace, dir *leaderboardDirectory) error {
	data, err := dir.toProto().MarshalVT()
	if err != nil {
		return fmt.Errorf("saveLeaderboardDirectory: marshal: %w", err)
	}
	tx.Set(fdb.Key(extraSubspace.Bytes()), data)
	return nil
}

// loadSubDirectory loads or creates a per-group sub-directory.
// Matches Java's TimeWindowLeaderboardIndexMaintainer.loadSubDirectory().
func loadLeaderboardSubDirectory(
	tx fdb.ReadTransaction,
	extraSubspace subspace.Subspace,
	dir *leaderboardDirectory,
	group tuple.Tuple,
) (*leaderboardSubDirectory, error) {
	// Check cache.
	if cached := dir.getSubDirectory(group); cached != nil {
		return cached, nil
	}

	// Build the key: extraSubspace.pack(null, group...)
	keyTuple := make(tuple.Tuple, 0, 1+len(group))
	keyTuple = append(keyTuple, subDirectoryPrefix...)
	keyTuple = append(keyTuple, group...)
	key := extraSubspace.Pack(keyTuple)

	bytes, err := tx.Get(fdb.Key(key)).Get()
	if err != nil {
		return nil, fmt.Errorf("loadSubDirectory: %w", err)
	}

	var sub *leaderboardSubDirectory
	if bytes == nil {
		// Default inherits from directory.
		sub = &leaderboardSubDirectory{
			Group:          group,
			HighScoreFirst: dir.HighScoreFirst,
		}
	} else {
		pb := &gen.TimeWindowLeaderboardSubDirectory{}
		if err := pb.UnmarshalVT(bytes); err != nil {
			return nil, fmt.Errorf("loadSubDirectory: unmarshal: %w", err)
		}
		sub = &leaderboardSubDirectory{
			Group:          group,
			HighScoreFirst: pb.GetHighScoreFirst(),
		}
	}

	dir.addSubDirectory(sub)
	return sub, nil
}

// saveLeaderboardSubDirectory persists a per-group sub-directory to FDB.
// Matches Java's TimeWindowLeaderboardIndexMaintainer.saveSubDirectory().
func saveLeaderboardSubDirectory(
	tx fdb.WritableTransaction,
	extraSubspace subspace.Subspace,
	sub *leaderboardSubDirectory,
) error {
	keyTuple := make(tuple.Tuple, 0, 1+len(sub.Group))
	keyTuple = append(keyTuple, subDirectoryPrefix...)
	keyTuple = append(keyTuple, sub.Group...)
	key := extraSubspace.Pack(keyTuple)

	pb := &gen.TimeWindowLeaderboardSubDirectory{
		HighScoreFirst: proto.Bool(sub.HighScoreFirst),
	}
	data, err := pb.MarshalVT()
	if err != nil {
		return fmt.Errorf("saveSubDirectory: marshal: %w", err)
	}
	tx.Set(fdb.Key(key), data)
	return nil
}

// isHighScoreFirst resolves the highScoreFirst setting for a specific group.
// Per-group override takes precedence over directory default.
func (m *timeWindowLeaderboardIndexMaintainer) isHighScoreFirst(
	dir *leaderboardDirectory,
	group tuple.Tuple,
) (bool, error) {
	sub, err := loadLeaderboardSubDirectory(m.tx, m.secondarySubspace, dir, group)
	if err != nil {
		return false, err
	}
	return sub.HighScoreFirst, nil
}

// encodeSignedLong encodes a signed int64 for use with FDB's atomic BYTE_MAX mutation.
// Maps signed comparison to unsigned: value + 2^63 stored as little-endian uint64.
// Matches Java's encodeSignedLong() in TimeWindowLeaderboardIndexMaintainer.
func encodeSignedLong(value int64) []byte {
	// value - MinInt64 = value + 2^63, wraps correctly for uint64.
	// Maps signed comparison to unsigned: MinInt64→0, 0→2^63, MaxInt64→MaxUint64.
	unsigned := uint64(value - math.MinInt64)
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, unsigned)
	return buf
}

// negateScore negates the score element at the given position in a tuple for high-score-first ordering.
// Returns error if the element type is not a supported numeric type.
// Matches Java's TupleHelpers.negate() + TupleHelpers.set().
func negateScore(t tuple.Tuple, position int) (tuple.Tuple, error) {
	if position < 0 || position >= len(t) {
		return t, nil
	}
	result := make(tuple.Tuple, len(t))
	copy(result, t)
	switch v := t[position].(type) {
	case int64:
		if v == math.MinInt64 {
			// Java: BigInteger.valueOf(Long.MIN_VALUE).negate() → positive 2^63 as BigInteger.
			result[position] = new(big.Int).Neg(big.NewInt(math.MinInt64))
		} else {
			result[position] = -v
		}
	case int:
		result[position] = int64(-v)
	case *big.Int:
		negated := new(big.Int).Neg(v)
		// Java normalizes BigInteger equal to Long.MIN_VALUE back to long.
		if negated.IsInt64() && negated.Int64() == math.MinInt64 {
			result[position] = math.MinInt64
		} else {
			result[position] = negated
		}
	case float64:
		result[position] = -v
	case float32:
		result[position] = -float64(v)
	case nil:
		// nil (null) values are not negated — pass through. Matches Java's null handling.
	default:
		return nil, fmt.Errorf("negateScore: unsupported type %T at position %d", t[position], position)
	}
	return result, nil
}

// negateScoreRange negates a TupleRange for high-score-first scanning.
// Swaps low/high and negates the score element at groupPrefixSize position.
// Matches Java's TimeWindowLeaderboardIndexMaintainer.negateScoreRange().
func negateScoreRange(r TupleRange, groupPrefixSize int) (TupleRange, error) {
	low := r.Low
	high := r.High
	lowEndpoint := r.LowEndpoint
	highEndpoint := r.HighEndpoint

	if low == nil || len(low) < groupPrefixSize {
		if lowEndpoint == EndpointTypeTreeStart {
			lowEndpoint = EndpointTypeTreeEnd
		}
	} else {
		var err error
		low, err = negateScore(low, groupPrefixSize)
		if err != nil {
			return TupleRange{}, err
		}
	}

	if high == nil || len(high) < groupPrefixSize {
		// NOTE: Java checks lowEndpoint here (not highEndpoint). Matching Java exactly.
		// This is a known Java quirk where the first if-block may have mutated lowEndpoint
		// from TREE_START to TREE_END, and this second block then sees the mutated value.
		if lowEndpoint == EndpointTypeTreeEnd {
			lowEndpoint = EndpointTypeTreeStart
		}
	} else {
		var err error
		high, err = negateScore(high, groupPrefixSize)
		if err != nil {
			return TupleRange{}, err
		}
	}

	// Swap: high becomes low, low becomes high (with swapped endpoints).
	return TupleRange{
		Low:          high,
		High:         low,
		LowEndpoint:  highEndpoint,
		HighEndpoint: lowEndpoint,
	}, nil
}

// orderedScoreIndexKey represents an index entry with its score key (possibly negated)
// for ordering. Used to find the "best" score for a leaderboard.
type orderedScoreIndexKey struct {
	entry    indexEntry
	scoreKey tuple.Tuple
}

// TimeWindowSpec defines parameters for creating a batch of time windows.
// Matches Java's TimeWindowLeaderboardWindowUpdate.TimeWindowSpec.
type TimeWindowSpec struct {
	Type           int
	BaseTimestamp  int64
	StartIncrement int64
	Duration       int64
	Count          int
}

// TimeWindowLeaderboardWindowUpdate is an operation to manage active time windows.
// Matches Java's TimeWindowLeaderboardWindowUpdate.
type TimeWindowLeaderboardWindowUpdate struct {
	UpdateTimestamp int64
	HighScoreFirst  bool
	DeleteBefore    int64
	AllTime         bool
	Specs           []TimeWindowSpec
	NLevels         int
	Rebuild         TimeWindowRebuild
}

// TimeWindowRebuild controls when to rebuild the index after adding windows.
type TimeWindowRebuild int

const (
	TimeWindowRebuildNever                TimeWindowRebuild = 0
	TimeWindowRebuildAlways               TimeWindowRebuild = 1
	TimeWindowRebuildIfOverlappingChanged TimeWindowRebuild = 2
)

// prependLeaderboardKey prepends a leaderboard's subspace key to a TupleRange.
// Handles nil bounds correctly (converting TREE_START/TREE_END to RANGE_INCLUSIVE
// when prepending). Matches Java's TupleRange.prepend().
func prependLeaderboardKey(r TupleRange, lbKey tuple.Tuple) TupleRange {
	var newLow tuple.Tuple
	var newLowEndpoint EndpointType
	if r.Low == nil {
		newLow = lbKey
		newLowEndpoint = EndpointTypeRangeInclusive
	} else {
		newLow = make(tuple.Tuple, 0, len(lbKey)+len(r.Low))
		newLow = append(newLow, lbKey...)
		newLow = append(newLow, r.Low...)
		newLowEndpoint = r.LowEndpoint
	}

	var newHigh tuple.Tuple
	var newHighEndpoint EndpointType
	if r.High == nil {
		newHigh = lbKey
		newHighEndpoint = EndpointTypeRangeInclusive
	} else {
		newHigh = make(tuple.Tuple, 0, len(lbKey)+len(r.High))
		newHigh = append(newHigh, lbKey...)
		newHigh = append(newHigh, r.High...)
		newHighEndpoint = r.HighEndpoint
	}

	return TupleRange{
		Low:          newLow,
		High:         newHigh,
		LowEndpoint:  newLowEndpoint,
		HighEndpoint: newHighEndpoint,
	}
}
