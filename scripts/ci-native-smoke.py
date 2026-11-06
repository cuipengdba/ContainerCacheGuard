#!/usr/bin/env python3
"""Native Linux CI correctness smoke, using synthetic data in a fresh directory.

The source server is built from the same SHA256-pinned PG18 archive as the
embedded clients. No system PostgreSQL service or existing data is touched.
This proves small-scale correctness on the runner, not production performance.
"""
import argparse
import hashlib
import json
import os
import pathlib
import platform
import shlex
import subprocess
import tarfile
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--binary', required=True)
p.add_argument('--source-archive', required=True)
p.add_argument('--work', required=True)
p.add_argument('--report-dir', required=True)
a = p.parse_args()
work = pathlib.Path(a.work).resolve()
reports = pathlib.Path(a.report_dir).resolve()
binary = pathlib.Path(a.binary).resolve()
archive = pathlib.Path(a.source_archive).resolve()
if os.geteuid() == 0 or platform.system() != 'Linux' or work.exists() or reports.exists():
    p.error('Use a non-root Linux account and new work/report directories')
work.mkdir(mode=0o700)
reports.mkdir(parents=True, mode=0o700)
report = dict(success=False, architecture=platform.machine(), page_size=os.sysconf('SC_PAGE_SIZE'),
              binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(), tests=[],
              scope='native CPU, synthetic correctness smoke; not large-data or Kubernetes acceptance')
started = []
env = {k: v for k, v in os.environ.items() if not k.startswith('PG')}
env['LC_ALL'] = 'C'


