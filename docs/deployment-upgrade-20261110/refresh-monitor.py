"""Fetch restricted read-only SSH report and update local charts/summary."""
import json,pathlib,time,datetime,html,statistics,sys
import paramiko
BASE=pathlib.Path(__file__).resolve().parent
c=paramiko.SSHClient();c.load_system_host_keys(str(pathlib.Path.home()/'.ssh/known_hosts'))
c.connect('20.180.12.171',username='root',key_filename=str(pathlib.Path.home()/'.ssh/guards171-monitor'),allow_agent=False,look_for_keys=False,timeout=15)
try:
 _,out,err=c.exec_command('report',timeout=30);raw=out.read();error=err.read();rc=out.channel.recv_exit_status()
 if rc:raise RuntimeError(error.decode(errors='replace'))
 data=json.loads(raw)
finally:c.close()
data['received_local_epoch']=time.time()
(BASE/'hourly-latest.json').write_text(json.dumps(data,ensure_ascii=False),encoding='utf-8')
samples=data['samples'];assert samples,'No monitoring samples'
G=1024**3;M=1024**2
def stamp(t):return datetime.datetime.fromtimestamp(t,datetime.timezone(datetime.timedelta(hours=8))).strftime('%m-%d %H:%M:%S')
def ordinary(d):
 m=d.get('memory_stat',{});return max(0,m.get('file',0)-m.get('shmem',0))/G
def io(d):
 # Count the calibrated DM layer only; physical layer would double-count.
 for line in (d.get('io_stat') or '').splitlines():
  f=line.split()
  if f[0]=='253:0':return sum(int(x.split('=')[1]) for x in f[1:] if x.startswith(('rbytes=','wbytes=')))
 return None
def rate(v,old,dt):return (v-old)/dt if v is not None and old is not None and v>=old and dt>0 else None
series={k:[] for k in ['memory','file','anon','shmem','cpu','io','sql','host_memory','host_cpu','network','host_disk']}
def network_total(h):
 interfaces=[]
 for line in h.get('network','').splitlines():
  if ':' not in line:continue
  name,rest=line.split(':',1);name=name.strip();v=rest.split()
  if name.startswith(('eth','ens','enp','eno')):interfaces.append((name,int(v[0])+int(v[8])))
 return sum(v for _,v in interfaces) if interfaces else None
def disk_total(h):
 for line in h.get('diskstats','').splitlines():
  f=line.split()
  if f[:2]==['8','0']:return (int(f[5])+int(f[9]))*512
 return None
prev=None
for s in samples:
 dt=s['time']-prev['time'] if prev else 0
 for role in ['dbs','hisdbs']:
  d=s['containers'].get(role,{});p=prev['containers'].get(role,{}) if prev else {}
  if d.get('error'):continue
  for key,val in [('memory',d.get('memory_current',0)/G),('file',ordinary(d)),('anon',d.get('memory_stat',{}).get('anon',0)/G),('shmem',d.get('memory_stat',{}).get('shmem',0)/G),('sql',d.get('sql',{}).get('probe_ms'))]:series[key].append((s['time'],role,val))
  if p.get('cid')==d.get('cid'):
   v=rate(d.get('cpu_stat',{}).get('usage_usec'),p.get('cpu_stat',{}).get('usage_usec'),dt)
   series['cpu'].append((s['time'],role,v/1e6 if v is not None else None))
   v=rate(io(d),io(p),dt);series['io'].append((s['time'],role,v/M if v is not None else None))
 h=s['host'];series['host_memory'].append((s['time'],'宿主机可用',h['memory']['MemAvailable']/G))
 if prev:
  ticks=h['cpu_ticks'];old=prev['host']['cpu_ticks'];total=sum(ticks[:8])-sum(old[:8]);idle=sum(ticks[3:5])-sum(old[3:5])
  series['host_cpu'].append((s['time'],'宿主机',(1-idle/total)*100 if total>0 else None))
  for key,fn in [('network',network_total),('host_disk',disk_total)]:
   v=rate(fn(h),fn(prev['host']),dt);series[key].append((s['time'],'宿主机',v/M if v is not None else None))
 prev=s
last=samples[-1]
from monitor_summary import analyze
events=json.loads((BASE/'deployment-events.json').read_text(encoding='utf-8')) if (BASE/'deployment-events.json').exists() else []
analysis,lines=analyze(data,events)
(BASE/'hourly-analysis.json').write_text(json.dumps(analysis,ensure_ascii=False,indent=2),encoding='utf-8')
cards=[]
for role in ['dbs','hisdbs']:
 d=last['containers'].get(role,{});m=d.get('memory_current');crg=d.get('crg',{});ccg=d.get('ccg',{})
 value=f'{m/G:.2f} GiB' if m is not None else '未知'
 cards.append(f'<div class="card"><h2>🗄️ {role.upper()}</h2><b>{value}</b><p>容器ID：{html.escape(str(d.get("cid","未知"))[:12])}</p><p>CPU 配额：{html.escape(str(d.get("cpu_max","未知")))}</p><p>内存硬限额：{html.escape(str(d.get("memory_max","未知")))}</p><p>SQL：{"正常" if d.get("sql",{}).get("ok") else "异常/未知"} · OOM累计：{html.escape(str((d.get("memory_events") or {}).get("oom_kill","未知")))}</p><p>ResourceGuard：{html.escape(str(crg.get("state","未知")))}</p><p>CacheGuard：{html.escape(str(ccg.get("decision",{})))}</p></div>')

