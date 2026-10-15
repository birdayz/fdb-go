// Package placementprobe implements the multi-node campaign's placement probe
// (H5) and ballast/control writer (H6).
//
// The probe lists every physical key of a Record Layer store's record
// subspace prefix+(1,) and index subspace prefix+(2,), the store's shard
// boundaries and the storage servers holding each key, as the
// fdb-bench-placement-probe/v3 document that
// tools/benchcampaign/multinode/placement_receipt.py judges. Store metadata
// (header (0,), index state and other subspaces (3..10,)) is out of scope by
// design and is never listed.
//
// One snapshot. Everything the placement claim rests on is read in ONE
// transaction pinned at read version V1 with READ_SYSTEM_KEYS: the
// data-distribution mode, the moveKeysLock owner and write token, the
// \xff/keyServers entries of the store range (shard boundaries and team
// identities, compared as raw bytes, never decoded), every key of the two data
// subspaces, and every key's storage addresses. The production backend is
// libfdb_c (cmd/placementprobe, build tag libfdbc): its getAddressesForKey
// (NativeAPI.actor.cpp getAddressesForKeyActor) reads \xff/serverTag,
// \xff/keyServers and \xff/serverList transactionally, so addresses are those
// of the shard map at V1. The pure-Go client's LocalityGetAddressesForKey
// answers from its location cache instead (a divergence from libfdb_c); that
// backend is DIAGNOSTIC ONLY: its documents carry addresses_basis kind
// "location-cache (diagnostic)", the probe refuses it without --diagnostic,
// and placement_receipt.py fails such a document's integrity.
//
// Stability over [V1, V2] is established by the moveKeysLock and the raw shard
// map, not by boundary identity (FoundationDB 7.3.77):
//   - `datadistribution off|on` (ManagementAPI setDDMode; SpecialKeySpace
//     DataDistributionImpl::commit) writes the mode, the lock owner
//     (dataDistributionModeLock) AND a fresh random write token;
//   - takeMoveKeysLock (MoveKeys.actor.cpp), which DataDistributor::init runs
//     on every DD (re)start BEFORE it reads the mode, writes ONLY a random
//     owner and leaves the write token unchanged; a healthy frozen cluster may
//     therefore show any owner, and an owner-only change is caught solely by
//     the owner-identity check;
//   - the relocation paths startMoveKeys, finishMoveKeys, startMoveShards,
//     finishMoveShards, removeStorageServer, removeKeysFromFailedServer and
//     cleanUpDataMove{Background,Core} call checkMoveKeysLock in their
//     transaction, which writes a new token (and, when it takes the lock from
//     the previous owner, the owner too). Not every keyServers write is guarded:
//     cleanUpSingleShardDataMove (MoveKeys.actor.cpp:630-703) writes it without
//     checkMoveKeysLock, though only with SHARD_ENCODE_LOCATION_METADATA, which
//     is off by default.
//
// The probe requires the mode key present and 0 at V1 and V2, the owner present
// and identical at V1 and V2 (any value, recorded), the write token present and
// identical, and the raw keyServers bytes identical, and emits nothing
// otherwise: a toggle writes a new token, a DD restart writes a new owner, a
// guarded relocation commits a new token, and any shard-map write, guarded or
// not, changes the raw keyServers bytes.
//
// The ballast tool writes only into [prefix+(1,1000000), strinc(prefix+(1,)))
// and only while data distribution is on; it clears only that range and only
// while it is off, reading \xff/dataDistributionMode inside the committing
// transaction so a concurrent mode change conflicts the commit. Every
// destructive operation takes an OwnedStore, whose prefix can only come from
// the store's fixture receipt (OpenStore) or the fixed scratch prefix
// (OpenControlStore) and is re-validated at every call.
package placementprobe

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Formats and fixed layout constants. They mirror placement_receipt.py.
const (
	ProbeFormat   = "fdb-bench-placement-probe/v3"
	ReceiptFormat = "fdb-bench-ballast-receipt/v2"
	Tool          = "placementprobe/v2"

	// AddressTransactional is the only address basis placement receipts accept.
	AddressTransactional = "transactional"
	// AddressLocationCache marks diagnostic documents from the pure-Go backend.
	AddressLocationCache = "location-cache (diagnostic)"

	RecordKey = 1
	IndexKey  = 2
	// BallastFirstID is the first record primary key the ballast uses; every
	// fixture ID is below it.
	BallastFirstID = 1_000_000
	// BallastValueBytes is the size of one ballast value.
	BallastValueBytes = 100_000
	// BallastKeysPerTransaction keeps each write transaction near 5 MB, below
	// FoundationDB's 10 MB transaction limit.
	BallastKeysPerTransaction = 50
	// ControlMutationID is the record the on-cluster mutation controls damage.
	ControlMutationID = 7
	// IndexName is the fixtures' VALUE index on price (= order ID).
	IndexName = "price_idx"
)