def run(args, expect_success=True, timeout=1200, cwd=None):
    args = list(map(str, args))
    before = time.monotonic()
    result = subprocess.run(args, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
    with (reports / 'commands.log').open('a') as out:
        out.write(shlex.join(args) + '\n' + result.stdout + result.stderr + '\n')
    if (result.returncode == 0) != expect_success:
        raise RuntimeError('Unexpected exit ' + str(result.returncode) + '\n' +
                           (result.stdout + result.stderr)[-12000:])
    return result.stdout.strip(), time.monotonic() - before


try:
    manifest = json.loads((pathlib.Path(__file__).parent / 'sources.json').read_text())
    expected = next(x for x in manifest if x['name'] == archive.name)
    if hashlib.sha256(archive.read_bytes()).hexdigest() != expected['sha256']:
        raise RuntimeError('Server source SHA256 mismatch')
    with tarfile.open(archive) as source:
        source.extractall(work, filter='data')
    source = work / 'postgresql-18.6'
    pg = work / 'server'
    run(['./configure', '--prefix=' + str(pg), '--without-readline', '--without-icu',
         '--without-zlib', '--without-lz4', '--without-zstd', '--without-ssl',
         '--without-libxml', '--without-libxslt', '--disable-nls'], cwd=source)
    run(['make', '-j2'], cwd=source)
    run(['make', 'install'], cwd=source)
    data, socket, repo = work / 'data', work / 'socket', work / 'repo'
    socket.mkdir()
    repo.mkdir()
    # Unix socket only. Ports identify socket names; no TCP listener is opened.
    port = 25452
    config = work / 'guard.json'
    config.write_text(json.dumps(dict(
        backend='native', pgdata=str(data), repository=str(repo), pg_host=str(socket),
        pg_port=port, pg_user='postgres', runtime_dir=str(work / 'run'),
        lock_files=[str(work / 'device.lock')], read_bytes_per_second=32 << 20,
        write_bytes_per_second=32 << 20, flush_bytes=8 << 20, read_window_bytes=8 << 20,
        rate_burst_bytes=1 << 20, bitmap_budget_bytes=64 << 20,
        min_node_available_bytes=128 << 20, warn_node_available_bytes=256 << 20,
        deadline_seconds=600, poll_ms=250), indent=2))

    def sql(statement, dbport=port):
        return run([pg / 'bin/psql', '-XAtw', '-h', socket, '-p', dbport, '-U', 'postgres',
                    '-d', 'postgres', '-v', 'ON_ERROR_STOP=1', '-c', statement])[0]

    def start(path, dbport):
        run([pg / 'bin/pg_ctl', '-D', path, '-l', reports / ('server-' + str(dbport) + '.log'),
             '-o', "-c listen_addresses='' -k " + str(socket) + ' -p ' + str(dbport), '-w', 'start'])
        started.append(path)

    def stop(path):
        run([pg / 'bin/pg_ctl', '-D', path, '-m', 'fast', '-w', 'stop'])
        started.remove(path)

    def tool(*args, expect_success=True):
        _, seconds = run([binary, '-config', config, *args], expect_success=expect_success)
        report['tests'].append(dict(command=list(map(str, args)), seconds=seconds,
                                    expected_success=expect_success))

    def contents(dbport=port):
        # Deterministic ordered payload digest as well as rows and schema counts.
        return sql("SELECT count(*),sum(id),md5(string_agg(payload,',' ORDER BY id)) FROM proof; "
                   "SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'business_%'; "
                   "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace "
                   "WHERE n.nspname LIKE 'business_%' AND c.relkind='r';", dbport)

    run([binary, '--version'])
    for command in ['basebackup', 'verifybackup', 'combinebackup']:
        run([binary, command, '--version'])
        run([binary, command, '--help'])
    run([pg / 'bin/initdb', '-D', data, '-U', 'postgres', '-A', 'trust', '--locale=C', '--data-checksums'])
    with (data / 'postgresql.conf').open('a') as out:
        out.write("\nshared_buffers='32MB'\nmax_connections=10\nwal_level=replica\n"
                  "max_wal_senders=4\nsummarize_wal=on\nwal_summary_keep_time='2d'\n")
    start(data, port)
    sql("CREATE TABLE proof AS SELECT g AS id,repeat(md5(g::text),4) AS payload "
        "FROM generate_series(1,100000) g; DO $$BEGIN FOR i IN 1..100 LOOP "
        "EXECUTE format('CREATE SCHEMA business_%s',i); "
        "EXECUTE format('CREATE TABLE business_%s.t AS SELECT generate_series(1,100) AS id',i); "
        "END LOOP; END$$; CHECKPOINT;")
    backups, expected = [], []
    for generation in range(3):
        if generation:
            low = 100001 + (generation - 1) * 100
            sql(f"INSERT INTO proof SELECT g,md5(g::text) FROM generate_series({low},{low + 99}) g; "
                f"UPDATE proof SET payload='changed-'||id WHERE id BETWEEN {generation * 10} "
                f"AND {generation * 10 + 10}; CHECKPOINT; SELECT pg_switch_wal();")
        expected.append(contents())
        dest = repo / ('full' if generation == 0 else 'incr' + str(generation))
        args = ['basebackup', '-D', dest, '-Fp', '-Xstream', '--checkpoint=fast',
                '--max-rate=32M', '--manifest-checksums=SHA256', '-w']
        if generation:
            args.append('--incremental=' + str(backups[-1] / 'backup_manifest'))
        tool(*args)
        backups.append(dest)
    for generation in range(3):
        restored = work / ('restored-' + str(generation))
        tool('combinebackup', '-o', restored, *backups[:generation + 1])
        restored_port = port + generation + 1
        start(restored, restored_port)
        actual = contents(restored_port)
        if actual != expected[generation]:
            raise RuntimeError('Restored rows or payload differ: generation ' + str(generation))
        stop(restored)
        report['tests'].append(dict(test='actual-startup-and-content-match', generation=generation,
                                    result=actual))
    tool('combinebackup', '-o', data, backups[0], expect_success=False)
    tool('combinebackup', '-o', work / 'missing-parent', backups[2], expect_success=False)
    tool('verifybackup', '--skip-checksums', '--no-parse-wal', backups[0])
    victims = [x for x in (backups[2] / 'base').rglob('INCREMENTAL.*') if x.stat().st_size > 8192]
    if not victims:
        raise RuntimeError('No real incremental file found for corruption test')
    victim = victims[0]
    with victim.open('r+b') as out:
        out.seek(-1, 2)
        value = out.read(1)
        out.seek(-1, 2)
        out.write(bytes([value[0] ^ 255]))
        out.flush()
        os.fsync(out.fileno())
    tool('verifybackup', backups[2], expect_success=False)
    tool('verifybackup', '--skip-checksums', '--no-parse-wal', backups[2], expect_success=False)
    report['tests'].append(dict(test='corruption-refused-even-with-weak-flags'))
    report['success'] = True
finally:
    for path in reversed(started):
        result = subprocess.run([str(pg / 'bin/pg_ctl'), '-D', str(path), '-m', 'fast', '-w', 'stop'],
                                env=env, capture_output=True, text=True, timeout=60)
        if result.returncode:
            report['success'] = False
            report.setdefault('cleanup_errors', []).append(result.stderr)
    (reports / 'results.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2), flush=True)
