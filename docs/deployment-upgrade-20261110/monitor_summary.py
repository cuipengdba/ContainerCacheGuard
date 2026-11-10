"""Read-only report analysis. Counters never cross container/process generations."""
import collections,datetime,json,statistics,time
G=2**30;M=2**20
def stamp(t):return datetime.datetime.fromtimestamp(t,datetime.timezone(datetime.timedelta(hours=8))).strftime('%m-%d %H:%M:%S')
def mean(v):return statistics.mean(v) if v else None
def fmt(v,n=2):return f'{v:.{n}f}' if v is not None else '未知'
def delta(a,z,key):
 x=a.get(key);y=z.get(key)
 return y-x if isinstance(x,(int,float)) and isinstance(y,(int,float)) and y>=x else None
def ios(d):
 for l in (d.get('io_stat') or '').splitlines():
  p=l.split()
  if p[0]=='253:0':return {k:int(v) for k,v in (z.split('=') for z in p[1:])}
 return {}
def disk(h):
 for l in (h.get('diskstats') or '').splitlines():
  p=l.split()
  if p[:2]==['8','0']:return {'read':int(p[5])*512,'write':int(p[9])*512,'busy_ms':int(p[12])}
 return {}
def net(h):
 o={}
 for l in (h.get('network') or '').splitlines():
  if ':' not in l:continue
  k,v=l.split(':',1);k=k.strip();v=v.split()
  if k.startswith(('eth','ens','enp','eno')):o[k]={'rx':int(v[0]),'tx':int(v[8])}
 return o
def apiok(d):
 try:return d.get('code')==200 and json.loads(d.get('body','{}')).get('Result')=='000'
 except (ValueError,TypeError):return False
