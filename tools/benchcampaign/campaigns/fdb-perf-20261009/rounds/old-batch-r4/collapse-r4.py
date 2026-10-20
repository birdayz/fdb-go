#!/usr/bin/env python3
import collections
import json
from pathlib import Path
import re
import statistics
import sys
p = Path(sys.argv[1])
mode_names = {0:'unknown',1:'kernel',2:'user',3:'hypervisor',4:'guest-kernel',5:'guest-user'}
result = {}
for arm in ['old', 'cgo']:
    stacks_path = p / (arm + '-stacks.perf-script')
    if not stacks_path.exists(): continue
    raw = (p / (arm + '-raw-events.txt')).read_text()
    sample_matches = re.findall(r'PERF_RECORD_SAMPLE\(IP, (0x[0-9a-fA-F]+)\):\s+\d+/\d+:\s+(0x[0-9a-fA-F]+)\s+period:\s+(\d+)', raw)
    assert sample_matches
    raw_samples = [(int(misc,16)&7, int(ip,16), int(period)) for misc,ip,period in sample_matches]
    mode_counts = collections.Counter(mode_names.get(m,'other') for m,ip,period in raw_samples)
    mode_periods = collections.Counter()
    for m,ip,period in raw_samples: mode_periods[mode_names.get(m,'other')] += period
    blocks = [x for x in stacks_path.read_text().split('\n\n') if x.strip()]
    leaves = [line for line in (p / (arm + '-leaves.perf-script')).read_text().splitlines() if line.strip()]
    assert len(blocks) == len(leaves) == len(raw_samples), (arm,len(blocks),len(leaves),len(raw_samples))
    recorded = re.search(r'\((\d+) samples\)', (p / (arm + '-record.stderr')).read_text())
    assert recorded and int(recorded.group(1)) == len(raw_samples)
    folded = collections.Counter();folded_period = collections.Counter();script_modes = collections.Counter();script_mode_periods = collections.Counter()
    quality = collections.Counter();native_depth = [];native_user_depth = [];native_examples = []
    native_leaf_examples = []
    frame_rx = re.compile(r'^\s+([0-9a-fA-F]+)\s+(.+)\s+\((.+)\)$')
    leaf_rx = re.compile(r'^(\S+)\s+(\d+)\s+\S+:\s+([0-9a-fA-F]+)\s+(.+)\s+\((.+)\)$')
    for block, leaf in zip(blocks, leaves):
        lines = block.splitlines()
        header = re.match(r'^(.+?)\s+(\d+)/(\d+)\s+(\S+)\s+([\d.]+):\s+(.+):\s*$', lines[0])
        assert header, lines[0]
        lm = leaf_rx.match(leaf);assert lm,leaf
        leaf_misc,period,leaf_ip,leaf_sym,leaf_dso = lm.groups()
        period = int(period)
        frames = []
        for line in lines[1:]:
            if not line.strip(): continue
            m = frame_rx.match(line);assert m,line
            ip,sym,dso = m.groups();frames.append((int(ip,16),sym.strip(),dso))
        assert frames and frames[0][0] == int(leaf_ip,16), (leaf,frames[:1])
        assert header.group(4) == leaf_misc
        cpu_mode = {'K':'kernel','U':'user','H':'hypervisor','G':'guest'}.get(leaf_misc[0], 'other')
        script_modes[cpu_mode] += 1;script_mode_periods[cpu_mode] += period
        names = [f'{dso}!{sym if sym != "[unknown]" else sym+"@"+hex(ip)}'.replace(';',':') for ip,sym,dso in reversed(frames)]
        key = ';'.join([arm,cpu_mode,header.group(1).strip().replace(';',':'),*names])
        folded[key] += 1;folded_period[key] += period
        quality['all_samples'] += 1
        if any(sym == '[unknown]' for ip,sym,dso in frames): quality['samples_with_unknown_frame'] += 1
        if leaf_sym == '[unknown]': quality['unknown_leaf_samples'] += 1
        if any('libfdb_c' in dso for ip,sym,dso in frames):
            quality['any_libfdb_frame_samples'] += 1
            if len(native_examples) < 5: native_examples.append(block)
        if 'libfdb_c' in leaf_dso:
            quality['libfdb_leaf_samples'] += 1
            depth = sum('libfdb_c' in dso for ip,sym,dso in frames)
            user_depth = sum(not dso.startswith('[kernel') for ip,sym,dso in frames)
            native_depth.append(depth);native_user_depth.append(user_depth)
            if depth == 1: quality['libfdb_leaf_only_one_native_frame'] += 1
            if user_depth == 1: quality['libfdb_leaf_only_one_user_frame'] += 1
            if any(sym == '[unknown]' for ip,sym,dso in frames): quality['libfdb_leaf_with_unknown_frame'] += 1
            if len(native_leaf_examples) < 8: native_leaf_examples.append(block)
    assert script_modes == mode_counts and script_mode_periods == mode_periods
    assert sum(folded.values()) == len(raw_samples)
    assert sum(folded_period.values()) == sum(period for m,ip,period in raw_samples)
    for suffix, values in [('samples.folded',folded),('period.folded',folded_period)]:
        (p / (arm+'-'+suffix)).write_text(''.join(f'{key} {count}\n' for key,count in sorted(values.items())))
    for kind,limit in [('flat',80),('children',60)]:
        source = (p / (arm+'-'+kind+'-full.txt')).read_text().splitlines()
        headings = [s for s in source if s.startswith('#')]
        entries = [s for s in source if re.match(r'^\s*\d+\.\d+%\s',s)]
        assert entries
        text = '\n'.join(headings) + f'\n# Extracted first {min(limit,len(entries))} of {len(entries)} symbol-table rows; callchain body omitted here, retained in full report.\n' + '\n'.join(entries[:limit]) + '\n'
        (p / (arm+'-'+kind+'-top'+str(limit)+'.txt')).write_text(text)
    details = {'sample_count':len(raw_samples),'mode_sample_counts':dict(mode_counts),'mode_sample_percent':{k:100*v/len(raw_samples) for k,v in mode_counts.items()},'mode_period_counts':dict(mode_periods),'mode_period_percent':{k:100*v/sum(mode_periods.values()) for k,v in mode_periods.items()},'total_period':sum(mode_periods.values()),'quality':dict(quality),'libfdb_leaf_native_depth_histogram':dict(collections.Counter(native_depth)),'libfdb_leaf_user_depth_histogram':dict(collections.Counter(native_user_depth)),'libfdb_leaf_native_depth_median':statistics.median(native_depth) if native_depth else None,'libfdb_leaf_user_depth_median':statistics.median(native_user_depth) if native_user_depth else None,'lost_record_count':len(re.findall(r'PERF_RECORD_LOST\b',raw)),'throttle_record_count':len(re.findall(r'PERF_RECORD_THROTTLE\b',raw)),'unthrottle_record_count':len(re.findall(r'PERF_RECORD_UNTHROTTLE\b',raw))}
    result[arm] = details
    (p / (arm+'-native-stack-examples.txt')).write_text('ANY NATIVE FRAME\n\n'+'\n\n'.join(native_examples)+'\n\nNATIVE LEAF\n\n'+'\n\n'.join(native_leaf_examples)+'\n')
(p / 'sample-split-and-quality.json').write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps(result,indent=2))