// FoundationDB 7.3.77 system keys (fdbclient/SystemData.cpp).
var (
	// DDModeKey is dataDistributionModeKey: a BinaryWriter int (4 bytes, little
	// endian); absent means 1 (on).
	DDModeKey = []byte("\xff/dataDistributionMode")
	// MoveKeysLockOwnerKey and MoveKeysLockWriteKey hold BinaryWriter UIDs.
	MoveKeysLockOwnerKey = []byte("\xff/moveKeysLock/Owner")
	MoveKeysLockWriteKey = []byte("\xff/moveKeysLock/Write")
	// KeyServersPrefix is keyServersKeys.begin.
	KeyServersPrefix = []byte("\xff/keyServers/")
	// DDModeLock is dataDistributionModeLock = UID(6345, 3425), serialized as
	// two little-endian uint64 (UID::serialize_unversioned): the owner setDDMode
	// writes. Informational only: a DD restart replaces it with a random owner.
	DDModeLock = uidBytes(6345, 3425)
)

func uidBytes(a, b uint64) []byte {
	out := make([]byte, 16)
	binary.LittleEndian.PutUint64(out, a)
	binary.LittleEndian.PutUint64(out[8:], b)
	return out
}

// storeSpec describes one owned store.
type storeSpec struct {
	tuple   tuple.Tuple // nil for SQL: the prefix comes from the receipt
	payload string      // fixture receipt payload that must match, if any
	ballast bool        // ballast may be written/cleared
	control bool        // scratch control store (H6 qualification)
}

var stores = map[string]storeSpec{
	"benchfetch/small":         {tuple: tuple.Tuple{"benchfetch", "small"}, payload: "small", ballast: true},
	"benchfetch/split":         {tuple: tuple.Tuple{"benchfetch", "split"}, payload: "split", ballast: true},
	"benchfetch/planskip":      {tuple: tuple.Tuple{"benchfetch", "planskip"}, ballast: true},
	"benchfetch/control-small": {tuple: tuple.Tuple{"benchfetch", "control", "small"}, payload: "control-small", control: true},
	"benchfetch/control-split": {tuple: tuple.Tuple{"benchfetch", "control", "split"}, payload: "control-split", control: true},
	"sql/IDX_A":                {ballast: true},
}

// benchfetchPrefix is the tuple prefix every benchfetch store shares; a SQL
// store prefix must not overlap it.
var benchfetchPrefix = tuple.Tuple{"benchfetch"}.Pack()

