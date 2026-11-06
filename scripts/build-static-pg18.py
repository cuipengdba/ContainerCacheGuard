#!/usr/bin/env python3
"""Build private, fully static PG18 clients with directly linked I/O adapters.

This target94 development recipe never changes /usr or installs host packages.
Release automation will use the same pinned source inputs on both architectures.
"""
import hashlib,json,os,pathlib,platform,shutil,subprocess,tarfile
s=pathlib.Path(os.environ.get('PGCACHEGUARD_BUILD_ROOT','/var/tmp/pg18guard-dev-20261105/static-build')).resolve()
source=s/'sources';deps=s/'deps';cc=s/'toolchain/bin/musl-gcc'
project=pathlib.Path(os.environ.get('PGCACHEGUARD_PROJECT_ROOT',str(s.parent/'src'))).resolve();output=s/'pg18/bin';output.mkdir(parents=True,exist_ok=True)
arch={'x86_64':'amd64','aarch64':'arm64'}.get(platform.machine())
if not arch:raise RuntimeError('native Linux amd64 or arm64 build required')
headers=s/'kernel-headers';headers.mkdir(exist_ok=True)
for name in ['linux','asm','asm-generic']:
    if not (headers/name).exists():
        candidates=[pathlib.Path('/usr/include')/name]
        multiarch=subprocess.check_output(['gcc','-print-multiarch'],text=True).strip()
        if multiarch:candidates.append(pathlib.Path('/usr/include')/multiarch/name)
        origin=next((p for p in candidates if p.is_dir()),None)
        if not origin:raise RuntimeError('Linux development headers missing: '+name)
        shutil.copytree(origin,headers/name,symlinks=True)
env={**os.environ,'CC':str(cc),'CFLAGS':'-O2 -fPIC',
     'CPPFLAGS':'-I'+str(deps/'include')+' -I'+str(headers),
     'LDFLAGS':'-static -L'+str(deps/'lib'),
     'PKG_CONFIG_PATH':str(deps/'lib/pkgconfig'),
     # These static-only source builds do not install pkg-config metadata.
     # Supplying explicit flags also prevents host shared libraries being used.
     'LZ4_CFLAGS':'-I'+str(deps/'include'),
     'LZ4_LIBS':'-L'+str(deps/'lib')+' -llz4',
     'ZSTD_CFLAGS':'-I'+str(deps/'include'),
     'ZSTD_LIBS':'-L'+str(deps/'lib')+' -lzstd',
     'PERL5LIB':':'.join(str(s/p) for p in ['perl-root/usr/lib64/perl5/vendor_perl',
          'perl-root/usr/lib64/perl5','perl-root/usr/share/perl5/vendor_perl','perl-root/usr/share/perl5'])}
def run(args,cwd,timeout=1200):
    print('BUILD',' '.join(map(str,args)),flush=True)
    subprocess.run(list(map(str,args)),cwd=cwd,env=env,check=True,timeout=timeout)
def extract(name,digest):
    p=source/name
    if hashlib.sha256(p.read_bytes()).hexdigest()!=digest:raise RuntimeError('source checksum mismatch: '+name)
    root=name.split('.tar.')[0];folder=s/root
    if not folder.exists():
        with tarfile.open(p) as t:
            for e in t:
                parts=pathlib.PurePosixPath(e.name).parts
                if not parts or parts[0]!=root or '..' in parts or e.islnk():raise RuntimeError('unsafe source member: '+e.name)
                if e.issym():
                    resolved=os.path.normpath(str(pathlib.PurePosixPath(e.name).parent/e.linkname))
                    if not resolved.startswith(root+'/'):raise RuntimeError('source link escapes archive root')
            t.extractall(s)
    return folder
openssl=extract('openssl-3.5.9.tar.gz','603f5602e2eef00d77fbd429d34dcd5822bb301757a1bc9cdb24c670f1eb859a')
if not all((deps/'lib'/p).exists() for p in ['libssl.a','libcrypto.a']):
    run(['./Configure','linux-x86_64' if arch=='amd64' else 'linux-aarch64','no-shared','no-tests','no-module','no-dso',
         '--prefix='+str(deps),'--openssldir=/etc/ssl','--libdir=lib'],openssl)
    run(['make','-j2','build_libs'],openssl)
    run(['make','install_dev'],openssl)
