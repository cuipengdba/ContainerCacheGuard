//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// WebPoint contains only the fields needed for a bounded recent-history graph.
// The separate 48-hour lab collector stores long-term evidence outside the daemon.
type WebPoint struct {
	Unavailable  []string  `json:"unavailable,omitempty"`
	Time         time.Time `json:"time"`
	Valid        bool      `json:"valid"`
	IntervalMS   int       `json:"sample_interval_ms"`
	File         int64     `json:"file_bytes"`
	ActiveFile   int64     `json:"active_file_bytes"`
	InactiveFile int64     `json:"inactive_file_bytes"`
	Anon         int64     `json:"anon_bytes"`
	Shmem        int64     `json:"shmem_bytes"`
	Slab         int64     `json:"slab_bytes"`
	Current      int64     `json:"current_bytes"`
	Limit        int64     `json:"limit_bytes"`
	Dirty        int64     `json:"dirty_bytes"`
	Writeback    int64     `json:"writeback_bytes"`
	IOBPS        float64   `json:"io_bytes_per_second"`
}

type webState struct {
	mu      sync.RWMutex
	latest  Status
	ready   bool
	history []WebPoint
	next    int
	limit   int
	server  *http.Server
	listen  net.Listener
}

func startWeb(ctx context.Context, c Config) (*webState, error) {
	if c.WebListen == "" {
		return nil, nil
	}
	var token string
	if c.WebTokenFile != "" {
		b, err := os.ReadFile(c.WebTokenFile)
		if err != nil {
			return nil, fmt.Errorf("web token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
		if len(token) < 16 || len(token) > 4096 {
			return nil, errors.New("web token 长度必须为16..4096字节")
		}
	}
	listener, err := net.Listen("tcp", c.WebListen)
	if err != nil {
		return nil, fmt.Errorf("web listen: %w", err)
	}
	w := &webState{history: make([]WebPoint, 0, c.WebHistorySamples), limit: c.WebHistorySamples, listen: listener}
	sem := make(chan struct{}, 4)
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Cache-Control", "no-store")
			rw.Header().Set("X-Content-Type-Options", "nosniff")
			rw.Header().Set("X-CCG-Server-Time", time.Now().UTC().Format(time.RFC3339Nano))
			if token != "" {
				user, password, ok := r.BasicAuth()
				if !ok || user != "ccg" || subtle.ConstantTimeCompare([]byte(password), []byte(token)) != 1 {
					rw.Header().Set("WWW-Authenticate", `Basic realm="ContainerCacheGuard"`)
					http.Error(rw, "authentication required", http.StatusUnauthorized)
					return
				}
			}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			default:
				http.Error(rw, "busy", http.StatusServiceUnavailable)
				return
			}
			next(rw, r)
		}
	}
	mux.HandleFunc("GET /", auth(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(rw, webPage)
	}))
	mux.HandleFunc("GET /api/v1/status", auth(func(rw http.ResponseWriter, _ *http.Request) {
		w.mu.RLock()
		value, ready := w.latest, w.ready
		w.mu.RUnlock()
		if !ready {
			http.Error(rw, "waiting for first sample", http.StatusServiceUnavailable)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(value)
	}))
	mux.HandleFunc("GET /api/v1/history", auth(func(rw http.ResponseWriter, _ *http.Request) {
		w.mu.RLock()
		points := w.pointsLocked()
		w.mu.RUnlock()
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(points)
	}))
	mux.HandleFunc("GET /healthz", auth(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(rw, "ok\n")
	}))
	mux.HandleFunc("GET /readyz", auth(func(rw http.ResponseWriter, _ *http.Request) {
		w.mu.RLock()
		snapshot, present := w.latest, w.ready
		w.mu.RUnlock()
		h := evaluateSnapshot(snapshot, present, time.Now())
		rw.Header().Set("Content-Type", "application/json")
		if !h.Ready {
			rw.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(rw).Encode(map[string]any{"ready": h.Ready, "reason": h.Reason})
	}))
	mux.HandleFunc("GET /metrics", auth(func(rw http.ResponseWriter, _ *http.Request) {
		w.mu.RLock()
		snapshot, present := w.latest, w.ready
		w.mu.RUnlock()
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(rw, prometheusSnapshot(snapshot, present, time.Now()))
	}))
	w.server = &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); w.close() }()
	go func() { _ = w.server.Serve(listener) }()
	return w, nil
}