// Stores returns the owned store names, sorted.
func Stores() []string {
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// FixtureReceipt is the benchmark's untimed fixture receipt (the JSON after
// FDB_BENCH_AUDIT with phase "fixture"), or the H6 control equivalent.
type FixtureReceipt struct {
	Phase          string `json:"phase"`
	Payload        string `json:"payload,omitempty"`
	PrefixBytes    *int   `json:"prefix_bytes"`
	Rows           *int   `json:"rows"`
	Chunks         *int   `json:"chunks,omitempty"`
	StorePrefixHex string `json:"store_prefix_hex,omitempty"`
	Source         string `json:"source,omitempty"`
}

const auditMarker = "FDB_BENCH_AUDIT "

// ParseFixtureReceipt accepts either one JSON object or a benchmark log, from
// which it takes the FDB_BENCH_AUDIT lines with phase "fixture" (and, for
// stores that have one, the matching payload). Exactly one distinct receipt
// must remain; the per-benchmark "case" field is ignored for that comparison.
func ParseFixtureReceipt(data []byte, store string) (FixtureReceipt, error) {
	spec, ok := stores[store]
	if !ok {
		return FixtureReceipt{}, fmt.Errorf("store %q is not an owned store (%s)", store, strings.Join(Stores(), ", "))
	}
	var candidates []string
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' {
		candidates = append(candidates, string(trimmed))
	} else {
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 1<<16), 1<<24)
		for sc.Scan() {
			line := sc.Text()
			if i := strings.Index(line, auditMarker); i >= 0 {
				candidates = append(candidates, line[i+len(auditMarker):])
			}
		}
		if err := sc.Err(); err != nil {
			return FixtureReceipt{}, fmt.Errorf("read fixture receipt: %w", err)
		}
	}
	distinct := map[string]FixtureReceipt{}
	for _, c := range candidates {
		var r FixtureReceipt
		if err := json.Unmarshal([]byte(c), &r); err != nil {
			return FixtureReceipt{}, fmt.Errorf("malformed fixture receipt %q: %w", c, err)
		}
		if r.Phase != "fixture" || (spec.payload != "" && r.Payload != spec.payload) {
			continue
		}
		canon, err := json.Marshal(r) // drops "case" and any other field
		if err != nil {
			return FixtureReceipt{}, err
		}
		distinct[string(canon)] = r
	}
	if len(distinct) != 1 {
		return FixtureReceipt{}, fmt.Errorf("found %d distinct fixture receipts for %s, need exactly 1", len(distinct), store)
	}
	var only FixtureReceipt
	for _, r := range distinct {
		only = r
	}
	return only, nil
}

func validSQLPrefix(p []byte) error {
	if len(p) == 0 || p[0] == 0xff {
		return errors.New("store prefix is empty or in the system keyspace")
	}
	if bytes.HasPrefix(p, benchfetchPrefix) || bytes.HasPrefix(benchfetchPrefix, p) {
		return errors.New("SQL store prefix overlaps the benchfetch stores")
	}
	return nil
}

// ResolvePrefix returns the owned store's key prefix, cross-checked against
// the fixture receipt. Unknown stores and any mismatch are refused.
func ResolvePrefix(store string, r FixtureReceipt) ([]byte, error) {
	spec, ok := stores[store]
	if !ok {
		return nil, fmt.Errorf("store %q is not an owned store", store)
	}
	if r.Phase != "fixture" {
		return nil, errors.New("fixture receipt phase is not \"fixture\"")
	}
	if spec.payload != "" && r.Payload != spec.payload {
		return nil, fmt.Errorf("fixture receipt payload %q, store %s needs %q", r.Payload, store, spec.payload)
	}
	if r.PrefixBytes == nil || r.Rows == nil || *r.Rows <= 0 {
		return nil, errors.New("fixture receipt lacks prefix_bytes or a positive rows")
	}
	var prefix []byte
	if spec.tuple == nil {
		if r.StorePrefixHex == "" {
			return nil, fmt.Errorf("store %s needs store_prefix_hex in its fixture receipt (H4)", store)
		}
		p, err := hex.DecodeString(r.StorePrefixHex)
		if err != nil {
			return nil, fmt.Errorf("store_prefix_hex: %w", err)
		}
		if err := validSQLPrefix(p); err != nil {
			return nil, err
		}
		prefix = p
	} else {
		prefix = spec.tuple.Pack()
		if r.StorePrefixHex != "" && r.StorePrefixHex != hex.EncodeToString(prefix) {
			return nil, fmt.Errorf("fixture receipt store_prefix_hex %s differs from %s's prefix %x", r.StorePrefixHex, store, prefix)
		}
	}
	if *r.PrefixBytes != len(prefix) {
		return nil, fmt.Errorf("fixture receipt prefix_bytes %d != %d for %s", *r.PrefixBytes, len(prefix), store)
	}
	return prefix, nil
}

// OwnedStore is an owned store with its validated prefix. Its fields are
// unexported: outside this package it can only be obtained from OpenStore or
// OpenControlStore, and every operation re-validates it (check).
type OwnedStore struct {
	name   string
	prefix []byte
}

// OpenStore resolves a store's prefix from its fixture receipt.
func OpenStore(store string, r FixtureReceipt) (OwnedStore, error) {
	p, err := ResolvePrefix(store, r)
	if err != nil {
		return OwnedStore{}, err
	}
	s := OwnedStore{name: store, prefix: p}
	return s, s.check(false, false)
}

