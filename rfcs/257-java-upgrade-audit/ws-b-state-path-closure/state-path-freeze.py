import hashlib,json,subprocess,sys
from pathlib import Path
manifest=Path('/var/tmp/fdb-upgrade-recovery/ws-b/state-path-tree-freeze.json')
names=sorted(set(subprocess.check_output(['git','ls-files','-co','--exclude-standard','-z']).decode().split('\0'))-{''})
current={n:hashlib.sha256(Path(n).read_bytes()).hexdigest() for n in names if Path(n).is_file()}
assert len(current)>4500
if sys.argv[1]=='save':
 manifest.write_text(json.dumps(current,indent=2)+'\n')
 print('Frozen',len(current),'tracked/untracked nonignored files')
else:
 old=json.loads(manifest.read_text())
 changed=[n for n in old.keys()|current.keys() if old.get(n)!=current.get(n)]
 assert not changed, changed
 print('Verified unchanged',len(current),'tracked/untracked nonignored files')
