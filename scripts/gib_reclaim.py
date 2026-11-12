#!/usr/bin/env python3
"""Isolated native >=1GiB cache experiment, never a field acceptance test.

Only creates files in its own temporary tree. Does not change cgroup/CPU/IO
limits, call global drop_caches, restart a database, or clear OOM history.
PostgreSQL 14 must already be running with PG* connection variables.
The HTTP endpoint is a LOCAL TEST FIXTURE, never a CCG service.
"""
import argparse, datetime, hashlib, http.server, json, os, pathlib, platform
import statistics, subprocess, tempfile, threading, time, urllib.request

GiB=1<<30

def command(args,timeout=120):
    p=subprocess.run(list(map(str,args)),capture_output=True,text=True,timeout=timeout)
    if p.returncode: raise RuntimeError(p.stdout+p.stderr)
    return p.stdout

def sql(query):
    return command(['psql','-X','-A','-t','-v','ON_ERROR_STOP=1','-c',query],15)

def summary(samples):
    if not samples:return {'samples':0,'p99_ms':None,'max_ms':None}
    ordered=sorted(samples)
    return {'samples':len(samples),'p99_ms':ordered[min(len(ordered)-1,int(.99*len(ordered)))], 'max_ms':max(samples),'mean_ms':statistics.mean(samples)}

