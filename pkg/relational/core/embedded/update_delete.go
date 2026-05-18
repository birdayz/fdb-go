package embedded

import (
	"context"
	"database/sql/driver"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// UPDATE and DELETE executors.
//
// Both run the same pushdown dispatcher (pkPushdownCursor) over the
// WHERE clause to pick the narrowest cursor available — PK equality
// / range / IN-list / composite variants / secondary index shapes —
// then re-apply the full WHERE via evalPredicate on each yielded
// row for correctness.
//
// UPDATE clones each matching record, applies every SET element with
// evalExpr (against the cloned msg so `SET x = x + 1` sees pre-
// update values), enforces NOT NULL on required columns, and saves
// the cloned message. Secondary UNIQUE indexes can still fire during
// SaveRecord, surfaced via wrapSaveRecordError so callers see 23505
// rather than the raw recordlayer error type.
//
// DELETE removes each matching record by primary key via
// store.DeleteRecord. No wrap needed — DeleteRecord doesn't have
// INSERT-style unique-violation failure modes.
//
// Both record the source alias via pushSourceAliases so correlated
// EXISTS / IN subqueries in WHERE resolve outer-row refs through
// the scope stack.
//
// Destined for plan/physical/{update,delete}.go per RFC 021 Phase 1c.

// execUpdate executes UPDATE <table> SET col = val [, ...] [WHERE col = val].
func (c *EmbeddedConnection) execUpdate(ctx context.Context, upd antlrgen.IUpdateStatementContext) (int64, error) {
	if c.sess.Schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.sess.DBPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	rawTableName := functions.FullIdToName(upd.TableName().FullId())
	tableName, resolveErr := functions.ResolveQualifiedTableName(rawTableName, c.sess.Schema)
	if resolveErr != nil {
		return 0, resolveErr
	}
	whereExpr := upd.WhereExpr()
	if whereExpr != nil {
		if err := rejectTopLevelParenthesizedWhere(whereExpr.Expression()); err != nil {
			return 0, err
		}
	}
	updatedElems := upd.AllUpdatedElement()

	var updated int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		updated = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeUndefinedTable, "Unknown table %s", strings.ToUpper(tableName))
		}
		msgDesc := rt.Descriptor

		// Java alignment: UPDATE on a PK column with a non-NULL value
		// is rejected with 'record does not exist' (Java's in-place
		// UPDATE uses the PK value to look up the source row;
		// modifying the PK column leaves no row at the new key on
		// save). The check fires INSIDE the row-update loop, after
		// the NULL-into-NOT-NULL check, so SET id = NULL on a PK
		// column still surfaces the more specific NotNullViolation
		// rather than this PK-rejection.
		pkCols := extractPKUserFields(rt.PrimaryKey)
		pkSet := make(map[string]struct{}, len(pkCols))
		for _, p := range pkCols {
			pkSet[p] = struct{}{}
		}

		ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := c.newStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		cursor := pkPushdownCursor(ctx, c, store, rt, md, whereExpr, tableName)
		defer cursor.Close() //nolint:errcheck

		// Record the source alias so correlated EXISTS / IN inside WHERE
		// can resolve outer-row refs. UPDATE/DELETE don't expose a user
		// alias in the grammar today; descriptor name + tableName match.
		defer c.pushSourceAliases(tableName)()

		for {
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			match, matchErr := evalPredicate(ctx, c, rec.Record, whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}

			cloned := proto.Clone(rec.Record)
			clonedRefl := cloned.ProtoReflect()
			// SQL standard: every SET right-hand side reads the row's
			// PRE-UPDATE state. Evaluate all RHS expressions first
			// against the original (unmodified) record, then apply all
			// assignments in a second pass. Pre-fix Go evaluated each
			// RHS against the in-progress `cloned`, so `UPDATE t SET
			// x = x + y, y = y - x` read the already-updated x in the
			// second SET.
			type pendingUpdate struct {
				fd      protoreflect.FieldDescriptor
				colName string
				val     driver.Value
			}
			pending := make([]pendingUpdate, 0, len(updatedElems))
			for _, elem := range updatedElems {
				colName := functions.FullIdToName(elem.FullColumnName().FullId())
				fd := msgDesc.Fields().ByName(protoreflect.Name(colName))
				if fd == nil {
					// Java verbatim: 'Attempting to query non existing
					// column NAME' (uppercased identifier). Aligned
					//  to match the SELECT path's same
					// alignment.
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
						"Attempting to query non existing column %s", strings.ToUpper(colName))
				}
				val, evalErr := evalExpr(ctx, c, rec.Record, elem.Expression())
				if evalErr != nil {
					return nil, evalErr
				}
				pending = append(pending, pendingUpdate{fd: fd, colName: colName, val: val})
			}
			for _, p := range pending {
				if p.val == nil {
					// UPDATE SET col = NULL on a NOT NULL column must reject
					// with ErrCodeNotNullViolation (23502), matching Java.
					if p.fd.Cardinality() == protoreflect.Required {
						return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
							"NULL value in column %q violates NOT NULL constraint", p.colName)
					}
					clonedRefl.Clear(p.fd)
					continue
				}
				// Java alignment : UPDATE on a PK column
				// with a non-NULL value is rejected with verbatim
				// 'record does not exist' (Java's RecordDoesNotExist
				// from the in-place save lookup). NULL case already
				// handled above (NotNullViolation, more specific).
				if _, isPK := pkSet[p.colName]; isPK {
					return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
						"record does not exist")
				}
				protoVal, convErr := functions.ConvertToProtoValue(p.fd, p.val)
				if convErr != nil {
					return nil, convErr
				}
				clonedRefl.Set(p.fd, protoVal)
			}
			// UPDATE legitimately overwrites an existing record, so no
			// existence check — but secondary UNIQUE indexes can still
			// fire if the UPDATE sets an indexed column to a value
			// another row already holds. Wrap so callers get SQLSTATE
			// 23505 instead of the raw recordlayer error type.
			if _, saveErr := store.SaveRecord(cloned); saveErr != nil {
				return nil, wrapSaveRecordError(saveErr)
			}
			updated++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return updated, nil
}

// execDelete executes DELETE FROM <table> [WHERE col = value].
func (c *EmbeddedConnection) execDelete(ctx context.Context, del antlrgen.IDeleteStatementContext) (int64, error) {
	if c.sess.Schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.sess.DBPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	rawDelTableName := functions.FullIdToName(del.TableName().FullId())
	tableName, resolveErr := functions.ResolveQualifiedTableName(rawDelTableName, c.sess.Schema)
	if resolveErr != nil {
		return 0, resolveErr
	}
	whereExpr := del.WhereExpr()
	if whereExpr != nil {
		if err := rejectTopLevelParenthesizedWhere(whereExpr.Expression()); err != nil {
			return 0, err
		}
	}

	var deleted int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		deleted = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		rt := md.GetRecordType(tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeUndefinedTable, "Unknown table %s", strings.ToUpper(tableName))
		}

		ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := c.newStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		cursor := pkPushdownCursor(ctx, c, store, rt, md, whereExpr, tableName)
		defer cursor.Close() //nolint:errcheck

		// Record the source alias so correlated EXISTS / IN inside WHERE
		// can resolve outer-row refs (mirrors execUpdate).
		defer c.pushSourceAliases(tableName)()

		for {
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			match, matchErr := evalPredicate(ctx, c, rec.Record, whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}
			if _, delErr := store.DeleteRecord(rec.PrimaryKey); delErr != nil {
				return nil, delErr
			}
			deleted++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
