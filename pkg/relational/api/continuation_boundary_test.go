package api

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

type testContinuation struct {
	serialized []byte
	state      []byte
	reason     ContinuationReason
}

func (c *testContinuation) Serialize() []byte          { return c.serialized }
func (c *testContinuation) ExecutionState() []byte     { return c.state }
func (c *testContinuation) Reason() ContinuationReason { return c.reason }
func continuationVersion() *int32                      { return proto.Int32(1) }
func continuationReason(reason gen.ContinuationProto_Reason) *gen.ContinuationProto_Reason {
	return reason.Enum()
}

func marshalContinuationProto(t *testing.T, wire *gen.ContinuationProto) []byte {
	t.Helper()
	serialized, err := (proto.MarshalOptions{Deterministic: true}).Marshal(wire)
	if err != nil {
		t.Fatalf("marshal continuation: %v", err)
	}
	return serialized
}

func queryBoundaryContinuation(t *testing.T, state []byte) *testContinuation {
	t.Helper()
	return &testContinuation{
		serialized: marshalContinuationProto(t, &gen.ContinuationProto{
			Version:        continuationVersion(),
			ExecutionState: cloneContinuationBytes(state),
			Reason: continuationReason(
				gen.ContinuationProto_QUERY_EXECUTION_LIMIT_REACHED,
			),
		}),
		state:  state,
		reason: ContinuationQueryExecutionLimitReached,
	}
}

func TestEndContinuationIsParseablePresentEmptyVersionOne(t *testing.T) {
	end := EndContinuation()
	if end == nil {
		t.Fatal("EndContinuation returned nil")
	}
	if !AtEnd(end) {
		t.Fatalf("AtEnd(EndContinuation()) = false; state = %#v", end.ExecutionState())
	}
	if end.ExecutionState() == nil {
		t.Fatal("END execution state is absent; want present empty bytes")
	}
	if got := end.Reason(); got != ContinuationCursorAfterLast {
		t.Fatalf("END reason = %v, want %v", got, ContinuationCursorAfterLast)
	}

	var first gen.ContinuationProto
	if err := proto.Unmarshal(end.Serialize(), &first); err != nil {
		t.Fatalf("parse END: %v", err)
	}
	assertEndWireShape(t, &first)

	// Pin presence through a parse/remarshal cycle. Proto3 optional bytes must
	// not collapse present-empty execution_state into an absent BEGIN state.
	remarshaled := marshalContinuationProto(t, &first)
	var second gen.ContinuationProto
	if err := proto.Unmarshal(remarshaled, &second); err != nil {
		t.Fatalf("reparse END: %v", err)
	}
	assertEndWireShape(t, &second)

	serialized := end.Serialize()
	serialized[0] ^= 0xff
	state := end.ExecutionState()
	state = append(state, 1)
	if len(state) != 1 {
		t.Fatalf("mutated caller-owned END state length = %d, want 1", len(state))
	}
	if bytes.Equal(serialized, end.Serialize()) {
		t.Fatal("mutating returned END bytes changed the immutable snapshot")
	}
	if !AtEnd(end) || len(end.ExecutionState()) != 0 {
		t.Fatal("mutating returned END state changed the immutable snapshot")
	}
}

func assertEndWireShape(t *testing.T, wire *gen.ContinuationProto) {
	t.Helper()
	message := wire.ProtoReflect()
	fields := message.Descriptor().Fields()
	for _, number := range []protowire.Number{1, 2, 6} {
		if !message.Has(fields.ByNumber(number)) {
			t.Fatalf("END field %d is absent", number)
		}
	}
	if wire.GetVersion() != 1 {
		t.Fatalf("END version = %d, want 1", wire.GetVersion())
	}
	if wire.GetExecutionState() == nil || len(wire.GetExecutionState()) != 0 {
		t.Fatalf("END execution_state = %#v, want present empty", wire.GetExecutionState())
	}
	if wire.GetReason() != gen.ContinuationProto_CURSOR_AFTER_LAST {
		t.Fatalf("END reason = %v", wire.GetReason())
	}
	if wire.GetCompiledStatement() != nil {
		t.Fatal("END unexpectedly carries a compiled statement")
	}
}