def analyze(x,events=()):
 ss=x['samples'];first=ss[0];last=ss[-1];duration=last['time']-first['time'];h=last['host'];cp=[]
 for a,z in zip(ss,ss[1:]):
  p=a['host']['cpu_ticks'];q=z['host']['cpu_ticks'];t=sum(q[:8])-sum(p[:8]);idle=sum(q[3:5])-sum(p[3:5])
  if t>0:cp.append(100*(1-idle/t))
 age=x.get('latest_age_seconds');skew=x.get('received_local_epoch',time.time())-x['generated']
 out={'start':first['time'],'end':last['time'],'duration_seconds':duration,'samples':len(ss),'latest_age_seconds':age,'clock_offset_estimate_seconds':skew,'host':{'cpu_avg_peak_percent':[mean(cp),max(cp) if cp else None],'memory_total_GiB':h['memory']['MemTotal']/G,'memory_available_GiB':h['memory']['MemAvailable']/G,'disk_GiB':{k:v/G for k,v in h['disk_usage'].items()},'pressure':h['pressure']},'containers':{}}
 lines=[f'采样窗口（北京时间，服务器时钟）：{stamp(first["time"])} 至 {stamp(last["time"])}，{len(ss)} 样本，跨度 {duration/60:.1f} 分钟；最新样本距服务器报告生成 {fmt(age,0)} 秒。']
 if duration<3600:lines.append('本次窗口不足完整一小时，按实际采样范围报告。')
 if age is None or age>180:lines.append('异常：最新样本缺失或超过180秒，不能视为实时状态。')
 if abs(skew)>30:lines.append(f'服务器时钟与本机约差 {skew:.0f} 秒（含传输耗时）；时间窗和计划停机均按服务器时钟对齐。')
 for label,fn,keys in [('disk',disk,['read','write']),('network',net,[])]:
  a=fn(first['host']);z=fn(h)
  if label=='disk':out['host']['disk_read_write_MiBs']=[delta(a,z,k)/duration/M if delta(a,z,k) is not None and duration>0 else None for k in keys]
  else:out['host']['network_read_write_MiBs']={k:[delta(a[k],v,q)/duration/M if delta(a[k],v,q) is not None and duration>0 else None for q in ['rx','tx']] for k,v in z.items() if k in a}
 lines.append(f'宿主机：CPU均值/峰值 {fmt(mean(cp))}%/{fmt(max(cp) if cp else None)}%；内存可用 {h["memory"]["MemAvailable"]/G:.2f}/{h["memory"]["MemTotal"]/G:.2f} GiB；磁盘已用/总量 {h["disk_usage"]["used"]/G:.2f}/{h["disk_usage"]["total"]/G:.2f} GiB。')
 lines.append('宿主磁盘读/写 '+ '/'.join(fmt(v) for v in out['host']['disk_read_write_MiBs'])+' MiB/s；物理网卡接收/发送 '+json.dumps(out['host'].get('network_read_write_MiBs'),ensure_ascii=False)+' MiB/s。')
 lines.append('PSI：'+json.dumps(h['pressure'],ensure_ascii=False)+'；null=未采集/不可用，不能当作0。')
 for role in ['dbs','hisdbs']:
  groups=[]
  for s in ss:
   d=s['containers'].get(role,{})
   if not groups or groups[-1][0][1].get('cid')!=d.get('cid'):groups.append([])
   groups[-1].append((s['time'],d))
  out['containers'][role]=[]
  if len(groups)>1:lines.append(f'{role.upper()}：窗口内发生容器代际变化，以下分段统计，累计量不跨ID相减。')
  for g in groups:
   t0,a=g[0];t1,z=g[-1];cpu=[];rd=[];wr=[];th=pe=tu=0;ccgd=collections.Counter();ccgrestarts=[];periods_observed=False
   for (t,d),(u,e) in zip(g,g[1:]):
    dt=u-t
    if dt<=0:continue
    ac=d.get('cpu_stat') or {};zc=e.get('cpu_stat') or {};v=delta(ac,zc,'usage_usec')
    if v is not None:
     cpu.append(v/dt/1e6);periods_observed=True;th+=delta(ac,zc,'nr_throttled') or 0;pe+=delta(ac,zc,'nr_periods') or 0;tu+=delta(ac,zc,'throttled_usec') or 0
    for key,vals in [('rbytes',rd),('wbytes',wr)]:
     v=delta(ios(d),ios(e),key)
     if v is not None:vals.append(v/dt/M)
    c=d.get('ccg',{});v=e.get('ccg',{})
    if c.get('pid') and c.get('pid')==v.get('pid'):
     for k in ['advice_bytes','observed_net_reduction_bytes','completed_inventory_sweeps','errors','inventory_overflow','hint_overflows','hint_queue_dropped']:
      inc=delta(c.get('counters',{}),v.get('counters',{}),k)
      if inc is not None:ccgd[k]+=inc
    elif c.get('pid')!=v.get('pid'):ccgrestarts.append(stamp(u))
   cid=z.get('cid');m=z.get('memory_stat') or {};ccg=z.get('ccg',{});crg=z.get('crg',{});mem=[d['memory_current']/G for _,d in g if isinstance(d.get('memory_current'),(int,float))]
   failures={k:[] for k in ['sql','api','container']}
   for t,d in g:
    planned=any(ev['role']==role and ev['start']<=t<=ev['ready'] for ev in events)
    for kind,ok in [('sql',d.get('sql',{}).get('ok')),('api',apiok(d.get('api',{}))),('container',not d.get('error'))]:
     if not ok:failures[kind].append({'time':stamp(t),'planned':planned,'error':d.get(kind,{}).get('error') if kind!='container' else d.get('error')})
   lat={kind:[d[kind]['probe_ms'] for _,d in g if isinstance(d.get(kind,{}).get('probe_ms'),(int,float))] for kind in ['sql','api']}
   ed={k:delta(a.get('memory_events') or {},z.get('memory_events') or {},k) for k in ['oom','oom_kill','max']}
   r={'cid':cid,'start':t0,'end':t1,'samples':len(g),'cpu_avg_peak_cores':[mean(cpu),max(cpu) if cpu else None],'throttled_periods':th if periods_observed else None,'periods':pe if periods_observed else None,'throttled_seconds':tu/1e6 if periods_observed else None,'memory_current_GiB':z.get('memory_current')/G if z.get('memory_current') is not None else None,'memory_sample_peak_GiB':max(mem) if mem else None,'memory_cgroup_peak_GiB':z.get('memory_peak')/G if z.get('memory_peak') is not None else None,'memory_components_GiB':{k:m[k]/G if k in m else None for k in ['anon','file','shmem']},'io_avg_read_write_MiBs':[mean(rd),mean(wr)],'memory_events_delta':ed,'memory_events_total':z.get('memory_events'),'failures':failures,'probe_avg_max_ms':{k:[mean(v),max(v) if v else None] for k,v in lat.items()},'limits':{k:z.get(k) for k in ['cpu_max','memory_max','io_max']},'crg_state':crg.get('state'),'crg_items':crg.get('items'),'crg_automatic_io':crg.get('automatic_io'),'ccg_pid':ccg.get('pid'),'ccg_mode':ccg.get('mode'),'ccg_action':ccg.get('action'),'ccg_decision':ccg.get('decision'),'ccg_protection':ccg.get('protection'),'ccg_degraded':ccg.get('degraded'),'ccg_counters_delta':dict(ccgd),'ccg_counters_total':ccg.get('counters'),'ccg_restarts':ccgrestarts,'status_age_seconds':{k:z.get(k+'_status_age_seconds') for k in ['crg','ccg']}}
   changes=[]
   for (t,d),(u,e) in zip(g,g[1:]):
    keys=['cpu_max','memory_max','io_max']
    if d.get('cid') and any(d.get(k)!=e.get(k) for k in keys):changes.append({'time':stamp(u),'before':{k:d.get(k) for k in keys},'after':{k:e.get(k) for k in keys}})
   r['limit_changes']=changes
   out['containers'][role].append(r)
   if changes:lines.append('  '+role.upper()+' 本代容器限额变更（不重置累计量）：'+json.dumps(changes,ensure_ascii=False))
   lines.append(f'{role.upper()} {str(cid)[:12]} [{stamp(t0)}—{stamp(t1)}]：CPU均值/峰值 '+ '/'.join(fmt(v) for v in r['cpu_avg_peak_cores'])+f' 核，节流 {fmt(r["throttled_seconds"])} 秒/{r["throttled_periods"]}周期；内存当前/采样峰值 '+ '/'.join(fmt(v) for v in [r['memory_current_GiB'],r['memory_sample_peak_GiB']])+' GiB，anon/file/shmem '+ '/'.join(fmt(v) for v in r['memory_components_GiB'].values())+' GiB；磁盘读/写 '+ '/'.join(fmt(v) for v in r['io_avg_read_write_MiBs'])+' MiB/s。')
   lines.append('  OOM增量/累计 '+json.dumps(ed)+' / '+json.dumps(z.get('memory_events'))+'；SQL/API异常 '+json.dumps({k:failures[k] for k in ['sql','api']},ensure_ascii=False)+'；探针均值/最大ms '+json.dumps(r['probe_avg_max_ms'],ensure_ascii=False)+'（缺失即未知）。')
   lines.append('  CRG='+str(r['crg_state'])+'，实际限制 '+json.dumps(r['limits'])+'；CCG='+str(r['ccg_mode'])+'/'+str(r['ccg_action'])+' '+json.dumps(r['ccg_decision'],ensure_ascii=False)+'；工作增量 '+json.dumps(r['ccg_counters_delta'])+'；完整统计见hourly-analysis.json。')
   for k,v in r['status_age_seconds'].items():
    limit=360 if k=='crg' else 180
    if v is not None and v>limit:lines.append(f'  异常：{k} 状态已过期 {v:.0f}秒（阈值{limit}秒；CRG相同状态约5分钟才落盘）。')
   if ccgrestarts:lines.append('  CCG进程重启（计数不跨PID相减）：'+', '.join(ccgrestarts))
 return out,lines
