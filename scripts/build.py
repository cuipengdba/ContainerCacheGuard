#!/usr/bin/env python3
"""Static Linux builds with only the standard Go toolchain."""
import argparse, hashlib, os, pathlib, subprocess
root=pathlib.Path(__file__).resolve().parents[1]
default_version=(root/'VERSION').read_text().strip() if (root/'VERSION').exists() else 'dev'
p=argparse.ArgumentParser();p.add_argument('--version',default=default_version);p.add_argument('--commit',default='unknown');p.add_argument('--go',default='go');a=p.parse_args()
dist=root/'dist';dist.mkdir(exist_ok=True)
if any(c.isspace() or c in "'\"" for c in a.version+a.commit):raise SystemExit('version/commit must not contain whitespace or quotes')
for arch in ['amd64','arm64']:
    env=dict(os.environ,GOOS='linux',GOARCH=arch,CGO_ENABLED='0')
    subprocess.run([a.go,'build','-trimpath','-ldflags',f'-s -w -X main.version={a.version} -X main.commit={a.commit}','-o',str(dist/f'containercacheguard-linux-{arch}'),'.'],cwd=root,env=env,check=True)
paths=[dist/f'containercacheguard-linux-{a}' for a in ['amd64','arm64']]
(dist/'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.name+'\n' for p in paths))
print(dist)
