#!/usr/bin/env python3
"""Destructive ONLY to a new, isolated CI cgroup and private fixture files.

Requires root on an ephemeral VM, never run on an existing database/container.
Real cgroup2/memory.reclaim and >=1GiB clean file cache; synthetic node envelope
is explicitly NOT proof of kubelet eviction avoidance. No global drop_caches.
"""
import argparse, datetime, hashlib, http.server, json, os, pathlib, platform
import signal, statistics, subprocess, sys, tempfile, threading, time, urllib.request

GiB=1<<30; MiB=1<<20
def stamp(): return datetime.datetime.now(datetime.timezone.utc).isoformat()
def atomic(path,value):
    tmp=path.with_suffix('.new');tmp.write_text(json.dumps(value));tmp.chmod(0o644);tmp.replace(path)
def values(path):
    return {k:int(v) for k,v in (line.split() for line in path.read_text().splitlines())}
def run(args,**kwargs):
    p=subprocess.run(list(map(str,args)),capture_output=True,text=True,timeout=30,**kwargs)
    if p.returncode: raise RuntimeError(p.stdout+p.stderr)
    return p.stdout
def sql(): return run(['psql','-X','-A','-t','-v','ON_ERROR_STOP=1','-c','SELECT count(*) FROM ccg_rc16_probe;']).strip()
def percentile(data):
    return sorted(data)[min(len(data)-1,int(.99*len(data)))] if data else None
def holder(cgroup,path,ready):
    (pathlib.Path(cgroup)/'cgroup.procs').write_text(str(os.getpid()))
    digest=hashlib.sha256()
    with open(path,'rb',buffering=0) as f:
        while True:
            b=f.read(4*MiB)
            if not b: break
            digest.update(b)
    pathlib.Path(ready).write_text(digest.hexdigest())
    while True: time.sleep(1)