func TestContinuationAvailableErrorSnapshotsProducer(t *testing.T) {
	state := []byte("TOP_SECRET_CANARY")
	producer := queryBoundaryContinuation(t, state)
	originalSerialized := cloneContinuationBytes(producer.serialized)
	originalState := cloneContinuationBytes(producer.state)

	boundary, err := NewContinuationAvailableError(producer)
	if err != nil {
		t.Fatalf("NewContinuationAvailableError: %v", err)
	}
	if strings.Contains(boundary.Error(), "TOP_SECRET_CANARY") {
		t.Fatalf("boundary Error leaked continuation state: %q", boundary.Error())
	}
	if got := boundary.Reason(); got != ContinuationQueryExecutionLimitReached {
		t.Fatalf("boundary reason = %v", got)
	}

	for i := range producer.serialized {
		producer.serialized[i] ^= 0xff
	}
	for i := range producer.state {
		producer.state[i] = 'x'
	}

	carried := boundary.Continuation()
	if carried == nil {
		t.Fatal("boundary continuation is nil")
	}
	if got := carried.Serialize(); !bytes.Equal(got, originalSerialized) {
		t.Fatalf("carrier serialized bytes changed after producer mutation\n got: %x\nwant: %x", got, originalSerialized)
	}
	if got := carried.ExecutionState(); !bytes.Equal(got, originalState) {
		t.Fatalf("carrier execution state = %q, want %q", got, originalState)
	}

	returnedSerialized := carried.Serialize()
	returnedSerialized[0] ^= 0xff
	returnedState := carried.ExecutionState()
	returnedState[0] ^= 0xff
	if bytes.Equal(returnedSerialized, boundary.Continuation().Serialize()) {
		t.Fatal("Serialize returned carrier-owned memory")
	}
	if bytes.Equal(returnedState, boundary.Continuation().ExecutionState()) {
		t.Fatal("ExecutionState returned carrier-owned memory")
	}
}

