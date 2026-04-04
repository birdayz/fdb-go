// Package main implements the differential serialization fuzzer.
//
// The oracle subprocess (C++ binary) serializes FDB messages using the
// real C++ ObjectWriter. We compare its output byte-for-byte against
// Go's MarshalFDB to catch wire-format divergences.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
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
)

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
	o.stdin.Close()
	return o.cmd.Wait()
}

// writeBytes writes a length-prefixed byte slice.
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

func (o *Oracle) writeU32(v uint32) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	_, err := o.stdin.Write(buf[:])
	return err
}

func (o *Oracle) writeI32(v int32) error {
	return o.writeU32(uint32(v))
}

func (o *Oracle) writeI64(v int64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(v))
	_, err := o.stdin.Write(buf[:])
	return err
}

func (o *Oracle) writeBool(v bool) error {
	b := byte(0)
	if v {
		b = 1
	}
	_, err := o.stdin.Write([]byte{b})
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
		return nil, nil // error response
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

// SerializeGetReadVersionRequest sends a GetReadVersionRequest to the oracle.
func (o *Oracle) SerializeGetReadVersionRequest(flags, transactionCount uint32, maxVersion int64) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetReadVersionRequest); err != nil {
		return nil, err
	}
	if err := o.writeU32(flags); err != nil {
		return nil, err
	}
	if err := o.writeU32(transactionCount); err != nil {
		return nil, err
	}
	if err := o.writeI64(maxVersion); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetValueRequest sends a GetValueRequest to the oracle.
func (o *Oracle) SerializeGetValueRequest(key []byte, version, tenantId int64) ([]byte, error) {
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
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyRequest sends a GetKeyRequest to the oracle.
func (o *Oracle) SerializeGetKeyRequest(key []byte, orEqual bool, offset int32, version, tenantId int64) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(key); err != nil {
		return nil, err
	}
	if err := o.writeBool(orEqual); err != nil {
		return nil, err
	}
	if err := o.writeI32(offset); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyValuesRequest sends a GetKeyValuesRequest to the oracle.
func (o *Oracle) SerializeGetKeyValuesRequest(
	beginKey []byte, beginOrEqual bool, beginOffset int32,
	endKey []byte, endOrEqual bool, endOffset int32,
	version int64, limit, limitBytes int32, tenantId int64,
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
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	return o.readResponse()
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
	readSnapshot, tenantId int64,
	mutations []Mutation,
	readConflictRanges, writeConflictRanges []ConflictRange,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeCommitTransactionRequest); err != nil {
		return nil, err
	}
	if err := o.writeI64(readSnapshot); err != nil {
		return nil, err
	}
	if err := o.writeI64(tenantId); err != nil {
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

	return o.readResponse()
}