func (w *webState) publish(s Status) {
	m := s.Metrics
	p := WebPoint{Time: s.Time, Valid: m.Error == "", IntervalMS: s.IntervalMS, File: m.File, ActiveFile: m.ActiveFile,
		InactiveFile: m.InactiveFile, Anon: m.Anon, Shmem: m.Shmem, Slab: m.Slab,
		Current: m.Current, Limit: m.Limit, Dirty: m.Dirty, Writeback: m.Writeback,
		IOBPS: m.IOBytesPerSecond}
	if m.Available != nil {
		for _, key := range []string{"file_bytes", "active_file_bytes", "inactive_file_bytes", "anon_bytes", "shmem_bytes", "slab_bytes", "current_bytes", "dirty_bytes", "writeback_bytes", "io_bytes_per_second"} {
			if !m.Available[key] {
				p.Unavailable = append(p.Unavailable, key)
			}
		}
	}
	w.mu.Lock()
	w.latest, w.ready = s, true
	if len(w.history) >= w.limit {
		w.history[w.next] = p
		w.next = (w.next + 1) % w.limit
	} else {
		w.history = append(w.history, p)
	}
	w.mu.Unlock()
}

// Caller holds the read lock. Rotating only when requested avoids copying the
// entire chart on every one-second metrics sample.
func (w *webState) pointsLocked() []WebPoint {
	if len(w.history) < w.limit || w.next == 0 {
		return append([]WebPoint(nil), w.history...)
	}
	points := make([]WebPoint, 0, len(w.history))
	points = append(points, w.history[w.next:]...)
	return append(points, w.history[:w.next]...)
}

func (w *webState) close() {
	if w != nil && w.listen != nil {
		_ = w.listen.Close()
	}
}

