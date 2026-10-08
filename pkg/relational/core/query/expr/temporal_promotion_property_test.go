package expr_test

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/expr"
)

// The temporal extension's property (RFC-257 WS-E, ws-e-design.md 4.3): every
// DATE and TIMESTAMP value, from every producer, holds canonical UTC text in
// the years 0000-9999, and wherever a DATE meets a TIMESTAMP the DATE is
// promoted to its midnight. Then every comparison answers the INSTANT order
// (a DATE being its UTC day's midnight), NULL answers UNKNOWN, NOT negates,
// commuting the operands with the operator answers the same, and GREATEST and
// LEAST pick the later and earlier instant.

type temporalClock time.Time

func (c temporalClock) StatementNow() time.Time { return time.Time(c) }

// temporalOperand is one produced value: the Value, the instant it denotes
// (nil for NULL), its producer and, for a clock producer, the statement
// clock it must be evaluated under.
type temporalOperand struct {
	value    values.Value
	instant  *time.Time
	producer string
	clock    *time.Time
}

var (
	temporalFloor   = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
	temporalCeiling = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
)

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// offsetText renders t in a zone whose local year stays in 0000-9999, as an
// RFC 3339 text a caller might write.
func offsetText(t time.Time, rng *rand.Rand) string {
	offset := (rng.IntN(29) - 14) * 3600
	local := t.In(time.FixedZone("", offset))
	if local.Year() < 0 || local.Year() > 9999 {
		local = t.UTC()
	}
	return local.Format(time.RFC3339)
}

func castOperand(t *testing.T, r *expr.Resolver, text string, target values.Type) values.Value {
	t.Helper()
	v, err := r.ResolveCast(&values.ConstantValue{Value: text, Typ: values.NotNullString}, target)
	if err != nil {
		t.Fatalf("CAST(%q AS %s): %v", text, target, err)
	}
	return v
}

func dateOperands(t *testing.T, r *expr.Resolver, at time.Time, rng *rand.Rand) []temporalOperand {
	day := utcDay(at)
	clock := at
	return []temporalOperand{
		{castOperand(t, r, day.Format("2006-01-02"), values.NullableDate), &day, "CAST(day AS DATE)", nil},
		{castOperand(t, r, offsetText(at, rng), values.NullableDate), &day, "CAST(rfc3339 AS DATE)", nil},
		{values.NewScalarFunctionValue("CURRENT_DATE", values.NullableDate), &day, "CURRENT_DATE", &clock},
	}
}

func timestampOperands(t *testing.T, r *expr.Resolver, at time.Time, rng *rand.Rand) []temporalOperand {
	at = at.Truncate(time.Second)
	clock := at
	return []temporalOperand{
		{castOperand(t, r, at.UTC().Format("2006-01-02 15:04:05"), values.NullableTimestamp), &at, "CAST(text AS TIMESTAMP)", nil},
		{castOperand(t, r, offsetText(at, rng), values.NullableTimestamp), &at, "CAST(rfc3339 AS TIMESTAMP)", nil},
		// What the driver binds for a time.Time (embedded.timeParameter).
		{&values.ConstantValue{Value: values.CanonicalTimestampText(at), Typ: values.NotNullTimestamp}, &at, "bound time.Time", nil},
		{values.NewScalarFunctionValue("CURRENT_TIMESTAMP", values.NullableTimestamp), &at, "CURRENT_TIMESTAMP", &clock},
	}
}

func nullOperands(t *testing.T, r *expr.Resolver) []temporalOperand {
	return []temporalOperand{
		{values.NewNullValue(values.NullableDate), nil, "NULL DATE", nil},
		{values.NewNullValue(values.NullableTimestamp), nil, "NULL TIMESTAMP", nil},
	}
}

// temporalInstants are the domain edges, a day and its midnight, a second
// either side of midnight, and seeded random instants across the domain.
func temporalInstants(rng *rand.Rand) [][2]time.Time {
	midnight := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	pairs := [][2]time.Time{
		{temporalFloor, temporalFloor},
		{temporalCeiling, temporalCeiling},
		{temporalFloor, temporalCeiling},
		{midnight, midnight},
		{midnight, midnight.Add(time.Second)},
		{midnight, midnight.Add(-time.Second)},
		{midnight, midnight.Add(10 * time.Hour)},
		{midnight.Add(23*time.Hour + 59*time.Minute + 59*time.Second), midnight.Add(24 * time.Hour)},
	}
	span := temporalCeiling.Unix() - temporalFloor.Unix()
	for range 40 {
		a := time.Unix(temporalFloor.Unix()+rng.Int64N(span), 0).UTC()
		b := time.Unix(temporalFloor.Unix()+rng.Int64N(span), 0).UTC()
		pairs = append(pairs, [2]time.Time{a, b}, [2]time.Time{a, utcDay(a)}, [2]time.Time{a, a.Add(time.Duration(rng.IntN(7200)-3600) * time.Second)})
	}
	for i := range pairs {
		for j := range pairs[i] {
			if pairs[i][j].Before(temporalFloor) {
				pairs[i][j] = temporalFloor
			}
			if pairs[i][j].After(temporalCeiling) {
				pairs[i][j] = temporalCeiling
			}
		}
	}
	return pairs
}

var temporalComparisons = []struct {
	op, commuted predicates.ComparisonType
	holds        func(c int) bool
}{
	{predicates.ComparisonEquals, predicates.ComparisonEquals, func(c int) bool { return c == 0 }},
	{predicates.ComparisonNotEquals, predicates.ComparisonNotEquals, func(c int) bool { return c != 0 }},
	{predicates.ComparisonLessThan, predicates.ComparisonGreaterThan, func(c int) bool { return c < 0 }},
	{predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq, func(c int) bool { return c <= 0 }},
	{predicates.ComparisonGreaterThan, predicates.ComparisonLessThan, func(c int) bool { return c > 0 }},
	{predicates.ComparisonGreaterThanEq, predicates.ComparisonLessThanOrEq, func(c int) bool { return c >= 0 }},
}

