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

// readResponse reads a length-prefixed response from the oracle; an error
// response (length 0) is returned as an error, never as nil bytes.
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
