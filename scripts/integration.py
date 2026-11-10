#!/usr/bin/env python3
"""Linux CLI regression using only a private temporary directory.
Never calls global drop_caches or changes cgroup settings. Requires readable
memory cgroup and an ext4/xfs/btrfs/overlay/f2fs temporary directory.
"""
import argparse, datetime, hashlib, json, os, pathlib, signal, subprocess, tempfile, time

def run(args):
    r=subprocess.run(list(map(str,args)),capture_output=True,text=True,timeout=120)
    if r.returncode:raise RuntimeError(r.stdout+r.stderr)
    return r.stdout
def wait_state(p,predicate,timeout=20):
    end=time.monotonic()+timeout
    while time.monotonic()<end:
        try:
            s=json.loads(p.read_text())
            if predicate(s):return s
        except (FileNotFoundError,json.JSONDecodeError):pass
        time.sleep(.1)
    raise RuntimeError('state timeout: '+(p.read_text() if p.exists() else 'missing'))
def main():
    ap=argparse.ArgumentParser();ap.add_argument('binary');ap.add_argument('--output');args=ap.parse_args();binary=str(pathlib.Path(args.binary).resolve());checks=[]
    with tempfile.TemporaryDirectory(prefix='ccg-regression-') as tmp:
        w=pathlib.Path(tmp);data=w/'data';data.mkdir();file=data/'payload';protected=data/'protected';body=os.urandom(1<<20)
        for f in [file,protected]:
            with f.open('wb') as out:
                for _ in range(64):out.write(body)
                out.flush();os.fsync(out.fileno())
        digest=hashlib.sha256(file.read_bytes()).hexdigest()
        cfg=json.loads(run([binary,'init-config','--mode','paths','--path',data]));cfg.update(action='enforce',trigger='always',warmup_seconds=0,protect_seconds=1,min_file_age_seconds=0,recheck_seconds=1,cooldown_seconds=3,interval_ms=100,log_interval_seconds=1,worker_budget_ms=50,max_cpu_percent=50,max_dirty_bytes=1<<40,max_io_psi_avg10=0,initial_reclaim_bytes_per_second=16<<20,reclaim_bytes_per_second=32<<20,exclude_paths=[str(protected)],state_file=str(w/'run/status.json'))
        config=w/'config.json';config.write_text(json.dumps(cfg));doctor=json.loads(run([binary,'doctor','--config',config]));checks.append('doctor_real_cgroup')
        initial=json.loads(run([binary,'scan','--path',file]))
        log=(w/'daemon.log').open('w');p=subprocess.Popen([binary,'run','--config',str(config)],stdout=log,stderr=subprocess.STDOUT)
        try:
            state=w/'run/status.json';s=wait_state(state,lambda s:s['counters']['observed_net_reduction_bytes']>=16<<20);checks.append('enforce_measured_reduction')
            os.kill(p.pid,signal.SIGUSR1);wait_state(state,lambda s:s['decision']['state']=='SUSPENDED');time.sleep(.3);s=json.loads(state.read_text());n=s['counters']['advice_bytes'];time.sleep(1.5);s=json.loads(state.read_text());assert s['counters']['advice_bytes']==n;checks.append('pause_stops_advice')
            second=subprocess.run([binary,'run','--config',str(config),'--duration','1s'],capture_output=True,text=True,timeout=10);assert second.returncode!=0 and 'another daemon' in second.stderr;checks.append('duplicate_instance_rejected')
            hot=json.loads(run([binary,'scan','--path',protected]));assert hot['Resident']>=60<<20;checks.append('excluded_file_stays_resident')
            os.kill(p.pid,signal.SIGUSR2);wait_state(state,lambda s:not s['paused']);checks.append('resume')
        finally:
            p.terminate();p.wait(10);log.close()
        assert hashlib.sha256(file.read_bytes()).hexdigest()==digest;checks.append('content_unchanged')
        started=time.monotonic();timed=subprocess.run([binary,'scan','--path',str(file),'--scan-bytes-per-second',str(4<<20),'--duration','100ms'],capture_output=True,text=True,timeout=3);assert timed.returncode!=0 and time.monotonic()-started<2;assert json.loads(timed.stdout)['Complete'] is False;checks.append('scan_deadline_inside_large_file')
        # A real signal flap must not restart advice on its first good sample.
        cfg.update(mode='paths',action='enforce',recovery_healthy_seconds=2,
                   business_signal=str(w/'business.json'),signal_max_age_seconds=120,
                   state_file=str(w/'recovery/status.json'))
        def business_signal(pause):
            signal_tmp=w/'business.tmp'
            signal_tmp.write_text(json.dumps(dict(time=datetime.datetime.now(datetime.timezone.utc).isoformat(),pause=pause)))
            signal_tmp.replace(w/'business.json')
        business_signal(True)
        config.write_text(json.dumps(cfg))
        with (w/'recovery.log').open('w') as log:
            p=subprocess.Popen([binary,'run','--config',str(config)],stdout=log,stderr=subprocess.STDOUT)
            try:
                state=w/'recovery/status.json'
                wait_state(state,lambda s:s['decision']['reason']=='business_pause')
                business_signal(False)
                wait_state(state,lambda s:s['decision']['reason']=='waiting_healthy_recovery')
                time.sleep(.5)
                assert json.loads(state.read_text())['counters']['advice_bytes']==0
                checks.append('risk_recovery_holds_advice')
                business_signal(True)
                wait_state(state,lambda s:s['decision']['reason']=='business_pause' and s['recovery']['healthy_seconds']==0)
                resumed_after=time.monotonic()
                business_signal(False)
                s=wait_state(state,lambda s:s['counters']['advice_bytes']>0)
                assert time.monotonic()-resumed_after>=2 and s['recovery']['recovery_resumes_total']>=1
                checks.append('continuous_health_resumes_after_flap')
            finally:
                p.terminate();p.wait(10)
        assert hashlib.sha256(file.read_bytes()).hexdigest()==digest
        checks.append('recovery_keeps_content_unchanged')
        cfg.update(business_signal='')
        # Auto scope is tested separately, in observe mode; it never evicts
        # arbitrary runner data while discovering mounted data directories.
        cfg.update(mode='auto',paths=[],auto_roots=[str(data)],action='observe',state_file=str(w/'auto/status.json'))
        config.write_text(json.dumps(cfg));run([binary,'run','--config',config,'--duration','2s']);s=json.loads((w/'auto/status.json').read_text());assert str(data) in s['roots'] and s['counters']['advice_bytes']==0;checks.append('auto_discovers_and_observe_never_advises')
        report=dict(success=True,checks=checks,kernel=os.uname().release,arch=os.uname().machine,page_size=os.sysconf('SC_PAGE_SIZE'),initial_resident=initial['Resident'],kubernetes_eviction_test=False)
        if args.output:pathlib.Path(args.output).write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps(report,indent=2))
if __name__=='__main__':main()
