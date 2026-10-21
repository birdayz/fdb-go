"""Verify every archived source member before removing a disposable worktree."""
import argparse, hashlib, json, os, pathlib, subprocess, tarfile
p=argparse.ArgumentParser()
for key in ['archive','worktree','inputs','tree','output']: p.add_argument('--'+key,type=pathlib.Path,required=True)
a=p.parse_args(); checked=[]
with tarfile.open(a.archive,'r:gz') as t:
    for m in t:
        f=a.worktree/m.name
        if m.isfile():
            live=f.read_bytes(); assert t.extractfile(m).read()==live,m.name
            assert bool(m.mode & 0o111)==bool(f.stat().st_mode & 0o111),m.name
            checked.append({'path':m.name,'sha256':hashlib.sha256(live).hexdigest()})
        elif m.issym():
            assert os.readlink(f)==m.linkname,m.name
            checked.append({'path':m.name,'symlink':m.linkname})
        else: assert m.isdir(),m.name
subprocess.run(['sha256sum','-c',str(a.inputs)],cwd=a.worktree,check=True)
status=subprocess.check_output(['git','status','--short','--untracked-files=all'],cwd=a.worktree,text=True)
a.output.write_text(json.dumps({'tree':a.tree.read_text().strip(),'archive_sha256':hashlib.sha256(a.archive.read_bytes()).hexdigest(),'entries':len(checked),'status':status,'files':checked},indent=2)+'\n')
print('PRESERVED',a.worktree,len(checked))
