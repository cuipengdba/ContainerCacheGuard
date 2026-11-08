#!/usr/bin/env python3
"""Bounded PostgreSQL 14 write load for the authorized 10.110.32.94 lab.

This is an auxiliary experiment tool, not part of the cache-control daemon.
It creates a separate ccg_loadtest database, preserving clouddb's migrated
business schemas and relation counts.
"""

import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time


ROLES = {
    "dbs": ("/opt/PUC-784/dbspod/postgresql/pgsql", "/opt/PUC-784/dbspod/postgresql/pgdata", 14005),
    "hisdbs": ("/opt/PUC-784/hisdbspod/postgresql/pgsql", "/opt/PUC-784/hisdbspod/postgresql/pgdata", 16911),
}
TEST_DATABASE = 'ccg_loadtest'
stop = False


def request_stop(_sig, _frame):
    global stop
    stop = True


def run(argv, env, timeout):
    return subprocess.run(argv, env=env, text=True, stdout=subprocess.PIPE,
                          stderr=subprocess.STDOUT, timeout=timeout, check=True,
                          cwd="/tmp").stdout


def pg_env(pg):
    env = os.environ.copy()
    env["LD_LIBRARY_PATH"] = str(pg / "lib")
    env["PATH"] = str(pg / "bin") + os.pathsep + env.get("PATH", "")
    return env


def as_postgres(argv):
    return ["setpriv", "--reuid=999", "--regid=998", "--clear-groups", *map(str, argv)]


def sql(pg, sock, port, statement, database=TEST_DATABASE):
    return run(as_postgres([pg / "bin/psql", "-XAtw", "-v", "ON_ERROR_STOP=1",
                            "-h", sock, "-p", port, "-U", "postgres", "-d", database,
                            "-c", statement]), pg_env(pg), 180)


def ensure_database(pg, sock, port):
    found = sql(pg, sock, port,
                f"SELECT 1 FROM pg_database WHERE datname='{TEST_DATABASE}'",
                database='postgres').strip()
    if found != '1':
        sql(pg, sock, port, f'CREATE DATABASE {TEST_DATABASE}', database='postgres')


def prepare(pg, sock, port, state_rows):
    statement = """
    CREATE SCHEMA IF NOT EXISTS ccg_load;
    CREATE TABLE IF NOT EXISTS ccg_load.state (
      id integer PRIMARY KEY, payload text NOT NULL, touched timestamptz NOT NULL DEFAULT now());
    CREATE TABLE IF NOT EXISTS ccg_load.events (
      id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
      key_id integer NOT NULL, payload text NOT NULL, created timestamptz NOT NULL DEFAULT now());
    INSERT INTO ccg_load.state(id,payload)
    SELECT s, '' FROM generate_series(1, %d) s ON CONFLICT (id) DO NOTHING;
    """ % state_rows
    sql(pg, sock, port, statement)


def counters(pg, sock, port):
    query = ("SELECT xact_commit,tup_inserted,tup_updated,"
             f"pg_database_size('{TEST_DATABASE}'),wal_bytes "
             f"FROM pg_stat_database,pg_stat_wal WHERE datname='{TEST_DATABASE}'")
    values = sql(pg, sock, port, query).strip().splitlines()[-1].split('|')
    return dict(zip(('commits', 'inserts', 'updates', 'database_bytes', 'wal_bytes'),
                    map(int, values)))


