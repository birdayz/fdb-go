#!/usr/bin/env python3
"""Decode profile.proto and fold resolved Go stacks, collapsing all sample labels."""
import collections
import gzip
import hashlib
import json
from pathlib import Path
import sys

def varint(data, pos):
    value=0
    shift=0
    while True:
        byte=data[pos];pos+=1
        value|=(byte&127)<<shift
        if byte<128:return value,pos
        shift+=7
        assert shift<=63

def fields(data):
    out=collections.defaultdict(list)
    pos=0
    while pos<len(data):
        key,pos=varint(data,pos);number,wire=key>>3,key&7
        if wire==0:value,pos=varint(data,pos)
        elif wire==2:
            length,pos=varint(data,pos);value=data[pos:pos+length];pos+=length
            assert len(value)==length
        elif wire in (1,5):
            length=8 if wire==1 else 4;value=data[pos:pos+length];pos+=length
            assert len(value)==length
        else:raise ValueError('unsupported protobuf wire type '+str(wire))
        out[number].append(value)
    assert pos==len(data)
    return out

def repeated(values):
    out=[]
    for value in values:
        if isinstance(value,int):out.append(value);continue
        pos=0
        while pos<len(value):
            item,pos=varint(value,pos);out.append(item)
    return out

def one(obj,key,default=0):
    values=obj.get(key,[default]);assert len(values)==1
    return values[0]

source=Path(sys.argv[1]);dest=Path(sys.argv[2]);dest.mkdir(exist_ok=False)
data=fields(gzip.decompress(source.read_bytes()))
strings=[x.decode('utf-8') for x in data[6]]
assert strings[0]==''
functions={}
for blob in data[5]:
    f=fields(blob);identifier=one(f,1);assert identifier not in functions
    functions[identifier]=strings[one(f,2)]
locations={}
for blob in data[4]:
    loc=fields(blob);identifier=one(loc,1);assert identifier not in locations
    names=[functions[one(fields(line),1)] for line in loc[4]]
    locations[identifier]=names or ['[address:'+hex(one(loc,3))+']']
types=[]
for blob in data[1]:
    value=fields(blob);types.append((strings[one(value,1)],strings[one(value,2)]))
folds=[collections.Counter() for _ in types];totals=[0]*len(types);sample_count=0
for blob in data[2]:
    sample=fields(blob);ids=repeated(sample[1]);values=repeated(sample[2])
    assert len(values)==len(types) and ids and all(v<(1<<63) for v in values)
    frames=[name for identifier in ids for name in locations[identifier]]
    key=';'.join(name.replace(';',':').replace('\n',' ') for name in reversed(frames))
    for i,value in enumerate(values):
        totals[i]+=value
        if value:folds[i][key]+=value
    sample_count+=1
assert sample_count>0
report=dict(source=str(source),sha256=hashlib.sha256(source.read_bytes()).hexdigest(),samples=sample_count,
            scope='Resolved profile.proto functions, root-to-leaf including inline frames; sample labels collapsed; weights preserve profile values and units. Go heap includes sampled lifetime allocations, not native heap.',types=[])
for (name,unit),fold,total in zip(types,folds,totals):
    assert sum(fold.values())==total
    assert name.replace('_','').isalnum() and unit.replace('_','').isalnum()
    filename=name+'-'+unit+'.folded'
    (dest/filename).write_text(''.join(f'{stack} {weight}\n' for stack,weight in sorted(fold.items())))
    report['types'].append(dict(name=name,unit=unit,total=total,stacks=len(fold),file=filename))
(dest/'fold-validation.json').write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps(report,indent=2))
