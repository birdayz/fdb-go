package embedded

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
)

var errNotFastInsert = fmt.Errorf("not a fast-insert candidate")

// tryFastInsert attempts to execute a simple INSERT INTO t VALUES (...)
// without using the ANTLR parser. Returns (rowCount, nil) on success,
// or (0, errNotFastInsert) to fall back to the normal parse path.
//
// Matches only: INSERT INTO <table> VALUES (<vals>), (<vals>), ...
// No column list, no subquery, no ON DUPLICATE KEY, no DEFAULT.
func (c *EmbeddedConnection) tryFastInsert(ctx context.Context, sql string) (int64, error) {
	if c.sess.Schema == "" || c.sess.DBPath == "" {
		return 0, fmt.Errorf("fast-insert: no schema/db: schema=%q dbpath=%q", c.sess.Schema, c.sess.DBPath)
	}
	fmt.Fprintf(os.Stderr, "[FAST INSERT TRACE] schema=%q dbpath=%q sqllen=%d\n", c.sess.Schema, c.sess.DBPath, len(sql))

	tableName, valueRows, err := parseFastInsert(sql)
	if err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}

	resolvedTable, resolveErr := resolveTableForFastInsert(tableName, c.sess.Schema)
	if resolveErr != nil {
		return 0, errNotFastInsert
	}

	var totalRows int64
	_, txErr := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		totalRows = 0
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, ok := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !ok {
			return nil, errNotFastInsert
		}
		md := rlTmpl.Underlying()
		rt := md.GetRecordType(resolvedTable)
		if rt == nil {
			return nil, errNotFastInsert
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

		fds := msgDesc.Fields()
		numCols := fds.Len()

		for _, vals := range valueRows {
			if len(vals) != numCols {
				return nil, errNotFastInsert
			}
			msg := dynamicpb.NewMessage(msgDesc)
			for i, val := range vals {
				fd := fds.Get(i)
				pv, convErr := convertFastValue(val, fd)
				if convErr != nil {
					return nil, errNotFastInsert
				}
				msg.Set(fd, pv)
			}
			if _, saveErr := store.SaveRecord(msg); saveErr != nil {
				return nil, saveErr
			}
			totalRows++
		}
		return nil, nil
	})
	if txErr != nil {
		return 0, errNotFastInsert
	}
	return totalRows, nil
}

func resolveTableForFastInsert(raw, schema string) (string, error) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) == 2 {
		if !strings.EqualFold(parts[0], schema) {
			return "", errNotFastInsert
		}
		return parts[1], nil
	}
	return raw, nil
}

// parseFastInsert extracts table name and value rows from a simple INSERT.
// Returns errNotFastInsert for anything it can't handle.
func parseFastInsert(sql string) (string, [][]string, error) {
	s := strings.TrimSpace(sql)
	if len(s) < 20 {
		return "", nil, errNotFastInsert
	}

	// Match "INSERT INTO"
	upper := strings.ToUpper(s[:12])
	if !strings.HasPrefix(upper, "INSERT INTO ") {
		return "", nil, errNotFastInsert
	}
	s = strings.TrimSpace(s[12:])

	// Extract table name (until whitespace)
	idx := strings.IndexByte(s, ' ')
	if idx < 1 {
		return "", nil, errNotFastInsert
	}
	tableName := s[:idx]
	s = strings.TrimSpace(s[idx:])

	// Match "VALUES"
	if len(s) < 7 || !strings.EqualFold(s[:6], "VALUES") {
		return "", nil, errNotFastInsert
	}
	s = strings.TrimSpace(s[6:])

	// Parse value rows: (v1, v2), (v3, v4), ...
	var rows [][]string
	for len(s) > 0 {
		if s[0] != '(' {
			return "", nil, errNotFastInsert
		}
		end := strings.IndexByte(s, ')')
		if end < 0 {
			return "", nil, errNotFastInsert
		}
		inner := s[1:end]
		vals := splitValues(inner)
		rows = append(rows, vals)
		s = strings.TrimSpace(s[end+1:])
		if len(s) > 0 && s[0] == ',' {
			s = strings.TrimSpace(s[1:])
		}
	}
	if len(rows) == 0 {
		return "", nil, errNotFastInsert
	}
	return tableName, rows, nil
}

func splitValues(s string) []string {
	var vals []string
	var current strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\'' && !inQuote {
			inQuote = true
			current.WriteByte(ch)
		} else if ch == '\'' && inQuote {
			if i+1 < len(s) && s[i+1] == '\'' {
				current.WriteString("''")
				i++
			} else {
				current.WriteByte(ch)
				inQuote = false
			}
		} else if ch == ',' && !inQuote {
			vals = append(vals, strings.TrimSpace(current.String()))
			current.Reset()
		} else {
			current.WriteByte(ch)
		}
	}
	vals = append(vals, strings.TrimSpace(current.String()))
	return vals
}

func convertFastValue(val string, fd protoreflect.FieldDescriptor) (protoreflect.Value, error) {
	if strings.EqualFold(val, "null") {
		return protoreflect.Value{}, errNotFastInsert
	}

	switch fd.Kind() {
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return protoreflect.Value{}, errNotFastInsert
		}
		return protoreflect.ValueOfInt64(n), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(val, 10, 32)
		if err != nil {
			return protoreflect.Value{}, errNotFastInsert
		}
		return protoreflect.ValueOfInt32(int32(n)), nil
	case protoreflect.DoubleKind:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return protoreflect.Value{}, errNotFastInsert
		}
		return protoreflect.ValueOfFloat64(f), nil
	case protoreflect.FloatKind:
		f, err := strconv.ParseFloat(val, 32)
		if err != nil {
			return protoreflect.Value{}, errNotFastInsert
		}
		return protoreflect.ValueOfFloat32(float32(f)), nil
	case protoreflect.BoolKind:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return protoreflect.Value{}, errNotFastInsert
		}
		return protoreflect.ValueOfBool(b), nil
	case protoreflect.StringKind:
		if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
			inner := val[1 : len(val)-1]
			inner = strings.ReplaceAll(inner, "''", "'")
			return protoreflect.ValueOfString(inner), nil
		}
		return protoreflect.Value{}, errNotFastInsert
	default:
		return protoreflect.Value{}, errNotFastInsert
	}
}
