// Package difforacle implements the differential serialization fuzzer.
//
// The oracle subprocess (C++ binary) serializes FDB messages using the
// real C++ ObjectWriter. We compare its output byte-for-byte against
// Go's MarshalFDB to catch wire-format divergences.
//
// This package is built as a Go library + fuzz test only — the runnable
// binary is the C++ diff-oracle (the //cmd/fdb-diff-oracle:diff_oracle_bin
// genrule), so there is intentionally no func main here. It lives under
// cmd/ for proximity to its cpp/ sibling, not because it is a Go binary.
package difforacle

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"sync"
)

// Message type enum — must match C++ MsgType enum.
const (
	typeGetReadVersionRequest        = 0
	typeGetValueRequest              = 1
	typeGetKeyRequest                = 2
	typeGetKeyValuesRequest          = 3
	typeGetKeyServerLocationsRequest = 4
	typeCommitTransactionRequest     = 5
	typeGetReadVersionReply          = 6
	typeGetValueReply                = 7
	typeGetKeyReply                  = 8
	typeGetKeyValuesReply            = 9
	typeGetKeyServerLocationsReply   = 10
	typeCommitID                     = 11
	typeError                        = 12
	typeClientDBInfo                 = 13
	typeOpenDatabaseCoordRequest     = 14
	typeNetworkAddress               = 15
	typeEndpoint                     = 16
	typeReplyPromise                 = 17
	typeNetworkAddressV6             = 18
	typeReadOptions                  = 19
)

// fuzzReader consumes bytes from a single []byte fuzz input.
// When data is exhausted, returns zero values.
type fuzzReader struct {
	data []byte
	pos  int
}

func (r *fuzzReader) byte() byte {
	if r.pos >= len(r.data) {
		return 0
	}
	b := r.data[r.pos]
	r.pos++
	return b
}

func (r *fuzzReader) bool() bool {
	return r.byte()&1 != 0
}

func (r *fuzzReader) uint16() uint16 {
	lo := r.byte()
	hi := r.byte()
	return uint16(lo) | uint16(hi)<<8
}

func (r *fuzzReader) int32() int32 {
	var buf [4]byte
	for i := range buf {
		buf[i] = r.byte()
	}
	return int32(binary.LittleEndian.Uint32(buf[:]))
}

func (r *fuzzReader) uint32() uint32 {
	var buf [4]byte
	for i := range buf {
		buf[i] = r.byte()
	}
	return binary.LittleEndian.Uint32(buf[:])
}

func (r *fuzzReader) int64() int64 {
	var buf [8]byte
	for i := range buf {
		buf[i] = r.byte()
	}
	return int64(binary.LittleEndian.Uint64(buf[:]))
}

func (r *fuzzReader) uint64() uint64 {
	var buf [8]byte
	for i := range buf {
		buf[i] = r.byte()
	}
	return binary.LittleEndian.Uint64(buf[:])
}

func (r *fuzzReader) float64() float64 {
	bits := r.uint64()
	return math.Float64frombits(bits)
}

func (r *fuzzReader) bytes() []byte {
	n := int(r.byte())
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	if n < 0 {
		n = 0
	}
	out := make([]byte, n)
	copy(out, r.data[r.pos:])
	r.pos += n
	return out
}

func (r *fuzzReader) uid() [16]byte {
	var out [16]byte
	for i := range out {
		out[i] = r.byte()
	}
	return out
}

// vecCount returns a count for vectors, capped at 16.
func (r *fuzzReader) vecCount() int {
	n := int(r.byte())
	if n > 16 {
		n = 16
	}
	return n
}

// Oracle wraps the C++ diff-oracle subprocess. It is NOT safe for
// concurrent use — the binary protocol over stdin/stdout is inherently
// sequential.
type Oracle struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	mu     sync.Mutex
}

// NewOracle starts the C++ oracle subprocess. The binaryPath should
// point to the compiled diff-oracle binary (built via build.sh).
func NewOracle(binaryPath string) (*Oracle, error) {
	if _, err := os.Stat(binaryPath); err != nil {
		return nil, fmt.Errorf("oracle binary not found at %s: %w", binaryPath, err)
	}

	cmd := exec.Command(binaryPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("start oracle: %w", err)
	}

	return &Oracle{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

// Close shuts down the oracle subprocess.
func (o *Oracle) Close() error {
	stdinErr := o.stdin.Close()
	stdoutErr := o.stdout.Close()
	waitErr := o.cmd.Wait()
	if waitErr != nil {
		return waitErr
	}
	if stdinErr != nil {
		return stdinErr
	}
	return stdoutErr
}

// writeBytes writes a length-prefixed byte slice (4-byte LE length + data).
func (o *Oracle) writeBytes(data []byte) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(len(data)))
	if _, err := o.stdin.Write(buf[:]); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := o.stdin.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func (o *Oracle) writeU8(v uint8) error {
	_, err := o.stdin.Write([]byte{v})
	return err
}

func (o *Oracle) writeU16(v uint16) error {
	var buf [2]byte
	binary.LittleEndian.PutUint16(buf[:], v)
	_, err := o.stdin.Write(buf[:])
	return err
}

func (o *Oracle) writeU32(v uint32) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	_, err := o.stdin.Write(buf[:])
	return err
}

