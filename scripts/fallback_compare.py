#!/usr/bin/env python3
"""Three sequential comparisons in new cgroup-v2 fixtures on a disposable Linux runner."""

import argparse
import ctypes
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import mmap
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
import uuid

MiB = 1 << 20
GiB = 1 << 30
PAGE = os.sysconf("SC_PAGE_SIZE")
LIBC = ctypes.CDLL(None, use_errno=True)
LIBC.mincore.argtypes = (ctypes.c_void_p, ctypes.c_size_t, ctypes.c_void_p)


def data_values(path):
    return {key: int(value) for key, value in (line.split() for line in path.read_text().splitlines())}


def io_values(path):
    totals = {"rbytes": 0, "wbytes": 0, "rios": 0, "wios": 0}
    for line in path.read_text().splitlines():
        for field in line.split()[1:]:
            key, value = field.split("=")
            if key in totals:
                totals[key] += int(value)
    return totals


def resident(path):
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOATIME), "rb") as handle, mmap.mmap(handle.fileno(), 0, access=mmap.ACCESS_COPY) as mapping:
        sentinel = ctypes.c_char.from_buffer(mapping)
        pages = (len(mapping) + PAGE - 1) // PAGE
        vec = (ctypes.c_ubyte * pages)()
        if LIBC.mincore(ctypes.addressof(sentinel), len(mapping), vec) != 0:
            raise OSError(ctypes.get_errno(), "mincore")
        del sentinel
        return sum(value & 1 for value in vec) * PAGE


def fixture_file(path, size_mib):
    block = os.urandom(MiB)
    with path.open("wb", buffering=0) as handle:
        for _ in range(size_mib):
            handle.write(block)
        os.fsync(handle.fileno())
        os.posix_fadvise(handle.fileno(), 0, 0, os.POSIX_FADV_DONTNEED)


def join(cgroup):
    (Path(cgroup) / "cgroup.procs").write_text(str(os.getpid()))


def helper(mode, cg, target="", marker=""):
    join(cg)
    if mode == "exec":
        os.execv(target, [target, *sys.argv[4:]])
    if mode == "fill":
        with open(target, "rb", buffering=0) as handle:
            time.sleep(2)
            while handle.read(4 * MiB):
                pass
            Path(marker).touch()
            time.sleep(2)
    elif mode == "hot":
        with open(target, "r+b", buffering=0) as handle:
            while True:
                handle.seek(0)
                handle.read(4 * MiB)
                handle.seek(0)
                handle.write(b"hot")
                time.sleep(.12)
    elif mode == "pressure":
        memory = bytearray(int(target) * MiB)
        for offset in range(0, len(memory), PAGE):
            memory[offset] = 1
        Path(marker).touch()
        time.sleep(108)


def p99(values):
    values = sorted(values)
    return round(values[min(len(values) - 1, int(.99 * len(values)))], 3) if values else None


def command(*argv, **kwargs):
    return subprocess.run(list(map(str, argv)), check=True, text=True, capture_output=True, **kwargs).stdout


class Probe:
    def __init__(self):
        self.stop = threading.Event()
        self.sql, self.api, self.errors = [], [], []
        probe = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                tick = time.monotonic()
                try:
                    result = command("psql", "-XAt", "-v", "ON_ERROR_STOP=1", "-c",
                                     "UPDATE fallback_probe SET n=n+1 WHERE id=1 RETURNING id", timeout=5)
                    assert result.splitlines()[0] == "1"
                    probe.sql.append((time.monotonic() - tick) * 1000)
                    self.send_response(200)
                except Exception as exc:
                    probe.errors.append(type(exc).__name__)
                    self.send_response(500)
                self.end_headers()

            def log_message(self, *_):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.http = threading.Thread(target=self.server.serve_forever)
        self.http.start()
        self.client = threading.Thread(target=self.sample)
        self.client.start()

    def sample(self):
        while not self.stop.is_set():
            start = time.monotonic()
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{self.server.server_port}/", timeout=6):
                    self.api.append((time.monotonic() - start) * 1000)
            except Exception as exc:
                self.errors.append(type(exc).__name__)
            self.stop.wait(.25)

    def finish(self):
        self.stop.set()
        self.client.join(8)
        self.server.shutdown()
        self.server.server_close()
        self.http.join(8)
        return {"samples": len(self.api), "errors": self.errors, "sql_p99_ms": p99(self.sql), "api_p99_ms": p99(self.api)}


