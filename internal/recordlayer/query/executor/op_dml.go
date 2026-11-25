// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/internal/recordlayer/protoname"
	"fdb.dev/internal/unicode15"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func executeDelete(
	ctx context.Context,
	p *plans.RecordQueryDeletePlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	if err := enforceSerializable(props, "RecordQueryDeletePlan"); err != nil {
		return nil, err
	}
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	defer innerCursor.Close()

	// Pre-materialize the full target set BEFORE deleting anything. A resource-limit
	// cut-off (CollectAllBounded → errIfBufferTruncated → 54F01) must abort the DELETE
	// with ZERO records removed — never leave a partially-applied DELETE staged in an
	// explicit transaction that a later commit would persist (RFC-106a). DML runs
	// in one transaction, so the target set is bounded by the tx; the materialization
	// cap is the memory backstop.
	targets, _, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "DELETE target set")
	if err != nil {
		return nil, err
	}

	// Re-check the statement deadline AFTER collection and BEFORE any mutation: if the
	// deadline already passed (collection itself is ctx-gated, but the window after it
	// is not), abort with ZERO records changed (RFC-106a). The mutation loop then
	// runs to completion uninterrupted — checking ctx mid-loop would reintroduce the
	// partial-mutation hazard pre-materialization exists to prevent; the loop only
	// stages local writes over a tx-bounded target set.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// RFC-130: the DML results echo is NOT separately byte-charged.
	// The mutation's memory is bounded by its pre-materialized + charged target
	// set (CollectAllBounded above). Charging the echo here would (a) for DELETE
	// re-count the same already-charged target rows, and (b) fire AFTER
	// store.DeleteRecord/SaveRecord has staged a write — and runInTx does NOT roll
	// back on a statement error, so a 54F01 mid-loop could persist a PARTIAL
	// mutation. The no-partial-mutation guarantee (all accounting before the
	// mutation loop) outranks a ~2x precision gain on the echo.
	var results []QueryResult
	for _, qr := range targets {
		if qr.PrimaryKey == nil {
			continue
		}
		var deleted bool
		var err error
		if props.DryRun {
			// DRY RUN: validate + preview the delete without staging a write
			// (Java RecordQueryDeletePlan + dryRunDeleteRecordAsync). The
			// `if deleted` echo filter below is preserved (Java's
			// .filter(isDeleted -> isDeleted)) so only would-be-deleted PKs echo.
			deleted, err = store.DryRunDeleteRecord(qr.PrimaryKey)
		} else {
			deleted, err = store.DeleteRecord(qr.PrimaryKey)
		}
		if err != nil {
			return nil, fmt.Errorf("executor: deleting record pk=%v: %w", qr.PrimaryKey, err)
		}
		if deleted {
			results = append(results, qr)
		}
	}
	return recordlayer.FromList(results), nil
}

func executeInsert(
	ctx context.Context,
	p *plans.RecordQueryInsertPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	if err := enforceSerializable(props, "RecordQueryInsertPlan"); err != nil {
		return nil, err
	}
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	defer innerCursor.Close()

	// Materialize the inner rows BEFORE writing any record so that
	// INSERT … SELECT reading the target table doesn't re-scan its own
	// freshly-inserted rows (the Halloween problem). Bounded by the
	// materialization limit, the same guard the other buffering operators
	// use. (Note: a single INSERT that paginates across transactions can
	// still re-read across page boundaries — that extreme case is a known
	// limitation, RFC-035.)
	innerRows, _, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "INSERT source")
	if err != nil {
		return nil, err
	}

	// Re-check the statement deadline after collection, before any write — abort
	// with ZERO records inserted if already expired (RFC-106a; see executeDelete).
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	built, err := buildInsertMessages(p, store, props.State, innerRows)
	if err != nil {
		return nil, err
	}
	return saveInsertMessages(store, props, built)
}

