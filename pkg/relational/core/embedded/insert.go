package embedded

import (
	"context"
	"errors"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// INSERT executor.
//
//   execInsert       INSERT INTO t (cols) VALUES (...), (...)
//   execInsertSelect INSERT INTO t (cols) SELECT … (two-phase: run
//                    SELECT in its own transaction, then insert rows
//                    — NOT atomic with respect to concurrent writers,
//                    known TOCTOU limitation)
//   wrapSaveRecordError  translates record-layer SaveRecord failures
//                    into api.Error values carrying the Java-matching
//                    SQLSTATE (23505 unique violation, 22000 conversion,
//                    etc.). Shared with UPDATE / DELETE paths.
//
// Both executors enforce NOT NULL at save time (Required proto fields),
// arity validation (explicit vs derived column list), and
// ErrorIfExists semantics so duplicate PRIMARY KEY surfaces 23505
// rather than silently overwriting (diverges from bare SaveRecord).
// Destined for plan/physical/insert.go per RFC 021 Phase 1c.

// wrapSaveRecordError translates record-layer-level errors thrown by
// store.SaveRecord into api.Error values carrying the Java-matching
// SQLSTATE. Without this, SQL callers would see a raw recordlayer
// error type that doesn't `errors.As` to `*api.Error`, defeating the
// SQLSTATE contract that the relational layer documents.
//
// Java's relational layer performs the equivalent mapping in
// RelationalException.toRelationalException (class 23 -> SQLSTATE).
func wrapSaveRecordError(err error) error {
	if err == nil {
		return nil
	}
	var uniqErr *recordlayer.RecordIndexUniquenessViolationError
	if errors.As(err, &uniqErr) {
		return api.WrapErrorf(err, api.ErrCodeUniqueConstraintViolation,
			"unique index %q violated: value %v already exists", uniqErr.IndexName, uniqErr.IndexKey)
	}
	var existsErr *recordlayer.RecordAlreadyExistsError
	if errors.As(err, &existsErr) {
		// Java verbatim: 'record already exists' (the
		// RecordAlreadyExistsException.getMessage() — fdb-relational
		// doesn't include the PK in the message).
		return api.WrapErrorf(err, api.ErrCodeUniqueConstraintViolation,
			"record already exists")
	}
	var keySizeErr *recordlayer.IndexKeySizeError
	if errors.As(err, &keySizeErr) {
		return api.WrapErrorf(err, api.ErrCodeInvalidParameter,
			"index %q key size %d exceeds limit %d", keySizeErr.IndexName, keySizeErr.KeySize, keySizeErr.Limit)
	}
	var valueSizeErr *recordlayer.IndexValueSizeError
	if errors.As(err, &valueSizeErr) {
		return api.WrapErrorf(err, api.ErrCodeInvalidParameter,
			"index %q value size %d exceeds limit %d", valueSizeErr.IndexName, valueSizeErr.ValueSize, valueSizeErr.Limit)
	}
	// Already a relational-layer error (e.g. from validation upstream of
	// the save) — pass through untouched.
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return err
	}
	// Unknown record-layer error — wrap as internal so callers still see a
	// stable SQLSTATE and can `errors.As` to *api.Error for logging. The
	// original record-layer error is preserved via %w.
	return api.WrapErrorf(err, api.ErrCodeInternalError, "record save failed")
}