func (o *Oracle) writeI32(v int32) error {
	return o.writeU32(uint32(v))
}

func (o *Oracle) writeU64(v uint64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	_, err := o.stdin.Write(buf[:])
	return err
}

func (o *Oracle) writeI64(v int64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(v))
	_, err := o.stdin.Write(buf[:])
	return err
}

func (o *Oracle) writeF64(v float64) error {
	return o.writeU64(math.Float64bits(v))
}

func (o *Oracle) writeBool(v bool) error {
	b := byte(0)
	if v {
		b = 1
	}
	_, err := o.stdin.Write([]byte{b})
	return err
}

func (o *Oracle) writeUID(v [16]byte) error {
	_, err := o.stdin.Write(v[:])
	return err
}

// readResponse reads a length-prefixed response from the oracle.
// Returns nil if the oracle returned an error response (length=0).
func (o *Oracle) readResponse() ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(o.stdout, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("read response length: %w", err)
	}
	n := binary.LittleEndian.Uint32(lenBuf[:])
	if n == 0 {
		return nil, fmt.Errorf("oracle returned error response (length=0)")
	}
	if n > 10*1024*1024 {
		return nil, fmt.Errorf("response too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(o.stdout, buf); err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	return buf, nil
}

// Mutation represents a single mutation for CommitTransactionRequest.
type Mutation struct {
	Type   uint8
	Param1 []byte
	Param2 []byte
}

// ConflictRange represents a key range for conflict detection.
type ConflictRange struct {
	Begin []byte
	End   []byte
}

// --- Serialize methods for all 18 types ---

// SerializeGetReadVersionRequest sends a GetReadVersionRequest to the oracle.
func (o *Oracle) SerializeGetReadVersionRequest(
	transactionCount, flags uint32,
	hasTags bool, tags []byte,
	hasDebugID bool, debugID []byte,
	maxVersion int64,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetReadVersionRequest); err != nil {
		return nil, err
	}
	if err := o.writeU32(transactionCount); err != nil {
		return nil, err
	}
	if err := o.writeU32(flags); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasTags); err != nil {
		return nil, err
	}
	if hasTags {
		if err := o.writeBytes(tags); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasDebugID); err != nil {
		return nil, err
	}
	if hasDebugID {
		if err := o.writeBytes(debugID); err != nil {
			return nil, err
		}
	}
	if err := o.writeI64(maxVersion); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetValueRequest sends a GetValueRequest to the oracle.
func (o *Oracle) SerializeGetValueRequest(
	key []byte, version int64,
	hasTags bool, tags []byte,
	tenantId int64,
	hasOptions bool,
	ssLatestCommitVersions []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetValueRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(key); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasTags); err != nil {
		return nil, err
	}
	if hasTags {
		if err := o.writeBytes(tags); err != nil {
			return nil, err
		}
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasOptions); err != nil {
		return nil, err
	}
	if err := o.writeBytes(ssLatestCommitVersions); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyRequest sends a GetKeyRequest to the oracle.
func (o *Oracle) SerializeGetKeyRequest(
	selKey []byte, selOrEqual bool, selOffset int32,
	version int64,
	hasTags bool, tags []byte,
	tenantId int64,
	hasOptions bool,
	ssLatestCommitVersions []byte,
	field10 []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(selKey); err != nil {
		return nil, err
	}
	if err := o.writeBool(selOrEqual); err != nil {
		return nil, err
	}
	if err := o.writeI32(selOffset); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasTags); err != nil {
		return nil, err
	}
	if hasTags {
		if err := o.writeBytes(tags); err != nil {
			return nil, err
		}
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasOptions); err != nil {
		return nil, err
	}
	if err := o.writeBytes(ssLatestCommitVersions); err != nil {
		return nil, err
	}
	if err := o.writeBytes(field10); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyValuesRequest sends a GetKeyValuesRequest to the oracle.