def script_text(state_rows, inserts, updates, event_bytes, update_bytes):
    # PostgreSQL repeat() yields a predictable byte count; randomness is only
    # for key placement, not a claim that this reproduces application SQL.
    return (f"\\set key random(1, {state_rows - updates})\n"
            "BEGIN;\n"
            f"INSERT INTO ccg_load.events(key_id,payload) "
            f"SELECT :key, repeat('e', {event_bytes}) FROM generate_series(1,{inserts});\n"
            f"UPDATE ccg_load.state SET payload=repeat('u', {update_bytes}), "
            f"touched=clock_timestamp() WHERE id BETWEEN :key AND :key+{updates - 1};\n"
            "COMMIT;\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--role", choices=ROLES, required=True)
    parser.add_argument("--rate", type=int, required=True, help="target committed transactions per second")
    parser.add_argument("--clients", type=int, default=4)
    parser.add_argument("--round-seconds", type=int, default=3600)
    parser.add_argument("--state-rows", type=int, default=100000)
    parser.add_argument("--inserts-per-tx", type=int, default=3)
    parser.add_argument("--updates-per-tx", type=int, default=4)
    parser.add_argument("--event-bytes", type=int, default=256)
    parser.add_argument("--update-bytes", type=int, default=1024)
    parser.add_argument("--report-dir", type=Path, default=Path("/var/tmp/ccg-loadgen"))
    parser.add_argument("--min-free-gib", type=int, default=64,
                        help="pause new rounds below this free space on the PGDATA filesystem")
    parser.add_argument("--target94", action="store_true", help="required explicit target acknowledgement")
    parser.add_argument("--once", action="store_true", help="run one finite round, useful for safe smoke testing")
    args = parser.parse_args()
    addresses = subprocess.run(["ip", "-4", "-o", "addr", "show", "scope", "global"],
                               text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                               check=True).stdout
    if (not args.target94 or not Path("/opt/PUC-784").is_dir()
            or "10.110.32.94/" not in addresses):
        parser.error("run only on the authorized 94 target with --target94")
    if not 1 <= args.rate <= 1000 or not 1 <= args.clients <= 32 or not 60 <= args.round_seconds <= 86400:
        parser.error("rate/clients/round-seconds outside bounded lab range")
    if not 1000 <= args.state_rows <= 1000000 or not 1 <= args.inserts_per_tx <= 20 or not 1 <= args.updates_per_tx <= 20:
        parser.error("state rows or rows-per-transaction outside bounded lab range")
    if not 1 <= args.event_bytes <= 4096 or not 1 <= args.update_bytes <= 8192:
        parser.error("payload bytes outside bounded lab range")
    if not 1 <= args.min_free_gib <= 512:
        parser.error("min-free-gib outside bounded lab range")
    pg_name, sock_name, port = ROLES[args.role]
    pg, sock = Path(pg_name), Path(sock_name)
    if not (pg / "bin/pgbench").is_file() or not sock.is_dir():
        parser.error("PG14 binaries or socket directory missing")
    version = run(as_postgres([pg / "bin/psql", "-XAtw", "-h", sock, "-p", port,
                               "-U", "postgres", "-d", "postgres", "-c", "SHOW server_version_num"]),
                  pg_env(pg), 30).strip().splitlines()[-1]
    if not version.startswith("14"):
        parser.error("target is not PostgreSQL 14")
    args.report_dir.mkdir(parents=True, exist_ok=True)
    os.chmod(args.report_dir, 0o700)
    ensure_database(pg, sock, port)
    prepare(pg, sock, port, args.state_rows)
    signal.signal(signal.SIGTERM, request_stop)
    signal.signal(signal.SIGINT, request_stop)
    with tempfile.TemporaryDirectory(prefix="ccg-pgbench-") as temp:
        os.chown(temp, 999, 998)
        os.chmod(temp, 0o700)
        script = Path(temp) / "load.sql"
        script.write_text(script_text(args.state_rows, args.inserts_per_tx,
                                      args.updates_per_tx, args.event_bytes, args.update_bytes))
        os.chown(script, 999, 998)
        os.chmod(script, 0o600)
        while not stop:
            free = shutil.disk_usage(sock).free
            if free < args.min_free_gib * (1 << 30):
                print(json.dumps({"role": args.role, "state": "paused_low_disk",
                                  "free_bytes": free, "minimum_gib": args.min_free_gib}), flush=True)
                time.sleep(60)
                continue
            before = counters(pg, sock, port)
            cmd = as_postgres([pg / "bin/pgbench", "-n", "-c", args.clients,
                               "-j", min(args.clients, 4), "-R", args.rate,
                               "-T", args.round_seconds, "-P", 60, "-f", script,
                               "-h", sock, "-p", port, "-U", "postgres", TEST_DATABASE])
            started = time.time()
            process = subprocess.Popen(cmd, env=pg_env(pg), stdout=subprocess.PIPE,
                                       stderr=subprocess.STDOUT, text=True, cwd="/tmp")
            log_path = args.report_dir / f"{args.role}-{int(started)}.log"
            with log_path.open("w") as log:
                while process.poll() is None:
                    line = process.stdout.readline()
                    if line:
                        log.write(line)
                        log.flush()
                    if stop:
                        process.terminate()
                remainder = process.stdout.read()
                if remainder:
                    log.write(remainder)
            result = {"role": args.role, "started_epoch": started,
                      "ended_epoch": time.time(), "exit_code": process.wait(),
                      "target_tps": args.rate, "log": str(log_path),
                      "before": before, "after": counters(pg, sock, port)}
            print(json.dumps(result), flush=True)
            if stop:
                break
            if args.once:
                break
            if result["exit_code"] != 0:
                time.sleep(30)
    return 0


if __name__ == "__main__":
    sys.exit(main())