func buildInsertMessages(
	p *plans.RecordQueryInsertPlan,
	store *recordlayer.FDBRecordStore,
	state *recordlayer.ExecuteState,
	innerRows []QueryResult,
) ([]proto.Message, error) {
	// Resolved lazily on the first computed-row datum.
	var targetDesc protoreflect.MessageDescriptor

	// Phase 1: build every record to insert and charge its ACTUAL size against the
	// budget — BEFORE any write. Charging the built record (not the source row) accounts
	// INSERT … VALUES with a large literal / a growing projection for its true bytes;
	// all charging precedes phase 2's saves, so a budget
	// breach — or any build error — returns with zero SaveRecord calls (no partial
	// INSERT). The built messages ARE the echo content, so no extra residency.
	// proto.Size is gated on HasMemLimit (zero-overhead when off).
	built := make([]proto.Message, 0, len(innerRows))
	for _, qr := range innerRows {
		// INSERT always coerces the inner result to the target type (Java's
		// InsertExpression computation value), so build from the row datum
		// — INSERT … VALUES (Explode of literal RecordConstructors) and
		// INSERT … SELECT (projection aliased to the target columns) both
		// produce a datum keyed by the target column names. A datum-less
		// stored record (rare) is saved as-is.
		var msg proto.Message
		var err error
		if datum := positionalToMap(qr.Positional); datum != nil {
			if targetDesc == nil {
				rt := store.GetMetaData().GetRecordType(p.GetTargetRecordType())
				if rt == nil {
					return nil, fmt.Errorf("executor: INSERT target record type %q not found", p.GetTargetRecordType())
				}
				targetDesc = rt.Descriptor
			}
			msg, err = buildInsertRecord(targetDesc, datum)
			if err != nil {
				return nil, err
			}
		} else {
			if qr.Record == nil || qr.Record.Record == nil {
				continue
			}
			msg = qr.Record.Record
		}

		if state.HasMemLimit() {
			// Match the stored-row estimator: proto wire size PLUS the packed PK tuple
			// the echo's FDBStoredRecord holds separately. The PK is not
			// assigned until SaveRecord, so derive it from the built record via the target
			// type's primary-key expression (best-effort: a derivation error charges the
			// record size alone — still a conservative ceiling for the dominant payload).
			pkBytes := int64(0)
			if rt := store.GetMetaData().GetRecordType(p.GetTargetRecordType()); rt != nil && rt.PrimaryKey != nil {
				// The record type is supplied because a record-type-prefixed
				// primary key resolves its leading component from it; without
				// it the estimate would miss that component entirely.
				stored := &recordlayer.FDBStoredRecord[proto.Message]{RecordType: rt, Record: msg}
				if kt, kerr := rt.PrimaryKey.Evaluate(stored, msg); kerr == nil && len(kt) > 0 {
					pk := make(tuple.Tuple, len(kt[0]))
					for i, e := range kt[0] {
						pk[i] = e
					}
					pkBytes = int64(len(pk.Pack()))
				}
			}
			if cerr := state.ChargeMemory(int64(proto.Size(msg)) + pkBytes); cerr != nil {
				return nil, cerr
			}
		}
		built = append(built, msg)
	}
	return built, nil
}

func saveInsertMessages(
	store *recordlayer.FDBRecordStore,
	props recordlayer.ExecuteProperties,
	built []proto.Message,
) (recordlayer.RecordCursor[QueryResult], error) {
	// Phase 2: save the already-charged records (no further charging — the budget is
	// settled before the first write).
	results := make([]QueryResult, 0, len(built))
	if !props.DryRun {
		// INSERT's store pipeline sizer selects the save/read window (Java default: one).
		saved, serr := store.SaveRecordsPipelined(built, recordlayer.RecordExistenceCheckErrorIfExists)
		if serr != nil {
			return nil, fmt.Errorf("executor: inserting record: %w", serr)
		}
		for _, stored := range saved {
			results = append(results, FromStoredRecord(stored))
		}
		return recordlayer.FromList(results), nil
	}
	for _, msg := range built {
		// DRY RUN: validate (incl. the existence check → 23505 on an existing
		// PK, parity with the real path) and preview the insert without
		// staging a write; echo from the returned would-be-stored record
		// (Java RecordQueryInsertPlan + dryRunSaveRecordAsync), not a real save.
		stored, serr := store.DryRunSaveRecord(msg, recordlayer.RecordExistenceCheckErrorIfExists)
		if serr != nil {
			return nil, fmt.Errorf("executor: inserting record: %w", serr)
		}
		results = append(results, FromStoredRecord(stored))
	}
	return recordlayer.FromList(results), nil
}

