# WS-C queue follow-up: error context, time/cancellation, closed enum widths

Tracking: TODO.md, “WS-C queue error context and limit/enum boundary coverage”.
This supersedes the parent directory's source verification for the files changed
here; parent logs/hashes retain their historical meaning. This is not WS-C
implementation acceptance and does not enable format15/state4.

## Changes and source authority

PendingWritesQueue.toQueueEntry in tagged Java attaches KEY_TUPLE, reader VERSION,
STORED_VERSION, EXPECTED_TYPE and ACTUAL_TYPE at the appropriate error sites. Go
now carries these structured fields rather than mislabeling the queue key as a
record PRIMARY_KEY. EXPECTED_TYPE uses the bound protobuf full name, a stable
language-independent identity; Java's value is its generated Java class name.
The JVM test asserts the Java class and corresponding Go protobuf identity,
compares the actual URL and optional version fields, and pins the queue key.
Existing index-corruption errors retain their previous formatting.

Java's CursorLimitManager gives one free initial pass, anchors the queue clock at
cursor creation rather than transaction/shared-state creation, and prioritizes
scan > byte > time. The new real-FDB/sim-clock table exercises six cases: old shared
state/new cursor, expired split-entry completion, source exhaustion over time,
scan over byte/time, byte over time, and fail-mode interruption mid-assembly.
A compiled mutation reusing the shared state's old clock fails the first case;
the original file was restored byte-for-byte before final verification.
Cancellation/close coverage proves canceled calls consume no physical KV or entry,
resume retains the next entry, repeated Close is safe, and a closed cursor errors.

Generated Java IndexBuildProto.java reads operation with `int tmpRaw =
input.readEnum()` before enum lookup and sends unknown values through
mergeUnknownVarintField. New live JVM cases exposed two bugs:
1. Overwide wire varints whose low int32 is a valid operation were rejected by Go.
2. Overwide unknown enum values retained different bytes on Go reserialization.
Go now classifies int32 values and stores unknown enum values with Java's signed
normalization. The envelope matrix has 19 cases, including positive overwide known
operations, overwide unknown values, and negative unknown values. Successful index
payload cases compare reserialized payload bytes as well as decoded operation.
The two observed-red logs are retained, followed by the restored/fixed green run.

## Verification

- Focused race: recordlayer 13/3449 and conformance 4/1515 selected specs passed;
  both targets executed uncached with --@rules_go//go/config:race. Ordinary Go
  tests and eight fuzz seed cases also execute in these targets.
- Final just test: 93/93 targets pass, 2 executed / 91 cached. The earlier full run
  after structured errors executed 43/93 targets; after enum fixes it executed
  4/93. No claim that the final run was wholly uncached.
- FuzzPendingQueuePayload: 15 seconds, 26,623,725 executions, eight seed cases,
  no failures. Bazel warned that coverage instrumentation was absent, so this was
  unguided fuzzing, NOT coverage-guided evidence. The retained fuzz target checks
  required closed-enum validity and normalization round-trip stability.
- Six source hashes unchanged after final race and full verification.

Logs adjacent include full/race runs, enum red/green, time, cancellation, fuzz and
clock mutation. Shared maintenance gating, replay, policy/state and lifecycle
integration remain open, followed by completed-milestone implementation reviews.
No publication or operational changes occurred.