// execInsert executes INSERT INTO table (col1, col2, ...) VALUES (...), (...).
func (c *EmbeddedConnection) execInsert(ctx context.Context, ins antlrgen.IInsertStatementContext) (int64, error) {
	if c.sess.Schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.sess.DBPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	// Explicit column list (optional).
	colCtx := ins.UidListWithNestingsInParens()
	var explicitCols []string // nil = no column list (use schema order)
	if colCtx != nil {
		for _, uw := range colCtx.UidListWithNestings().AllUidWithNestings() {
			explicitCols = append(explicitCols, functions.StripIdentifierQuotes(uw.Uid().GetText()))
		}
	}

	rawTableName := functions.FullIdToName(ins.TableName().FullId())
	tableName, resolveErr := functions.ResolveQualifiedTableName(rawTableName, c.sess.Schema)
	if resolveErr != nil {
		return 0, resolveErr
	}

	// Handle INSERT INTO ... SELECT (insertStatementValueSelect).
	if selCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueSelectContext); ok {
		// Java alignment (TODO #55): fdb-relational rejects any
		// INSERT…(cols) SELECT shape — `setting column ordering for
		// insert with select is not supported`. Plain `INSERT INTO t
		// SELECT …` (no column list) is accepted by both engines.
		if len(explicitCols) > 0 {
			return 0, api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"setting column ordering for insert with select is not supported")
		}
		return c.execInsertSelect(ctx, tableName, explicitCols, selCtx.QueryExpressionBody())
	}

	// Only handle VALUES path.
	valCtx, ok := ins.InsertStatementValue().(*antlrgen.InsertStatementValueValuesContext)
	if !ok {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "only INSERT ... VALUES (...) is supported")
	}

	var totalRows int64
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		totalRows = 0 // reset on retry
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

		// Resolve column order: explicit list or all fields in descriptor order.
		cols := explicitCols
		if cols == nil {
			fds := msgDesc.Fields()
			cols = make([]string, fds.Len())
			for i := 0; i < fds.Len(); i++ {
				cols[i] = string(fds.Get(i).Name())
			}
		}

		for _, rowCtx := range valCtx.AllRecordConstructorForInsert() {
			exprs := rowCtx.AllExpressionWithOptionalName()
			// Java alignment (inserts-updates-deletes.yamsql):
			//   - Explicit column list + arity mismatch (either direction) →
			//     42601 SYNTAX_ERROR. Java 4.1.5.0+ treats the mismatch
			//     as a parse-level error because the user named the target
			//     columns explicitly.
			//   - Implicit column list (schema-derived) + fewer VALUES than
			//     columns → 22000 CANNOT_CONVERT_TYPE. Java surfaces this
			//     as a type-conversion error since the partial tuple can't
			//     be coerced into the full row.
			if len(exprs) != len(cols) {
				if explicitCols != nil {
					return nil, api.NewErrorf(api.ErrCodeSyntaxError,
						"INSERT column list has %d columns but VALUES has %d", len(cols), len(exprs))
				}
				// Java verbatim for plain INSERT VALUES with arity
				// mismatch: 'provided record cannot be assigned as its
				// type is incompatible with the target type'.
				return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
					"provided record cannot be assigned as its type is incompatible with the target type")
			}
			msg := dynamicpb.NewMessage(msgDesc)
			for i, col := range cols {
				fd := msgDesc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found in table %q", col, tableName)
				}
				// Java alignment (TODO #60): a parenthesized expression
				// at a row-constructor slot — `INSERT INTO t VALUES (1,
				// (2+3))` — is treated as a single-element record
				// constructor; Java rejects with byte-equal `expected
				// Record but got Primitive`. Detect the structural
				// shape (PredicatedExpression wrapping a
				// RecordConstructorExpressionAtom) and reject.
				if pred, ok := exprs[i].Expression().(*antlrgen.PredicatedExpressionContext); ok && pred.Predicate() == nil {
					if _, isRC := pred.ExpressionAtom().(*antlrgen.RecordConstructorExpressionAtomContext); isRC {
						return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
							"expected Record but got Primitive")
					}
				}
				val, evalErr := evalExpr(ctx, c, nil, exprs[i].Expression())
				if evalErr != nil {
					return nil, evalErr
				}
				if val == nil {
					// NULL — must reject for NOT NULL columns per SQL standard.
					if fd.Cardinality() == protoreflect.Required {
						return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
							"NULL value in column %q violates NOT NULL constraint", col)
					}
					// Nullable — leave field absent (proto2 optional semantics).
					continue
				}
				protoVal, convErr := functions.ConvertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				msg.Set(fd, protoVal)
			}
			// Catch the case where a NOT NULL column is missing from the
			// explicit column list entirely (no value provided at all).
			fds := msgDesc.Fields()
			for i := 0; i < fds.Len(); i++ {
				fd := fds.Get(i)
				if fd.Cardinality() == protoreflect.Required && !msg.Has(fd) {
					return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
						"column %q has NOT NULL constraint but no value was provided", fd.Name())
				}
			}
			// ErrorIfExists: duplicate PRIMARY KEY raises
			// *recordlayer.RecordAlreadyExistsError which wrapSaveRecordError
			// maps to SQLSTATE 23505 (unique_constraint_violation). Without
			// this check, plain SaveRecord silently overwrites the existing
			// row — divergence from Java's INSERT semantics.
			if _, saveErr := store.SaveRecordWithOptions(msg, recordlayer.RecordExistenceCheckErrorIfExists); saveErr != nil {
				return nil, wrapSaveRecordError(saveErr)
			}
			totalRows++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return totalRows, nil
}