// buildInsertRecord materializes a proto message of the target record
// type from a computed row datum (column name → value). Used when the
// INSERT inner produces computed rows (the literal-VALUES Explode, or a
// projected SELECT) rather than stored records.
//
// It iterates the TARGET fields and pulls each from the datum
// (case-insensitively), ignoring datum keys that don't name a target
// column. This matters for INSERT … SELECT: the projection is aliased to
// the target columns, but the datum also carries the projection's own
// output names — those extra keys must be ignored, not error.
//
// For INSERT … VALUES, arity / NOT NULL / "expected Record but got
// Primitive" are enforced at plan time (buildInsertValuesArray). For
// INSERT … SELECT, a NULL projected into a NOT NULL column is NOT caught
// here — it falls through to the record store's Required-field marshal at
// save time (a less precise error than the plan-time NOT_NULL_VIOLATION).
// This matches Java, where proto enforcement is the backstop for dynamic
// sources; it's intentional, not an oversight.
func buildInsertRecord(desc protoreflect.MessageDescriptor, datum map[string]any) (proto.Message, error) {
	msg := dynamicpb.NewMessage(desc)
	refl := msg.ProtoReflect()
	// An exact key wins; the folded match is a fallback only for a key no
	// other datum key folds to, so quoted columns differing only in case
	// ("COLUMN", "column") each keep their own value as in Java.
	folded := make(map[string]any, len(datum))
	foldedCount := make(map[string]int, len(datum))
	for k, v := range datum {
		lk := unicode15.ToLowerString(k)
		folded[lk] = v
		foldedCount[lk]++
	}
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		v, ok := datum[string(fd.Name())]
		if !ok {
			lk := unicode15.ToLowerString(string(fd.Name()))
			if foldedCount[lk] == 1 {
				v, ok = folded[lk]
			}
		}
		if !ok || v == nil {
			continue // absent / NULL → leave field unset (SQL NULL)
		}
		// INSERT … VALUES pre-converts each field to a protoreflect.Value
		// at plan time. Composite values can belong to an equivalent planning
		// descriptor rather than the target schema descriptor, so re-home them
		// before Set; protobuf rejects a foreign message/list descriptor. Java's
		// MessageHelpers.deepCopyMessageIfNeeded does the same by field number.
		// Projected-SELECT rows carry plain Go values, converted below.
		if pv, ok := v.(protoreflect.Value); ok {
			rematerialized, err := rematerializeProtoValue(fd, pv)
			if err != nil {
				return nil, err
			}
			refl.Set(fd, rematerialized)
			continue
		}
		pv, err := goToProtoValue(fd, v)
		if err != nil {
			return nil, err
		}
		refl.Set(fd, pv)
	}
	return msg, nil
}

