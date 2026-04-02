package main

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"
)

// stackEntry is a value on the operand stack.
type stackEntry struct {
	value any
	idx   int // instruction number that pushed this
}

// StackMachine executes binding tester instructions.
type StackMachine struct {
	db      *client.Database
	prefix  []byte
	trName  string
	stack   []stackEntry
	lastVer int64

	trMap map[string]*client.Transaction
	trMu  sync.RWMutex
	wg    sync.WaitGroup
}

// NewStackMachine creates a stack machine with the given prefix.
func NewStackMachine(db *client.Database, prefix []byte) *StackMachine {
	sm := &StackMachine{
		db:     db,
		prefix: prefix,
		trName: string(prefix),
		trMap:  make(map[string]*client.Transaction),
	}
	sm.trMap[sm.trName] = db.CreateTransaction()
	return sm
}

// Run reads instructions from FDB and executes them.
func (sm *StackMachine) Run(ctx context.Context) error {
	// Read all instructions via range scan on prefix.
	kvs, err := sm.readInstructions(ctx)
	if err != nil {
		return fmt.Errorf("read instructions: %w", err)
	}

	for i, kv := range kvs {
		inst, err := tuple.Unpack(kv.Value)
		if err != nil {
			return fmt.Errorf("unpack instruction %d: %w", i, err)
		}

		op, ok := inst[0].(string)
		if !ok {
			return fmt.Errorf("instruction %d: operation is %T, want string", i, inst[0])
		}

		var arg any
		if len(inst) > 1 {
			arg = inst[1]
		}

		if err := sm.execute(ctx, i, op, arg); err != nil {
			return fmt.Errorf("instruction %d (%s): %w", i, op, err)
		}
	}

	sm.wg.Wait()
	return nil
}

func (sm *StackMachine) readInstructions(ctx context.Context) ([]client.KeyValue, error) {
	var all []client.KeyValue
	result, err := sm.db.Transact(ctx, func(tx *client.Transaction) (any, error) {
		begin := tuple.Tuple{sm.prefix}.Pack()
		end := append(tuple.Tuple{sm.prefix}.Pack(), 0xff)
		kvs, _, err := tx.GetRange(ctx, begin, end, 10000)
		return kvs, err
	})
	if err != nil {
		return nil, err
	}
	all = result.([]client.KeyValue)
	return all, nil
}

func (sm *StackMachine) currentTr() *client.Transaction {
	sm.trMu.RLock()
	defer sm.trMu.RUnlock()
	return sm.trMap[sm.trName]
}

func (sm *StackMachine) push(idx int, value any) {
	sm.stack = append(sm.stack, stackEntry{value: value, idx: idx})
}

func (sm *StackMachine) pop() stackEntry {
	n := len(sm.stack)
	e := sm.stack[n-1]
	sm.stack = sm.stack[:n-1]
	return e
}

func (sm *StackMachine) popBytes() []byte {
	e := sm.pop()
	switch v := e.value.(type) {
	case []byte:
		return v
	case string:
		return []byte(v)
	default:
		panic(fmt.Sprintf("expected bytes, got %T", e.value))
	}
}

func (sm *StackMachine) popInt64() int64 {
	e := sm.pop()
	switch v := e.value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case *big.Int:
		return v.Int64()
	default:
		panic(fmt.Sprintf("expected int64, got %T: %v", e.value, e.value))
	}
}

func (sm *StackMachine) popString() string {
	e := sm.pop()
	switch v := e.value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		panic(fmt.Sprintf("expected string, got %T", e.value))
	}
}

func (sm *StackMachine) pushError(idx int, err error) {
	// Pack error as tuple: ("ERROR", "code")
	code := "0"
	var fdbErr *fdbError
	if e, ok := asFDBError(err); ok {
		fdbErr = e
		code = strconv.Itoa(fdbErr.code)
	}
	packed := tuple.Tuple{[]byte("ERROR"), []byte(code)}.Pack()
	sm.push(idx, packed)
}

type fdbError struct {
	code int
}

func asFDBError(err error) (*fdbError, bool) {
	if err == nil {
		return nil, false
	}
	// Try to extract error code from our wire.FDBError
	s := err.Error()
	if strings.HasPrefix(s, "fdb error ") {
		code, e := strconv.Atoi(strings.TrimPrefix(s, "fdb error "))
		if e == nil {
			return &fdbError{code: code}, true
		}
	}
	// Walk the error chain for wrapped FDB errors
	for _, prefix := range []string{"GRV: fdb error ", "GetValue: fdb error ", "GetKey: fdb error ", "GetKeyValues: fdb error ", "commit: fdb error "} {
		if strings.Contains(s, "fdb error ") {
			idx := strings.Index(s, "fdb error ")
			rest := s[idx+len("fdb error "):]
			code, e := strconv.Atoi(strings.Split(rest, " ")[0])
			if e == nil {
				return &fdbError{code: code}, true
			}
		}
		_ = prefix
	}
	return nil, false
}

// strinc returns the next byte string after prefix (for range end).
func strinc(prefix []byte) []byte {
	for i := len(prefix) - 1; i >= 0; i-- {
		if prefix[i] < 0xff {
			result := make([]byte, i+1)
			copy(result, prefix)
			result[i]++
			return result
		}
	}
	return nil
}