def chart(key,title,unit,desc):
 points=series[key];valid=[v for _,_,v in points if v is not None];t0=samples[0]['time'];t1=last['time'];mx=max(valid,default=1)*1.15 or 1
 colors={'dbs':'#60a5fa','hisdbs':'#34d399','宿主机可用':'#c084fc','宿主机':'#fbbf24'}
 svg='<svg viewBox="0 0 720 225" role="img" aria-label="'+html.escape(title)+'">'
 for i in range(5):
  y=175-i*38;svg+=f'<path d="M60 {y}H700" stroke="#263244"/><text x="4" y="{y+4}">{mx*i/4:.1f}</text>'
 for role in sorted(set(r for _,r,_ in points)):
  segments=[];ps=[];previous_cid=None;previous_time=None
  by_time={s['time']:s['containers'].get(role,{}).get('cid') for s in samples} if role in ['dbs','hisdbs'] else {}
  for t,r,v in points:
   if r!=role:continue
   cid=by_time.get(t)
   if v is None or (previous_time is not None and (t-previous_time>90 or cid!=previous_cid)):
    if ps:segments.append(ps);ps=[]
   if v is not None:ps.append(f'{60+(t-t0)/max(1,t1-t0)*640:.1f},{175-v/mx*152:.1f}')
   previous_cid=cid;previous_time=t
  if ps:segments.append(ps)
  for ps in segments:svg+=f'<polyline points="{" ".join(ps)}" fill="none" stroke="{colors.get(role,"white")}" stroke-width="2.5"/>'
 svg+=f'<text x="60" y="205">{stamp(t0)[6:]}</text><text x="575" y="205">{stamp(t1)[6:]}</text></svg>'
 return '<section><h2>'+title+' <small>'+unit+'</small></h2>'+svg+'<p>'+desc+'</p></section>'
charts=[chart('memory','🧠 容器总内存','GiB','cgroup memory.current；最新配置DBS硬上限15.46GiB（25%），HISDBS21.65GiB（35%）；历史旧容器限制见分段报告。'),chart('file','📁 普通文件缓存','GiB','max(file − shmem, 0)；CacheGuard 只治理支持的普通文件页。'),chart('shmem','🔗 共享内存','GiB','包括数据库共享映射；CacheGuard 无法替代 shared_buffers 容量规划。'),chart('anon','📦 匿名内存','GiB','进程堆等内存，CacheGuard 不主动回收。'),chart('cpu','⚙️ 容器 CPU','核','同一容器ID相邻样本 usage_usec 增量；最新配置两个容器均为8.4核（30%）。'),chart('io','💽 容器磁盘流量','MiB/s','只统计253:0层的读写增量，避免与底层磁盘重复计数。'),chart('sql','🩺 SQL 探针耗时','ms','含crictl和客户端启动开销，不是业务SQL的p99。'),chart('host_memory','🏠 宿主机可用内存','GiB','MemAvailable 为内核估计的可用量，不等于空闲量。'),chart('host_cpu','⚡ 宿主机 CPU','%','基于/proc/stat；蓝色DBS、绿色HISDBS，其余图按标题显示宿主机。')]
doc='''<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>DBS / HISDBS 资源监控</title><style>body{background:#0c1422;color:#dce7f5;font:15px system-ui;max-width:1500px;margin:36px auto;padding:0 24px}h1{font-size:30px}h2{font-size:18px}b{font-size:28px}small,p{color:#9cacc0}small{font-size:13px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(460px,1fr));gap:20px}.card,section{background:#142033;padding:22px;border:1px solid #26364c;border-radius:14px}svg{width:100%}svg text{fill:#9cacc0;font-size:12px}pre{white-space:pre-wrap;line-height:1.7}.blue{color:#60a5fa}.green{color:#34d399}</style><h1>DBS / HISDBS 资源监控</h1>'''
charts.extend([chart('network','🌐 宿主机网络收发','MiB/s','累计物理接口eth/ens/enp/eno收发；hostNetwork容器不重复归属流量。'),chart('host_disk','🖴 宿主机物理磁盘流量','MiB/s','仅8:0物理盘扇区增量，读写合计，不与DM层相加。')])
doc+=f'<p>更新 {stamp(data["generated"])}（北京时间） · 每分钟采样 · 本页每次执行 refresh-monitor.py 更新 · <span class="blue">● DBS</span> <span class="green">● HISDBS</span></p><div class="grid">'+''.join(cards)+'</div><pre>'+html.escape('\n'.join(lines))+'</pre><div class="grid">'+''.join(charts)+'</div><p>监测另含磁盘容量、PSI、cpu.stat节流、OOM、refault与原始Guard状态，详见hourly-analysis.json与hourly-latest.json。缺失值不代表0；容器重建后累计计数重新计算。报告不是全量数据校验或备份恢复验收。</p></html>'
(BASE/'资源监控.html').write_text(doc,encoding='utf-8')
(BASE/'hourly-summary.txt').write_text('\n'.join(lines)+'\n',encoding='utf-8')
print('\n'.join(lines));print('CHART: '+str(BASE/'资源监控.html'))
