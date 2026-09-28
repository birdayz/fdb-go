//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/relational/api"
)

// RFC-142 R5 / RFC-257: an explicit JOIN whose right side is spelled as a
// correlated array (`FROM w JOIN w.arr AS v ON …`). Java's visitInnerJoin
// visits the right side through visitAtomTableItem, the comma source's own
// path, so generateAccess reads the path as a correlated field, and the ON is
// accumulated as an inner-join predicate over one flat select: `w JOIN w.arr
// AS v ON c` is `FROM w, w.arr AS v WHERE c`. This spec measures the family.
var _ = Describe("RFC-142 R5: an explicit JOIN's correlated array source", func() {
	It("reads an explicit JOIN's correlated array as the target does", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		p := "WSFJOIN" + strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))
		const body = `create table w(id bigint, f bigint, arr bigint array, primary key(id)) ` +
			`create type as struct s(k bigint, g string) ` +
			`create table r(id bigint, items s array, primary key(id)) ` +
			`create table h(id bigint, f bigint, primary key(id)) ` +
			`create type as struct b(k bigint, tags bigint array) ` +
			`create table q(id bigint, bs b array, primary key(id)) ` +
			`create table kk(id bigint, k bigint, items s array, primary key(id)) ` +
			`create type as struct c(k bigint, bs b array) ` +
			`create table qq(id bigint, cs c array, primary key(id)) ` +
			`create table w2(id bigint, a bigint array, b bigint array, primary key(id)) ` +
			`create table g(k bigint, v bigint, primary key(k))`
		setup := []string{
			`INSERT INTO w VALUES (1, 1, [10, 11])`,
			`INSERT INTO w VALUES (2, 2, [20])`,
			`INSERT INTO w VALUES (3, 3, [])`,
			`INSERT INTO r VALUES (1, [(5, 'a'), (6, 'b')])`,
			`INSERT INTO h VALUES (1, 10)`,
			`INSERT INTO q VALUES (1, [(1, [7, 8]), (2, [9])])`,
			`INSERT INTO kk VALUES (1, 5, [(5, 'a'), (6, 'b')])`,
			`INSERT INTO kk VALUES (2, 6, [(5, 'c')])`,
			`INSERT INTO qq VALUES (1, [(1, [(1, [7, 8]), (2, [9])]), (2, [])])`,
			`INSERT INTO w2 VALUES (1, [1, 2], [3])`,
			`INSERT INTO w2 (id, b) VALUES (2, [4])`,
			`INSERT INTO g VALUES (1, 100)`,
			`INSERT INTO g VALUES (3, 300)`,
		}
		queries := []string{
			`SELECT v FROM w, w.arr AS v`,
			`SELECT v FROM w INNER JOIN w.arr AS v ON 1 = 1`,
			`SELECT v FROM w JOIN w.arr AS v ON 1 = 1`,
			`SELECT v FROM w JOIN w.arr AS v ON v > 10`,
			`SELECT w.id, v FROM w JOIN w.arr AS v ON v = w.id + 9`,
			`SELECT a.id, v FROM w AS a JOIN a.arr AS v ON v > 10`,
			`SELECT v, p FROM w JOIN w.arr AS v AT p ON p = 1`,
			`SELECT x.k FROM r JOIN r.items AS x ON x.k = 6`,
			`SELECT v, h.f FROM w JOIN w.arr AS v ON 1 = 1 JOIN h ON h.id = w.id`,
			`SELECT v FROM w JOIN w.arr AS v ON 1 = 1 WHERE v < 20`,
			`SELECT v FROM w JOIN w.arr AS v USING (id)`,
			`SELECT v FROM w JOIN nosuch.arr AS v ON 1 = 1`,
			`SELECT v FROM h JOIN w.arr AS v ON 1 = 1`,
			`SELECT COUNT(*) FROM w JOIN w.arr AS v ON 1 = 1`,
			`SELECT h.id FROM w JOIN h AT p ON 1 = 1`,
			`SELECT t FROM q, q.bs, bs.tags AS t`,
			`SELECT t FROM q JOIN q.bs ON 1 = 1, bs.tags AS t`,
			`SELECT t FROM q JOIN q.bs ON bs.k = 1, bs.tags AS t`,
			`SELECT t FROM q JOIN q.bs AS b ON b.k = 2 JOIN b.tags AS t ON 1 = 1`,
			`SELECT t FROM q JOIN q.bs ON bs.k = 1 JOIN bs.tags AS t ON t > 7`,
			// A source in a subquery unnesting an outer query's array: the
			// path is read with the column lookup over the operators in scope,
			// the outer query's included.
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h, w.arr AS v WHERE v = h.f)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h JOIN w.arr AS v ON v = h.f)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h, w.arr AS v WHERE v = 20)`,
			`SELECT id FROM w WHERE NOT EXISTS (SELECT 1 FROM h, w.arr AS v WHERE v = h.f)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w AS i, i.arr AS v WHERE v = 20)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w, w.arr AS v WHERE v = 20)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w, w.arr AS v, h AS v)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w, w.arr AS v)`,
			`SELECT id FROM w AS o WHERE EXISTS (SELECT 1 FROM h, o.arr AS v WHERE v = h.f)`,
			`SELECT r.id FROM r WHERE EXISTS (SELECT 1 FROM h, r.items AS x WHERE x.k = 6)`,
			// A path naming no source, or a hidden one.
			`SELECT v FROM w, nosuch.arr AS v`,
			`SELECT v FROM w AS a, w.arr AS v`,
			`SELECT v FROM w, w.f AS v`,
			// USING over an unnest: the right copy is the element's member,
			// hidden; a scalar element has no such member.
			`SELECT k FROM kk JOIN kk.items AS i USING (k)`,
			`SELECT kk.id, i.g FROM kk JOIN kk.items AS i USING (k)`,
			`SELECT i.k FROM kk JOIN kk.items AS i USING (k)`,
			`SELECT h.id FROM w JOIN h AT p USING (id)`,
			`SELECT v FROM w JOIN w.arr AS v AT p USING (id)`,
			`SELECT id FROM w JOIN w.arr AS id USING (id)`,
			`SELECT k FROM kk JOIN kk.items AS i AT p USING (k)`,
			// An ON EXISTS over an unnest leg (lifted whole into the WHERE), and
			// an unnest after an outer join.
			`SELECT v FROM w JOIN w.arr AS v ON EXISTS (SELECT 1 FROM h WHERE h.f = v)`,
			`SELECT v FROM w LEFT JOIN h ON h.id = w.id, w.arr AS v`,
			// A derived table sees the FROM's sources to its left (a lateral
			// derived table), and only those.
			`SELECT d.k FROM w, (SELECT v AS k FROM w.arr AS v) AS d`,
			`SELECT d.x FROM w, (SELECT h.f AS x FROM h WHERE h.id = w.id) AS d`,
			`SELECT d.k FROM w, (SELECT v AS k FROM w.arr AS v WHERE v > 10) AS d`,
			`SELECT d.c FROM w, (SELECT COUNT(*) AS c FROM w.arr AS v) AS d`,
			`SELECT w.id, d.k FROM w JOIN (SELECT v AS k FROM w.arr AS v) AS d ON d.k > 10`,
			`SELECT d.k FROM w AS a, (SELECT v AS k FROM a.arr AS v) AS d`,
			`SELECT d.x FROM w, (SELECT w.f AS x FROM h) AS d`,
			`SELECT d.k FROM (SELECT v AS k FROM w.arr AS v) AS d, w`,
			// A block's first FROM item that names an enclosing query's array
			// is that array's unnest, whatever the block projects, joins or
			// asks of its ordinal.
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v WHERE v = 20)`,
			`SELECT id FROM w WHERE EXISTS (SELECT * FROM w.arr AS v WHERE v = 20)`,
			`SELECT id FROM r WHERE EXISTS (SELECT x.k FROM r.items AS x WHERE x.k = 6)`,
			`SELECT id FROM w WHERE EXISTS (SELECT v FROM w.arr AS v, h WHERE v = h.f)`,
			`SELECT id FROM w WHERE EXISTS (SELECT v FROM w.arr AS v JOIN h ON v = h.f)`,
			`SELECT id FROM w WHERE EXISTS (SELECT v, p FROM w.arr AS v AT p, h WHERE p = h.id)`,
			`SELECT d.v, d.f FROM w, (SELECT v, h.f FROM w.arr AS v, h) AS d`,
			`SELECT d.* FROM w, (SELECT * FROM w.arr AS v, h) AS d`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v AT p WHERE p = 2)`,
			`SELECT id FROM w WHERE EXISTS (SELECT p FROM w.arr AT p)`,
			// A USING beside an unnest leg resolves each column on every left
			// operator first, then on the right (resolveJoinUsingClause): an
			// unnest leg is described by its own operator on either side.
			`SELECT kk.id, i.g FROM kk JOIN h ON h.id = kk.id JOIN kk.items AS i USING (k)`,
			`SELECT a.id, i.g FROM kk AS a JOIN kk AS b ON a.id = b.id JOIN a.items AS i USING (k)`,
			`SELECT v, h.f FROM w JOIN w.arr AS v ON 1 = 1 JOIN h USING (id)`,
			`SELECT kk.id, i.g FROM kk JOIN kk.items AS i USING (k) JOIN h ON h.id = kk.id`,
			`SELECT a.id FROM kk AS a JOIN kk.items AS i USING (k) JOIN kk AS b USING (k)`,
			`SELECT id FROM kk WHERE EXISTS (SELECT 1 FROM kk.items AS i JOIN h USING (k))`,
			`SELECT id FROM kk WHERE EXISTS (SELECT 1 FROM kk.items AS i JOIN kk AS z USING (k))`,
			// An aggregate over it reads the element as the input itself.
			`SELECT d.s FROM w, (SELECT SUM(v) AS s FROM w.arr AS v) AS d`,
			// A chained unnest over a block's first FROM item: the item is the
			// block's first quantifier and the next item explodes its element.
			`SELECT id FROM q WHERE EXISTS (SELECT t FROM q.bs AS b, b.tags AS t WHERE t = 9)`,
			`SELECT id FROM q WHERE NOT EXISTS (SELECT t FROM q.bs AS b, b.tags AS t WHERE t = 9)`,
			`SELECT id FROM q WHERE EXISTS (SELECT 1 FROM q.bs AS b, b.tags AS t WHERE t = q.id + 8)`,
			`SELECT d.t FROM q, (SELECT t FROM q.bs AS b, b.tags AS t) AS d`,
			`SELECT d.t, d.k FROM q, (SELECT t, b.k AS k FROM q.bs AS b, b.tags AS t WHERE b.k = 1) AS d`,
			`SELECT d.k FROM q, (SELECT b.k AS k FROM q.bs AS b, b.tags AS t WHERE t = 9) AS d`,
			`SELECT d.t, d.o, d.p FROM q, (SELECT t, o, p FROM q.bs AS b AT o, b.tags AS t AT p) AS d`,
			`SELECT id FROM q WHERE EXISTS (SELECT t FROM q.bs AS b AT o, b.tags AS t AT p WHERE p = o)`,
			`SELECT d.t, d.u FROM q, (SELECT t, u FROM q.bs AS b, b.tags AS t, b.tags AS u) AS d`,
			`SELECT d.c FROM q, (SELECT COUNT(*) AS c FROM q.bs AS b, b.tags AS t) AS d`,
			`SELECT d.t FROM q, (SELECT t FROM q.bs AS b JOIN b.tags AS t ON t > 7) AS d`,
			`SELECT id FROM q WHERE EXISTS (SELECT 1 FROM q.bs AS b, b.tags AS t, h WHERE t + 1 = h.f)`,
			`SELECT id FROM q WHERE EXISTS (SELECT 1 FROM h, q.bs AS b, b.tags AS t WHERE t + 1 = h.f)`,
			`SELECT d.t FROM qq, (SELECT t FROM qq.cs AS c, c.bs AS b, b.tags AS t) AS d`,
			`SELECT id FROM qq WHERE EXISTS (SELECT 1 FROM qq.cs AS c, c.bs AS b, b.tags AS t WHERE t = c.k + 8)`,
			`SELECT d.t FROM qq, (SELECT t FROM qq.cs AS c, c.bs AS b, c.bs AS b2, b2.tags AS t) AS d`,
			`SELECT id FROM qq WHERE EXISTS (SELECT 1 FROM qq.cs AS c, c.bs AS b, c.bs AS b2, b2.tags AS t WHERE t = b.k + 7)`,
			`SELECT v, v2 FROM w, w.arr AS v, w.arr AS v2`,
			`SELECT v, v2 FROM w, h, w.arr AS v, w.arr AS v2`,
			`SELECT v, v2, h.f FROM h, w, w.arr AS v, w.arr AS v2`,
			`SELECT id, x, y FROM w2, w2.a AS x, w2.b AS y`,
			// A WHERE reading the element of the link under a spine's tip, whose
			// binding also names the spine's merged row.
			`SELECT t FROM q, q.bs AS b, b.tags AS t WHERE t > b.k + 6`,
			`SELECT t, o FROM q, q.bs AS b AT o, b.tags AS t WHERE t > b.k + 6`,
			`SELECT t, o FROM q, q.bs AS b AT o, b.tags AS t WHERE t > o + 6`,
			`SELECT d.t FROM q, (SELECT t FROM q.bs AS b, b.tags AS t WHERE t > b.k + 6) AS d`,
			// A WHERE EXISTS over a spine reading one of its elements.
			`SELECT id FROM q WHERE EXISTS (SELECT t FROM q.bs AS b, b.tags AS t WHERE EXISTS (SELECT 1 FROM h WHERE h.f = t + 1))`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v, w.arr AS v2 WHERE EXISTS (SELECT 1 FROM h WHERE h.f = v2))`,
			// A lateral derived leg reading an AT ordinal or a mid-link element:
			// the pair is declared with its AS/AT names and the Explode flows
			// _0/_1, bound to it by position.
			`SELECT d.id FROM w, w.arr AS v AT p, (SELECT h.id FROM h WHERE h.f = p + 9) AS d`,
			`SELECT d.x FROM w, w.arr AS v AT p, (SELECT p + h.f AS x FROM h) AS d`,
			`SELECT d.x FROM w, w.arr AS v AT p, (SELECT h.f AS x FROM h WHERE h.f > p) AS d`,
			`SELECT d.c FROM w, w.arr AS v AT p, (SELECT COUNT(*) + p AS c FROM h) AS d`,
			// An enclosing block's value in a post-aggregate expression is a
			// constant across the aggregated rows (isComposableFrom's
			// constantCorrelations arm); an inner source shadowing the outer
			// name is not one (a non-grouped source reference).
			`SELECT d.c FROM w, (SELECT COUNT(*) + w.f AS c FROM h) AS d`,
			`SELECT d.c FROM w, w.arr AS v, (SELECT MAX(h.f) - v AS c FROM h) AS d`,
			`SELECT d.c FROM w, (SELECT COUNT(*) + w.f AS c FROM w AS w) AS d`,
			`SELECT COUNT(*) + h.f FROM h`,
			// Every arithmetic operator shares one left-associative precedence
			// in both engines' grammar.
			`SELECT h.f - h.id * 2 FROM h`,
			`SELECT w.id FROM w, h WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > h.f)`,
			`SELECT w.id FROM w, h WHERE NOT EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > h.f)`,
			`SELECT w.id, h.id FROM w, h WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > h.f)`,
			`SELECT w.id, h.id FROM h, w WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > h.f)`,
			`SELECT w.id, h.id, g.k FROM w, h, g WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > h.f AND z.v > g.v)`,
			`SELECT w.id, h.id FROM w, h WHERE w.id = h.id AND EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > h.f)`,
			`SELECT w.id, COUNT(*) FROM w, h WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > h.f) GROUP BY w.id`,
			`SELECT w.id, h.id FROM w, h WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = w.id AND z.v > h.f) AND w.f < 3`,
			// A WHERE EXISTS over a FROM holding a derived table beside another
			// source — a star over a join, a projection over a join, or a star
			// over one table — reading the derived leg alone or both legs.
			`SELECT a.id, a.k FROM (SELECT * FROM w, g) AS a, h AS d WHERE EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.k AND z.f = d.f)`,
			`SELECT a.id FROM (SELECT * FROM w) AS a, h AS d WHERE EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.id AND z.f = d.f)`,
			`SELECT a.id, a.k FROM (SELECT w.id, g.k FROM w, g) AS a, h AS d WHERE EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.k)`,
			`SELECT a.id, a.k FROM (SELECT w.id, g.k FROM w, g) AS a, h AS d WHERE NOT EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.k)`,
			`SELECT a.id, a.k, d.id FROM (SELECT w.id, g.k FROM w, g) AS a, h AS d WHERE EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.k AND z.f = d.f)`,
			`SELECT a.id, d.id FROM h AS d, (SELECT * FROM w) AS a WHERE EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.id AND z.f = d.f)`,
			`SELECT a.id FROM (SELECT * FROM w) AS a, h AS d WHERE NOT EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.id AND z.f = d.f)`,
			// A LEFT JOIN inside a lateral derived table, both legs reading the
			// enclosing row: w1's preserved row meets a non-empty inner (g3)
			// that its ON rejects, so it is null-extended.
			`SELECT w.id, d.aid, d.bk FROM w, (SELECT a.id AS aid, b.k AS bk FROM (SELECT h.id FROM h WHERE h.f = w.f * 10) AS a LEFT JOIN (SELECT g.k FROM g WHERE g.k = w.id + 2) AS b ON a.id = b.k) AS d`,
			`SELECT w.id, d.aid, d.bk FROM w, (SELECT a.id AS aid, b.k AS bk FROM (SELECT h.id FROM h WHERE h.f = w.f * 10) AS a LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON a.id = b.k) AS d`,
			`SELECT w.id FROM w WHERE EXISTS (SELECT 1 FROM (SELECT h.id FROM h WHERE h.f = w.f * 10) AS a LEFT JOIN (SELECT g.k FROM g WHERE g.k = w.id + 2) AS b ON a.id = b.k WHERE b.k IS NULL)`,
			// Its ON not reading the preserved leg (the rewrite leaves it a LEFT
			// OUTER select), a constant ON, and an ON reading only the preserved leg.
			`SELECT w.id, d.aid, d.bk FROM w, (SELECT a.id AS aid, b.k AS bk FROM (SELECT h.id FROM h WHERE h.f = w.f * 10) AS a LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON b.k > 1) AS d`,
			`SELECT w.id, d.aid, d.bk FROM w, (SELECT a.id AS aid, b.k AS bk FROM (SELECT h.id FROM h WHERE h.f = w.f * 10) AS a LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON 1 = 1) AS d`,
			`SELECT w.id, d.aid, d.bk FROM w, (SELECT a.id AS aid, b.k AS bk FROM (SELECT h.id FROM h WHERE h.f = w.f * 10) AS a LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON a.id > 1) AS d`,
			// The same three over a preserved leg with rows for every w (kk's ids
			// up to w.id), and an ON reading both legs as the control.
			`SELECT w.id, d.aid, d.bk FROM w, (SELECT a.id AS aid, b.k AS bk FROM (SELECT kk.id FROM kk WHERE kk.id <= w.id) AS a LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON b.k > 1) AS d`,
			`SELECT w.id, d.aid, d.bk FROM w, (SELECT a.id AS aid, b.k AS bk FROM (SELECT kk.id FROM kk WHERE kk.id <= w.id) AS a LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON 1 = 1) AS d`,
			`SELECT w.id, d.aid, d.bk FROM w, (SELECT a.id AS aid, b.k AS bk FROM (SELECT kk.id FROM kk WHERE kk.id <= w.id) AS a LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON a.id > 1) AS d`,
			`SELECT w.id, d.aid, d.bk FROM w, (SELECT a.id AS aid, b.k AS bk FROM (SELECT kk.id FROM kk WHERE kk.id <= w.id) AS a LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON b.k = a.id + 2) AS d`,
			// A derived leg over a star join whose tables share column names,
			// read by name (ambiguous in both engines); over an outer-join
			// body (its null-supplying columns read, counted, joined on and
			// tested for NULL); and with a column-alias list (a WITH list: a
			// derived table's is a syntax error in both) — each beside another
			// source.
			`SELECT a.id FROM (SELECT * FROM w, h) AS a, g WHERE g.k = 1`,
			`SELECT a.f, g.v FROM (SELECT * FROM w, h) AS a, g WHERE g.k = 3`,
			`SELECT a.id, a.v, d.f FROM (SELECT * FROM w LEFT JOIN g ON g.k = w.id) AS a, h AS d WHERE d.id = a.k`,
			`SELECT a.id, a.k, d.id FROM (SELECT * FROM w LEFT JOIN g ON g.k = w.id) AS a, h AS d WHERE d.f = 10`,
			`SELECT a.id, a.k FROM (SELECT * FROM g RIGHT JOIN w ON g.k = w.id) AS a, h AS d WHERE d.f = 10`,
			`SELECT a.id, a.k, d.id FROM h AS d, (SELECT * FROM w LEFT JOIN g ON g.k = w.id) AS a WHERE a.k IS NULL`,
			`SELECT a.id, a.v FROM (SELECT * FROM w LEFT JOIN g ON g.k = w.id) AS a, h AS d WHERE a.id >= d.id AND a.v IS NOT NULL`,
			`SELECT COUNT(*), COUNT(a.k) FROM (SELECT * FROM w LEFT JOIN g ON g.k = w.id) AS a, h AS d`,
			`SELECT a.id, b.id FROM (SELECT * FROM w LEFT JOIN g ON g.k = w.id) AS a, (SELECT * FROM w LEFT JOIN g ON g.k = w.id + 1) AS b WHERE a.id = b.id`,
			`SELECT a.id FROM (SELECT * FROM w LEFT JOIN g ON g.k = w.id) AS a, h AS d WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = a.id AND z.v > d.f)`,
			`SELECT a.id, a.k, d.id FROM (SELECT * FROM w LEFT JOIN g ON g.k = w.id) AS a LEFT JOIN h AS d ON d.id = a.k`,
			`WITH a (i, f, arr, k, v) AS (SELECT * FROM w, g) SELECT a.i, a.k, d.f FROM a, h AS d WHERE d.id = a.k`,
			`WITH a (i, j) AS (SELECT w.id, g.k FROM w, g) SELECT a.i, d.f FROM a, h AS d WHERE d.id = a.j`,
			// A WHERE EXISTS whose conjuncts stay inside its body (a join, a
			// GROUP BY, a derived table), and over a column-list CTE and a
			// star-unnest derived table beside another source.
			`SELECT w.id, h.id FROM w, h WHERE EXISTS (SELECT 1 FROM g AS z, g AS y WHERE z.k = w.id AND y.v > h.f AND z.k = y.k)`,
			`SELECT w.id, h.id FROM w, h WHERE EXISTS (SELECT z.k FROM g AS z WHERE z.k = w.id AND z.v > h.f GROUP BY z.k)`,
			`SELECT w.id, h.id FROM w, h WHERE EXISTS (SELECT 1 FROM (SELECT * FROM g AS z WHERE z.k = w.id AND z.v > h.f) AS q)`,
			`WITH a (i, j) AS (SELECT w.id, g.k FROM w, g) SELECT a.i, d.id FROM a, h AS d WHERE EXISTS (SELECT 1 FROM h AS z WHERE z.id = a.j AND z.f = d.f)`,
			`SELECT a.id, a.v FROM (SELECT * FROM w, w.arr AS v) AS a, h AS d WHERE EXISTS (SELECT 1 FROM h AS z WHERE z.f = a.v AND z.id = d.id)`,
			// A star derived table over one filtered table as a leg of a join:
			// merging its body would rename the join's leg to the body's table.
			`SELECT a.id, d.id FROM (SELECT * FROM w WHERE w.f > 1) AS a, h AS d`,
			`SELECT a.id, d.id FROM (SELECT * FROM w WHERE w.f > 1) AS a, h AS d WHERE a.id = d.f - 8`,
			`SELECT a.id, g.k FROM (SELECT * FROM w WHERE w.f > 1) AS a, h AS d, g WHERE a.id = g.k AND d.id = 1`,
			`SELECT a.id, a.f, g.v FROM h AS d, (SELECT * FROM w WHERE w.f > 1) AS a, g WHERE a.id = g.k AND g.k >= d.id`,
			`SELECT a.id, g.k FROM (SELECT * FROM w WHERE w.f > 1) AS a, (SELECT * FROM h) AS d, g WHERE a.id = g.k AND d.id = 1`,
			`SELECT g.k, COUNT(*) FROM (SELECT * FROM w WHERE w.f > 1) AS a, h AS d, g WHERE a.id >= g.k AND d.id = 1 GROUP BY g.k`,
			`SELECT a.id, d.id FROM (SELECT * FROM w WHERE w.f > 1) AS a LEFT JOIN h AS d ON d.id = a.id - 1`,
			`SELECT a.id, d.id FROM (SELECT * FROM w WHERE w.f > 1) AS a, h AS d WHERE EXISTS (SELECT 1 FROM g AS z WHERE z.k = a.id AND z.v > d.f)`,
			// A derived table over a star join beside another source: the
			// merge of its body into the join would re-tile the join's row by
			// the body's legs, which the join's readers do not name.
			`SELECT d.x FROM (SELECT * FROM w, h) AS a, (SELECT h.f AS x FROM h) AS d`,
			`SELECT d.x FROM (SELECT * FROM w, g) AS a, (SELECT h.f AS x FROM h) AS d`,
			`SELECT a.id, a.k, a.v, d.id FROM (SELECT * FROM w, g) AS a, h AS d WHERE d.id = a.k`,
			`SELECT a.f, a.v, d.f FROM (SELECT * FROM w, g) AS a, h AS d WHERE d.id = a.id AND a.k > 1`,
			`SELECT e.id, a.id, a.k FROM h AS e, (SELECT * FROM w, g) AS a WHERE e.id = a.k AND a.id = e.id`,
			`WITH a AS (SELECT * FROM w, g) SELECT a.id, a.v, d.f FROM a, h AS d WHERE a.k = d.id`,
			`SELECT a.k, COUNT(*) FROM (SELECT * FROM w, g) AS a, h AS d WHERE a.id = d.id GROUP BY a.k`,
			`SELECT a.id, d.id FROM (SELECT * FROM w, g) AS a LEFT JOIN h AS d ON d.id = a.k`,
			`SELECT a.id, a.k, d.k FROM (SELECT * FROM w, g) AS a, (SELECT * FROM w, g) AS d WHERE a.k = d.id AND a.id = d.k`,
			`SELECT d.s FROM w, (SELECT SUM(h.f * w.f) AS s FROM h) AS d`,
			`SELECT d.c FROM w, (SELECT COUNT(*) AS c FROM h HAVING COUNT(*) < w.f) AS d`,
			`SELECT d.k, d.c FROM w, (SELECT h.id AS k, SUM(h.f) + w.f AS c FROM h GROUP BY h.id) AS d`,
			`SELECT d.k FROM w, (SELECT h.id AS k FROM h GROUP BY h.id HAVING COUNT(*) < w.f) AS d`,
			`SELECT d.k FROM w, (SELECT h.id AS k FROM h GROUP BY h.id HAVING COUNT(*) < h.f) AS d`,
			`SELECT d.id FROM q, q.bs AS b, b.tags AS t, (SELECT h.id FROM h WHERE h.f = b.k + 8) AS d`,
			`SELECT d.id FROM q, q.bs AS b AT o, b.tags AS t, (SELECT h.id FROM h WHERE h.f = o + 9) AS d`,
			// An unnest behind a later table inside a derived leg: the leg's gate
			// reads the rotated cluster the body translates to.
			`SELECT d.t, d.f FROM q, (SELECT t, h.f FROM q.bs AS b, b.tags AS t, h) AS d`,
			`SELECT d.b, d.f FROM w, (SELECT b.k AS b, h.f FROM q, q.bs AS b, h) AS d`,
			`SELECT d.t, d.f FROM w, (SELECT t, h.f FROM q, q.bs AS b, b.tags AS t, h) AS d`,
			`SELECT d.t FROM w, (SELECT t FROM q, q.bs AS b, b.tags AS t, h WHERE t + 1 = h.f) AS d`,
			`SELECT v, p, v2 FROM w, w.arr AS v AT p, w.arr AS v2 WHERE v2 > v`,
			`SELECT x.k, x2.k, y FROM q, q.bs AS x, q.bs AS x2, x.tags AS y`,
			`SELECT t, x.k FROM q, q.bs AS b, b.tags AS t, q.bs AS x`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h, w.arr AS v, w.arr AS v2 WHERE v2 = v + 1)`,
			`SELECT d.v, d.p FROM w, (SELECT v, p FROM w.arr AS v, w.arr AS v2 AT p) AS d`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v, w.arr AS v2 WHERE v2 = v + 1)`,
			// Lateral legs correlated to other legs of one FROM: each is one more
			// quantifier of the block, planned inside the leg it reads.
			`SELECT e.k FROM w, (SELECT v AS k FROM w.arr AS v) AS d, (SELECT d.k AS k FROM h) AS e`,
			`SELECT e.k FROM w, (SELECT w.f AS k FROM h) AS d, (SELECT d.k AS k FROM h) AS e`,
			`SELECT e.k FROM w, h, (SELECT w.f + h.f AS k FROM h AS h2) AS e`,
			`SELECT d.k, e.k FROM w, (SELECT v AS k FROM w.arr AS v) AS d, (SELECT w.f AS k FROM h) AS e`,
			`SELECT e.k FROM w, (SELECT w.f AS k FROM h) AS d, (SELECT w.f AS k FROM h AS h2) AS e`,
			`SELECT e.k FROM h, w, (SELECT w.f AS k FROM h AS h2) AS d, (SELECT d.k AS k FROM h AS h3) AS e`,
			`SELECT d.id FROM q, q.bs AS b, (SELECT h.id FROM h WHERE h.f = b.k + 8) AS d`,
			// A LEFT JOIN's null-supplying leg read by a later leg or an EXISTS.
			`SELECT w.id FROM w LEFT JOIN h ON h.id = w.id WHERE h.id IS NULL AND NOT EXISTS (SELECT 1 FROM q WHERE q.id = h.id)`,
			`SELECT w.id FROM w LEFT JOIN h ON h.id = w.id WHERE NOT EXISTS (SELECT 1 FROM q WHERE q.id = h.id)`,
			`SELECT w.id, h.f FROM w LEFT JOIN h ON h.id = w.id WHERE NOT EXISTS (SELECT 1 FROM q WHERE q.id = h.id)`,
			`SELECT w.id FROM w LEFT JOIN h ON h.id = w.id WHERE EXISTS (SELECT 1 FROM q WHERE q.id = h.id)`,
			`SELECT w.id, d.x FROM w LEFT JOIN h ON h.id = w.id, (SELECT h.f AS x FROM q) AS d`,
			`SELECT w.id, d.x FROM w LEFT JOIN h ON h.id = w.id, (SELECT q.id AS x FROM q WHERE q.id = h.id) AS d`,
			// A name the statement wrote is compared exactly (the quoted
			// spellings are 42703 in both engines; the exact ones answer).
			`SELECT w.id FROM w WHERE EXISTS (SELECT E FROM w.arr AS "e" WHERE "e" = 20)`,
			`SELECT d."ka" FROM (SELECT f AS "kA", id AS "Ka" FROM w) d`,
			`WITH c AS (SELECT id AS "x" FROM w) SELECT c."X" FROM c`,
			`WITH c AS (SELECT MIN(id) AS "x" FROM w) SELECT c."X" FROM c`,
			`WITH c AS (SELECT id AS "x" FROM w) SELECT c."x" FROM c`,
			`SELECT w.id FROM w, w.arr AS "e" WHERE E = 20`,
			`SELECT w.id FROM w, w.arr AS "e" AT "o" WHERE O = 1`,
			`SELECT d.K FROM (SELECT id AS "k" FROM w) AS d`,
			`SELECT K FROM (SELECT id AS "k" FROM w) AS d`,
			`WITH c("k") AS (SELECT id FROM w) SELECT K FROM c`,
			`WITH c AS (SELECT id AS "k" FROM w) SELECT K FROM c`,
			`SELECT "k" FROM (SELECT id AS "k" FROM w) AS d`,
			`SELECT d.k FROM (SELECT id AS k FROM w) AS d`,
		}
		dml := []string{}
		reads := []string{}
		javaT, goT := p+"_JT", p+"_GT"
		subst := func(stmt, tmpl string) string {
			return strings.NewReplacer("{T}", tmpl, "{t}", strings.ToLower(tmpl)).Replace(stmt)
		}
		javaDB := "/TEST/" + p + "_J"
		javaDDL := func(stmts ...string) {
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjCatalogDdlJava", map[string]any{"clusterFile": clusterFile, "statements": stmts}, &out)).To(Succeed())
			for i, o := range out["outcomes"].([]any) {
				Expect(o).To(Equal("OK"), "%s", stmts[i])
			}
		}
		javaDDL("CREATE DATABASE "+javaDB, "CREATE SCHEMA TEMPLATE "+javaT+" "+body, "CREATE SCHEMA "+javaDB+"/S WITH TEMPLATE "+javaT)
		defer javaDDL("DROP DATABASE IF EXISTS "+javaDB, "DROP SCHEMA TEMPLATE IF EXISTS "+javaT)
		javaExec := func(stmt string) string {
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjExecuteJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S", "sql": subst(stmt, javaT),
			}, &out)).To(Succeed())
			o := out["outcome"].(string)
			if f := strings.SplitN(o, " ", 4); f[0] == "ERROR" && len(f) == 4 {
				return "ERROR " + f[1] + " " + f[3]
			}
			return o
		}
		javaRead := func(q string) string {
			var out struct {
				Rows [][]any `json:"rows"`
			}
			err := java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S", "querySql": subst(q, javaT),
			}, &out)
			var je *JavaError
			if errors.As(err, &je) {
				return "ERROR " + je.SQLState + " " + je.Message
			}
			Expect(err).NotTo(HaveOccurred(), q)
			return sortedRows(out.Rows)
		}

		goDBPath := "/TEST/" + p + "_G"
		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		for _, stmt := range []string{
			"CREATE DATABASE " + goDBPath, "CREATE SCHEMA TEMPLATE " + goT + " " + body,
			"CREATE SCHEMA " + goDBPath + "/S WITH TEMPLATE " + goT,
		} {
			_, err := sysDB.ExecContext(ctx, stmt)
			Expect(err).NotTo(HaveOccurred(), "%s", stmt)
		}
		defer func() {
			_, _ = sysDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+goDBPath)
			_, _ = sysDB.ExecContext(ctx, "DROP SCHEMA TEMPLATE IF EXISTS "+goT)
		}()
		gdb, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", goDBPath, clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer gdb.Close()
		goErr := func(err error) string {
			var ae *api.Error
			if errors.As(err, &ae) {
				return "ERROR " + string(ae.Code) + " " + ae.Message
			}
			return "ERROR ? " + err.Error()
		}
		goExec := func(stmt string) string {
			res, err := gdb.ExecContext(ctx, subst(stmt, goT))
			if err != nil {
				return goErr(err)
			}
			n, err := res.RowsAffected()
			Expect(err).NotTo(HaveOccurred())
			return fmt.Sprintf("OK %d", n)
		}
		goRead := func(q string) string {
			rs, err := gdb.QueryContext(ctx, subst(q, goT))
			if err != nil {
				return goErr(err)
			}
			defer rs.Close()
			cols, err := rs.Columns()
			Expect(err).NotTo(HaveOccurred())
			var rows [][]any
			for rs.Next() {
				cells := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range cells {
					ptrs[i] = &cells[i]
				}
				Expect(rs.Scan(ptrs...)).To(Succeed())
				for i := range cells {
					cells[i] = wsjEnumJSONShape(cells[i])
				}
				rows = append(rows, cells)
			}
			if err := rs.Err(); err != nil {
				return goErr(err)
			}
			return sortedRows(rows)
		}
		// A refusal agrees by its SQLSTATE; rows agree literally.
		state := func(o string) string {
			if f := strings.SplitN(o, " ", 3); f[0] == "ERROR" && len(f) >= 2 {
				return "ERROR " + f[1]
			}
			return o
		}

		for _, stmt := range setup {
			j, g := javaExec(stmt), goExec(stmt)
			Expect(j).To(Equal("OK 1"), stmt)
			Expect(g).To(Equal("OK 1"), stmt)
		}
		var diffs []string
		compare := func(stmt, j, g string) {
			GinkgoWriter.Printf("WSFJOIN %s\n  java=%s\n  go  =%s\n", stmt, j, g)
			if state(j) != state(g) {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s", stmt, j, g))
			}
		}
		answers := map[string]string{}
		for _, q := range queries {
			g := goRead(q)
			answers[q] = g
			compare(q, javaRead(q), g)
		}
		// Pinned with both answers.
		//  - An explicit JOIN over a correlated array without an ON (a plain or
		//    CROSS JOIN): the target crashes (XXXXX, visitInnerJoin reads a null
		//    ON expression); Go answers the comma form's rows, which is what the
		//    target's grammar and its flat-select design make the statement.
		//  - An OUTER join over a correlated array: the target crashes (XXXXX
		//    "quantifier does not flow records"); Go reads an outer join's source
		//    as a table, which the path is not (42703).
		//  - An unnest leg BEFORE an outer join: the target answers; Go refuses
		//    the JOIN spelling at translation (0AF00, the lateral unnest enclosed
		//    by an outer-join box does not ordinalize) and the comma spelling at
		//    parse (0A000, a JOIN after comma sources, CQ-72). TODO.md "An unnest
		//    leg before an outer join".
		//  - A derived table reading a prior FROM source, beside a later source:
		//    the target crashes (XXXXX, "is not an element of this graph"); Go
		//    answers the empty correlated result the statement defines.
		//  - A later lateral derived table reading a chain's LAST element, and a
		//    WHERE EXISTS over a spine (with a table at its bottom): the target
		//    answers; Go refuses (0AF00) — the chain's merged row is bound under
		//    its link's own binding, so a consumer below it cannot name the
		//    element, and the EXISTS lowering skips a spine link. TODO.md "A
		//    lateral unnest behind a later leg inside a derived leg, and a later
		//    leg reading a chain's element".
		//  - An aggregate over a first FROM item's unnest in a scalar
		//    subquery, in the select list or a WHERE: the target's grammar has
		//    no scalar subquery there (42601); Go's scalar subqueries are a
		//    read-side extension, and these are its rows.
		//  - An enclosing block's value in a grouped block's output or HAVING,
		//    where a grouping key shares its field name (`GROUP BY h.f` beside
		//    `w.f`): the target accepts it semantically and then cannot plan it
		//    (0AF00); the same shapes grouped on `h.id` answer in both engines
		//    (the reads above). Go answers both. A grouping key reading the
		//    enclosing value crashes the target (XX000, a VerifyException);
		//    Go answers it. A scalar subquery in the select list is the
		//    extension noted above.
		//  - A prior derived column typed NULL: the target crashes (XXXXX
		//    "should not be called"); Go refuses — a later lateral derived body
		//    reading it is its own 42703 (the prefix Go cannot describe falls
		//    back to the enclosing scope), one that does not read it leaves the
		//    refusal to the prior source (0AF00). Both engines refuse.
		//  - An enclosing value in a grouped derived body's ORDER BY (with or
		//    without LIMIT): the target has neither ORDER BY nor LIMIT in a
		//    subquery (0A000 "order by is not supported in subquery", 0AF00
		//    "LIMIT clause is not supported"); both are Go read-side
		//    extensions, and these are its rows (TestFDB_PostAggregate
		//    OuterReferenceInOrderBy asserts them over several groups).
		//  - An outer join whose preserved side is a first FROM item's scalar
		//    unnest: the target crashes (XXXXX "quantifier does not flow
		//    records") on LEFT and has no FULL in its grammar (42601); Go
		//    refuses at translation (0AF00, the outer box over a bare-element
		//    leg takes no ordinal seed) — both engines refuse.
		declared := map[string][2]string{
			// An ON reading an alias buried in a preserved join (a1 under a1 JOIN
			// a2): the target fails planning with an internal error ("Node
			// Reference@… is not an element of this graph", XXXXX); Go answers,
			// its rows checked by hand (b.k = a1.id + 1 matches nothing for w1's
			// a1 = 1; kk: a1 = 1 meets g3, a1 = 2 is null-extended).
			`SELECT w.id, d.id1, d.bk FROM w, (SELECT a1.id AS id1, b.k AS bk FROM (SELECT h.id FROM h WHERE h.f = w.f * 10) AS a1 JOIN h AS a2 ON a2.id = a1.id LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON b.k = a1.id + 1) AS d`:   {"ERROR XXXXX", "[[1 1 <nil>]]"},
			`SELECT w.id, d.id1, d.bk FROM w, (SELECT a1.id AS id1, b.k AS bk FROM (SELECT kk.id FROM kk WHERE kk.id <= w.id) AS a1 JOIN kk AS a2 ON a2.id = a1.id LEFT JOIN (SELECT g.k FROM g WHERE g.k >= w.id) AS b ON b.k = a1.id + 2) AS d`: {"ERROR XXXXX", "[[1 1 3] [2 1 3] [2 2 <nil>] [3 1 3] [3 2 <nil>]]"},
			`SELECT w.id, d.k FROM w, (SELECT h.id AS k FROM h GROUP BY h.id ORDER BY (h.id - w.id) * (h.id - w.id) LIMIT 1) AS d`:                                                                                                                {"ERROR 0AF00", "[[1 1] [2 1] [3 1]]"},
			`SELECT w.id, d.k, d.c FROM w, (SELECT h.f AS k, COUNT(*) AS c FROM h GROUP BY h.f ORDER BY COUNT(*) * w.f DESC LIMIT 1) AS d`:                                                                                                        {"ERROR 0AF00", "[[1 10 1] [2 10 1] [3 10 1]]"},
			`SELECT d.k FROM w, (SELECT h.f AS k FROM h GROUP BY h.f ORDER BY COUNT(*) + w.f) AS d`:                                                                                                                                               {"ERROR 0A000", "[[10] [10] [10]]"},
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v LEFT JOIN h ON h.f = v)`:                                                                                                                                                     {"ERROR XXXXX", "ERROR 0AF00"},
			`SELECT d.v, d.f FROM w, (SELECT v, h.f FROM w.arr AS v LEFT JOIN h ON h.f = v) AS d`:                                                                                                                                                 {"ERROR XXXXX", "ERROR 0AF00"},
			`SELECT d.v, d.f FROM w, (SELECT v, h.f FROM w.arr AS v FULL JOIN h ON h.f = v) AS d`:                                                                                                                                                 {"ERROR 42601", "ERROR 0AF00"},
			`SELECT w.id, (SELECT SUM(v) FROM w.arr AS v) FROM w`:                                                                                                                                                                                 {"ERROR 42601", "[[1 21] [2 20] [3 <nil>]]"},
			`SELECT id FROM w WHERE (SELECT SUM(v) FROM w.arr AS v) > 20`:                                                                                                                                                                         {"ERROR 42601", "[[1]]"},
			`SELECT r.id, (SELECT MAX(x.k) FROM r.items AS x) FROM r`:                                                                                                                                                                             {"ERROR 42601", "[[1 6]]"},
			`SELECT d.x FROM w, (SELECT w.f AS x FROM h WHERE h.f = w.f) AS d, h`:                                                                                                                                                                 {"ERROR XXXXX", "[]"},
			`SELECT v, h.f FROM w JOIN w.arr AS v ON v > 10 LEFT JOIN h ON h.id = w.id`:                                                                                                                                                           {"[[11 10] [20 <nil>]]", "ERROR 0AF00"},
			`SELECT d.id FROM q, q.bs AS b, b.tags AS t, (SELECT h.id FROM h WHERE h.f = t + 1) AS d`:                                                                                                                                             {"[[1]]", "ERROR 0AF00"},
			`SELECT t FROM q, q.bs AS b, b.tags AS t WHERE EXISTS (SELECT 1 FROM h WHERE h.f = t + 1)`:                                                                                                                                            {"[[9]]", "ERROR 0AF00"},
			`SELECT t FROM q, q.bs AS b, b.tags AS t WHERE EXISTS (SELECT 1 FROM h WHERE h.f = b.k + 8)`:                                                                                                                                          {"[[9]]", "ERROR 0AF00"},
			`SELECT t FROM q, q.bs AS b, b.tags AS t WHERE NOT EXISTS (SELECT 1 FROM h WHERE h.f = t + 1)`:                                                                                                                                        {"[[7] [8]]", "ERROR 0AF00"},
			`SELECT v, v2 FROM w, w.arr AS v, w.arr AS v2 WHERE EXISTS (SELECT 1 FROM h WHERE h.f = v)`:                                                                                                                                           {"[[10 10] [10 11]]", "ERROR 0AF00"},
			`SELECT v, h.f FROM w, w.arr AS v LEFT JOIN h ON h.id = w.id WHERE v > 10`:                                                                                                                                                            {"[[11 10] [20 <nil>]]", "ERROR 0A000"},
			`SELECT v, h.f FROM w, w.arr AS v LEFT JOIN h ON h.id = w.id`:                                                                                                                                                                         {"[[10 10] [11 10] [20 <nil>]]", "ERROR 0A000"},
			`SELECT v FROM w CROSS JOIN w.arr AS v`:                                                                                                                                                                                               {"ERROR XXXXX", "[[10] [11] [20]]"},
			`SELECT v FROM w JOIN w.arr AS v`:                                                                                                                                                                                                     {"ERROR XXXXX", "[[10] [11] [20]]"},
			`SELECT v FROM w LEFT JOIN w.arr AS v ON v = 1`:                                                                                                                                                                                       {"ERROR XXXXX", "ERROR 42703"},
			`SELECT v FROM w LEFT JOIN w.arr AS v ON 1 = 1`:                                                                                                                                                                                       {"ERROR XXXXX", "ERROR 42703"},
			`SELECT v FROM w RIGHT JOIN w.arr AS v ON 1 = 1`:                                                                                                                                                                                      {"ERROR XXXXX", "ERROR 42703"},
			`SELECT w.id, (SELECT COUNT(*) + w.f FROM h) FROM w`:                                                                                                                                                                                  {"ERROR 42601", "[[1 2] [2 3] [3 4]]"},
			`SELECT d.k FROM w, (SELECT h.f AS k FROM h GROUP BY h.f HAVING COUNT(*) < w.f) AS d`:                                                                                                                                                 {"ERROR 0AF00", "[[10] [10]]"},
			`SELECT d.k, d.c FROM w, (SELECT h.f AS k, SUM(h.id) * w.f AS c FROM h GROUP BY h.f) AS d`:                                                                                                                                            {"ERROR 0AF00", "[[10 1] [10 2] [10 3]]"},
			`SELECT d.k, d.s FROM w, (SELECT h.f + w.f AS k, COUNT(*) AS s FROM h GROUP BY h.f + w.f) AS d`:                                                                                                                                       {"ERROR XX000", "[[11 1] [12 1] [13 1]]"},
			`SELECT d.x FROM (SELECT NULL AS n FROM h) AS a, (SELECT a.n AS x FROM h) AS d`:                                                                                                                                                       {"ERROR XXXXX", "ERROR 42703"},
			`SELECT d.x FROM (SELECT NULL AS n FROM h) AS a, (SELECT h.f AS x FROM h) AS d`:                                                                                                                                                       {"ERROR XXXXX", "ERROR 0AF00"},
		}
		for q, want := range declared {
			j, g := javaRead(q), goRead(q)
			GinkgoWriter.Printf("WSFJOIN %s (declared)\n  java=%s\n  go  =%s\n", q, j, g)
			if state(j) != want[0] || state(g) != want[1] {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s, declared java=%s go=%s", q, j, g, want[0], want[1]))
			}
		}
		for _, stmt := range dml {
			g := goExec(stmt)
			answers[stmt] = g
			compare(stmt, javaExec(stmt), g)
		}
		for _, q := range reads {
			compare(q, javaRead(q), goRead(q))
		}
		Expect(diffs).To(BeEmpty(), strings.Join(diffs, "\n"))

		// Go's answers, asserted literally, so both engines moving together
		// cannot pass as agreement.
		for q, want := range map[string]string{
			`SELECT v FROM w JOIN w.arr AS v ON v > 10`:                                             "[[11] [20]]",
			`SELECT w.id, v FROM w JOIN w.arr AS v ON v = w.id + 9`:                                 "[[1 10]]",
			`SELECT v, p FROM w JOIN w.arr AS v AT p ON p = 1`:                                      "[[10 1] [20 1]]",
			`SELECT x.k FROM r JOIN r.items AS x ON x.k = 6`:                                        "[[6]]",
			`SELECT v, h.f FROM w JOIN w.arr AS v ON 1 = 1 JOIN h ON h.id = w.id`:                   "[[10 10] [11 10]]",
			`SELECT COUNT(*) FROM w JOIN w.arr AS v ON 1 = 1`:                                       "[[3]]",
			`SELECT v FROM w JOIN w.arr AS v USING (id)`:                                            "ERROR 42703",
			`SELECT h.id FROM w JOIN h AT p ON 1 = 1`:                                               "ERROR 42809",
			`SELECT id FROM w WHERE EXISTS (SELECT * FROM w.arr AS v WHERE v = 20)`:                 "[[2]]",
			`SELECT id FROM w WHERE EXISTS (SELECT v FROM w.arr AS v, h WHERE v = h.f)`:             "[[1]]",
			`SELECT id FROM w WHERE EXISTS (SELECT p FROM w.arr AT p)`:                              "[[1] [2]]",
			`SELECT d.* FROM w, (SELECT * FROM w.arr AS v, h) AS d`:                                 "[[10 1 10] [11 1 10] [20 1 10]]",
			`SELECT w.id FROM w, w.arr AS "e" WHERE E = 20`:                                         "ERROR 42703",
			`WITH c("k") AS (SELECT id FROM w) SELECT K FROM c`:                                     "ERROR 42703",
			`SELECT d.s FROM w, (SELECT SUM(v) AS s FROM w.arr AS v) AS d`:                          "[[20] [21] [<nil>]]",
			`SELECT d."ka" FROM (SELECT f AS "kA", id AS "Ka" FROM w) d`:                            "ERROR 42703",
			`SELECT kk.id, i.g FROM kk JOIN h ON h.id = kk.id JOIN kk.items AS i USING (k)`:         "[[1 a]]",
			`SELECT a.id, i.g FROM kk AS a JOIN kk AS b ON a.id = b.id JOIN a.items AS i USING (k)`: "ERROR 42702",
			`SELECT v, h.f FROM w JOIN w.arr AS v ON 1 = 1 JOIN h USING (id)`:                       "[[10 10] [11 10]]",
		} {
			got, ok := answers[q]
			Expect(ok).To(BeTrue(), "%s is not a statement of this spec", q)
			Expect(state(got)).To(Equal(want), q)
		}
		// And the words Go shares with Java's resolveJoinUsingClause refusal:
		// the USING column resolved on the unnest's own operator.
		for q, holds := range map[string]string{
			`SELECT v FROM w JOIN w.arr AS v USING (id)`:         "Unknown reference ID",
			`SELECT v FROM w JOIN w.arr AS v AT p USING (id)`:    "Unknown reference ID",
			`SELECT k FROM kk JOIN kk.items AS i AT p USING (k)`: "Unknown reference K",
		} {
			Expect(answers[q]).To(ContainSubstring(holds), q)
		}
	})
})

// sortedRows renders rows as a sorted multiset: an unnest's element order is
// not the target's to promise, and an ORDER BY over an element is a plan the
// target cannot make (0AF00), so the rows compare unordered.
func sortedRows(rows [][]any) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprint(r)
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}
