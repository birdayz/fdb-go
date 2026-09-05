// Package queryfixtures holds SQL texts that more than one test package must
// run VERBATIM.
//
// A text lives here only when two packages assert about the SAME statement and
// one of them is the other's precondition — a census pinned over a plan is a
// statement about the query that produced it, so it is worth nothing if the
// query drifts. Keeping the two copies in step used to be a comment asking the
// next editor to remember; a shared constant makes it the compiler's job, which
// is the only version that survives an edit made in one package by someone who
// has never opened the other.
package queryfixtures

// DuplicateNameJoinQuery is a FULL OUTER JOIN whose ordinal row names `ID`
// twice, once from each id leg.
//
// Such a row's synthesised descriptor cannot validate, and the repository keeps
// the bad message, so every type asked for AFTER it fails the same way. The
// scope is therefore walk order, not the whole plan: as measured on this query
// THREE of the FOUR record constructors end up unstamped, and the fourth —
// resolved before the bad message was appended — keeps its descriptor. That
// survivor is not a detail to round off; a run where nothing survived would
// mean something other than this defect, and the census fatals on it.
//
// What this text does NOT carry is the struct claim. It selects `a.id, c.id,
// d.foo` and has no struct column, computed or stored, so nothing about raw
// maps versus api.Struct can be read off it. That cost is measured by three
// OTHER texts in the same FDB test, which are not shared and do not belong
// here. Attaching the struct claim to this constant once made it describe a
// plan that cannot exhibit it.
//
// Two packages pin what this text does carry: one asserts the descriptor census
// over the baked plan, the other reads the whole outer-join result back out of
// a real store and checks every row arrives with both `ID` slots. The census is
// that row-arrival read's precondition — and only that read's; the census pin
// says in as many words that it asserts nothing about the struct texts. Sharing
// the constant is what keeps the two halves measuring one plan.
//
// The `a.id + 1 = c.id` predicate is load-bearing: it keeps the two `ID` slots
// holding DIFFERENT values, so a read that took one slot twice would be caught.
// Under an equality predicate both slots are equal and the check cannot
// discriminate.
const DuplicateNameJoinQuery = "WITH d AS (SELECT id AS bid, EXISTS (SELECT 1 FROM b_md AS x WHERE x.id = b_md.id) AS foo FROM b_md) " +
	"SELECT a.id, c.id, d.foo FROM a_md AS a JOIN d ON a.id = d.bid FULL OUTER JOIN c_md AS c ON a.id + 1 = c.id"