func TestContinuationAvailableErrorRejectsInvalidEnvelopeOrProducer(t *testing.T) {
	validState := []byte{1, 2, 3}
	valid := queryBoundaryContinuation(t, validState)

	unknown := cloneContinuationBytes(valid.serialized)
	unknown = protowire.AppendTag(unknown, 100, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 1)
	duplicateState := cloneContinuationBytes(valid.serialized)
	duplicateState = protowire.AppendTag(duplicateState, 2, protowire.BytesType)
	duplicateState = protowire.AppendBytes(duplicateState, validState)

	tests := []struct {
		name         string
		continuation Continuation
	}{
		{name: "nil", continuation: nil},
		{name: "malformed", continuation: &testContinuation{
			serialized: []byte{0xff}, state: validState,
			reason: ContinuationQueryExecutionLimitReached,
		}},
		{name: "oversized", continuation: &testContinuation{
			serialized: make([]byte, maxContinuationBytes+1), state: validState,
			reason: ContinuationQueryExecutionLimitReached,
		}},
		{name: "missing version", continuation: &testContinuation{
			serialized: marshalContinuationProto(t, &gen.ContinuationProto{
				ExecutionState: validState,
				Reason:         continuationReason(gen.ContinuationProto_QUERY_EXECUTION_LIMIT_REACHED),
			}), state: validState, reason: ContinuationQueryExecutionLimitReached,
		}},
		{name: "wrong version", continuation: &testContinuation{
			serialized: marshalContinuationProto(t, &gen.ContinuationProto{
				Version: proto.Int32(2), ExecutionState: validState,
				Reason: continuationReason(gen.ContinuationProto_QUERY_EXECUTION_LIMIT_REACHED),
			}), state: validState, reason: ContinuationQueryExecutionLimitReached,
		}},
		{name: "missing state", continuation: &testContinuation{
			serialized: marshalContinuationProto(t, &gen.ContinuationProto{
				Version: continuationVersion(),
				Reason:  continuationReason(gen.ContinuationProto_QUERY_EXECUTION_LIMIT_REACHED),
			}), state: validState, reason: ContinuationQueryExecutionLimitReached,
		}},
		{name: "empty state", continuation: &testContinuation{
			serialized: marshalContinuationProto(t, &gen.ContinuationProto{
				Version: continuationVersion(), ExecutionState: make([]byte, 0),
				Reason: continuationReason(gen.ContinuationProto_QUERY_EXECUTION_LIMIT_REACHED),
			}), state: make([]byte, 0), reason: ContinuationQueryExecutionLimitReached,
		}},
		{name: "missing reason", continuation: &testContinuation{
			serialized: marshalContinuationProto(t, &gen.ContinuationProto{
				Version: continuationVersion(), ExecutionState: validState,
			}), state: validState, reason: ContinuationUserRequested,
		}},
		{name: "unknown reason", continuation: &testContinuation{
			serialized: marshalContinuationProto(t, &gen.ContinuationProto{
				Version: continuationVersion(), ExecutionState: validState,
				Reason: continuationReason(gen.ContinuationProto_Reason(99)),
			}), state: validState, reason: ContinuationReason(99),
		}},
		{name: "user requested reason", continuation: &testContinuation{
			serialized: marshalContinuationProto(t, &gen.ContinuationProto{
				Version: continuationVersion(), ExecutionState: validState,
				Reason: continuationReason(gen.ContinuationProto_USER_REQUESTED_CONTINUATION),
			}), state: validState, reason: ContinuationUserRequested,
		}},
		{name: "terminal reason", continuation: &testContinuation{
			serialized: marshalContinuationProto(t, &gen.ContinuationProto{
				Version: continuationVersion(), ExecutionState: validState,
				Reason: continuationReason(gen.ContinuationProto_CURSOR_AFTER_LAST),
			}), state: validState, reason: ContinuationCursorAfterLast,
		}},
		{name: "method state mismatch", continuation: &testContinuation{
			serialized: valid.serialized, state: []byte{9},
			reason: ContinuationQueryExecutionLimitReached,
		}},
		{name: "method reason mismatch", continuation: &testContinuation{
			serialized: valid.serialized, state: validState,
			reason: ContinuationTransactionLimitReached,
		}},
		{name: "unknown field", continuation: &testContinuation{
			serialized: unknown, state: validState,
			reason: ContinuationQueryExecutionLimitReached,
		}},
		{name: "duplicate state", continuation: &testContinuation{
			serialized: duplicateState, state: validState,
			reason: ContinuationQueryExecutionLimitReached,
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			boundary, err := NewContinuationAvailableError(test.continuation)
			if err == nil || boundary != nil {
				t.Fatalf("NewContinuationAvailableError = (%v, %v), want nil, error", boundary, err)
			}
			apiErr := AsError(err)
			if apiErr == nil || apiErr.Code != ErrCodeInvalidContinuation {
				t.Fatalf("error = %T %v, want SQLSTATE %s", err, err, ErrCodeInvalidContinuation)
			}
			if strings.Contains(err.Error(), "TOP_SECRET_CANARY") {
				t.Fatalf("validation error leaked token contents: %q", err)
			}
		})
	}
}

func TestContinuationAvailableErrorDoesNotDecodeOpaqueCompiledStatement(t *testing.T) {
	state := []byte{1, 2, 3}
	producer := queryBoundaryContinuation(t, state)

	// CompiledStatement.arguments is field 4. If the carrier recursively
	// unmarshals this opaque field, these 32K empty child messages become 32K
	// heap objects despite occupying only 64 KiB on the wire. The top-level
	// carrier scanner must instead validate framing and copy the bytes once.
	compiledStatement := bytes.Repeat([]byte{0x22, 0x00}, 32<<10)
	producer.serialized = protowire.AppendTag(producer.serialized, 5, protowire.BytesType)
	producer.serialized = protowire.AppendBytes(producer.serialized, compiledStatement)

	allocations := testing.AllocsPerRun(3, func() {
		boundary, err := NewContinuationAvailableError(producer)
		if err != nil || boundary == nil {
			t.Fatalf("NewContinuationAvailableError = (%v, %v)", boundary, err)
		}
	})
	if allocations > 64 {
		t.Fatalf("opaque compiled statement caused %.0f allocations, want <= 64", allocations)
	}
}

func TestContinuationAvailableErrorZeroValueIsSafe(t *testing.T) {
	var boundary ContinuationAvailableError
	if boundary.Continuation() != nil {
		t.Fatal("zero-value boundary returned a continuation")
	}
	if boundary.Error() != continuationAvailableMessage {
		t.Fatalf("zero-value boundary Error = %q", boundary.Error())
	}
}
