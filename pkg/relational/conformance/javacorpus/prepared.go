package javacorpus

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"github.com/google/uuid"

	"fdb.dev/pkg/recordlayer/vectorcodec"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// preparedMix is TestBlock.getRunAsPreparedMix. Draw the cache-pass choice even
// when its metric assertion is unavailable, preserving Java's random stream.
func preparedMix(kind string, repetitions int64, random *javaRandom) []bool {
	mix, _ := preparedMixAndCacheCheck(kind, repetitions, random)
	return mix
}

// preparedMixAndCacheCheck is getRunAsPreparedMix whole: the repetitions' mix
// and whether the check_cache run is prepared (its Pair's right side).
func preparedMixAndCacheCheck(kind string, repetitions int64, random *javaRandom) ([]bool, bool) {
	out := make([]bool, repetitions)
	if kind == "simple" {
		return out, false
	}
	if kind == "prepared" {
		for i := range out {
			out[i] = true
		}
		return out, true
	}
	if repetitions == 1 {
		out[0] = random.next(1) != 0
		return out, out[0]
	}
	for {
		hasSimple, hasPrepared := false, false
		for i := range out {
			out[i] = random.next(1) != 0
			hasSimple = hasSimple || !out[i]
			hasPrepared = hasPrepared || out[i]
		}
		if hasSimple && hasPrepared {
			return out, random.next(1) != 0
		}
	}
}

func (r *runner) adaptPreparedQuery(cmd *javayamsql.Command, at string) (string, []any, bool) {
	query := cmd.Query
	args := make([]any, 0, len(cmd.Segments))
	for _, segment := range cmd.Segments {
		if segment.Kind == javayamsql.SegmentUnbound {
			r.skip(SkipRandomInjection, at, segment.Body)
			return "", nil, false
		}
		value, err := preparedValue(segment.Value)
		if err != nil {
			r.skip(SkipRandomInjection, at, err.Error())
			return "", nil, false
		}
		query = strings.ReplaceAll(query, segment.Raw, "?")
		args = append(args, value)
	}
	return query, args, true
}

func preparedValue(v *javayamsql.Value) (any, error) {
	switch v.TagName() {
	case javayamsql.TagUUID:
		text, ok := v.AsString()
		parsed, valid := values.ParseJavaUUID(text)
		if !ok || !valid {
			return nil, fmt.Errorf("invalid UUID parameter %q", text)
		}
		return uuid.UUID(parsed), nil
	case javayamsql.TagNullArg:
		return nil, nil
	case javayamsql.TagBytes:
		return decodeBytesTag(v)
	case javayamsql.TagRandomStr:
		text, _ := v.AsString()
		return expandRandomStr(text)
	case javayamsql.TagVector16, javayamsql.TagVector32, javayamsql.TagVector64:
		vec := make([]float64, len(v.Untag().Seq))
		for i, x := range v.Untag().Seq {
			switch x.Untag().Kind {
			case javayamsql.KindFloat:
				vec[i] = x.Untag().Float
			case javayamsql.KindInt:
				vec[i] = float64(x.Untag().Int)
			default:
				return nil, fmt.Errorf("invalid vector component %s", x.Kind)
			}
		}
		typ := byte(vectorcodec.TypeDouble)
		if v.TagName() == javayamsql.TagVector16 {
			typ = vectorcodec.TypeHalf
		} else if v.TagName() == javayamsql.TagVector32 {
			typ = vectorcodec.TypeSingle
		}
		return api.Vector(vectorcodec.SerializeAs(typ, vec)), nil
	case javayamsql.TagInList:
		inner := v.Untag()
		if len(inner.Map) != 1 {
			return nil, fmt.Errorf("!in expects a set of exactly 1 element")
		}
		return preparedValue(inner.Map[0].Key)
	}
	u := v.Untag()
	switch u.Kind {
	case javayamsql.KindNull:
		return nil, nil
	case javayamsql.KindBool:
		return u.Bool, nil
	case javayamsql.KindString:
		return u.Str, nil
	case javayamsql.KindInt:
		if v.TagName() != javayamsql.TagLong && u.Int >= math.MinInt32 && u.Int <= math.MaxInt32 {
			return int32(u.Int), nil
		}
		return u.Int, nil
	case javayamsql.KindFloat:
		if v.TagName() == javayamsql.TagFloat {
			return float32(u.Float), nil
		}
		return u.Float, nil
	case javayamsql.KindSeq:
		out := make([]any, len(u.Seq))
		for i, x := range u.Seq {
			var err error
			out[i], err = preparedValue(x)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported prepared parameter of kind %s", u.Kind)
}

type statementPreparer interface {
	PrepareContext(context.Context, string) (*sql.Stmt, error)
}

type preparedExecer struct {
	base statementPreparer
	args []any
}

func (p preparedExecer) ExecContext(ctx context.Context, query string, _ ...any) (sql.Result, error) {
	stmt, err := p.base.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	return stmt.ExecContext(ctx, p.args...)
}

func (p preparedExecer) QueryContext(ctx context.Context, query string, _ ...any) (*sql.Rows, error) {
	stmt, err := p.base.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	// database/sql retains the statement's connection until the returned rows close.
	return stmt.QueryContext(ctx, p.args...)
}