func (o *Oracle) SerializeGetKeyValuesRequest(
	beginKey []byte, beginOrEqual bool, beginOffset int32,
	endKey []byte, endOrEqual bool, endOffset int32,
	version int64, limit, limitBytes int32,
	hasTags bool, tags []byte,
	tenantId int64,
	hasOptions bool,
	ssLatestCommitVersions []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyValuesRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(beginKey); err != nil {
		return nil, err
	}
	if err := o.writeBool(beginOrEqual); err != nil {
		return nil, err
	}
	if err := o.writeI32(beginOffset); err != nil {
		return nil, err
	}
	if err := o.writeBytes(endKey); err != nil {
		return nil, err
	}
	if err := o.writeBool(endOrEqual); err != nil {
		return nil, err
	}
	if err := o.writeI32(endOffset); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeI32(limit); err != nil {
		return nil, err
	}
	if err := o.writeI32(limitBytes); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasTags); err != nil {
		return nil, err
	}
	if hasTags {
		if err := o.writeBytes(tags); err != nil {
			return nil, err
		}
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasOptions); err != nil {
		return nil, err
	}
	if err := o.writeBytes(ssLatestCommitVersions); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyServerLocationsRequest sends a GetKeyServerLocationsRequest to the oracle.
func (o *Oracle) SerializeGetKeyServerLocationsRequest(
	begin []byte, hasEnd bool, end []byte,
	limit int32, reverse bool,
	tenantId, minTenantVersion int64,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyServerLocationsRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(begin); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasEnd); err != nil {
		return nil, err
	}
	if hasEnd {
		if err := o.writeBytes(end); err != nil {
			return nil, err
		}
	}
	if err := o.writeI32(limit); err != nil {
		return nil, err
	}
	if err := o.writeBool(reverse); err != nil {
		return nil, err
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	if err := o.writeI64(minTenantVersion); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeCommitTransactionRequest sends a CommitTransactionRequest to the oracle.
func (o *Oracle) SerializeCommitTransactionRequest(
	readSnapshot int64,
	mutations []Mutation,
	readConflictRanges, writeConflictRanges []ConflictRange,
	flags uint32,
	hasDebugID bool, debugID []byte,
	hasCommitCostEstimation bool, commitCostEstimation []byte,
	hasTagSet bool, tagSet []byte,
	tenantId int64,
	idempotencyId []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeCommitTransactionRequest); err != nil {
		return nil, err
	}
	if err := o.writeI64(readSnapshot); err != nil {
		return nil, err
	}

	// Mutations
	if err := o.writeU32(uint32(len(mutations))); err != nil {
		return nil, err
	}
	for _, m := range mutations {
		if err := o.writeU8(m.Type); err != nil {
			return nil, err
		}
		if err := o.writeBytes(m.Param1); err != nil {
			return nil, err
		}
		if err := o.writeBytes(m.Param2); err != nil {
			return nil, err
		}
	}

	// Read conflict ranges
	if err := o.writeU32(uint32(len(readConflictRanges))); err != nil {
		return nil, err
	}
	for _, cr := range readConflictRanges {
		if err := o.writeBytes(cr.Begin); err != nil {
			return nil, err
		}
		if err := o.writeBytes(cr.End); err != nil {
			return nil, err
		}
	}

	// Write conflict ranges
	if err := o.writeU32(uint32(len(writeConflictRanges))); err != nil {
		return nil, err
	}
	for _, cr := range writeConflictRanges {
		if err := o.writeBytes(cr.Begin); err != nil {
			return nil, err
		}
		if err := o.writeBytes(cr.End); err != nil {
			return nil, err
		}
	}

	if err := o.writeU32(flags); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasDebugID); err != nil {
		return nil, err
	}
	if hasDebugID {
		if err := o.writeBytes(debugID); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasCommitCostEstimation); err != nil {
		return nil, err
	}
	if hasCommitCostEstimation {
		if err := o.writeBytes(commitCostEstimation); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasTagSet); err != nil {
		return nil, err
	}
	if hasTagSet {
		if err := o.writeBytes(tagSet); err != nil {
			return nil, err
		}
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	if err := o.writeBytes(idempotencyId); err != nil {
		return nil, err
	}

	return o.readResponse()
}

