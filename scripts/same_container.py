#!/usr/bin/env python3
"""Ephemeral PG14+backup+CCG integration, not a production eviction test.

Only creates/removes its uniquely named CI container. No host data mounts,
drop_caches, global tuning, existing database restart, or remote lab access.
"""
import argparse, datetime, json, pathlib, subprocess, tempfile, threading, time, uuid


def command(args, timeout=120, check=True):
    p = subprocess.run(list(map(str, args)), capture_output=True, text=True, timeout=timeout)
    if check and p.returncode:
        raise RuntimeError('command failed: '+p.stdout[-1500:]+p.stderr[-1500:])
    return p


def main():
    ap = argparse.ArgumentParser(); ap.add_argument('binary'); ap.add_argument('--output', required=True); a=ap.parse_args()
    name='ccg-rc18-'+uuid.uuid4().hex[:12]
    report={'success':False,'scope':'ephemeral single container PG14, write load, pg_basebackup, pg_verifybackup, isolated recovery and CCG; not DBS 1200-schema/1.5TB or K8s eviction/48h acceptance','checks':[]}
    probes=[]; stopping=threading.Event(); writer=None; created=False
    def ex(*args, check=True): return command(['docker','exec','-u','postgres',name,*args],check=check)
    def sql(q): return ex('psql','-XAt','-v','ON_ERROR_STOP=1','-U','postgres','-c',q).stdout.strip()
    def state():
        try:return json.loads(ex('cat','/tmp/ccg-state/status.json').stdout)
        except Exception:return None
    try:
        command(['docker','run','-d','--name',name,'--memory=2g','--memory-swap=2g','-e','POSTGRES_PASSWORD=ccg-isolated-ci-only','postgres:14','-c','shared_buffers=64MB'])
        created=True
        for _ in range(90):
            if ex('pg_isready','-U','postgres',check=False).returncode==0:break
            time.sleep(1)
        else:raise AssertionError('PG14 not ready')
        command(['docker','cp',pathlib.Path(a.binary).resolve(),name+':/usr/local/bin/ccg'])
        command(['docker','exec',name,'chmod','755','/usr/local/bin/ccg'])
        ex('mkdir','-m','700','/tmp/ccg-state','/tmp/ccg-tasks','/tmp/ccg-backup','/tmp/ccg-restore')
        cfg=json.loads(ex('ccg','init-config','--path','/var/lib/postgresql/data','--path','/tmp/ccg-backup').stdout)
        cfg.update(instance_id='ci/same-container',action='enforce',state_file='/tmp/ccg-state/status.json',log_directory='/tmp/ccg-state/logs',
                   cache_target_bytes=16<<20,warmup_seconds=1,recovery_healthy_seconds=1,protect_seconds=1,min_file_age_seconds=1,
                   interval_ms=200,log_interval_seconds=1,max_io_psi_avg10=90,max_dirty_bytes=128<<20,
                   path_policies=[dict(path='/var/lib/postgresql/data',role='database-source'),dict(path='/tmp/ccg-backup',role='backup-output',low_reuse=True)],
                   task_signal_directory='/tmp/ccg-tasks',task_source_id='ci-tasks')
        with tempfile.TemporaryDirectory(prefix='ccg-same-') as tmp:
            p=pathlib.Path(tmp)/'config.json';p.write_text(json.dumps(cfg));command(['docker','cp',p,name+':/tmp/ccg-config.json'])
        command(['docker','exec','-d','-u','postgres',name,'ccg','run','--config','/tmp/ccg-config.json'])
        sql("CREATE TABLE ccg_same(id int PRIMARY KEY, n bigint, pad text); INSERT INTO ccg_same SELECT i,0,repeat(md5(i::text),64) FROM generate_series(1,30000)i; CHECKPOINT;")
        def load():
            while not stopping.is_set():
                begin=time.monotonic()
                try:sql('UPDATE ccg_same SET n=n+1 WHERE id=1; SELECT count(*) FROM ccg_same;');probes.append({'ms':1000*(time.monotonic()-begin),'error':False})
                except Exception:probes.append({'error':True})
                stopping.wait(.2)
        writer=threading.Thread(target=load,daemon=True);writer.start()
        baseline=state();assert baseline and not baseline['web_service_enabled']
        wrap=['ccg','task-run','--directory','/tmp/ccg-tasks','--source-id','ci-tasks','--instance-id','ci/same-container']
        ex(*wrap,'--stage','backup','--root','/var/lib/postgresql/data','--root','/tmp/ccg-backup','--','pg_basebackup','-h','/var/run/postgresql','-U','postgres','-D','/tmp/ccg-backup/full','--max-rate=20M','-X','stream')
        ex(*wrap,'--stage','verify','--root','/tmp/ccg-backup','--','pg_verifybackup','/tmp/ccg-backup/full')
        report['checks'].append('live_write_backup_and_pg_verifybackup_succeeded')
        # Keep a verify task alive so the controller observes a protected phase.
        command(['docker','exec','-d','-u','postgres',name,*wrap,'--stage','verify','--root','/tmp/ccg-backup','--','sleep','6'])
        deadline=time.monotonic()+8;seen=None
        while time.monotonic()<deadline:
            seen=state()
            if seen and any(t['stage']=='verify' for t in seen.get('tasks',{}).get('active',[])):break
            time.sleep(.2)
        else:raise AssertionError('verify task not observed')
        report['checks'].append('task_phase_observed_in_same_container')
        report['during_verify_status']=seen
        # Exact return code and literal arguments must survive the wrapper.
        result=ex(*wrap,'--stage','sync','--root','/tmp/ccg-backup','--','sh','-c','test "$1" = \'a b;$x\' && exit 7','test','a b;$x',check=False)
        assert result.returncode==7;report['checks'].append('wrapper_preserved_arguments_and_exit7')
        # A separate restored instance in the SAME disposable container;
        # original PG is left running. Network is disabled for restored PG.
        ex('cp','-a','/tmp/ccg-backup/full/.','/tmp/ccg-restore/')
        ex('mkdir','-m','700','/tmp/restore-socket')
        ex('pg_ctl','-D','/tmp/ccg-restore','-l','/tmp/restore-startup.log','-o',"-p 55433 -c listen_addresses='' -c unix_socket_directories=/tmp/restore-socket",'-w','start')
        rows=ex('psql','-h','/tmp/restore-socket','-p','55433','-U','postgres','-XAt','-c','SELECT count(*) FROM ccg_same;').stdout.strip()
        assert rows=='30000';report['checks'].append('independent_restore_started_and_30000_rows_verified')
        ex('pg_ctl','-D','/tmp/ccg-restore','-m','fast','-w','stop')
        time.sleep(2);last=state();assert last
        assert last['metrics']['oom_kill_total']==baseline['metrics']['oom_kill_total'];report['checks'].append('no_new_visible_cgroup_oom')
        stopping.set();writer.join(30)
        assert probes and not any(p['error'] for p in probes)
        times=sorted(p['ms'] for p in probes);report['sql_probes']={'samples':len(times),'errors':0,'p99_ms':times[min(len(times)-1,int(.99*len(times)))],'max_ms':max(times)}
        report['last_status']=last
        # Actual process cgroups are read, not inferred from the diagram.
        pgpid=ex('head','-n','1','/var/lib/postgresql/data/postmaster.pid').stdout.strip()
        cgpid=str(last['pid']);pgcg=ex('cat','/proc/'+pgpid+'/cgroup').stdout;ccgcg=ex('cat','/proc/'+cgpid+'/cgroup').stdout
        assert pgcg==ccgcg;report['same_cgroup_verified']=True
        report['success']=True
    except Exception as exc:report['error']=str(exc);raise
    finally:
        stopping.set()
        if writer:writer.join(30)
        if created:command(['docker','rm','-f',name],check=False)
        pathlib.Path(a.output).write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps({k:v for k,v in report.items() if not k.endswith('status')},indent=2))


if __name__=='__main__':main()