def main():
    if len(sys.argv)>1 and sys.argv[1]=='--holder': return holder(*sys.argv[2:])
    ap=argparse.ArgumentParser();ap.add_argument('binary');ap.add_argument('--output',required=True);a=ap.parse_args()
    assert os.geteuid()==0,'ephemeral root runner required'
    binary=str(pathlib.Path(a.binary).resolve());out=pathlib.Path(a.output).resolve()
    report={'success':False,'scope':'isolated leaf cgroup; real kernel reclaim; synthetic node envelope; real PG14 SQL and local fixture API; not real DBS/HISDBS load, K8s eviction avoidance, or 48h acceptance',
            'architecture':platform.machine(),'kernel':platform.release(),'page_size':os.sysconf('SC_PAGE_SIZE'),
            'forbidden_operations_used':[],'modified_existing_cgroup_limits':False,'node_signal_synthetic':True,'kubernetes_eviction_acceptance':False}
    procs=[];stop=threading.Event();cg=None;parent=None;server=None;probes=[];phase={'name':'baseline'}
    try:
        assert sql()=='10000'
        with tempfile.TemporaryDirectory(prefix='ccg-kernel-') as tmp:
            w=pathlib.Path(tmp);w.chmod(0o755);data=w/'data';data.mkdir();payload=data/'cold';ready=w/'ready'
            parent=pathlib.Path('/sys/fs/cgroup')/('ccg-rc17-'+w.name);parent.mkdir()
            (parent/'cgroup.subtree_control').write_text('+memory +io');cg=parent/'fixture';cg.mkdir()
            (cg/'memory.max').write_text(str(2*GiB));(cg/'memory.swap.max').write_text('0')
            assert (cg/'memory.reclaim').exists(),'kernel lacks memory.reclaim'
            # Set up ONLY our fixture, with clean uncharged cache before the
            # holder populates it inside the new leaf; never clear host cache.
            chunk=os.urandom(MiB);expected=hashlib.sha256()
            with payload.open('wb',buffering=0) as f:
                for _ in range(1152): f.write(chunk);expected.update(chunk)
                os.fsync(f.fileno());os.posix_fadvise(f.fileno(),0,0,os.POSIX_FADV_DONTNEED)
            p=subprocess.Popen([sys.executable,__file__,'--holder',str(cg),str(payload),str(ready)],stdout=subprocess.DEVNULL);procs.append(p)
            deadline=time.monotonic()+90
            while not ready.exists():
                assert p.poll() is None,'holder exited'
                assert time.monotonic()<deadline,'fixture fill timed out'
                time.sleep(.2)
            assert ready.read_text()==expected.hexdigest()
            before=values(cg/'memory.stat');assert before['file']-before['shmem']>=GiB,before
            report['initial_file_bytes']=before['file']-before['shmem'];report['initial_current_bytes']=int((cg/'memory.current').read_text())
            socketdir=w/'socket';socketdir.mkdir();socketdir.chmod(0o755)
            business=w/'business.json';node=w/'kernel.json';socket=socketdir/'agent.sock'
            cfg=json.loads(run([binary,'init-config','--mode','paths','--path',data]));cfg.update(action='enforce',interval_ms=1000,warmup_seconds=10,log_interval_seconds=1,
                cache_target_bytes=8*GiB,min_file_age_seconds=3600,protect_seconds=3600,access_hints=False,
                max_io_bytes_per_second=GiB,max_file_refaults_per_second=100000,max_io_psi_avg10=90,max_cpu_percent=90,max_rss_bytes=256*MiB,
                business_signal=str(business),business_source_id='real-pg14-api',max_business_p99_ms=2000,node_id='isolated-ci',
                cgroup_path=str(cg),state_file=str(w/'client/status.json'),log_directory=str(w/'client/logs'))
            cfg['kernel_reclaim'].update(enabled=True,target='fixture',socket=str(socket),signal_file=str(node),signal_source_id='synthetic-ci-node',
                min_headroom_bytes=1536*MiB,request_max_bytes=256*MiB,max_memory_psi_avg10=90)
            # Separate durable journal remains bound across executor restart.
            state=pathlib.Path('/var/lib/containercacheguard/reclaim');state.mkdir(parents=True,exist_ok=True,mode=0o700)
            cfg['kernel_reclaim']['state_directory']=str(state)
            config=w/'config.json';atomic(config,cfg);agentcfg=w/'agent.json';atomic(agentcfg,{'socket':str(socket),'state_directory':str(state),'log_directory':str(w/'agent-logs'),
                'targets':[{'name':'fixture','cgroup':str(cg),'config':str(config),'peer_uid':0,'priority':0}]})
            run([binary,'check-config','--config',config])
            class Handler(http.server.BaseHTTPRequestHandler):
                def do_GET(self):
                    begin=time.monotonic();ok=False
                    try: ok=sql()=='10000'
                    except Exception: pass
                    probes.append({'time':time.monotonic(),'phase':phase['name'],'sql_ms':1000*(time.monotonic()-begin),'error':not ok})
                    self.send_response(200 if ok else 500);self.end_headers();self.wfile.write(b'ok' if ok else b'failed')
                def log_message(self,*args): pass
            server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler)
            threading.Thread(target=server.serve_forever,daemon=True).start()
            boot=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()
            def produce():
                while not stop.is_set():
                    begin=time.monotonic();ok=False
                    try:
                        with urllib.request.urlopen('http://127.0.0.1:%d/'%server.server_port,timeout=20) as r: ok=r.read()==b'ok'
                    except Exception: pass
                    probes.append({'time':time.monotonic(),'phase':phase['name'],'api_ms':1000*(time.monotonic()-begin),'error':not ok})
                    recent=[p for p in probes if p['time']>time.monotonic()-10 and 'api_ms' in p];ts=stamp()
                    atomic(business,{'time':ts,'source_id':'real-pg14-api','p99_ms':percentile([p['api_ms'] for p in recent]) or 0,'pause':not ok})
                    atomic(node,{'schema_version':2,'source_id':'synthetic-ci-node','node_id':'isolated-ci','boot_id':boot,'time':ts,'config_version':'fixture-only-v1',
                        'allocatable_required':False,'memory_pressure':False,'constraints':[{'kind':'node','time':ts,'available_bytes':8*GiB,'hard_bytes':256*MiB,'soft_bytes':512*MiB}],
                        'business_time':ts,'business_samples':len(recent),'business_error_rate':sum(p['error'] for p in recent)/max(1,len(recent))})
                    stop.wait(.25)
            thread=threading.Thread(target=produce,daemon=True);thread.start()
            time.sleep(10);phase['name']='kernel'
            # Start only new test client in our leaf; agent stays outside it.
            loghandles=[]
            def start_agent():
                f=(w/('agent-'+str(len(procs))+'.log')).open('w');loghandles.append(f)
                p=subprocess.Popen([binary,'reclaim-agent','--config',str(agentcfg)],stdout=f,stderr=subprocess.STDOUT);procs.append(p);return p
            agent=start_agent()
            def join(): (cg/'cgroup.procs').write_text(str(os.getpid()))
            f=(w/'client-console.log').open('w');loghandles.append(f)
            client=subprocess.Popen([binary,'run','--config',str(config)],preexec_fn=join,stdout=f,stderr=subprocess.STDOUT);procs.append(client)
            samples=[];finished=None;restarted=False;deadline=time.monotonic()+210
            while time.monotonic()<deadline:
                assert agent.poll() is None, 'agent exited: '+''.join(p.read_text()[-10000:] for p in w.glob('agent-*.log'))
                assert client.poll() is None,'client exited: '+(w/'client-console.log').read_text()[-10000:]
                stats=values(cg/'memory.stat');samples.append({'time':stamp(),'current_bytes':int((cg/'memory.current').read_text()),'file_bytes':stats['file'],'shmem_bytes':stats['shmem'],'anon_bytes':stats['anon'],'slab_bytes':stats['slab'],'io_stat':(cg/'io.stat').read_text(),'oom':values(cg/'memory.events')})
                journal=state/'journal.json'
                if journal.exists():
                    j=json.loads(journal.read_text());entries=[v for v in j['targets'].values() if v.get('target_identity') and v.get('phase')=='WAIT_RECOVERY']
                    if entries:
                        status=entries[0];finished=finished or time.monotonic()
                        if time.monotonic()-finished>10 and not restarted:
                            agent.terminate();agent.wait(10);agent=start_agent();restarted=True
                        if any(o['requested_seconds']==120 for o in status.get('observations',[])):
                            report['final_status']=status;report['node_request_budget']=j['budget'];break
                time.sleep(1)
            assert 'final_status' in report,'no completed kernel event/rechecks: '+''.join(p.read_text()[-10000:] for p in w.glob('agent-*.log'))
            status=report['final_status'];assert status['kernel_result']=='accepted_request_not_measured_bytes',status
            assert len(report['node_request_budget'])==1,'duplicate kernel write reservation'
            assert restarted,'restart dedup not exercised'
            assert {1,5,30,120}.issubset({o['requested_seconds'] for o in status['observations']})
            actual=report['initial_current_bytes']-samples[-1]['current_bytes'];report['current_net_reduction_bytes']=actual
            file_net=report['initial_file_bytes']-(samples[-1]['file_bytes']-samples[-1]['shmem_bytes']);report['file_net_reduction_bytes']=file_net
            assert actual>=128*MiB and file_net>=128*MiB,(actual,file_net)
            assert all(s['oom']['oom_kill']==0 for s in samples),'test OOM'
            report['samples']=samples;report['restart_dedup_verified']=True;report['single_reservation_verified']=True
            for label in ('baseline','kernel'):
                d=[p for p in probes if p['phase']==label];report[label+'_probes']={'samples':len(d),'errors':sum(p['error'] for p in d),'sql_p99_ms':percentile([p['sql_ms'] for p in d if 'sql_ms'in p]),'api_p99_ms':percentile([p['api_ms'] for p in d if 'api_ms'in p])}
                assert d and not any(p['error'] for p in d)
            report['success']=True
    except Exception as exc:
        report['error']=str(exc);raise
    finally:
        stop.set()
        for p in reversed(procs):
            if p.poll() is None:
                p.terminate()
                try:p.wait(10)
                except subprocess.TimeoutExpired:p.kill();p.wait(10)
        if server:server.shutdown()
        if cg:
            try:cg.rmdir()
            except OSError:pass
        if parent:
            try:parent.rmdir()
            except OSError:pass
        out.write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps({k:v for k,v in report.items() if k not in ('samples','final_status')},indent=2))
if __name__=='__main__':main()
