package client

// libfdb_c copies mutation arguments at the call; a caller may reuse its
// buffers as soon as Set/Clear/ClearRange/Atomic returns.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/wire/types"
)

func scribbleCallerBuf(b []byte) {
	for i := range b {
		b[i] = 0xEE
	}
}

func sameNilAndBytes(got, want []byte) bool {
	return (got == nil) == (want == nil) && bytes.Equal(got, want)
}

func assertOwnedMutations(t *testing.T, where string, got, want []Mutation) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d mutations, want %d", where, len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Type != w.Type || !sameNilAndBytes(g.Key, w.Key) || !sameNilAndBytes(g.Value, w.Value) {
			t.Fatalf("MUTATION_INPUT_ALIASED: %s[%d] = {%d %q %q}, want {%d %q %q}", where, i, g.Type, g.Key, g.Value, w.Type, w.Key, w.Value)
		}
	}
}

func assertOwnedConflicts(t *testing.T, where string, got, want []KeyRange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d write conflicts, want %d", where, len(got), len(want))
	}
	for i, w := range want {
		if !bytes.Equal(got[i].Begin, w.Begin) || !bytes.Equal(got[i].End, w.End) {
			t.Fatalf("%s[%d] = [%q,%q), want [%q,%q)", where, i, got[i].Begin, got[i].End, w.Begin, w.End)
		}
	}
}

func pointConflict(k string) []KeyRange {
	return []KeyRange{{Begin: []byte(k), End: []byte(k + "\x00")}}
}

func TestOwnedMutationSnapshotSurvivesArenaReuse(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	tx.Set([]byte("original"), []byte("owned-value"))
	input := tx.captureCommit(tx.mutations, tx.writeConflicts)
	tx.Reset()
	tx.Set([]byte("replaced"), []byte("other-value"))
	assertOwnedMutations(t, "detached after reset", input.muts, []Mutation{{
		Type: MutSetValue, Key: []byte("original"), Value: []byte("owned-value"),
	}})
	assertOwnedConflicts(t, "detached after reset", input.writeConflicts, pointConflict("original"))
}

func TestNativeCommitCapturesReadStateOptions(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	span := types.SpanContext{TraceID: [16]byte{1, 2, 3}, SpanID: 456}
	tx.spanContext.Store(&span)
	tx.SetPriority(PriorityBatch)
	tx.SetUseGrvCache()
	tx.SetSkipGrvCache()
	tx.SetCausalReadRisky(true)
	tx.SetReadLockAware()
	if err := tx.SetTag("captured-tag"); err != nil {
		t.Fatal(err)
	}
	lease := tx.enterState()
	input := tx.captureCommit(nil, nil)
	defer input.release()
	tx.captureNativeReadState(input)
	lease.release()
	captured := input.readState
	if captured.readVersionState != tx.readVersionState {
		t.Fatal("native GRV must share only the read-version future, not the public transaction")
	}
	tx.Reset()
	tx.SetReadVersion(123)
	tx.SetPriority(PrioritySystemImmediate)
	tx.SetCausalReadRisky(false)
	tx.SetLockAware()
	if err := tx.SetTag("successor-tag"); err != nil {
		t.Fatal(err)
	}
	request := captured.request
	// CommitProxyInterface.h:303,308: PRIORITY_BATCH and FLAG_CAUSAL_READ_RISKY.
	if request.flags != (1<<24)|1 || !request.useGrvCache || !request.skipGrvCache || !request.lockAware || len(request.tags) != 1 || request.tags[0] != "captured-tag" || request.span != span || input.span != span {
		t.Fatalf("native GRV options changed with successor: %+v", request)
	}
	if captured.readVersionState == tx.readVersionState {
		t.Fatal("native captured read future followed Reset into successor")
	}
}

func TestNativeReadStateCapturesGRVLockAwareness(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"none", "read", "commit"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			defer tx.Cancel()
			switch mode {
			case "read":
				tx.SetReadLockAware()
			case "commit":
				tx.SetLockAware()
			}
			lease := tx.enterState()
			input := tx.captureCommit(nil, nil)
			defer input.release()
			tx.captureNativeReadState(input)
			lease.release()
			if got, want := input.readState.request.lockAware, mode != "none"; got != want {
				t.Fatalf("GRV lock-awareness = %t, want %t", got, want)
			}
			if got, want := input.lockAware, mode == "commit"; got != want {
				t.Fatalf("commit lock-awareness = %t, want %t", got, want)
			}
		})
	}
}

