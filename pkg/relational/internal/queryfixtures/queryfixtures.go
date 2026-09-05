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
// scope is therefore walk order, not the whole plan: on this query THREE of the
// FOUR record constructors end up unstamped, and the fourth — resolved before
// the bad message was appended — keeps its descriptor. That survivor is not a
// detail to round off; the census asserts it, and a run where nothing survived
// would mean something other than this defect. A computed STRUCT read through
// one of the unstamped constructors comes back as a raw map, while a STORED
// struct column through the same plan is unaffected, carrying its own stored
// descriptor rather than a constructor's.
//
// Two packages pin that: one asserts the descriptor census over the baked plan,
// the other reads the user-visible value out of a real store. The census is
// the value read's precondition, which is why the text is shared rather than
// copied.
//
// The `a.id + 1 = c.id` predicate is load-bearing: it keeps the two `ID` slots
// holding DIFFERENT values, so a read that took one slot twice would be caught.
// Under an equality predicate both slots are equal and the check cannot
// discriminate.
const DuplicateNameJoinQuery = "WITH d AS (SELECT id AS bid, EXISTS (SELECT 1 FROM b_md AS x WHERE x.id = b_md.id) AS foo FROM b_md) " +
	"SELECT a.id, c.id, d.foo FROM a_md AS a JOIN d ON a.id = d.bid FULL OUTER JOIN c_md AS c ON a.id + 1 = c.id"
