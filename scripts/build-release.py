#!/usr/bin/env python3
"""Native Linux self-contained build; never install files into the host OS."""
import argparse, hashlib, json, os, pathlib, platform, shutil, subprocess, sys, urllib.request

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--build-root', required=True, help='New private build directory; at least 3 GiB free')
p.add_argument('--source-cache', help='Optional directory containing pinned upstream tarballs')
p.add_argument('--offline', action='store_true', help='Never download; all source archives must be cached')
a = p.parse_args()

def sha256(path):
    digest = hashlib.sha256()
    with path.open('rb') as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b''):
            digest.update(chunk)
    return digest.hexdigest()

project = pathlib.Path(__file__).resolve().parents[1]
root = pathlib.Path(a.build_root).resolve()
arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine())
if platform.system() != 'Linux' or not arch:
    p.error('Run natively on Linux x86_64 or aarch64; cross-build is a separate recipe')
if root == project or project.is_relative_to(root) or root.is_relative_to(project):
    p.error('build root must be separate from the project checkout')
if root.exists() and any(root.iterdir()):
    p.error('Use a new empty build root; failed build trees are never erased')
root.mkdir(parents=True, exist_ok=True)
sources = root / 'sources'; sources.mkdir(exist_ok=True)
manifest = json.loads((project / 'scripts/sources.json').read_text())
for entry in manifest:
    target = sources / entry['name']
    cached = pathlib.Path(a.source_cache).resolve() / entry['name'] if a.source_cache else None
    if cached and cached.is_file():
        shutil.copy2(cached, target)
    else:
        if a.offline:
            raise RuntimeError('Missing cached source: ' + entry['name'])
        print('Downloading', entry['name'], flush=True)
        with urllib.request.urlopen(entry['url'], timeout=120) as response, target.open('xb') as out:
            shutil.copyfileobj(response, out)
    digest = sha256(target)
    if digest != entry['sha256']:
        raise RuntimeError('SHA256 mismatch: ' + entry['name'])
env = {**os.environ, 'PGCACHEGUARD_BUILD_ROOT': str(root), 'PGCACHEGUARD_PROJECT_ROOT': str(project)}
(project / 'dist').mkdir(exist_ok=True)
assets = project / 'internal/native/assets'; assets.mkdir(exist_ok=True)
for script in ['build-static-bootstrap.py', 'build-static-pg18.py']:
    subprocess.run([sys.executable, str(project / 'scripts' / script)], env=env, check=True)
shutil.copy2(root / ('tools-linux-' + arch + '.tar.gz'), assets)
if arch == 'amd64':
    # Optional external-worker development backend; the bundled backend does not load this .so.
    subprocess.run(['cc', '-O2', '-Wall', '-Wextra', '-Werror', '-fPIC', '-shared', '-pthread',
                    str(project / 'native/guard.c'), '-o', str(assets / 'libpg18guard.so')], check=True)
binary = project / 'dist' / ('pgcacheguard-linux-' + arch)
subprocess.run(['go', 'build', '-trimpath', '-ldflags=-s -w', '-o', str(binary), '.'],
               cwd=project, env={**env, 'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOARCH': arch}, check=True)
program = subprocess.check_output(['readelf', '-l', str(binary)], text=True)
dynamic = subprocess.check_output(['readelf', '-d', str(binary)], text=True)
if 'INTERP' in program or '(NEEDED)' in dynamic:
    raise RuntimeError('Controller binary is not fully static')
digest = sha256(binary)
(binary.parent / (binary.name + '.sha256')).write_text(digest + '  ' + binary.name + '\n')
shutil.copy2(root / 'pg18/build-results.json', binary.parent / ('native-build-' + arch + '.json'))
print('Built', binary, 'SHA256', digest, flush=True)