pg=extract('postgresql-18.6.tar.bz2','555610c24d53e4316da5b7d3fc25c279d96856d5e0e23ee308c328c5fa881d9f')
# The upstream build normally makes shared libpq as well. This bundle only
# links libpq.a, so suppress that unused shared target in this private tree.
makefile=pg/'src/Makefile.shlib';text=makefile.read_text()
text=text.replace('all-lib: all-shared-lib\n','all-lib: all-static-lib\n',1);makefile.write_text(text)
pqmake=pg/'src/interfaces/libpq/Makefile';text=pqmake.read_text()
text=text.replace('libpq-refs-stamp: $(shlib)','libpq-refs-stamp: $(stlib)',1)
pqmake.write_text(text)
base=pg/'src/bin/pg_basebackup/pg_basebackup.c';text=base.read_text()
if '#include <sys/socket.h>' not in text:
    text=text.replace('#include <sys/stat.h>','#include <sys/stat.h>\n#include <sys/socket.h>',1)
if '#include "pgcacheguard-identity.inc"' not in text:
    text=text.replace('\tatexit(disconnect_atexit);','\tatexit(disconnect_atexit);\n#include "pgcacheguard-identity.inc"',1)
    if '#include "pgcacheguard-identity.inc"' not in text:raise RuntimeError('upstream connection marker changed')
base.write_text(text)
shutil.copy2(project/'native/basebackup-identity.inc',base.parent/'pgcacheguard-identity.inc')
run(['./configure','--prefix='+str(s/'pg18'),'--without-readline','--without-icu',
     '--with-ssl=openssl','--with-lz4','--with-zstd','--without-libxml',
     '--without-libxslt','--disable-rpath','--disable-nls'],pg)
run(['make','-j2','-C','src/backend','generated-headers'],pg)
guard=project/'dist/guard-static.o'
run([cc,'-O2','-Wall','-Wextra','-Werror','-pthread','-DPGCACHEGUARD_STATIC','-c',
     project/'native/guard.c','-o',guard],project)
link=(str(guard)+' -static -pthread -Wl,--strip-all,--wrap=fread,--wrap=fwrite,--wrap=fflush,--wrap=fclose'
      +' '+str(pg/'src/common/encnames_shlib.o'))
report=[]
for tool in ['pg_basebackup','pg_verifybackup','pg_combinebackup','pg_waldump','psql']:
    built=pg/'src/bin'/tool/tool
    built.unlink(missing_ok=True) # force relink of the separately supplied guard object
    # Building an individual bin subtree can recurse into the same archive
    # from both libpq and frontend-utils. Serialize that small dependency DAG.
    run(['make','-j1','-C','src/bin/'+tool,tool,'LDFLAGS_EX='+link],pg)
    dynamic=subprocess.run(['readelf','-d',str(built)],capture_output=True,text=True,check=True).stdout
    program=subprocess.run(['readelf','-l',str(built)],capture_output=True,text=True,check=True).stdout
    if '(NEEDED)' in dynamic or 'INTERP' in program:raise RuntimeError('binary is not fully static: '+tool)
    shutil.copy2(built,output/tool)
    version=subprocess.check_output([str(output/tool),'--version'],text=True).strip()
    report.append(dict(tool=tool,version=version,static=True,size=(output/tool).stat().st_size,
                       sha256=hashlib.sha256((output/tool).read_bytes()).hexdigest()))
    print(json.dumps(report[-1]),flush=True)
(s/'pg18/build-results.json').write_text(json.dumps(dict(success=True,tools=report,
    features=dict(ssl=True,gzip=True,lz4=True,zstd=True,gssapi=False,ldap=False)),indent=2)+'\n')
with tarfile.open(s/('tools-linux-'+arch+'.tar.gz'),'w:gz') as t:
    for x in report:t.add(output/x['tool'],arcname=x['tool'],recursive=False)
print('STATIC_PG18_BUILD_PASSED',flush=True)