// OpenControlStore returns a scratch control store with its fixed tuple
// prefix. Control stores have no benchmark fixture receipt until
// control-write emits one.
func OpenControlStore(store string) (OwnedStore, error) {
	spec, ok := stores[store]
	if !ok || !spec.control {
		return OwnedStore{}, fmt.Errorf("store %q is not a scratch control store", store)
	}
	return OwnedStore{name: store, prefix: spec.tuple.Pack()}, nil
}

// Name returns the store name.
func (s OwnedStore) Name() string { return s.name }

// Prefix returns a copy of the store prefix.
func (s OwnedStore) Prefix() []byte { return append([]byte(nil), s.prefix...) }

// check is the central prefix validation every exported operation runs: the
// store must be owned, its prefix must be exactly the store's tuple prefix
// (or, for SQL, a valid non-overlapping user prefix), and the operation must
// be allowed on it (ballast: benchmark stores; control: scratch stores).
func (s OwnedStore) check(ballast, control bool) error {
	spec, ok := stores[s.name]
	if !ok {
		return fmt.Errorf("store %q is not an owned store", s.name)
	}
	if spec.tuple != nil {
		if !bytes.Equal(s.prefix, spec.tuple.Pack()) {
			return fmt.Errorf("prefix %x is not %s's owned prefix", s.prefix, s.name)
		}
	} else if err := validSQLPrefix(s.prefix); err != nil {
		return err
	}
	if ballast && !spec.ballast {
		return fmt.Errorf("store %s does not take ballast", s.name)
	}
	if control && !spec.control {
		return fmt.Errorf("store %s is not a scratch control store", s.name)
	}
	return nil
}

// concat returns a fresh a||b (never aliasing either argument).
func concat(a, b []byte) []byte {
	out := make([]byte, 0, len(a)+len(b))
	return append(append(out, a...), b...)
}

func strinc(key []byte) ([]byte, error) { return fdb.Strinc(key) }

// Subspaces returns the record and index data subspaces of prefix.
func Subspaces(prefix []byte) (record, index []byte) {
	return concat(prefix, tuple.Tuple{int64(RecordKey)}.Pack()), concat(prefix, tuple.Tuple{int64(IndexKey)}.Pack())
}

// BallastRange is [prefix+(1,1000000), strinc(prefix+(1,))).
func BallastRange(prefix []byte) (begin, end []byte, err error) {
	record, _ := Subspaces(prefix)
	end, err = strinc(record)
	if err != nil {
		return nil, nil, err
	}
	return concat(prefix, tuple.Tuple{int64(RecordKey), int64(BallastFirstID)}.Pack()), end, nil
}

// DDMode decodes a dataDistributionMode value; absent means 1 (on).
func DDMode(value []byte, present bool) (int32, error) {
	if !present {
		return 1, nil
	}
	if len(value) != 4 {
		return 0, fmt.Errorf("dataDistributionMode value has %d bytes, want 4", len(value))
	}
	return int32(binary.LittleEndian.Uint32(value)), nil
}

// ClusterIdentity names the cluster a document was taken on.
type ClusterIdentity struct {
	ConnectionStringID string `json:"connection_string_id"`
	ClusterFileSHA256  string `json:"cluster_file_sha256"`
}

// ParseClusterIdentity derives the identity from cluster file bytes: the
// SHA-256 of the file and the description:id of its connection string.
func ParseClusterIdentity(data []byte) (ClusterIdentity, error) {
	sum := sha256.Sum256(data)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		at := strings.Index(line, "@")
		colon := strings.Index(line, ":")
		if at <= 0 || colon <= 0 || colon > at {
			return ClusterIdentity{}, fmt.Errorf("malformed connection string %q", line)
		}
		return ClusterIdentity{ConnectionStringID: line[:at], ClusterFileSHA256: hex.EncodeToString(sum[:])}, nil
	}
	return ClusterIdentity{}, errors.New("cluster file has no connection string")
}

// DDModeRead is one observation of the data-distribution mode.
type DDModeRead struct {
	ReadVersion int64 `json:"read_version"`
	Mode        int32 `json:"mode"`
	KeyPresent  bool  `json:"key_present"`
}

// ProbeKey is one physical key with its storage addresses.
type ProbeKey struct {
	Kind      string   `json:"kind"`
	ID        *int64   `json:"id"`
	KeyHex    string   `json:"key_hex"`
	Addresses []string `json:"addresses"`
}