// updateTargetField is the message and descriptor field an UPDATE assigns,
// addressed by the transform's resolved ordinals as Java's
// MessageHelpers.transformMessage addresses targetDescriptor.getFields().get(index),
// level by level through struct fields. A struct on the way that is NULL is
// created, as Java's transformation builds the nested message whether or not
// the current one is null. The descriptor carries storage names
// (ProtoUtils.toProtoBufCompliantName escapes '.', '$' and a leading '__');
// each field at an ordinal must be the one the transform names, its user
// identifier decoded once, or the record is not of the type the statement was
// planned over.
func updateTargetField(msg protoreflect.Message, t expressions.UpdateTransform) (protoreflect.Message, protoreflect.FieldDescriptor, error) {
	if len(t.FieldOrdinals) == 0 || len(t.FieldOrdinals) != len(t.FieldNames) {
		return nil, nil, fmt.Errorf("executor: update field %q has %d ordinals for %d names", t.FieldPath(), len(t.FieldOrdinals), len(t.FieldNames))
	}
	for i, ordinal := range t.FieldOrdinals {
		fields := msg.Descriptor().Fields()
		if ordinal < 0 || ordinal >= fields.Len() {
			return nil, nil, fmt.Errorf("executor: update field %q has ordinal %d outside the descriptor's %d fields", t.FieldPath(), ordinal, fields.Len())
		}
		fd := fields.Get(ordinal)
		if name := protoname.ToUserIdentifier(string(fd.Name())); name != t.FieldNames[i] {
			return nil, nil, fmt.Errorf("executor: update field %q at ordinal %d is field %q of the descriptor", t.FieldPath(), ordinal, name)
		}
		if i == len(t.FieldOrdinals)-1 {
			return msg, fd, nil
		}
		// Only a struct is descended (the resolver's and the plan's rule): not
		// an array, a map, a UUID or a nullable array's wrapper message.
		if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() ||
			string(fd.Message().FullName()) == uuidProtoMessageName || values.IsWrappedArrayDescriptor(fd.Message()) {
			return nil, nil, fmt.Errorf("executor: update field %q: %q is not a struct", t.FieldPath(), t.FieldNames[i])
		}
		msg = msg.Mutable(fd).Message()
	}
	return nil, nil, fmt.Errorf("executor: update field %q is empty", t.FieldPath())
}

func executeUpdate(
	ctx context.Context,
	p *plans.RecordQueryUpdatePlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	if err := enforceSerializable(props, "RecordQueryUpdatePlan"); err != nil {
		return nil, err
	}
	innerCursor, err := ExecutePlan(ctx, p.GetInner(), store, evalCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, err
	}
	defer innerCursor.Close()

	transforms := p.GetTransforms()
	// Each transform's field type, read off the planned target type once: the
	// NULL assignment check needs its nullability.
	targetRecord, ok := p.GetTargetType().(*values.RecordType)
	if !ok {
		return nil, fmt.Errorf("executor: update target type %v is not a record", p.GetTargetType())
	}
	leafTypes := make([]values.Type, len(transforms))
	for i, t := range transforms {
		leaf, err := plans.UpdateTargetFieldType(targetRecord, t)
		if err != nil {
			return nil, err
		}
		leafTypes[i] = leaf
	}
	inputQOV, err := requireSoleInputQOV(p)
	if err != nil {
		return nil, fmt.Errorf("update input owner: %w", err)
	}
	targetQOV, err := values.NewQuantifiedObjectValue(p.GetTargetAlias(), p.GetTargetType())
	if err != nil {
		return nil, fmt.Errorf("update target owner: %w", err)
	}

	// Pre-materialize the full target set BEFORE applying any update — a resource-limit
	// cut-off must abort with ZERO records changed, never a partially-applied UPDATE
	// staged in an explicit transaction (RFC-106a; see executeDelete).
	targets, _, err := CollectAllBounded(ctx, innerCursor, props.State, props.GetMaterializationLimit(), "UPDATE target set")
	if err != nil {
		return nil, err
	}

	// Re-check the statement deadline after collection, before any mutation — abort
	// with ZERO records changed if already expired (RFC-106a; see executeDelete).
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	built, err := buildPendingUpdates(evalCtx, props.State, targets, transforms, leafTypes, inputQOV, targetQOV)
	if err != nil {
		return nil, err
	}
	resultType, ok := p.GetResultType().(*values.RecordType)
	if !ok || resultType == nil || len(resultType.Fields) != 2 {
		return nil, layoutBindingError(values.LayoutTypeMismatch, "update result is not exact {old,new}")
	}
	return applyPendingUpdates(store, props, resultType, built)
}

type pendingUpdate struct {
	old QueryResult
	msg proto.Message
}