// SerializeGetReadVersionReply sends a GetReadVersionReply to the oracle.
func (o *Oracle) SerializeGetReadVersionReply(
	processBusyTime int32, version int64, locked bool,
	hasMetadataVersion bool, metadataVersion []byte,
	tagThrottleInfo []byte, midShardSize int64,
	rkDefaultThrottled, rkBatchThrottled bool,
	ssVersionVectorDelta []byte,
	proxyId [16]byte, proxyTagThrottledDuration float64,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetReadVersionReply); err != nil {
		return nil, err
	}
	if err := o.writeI32(processBusyTime); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeBool(locked); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasMetadataVersion); err != nil {
		return nil, err
	}
	if hasMetadataVersion {
		if err := o.writeBytes(metadataVersion); err != nil {
			return nil, err
		}
	}
	if err := o.writeBytes(tagThrottleInfo); err != nil {
		return nil, err
	}
	if err := o.writeI64(midShardSize); err != nil {
		return nil, err
	}
	if err := o.writeBool(rkDefaultThrottled); err != nil {
		return nil, err
	}
	if err := o.writeBool(rkBatchThrottled); err != nil {
		return nil, err
	}
	if err := o.writeBytes(ssVersionVectorDelta); err != nil {
		return nil, err
	}
	if err := o.writeUID(proxyId); err != nil {
		return nil, err
	}
	if err := o.writeF64(proxyTagThrottledDuration); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetValueReply sends a GetValueReply to the oracle.
func (o *Oracle) SerializeGetValueReply(
	penalty float64,
	hasError bool, errorData []byte,
	hasValue bool, value []byte,
	cached bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetValueReply); err != nil {
		return nil, err
	}
	if err := o.writeF64(penalty); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasError); err != nil {
		return nil, err
	}
	if hasError {
		if err := o.writeBytes(errorData); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasValue); err != nil {
		return nil, err
	}
	if hasValue {
		if err := o.writeBytes(value); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(cached); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyReply sends a GetKeyReply to the oracle.
func (o *Oracle) SerializeGetKeyReply(
	penalty float64,
	hasError bool, errorData []byte,
	cached bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyReply); err != nil {
		return nil, err
	}
	if err := o.writeF64(penalty); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasError); err != nil {
		return nil, err
	}
	if hasError {
		if err := o.writeBytes(errorData); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(cached); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyValuesReply sends a GetKeyValuesReply to the oracle.
func (o *Oracle) SerializeGetKeyValuesReply(
	penalty float64,
	hasError bool, errorData []byte,
	data []byte, version int64,
	more, cached bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyValuesReply); err != nil {
		return nil, err
	}
	if err := o.writeF64(penalty); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasError); err != nil {
		return nil, err
	}
	if hasError {
		if err := o.writeBytes(errorData); err != nil {
			return nil, err
		}
	}
	if err := o.writeBytes(data); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeBool(more); err != nil {
		return nil, err
	}
	if err := o.writeBool(cached); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyServerLocationsReply sends a GetKeyServerLocationsReply to the oracle.
func (o *Oracle) SerializeGetKeyServerLocationsReply(
	results, resultsTssMapping, resultsTagMapping []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyServerLocationsReply); err != nil {
		return nil, err
	}
	if err := o.writeBytes(results); err != nil {
		return nil, err
	}
	if err := o.writeBytes(resultsTssMapping); err != nil {
		return nil, err
	}
	if err := o.writeBytes(resultsTagMapping); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeCommitID sends a CommitID to the oracle.
func (o *Oracle) SerializeCommitID(
	version int64, txnBatchId uint16,
	hasMetadataVersion bool, metadataVersion []byte,
	hasConflictingKRIndices bool, conflictingKRIndices []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeCommitID); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeU16(txnBatchId); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasMetadataVersion); err != nil {
		return nil, err
	}
	if hasMetadataVersion {
		if err := o.writeBytes(metadataVersion); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasConflictingKRIndices); err != nil {
		return nil, err
	}
	if hasConflictingKRIndices {
		if err := o.writeBytes(conflictingKRIndices); err != nil {
			return nil, err
		}
	}
	return o.readResponse()
}

// SerializeError sends an Error to the oracle.
func (o *Oracle) SerializeError(errorCode uint16) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeError); err != nil {
		return nil, err
	}
	if err := o.writeU16(errorCode); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeClientDBInfo sends a ClientDBInfo to the oracle.
func (o *Oracle) SerializeClientDBInfo(
	grvProxies, commitProxies []byte,
	id [16]byte,
	hasForward bool, forward []byte,
	history []byte,
	hasEncryptKeyProxy bool, encryptKeyProxy []byte,
	clusterId [16]byte, clusterType int32,
	hasMetaclusterName bool, metaclusterName []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeClientDBInfo); err != nil {
		return nil, err
	}
	if err := o.writeBytes(grvProxies); err != nil {
		return nil, err
	}
	if err := o.writeBytes(commitProxies); err != nil {
		return nil, err
	}
	if err := o.writeUID(id); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasForward); err != nil {
		return nil, err
	}
	if hasForward {
		if err := o.writeBytes(forward); err != nil {
			return nil, err
		}
	}
	if err := o.writeBytes(history); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasEncryptKeyProxy); err != nil {
		return nil, err
	}
	if hasEncryptKeyProxy {
		if err := o.writeBytes(encryptKeyProxy); err != nil {
			return nil, err
		}
	}
	if err := o.writeUID(clusterId); err != nil {
		return nil, err
	}
	if err := o.writeI32(clusterType); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasMetaclusterName); err != nil {
		return nil, err
	}
	if hasMetaclusterName {
		if err := o.writeBytes(metaclusterName); err != nil {
			return nil, err
		}
	}
	return o.readResponse()
}

