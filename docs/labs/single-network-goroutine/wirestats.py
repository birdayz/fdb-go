#!/usr/bin/env python3
"""Per-port, per-direction packet/byte/frame counts of a loopback pcap.

Usage: wirestats.py capture.pcap SERVER_PORTS [OPS]

Reassembles each TCP direction in sequence order (the capture must have no
drops: wirecapture.sh checks), skips the ConnectPacket, splits FDB frames
([len 4][xxh3 8][token 16][body]) and classifies requests by root file
identifier. Prints JSON; with OPS, also every count per operation.
"""

import collections
import json
import struct
import sys

FILE_IDS = {838566: "grv", 93948: "commit", 8454530: "get", 6795746: "getkeyvalues"}


def packets(path):
    with open(path, "rb") as f:
        magic = f.read(24)
        nano = magic[:4] in (b"\x4d\x3c\xb2\xa1",)
        assert magic[:4] in (b"\xd4\xc3\xb2\xa1", b"\x4d\x3c\xb2\xa1"), "little-endian pcap only"
        link = struct.unpack("<I", magic[20:24])[0]
        while True:
            h = f.read(16)
            if len(h) < 16:
                return
            sec, frac, incl, _ = struct.unpack("<IIII", h)
            data = f.read(incl)
            ts = sec * 10**9 + (frac if nano else frac * 1000)
            off = 14 if link == 1 else 16 if link == 113 else 0
            ip = data[off:]
            if not ip or ip[0] >> 4 != 4:
                continue
            ihl = (ip[0] & 15) * 4
            total = struct.unpack(">H", ip[2:4])[0]
            tcp = ip[ihl:total]
            sport, dport, seq = struct.unpack(">HHI", tcp[:8])
            flags = tcp[13]
            doff = (tcp[12] >> 4) * 4
            yield ts, sport, dport, seq, flags, tcp[doff:]


def body_file_id(body):
    off = 8 if len(body) >= 16 and body[7] == 0x0F and body[6] == 0xDB else 0
    if len(body) < off + 8:
        return None
    return struct.unpack("<I", body[off + 4:off + 8])[0]


def main():
    path, ports = sys.argv[1], {int(p) for p in sys.argv[2].split(",")}
    ops = int(sys.argv[3]) if len(sys.argv) > 3 else 0
    streams = {}  # (src, dst) -> state
    stat = collections.defaultdict(lambda: collections.Counter())
    flows = set()
    for ts, sp, dp, seq, flags, payload in packets(path):
        if sp not in ports and dp not in ports:
            continue
        key = (sp, dp)
        server = dp if dp in ports else sp
        direction = "c2s" if dp in ports else "s2c"
        if flags & 0x02:  # SYN
            streams[key] = {"next": (seq + 1) & 0xFFFFFFFF, "buf": b"", "connect": True}
            if direction == "c2s":
                flows.add(key)
            continue
        st = streams.get(key)
        if st is None or not payload:
            continue
        if seq != st["next"]:
            if ((seq - st["next"]) & 0xFFFFFFFF) > 1 << 31:
                continue  # retransmission of old data
            sys.exit(f"stream hole on {key}")
        st["next"] = (seq + len(payload)) & 0xFFFFFFFF
        s = stat[f"{server}/{direction}"]
        s["segments"] += 1
        s["bytes"] += len(payload)
        st["buf"] += payload
        buf, frames = st["buf"], 0
        while True:
            if st["connect"]:
                if len(buf) < 4:
                    break
                n = struct.unpack("<I", buf[:4])[0]
                if len(buf) < 4 + n:
                    break
                buf, st["connect"] = buf[4 + n:], False
                continue
            if len(buf) < 12:
                break
            n = struct.unpack("<I", buf[:4])[0]
            if len(buf) < 12 + n:
                break
            payload_f = buf[12:12 + n]
            buf = buf[12 + n:]
            frames += 1
            if direction == "c2s":
                kind = FILE_IDS.get(body_file_id(payload_f[16:]), "other")
                s["req_" + kind] += 1
        st["buf"] = buf
        s["frames"] += frames
        s[f"segs_with_{min(frames, 9)}_frames"] += 1
    out = {"client_flows": len(flows), "flows_by_port": dict(collections.Counter(d for _, d in flows))}
    for k, s in sorted(stat.items()):
        d = dict(s)
        d["frames_per_segment"] = s["frames"] / s["segments"] if s["segments"] else 0
        if ops:
            d.update({f"{m}_per_op": s[m] / ops for m in ("segments", "bytes", "frames")})
        out[k] = d
    print(json.dumps(out, indent=1, sort_keys=True))


if __name__ == "__main__":
    main()
