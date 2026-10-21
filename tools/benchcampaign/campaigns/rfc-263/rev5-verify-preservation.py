import hashlib, json, os, pathlib, subprocess, sys, tarfile
P=pathlib.Path(sys.argv[1] if len(sys.argv)>1 else '/tmp/rfc263-rev5.QgGUbU'); W=P/'scratch'
checked=[]
with tarfile.open(P/'rev5-source.tar.gz','r:gz') as t:
    for m in t:
        p=W/m.name
        if m.isfile():
            archived=t.extractfile(m).read(); live=p.read_bytes()
            assert archived==live, m.name
            assert bool(m.mode & 0o111)==bool(p.stat().st_mode & 0o111), m.name
            checked.append({'path':m.name,'sha256':hashlib.sha256(live).hexdigest()})
        elif m.issym():
            assert os.readlink(p)==m.linkname, m.name
            checked.append({'path':m.name,'symlink':m.linkname})
        else: assert m.isdir(), (m.name,m.type)
subprocess.run(['sha256sum','-c',str(P/'rev5-inputs.sha256')],cwd=W,check=True)
status=subprocess.check_output(['git','status','--short','--untracked-files=all'],cwd=W,text=True)
(P/'worktree-preservation-verification.json').write_text(json.dumps({'tree':(P/'rev5-tree.txt').read_text().strip(),'source_archive_sha256':hashlib.sha256((P/'rev5-source.tar.gz').read_bytes()).hexdigest(),'entries':len(checked),'status':status,'files':checked},indent=2)+'\n')
print('PRESERVATION_VERIFIED',len(checked))