const webPage = `<!doctype html><html lang="zh-CN"><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>ContainerCacheGuard 缓存监控</title>
<style>
:root{color-scheme:dark;font-family:system-ui,sans-serif;background:#101722;color:#e9f0f7}
body{margin:0 auto;max-width:1180px;padding:24px}h1{margin:0;font-size:25px}small{color:#a8b5c5}
.top{display:flex;justify-content:space-between;gap:18px;align-items:center;flex-wrap:wrap}
.badge{background:#21354b;border-radius:99px;padding:7px 13px}.bad{background:#613335}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(190px,1fr));gap:12px;margin:22px 0}
.card,.panel{background:#192433;border:1px solid #304154;border-radius:12px;padding:16px}
.label{color:#a8b5c5;font-size:13px}.value{font-size:25px;font-weight:650;margin:5px 0}
.unit{font-size:13px;color:#a8b5c5}.panel{margin:14px 0}canvas{width:100%;height:270px}
.legend{display:flex;gap:18px;flex-wrap:wrap;color:#bac9d8;font-size:13px}.dot{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:5px}
pre{overflow:auto;white-space:pre-wrap;color:#c5d4e2;font-size:12px}a{color:#78c8ff}
</style><body><div class="top"><div><h1>ContainerCacheGuard</h1><small>容器内存与文件缓存 · 秒级状态</small></div><span id="state" class="badge">连接中</span></div>
<div class="grid" id="cards"></div>
<div class="panel"><div class="top"><strong>最近内存变化</strong><small id="range"></small></div><canvas id="chart"></canvas><div class="legend" id="legend"></div></div>
<div class="panel"><strong>当前判断与建议</strong><pre id="explain">等待首个采样</pre></div>
<div class="panel"><strong>治理与诊断</strong><pre id="detail">等待首个采样</pre></div>
<small>file 包含 shmem；“普通文件缓存”按 file − shmem 估算。匿名内存、共享内存和 slab 只监测，不由文件回收接口清理。图表来自有界内存历史，刷新页面不会触发文件扫描。</small>
<script>
const fields=[['普通文件缓存','ordinary','#5fd2d8'],['文件缓存 file','file_bytes','#7bb8ff'],['活跃文件 active_file','active_file_bytes','#ffb96b'],['非活跃文件 inactive_file','inactive_file_bytes','#9f8dff'],['匿名内存 anon','anon_bytes','#eb91be'],['共享内存 shmem','shmem_bytes','#73d79c'],['Slab','slab_bytes','#e7d47e'],['内存总量','current_bytes','#ffffff'],['脏页 dirty','dirty_bytes','#ed806e'],['写回 writeback','writeback_bytes','#dcad78']];
const gib=x=>Number.isFinite(x)?(x/1073741824).toFixed(2)+' GiB':'未知';
function card(label,value,color){return '<div class="card"><div class="label">'+label+'</div><div class="value" style="color:'+color+'">'+gib(value)+'</div></div>'}
function render(s,serverTime){const m=s.metrics||{},time=Date.parse(s.time),mt=Date.parse(m.time),server=Date.parse(serverTime),age=server-Math.min(time,Number.isFinite(mt)?mt:time),window=Math.max(1000,3*(s.sample_interval_ms||1000)),fresh=Number.isFinite(age)&&age>=-1000&&age<=window&&time-server<=1000&&(!Number.isFinite(mt)||mt-server<=1000);
 document.getElementById('state').textContent=(fresh?'实时 · ':'数据已过期 · ')+s.decision.state;
 document.getElementById('state').className='badge'+(fresh?'':' bad');
 const known=key=>fresh&&!m.error&&(!m.available_metrics||m.available_metrics[key]===true);
 document.getElementById('cards').innerHTML=fields.map(([label,key,color])=>card(label,key==='ordinary'?(known('file_bytes')&&known('shmem_bytes')?Math.max(0,m.file_bytes-m.shmem_bytes):null):known(key)&&!(key==='slab_bytes'&&m.version!==2)?m[key]:null,color)).join('');
 const dx=fresh?(s.diagnostics||{}):{valid:false,notes:[{message:'采样已过期，当前构成判断不可用；请检查守护进程和采集状态。'}]};
 document.getElementById('explain').textContent=(dx.notes||[]).map(n=>n.message).join('\n\n')+'\n\n'+(dx.valid?'普通文件页以外的计费用量约 '+gib(dx.other_charged_bytes)+'，其中可能含匿名、共享和内核内存；不是一种可直接清理的缓存。':'当前构成判断不可用，先检查采集错误。');
 document.getElementById('explain').textContent+='\n\n发现提示：等待 '+(s.counters.pending_hint_paths||0)+' 个，重试 '+(s.counters.hint_discovery_retries||0)+' 次，队列丢弃 '+(s.counters.hint_queue_dropped||0)+' 次。丢弃后仍依赖普通补扫。';
 const measured=(value,format,key)=>fresh&&!m.error&&(!key||known(key))&&Number.isFinite(value)?format(value):'未知';
 const mem=(value,key)=>measured(value,gib,key),decimal=(value,key)=>measured(value,n=>n.toFixed(1),key);
 document.getElementById('detail').textContent=[
  '采样：'+s.time,
  '容器：'+m.cgroup,
  '策略：'+s.mode+' / '+s.action,
  '状态：'+s.decision.state+' · '+s.decision.reason,
  '采集错误：'+(m.error||'无'),
  'IO：'+decimal(m.io_bytes_per_second/1048576,'io_bytes_per_second')+' MiB/s · IO PSI：'+decimal(m.io_psi_avg10,'io_psi_avg10'),
  '本容器用量：'+mem(m.current_bytes,'current_bytes')+' · 可见最严限额：'+(m.limit_bytes?mem(m.limit_bytes,'limit_bytes'):'未知'),
  '共享父组用量：'+(!m.error&&m.pressured_ancestor_limit_bytes?mem(m.pressured_ancestor_current_bytes)+' / '+mem(m.pressured_ancestor_limit_bytes):'未知或未见有限额')+' · 宿主可用：'+mem(m.host_memavailable_bytes,'host_memavailable_bytes'),
  'OOM kill：'+(m.version===2?measured(m.oom_kill_total,String,'oom_kill_total'):'未知')+' · 建议范围：'+gib(s.counters.advice_bytes||0)+' · 净驻留下降：'+gib(s.counters.observed_net_reduction_bytes||0),
  '自身 RSS：'+decimal(m.self_rss_bytes/1048576,'self_rss_bytes')+' MiB · CPU：'+decimal(m.self_cpu_percent,'self_cpu_percent')+'%',
  '文件页 refault：'+(m.file_refault_rate_valid===true?decimal(m.file_refaults_per_second):'未知')+' 次/秒（不能单独归因于 CCG）',
  '健康恢复：'+(s.recovery?.pending?'等待连续有效采样 '+(s.recovery.healthy_seconds||0).toFixed(1)+' / '+s.recovery.required_seconds+' 秒':'无需等待或已完成')+' · 恢复次数：'+(s.recovery?.recovery_resumes_total||0),
	'保护数据：'+(!fresh?'未知':s.protection?(s.protection.ready?'已启用保护所需数据齐全':'缺少 '+(s.protection.missing_metrics||[]).join(', ')):'旧版本未报告'),
	'每日日志：'+(s.logging?.enabled?s.logging.current_file+' · 受管总量 '+gib(s.logging.managed_bytes)+' / '+gib(s.logging.max_total_bytes)+' · 最多 '+s.logging.keep_days+' 天':'未启用或旧版本未报告'),
  '目录遍历：'+(s.counters.completed_inventory_sweeps||0)+'轮 · 文件索引轮换：'+(s.counters.index_rotations||0)+'次',
  '监控范围：'+(s.roots||[]).join(', '),
  '能力降级：'+(s.degraded||[]).join(', ')
 ].join('\n');
}
function graph(h){
 if(!h.length)return;
 const c=document.getElementById('chart'),dpr=devicePixelRatio||1,w=c.clientWidth,ht=270;
 c.width=w*dpr;c.height=ht*dpr;const x=c.getContext('2d');x.scale(dpr,dpr);x.clearRect(0,0,w,ht);
 const keys=[['file_bytes','#7bb8ff','文件缓存'],['active_file_bytes','#ffb96b','活跃文件'],['anon_bytes','#eb91be','匿名内存'],['shmem_bytes','#73d79c','共享内存'],['current_bytes','#ffffff','内存总量']];
 const good=h.filter(p=>p.valid!==false),max=Math.max(1,...good.map(p=>p.current_bytes||0),...good.map(p=>p.file_bytes||0))*1.1;
 const first=Date.parse(h[0].time),span=Math.max(1,Date.parse(h.at(-1).time)-first);
 x.strokeStyle='#34485b';x.lineWidth=1;
 for(let i=0;i<5;i++){let y=15+(ht-35)*i/4;x.beginPath();x.moveTo(50,y);x.lineTo(w-8,y);x.stroke();x.fillStyle='#a8b5c5';x.font='11px sans-serif';x.fillText(((max*(1-i/4))/1073741824).toFixed(1),2,y+3)}
 for(const [key,color] of keys){
  x.strokeStyle=color;x.lineWidth=2;x.beginPath();let previous=null;
  for(const p of h){
   const t=Date.parse(p.time),value=p[key];
   if(p.valid===false||p.unavailable?.includes(key)||!Number.isFinite(t)||!Number.isFinite(value)){previous=null;continue}
   const px=51+(w-60)*(t-first)/span,py=15+(ht-35)*(1-value/max);
   const gap=previous&&t-Date.parse(previous.time)>Math.max(10000,3*(p.sample_interval_ms||1000),3*(previous.sample_interval_ms||1000));
   if(!previous||gap)x.moveTo(px,py);else x.lineTo(px,py);
   previous=p;
  }
  x.stroke();
 }
 document.getElementById('range').textContent=new Date(h[0].time).toLocaleString()+' — '+new Date(h.at(-1).time).toLocaleString();
 document.getElementById('legend').innerHTML=keys.map(([k,color,label])=>'<span><i class="dot" style="background:'+color+'"></i>'+label+'</span>').join('');
}
async function update(){try{let r=await fetch('/api/v1/status',{cache:'no-store'});if(!r.ok)throw Error('HTTP '+r.status);let serverTime=r.headers.get('X-CCG-Server-Time');render(await r.json(),serverTime)}catch(e){document.getElementById('state').textContent='连接异常 '+e.message;document.getElementById('state').className='badge bad'}}
async function history(){try{let r=await fetch('/api/v1/history',{cache:'no-store'});if(r.ok)graph(await r.json())}catch(e){}}
update();history();setInterval(update,2000);setInterval(history,15000);
</script></body></html>`
