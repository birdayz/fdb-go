"""Safe pre-split conditioning of one tenant (store) -- used by leanrun.py and by condtest/.

Sequence per producer (all with the SAME tool for every variant, the Go own-client executable):
  setup(arm)            fresh store, baseline written by the control executable
  snapshot(S)           S = store prefix (layout record_prefix without its last tuple element)
  ingest(go_pure, full) writes the whole measured population into the real measured store -> DD splits shards
  wait DD quiet
  restore(S)            ONE transaction: clearrange [S, S+\xff) then set every snapshotted baseline key again
  (DD shard boundaries survive: empty shards merge only after minutes -- same property the proven run relied on)
Then the variant's own measured ingest runs against the very same store.
"""
import hashlib
import re

ESC = re.compile(r"\\x([0-9a-fA-F]{2})|\\\\")


def unesc(s):
    out, i = bytearray(), 0
    while i < len(s):
        if s.startswith("\\\\", i):
            out.append(0x5c); i += 2
        elif s.startswith("\\x", i):
            out.append(int(s[i + 2:i + 4], 16)); i += 4
        else:
            out += s[i].encode(); i += 1
    return bytes(out)


def esc(b):
    return "".join("\\x%02x" % c for c in b)


def store_prefix(layout):
    rp = layout["record_prefix"]
    if not rp.endswith("1501"):
        raise ValueError("unexpected record prefix " + rp)
    return bytes.fromhex(rp[:-4])


def parse_getrange(text):
    """fdbcli getrange output -> [(key, value)].  Lines look like: `KEY' is `VALUE'  (escaped)."""
    items = []
    for line in text.splitlines():
        if not line.startswith("`"):
            continue
        k, sep, v = line[1:].partition("' is `")
        if not sep or not v.endswith("'"):
            raise ValueError("unparsable getrange line: " + line[:80])
        items.append((unesc(k), unesc(v[:-1])))
    return items


def getrange_cmd(begin, end, limit=100000):
    return "getrange %s %s %d" % (esc(begin), esc(end), limit)


def restore_cmd(prefix, snapshot):
    end = prefix + b"\xff"
    parts = ["writemode on", "begin", "clearrange %s %s" % (esc(prefix), esc(end))]
    parts += ["set %s %s" % (esc(k), esc(v) if v else '""') for k, v in snapshot]
    parts.append("commit")
    return "; ".join(parts)


def digest(items):
    h = hashlib.sha256()
    for k, v in items:
        h.update(len(k).to_bytes(4, "big") + k + len(v).to_bytes(4, "big") + v)
    return h.hexdigest()