def stop_children(children):
    for proc in children:
        if proc.poll() is None:
            proc.terminate()
    for proc in children:
        try:
            proc.wait(10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(5)


def trial(name, binary, vmtouch, base, result):
    area = base / name
    area.mkdir()
    pgdata, backup, scratch = (area / part for part in ("pgdata", "backup", "scratch"))
    for path in (pgdata, backup, scratch):
        path.mkdir()
    closed, fixed, hot = scratch / "ephemeral-path-data", backup / "fixed-backup-file", scratch / "hot-file"
    # Directory pressure is present in both categories and is identical per arm.
    for root in (backup, scratch):
        crowded = root / "large"
        crowded.mkdir()
        for n in range(2500):
            (crowded / str(n)).touch()
    for path, size in ((closed, 1152), (fixed, 128), (hot, 32)):
        fixture_file(path, size)
    # The hot workload deliberately writes repeatedly into an open file.
    children = []
    parent = Path("/sys/fs/cgroup") / ("ccg-fallback-" + uuid.uuid4().hex)
    parent.mkdir()
    leaf, sibling = parent / "target", parent / "sibling"
    log = (area / "console.log").open("w")
    backup_log_path = area / "backup.log"
    backup_log = backup_log_path.open("w")
    probe = None
    try:
        (parent / "cgroup.subtree_control").write_text("+memory +io")
        for cg in (leaf, sibling):
            cg.mkdir()
            (cg / "memory.swap.max").write_text("0")
        (parent / "memory.max").write_text(str(2700 * MiB))
        state = area / "state"
        cfg = json.loads(command(binary, "init-config", "--mode", "paths", "--path", pgdata, "--path", backup))
        cfg.update(action="enforce" if name != "legacy" else "observe", cgroup_path=str(leaf), state_file=str(state / "status.json"),
                   log_directory=str(state / "log"), interval_ms=200, log_interval_seconds=1, warmup_seconds=2,
                   recovery_healthy_seconds=0, protect_seconds=5, min_file_age_seconds=5,
                   recheck_seconds=2, extended_recheck_seconds=[8, 30], rescan_seconds=3600,
                   cache_target_bytes=128 * MiB, initial_reclaim_bytes_per_second=64 * MiB,
                   reclaim_bytes_per_second=64 * MiB, scan_bytes_per_second=768 * MiB, worker_budget_ms=100,
                   max_cpu_percent=50, max_dirty_bytes=128 * MiB, max_io_psi_avg10=90)
        if name == "fallback":
            cfg["cache_fallback"] = dict(enabled=True, pgdata=str(pgdata), backup_directories=[str(backup)],
                                         auto_directories=[str(scratch)], auto_action="enforce", interval_seconds=30,
                                         parent_headroom_bytes=550 * MiB,
                                         round_entries_per_scope=8192, round_work_ms_per_scope=1500,
                                         round_advice_bytes_per_scope=1280 * MiB, expected_memory_high_bytes=2560 * MiB)
        config = area / "config.json"
        config.write_text(json.dumps(cfg))
        oom_before = data_values(leaf / "memory.events")
        io_before = io_values(leaf / "io.stat")

        def spawn(*arguments, output=log):
            proc = subprocess.Popen([sys.executable, __file__, *map(str, arguments)], stdout=output, stderr=output)
            children.append(proc)
            return proc

        ccg = spawn("exec", leaf, binary, "run", "--config", config)
        ready = area / "filled"
        filler = spawn("fill", leaf, closed, ready)
        fixed_fill = spawn("fill", leaf, fixed, area / "fixed-ready")
        spawn("hot", leaf, hot)
        probe = Probe()
        start = time.monotonic()
        snapshots = result["series"] = []
        finding = None
        first_advice = None
        first_drop = None
        source_seen = set()
        awaiting_fill = True
        legacy = None
        legacy_at = None
        pressure = None
        backup_job = None
        sql_backup = backup / "concurrent-pg"
        max_caps = {"fixed": 0, "auto": 0}
        available_at_drop = None
        reads_at_close = None
        report_rounds = []
        last_round = 0
        while (elapsed := time.monotonic() - start) < 88:
            assert ccg.poll() is None, (area / "console.log").read_text()[-4000:]
            if elapsed >= 19 and pressure is None:
                pressure = spawn("pressure", sibling, 1024, area / "pressure-ready")
            if elapsed >= 27 and backup_job is None:
                backup_job = spawn("exec", leaf, shutil.which("pg_basebackup"), "-D", sql_backup, "-X", "stream",
                                   "--checkpoint=fast", "--max-rate=2M", "-d",
                                   "host=127.0.0.1 port=5432 user=postgres dbname=postgres", output=backup_log)
            if backup_job is not None and backup_job.poll() not in (None, 0):
                raise RuntimeError(f"pg_basebackup exited {backup_job.returncode}: {backup_log_path.read_text()}")
            if elapsed >= 30 and name == "legacy" and legacy is None:
                legacy_at = elapsed
                legacy = spawn("exec", leaf, vmtouch, "-e", pgdata, backup, scratch)
            status = {}
            try:
                status = json.loads((state / "status.json").read_text())
            except FileNotFoundError:
                pass
            counters = status.get("counters", {})
            backstop = counters.get("cache_fallback") or {}
            cold = resident(closed)
            memory = data_values(leaf / "memory.stat")
            remaining = int((parent / "memory.max").read_text()) - int((parent / "memory.current").read_text())
            if ready.exists() and filler.poll() == 0 and fixed_fill.poll() == 0 and awaiting_fill:
                result["closed_initial_bytes"] = cold
                result["producer_exit_second"] = round(elapsed, 2)
                reads_at_close = io_values(leaf / "io.stat")["rbytes"]
                awaiting_fill = False
            for row in backstop.get("recent_findings") or []:
                if row["path"] == str(closed):
                    source_seen.add(row["source"])
                    finding = row if finding is None else finding
            for scope in ("fixed", "auto"):
                maximum = (backstop.get(scope) or {}).get("round_requested_bytes", 0)
                max_caps[scope] = max(max_caps[scope], maximum)
            if backstop.get("round", 0) > last_round:
                last_round = backstop["round"]
                report_rounds.append({"round": last_round, "at": round(elapsed, 2), "reason": backstop["trigger"]})
            advised = (backstop.get("auto") or {}).get("advice_bytes_total", 0)
            if first_advice is None and advised:
                first_advice = elapsed
            if not awaiting_fill and first_drop is None and cold <= result["closed_initial_bytes"] - GiB:
                first_drop, available_at_drop = elapsed, remaining
            snapshots.append(dict(t=round(elapsed, 2), resident=cold, fixed=resident(fixed), hot=resident(hot),
                                  file=memory["file"]-memory["shmem"], file_refaults=memory.get("workingset_refault_file"),
                                  parent_remaining=remaining, cgroup_read=io_values(leaf / "io.stat")["rbytes"],
                                  advice_auto=advised, permission=backstop.get("permission"),
                                  daemon_cpu_percent=status.get("metrics", {}).get("self_cpu_percent", 0),
                                  daemon_rss_bytes=status.get("metrics", {}).get("self_rss_bytes", 0)))
            result["last_backstop"] = backstop
            time.sleep(.45)
        assert not awaiting_fill and result["closed_initial_bytes"] >= GiB
        assert backup_job is not None and backup_job.wait(25) == 0, backup_log_path.read_text()
        verify = sorted(Path("/usr/lib/postgresql").glob("*/bin/pg_verifybackup"))[-1]
        command(verify, "--no-parse-wal", sql_backup, timeout=30)
        if legacy:
            assert legacy.wait(10) == 0
        if pressure is not None:
            assert (area / "pressure-ready").exists()
        result.update(probe.finish())
        probe = None
        oom_after = data_values(leaf / "memory.events")
        parent_events = data_values(parent / "memory.events")
        output_io = io_values(leaf / "io.stat")
        result.update(rounds=report_rounds, max_scope_requested=max_caps,
                      finding=finding, discovery_sources=sorted(source_seen), first_auto_advice_seconds=first_advice,
                      first_gib_drop_seconds=first_drop, parent_remaining_at_drop=available_at_drop,
                      continued_seconds_after_gib_drop=(snapshots[-1]["t"]-first_drop) if first_drop else None,
                      final_net_decrease=result["closed_initial_bytes"]-snapshots[-1]["resident"],
                      backup_manifest_verified=True, backup_wal_parsing_in_this_trial=False,
                      vmtouch_started_at=legacy_at, io_bytes_and_ops={k: output_io[k]-io_before[k] for k in output_io},
                      read_bytes_after_producer_exit=output_io["rbytes"]-reads_at_close,
                      ccg_peak_sampled_cpu_percent=max(s["daemon_cpu_percent"] for s in snapshots),
                      ccg_peak_sampled_rss_bytes=max(s["daemon_rss_bytes"] for s in snapshots),
                      oom_before=oom_before, oom_after=oom_after, parent_memory_events=parent_events)
        assert len(result["errors"]) == 0 and result["samples"] > 100
        assert oom_after["oom_kill"] == oom_before["oom_kill"] and parent_events["oom_kill"] == 0
        if name == "fallback":
            assert finding is not None, "The short-lived file was not discovered"
            assert first_advice is not None and first_advice > result["producer_exit_second"], "No post-close advice"
            assert first_advice < 40, "Missed the first eligible 30-second round"
            assert first_drop is not None and result["final_net_decrease"] >= GiB
            assert min(s["resident"] for s in snapshots if s["t"] >= first_drop) == snapshots[-1]["resident"]
            assert max_caps["auto"] <= 1280 * MiB and max_caps["fixed"] <= 1280 * MiB
            assert result["last_backstop"]["auto"]["delayed_net_bytes_total"] >= GiB
            assert "memory_high_drift" in (area / "console.log").read_text()
    finally:
        if probe:
            probe.finish()
        stop_children(children)
        log.close()
        backup_log.close()
        for cg in (leaf, sibling, parent):
            if cg.exists():
                cg.rmdir()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("binary", type=Path)
    parser.add_argument("vmtouch", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    assert os.geteuid() == 0
    assert command("psql", "-XAt", "-c", "SHOW server_version").startswith("14.")
    print(command("pg_basebackup", "--version").strip(), flush=True)
    command("psql", "-XAt", "-c", "CREATE TABLE fallback_probe(id int PRIMARY KEY, n bigint); INSERT INTO fallback_probe VALUES(1,0)")
    report = dict(success=False, kernel=platform.release(), architecture=platform.machine(), trials={},
                  kubelet_pod_available_bytes=None, eviction_acceptance=False,
                  scope="Disposable cgroup-v2 development fixtures and a PG14 service. The legacy comparator receives the same three directories. No live kubelet in this comparison.")
    try:
        with tempfile.TemporaryDirectory(prefix="ccg-fallback-") as tmp:
            failures = []
            for name in ("original", "fallback", "legacy"):
                result = report["trials"][name] = {}
                try:
                    trial(name, args.binary.resolve(), args.vmtouch.resolve(), Path(tmp), result)
                    print(name, {k: result[k] for k in ("final_net_decrease", "first_gib_drop_seconds", "sql_p99_ms")}, flush=True)
                except Exception as exc:
                    result["failure"] = f"{type(exc).__name__}: {exc}"
                    print(name, result["failure"][:1000], flush=True)
                    failures.append(name)
            assert not failures, failures
        report["success"] = True
    finally:
        args.output.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] in {"exec", "fill", "hot", "pressure"}:
        helper(*sys.argv[1:5])
    else:
        main()
