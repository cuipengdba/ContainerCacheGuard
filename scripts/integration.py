#!/usr/bin/env python3
"""Linux CLI regression using only a private temporary directory.
Never calls global drop_caches or changes cgroup settings. Requires readable
memory cgroup and an ext4/xfs/btrfs/overlay/f2fs temporary directory.
"""
import argparse, datetime, hashlib, json, os, pathlib, signal, socket, subprocess, tempfile, time

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
def listening_tcp_sockets(pid):
    # Match actual process FDs to network-namespace socket tables. Merely
    # probing an expected port would miss a listener bound to a random port.
    proc=pathlib.Path('/proc')/str(pid)
    inodes=set()
    for fd in (proc/'fd').iterdir():
        try:target=os.readlink(fd)
        except FileNotFoundError:continue
        if target.startswith('socket:['):inodes.add(target[8:-1])
    result=[]
    for name in ['tcp','tcp6']:
        table=proc/'net'/name
        if name=='tcp6' and not table.exists():continue
        for line in table.read_text().splitlines()[1:]:
            parts=line.split()
            if parts[3]=='0A' and parts[9] in inodes:result.append(parts[1])
    return result
def main():
    ap=argparse.ArgumentParser();ap.add_argument('binary');ap.add_argument('--output');args=ap.parse_args();binary=str(pathlib.Path(args.binary).resolve());checks=[]
    with tempfile.TemporaryDirectory(prefix='ccg-regression-') as tmp:
        w=pathlib.Path(tmp);data=w/'data';data.mkdir();file=data/'payload';protected=data/'protected';body=os.urandom(1<<20)
        for f in [file,protected]:
            with f.open('wb') as out:
                for _ in range(64):out.write(body)
                out.flush();os.fsync(out.fileno())
        digest=hashlib.sha256(file.read_bytes()).hexdigest()
        cfg=json.loads(run([binary,'init-config','--mode','paths','--path',data]))
        assert not any(k in cfg for k in ['web_listen','web_token_file','web_history_samples']);checks.append('new_config_has_no_web_settings')
        token=w/'must-not-read-token';os.mkfifo(token,0o600)
        occupied=socket.socket();occupied.bind(('127.0.0.1',0));occupied.listen(1)
        cfg.update(web_listen='127.0.0.1:'+str(occupied.getsockname()[1]),web_token_file=str(token),web_history_samples=21600,action='enforce',trigger='always',warmup_seconds=0,protect_seconds=1,min_file_age_seconds=0,recheck_seconds=1,cooldown_seconds=3,interval_ms=100,log_interval_seconds=1,worker_budget_ms=50,max_cpu_percent=50,max_dirty_bytes=1<<40,max_io_psi_avg10=0,initial_reclaim_bytes_per_second=16<<20,reclaim_bytes_per_second=32<<20,exclude_paths=[str(protected)],state_file=str(w/'run/status.json'))
        config=w/'config.json';config.write_text(json.dumps(cfg));doctor=json.loads(run([binary,'doctor','--config',config]));checks.append('doctor_real_cgroup')
        initial=json.loads(run([binary,'scan','--path',file]))
        log=(w/'daemon.log').open('w');p=subprocess.Popen([binary,'run','--config',str(config)],stdout=log,stderr=subprocess.STDOUT)
        try:
            state=w/'run/status.json';s=wait_state(state,lambda s:s['counters']['observed_net_reduction_bytes']>=16<<20);checks.append('enforce_measured_reduction')
            os.kill(p.pid,signal.SIGUSR1);wait_state(state,lambda s:s['decision']['state']=='SUSPENDED');time.sleep(.3);s=json.loads(state.read_text());n=s['counters']['advice_bytes'];time.sleep(1.5);s=json.loads(state.read_text());assert s['counters']['advice_bytes']==n;checks.append('pause_stops_advice')
            assert not s['web_service_enabled'] and 'web_address' not in s and not listening_tcp_sockets(p.pid)
            checks.append('no_http_tcp_listener_with_legacy_web_config')
            checks.append('legacy_web_config_ignores_occupied_port_and_token_fifo')
            assert s['paused'] and not s['decision']['allow'] and not s['metrics'].get('error')
            checks.append('paused_state_file_remains_available')
            assert s['metrics']['current_bytes']>0 and s['metrics']['available_metrics']['current_bytes']
            checks.append('state_file_has_real_cgroup_values')
            second=subprocess.run([binary,'run','--config',str(config),'--duration','1s'],capture_output=True,text=True,timeout=10);assert second.returncode!=0 and 'another daemon' in second.stderr;checks.append('duplicate_instance_rejected')
            hot=json.loads(run([binary,'scan','--path',protected]));assert hot['Resident']>=60<<20;checks.append('excluded_file_stays_resident')
            os.kill(p.pid,signal.SIGUSR2);wait_state(state,lambda s:not s['paused']);checks.append('resume')
        finally:
            p.terminate();p.wait(10);log.close();occupied.close()
        assert hashlib.sha256(file.read_bytes()).hexdigest()==digest;checks.append('content_unchanged')
        daily=pathlib.Path(s['logging']['directory'])
        daily_files=list(daily.glob('containercacheguard-????-??-??.log'))
        assert len(daily_files)==1 and s['logging']['keep_days']==30 and s['logging']['max_total_bytes']==1000000000
        events=[json.loads(line) for line in daily_files[0].read_text().splitlines()]
        assert any(e.get('event')=='daemon_started' for e in events) and any(e.get('event')=='daemon_stopped' for e in events)
        assert any(e.get('metrics',{}).get('current_bytes',0)>0 for e in events)
        checks.append('daily_file_console_and_lifecycle_logs')
        deprecated=[e for e in events if e.get('event')=='deprecated_web_settings_ignored']
        assert len(deprecated)==1 and set(deprecated[0]['fields'])=={'web_listen','web_token_file','web_history_samples'}
        assert str(token) not in daily_files[0].read_text()
        checks.append('deprecated_web_warning_once_without_token_path')
        statuses=[e for e in events if e.get('event')=='status']
        assert statuses and all(e.get('message') and e.get('level') and 'summary' in e for e in statuses)
        assert any(e['summary'].get('current_mib',0)>0 for e in statuses)
        assert any(e.get('since_previous_log') and e['since_previous_log']['valid'] for e in statuses)
        assert any(e.get('summary',{}).get('slab_mib') is None for e in statuses) or statuses[0]['metrics']['available_metrics'].get('slab_bytes')
        checks.append('detailed_chinese_summary_and_interval_changes')
        assert len((w/'daemon.log').read_text().splitlines())==len(events)
        checks.append('console_and_daily_file_receive_same_records')
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
        # Production must continue rejecting ordinary files as cgroup metrics.
        fake=w/'fake-cgroup';fake.mkdir()
        cfg.update(cgroup_path=str(fake),state_file=str(w/'fake/status.json'))
        config.write_text(json.dumps(cfg))
        rejected=subprocess.run([binary,'run','--config',str(config),'--duration','2s'],capture_output=True,text=True,timeout=5)
        assert rejected.returncode!=0 and 'real cgroup filesystem' in rejected.stderr
        checks.append('fake_cgroup_filesystem_is_rejected')
        cfg.update(cgroup_path='')
        # Auto scope is tested separately, in observe mode; it never evicts
        # arbitrary runner data while discovering mounted data directories.
        cfg.update(mode='auto',paths=[],auto_roots=[str(data)],action='observe',state_file=str(w/'auto/status.json'))
        config.write_text(json.dumps(cfg));run([binary,'run','--config',config,'--duration','2s']);s=json.loads((w/'auto/status.json').read_text());assert str(data) in s['roots'] and s['counters']['advice_bytes']==0;checks.append('auto_discovers_and_observe_never_advises')
        cfg.update(state_file=str(w/'legacy-flags/status.json'))
        config.write_text(json.dumps(cfg))
        run([binary,'run','--config',config,'--duration','1s','--web-listen','not-an-address','--web-token-file',token])
        assert not json.loads((w/'legacy-flags/status.json').read_text())['web_service_enabled']
        checks.append('legacy_web_flags_are_inert')
        # A matching FIFO must fail before any daemon work starts, never block.
        unsafe=w/'unsafe-logs';unsafe.mkdir(mode=0o700)
        day=datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%d')
        os.mkfifo(unsafe/('containercacheguard-'+day+'.log'),0o600)
        cfg.update(action='enforce',log_directory=str(unsafe),log_timezone='UTC',state_file=str(w/'logfail/status.json'))
        config.write_text(json.dumps(cfg))
        failed=subprocess.run([binary,'run','--config',str(config),'--duration','2s'],capture_output=True,text=True,timeout=5)
        assert failed.returncode!=0 and 'daily logs unavailable' in failed.stderr
        assert not (w/'logfail/status.json').exists()
        checks.append('unsafe_log_destination_fails_before_worker_start')
        # Strict doctor establishes rate baselines but never weakens guards.
        cfg.update(action='observe',web_listen='',web_token_file='',cgroup_path='',log_directory='',state_file=str(w/'strict/status.json'))
        config.write_text(json.dumps(cfg))
        strict=json.loads(run([binary,'doctor','--config',config,'--require-ready']))
        assert strict['preflight']['protection_inputs_ready'] and strict['protection']['ready']
        checks.append('strict_doctor_establishes_two_sample_baseline')
        cfg.update(business_signal=str(w/'missing-business-signal'),max_business_p99_ms=100)
        config.write_text(json.dumps(cfg))
        bad=subprocess.run([binary,'doctor','--config',str(config),'--require-ready'],capture_output=True,text=True,timeout=10)
        assert bad.returncode!=0 and not json.loads(bad.stdout)['preflight']['protection_inputs_ready']
        checks.append('strict_doctor_rejects_missing_enabled_business_input')
        # Deliberately nonexistent cgroup proves report never collects metrics.
        cfg.update(cgroup_path=str(w/'not-a-real-cgroup'))
        config.write_text(json.dumps(cfg))
        sample=json.loads(state.read_text())
        at=datetime.datetime(2026,11,11,10,59,50,tzinfo=datetime.timezone(datetime.timedelta(hours=8)))
        records=[]
        for i in range(3):
            item=json.loads(json.dumps(sample))
            stamp=(at+datetime.timedelta(seconds=i*10)).isoformat()
            item.update(time=stamp,event='status',pid=12345,version='fixture',action='observe',decision=dict(state='OBSERVE',allow=False,reason='observation_only'))
            item['metrics'].update(time=stamp,cgroup='/nonexistent-offline-report',version=2)
            item['counters'].update(advice_bytes=0,observed_net_reduction_bytes=0,observed_reappeared_bytes=0)
            records.append(json.dumps(item))
        reportdir=w/'reportlogs';reportdir.mkdir();source=reportdir/'containercacheguard-2026-11-11.log'
        source.write_text('\n'.join(records)+'\n')
        summary=json.loads(run([binary,'report','--config',config,'--log-directory',reportdir,'--format','json','--instance','DBS']))
        assert len(summary['hours'])==2 and sum(h['observed_interval_seconds'] for h in summary['hours'])==20
        assert all(h['advice_allowed_interval_seconds']==0 for h in summary['hours'])
        checks.append('offline_report_does_not_read_configured_cgroup_or_data')
        checks.append('hour_report_splits_boundary_and_labels_observe')
        markdown=w/'hourly.md'
        run([binary,'report','--log-file',source,'--output',markdown])
        assert '小时统计' in markdown.read_text() and '不能证明业务安全' in markdown.read_text()
        checks.append('offline_report_writes_chinese_markdown')
        for flags in [['--log-file',source,'--output',source],['--log-file',source,'--max-gap','0s']]:
            bad=subprocess.run([binary,'report',*map(str,flags)],capture_output=True,text=True,timeout=10)
            assert bad.returncode!=0
        checks.append('offline_report_rejects_overwrite_and_invalid_gap')
        os.mkfifo(reportdir/'containercacheguard-2026-11-12.log',0o600)
        bad=subprocess.run([binary,'report','--log-directory',str(reportdir)],capture_output=True,text=True,timeout=5)
        assert bad.returncode!=0
        checks.append('offline_report_rejects_fifo_without_blocking')
        report=dict(success=True,checks=checks,kernel=os.uname().release,arch=os.uname().machine,page_size=os.sysconf('SC_PAGE_SIZE'),initial_resident=initial['Resident'],kubernetes_eviction_test=False)
        if args.output:pathlib.Path(args.output).write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps(report,indent=2))
if __name__=='__main__':main()
