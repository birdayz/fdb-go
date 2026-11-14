#!/usr/bin/env python3
"""Fake hcloud for the teardown/watchdog test: JSON state in $FAKE_HCLOUD_STATE.  Deleting a server takes two list calls
to disappear (like the real asynchronous delete); firewalls/networks cannot be deleted while a server exists."""
import json, os, sys
p = os.environ["FAKE_HCLOUD_STATE"]
st = json.load(open(p))
a = sys.argv[1:]
log = open(p + ".calls", "a"); log.write(" ".join(a) + "\n"); log.close()
kind, op = a[0], a[1]
def save(): json.dump(st, open(p, "w"))
if op == "list":
    sel = a[a.index("-l") + 1].split("=")
    out = []
    for i in list(st[kind]):
        if i["labels"].get(sel[0]) != sel[1]:
            continue
        if "dying" in i:
            i["dying"] -= 1
            if i["dying"] <= 0:
                st[kind].remove(i)
                continue
        out.append(i)
    save(); print(json.dumps(out) if out else "null")
elif op == "delete":
    ident = int(a[2])
    if kind in ("firewall", "network") and any(x["labels"].get("campaign") == "lean-unit" for x in st["server"]):
        print("resource in use", file=sys.stderr); sys.exit(1)
    for i in st[kind]:
        if i["id"] == ident:
            if kind == "server": i.setdefault("dying", 2)
            else: st[kind] = [x for x in st[kind] if x["id"] != ident]
    save()