// SerializeOpenDatabaseCoordRequest sends an OpenDatabaseCoordRequest to the oracle.
func (o *Oracle) SerializeOpenDatabaseCoordRequest(
	issues, supportedVersions, traceLogGroup []byte,
	knownClientInfoID [16]byte,
	clusterKey, coordinators, hostnames []byte,
	internal bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeOpenDatabaseCoordRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(issues); err != nil {
		return nil, err
	}
	if err := o.writeBytes(supportedVersions); err != nil {
		return nil, err
	}
	if err := o.writeBytes(traceLogGroup); err != nil {
		return nil, err
	}
	if err := o.writeUID(knownClientInfoID); err != nil {
		return nil, err
	}
	if err := o.writeBytes(clusterKey); err != nil {
		return nil, err
	}
	if err := o.writeBytes(coordinators); err != nil {
		return nil, err
	}
	if err := o.writeBytes(hostnames); err != nil {
		return nil, err
	}
	if err := o.writeBool(internal); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeNetworkAddress sends a NetworkAddress to the oracle.
func (o *Oracle) SerializeNetworkAddress(
	ipAddr uint32, port, flags uint16, fromHostname bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeNetworkAddress); err != nil {
		return nil, err
	}
	if err := o.writeU32(ipAddr); err != nil {
		return nil, err
	}
	if err := o.writeU16(port); err != nil {
		return nil, err
	}
	if err := o.writeU16(flags); err != nil {
		return nil, err
	}
	if err := o.writeBool(fromHostname); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeNetworkAddressV6 sends an IPv6 NetworkAddress to the oracle (16-byte address),
// exercising the IPAddress variant tag=2 (count-prefixed vector) marshal path.
func (o *Oracle) SerializeNetworkAddressV6(
	ip16 [16]byte, port, flags uint16, fromHostname bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeNetworkAddressV6); err != nil {
		return nil, err
	}
	if err := o.writeBytes(ip16[:]); err != nil {
		return nil, err
	}
	if err := o.writeU16(port); err != nil {
		return nil, err
	}
	if err := o.writeU16(flags); err != nil {
		return nil, err
	}
	if err := o.writeBool(fromHostname); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeEndpoint sends an Endpoint to the oracle.
func (o *Oracle) SerializeEndpoint(
	ipAddr uint32, port, flags uint16, fromHostname bool,
	token [16]byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeEndpoint); err != nil {
		return nil, err
	}
	if err := o.writeU32(ipAddr); err != nil {
		return nil, err
	}
	if err := o.writeU16(port); err != nil {
		return nil, err
	}
	if err := o.writeU16(flags); err != nil {
		return nil, err
	}
	if err := o.writeBool(fromHostname); err != nil {
		return nil, err
	}
	if err := o.writeUID(token); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeReplyPromise sends a ReplyPromise to the oracle.
func (o *Oracle) SerializeReplyPromise(token [16]byte) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeReplyPromise); err != nil {
		return nil, err
	}
	if err := o.writeUID(token); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeReadOptions sends a ReadOptions (carried inside a GetValueRequest with key="ro")
// to the oracle. Exercises RFC-117's Optional<Version> consistencyCheckStartVersion + the
// sibling Optional<UID> debugID + lockAware. The field order matches the C++ handleReadOptions.
func (o *Oracle) SerializeReadOptions(
	roType int32, cacheResult bool,
	hasDebugID bool, debugID [16]byte,
	hasCCSV bool, ccsv int64,
	lockAware bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeReadOptions); err != nil {
		return nil, err
	}
	if err := o.writeI32(roType); err != nil {
		return nil, err
	}
	if err := o.writeBool(cacheResult); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasDebugID); err != nil {
		return nil, err
	}
	if hasDebugID {
		if err := o.writeUID(debugID); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasCCSV); err != nil {
		return nil, err
	}
	if hasCCSV {
		if err := o.writeI64(ccsv); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(lockAware); err != nil {
		return nil, err
	}
	return o.readResponse()
}
