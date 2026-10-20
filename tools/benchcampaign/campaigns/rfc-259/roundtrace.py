import re, sys, statistics
from collections import Counter

def load(d):
    ss = open(f'{d}/ss.txt').read().splitlines()
    client_ports = set()
    for l in ss:
        m = re.match(r'\s*\d+\s+\d+\s+\S+:(\d+)\s+\S+:4721\s.*clientbench', l)
        if m:
            client_ports.add(m.group(1))
    fds = set()
    for l in ss:
        m = re.match(r'\s*\d+\s+\d+\s+\S+:4721\s+\S+:(\d+)\s.*fdbserver",pid=\d+,fd=(\d+)', l)
        if m and m.group(1) in client_ports:
            fds.add(int(m.group(2)))
    ev = []
    for l in open(f'{d}/trace.txt'):
        m = re.match(r'\s*([\d.]+) \(.*?\): \S+ (recvfrom|sendmsg)\(fd: (\d+),.*\)\s+= (-?\d+)', l)
        if m and int(m.group(3)) in fds and int(m.group(4)) > 0:
            ev.append((float(m.group(1)), m.group(2), int(m.group(4))))
    return fds, ev

def bursts(times, gap):
    out, cur = [], []
    for t in times:
        if cur and t - cur[-1] > gap:
            out.append(cur); cur = []
        cur.append(t)
    if cur: out.append(cur)
    return out

for label in sys.argv[1:]:
    fds, ev = load(f'roundtrace/{label}')
    rx = [(t, b) for t, k, b in ev if k == 'recvfrom']
    tx = [(t, b) for t, k, b in ev if k == 'sendmsg']
    span = (ev[-1][0] - ev[0][0]) if ev else 0
    print(f'== {label}: client fds on stateless {sorted(fds)}; {len(rx)} reads, {len(tx)} sends in {span:.0f} ms')
    rb = bursts([t for t, _ in rx], 0.3)
    print(f'   arrival bursts (gap>0.3ms): {len(rb)} = {len(rb)/span*1000:.0f}/s; reads per burst {Counter(len(b) for b in rb).most_common(6)}')
    sizes = Counter(b for _, b in rx)
    print(f'   read sizes (bytes: count): {sizes.most_common(10)}')
    print(f'   send sizes: {Counter(b for _, b in tx).most_common(8)}')
    t0 = ev[len(ev)//2][0]
    print('   timeline (ms from t0):')
    for t, k, b in ev:
        if t0 <= t < t0 + 25:
            print(f'     {t-t0:7.3f} {"<- read " if k=="recvfrom" else "-> send "} {b}')
