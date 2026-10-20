#!/usr/bin/env python3
"""GRV requests sent between consecutive commit batches of one wirecapture run.

Usage: wire_grv_rounds.py <wirecapture.sh output dir>. Reads events.csv; prints
sample batches from the middle of the run and the most common
(commit batch size, GRV txn counts) patterns.
"""
import csv, sys, statistics as st
from collections import defaultdict
ev=list(csv.DictReader(open(sys.argv[1]+'/events.csv')))
com=[e for e in ev if e['kind']=='commit' and int(e['reply_ns'])>0]
grv=[e for e in ev if e['kind']=='grv' and int(e['reply_ns'])>0]
# server commit batches by version, with reply time = max reply_ns
b=defaultdict(list)
for e in com: b[int(e['version'])].append(e)
batches=sorted(b.values(), key=lambda x: min(int(e['reply_ns']) for e in x))
grv.sort(key=lambda e:int(e['sent_ns']))
# for a sample of batches in the middle, list GRV requests sent between this batch's first reply and next batch's first reply
out=[]
gi=0
pat=defaultdict(int)
for i in range(len(batches)//2, len(batches)//2+400):
    if i+1>=len(batches): break
    t0=min(int(e['reply_ns']) for e in batches[i]); t1=min(int(e['reply_ns']) for e in batches[i+1])
    sz=len(batches[i])
    gs=[g for g in grv if t0<=int(g['sent_ns'])<t1]
    key=(sz, tuple((int(g['txn_count'])) for g in gs))
    pat[key]+=1
    if len(out)<6: out.append((sz, [(int(g['txn_count']), (int(g['sent_ns'])-t0)//1000, (int(g['reply_ns'])-t0)//1000, int(g['version'])%100000) for g in gs], (max(int(e['reply_ns']) for e in batches[i])-t0)//1000))
for o in out: print("batch",o[0],"replies spread",o[2],"us; GRVs (txns, sent+us, reply+us, ver):",o[1])
print("patterns (batch size, GRV txn counts) top:")
for k,v in sorted(pat.items(), key=lambda x:-x[1])[:8]: print(" ",v,k)
