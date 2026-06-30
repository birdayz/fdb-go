# RFC-160: `UPDATE` of a PRIMARY KEY column — the `XXXXX` error is Java-faithful, not a bug (item 1085)

**Status:** Implemented
**Gate:** Torvalds + codex + @claude (conformance/error-classification; no production change → no Graefe)

## Problem (as filed) vs. reality

TODO item 1085 framed `UPDATE t SET id = <new> WHERE id = <old>` (id is the PK) returning a "leaky
XXXXX (`ErrCodeUnknown`) 'record does not exist'" as a Go bug to fix — "either a clean user-facing
rejection ('cannot update primary key', proper 42-class SQLSTATE) or record relocation, matching
Java." **Reading Java settles it the other way: the `XXXXX` is exactly what Java produces.**

## Investigation (Java is the spec)

- **Behavior is identical.** Java's `RecordQueryUpdatePlan.saveRecordAsync`
  (`RecordQueryUpdatePlan.java:105`) saves the post-transform message with
  `ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED`. When the SET changes the PK, the save targets the
  NEW primary key (which has no record) and the existence check throws
  `RecordDoesNotExistException`. Go does the same: `executeUpdate` applies the SET and calls
  `SaveRecordWithOptions(msg, RecordExistenceCheckErrorIfNotExistsOrTypeChanged)`, which fails the
  existence check at the new PK. Neither engine relocates; both fail-closed (table unchanged).
- **SQLSTATE is identical.** Java's `ExceptionUtil.recordCoreToRelationalException`
  (`ExceptionUtil.java:59-84`) maps specific `RecordCoreException` subtypes (AlreadyExists/Index
  uniqueness → `UNIQUE_CONSTRAINT_VIOLATION` 23505, Deserialization → `DESERIALIZATION_FAILURE`,
  MetaData → `SYNTAX_OR_ACCESS_VIOLATION`, …) — but **`RecordDoesNotExistException` is not in the
  list, so it falls to the default `ErrorCode.UNKNOWN`** (line 63). And `ErrorCode.UNKNOWN("XXXXX")`
  (`ErrorCode.java:172`) is byte-identical to Go's `ErrCodeUnknown = "XXXXX"` (`errcode.go:144`).

So `UPDATE SET <pk>` yields **`XXXXX` in both engines**. There is no clean "cannot update primary
key" SQLSTATE in Java to match — inventing one in Go would be a **Go-only divergence**, forbidden by
the conformance principle ("doesn't work in Java → doesn't work in Go"). The relocation alternative
is likewise not what Java does.

## Fix

No production change — Go already matches Java. Instead:
- **Reframe TODO 1085** as resolved/Java-faithful (do NOT "fix" it into a divergence).
- **Strengthen the sentinel** `update_primary_key_probe_test.go` from a "known-issue, don't over-pin
  the wording" probe into a **conformance pin**: assert the SQLSTATE is `XXXXX` (matching Java's
  `RecordDoesNotExistException → ErrorCode.UNKNOWN`), keep the no-corruption + fail-closed invariants,
  and document that this is Java-faithful so a future reader doesn't replace it with a clean
  Go-only code. (The leaky `executor: updating record:` *message* prefix is a cosmetic wording
  difference, not an SQLSTATE divergence; the cross-engine contract is the SQLSTATE, which matches.)

## Test plan

- `update_primary_key_probe_test.go`: `UPDATE t SET id = 99 WHERE id = 1` → SQLSTATE `XXXXX`
  (Java-faithful), table unchanged, non-PK UPDATE still works.
- Full `just test` green.