func buildPendingUpdates(
	evalCtx *EvaluationContext,
	state *recordlayer.ExecuteState,
	targets []QueryResult,
	transforms []expressions.UpdateTransform,
	leafTypes []values.Type,
	inputQOV, targetQOV values.QuantifiedObjectValue,
) ([]pendingUpdate, error) {
	// Phase 1: build every updated record and charge its ACTUAL post-transform size
	// against the budget — BEFORE any write is staged. Charging the built record (not
	// the source row) accounts a growing UPDATE (small row → large value) for its true
	// bytes; doing all of it before phase 2's saves means a
	// budget breach — or any build/transform error — returns with zero SaveRecord calls
	// (no partial mutation). The built messages ARE the echo content, so no extra
	// residency. proto.Size is gated on HasMemLimit (zero-overhead when off).
	built := make([]pendingUpdate, 0, len(targets))
	for _, qr := range targets {
		if qr.Record == nil || qr.Record.Record == nil {
			continue
		}

		msg := proto.Clone(qr.Record.Record)
		refl := msg.ProtoReflect()
		if qr.Positional == nil {
			return nil, layoutBindingError(values.LayoutRuntimeShape,
				"update target carries no positional row")
		}
		// SET expressions are resolved against the SQL target name (A.x),
		// while the selected physical child is addressed through this plan's
		// generated quantifier edge. Declare both exact views of the same stored
		// row through the ordinary frontier binder. The selected child's
		// OrdinalLayout remains authoritative; a same-spelled foreign type or an
		// unrelated owner therefore stays loud instead of borrowing the ambient
		// positional row.
		rowCtx, err := frontierRowContext(
			qr.Positional, evalCtx, hasBindingContext(evalCtx) || (evalCtx != nil && len(evalCtx.constants) > 0), inputQOV, targetQOV)
		if err != nil {
			return nil, fmt.Errorf("update target binding: %w", err)
		}

		for i, t := range transforms {
			owner, fd, err := updateTargetField(refl, t)
			if err != nil {
				return nil, err
			}
			newVal, err := t.NewValue.Evaluate(rowCtx)
			if err != nil {
				return nil, err
			}
			if newVal == nil {
				// Java's coerceObject refuses a NULL for a slot whose type is
				// not nullable (SemanticException NULL_ASSIGNMENT), when the
				// row is transformed.
				if !leafTypes[i].IsNullable() {
					return nil, &values.NullAssignmentError{Field: t.FieldPath(), To: leafTypes[i]}
				}
				owner.Clear(fd)
			} else {
				pv, err := goToProtoValue(fd, newVal)
				if err != nil {
					return nil, fmt.Errorf("executor: converting update value for %q: %w", t.FieldPath(), err)
				}
				owner.Set(fd, pv)
			}
		}

		if state.HasMemLimit() {
			// Match the stored-row estimator (estimateQueryResultBytes): proto wire size
			// PLUS the packed PK tuple, which the echo's FDBStoredRecord holds separately.
			// An UPDATE does not change the PK, so the target's PK is the echo's PK.
			if err := state.ChargeMemory(int64(proto.Size(msg)) + int64(len(qr.Record.PrimaryKey.Pack()))); err != nil {
				return nil, err
			}
		}
		built = append(built, pendingUpdate{old: qr, msg: msg})
	}
	return built, nil
}

func applyPendingUpdates(
	store *recordlayer.FDBRecordStore,
	props recordlayer.ExecuteProperties,
	resultType *values.RecordType,
	built []pendingUpdate,
) (recordlayer.RecordCursor[QueryResult], error) {
	// Phase 2: save the already-charged records (no further charging — the budget is
	// settled before the first write).
	results := make([]QueryResult, 0, len(built))
	for _, pending := range built {
		var stored *recordlayer.FDBStoredRecord[proto.Message]
		var err error
		if props.DryRun {
			// DRY RUN: validate + preview the update without staging a write; echo
			// from the returned would-be-stored record (Java RecordQueryUpdatePlan
			// + dryRunSaveRecordAsync), not a real save.
			stored, err = store.DryRunSaveRecord(pending.msg, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
		} else {
			stored, err = store.SaveRecordWithOptions(pending.msg, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
		}
		if err != nil {
			return nil, fmt.Errorf("executor: updating record: %w", err)
		}
		newResult := FromStoredRecord(stored)
		results = append(results, QueryResult{
			Positional: &PositionalRow{
				Type:  resultType,
				Slots: []any{pending.old.Positional, newResult.Positional},
			},
			Record:     newResult.Record,
			PrimaryKey: newResult.PrimaryKey,
		})
	}
	return recordlayer.FromList(results), nil
}