// AddressBasis records how the addresses were obtained.
type AddressBasis struct {
	Kind        string `json:"kind"`
	ReadVersion int64  `json:"read_version"`
	Method      string `json:"method"`
}

// Recheck records the second snapshot.
type Recheck struct {
	ReadVersion         int64 `json:"read_version"`
	BoundariesIdentical bool  `json:"boundaries_identical"`
}

// DDModes records the data-distribution mode at V1 and V2.
type DDModes struct {
	RequiredOff bool       `json:"required_off"`
	AtV1        DDModeRead `json:"at_v1"`
	AtV2        DDModeRead `json:"at_v2"`
}

// MoveKeysLock records the moveKeysLock owner and write token at V1 and V2.
type MoveKeysLock struct {
	OwnerHexAtV1        string `json:"owner_hex_at_v1"`
	OwnerHexAtV2        string `json:"owner_hex_at_v2"`
	OwnerUnchanged      bool   `json:"owner_unchanged"`
	WriteTokenHexAtV1   string `json:"write_token_hex_at_v1"`
	WriteTokenHexAtV2   string `json:"write_token_hex_at_v2"`
	WriteTokenUnchanged bool   `json:"write_token_unchanged"`
}

// KeyServers records the raw \xff/keyServers entries covering the store range:
// from the last entry <= keyServers+prefix to keyServers+strinc(prefix).
type KeyServers struct {
	BeginHex    string `json:"begin_hex"`
	EndHex      string `json:"end_hex"`
	Entries     int    `json:"entries"`
	SHA256AtV1  string `json:"sha256_at_v1"`
	SHA256AtV2  string `json:"sha256_at_v2"`
	Unchanged   bool   `json:"unchanged"`
	Description string `json:"description"`
}

// ProbeDoc is the fdb-bench-placement-probe/v3 document.
type ProbeDoc struct {
	Format                string          `json:"format"`
	Tool                  string          `json:"tool"`
	Store                 string          `json:"store"`
	StorePrefixHex        string          `json:"store_prefix_hex"`
	FixtureReceiptSHA256  string          `json:"fixture_receipt_sha256"`
	Cluster               ClusterIdentity `json:"cluster"`
	ReadVersion           int64           `json:"read_version"`
	BoundariesReadVersion int64           `json:"boundaries_read_version"`
	BoundaryRangeHex      [2]string       `json:"boundary_range_hex"`
	BoundariesComplete    bool            `json:"boundaries_complete"`
	BoundariesHex         []string        `json:"boundaries_hex"`
	ScannedSubspacesHex   [2]string       `json:"scanned_subspaces_hex"`
	KeysComplete          bool            `json:"keys_complete"`
	Keys                  []ProbeKey      `json:"keys"`
	AddressesBasis        AddressBasis    `json:"addresses_basis"`
	Recheck               Recheck         `json:"recheck"`
	DDMode                DDModes         `json:"dd_mode"`
	MoveKeysLock          MoveKeysLock    `json:"move_keys_lock"`
	KeyServers            KeyServers      `json:"key_servers"`
}

// ProbeInput selects the store to probe.
type ProbeInput struct {
	Store         OwnedStore
	FixtureSHA256 string
	Cluster       ClusterIdentity
	// Diagnostic permits a location-cache address source and data
	// distribution on; such documents always fail placement_receipt integrity.
	Diagnostic bool
}

// keyID decodes the primary-key ID: element 1 of a record key (1, id, ...),
// the last element of an index key (2, index, value..., id). nil when the key
// does not decode to an integer there; the receipt then rejects it.
func keyID(kind string, prefix, key []byte) *int64 {
	t, err := tuple.Unpack(key[len(prefix):])
	if err != nil || len(t) < 2 {
		return nil
	}
	e := t[len(t)-1]
	if kind == "record" {
		e = t[1]
	}
	v, ok := e.(int64)
	if !ok {
		return nil
	}
	return &v
}

// snapshot is everything read in one transaction at one read version.
type snapshot struct {
	version              int64
	dd                   DDModeRead
	owner, write         []byte
	ownerSet, writeSet   bool
	keyServers           []KV
	records, index       [][]byte
	recordAddr, indexAdr [][]string
}