func TestMutationInputsOwnedByTransaction(t *testing.T) {
	t.Parallel()
	operand := []byte("\x01\x02\x03\x04\x05\x06\x07\x08")
	svk := append([]byte("vs"), make([]byte, 14)...) // zero placeholder at 2, 4-byte offset suffix
	binary.LittleEndian.PutUint32(svk[len(svk)-4:], 2)
	svv := append([]byte("stamp-"), make([]byte, 14)...)

	type tc struct {
		name     string
		key, val []byte // caller buffers; val is the value, end or operand
		do       func(tx *Transaction, k, v []byte)
		want     Mutation
		conflict []KeyRange
	}
	set := func(name, k string, v []byte) tc {
		return tc{
			name, []byte(k), v, func(tx *Transaction, k, v []byte) { tx.Set(k, v) },
			Mutation{Type: MutSetValue, Key: []byte(k), Value: v},
			pointConflict(k),
		}
	}
	atomic := func(name string, op MutationType, k string, v []byte) tc {
		return tc{
			name, []byte(k), v, func(tx *Transaction, k, v []byte) { tx.Atomic(op, k, v) },
			Mutation{Type: op, Key: []byte(k), Value: v},
			pointConflict(k),
		}
	}
	cases := []tc{
		set("Set", "set-k", []byte("set-v")),
		set("SetNilValue", "set-nil", nil),
		set("SetEmptyValue", "set-empty", []byte{}),
		{
			"Clear", []byte("clr-k"), nil, func(tx *Transaction, k, _ []byte) { tx.Clear(k) },
			Mutation{Type: MutClearRange, Key: []byte("clr-k"), Value: []byte("clr-k\x00")},
			pointConflict("clr-k"),
		},
		{
			"ClearRange", []byte("cr-a"), []byte("cr-z"), func(tx *Transaction, b, e []byte) { _ = tx.ClearRange(b, e) }, // an error buffers nothing
			Mutation{Type: MutClearRange, Key: []byte("cr-a"), Value: []byte("cr-z")},
			[]KeyRange{{Begin: []byte("cr-a"), End: []byte("cr-z")}},
		},
		atomic("CompareAndClearNil", MutCompareAndClear, "cac-nil", nil),
		atomic("CompareAndClearEmpty", MutCompareAndClear, "cac-empty", []byte{}),
		atomic("SetVersionstampedValue", MutSetVersionstampedValue, "svv-k", svv),
		// The transformed key is already a fresh array; the operand is not.
		{
			"SetVersionstampedKey", svk, operand, func(tx *Transaction, k, v []byte) { tx.Atomic(MutSetVersionstampedKey, k, v) },
			Mutation{Type: MutSetVersionstampedKey, Key: append([]byte(nil), svk...), Value: operand},
			nil,
		},
		// A malformed key is buffered as given and validated at commit.
		{
			"SetVersionstampedKeyMalformed", []byte("ab"), operand, func(tx *Transaction, k, v []byte) { tx.Atomic(MutSetVersionstampedKey, k, v) },
			Mutation{Type: MutSetVersionstampedKey, Key: []byte("ab"), Value: operand},
			nil,
		},
	}
	for _, op := range []MutationType{
		MutAddValue, MutAnd, MutOr, MutXor, MutAppendIfFits, MutMax, MutMin,
		MutByteMin, MutByteMax, MutMinV2, MutAndV2, MutCompareAndClear,
	} {
		cases = append(cases, atomic(fmt.Sprintf("Atomic%d", op), op, fmt.Sprintf("atomic-%d", op), operand))
	}

	for _, c := range cases {
		for _, rywDisabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rywDisabled=%t", c.name, rywDisabled), func(t *testing.T) {
				t.Parallel()
				tx := &Transaction{txOptions: txOptions{rywDisabled: rywDisabled}}
				defer tx.Cancel()
				k, v := bytes.Clone(c.key), bytes.Clone(c.val) // Clone keeps nil vs present-empty
				c.do(tx, k, v)
				scribbleCallerBuf(k)
				scribbleCallerBuf(v)

				want := []Mutation{c.want}
				assertOwnedMutations(t, "buffered", tx.mutations, want)
				assertOwnedConflicts(t, "write conflicts", tx.writeConflicts, c.conflict)

				lease := tx.enterState()
				input := tx.captureCommit(tx.mutations, tx.writeConflicts)
				lease.release()
				scribbleCallerBuf(k)
				assertOwnedMutations(t, "captured", input.muts, want)
				assertOwnedConflicts(t, "captured conflicts", input.writeConflicts, c.conflict)

				if rywDisabled || (c.want.Type != MutSetValue && c.want.Type != MutClearRange) {
					return
				}
				tx.ryw.mu.Lock()
				got, known, err := tx.ryw.localPointLocked(c.want.Key)
				tx.ryw.mu.Unlock()
				switch {
				case err != nil || !known:
					t.Fatalf("RYW point: known=%t err=%v", known, err)
				case c.want.Type == MutClearRange && got != nil:
					t.Fatalf("RYW point after clear = %q, want absent", got)
				case c.want.Type == MutSetValue && (got == nil || !bytes.Equal(got, c.want.Value)):
					// A Set of nil or empty bytes is a present, empty value.
					t.Fatalf("RYW point = %q (nil=%t), want present %q", got, got == nil, c.want.Value)
				}
			})
		}
	}
}