// execInsertSelect implements INSERT INTO table (cols) SELECT ...
// It evaluates the SELECT query and inserts each row into the table.
func (c *EmbeddedConnection) execInsertSelect(ctx context.Context, tableName string, explicitCols []string, body antlrgen.IQueryExpressionBodyContext) (int64, error) {
	if c.sess.Schema == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no schema selected")
	}
	if c.sess.DBPath == "" {
		return 0, api.NewError(api.ErrCodeUnsupportedOperation, "no database selected")
	}

	// Execute the SELECT in a separate transaction from the INSERT. The two operations are
	// not atomic — a concurrent writer may modify rows between the SELECT and INSERT
	// (TOCTOU window). This is a known limitation of the current implementation.
	srcCols, _, srcRows, err := c.execQueryBodyRows(ctx, body)
	if err != nil {
		return 0, err
	}

	var totalRows int64
	_, err = c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		totalRows = 0
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

		// Determine target columns. When the user specifies an explicit
		// column list (`INSERT INTO t (c1, c2) SELECT ...`), match by
		// that list. Otherwise fall back to positional mapping against
		// the table's declared field order — matches Postgres / SQL-92
		// semantics. Previously we used srcCols (the SELECT output
		// names), which broke on expression projections like
		// `SELECT id + 100, v * 2` because the synthetic output name
		// "id+100" isn't a real table field.
		var cols []string
		if explicitCols != nil {
			cols = explicitCols
		} else {
			fds := msgDesc.Fields()
			cols = make([]string, fds.Len())
			for i := 0; i < fds.Len(); i++ {
				cols[i] = string(fds.Get(i).Name())
			}
		}
		if len(cols) != len(srcCols) {
			// Java alignment: column-count mismatch errors 22000.
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.")
		}

		for _, row := range srcRows {
			msg := dynamicpb.NewMessage(msgDesc)
			for i, col := range cols {
				fd := msgDesc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "column %q not found in table %q", col, tableName)
				}
				val := row[i]
				if val == nil {
					// NOT NULL enforcement — matches Java's SQLSTATE 23502.
					if fd.Cardinality() == protoreflect.Required {
						return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
							"NULL value in column %q violates NOT NULL constraint", col)
					}
					continue
				}
				protoVal, convErr := functions.ConvertToProtoValue(fd, val)
				if convErr != nil {
					return nil, convErr
				}
				msg.Set(fd, protoVal)
			}
			// Missing-from-column-list check, same as execInsert.
			fds := msgDesc.Fields()
			for i := 0; i < fds.Len(); i++ {
				fd := fds.Get(i)
				if fd.Cardinality() == protoreflect.Required && !msg.Has(fd) {
					return nil, api.NewErrorf(api.ErrCodeNotNullViolation,
						"column %q has NOT NULL constraint but no value was provided", fd.Name())
				}
			}
			// ErrorIfExists: same rationale as execInsert above.
			if _, saveErr := store.SaveRecordWithOptions(msg, recordlayer.RecordExistenceCheckErrorIfExists); saveErr != nil {
				return nil, wrapSaveRecordError(saveErr)
			}
			totalRows++
		}
		return nil, nil
	})
	if err != nil {
		return 0, err
	}
	return totalRows, nil
}