func takeSnapshot(c Cluster, version int64, prefix, end []byte, withData bool) (*snapshot, error) {
	t, err := c.At(version)
	if err != nil {
		return nil, err
	}
	s := &snapshot{version: version}
	v, present, err := t.Get(DDModeKey)
	if err != nil {
		return nil, fmt.Errorf("read data-distribution mode: %w", err)
	}
	mode, err := DDMode(v, present)
	if err != nil {
		return nil, err
	}
	s.dd = DDModeRead{ReadVersion: version, Mode: mode, KeyPresent: present}
	if s.owner, s.ownerSet, err = t.Get(MoveKeysLockOwnerKey); err != nil {
		return nil, fmt.Errorf("read moveKeysLock owner: %w", err)
	}
	if s.write, s.writeSet, err = t.Get(MoveKeysLockWriteKey); err != nil {
		return nil, fmt.Errorf("read moveKeysLock write token: %w", err)
	}
	if s.keyServers, err = t.Range(concat(KeyServersPrefix, prefix), concat(KeyServersPrefix, end), true); err != nil {
		return nil, fmt.Errorf("read keyServers: %w", err)
	}
	if !withData {
		return s, nil
	}
	record, index := Subspaces(prefix)
	for _, sub := range []struct {
		begin []byte
		keys  *[][]byte
	}{{record, &s.records}, {index, &s.index}} {
		subEnd, err := strinc(sub.begin)
		if err != nil {
			return nil, err
		}
		kvs, err := t.Range(sub.begin, subEnd, false)
		if err != nil {
			return nil, fmt.Errorf("scan data subspace: %w", err)
		}
		for _, kv := range kvs {
			*sub.keys = append(*sub.keys, kv.Key)
		}
	}
	all := append(append([][]byte(nil), s.records...), s.index...)
	addrs, err := t.Addresses(all)
	if err != nil {
		return nil, fmt.Errorf("addresses at V1: %w", err)
	}
	if len(addrs) != len(all) {
		return nil, fmt.Errorf("backend returned %d address lists for %d keys", len(addrs), len(all))
	}
	s.recordAddr, s.indexAdr = addrs[:len(s.records)], addrs[len(s.records):]
	return s, nil
}

// boundaries returns the keyServers entry keys inside [prefix, end), stripped
// of the keyServers prefix (LocalityGetBoundaryKeys semantics).
func (s *snapshot) boundaries(prefix []byte) [][]byte {
	begin := concat(KeyServersPrefix, prefix)
	var out [][]byte
	for _, kv := range s.keyServers {
		if bytes.Compare(kv.Key, begin) >= 0 {
			out = append(out, kv.Key[len(KeyServersPrefix):])
		}
	}
	return out
}