def main():
    ap=argparse.ArgumentParser(); ap.add_argument('binary'); ap.add_argument('--output',required=True); a=ap.parse_args()
    binary=str(pathlib.Path(a.binary).resolve())
    report={'success':False,'scope':'isolated known-low-reuse file experiment with real PG14 SQL/local fixture API; not production load replay or Kubernetes eviction/backup/restore acceptance',
            'kernel':platform.release(),'architecture':platform.machine(),'page_size':os.sysconf('SC_PAGE_SIZE'),
            'cold_fixture_bytes':9*(128<<20),'forbidden_operations_used':[]}
    probes=[]; stop=threading.Event(); phase={'name':'setup'}
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            start=time.monotonic()
            try:
                sql('UPDATE ccg_rc16_probe SET n=n+1 WHERE id=1; SELECT count(*) FROM ccg_rc16_probe;')
                body=b'healthy'; self.send_response(200)
                probes.append({'phase':phase['name'],'sql_ms':1000*(time.monotonic()-start),'error':False})
            except Exception as exc:
                body=type(exc).__name__.encode(); self.send_response(500)
                probes.append({'phase':phase['name'],'error':True})
            self.end_headers(); self.wfile.write(body)
        def log_message(self,*args): pass
    server=None; thread=None; daemon=None
    try:
        version=sql('SHOW server_version;').strip(); assert version.split('.')[0]=='14',version
        report['postgresql_version']=version
        sql('DROP TABLE IF EXISTS ccg_rc16_probe; CREATE TABLE ccg_rc16_probe(id int PRIMARY KEY,n bigint,pad text); INSERT INTO ccg_rc16_probe SELECT i,0,repeat(\'p\',256) FROM generate_series(1,10000)i;')
        server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler)
        threading.Thread(target=server.serve_forever,daemon=True).start()
        def probe():
            while not stop.is_set():
                begin=time.monotonic(); label=phase['name']
                try:
                    with urllib.request.urlopen('http://127.0.0.1:%d/'%server.server_port,timeout=20) as response: assert response.read()==b'healthy'
                    probes.append({'phase':label,'api_ms':1000*(time.monotonic()-begin),'error':False})
                except Exception: probes.append({'phase':label,'error':True})
                stop.wait(.25)
        thread=threading.Thread(target=probe,daemon=True); thread.start()
        with tempfile.TemporaryDirectory(prefix='ccg-gib-') as tmp:
            w=pathlib.Path(tmp); data=w/'cold';data.mkdir(); files=[]; expected=hashlib.sha256()
            chunk=os.urandom(1<<20)
            for i in range(9):
                p=data/str(100000+i);files.append(p)
                with p.open('wb') as out:
                    for _ in range(128):out.write(chunk);expected.update(chunk)
                    out.flush();os.fsync(out.fileno())
                p.chmod(0o700)
            # Fixture is deliberately idle and low-reuse; disk-read rate alone
            # would not establish that a real PG relation is cold.
            def resident():return json.loads(command([binary,'scan','--path',data,'--scan-bytes-per-second',str(1<<30)]))['Resident']
            before=resident();assert before>=GiB,('less than 1GiB resident fixture',before)
            report['initial_resident_bytes']=before
            cfg=json.loads(command([binary,'init-config','--mode','paths','--path',data]))
            cfg.update(action='observe',access_hints=False,trigger='always',warmup_seconds=0,protect_seconds=1,min_file_age_seconds=0,
                       recheck_seconds=1,extended_recheck_seconds=[3,7],cooldown_seconds=10,rescan_seconds=3600,
                       interval_ms=200,log_interval_seconds=1,worker_budget_ms=100,max_cpu_percent=50,max_rss_bytes=256<<20,
                       scan_bytes_per_second=512<<20,initial_reclaim_bytes_per_second=64<<20,reclaim_bytes_per_second=64<<20,
                       max_dirty_bytes=128<<20,max_io_bytes_per_second=512<<20,max_file_refaults_per_second=100000,max_io_psi_avg10=90,
                       state_file=str(w/'observe/status.json'),log_directory=str(w/'observe/logs'))
            config=w/'config.json'; state=None
            def start(label):
                nonlocal daemon,state
                cfg.update(action=label,state_file=str(w/label/'status.json'),log_directory=str(w/label/'logs'))
                config.write_text(json.dumps(cfg)); state=pathlib.Path(cfg['state_file'])
                log=(w/(label+'.stdout')).open('w')
                daemon=subprocess.Popen([binary,'run','--config',config],stdout=log,stderr=subprocess.STDOUT)
                return log
            def status():
                try:return json.loads(state.read_text())
                except (FileNotFoundError,json.JSONDecodeError):return None
            phase['name']='observe';log=start('observe'); time.sleep(10)
            observed=status();assert observed and observed['counters']['advice_bytes']==0
            report['observe_end_resident_bytes']=resident()
            assert report['observe_end_resident_bytes']>=GiB,'baseline lost fixture pages to external reclaim'
            daemon.terminate();daemon.wait(15);daemon=None;log.close()
            phase['name']='enforce';log=start('enforce'); began=time.monotonic(); states=[]
            while time.monotonic()-began<120:
                if daemon.poll() is not None:raise RuntimeError((w/'enforce.stdout').read_text()[-4000:])
                s=status()
                if s:
                    states.append(s)
                    if s['counters']['observed_net_reduction_bytes']>=GiB:break
                time.sleep(.2)
            else:raise AssertionError('1GiB advice reduction not reached; last='+json.dumps(status()))
            report['seconds_to_1gib_immediate_region_decrease']=time.monotonic()-began
            report['persistence']=[]
            for delay in [0,5,15]:
                if delay:time.sleep(delay)
                value=resident();report['persistence'].append({'seconds_since_1gib_gate':time.monotonic()-began-report['seconds_to_1gib_immediate_region_decrease'],'resident_bytes':value,'decrease_from_observe_end_bytes':report['observe_end_resident_bytes']-value})
                assert report['observe_end_resident_bytes']-value>=GiB,('GiB decrease not sustained',report['persistence'])
            last=status();report['last_status']=last
            first=states[0]['metrics'];end=last['metrics']
            report['oom_kill_before']=first['oom_kill_total'];report['oom_kill_after']=end['oom_kill_total']
            assert report['oom_kill_after']==report['oom_kill_before'],'new OOM observed'
            rates=[s['metrics']['io_bytes_per_second'] for s in states if s['metrics'].get('available_metrics',{}).get('io_bytes_per_second')]
            report['sampled_visible_cgroup_io_peak_bytes_per_second']=max(rates,default=None)
            report['sampled_self_cpu_peak_percent']=max(s['metrics']['self_cpu_percent'] for s in states)
            report['sampled_self_rss_peak_bytes']=max(s['metrics']['self_rss_bytes'] for s in states)
            report['visible_cgroup_ordinary_cache_change_bytes']=(end['file_bytes']-end['shmem_bytes'])-(first['file_bytes']-first['shmem_bytes'])
            daemon.terminate();daemon.wait(15);daemon=None;log.close()
            # Content hashing is AFTER persistence measurements; it rereads the
            # fixture and must never be mistaken for spontaneous reappearance.
            got=hashlib.sha256()
            for p in files:
                with p.open('rb') as src:
                    while block:=src.read(1<<20):got.update(block)
            assert got.hexdigest()==expected.hexdigest(),'fixture content changed'
            report['fixture_content_sha256_verified']=True
        stop.set();thread.join(25)
        report['probes']={}
        for label in ['observe','enforce']:
            group=[p for p in probes if p['phase']==label]
            report['probes'][label]={'sql':summary([p['sql_ms'] for p in group if 'sql_ms'in p]),'api':summary([p['api_ms'] for p in group if 'api_ms'in p]),'errors':sum(p['error'] for p in group)}
            assert report['probes'][label]['errors']==0 and report['probes'][label]['sql']['samples']>=10
        report['success']=True
    except Exception as exc:
        report['error']=str(exc);raise
    finally:
        if daemon:daemon.terminate();daemon.wait(15)
        stop.set()
        if thread:thread.join(25)
        if server:server.shutdown();server.server_close()
        pathlib.Path(a.output).write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps({k:v for k,v in report.items() if k!='last_status'},indent=2))

if __name__=='__main__':main()
