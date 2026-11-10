#!/usr/bin/python3
"""Read-only host/container samples. No table scans or database mutations."""
import datetime,json,pathlib,subprocess,time,urllib.request,shlex,sys,statistics,os
BASE=pathlib.Path('/opt/container-guards171')
LOG=BASE/'monitor';LOG.mkdir(mode=0o700,exist_ok=True)
def run(args,timeout=12):
 p=subprocess.run(args,text=True,capture_output=True,timeout=timeout)
 if p.returncode:raise RuntimeError(p.stderr[-500:])
 return p.stdout.strip()
def read(p):
 try:return pathlib.Path(p).read_text().strip()
 except OSError:return None
def kv(p):
 v=read(p)
 return {a:int(b) for a,b in (line.split() for line in v.splitlines())} if v else None
def atomic(p,data):
 tmp=p.with_suffix('.tmp');tmp.write_text(json.dumps(data,ensure_ascii=False)+'\n');tmp.replace(p)
def sample():
 now=time.time();s={'time':now,'iso':datetime.datetime.now(datetime.timezone.utc).isoformat(),'containers':{}}
 mem={a:int(b.split()[0])*1024 for a,b in (l.split(':',1) for l in read('/proc/meminfo').splitlines())}
 stat=read('/proc/stat').splitlines()[0].split()[1:]
 s['host']={'cpu_ticks':[int(v) for v in stat],'memory':{k:mem.get(k) for k in ['MemTotal','MemAvailable','Cached','Buffers','Shmem','Slab','SwapTotal','SwapFree']},'loadavg':read('/proc/loadavg'),'diskstats':read('/proc/diskstats'),'network':read('/proc/net/dev'),'pressure':{k:read('/proc/pressure/'+k) for k in ['cpu','memory','io']},'disk_usage':dict(zip(['total','used','free'],__import__('shutil').disk_usage('/opt')))}
 try:containers=json.loads(run(['crictl','ps','-o','json']))['containers']
 except Exception as e:s['error']=str(e);return s
 for role,port in [('dbs',14005),('hisdbs',16911)]:
  d=s['containers'][role]={}
  try:
   cs=[c for c in containers if c['metadata']['name']==role and c.get('labels',{}).get('io.kubernetes.pod.namespace')=='puc'];assert len(cs)==1
   cid=cs[0]['id'];ins=json.loads(run(['crictl','inspect',cid]));pid=ins['info']['pid']
   rel=next(l.split(':',2)[2] for l in read(f'/proc/{pid}/cgroup').splitlines() if l.startswith('0::'))
   cg=pathlib.Path('/sys/fs/cgroup')/rel.lstrip('/');root=pathlib.Path(f'/proc/{pid}/root')
   d.update({'cid':cid,'pid':pid,'pod':cs[0].get('labels',{}).get('io.kubernetes.pod.name'),'cgroup':str(cg),'memory_current':int(read(cg/'memory.current')),'memory_max':read(cg/'memory.max'),'memory_stat':kv(cg/'memory.stat'),'memory_events':kv(cg/'memory.events'),'cpu_stat':kv(cg/'cpu.stat'),'cpu_max':read(cg/'cpu.max'),'pids_current':read(cg/'pids.current'),'io_stat':read(cg/'io.stat'),'io_max':read(cg/'io.max'),'pressure':{k:read(cg/(k+'.pressure')) for k in ['cpu','memory','io']}})
   for key,path in [('crg','tmp/container-guards/crg/status.json'),('ccg','tmp/container-guards/ccg/status.json')]:
    try:d[key]=json.loads((root/path).read_text())
    except Exception as e:d[key]={'error':str(e)}
   q="select json_build_object('ok',true,'backends',sum(numbackends),'commits',sum(xact_commit),'rollbacks',sum(xact_rollback),'blocks_read',sum(blks_read),'blocks_hit',sum(blks_hit),'temp_bytes',sum(temp_bytes),'deadlocks',sum(deadlocks)) from pg_stat_database"
   cmd='PGOPTIONS='+shlex.quote('-c statement_timeout=5000')+' LD_LIBRARY_PATH=/opt/postgresql/pgsql/lib /opt/postgresql/pgsql/bin/psql -XAtw -h /opt/postgresql/pgdata -p '+str(port)+' -d postgres -c '+shlex.quote(q)
   t=time.monotonic()
   try:d['sql']=json.loads(run(['crictl','exec',cid,'su','-','postgres','-c',cmd],timeout=9));d['sql']['probe_ms']=(time.monotonic()-t)*1000
   except Exception as e:d['sql']={'ok':False,'error':str(e)}
   try:
    req=urllib.request.Request(f'http://127.0.0.1:{14015 if role=="dbs" else 16912}/test',data=b'{}',headers={'Content-Type':'application/json'})
    with urllib.request.urlopen(req,timeout=5) as r:d['api']={'code':r.status,'body':r.read(400).decode(errors='replace')}
   except Exception as e:d['api']={'error':str(e)}
  except Exception as e:d['error']=str(e)
 return s
def report():
 now=time.time();samples=[]
 for path in sorted(LOG.glob('samples-*.jsonl'))[-2:]:
  for line in path.read_text().splitlines():
   try:
    s=json.loads(line)
    if now-s['time']<=3660:samples.append(s)
   except (ValueError,KeyError):pass
 return {'generated':now,'sample_interval_seconds':60,'latest_age_seconds':now-samples[-1]['time'] if samples else None,'samples':samples}
if __name__=='__main__':
 if '--report' in sys.argv:print(json.dumps(report(),ensure_ascii=False));raise SystemExit
 while True:
  started=time.time()
  try:
   s=sample();path=LOG/('samples-'+datetime.datetime.now().strftime('%Y%m%d')+'.jsonl')
   with path.open('a') as f:f.write(json.dumps(s,ensure_ascii=False)+'\n')
   atomic(LOG/'latest.json',s)
   for old in LOG.glob('samples-????????.jsonl'):
    if time.time()-old.stat().st_mtime>14*86400:old.unlink()
  except Exception as e:print(str(e),flush=True)
  time.sleep(max(1,60-(time.time()-started)))