// keyServersDigest hashes the raw entries with length framing.
func keyServersDigest(kvs []KV) string {
	h := sha256.New()
	var n [4]byte
	for _, kv := range kvs {
		for _, part := range [][]byte{kv.Key, kv.Value} {
			binary.BigEndian.PutUint32(n[:], uint32(len(part)))
			h.Write(n[:])
			h.Write(part)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hexAll(keys [][]byte) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = hex.EncodeToString(k)
	}
	return out
}

func sameKeys(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Probe builds the placement probe document from one snapshot at V1 and a
// recheck snapshot at V2 >= V1. It never judges placement; the receipt does.
// It emits nothing when the recheck differs (lock owner, write token,
// keyServers bytes, boundaries), when data distribution is not off at V1 and
// V2, or when the backend's addresses are not transactional (unless
// Diagnostic).
func Probe(c Cluster, in ProbeInput) (*ProbeDoc, error) {
	if err := in.Store.check(false, false); err != nil {
		return nil, err
	}
	basis := c.AddressBasis()
	if basis != AddressTransactional && !in.Diagnostic {
		return nil, fmt.Errorf("address basis %q is not transactional; this backend is diagnostic only", basis)
	}
	prefix := in.Store.prefix
	end, err := strinc(prefix)
	if err != nil {
		return nil, err
	}
	v1, err := c.ReadVersion()
	if err != nil {
		return nil, fmt.Errorf("read version V1: %w", err)
	}
	if v1 <= 0 {
		return nil, fmt.Errorf("read version V1 %d is not positive", v1)
	}
	s1, err := takeSnapshot(c, v1, prefix, end, true)
	if err != nil {
		return nil, err
	}
	v2, err := c.ReadVersion()
	if err != nil {
		return nil, fmt.Errorf("read version V2: %w", err)
	}
	if v2 < v1 {
		return nil, fmt.Errorf("read version V2 %d precedes V1 %d", v2, v1)
	}
	s2, err := takeSnapshot(c, v2, prefix, end, false)
	if err != nil {
		return nil, err
	}
	if !in.Diagnostic {
		for _, s := range []*snapshot{s1, s2} {
			if s.dd.Mode != 0 || !s.dd.KeyPresent {
				return nil, fmt.Errorf("data distribution mode is %d (key present %t) at %d, must be 0", s.dd.Mode, s.dd.KeyPresent, s.version)
			}
		}
	}
	if !s1.ownerSet || !s2.ownerSet || !bytes.Equal(s1.owner, s2.owner) {
		return nil, errors.New("moveKeysLock owner missing or changed between V1 and V2 (a data distributor took the lock); no probe emitted")
	}
	if !s1.writeSet || !s2.writeSet || !bytes.Equal(s1.write, s2.write) {
		return nil, errors.New("moveKeysLock write token missing or changed between V1 and V2; no probe emitted")
	}
	d1, d2 := keyServersDigest(s1.keyServers), keyServersDigest(s2.keyServers)
	if d1 != d2 {
		return nil, errors.New("keyServers entries changed between V1 and V2; no probe emitted")
	}
	b1, b2 := s1.boundaries(prefix), s2.boundaries(prefix)
	if !sameKeys(b1, b2) {
		return nil, errors.New("shard boundaries changed between V1 and V2; no probe emitted")
	}
	record, index := Subspaces(prefix)
	doc := &ProbeDoc{
		Format:                ProbeFormat,
		Tool:                  Tool,
		Store:                 in.Store.name,
		StorePrefixHex:        hex.EncodeToString(prefix),
		FixtureReceiptSHA256:  in.FixtureSHA256,
		Cluster:               in.Cluster,
		ReadVersion:           v1,
		BoundariesReadVersion: v1,
		BoundaryRangeHex:      [2]string{hex.EncodeToString(prefix), hex.EncodeToString(end)},
		BoundariesComplete:    true, // every keyServers entry of the range, no limit
		BoundariesHex:         hexAll(b1),
		ScannedSubspacesHex:   [2]string{hex.EncodeToString(record), hex.EncodeToString(index)},
		KeysComplete:          true, // unlimited scans of both subspaces at V1
		Keys:                  []ProbeKey{},
		AddressesBasis: AddressBasis{
			Kind: basis, ReadVersion: v1,
			Method: "LocalityGetAddressesForKey in the V1 snapshot transaction (" + basis + ")",
		},
		Recheck: Recheck{ReadVersion: v2, BoundariesIdentical: true},
		DDMode:  DDModes{RequiredOff: !in.Diagnostic, AtV1: s1.dd, AtV2: s2.dd},
		MoveKeysLock: MoveKeysLock{
			OwnerHexAtV1: hex.EncodeToString(s1.owner), OwnerHexAtV2: hex.EncodeToString(s2.owner),
			OwnerUnchanged:    true,
			WriteTokenHexAtV1: hex.EncodeToString(s1.write), WriteTokenHexAtV2: hex.EncodeToString(s2.write),
			WriteTokenUnchanged: true,
		},
		KeyServers: KeyServers{
			BeginHex: hex.EncodeToString(concat(KeyServersPrefix, prefix)),
			EndHex:   hex.EncodeToString(concat(KeyServersPrefix, end)),
			Entries:  len(s1.keyServers), SHA256AtV1: d1, SHA256AtV2: d2, Unchanged: true,
			Description: "raw entries from the last keyServers key <= begin up to end; sha256 over " +
				"(uint32 BE length, bytes) of each key and value",
		},
	}
	for _, part := range []struct {
		kind  string
		keys  [][]byte
		addrs [][]string
	}{{"record", s1.records, s1.recordAddr}, {"index", s1.index, s1.indexAdr}} {
		for i, k := range part.keys {
			addrs := append([]string(nil), part.addrs[i]...)
			sort.Strings(addrs)
			doc.Keys = append(doc.Keys, ProbeKey{
				Kind: part.kind, ID: keyID(part.kind, prefix, k), KeyHex: hex.EncodeToString(k), Addresses: addrs,
			})
		}
	}
	return doc, nil
}