func triBoolText(b predicates.TriBool) string {
	if b == nil {
		return "UNKNOWN"
	}
	return fmt.Sprint(*b)
}

func TestTemporalComparisonsAnswerTheInstantOrder(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	rng := rand.New(rand.NewPCG(257, 43))
	evaluated := 0
	for _, pair := range temporalInstants(rng) {
		var lefts, rights []temporalOperand
		lefts = append(lefts, dateOperands(t, r, pair[0], rng)...)
		lefts = append(lefts, timestampOperands(t, r, pair[0], rng)...)
		lefts = append(lefts, nullOperands(t, r)...)
		// The statement clock is one per statement: clock producers stand on
		// the left only, and commuting moves them right.
		for _, o := range dateOperands(t, r, pair[1], rng) {
			if o.clock == nil {
				rights = append(rights, o)
			}
		}
		for _, o := range timestampOperands(t, r, pair[1], rng) {
			if o.clock == nil {
				rights = append(rights, o)
			}
		}
		rights = append(rights, nullOperands(t, r)...)
		for _, left := range lefts {
			var ctx any
			if left.clock != nil {
				ctx = temporalClock(*left.clock)
			}
			for _, right := range rights {
				for _, cmp := range temporalComparisons {
					want := "UNKNOWN"
					if left.instant != nil && right.instant != nil {
						want = fmt.Sprint(cmp.holds(left.instant.Compare(*right.instant)))
					}
					name := fmt.Sprintf("%s(%v) %v %s(%v)", left.producer, left.instant, cmp.op, right.producer, right.instant)
					eval := func(p predicates.QueryPredicate, err error) string {
						if err != nil {
							t.Fatalf("%s: resolve: %v", name, err)
						}
						got, err := p.Eval(ctx)
						if err != nil {
							t.Fatalf("%s: eval: %v", name, err)
						}
						return triBoolText(got)
					}
					pred, err := r.ResolveComparison(cmp.op, left.value, right.value)
					if got := eval(pred, err); got != want {
						t.Fatalf("%s = %s, want %s", name, got, want)
					}
					notWant := map[string]string{"true": "false", "false": "true", "UNKNOWN": "UNKNOWN"}[want]
					if got := eval(r.ResolveNot(pred), nil); got != notWant {
						t.Fatalf("NOT %s = %s, want %s", name, got, notWant)
					}
					if got := eval(r.ResolveComparison(cmp.commuted, right.value, left.value)); got != want {
						t.Fatalf("commuted %s = %s, want %s", name, got, want)
					}
					evaluated++
				}
			}
		}
	}
	if evaluated < 10000 {
		t.Fatalf("only %d comparisons evaluated", evaluated)
	}
}

// GREATEST and LEAST over a DATE and a TIMESTAMP are TIMESTAMP-typed and pick
// by instant; over two DATEs they stay DATE.
func TestTemporalGreatestLeastPickByInstant(t *testing.T) {
	t.Parallel()
	a, s := buildScope(t)
	r := expr.New(a, s)
	rng := rand.New(rand.NewPCG(257, 44))
	for _, pair := range temporalInstants(rng) {
		day, at := utcDay(pair[0]), pair[1].Truncate(time.Second)
		dayText, atText := day.Format("2006-01-02"), at.Format("2006-01-02 15:04:05")
		for _, c := range []struct {
			args, wantType  string
			greatest, least string
		}{
			{
				fmt.Sprintf("CAST('%s' AS DATE), CAST('%s' AS TIMESTAMP)", dayText, atText), "TIMESTAMP",
				latest(day, at).Format("2006-01-02 15:04:05"), earliest(day, at).Format("2006-01-02 15:04:05"),
			},
			{
				fmt.Sprintf("CAST('%s' AS TIMESTAMP), CAST('%s' AS DATE)", atText, dayText), "TIMESTAMP",
				latest(day, at).Format("2006-01-02 15:04:05"), earliest(day, at).Format("2006-01-02 15:04:05"),
			},
			{
				fmt.Sprintf("CAST('%s' AS DATE), CAST('%s' AS DATE)", dayText, utcDay(at).Format("2006-01-02")), "DATE",
				latest(day, utcDay(at)).Format("2006-01-02"), earliest(day, utcDay(at)).Format("2006-01-02"),
			},
		} {
			for fn, want := range map[string]string{"GREATEST": c.greatest, "LEAST": c.least} {
				q := fmt.Sprintf("SELECT * FROM users WHERE %s(%s)", fn, c.args)
				v, err := r.WalkExpression(parseFirstWhereExpr(t, q))
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				if v.Type().Code().String() != c.wantType {
					t.Fatalf("%s: type %s, want %s", q, v.Type(), c.wantType)
				}
				got, err := v.Evaluate(nil)
				if err != nil || got != want {
					t.Fatalf("%s = %v, %v; want %s", q, got, err, want)
				}
			}
		}
	}
	// A NULL argument answers NULL.
	v, err := r.WalkExpression(parseFirstWhereExpr(t, "SELECT * FROM users WHERE GREATEST(CAST(NULL AS DATE), CAST('2024-01-01 00:00:00' AS TIMESTAMP))"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v.Evaluate(nil); err != nil || got != nil {
		t.Fatalf("GREATEST with a NULL argument = %v, %v; want NULL", got, err)
	}
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
