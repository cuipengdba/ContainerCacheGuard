#!/usr/bin/env python3
"""Build private musl/compression dependencies. Never install into the host OS."""
import hashlib
import json
import os
import pathlib
import subprocess
import tarfile

s = pathlib.Path(os.environ.get('PGCACHEGUARD_BUILD_ROOT', '/var/tmp/pg18guard-dev-20261105/static-build')).resolve()
src = s / 'sources'
prefix = s / 'toolchain'
dependencies = s / 'deps'

def run(args, cwd, env=None):
    print('BUILD', ' '.join(map(str, args)), flush=True)
    subprocess.run(list(map(str, args)), cwd=cwd, env=env, check=True)

if (src / 'bootstrap-sources.tar').exists():
    with tarfile.open(src / 'bootstrap-sources.tar') as archive:
        for entry in archive:
            if not entry.isfile() or '/' in entry.name or '..' in entry.name:
                raise RuntimeError('unexpected source bundle member')
        archive.extractall(src)
expected = {
    'musl-1.2.6': 'd585fd3b613c66151fc3249e8ed44f77020cb5e6c1e635a616d3f9f82460512a',
    'zlib-1.4': '02dd159783677a749c04adbdd02b55b8a2bcc7e054f51b76b549199a916ad584',
    'lz4-1.11.0': '247e5fa64da5f7b75ee3dd3bed484076396c9ebe8254d9ccf693eb62800c5e26',
    'zstd-1.5.7': '37d7284556b20954e56e1ca85b80226768902e2edabd3b649e9e72c0c9012ee3',
}
report = []
for name in ['musl-1.2.6', 'zlib-1.4', 'lz4-1.11.0', 'zstd-1.5.7']:
    archive = src / (name + '.tar.gz')
    if hashlib.sha256(archive.read_bytes()).hexdigest() != expected[name]:
        raise RuntimeError('source SHA256 mismatch: ' + name)
    report.append(dict(name=name, sha256=hashlib.sha256(archive.read_bytes()).hexdigest()))
    target = s / name
    if not target.exists():
        with tarfile.open(archive) as t:
            for e in t:
                parts = pathlib.PurePosixPath(e.name).parts
                if not parts or parts[0] != name or '..' in parts or e.islnk():
                    raise RuntimeError('unexpected upstream archive layout: ' + e.name)
                if e.issym():
                    resolved = os.path.normpath(str(pathlib.PurePosixPath(e.name).parent / e.linkname))
                    if not resolved.startswith(name + '/'):
                        raise RuntimeError('source link escapes archive root')
            t.extractall(s)
(s / 'bootstrap-inputs.json').write_text(json.dumps(report, indent=2)+'\n')
run(['./configure', '--prefix=' + str(prefix), '--disable-shared'], s / 'musl-1.2.6')
run(['make', '-j2'], s / 'musl-1.2.6')
run(['make', 'install'], s / 'musl-1.2.6')
env = {**os.environ, 'CC': str(prefix / 'bin/musl-gcc'),
       'CFLAGS': '-O2 -fPIC', 'LDFLAGS': '-static'}
run(['./configure', '--static', '--prefix=' + str(dependencies)], s / 'zlib-1.4', env)
run(['make', '-j2'], s / 'zlib-1.4', env)
run(['make', 'install'], s / 'zlib-1.4', env)
run(['make', '-j2', 'liblz4.a', 'CC=' + env['CC']], s / 'lz4-1.11.0/lib', env)
(dependencies / 'lib').mkdir(parents=True, exist_ok=True)
(dependencies / 'include').mkdir(parents=True, exist_ok=True)
import shutil
shutil.copy2(s / 'lz4-1.11.0/lib/liblz4.a', dependencies / 'lib/liblz4.a')
for h in (s / 'lz4-1.11.0/lib').glob('*.h'):
    shutil.copy2(h, dependencies / 'include' / h.name)
run(['make', '-j2', 'libzstd.a', 'CC=' + env['CC'], 'ZSTD_LEGACY_SUPPORT=0'], s / 'zstd-1.5.7/lib', env)
shutil.copy2(s / 'zstd-1.5.7/lib/libzstd.a', dependencies / 'lib/libzstd.a')
for h in (s / 'zstd-1.5.7/lib').glob('*.h'):
    shutil.copy2(h, dependencies / 'include' / h.name)
print('STATIC_BOOTSTRAP_PASSED', flush=True)
